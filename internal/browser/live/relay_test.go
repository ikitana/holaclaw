package live

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"holaclaw/internal/browser/client"
	"holaclaw/internal/browser/proto"
)

// The sidecar sends text messages that are not meta — its activity account of
// what the engine served. They reach the watchers as they came, without the
// holder, and they do not become the message a watcher joining later is caught
// up with: that is the tab list and the viewport, and nothing else.
func TestRelayNonMetaTextIsNotTheState(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()
	if err := h.svc.Takeover(ctx, "main", "u1"); err != nil {
		t.Fatalf("takeover: %v", err)
	}
	first := newViewer()
	defer h.watch(t, "main", "u1", first)()
	up := h.side.stream(0)

	up.frames <- client.Frame{Meta: &proto.StreamMessage{Type: proto.StreamMeta, Width: 800, Height: 600}}
	up.frames <- client.Frame{Meta: &proto.StreamMessage{
		Type:     proto.StreamActivity,
		Activity: &proto.Activity{Op: "act", Kind: proto.ActClick, Ref: "g1r3", OK: true, At: time.Now().UTC()},
	}}

	meta := decode(t, <-first.texts)
	if meta.Type != proto.StreamMeta || meta.Holder != "u1" {
		t.Fatalf("meta = %+v", meta)
	}
	act := decode(t, <-first.texts)
	if act.Type != proto.StreamActivity || act.Activity == nil || act.Activity.Ref != "g1r3" {
		t.Fatalf("activity = %+v", act)
	}
	if act.Holder != "" {
		t.Fatalf("an activity message was given the holder: %+v", act)
	}

	// A watcher that joins now is caught up with the meta, not the activity.
	second := newViewer()
	defer h.watch(t, "main", "u2", second)()
	replay := decode(t, <-second.texts)
	if replay.Type != proto.StreamMeta || replay.Width != 800 {
		t.Fatalf("replay = %+v, want the last meta", replay)
	}
	select {
	case raw := <-second.texts:
		t.Fatalf("a joining watcher was replayed more than the meta: %s", raw)
	case <-time.After(50 * time.Millisecond):
	}
}

func decode(t testing.TB, raw []byte) proto.StreamMessage {
	t.Helper()
	var m proto.StreamMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return m
}
