//go:build linux

package page

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"holaclaw/internal/browser/proto"
)

// The live view carries the engine's own account of what it served: the ring
// of what happened before the viewer arrived, then each observe and act as it
// returns — and never the text an action typed.
func TestIntegrationActivityReachesTheStream(t *testing.T) {
	e := open(t, "alpha")
	goTo(t, e, "alpha", "/form")
	obs := observe(t, e, "alpha", proto.ObserveRequest{TextBytes: 1 << 10})
	dep := refOf(t, obs, "textbox", "Departure airport")

	v := &testViewer{}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	in := make(chan proto.StreamMessage, 4)
	done := make(chan error, 1)
	go func() { done <- e.Stream(ctx, "alpha", in, v) }()

	// The navigate and the observe are already in the ring: a viewer that
	// joins mid-task is told how the page got where it is.
	waitFor(t, "the activity ring replay", func() bool {
		return len(activitiesOf(v, "observe")) > 0 && len(activitiesOf(v, "act")) > 0
	})
	first := activitiesOf(v, "observe")[0]
	if first.Total == 0 || len(first.Shortlist) == 0 || !first.OK {
		t.Fatalf("replayed observation = %+v", first)
	}

	// And what happens next arrives as it happens.
	before := len(activitiesOf(v, "act"))
	res, err := e.Act(ctxT(t), "alpha", proto.Action{
		Kind: proto.ActType, Ref: dep, Text: "flamingo", Clear: true,
	})
	if err != nil || !res.OK {
		t.Fatalf("type: %+v, %v", res, err)
	}
	waitFor(t, "the typing to reach the viewer", func() bool {
		return len(activitiesOf(v, "act")) > before
	})
	last := activitiesOf(v, "act")
	typed := last[len(last)-1]
	if typed.Kind != proto.ActType || typed.Ref != dep || !typed.Clear || !typed.OK {
		t.Fatalf("typing = %+v", typed)
	}
	if !strings.HasPrefix(typed.Target, "textbox ") {
		t.Fatalf("typing target = %q, want the element the last observation named", typed.Target)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Stream did not return")
	}

	// Nothing the action typed is anywhere on the wire.
	v.mu.Lock()
	raw, _ := json.Marshal(v.metas)
	v.mu.Unlock()
	if strings.Contains(string(raw), "flamingo") {
		t.Fatal("the typed text reached the live view")
	}
}

// activitiesOf is the viewer's activity messages of one kind, in order.
func activitiesOf(v *testViewer, op string) []proto.Activity {
	v.mu.Lock()
	defer v.mu.Unlock()
	var out []proto.Activity
	for _, m := range v.metas {
		if m.Type == proto.StreamActivity && m.Activity != nil && m.Activity.Op == op {
			out = append(out, *m.Activity)
		}
	}
	return out
}
