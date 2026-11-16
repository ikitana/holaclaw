package agent

import (
	"context"
	"strings"
	"testing"

	"holaclaw/internal/event"
)

func knownTools(names ...string) func(string) bool {
	return func(n string) bool {
		for _, k := range names {
			if k == n {
				return true
			}
		}
		return false
	}
}

func TestDetectLeakedCalls(t *testing.T) {
	known := knownTools("files.read", "echo")
	cases := []struct {
		name     string
		text     string
		want     bool
		tool     string
		args     string
		stripped string
	}{
		{
			name: "fenced json",
			text: "Sure.\n```json\n{\"name\": \"files.read\", \"arguments\": {\"path\": \"/a\"}}\n```\n",
			want: true, tool: "files.read", args: `{"path":"/a"}`, stripped: "Sure.",
		},
		{
			name: "tool_call tag",
			text: "<tool_call>{\"name\":\"echo\",\"arguments\":{\"text\":\"x\"}}</tool_call>",
			want: true, tool: "echo", args: `{"text":"x"}`, stripped: "",
		},
		{
			name: "function tag",
			text: `Let me look: <function=files.read>{"path": "/etc/hosts"}</function>`,
			want: true, tool: "files.read", args: `{"path": "/etc/hosts"}`, stripped: "Let me look:",
		},
		{
			name: "tool_calls marker",
			text: `[TOOL_CALLS] [{"name": "echo", "arguments": {"text": "hi"}}]`,
			want: true, tool: "echo", args: `{"text":"hi"}`, stripped: "",
		},
		{
			name: "bare object",
			text: `I will call {"name":"echo","arguments":{"text":"hi"}} now.`,
			want: true, tool: "echo", args: `{"text":"hi"}`, stripped: "I will call  now.",
		},
		{
			name: "nested function object",
			text: `{"type":"function","function":{"name":"echo","arguments":{"text":"z"}}}`,
			want: true, tool: "echo", args: `{"text":"z"}`, stripped: "",
		},
		{
			name: "arguments as json string",
			text: `<tool_call>{"name":"echo","arguments":"{\"text\":\"s\"}"}</tool_call>`,
			want: true, tool: "echo", args: `{"text":"s"}`, stripped: "",
		},
		{
			name: "tool key instead of name",
			text: `{"tool":"echo","args":{"text":"y"}}`,
			want: true, tool: "echo", args: `{"text":"y"}`, stripped: "",
		},
		// Negatives.
		{name: "plain prose", text: "Here is the answer: 42.", want: false},
		{
			name: "json the user asked for",
			text: "```json\n{\"name\": \"Ada\", \"arguments\": {\"age\": 36}}\n```",
			want: false,
		},
		{name: "bare json answer", text: `{"city":"Madrid","pop":3.3}`, want: false},
		{
			name: "code block without a known tool",
			text: "```go\nfunc main() { println(\"echo\") }\n```",
			want: false,
		},
		{name: "tool name only in prose", text: "I could use files.read for that.", want: false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			calls, stripped, ok := detectLeakedCalls(c.text, known)
			if ok != c.want {
				t.Fatalf("detected = %v (calls %+v)", ok, calls)
			}
			if !c.want {
				if stripped != c.text {
					t.Fatalf("text changed on a negative: %q", stripped)
				}
				return
			}
			if len(calls) != 1 || calls[0].Name != c.tool {
				t.Fatalf("calls %+v", calls)
			}
			if canonicalArgs(calls[0].Args) != canonicalArgs(c.args) {
				t.Fatalf("args %q, want %q", calls[0].Args, c.args)
			}
			if stripped != c.stripped {
				t.Fatalf("stripped %q, want %q", stripped, c.stripped)
			}
		})
	}
}

func TestDetectLeakedCallsNoTools(t *testing.T) {
	_, _, ok := detectLeakedCalls(`{"name":"echo","arguments":{}}`, knownTools())
	if ok {
		t.Fatalf("fired without known tools")
	}
}

func TestTextLeakReElicitsOnce(t *testing.T) {
	leak := "Working on it.\n```json\n{\"name\":\"echo\",\"arguments\":{\"text\":\"x\"}}\n```"
	g := newRig(t, textStep(leak), textStep("the real answer")).
		withTools(spec("echo", objectSchema))

	res, err := g.turn(context.Background())
	if err != nil {
		t.Fatalf("Turn: %v", err)
	}
	if len(g.tools.calls) != 0 {
		t.Fatalf("leaked call was executed")
	}
	rej, ok := g.ev.first(event.ToolRejected)
	if !ok || g.ev.attrs(t, rej)["reason"] != "text_leak" {
		t.Fatalf("tool.rejected: %+v", rej)
	}
	last := lastReq(g)
	if !strings.Contains(last.Messages[len(last.Messages)-1].Text(), "written as text") {
		t.Fatalf("no re-elicitation note: %+v", last.Messages)
	}
	if res.Reply.Text != "the real answer" || res.Steps != 2 {
		t.Fatalf("reply=%q steps=%d", res.Reply.Text, res.Steps)
	}
}

func TestTextLeakTwiceDeliversStrippedText(t *testing.T) {
	leak := "Here you go.\n<tool_call>{\"name\":\"echo\",\"arguments\":{}}</tool_call>"
	g := newRig(t, textStep(leak), textStep(leak)).withTools(spec("echo", objectSchema))

	res, err := g.turn(context.Background())
	if err != nil {
		t.Fatalf("Turn: %v", err)
	}
	if len(g.tools.calls) != 0 {
		t.Fatalf("leaked call was executed")
	}
	if res.Reply.Text != "Here you go." {
		t.Fatalf("reply %q", res.Reply.Text)
	}
}

func TestTextLeakTwiceEmptyTextApologises(t *testing.T) {
	leak := "<tool_call>{\"name\":\"echo\",\"arguments\":{}}</tool_call>"
	g := newRig(t, textStep(leak), textStep(leak)).withTools(spec("echo", objectSchema))
	res, err := g.turn(context.Background())
	if err != nil {
		t.Fatalf("Turn: %v", err)
	}
	if res.Reply.Text != leakReply {
		t.Fatalf("reply %q", res.Reply.Text)
	}
}

func TestStrictModelPromotesLeakedCall(t *testing.T) {
	leak := "ok\n```json\n{\"name\":\"echo\",\"arguments\":{\"text\":\"x\"}}\n```"
	g := newRig(t, textStep(leak), textStep("done")).
		withTools(spec("echo", `{"type":"object","required":["text"],"properties":{"text":{"type":"string"}}}`))
	g.r.Model.Strict = true

	res, err := g.turn(context.Background())
	if err != nil {
		t.Fatalf("Turn: %v", err)
	}
	if len(g.tools.calls) != 1 || g.tools.calls[0].Name != "echo" {
		t.Fatalf("promoted call not executed: %+v", g.tools.calls)
	}
	e, ok := g.ev.first(event.ToolRepaired)
	if !ok || g.ev.attrs(t, e)["stage"] != "promoted" {
		t.Fatalf("tool.repaired: %+v", e)
	}
	if res.New[0].Text != "ok" || len(res.New[0].ToolCalls) != 1 {
		t.Fatalf("assistant message: %+v", res.New[0])
	}
}

func TestStrictModelDoesNotPromoteInvalidArgs(t *testing.T) {
	leak := "```json\n{\"name\":\"echo\",\"arguments\":{\"wrong\":1}}\n```"
	g := newRig(t, textStep(leak), textStep("plain")).
		withTools(spec("echo", `{"type":"object","required":["text"],"properties":{"text":{"type":"string"}}}`))
	g.r.Model.Strict = true

	if _, err := g.turn(context.Background()); err != nil {
		t.Fatalf("Turn: %v", err)
	}
	if len(g.tools.calls) != 0 {
		t.Fatalf("schema-invalid leak was promoted")
	}
	rej, ok := g.ev.first(event.ToolRejected)
	if !ok || g.ev.attrs(t, rej)["reason"] != "text_leak" {
		t.Fatalf("tool.rejected: %+v", rej)
	}
}

func TestStrictModelDoesNotPromoteTwoCalls(t *testing.T) {
	leak := `[TOOL_CALLS] [{"name":"echo","arguments":{}},{"name":"echo","arguments":{"a":1}}]`
	g := newRig(t, textStep(leak), textStep("plain")).withTools(spec("echo", objectSchema))
	g.r.Model.Strict = true

	if _, err := g.turn(context.Background()); err != nil {
		t.Fatalf("Turn: %v", err)
	}
	if len(g.tools.calls) != 0 {
		t.Fatalf("multiple leaked calls were promoted")
	}
	if g.ev.count(event.ToolRejected) != 2 {
		t.Fatalf("tool.rejected count %d", g.ev.count(event.ToolRejected))
	}
}

func TestNativeToolCallsSkipLeakDetection(t *testing.T) {
	// Text that looks like a leak is ignored when native calls are present.
	st := callStep(call("c1", "echo", `{}`))
	st.resp.Text = `{"name":"echo","arguments":{}}`
	g := newRig(t, st, textStep("done")).withTools(spec("echo", objectSchema))

	if _, err := g.turn(context.Background()); err != nil {
		t.Fatalf("Turn: %v", err)
	}
	if len(g.tools.calls) != 1 {
		t.Fatalf("executed %d calls", len(g.tools.calls))
	}
	if g.ev.count(event.ToolRejected) != 0 {
		t.Fatalf("leak detector fired on a native call")
	}
}

func TestLeakInFinalModeIsDelivered(t *testing.T) {
	leak := "almost done\n<tool_call>{\"name\":\"echo\",\"arguments\":{}}</tool_call>"
	g := newRig(t,
		callStep(call("c1", "echo", `{}`)),
		textStep(leak),
	).withTools(spec("echo", objectSchema))
	g.r.Def.MaxSteps = 1

	res, err := g.turn(context.Background())
	if err != nil {
		t.Fatalf("Turn: %v", err)
	}
	if res.Reply.Text != "almost done" || res.Stop != StopStepLimit {
		t.Fatalf("reply=%q stop=%q", res.Reply.Text, res.Stop)
	}
	if len(g.tools.calls) != 1 {
		t.Fatalf("executed %d calls", len(g.tools.calls))
	}
}
