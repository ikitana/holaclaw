package googlechat

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"holaclaw/internal/channel"
	"holaclaw/internal/event"
)

var t0 = time.Date(2026, 10, 6, 10, 0, 0, 0, time.UTC)

func TestPullStoresThenAcksInCreateTimeOrder(t *testing.T) {
	f := newFake(t)
	late := encode(msgEvent("spaces/D1", "DIRECT_MESSAGE", "m2", t0.Add(time.Second), "second", nil))
	early := encode(msgEvent("spaces/D1", "DIRECT_MESSAGE", "m1", t0, "first", nil))
	f.queuePull([]string{late, early})

	a, _ := newAdapter(t, f, nil)
	var mu sync.Mutex
	var order []string // "in:<text>" and "ack:<n>" as they happen
	in := func(_ context.Context, env event.Envelope) error {
		mu.Lock()
		defer mu.Unlock()
		order = append(order, "in:"+env.Message.Text+":acks="+strconv.Itoa(len(f.records("ack"))))
		return nil
	}
	r := run(t, a, in)
	st := r.waitStatus(t, channel.StateConnected)
	if st.detail != "projects/proj-1/subscriptions/holaclaw" {
		t.Fatalf("connected detail = %q", st.detail)
	}
	eventually(t, "two acks", func() bool { return len(f.records("ack")) == 2 })
	_ = r.stop()

	mu.Lock()
	defer mu.Unlock()
	want := []string{"in:first:acks=0", "in:second:acks=1"}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Fatalf("order = %v, want %v (sorted by createTime, each stored before its ack)", order, want)
	}
	pull := f.records("pull")[0].json(t)
	if pull["returnImmediately"] != false || pull["maxMessages"] == nil {
		t.Fatalf("pull body = %v", pull)
	}
	if len(f.records("modack")) != 0 {
		t.Fatal("nothing should be nacked")
	}
}

func TestInboundErrorNacksRestOfBatch(t *testing.T) {
	f := newFake(t)
	m1 := encode(msgEvent("spaces/D1", "DIRECT_MESSAGE", "m1", t0, "one", nil))
	m2 := encode(msgEvent("spaces/D1", "DIRECT_MESSAGE", "m2", t0.Add(time.Second), "two", nil))
	f.queuePull([]string{m1, m2})
	f.queuePull([]string{m1, m2}) // Pub/Sub redelivers both

	a, _ := newAdapter(t, f, nil)
	var mu sync.Mutex
	fail := true
	var stored []string
	in := func(_ context.Context, env event.Envelope) error {
		mu.Lock()
		defer mu.Unlock()
		if fail {
			fail = false
			return errors.New("store down")
		}
		stored = append(stored, env.Message.Text)
		return nil
	}
	r := run(t, a, in)
	eventually(t, "redelivered batch acked", func() bool { return len(f.records("ack")) == 2 })
	_ = r.stop()

	mods := f.records("modack")
	if len(mods) != 1 {
		t.Fatalf("modifyAckDeadline calls = %d, want 1", len(mods))
	}
	body := mods[0].json(t)
	if body["ackDeadlineSeconds"] != float64(0) || len(body["ackIds"].([]any)) != 2 {
		t.Fatalf("modifyAckDeadline body = %v, want both ids with deadline 0", body)
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(stored, ",") != "one,two" {
		t.Fatalf("stored = %v", stored)
	}
}

// A rejection that persists is redelivered at once by Pub/Sub; the loop backs
// off before the next pull instead of spinning on it.
func TestPersistentInboundErrorBacksOff(t *testing.T) {
	oldBase, oldMax := backoffBase, backoffMax
	backoffBase, backoffMax = 20*time.Millisecond, 80*time.Millisecond
	t.Cleanup(func() { backoffBase, backoffMax = oldBase, oldMax })

	f := newFake(t)
	m := encode(msgEvent("spaces/D1", "DIRECT_MESSAGE", "m1", t0, "one", nil))
	f.handle("pull", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, 200, map[string]any{"receivedMessages": []any{
			map[string]any{"ackId": "ack-1", "message": map[string]any{"data": m, "messageId": "1"}},
		}})
	})
	a, _ := newAdapter(t, f, nil)
	var mu sync.Mutex
	accept := false
	in := func(context.Context, event.Envelope) error {
		mu.Lock()
		defer mu.Unlock()
		if !accept {
			return errors.New("store down")
		}
		return nil
	}
	r := run(t, a, in)
	time.Sleep(400 * time.Millisecond)
	pulls := len(f.records("pull"))
	if pulls < 2 || pulls > 25 {
		t.Fatalf("pulls in 400ms = %d, want a handful (backed off, still retrying)", pulls)
	}
	if n := len(f.records("modack")); n < pulls-1 {
		t.Fatalf("nacks = %d, pulls = %d: every rejected batch is nacked", n, pulls)
	}

	// Once Inbound accepts, the event is acked and the loop goes on.
	mu.Lock()
	accept = true
	mu.Unlock()
	eventually(t, "the event acked", func() bool { return len(f.records("ack")) > 0 })
	_ = r.stop()
}

func TestUnparseableEventsAreAcked(t *testing.T) {
	f := newFake(t)
	secret := "very-private-message-content"
	f.queuePull([]string{
		"!!!not base64!!!",
		encode(map[string]any{"type": "MESSAGE"}), // no message
		encodeRaw("{not json " + secret),
		encode(map[string]any{"type": "WIDGET_UPDATED", "x": secret}),
		encode(map[string]any{"eventTime": "x"}),
	})
	a, logs := newAdapter(t, f, nil)
	r := run(t, a, nil)
	eventually(t, "junk acked", func() bool { return len(f.records("ack")) == 1 })
	_ = r.stop()
	if ids := f.records("ack")[0].json(t)["ackIds"].([]any); len(ids) != 5 {
		t.Fatalf("acked %d of 5", len(ids))
	}
	if len(r.envs) != 0 {
		t.Fatal("junk produced an envelope")
	}
	if strings.Contains(logs.String(), secret) {
		t.Fatalf("event content reached the log:\n%s", logs.String())
	}
	if !strings.Contains(logs.String(), "unhandled event type WIDGET_UPDATED") {
		t.Fatalf("drop reason not logged:\n%s", logs.String())
	}
}

func encodeRaw(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func TestRedeliveryIsDeduped(t *testing.T) {
	f := newFake(t)
	m := encode(msgEvent("spaces/D1", "DIRECT_MESSAGE", "m1", t0, "hi", nil))
	f.queuePull([]string{m})
	f.queuePull([]string{m})
	a, _ := newAdapter(t, f, nil)
	r := run(t, a, nil)
	eventually(t, "both acked", func() bool { return len(f.records("ack")) == 2 })
	_ = r.stop()
	if env := r.nextEnv(t); env.Origin.PlatformMsgID != "spaces/D1/messages/m1" {
		t.Fatalf("env = %+v", env.Origin)
	}
	if n := len(r.envs); n != 0 {
		t.Fatalf("%d more envelopes, want the message once", n)
	}
}

func TestStatusLoggedOutThenConnected(t *testing.T) {
	for _, code := range []int{401, 403} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			f := newFake(t)
			f.queuePull(code)
			f.queuePull(code) // a 401 is retried once with a fresh token
			a, _ := newAdapter(t, f, nil)
			r := run(t, a, nil)
			st := r.waitStatus(t, channel.StateLoggedOut)
			if !strings.Contains(st.detail, http.StatusText(code)) && !strings.Contains(st.detail, "scripted") {
				t.Fatalf("detail = %q", st.detail)
			}
			r.waitStatus(t, channel.StateConnected)
		})
	}
}

// TestConnectedBeforeFirstPullReturns: Pub/Sub holds an empty pull open, so
// connected comes from the subscription check, not from the first pull.
func TestConnectedBeforeFirstPullReturns(t *testing.T) {
	f := newFake(t)
	release := make(chan struct{})
	f.handle("pull", func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
		writeJSON(w, 200, map[string]any{})
	})
	a, _ := newAdapter(t, f, nil)
	r := run(t, a, nil)
	defer close(release)
	st := r.waitStatus(t, channel.StateConnected)
	if st.detail != "projects/proj-1/subscriptions/holaclaw" {
		t.Fatalf("connected detail = %q", st.detail)
	}
	if n := len(f.records("subcheck")); n != 1 {
		t.Fatalf("subscription checks = %d, want 1", n)
	}
}

// TestSubscriptionCheckRefusedIsLoggedOut: a key without Pub/Sub Subscriber
// is logged out before any pull.
func TestSubscriptionCheckRefusedIsLoggedOut(t *testing.T) {
	f := newFake(t)
	f.handle("subcheck", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, 200, map[string]any{})
	})
	a, _ := newAdapter(t, f, nil)
	r := run(t, a, nil)
	st := r.waitStatus(t, channel.StateLoggedOut)
	if !strings.Contains(st.detail, consumePermission) {
		t.Fatalf("detail = %q", st.detail)
	}
	if n := len(f.records("pull")); n != 0 {
		t.Fatalf("pulls = %d, want none before the check passes", n)
	}
}

// TestFailedPullChecksAgain: after a failed pull, connected is reported only
// once the subscription check passes again.
func TestFailedPullChecksAgain(t *testing.T) {
	f := newFake(t)
	f.queuePull(503)
	a, _ := newAdapter(t, f, nil)
	r := run(t, a, nil)
	r.waitStatus(t, channel.StateConnected)
	r.waitStatus(t, channel.StateReconnecting)
	r.waitStatus(t, channel.StateConnected)
	if n := len(f.records("subcheck")); n < 2 {
		t.Fatalf("subscription checks = %d, want one per connection", n)
	}
}

func TestStatusReconnectingOnTransient(t *testing.T) {
	f := newFake(t)
	f.queuePull(503)
	f.queuePull(429)
	a, _ := newAdapter(t, f, nil)
	r := run(t, a, nil)
	r.waitStatus(t, channel.StateReconnecting)
	r.waitStatus(t, channel.StateConnected)
}

func TestMissingKeyIsUnlinked(t *testing.T) {
	f := newFake(t)
	var mu sync.Mutex
	key := ""
	a, _ := newAdapter(t, f, func(o *Options) {
		o.Key = func() string { mu.Lock(); defer mu.Unlock(); return key }
	})
	r := run(t, a, nil)
	r.waitStatus(t, channel.StateUnlinked)
	if len(f.records("")) != 0 {
		t.Fatal("no request should be made without a key")
	}
	mu.Lock()
	key = keyJSON(nil)
	mu.Unlock()
	r.waitStatus(t, channel.StateConnected)
}

func TestBadKeyIsLoggedOut(t *testing.T) {
	f := newFake(t)
	a, _ := newAdapter(t, f, func(o *Options) { o.Key = func() string { return `{"type":"authorized_user"}` } })
	r := run(t, a, nil)
	st := r.waitStatus(t, channel.StateLoggedOut)
	if !strings.Contains(st.detail, "bad_key") {
		t.Fatalf("detail = %q", st.detail)
	}
}

func TestRunReturnsOnCancel(t *testing.T) {
	f := newFake(t)
	a, _ := newAdapter(t, f, nil)
	r := run(t, a, nil)
	r.waitStatus(t, channel.StateConnected)
	if err := r.stop(); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run returned %v", err)
	}
}
