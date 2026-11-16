package backup

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"holaclaw/internal/event"
)

// Status is where a managed backup stands.
type Status string

const (
	StatusRunning Status = "running"
	StatusReady   Status = "ready"
	StatusFailed  Status = "failed"
)

// Origin is how a managed backup came to be.
type Origin string

const (
	// OriginManual is a backup somebody asked for. It is kept until somebody
	// deletes it.
	OriginManual Origin = "manual"
	// OriginAutomatic is a backup the daemon took on its own schedule; only
	// the newest ManagerOptions.Keep of them are kept.
	OriginAutomatic Origin = "automatic"
	// OriginUpload is an archive a client sent in. Kept like a manual one.
	OriginUpload Origin = "upload"
)

// MaxUpload caps an uploaded archive.
const MaxUpload = 16 << 30

// DefaultEvery is how often the daemon takes a backup on its own when
// ManagerOptions.Every does not say.
const DefaultEvery = 24 * time.Hour

// Who the daemon's own backups and deletions are recorded as.
const (
	bySchedule  = "schedule"
	byRetention = "retention"
)

// Record is one managed backup. It is kept as <id>.json beside the archive
// <id>.tar.gz, not in the database: a restored database must not bring back
// the list of backups of the installation it came from.
type Record struct {
	ID      string    `json:"id"`
	Created time.Time `json:"created"`
	// Size is the archive's size in bytes; 0 until the backup is ready.
	Size int64 `json:"size"`
	// Version is the holaclaw that wrote the backup; empty for an upload,
	// whose archive does not say.
	Version         string `json:"version"`
	Origin          Origin `json:"origin"`
	SecretsIncluded bool   `json:"secrets_included"`
	Status          Status `json:"status"`
	// Reason says why a failed backup failed.
	Reason string `json:"reason,omitempty"`
}

var (
	// ErrNotFound is an id no record has.
	ErrNotFound = errors.New("backup: not found")
	// ErrBusy refuses a second backup while one is running.
	ErrBusy = errors.New("backup: another backup is running")
	// ErrNotReady is a backup with no archive to read: still running, or failed.
	ErrNotReady = errors.New("backup: not ready")
	// ErrBadArchive is an archive that is not a holaclaw backup, or does not
	// match its own manifest.
	ErrBadArchive = errors.New("backup: bad archive")
	// ErrIncompatible is an archive this holaclaw cannot restore: its
	// database was written by a newer one.
	ErrIncompatible = errors.New("backup: incompatible archive")
)

// Reasons a backup is failed without an error of its own.
const (
	reasonRestart  = "interrupted by a daemon restart"
	reasonShutdown = "interrupted by the daemon shutting down"
	reasonMissing  = "the archive file is missing"
)

// ManagerOptions configures a Manager.
type ManagerOptions struct {
	// Dir holds the archives and their records. It must not be a directory
	// Create archives: the daemon uses <data_dir>/backups, which it is not.
	Dir string
	// DataDir, ConfigDir and SecretsPath are what Options carries.
	DataDir     string
	ConfigDir   string
	SecretsPath string
	// DB is the live store handle every snapshot goes through.
	DB *sql.DB
	// Version is recorded on each backup.
	Version string
	// Every is how often an automatic backup is due; zero or less means
	// DefaultEvery.
	Every time.Duration
	// Keep is how many automatic backups are kept; the older ones are
	// deleted when a new one is ready. Zero or less takes none at all.
	Keep int
	// Events receives backup.created, backup.failed and backup.deleted; nil
	// drops them.
	Events event.Sink
	// Logger defaults to slog.Default().
	Logger *slog.Logger
}

// Manager runs backups inside the daemon and keeps the ones it wrote: one at
// a time, asynchronously, each a file that is complete whenever its record
// says ready.
type Manager struct {
	opts ManagerOptions
	// ctx ends when the Manager is closed, and the running backup with it.
	ctx  context.Context
	stop context.CancelFunc
	// create is Create; tests replace it to hold a backup running.
	create func(ctx context.Context, opts Options) (string, error)

	mu   sync.Mutex
	recs map[string]Record
	job  *job // the running backup, or nil
	// restoring is set once a restore is staged: the data is about to be
	// replaced, so nothing new starts.
	restoring bool
	wg        sync.WaitGroup
}

// job is the one running backup.
type job struct {
	id     string
	cancel context.CancelFunc
	done   chan struct{}
	// deleted is set, under Manager.mu, when the record was deleted while
	// the backup ran: the outcome is then nobody's.
	deleted bool
}

// NewManager opens the managed directory and settles what a previous daemon
// left in it: a backup that was still running is failed, and partial files
// are removed. Close it before the store.
func NewManager(opts ManagerOptions) (*Manager, error) {
	if opts.Dir == "" {
		return nil, errors.New("backup: no directory for managed backups")
	}
	if opts.Events == nil {
		opts.Events = event.Discard
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Every <= 0 {
		opts.Every = DefaultEvery
	}
	if err := os.MkdirAll(opts.Dir, 0o700); err != nil {
		return nil, fmt.Errorf("backup: %w", err)
	}
	ctx, stop := context.WithCancel(context.Background())
	m := &Manager{opts: opts, ctx: ctx, stop: stop, create: Create, recs: map[string]Record{}}
	if err := m.recover(); err != nil {
		stop()
		return nil, err
	}
	return m, nil
}

// recover loads the records on disk. Nothing is running yet, so every staged
// file is a leftover and every running record was interrupted.
func (m *Manager) recover() error {
	entries, err := os.ReadDir(m.opts.Dir)
	if err != nil {
		return fmt.Errorf("backup: %w", err)
	}
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, tmpPrefix) {
			_ = os.Remove(filepath.Join(m.opts.Dir, name))
			continue
		}
		id, ok := strings.CutSuffix(name, recordExt)
		if !ok || !event.ID(id).Valid() {
			continue
		}
		body, err := os.ReadFile(filepath.Join(m.opts.Dir, name))
		if err != nil {
			return fmt.Errorf("backup: %w", err)
		}
		var rec Record
		if err := json.Unmarshal(body, &rec); err != nil || rec.ID != id {
			m.opts.Logger.Warn("backup: unreadable record skipped", "file", name)
			continue
		}
		if rec.Origin == "" {
			rec.Origin = OriginManual
		}
		changed := false
		switch rec.Status {
		case StatusRunning:
			_ = os.Remove(m.archive(id))
			rec.Status, rec.Reason, changed = StatusFailed, reasonRestart, true
		case StatusReady:
			if _, err := os.Stat(m.archive(id)); err != nil {
				rec.Status, rec.Reason, rec.Size, changed = StatusFailed, reasonMissing, 0, true
			}
		}
		if changed {
			if err := m.save(rec); err != nil {
				return err
			}
		}
		m.recs[id] = rec
	}
	return nil
}

const (
	recordExt  = ".json"
	archiveExt = ".tar.gz"
)

func (m *Manager) archive(id string) string { return filepath.Join(m.opts.Dir, id+archiveExt) }
func (m *Manager) record(id string) string  { return filepath.Join(m.opts.Dir, id+recordExt) }

// save writes a record through a temp file, so a crash never leaves half a
// record behind.
func (m *Manager) save(rec Record) error {
	body, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return fmt.Errorf("backup: %w", err)
	}
	tmp := filepath.Join(m.opts.Dir, tmpPrefix+rec.ID+recordExt)
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("backup: %w", err)
	}
	if err := os.Rename(tmp, m.record(rec.ID)); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("backup: %w", err)
	}
	return nil
}

// Start begins a manual backup and returns its record at once, still
// running. A backup already running refuses with ErrBusy. by is who asked,
// for the trace.
func (m *Manager) Start(includeSecrets bool, by string) (Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.start(OriginManual, includeSecrets, by)
}

// Automatic starts the daemon's own backup when one is due: automatic
// backups are on, and none was taken (or is being taken) in the last
// ManagerOptions.Every. A failed one does not count, so the next call tries
// again.
// It never includes secrets. A backup already running is not an error: the
// next call finds out whether one is still due.
func (m *Manager) Automatic(now time.Time) (Record, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.opts.Keep <= 0 {
		return Record{}, false, nil
	}
	for _, r := range m.recs {
		if r.Origin == OriginAutomatic && r.Status != StatusFailed && now.Sub(r.Created) < m.opts.Every {
			return Record{}, false, nil
		}
	}
	rec, err := m.start(OriginAutomatic, false, bySchedule)
	if errors.Is(err, ErrBusy) {
		return Record{}, false, nil
	}
	return rec, err == nil, err
}

// start is Start for any origin; the caller holds m.mu.
func (m *Manager) start(origin Origin, includeSecrets bool, by string) (Record, error) {
	if m.job != nil || m.restoring {
		return Record{}, ErrBusy
	}
	if err := m.ctx.Err(); err != nil {
		return Record{}, fmt.Errorf("backup: %w", err)
	}
	now := time.Now().UTC()
	rec := Record{
		ID: string(event.NewIDAt(now)), Created: now, Version: m.opts.Version, Origin: origin,
		SecretsIncluded: includeSecrets, Status: StatusRunning,
	}
	if err := m.save(rec); err != nil {
		return Record{}, err
	}
	ctx, cancel := context.WithCancel(m.ctx)
	j := &job{id: rec.ID, cancel: cancel, done: make(chan struct{})}
	m.job = j
	m.recs[rec.ID] = rec
	m.wg.Add(1)
	go m.run(ctx, j, rec, by)
	return rec, nil
}

// run writes the archive of one job and records how it ended.
func (m *Manager) run(ctx context.Context, j *job, rec Record, by string) {
	defer m.wg.Done()
	defer close(j.done)
	defer j.cancel()

	out := m.archive(rec.ID)
	_, err := m.create(ctx, Options{
		DataDir: m.opts.DataDir, ConfigDir: m.opts.ConfigDir, SecretsPath: m.opts.SecretsPath,
		Out: out, IncludeSecrets: rec.SecretsIncluded, DB: m.opts.DB, TmpDir: m.opts.Dir,
	})
	if err == nil {
		var info os.FileInfo
		if info, err = os.Stat(out); err == nil {
			rec.Status, rec.Size = StatusReady, info.Size()
		}
	}
	if err != nil {
		_ = os.Remove(out)
		rec.Status, rec.Reason = StatusFailed, err.Error()
		if m.ctx.Err() != nil {
			rec.Reason = reasonShutdown
		}
	}

	m.mu.Lock()
	m.job = nil
	if j.deleted {
		// Delete owns the record now; the archive, if it made it, goes too.
		m.mu.Unlock()
		_ = os.Remove(out)
		return
	}
	m.recs[rec.ID] = rec
	if serr := m.save(rec); serr != nil {
		m.opts.Logger.Error("backup: save record", "id", rec.ID, "error", serr)
	}
	m.mu.Unlock()

	attrs := map[string]any{
		"backup_id": rec.ID, "origin": string(rec.Origin), "secrets_included": rec.SecretsIncluded, "by": by,
	}
	if rec.Status == StatusReady {
		attrs["size"] = rec.Size
		m.emit(event.BackupCreated, attrs)
	} else {
		attrs["reason"] = rec.Reason
		m.opts.Logger.Warn("backup failed", "id", rec.ID, "reason", rec.Reason)
		m.emit(event.BackupFailed, attrs)
	}
	if rec.Origin == OriginAutomatic {
		m.prune()
	}
}

// prune applies retention to the automatic backups: the newest Keep ready
// ones stay, and of the failed ones only the latest, so a run of failures
// neither piles up nor pushes a good backup out. Manual backups and uploads
// are never touched.
func (m *Manager) prune() {
	m.mu.Lock()
	var auto []Record
	for _, r := range m.recs {
		if r.Origin == OriginAutomatic && r.Status != StatusRunning {
			auto = append(auto, r)
		}
	}
	sort.Slice(auto, func(i, j int) bool { return auto[i].ID > auto[j].ID })
	var victims []Record
	ready, failed := 0, 0
	for _, r := range auto {
		if r.Status == StatusReady {
			if ready++; ready > m.opts.Keep {
				victims = append(victims, r)
			}
		} else if failed++; failed > 1 {
			victims = append(victims, r)
		}
	}
	for _, r := range victims {
		delete(m.recs, r.ID)
	}
	m.mu.Unlock()

	for _, r := range victims {
		if err := m.remove(r, byRetention); err != nil {
			m.opts.Logger.Warn("backup: retention", "id", r.ID, "error", err)
		}
	}
}

// remove deletes the files of a record already taken out of the list.
func (m *Manager) remove(rec Record, by string) error {
	var err error
	for _, p := range []string{m.archive(rec.ID), m.record(rec.ID)} {
		if rerr := os.Remove(p); rerr != nil && !errors.Is(rerr, os.ErrNotExist) && err == nil {
			err = fmt.Errorf("backup: %w", rerr)
		}
	}
	m.emit(event.BackupDeleted, map[string]any{
		"backup_id": rec.ID, "origin": string(rec.Origin), "status": string(rec.Status), "by": by,
	})
	return err
}

// Import keeps an archive a client sent as a managed backup. The stream is
// checked against its own manifest as it is written, and nothing is listed
// unless it matches; a stream that does not is ErrBadArchive.
func (m *Manager) Import(r io.Reader, by string) (Record, error) {
	tmp, err := os.CreateTemp(m.opts.Dir, tmpPrefix+"upload-*")
	if err != nil {
		return Record{}, fmt.Errorf("backup: %w", err)
	}
	tmpName := tmp.Name()
	done := false
	defer func() {
		_ = tmp.Close()
		if !done {
			_ = os.Remove(tmpName)
		}
	}()
	// One pass: the bytes go to disk while the reader checks them. Whatever
	// follows the tar stream is drained too, so the file is the whole upload.
	tee := io.TeeReader(r, tmp)
	manifest, verr := readArchive(tee, "the upload", nil)
	if _, err := io.Copy(io.Discard, tee); err != nil {
		return Record{}, err // the transport's error: too large, cut short
	}
	if verr != nil {
		return Record{}, fmt.Errorf("%w: %w", ErrBadArchive, verr)
	}
	if err := tmp.Sync(); err != nil {
		return Record{}, fmt.Errorf("backup: %w", err)
	}
	info, err := tmp.Stat()
	if err != nil {
		return Record{}, fmt.Errorf("backup: %w", err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now().UTC()
	rec := Record{
		ID: string(event.NewIDAt(now)), Created: now, Size: info.Size(), Origin: OriginUpload,
		SecretsIncluded: manifest.SecretsIncluded, Status: StatusReady,
	}
	if err := os.Rename(tmpName, m.archive(rec.ID)); err != nil {
		return Record{}, fmt.Errorf("backup: %w", err)
	}
	done = true
	if err := m.save(rec); err != nil {
		_ = os.Remove(m.archive(rec.ID))
		return Record{}, err
	}
	m.recs[rec.ID] = rec
	m.emit(event.BackupCreated, map[string]any{
		"backup_id": rec.ID, "origin": string(rec.Origin), "secrets_included": rec.SecretsIncluded,
		"size": rec.Size, "by": by,
	})
	return rec, nil
}

// emit records a backup event. It outlives the Manager's own context: the
// trace of a backup cut short by a shutdown is still written.
func (m *Manager) emit(t event.Type, attrs map[string]any) {
	m.opts.Events.Emit(context.WithoutCancel(m.ctx), event.New(t, "", "", "", attrs))
}

// List returns every record, newest first.
func (m *Manager) List() []Record {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Record, 0, len(m.recs))
	for _, r := range m.recs {
		out = append(out, r)
	}
	// Ids are ULIDs: their order is the order the backups were asked for.
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	return out
}

// Get returns one record, or ErrNotFound.
func (m *Manager) Get(id string) (Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec, ok := m.recs[id]
	if !ok {
		return Record{}, ErrNotFound
	}
	return rec, nil
}

// Open returns the archive of a ready backup for reading; the caller closes
// it. A running or failed backup is ErrNotReady.
func (m *Manager) Open(id string) (*os.File, Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec, ok := m.recs[id]
	switch {
	case !ok:
		return nil, Record{}, ErrNotFound
	case rec.Status != StatusReady:
		return nil, rec, ErrNotReady
	}
	// Opened under the lock: a Delete that follows unlinks the name, and this
	// reader keeps the whole file.
	f, err := os.Open(m.archive(id))
	if err != nil {
		return nil, rec, fmt.Errorf("backup: %w", err)
	}
	return f, rec, nil
}

// Verify re-checks a ready backup's archive against its manifest. The error
// of a damaged archive is the one Verify returns; ErrNotFound and ErrNotReady
// mean there was nothing to check.
func (m *Manager) Verify(id string) error {
	f, _, err := m.Open(id)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	return verify(f, id+archiveExt)
}

// Delete removes a record and its archive. Deleting the running backup
// cancels it first.
func (m *Manager) Delete(id, by string) error {
	m.mu.Lock()
	rec, ok := m.recs[id]
	if !ok {
		m.mu.Unlock()
		return ErrNotFound
	}
	// Gone from the list at once, so a second Delete is a not found.
	delete(m.recs, id)
	var running *job
	if m.job != nil && m.job.id == id {
		running = m.job
		running.deleted = true
		running.cancel()
	}
	m.mu.Unlock()
	if running != nil {
		<-running.done
	}
	return m.remove(rec, by)
}

// Close stops the running backup, if any, which is then recorded as failed,
// and waits for it. No backup starts after it.
func (m *Manager) Close() {
	m.stop()
	m.wg.Wait()
}
