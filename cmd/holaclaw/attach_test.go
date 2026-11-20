package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"holaclaw/internal/api"
	"holaclaw/internal/api/gen"
	"holaclaw/internal/daemon"
	"holaclaw/internal/event"
	"holaclaw/internal/provider"
	"holaclaw/internal/testmodel"
)

// These tests run the CLI against a real daemon with a scripted model: the
// attached mode of `chat` and the admin commands have no other honest test.

// daemonEnv is a running daemon plus the config directory pointing at it.
type daemonEnv struct {
	dir    string
	socket string
	model  *testmodel.Server
	daemon *daemon.Daemon
}

// startDaemon writes a config directory, runs the daemon in this process and
// waits until its socket answers.
func startDaemon(t *testing.T, tools, policy string, script ...testmodel.Step) *daemonEnv {
	t.Helper()
	return launchDaemon(t, true, tools, policy, script...)
}

// launchDaemon is startDaemon; without owner the database stays as a fresh
// host has it, waiting for `holaclaw init`.
func launchDaemon(t *testing.T, owner bool, tools, policy string, script ...testmodel.Step) *daemonEnv {
	t.Helper()
	model := testmodel.New(t, script...)
	dir := t.TempDir()
	e := &daemonEnv{dir: dir, socket: shortSocket(t), model: model}
	writeDaemonConfig(t, e, tools, policy)
	return runDaemon(t, e, owner)
}

// runDaemon runs the daemon of e.dir in this process and waits until its
// socket answers.
func runDaemon(t *testing.T, e *daemonEnv, owner bool) *daemonEnv {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	d, err := daemon.New(ctx, daemon.Options{
		ConfigDir: e.dir, Debounce: -1,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError})),
		OnListen: func(string, net.Addr) {},
	})
	if err != nil {
		cancel()
		t.Fatalf("daemon.New: %v", err)
	}
	e.daemon = d
	if owner {
		// What `holaclaw init` does on a fresh host.
		if _, err := d.Auth.CreateOwner(ctx, true, nil); err != nil {
			cancel()
			t.Fatalf("owner: %v", err)
		}
	}
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("daemon.Run: %v", err)
			}
		case <-time.After(15 * time.Second):
			t.Error("daemon did not shut down")
		}
	})

	client, err := api.Dial(e.socket, "")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, err := client.Status(context.Background())
		var ce *api.ClientError
		if err == nil || (!owner && errors.As(err, &ce) && ce.Code == api.CodeNotInitialised) {
			return e
		} else if time.Now().After(deadline) {
			t.Fatalf("daemon did not come up: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// shortSocket keeps the socket path well inside the ~107-byte limit.
func shortSocket(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "hb")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	return filepath.Join(d, "s")
}

func writeDaemonConfig(t *testing.T, e *daemonEnv, tools, policy string) {
	t.Helper()
	if tools == "" {
		tools = `["memory"]`
	}
	files := map[string]string{
		"config.toml": fmt.Sprintf(`version = 1

[server]
socket   = %q
data_dir = %q
secrets_dir = %q

[providers.test]
type     = "openai-compatible"
base_url = %q

[models.m]
provider = "test"
id       = "m"
context  = 8192
tools    = true
`, e.socket, filepath.Join(e.dir, "data"), e.dir, e.model.URL()),
		filepath.Join("agents", "main", "agent.toml"): fmt.Sprintf(`version = 1
tools = %s

[models]
chat = "m"

[policy]
%s
`, tools, policy),
		filepath.Join("agents", "main", "prompt.md"): "You are Hola, a test agent.\n",
	}
	for rel, body := range files {
		full := filepath.Join(e.dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
}

// --- chat ----------------------------------------------------------------------

func TestChatOneShotAgainstTheDaemon(t *testing.T) {
	e := startDaemon(t, "", "", testmodel.Step{Text: "the build is green"})
	code, out, errOut := runCLI(t, "", "chat", "--config", e.dir, "-m", "how is the build going")
	if code != 0 {
		t.Fatalf("exit = %d\n%s", code, errOut)
	}
	if !strings.Contains(out, "the build is green") {
		t.Errorf("stdout = %q", out)
	}
	if strings.Contains(errOut, "not running") {
		t.Errorf("it fell back to the in-process mode: %q", errOut)
	}
	if e.model.Calls() != 1 {
		t.Errorf("model calls = %d, want 1", e.model.Calls())
	}
}

func TestChatApprovalPromptAnsweredYes(t *testing.T) {
	e := startDaemon(t, `["files","memory"]`, `write = "ask"`,
		testmodel.Step{ToolCalls: []provider.ToolCall{
			{ID: "c1", Name: "files.write", Arguments: `{"path":"asked.txt","content":"ok"}`},
		}},
		testmodel.Step{Text: "written"},
	)
	code, out, errOut := runCLI(t, "y\n", "chat", "--config", e.dir, "-m", "write asked.txt")
	if code != 0 {
		t.Fatalf("exit = %d\n%s", code, errOut)
	}
	if !strings.Contains(errOut, "[y]es once") {
		t.Errorf("no approval prompt on stderr: %q", errOut)
	}
	if !strings.Contains(out, "written") {
		t.Errorf("stdout = %q", out)
	}
	if _, err := os.Stat(filepath.Join(e.dir, "data", "workspace", "main", "asked.txt")); err != nil {
		t.Errorf("the approved write did not happen: %v", err)
	}
}

func TestChatStartsANewConversationEachTime(t *testing.T) {
	e := startDaemon(t, "", "", testmodel.Step{Text: "first"}, testmodel.Step{Text: "second"})
	if code, _, errOut := runCLI(t, "", "chat", "--config", e.dir, "-m", "hola there"); code != 0 {
		t.Fatalf("first turn: exit = %d\n%s", code, errOut)
	}
	if code, _, errOut := runCLI(t, "", "chat", "--config", e.dir, "-m", "and again"); code != 0 {
		t.Fatalf("second turn: exit = %d\n%s", code, errOut)
	}
	reqs := e.model.Requests()
	if len(reqs) != 2 {
		t.Fatalf("model saw %d requests, want 2", len(reqs))
	}
	if strings.Contains(reqs[1].Text(), "hola there") {
		t.Errorf("a chat with no flags joined the previous conversation:\n%s", reqs[1].Text())
	}
}

// Issue #82: --continue resumes the most recent conversation and --resume
// the one it names; nothing else does.
func TestChatResumeAndContinue(t *testing.T) {
	e := startDaemon(t, "", "",
		testmodel.Step{Text: "one"}, testmodel.Step{Text: "two"},
		testmodel.Step{Text: "continued"}, testmodel.Step{Text: "resumed"})
	_, _, errOut := runCLI(t, "in the first one\n/quit\n", "chat", "--config", e.dir)
	first := chatConversation(t, errOut)
	for _, args := range [][]string{
		{"-m", "in the second one"},
		{"--continue", "-m", "where am I"},
		{"--resume", first, "-m", "back to the first one"},
	} {
		if code, _, errOut := runCLI(t, "", append([]string{"chat", "--config", e.dir}, args...)...); code != 0 {
			t.Fatalf("%v: exit = %d\n%s", args, code, errOut)
		}
	}
	reqs := e.model.Requests()
	if len(reqs) != 4 {
		t.Fatalf("model saw %d requests, want 4", len(reqs))
	}
	if got := reqs[2].Text(); !strings.Contains(got, "in the second one") || strings.Contains(got, "in the first one") {
		t.Errorf("--continue did not resume the most recent conversation:\n%s", got)
	}
	if got := reqs[3].Text(); !strings.Contains(got, "in the first one") || strings.Contains(got, "in the second one") {
		t.Errorf("--resume %s did not join it:\n%s", first, got)
	}
}

// --resume with no conversation asks which one, newest first.
func TestChatResumePicks(t *testing.T) {
	e := startDaemon(t, "", "",
		testmodel.Step{Text: "one"}, testmodel.Step{Text: "two"}, testmodel.Step{Text: "picked"})
	for _, msg := range []string{"in the first one", "in the second one"} {
		if code, _, errOut := runCLI(t, "", "chat", "--config", e.dir, "-m", msg); code != 0 {
			t.Fatalf("%s: exit = %d\n%s", msg, code, errOut)
		}
	}
	code, _, errOut := runCLI(t, "2\n", "chat", "--config", e.dir, "-r", "-m", "back to the first one")
	if code != 0 {
		t.Fatalf("exit = %d\n%s", code, errOut)
	}
	if strings.Index(errOut, "in the second one") > strings.Index(errOut, "in the first one") {
		t.Errorf("the picker did not list the conversations newest first:\n%s", errOut)
	}
	reqs := e.model.Requests()
	if len(reqs) != 3 {
		t.Fatalf("model saw %d requests, want 3", len(reqs))
	}
	if got := reqs[2].Text(); !strings.Contains(got, "in the first one") || strings.Contains(got, "in the second one") {
		t.Errorf("the chat did not resume the conversation picked:\n%s", got)
	}
}

func TestChatResumeRefusesAnUnknownConversation(t *testing.T) {
	e := startDaemon(t, "", "")
	code, _, errOut := runCLI(t, "", "chat", "--config", e.dir, "--resume", "nope", "-m", "hola")
	if code == 0 || !strings.Contains(errOut, `no conversation "nope"`) {
		t.Errorf("exit = %d, stderr = %q", code, errOut)
	}
	if e.model.Calls() != 0 {
		t.Errorf("model calls = %d, want 0", e.model.Calls())
	}
}

func TestChatResumeAndContinueAreExclusive(t *testing.T) {
	e := startDaemon(t, "", "")
	code, _, errOut := runCLI(t, "", "chat", "--config", e.dir, "--resume", "x", "--continue", "-m", "hola")
	if code == 0 || !strings.Contains(errOut, "none of the others can be") {
		t.Errorf("exit = %d, stderr = %q", code, errOut)
	}
}

var bannerConversation = regexp.MustCompile(`conversation (\S+)`)

// chatConversation is the conversation an interactive plain chat says it
// joined.
func chatConversation(t *testing.T, stderr string) string {
	t.Helper()
	m := bannerConversation.FindStringSubmatch(stderr)
	if m == nil {
		t.Fatalf("no conversation in the banner: %q", stderr)
	}
	return m[1]
}

// --- tokens --------------------------------------------------------------------

var tokenID = regexp.MustCompile(`(?m)^id\s+(\S+)`)

func TestTokenCreateListRevoke(t *testing.T) {
	e := startDaemon(t, "", "")

	// Nothing defaults to the owner: a token names the identity it acts as.
	if code, _, _ := runCLI(t, "", "token", "create", "--config", e.dir, "--name", "ci", "--scope", "client"); code == 0 {
		t.Fatal("token create with no --identity or --binding succeeded")
	}
	owner, err := e.daemon.Auth.Owner(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	code, out, errOut := runCLI(t, "", "token", "create", "--config", e.dir, "--name", "holaclaw", "--identity", string(owner.ID))
	if code != 0 {
		t.Fatalf("token create: exit = %d\n%s", code, errOut)
	}
	binding := regexp.MustCompile(`(?m)^binding\s+holaclaw:(\S+)`).FindStringSubmatch(out)
	if binding == nil {
		t.Fatalf("no binding in:\n%s", out)
	}
	m := tokenID.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("no token id in:\n%s", out)
	}
	if !strings.Contains(out, "secret ") || !strings.Contains(out, "shown once") {
		t.Errorf("the secret was not printed once: %q", out)
	}

	code, out, _ = runCLI(t, "", "token", "list", "--config", e.dir)
	if code != 0 {
		t.Fatalf("token list: exit = %d", code)
	}
	if !strings.Contains(out, m[1]) || !strings.Contains(out, "holaclaw:"+binding[1]) || !strings.Contains(out, "active") {
		t.Errorf("token list = %q", out)
	}
	// identity show lists the new binding with its token.
	if _, out, _ = runCLI(t, "", "identity", "show", string(owner.ID), "--config", e.dir); !regexp.MustCompile(`holaclaw:` + binding[1] + `\s+"holaclaw"\s+1 active token`).MatchString(out) {
		t.Errorf("identity show = %q", out)
	}

	// A second token joins the binding, and is raised to admin.
	code, out, errOut = runCLI(t, "", "token", "create", "--config", e.dir, "--name", "backup", "--binding", "holaclaw:"+binding[1])
	if code != 0 || !strings.Contains(out, "holaclaw:"+binding[1]) {
		t.Fatalf("token create --binding: exit = %d\n%s%s", code, out, errOut)
	}
	backup := tokenID.FindStringSubmatch(out)
	if code, out, errOut = runCLI(t, "", "token", "set", "--config", e.dir, backup[1], "--scope", "admin"); code != 0 || !strings.Contains(out, "scope admin") {
		t.Fatalf("token set: exit = %d\n%s%s", code, out, errOut)
	}
	// Rotating prints a new secret and revokes the old token.
	code, out, errOut = runCLI(t, "", "token", "rotate", "--config", e.dir, backup[1])
	if code != 0 || !strings.Contains(out, "revoked "+backup[1]) || !strings.Contains(out, "secret ") {
		t.Fatalf("token rotate: exit = %d\n%s%s", code, out, errOut)
	}
	rotated := tokenID.FindStringSubmatch(out)

	if code, _, errOut = runCLI(t, "", "token", "revoke", "--config", e.dir, m[1]); code != 0 {
		t.Fatalf("token revoke: exit = %d\n%s", code, errOut)
	}
	// Revoked tokens are listed only with --all.
	_, out, _ = runCLI(t, "", "token", "list", "--config", e.dir, "--binding", "holaclaw:"+binding[1])
	listed := func(id string) bool { return regexp.MustCompile(`(?m)^` + id + `\s`).MatchString(out) }
	if listed(m[1]) || listed(backup[1]) || !listed(rotated[1]) ||
		!strings.Contains(out, "rotate · "+backup[1]) {
		t.Errorf("token list = %q", out)
	}
	_, out, _ = runCLI(t, "", "token", "list", "--config", e.dir, "--binding", "holaclaw:"+binding[1], "--all")
	if !strings.Contains(out, m[1]) || !strings.Contains(out, "revoked") || !strings.Contains(out, "token create · "+string(owner.ID)) {
		t.Errorf("token list --all = %q", out)
	}
}

// identity set --role admin names the tokens a promotion left at client.
func TestIdentitySetAdminListsClientTokens(t *testing.T) {
	e := startDaemon(t, "", "")
	ctx := context.Background()
	carla, err := e.daemon.Auth.CreateIdentity(ctx, "carla", "user", "")
	if err != nil {
		t.Fatal(err)
	}
	_, tok, err := e.daemon.Auth.CreateToken(ctx, "phone", "", carla.ID)
	if err != nil {
		t.Fatal(err)
	}
	code, out, errOut := runCLI(t, "", "identity", "set", string(carla.ID), "--role", "admin", "--config", e.dir)
	if code != 0 || !strings.Contains(out, "1 token stays at client scope") || !strings.Contains(out, string(tok.ID)) {
		t.Fatalf("identity set: exit = %d\n%s%s", code, out, errOut)
	}
	// Already an admin: nothing was promoted, so there is nothing to say.
	code, out, errOut = runCLI(t, "", "identity", "set", string(carla.ID), "--role", "admin", "--config", e.dir)
	if code != 0 || strings.Contains(out, "client scope") {
		t.Fatalf("identity set again: exit = %d\n%s%s", code, out, errOut)
	}
}

func TestStatusAndApprovalsAgainstTheDaemon(t *testing.T) {
	e := startDaemon(t, "", "")

	code, out, errOut := runCLI(t, "", "status", "--config", e.dir)
	if code != 0 {
		t.Fatalf("status: exit = %d\n%s", code, errOut)
	}
	if !strings.Contains(out, "version") || !strings.Contains(out, "sessions active") {
		t.Errorf("status = %q", out)
	}

	code, out, _ = runCLI(t, "", "approvals", "--config", e.dir)
	if code != 0 || !strings.Contains(out, "no pending approvals") {
		t.Errorf("approvals: exit = %d, out = %q", code, out)
	}
}

// --- trace ---------------------------------------------------------------------

var traceID = regexp.MustCompile(`trace (\S+)`)

func TestTraceThroughTheDaemonAndFromTheStore(t *testing.T) {
	e := startDaemon(t, "", "", testmodel.Step{Text: "green"})
	code, _, errOut := runCLI(t, "", "chat", "--config", e.dir, "-m", "how is the build", "--trace")
	if code != 0 {
		t.Fatalf("chat: exit = %d\n%s", code, errOut)
	}
	m := traceID.FindStringSubmatch(errOut)
	if m == nil {
		t.Fatalf("no trace id on stderr: %q", errOut)
	}

	code, out, errOut := runCLI(t, "", "trace", "--config", e.dir, m[1])
	if code != 0 {
		t.Fatalf("trace: exit = %d\n%s", code, errOut)
	}
	// run.finished is emitted just after the reply the chat command waits for,
	// so it is not asserted here.
	for _, want := range []string{"channel.received", "model.request", "reply"} {
		if !strings.Contains(out, want) {
			t.Errorf("trace output has no %s:\n%s", want, out)
		}
	}

	code, out, _ = runCLI(t, "", "trace", "--config", e.dir, "--json", m[1])
	if code != 0 {
		t.Fatalf("trace --json: exit = %d", code)
	}
	var tr struct {
		TraceID string `json:"trace_id"`
		Events  []struct {
			Type string `json:"type"`
		} `json:"events"`
	}
	if err := json.Unmarshal([]byte(out), &tr); err != nil {
		t.Fatalf("trace --json is not JSON: %v", err)
	}
	if tr.TraceID != m[1] || len(tr.Events) == 0 {
		t.Errorf("trace = %+v", tr)
	}
	if code, _, _ := runCLI(t, "", "trace", "--config", e.dir, "01ARZ3NDEKTSV4RRFFQ69G5FAV"); code != 1 {
		t.Errorf("unknown trace: exit = %d, want 1", code)
	}
}

// --- memory and users ----------------------------------------------------------

func TestMemoryAddSearchForget(t *testing.T) {
	e := startDaemon(t, "", "")
	const partition = "dm/test"

	code, out, errOut := runCLI(t, "", "memory", "add", "--config", e.dir,
		"--agent", "main", "--partition", partition, "Angel prefers dark roast coffee")
	if code != 0 {
		t.Fatalf("memory add: exit = %d\n%s", code, errOut)
	}
	id := strings.Fields(strings.TrimSpace(out))
	if len(id) < 2 {
		t.Fatalf("memory add output = %q", out)
	}

	code, out, _ = runCLI(t, "", "memory", "search", "--config", e.dir,
		"--agent", "main", "--partition", partition, "coffee")
	if code != 0 {
		t.Fatalf("memory search: exit = %d", code)
	}
	if !strings.Contains(out, "dark roast coffee") {
		t.Errorf("memory search = %q", out)
	}

	code, _, errOut = runCLI(t, "", "memory", "forget", "--config", e.dir,
		"--agent", "main", "--partition", partition, id[1])
	if code != 0 {
		t.Fatalf("memory forget: exit = %d\n%s", code, errOut)
	}
	_, out, _ = runCLI(t, "", "memory", "search", "--config", e.dir,
		"--agent", "main", "--partition", partition, "coffee")
	if strings.Contains(out, "dark roast coffee") {
		t.Errorf("the note survived being forgotten: %q", out)
	}
}

// newIdentity creates a confirmed identity through the CLI and returns its id.
func newIdentity(t *testing.T, e *daemonEnv, name, role string) string {
	t.Helper()
	code, out, errOut := runCLI(t, "", "identity", "create", "--name", name, "--role", role, "--config", e.dir)
	if code != 0 {
		t.Fatalf("identity create: exit = %d\n%s", code, errOut)
	}
	return createdID(t, out)
}

func TestInviteCreate(t *testing.T) {
	e := startDaemon(t, "", "")
	id := newIdentity(t, e, "Carla", "user")
	code, out, errOut := runCLI(t, "", "invite", "create", "--config", e.dir, "--identity", id, "--channel", "holaclaw")
	if code != 0 {
		t.Fatalf("invite create: exit = %d\n%s", code, errOut)
	}
	for _, want := range []string{"code     ", "channel  holaclaw", "scope    client", "expires  ", "redeem   Enter this code in the app"} {
		if !strings.Contains(out, want) {
			t.Errorf("invite create = %q, want %q", out, want)
		}
	}
}

// The flags the CLI can check are checked before anything is sent.
func TestInviteCreateFlagRules(t *testing.T) {
	dir := t.TempDir()
	id := "01ARZ3NDEKTSV4RRFFQ69G5FAV"
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--identity", id}, `required flag(s) "channel" not set`},
		{[]string{"--channel", "tg-main"}, "--identity is required, unless --room"},
		{[]string{"--channel", "tg-main", "--identity", "users/12345"}, "--identity must be an identity id"},
		{[]string{"--channel", "tg-main", "--room", "--identity", id}, "--room takes no --identity or --scope"},
		{[]string{"--channel", "tg-main", "--room", "--scope", "client"}, "--room takes no --identity or --scope"},
		{[]string{"--channel", "tg-main", "--identity", id, "--role", "user"}, "unknown flag: --role"},
	} {
		args := append([]string{"invite", "create", "--config", dir}, tc.args...)
		code, _, errOut := runCLI(t, "", args...)
		if code != 2 || !strings.Contains(errOut, tc.want) {
			t.Errorf("%v: exit = %d, stderr = %q; want 2 and %q", tc.args, code, errOut, tc.want)
		}
	}
}

// What only the server knows is refused there: an unknown identity, a TTL
// above its cap, a scope the role does not allow.
func TestInviteCreateServerRefusals(t *testing.T) {
	e := startDaemon(t, "", "")
	id := newIdentity(t, e, "Carla", "user")
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--channel", "holaclaw", "--identity", "01ARZ3NDEKTSV4RRFFQ69G5FAV"}, "holaclaw:"},
		{[]string{"--channel", "nope", "--identity", id}, `unknown channel "nope"`},
		{[]string{"--channel", "holaclaw", "--identity", id, "--ttl", "25h"}, "ttl_seconds must be between 60 and 86400"},
		{[]string{"--channel", "holaclaw", "--identity", id, "--scope", "admin"}, "has user"},
	} {
		args := append([]string{"invite", "create", "--config", e.dir}, tc.args...)
		code, _, errOut := runCLI(t, "", args...)
		if code != 1 || !strings.Contains(errOut, tc.want) {
			t.Errorf("%v: exit = %d, stderr = %q; want 1 and %q", tc.args, code, errOut, tc.want)
		}
	}
	// A day is fine while the identity has no bindings.
	if code, _, errOut := runCLI(t, "", "invite", "create", "--config", e.dir,
		"--channel", "holaclaw", "--identity", id, "--ttl", "24h"); code != 0 {
		t.Fatalf("--ttl 24h: exit = %d\n%s", code, errOut)
	}
}

func TestApproveUnknownID(t *testing.T) {
	e := startDaemon(t, "", "")
	code, _, errOut := runCLI(t, "", "approve", "--config", e.dir, "01ARZ3NDEKTSV4RRFFQ69G5FAV")
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(errOut, "holaclaw:") {
		t.Errorf("stderr = %q", errOut)
	}
}

// --- no daemon -----------------------------------------------------------------

func TestCommandsSayTheDaemonIsNotRunning(t *testing.T) {
	dir := t.TempDir()
	e := &daemonEnv{dir: dir, socket: shortSocket(t), model: testmodel.New(t)}
	writeDaemonConfig(t, e, "", "")

	for _, args := range [][]string{
		{"status", "--config", dir},
		{"approvals", "--config", dir},
		{"token", "list", "--config", dir},
	} {
		code, _, errOut := runCLI(t, "", args...)
		if code != 1 {
			t.Errorf("%v: exit = %d, want 1", args, code)
		}
		if !strings.Contains(errOut, "daemon is not running") || !strings.Contains(errOut, "holaclaw run") {
			t.Errorf("%v: stderr = %q", args, errOut)
		}
	}
}

func TestChatFallsBackToTheInProcessMode(t *testing.T) {
	model := testmodel.New(t, testmodel.Step{Text: "answered in this process"})
	dir := t.TempDir()
	e := &daemonEnv{dir: dir, socket: shortSocket(t), model: model}
	writeDaemonConfig(t, e, "", "")

	code, out, errOut := runCLI(t, "", "chat", "--config", dir, "-m", "hola", "--trace")
	if code != 0 {
		t.Fatalf("exit = %d\n%s", code, errOut)
	}
	if !strings.Contains(errOut, "daemon is not running") {
		t.Errorf("no fallback notice on stderr: %q", errOut)
	}
	if !strings.Contains(out, "answered in this process") {
		t.Errorf("stdout = %q", out)
	}

	// With no daemon, `trace` falls back to opening the store directly.
	m := traceID.FindStringSubmatch(errOut)
	if m == nil {
		t.Fatalf("no trace id on stderr: %q", errOut)
	}
	code, out, errOut = runCLI(t, "", "trace", "--config", dir, m[1])
	if code != 0 {
		t.Fatalf("trace from the store: exit = %d\n%s", code, errOut)
	}
	if !strings.Contains(out, "model.request") || !strings.Contains(out, "reply") {
		t.Errorf("trace from the store:\n%s", out)
	}
}

// --- progress lines --------------------------------------------------------------

// scriptedDaemon is a stand-in Control API: it answers the four calls `chat`
// makes and replays a fixed event stream for the turn. The real daemon cannot
// produce a browser step without a browser, and the point here is that the
// lines of a long tool call reach the terminal in order.
func scriptedDaemon(t *testing.T, reply string, script []gen.Event) *httptest.Server {
	t.Helper()
	const traceID = "t1"
	sent := make(chan struct{})
	once := sync.Once{}

	mux := http.NewServeMux()
	write := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(v); err != nil {
			t.Errorf("encode: %v", err)
		}
	}
	mux.HandleFunc("/v1/status", func(w http.ResponseWriter, _ *http.Request) {
		write(w, gen.Status{Version: "test"})
	})
	mux.HandleFunc("/v1/sessions", func(w http.ResponseWriter, _ *http.Request) {
		write(w, gen.Session{Id: "s1", Agent: "main", Key: "main/cli/cli"})
	})
	mux.HandleFunc("/v1/sessions/s1/messages", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			toolText, toolName := "one stop, LHR→CDG, 149€", "browser.do"
			write(w, map[string]any{"messages": []gen.Message{
				{Id: "m1", SessionId: "s1", TraceId: traceID, Role: gen.MessageRoleTool,
					ToolName: &toolName, Text: &toolText},
				{Id: "m2", SessionId: "s1", TraceId: traceID, Role: gen.MessageRoleAssistant, Text: &reply},
			}})
			return
		}
		var in gen.MessageSend
		_ = json.NewDecoder(r.Body).Decode(&in)
		once.Do(func() { close(sent) })
		write(w, gen.MessageAccepted{Id: in.Id, TraceId: traceID})
	})
	mux.HandleFunc("/v1/events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		select {
		case <-sent:
		case <-r.Context().Done():
			return
		}
		for _, ev := range script {
			ev.Schema, ev.TraceId, ev.Time = 1, traceID, time.Now()
			b, err := json.Marshal(ev)
			if err != nil {
				t.Errorf("marshal event: %v", err)
				return
			}
			_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
			w.(http.Flusher).Flush()
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// browserTurn is one turn of a browser task as the daemon streams it.
func browserTurn() []gen.Event {
	return []gen.Event{
		progEvent(event.ModelRequest, map[string]any{"step": 1, "model": "cloud", "model_id": "deepseek/deepseek-v4.1-flash"}),
		progEvent(event.ToolCallEvent, map[string]any{"tool": "browser.do", "args": `{"intent":"Go to Google Flights"}`}),
		progEvent(event.BrowserStep, map[string]any{
			"n": 1, "action": "click", "ref": "r5",
			"effect_p": 0.91, "goal_p": 0.12, "took_ms": 329,
		}),
		withDuration(progEvent(event.ToolResult, map[string]any{
			"tool": "browser.do", "is_error": false, "tainted": true,
		}), 65*time.Second),
		progEvent(event.Reply, map[string]any{"text_bytes": 9}),
	}
}

// progressLines is the stderr lines of a run with their elapsed field cut off:
// the clock is real time and only the shape of it is pinned, in
// TestProgressLayout.
func progressLines(errOut string) []string {
	var out []string
	for _, line := range strings.Split(errOut, "\n") {
		r := []rune(line)
		if len(r) > 9 && string(r[:2]) == "· " {
			out = append(out, string(r[9:]))
		}
	}
	return out
}

func TestChatPrintsProgressLinesForATurn(t *testing.T) {
	srv := scriptedDaemon(t, "one stop, 149€", browserTurn())

	code, out, errOut := runCLI(t, "", "chat", "--addr", srv.URL, "-m", "find me a flight", "-vv")
	if code != 0 {
		t.Fatalf("exit = %d\n%s", code, errOut)
	}
	if !strings.Contains(out, "one stop, 149€") {
		t.Errorf("stdout = %q", out)
	}
	want := []string{
		"model step 1 → cloud (deepseek/deepseek-v4.1-flash)",
		"tool browser.do  intent: Go to Google Flights",
		"browser: step 1 click r5 · effect 0.91 · goal 0.12 · 329ms",
		"tool browser.do → ok in 65.0s (tainted)",
	}
	got := progressLines(errOut)
	if len(got) != len(want) {
		t.Fatalf("progress lines =\n%q\nwant\n%q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestChatDefaultVerbosityHidesToolAndBrowserLines: with no -v flag, the
// model's own steps still print, but the tool calls and browser detail
// behind them do not — that is what -v and -vv are for.
func TestChatDefaultVerbosityHidesToolAndBrowserLines(t *testing.T) {
	srv := scriptedDaemon(t, "one stop, 149€", browserTurn())

	code, _, errOut := runCLI(t, "", "chat", "--addr", srv.URL, "-m", "find me a flight")
	if code != 0 {
		t.Fatalf("exit = %d\n%s", code, errOut)
	}
	got := progressLines(errOut)
	want := []string{"model step 1 → cloud (deepseek/deepseek-v4.1-flash)"}
	if len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("progress lines =\n%q\nwant\n%q", got, want)
	}
}

// TestChatVOnlyShowsToolAndBrowserSummaries: -v adds the tool call and its
// result, but not the browser step underneath or the tool's arguments.
func TestChatVOnlyShowsToolAndBrowserSummaries(t *testing.T) {
	srv := scriptedDaemon(t, "one stop, 149€", browserTurn())

	code, _, errOut := runCLI(t, "", "chat", "--addr", srv.URL, "-m", "find me a flight", "-v")
	if code != 0 {
		t.Fatalf("exit = %d\n%s", code, errOut)
	}
	got := progressLines(errOut)
	want := []string{
		"model step 1 → cloud (deepseek/deepseek-v4.1-flash)",
		"tool browser.do",
		"tool browser.do → ok in 65.0s (tainted)",
	}
	if len(got) != len(want) {
		t.Fatalf("progress lines =\n%q\nwant\n%q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestChatVVVPrintsTheFullToolOutput: -vvv is the one level that dumps a
// tool's whole result text (e.g. browser.do's entire output), fetched from
// the transcript rather than carried on the trace event.
func TestChatVVVPrintsTheFullToolOutput(t *testing.T) {
	srv := scriptedDaemon(t, "one stop, 149€", browserTurn())

	code, _, errOut := runCLI(t, "", "chat", "--addr", srv.URL, "-m", "find me a flight", "-vvv")
	if code != 0 {
		t.Fatalf("exit = %d\n%s", code, errOut)
	}
	if !strings.Contains(errOut, "tool output — browser.do:") {
		t.Errorf("stderr missing the tool output heading:\n%s", errOut)
	}
	if !strings.Contains(errOut, "one stop, LHR→CDG, 149€") {
		t.Errorf("stderr missing the full tool text:\n%s", errOut)
	}
}

func TestChatQuietPrintsNothingButTheReply(t *testing.T) {
	srv := scriptedDaemon(t, "one stop, 149€", browserTurn())

	code, out, errOut := runCLI(t, "", "chat", "--addr", srv.URL, "-m", "find me a flight", "-q")
	if code != 0 {
		t.Fatalf("exit = %d\n%s", code, errOut)
	}
	if !strings.Contains(out, "one stop, 149€") {
		t.Errorf("stdout = %q", out)
	}
	if errOut != "" {
		t.Errorf("--quiet wrote to stderr: %q", errOut)
	}
}

// --- the info line and the layout of a turn -----------------------------------

// countedTurn is a turn whose events carry everything the info line is made
// of: the model, the tokens of two model calls, and one tool call.
func countedTurn() []gen.Event {
	return []gen.Event{
		progEvent(event.RunStarted, map[string]any{"agent": "main", "model": "cloud"}),
		progEvent(event.ModelRequest, map[string]any{"step": 1, "model": "cloud"}),
		progEvent(event.ModelResponse, map[string]any{
			"model": "cloud", "prompt_tokens": 1000, "completion_tokens": 100, "tool_calls": 1,
		}),
		progEvent(event.ToolCallEvent, map[string]any{"tool": "browser.do", "args": `{"intent":"Go to Google Flights"}`}),
		progEvent(event.ToolResult, map[string]any{"tool": "browser.do", "is_error": false}),
		progEvent(event.ModelResponse, map[string]any{
			"model": "cloud", "prompt_tokens": 600, "completion_tokens": 118,
		}),
		progEvent(event.Reply, map[string]any{"text_bytes": 9}),
	}
}

var infoLineRe = regexp.MustCompile(`(?m)^· \d+\.\ds · cloud · 1\.6k→218 tokens · 1 tool call(.*)$`)

func TestChatPrintsTheInfoLineAfterAReply(t *testing.T) {
	srv := scriptedDaemon(t, "one stop, 149€", countedTurn())

	code, out, errOut := runCLI(t, "find me a flight\n/quit\n", "chat", "--addr", srv.URL, "--trace")
	if code != 0 {
		t.Fatalf("exit = %d\n%s", code, errOut)
	}
	if !strings.Contains(out, "one stop, 149€") {
		t.Errorf("stdout = %q", out)
	}
	m := infoLineRe.FindStringSubmatch(errOut)
	if m == nil {
		t.Fatalf("no info line on stderr:\n%s", errOut)
	}
	if !strings.Contains(m[1], " · trace t1") {
		t.Errorf("--trace did not put the trace id on the info line: %q", m[0])
	}
	// One blank line before the prompt separates the turns, and the banner
	// says how to leave.
	if !strings.Contains(errOut, "/quit to leave, Ctrl-C aborts a turn") {
		t.Errorf("no banner on stderr:\n%s", errOut)
	}
	if !strings.Contains(errOut, "\n\n> ") {
		t.Errorf("no blank line before the prompt:\n%q", errOut)
	}
}

// Without --trace the same line is printed without a trace id, and --quiet
// prints no line at all.
func TestChatInfoLineIsQuietUnderQuiet(t *testing.T) {
	srv := scriptedDaemon(t, "one stop, 149€", countedTurn())
	code, _, errOut := runCLI(t, "find me a flight\n/quit\n", "chat", "--addr", srv.URL)
	if code != 0 {
		t.Fatalf("exit = %d\n%s", code, errOut)
	}
	if m := infoLineRe.FindStringSubmatch(errOut); m == nil {
		t.Fatalf("no info line on stderr:\n%s", errOut)
	} else if strings.Contains(m[0], "trace") {
		t.Errorf("a trace id without --trace: %q", m[0])
	}

	srv = scriptedDaemon(t, "one stop, 149€", countedTurn())
	code, out, errOut := runCLI(t, "find me a flight\n/quit\n", "chat", "--addr", srv.URL, "-q")
	if code != 0 {
		t.Fatalf("quiet: exit = %d\n%s", code, errOut)
	}
	if !strings.Contains(out, "one stop, 149€") {
		t.Errorf("stdout = %q", out)
	}
	if strings.Contains(errOut, "tokens") {
		t.Errorf("--quiet printed an info line: %q", errOut)
	}
}

// A one-shot run is somebody's script: it prints the reply and the trace id
// it has always printed, and no info line.
func TestChatOneShotOutputIsUnchanged(t *testing.T) {
	srv := scriptedDaemon(t, "one stop, 149€", countedTurn())
	code, out, errOut := runCLI(t, "", "chat", "--addr", srv.URL, "-m", "find me a flight", "--trace")
	if code != 0 {
		t.Fatalf("exit = %d\n%s", code, errOut)
	}
	if out != "one stop, 149€\n" {
		t.Errorf("stdout = %q", out)
	}
	if strings.Contains(errOut, "tokens") {
		t.Errorf("-m printed an info line: %q", errOut)
	}
	if !strings.Contains(errOut, "trace t1\n") {
		t.Errorf("-m --trace = %q", errOut)
	}
}
