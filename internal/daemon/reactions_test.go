package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"holaclaw/internal/auth"
	"holaclaw/internal/channel"
	"holaclaw/internal/event"
	"holaclaw/internal/store"
	"holaclaw/internal/store/storetest"
	"holaclaw/internal/tool"
)

// reactChannel is a fakeChannel that takes reactions and numbers what it
// sends "c1:<n>", the way adapters qualify their ids.
type reactChannel struct {
	fakeChannel
	limits  []string
	reactMu sync.Mutex
	n       int
	reacted []event.Origin
	emoji   []string
	prompts int
	choices []string // of the last button prompt
}

func (c *reactChannel) Limits() channel.Limits { return channel.Limits{Reactions: c.limits} }

func (c *reactChannel) Send(ctx context.Context, out channel.Outbound) (channel.Receipt, error) {
	if _, err := c.fakeChannel.Send(ctx, out); err != nil {
		return channel.Receipt{}, err
	}
	c.reactMu.Lock()
	defer c.reactMu.Unlock()
	c.n++
	return channel.Receipt{PlatformMsgIDs: []string{fmt.Sprintf("c1:%d", c.n)}}, nil
}

func (c *reactChannel) React(_ context.Context, target event.Origin, emoji string) error {
	c.reactMu.Lock()
	defer c.reactMu.Unlock()
	c.reacted = append(c.reacted, target)
	c.emoji = append(c.emoji, emoji)
	return nil
}

// buttonChannel is a reactChannel with approval buttons.
type buttonChannel struct{ *reactChannel }

func (c buttonChannel) PromptApproval(_ context.Context, _ event.Origin, _ event.ID, _ string, choices []string) error {
	c.reactMu.Lock()
	defer c.reactMu.Unlock()
	c.prompts++
	c.choices = choices
	return nil
}

// reactRig is a daemon with one channel "fake", an owner linked as fake:1
// with a DM session on it, and a plain user linked as fake:2.
type reactRig struct {
	d    *Daemon
	cs   *channels
	ch   *reactChannel
	sess store.Session
	ev   *recSink
}

type recSink struct {
	mu  sync.Mutex
	evs []event.Event
}

func (r *recSink) Emit(_ context.Context, e event.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.evs = append(r.evs, e)
}

func (r *recSink) of(t event.Type) []event.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []event.Event
	for _, e := range r.evs {
		if e.Type == t {
			out = append(out, e)
		}
	}
	return out
}

func newReactRig(t *testing.T, ch channel.Channel, rc *reactChannel) *reactRig {
	t.Helper()
	d := browserDaemon(t)
	st := d.App.Store
	ev := &recSink{}
	d.App.Events = event.Multi(st, ev)
	d.Auth = auth.New(st)
	ctx := context.Background()
	sess := seedConversation(t, d, store.RoleOwner)
	storetest.Bind(t, st, "fake", []string{"fake:1"}, sess.UserID, "Owner")
	i := store.Identity{ID: event.NewID(), Name: "nina", Role: store.RoleUser, CreatedAt: time.Now().UTC()}
	if err := st.CreateIdentity(ctx, i); err != nil {
		t.Fatal(err)
	}
	storetest.Bind(t, st, "fake", []string{"fake:2"}, i.ID, "Nina")
	return &reactRig{d: d, cs: &channels{d: d, byName: map[string]channel.Channel{"fake": ch}}, ch: rc, sess: sess, ev: ev}
}

// ask raises a tool approval in the session and shows its prompt.
func (rr *reactRig) ask(t *testing.T) tool.Approval {
	t.Helper()
	return rr.raise(t, tool.Approval{Tool: "files.write", Summary: "write a.txt"})
}

// askStep is ask for a browser step: a once-only approval.
func (rr *reactRig) askStep(t *testing.T) tool.Approval {
	t.Helper()
	return rr.raise(t, tool.Approval{Tool: "browser.do", Summary: "browser: a move classified send: click on g4r2", OnceOnly: true})
}

func (rr *reactRig) raise(t *testing.T, a tool.Approval) tool.Approval {
	t.Helper()
	ctx := context.Background()
	// A suspended tool call, as the policy gate raises it; it gives up when the
	// test ends.
	a.ID, a.SessionID, a.Agent, a.TraceID = event.NewID(), rr.sess.ID, "main", event.NewID()
	waitCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = rr.d.Approvals.Request(waitCtx, a)
	}()
	t.Cleanup(func() { cancel(); <-done })
	until(t, "the approval to be pending", func() bool {
		_, err := rr.d.Approvals.Get(ctx, a.ID)
		return err == nil
	})
	attrs, _ := json.Marshal(map[string]any{"id": a.ID})
	rr.cs.prompt(ctx, event.Event{Type: event.ApprovalRequested, TraceID: a.TraceID, SessionID: rr.sess.ID, Attrs: attrs})
	return a
}

func (rr *reactRig) reaction(alias, target, emoji string, removed bool) {
	ctx := context.Background()
	sent, err := rr.d.App.Store.GetSent(ctx, "fake", target)
	if err != nil {
		panic(err)
	}
	env := event.Envelope{
		TraceID:  event.NewID(),
		Origin:   event.Origin{Channel: "fake", Conversation: "c1", ConversationKind: event.ConversationDM, PlatformMsgID: "c1:r" + emoji},
		Sender:   event.Sender{Aliases: []string{alias}},
		Reaction: &event.Reaction{TargetMsgID: target, Emoji: emoji, Removed: removed},
	}
	rr.cs.answerByReaction(ctx, env, sent)
}

func (rr *reactRig) status(t *testing.T, id event.ID) tool.Approval {
	t.Helper()
	a, err := rr.d.Approvals.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// On a channel with reactions and no buttons the prompt is text that says
// how to answer, and its ids are kept as the approval's prompt.
func TestApprovalPromptByReaction(t *testing.T) {
	rc := &reactChannel{}
	rr := newReactRig(t, rc, rc)
	a := rr.ask(t)
	sent := rc.messages()
	if len(sent) != 1 || !strings.HasSuffix(sent[0].Text, "\n\n"+reactToAnswer) || !strings.HasPrefix(sent[0].Text, "Allow: write a.txt?") {
		t.Fatalf("prompt = %+v", sent)
	}
	if rc.targets[0].PlatformMsgID != "" {
		t.Fatal("the prompt was sent as a reply")
	}
	row, err := rr.d.App.Store.GetSent(context.Background(), "fake", "c1:1")
	if err != nil || row.ApprovalID != a.ID || row.MessageID != "" || row.SessionID != rr.sess.ID {
		t.Fatalf("prompt row = %+v, %v", row, err)
	}
}

// Channels with buttons keep them: no text prompt.
func TestApprovalPromptKeepsButtons(t *testing.T) {
	rc := &reactChannel{}
	rr := newReactRig(t, buttonChannel{rc}, rc)
	rr.ask(t)
	if rc.prompts != 1 || len(rc.messages()) != 0 {
		t.Fatalf("buttons %d, texts %+v", rc.prompts, rc.messages())
	}
}

// A once-only approval offers once and deny, as buttons or as reactions, and
// a 🔁 or ✅ it never offered leaves it pending and unanswered.
func TestOnceOnlyApprovalPrompt(t *testing.T) {
	rc := &reactChannel{}
	rr := newReactRig(t, buttonChannel{rc}, rc)
	rr.askStep(t)
	if got := strings.Join(rc.choices, " "); got != "once deny" {
		t.Fatalf("buttons = %q", got)
	}
	rr.ask(t)
	if got := strings.Join(rc.choices, " "); got != "once session always deny" {
		t.Fatalf("buttons = %q", got)
	}

	rc = &reactChannel{}
	rr = newReactRig(t, rc, rc)
	a := rr.askStep(t)
	if sent := rc.messages(); len(sent) != 1 || !strings.HasSuffix(sent[0].Text, "\n\n"+reactToAnswerOnce) {
		t.Fatalf("prompt = %+v", sent)
	}
	rr.reaction("fake:1", "c1:1", "🔁", false)
	rr.reaction("fake:1", "c1:1", "✅", false)
	if got := rr.status(t, a.ID); got.Status != tool.ApprovalPending {
		t.Fatalf("status = %s", got.Status)
	}
	if n := len(rc.messages()); n != 1 {
		t.Fatalf("%d messages sent, want only the prompt", n)
	}
	rr.reaction("fake:1", "c1:1", "👍", false)
	if got := rr.status(t, a.ID); got.Status != tool.ApprovalApproved || got.Choice != tool.GrantOnce {
		t.Fatalf("approval = %+v", got)
	}
	if gs, err := rr.d.Approvals.ListGrants(context.Background(), "main"); err != nil || len(gs) != 0 {
		t.Fatalf("grants = %+v err=%v", gs, err)
	}
}

// A non-admin, an unknown emoji, a removal and a stranger change nothing and
// get no answer. The owner's 👍🏽 approves once and is answered by name as a
// reply to the prompt; a later change finds it settled.
func TestApprovalByReactionRules(t *testing.T) {
	rc := &reactChannel{}
	rr := newReactRig(t, rc, rc)
	a := rr.ask(t)

	rr.reaction("fake:2", "c1:1", "👍", false) // a plain user
	rr.reaction("fake:9", "c1:1", "👍", false) // a stranger
	rr.reaction("fake:1", "c1:1", "🦄", false) // answers nothing
	rr.reaction("fake:1", "c1:1", "", true)   // a removal
	if got := rr.status(t, a.ID); got.Status != tool.ApprovalPending {
		t.Fatalf("status = %s", got.Status)
	}
	if n := len(rc.messages()); n != 1 {
		t.Fatalf("%d messages sent, want only the prompt", n)
	}

	rr.reaction("fake:1", "c1:1", "👍🏽", false)
	got := rr.status(t, a.ID)
	if got.Status != tool.ApprovalApproved || got.Choice != tool.GrantOnce || got.ResolvedBy != rr.sess.UserID {
		t.Fatalf("approval = %+v", got)
	}
	msgs := rc.messages()
	if len(msgs) != 2 || msgs[1].Text != "Approved (once) by owner" || rc.targets[1].PlatformMsgID != "c1:1" {
		t.Fatalf("answer = %+v to %+v", msgs, rc.targets)
	}
	// The prompt and the answer are not transcript messages: no id, and the
	// answer's platform id points at nothing a reaction could quote.
	if msgs[0].ID != "" || msgs[1].ID != "" {
		t.Fatalf("ids = %q, %q", msgs[0].ID, msgs[1].ID)
	}
	if _, err := rr.d.App.Store.GetSent(context.Background(), "fake", "c1:2"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the answer's id was kept: %v", err)
	}

	rr.reaction("fake:1", "c1:1", "👎", false)
	rr.reaction("fake:1", "c1:1", "", true)
	if got := rr.status(t, a.ID); got.Status != tool.ApprovalApproved {
		t.Fatalf("a later reaction changed it: %s", got.Status)
	}
	if n := len(rc.messages()); n != 2 {
		t.Fatalf("%d messages, a later reaction was answered", n)
	}
}

// The answer names the identity as a cleaned label: no markup, one line.
func TestApprovalAnswerNameIsSanitised(t *testing.T) {
	rc := &reactChannel{}
	rr := newReactRig(t, rc, rc)
	if _, err := rr.d.App.Store.DB().Exec(`UPDATE identities SET name = ? WHERE id = ?`, "<b>Angel</b>\n# boss", string(rr.sess.UserID)); err != nil {
		t.Fatal(err)
	}
	rr.ask(t)
	rr.reaction("fake:1", "c1:1", "👍", false)
	if msgs := rc.messages(); len(msgs) != 2 || msgs[1].Text != "Approved (once) by bAngel/b boss" {
		t.Fatalf("answer = %+v", msgs)
	}
}

func TestApprovalByReactionDenyAndScopes(t *testing.T) {
	for emoji, want := range map[string]string{
		"👎":  "Denied by owner",
		"🔁":  "Approved (for this session) by owner",
		"✅":  "Approved (always) by owner",
		"👍️": "Approved (once) by owner", // with U+FE0F
	} {
		rc := &reactChannel{}
		rr := newReactRig(t, rc, rc)
		rr.ask(t)
		rr.reaction("fake:1", "c1:1", emoji, false)
		if msgs := rc.messages(); len(msgs) != 2 || msgs[1].Text != want {
			t.Errorf("%s: %+v", emoji, msgs)
		}
	}
}

func TestReactionChoice(t *testing.T) {
	for in, want := range map[string]string{
		"👍": "once", "👍🏻": "once", "👍🏿": "once", "👍️": "once",
		"🔁": "session", "✅": "always", "👎🏼": "deny",
		"❤️": "", "": "", "👍👍": "", "ok": "",
	} {
		if got := reactionChoice(in); got != want {
			t.Errorf("reactionChoice(%q) = %q, want %q", in, got, want)
		}
	}
}

// The react backend: the target is the turn's inbound message, the emoji
// must be on the channel's list (emoji.Base compared, sent in the list's form), and
// the event names the stored message it lands under.
func TestReactBackend(t *testing.T) {
	rc := &reactChannel{limits: []string{"👍", "❤"}}
	rr := newReactRig(t, rc, rc)
	ctx := context.Background()
	st := rr.d.App.Store
	trace := event.NewID()
	call := tool.Call{TraceID: trace, SessionID: rr.sess.ID, Origin: rr.sess.Origin}

	if err := rr.cs.React(ctx, call, "👍"); err == nil || !strings.Contains(err.Error(), "no message to react to") {
		t.Fatalf("no inbound: err = %v", err)
	}

	st.Emit(ctx, event.New(event.ChannelReceived, trace, "", "", map[string]any{
		"channel": "fake", "conversation": "c1", "platform_msg_id": "c1:77",
	}))
	in := event.Message{Schema: event.SchemaVersion, ID: event.NewID(), SessionID: rr.sess.ID, TraceID: trace,
		Role: event.RoleUser, Text: "deployed?", CreatedAt: time.Now().UTC()}
	if _, err := st.AppendMessage(ctx, in, false); err != nil {
		t.Fatal(err)
	}

	if err := rr.cs.React(ctx, call, "🦄"); err == nil || !strings.Contains(err.Error(), "only these emoji: 👍 ❤") {
		t.Fatalf("outside the list: err = %v", err)
	}
	if len(rc.emoji) != 0 {
		t.Fatal("a refused emoji was sent")
	}
	if err := rr.cs.React(ctx, call, "❤️"); err != nil {
		t.Fatalf("react: %v", err)
	}
	if len(rc.emoji) != 1 || rc.emoji[0] != "❤" || rc.reacted[0].PlatformMsgID != "c1:77" || rc.reacted[0].Conversation != "c1" {
		t.Fatalf("reacted %q on %+v", rc.emoji, rc.reacted)
	}
	evs := rr.ev.of(event.ChannelReacted)
	if len(evs) != 1 || evs[0].SessionID != rr.sess.ID || evs[0].MessageID != in.ID {
		t.Fatalf("channel.reacted = %+v", evs)
	}
	var attrs map[string]any
	_ = json.Unmarshal(evs[0].Attrs, &attrs)
	if attrs["emoji"] != "❤" || attrs["platform_msg_id"] != "c1:77" {
		t.Fatalf("attrs = %v", attrs)
	}
	// A skin tone is matched to its base emoji on the list.
	if err := rr.cs.React(ctx, call, "👍🏽"); err != nil || rc.emoji[len(rc.emoji)-1] != "👍" {
		t.Fatalf("skin tone: err = %v, sent %q", err, rc.emoji)
	}

	// A channel without reactions refuses, and is never offered the tool.
	plain := &channels{d: rr.d, byName: map[string]channel.Channel{"fake": &fakeChannel{}}}
	if err := plain.React(ctx, call, "👍"); err == nil {
		t.Fatal("a channel without Reactor reacted")
	}
	if plain.canReact("fake") || !rr.cs.canReact("fake") || (*channels)(nil).canReact("fake") {
		t.Fatal("canReact")
	}
}

// Delivered replies keep their platform ids, pointing at the message;
// a send outside a session keeps none.
func TestSendKeepsPlatformIDs(t *testing.T) {
	rc := &reactChannel{}
	rr := newReactRig(t, rc, rc)
	ctx := context.Background()
	// The row is there by the time channel.sent goes out.
	atEvent := errors.New("no channel.sent")
	rr.d.App.Events = event.SinkFunc(func(ctx context.Context, e event.Event) {
		if e.Type == event.ChannelSent && atEvent != nil {
			_, atEvent = rr.d.App.Store.GetSent(ctx, "fake", "c1:1")
		}
	})
	msg := event.Message{ID: event.NewID(), SessionID: rr.sess.ID, Role: event.RoleAssistant, Text: "hi"}
	if err := rr.cs.send(ctx, rr.sess.Origin, "", "", msg); err != nil {
		t.Fatal(err)
	}
	if atEvent != nil {
		t.Fatalf("at channel.sent: %v", atEvent)
	}
	row, err := rr.d.App.Store.GetSent(ctx, "fake", "c1:1")
	if err != nil || row.MessageID != msg.ID || row.ApprovalID != "" || row.Conversation != "c1" {
		t.Fatalf("row = %+v, %v", row, err)
	}
	if err := rr.cs.send(ctx, rr.sess.Origin, "", "", event.Message{ID: event.NewID(), Text: "notice"}); err != nil {
		t.Fatal(err)
	}
	if _, err := rr.d.App.Store.GetSent(ctx, "fake", "c1:2"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a sessionless send was kept: %v", err)
	}
}
