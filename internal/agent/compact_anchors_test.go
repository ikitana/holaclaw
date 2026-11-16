package agent

import (
	"strings"
	"testing"

	"holaclaw/internal/event"
	"holaclaw/internal/tokens"
)

func TestHarvestAnchors(t *testing.T) {
	tests := []struct {
		name string
		text string
		want []string
		not  []string
	}{
		{
			name: "ulid and ticket ids",
			text: "session 01ARZ3NDEKTSV4RRFFQ69G5FAV for HOLA-42 and JEV-1 done",
			want: []string{"01ARZ3NDEKTSV4RRFFQ69G5FAV", "HOLA-42", "JEV-1"},
		},
		{
			name: "file paths",
			text: "edited internal/agent/compact.go and /etc/holaclaw/config.toml and ~/notes.md",
			want: []string{"internal/agent/compact.go", "/etc/holaclaw/config.toml"},
			not:  []string{"notes.md"},
		},
		{
			name: "urls, and a url's path is not a second anchor",
			text: "see https://example.com/docs/api#tools for details",
			want: []string{"https://example.com/docs/api#tools"},
			not:  []string{"example.com/docs/api"},
		},
		{
			name: "error lines",
			text: "user: run it\nresult of shell.run: Error: connection refused\nassistant: retrying",
			want: []string{"result of shell.run: Error: connection refused"},
			not:  []string{"assistant: retrying"},
		},
		{
			name: "panic and exception lines",
			text: "ok so far\nresult of shell.run: panic: nil map\nresult of shell.run: java.lang.NullPointerException",
			want: []string{"result of shell.run: panic: nil map", "result of shell.run: java.lang.NullPointerException"},
			not:  []string{"ok so far"},
		},
		{
			name: "deduplicated",
			text: "HOLA-42 HOLA-42 HOLA-42",
			want: []string{"HOLA-42"},
		},
		{
			name: "nothing to anchor",
			text: "we talked about the weather",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := harvestAnchors([]event.Message{cpAt(0, event.RoleUser, tc.text)})
			for _, w := range tc.want {
				if !containsAnchor(got, w) {
					t.Fatalf("anchor %q missing from %v", w, got)
				}
			}
			for _, n := range tc.not {
				if containsAnchor(got, n) {
					t.Fatalf("anchor %q should not be harvested: %v", n, got)
				}
			}
			if tc.want == nil && tc.not == nil && len(got) != 0 {
				t.Fatalf("anchors = %v, want none", got)
			}
			seen := map[string]bool{}
			for _, a := range got {
				if seen[a] {
					t.Fatalf("duplicate anchor %q", a)
				}
				seen[a] = true
			}
		})
	}
}

func TestHarvestAnchorsCaps(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 200; i++ {
		b.WriteString("HOLA-")
		b.WriteString(strings.Repeat("9", 1+i%4))
		b.WriteString(" /srv/data/file")
		b.WriteString(strings.Repeat("x", 1+i%7))
		b.WriteString(".txt\n")
	}
	got := harvestAnchors([]event.Message{cpAt(0, event.RoleUser, b.String())})
	if len(got) > MaxAnchors {
		t.Fatalf("%d anchors, cap %d", len(got), MaxAnchors)
	}
	if n := tokens.Estimate(strings.Join(got, "\n")); n > MaxAnchorTokens {
		t.Fatalf("anchors are %d tokens, cap %d", n, MaxAnchorTokens)
	}
}

func TestHarvestAnchorsErrorLineCap(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 30; i++ {
		b.WriteString("failed to open thing ")
		b.WriteString(strings.Repeat("y", i+1))
		b.WriteString("\n")
	}
	got := harvestAnchors([]event.Message{cpAt(0, event.RoleUser, b.String())})
	if len(got) > maxErrorAnchors {
		t.Fatalf("%d error anchors, cap %d", len(got), maxErrorAnchors)
	}
}

func TestHarvestAnchorsLongLineTrimmed(t *testing.T) {
	long := "error: " + strings.Repeat("z", 400)
	got := harvestAnchors([]event.Message{cpAt(0, event.RoleUser, long)})
	if len(got) != 1 {
		t.Fatalf("anchors = %v", got)
	}
	if n := len([]rune(got[0])); n > maxAnchorRunes+1 {
		t.Fatalf("anchor is %d runes", n)
	}
}

func TestAnchorBlock(t *testing.T) {
	if s := anchorBlock(nil); s != "" {
		t.Fatalf("empty anchor block = %q", s)
	}
	s := anchorBlock([]string{"HOLA-1", "a/b.go"})
	if s != "\n\nAnchors:\n- HOLA-1\n- a/b.go" {
		t.Fatalf("block = %q", s)
	}
}

func containsAnchor(anchors []string, want string) bool {
	for _, a := range anchors {
		if a == want {
			return true
		}
	}
	return false
}
