package agent

import (
	"context"
	"errors"
	"testing"

	"holaclaw/internal/event"
)

func TestNextID(t *testing.T) {
	tests := []struct {
		name string
		id   event.ID
		want event.ID
		ok   bool
	}{
		{"entropy plus one", "01ARZ3NDEKTSV4RRFFQ69G5FA0", "01ARZ3NDEKTSV4RRFFQ69G5FA1", true},
		{"carry", "01ARZ3NDEKTSV4RRFFQ69G5FZZ", "01ARZ3NDEKTSV4RRFFQ69G5G00", true},
		{"carry across the entropy", "01ARZ3NDEKZZZZZZZZZZZZZZZZ", "", false},
		{"timestamp untouched", "01ARZ3NDEK0000000000000000", "01ARZ3NDEK0000000000000001", true},
		{"not a ulid", "nope", "", false},
		{"bad alphabet", "01ARZ3NDEKTSV4RRFFQ69G5FAU", "", false},
		{"wrong length", "01ARZ3NDEKTSV4RRFFQ69G5FA", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := nextID(tc.id)
			if ok != tc.ok || got != tc.want {
				t.Fatalf("nextID(%s) = %q,%v want %q,%v", tc.id, got, ok, tc.want, tc.ok)
			}
			if ok {
				if got <= tc.id {
					t.Fatalf("%s does not sort after %s", got, tc.id)
				}
				if len(got) != len(tc.id) {
					t.Fatalf("length changed: %s", got)
				}
			}
		})
	}
}

// The ids of two real messages from different milliseconds are never
// adjacent, so the constructed summary id always fits between them.
func TestNextIDFitsBetweenRealIDs(t *testing.T) {
	msgs := cpChat(20, 10)
	for i := 0; i+1 < len(msgs); i++ {
		id, ok := nextID(msgs[i].ID)
		if !ok {
			t.Fatalf("nextID(%s) refused", msgs[i].ID)
		}
		if id <= msgs[i].ID || id >= msgs[i+1].ID {
			t.Fatalf("%s does not sit between %s and %s", id, msgs[i].ID, msgs[i+1].ID)
		}
	}
}

// When the id just above the last summarised message is already taken by the
// first kept message, the cut moves one group earlier.
func TestChooseCutAdjacencyFallback(t *testing.T) {
	ids := []event.ID{
		"01ARZ3NDEK0000000000000000",
		"01ARZ3NDEK0000000000000001",
		"01ARZ3NDEL0000000000000000",
		"01ARZ3NDEL0000000000000001",
		"01ARZ3NDEM0000000000000000",
	}
	msgs := make([]event.Message, len(ids))
	for i, id := range ids {
		role := event.RoleUser
		if i%2 == 1 {
			role = event.RoleAssistant
		}
		msgs[i] = event.Message{ID: id, Role: role, Text: cpFiller(400)}
	}
	// Candidate boundaries are the user messages at 2 and 4. Cutting at 4
	// would need the id above msgs[3], which is msgs[4] itself.
	items := cpItems(msgs)
	cut, summaryID, ok := chooseCut(msgs, items)
	if !ok {
		t.Fatal("chooseCut refused")
	}
	if cut != 2 {
		t.Fatalf("cut = %d, want 2 (one group earlier than the adjacent one)", cut)
	}
	if summaryID <= msgs[1].ID || summaryID >= msgs[2].ID {
		t.Fatalf("summary id %s does not sit between %s and %s", summaryID, msgs[1].ID, msgs[2].ID)
	}
}

// The cut never splits an assistant tool call from its results, whatever the
// shape of the transcript.
func TestChooseCutNeverSplitsToolGroups(t *testing.T) {
	assistantCalls := func(offset int, ids ...string) event.Message {
		m := cpAt(offset, event.RoleAssistant, "working")
		for _, id := range ids {
			m.ToolCalls = append(m.ToolCalls, event.ToolCall{ID: id, Name: "files.read", Arguments: "{}"})
		}
		return m
	}
	shapes := map[string][]event.Message{
		"one call one result": {
			cpAt(0, event.RoleUser, cpFiller(400)),
			assistantCalls(1, "a"),
			cpTool(2, "files.read", "a", cpFiller(400)),
			cpAt(3, event.RoleUser, cpFiller(400)),
			assistantCalls(4, "b"),
			cpTool(5, "files.read", "b", cpFiller(400)),
			cpAt(6, event.RoleUser, cpFiller(100)),
			cpAt(7, event.RoleAssistant, cpFiller(100)),
		},
		"several results per call": {
			cpAt(0, event.RoleUser, cpFiller(400)),
			assistantCalls(1, "a", "b", "c"),
			cpTool(2, "files.read", "a", cpFiller(200)),
			cpTool(3, "files.read", "b", cpFiller(200)),
			cpTool(4, "files.read", "c", cpFiller(200)),
			cpAt(5, event.RoleUser, cpFiller(400)),
			assistantCalls(6, "d"),
			cpTool(7, "files.read", "d", cpFiller(400)),
		},
		"back to back tool turns": {
			cpAt(0, event.RoleUser, cpFiller(300)),
			assistantCalls(1, "a"),
			cpTool(2, "files.read", "a", cpFiller(300)),
			assistantCalls(3, "b"),
			cpTool(4, "files.read", "b", cpFiller(300)),
			cpAt(5, event.RoleUser, cpFiller(300)),
			assistantCalls(6, "c"),
			cpTool(7, "files.read", "c", cpFiller(300)),
		},
		"orphan tool result at the boundary": {
			cpAt(0, event.RoleUser, cpFiller(400)),
			assistantCalls(1, "a"),
			cpTool(2, "files.read", "a", cpFiller(400)),
			cpTool(3, "files.read", "ghost", cpFiller(400)), // crashed turn
			cpAt(4, event.RoleUser, cpFiller(400)),
			cpAt(5, event.RoleAssistant, cpFiller(400)),
		},
	}
	for name, msgs := range shapes {
		t.Run(name, func(t *testing.T) {
			cut, _, ok := chooseCut(msgs, cpItems(msgs))
			if !ok {
				t.Fatal("chooseCut refused")
			}
			if msgs[cut].Role == event.RoleTool {
				t.Fatalf("cut %d starts on a tool result: the call would be orphaned", cut)
			}
			if msgs[cut].Role != event.RoleUser {
				t.Fatalf("cut %d starts on a %s, not a user turn", cut, msgs[cut].Role)
			}
			// Every kept tool result has its call in the kept region.
			calls := map[string]bool{}
			for _, m := range msgs[cut:] {
				for _, tc := range m.ToolCalls {
					calls[tc.ID] = true
				}
				if m.Role == event.RoleTool && !calls[m.ToolCallID] {
					t.Fatalf("kept tool result %s has no kept call", m.ToolCallID)
				}
			}
			// And every summarised call has its results summarised too.
			for _, m := range msgs[:cut] {
				for _, tc := range m.ToolCalls {
					found := false
					for _, r := range msgs[:cut] {
						if r.Role == event.RoleTool && r.ToolCallID == tc.ID {
							found = true
						}
					}
					if !found {
						t.Fatalf("call %s was summarised without its result", tc.ID)
					}
				}
			}
		})
	}
}

func TestChooseCutNothingToSummarise(t *testing.T) {
	msgs := []event.Message{cpAt(0, event.RoleUser, "hi"), cpAt(1, event.RoleAssistant, "hello")}
	if _, _, ok := chooseCut(msgs, cpItems(msgs)); ok {
		t.Fatal("chooseCut accepted a transcript with a single user turn")
	}
}

func TestCompactNoCutPoint(t *testing.T) {
	g := newCompactRig(t, cpSummaryStep("S"))
	g.add(cpAt(0, event.RoleUser, cpFiller(3000)), cpAt(1, event.RoleAssistant, cpFiller(200)))
	res, err := g.c.Compact(context.Background(), g.in)
	var ce *CompactError
	if !errors.As(err, &ce) || ce.Kind != "no_cut_point" {
		t.Fatalf("err = %v, want no_cut_point", err)
	}
	if res.Compacted || g.prov.n != 0 || g.st.appends != 0 {
		t.Fatalf("res = %+v, calls = %d", res, g.prov.n)
	}
}

// The kept tail is the smallest one that still reaches MinTailTokens.
func TestChooseCutKeepsMinimalTail(t *testing.T) {
	msgs := cpChat(20, 200)
	items := cpItems(msgs)
	cut, _, ok := chooseCut(msgs, items)
	if !ok {
		t.Fatal("chooseCut refused")
	}
	if tail := sumTokens(items[cut:]); tail < MinTailTokens {
		t.Fatalf("tail is %d tokens, want at least %d", tail, MinTailTokens)
	}
	// One group less would not reach the minimum.
	if cut+2 < len(msgs) {
		if tail := sumTokens(items[cut+2:]); tail >= MinTailTokens {
			t.Fatalf("a smaller tail of %d tokens was available at %d", tail, cut+2)
		}
	}
}

// The final pairing pass moves a cut off any tool result at the head of the
// kept tail: its call is necessarily in the summarised prefix.
func TestRepairCut(t *testing.T) {
	msgs := []event.Message{
		cpAt(0, event.RoleUser, "go"),
		cpAt(1, event.RoleAssistant, "working"),
		cpTool(2, "files.read", "a", "one"),
		cpTool(3, "files.read", "b", "two"),
		cpAt(4, event.RoleUser, "thanks"),
	}
	for cut, want := range map[int]int{0: 0, 1: 1, 2: 4, 3: 4, 4: 4} {
		if got := repairCut(msgs, cut); got != want {
			t.Fatalf("repairCut(%d) = %d, want %d", cut, got, want)
		}
	}
	if got := repairCut(msgs[:4], 2); got != 4 {
		t.Fatalf("repairCut past the end = %d, want 4", got)
	}
}

func cpItems(msgs []event.Message) []built {
	c := &Compactor{}
	return c.convertAll(context.Background(), CompactInput{}, msgs)
}

// The summary slots in by position, not id: a question taken by a later turn
// has an id older than the answer before it, but is placed after it.
func TestChooseCutUsesPosition(t *testing.T) {
	msgs := []event.Message{
		{ID: "01ARZ3NDEK0000000000000000", Role: event.RoleUser, Text: cpFiller(400)},
		{ID: "01ARZ3NDEM0000000000000000", Role: event.RoleAssistant, Text: cpFiller(400)},
		{ID: "01ARZ3NDEK0000000000000001", Pos: "01ARZ3NDEN0000000000000000", Role: event.RoleUser, Text: cpFiller(400)},
	}
	cut, summaryID, ok := chooseCut(msgs, cpItems(msgs))
	if !ok || cut != 2 {
		t.Fatalf("cut = %d, ok = %v", cut, ok)
	}
	if summaryID <= msgs[1].ID || summaryID >= msgs[2].Pos {
		t.Fatalf("summary id %s does not sit between %s and %s", summaryID, msgs[1].ID, msgs[2].Pos)
	}
}
