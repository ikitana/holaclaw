package googlechat

import (
	"context"
	"strings"
	"sync"
	"testing"

	"holaclaw/internal/event"
)

const approvalID = event.ID("01J9Z3K4M5N6P7Q8R9S0T1V2W3")

func TestPromptApprovalPostsCard(t *testing.T) {
	f := newFake(t)
	a, _ := newAdapter(t, f, nil)
	target := event.Origin{Conversation: "spaces/S1", Thread: "spaces/S1/threads/T1"}
	if err := a.PromptApproval(context.Background(), target, approvalID, "Allow: **exec**?", []string{"once", "session", "always", "deny"}); err != nil {
		t.Fatal(err)
	}
	c := f.records("create")
	if len(c) != 1 {
		t.Fatalf("creates = %d", len(c))
	}
	body := c[0].json(t)
	if body["text"] != "Allow: *exec*?" {
		t.Errorf("text = %v", body["text"])
	}
	cards := body["cardsV2"].([]any)
	card := cards[0].(map[string]any)
	if card["cardId"] != "approval-"+string(approvalID) {
		t.Errorf("cardId = %v", card["cardId"])
	}
	widgets := card["card"].(map[string]any)["sections"].([]any)[0].(map[string]any)["widgets"].([]any)
	buttons := widgets[0].(map[string]any)["buttonList"].(map[string]any)["buttons"].([]any)
	if len(buttons) != 4 {
		t.Fatalf("buttons = %d", len(buttons))
	}
	b := buttons[3].(map[string]any)
	action := b["onClick"].(map[string]any)["action"].(map[string]any)
	params := action["parameters"].([]any)
	if b["text"] != "deny" || action["function"] != "hb_approval" ||
		params[0].(map[string]any)["value"] != string(approvalID) || params[1].(map[string]any)["value"] != "deny" {
		t.Fatalf("button = %v", b)
	}
	if c[0].Query.Get("messageReplyOption") == "" {
		t.Error("the prompt goes to the conversation's thread")
	}
}

type pressRec struct {
	id      event.ID
	choice  string
	aliases string
}

func clickEvent(fn string, params map[string]string, user map[string]any) map[string]any {
	var list []any
	for k, v := range params {
		list = append(list, map[string]any{"key": k, "value": v})
	}
	return map[string]any{
		"type":    "CARD_CLICKED",
		"space":   map[string]any{"name": "spaces/S1", "spaceType": "SPACE"},
		"message": map[string]any{"name": "spaces/S1/messages/M1", "text": "Allow: exec?", "sender": map[string]any{"name": "users/app", "type": "BOT"}},
		"user":    user,
		"action":  map[string]any{"actionMethodName": fn, "parameters": list},
	}
}

func TestCardClickUsesEventUserAndPatchesCard(t *testing.T) {
	f := newFake(t)
	var mu sync.Mutex
	var got []pressRec
	a, _ := newAdapter(t, f, func(o *Options) {
		o.OnApproval = func(_ context.Context, id event.ID, choice string, from []string) bool {
			mu.Lock()
			defer mu.Unlock()
			got = append(got, pressRec{id, choice, strings.Join(from, ",")})
			return true
		}
	})
	ev := clickEvent("hb_approval", map[string]string{
		"approval_id": string(approvalID), "choice": "once",
		"user": "users/OWNER", "aliases": "owner@example.com", // forged: must be ignored
	}, alice)
	if envs := deliver(t, a, ev); len(envs) != 0 {
		t.Fatal("a click is no envelope")
	}
	mu.Lock()
	if len(got) != 1 || got[0] != (pressRec{approvalID, "once", "users/111,alice@example.com"}) {
		t.Fatalf("OnApproval = %+v", got)
	}
	mu.Unlock()

	p := f.records("patch")
	if len(p) != 1 || p[0].Path != "/v1/spaces/S1/messages/M1" || p[0].Query.Get("updateMask") != "text,cardsV2" {
		t.Fatalf("patch = %+v", p)
	}
	body := p[0].json(t)
	if body["text"] != "Allow: exec?\n\n_Approved (once) by Alice_" {
		t.Errorf("patched text = %q", body["text"])
	}
	if cards, ok := body["cardsV2"].([]any); !ok || len(cards) != 0 {
		t.Errorf("cardsV2 must be sent empty to remove the buttons: %v", body["cardsV2"])
	}
	if len(f.records("ack")) != 1 {
		t.Error("click not acked")
	}

	// Pub/Sub redelivers the same click: one press only.
	deliver(t, a, ev)
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("redelivered click resolved again: %+v", got)
	}
}

func TestCardClickFromCommonLayout(t *testing.T) {
	f := newFake(t)
	called := 0
	a, _ := newAdapter(t, f, func(o *Options) {
		o.OnApproval = func(context.Context, event.ID, string, []string) bool { called++; return true }
	})
	ev := clickEvent("", nil, alice)
	delete(ev, "action")
	ev["common"] = map[string]any{"invokedFunction": "hb_approval", "parameters": map[string]any{"approval_id": string(approvalID), "choice": "deny"}}
	deliver(t, a, ev)
	if called != 1 {
		t.Fatalf("called = %d", called)
	}
	if text := f.records("patch")[0].json(t)["text"]; text != "Allow: exec?\n\n_Denied by Alice_" {
		t.Errorf("patched text = %q", text)
	}
}

// A press core refuses (not an owner or admin, or already settled) leaves the
// card and its buttons in place for an admin to answer.
func TestCardClickNotAcceptedLeavesCard(t *testing.T) {
	f := newFake(t)
	called := 0
	a, _ := newAdapter(t, f, func(o *Options) {
		o.OnApproval = func(context.Context, event.ID, string, []string) bool { called++; return false }
	})
	deliver(t, a, clickEvent("hb_approval", map[string]string{"approval_id": string(approvalID), "choice": "once"}, alice))
	if called != 1 {
		t.Fatalf("called = %d", called)
	}
	if p := f.records("patch"); len(p) != 0 {
		t.Fatalf("a refused press rewrote the card: %+v", p)
	}
	if len(f.records("ack")) != 1 {
		t.Error("click not acked")
	}
}

func TestCardClickJunkIgnored(t *testing.T) {
	f := newFake(t)
	called := 0
	a, _ := newAdapter(t, f, func(o *Options) {
		o.OnApproval = func(context.Context, event.ID, string, []string) bool { called++; return true }
	})
	bot := map[string]any{"name": "users/999", "type": "BOT"}
	for _, ev := range []map[string]any{
		clickEvent("other_fn", map[string]string{"approval_id": string(approvalID), "choice": "once"}, alice),
		clickEvent("hb_approval", map[string]string{"approval_id": "not-a-ulid", "choice": "once"}, alice),
		clickEvent("hb_approval", map[string]string{"approval_id": string(approvalID), "choice": "ONCE; drop"}, alice),
		clickEvent("hb_approval", map[string]string{"approval_id": string(approvalID)}, alice),
		clickEvent("hb_approval", map[string]string{"approval_id": string(approvalID), "choice": "once"}, nil),
		clickEvent("hb_approval", map[string]string{"approval_id": string(approvalID), "choice": "once"}, bot),
	} {
		deliver(t, a, ev)
	}
	if called != 0 || len(f.records("patch")) != 0 {
		t.Fatalf("junk resolved an approval: called=%d patches=%d", called, len(f.records("patch")))
	}
	if len(f.records("ack")) != 6 {
		t.Fatalf("acks = %d, want 6", len(f.records("ack")))
	}
}

func TestSettleDefusesDisplayName(t *testing.T) {
	f := newFake(t)
	a, _ := newAdapter(t, f, func(o *Options) {
		o.OnApproval = func(context.Context, event.ID, string, []string) bool { return true }
	})
	evil := human("666", "*<users/all>*", "")
	deliver(t, a, clickEvent("hb_approval", map[string]string{"approval_id": string(approvalID), "choice": "deny"}, evil))
	text := f.records("patch")[0].json(t)["text"].(string)
	if strings.Contains(text, "<users/all>") {
		t.Fatalf("display name reached the card as markup: %q", text)
	}
}
