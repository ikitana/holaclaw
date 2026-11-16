package main

import (
	"compress/gzip"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"holaclaw/internal/api/gen"
	"holaclaw/internal/backup"
)

var backupIDLine = regexp.MustCompile(`(?m)^id\s+(\S+)$`)

// readGzip returns the tar stream inside an archive, to look for what must
// or must not be in it.
func readGzip(t *testing.T, path string) []byte {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(gz)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// createBackup runs `backup create --wait` against a daemon and returns the
// id of the ready backup.
func createBackup(t *testing.T, e *daemonEnv, extra ...string) string {
	t.Helper()
	args := append([]string{"backup", "create", "--config", e.dir, "--wait"}, extra...)
	code, out, errOut := runCLI(t, "", args...)
	if code != 0 {
		t.Fatalf("backup create: exit = %d\n%s", code, errOut)
	}
	m := backupIDLine.FindStringSubmatch(out)
	if m == nil || !strings.Contains(out, "status  ready") || !strings.Contains(out, "size ") {
		t.Fatalf("backup create printed %q", out)
	}
	return m[1]
}

// The managed backups, end to end through the socket: the daemon is running
// and has written to its database, which is exactly when the offline backup
// has to refuse.
func TestBackupManagedByTheDaemon(t *testing.T) {
	e := startDaemon(t, "", "")

	code, out, _ := runCLI(t, "", "backup", "list", "--config", e.dir)
	if code != 0 || strings.TrimSpace(out) != "no backups" {
		t.Fatalf("empty list: exit = %d, stdout = %q", code, out)
	}
	if code, _, errOut := runCLI(t, "", "backup", "--config", e.dir, "--out", t.TempDir()); code != 1 ||
		!strings.Contains(errOut, "stop the daemon first") {
		t.Fatalf("offline backup of a live daemon: exit = %d, stderr = %q", code, errOut)
	}

	id := createBackup(t, e)

	code, out, errOut := runCLI(t, "", "backup", "list", "--config", e.dir)
	if code != 0 || !strings.Contains(out, id) || !strings.Contains(out, "ready") || !strings.Contains(out, "SECRETS") ||
		!strings.Contains(out, "ORIGIN") || !strings.Contains(out, "manual") {
		t.Fatalf("list: exit = %d\n%s%s", code, out, errOut)
	}
	code, out, _ = runCLI(t, "", "backup", "list", "--config", e.dir, "--json")
	var listed []gen.Backup
	if err := json.Unmarshal([]byte(out), &listed); code != 0 || err != nil || len(listed) != 1 ||
		listed[0].Id != id || listed[0].Status != gen.BackupStatusReady || listed[0].SecretsIncluded {
		t.Fatalf("list --json: exit = %d, err = %v\n%s", code, err, out)
	}

	// Flags after the id, and a directory as --out.
	outDir := t.TempDir()
	code, out, errOut = runCLI(t, "", "backup", "download", id, "--config", e.dir, "--out", outDir)
	path := strings.TrimSpace(out)
	if code != 0 || path != filepath.Join(outDir, "holaclaw-backup-"+id+".tar.gz") {
		t.Fatalf("download: exit = %d, stdout = %q\n%s", code, out, errOut)
	}
	info, err := os.Stat(path)
	if err != nil || info.Size() != listed[0].Size || info.Mode().Perm() != 0o600 {
		t.Fatalf("downloaded archive: %v, %v (the record says %d bytes)", info, err, listed[0].Size)
	}
	if left, _ := os.ReadDir(outDir); len(left) != 1 {
		t.Errorf("download left %d files in its directory", len(left))
	}
	if err := backup.Verify(path); err != nil {
		t.Fatalf("the downloaded archive does not verify: %v", err)
	}

	// verify takes the file, with no daemon in the way, or the id.
	for _, target := range []string{path, id} {
		code, out, errOut = runCLI(t, "", "backup", "verify", "--config", e.dir, target)
		if code != 0 || !strings.Contains(out, "ok: "+target+" matches its manifest") {
			t.Fatalf("verify %s: exit = %d, stdout = %q\n%s", target, code, out, errOut)
		}
	}
	if code, _, errOut = runCLI(t, "", "backup", "verify", "--config", e.dir, "01NOPE"); code != 1 ||
		!strings.Contains(errOut, "neither a file nor a backup") {
		t.Fatalf("verify of an unknown id: exit = %d, stderr = %q", code, errOut)
	}

	code, out, errOut = runCLI(t, "", "backup", "rm", "--config", e.dir, id)
	if code != 0 || !strings.Contains(out, "removed "+id) {
		t.Fatalf("rm: exit = %d\n%s%s", code, out, errOut)
	}
	if code, _, errOut = runCLI(t, "", "backup", "rm", "--config", e.dir, id); code != 1 || !strings.Contains(errOut, "not_found") {
		t.Fatalf("second rm: exit = %d, stderr = %q", code, errOut)
	}
	target := filepath.Join(t.TempDir(), "gone.tar.gz")
	if code, _, _ = runCLI(t, "", "backup", "download", "--config", e.dir, "--out", target, id); code != 1 {
		t.Fatalf("download of a removed backup: exit = %d", code)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("a failed download left %s behind (%v)", target, err)
	}
	if left, _ := os.ReadDir(filepath.Join(e.dir, "data", "backups")); len(left) != 0 {
		t.Fatalf("%d files left in the managed directory", len(left))
	}
}

func TestBackupCreateJSONAndSecrets(t *testing.T) {
	e := startDaemon(t, "", "")
	const marker = "CLI-BACKUP-SECRET-5b1e"
	if err := os.WriteFile(filepath.Join(e.dir, "auth.toml"), []byte("version = 1\n# "+marker+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Without --wait the answer is the accepted record, whatever its state.
	code, out, errOut := runCLI(t, "", "backup", "create", "--config", e.dir, "--json")
	var b gen.Backup
	if err := json.Unmarshal([]byte(out), &b); code != 0 || err != nil || b.Id == "" || b.SecretsIncluded {
		t.Fatalf("create --json: exit = %d, err = %v\n%s%s", code, err, out, errOut)
	}
	// One at a time: wait for it through the second request being accepted.
	var id string
	for id == "" {
		code, out, errOut = runCLI(t, "", "backup", "create", "--config", e.dir, "--wait", "--include-secrets", "--json")
		switch {
		case code == 0:
			if err := json.Unmarshal([]byte(out), &b); err != nil || b.Status != gen.BackupStatusReady || !b.SecretsIncluded {
				t.Fatalf("create --wait --json: %v\n%s", err, out)
			}
			id = b.Id
		case !strings.Contains(errOut, "another backup is running"):
			t.Fatalf("create --include-secrets: exit = %d\n%s", code, errOut)
		}
	}

	path := filepath.Join(t.TempDir(), "with-secrets.tar.gz")
	if code, _, errOut = runCLI(t, "", "backup", "download", "--config", e.dir, "--out", path, id); code != 0 {
		t.Fatalf("download: exit = %d\n%s", code, errOut)
	}
	if !strings.Contains(string(readGzip(t, path)), marker) {
		t.Fatal("auth.toml is missing from a backup that asked for it")
	}
}

func TestBackupSubcommandsNeedTheDaemon(t *testing.T) {
	dir := fixture(t, "minimal")
	for _, args := range [][]string{
		{"backup", "list"}, {"backup", "create"}, {"backup", "download", "01X"},
		{"backup", "verify", "01X"}, {"backup", "rm", "01X"}, {"backup", "restore", "01X", "--yes"},
		{"backup", "upload", filepath.Join(dir, "config.toml")},
	} {
		code, _, errOut := runCLI(t, "", append(args, "--config", dir, "--addr", filepath.Join(t.TempDir(), "no.sock"))...)
		if code != 1 || !strings.Contains(errOut, "daemon is not running") {
			t.Errorf("%v: exit = %d, stderr = %q", args, code, errOut)
		}
	}
}

func TestBackupUsageErrors(t *testing.T) {
	for _, args := range [][]string{
		{"backup", "remove", "01X"}, // not a subcommand, and not a silent offline backup either
		{"backup", "download"},
		{"backup", "verify"},
		{"backup", "rm"},
		{"backup", "rm", "a", "b"},
		{"backup", "upload"},
		{"backup", "restore"},
		{"backup", "list", "extra"},
		{"backup", "create", "extra"},
	} {
		if code, _, errOut := runCLI(t, "", args...); code != 2 || !strings.Contains(errOut, "usage: holaclaw backup") {
			t.Errorf("%v: exit = %d, stderr = %q", args, code, errOut)
		}
	}
	for _, sub := range []string{"list", "create", "download", "upload", "verify", "restore", "rm"} {
		if code, out, _ := runCLI(t, "", "backup", sub, "-h"); code != 0 || !strings.Contains(out, "usage: holaclaw backup "+sub) {
			t.Errorf("backup %s -h: exit = %d, stdout = %q", sub, code, out)
		}
	}
}

// An archive the offline backup wrote is uploaded, listed as an upload, and
// comes back byte for byte.
func TestBackupUpload(t *testing.T) {
	e := startDaemon(t, "", "")
	id := createBackup(t, e)
	dir := t.TempDir()
	path := filepath.Join(dir, "a.tar.gz")
	if code, _, errOut := runCLI(t, "", "backup", "download", id, "--config", e.dir, "--out", path); code != 0 {
		t.Fatalf("download: exit = %d\n%s", code, errOut)
	}

	code, out, errOut := runCLI(t, "", "backup", "upload", path, "--config", e.dir)
	m := backupIDLine.FindStringSubmatch(out)
	if code != 0 || m == nil || m[1] == id || !strings.Contains(out, "status  ready") {
		t.Fatalf("upload: exit = %d, stdout = %q\n%s", code, out, errOut)
	}
	uploaded := m[1]
	code, out, _ = runCLI(t, "", "backup", "list", "--config", e.dir, "--json")
	var listed []gen.Backup
	if err := json.Unmarshal([]byte(out), &listed); code != 0 || err != nil || len(listed) != 2 ||
		listed[0].Id != uploaded || listed[0].Origin != gen.Upload || listed[1].Origin != gen.Manual {
		t.Fatalf("list --json: exit = %d, err = %v\n%s", code, err, out)
	}
	back := filepath.Join(dir, "b.tar.gz")
	if code, _, errOut := runCLI(t, "", "backup", "download", uploaded, "--config", e.dir, "--out", back); code != 0 {
		t.Fatalf("download of the upload: exit = %d\n%s", code, errOut)
	}
	sent, _ := os.ReadFile(path)
	got, _ := os.ReadFile(back)
	if len(sent) == 0 || string(sent) != string(got) {
		t.Fatal("the upload does not come back as it was sent")
	}

	// What is not a backup is refused, and a missing file never reaches the daemon.
	junk := filepath.Join(dir, "junk.tar.gz")
	if err := os.WriteFile(junk, []byte("not an archive"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, errOut := runCLI(t, "", "backup", "upload", junk, "--config", e.dir); code != 1 || !strings.Contains(errOut, "bad_request") {
		t.Fatalf("upload of junk: exit = %d, stderr = %q", code, errOut)
	}
	if code, _, _ := runCLI(t, "", "backup", "upload", filepath.Join(dir, "missing"), "--config", e.dir); code != 1 {
		t.Fatalf("upload of a missing file: exit = %d", code)
	}
}

// restore asks before it destroys anything. The restore itself needs a
// daemon that can restart, which the daemon package tests end to end; this
// one, started without that, answers not_implemented once confirmed.
func TestBackupRestoreAsksFirst(t *testing.T) {
	e := startDaemon(t, "", "")
	id := createBackup(t, e)

	for _, answer := range []string{"", "no\n", "y\n"} {
		code, out, errOut := runCLI(t, answer, "backup", "restore", id, "--config", e.dir)
		if code != 1 || !strings.Contains(errOut, "Type yes to go on") || !strings.Contains(errOut, "not restored") || out != "" {
			t.Fatalf("restore answered %q: exit = %d, stdout = %q, stderr = %q", answer, code, out, errOut)
		}
	}
	if code, _, errOut := runCLI(t, "", "backup", "restore", "01NOPE", "--yes", "--config", e.dir); code != 1 ||
		!strings.Contains(errOut, "not_found") {
		t.Fatalf("restore of an unknown id: exit = %d, stderr = %q", code, errOut)
	}
	for _, args := range [][]string{{"--yes"}, {}} {
		code, _, errOut := runCLI(t, "yes\n", append([]string{"backup", "restore", id, "--config", e.dir}, args...)...)
		if code != 1 || !strings.Contains(errOut, "not_implemented") {
			t.Fatalf("confirmed restore %v: exit = %d, stderr = %q", args, code, errOut)
		}
	}
	if _, err := os.Stat(filepath.Join(e.dir, "data", "restore")); !os.IsNotExist(err) {
		t.Fatalf("a restore was staged (%v)", err)
	}
}

// initialised is a config directory `holaclaw init` wrote, with an auth.toml,
// and an offline backup of it that includes the secrets.
func initialised(t *testing.T) (dir, archive string) {
	t.Helper()
	dir = filepath.Join(t.TempDir(), "holaclaw")
	if code, _, errOut := runCLI(t, "", "init", "--config", dir, "--provider", "openai-codex", "--token-file", filepath.Join(t.TempDir(), "token")); code != 0 {
		t.Fatalf("init: exit = %d\n%s", code, errOut)
	}
	if err := os.MkdirAll(filepath.Join(dir, "secrets"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "secrets", "auth.toml"), []byte("version = 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	archive = filepath.Join(t.TempDir(), "backup.tar.gz")
	if code, _, errOut := runCLI(t, "", "backup", "--config", dir, "--out", archive, "--include-secrets"); code != 0 {
		t.Fatalf("offline backup: exit = %d\n%s", code, errOut)
	}
	return dir, archive
}

// A new host, with no daemon and nothing on disk: one command and the
// installation is there, owner included.
func TestBackupRestoreFileRecreatesAnInstallation(t *testing.T) {
	// The restored config names no socket: keep the default one away from a
	// daemon running on this host.
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	src, archive := initialised(t)
	dir := filepath.Join(t.TempDir(), "restored")

	// It asks first, having said what it would do, and writes nothing on a no.
	code, out, errOut := runCLI(t, "no\n", "backup", "restore", archive, "--config", dir)
	if code != 1 || out != "" || !strings.Contains(errOut, "Type yes to go on") || !strings.Contains(errOut, "not restored") ||
		strings.Contains(errOut, "The database is replaced") {
		t.Fatalf("restore answered no: exit = %d, stdout = %q, stderr = %q", code, out, errOut)
	}
	for _, want := range []string{
		dir + " does not exist and is created",
		"create  " + filepath.Join(dir, "config.toml"),
		"create  " + filepath.Join(dir, "data", "holaclaw.db"),
		// init names its secrets directory in full, so that is where they go.
		filepath.Join(src, "secrets", "auth.toml"),
	} {
		if !strings.Contains(errOut, want) {
			t.Errorf("the plan does not say %q:\n%s", want, errOut)
		}
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("a refused restore wrote %s (%v)", dir, err)
	}

	code, out, errOut = runCLI(t, "yes\n", "backup", "restore", archive, "--config", dir)
	if code != 0 || !strings.Contains(out, "restored "+archive+" into "+dir) || !strings.Contains(errOut, "holaclaw run --config "+dir) {
		t.Fatalf("restore: exit = %d, stdout = %q\n%s", code, out, errOut)
	}
	want, _ := os.ReadFile(filepath.Join(src, "config.toml"))
	got, err := os.ReadFile(filepath.Join(dir, "config.toml"))
	if err != nil || string(got) != string(want) {
		t.Fatalf("config.toml was not restored: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "agents")); err != nil {
		t.Errorf("the agents were not restored: %v", err)
	}
	// The owner came with the database: there is nothing left to initialise.
	if code, _, errOut := runCLI(t, "", "init", "--config", dir); code != 1 || !strings.Contains(errOut, "already initialised") {
		t.Fatalf("init after a restore: exit = %d, stderr = %q", code, errOut)
	}

	// Over an installation: what differs is replaced and kept.
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), append(got, "# edited\n"...), 0o600); err != nil {
		t.Fatal(err)
	}
	code, _, errOut = runCLI(t, "", "backup", "restore", "--yes", "--config", dir, archive)
	if code != 0 || !strings.Contains(errOut, "replace  "+filepath.Join(dir, "config.toml")) ||
		!strings.Contains(errOut, "holaclaw.db.pre-restore") || !strings.Contains(errOut, "3 already as in the backup") ||
		!strings.Contains(errOut, "The database is replaced") {
		t.Fatalf("second restore: exit = %d\n%s", code, errOut)
	}
	kept, _ := filepath.Glob(filepath.Join(dir, ".backup-*", "config.toml"))
	if len(kept) != 1 {
		t.Fatalf("the replaced config.toml was kept in %v", kept)
	}
	if body, _ := os.ReadFile(kept[0]); !strings.Contains(string(body), "# edited") {
		t.Errorf("the kept config.toml is %q", body)
	}
	if _, err := os.Stat(filepath.Join(dir, "data", "holaclaw.db.pre-restore")); err != nil {
		t.Errorf("the replaced database was not kept: %v", err)
	}
}

func TestBackupRestoreFileRefusals(t *testing.T) {
	// A path that is not there is not looked up as the id of a backup.
	missing := filepath.Join(t.TempDir(), "backup.tar.gz")
	if code, _, errOut := runCLI(t, "", "backup", "restore", missing, "--yes", "--config", t.TempDir()); code != 1 ||
		!strings.Contains(errOut, "no such file") || strings.Contains(errOut, "daemon is not running") {
		t.Fatalf("restore of a missing file: exit = %d, stderr = %q", code, errOut)
	}

	// What is not a backup changes nothing.
	dir := filepath.Join(t.TempDir(), "restored")
	junk := filepath.Join(t.TempDir(), "junk.tar.gz")
	if err := os.WriteFile(junk, []byte("not an archive"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := runCLI(t, "", "backup", "restore", junk, "--yes", "--config", dir); code != 1 {
		t.Fatalf("restore of junk: exit = %d", code)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("a refused restore wrote %s (%v)", dir, err)
	}

	// A running daemon has the database open: the files are not swapped under it.
	e := startDaemon(t, "", "")
	id := createBackup(t, e)
	archive := filepath.Join(t.TempDir(), "a.tar.gz")
	if code, _, errOut := runCLI(t, "", "backup", "download", id, "--config", e.dir, "--out", archive); code != 0 {
		t.Fatalf("download: exit = %d\n%s", code, errOut)
	}
	code, _, errOut := runCLI(t, "", "backup", "restore", archive, "--yes", "--config", e.dir)
	if code != 1 || !strings.Contains(errOut, "is running: stop it first") {
		t.Fatalf("restore under a running daemon: exit = %d, stderr = %q", code, errOut)
	}
	if _, err := os.Stat(filepath.Join(e.dir, "data", "holaclaw.db.pre-restore")); !os.IsNotExist(err) {
		t.Fatalf("the database was replaced under the daemon (%v)", err)
	}
}
