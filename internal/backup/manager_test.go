package backup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"holaclaw/internal/event"
)

// recorder keeps the events a Manager emitted.
type recorder struct {
	mu     sync.Mutex
	events []event.Event
}

func (r *recorder) Emit(_ context.Context, e event.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}

// all is a snapshot: the manager emits from its own goroutine.
func (r *recorder) all() []event.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]event.Event(nil), r.events...)
}

func (r *recorder) types() []event.Type {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]event.Type, 0, len(r.events))
	for _, e := range r.events {
		out = append(out, e.Type)
	}
	return out
}

// managed is a Manager over the package fixture, its directory inside the
// data directory as the daemon has it.
type managed struct {
	*Manager
	dir    string
	events *recorder
	opts   ManagerOptions
}

func newManaged(t *testing.T) *managed {
	t.Helper()
	configDir, dataDir, db := fixture(t)
	rec := &recorder{}
	opts := ManagerOptions{
		Dir: filepath.Join(dataDir, "backups"), DataDir: dataDir, ConfigDir: configDir,
		SecretsPath: filepath.Join(configDir, "secrets", "auth.toml"),
		DB:          db.DB(), Version: "test", Events: rec,
	}
	m, err := NewManager(opts)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	t.Cleanup(func() {
		m.Close()
		_ = db.Close()
	})
	return &managed{Manager: m, dir: opts.Dir, events: rec, opts: opts}
}

// settle waits for a backup to leave running.
func (m *managed) settle(t *testing.T, id string) Record {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		rec, err := m.Get(id)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if rec.Status != StatusRunning {
			// The record is saved before the job emits its event; wait for
			// the job to finish so callers can check events.
			m.wg.Wait()
			return rec
		}
		if time.Now().After(deadline) {
			t.Fatal("backup is still running")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// hold replaces the archive writer with one that stages a partial file and
// then blocks until released or cancelled.
func (m *managed) hold() (started <-chan struct{}, release func()) {
	s, r := make(chan struct{}), make(chan struct{})
	var once sync.Once
	m.create = func(ctx context.Context, opts Options) (string, error) {
		partial := filepath.Join(filepath.Dir(opts.Out), tmpPrefix+"held")
		if err := os.WriteFile(partial, []byte("half an archive"), 0o600); err != nil {
			return "", err
		}
		defer func() { _ = os.Remove(partial) }()
		once.Do(func() { close(s) })
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-r:
			return Create(ctx, opts)
		}
	}
	return s, func() { close(r) }
}

// files lists the managed directory.
func (m *managed) files(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(m.dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

func TestManagerCreatesAReadyBackup(t *testing.T) {
	m := newManaged(t)
	rec, err := m.Start(false, "admin")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if rec.Status != StatusRunning || !event.ID(rec.ID).Valid() || rec.Version != "test" {
		t.Fatalf("started record = %+v", rec)
	}
	rec = m.settle(t, rec.ID)
	if rec.Status != StatusReady || rec.Size == 0 || rec.Reason != "" {
		t.Fatalf("settled record = %+v", rec)
	}

	path := filepath.Join(m.dir, rec.ID+".tar.gz")
	if err := Verify(path); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if info, err := os.Stat(path); err != nil || info.Size() != rec.Size || info.Mode().Perm() != 0o600 {
		t.Fatalf("archive: %v, %v", info, err)
	}
	if err := m.Verify(rec.ID); err != nil {
		t.Fatalf("Manager.Verify: %v", err)
	}
	if strings.Contains(string(rawGunzip(t, path)), secretMarker) {
		t.Fatal("auth.toml is in a backup that did not ask for it")
	}
	if got := m.files(t); len(got) != 2 {
		t.Fatalf("managed directory holds %v, want one archive and one record", got)
	}

	f, got, err := m.Open(rec.ID)
	if err != nil || got != rec {
		t.Fatalf("Open: %+v, %v", got, err)
	}
	_ = f.Close()
	if types := m.events.types(); len(types) != 1 || types[0] != event.BackupCreated {
		t.Fatalf("events = %v", types)
	}
}

func TestManagerIncludesSecretsWhenAsked(t *testing.T) {
	m := newManaged(t)
	rec, err := m.Start(true, "admin")
	if err != nil {
		t.Fatal(err)
	}
	rec = m.settle(t, rec.ID)
	if rec.Status != StatusReady || !rec.SecretsIncluded {
		t.Fatalf("record = %+v", rec)
	}
	if !strings.Contains(string(rawGunzip(t, filepath.Join(m.dir, rec.ID+".tar.gz"))), secretMarker) {
		t.Fatal("auth.toml is missing from a backup that asked for it")
	}
	for _, e := range m.events.all() {
		if strings.Contains(string(e.Attrs), secretMarker) {
			t.Fatal("an event carries file contents")
		}
	}
}

// A backup holds the database and the blobs, never the backups before it.
func TestManagerNeverArchivesEarlierBackups(t *testing.T) {
	m := newManaged(t)
	first, err := m.Start(false, "admin")
	if err != nil {
		t.Fatal(err)
	}
	m.settle(t, first.ID)
	second, err := m.Start(false, "admin")
	if err != nil {
		t.Fatal(err)
	}
	m.settle(t, second.ID)
	for name := range entries(t, filepath.Join(m.dir, second.ID+".tar.gz")) {
		if strings.Contains(name, "backups") || strings.Contains(name, first.ID) {
			t.Fatalf("the second backup archived %q", name)
		}
	}
	if list := m.List(); len(list) != 2 || list[0].ID != second.ID || list[1].ID != first.ID {
		t.Fatalf("List = %+v, want newest first", list)
	}
}

func TestManagerRunsOneBackupAtATime(t *testing.T) {
	m := newManaged(t)
	started, release := m.hold()
	first, err := m.Start(false, "admin")
	if err != nil {
		t.Fatal(err)
	}
	<-started
	if _, err := m.Start(false, "admin"); !errors.Is(err, ErrBusy) {
		t.Fatalf("second Start = %v, want ErrBusy", err)
	}
	if _, _, err := m.Open(first.ID); !errors.Is(err, ErrNotReady) {
		t.Fatalf("Open of a running backup = %v, want ErrNotReady", err)
	}
	if err := m.Verify(first.ID); !errors.Is(err, ErrNotReady) {
		t.Fatalf("Verify of a running backup = %v, want ErrNotReady", err)
	}
	release()
	if rec := m.settle(t, first.ID); rec.Status != StatusReady {
		t.Fatalf("record = %+v", rec)
	}
	next, err := m.Start(false, "admin")
	if err != nil {
		t.Fatalf("Start after the first finished: %v", err)
	}
	m.settle(t, next.ID)
}

func TestManagerFailureLeavesNoPartialFile(t *testing.T) {
	m := newManaged(t)
	m.create = func(ctx context.Context, opts Options) (string, error) {
		// A disk that fills halfway: the real writer, on a file that fails.
		opts.Out = filepath.Join(opts.Out, "not-a-directory", "x.tar.gz")
		if err := os.WriteFile(filepath.Dir(filepath.Dir(opts.Out)), []byte("in the way"), 0o600); err != nil {
			return "", err
		}
		return Create(ctx, opts)
	}
	rec, err := m.Start(false, "admin")
	if err != nil {
		t.Fatal(err)
	}
	rec = m.settle(t, rec.ID)
	if rec.Status != StatusFailed || rec.Reason == "" || rec.Size != 0 {
		t.Fatalf("record = %+v", rec)
	}
	if got := m.files(t); len(got) != 1 || got[0] != rec.ID+".json" {
		t.Fatalf("managed directory holds %v, want only the failed record", got)
	}
	if _, _, err := m.Open(rec.ID); !errors.Is(err, ErrNotReady) {
		t.Fatalf("Open of a failed backup = %v, want ErrNotReady", err)
	}
	if types := m.events.types(); len(types) != 1 || types[0] != event.BackupFailed {
		t.Fatalf("events = %v", types)
	}
	// A failure does not block the next backup.
	m.create = Create
	next, err := m.Start(false, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if got := m.settle(t, next.ID); got.Status != StatusReady {
		t.Fatalf("next record = %+v", got)
	}
}

// A snapshot that cannot be taken fails the backup the same way.
func TestManagerFailsOnAClosedDatabase(t *testing.T) {
	m := newManaged(t)
	if err := m.opts.DB.Close(); err != nil {
		t.Fatal(err)
	}
	rec, err := m.Start(false, "admin")
	if err != nil {
		t.Fatal(err)
	}
	rec = m.settle(t, rec.ID)
	if rec.Status != StatusFailed || !strings.Contains(rec.Reason, "VACUUM INTO") {
		t.Fatalf("record = %+v", rec)
	}
	if got := m.files(t); len(got) != 1 {
		t.Fatalf("managed directory holds %v", got)
	}
}

func TestManagerDelete(t *testing.T) {
	m := newManaged(t)
	rec, err := m.Start(false, "admin")
	if err != nil {
		t.Fatal(err)
	}
	m.settle(t, rec.ID)
	if err := m.Delete(rec.ID, "admin"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if got := m.files(t); len(got) != 0 {
		t.Fatalf("managed directory holds %v after a delete", got)
	}
	if _, err := m.Get(rec.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after delete = %v", err)
	}
	if err := m.Delete(rec.ID, "admin"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second Delete = %v, want ErrNotFound", err)
	}
	for _, id := range []string{"", "nope", "../" + rec.ID, "..%2f"} {
		if _, err := m.Get(id); !errors.Is(err, ErrNotFound) {
			t.Errorf("Get(%q) = %v, want ErrNotFound", id, err)
		}
		if _, _, err := m.Open(id); !errors.Is(err, ErrNotFound) {
			t.Errorf("Open(%q) = %v, want ErrNotFound", id, err)
		}
	}
	if types := m.events.types(); len(types) != 2 || types[1] != event.BackupDeleted {
		t.Fatalf("events = %v", types)
	}
}

func TestManagerDeleteCancelsARunningBackup(t *testing.T) {
	m := newManaged(t)
	started, _ := m.hold()
	rec, err := m.Start(false, "admin")
	if err != nil {
		t.Fatal(err)
	}
	<-started
	if err := m.Delete(rec.ID, "admin"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if got := m.files(t); len(got) != 0 {
		t.Fatalf("managed directory holds %v after cancelling", got)
	}
	if len(m.List()) != 0 {
		t.Fatalf("List = %+v", m.List())
	}
	// Cancelled, not failed: the only event is the delete.
	if types := m.events.types(); len(types) != 1 || types[0] != event.BackupDeleted {
		t.Fatalf("events = %v", types)
	}
	m.create = Create
	next, err := m.Start(false, "admin")
	if err != nil {
		t.Fatalf("Start after a cancel: %v", err)
	}
	m.settle(t, next.ID)
}

// A cancelled context stops the real writer, and nothing is left behind.
func TestCreateStopsOnCancel(t *testing.T) {
	configDir, _, db := fixture(t)
	defer func() { _ = db.Close() }()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	out := filepath.Join(t.TempDir(), "b.tar.gz")
	_, err := Create(ctx, Options{ConfigDir: configDir, Out: out})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Create = %v, want context.Canceled", err)
	}
	if got, _ := os.ReadDir(filepath.Dir(out)); len(got) != 0 {
		t.Fatalf("a cancelled backup left %d files", len(got))
	}
}

// What a daemon killed mid-backup leaves behind: a running record and staged
// files. The next daemon reports it failed and cleans up.
func TestManagerRecoversFromARestart(t *testing.T) {
	m := newManaged(t)
	ready, err := m.Start(false, "admin")
	if err != nil {
		t.Fatal(err)
	}
	ready = m.settle(t, ready.ID)

	interrupted := Record{ID: string(event.NewID()), Created: time.Now().UTC(), Version: "test", Status: StatusRunning}
	lost := Record{ID: string(event.NewID()), Created: time.Now().UTC(), Version: "test", Status: StatusReady, Size: 9}
	for _, rec := range []Record{interrupted, lost} {
		body, _ := json.Marshal(rec)
		if err := os.WriteFile(filepath.Join(m.dir, rec.ID+".json"), body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{tmpPrefix + "123", tmpPrefix + "db-456", "junk.json", "notes.txt"} {
		if err := os.WriteFile(filepath.Join(m.dir, name), []byte("{"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	again, err := NewManager(m.opts)
	if err != nil {
		t.Fatalf("NewManager after a restart: %v", err)
	}
	got, err := again.Get(interrupted.ID)
	if err != nil || got.Status != StatusFailed || got.Reason != reasonRestart {
		t.Fatalf("interrupted record = %+v, %v", got, err)
	}
	if got, err := again.Get(lost.ID); err != nil || got.Status != StatusFailed || got.Reason != reasonMissing {
		t.Fatalf("record without an archive = %+v, %v", got, err)
	}
	if got, err := again.Get(ready.ID); err != nil || got != ready {
		t.Fatalf("ready record = %+v, %v", got, err)
	}
	if err := again.Verify(ready.ID); err != nil {
		t.Fatalf("Verify after a restart: %v", err)
	}
	for _, name := range m.files(t) {
		if strings.HasPrefix(name, tmpPrefix) {
			t.Errorf("staged file %s survived the restart", name)
		}
	}
	// The verdict is on disk: a third start reads it back unchanged.
	third, err := NewManager(m.opts)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := third.Get(interrupted.ID); got.Reason != reasonRestart {
		t.Fatalf("after a second restart = %+v", got)
	}
	if len(third.List()) != 3 {
		t.Fatalf("List = %+v", third.List())
	}
}

func TestManagerShutdownFailsTheRunningBackup(t *testing.T) {
	configDir, dataDir, db := fixture(t)
	defer func() { _ = db.Close() }()
	rec := &recorder{}
	m, err := NewManager(ManagerOptions{
		Dir: filepath.Join(dataDir, "backups"), DataDir: dataDir, ConfigDir: configDir, DB: db.DB(), Events: rec,
	})
	if err != nil {
		t.Fatal(err)
	}
	mm := &managed{Manager: m, dir: filepath.Join(dataDir, "backups")}
	started, _ := mm.hold()
	r, err := m.Start(false, "admin")
	if err != nil {
		t.Fatal(err)
	}
	<-started
	m.Close()
	got, err := m.Get(r.ID)
	if err != nil || got.Status != StatusFailed || got.Reason != reasonShutdown {
		t.Fatalf("record = %+v, %v", got, err)
	}
	if files := mm.files(t); len(files) != 1 {
		t.Fatalf("managed directory holds %v", files)
	}
	if _, err := m.Start(false, "admin"); err == nil {
		t.Fatal("Start after shutdown succeeded")
	}
	if types := rec.types(); len(types) != 1 || types[0] != event.BackupFailed {
		t.Fatalf("events = %v", types)
	}
}

// A backup taken while the store is being written to is a consistent
// snapshot: it verifies, and its database opens.
func TestManagerBacksUpUnderConcurrentWrites(t *testing.T) {
	m := newManaged(t)
	db := m.opts.DB
	if _, err := db.Exec(`CREATE TABLE backup_probe (n INTEGER PRIMARY KEY, body TEXT)`); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := db.Exec(`INSERT INTO backup_probe (body) VALUES (?)`, fmt.Sprintf("row %d", i)); err != nil {
				t.Errorf("insert: %v", err)
				return
			}
		}
	}()
	defer wg.Wait()
	defer close(stop)

	// Writes are landing in the WAL: the offline path would refuse here.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if info, err := os.Stat(filepath.Join(m.opts.DataDir, dbFileName+"-wal")); err == nil && info.Size() > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the WAL never filled")
		}
		time.Sleep(time.Millisecond)
	}
	if _, err := Create(context.Background(), Options{DataDir: m.opts.DataDir, Out: t.TempDir()}); !errors.Is(err, ErrWALPresent) {
		t.Fatalf("offline Create = %v, want ErrWALPresent", err)
	}

	rec, err := m.Start(false, "admin")
	if err != nil {
		t.Fatal(err)
	}
	rec = m.settle(t, rec.ID)
	if rec.Status != StatusReady {
		t.Fatalf("record = %+v", rec)
	}
	if err := m.Verify(rec.ID); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	snapshot := entries(t, filepath.Join(m.dir, rec.ID+".tar.gz"))[entryDB]
	if !strings.HasPrefix(string(snapshot), "SQLite format 3\x00") {
		t.Fatal("the archived database is not a SQLite file")
	}
}
