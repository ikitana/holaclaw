package page

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/oklog/ulid/v2"

	"holaclaw/internal/browser/cdp"
	"holaclaw/internal/browser/proto"
)

// A download is a file a hostile page chose to give us: it lands in the
// agent's quarantine under an opaque id, capped per file and per agent, and
// crosses to the daemon through the socket rather than the filesystem (docs/browser.md).
// The name the page suggested is kept as a label and never used as a path.

// quarantine is one agent's download directory and what is in it.
type quarantine struct {
	dir string
	log *slog.Logger

	mu      sync.Mutex
	pending map[string]*pendingDownload // by the browser's GUID
	files   []*quarantined              // completed, in completion order
	total   int64
}

// pendingDownload is a download in flight.
type pendingDownload struct {
	guid      string
	name      string
	cancelled bool
}

// quarantined is a file waiting to be fetched.
type quarantined struct {
	id   string // opaque, one path segment: a ULID
	name string // the sanitised suggested name
	size int64
	at   time.Time
}

func newQuarantine(dir string, log *slog.Logger) *quarantine {
	return &quarantine{dir: dir, log: log, pending: map[string]*pendingDownload{}}
}

// willBegin records a download before its first byte.
func (q *quarantine) willBegin(ev cdp.DownloadWillBeginEvent) {
	if q == nil {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	q.pending[ev.GUID] = &pendingDownload{guid: ev.GUID, name: sanitiseName(ev.SuggestedFilename)}
}

// progress applies the caps and files a finished download. Chromium writes the
// bytes to <dir>/<guid> ("allowAndName"), which is renamed to the opaque id.
func (q *quarantine) progress(ctx context.Context, conn *cdp.Conn, ev cdp.DownloadProgressEvent) {
	if q == nil {
		return
	}
	q.mu.Lock()
	p := q.pending[ev.GUID]
	q.mu.Unlock()
	if p == nil {
		return
	}
	path := filepath.Join(q.dir, ev.GUID)

	switch ev.State {
	case "inProgress":
		if ev.ReceivedBytes <= proto.MaxDownloadBytes && ev.TotalBytes <= proto.MaxDownloadBytes {
			return
		}
		q.cancel(ctx, conn, p, path)
	case "canceled":
		q.forget(p.guid, path)
	case "completed":
		fi, err := os.Stat(path)
		if err != nil {
			q.forget(p.guid, path)
			return
		}
		q.mu.Lock()
		over := fi.Size() > proto.MaxDownloadBytes || q.total+fi.Size() > proto.MaxDownloadTotal
		q.mu.Unlock()
		if over {
			q.log.Warn("browser: download over the quarantine cap, deleted", "bytes", fi.Size())
			q.forget(p.guid, path)
			return
		}
		id := ulid.Make().String()
		if err := os.Rename(path, filepath.Join(q.dir, id)); err != nil {
			q.forget(p.guid, path)
			return
		}
		q.mu.Lock()
		delete(q.pending, p.guid)
		q.files = append(q.files, &quarantined{id: id, name: p.name, size: fi.Size(), at: time.Now()})
		q.total += fi.Size()
		q.mu.Unlock()
	}
}

// cancel stops a download that is over the per-file cap and deletes what
// arrived. internal/browser/cdp has no typed helper for Browser.cancelDownload;
// the raw call goes through the same Conn.Call.
func (q *quarantine) cancel(ctx context.Context, conn *cdp.Conn, p *pendingDownload, path string) {
	q.mu.Lock()
	if p.cancelled {
		q.mu.Unlock()
		return
	}
	p.cancelled = true
	q.mu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, attachTimeout)
	defer cancel()
	if err := rawCall(ctx, conn, "", "Browser.cancelDownload", map[string]any{"guid": p.guid}, nil); err != nil {
		q.log.Warn("browser: cancel download", "err", err)
	}
	q.log.Warn("browser: download over the per-file cap, cancelled", "cap", proto.MaxDownloadBytes)
	q.forget(p.guid, path)
}

// forget drops a pending download and its partial file.
func (q *quarantine) forget(guid, path string) {
	q.mu.Lock()
	delete(q.pending, guid)
	q.mu.Unlock()
	_ = os.Remove(path)
}

// mark is the number of files filed so far; see since.
func (q *quarantine) mark() int {
	if q == nil {
		return 0
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.files)
}

// since returns the files filed after a mark: what one action downloaded.
func (q *quarantine) since(mark int) []proto.Download {
	if q == nil {
		return nil
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if mark < 0 || mark > len(q.files) {
		return nil
	}
	out := make([]proto.Download, 0, len(q.files)-mark)
	for _, f := range q.files[mark:] {
		out = append(out, proto.Download{ID: f.id, Name: f.name, Size: f.size})
	}
	return out
}

// take removes a file from the quarantine's books and returns it.
func (q *quarantine) take(id string) (*quarantined, bool) {
	if q == nil {
		return nil, false
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	for i, f := range q.files {
		if f.id != id {
			continue
		}
		q.files = append(q.files[:i], q.files[i+1:]...)
		q.total -= f.size
		return f, true
	}
	return nil, false
}

// sweep deletes anything older than MaxDownloadAge, books and disk alike: a
// file nobody fetched is not kept around for a page to point at later.
func (q *quarantine) sweep() {
	if q == nil {
		return
	}
	cutoff := time.Now().Add(-proto.MaxDownloadAge)
	q.mu.Lock()
	kept := q.files[:0]
	var stale []string
	for _, f := range q.files {
		if f.at.Before(cutoff) {
			stale = append(stale, f.id)
			q.total -= f.size
			continue
		}
		kept = append(kept, f)
	}
	q.files = kept
	q.mu.Unlock()
	for _, id := range stale {
		_ = os.Remove(filepath.Join(q.dir, id))
	}
	// Files on disk the books do not know about — a crash, an earlier run —
	// are swept on age too.
	entries, err := os.ReadDir(q.dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || info.ModTime().After(cutoff) || q.known(e.Name()) {
			continue
		}
		_ = os.Remove(filepath.Join(q.dir, e.Name()))
	}
}

func (q *quarantine) known(id string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, f := range q.files {
		if f.id == id {
			return true
		}
	}
	return false
}

// remove deletes the agent's whole quarantine.
func (q *quarantine) remove() {
	if q == nil {
		return
	}
	q.mu.Lock()
	q.files, q.total = nil, 0
	q.pending = map[string]*pendingDownload{}
	q.mu.Unlock()
	_ = os.RemoveAll(q.dir)
}

// Download opens one quarantined file and removes it once the reader is
// closed: a file crosses the socket exactly once.
func (e *Engine) Download(_ context.Context, agent, id string) (io.ReadCloser, proto.Download, error) {
	s, err := e.session(agent)
	if err != nil {
		return nil, proto.Download{}, err
	}
	if strings.ContainsAny(id, `/\.`) || id == "" {
		return nil, proto.Download{}, errf(proto.ErrBadAction, "id")
	}
	f, ok := s.dl.take(id)
	if !ok {
		return nil, proto.Download{}, errf(proto.ErrBadAction, "unknown download")
	}
	path := filepath.Join(s.dl.dir, f.id)
	file, err := os.Open(path)
	if err != nil {
		return nil, proto.Download{}, errf(proto.ErrBadAction, "unknown download")
	}
	meta := proto.Download{ID: f.id, Name: f.name, Size: f.size}
	return &downloadReader{File: file, path: path}, meta, nil
}

// downloadReader deletes the file when the reader closes.
type downloadReader struct {
	*os.File
	path string
	once sync.Once
}

func (d *downloadReader) Close() error {
	err := d.File.Close()
	d.once.Do(func() { _ = os.Remove(d.path) })
	return err
}

// sanitiseName makes a page's suggested file name safe to show and to store as
// a label: one path element, no control characters, capped. It is never used
// to build a path — the file on disk is named by its opaque id.
func sanitiseName(name string) string {
	name = filepath.Base(strings.TrimSpace(name))
	name = strings.Map(func(r rune) rune {
		switch {
		case r == '/' || r == '\\' || r == 0:
			return '_'
		case unicode.IsControl(r):
			return -1
		default:
			return r
		}
	}, name)
	name = strings.Trim(name, " .")
	if name == "" || name == "." || name == ".." {
		return "download"
	}
	return truncate(name, 120)
}
