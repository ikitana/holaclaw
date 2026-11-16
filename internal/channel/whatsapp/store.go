package whatsapp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"sync"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"

	// whatsmeow reads the "sqlite" driver name as its SQLite dialect, so the
	// session store needs no CGO (§5.2).
	_ "modernc.org/sqlite"
)

// osName sets the device name the phone lists, once per process: whatsmeow
// keeps it in a package variable.
var osName sync.Once

// openSession opens the session database at path (directory 0700, file 0600)
// and a client on its device, a fresh one when the store has none. Auto
// reconnect is off: the adapter owns that policy. Acks wait for the handler,
// and decrypted messages are buffered until then, so a message is acked only
// once Inbound stored it (§5.1).
func openSession(ctx context.Context, path string, log *slog.Logger) (waClient, error) {
	if err := prepareFile(path); err != nil {
		return nil, err
	}
	osName.Do(func() {
		name := "holaclaw"
		store.DeviceProps.Os = &name
	})
	wl := newLogBridge(log, "whatsmeow")
	container, err := sqlstore.New(ctx, "sqlite", sessionDSN(path), wl.Sub("Database"))
	if err != nil {
		return nil, fmt.Errorf("whatsapp: open session store: %w", err)
	}
	device, err := container.GetFirstDevice(ctx)
	if err != nil {
		_ = container.Close()
		return nil, fmt.Errorf("whatsapp: read session store: %w", err)
	}
	cli := whatsmeow.NewClient(device, wl.Sub("Client"))
	cli.EnableAutoReconnect = false
	cli.SynchronousAck = true
	cli.EnableDecryptedEventBuffer = true
	return &realClient{cli: cli, container: container}, nil
}

// sessionDSN is the SQLite URI of the session file. SQLite decodes a "file:"
// URI, so the path is escaped: a '?', '#' or '%' in an instance name must
// name a file, not start a query. whatsmeow refuses to migrate without
// foreign keys.
//
// Every transaction begins IMMEDIATE: a decrypt is one transaction that
// reads and then writes, and a deferred one that has to upgrade while
// another connection writes (an outbound send encrypting) gets SQLITE_BUSY
// at once, without waiting on busy_timeout. WAL keeps readers from blocking
// that writer. synchronous stays FULL: a committed ratchet step must
// survive power loss.
func sessionDSN(path string) string {
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(path),
		RawQuery: "_pragma=foreign_keys(1)" + // whatsmeow refuses to migrate without them
			"&_pragma=busy_timeout(5000)" + // give a blocked writer time instead of failing it immediately
			"&_pragma=journal_mode(WAL)" + // readers never block the one writer
			"&_txlock=immediate", // claim the write lock up front, so busy_timeout applies on an upgrade
	}
	return u.String()
}

// prepareFile makes the session directory 0700 and the file 0600 before
// SQLite opens it, so the keys are never readable by anyone else, not even
// for a moment. SQLite creates the -wal and -shm files with the database's
// own mode, so they are 0600 too.
func prepareFile(path string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("whatsapp: session directory: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("whatsapp: session directory: %w", err)
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("whatsapp: session file: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("whatsapp: session file: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("whatsapp: session file: %w", err)
	}
	return nil
}

// removeSession deletes the session database and SQLite's side files.
func removeSession(path string) error {
	var errs []error
	for _, p := range []string{path, path + "-journal", path + "-wal", path + "-shm"} {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
