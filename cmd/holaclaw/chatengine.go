package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"holaclaw/internal/agent"
	"holaclaw/internal/api"
	"holaclaw/internal/api/gen"
	"holaclaw/internal/app"
	"holaclaw/internal/auth"
	"holaclaw/internal/event"
	"holaclaw/internal/session"
	"holaclaw/internal/store"
	"holaclaw/internal/tool"
	"holaclaw/internal/tool/tools"
)

// One conversation, without a screen. `chat` has two of these — a daemon on
// the other end of the Control API, or the agent loop in this process — and
// two ways to draw them (the line-by-line one in chatplain.go, the full-screen
// one in tui.go). Splitting them here is what lets both frontends drive both
// modes without either knowing which it has.

// engineInfo is what a header or a banner says about a chat.
type engineInfo struct {
	agent        string
	conversation string
	model        string // "" until the first turn names one, in attached mode
	sessionID    string
	attached     bool // true when a daemon is running the turns
}

// turnResult is what one turn came to. The counts are the runner's own and
// are only filled in by the in-process mode; attached mode reads the same
// numbers off the event stream instead.
type turnResult struct {
	traceID  string
	aborted  bool
	streamed bool // the reply arrived token by token
	steps    int
	stop     string
}

// replyInfo is the end of a turn: the text, when it was not streamed, and
// whatever the agent attached to it.
type replyInfo struct {
	text        string
	attachments []attachmentInfo
}

type attachmentInfo struct{ name, mime string }

// histMsg is one message of a conversation that happened before this chat
// opened, for the frontend to show as scrollback.
type histMsg struct {
	role     string // user | assistant | tool | system
	text     string
	toolName string
	traceID  string
	at       time.Time
}

// approvalReq is a tool call waiting on a person, as the frontend needs to
// show it. It is never a tool.Approval: attached mode only ever sees the
// event attributes.
type approvalReq struct {
	id      string // API id; empty in the in-process mode
	tool    string
	summary string
	args    string    // raw JSON, already capped by whoever asked
	expires time.Time // zero when nobody said
	local   bool      // in-process: a choice lasts for this run only
}

// The three things a person can say yes with, as the API spells them.
const (
	choiceOnce    = "once"
	choiceSession = "session"
	choiceAlways  = "always"
)

type approvalAnswer struct {
	approve bool
	choice  string
}

// chatSink is the frontend: everything a turn produces arrives here in the
// order it happened, from whichever goroutine the turn is running on.
type chatSink interface {
	// token is one delta of a streamed reply.
	token(string)
	// event is one trace event of the turn — the agent at work.
	event(gen.Event)
	// note is a short aside from the command itself, not from the agent.
	note(string)
	// reply ends the turn's output.
	reply(replyInfo)
	// approve asks the person and blocks until they answer. A frontend with
	// nobody at it denies.
	approve(context.Context, approvalReq) approvalAnswer
}

// chatEngine is one conversation, either mode.
type chatEngine interface {
	info() engineInfo
	// run sends one message and follows it to the reply.
	run(ctx context.Context, text string, sink chatSink) (turnResult, error)
	// abort asks for the running turn to stop. It is called from another
	// goroutine than run.
	abort(ctx context.Context) error
	// history is the conversation so far.
	history(ctx context.Context) ([]histMsg, error)
	// toolOutputs is the full text of every tool result of one trace — what
	// a progress line never carries. Each result names the call it answered,
	// so a frontend looking at one line can pick out that line's own.
	toolOutputs(ctx context.Context, traceID string) []toolOutput
	// reset moves the chat to a fresh conversation.
	reset(ctx context.Context, conversation string) error
	close() error
}

// --- attached mode --------------------------------------------------------------

// attachedEngine drives one conversation through the Control API: a session,
// one SSE stream for as long as the chat lives, and a turn per message.
type attachedEngine struct {
	client *api.Client
	agent  string

	mu           sync.Mutex
	conversation string
	sess         gen.Session
	stream       *api.EventStream
	events       chan gen.Event
	model        string
}

func newAttachedEngine(ctx context.Context, client *api.Client, agent, conversation string) (*attachedEngine, error) {
	e := &attachedEngine{client: client, agent: agent}
	if err := e.open(ctx, conversation); err != nil {
		return nil, err
	}
	return e, nil
}

// open creates the session and starts pumping its events into a channel, so
// run can select on them alongside everything else it is waiting for.
func (e *attachedEngine) open(ctx context.Context, conversation string) error {
	sess, err := e.client.CreateSession(ctx, gen.SessionCreate{Agent: &e.agent, Conversation: &conversation})
	if err != nil {
		return err
	}
	stream, err := e.client.Events(ctx, sess.Id, "")
	if err != nil {
		return err
	}
	events := make(chan gen.Event, 256)
	go func() {
		defer close(events)
		for {
			ev, err := stream.Next()
			if err != nil {
				return
			}
			events <- ev
		}
	}()

	e.mu.Lock()
	old := e.stream
	e.conversation, e.sess, e.stream, e.events = conversation, sess, stream, events
	e.mu.Unlock()
	if old != nil {
		_ = old.Close()
	}
	return nil
}

func (e *attachedEngine) info() engineInfo {
	e.mu.Lock()
	defer e.mu.Unlock()
	return engineInfo{
		agent: e.agent, conversation: e.conversation, model: e.model,
		sessionID: e.sess.Id, attached: true,
	}
}

func (e *attachedEngine) run(ctx context.Context, text string, sink chatSink) (turnResult, error) {
	e.mu.Lock()
	sess, events := e.sess, e.events
	e.mu.Unlock()

	acc, err := e.client.SendMessage(ctx, sess.Id, gen.MessageSend{Id: string(event.NewID()), Text: &text})
	if err != nil {
		return turnResult{}, err
	}
	res := turnResult{traceID: acc.TraceId}
	if acc.Duplicate {
		sink.note("that message was already sent")
		return res, nil
	}

	for {
		select {
		case <-ctx.Done():
			return res, ctx.Err()
		case ev, ok := <-events:
			if !ok {
				return res, errors.New("the event stream closed before the reply arrived")
			}
			// An approval is the one thing that can arrive on another trace
			// and still be this turn's business.
			if ev.TraceId != acc.TraceId && ev.Type != string(event.ApprovalRequested) {
				continue
			}
			switch event.Type(ev.Type) {
			case event.ModelToken:
				if d := evAttr(ev, "delta"); d != "" {
					res.streamed = true
					sink.token(d)
				}
			case event.ApprovalRequested:
				sink.event(ev)
				if err := e.resolve(ctx, sink, ev); err != nil {
					return res, err
				}
			case event.Reply:
				out := replyInfo{}
				if !res.streamed {
					out.text = e.finalReply(ctx, sess.Id)
				}
				sink.reply(out)
				return res, nil
			case event.RunAborted:
				res.aborted = true
				return res, nil
			default:
				e.remember(ev)
				sink.event(ev)
			}
		}
	}
}

// remember keeps the model a turn actually ran on, so a header can name it
// without the chat having to ask the daemon what the config says.
func (e *attachedEngine) remember(ev gen.Event) {
	var m string
	switch event.Type(ev.Type) {
	case event.RunStarted, event.ModelRequest, event.ModelResponse:
		m = evAttr(ev, "model")
	case event.ModelFallback:
		m = evAttr(ev, "to")
	}
	if m == "" {
		return
	}
	e.mu.Lock()
	e.model = m
	e.mu.Unlock()
}

// resolve asks the frontend about one pending approval and tells the daemon
// the answer.
func (e *attachedEngine) resolve(ctx context.Context, sink chatSink, ev gen.Event) error {
	id := evAttr(ev, "id")
	if id == "" {
		return nil
	}
	req := approvalReq{id: id, tool: evAttr(ev, "tool"), summary: evAttr(ev, "summary"), args: evAttr(ev, "args")}
	if req.summary == "" {
		req.summary = req.tool
	}
	if t, err := time.Parse(time.RFC3339, evAttr(ev, "expires_at")); err == nil {
		req.expires = t
	}
	ans := sink.approve(ctx, req)
	in := gen.ApprovalResolve{Approve: ans.approve}
	if ans.approve {
		in.Choice = ptrOf(gen.ApprovalResolveChoice(ans.choice))
	}
	_, err := e.client.ResolveApproval(ctx, id, in)
	return err
}

// finalReply fetches the last assistant message, for a provider without token
// streaming or a turn that was resumed.
func (e *attachedEngine) finalReply(ctx context.Context, sessionID string) string {
	msgs, err := e.client.ListMessages(ctx, sessionID, "", 0)
	if err != nil {
		return ""
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == gen.MessageRoleAssistant && msgs[i].Text != nil {
			return *msgs[i].Text
		}
	}
	return ""
}

func (e *attachedEngine) abort(ctx context.Context) error {
	e.mu.Lock()
	id := e.sess.Id
	e.mu.Unlock()
	return e.client.AbortSession(ctx, id)
}

func (e *attachedEngine) history(ctx context.Context) ([]histMsg, error) {
	e.mu.Lock()
	id := e.sess.Id
	e.mu.Unlock()
	msgs, err := e.client.ListMessages(ctx, id, "", 0)
	if err != nil {
		return nil, err
	}
	out := make([]histMsg, 0, len(msgs))
	for _, m := range msgs {
		h := histMsg{role: string(m.Role), traceID: m.TraceId, at: m.CreatedAt}
		if m.Text != nil {
			h.text = *m.Text
		}
		if m.ToolName != nil {
			h.toolName = *m.ToolName
		}
		out = append(out, h)
	}
	return out, nil
}

func (e *attachedEngine) toolOutputs(ctx context.Context, traceID string) []toolOutput {
	e.mu.Lock()
	id := e.sess.Id
	e.mu.Unlock()
	msgs, err := e.client.ListMessages(ctx, id, "", 0)
	if err != nil {
		return nil
	}
	var out []toolOutput
	for _, m := range msgs {
		if m.Role != gen.MessageRoleTool || m.TraceId != traceID || m.Text == nil {
			continue
		}
		name, callID := "", ""
		if m.ToolName != nil {
			name = *m.ToolName
		}
		if m.ToolCallId != nil {
			callID = *m.ToolCallId
		}
		out = append(out, toolOutput{name: name, callID: callID, text: *m.Text})
	}
	return out
}

func (e *attachedEngine) reset(ctx context.Context, conversation string) error {
	return e.open(ctx, conversation)
}

func (e *attachedEngine) close() error {
	e.mu.Lock()
	stream := e.stream
	e.stream = nil
	e.mu.Unlock()
	if stream == nil {
		return nil
	}
	return stream.Close()
}

// --- in-process mode -------------------------------------------------------------

// localEngine runs the agent loop in this process, with no session actor: the
// single-user path for trying a config out before there is a daemon. The
// policy gate is the same one the daemon installs — the loop never reaches a
// tool without it (docs/security.md) — with the frontend standing in for the
// approvals framework: decisions live for this run only, nothing is persisted.
type localEngine struct {
	a         *app.App
	runner    *agent.Runner
	agentName string
	workspace string
	grants    *runGrants

	mu           sync.Mutex
	conversation string
	sess         store.Session
	origin       event.Origin
	sink         chatSink
	cancel       context.CancelFunc
}

func newLocalEngine(ctx context.Context, o chatOptions, pick func([]conversationInfo) (string, error)) (*localEngine, error) {
	a, err := app.Open(ctx, o.dir)
	if err != nil {
		return nil, err
	}
	e := &localEngine{a: a, agentName: o.agent, grants: newRunGrants()}

	// Everything the loop, the gate and the browser emit goes to the store and
	// to whoever is drawing this chat, so a local run shows what it is doing.
	a.Events = event.Multi(a.Store, event.SinkFunc(e.emit))

	exec, err := a.Executor(o.agent)
	if err != nil {
		_ = a.Close()
		return nil, err
	}
	if exec != nil {
		// Same guard as the daemon: a local chat is not a way around it.
		g, err := a.Guard(o.agent)
		if err != nil {
			_ = a.Close()
			return nil, err
		}
		exec = tool.NewGate(exec, tool.GateConfig{
			Policy:      a.Policy(o.agent),
			Grants:      e.grants,
			Approver:    e,
			Guard:       g,
			Events:      a.Events,
			ExecCommand: tools.ExecCommand,
		})
	}
	runner, err := a.Runner(o.agent, exec, a.Events)
	if err != nil {
		_ = a.Close()
		return nil, err
	}
	e.runner = runner
	e.workspace = filepath.Join(a.DataDir, "workspace", o.agent)
	conversation, err := chooseConversation(o, func() ([]conversationInfo, error) {
		return localConversations(ctx, a.Store, o.agent)
	}, pick)
	if err != nil {
		_ = a.Close()
		return nil, err
	}
	if err := e.reset(ctx, conversation); err != nil {
		_ = a.Close()
		return nil, err
	}
	return e, nil
}

func (e *localEngine) info() engineInfo {
	e.mu.Lock()
	defer e.mu.Unlock()
	return engineInfo{
		agent: e.agentName, conversation: e.conversation, model: e.runner.Model.ID,
		sessionID: string(e.sess.ID),
	}
}

// reset opens the conversation as the unix socket would: the in-process chat
// runs on the host, so it arrives as holaclaw:cli and its conversations are the
// owner's, the same ones an attached chat sees. Before `holaclaw init` there is
// no owner, and the conversation belongs to nobody.
func (e *localEngine) reset(ctx context.Context, conversation string) error {
	origin := event.Origin{
		Channel: event.ChannelHolaclaw, ChannelType: event.ChannelHolaclaw,
		Conversation: conversation, ConversationKind: event.ConversationDM,
	}
	p, err := auth.New(e.a.Store).SocketPrincipal(ctx)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	sess, _, err := e.a.Store.EnsureSession(ctx, store.Session{
		ID: event.NewID(), Key: session.Key(e.agentName, origin, p.UserID), Agent: e.agentName,
		Kind: store.SessionChat, UserID: p.UserID, Origin: origin,
	})
	if err != nil {
		return err
	}
	e.mu.Lock()
	e.conversation, e.sess, e.origin = conversation, sess, origin
	e.mu.Unlock()
	return nil
}

func (e *localEngine) run(ctx context.Context, text string, sink chatSink) (turnResult, error) {
	e.mu.Lock()
	sess, origin := e.sess, e.origin
	e.sink = sink
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		e.sink = nil
		e.mu.Unlock()
	}()

	// A local turn has no session actor to abort it, so abort cancels the
	// context the runner was handed and nothing else.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	e.mu.Lock()
	e.cancel = cancel
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		e.cancel = nil
		e.mu.Unlock()
	}()

	traceID := event.NewID()
	res := turnResult{traceID: string(traceID)}
	e.a.Store.Emit(ctx, event.New(event.ChannelReceived, traceID, sess.ID, "", map[string]any{"channel": event.ChannelHolaclaw}))
	msg := event.Message{
		Schema: event.SchemaVersion, ID: event.NewID(), SessionID: sess.ID, TraceID: traceID,
		Role: event.RoleUser, Text: text, CreatedAt: time.Now().UTC(),
	}
	if _, err := e.a.Store.AppendMessage(ctx, msg, false); err != nil {
		return res, err
	}
	history, err := e.a.Store.ListMessages(ctx, sess.ID, sess.SummaryMsgID, 0)
	if err != nil {
		return res, err
	}
	call := tool.Call{
		Agent: e.agentName, Role: string(store.RoleOwner), Origin: origin,
		RunKind: tool.RunInteractive, Partition: "cli/" + e.conversation, Workspace: e.workspace,
	}
	sc := call
	sc.TraceID, sc.SessionID = traceID, sess.ID
	out, err := e.runner.Turn(ctx, agent.TurnInput{
		SessionID: sess.ID, TraceID: traceID, History: history,
		Injections: []agent.Injection{{Text: "now: " + time.Now().Format("2006-01-02 15:04 MST")}},
		Call:       call,
		Skills:     app.SkillEntries(e.a.RunSkills(sc)),
		OnText: func(d string) {
			res.streamed = true
			sink.token(d)
		},
	})
	if err != nil {
		if errors.Is(err, context.Canceled) {
			res.aborted = true
			return res, nil
		}
		return res, err
	}
	ri := replyInfo{}
	if !res.streamed {
		ri.text = out.Reply.Text
	}
	for _, at := range out.Reply.Attachments {
		ri.attachments = append(ri.attachments, attachmentInfo{name: at.Name, mime: at.MIME})
	}
	sink.reply(ri)
	res.steps, res.stop = out.Steps, string(out.Stop)
	e.a.Store.Emit(ctx, event.New(event.ChannelSent, traceID, sess.ID, "", map[string]any{"channel": event.ChannelHolaclaw}))
	return res, nil
}

func (e *localEngine) abort(context.Context) error {
	e.mu.Lock()
	cancel := e.cancel
	e.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return nil
}

func (e *localEngine) history(ctx context.Context) ([]histMsg, error) {
	e.mu.Lock()
	id := e.sess.ID
	e.mu.Unlock()
	msgs, err := e.a.Store.ListMessages(ctx, id, "", 0)
	if err != nil {
		return nil, err
	}
	out := make([]histMsg, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, histMsg{
			role: string(m.Role), text: m.Text, toolName: m.ToolName,
			traceID: string(m.TraceID), at: m.CreatedAt,
		})
	}
	return out, nil
}

func (e *localEngine) toolOutputs(ctx context.Context, traceID string) []toolOutput {
	e.mu.Lock()
	id := e.sess.ID
	e.mu.Unlock()
	msgs, err := e.a.Store.ListMessages(ctx, id, "", 0)
	if err != nil {
		return nil
	}
	var out []toolOutput
	for _, m := range msgs {
		if m.Role != event.RoleTool || string(m.TraceID) != traceID {
			continue
		}
		out = append(out, toolOutput{name: m.ToolName, callID: m.ToolCallID, text: m.Text})
	}
	return out
}

func (e *localEngine) close() error { return e.a.Close() }

// emit is the event.Sink half of the engine: what the loop, the gate and the
// browser trace, converted to the API's shape so both frontends read one type.
// The deltas of a streamed reply arrive through OnText instead, and the reply
// itself through sink.reply, so neither is forwarded twice.
func (e *localEngine) emit(_ context.Context, ev event.Event) {
	switch ev.Type {
	case event.ModelToken, event.Reply:
		return
	}
	e.mu.Lock()
	sink := e.sink
	e.mu.Unlock()
	if sink == nil {
		return
	}
	sink.event(genEvent(ev))
}

// Request implements tool.Approver: the human end of the policy gate in local
// mode. There is no approvals table and no timeout here — the frontend denies
// when nobody is there to say yes.
func (e *localEngine) Request(ctx context.Context, a tool.Approval) (tool.Approval, error) {
	a.Status = tool.ApprovalDenied
	e.mu.Lock()
	sink := e.sink
	e.mu.Unlock()
	if sink == nil {
		return a, nil
	}
	ans := sink.approve(ctx, approvalReq{
		tool: a.Tool, summary: a.Summary, args: a.Args, expires: a.ExpiresAt, local: true,
	})
	if !ans.approve {
		return a, nil
	}
	switch ans.choice {
	case choiceOnce:
		a.Status, a.Choice = tool.ApprovalApproved, tool.GrantOnce
	default:
		// "always" is a session grant here: local mode persists nothing.
		a.Status, a.Choice = tool.ApprovalApproved, tool.GrantSession
		e.grants.add(a.Agent, a.Tool, execPattern(a.Args))
	}
	return a, nil
}

// --- the local grants ------------------------------------------------------------

// execPattern is the command an exec approval was about, so a session grant
// covers that command rather than every use of the tool. Anything else has no
// pattern and the grant covers the tool.
func execPattern(args string) string {
	cmd, err := tools.ExecCommand([]byte(args))
	if err != nil {
		return ""
	}
	return cmd
}

// runGrants is the gate's GrantMatcher for one local run: the "session" (and
// "always") choices made at the terminal, held in memory and forgotten when
// the process exits.
type runGrants struct {
	mu   sync.Mutex
	list []tool.Grant
}

func newRunGrants() *runGrants { return &runGrants{} }

func (g *runGrants) add(agent, name, pattern string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.list = append(g.list, tool.Grant{Agent: agent, Tool: name, Pattern: pattern, Scope: tool.GrantSession})
}

// MatchGrant implements tool.GrantMatcher. A grant without a pattern covers
// every call of that tool; one with a pattern covers the commands it prefixes,
// by the same rule the persisted grants use.
func (g *runGrants) MatchGrant(_ context.Context, agent, name, _, _, pattern string) (tool.Grant, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, gr := range g.list {
		if gr.Agent != agent || gr.Tool != name {
			continue
		}
		if gr.Pattern == "" || gr.Pattern == pattern || tool.MatchCommand(gr.Pattern, pattern) {
			return gr, true
		}
	}
	return tool.Grant{}, false
}

// --- shared odds and ends --------------------------------------------------------

// toolOutput is one tool call's full result. The call id is what ties it back
// to the line it came from: a turn holds many of these, and a frontend
// showing one tool line has no business showing the others' output with it.
type toolOutput struct {
	name   string
	callID string
	text   string
}

// ptrOf is the one-liner the generated API types need for their optional
// fields.
func ptrOf[T any](v T) *T { return &v }

// evAttr reads one string attribute of an event.
func evAttr(ev gen.Event, key string) string {
	if ev.Attrs == nil {
		return ""
	}
	if v, ok := (*ev.Attrs)[key].(string); ok {
		return v
	}
	return ""
}

// conversationBase is what a new conversation's key starts with when there
// is no other conversation to name it after.
const conversationBase = "cli"

// conversationSuffix is what a new chat and /new append, so two chats started
// a second apart never land in the same conversation. A base that already
// carries one loses it first: /new in a conversation /new started is a
// sibling of it, not cli-<id>-<id>.
func conversationSuffix(base string) string {
	if head, tail, ok := cutLast(base, "-"); ok && event.ID(strings.ToUpper(tail)).Valid() {
		base = head
	}
	if base == "" {
		base = conversationBase
	}
	return base + "-" + strings.ToLower(string(event.NewID()))
}

func cutLast(s, sep string) (before, after string, ok bool) {
	i := strings.LastIndex(s, sep)
	if i < 0 {
		return s, "", false
	}
	return s[:i], s[i+len(sep):], true
}

// conversationInfo is one conversation a chat could resume, and when it was
// last used. first is its opening message, fetched only for the ones a
// person is asked to choose from.
type conversationInfo struct {
	name  string
	at    time.Time
	first func() string
}

// chooseConversation is the conversation a chat joins: a new one unless
// --resume names one, which must exist, or asks to pick one, or --continue
// asks for the most recent, which falls back to a new one when there is none.
// list is only called when the answer depends on it.
func chooseConversation(o chatOptions, list func() ([]conversationInfo, error),
	pick func([]conversationInfo) (string, error)) (string, error) {
	if o.resume == "" && !o.cont {
		return conversationSuffix(""), nil
	}
	convs, err := list()
	if err != nil {
		return "", err
	}
	// Conversations come oldest first; newest first, a tie keeps the newer
	// session ahead.
	convs = slices.Clone(convs)
	slices.Reverse(convs)
	slices.SortStableFunc(convs, func(a, b conversationInfo) int { return b.at.Compare(a.at) })
	switch o.resume {
	case "":
		if len(convs) == 0 {
			return conversationSuffix(""), nil
		}
		return convs[0].name, nil
	case pickConversation:
		if len(convs) == 0 {
			return "", fmt.Errorf("agent %s has no conversation to resume", o.agent)
		}
		return pick(convs)
	}
	for _, c := range convs {
		if c.name == o.resume {
			return c.name, nil
		}
	}
	return "", fmt.Errorf("agent %s has no conversation %q", o.agent, o.resume)
}

// pickMax is how many conversations the picker offers.
const pickMax = 10

// askConversation lists the most recent conversations, newest first, and
// reads which one to resume: its number, its name, or nothing for the first.
// stdin is read a byte at a time, so a line typed ahead for the chat itself
// stays where the chat will read it.
func askConversation(c *cli, agent string, convs []conversationInfo) (string, error) {
	shown := convs[:min(len(convs), pickMax)]
	_, _ = fmt.Fprintf(c.stderr, "recent conversations of %s:\n", agent)
	for i, cv := range shown {
		first := ""
		if cv.first != nil {
			first = clip(strings.Join(strings.Fields(cv.first()), " "), 60)
		}
		_, _ = fmt.Fprintf(c.stderr, "%3d  %s  %-30s  %s\n", i+1, cv.at.Local().Format("Jan _2 15:04"), cv.name, first)
	}
	_, _ = fmt.Fprint(c.stderr, "resume which? [1] ")
	line, err := readLine(c.stdin)
	if err != nil && line == "" {
		return "", errors.New("no conversation chosen")
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return shown[0].name, nil
	}
	if n, err := strconv.Atoi(line); err == nil {
		if n < 1 || n > len(shown) {
			return "", fmt.Errorf("no conversation number %d", n)
		}
		return shown[n-1].name, nil
	}
	for _, cv := range convs {
		if cv.name == line {
			return cv.name, nil
		}
	}
	return "", fmt.Errorf("agent %s has no conversation %q", agent, line)
}

// readLine reads up to and without a newline, one byte at a time.
func readLine(r io.Reader) (string, error) {
	var b strings.Builder
	buf := make([]byte, 1)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if buf[0] == '\n' {
				return b.String(), nil
			}
			b.WriteByte(buf[0])
		}
		if err != nil {
			return b.String(), err
		}
	}
}

// firstMessages is how far into a transcript the picker looks for the
// opening user message.
const firstMessages = 20

// attachedConversations is the agent's chat conversations on the Control
// API, which is where every attached chat lives, oldest first. The daemon
// moves updated_at on every turn.
func attachedConversations(ctx context.Context, client *api.Client, agent string) ([]conversationInfo, error) {
	sessions, err := client.ListSessions(ctx)
	if err != nil {
		return nil, err
	}
	var out []conversationInfo
	for _, s := range sessions {
		if s.Agent != agent || s.Kind != gen.SessionKindChat ||
			s.Origin.Channel != event.ChannelHolaclaw || s.Origin.Conversation == "" {
			continue
		}
		id := s.Id
		out = append(out, conversationInfo{name: s.Origin.Conversation, at: s.UpdatedAt, first: func() string {
			msgs, err := client.ListMessages(ctx, id, "", firstMessages)
			if err != nil {
				return ""
			}
			for _, m := range msgs {
				if m.Role == gen.MessageRoleUser && m.Text != nil {
					return *m.Text
				}
			}
			return ""
		}})
	}
	return out, nil
}

// localConversations is attachedConversations for the in-process mode, whose
// sessions are on the holaclaw channel. Nothing here moves updated_at on a turn,
// so a session's last trace is what says when it was last used.
func localConversations(ctx context.Context, st store.Store, agent string) ([]conversationInfo, error) {
	sessions, err := st.ListSessions(ctx)
	if err != nil {
		return nil, err
	}
	traces, _ := st.(store.TraceLister)
	var out []conversationInfo
	for _, s := range sessions {
		if s.Agent != agent || s.Kind != store.SessionChat ||
			s.Origin.Channel != event.ChannelHolaclaw || s.Origin.Conversation == "" {
			continue
		}
		at := s.UpdatedAt
		if traces != nil {
			ts, err := traces.ListTraces(ctx, s.ID)
			if err != nil {
				return nil, err
			}
			// Newest first.
			if len(ts) > 0 && ts[0].EndedAt.After(at) {
				at = ts[0].EndedAt
			}
		}
		id := s.ID
		out = append(out, conversationInfo{name: s.Origin.Conversation, at: at, first: func() string {
			msgs, err := st.ListMessages(ctx, id, "", firstMessages)
			if err != nil {
				return ""
			}
			for _, m := range msgs {
				if m.Role == event.RoleUser {
					return m.Text
				}
			}
			return ""
		}})
	}
	return out, nil
}
