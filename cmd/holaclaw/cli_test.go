package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// runCLI drives one invocation exactly as main does, and returns the exit
// status with whatever the command wrote.
func runCLI(t *testing.T, stdin string, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut strings.Builder
	code := run(context.Background(), args, strings.NewReader(stdin), &out, &errOut)
	t.Logf("holaclaw %s -> %d\nstdout:\n%s\nstderr:\n%s", strings.Join(args, " "), code, out.String(), errOut.String())
	return code, out.String(), errOut.String()
}

// fixture copies a testdata config directory into a temp dir: `check` creates
// the data directory it probes, and the repository must not grow one.
func fixture(t *testing.T, name string) string {
	t.Helper()
	src := filepath.Join("..", "..", "testdata", "config", name)
	dst := t.TempDir()
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		body, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		// The secrets go in the temp dir too, not in the fixture's /etc.
		if rel == "config.toml" {
			body = bytes.Replace(body, []byte(`"/etc/holaclaw-secrets"`), fmt.Appendf(nil, "%q", filepath.Join(dst, "secrets")), 1)
		}
		return os.WriteFile(target, body, 0o644)
	})
	if err != nil {
		t.Fatalf("copy fixture %s: %v", name, err)
	}
	return dst
}

// --- check ---------------------------------------------------------------------

func TestCheckGoodConfig(t *testing.T) {
	dir := fixture(t, "minimal")
	code, out, _ := runCLI(t, "", "check", "--config", dir, "--offline")
	if code != 0 {
		t.Fatalf("exit = %d, want 0\n%s", code, out)
	}
	// Nothing but info is allowed to show up here; naming the ids keeps the
	// assertion honest when a new rule starts firing.
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" || line == "ok: no findings" {
			continue
		}
		if !strings.Contains(line, " info:") {
			t.Errorf("unexpected finding on a good config: %s", line)
		}
	}
}

func TestCheckUnknownKey(t *testing.T) {
	dir := fixture(t, "bad-unknown-key")
	code, out, _ := runCLI(t, "", "check", "--config", dir, "--offline")
	if code != 2 {
		t.Fatalf("exit = %d, want 2\n%s", code, out)
	}
	if !strings.Contains(out, "CFG001") {
		t.Errorf("no CFG001 in output:\n%s", out)
	}
	if !strings.Contains(out, "sockett") || !strings.Contains(out, `did you mean "socket"`) {
		t.Errorf("no did-you-mean hint:\n%s", out)
	}
}

func TestCheckJSON(t *testing.T) {
	dir := fixture(t, "bad-unknown-key")
	code, out, _ := runCLI(t, "", "check", "--config", dir, "--offline", "--json")
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	var findings []struct {
		ID       string `json:"id"`
		Severity string `json:"severity"`
		File     string `json:"file"`
		Key      string `json:"key"`
		Message  string `json:"message"`
		Fix      string `json:"fix"`
	}
	if err := json.Unmarshal([]byte(out), &findings); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	if len(findings) == 0 || findings[0].ID != "CFG001" {
		t.Fatalf("findings = %+v", findings)
	}
	if findings[0].Key != "server.sockett" || findings[0].Fix == "" {
		t.Errorf("finding = %+v", findings[0])
	}
}

func TestCheckSkillTool(t *testing.T) {
	dir := fixture(t, "bad-skill-tool")
	code, out, _ := runCLI(t, "", "check", "--config", dir, "--offline")
	if code != 1 {
		t.Fatalf("exit = %d, want 1\n%s", code, out)
	}
	if !strings.Contains(out, "SKL001 warning: agents/main/skills/release: metadata.holaclaw.requires.tools: skill release (agent) requires tool exec, which agent main does not list: hidden") {
		t.Errorf("no SKL001 in output:\n%s", out)
	}

	code, out, _ = runCLI(t, "", "check", "--config", dir, "--offline", "--json")
	if code != 1 {
		t.Fatalf("--json: exit = %d, want 1", code)
	}
	var findings []struct{ ID, File, Fix string }
	if err := json.Unmarshal([]byte(out), &findings); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	if len(findings) != 1 || findings[0].ID != "SKL001" || findings[0].Fix == "" {
		t.Fatalf("findings = %+v", findings)
	}
}

func TestCheckOnlyAndSkip(t *testing.T) {
	dir := fixture(t, "bad-unknown-key")

	code, out, _ := runCLI(t, "", "check", "--config", dir, "--offline", "--only", "SEC")
	if code != 0 {
		t.Errorf("--only SEC: exit = %d, want 0\n%s", code, out)
	}
	if strings.Contains(out, "CFG001") {
		t.Errorf("--only SEC still printed CFG001:\n%s", out)
	}

	code, out, _ = runCLI(t, "", "check", "--config", dir, "--offline", "--skip", "CFG")
	if code != 0 {
		t.Errorf("--skip CFG: exit = %d, want 0\n%s", code, out)
	}
	if strings.Contains(out, "CFG001") {
		t.Errorf("--skip CFG still printed CFG001:\n%s", out)
	}
}

// --- config --------------------------------------------------------------------

func TestConfigMigrateDryRunChangesNothing(t *testing.T) {
	dir := fixture(t, "minimal")
	before := treeOf(t, dir)

	code, out, _ := runCLI(t, "", "config", "migrate", "--config", dir)
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(out, "nothing to migrate") {
		t.Errorf("output = %q", out)
	}
	if after := treeOf(t, dir); after != before {
		t.Errorf("the dry run changed the directory:\n%v\n%v", before, after)
	}
}

func TestConfigSchemaIsValidJSON(t *testing.T) {
	for _, file := range []string{"config.toml", "channels.toml", "agent.toml"} {
		code, out, _ := runCLI(t, "", "config", "schema", file)
		if code != 0 {
			t.Fatalf("%s: exit = %d", file, code)
		}
		var doc map[string]any
		if err := json.Unmarshal([]byte(out), &doc); err != nil {
			t.Fatalf("%s: not JSON: %v", file, err)
		}
		if doc["title"] != file {
			t.Errorf("%s: title = %v", file, doc["title"])
		}
		if doc["$schema"] == nil || doc["properties"] == nil {
			t.Errorf("%s: schema is missing its head: %v", file, doc)
		}
	}
	if code, _, _ := runCLI(t, "", "config", "schema", "nope.toml"); code != 1 {
		t.Errorf("unknown schema file: exit = %d, want 1", code)
	}
}

// treeOf hashes a directory's file names and contents, for "nothing changed".
func treeOf(t *testing.T, dir string) string {
	t.Helper()
	var b strings.Builder
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		if d.IsDir() {
			b.WriteString("d " + rel + "\n")
			return nil
		}
		body, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		b.WriteString("f " + rel + " " + info.Mode().String() + " " + string(body) + "\n")
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	return b.String()
}

// --- auth ----------------------------------------------------------------------

func TestAuthSetListRemove(t *testing.T) {
	dir := t.TempDir()
	const secret = "sk-super-secret-value"
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(fmt.Sprintf("version = 1\n[server]\nsecrets_dir = %q\n", dir)), 0o600); err != nil {
		t.Fatal(err)
	}

	// The value comes from stdin: an argument would be visible to every other
	// process on the machine.
	if code, _, errOut := runCLI(t, secret+"\n", "auth", "set", "--config", dir, "providers.openrouter", "api_key"); code != 0 {
		t.Fatalf("auth set: exit = %d\n%s", code, errOut)
	}
	path := filepath.Join(dir, "auth.toml")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("auth.toml: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("auth.toml mode = %v, want 0600", info.Mode().Perm())
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), secret) {
		t.Fatalf("the secret was not stored:\n%s", body)
	}

	code, out, _ := runCLI(t, "", "auth", "list", "--config", dir)
	if code != 0 {
		t.Fatalf("auth list: exit = %d", code)
	}
	if !strings.Contains(out, "providers.openrouter") || !strings.Contains(out, "api_key") {
		t.Errorf("auth list = %q", out)
	}
	if strings.Contains(out, secret) {
		t.Fatalf("auth list printed the value: %q", out)
	}

	// A second key, then remove just that one, then the whole entry.
	if code, _, _ := runCLI(t, "", "auth", "set", "--config", dir, "providers.openrouter", "org", "acme"); code != 0 {
		t.Fatalf("auth set with an argument value: exit = %d", code)
	}
	if code, _, _ := runCLI(t, "", "auth", "rm", "--config", dir, "providers.openrouter", "org"); code != 0 {
		t.Fatalf("auth rm key: exit = %d", code)
	}
	_, out, _ = runCLI(t, "", "auth", "list", "--config", dir)
	if strings.Contains(out, "org") {
		t.Errorf("the removed key is still listed: %q", out)
	}
	if code, _, _ := runCLI(t, "", "auth", "rm", "--config", dir, "providers.openrouter"); code != 0 {
		t.Fatalf("auth rm entry: exit = %d", code)
	}
	_, out, _ = runCLI(t, "", "auth", "list", "--config", dir)
	if !strings.Contains(out, "no secrets") {
		t.Errorf("auth list after removal = %q", out)
	}
}

func TestAuthSetRejectsABadSection(t *testing.T) {
	dir := t.TempDir()
	code, _, errOut := runCLI(t, "", "auth", "set", "--config", dir, "models.gemma", "api_key", "x")
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(errOut, "unknown section") {
		t.Errorf("stderr = %q", errOut)
	}
}

// The connectors section of auth.toml: a client secret is stored from stdin,
// and `auth rm connectors.<id>` is the SEC004 fix (docs/connectors.md §9).
func TestAuthConnectorsSection(t *testing.T) {
	dir := t.TempDir()
	const secret = "connector-client-secret-1234"
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(fmt.Sprintf("version = 1\n[server]\nsecrets_dir = %q\n", dir)), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, errOut := runCLI(t, secret+"\n", "auth", "set", "--config", dir, "connectors.github-admin", "client_secret"); code != 0 {
		t.Fatalf("auth set: exit = %d\n%s", code, errOut)
	}
	if code, _, _ := runCLI(t, "", "auth", "set", "--config", dir, "connectors.github-admin", "refresh_token", "refresh-value-5678"); code != 0 {
		t.Fatalf("auth set refresh_token: exit = %d", code)
	}
	_, out, _ := runCLI(t, "", "auth", "list", "--config", dir)
	if !strings.Contains(out, "connectors.github-admin: client_secret, refresh_token") || strings.Contains(out, secret) {
		t.Fatalf("auth list = %q", out)
	}

	if code, _, _ := runCLI(t, "", "auth", "rm", "--config", dir, "connectors.github-admin", "refresh_token"); code != 0 {
		t.Fatalf("auth rm key: exit = %d", code)
	}
	_, out, _ = runCLI(t, "", "auth", "list", "--config", dir)
	if strings.Contains(out, "refresh_token") || !strings.Contains(out, "client_secret") {
		t.Fatalf("auth list after removing a key = %q", out)
	}
	code, out, _ := runCLI(t, "", "auth", "rm", "--config", dir, "connectors.github-admin")
	if code != 0 || !strings.Contains(out, "removed connectors.github-admin") {
		t.Fatalf("auth rm entry: exit = %d, %q", code, out)
	}
	_, out, _ = runCLI(t, "", "auth", "list", "--config", dir)
	if !strings.Contains(out, "no secrets") {
		t.Errorf("auth list after removal = %q", out)
	}
}

// --- backup --------------------------------------------------------------------

func TestBackupAndVerifyRoundTrip(t *testing.T) {
	dir := fixture(t, "minimal")
	out := filepath.Join(t.TempDir(), "snapshot.tar.gz")

	code, stdout, errOut := runCLI(t, "", "backup", "--config", dir, "--out", out)
	if code != 0 {
		t.Fatalf("backup: exit = %d\n%s", code, errOut)
	}
	if strings.TrimSpace(stdout) != out {
		t.Errorf("backup printed %q, want %q", strings.TrimSpace(stdout), out)
	}
	if _, err := os.Stat(out); err != nil {
		t.Fatalf("archive: %v", err)
	}

	code, stdout, errOut = runCLI(t, "", "backup", "verify", out)
	if code != 0 {
		t.Fatalf("verify: exit = %d\n%s", code, errOut)
	}
	if !strings.Contains(stdout, "matches its manifest") {
		t.Errorf("verify = %q", stdout)
	}

	// A truncated archive must not verify.
	broken := filepath.Join(t.TempDir(), "broken.tar.gz")
	body, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(broken, body[:len(body)/2], 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := runCLI(t, "", "backup", "verify", broken); code != 1 {
		t.Errorf("a truncated archive verified: exit = %d", code)
	}
}

// A live daemon is the one thing an offline backup cannot work around: the
// refusal names the fix, and the CLI passes it through unchanged.
func TestBackupRefusesAPopulatedWAL(t *testing.T) {
	dir := fixture(t, "minimal")
	data := filepath.Join(dir, "data")
	if err := os.MkdirAll(data, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"holaclaw.db": "not really sqlite", "holaclaw.db-wal": "pending writes"} {
		if err := os.WriteFile(filepath.Join(data, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	code, _, errOut := runCLI(t, "", "backup", "--config", dir, "--out", filepath.Join(t.TempDir(), "b.tar.gz"))
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(errOut, "stop the daemon first") {
		t.Errorf("stderr = %q", errOut)
	}
}

// --- usage ---------------------------------------------------------------------

func TestUsageAndUnknownCommand(t *testing.T) {
	code, out, errOut := runCLI(t, "")
	if code != 2 || out != "" {
		t.Errorf("no arguments: exit = %d, stdout = %q, want 2 and nothing", code, out)
	}
	// The groups in their order, then the note on where things are found.
	at := 0
	for _, s := range []string{"usage: holaclaw <command>", "daemon:", "chat:", "inspect:", "admin:", "$HOLACLAW_CONFIG", "$HOLACLAW_ADDR"} {
		i := strings.Index(errOut[at:], s)
		if i < 0 {
			t.Fatalf("usage has no %q after offset %d:\n%s", s, at, errOut)
		}
		at += i
	}
	if code, _, errOut = runCLI(t, "", "nope"); code != 2 || !strings.Contains(errOut, `unknown command "nope" for "holaclaw"`) {
		t.Errorf("unknown command: exit = %d, stderr = %q", code, errOut)
	}
	if code, _, errOut = runCLI(t, "", "chekc"); code != 2 || !strings.Contains(errOut, "Did you mean this?\n\tcheck") {
		t.Errorf("a typo: exit = %d, stderr = %q", code, errOut)
	}
}

// Help that was asked for is not an error: it goes to stdout and exits 0.
func TestHelpGoesToStdout(t *testing.T) {
	for _, args := range [][]string{{"help"}, {"-h"}, {"--help"}, {"help", "auth"}, {"auth", "help"}, {"auth", "set", "-h"}} {
		code, out, errOut := runCLI(t, "", args...)
		if code != 0 || errOut != "" || !strings.Contains(out, "usage: holaclaw") {
			t.Errorf("%v: exit = %d, stdout = %q, stderr = %q", args, code, out, errOut)
		}
	}
	for _, args := range [][]string{{"help", "nope"}, {"help", "auth", "nope"}} {
		if code, _, errOut := runCLI(t, "", args...); code != 2 || !strings.Contains(errOut, `unknown command "nope"`) {
			t.Errorf("%v: exit = %d, stderr = %q", args, code, errOut)
		}
	}
}

// A word a group does not know is an error with the group's usage, never its
// help: cobra's own default would print the help and succeed.
func TestUnknownSubcommand(t *testing.T) {
	for _, group := range []string{"auth", "token", "config", "identity", "invite", "memory", "skills", "backup", "completion"} {
		code, out, errOut := runCLI(t, "", group, "bogus")
		if code != 2 || out != "" {
			t.Errorf("%s bogus: exit = %d, stdout = %q", group, code, out)
		}
		if !strings.Contains(errOut, fmt.Sprintf("unknown command %q for %q", "bogus", "holaclaw "+group)) ||
			!strings.Contains(errOut, "usage: holaclaw "+group) {
			t.Errorf("%s bogus: stderr = %q", group, errOut)
		}
	}
	if code, _, errOut := runCLI(t, "", "auth", "lsit"); code != 2 || !strings.Contains(errOut, "Did you mean this?\n") || !strings.Contains(errOut, "\tlist\n") {
		t.Errorf("auth lsit: exit = %d, stderr = %q", code, errOut)
	}
}

// A group that has nothing to do on its own says how it is used, and fails.
func TestGroupWithoutSubcommand(t *testing.T) {
	for _, group := range []string{"auth", "token", "config", "identity", "invite", "completion", "login", "logout"} {
		code, out, errOut := runCLI(t, "", group)
		if code != 2 || out != "" || !strings.HasPrefix(errOut, "usage: holaclaw "+group) {
			t.Errorf("%s: exit = %d, stdout = %q, stderr = %q", group, code, out, errOut)
		}
	}
}

// A wrong flag or a wrong number of arguments is exit 2 with the command's
// usage, before anything runs.
func TestUsageErrors(t *testing.T) {
	for _, c := range []struct {
		args []string
		says string
	}{
		{[]string{"version", "--bogus"}, "unknown flag: --bogus"},
		{[]string{"chat", "-x"}, "unknown shorthand flag: 'x' in -x"},
		{[]string{"run", "-config", "/nowhere"}, "unknown shorthand flag: 'c' in -config"},
		{[]string{"check", "--offline=maybe"}, "invalid argument"},
		{[]string{"version", "extra"}, `unknown command "extra" for "holaclaw version"`},
		{[]string{"init", "extra"}, `unknown command "extra" for "holaclaw init"`},
		{[]string{"login", "whatsapp", "a", "b"}, `unexpected argument "a"; name the instance with --name a`},
		{[]string{"config", "schema"}, "accepts 1 arg(s), received 0"},
		{[]string{"token", "revoke", "a", "b"}, "accepts 1 arg(s), received 2"},
		{[]string{"trace", "a", "b"}, "accepts at most 1 arg(s), received 2"},
		{[]string{"auth", "set", "providers.x"}, "accepts between 2 and 3 arg(s), received 1"},
		{[]string{"auth", "rm"}, "requires at least 1 arg(s), only received 0"},
		{[]string{"token", "create"}, `required flag(s) "name" not set`},
		{[]string{"token", "create", "--name", "x"}, "at least one of the flags in the group [identity binding] is required"},
		{[]string{"token", "set", "x"}, `required flag(s) "scope" not set`},
		{[]string{"identity", "confirm", "x"}, `required flag(s) "name" not set`},
		{[]string{"identity", "set", "x"}, "at least one of the flags in the group [name role] is required"},
		{[]string{"approve", "x", "--session", "--always"}, "[session always] are set none of the others can be"},
		{[]string{"memory", "forget", "x"}, `required flag(s) "partition" not set`},
		{[]string{"skills", "revert", "main", "x", "--to", "0"}, "--to must be a version number"},
	} {
		code, out, errOut := runCLI(t, "", c.args...)
		if code != 2 || out != "" {
			t.Errorf("%v: exit = %d, stdout = %q", c.args, code, out)
		}
		if !strings.HasPrefix(errOut, "holaclaw: ") || !strings.Contains(errOut, c.says) {
			t.Errorf("%v: stderr = %q, want %q", c.args, errOut, c.says)
		}
		path := c.args[0]
		switch c.args[0] {
		case "config", "token", "auth", "identity", "memory", "skills":
			path += " " + c.args[1]
		}
		if !strings.Contains(errOut, "usage: holaclaw "+path) {
			t.Errorf("%v: no usage of %q in stderr = %q", c.args, path, errOut)
		}
	}
}

// Flags may follow the positionals everywhere except where the positionals
// are free text or a secret: there the words are taken as typed.
func TestFlagsAfterTextAreText(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(fmt.Sprintf("version = 1\n[server]\nsecrets_dir = %q\n", dir)), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, errOut := runCLI(t, "", "auth", "set", "--config", dir, "providers.x", "api_key", "-not-a-flag"); code != 0 {
		t.Fatalf("a value that starts with a dash: exit = %d\n%s", code, errOut)
	}
	body, err := os.ReadFile(filepath.Join(dir, "auth.toml"))
	if err != nil || !strings.Contains(string(body), "-not-a-flag") {
		t.Fatalf("auth.toml = %q, %v", body, err)
	}
	// No daemon answers: the dial fails (1), the words are not a bad flag (2).
	code, _, errOut := runCLI(t, "", "memory", "add", "--config", dir, "--partition", "dm/x", "use", "--force", "with", "care")
	if code != 1 || strings.Contains(errOut, "unknown flag") {
		t.Errorf("memory add: exit = %d, stderr = %q", code, errOut)
	}
	// Elsewhere the flags go anywhere.
	if code, _, errOut := runCLI(t, "", "auth", "rm", "providers.x", "--config", dir); code != 0 {
		t.Errorf("auth rm with --config last: exit = %d\n%s", code, errOut)
	}
}

// commandPaths walks the whole tree: every command and subcommand anybody
// can see, as the arguments that reach it.
func commandPaths(t *testing.T) [][]string {
	t.Helper()
	var paths [][]string
	var walk func(cmd *cobra.Command)
	walk = func(cmd *cobra.Command) {
		paths = append(paths, strings.Fields(cmd.CommandPath())[1:])
		for _, sub := range cmd.Commands() {
			if sub.IsAvailableCommand() {
				walk(sub)
			}
		}
	}
	walk(newRoot(&cli{}))
	if len(paths) < 50 {
		t.Fatalf("the tree has only %d commands", len(paths))
	}
	return paths
}

// Every command and subcommand answers --help without doing anything.
func TestEveryCommandHasHelp(t *testing.T) {
	for _, path := range commandPaths(t) {
		code, out, errOut := runCLI(t, "", append(path, "--help")...)
		if code != 0 || errOut != "" {
			t.Errorf("%v --help: exit = %d, stderr = %q", path, code, errOut)
		}
		if !strings.Contains(out, strings.TrimSpace("usage: holaclaw "+strings.Join(path, " "))) {
			t.Errorf("%v --help: stdout = %q", path, out)
		}
	}
}

// The tree docgen renders is the tree help prints, minus what is hidden.
func TestCLITree(t *testing.T) {
	code, out, _ := runCLI(t, "", "__cli-tree")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	var root cliNode
	if err := json.Unmarshal([]byte(out), &root); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	n := 0
	var walk func(cliNode)
	walk = func(c cliNode) {
		n++
		if c.Path != "holaclaw" && c.Short == "" {
			t.Errorf("%s has no summary", c.Path)
		}
		for _, f := range c.Flags {
			if f.Name == "help" || (f.Name == "config" && c.Path != "holaclaw") {
				t.Errorf("%s lists --%s", c.Path, f.Name)
			}
		}
		for _, sub := range c.Commands {
			if strings.Contains(sub.Path, "__") {
				t.Errorf("the hidden %s is in the tree", sub.Path)
			}
			walk(sub)
		}
	}
	walk(root)
	if want := len(commandPaths(t)); n != want {
		t.Errorf("the tree has %d commands, help walks %d", n, want)
	}
	if len(root.Flags) != 1 || root.Flags[0].Name != "config" {
		t.Errorf("root flags = %+v", root.Flags)
	}
	if g := root.Commands[0].Group; g != groupDaemon {
		t.Errorf("first command is in group %q", g)
	}
}
