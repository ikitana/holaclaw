package api

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"holaclaw/internal/api/gen"
	"holaclaw/internal/backup"
	"holaclaw/internal/event"
)

const backupSecret = "BACKUP-SECRET-SHOULD-NEVER-LEAVE-71c2"

// eventLog keeps the events a backend emitted.
type eventLog struct {
	mu     sync.Mutex
	events []event.Event
}

func (l *eventLog) Emit(_ context.Context, e event.Event) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, e)
}

func (l *eventLog) types() []event.Type {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]event.Type, 0, len(l.events))
	for _, e := range l.events {
		out = append(out, e.Type)
	}
	return out
}

// withBackups gives the harness the real manager over its own store, as the
// daemon wires it, and returns the directory the backups land in.
func (h *harness) withBackups() (dir string, events *eventLog) {
	h.t.Helper()
	configDir := h.t.TempDir()
	secretsPath := filepath.Join(h.t.TempDir(), "auth.toml")
	for path, body := range map[string]string{
		filepath.Join(configDir, "config.toml"): "version = 1\n",
		secretsPath:                             "api_key = \"" + backupSecret + "\"\n",
	} {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			h.t.Fatal(err)
		}
	}
	dir = filepath.Join(h.st.Dir(), "backups")
	events = &eventLog{}
	m, err := backup.NewManager(backup.ManagerOptions{
		Dir: dir, DataDir: h.st.Dir(), ConfigDir: configDir, SecretsPath: secretsPath,
		DB: h.st.DB(), Version: "test", Events: events,
	})
	if err != nil {
		h.t.Fatalf("backup manager: %v", err)
	}
	h.t.Cleanup(m.Close)
	h.b.Backups = m
	return dir, events
}

// backupReady polls a backup until it leaves running.
func (h *harness) backupReady(id string) gen.Backup {
	h.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		b := decode[gen.Backup](h.t, h.do(http.MethodGet, "/backups/"+id, h.adminToken, nil), http.StatusOK)
		if b.Status != gen.BackupStatusRunning {
			return b
		}
		if time.Now().After(deadline) {
			h.t.Fatal("backup is still running")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func gunzip(t *testing.T, body []byte) string {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("gunzip: %v", err)
	}
	raw, err := io.ReadAll(gz)
	if err != nil {
		t.Fatalf("gunzip: %v", err)
	}
	return string(raw)
}

func TestBackupRoundTrip(t *testing.T) {
	h := newHarness(t)
	dir, events := h.withBackups()

	// No body at all is a backup without secrets.
	created := decode[gen.Backup](t, h.do(http.MethodPost, "/backups", h.adminToken, nil), http.StatusAccepted)
	if created.Status != gen.BackupStatusRunning || created.Id == "" || created.Version != "test" || created.SecretsIncluded {
		t.Fatalf("created = %+v", created)
	}
	ready := h.backupReady(created.Id)
	if ready.Status != gen.BackupStatusReady || ready.Size == 0 || ready.Reason != nil {
		t.Fatalf("ready = %+v", ready)
	}

	list := decode[struct {
		Backups []gen.Backup `json:"backups"`
	}](t, h.do(http.MethodGet, "/backups", h.adminToken, nil), http.StatusOK)
	if len(list.Backups) != 1 || list.Backups[0].Id != created.Id {
		t.Fatalf("list = %+v", list.Backups)
	}

	resp := h.do(http.MethodGet, "/backups/"+created.Id+"/content", h.adminToken, nil)
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("download: status %d, err %v", resp.StatusCode, err)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/gzip" {
		t.Errorf("Content-Type = %q", ct)
	}
	if cl := resp.Header.Get("Content-Length"); cl != strconv.FormatInt(ready.Size, 10) || int64(len(body)) != ready.Size {
		t.Errorf("Content-Length = %q, body %d bytes, record says %d", cl, len(body), ready.Size)
	}
	if cd := resp.Header.Get("Content-Disposition"); !strings.Contains(cd, created.Id+".tar.gz") {
		t.Errorf("Content-Disposition = %q", cd)
	}
	downloaded := filepath.Join(t.TempDir(), "downloaded.tar.gz")
	if err := os.WriteFile(downloaded, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := backup.Verify(downloaded); err != nil {
		t.Fatalf("the downloaded archive does not verify: %v", err)
	}
	if raw := gunzip(t, body); strings.Contains(raw, backupSecret) || strings.Contains(raw, "auth.toml") {
		t.Fatal("auth.toml is in an archive that did not ask for it")
	}

	v := decode[gen.BackupVerification](t, h.do(http.MethodPost, "/backups/"+created.Id+"/verify", h.adminToken, nil), http.StatusOK)
	if !v.Ok || v.Error != nil {
		t.Fatalf("verify = %+v", v)
	}
	if created.Origin != gen.Manual || ready.Origin != gen.Manual {
		t.Fatalf("origin = %q, %q, want manual", created.Origin, ready.Origin)
	}

	// A damaged archive is an answer, not a failed request.
	if err := os.WriteFile(filepath.Join(dir, created.Id+".tar.gz"), body[:len(body)/2], 0o600); err != nil {
		t.Fatal(err)
	}
	v = decode[gen.BackupVerification](t, h.do(http.MethodPost, "/backups/"+created.Id+"/verify", h.adminToken, nil), http.StatusOK)
	if v.Ok || v.Error == nil || *v.Error == "" {
		t.Fatalf("verify of a truncated archive = %+v", v)
	}

	resp = h.do(http.MethodDelete, "/backups/"+created.Id, h.adminToken, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete: status %d", resp.StatusCode)
	}
	errorBody(t, h.do(http.MethodGet, "/backups/"+created.Id, h.adminToken, nil), http.StatusNotFound, CodeNotFound)
	if left, _ := os.ReadDir(dir); len(left) != 0 {
		t.Fatalf("%d files left after the delete", len(left))
	}

	if got := events.types(); len(got) != 2 || got[0] != event.BackupCreated || got[1] != event.BackupDeleted {
		t.Fatalf("events = %v", got)
	}
	events.mu.Lock()
	defer events.mu.Unlock()
	for _, e := range events.events {
		if !strings.Contains(string(e.Attrs), created.Id) {
			t.Errorf("%s does not name the backup: %s", e.Type, e.Attrs)
		}
	}
}

// backupCalls is every backup operation, on an id that exists.
func backupCalls(id string) []struct{ method, path string } {
	return []struct{ method, path string }{
		{http.MethodGet, "/backups"},
		{http.MethodPost, "/backups"},
		{http.MethodGet, "/backups/" + id},
		{http.MethodGet, "/backups/" + id + "/content"},
		{http.MethodPost, "/backups/" + id + "/verify"},
		{http.MethodPost, "/backups/" + id + "/restore"},
		{http.MethodPost, "/backups/upload"},
		{http.MethodDelete, "/backups/" + id},
	}
}

func TestBackupsAreAdminOnly(t *testing.T) {
	h := newHarness(t)
	h.withBackups()
	created := decode[gen.Backup](t, h.do(http.MethodPost, "/backups", h.adminToken, nil), http.StatusAccepted)
	h.backupReady(created.Id)

	for _, token := range []string{h.ownerToken} {
		for _, c := range backupCalls(created.Id) {
			errorBody(t, h.do(c.method, c.path, token, nil), http.StatusForbidden, CodeForbidden)
		}
	}
	for _, c := range backupCalls(created.Id) {
		errorBody(t, h.do(c.method, c.path, "", nil), http.StatusUnauthorized, CodeUnauthorized)
	}
	// Nothing the refused calls did reached the backup.
	if b := h.backupReady(created.Id); b.Status != gen.BackupStatusReady {
		t.Fatalf("backup = %+v", b)
	}
}

func TestBackupUnknownID(t *testing.T) {
	h := newHarness(t)
	h.withBackups()
	h.b.Restart = func() { t.Error("restarted for a backup that does not exist") }
	for _, id := range []string{string(event.NewID()), "nope"} {
		for _, c := range backupCalls(id)[2:] {
			if !strings.Contains(c.path, id) {
				continue // the upload names no backup
			}
			errorBody(t, h.do(c.method, c.path, h.adminToken, nil), http.StatusNotFound, CodeNotFound)
		}
	}
	errorBody(t, h.do(http.MethodPost, "/backups", h.adminToken, map[string]any{"nope": true}),
		http.StatusBadRequest, CodeBadRequest)
}

func TestBackupsWithoutABackend(t *testing.T) {
	h := newHarness(t)
	for _, c := range backupCalls("x") {
		errorBody(t, h.do(c.method, c.path, h.adminToken, nil), http.StatusNotImplemented, CodeNotImplemented)
	}
}

// An admin may ask for the secrets and download them, over TCP like on the
// socket: the one place the API gives a credential back. Nobody below admin
// gets near it.
func TestBackupWithSecrets(t *testing.T) {
	h := newHarness(t)
	h.withBackups()
	withSecrets := gen.BackupCreate{IncludeSecrets: ptr(true)}

	for _, token := range []string{h.ownerToken} {
		errorBody(t, h.do(http.MethodPost, "/backups", token, withSecrets), http.StatusForbidden, CodeForbidden)
	}
	created := decode[gen.Backup](t, h.do(http.MethodPost, "/backups", h.adminToken, withSecrets), http.StatusAccepted)
	if !created.SecretsIncluded || created.Origin != gen.Manual {
		t.Fatalf("created = %+v", created)
	}
	if b := h.backupReady(created.Id); b.Status != gen.BackupStatusReady || !b.SecretsIncluded {
		t.Fatalf("backup = %+v", b)
	}

	for _, token := range []string{h.ownerToken} {
		errorBody(t, h.do(http.MethodGet, "/backups/"+created.Id+"/content", token, nil), http.StatusForbidden, CodeForbidden)
	}
	resp := h.do(http.MethodGet, "/backups/"+created.Id+"/content", h.adminToken, nil)
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("download: status %d, err %v", resp.StatusCode, err)
	}
	if !strings.Contains(gunzip(t, body), backupSecret) {
		t.Fatal("auth.toml is missing from an archive that asked for it")
	}
	// The record never carries what the archive does.
	rec := h.socket(http.MethodGet, "/backups", nil)
	if strings.Contains(rec.Body.String(), backupSecret) {
		t.Fatal("the list of backups carries a secret")
	}
}

// upload sends a raw archive body.
func (h *harness) upload(token string, body []byte) *http.Response {
	h.t.Helper()
	req, err := http.NewRequest(http.MethodPost, h.srv.URL+BaseURL+"/backups/upload", bytes.NewReader(body))
	if err != nil {
		h.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/gzip")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		h.t.Fatalf("upload: %v", err)
	}
	return resp
}

// archive takes a backup and returns its bytes.
func (h *harness) archive() (gen.Backup, []byte) {
	h.t.Helper()
	created := decode[gen.Backup](h.t, h.do(http.MethodPost, "/backups", h.adminToken, nil), http.StatusAccepted)
	ready := h.backupReady(created.Id)
	resp := h.do(http.MethodGet, "/backups/"+created.Id+"/content", h.adminToken, nil)
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusOK {
		h.t.Fatalf("download: status %d, err %v", resp.StatusCode, err)
	}
	return ready, body
}

func TestBackupUpload(t *testing.T) {
	h := newHarness(t)
	dir, events := h.withBackups()
	made, body := h.archive()

	for _, token := range []string{h.ownerToken} {
		errorBody(t, h.upload(token, body), http.StatusForbidden, CodeForbidden)
	}
	errorBody(t, h.upload("", body), http.StatusUnauthorized, CodeUnauthorized)

	up := decode[gen.Backup](t, h.upload(h.adminToken, body), http.StatusCreated)
	if up.Origin != gen.Upload || up.Status != gen.BackupStatusReady || up.Size != int64(len(body)) ||
		up.Id == made.Id || up.Version != "" {
		t.Fatalf("uploaded = %+v", up)
	}
	list := decode[struct {
		Backups []gen.Backup `json:"backups"`
	}](t, h.do(http.MethodGet, "/backups", h.adminToken, nil), http.StatusOK)
	if len(list.Backups) != 2 || list.Backups[0].Id != up.Id || list.Backups[1].Origin != gen.Manual {
		t.Fatalf("list = %+v", list.Backups)
	}
	v := decode[gen.BackupVerification](t, h.do(http.MethodPost, "/backups/"+up.Id+"/verify", h.adminToken, nil), http.StatusOK)
	if !v.Ok {
		t.Fatalf("verify of the upload = %+v", v)
	}
	resp := h.do(http.MethodGet, "/backups/"+up.Id+"/content", h.adminToken, nil)
	back, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !bytes.Equal(back, body) {
		t.Fatal("the upload does not come back as it was sent")
	}

	// What is not a backup is refused and leaves nothing.
	before, _ := os.ReadDir(dir)
	for name, bad := range map[string][]byte{
		"not gzip":  []byte("hello"),
		"truncated": body[:len(body)/2],
		"empty":     {},
	} {
		resp := h.upload(h.adminToken, bad)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", name, resp.StatusCode)
		}
		_ = resp.Body.Close()
	}
	if after, _ := os.ReadDir(dir); len(after) != len(before) {
		t.Fatalf("refused uploads left files: %d before, %d after", len(before), len(after))
	}
	if got := events.types(); len(got) != 2 || got[1] != event.BackupCreated {
		t.Fatalf("events = %v", got)
	}
}

func TestBackupRestoreStagesAndRestarts(t *testing.T) {
	h := newHarness(t)
	h.withBackups()
	made, _ := h.archive()
	staged := filepath.Join(h.st.Dir(), "restore")

	// A daemon that cannot restart itself cannot restore.
	errorBody(t, h.do(http.MethodPost, "/backups/"+made.Id+"/restore", h.adminToken, nil),
		http.StatusNotImplemented, CodeNotImplemented)

	var restarts int
	h.b.Restart = func() { restarts++ }
	for _, token := range []string{h.ownerToken} {
		errorBody(t, h.do(http.MethodPost, "/backups/"+made.Id+"/restore", token, nil), http.StatusForbidden, CodeForbidden)
	}
	errorBody(t, h.do(http.MethodPost, "/backups/nope/restore", h.adminToken, nil), http.StatusNotFound, CodeNotFound)
	if _, err := os.Stat(staged); !os.IsNotExist(err) || restarts != 0 {
		t.Fatalf("a refused restore staged something or restarted (%v, %d restarts)", err, restarts)
	}

	got := decode[gen.Backup](t, h.do(http.MethodPost, "/backups/"+made.Id+"/restore", h.adminToken, nil), http.StatusAccepted)
	if got.Id != made.Id || restarts != 1 {
		t.Fatalf("restore = %+v, %d restarts", got, restarts)
	}
	if _, err := os.Stat(filepath.Join(staged, "holaclaw.db")); err != nil {
		t.Fatalf("nothing was staged: %v", err)
	}
	// Until the restart happens, the data is spoken for.
	errorBody(t, h.do(http.MethodPost, "/backups/"+made.Id+"/restore", h.adminToken, nil), http.StatusConflict, CodeConflict)
	errorBody(t, h.do(http.MethodPost, "/backups", h.adminToken, nil), http.StatusConflict, CodeConflict)
	if restarts != 1 {
		t.Fatalf("%d restarts", restarts)
	}
}

func TestBackupRestoreRefusesADamagedArchive(t *testing.T) {
	h := newHarness(t)
	dir, _ := h.withBackups()
	h.b.Restart = func() { t.Error("the daemon restarted for a restore it refused") }
	made, body := h.archive()
	if err := os.WriteFile(filepath.Join(dir, made.Id+".tar.gz"), body[:len(body)/2], 0o600); err != nil {
		t.Fatal(err)
	}
	errorBody(t, h.do(http.MethodPost, "/backups/"+made.Id+"/restore", h.adminToken, nil), http.StatusConflict, CodeConflict)
	if _, err := os.Stat(filepath.Join(h.st.Dir(), "restore")); !os.IsNotExist(err) {
		t.Fatalf("a damaged archive was staged (%v)", err)
	}
	// Refused, not stuck: the next backup runs.
	next := decode[gen.Backup](t, h.do(http.MethodPost, "/backups", h.adminToken, nil), http.StatusAccepted)
	h.backupReady(next.Id)
}

// fakeBackups is a backend frozen in one state, for the answers that depend
// on a backup being caught mid-run.
type fakeBackups struct {
	rec     backup.Record
	deleted []string
}

func (f *fakeBackups) Start(bool, string) (backup.Record, error) {
	return backup.Record{}, backup.ErrBusy
}
func (f *fakeBackups) List() []backup.Record { return []backup.Record{f.rec} }
func (f *fakeBackups) Get(id string) (backup.Record, error) {
	if id != f.rec.ID {
		return backup.Record{}, backup.ErrNotFound
	}
	return f.rec, nil
}
func (f *fakeBackups) Open(id string) (*os.File, backup.Record, error) {
	if id != f.rec.ID {
		return nil, backup.Record{}, backup.ErrNotFound
	}
	return nil, f.rec, backup.ErrNotReady
}
func (f *fakeBackups) Verify(id string) error {
	_, _, err := f.Open(id)
	return err
}
func (f *fakeBackups) Delete(id, _ string) error {
	f.deleted = append(f.deleted, id)
	return nil
}
func (f *fakeBackups) Import(io.Reader, string) (backup.Record, error) {
	return backup.Record{}, backup.ErrBadArchive
}
func (f *fakeBackups) StageRestore(id, _ string) error {
	_, _, err := f.Open(id)
	return err
}

func TestBackupRunningAndFailed(t *testing.T) {
	for _, rec := range []backup.Record{
		{ID: "01RUNNING", Created: time.Now().UTC(), Version: "test", Status: backup.StatusRunning},
		{ID: "01FAILED", Created: time.Now().UTC(), Version: "test", Status: backup.StatusFailed, Reason: "disk full"},
	} {
		t.Run(string(rec.Status), func(t *testing.T) {
			h := newHarness(t)
			fake := &fakeBackups{rec: rec}
			h.b.Backups = fake

			errorBody(t, h.do(http.MethodPost, "/backups", h.adminToken, gen.BackupCreate{}), http.StatusConflict, CodeConflict)
			errorBody(t, h.do(http.MethodGet, "/backups/"+rec.ID+"/content", h.adminToken, nil), http.StatusConflict, CodeConflict)
			errorBody(t, h.do(http.MethodPost, "/backups/"+rec.ID+"/verify", h.adminToken, nil), http.StatusConflict, CodeConflict)
			h.b.Restart = func() { t.Error("restarted for a backup with no archive") }
			errorBody(t, h.do(http.MethodPost, "/backups/"+rec.ID+"/restore", h.adminToken, nil), http.StatusConflict, CodeConflict)

			got := decode[gen.Backup](t, h.do(http.MethodGet, "/backups/"+rec.ID, h.adminToken, nil), http.StatusOK)
			if string(got.Status) != string(rec.Status) || deref(got.Reason) != rec.Reason || got.Size != 0 {
				t.Fatalf("record = %+v", got)
			}

			resp := h.do(http.MethodDelete, "/backups/"+rec.ID, h.adminToken, nil)
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusNoContent || len(fake.deleted) != 1 {
				t.Fatalf("delete: status %d, backend saw %v", resp.StatusCode, fake.deleted)
			}
		})
	}
}

func TestClientBackups(t *testing.T) {
	h := newHarness(t)
	h.withBackups()
	ctx := context.Background()
	c, err := Dial(h.srv.URL, h.adminToken)
	if err != nil {
		t.Fatal(err)
	}
	created, err := c.CreateBackup(ctx, gen.BackupCreate{})
	if err != nil {
		t.Fatalf("CreateBackup: %v", err)
	}
	ready := h.backupReady(created.Id)
	if list, err := c.ListBackups(ctx); err != nil || len(list) != 1 {
		t.Fatalf("ListBackups = %+v, %v", list, err)
	}
	if got, err := c.GetBackup(ctx, created.Id); err != nil || got.Status != gen.BackupStatusReady {
		t.Fatalf("GetBackup = %+v, %v", got, err)
	}
	var buf bytes.Buffer
	if n, err := c.DownloadBackup(ctx, created.Id, &buf); err != nil || n != ready.Size || int64(buf.Len()) != n {
		t.Fatalf("DownloadBackup = %d, %v (record says %d)", n, err, ready.Size)
	}
	if v, err := c.VerifyBackup(ctx, created.Id); err != nil || !v.Ok {
		t.Fatalf("VerifyBackup = %+v, %v", v, err)
	}
	up, err := c.UploadBackup(ctx, bytes.NewReader(buf.Bytes()))
	if err != nil || up.Origin != gen.Upload || up.Size != ready.Size {
		t.Fatalf("UploadBackup = %+v, %v", up, err)
	}
	restarted := false
	h.b.Restart = func() { restarted = true }
	if got, err := c.RestoreBackup(ctx, up.Id); err != nil || got.Id != up.Id || !restarted {
		t.Fatalf("RestoreBackup = %+v, %v (restarted: %v)", got, err, restarted)
	}
	if err := c.DeleteBackup(ctx, created.Id); err != nil {
		t.Fatalf("DeleteBackup: %v", err)
	}
	var ce *ClientError
	if _, err := c.DownloadBackup(ctx, created.Id, io.Discard); !errors.As(err, &ce) || ce.Code != CodeNotFound {
		t.Fatalf("DownloadBackup of a deleted backup = %v", err)
	}
}
