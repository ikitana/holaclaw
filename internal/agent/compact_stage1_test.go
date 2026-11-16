package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"holaclaw/internal/event"
	"holaclaw/internal/tokens"
)

func cpTool(offset int, name, callID, text string) event.Message {
	m := cpAt(offset, event.RoleTool, text)
	m.ToolName, m.ToolCallID = name, callID
	return m
}

func cpImage(offset int, text string) event.Message {
	m := cpAt(offset, event.RoleUser, text)
	m.Attachments = []event.Attachment{{
		ID: event.ID("A" + text), BlobID: "b1", MIME: "image/png",
		Name: "shot.png", Size: 2048, Kind: event.KindImage,
	}}
	return m
}

func TestMechanical(t *testing.T) {
	big := cpFiller(MaxToolResultTokens * 2)

	tests := []struct {
		name  string
		msgs  []event.Message
		opt   MechanicalOpts
		check func(t *testing.T, out []event.Message)
	}{
		{
			name: "cap and spill",
			msgs: []event.Message{cpTool(0, "files.read", "c1", big)},
			opt:  MechanicalOpts{Spill: func(context.Context, string) (string, error) { return "deadbeef", nil }},
			check: func(t *testing.T, out []event.Message) {
				if !strings.Contains(out[0].Text, "[… full result: blob deadbeef, ") {
					t.Fatalf("no blob pointer: %q", tailOf(out[0].Text, 80))
				}
				if n := tokens.Estimate(out[0].Text); n > MaxToolResultTokens {
					t.Fatalf("capped result is %d tokens", n)
				}
				if !strings.HasPrefix(out[0].Text, "abcd") {
					t.Fatal("the head of the result was not kept")
				}
			},
		},
		{
			name: "truncate only without blobs",
			msgs: []event.Message{cpTool(0, "files.read", "c1", big)},
			check: func(t *testing.T, out []event.Message) {
				if !strings.Contains(out[0].Text, "[… full result truncated, ") {
					t.Fatalf("no pointer: %q", tailOf(out[0].Text, 80))
				}
			},
		},
		{
			name: "spill failure falls back to truncation",
			msgs: []event.Message{cpTool(0, "files.read", "c1", big)},
			opt:  MechanicalOpts{Spill: func(context.Context, string) (string, error) { return "", errors.New("no") }},
			check: func(t *testing.T, out []event.Message) {
				if !strings.Contains(out[0].Text, "truncated") {
					t.Fatalf("no pointer: %q", tailOf(out[0].Text, 80))
				}
			},
		},
		{
			name: "short results are untouched",
			msgs: []event.Message{cpTool(0, "files.read", "c1", "small")},
			check: func(t *testing.T, out []event.Message) {
				if out[0].Text != "small" {
					t.Fatalf("text = %q", out[0].Text)
				}
			},
		},
		{
			name: "dedupe identical results",
			msgs: []event.Message{
				cpTool(0, "files.read", "c1", "same bytes"),
				cpTool(1, "files.read", "c2", "same bytes"),
				cpTool(2, "shell.run", "c3", "same bytes"), // other tool: kept
			},
			check: func(t *testing.T, out []event.Message) {
				if out[0].Text != "same bytes" {
					t.Fatalf("first result changed: %q", out[0].Text)
				}
				if out[1].Text != "[same result as earlier call c1]" {
					t.Fatalf("second result = %q", out[1].Text)
				}
				if out[2].Text != "same bytes" {
					t.Fatalf("a different tool must not dedupe: %q", out[2].Text)
				}
			},
		},
		{
			name: "images dropped from all but the newest two user messages",
			msgs: []event.Message{cpImage(0, "one"), cpImage(1, "two"), cpImage(2, "three"), cpImage(3, "four")},
			check: func(t *testing.T, out []event.Message) {
				for i, m := range out[:2] {
					if len(m.Attachments) != 0 {
						t.Fatalf("message %d kept its image", i)
					}
					if !strings.Contains(m.Text, "[attachment: shot.png (image,") {
						t.Fatalf("stub lost from message %d: %q", i, m.Text)
					}
				}
				for i, m := range out[2:] {
					if len(m.Attachments) != 1 {
						t.Fatalf("newest message %d lost its image", i)
					}
				}
			},
		},
		{
			name: "non-image attachments stay",
			msgs: func() []event.Message {
				m := cpImage(0, "doc")
				m.Attachments = append(m.Attachments, event.Attachment{ID: "A2", Kind: event.KindDocument, Name: "r.md"})
				return []event.Message{m, cpAt(1, event.RoleUser, "x"), cpAt(2, event.RoleUser, "y")}
			}(),
			check: func(t *testing.T, out []event.Message) {
				if len(out[0].Attachments) != 1 || out[0].Attachments[0].Kind != event.KindDocument {
					t.Fatalf("attachments = %+v", out[0].Attachments)
				}
			},
		},
		{
			name: "protected tail is untouched",
			msgs: []event.Message{cpTool(0, "files.read", "c1", big), cpTool(1, "files.read", "c2", big)},
			opt:  MechanicalOpts{Protect: 1},
			check: func(t *testing.T, out []event.Message) {
				if out[1].Text != big {
					t.Fatal("the protected message was rewritten")
				}
				if out[0].Text == big {
					t.Fatal("the older message was not capped")
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := append([]event.Message(nil), tc.msgs...)
			out := Mechanical(context.Background(), tc.msgs, tc.opt)
			if len(out) != len(tc.msgs) {
				t.Fatalf("got %d messages, want %d", len(out), len(tc.msgs))
			}
			for i := range in {
				if tc.msgs[i].Text != in[i].Text || len(tc.msgs[i].Attachments) != len(in[i].Attachments) {
					t.Fatalf("Mechanical mutated its input at %d", i)
				}
			}
			tc.check(t, out)
		})
	}
}

// Stage 1 runs on the prefix only; the stored rows keep their full text.
func TestCompactStage1DoesNotRewriteRows(t *testing.T) {
	g := newCompactRig(t, cpSummaryStep("S"))
	var spilled int
	g.c.Blobs = func(context.Context, string) (string, error) { spilled++; return "blob1", nil }

	msgs := cpChat(30, 150)
	msgs[1] = cpTool(1, "files.read", "c1", cpFiller(3000))
	msgs[3] = cpTool(3, "files.read", "c2", cpFiller(3000)) // identical: deduped
	g.add(msgs...)

	if _, err := g.c.Compact(context.Background(), g.in); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if spilled != 1 {
		t.Fatalf("%d spills, want one (the duplicate needs none)", spilled)
	}
	sent := g.prov.reqs[0].Messages[0].Text()
	if !strings.Contains(sent, "blob blob1") || !strings.Contains(sent, "[same result as earlier call c1]") {
		t.Fatalf("stage 1 output not in the prompt:\n%s", firstRunes(sent, 600))
	}
	rows, err := g.st.ListMessages(context.Background(), cpSession, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if tokens.Estimate(rows[1].Text) < 3000 {
		t.Fatal("stage 1 rewrote a stored row")
	}
}

func tailOf(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return "…" + string(r[len(r)-n:])
}
