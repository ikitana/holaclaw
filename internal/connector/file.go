package connector

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/pelletier/go-toml/v2"

	"holaclaw/internal/config"
)

// connectorsFile is connectors.toml in the config directory. The daemon
// writes it whole: under an exclusive flock on a sibling lock file it
// re-reads the file, changes one entry, writes a temp file, fsyncs and
// renames it (docs/connectors.md §4). Comments are not preserved; every
// other entry keeps its keys, `trusted` included. An edit that changes
// nothing writes nothing. Every rewrite leaves the file mode 0600, whatever
// it was before, and a file with no version gets the current one.
type connectorsFile struct {
	Version    int64                     `toml:"version"`
	Connectors map[string]map[string]any `toml:"connectors"`
}

const fileHeader = "# holaclaw connectors.toml — written by the daemon over the API, or by hand.\n" +
	"# Machine file: comments and formatting are not preserved on rewrite.\n\n"

func (m *Manager) filePath() string { return filepath.Join(m.o.Dir, config.FileConnectors) }

// writeEntry sets cfg's entry: url, auth, scopes and client_id, and
// allow_insecure_transport when cfg has it. Other keys are kept.
func (m *Manager) writeEntry(cfg config.Connector) error {
	return m.editFile(func(f *connectorsFile) bool {
		e := f.Connectors[cfg.ID]
		if e == nil {
			e = map[string]any{}
			f.Connectors[cfg.ID] = e
		}
		e["url"], e["auth"] = cfg.URL, cfg.Auth
		delete(e, "scopes")
		if cfg.Scopes != nil {
			e["scopes"] = cfg.Scopes
		}
		delete(e, "client_id")
		if cfg.ClientID != "" {
			e["client_id"] = cfg.ClientID
		}
		if cfg.AllowInsecureTransport {
			e["allow_insecure_transport"] = true
		}
		return true
	})
}

// removeEntry deletes id's entry; a missing file or entry is no error and
// leaves the file as it is.
func (m *Manager) removeEntry(id string) error {
	return m.editFile(func(f *connectorsFile) bool {
		if _, ok := f.Connectors[id]; !ok {
			return false
		}
		delete(f.Connectors, id)
		return true
	})
}

// hasEntry reports whether connectors.toml on disk names id.
func (m *Manager) hasEntry(id string) (bool, error) {
	m.fileMu.Lock()
	defer m.fileMu.Unlock()
	f, err := m.readFile()
	if err != nil {
		return false, err
	}
	_, ok := f.Connectors[id]
	return ok, nil
}

// editFile re-reads the file under both locks and writes it back when apply
// reports a change. A symlink is read but never written through: the user
// manages that file (dotfiles, home-manager), so it is edited by hand.
func (m *Manager) editFile(apply func(*connectorsFile) bool) error {
	m.fileMu.Lock()
	defer m.fileMu.Unlock()
	path := m.filePath()
	unlock, err := flock(path + ".lock")
	if err != nil {
		return err
	}
	defer unlock()
	if info, err := os.Lstat(path); err == nil {
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			return ErrHandManaged
		case !info.Mode().IsRegular():
			return fmt.Errorf("connector: %s is not a regular file", config.FileConnectors)
		}
	}
	f, err := m.readFile()
	if err != nil {
		return err
	}
	if !apply(f) {
		return nil
	}
	body, err := toml.Marshal(f)
	if err != nil {
		return fmt.Errorf("connector: encode %s: %w", config.FileConnectors, err)
	}
	return writeAtomic(path, append([]byte(fileHeader), body...))
}

// readFile parses connectors.toml; a missing file is an empty one. A file
// that does not parse, or has a top-level key config would refuse, is an
// error, never overwritten.
func (m *Manager) readFile() (*connectorsFile, error) {
	f := &connectorsFile{Version: config.CurrentVersion, Connectors: map[string]map[string]any{}}
	b, err := os.ReadFile(m.filePath())
	if errors.Is(err, os.ErrNotExist) {
		return f, nil
	}
	if err != nil {
		return nil, fmt.Errorf("connector: %w", err)
	}
	if err := toml.NewDecoder(bytes.NewReader(b)).DisallowUnknownFields().Decode(f); err != nil {
		return nil, ErrBadFile // the decoder's text may quote the file
	}
	if f.Connectors == nil {
		f.Connectors = map[string]map[string]any{}
	}
	return f, nil
}

// writeAtomic swaps body in: temp file in the same directory, fsync,
// rename, fsync of the directory. A failure leaves no temp file. Once the
// rename is done the file is written, so a failed directory fsync is no
// error: callers must not undo a write that readers already see.
func writeAtomic(path string, body []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".connectors-*.toml")
	if err != nil {
		return fmt.Errorf("connector: %w", err)
	}
	done := false
	defer func() {
		_ = tmp.Close()
		if !done {
			_ = os.Remove(tmp.Name())
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return fmt.Errorf("connector: %w", err)
	}
	if _, err := tmp.Write(body); err != nil {
		return fmt.Errorf("connector: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("connector: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("connector: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("connector: %w", err)
	}
	done = true
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// flock takes an exclusive advisory lock on path, for other processes
// writing the file the same way. Linux only, like the daemon.
func flock(path string) (func(), error) {
	lf, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("connector: lock: %w", err)
	}
	if err := syscall.Flock(int(lf.Fd()), syscall.LOCK_EX); err != nil {
		_ = lf.Close()
		return nil, fmt.Errorf("connector: lock: %w", err)
	}
	return func() {
		_ = syscall.Flock(int(lf.Fd()), syscall.LOCK_UN)
		_ = lf.Close()
	}, nil
}
