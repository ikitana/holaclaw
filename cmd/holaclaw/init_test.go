package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"holaclaw/internal/app"
	"holaclaw/internal/auth"
	"holaclaw/internal/config"
	"holaclaw/internal/store"
	"holaclaw/internal/testmodel"
)

// ownerOf opens the database of a config directory and authenticates secret.
func ownerOf(t *testing.T, dir, secret string) auth.Principal {
	t.Helper()
	return ownerIn(t, filepath.Join(dir, "data"), secret)
}

// ownerIn opens the database in dataDir and authenticates secret.
func ownerIn(t *testing.T, dataDir, secret string) auth.Principal {
	t.Helper()
	st, err := store.Open(context.Background(), dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	p, err := auth.New(st).Authenticate(context.Background(), secret)
	if err != nil {
		t.Fatalf("the printed token does not authenticate: %v", err)
	}
	return p
}

// ownerAt opens the database in dataDir and returns the owner init made.
func ownerAt(t *testing.T, dataDir string) store.Identity {
	t.Helper()
	st, err := store.Open(context.Background(), dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	i, err := auth.New(st).Owner(context.Background())
	if err != nil {
		t.Fatalf("no owner after init: %v", err)
	}
	return i
}

// defaultConfigDir points the default config directory at a temp one through
// $HOLACLAW_CONFIG — the built-in default is /etc/holaclaw — and returns it.
func defaultConfigDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "hb")
	t.Setenv("HOLACLAW_CONFIG", dir)
	return dir
}

func TestInitWithoutDaemon(t *testing.T) {
	dir := t.TempDir()
	e := &daemonEnv{dir: dir, socket: shortSocket(t), model: testmodel.New(t)}
	writeDaemonConfig(t, e, "", "")

	code, out, errOut := runCLI(t, "", "init", "--config", dir)
	if code != 0 {
		t.Fatalf("init: exit = %d\n%s", code, errOut)
	}
	// No token at all, and nothing on stdout: the owner holds holaclaw:cli
	// alone, and stderr says how to pair a device.
	if out != "" {
		t.Fatalf("stdout = %q, want nothing", out)
	}
	owner := ownerAt(t, filepath.Join(dir, "data"))
	if !strings.Contains(errOut, "invite create --identity "+string(owner.ID)+" --channel holaclaw") {
		t.Fatalf("stderr does not say how to pair a device: %q", errOut)
	}
	st, err := store.Open(context.Background(), filepath.Join(dir, "data"))
	if err != nil {
		t.Fatal(err)
	}
	svc := auth.New(st)
	toks, terr := svc.ListTokens(context.Background(), auth.TokenFilter{All: true})
	bindings, berr := svc.Bindings(context.Background(), owner.ID)
	_ = st.Close()
	if terr != nil || berr != nil || len(toks) != 0 {
		t.Fatalf("tokens = %+v, %v %v; want none", toks, terr, berr)
	}
	if len(bindings) != 1 || bindings[0].Channel != "holaclaw" || bindings[0].Alias != auth.SocketAlias {
		t.Fatalf("owner bindings = %+v, want holaclaw:cli alone", bindings)
	}
	if strings.Contains(errOut, "warning") {
		t.Fatalf("a valid config got a warning: %q", errOut)
	}

	code, out, errOut = runCLI(t, "", "init", "--config", dir)
	if code != 1 || out != "" || !strings.Contains(errOut, "already initialised") {
		t.Fatalf("second init: exit = %d, stdout %q, stderr %q", code, out, errOut)
	}
}

// With no flags init uses the default config directory of every other
// command, and keeps the data and the secrets inside it, wherever it is run
// from.
func TestInitDefaultsToTheConfigDirectory(t *testing.T) {
	cfgDir := defaultConfigDir(t)
	wd := t.TempDir()
	t.Chdir(wd)

	code, _, errOut := runCLI(t, "", "init", "--provider", "openai-codex")
	if code != 0 {
		t.Fatalf("init: exit = %d\n%s", code, errOut)
	}
	if !strings.Contains(errOut, "Wrote a config for openai-codex") || !strings.Contains(errOut, "holaclaw run --config "+cfgDir) {
		t.Fatalf("stderr does not say what was written and how to run it: %q", errOut)
	}
	a, err := app.Static(cfgDir)
	if err != nil {
		t.Fatalf("the written config does not load: %v", err)
	}
	if want := filepath.Join(cfgDir, "data"); a.DataDir != want {
		t.Fatalf("data dir = %q, want %q", a.DataDir, want)
	}
	if want := filepath.Join(cfgDir, "secrets", config.FileAuth); a.Secrets.Path() != want {
		t.Fatalf("secrets = %q, want %q", a.Secrets.Path(), want)
	}
	for _, f := range []string{config.FileConfig, "agents/main/agent.toml", "agents/main/prompt.md"} {
		if _, err := os.Stat(filepath.Join(cfgDir, filepath.FromSlash(f))); err != nil {
			t.Fatalf("%s not written: %v", f, err)
		}
	}
	if p := ownerAt(t, a.DataDir); p.Role != store.RoleOwner {
		t.Fatalf("principal = %+v, want the owner", p)
	}
	if entries, err := os.ReadDir(wd); err != nil || len(entries) != 0 {
		t.Fatalf("init wrote into the current directory: %v, %v", entries, err)
	}

	code, _, errOut = runCLI(t, "", "init", "--provider", "openai-codex")
	if code != 1 || strings.Contains(errOut, "Wrote a config") || !strings.Contains(errOut, "already initialised") {
		t.Fatalf("second init: exit = %d, stderr %q", code, errOut)
	}
}

// A config directory with no config.toml but some files of its own keeps
// them, and its data goes inside it.
func TestInitKeepsExistingFiles(t *testing.T) {
	dir := t.TempDir()
	prompt := filepath.Join(dir, "agents", "main", "prompt.md")
	if err := os.MkdirAll(filepath.Dir(prompt), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(prompt, []byte("mine\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if code, _, errOut := runCLI(t, "", "init", "--config", dir, "--provider", "openai-codex"); code != 0 {
		t.Fatalf("init: exit = %d\n%s", code, errOut)
	}
	if body, err := os.ReadFile(prompt); err != nil || string(body) != "mine\n" {
		t.Fatalf("prompt.md = %q, %v; want it kept", body, err)
	}
	if _, err := os.Stat(filepath.Join(dir, config.FileConfig)); err != nil {
		t.Fatalf("config.toml not written: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "data", "holaclaw.db")); err != nil {
		t.Fatalf("no database in <config>/data: %v", err)
	}
}

// init reads server.data_dir to create the owner and nothing else: a config
// `holaclaw check` rejects still gets its owner, in the data directory it
// names, and a warning saying the config is not ready.
func TestInitWarnsAboutAnInvalidConfig(t *testing.T) {
	dir := t.TempDir()
	data := filepath.Join(t.TempDir(), "state")
	body := "version = 1\n[server]\ndata_dir = \"" + data + "\"\n[providers.x]\ntype = \"nope\"\n"
	if err := os.WriteFile(filepath.Join(dir, config.FileConfig), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := app.Static(dir); err == nil {
		t.Fatal("the config is meant to be invalid")
	}

	code, _, errOut := runCLI(t, "", "init", "--config", dir)
	if code != 0 {
		t.Fatalf("init: exit = %d\n%s", code, errOut)
	}
	if strings.Contains(errOut, "Wrote a config") {
		t.Fatalf("init wrote over an existing config: %q", errOut)
	}
	if !strings.Contains(errOut, "warning: the config in "+dir+" is not ready") {
		t.Fatalf("stderr does not warn about the config: %q", errOut)
	}
	if p := ownerAt(t, data); p.Role != store.RoleOwner {
		t.Fatalf("principal = %+v, want the owner", p)
	}
}

// A relative data_dir is relative to the config directory, not to where init
// runs: init and the daemon use the same database.
func TestInitRelativeDataDir(t *testing.T) {
	dir := t.TempDir()
	e := &daemonEnv{dir: dir, socket: shortSocket(t), model: testmodel.New(t)}
	writeDaemonConfig(t, e, "", "")
	cfg := filepath.Join(dir, config.FileConfig)
	body, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	body = []byte(strings.Replace(string(body), filepath.Join(dir, "data"), "state", 1))
	if err := os.WriteFile(cfg, body, 0o600); err != nil {
		t.Fatal(err)
	}
	wd := t.TempDir()
	t.Chdir(wd)

	code, _, errOut := runCLI(t, "", "init", "--config", dir)
	if code != 0 {
		t.Fatalf("init: exit = %d\n%s", code, errOut)
	}
	a, err := app.Static(dir)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "state"); a.DataDir != want {
		t.Fatalf("daemon data dir = %q, want %q", a.DataDir, want)
	}
	if p := ownerAt(t, a.DataDir); p.Role != store.RoleOwner {
		t.Fatalf("principal = %+v, want the owner", p)
	}
	if _, err := os.Stat(filepath.Join(wd, "state")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("init used the current directory: %v", err)
	}
}

func TestInitRejectsUnreadableConfig(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, config.FileConfig), []byte("[server\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := runCLI(t, "", "init", "--config", dir)
	if code != 1 || out != "" || !strings.Contains(errOut, config.FileConfig) {
		t.Fatalf("init: exit = %d, stdout %q, stderr %q", code, out, errOut)
	}
	if _, err := os.Stat(filepath.Join(dir, "data")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a broken config still got a database: %v", err)
	}
}

func TestInitRejectsArguments(t *testing.T) {
	if code, _, _ := runCLI(t, "", "init", "extra"); code != 2 {
		t.Fatalf("init extra: exit = %d, want 2", code)
	}
}

// --token-file creates an `init` binding with one admin token and writes the
// token to a new file only its owner can read, and nothing to stdout.
func TestInitTokenFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "config")
	path := filepath.Join(t.TempDir(), "token")

	code, out, errOut := runCLI(t, "", "init", "--config", dir, "--provider", "openai-codex", "--token-file", path)
	if code != 0 {
		t.Fatalf("init: exit = %d\n%s", code, errOut)
	}
	if out != "" || !strings.Contains(errOut, "admin token is in "+path) {
		t.Fatalf("stdout %q, stderr %q", out, errOut)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("token file mode = %v, want 0600", fi.Mode().Perm())
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	p := ownerOf(t, dir, strings.TrimSpace(string(body)))
	if p.Role != store.RoleOwner || p.Scope != auth.ScopeAdmin || p.Binding == auth.SocketAlias {
		t.Fatalf("principal = %+v, want the owner at admin scope", p)
	}
	st, err := store.Open(context.Background(), filepath.Join(dir, "data"))
	if err != nil {
		t.Fatal(err)
	}
	bindings, err := auth.New(st).Bindings(context.Background(), p.UserID)
	_ = st.Close()
	if err != nil || len(bindings) != 2 {
		t.Fatalf("owner bindings = %+v, %v; want holaclaw:cli and init", bindings, err)
	}
	for _, b := range bindings {
		if b.Channel != "holaclaw" || (b.Alias != auth.SocketAlias && (b.Alias != p.Binding || b.Display != "init")) {
			t.Fatalf("owner binding %+v", b)
		}
	}
}

// A token file that already exists is never overwritten: init fails, and
// takes back everything it created, the database included.
func TestInitTokenFileExists(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "config")
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("keep\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	code, out, errOut := runCLI(t, "", "init", "--config", dir, "--provider", "openai-codex", "--token-file", path)
	if code != 1 || out != "" || !strings.Contains(errOut, "write the token") {
		t.Fatalf("init: exit = %d, stdout %q, stderr %q", code, out, errOut)
	}
	if body, err := os.ReadFile(path); err != nil || string(body) != "keep\n" {
		t.Fatalf("token file = %q, %v; want it kept", body, err)
	}
	if entries, err := os.ReadDir(parent); err != nil || len(entries) != 0 {
		t.Fatalf("a failed init left %v behind (%v)", entries, err)
	}
}

// After a successful init, init does nothing: not even a config that went
// missing is written again.
func TestInitAfterSuccessWritesNothing(t *testing.T) {
	dir := t.TempDir()
	if code, _, errOut := runCLI(t, "", "init", "--config", dir, "--provider", "openai-codex"); code != 0 {
		t.Fatalf("init: exit = %d\n%s", code, errOut)
	}
	if err := os.Remove(filepath.Join(dir, config.FileConfig)); err != nil {
		t.Fatal(err)
	}

	code, out, errOut := runCLI(t, "", "init", "--config", dir)
	if code != 1 || out != "" || !strings.Contains(errOut, "already initialised") {
		t.Fatalf("init after success: exit = %d, stdout %q, stderr %q", code, out, errOut)
	}
	if _, err := os.Stat(filepath.Join(dir, config.FileConfig)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("init after success wrote a config: %v", err)
	}
}
