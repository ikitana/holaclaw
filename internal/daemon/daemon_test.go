package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"holaclaw/internal/api"
	"holaclaw/internal/api/gen"
	"holaclaw/internal/auth"
	"holaclaw/internal/event"
	"holaclaw/internal/provider"
	"holaclaw/internal/testmodel"
)

// These are the end-to-end tests of the assembled daemon: a real config
// directory, a real store, a scripted model endpoint, and nothing reaching
// inside — every assertion goes through the Control API, which is what a
// client has (ARCHITECTURE.md §2, docs/reference/api.md). Each test stays well under a few
// seconds, so the whole file runs under `go test -race`.

// wait is the deadline every "did it happen yet" helper uses.
const wait = 3 * time.Second

// --- harness -------------------------------------------------------------------

// opts is the shape of the config directory one test needs.
type opts struct {
	tools  string // TOML array literal for agent tools
	policy string // extra lines inside [policy]
	listen bool   // also bind a loopback TCP port
	embed  bool   // give the agent an embed role
}

type env struct {
	t       *testing.T
	dir     string // config directory
	dataDir string
	socket  string
	model   *testmodel.Server
	client  *api.Client
	daemon  *Daemon
	ctx     context.Context
	tcp     chan string
	stop    func()
	logger  *slog.Logger // nil: testLogger
}

// start writes a config directory pointing at a scripted model, launches the
// daemon and returns a client connected to its socket.
func start(t *testing.T, o opts, script ...testmodel.Step) *env {
	t.Helper()
	model := testmodel.New(t, script...)
	dir := t.TempDir()
	e := &env{
		t: t, dir: dir, dataDir: filepath.Join(dir, "data"),
		socket: shortSocket(t), model: model, ctx: context.Background(),
	}
	writeConfig(t, e, o, model.URL())
	e.launch()
	return e
}

// shortSocket returns a socket path well inside the ~107-byte limit: a test
// temp directory is far too deep for a unix socket.
func shortSocket(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "hb")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	return filepath.Join(d, "s")
}

// writeConfig renders the fixed set of config files (ARCHITECTURE.md §4).
func writeConfig(t *testing.T, e *env, o opts, modelURL string) {
	t.Helper()
	listen := ""
	if o.listen {
		listen = "listen   = \"127.0.0.1:0\"\n"
	}
	cfg := fmt.Sprintf(`version = 1

[server]
socket   = %q
data_dir = %q
secrets_dir = %q
%s
[providers.test]
type     = "openai-compatible"
base_url = %q

[models.m]
provider = "test"
id       = "m"
context  = 8192
tools    = true
`, e.socket, e.dataDir, e.dir, listen, modelURL)

	tools := o.tools
	if tools == "" {
		tools = `["memory"]`
	}
	roles := "chat = \"m\"\n"
	if o.embed {
		roles += "embed = \"m\"\n"
	}
	agent := fmt.Sprintf(`version = 1
tools = %s

[models]
%s
[policy]
%s
`, tools, roles, o.policy)

	write(t, filepath.Join(e.dir, "config.toml"), cfg)
	write(t, filepath.Join(e.dir, "agents", "main", "agent.toml"), agent)
	write(t, filepath.Join(e.dir, "agents", "main", "prompt.md"), "You are Hola, a test agent.\n")
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// launch assembles and runs the daemon, and waits until its socket answers.
func (e *env) launch() {
	t := e.t
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	e.tcp = make(chan string, 4)
	logger := e.logger
	if logger == nil {
		logger = testLogger(t)
	}
	d, err := New(ctx, Options{
		ConfigDir: e.dir, Logger: logger, Debounce: -1,
		OnListen: func(kind string, addr net.Addr) {
			if kind == "tcp" {
				select {
				case e.tcp <- addr.String():
				default:
				}
			}
		},
	})
	if err != nil {
		cancel()
		t.Fatalf("daemon.New: %v", err)
	}
	// What `holaclaw init` does on a fresh host; a restart finds the owner there.
	if _, err := d.Auth.CreateOwner(ctx, true, nil); err != nil && !errors.Is(err, auth.ErrInitialised) {
		cancel()
		t.Fatalf("owner: %v", err)
	}
	e.daemon = d
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()

	stopped := false
	e.stop = func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("daemon.Run: %v", err)
			}
		case <-time.After(15 * time.Second):
			t.Error("daemon did not shut down")
		}
	}
	t.Cleanup(e.stop)

	client, err := api.Dial(e.socket, "")
	if err != nil {
		t.Fatalf("api.Dial: %v", err)
	}
	e.client = client
	deadline := time.Now().Add(wait)
	for {
		if _, err := client.Status(context.Background()); err == nil {
			return
		} else if time.Now().After(deadline) {
			t.Fatalf("daemon did not come up: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// tcpAddr returns the bound loopback address (opts.listen).
func (e *env) tcpAddr() string {
	e.t.Helper()
	select {
	case a := <-e.tcp:
		return a
	case <-time.After(wait):
		e.t.Fatal("no tcp listener")
		return ""
	}
}

// testLogger sends daemon logs to the test log and goes quiet once the test
// is over: a goroutine outliving the test must not log into a finished t.
func testLogger(t *testing.T) *slog.Logger {
	w := &testWriter{t: t}
	t.Cleanup(func() {
		w.mu.Lock()
		w.done = true
		w.mu.Unlock()
	})
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

type testWriter struct {
	t    *testing.T
	mu   sync.Mutex
	done bool
}

func (w *testWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.done {
		w.t.Logf("daemon: %s", strings.TrimRight(string(p), "\n"))
	}
	return len(p), nil
}

// --- API helpers ---------------------------------------------------------------

// ownerID is the owner's identity: the one the socket arrives as.
func (e *env) ownerID() string {
	e.t.Helper()
	all, err := e.client.ListIdentities(context.Background(), false)
	if err != nil {
		e.t.Fatalf("list identities: %v", err)
	}
	for _, u := range all {
		if u.Role != nil && *u.Role == gen.UserRoleOwner {
			return u.Id
		}
	}
	e.t.Fatal("no owner")
	return ""
}

func (e *env) session(conversation string) gen.Session {
	e.t.Helper()
	s, err := e.client.CreateSession(e.ctx, gen.SessionCreate{Conversation: &conversation})
	if err != nil {
		e.t.Fatalf("create session: %v", err)
	}
	return s
}

func (e *env) send(sessionID, text string) gen.MessageAccepted {
	e.t.Helper()
	return sendWith(e.t, e.client, sessionID, string(event.NewID()), text)
}

func sendWith(t *testing.T, c *api.Client, sessionID, msgID, text string) gen.MessageAccepted {
	t.Helper()
	acc, err := c.SendMessage(context.Background(), sessionID, gen.MessageSend{Id: msgID, Text: &text})
	if err != nil {
		t.Fatalf("send message: %v", err)
	}
	return acc
}

func (e *env) history(sessionID string) []gen.Message {
	e.t.Helper()
	msgs, err := e.client.ListMessages(e.ctx, sessionID, "", 0)
	if err != nil {
		e.t.Fatalf("list messages: %v", err)
	}
	return msgs
}

// traceTypes is the set of event types recorded for one trace.
func (e *env) traceTypes(traceID string) map[string]int {
	e.t.Helper()
	tr, err := e.client.GetTrace(e.ctx, traceID)
	if err != nil {
		e.t.Fatalf("get trace: %v", err)
	}
	out := map[string]int{}
	for _, ev := range tr.Events {
		out[ev.Type]++
	}
	return out
}

// stream is one SSE subscription drained by a goroutine, so waiting for an
// event type can carry a deadline.
type stream struct {
	t    *testing.T
	ch   chan gen.Event
	seen []string
}

func (e *env) watch(focus string) *stream {
	e.t.Helper()
	return watchWith(e.t, e.client, focus)
}

// watchWith opens a stream with another client, such as a user's token.
func watchWith(t *testing.T, c *api.Client, focus string) *stream {
	t.Helper()
	s, err := c.Events(context.Background(), focus, "")
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	out := &stream{t: t, ch: make(chan gen.Event, 512)}
	go func() {
		for {
			ev, err := s.Next()
			if err != nil {
				close(out.ch)
				return
			}
			select {
			case out.ch <- ev:
			default:
			}
		}
	}()
	return out
}

// await returns the first event of type typ, failing the test on a timeout.
func (s *stream) await(typ event.Type) gen.Event {
	s.t.Helper()
	deadline := time.After(wait)
	for {
		select {
		case ev, ok := <-s.ch:
			if !ok {
				s.t.Fatalf("event stream closed before %s (saw %v)", typ, s.seen)
			}
			s.seen = append(s.seen, ev.Type)
			if ev.Type == string(typ) {
				return ev
			}
		case <-deadline:
			s.t.Fatalf("timed out waiting for %s (saw %v)", typ, s.seen)
		}
	}
}

// attr reads one string attribute of an event.
func attr(t *testing.T, ev gen.Event, key string) string {
	t.Helper()
	if ev.Attrs == nil {
		t.Fatalf("event %s has no attrs", ev.Type)
	}
	v, ok := (*ev.Attrs)[key]
	if !ok {
		t.Fatalf("event %s has no attr %q (%v)", ev.Type, key, *ev.Attrs)
	}
	s, ok := v.(string)
	if !ok {
		t.Fatalf("event %s attr %q is %T", ev.Type, key, v)
	}
	return s
}

func roles(msgs []gen.Message) []string {
	out := make([]string, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, string(m.Role))
	}
	return out
}

func text(m gen.Message) string {
	if m.Text == nil {
		return ""
	}
	return *m.Text
}

// lastAssistant returns the final assistant message, if any.
func lastAssistant(msgs []gen.Message) (gen.Message, bool) {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == gen.MessageRoleAssistant {
			return msgs[i], true
		}
	}
	return gen.Message{}, false
}

func call(id, name, args string) provider.ToolCall {
	return provider.ToolCall{ID: id, Name: name, Arguments: args}
}

// --- (a) one full turn ---------------------------------------------------------

func TestTurnReplyHistoryAndTrace(t *testing.T) {
	e := start(t, opts{}, testmodel.Step{Text: "the build is green"})
	sess := e.session("t1")
	evs := e.watch(sess.Id)

	acc := e.send(sess.Id, "how is the build going")
	reply := evs.await(event.Reply)
	if reply.TraceId != acc.TraceId {
		t.Errorf("reply trace = %s, want %s", reply.TraceId, acc.TraceId)
	}
	// run.finished closes the turn after the reply: wait for it, or the trace
	// below is read while the last hop is still being written.
	evs.await(event.RunFinished)

	msgs := e.history(sess.Id)
	if got := roles(msgs); len(got) != 2 || got[0] != "user" || got[1] != "assistant" {
		t.Fatalf("history roles = %v", got)
	}
	if got := text(msgs[0]); got != "how is the build going" {
		t.Errorf("user text = %q", got)
	}
	if got := text(msgs[1]); got != "the build is green" {
		t.Errorf("assistant text = %q", got)
	}

	types := e.traceTypes(acc.TraceId)
	for _, want := range []event.Type{
		event.ChannelReceived, event.RouterResolved, event.RunStarted, event.ContextBuilt,
		event.ModelRequest, event.ModelResponse, event.Reply, event.RunFinished,
	} {
		if types[string(want)] == 0 {
			t.Errorf("trace is missing %s (have %v)", want, types)
		}
	}
}

// --- (b) a tool turn -----------------------------------------------------------

func TestToolTurnWritesWorkspaceFile(t *testing.T) {
	e := start(t, opts{tools: `["files","memory"]`, policy: `write = "allow"`},
		testmodel.Step{ToolCalls: []provider.ToolCall{call("c1", "files.write", `{"path":"note.txt","content":"hola"}`)}},
		testmodel.Step{Text: "written"},
	)
	sess := e.session("t2")
	evs := e.watch(sess.Id)
	acc := e.send(sess.Id, "write note.txt with hola")
	evs.await(event.Reply)

	body, err := os.ReadFile(filepath.Join(e.dataDir, "workspace", "main", "note.txt"))
	if err != nil {
		t.Fatalf("workspace file: %v", err)
	}
	if string(body) != "hola" {
		t.Errorf("file body = %q", body)
	}

	types := e.traceTypes(acc.TraceId)
	if types[string(event.PolicyDecision)] == 0 {
		t.Errorf("no policy.decision events: %v", types)
	}
	if types[string(event.ToolCallEvent)] == 0 || types[string(event.ToolResult)] == 0 {
		t.Errorf("no tool events: %v", types)
	}

	msgs := e.history(sess.Id)
	var toolMsg *gen.Message
	for i := range msgs {
		if msgs[i].Role == gen.MessageRoleTool {
			toolMsg = &msgs[i]
		}
	}
	if toolMsg == nil {
		t.Fatalf("no tool result message stored: %v", roles(msgs))
	}
	if !strings.Contains(text(*toolMsg), "wrote") {
		t.Errorf("tool result = %q", text(*toolMsg))
	}
}

// --- (c) approvals -------------------------------------------------------------

func TestApprovalApproveOnceCompletesTurn(t *testing.T) {
	e := start(t, opts{tools: `["files","memory"]`, policy: `write = "ask"`},
		testmodel.Step{ToolCalls: []provider.ToolCall{call("c1", "files.write", `{"path":"asked.txt","content":"ok"}`)}},
		testmodel.Step{Text: "written after approval"},
	)
	sess := e.session("t3")
	evs := e.watch(sess.Id)
	e.send(sess.Id, "write asked.txt")

	req := evs.await(event.ApprovalRequested)
	id := attr(t, req, "id")
	if got := attr(t, req, "tool"); got != "files.write" {
		t.Errorf("approval tool = %q", got)
	}
	if _, err := e.client.ResolveApproval(e.ctx, id, gen.ApprovalResolve{
		Approve: true, Choice: ptrOf(gen.ApprovalResolveChoiceOnce),
	}); err != nil {
		t.Fatalf("resolve approval: %v", err)
	}
	evs.await(event.Reply)

	if _, err := os.Stat(filepath.Join(e.dataDir, "workspace", "main", "asked.txt")); err != nil {
		t.Fatalf("approved write did not happen: %v", err)
	}
}

func TestApprovalDenyStopsTheToolCall(t *testing.T) {
	e := start(t, opts{tools: `["files","memory"]`, policy: `write = "ask"`},
		testmodel.Step{ToolCalls: []provider.ToolCall{call("c1", "files.write", `{"path":"denied.txt","content":"no"}`)}},
		testmodel.Step{Text: "understood"},
	)
	sess := e.session("t4")
	evs := e.watch(sess.Id)
	e.send(sess.Id, "write denied.txt")

	req := evs.await(event.ApprovalRequested)
	if _, err := e.client.ResolveApproval(e.ctx, attr(t, req, "id"), gen.ApprovalResolve{Approve: false}); err != nil {
		t.Fatalf("resolve approval: %v", err)
	}
	evs.await(event.Reply)

	if _, err := os.Stat(filepath.Join(e.dataDir, "workspace", "main", "denied.txt")); !os.IsNotExist(err) {
		t.Fatalf("denied write happened anyway (stat err = %v)", err)
	}
	var tool string
	for _, m := range e.history(sess.Id) {
		if m.Role == gen.MessageRoleTool {
			tool = text(m)
		}
	}
	if !strings.Contains(tool, "denied") {
		t.Errorf("tool result = %q, want a denial", tool)
	}
}

// --- (d) exec is denied by default ---------------------------------------------

func TestExecIsDeniedWithoutAsking(t *testing.T) {
	e := start(t, opts{tools: `["exec","memory"]`},
		testmodel.Step{ToolCalls: []provider.ToolCall{call("c1", "exec", `{"command":"echo hi"}`)}},
		testmodel.Step{Text: "cannot run that"},
	)
	sess := e.session("t5")
	evs := e.watch(sess.Id)
	acc := e.send(sess.Id, "run echo hi")
	evs.await(event.Reply)

	pending, err := e.client.ListApprovals(e.ctx)
	if err != nil {
		t.Fatalf("list approvals: %v", err)
	}
	if len(pending) != 0 {
		t.Errorf("exec asked for approval: %+v", pending)
	}
	types := e.traceTypes(acc.TraceId)
	if types[string(event.ApprovalRequested)] != 0 {
		t.Errorf("approval.requested for a denied class: %v", types)
	}
	var tool string
	for _, m := range e.history(sess.Id) {
		if m.Role == gen.MessageRoleTool {
			tool = text(m)
		}
	}
	if !strings.Contains(tool, "denied by policy") {
		t.Errorf("tool result = %q, want a policy denial", tool)
	}
}

// --- (e) memory is saved, then injected into a later session -------------------

func TestMemorySavedIsInjectedIntoALaterSession(t *testing.T) {
	e := start(t, opts{tools: `["memory"]`},
		testmodel.Step{ToolCalls: []provider.ToolCall{
			call("c1", "memory.save", `{"text":"Angel prefers dark roast coffee"}`),
		}},
		testmodel.Step{Text: "noted"},
		testmodel.Step{Text: "dark roast"},
	)
	first := e.session("mem-1")
	evs1 := e.watch(first.Id)
	e.send(first.Id, "please remember that I prefer dark roast coffee")
	evs1.await(event.Reply)

	second := e.session("mem-2")
	evs2 := e.watch(second.Id)
	acc := e.send(second.Id, "which coffee do I prefer these days")
	evs2.await(event.Reply)

	reqs := e.model.Requests()
	if len(reqs) < 3 {
		t.Fatalf("model saw %d requests, want 3", len(reqs))
	}
	last := reqs[len(reqs)-1].Text()
	if !strings.Contains(last, "--- context ---") {
		t.Fatalf("last prompt has no volatile tail:\n%s", last)
	}
	if !strings.Contains(last, "dark roast coffee") {
		t.Errorf("memory was not injected into the prompt:\n%s", last)
	}
	if types := e.traceTypes(acc.TraceId); types[string(event.MemoryInjected)] == 0 {
		t.Errorf("no memory.injected event: %v", types)
	}
}

// --- (f) a crash inside a turn replays on restart ------------------------------

func TestRestartReplaysAnInterruptedTurn(t *testing.T) {
	// The daemon runs in a child process so it can be killed outright: a
	// graceful stop ends the turn row, and only a row left claimed by a crash
	// triggers the requeue this test is about (docs/sessions.md).
	model := testmodel.New(t, testmodel.Step{Text: "too late", Delay: 10 * time.Second})
	dir := t.TempDir()
	e := &env{
		t: t, dir: dir, dataDir: filepath.Join(dir, "data"),
		socket: shortSocket(t), model: model, ctx: context.Background(),
	}
	writeConfig(t, e, opts{}, model.URL())

	child := startChild(t, e.dir)
	client, err := api.Dial(e.socket, "")
	if err != nil {
		t.Fatalf("api.Dial: %v", err)
	}
	e.client = client
	waitUp(t, client)

	sess := e.session("replay")
	e.send(sess.Id, "answer me")

	// Wait until the model call is in flight: the message has left the
	// mailbox, so only the restart's requeue can rescue it.
	deadline := time.Now().Add(wait)
	for e.model.Calls() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("model was never called")
		}
		time.Sleep(10 * time.Millisecond)
	}
	child.kill()

	// A second daemon on the same directories, with a fresh script.
	replacement := testmodel.New(t, testmodel.Step{Text: "answered after the restart"})
	writeConfig(t, e, opts{}, replacement.URL())
	e.model = replacement
	e.launch()

	deadline = time.Now().Add(wait)
	for {
		if m, ok := lastAssistant(e.history(sess.Id)); ok {
			if got := text(m); got != "answered after the restart" {
				t.Fatalf("reply after restart = %q", got)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the interrupted message was never answered: %v", roles(e.history(sess.Id)))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestGracefulShutdownRequeuesTheInterruptedTurn is the same story as the test
// above, but with an orderly stop instead of a crash — and it fails today.
//
// The daemon bug: a cancelled turn still runs its deferred EndTurn (with
// context.WithoutCancel), so the session row is left clean, while the message
// it was answering was consumed by TakePending and the aborted turn delivers
// nothing. HealTurns + RequeueLastUser only rescue a row a crash left claimed,
// so `systemctl restart holaclaw` during a turn silently drops the user's
// message — docs/sessions.md says a restart replays instead of losing input.
//
// The fix belongs in internal/session (not editable from here): requeue the
// pending messages of a turn that ended because the manager was shutting down,
// the way the crash path does.
func TestGracefulShutdownRequeuesTheInterruptedTurn(t *testing.T) {

	e := start(t, opts{}, testmodel.Step{Text: "too late", Delay: 10 * time.Second})
	sess := e.session("graceful")
	e.send(sess.Id, "answer me")

	deadline := time.Now().Add(wait)
	for e.model.Calls() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("model was never called")
		}
		time.Sleep(10 * time.Millisecond)
	}
	e.stop()

	replacement := testmodel.New(t, testmodel.Step{Text: "answered after the restart"})
	writeConfig(t, e, opts{}, replacement.URL())
	e.model = replacement
	e.launch()

	deadline = time.Now().Add(wait)
	for {
		if _, ok := lastAssistant(e.history(sess.Id)); ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the message was dropped by the graceful stop: %v", roles(e.history(sess.Id)))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// --- (g) a token over loopback TCP ---------------------------------------------

func TestTCPTokenChatsButIsNotAdmin(t *testing.T) {
	e := start(t, opts{listen: true}, testmodel.Step{Text: "hello over tcp"})
	addr := e.tcpAddr()

	created, err := e.client.CreateToken(e.ctx, gen.TokenCreate{Name: "cli", Scope: ptrOf(gen.TokenCreateScopeClient), UserId: ptrOf(e.ownerID())})
	if err != nil {
		t.Fatalf("create token: %v", err)
	}
	c, err := api.Dial("http://"+addr, created.Secret)
	if err != nil {
		t.Fatalf("dial tcp: %v", err)
	}

	conversation := "over-tcp"
	sess, err := c.CreateSession(e.ctx, gen.SessionCreate{Conversation: &conversation})
	if err != nil {
		t.Fatalf("create session over tcp: %v", err)
	}
	sendWith(t, c, sess.Id, string(event.NewID()), "hola")

	deadline := time.Now().Add(wait)
	for {
		msgs, err := c.ListMessages(e.ctx, sess.Id, "", 0)
		if err != nil {
			t.Fatalf("list messages over tcp: %v", err)
		}
		if m, ok := lastAssistant(msgs); ok {
			if got := text(m); got != "hello over tcp" {
				t.Errorf("reply = %q", got)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no reply over tcp")
		}
		time.Sleep(20 * time.Millisecond)
	}

	_, err = c.ListApprovals(e.ctx)
	var ce *api.ClientError
	if !errors.As(err, &ce) || ce.Status != 403 {
		t.Fatalf("admin op with a client token: err = %v", err)
	}
}

// A listen address in use ends Run with the bind error, though ctx never
// ends.
func TestBindFailureEndsRun(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = busy.Close() }()
	model := testmodel.New(t)
	dir := t.TempDir()
	e := &env{t: t, dir: dir, dataDir: filepath.Join(dir, "data"), socket: shortSocket(t), model: model}
	writeConfig(t, e, opts{listen: true}, model.URL())
	path := filepath.Join(dir, "config.toml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	write(t, path, strings.Replace(string(raw), `"127.0.0.1:0"`, fmt.Sprintf("%q", busy.Addr().String()), 1))

	d, err := New(context.Background(), Options{ConfigDir: dir, Logger: testLogger(t), Debounce: -1})
	if err != nil {
		t.Fatalf("daemon.New: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- d.Run(context.Background()) }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), busy.Addr().String()) {
			t.Fatalf("Run = %v, want the bind error", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after a bind error")
	}
}

// --- (h) the same message id twice ---------------------------------------------

func TestDuplicateMessageIDAnswersOnce(t *testing.T) {
	e := start(t, opts{}, testmodel.Step{Text: "only once"}, testmodel.Step{Text: "twice would be wrong"})
	sess := e.session("idem")
	evs := e.watch(sess.Id)

	id := string(event.NewID())
	first := sendWith(t, e.client, sess.Id, id, "hola, how are you")
	if first.Duplicate {
		t.Fatal("the first send reported a duplicate")
	}
	second := sendWith(t, e.client, sess.Id, id, "hola, how are you")
	if !second.Duplicate {
		t.Error("the resend was not reported as a duplicate")
	}
	evs.await(event.Reply)

	// Give a second turn the chance to happen before counting.
	time.Sleep(200 * time.Millisecond)
	n := 0
	for _, m := range e.history(sess.Id) {
		if m.Role == gen.MessageRoleAssistant {
			n++
		}
	}
	if n != 1 {
		t.Errorf("assistant replies = %d, want 1 (%v)", n, roles(e.history(sess.Id)))
	}
	if calls := e.model.Calls(); calls != 1 {
		t.Errorf("model calls = %d, want 1", calls)
	}
}

// --- the daemon in a child process ---------------------------------------------

// childEnv marks the re-executed test binary as the daemon child.
const (
	childEnv    = "HOLACLAW_TEST_DAEMON"
	childCfgEnv = "HOLACLAW_TEST_CONFIG"
)

// TestDaemonChild is not a test: when the environment marks it, the test
// binary re-executes itself as a plain daemon, so the parent can SIGKILL it
// mid-turn the way a real crash would.
func TestDaemonChild(t *testing.T) {
	if os.Getenv(childEnv) != "1" {
		t.Skip("helper process, run by TestRestartReplaysAnInterruptedTurn")
	}
	d, err := New(context.Background(), Options{
		ConfigDir: os.Getenv(childCfgEnv), Debounce: -1,
		Logger: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
	})
	if err != nil {
		t.Fatalf("child daemon.New: %v", err)
	}
	// What `holaclaw init` does; a restarted child finds the owner there.
	if _, err := d.Auth.CreateOwner(context.Background(), true, nil); err != nil && !errors.Is(err, auth.ErrInitialised) {
		t.Fatalf("child owner: %v", err)
	}
	if err := d.Run(context.Background()); err != nil {
		t.Fatalf("child daemon.Run: %v", err)
	}
}

type child struct {
	t      *testing.T
	cmd    *exec.Cmd
	killed bool
}

// startChild launches the daemon child and reaps it when the test ends.
func startChild(t *testing.T, configDir string) *child {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=TestDaemonChild", "-test.timeout=60s")
	cmd.Env = append(os.Environ(), childEnv+"=1", childCfgEnv+"="+configDir)
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if err := cmd.Start(); err != nil {
		t.Fatalf("start daemon child: %v", err)
	}
	c := &child{t: t, cmd: cmd}
	t.Cleanup(c.kill)
	return c
}

// kill stops the child the way a crash does: no shutdown, no EndTurn.
func (c *child) kill() {
	if c.killed {
		return
	}
	c.killed = true
	_ = c.cmd.Process.Kill()
	_, _ = c.cmd.Process.Wait()
}

// waitUp blocks until the control socket answers.
func waitUp(t *testing.T, c *api.Client) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := c.Status(context.Background()); err == nil {
			return
		} else if time.Now().After(deadline) {
			t.Fatalf("daemon child did not come up: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// --- small helpers -------------------------------------------------------------

func ptrOf[T any](v T) *T { return &v }
