package backup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"holaclaw/internal/event"
	"holaclaw/internal/store"
)

// craft builds an archive with a manifest that matches files exactly, so a
// test can put anything in one. Entries are written in the order given.
func craft(t *testing.T, files ...[2]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	m := Manifest{Version: ManifestVersion, Created: time.Now().UTC()}
	for _, f := range files {
		name, body := f[0], f[1]
		if err := writeEntry(tw, name, []byte(body)); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256([]byte(body))
		m.Files = append(m.Files, ManifestFile{Path: name, Size: int64(len(body)), SHA256: hex.EncodeToString(sum[:])})
		if name == dirConfig+"auth.toml" {
			m.SecretsIncluded = true
		}
	}
	manifest, _ := json.Marshal(m)
	if err := writeEntry(tw, entryManifest, manifest); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// eventually polls until ok, for what a backup's goroutine does after the
// record has settled.
func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("never happened: %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (m *managed) count(origin Origin, status Status) int {
	n := 0
	for _, r := range m.List() {
		if r.Origin == origin && r.Status == status {
			n++
		}
	}
	return n
}

// --- automatic backups and retention ------------------------------------------

func TestAutomaticBackupsAreTakenWhenDueAndPruned(t *testing.T) {
	m := newManaged(t)
	m.Manager.opts.Keep = 2

	manual, err := m.Start(false, "admin")
	if err != nil {
		t.Fatal(err)
	}
	m.settle(t, manual.ID)

	first, started, err := m.Automatic(time.Now())
	if err != nil || !started || first.Origin != OriginAutomatic || first.SecretsIncluded {
		t.Fatalf("Automatic = %+v, %v, %v", first, started, err)
	}
	m.settle(t, first.ID)
	// One was just taken: nothing is due, now or in 23 hours.
	for _, at := range []time.Time{time.Now(), time.Now().Add(23 * time.Hour)} {
		if _, started, err := m.Automatic(at); err != nil || started {
			t.Fatalf("Automatic within the day = %v, %v", started, err)
		}
	}
	// A day later one is, every time.
	var newest Record
	for range 3 {
		rec, started, err := m.Automatic(time.Now().Add(25 * time.Hour))
		if err != nil || !started {
			t.Fatalf("Automatic a day later = %v, %v", started, err)
		}
		newest = m.settle(t, rec.ID)
	}
	eventually(t, "retention keeps two automatic backups", func() bool { return m.count(OriginAutomatic, StatusReady) == 2 })

	list := m.List()
	if len(list) != 3 || list[0].ID != newest.ID {
		t.Fatalf("List = %+v", list)
	}
	if _, err := m.Get(first.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the oldest automatic backup is still there: %v", err)
	}
	if got, err := m.Get(manual.ID); err != nil || got.Origin != OriginManual {
		t.Fatalf("the manual backup = %+v, %v: retention must never touch it", got, err)
	}
	if got := m.files(t); len(got) != 6 {
		t.Fatalf("managed directory holds %v, want three archives and three records", got)
	}
	pruned := func() (n int) {
		for _, e := range m.events.all() {
			if e.Type == event.BackupDeleted {
				n++
				if !strings.Contains(string(e.Attrs), `"by":"retention"`) {
					t.Errorf("a pruned backup is recorded as %s", e.Attrs)
				}
			}
		}
		return n
	}
	eventually(t, "two backups pruned, each with its event", func() bool { return pruned() == 2 })
}

func TestAutomaticBackupsOff(t *testing.T) {
	m := newManaged(t) // Keep is zero
	if _, started, err := m.Automatic(time.Now()); err != nil || started {
		t.Fatalf("Automatic with keep 0 = %v, %v", started, err)
	}
	if len(m.List()) != 0 {
		t.Fatalf("List = %+v", m.List())
	}
}

// A run of failures is retried, keeps only its latest record, and never
// pushes a good backup out.
func TestAutomaticFailuresDoNotPileUp(t *testing.T) {
	m := newManaged(t)
	m.Manager.opts.Keep = 1
	good, _, err := m.Automatic(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	m.settle(t, good.ID)

	m.create = func(context.Context, Options) (string, error) { return "", errors.New("disk full") }
	var last Record
	for range 3 {
		// A failed one does not count as taken: the very next call retries.
		rec, started, err := m.Automatic(time.Now().Add(25 * time.Hour))
		if err != nil || !started {
			t.Fatalf("Automatic after a failure = %v, %v", started, err)
		}
		last = m.settle(t, rec.ID)
		eventually(t, "one failed record", func() bool { return m.count(OriginAutomatic, StatusFailed) == 1 })
	}
	if got, err := m.Get(last.ID); err != nil || got.Reason == "" {
		t.Fatalf("the latest failure = %+v, %v", got, err)
	}
	if got, err := m.Get(good.ID); err != nil || got.Status != StatusReady {
		t.Fatalf("the good backup = %+v, %v", got, err)
	}
}

func TestAutomaticWaitsForARunningBackup(t *testing.T) {
	m := newManaged(t)
	m.Manager.opts.Keep = 1
	started, release := m.hold()
	manual, err := m.Start(false, "admin")
	if err != nil {
		t.Fatal(err)
	}
	<-started
	if _, ok, err := m.Automatic(time.Now()); err != nil || ok {
		t.Fatalf("Automatic while a backup runs = %v, %v", ok, err)
	}
	release()
	m.settle(t, manual.ID)
	if _, ok, err := m.Automatic(time.Now()); err != nil || !ok {
		t.Fatalf("Automatic afterwards = %v, %v", ok, err)
	}
}

// --- upload ----------------------------------------------------------------------

func TestImportKeepsAVerifiedArchive(t *testing.T) {
	m := newManaged(t)
	made, err := m.Start(true, "admin")
	if err != nil {
		t.Fatal(err)
	}
	made = m.settle(t, made.ID)
	body, err := os.ReadFile(filepath.Join(m.dir, made.ID+".tar.gz"))
	if err != nil {
		t.Fatal(err)
	}

	rec, err := m.Import(bytes.NewReader(body), "admin")
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if rec.Origin != OriginUpload || rec.Status != StatusReady || rec.Size != int64(len(body)) ||
		!rec.SecretsIncluded || rec.Version != "" || rec.ID == made.ID {
		t.Fatalf("imported record = %+v", rec)
	}
	if err := m.Verify(rec.ID); err != nil {
		t.Fatalf("Verify of the upload: %v", err)
	}
	stored, err := os.ReadFile(filepath.Join(m.dir, rec.ID+".tar.gz"))
	if err != nil || !bytes.Equal(stored, body) {
		t.Fatalf("the stored upload differs from what was sent (%v)", err)
	}
	if list := m.List(); len(list) != 2 || list[0].ID != rec.ID {
		t.Fatalf("List = %+v", list)
	}
	// It survives a restart as what it is.
	again, err := NewManager(m.opts)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := again.Get(rec.ID); err != nil || got != rec {
		t.Fatalf("after a restart = %+v, %v", got, err)
	}
}

func TestImportRefusesWhatIsNotABackup(t *testing.T) {
	m := newManaged(t)
	good := craft(t, [2]string{"data/blobs/ab/cd/abcd", "blob"})
	tampered := bytes.Clone(craft(t, [2]string{"config/config.toml", strings.Repeat("version = 1\n", 400)}))
	tampered[len(tampered)/2] ^= 0xff
	newer, _ := json.Marshal(Manifest{Version: ManifestVersion + 1})

	for name, body := range map[string][]byte{
		"not gzip":         []byte("hello, this is not an archive"),
		"empty":            nil,
		"truncated":        good[:len(good)/2],
		"tampered":         tampered,
		"no manifest":      gzipTar(t),
		"a newer manifest": gzipTar(t, [2]string{entryManifest, string(newer)}),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := m.Import(bytes.NewReader(body), "admin"); !errors.Is(err, ErrBadArchive) {
				t.Fatalf("Import = %v, want ErrBadArchive", err)
			}
		})
	}
	if got := m.files(t); len(got) != 0 {
		t.Fatalf("refused uploads left %v", got)
	}
	if len(m.List()) != 0 || len(m.events.types()) != 0 {
		t.Fatalf("refused uploads were listed or traced: %+v, %v", m.List(), m.events.types())
	}
	if _, err := m.Import(bytes.NewReader(good), "admin"); err != nil {
		t.Fatalf("Import of a good archive: %v", err)
	}
}

// gzipTar is craft without the manifest.
func gzipTar(t *testing.T, files ...[2]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, f := range files {
		if err := writeEntry(tw, f[0], []byte(f[1])); err != nil {
			t.Fatal(err)
		}
	}
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

// --- restore ---------------------------------------------------------------------

// probe is a table only the "after the backup" database has.
func probe(t *testing.T, s *store.SQLite) bool {
	t.Helper()
	var n int
	err := s.DB().QueryRow(`SELECT count(*) FROM sqlite_master WHERE name = 'after_backup'`).Scan(&n)
	if err != nil {
		t.Fatal(err)
	}
	return n == 1
}

func TestRestoreRoundTrip(t *testing.T) {
	m := newManaged(t)
	ctx := context.Background()
	dataDir := m.opts.DataDir
	rec, err := m.Start(true, "admin")
	if err != nil {
		t.Fatal(err)
	}
	m.settle(t, rec.ID)

	// What happens after the backup: a schema change and a new blob.
	if _, err := m.opts.DB.Exec(`CREATE TABLE after_backup (n INTEGER)`); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(ctx, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	later, err := st.PutBlob(ctx, strings.NewReader("a blob from after the backup"), "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	if !probe(t, st) {
		t.Fatal("the probe table is not there to begin with")
	}
	_ = st.Close()
	configBefore, _ := os.ReadFile(filepath.Join(m.opts.ConfigDir, "config.toml"))
	secretsBefore, _ := os.ReadFile(m.opts.SecretsPath)

	// Nothing staged yet: a start applies nothing.
	if _, applied, err := ApplyPendingRestore(dataDir); err != nil || applied {
		t.Fatalf("ApplyPendingRestore with nothing staged = %v, %v", applied, err)
	}

	if err := m.StageRestore(rec.ID, "admin"); err != nil {
		t.Fatalf("StageRestore: %v", err)
	}
	// Staged, not applied: the live database is untouched until the restart.
	if _, err := os.Stat(filepath.Join(dataDir, restoreDirName, restoreInfo)); err != nil {
		t.Fatalf("nothing staged: %v", err)
	}
	// The data is about to be replaced: no backup starts, no second restore.
	if _, err := m.Start(false, "admin"); !errors.Is(err, ErrBusy) {
		t.Fatalf("Start with a restore staged = %v, want ErrBusy", err)
	}
	if err := m.StageRestore(rec.ID, "admin"); !errors.Is(err, ErrBusy) {
		t.Fatalf("second StageRestore = %v, want ErrBusy", err)
	}

	// The daemon stops, and the next start applies it before opening the store.
	m.Close()
	_ = m.opts.DB.Close()
	info, applied, err := ApplyPendingRestore(dataDir)
	if err != nil || !applied || info.BackupID != rec.ID || info.By != "admin" {
		t.Fatalf("ApplyPendingRestore = %+v, %v, %v", info, applied, err)
	}
	if _, applied, err := ApplyPendingRestore(dataDir); err != nil || applied {
		t.Fatalf("a second ApplyPendingRestore = %v, %v", applied, err)
	}

	st, err = store.Open(ctx, dataDir)
	if err != nil {
		t.Fatalf("the restored database does not open: %v", err)
	}
	defer func() { _ = st.Close() }()
	if probe(t, st) {
		t.Fatal("the database still has what was written after the backup")
	}
	// The blob of the fixture is back in a store that knows it; the later
	// one is only a file nobody references.
	if _, err := os.Stat(filepath.Join(dataDir, "blobs", later.ID[0:2], later.ID[2:4], later.ID)); err != nil {
		t.Errorf("a blob the restore did not bring was removed: %v", err)
	}
	if _, _, err := st.OpenBlob(ctx, later.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("OpenBlob of the later blob = %v, want ErrNotFound", err)
	}
	for _, gone := range []string{restoreDirName, restoreTmpName} {
		if _, err := os.Stat(filepath.Join(dataDir, gone)); !os.IsNotExist(err) {
			t.Errorf("%s is still there (%v)", gone, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dataDir, dbFileName+previousDBSuffix)); err != nil {
		t.Errorf("the replaced database was not kept: %v", err)
	}
	// Config and secrets are the operator's: an archive that has both
	// changed neither.
	if now, _ := os.ReadFile(filepath.Join(m.opts.ConfigDir, "config.toml")); !bytes.Equal(now, configBefore) {
		t.Error("the restore rewrote config.toml")
	}
	if now, _ := os.ReadFile(m.opts.SecretsPath); !bytes.Equal(now, secretsBefore) {
		t.Error("the restore rewrote auth.toml")
	}
	// The managed backups are not part of what a restore replaces.
	again, err := NewManager(ManagerOptions{Dir: m.dir, DataDir: dataDir, DB: st.DB()})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := again.Get(rec.ID); err != nil || got.Status != StatusReady {
		t.Fatalf("the backup after its own restore = %+v, %v", got, err)
	}
}

// A blob the store already has is left alone, and one it lost comes back.
func TestRestoreMergesBlobs(t *testing.T) {
	m := newManaged(t)
	rec, err := m.Start(false, "admin")
	if err != nil {
		t.Fatal(err)
	}
	m.settle(t, rec.ID)
	var blob string
	for name := range entries(t, filepath.Join(m.dir, rec.ID+".tar.gz")) {
		if rel, ok := strings.CutPrefix(name, dirBlobs); ok {
			blob = filepath.Join(m.opts.DataDir, "blobs", filepath.FromSlash(rel))
		}
	}
	if blob == "" {
		t.Fatal("the fixture's blob is not in the archive")
	}
	if err := os.Remove(blob); err != nil {
		t.Fatal(err)
	}
	if err := m.StageRestore(rec.ID, "admin"); err != nil {
		t.Fatal(err)
	}
	m.Close()
	_ = m.opts.DB.Close()
	if _, applied, err := ApplyPendingRestore(m.opts.DataDir); err != nil || !applied {
		t.Fatalf("ApplyPendingRestore = %v, %v", applied, err)
	}
	if body, err := os.ReadFile(blob); err != nil || string(body) != "blob content" {
		t.Fatalf("the lost blob = %q, %v", body, err)
	}
}

func TestStageRestoreRefusals(t *testing.T) {
	m := newManaged(t)
	if err := m.StageRestore("nope", "admin"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown id = %v", err)
	}

	started, release := m.hold()
	running, err := m.Start(false, "admin")
	if err != nil {
		t.Fatal(err)
	}
	<-started
	if err := m.StageRestore(running.ID, "admin"); !errors.Is(err, ErrNotReady) {
		t.Fatalf("restore of a running backup = %v, want ErrNotReady", err)
	}
	release()
	ready := m.settle(t, running.ID)

	// Another backup is running: its snapshot must not race the restore.
	m.create = Create
	started, release = m.hold()
	other, err := m.Start(false, "admin")
	if err != nil {
		t.Fatal(err)
	}
	<-started
	if err := m.StageRestore(ready.ID, "admin"); !errors.Is(err, ErrBusy) {
		t.Fatalf("restore while a backup runs = %v, want ErrBusy", err)
	}
	release()
	m.settle(t, other.ID)

	for name, tc := range map[string]struct {
		body []byte
		want error
	}{
		"no database":      {craft(t, [2]string{"config/config.toml", "version = 1\n"}), ErrBadArchive},
		"not a database":   {craft(t, [2]string{entryDB, "this is not sqlite"}), ErrBadArchive},
		"a blob outside":   {craft(t, [2]string{dirBlobs + "../../holaclaw.db", "x"}), ErrBadArchive},
		"a blob elsewhere": {craft(t, [2]string{dirBlobs + "/etc/passwd", "x"}), ErrBadArchive},
	} {
		t.Run(name, func(t *testing.T) {
			rec, err := m.Import(bytes.NewReader(tc.body), "admin")
			if err != nil {
				t.Fatalf("Import: %v", err)
			}
			if err := m.StageRestore(rec.ID, "admin"); !errors.Is(err, tc.want) {
				t.Fatalf("StageRestore = %v, want %v", err, tc.want)
			}
		})
	}
	// A refusal leaves nothing staged and nothing blocked.
	for _, name := range []string{restoreDirName, restoreTmpName} {
		if _, err := os.Stat(filepath.Join(m.opts.DataDir, name)); !os.IsNotExist(err) {
			t.Errorf("%s exists after a refused restore (%v)", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(m.opts.DataDir), "holaclaw.db")); !os.IsNotExist(err) {
		t.Errorf("a blob name escaped the staging directory (%v)", err)
	}
	next, err := m.Start(false, "admin")
	if err != nil {
		t.Fatalf("Start after refused restores: %v", err)
	}
	m.settle(t, next.ID)
}

// A database a newer holaclaw migrated cannot be opened by this one: the
// restore is refused while the daemon is still up to say so.
func TestStageRestoreRefusesANewerSchema(t *testing.T) {
	m := newManaged(t)
	if _, err := m.opts.DB.Exec(`INSERT INTO schema_migrations (version, name, applied_at) VALUES (9999, 'future', 0)`); err != nil {
		t.Fatal(err)
	}
	rec, err := m.Start(false, "admin")
	if err != nil {
		t.Fatal(err)
	}
	m.settle(t, rec.ID)
	if err := m.StageRestore(rec.ID, "admin"); !errors.Is(err, ErrIncompatible) {
		t.Fatalf("StageRestore = %v, want ErrIncompatible", err)
	}
	if _, err := os.Stat(filepath.Join(m.opts.DataDir, restoreDirName)); !os.IsNotExist(err) {
		t.Fatalf("an incompatible restore was staged (%v)", err)
	}
}

// What a start that died halfway through applying leaves: the old database
// already moved aside, the staged one not yet in place.
func TestApplyPendingRestoreFinishesAnInterruptedOne(t *testing.T) {
	m := newManaged(t)
	rec, err := m.Start(false, "admin")
	if err != nil {
		t.Fatal(err)
	}
	m.settle(t, rec.ID)
	if err := m.StageRestore(rec.ID, "admin"); err != nil {
		t.Fatal(err)
	}
	m.Close()
	_ = m.opts.DB.Close()
	dataDir := m.opts.DataDir
	db := filepath.Join(dataDir, dbFileName)
	if err := os.Rename(db, db+previousDBSuffix); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, restoreTmpName), []byte("junk"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, applied, err := ApplyPendingRestore(dataDir); err != nil || !applied {
		t.Fatalf("ApplyPendingRestore = %v, %v", applied, err)
	}
	st, err := store.Open(context.Background(), dataDir)
	if err != nil {
		t.Fatalf("the database does not open: %v", err)
	}
	_ = st.Close()
	if _, err := os.Stat(db + previousDBSuffix); err != nil {
		t.Errorf("the replaced database was lost: %v", err)
	}
}

func TestApplyPendingRestoreRefusesWhatItCannotFinish(t *testing.T) {
	dataDir := t.TempDir()
	dir := filepath.Join(dataDir, restoreDirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// A directory of that name that is not a staged restore.
	if _, _, err := ApplyPendingRestore(dataDir); err == nil {
		t.Fatal("a restore directory without its record was accepted")
	}
	// Staged, but with no database to put in place and none already there:
	// opening the store now would create an empty one.
	body, _ := json.Marshal(Restore{BackupID: "01X"})
	if err := os.WriteFile(filepath.Join(dir, restoreInfo), body, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ApplyPendingRestore(dataDir); err == nil || !strings.Contains(err.Error(), "01X") {
		t.Fatalf("ApplyPendingRestore = %v", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("the evidence was removed: %v", err)
	}
}
