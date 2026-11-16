package whatsapp

import (
	"database/sql"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waAdv"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"

	"holaclaw/internal/channel"
	"holaclaw/internal/event"
)

func TestContract(t *testing.T) {
	a := New(Options{})
	if a.Name() != "whatsapp" || a.Type() != "whatsapp" {
		t.Fatalf("name, type = %q, %q", a.Name(), a.Type())
	}
	want := channel.Limits{
		TextChars: 4000, CaptionChars: 1024, MaxFileBytes: 100 << 20,
		Kinds: []event.Kind{event.KindImage, event.KindVideo, event.KindAudio, event.KindDocument},
	}
	if got := a.Limits(); !reflect.DeepEqual(got, want) {
		t.Fatalf("limits = %+v, want %+v", got, want)
	}
	if got := a.AliasNamespaces(); !reflect.DeepEqual(got, []string{"wa:", "wa:lid:"}) {
		t.Fatalf("namespaces = %v", got)
	}
	if got := a.LinkState(); got.Status != channel.LinkNone {
		t.Fatalf("initial link state = %+v", got)
	}
}

func TestSessionFilePermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "whatsapp", "wa.db")
	if err := prepareFile(path); err != nil {
		t.Fatal(err)
	}
	mode := func(p string) os.FileMode {
		st, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		return st.Mode().Perm()
	}
	if m := mode(filepath.Dir(path)); m != 0o700 {
		t.Fatalf("directory mode = %o, want 700", m)
	}
	if m := mode(path); m != 0o600 {
		t.Fatalf("file mode = %o, want 600", m)
	}
	// A file or directory left too open is tightened.
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := prepareFile(path); err != nil {
		t.Fatal(err)
	}
	if mode(path) != 0o600 || mode(filepath.Dir(path)) != 0o700 {
		t.Fatalf("modes after reopen = %o, %o", mode(path), mode(filepath.Dir(path)))
	}

	if err := removeSession(path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("session file after remove: %v", err)
	}
	if err := removeSession(path); err != nil {
		t.Fatalf("removing a missing session: %v", err)
	}
}

// TestRealSessionStore opens whatsmeow's sqlstore on modernc.org/sqlite, the
// way the daemon does: its migrations run, a device saved in one session is
// read back by the next, and the file stays 0600. Run with CGO_ENABLED=0 to
// prove the dialect needs no CGO.
func TestRealSessionStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "whatsapp", "wa.db")
	c, err := openSession(t.Context(), path, nil)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if pn, _ := c.OwnID(); !pn.IsEmpty() {
		t.Fatalf("a new store holds device %s", pn)
	}
	rc := c.(*realClient)
	if rc.cli.EnableAutoReconnect || !rc.cli.SynchronousAck || !rc.cli.EnableDecryptedEventBuffer {
		t.Fatal("client flags: want auto-reconnect off, synchronous ack and the decrypted event buffer on")
	}
	if os := store.DeviceProps.GetOs(); os != "holaclaw" {
		t.Fatalf("device name = %q, want holaclaw", os)
	}
	jid := types.NewJID(number, types.DefaultUserServer)
	rc.cli.Store.ID = &jid
	rc.cli.Store.Account = &waAdv.ADVSignedDeviceIdentity{Details: []byte{1}, AccountSignature: make([]byte, 64), AccountSignatureKey: make([]byte, 32), DeviceSignature: make([]byte, 64)}
	if err := rc.cli.Store.Save(t.Context()); err != nil {
		t.Fatalf("save device: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	c, err = openSession(t.Context(), path, nil)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if pn, _ := c.OwnID(); pn.User != number {
		t.Fatalf("reopened device = %v, want %s", pn, number)
	}
	if err := c.DeleteLocalDevice(t.Context()); err != nil {
		t.Fatalf("delete device: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("session file: %v, %v", st, err)
	}
}

// TestSessionStoreWaitsForTheWriter reproduces the decrypt that failed with
// SQLITE_BUSY: one transaction reads, then writes, while another connection
// holds the write lock. Begun deferred, as before, the upgrade fails at once
// whatever busy_timeout says; begun IMMEDIATE it waits and succeeds.
func TestSessionStoreWaitsForTheWriter(t *testing.T) {
	ctx := t.Context()
	open := func(t *testing.T, dsn string) *sql.DB {
		t.Helper()
		db, err := sql.Open("sqlite", dsn)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		if _, err := db.ExecContext(ctx, `CREATE TABLE identities (addr TEXT PRIMARY KEY)`); err != nil {
			t.Fatal(err)
		}
		return db
	}
	// writer begins a transaction that holds the write lock, as a send does.
	writer := func(t *testing.T, db *sql.DB) *sql.Tx {
		t.Helper()
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO identities VALUES ('send')`); err != nil {
			t.Fatal(err)
		}
		return tx
	}
	// decrypt reads, then writes, in one transaction.
	decrypt := func(db *sql.DB) error {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM identities`).Scan(&n); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO identities VALUES ('decrypt')`); err != nil {
			return err
		}
		return tx.Commit()
	}

	t.Run("deferred fails", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "wa.db")
		db := open(t, (&url.URL{Scheme: "file", Path: path, RawQuery: "_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)"}).String())
		w := writer(t, db)
		defer func() { _ = w.Rollback() }()
		start := time.Now()
		err := decrypt(db)
		if err == nil || !strings.Contains(err.Error(), "SQLITE_BUSY") || time.Since(start) > time.Second {
			t.Fatalf("deferred upgrade: %v after %v, want SQLITE_BUSY at once", err, time.Since(start))
		}
	})

	t.Run("immediate waits", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "wa.db")
		if err := prepareFile(path); err != nil {
			t.Fatal(err)
		}
		db := open(t, sessionDSN(path))
		var mode string
		if err := db.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&mode); err != nil || mode != "wal" {
			t.Fatalf("journal mode = %q, %v", mode, err)
		}
		w := writer(t, db)
		done := make(chan error, 1)
		go func() { done <- decrypt(db) }()
		select {
		case err := <-done:
			t.Fatalf("the decrypt did not wait for the writer: %v", err)
		case <-time.After(100 * time.Millisecond):
		}
		if err := w.Commit(); err != nil {
			t.Fatal(err)
		}
		if err := <-done; err != nil {
			t.Fatalf("decrypt after the writer: %v", err)
		}
		var n int
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM identities`).Scan(&n); err != nil || n != 2 {
			t.Fatalf("rows = %d, %v", n, err)
		}
	})
}

// TestSessionSideFiles: in WAL mode the store keeps -wal and -shm next to
// the database while it is open. They hold key material, so they are 0600
// like the database, and logout removes them.
func TestSessionSideFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "whatsapp", "wa.db")
	c, err := openSession(t.Context(), path, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		st, err := os.Stat(p)
		if err != nil {
			t.Fatalf("%s: %v", filepath.Base(p), err)
		}
		if m := st.Mode().Perm(); m != 0o600 {
			t.Fatalf("%s mode = %o, want 600", filepath.Base(p), m)
		}
	}
	// Logout closes the client and its container before removing the
	// files, the real order: nobody holds them open when removeSession
	// runs, the same as a logout outside a test.
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if err := removeSession(path); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s after remove: %v", filepath.Base(p), err)
		}
	}
}
