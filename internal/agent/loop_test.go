package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"holaclaw/internal/event"
	"holaclaw/internal/provider"
	"holaclaw/internal/tool"
)

func wantTypes(t *testing.T, got []event.Type, want ...event.Type) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("event types\n got %v\nwant %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("event types\n got %v\nwant %v", got, want)
		}
	}
}

func lastReq(g *rig) provider.Request { return g.prov.reqs[len(g.prov.reqs)-1] }

func TestTurnPlainReply(t *testing.T) {
	g := newRig(t, textStep("hi there"))
	g.prov.steps[0].resp.Usage = provider.Usage{PromptTokens: 10, CompletionTokens: 3}
	var streamed strings.Builder
	res, err := g.r.Turn(context.Background(), TurnInput{
		SessionID: "S1", TraceID: "T1",
		History: []event.Message{{ID: "M1", Role: event.RoleUser, Text: "hello"}},
		OnText:  func(d string) { streamed.WriteString(d) },
	})
	if err != nil {
		t.Fatalf("Turn: %v", err)
	}
	if res.Stop != StopFinal || res.Steps != 1 {
		t.Fatalf("stop=%q steps=%d", res.Stop, res.Steps)
	}
	if res.Reply.Text != "hi there" || streamed.String() != "hi there" {
		t.Fatalf("reply=%q streamed=%q", res.Reply.Text, streamed.String())
	}
	if res.Usage != (event.Usage{PromptTokens: 10, CompletionTokens: 3, UnpricedCalls: 1}) {
		t.Fatalf("usage %+v", res.Usage)
	}
	if len(res.New) != 1 || len(g.tr.msgs) != 1 {
		t.Fatalf("appended %d messages, stored %d", len(res.New), len(g.tr.msgs))
	}
	m := res.New[0]
	if m.ID == "" || m.TraceID != "T1" || m.SessionID != "S1" || m.Schema != event.SchemaVersion || m.CreatedAt.IsZero() {
		t.Fatalf("message not stamped: %+v", m)
	}
	wantTypes(t, g.ev.types(),
		event.RunStarted, event.ContextBuilt, event.ModelRequest, event.ModelResponse,
		event.Reply, event.RunFinished)
}

func TestTurnToolCallEventSequence(t *testing.T) {
	g := newRig(t,
		callStep(call("c1", "echo", `{"text":"x"}`)),
		textStep("done"),
	).withTools(spec("echo", objectSchema))

	res, err := g.turn(context.Background())
	if err != nil {
		t.Fatalf("Turn: %v", err)
	}
	wantTypes(t, g.ev.types(),
		event.RunStarted, event.ContextBuilt, event.ModelRequest, event.ModelResponse,
		event.ToolCallEvent, event.ToolResult,
		event.ContextBuilt, event.ModelRequest, event.ModelResponse,
		event.Reply, event.RunFinished)

	if got := g.tools.names(); len(got) != 1 || got[0] != "echo" {
		t.Fatalf("executed %v", got)
	}
	c := g.tools.calls[0]
	if c.TraceID != "T1" || c.SessionID != "S1" || c.Agent != "test" || c.Workspace != "/ws" || c.CallID != "c1" {
		t.Fatalf("trusted call fields not copied: %+v", c)
	}
	if len(res.New) != 3 {
		t.Fatalf("new messages %d", len(res.New))
	}
	if res.New[0].Role != event.RoleAssistant || len(res.New[0].ToolCalls) != 1 {
		t.Fatalf("assistant tool-call message: %+v", res.New[0])
	}
	if res.New[1].Role != event.RoleTool || res.New[1].ToolCallID != "c1" || res.New[1].ToolName != "echo" {
		t.Fatalf("tool message: %+v", res.New[1])
	}
	if res.New[2].Text != "done" || res.Reply.ID != res.New[2].ID {
		t.Fatalf("reply: %+v", res.New[2])
	}
	// The second request carries the assistant call and its result back.
	if n := len(lastReq(g).Messages); n != 3 {
		t.Fatalf("second request messages: %d, want user+assistant+tool", n)
	}
}

func TestEventChainParentsAndIDs(t *testing.T) {
	g := newRig(t, callStep(call("c1", "echo", `{}`)), textStep("ok")).
		withTools(spec("echo", objectSchema))
	if _, err := g.turn(context.Background()); err != nil {
		t.Fatalf("Turn: %v", err)
	}
	root, ok := g.ev.first(event.RunStarted)
	if !ok || root.ParentID != "" {
		t.Fatalf("run.started missing or not root: %+v", root)
	}
	for _, e := range g.ev.evs {
		if e.TraceID != "T1" || e.SessionID != "S1" {
			t.Fatalf("%s: trace/session ids not set: %+v", e.Type, e)
		}
		if e.Type == event.RunStarted {
			continue
		}
		if e.ParentID != root.SpanID {
			t.Fatalf("%s: parent %q, want run span %q", e.Type, e.ParentID, root.SpanID)
		}
	}
	rep, _ := g.ev.first(event.Reply)
	if rep.MessageID == "" {
		t.Fatalf("reply event has no message id")
	}
	tc, _ := g.ev.first(event.ToolCallEvent)
	a := g.ev.attrs(t, tc)
	if a["tool"] != "echo" || a["args"] != "{}" || a["args_bytes"] != float64(2) {
		t.Fatalf("tool.call attrs: %v", a)
	}
	tr, _ := g.ev.first(event.ToolResult)
	if g.ev.attrs(t, tr)["is_error"] != false {
		t.Fatalf("tool.result attrs: %v", g.ev.attrs(t, tr))
	}
	cb, _ := g.ev.first(event.ContextBuilt)
	cba := g.ev.attrs(t, cb)
	if cba["prefix_hash"] != "deadbeef" || cba["message_ids"] == nil {
		t.Fatalf("context.built attrs: %v", cba)
	}
}

func TestToolCallArgsAttrCapped(t *testing.T) {
	big := `{"text":"` + strings.Repeat("a", 4000) + `"}`
	g := newRig(t, callStep(call("c1", "echo", big)), textStep("ok")).
		withTools(spec("echo", objectSchema))
	if _, err := g.turn(context.Background()); err != nil {
		t.Fatalf("Turn: %v", err)
	}
	tc, _ := g.ev.first(event.ToolCallEvent)
	got, _ := g.ev.attrs(t, tc)["args"].(string)
	if len(got) > maxArgsAttr+4 {
		t.Fatalf("args attr not capped: %d bytes", len(got))
	}
}

func TestStepLimitFinalAnswer(t *testing.T) {
	g := newRig(t,
		callStep(call("c1", "echo", `{"i":1}`)),
		callStep(call("c2", "echo", `{"i":2}`)),
		textStep("final answer"),
	).withTools(spec("echo", objectSchema))
	g.r.Def.MaxSteps = 2

	res, err := g.turn(context.Background())
	if err != nil {
		t.Fatalf("Turn: %v", err)
	}
	if res.Stop != StopStepLimit {
		t.Fatalf("stop %q", res.Stop)
	}
	if g.ev.count(event.RunStepLimit) != 1 {
		t.Fatalf("run.step_limit events: %d", g.ev.count(event.RunStepLimit))
	}
	last := lastReq(g)
	if len(last.Tools) != 0 {
		t.Fatalf("tools still offered on the last call: %v", last.Tools)
	}
	if !strings.Contains(last.Messages[len(last.Messages)-1].Text(), "Step limit reached") {
		t.Fatalf("no step-limit note in prompt: %+v", last.Messages)
	}
	if res.Reply.Text != "final answer" {
		t.Fatalf("reply %q", res.Reply.Text)
	}
}

func TestRepeatedCallDetector(t *testing.T) {
	g := newRig(t,
		callStep(call("c1", "echo", `{"a":1,"b":2}`)),
		callStep(call("c2", "echo", `{"b":2,"a":1}`)), // same args, other key order
		callStep(call("c3", "echo", `{"a":1,"b":2}`)),
		textStep("answering"),
	).withTools(spec("echo", objectSchema))

	res, err := g.turn(context.Background())
	if err != nil {
		t.Fatalf("Turn: %v", err)
	}
	if n := len(g.tools.calls); n != 2 {
		t.Fatalf("executed %d calls, want 2", n)
	}
	if g.ev.count(event.RunLoopDetected) != 1 {
		t.Fatalf("run.loop_detected: %d", g.ev.count(event.RunLoopDetected))
	}
	var rejected string
	for _, m := range res.New {
		if m.Role == event.RoleTool && strings.Contains(m.Text, "You already called") {
			rejected = m.Text
		}
	}
	if !strings.Contains(rejected, "echo") || !strings.Contains(rejected, "2 times") {
		t.Fatalf("feedback message: %q", rejected)
	}
	if res.Stop != StopFinal {
		t.Fatalf("stop %q", res.Stop)
	}
}

func TestRepeatedCallTwiceStopsTurn(t *testing.T) {
	g := newRig(t,
		callStep(call("c1", "echo", `{"a":1}`)),
		callStep(call("c2", "echo", `{"a":1}`)),
		callStep(call("c3", "echo", `{"a":1}`)),
		callStep(call("c4", "echo", `{"a":1}`)),
		textStep("giving up"),
	).withTools(spec("echo", objectSchema))

	res, err := g.turn(context.Background())
	if err != nil {
		t.Fatalf("Turn: %v", err)
	}
	if res.Stop != StopLoopDetected {
		t.Fatalf("stop %q", res.Stop)
	}
	if n := len(g.tools.calls); n != 2 {
		t.Fatalf("executed %d calls, want 2", n)
	}
	if len(lastReq(g).Tools) != 0 {
		t.Fatalf("tools still offered")
	}
}

func TestUnknownToolIsRejected(t *testing.T) {
	g := newRig(t,
		callStep(call("c1", "nope", `{}`)),
		textStep("sorry"),
	).withTools(spec("echo", objectSchema), spec("fetch", objectSchema))

	res, err := g.turn(context.Background())
	if err != nil {
		t.Fatalf("Turn: %v", err)
	}
	if len(g.tools.calls) != 0 {
		t.Fatalf("unknown tool was executed")
	}
	fb := res.New[1]
	if fb.Role != event.RoleTool || !strings.Contains(fb.Text, "echo") || !strings.Contains(fb.Text, "fetch") {
		t.Fatalf("feedback: %+v", fb)
	}
	rej, ok := g.ev.first(event.ToolRejected)
	if !ok || g.ev.attrs(t, rej)["reason"] != "unknown_tool" {
		t.Fatalf("tool.rejected: %+v", rej)
	}
}

func TestNonObjectArgumentsRejected(t *testing.T) {
	g := newRig(t,
		callStep(call("c1", "echo", `"just a string"`)),
		textStep("ok"),
	).withTools(spec("echo", objectSchema))

	res, err := g.turn(context.Background())
	if err != nil {
		t.Fatalf("Turn: %v", err)
	}
	if len(g.tools.calls) != 0 {
		t.Fatalf("invalid call was executed")
	}
	if !strings.Contains(res.New[1].Text, "must be a JSON object") {
		t.Fatalf("feedback: %q", res.New[1].Text)
	}
	rej, _ := g.ev.first(event.ToolRejected)
	if g.ev.attrs(t, rej)["reason"] != "bad_arguments" {
		t.Fatalf("reason: %v", g.ev.attrs(t, rej))
	}
}

func TestSchemaViolationFedBack(t *testing.T) {
	s := spec("echo", `{"type":"object","properties":{"text":{"type":"string"}},"required":["text"]}`)
	g := newRig(t,
		callStep(call("c1", "echo", `{"other":1}`)),
		textStep("ok"),
	).withTools(s)

	res, err := g.turn(context.Background())
	if err != nil {
		t.Fatalf("Turn: %v", err)
	}
	if len(g.tools.calls) != 0 {
		t.Fatalf("invalid call was executed")
	}
	if !strings.Contains(res.New[1].Text, `"text"`) {
		t.Fatalf("feedback does not name the field: %q", res.New[1].Text)
	}
	rej, _ := g.ev.first(event.ToolRejected)
	if g.ev.attrs(t, rej)["reason"] != "schema" {
		t.Fatalf("reason: %v", g.ev.attrs(t, rej))
	}
}

func TestThreeInvalidCallsWithdrawTools(t *testing.T) {
	g := newRig(t,
		callStep(call("c1", "nope", `{}`)),
		callStep(call("c2", "nope", `{}`)),
		callStep(call("c3", "nope", `{}`)),
		textStep("plain answer"),
	).withTools(spec("echo", objectSchema))

	res, err := g.turn(context.Background())
	if err != nil {
		t.Fatalf("Turn: %v", err)
	}
	if len(g.tools.calls) != 0 {
		t.Fatalf("executor was reached")
	}
	if g.ev.count(event.ToolRejected) != 3 {
		t.Fatalf("tool.rejected: %d", g.ev.count(event.ToolRejected))
	}
	if len(lastReq(g).Tools) != 0 || res.Stop != StopStepLimit {
		t.Fatalf("tools not withdrawn (stop=%q)", res.Stop)
	}
}

func TestRepairedCallEmitsEvent(t *testing.T) {
	st := callStep(call("c1", "echo", `{"a":1}`))
	st.resp.Repairs = map[string]string{"c1": "trailing-comma"}
	g := newRig(t, st, textStep("ok")).withTools(spec("echo", objectSchema))
	if _, err := g.turn(context.Background()); err != nil {
		t.Fatalf("Turn: %v", err)
	}
	e, ok := g.ev.first(event.ToolRepaired)
	if !ok || g.ev.attrs(t, e)["stage"] != "trailing-comma" {
		t.Fatalf("tool.repaired: %+v", e)
	}
}

func TestToolErrorIsFedBack(t *testing.T) {
	g := newRig(t, callStep(call("c1", "echo", `{}`)), textStep("recovered")).
		withTools(spec("echo", objectSchema))
	g.tools.fn = func(tool.Call) (tool.Result, error) { return tool.Result{}, errors.New("boom") }

	res, err := g.turn(context.Background())
	if err != nil {
		t.Fatalf("Turn: %v", err)
	}
	if !strings.Contains(res.New[1].Text, "boom") {
		t.Fatalf("feedback: %q", res.New[1].Text)
	}
	tr, _ := g.ev.first(event.ToolResult)
	if g.ev.attrs(t, tr)["is_error"] != true {
		t.Fatalf("tool.result not marked as error")
	}
}

func TestTaintSticksToLaterCalls(t *testing.T) {
	g := newRig(t,
		callStep(call("c1", "fetch", `{}`)),
		callStep(call("c2", "echo", `{}`)),
		textStep("done"),
	).withTools(spec("fetch", objectSchema), spec("echo", objectSchema))
	g.tools.fn = func(c tool.Call) (tool.Result, error) {
		return tool.Result{Text: "x", Tainted: c.Name == "fetch"}, nil
	}

	res, err := g.turn(context.Background())
	if err != nil {
		t.Fatalf("Turn: %v", err)
	}
	if g.tools.calls[0].Tainted {
		t.Fatalf("first call already tainted")
	}
	if !g.tools.calls[1].Tainted {
		t.Fatalf("taint did not propagate to the second call")
	}
	if !res.New[1].Tainted {
		t.Fatalf("tool message not marked tainted")
	}
	if !res.Reply.Tainted {
		t.Fatalf("reply not marked tainted")
	}
}

// Taint decays: only the configured number of calls after a tainted result
// still count as tainted, not every later call in the turn (docs/security.md) — otherwise one tainted fetch would force every later exec/external
// call to Ask for the rest of the turn.
func TestTaintDecays(t *testing.T) {
	g := newRig(t,
		callStep(call("c1", "fetch", `{}`)),
		callStep(call("c2", "echo", `{"n":1}`)),
		callStep(call("c3", "echo", `{"n":2}`)),
		callStep(call("c4", "echo", `{"n":3}`)),
		textStep("done"),
	).withTools(spec("fetch", objectSchema), spec("echo", objectSchema))
	g.r.TaintDecay = 2
	g.tools.fn = func(c tool.Call) (tool.Result, error) {
		return tool.Result{Text: "x", Tainted: c.Name == "fetch"}, nil
	}

	if _, err := g.turn(context.Background()); err != nil {
		t.Fatalf("Turn: %v", err)
	}
	want := []bool{false, true, true, false} // fetch, +2 decayed calls, then clear
	for i, w := range want {
		if got := g.tools.calls[i].Tainted; got != w {
			t.Errorf("call %d tainted = %v, want %v", i, got, w)
		}
	}
}

func TestAttachmentsReachTheReply(t *testing.T) {
	g := newRig(t, callStep(call("c1", "echo", `{}`)), textStep("here")).
		withTools(spec("echo", objectSchema))
	g.tools.fn = func(tool.Call) (tool.Result, error) {
		return tool.Result{Text: "ok", Attachments: []event.Attachment{{ID: "A1", MIME: "image/png"}}}, nil
	}
	res, err := g.turn(context.Background())
	if err != nil {
		t.Fatalf("Turn: %v", err)
	}
	if len(res.Reply.Attachments) != 1 || res.Reply.Attachments[0].ID != "A1" {
		t.Fatalf("attachments: %+v", res.Reply.Attachments)
	}
}

// A turn that reacted and wrote no text ends with an empty, silent reply: no
// "your reply was empty" nudge, nothing to send. One that also wrote text
// sends the text.
func TestReactionOnlyTurnIsSilent(t *testing.T) {
	// The loop goes by Result.Reacted, not by the tool's name; "react"
	// itself is hidden from a call without CanReact.
	g := newRig(t, callStep(call("c1", "ack", `{"emoji":"👍"}`)), textStep("")).
		withTools(spec("ack", objectSchema))
	g.tools.fn = func(tool.Call) (tool.Result, error) {
		return tool.Result{Text: "reacted 👍", Reacted: true}, nil
	}
	res, err := g.turn(context.Background())
	if err != nil {
		t.Fatalf("Turn: %v", err)
	}
	if res.Reply.ID == "" || !Silent(res.Reply) || res.Steps != 2 {
		t.Fatalf("reply = %+v, steps %d", res.Reply, res.Steps)
	}

	g = newRig(t, callStep(call("c1", "ack", `{"emoji":"👍"}`)), textStep("done")).
		withTools(spec("ack", objectSchema))
	g.tools.fn = func(tool.Call) (tool.Result, error) {
		return tool.Result{Text: "reacted 👍", Reacted: true}, nil
	}
	if res, err = g.turn(context.Background()); err != nil || Silent(res.Reply) || res.Reply.Text != "done" {
		t.Fatalf("reply = %+v, %v", res.Reply, err)
	}

	// Without a reaction an empty answer is still nudged, then replaced.
	g = newRig(t, textStep(""), textStep(""))
	if res, err = g.turn(context.Background()); err != nil || Silent(res.Reply) {
		t.Fatalf("no reaction: reply = %+v, %v", res.Reply, err)
	}
}

func TestDroppedStreamRetriesOnce(t *testing.T) {
	g := newRig(t,
		step{resp: &provider.Response{Finish: provider.FinishDropped}},
		textStep("second try"),
	)
	res, err := g.turn(context.Background())
	if err != nil {
		t.Fatalf("Turn: %v", err)
	}
	if res.Reply.Text != "second try" || res.Steps != 1 {
		t.Fatalf("reply=%q steps=%d", res.Reply.Text, res.Steps)
	}
	e, ok := g.ev.first(event.ModelError)
	if !ok || g.ev.attrs(t, e)["kind"] != "dropped" {
		t.Fatalf("model.error: %+v", e)
	}
}

// A history ending with the assistant is never sent: ollama would hand that
// old answer back as the reply.
func TestAssistantLastIsNotSent(t *testing.T) {
	g := newRig(t, textStep("unused"))
	res, err := g.r.Turn(context.Background(), TurnInput{
		SessionID: "S1",
		TraceID:   "T1",
		History: []event.Message{
			{Schema: event.SchemaVersion, ID: "M1", Role: event.RoleUser, Text: "rain in Sevilla?"},
			{Schema: event.SchemaVersion, ID: "M2", Role: event.RoleAssistant, Text: "no rain"},
		},
		Call: tool.Call{Agent: "test", Role: "owner", RunKind: tool.RunInteractive, Workspace: "/ws"},
	})
	if !errors.Is(err, ErrAssistantLast) {
		t.Fatalf("err = %v", err)
	}
	if len(g.prov.reqs) != 0 {
		t.Fatalf("provider called %d times", len(g.prov.reqs))
	}
	if res.Reply.Text == "no rain" {
		t.Fatalf("old answer replayed: %+v", res.Reply)
	}
}

func TestDroppedTwiceFails(t *testing.T) {
	g := newRig(t,
		step{resp: &provider.Response{Finish: provider.FinishDropped}},
		step{resp: &provider.Response{Finish: provider.FinishDropped}},
	)
	res, err := g.turn(context.Background())
	if !errors.Is(err, ErrDroppedStream) {
		t.Fatalf("err = %v", err)
	}
	if res.Stop != StopError {
		t.Fatalf("stop %q", res.Stop)
	}
	if g.ev.count(event.ModelError) != 2 {
		t.Fatalf("model.error count %d", g.ev.count(event.ModelError))
	}
}

func TestTruncatedWithToolCallsReElicits(t *testing.T) {
	g := newRig(t,
		step{resp: &provider.Response{Finish: provider.FinishLength, ToolCalls: []provider.ToolCall{call("c1", "echo", `{}`)}}},
		textStep("short answer"),
	).withTools(spec("echo", objectSchema))

	res, err := g.turn(context.Background())
	if err != nil {
		t.Fatalf("Turn: %v", err)
	}
	if len(g.tools.calls) != 0 {
		t.Fatalf("truncated tool call was executed")
	}
	last := lastReq(g)
	if !strings.Contains(last.Messages[len(last.Messages)-1].Text(), "cut off") {
		t.Fatalf("no truncation note: %+v", last.Messages)
	}
	if res.Reply.Text != "short answer" {
		t.Fatalf("reply %q", res.Reply.Text)
	}
}

func TestTruncatedTextGetsMarker(t *testing.T) {
	g := newRig(t, step{
		deltas: []string{"a long answer that stops"},
		resp:   &provider.Response{Text: "a long answer that stops", Finish: provider.FinishLength},
	})
	res, err := g.turn(context.Background())
	if err != nil {
		t.Fatalf("Turn: %v", err)
	}
	if !strings.HasSuffix(res.Reply.Text, " […]") {
		t.Fatalf("reply %q", res.Reply.Text)
	}
}

func TestFallbackOnServerErrorAndStickiness(t *testing.T) {
	g := newRig(t, step{err: providerError(provider.ErrServer)})
	fb := &fakeProvider{name: "fb", steps: []step{
		callStep(call("c1", "echo", `{}`)),
		textStep("from fallback"),
	}}
	g.withTools(spec("echo", objectSchema))
	g.r.Fallback = &ModelRef{Model: provider.Model{Name: "backup", Provider: "fb"}, Provider: fb}

	res, err := g.turn(context.Background())
	if err != nil {
		t.Fatalf("Turn: %v", err)
	}
	if res.Reply.Text != "from fallback" {
		t.Fatalf("reply %q", res.Reply.Text)
	}
	if len(g.prov.reqs) != 1 || len(fb.reqs) != 2 {
		t.Fatalf("stickiness: main=%d fallback=%d", len(g.prov.reqs), len(fb.reqs))
	}
	e, ok := g.ev.first(event.ModelFallback)
	if !ok {
		t.Fatalf("no model.fallback event")
	}
	a := g.ev.attrs(t, e)
	if a["from"] != "main" || a["to"] != "backup" || a["reason"] != "server" {
		t.Fatalf("model.fallback attrs: %v", a)
	}
}

func TestFallbackKinds(t *testing.T) {
	cases := []struct {
		kind provider.ErrorKind
		want bool
	}{
		{provider.ErrTimeout, true},
		{provider.ErrServer, true},
		{provider.ErrNetwork, true},
		{provider.ErrRateLimit, true},
		{provider.ErrUsageLimit, true},
		{provider.ErrAuth, false},
		{provider.ErrBadRequest, false},
		{provider.ErrContext, false},
	}
	for _, c := range cases {
		g := newRig(t, step{err: providerError(c.kind)})
		fb := &fakeProvider{name: "fb", steps: []step{textStep("fallback reply")}}
		g.r.Fallback = &ModelRef{Model: provider.Model{Name: "backup"}, Provider: fb}
		res, err := g.turn(context.Background())
		used := g.ev.count(event.ModelFallback) == 1
		if used != c.want {
			t.Fatalf("%s: fallback used = %v, want %v", c.kind, used, c.want)
		}
		if c.want {
			if err != nil || res.Reply.Text != "fallback reply" {
				t.Fatalf("%s: err=%v reply=%q", c.kind, err, res.Reply.Text)
			}
		} else {
			if provider.KindOf(err) != c.kind || res.Stop != StopError {
				t.Fatalf("%s: err=%v stop=%q", c.kind, err, res.Stop)
			}
			if g.ev.count(event.ModelError) != 1 {
				t.Fatalf("%s: no model.error", c.kind)
			}
		}
	}
}

func TestNoFallbackReturnsTypedError(t *testing.T) {
	g := newRig(t, step{err: providerError(provider.ErrTimeout)})
	res, err := g.turn(context.Background())
	if provider.KindOf(err) != provider.ErrTimeout {
		t.Fatalf("err %v", err)
	}
	if res.Stop != StopError || len(res.New) != 0 {
		t.Fatalf("result %+v", res)
	}
}

func TestNoFallbackAfterTextStreamed(t *testing.T) {
	g := newRig(t, step{deltas: []string{"partial"}, err: providerError(provider.ErrServer)})
	fb := &fakeProvider{name: "fb", steps: []step{textStep("nope")}}
	g.r.Fallback = &ModelRef{Model: provider.Model{Name: "backup"}, Provider: fb}

	_, err := g.turn(context.Background())
	if provider.KindOf(err) != provider.ErrServer {
		t.Fatalf("err %v", err)
	}
	if g.ev.count(event.ModelFallback) != 0 || len(fb.reqs) != 0 {
		t.Fatalf("fell back after streaming text")
	}
}

func TestUsageLimitEvent(t *testing.T) {
	reset := time.Date(2026, 2, 3, 4, 0, 0, 0, time.UTC)
	g := newRig(t, step{err: &provider.Error{
		Kind: provider.ErrUsageLimit, Provider: "fake", ResetAt: reset,
	}})
	_, err := g.turn(context.Background())
	if provider.KindOf(err) != provider.ErrUsageLimit {
		t.Fatalf("err %v", err)
	}
	e, ok := g.ev.first(event.ModelUsageLimit)
	if !ok {
		t.Fatalf("no model.usage_limit event")
	}
	if got := g.ev.attrs(t, e)["reset_at"]; got != reset.Format(time.RFC3339) {
		t.Fatalf("reset time: %v", got)
	}
}

func TestAbortMidStream(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	g := newRig(t, step{deltas: []string{"partial ", "more"}})
	g.prov.steps[0].during = cancel

	res, err := g.turn(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err %v", err)
	}
	if res.Stop != StopAborted {
		t.Fatalf("stop %q", res.Stop)
	}
	if len(res.New) != 1 || !strings.HasSuffix(res.New[0].Text, " [aborted]") {
		t.Fatalf("partial message: %+v", res.New)
	}
	if g.ev.count(event.RunAborted) != 1 || g.ev.count(event.RunFinished) != 1 {
		t.Fatalf("events %v", g.ev.types())
	}
}

func TestAbortBeforeFirstCallStoresNothing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	g := newRig(t, textStep("never"))
	res, err := g.turn(ctx)
	if !errors.Is(err, context.Canceled) || res.Stop != StopAborted {
		t.Fatalf("err=%v stop=%q", err, res.Stop)
	}
	if len(res.New) != 0 || len(g.prov.reqs) != 0 {
		t.Fatalf("work happened after cancellation: %+v", res.New)
	}
}

func TestEmptyReplyReElicitsThenFallsBack(t *testing.T) {
	g := newRig(t, textStep(""), textStep(""))
	res, err := g.turn(context.Background())
	if err != nil {
		t.Fatalf("Turn: %v", err)
	}
	if res.Reply.Text != emptyReply {
		t.Fatalf("reply %q", res.Reply.Text)
	}
	if !strings.Contains(lastReq(g).Messages[len(lastReq(g).Messages)-1].Text(), "empty") {
		t.Fatalf("no empty-reply note")
	}
}

func TestEmptyReplyRecoveredOnRetry(t *testing.T) {
	g := newRig(t, textStep(""), textStep("here it is"))
	res, err := g.turn(context.Background())
	if err != nil {
		t.Fatalf("Turn: %v", err)
	}
	if res.Reply.Text != "here it is" || res.Steps != 2 {
		t.Fatalf("reply=%q steps=%d", res.Reply.Text, res.Steps)
	}
}

func TestNeedsCompactionSurfaced(t *testing.T) {
	g := newRig(t, textStep("ok"))
	g.b.compact = true
	res, err := g.turn(context.Background())
	if err != nil || !res.NeedsCompaction {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

func TestBuilderErrorStopsTurn(t *testing.T) {
	g := newRig(t, textStep("never"))
	g.b.buildErr = errors.New("budget blown")
	res, err := g.turn(context.Background())
	if err == nil || !strings.Contains(err.Error(), "budget blown") {
		t.Fatalf("err %v", err)
	}
	if res.Stop != StopError || len(g.prov.reqs) != 0 {
		t.Fatalf("result %+v", res)
	}
}

func TestTranscriptErrorStopsTurn(t *testing.T) {
	g := newRig(t, textStep("hello"))
	g.tr.err = errors.New("disk full")
	_, err := g.turn(context.Background())
	if err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("err %v", err)
	}
}

func TestNoExecutorIgnoresToolCalls(t *testing.T) {
	g := newRig(t, step{resp: &provider.Response{
		Text: "I would call a tool", Finish: provider.FinishToolCalls,
		ToolCalls: []provider.ToolCall{call("c1", "echo", `{}`)},
	}})
	res, err := g.turn(context.Background())
	if err != nil {
		t.Fatalf("Turn: %v", err)
	}
	if res.Reply.Text != "I would call a tool" {
		t.Fatalf("reply %q", res.Reply.Text)
	}
}

func TestCanonicalArgsEquality(t *testing.T) {
	cases := [][2]string{
		{`{"a":1,"b":2}`, `{"b":2,"a":1}`},
		{`{"a":{"x":1,"y":2}}`, `{"a":{"y":2,"x":1}}`},
		{` {"a": 1 } `, `{"a":1}`},
	}
	for _, c := range cases {
		if canonicalArgs(c[0]) != canonicalArgs(c[1]) {
			t.Fatalf("%s != %s", canonicalArgs(c[0]), canonicalArgs(c[1]))
		}
	}
	if canonicalArgs(`{"a":1}`) == canonicalArgs(`{"a":2}`) {
		t.Fatalf("different arguments canonicalised equal")
	}
	if got := canonicalArgs(`not json`); got != "not json" {
		t.Fatalf("unparsable args changed: %q", got)
	}
}

func TestValidateArgsTable(t *testing.T) {
	schema := json.RawMessage(`{
		"type":"object",
		"required":["path"],
		"properties":{
			"path":{"type":"string"},
			"count":{"type":"integer"},
			"ratio":{"type":"number"},
			"deep":{"type":"boolean"},
			"mode":{"enum":["fast","slow"]},
			"tags":{"type":"array","items":{"type":"string"}},
			"opts":{"type":"object","required":["k"],"properties":{"k":{"type":"string"}}}
		}}`)
	cases := []struct {
		name string
		args string
		want string // substring of the expected error, "" = valid
	}{
		{"ok", `{"path":"/a"}`, ""},
		{"ok full", `{"path":"/a","count":2,"ratio":1.5,"deep":true,"mode":"fast","tags":["x"],"opts":{"k":"v"}}`, ""},
		{"missing required", `{"count":1}`, `"path"`},
		{"wrong type", `{"path":7}`, `"path" must be a string`},
		{"float for integer", `{"path":"/a","count":1.5}`, `"count" must be an integer`},
		{"bad enum", `{"path":"/a","mode":"medium"}`, `"mode" must be one of: fast, slow`},
		{"bad array", `{"path":"/a","tags":"x"}`, `"tags" must be an array`},
		{"bad array item", `{"path":"/a","tags":[1]}`, `"tags[0]" must be a string`},
		{"nested required", `{"path":"/a","opts":{}}`, `"opts.k"`},
		{"nested type", `{"path":"/a","opts":{"k":3}}`, `"opts.k" must be a string`},
		{"unknown keys ignored", `{"path":"/a","extra":{"whatever":1}}`, ""},
		{"not an object", `[1,2]`, "must be an object"},
	}
	for _, c := range cases {
		err := validateArgs(schema, json.RawMessage(c.args))
		if c.want == "" {
			if err != nil {
				t.Fatalf("%s: unexpected error %v", c.name, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s: err = %v, want containing %q", c.name, err, c.want)
		}
	}
	if err := validateArgs(nil, json.RawMessage(`{}`)); err != nil {
		t.Fatalf("empty schema: %v", err)
	}
	if err := validateArgs(json.RawMessage(`not a schema`), json.RawMessage(`{}`)); err != nil {
		t.Fatalf("unusable schema: %v", err)
	}
}

// Taint is a property of the context, not of one turn: untrusted content that
// entered an earlier turn is still in the history window the builder renders.
// Provenance carries over always (the reply stays marked and wrapped);
// escalation carries over only when that content scored as steering, for the
// configured decay window and not forever (docs/security.md).
func TestTaintCarriesOverFromHistory(t *testing.T) {
	window := func(score float64, tainted bool) []event.Message {
		return []event.Message{
			{Schema: event.SchemaVersion, ID: "M1", Role: event.RoleUser, Text: "fetch it"},
			{Schema: event.SchemaVersion, ID: "M2", Role: event.RoleAssistant, Text: "",
				ToolCalls: []event.ToolCall{{ID: "c0", Name: "fetch", Arguments: "{}"}}},
			{Schema: event.SchemaVersion, ID: "M3", Role: event.RoleTool, ToolCallID: "c0",
				ToolName: "fetch", Text: "the page", Tainted: tainted, TaintScore: score},
			{Schema: event.SchemaVersion, ID: "M4", Role: event.RoleUser, Text: "now run echo"},
		}
	}
	clean := []event.Message{{Schema: event.SchemaVersion, ID: "M1", Role: event.RoleUser, Text: "run echo"}}

	for _, tc := range []struct {
		name           string
		history        []event.Message
		armed, tainted bool
		wantReplyScore float64
	}{
		// Steering content: the next turn starts armed.
		{"injected history", window(0.9, true), true, true, 0.9},
		// Untrusted but ordinary: still wrapped, arms nothing.
		{"clean untrusted history", window(0.1, true), false, true, 0.1},
		// Stored before the scorer existed: a bool with no score. Wrapped,
		// but it does not arm — escalation is bounded by design.
		{"legacy tainted history", window(0, true), false, true, 0},
		{"clean history", clean, false, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newRig(t, callStep(call("c1", "echo", `{}`)), textStep("done")).
				withTools(spec("echo", objectSchema))
			g.tools.fn = func(tool.Call) (tool.Result, error) { return tool.Result{Text: "x"}, nil }

			res, err := g.r.Turn(context.Background(), TurnInput{
				SessionID: "S1", TraceID: "T1", History: tc.history,
				Call: tool.Call{Agent: "test", Role: "owner", RunKind: tool.RunInteractive, Workspace: "/ws"},
			})
			if err != nil {
				t.Fatalf("Turn: %v", err)
			}
			if len(g.tools.calls) == 0 {
				t.Fatal("the tool was never called")
			}
			if got := g.tools.calls[0].Tainted; got != tc.armed {
				t.Errorf("first call tainted = %v, want %v", got, tc.armed)
			}
			if got := res.Reply.Tainted; got != tc.tainted {
				t.Errorf("reply tainted = %v, want %v", got, tc.tainted)
			}
			if got := res.Reply.TaintScore; got != tc.wantReplyScore {
				t.Errorf("reply score = %v, want %v", got, tc.wantReplyScore)
			}
		})
	}
}

// The caller may declare the turn tainted itself (inlined documents, a tainted
// recall hit): that reaches the policy gate too.
func TestTaintFromTheCallTemplate(t *testing.T) {
	g := newRig(t, callStep(call("c1", "echo", `{}`)), textStep("done")).
		withTools(spec("echo", objectSchema))
	g.tools.fn = func(tool.Call) (tool.Result, error) { return tool.Result{Text: "x"}, nil }

	if _, err := g.r.Turn(context.Background(), TurnInput{
		SessionID: "S1", TraceID: "T1",
		History: []event.Message{{Schema: event.SchemaVersion, ID: "M1", Role: event.RoleUser, Text: "hi"}},
		Call: tool.Call{Agent: "test", Role: "owner", RunKind: tool.RunInteractive,
			Workspace: "/ws", Tainted: true},
	}); err != nil {
		t.Fatalf("Turn: %v", err)
	}
	if !g.tools.calls[0].Tainted {
		t.Fatal("the caller's taint did not reach the tool call")
	}
}

func TestPerTurnStepLimitOnlyTightens(t *testing.T) {
	r := &Runner{Def: Definition{MaxSteps: 6}}
	for in, want := range map[int]int{0: 6, 3: 3, 6: 6, 20: 6} {
		if got := r.maxSteps(TurnInput{MaxSteps: in}); got != want {
			t.Errorf("MaxSteps %d → %d, want %d", in, got, want)
		}
	}
	if got := (&Runner{}).maxSteps(TurnInput{MaxSteps: 2}); got != 2 {
		t.Errorf("default agent limit with a run limit of 2 → %d", got)
	}
}

func TestSkillSaveNotLastIsRejected(t *testing.T) {
	g := newRig(t,
		callStep(call("c1", SkillSaveTool, `{}`), call("c2", "echo", `{}`)),
		textStep("done"),
	).withTools(spec(SkillSaveTool, objectSchema), spec("echo", objectSchema))

	res, err := g.turn(context.Background())
	if err != nil {
		t.Fatalf("Turn: %v", err)
	}
	if got := g.tools.names(); len(got) != 1 || got[0] != "echo" {
		t.Fatalf("executed %v, want only echo", got)
	}
	e, ok := g.ev.first(event.ToolRejected)
	if !ok || g.ev.attrs(t, e)["reason"] != "not_last" || g.ev.attrs(t, e)["tool"] != SkillSaveTool {
		t.Fatalf("tool.rejected: %+v", e)
	}
	if res.New[1].ToolCallID != "c1" || !strings.Contains(res.New[1].Text, "must be the last call") {
		t.Fatalf("feedback: %+v", res.New[1])
	}
	if len(lastReq(g).Tools) == 0 || res.Stop != StopFinal {
		t.Fatalf("tools withdrawn after a rejected save (stop=%q)", res.Stop)
	}
}

func TestSkillSaveNotLastDoesNotCountAsInvalid(t *testing.T) {
	s := callStep(call("a", SkillSaveTool, `{}`), call("b", "echo", `{"i":1}`))
	g := newRig(t,
		callStep(call("x", SkillSaveTool, `{}`), call("y", "echo", `{"i":2}`)),
		callStep(call("x2", SkillSaveTool, `{}`), call("y2", "echo", `{"i":3}`)),
		s,
		textStep("done"),
	).withTools(spec(SkillSaveTool, objectSchema), spec("echo", objectSchema))
	if _, err := g.turn(context.Background()); err != nil {
		t.Fatalf("Turn: %v", err)
	}
	if g.ev.count(event.RunStepLimit) != 0 {
		t.Fatalf("three rejected saves withdrew the tools")
	}
}

func TestSkillSaveLastEndsTheTurn(t *testing.T) {
	g := newRig(t,
		callStep(call("c1", "echo", `{}`), call("c2", SkillSaveTool, `{}`)),
		textStep("saved it"),
	).withTools(spec(SkillSaveTool, objectSchema), spec("echo", objectSchema))

	res, err := g.turn(context.Background())
	if err != nil {
		t.Fatalf("Turn: %v", err)
	}
	if got := g.tools.names(); len(got) != 2 || got[1] != SkillSaveTool {
		t.Fatalf("executed %v", got)
	}
	last := lastReq(g)
	if len(last.Tools) != 0 {
		t.Fatalf("tools still offered after a save: %v", last.Tools)
	}
	if !strings.Contains(last.Messages[len(last.Messages)-1].Text(), "skill is saved") {
		t.Fatalf("no saved note in prompt: %+v", last.Messages)
	}
	if res.Stop != StopFinal || res.Reply.Text != "saved it" {
		t.Fatalf("stop %q reply %q", res.Stop, res.Reply.Text)
	}
	if g.ev.count(event.RunStepLimit) != 0 || g.ev.count(event.ToolRejected) != 0 {
		t.Fatalf("unexpected events: %v", g.ev.types())
	}
}

func TestSkillSaveFailureKeepsTools(t *testing.T) {
	g := newRig(t,
		callStep(call("c1", SkillSaveTool, `{}`)),
		callStep(call("c2", SkillSaveTool, `{"fixed":true}`)),
		textStep("saved"),
	).withTools(spec(SkillSaveTool, objectSchema))
	g.tools.fn = func(c tool.Call) (tool.Result, error) {
		if c.CallID == "c1" {
			return tool.Result{Text: "SKL111: 1 step(s)", IsError: true}, nil
		}
		return tool.Result{Text: "saved skill x (new)"}, nil
	}
	if _, err := g.turn(context.Background()); err != nil {
		t.Fatalf("Turn: %v", err)
	}
	if len(g.prov.reqs) != 3 || len(g.prov.reqs[1].Tools) == 0 {
		t.Fatalf("tools withdrawn after a failed save")
	}
	if len(g.prov.reqs[2].Tools) != 0 {
		t.Fatalf("tools still offered after the fixed save")
	}
}
