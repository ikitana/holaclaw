package slack

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"testing"
	"time"

	"holaclaw/internal/channel"
	"holaclaw/internal/event"
)

func TestConnectHelloReportsConnected(t *testing.T) {
	f := newFakeSlack(t)
	a, _ := newAdapter(t, f, nil)
	st, stop := run(t, a, nil)
	defer func() { _ = stop() }()

	c := f.conn(t)
	c.push(t, map[string]any{"type": "hello", "num_connections": 1})
	s := waitStatus(t, st, channel.StateConnected)
	if !strings.Contains(s.msg, "UBOT") {
		t.Fatalf("status message %q does not name the bot user", s.msg)
	}
	auth := f.waitCalls(t, "auth.test", 1)
	if auth[0].Auth != "Bearer "+fakeBotToken {
		t.Fatalf("auth.test used %q", auth[0].Auth)
	}
	if got := f.callsFor("apps.connections.open"); len(got) != 1 || got[0].Auth != "Bearer "+fakeAppToken {
		t.Fatalf("apps.connections.open calls: %+v", got)
	}
}

func TestInvalidAuthLogsOutWithoutRetrying(t *testing.T) {
	f := newFakeSlack(t)
	f.set("auth.test", fail("invalid_auth"))
	a, logs := newAdapter(t, f, nil)

	st, stop := run(t, a, nil)
	waitStatus(t, st, channel.StateLoggedOut)
	err := stop()
	if !errors.Is(err, ErrInvalidAuth) {
		t.Fatalf("Run error = %v, want ErrInvalidAuth", err)
	}
	if got := f.callsFor("auth.test"); len(got) != 1 {
		t.Fatalf("auth.test called %d times, want 1 (no retry loop)", len(got))
	}
	assertNoTokens(t, logs.String(), err.Error())
}

func TestAckPrecedesInboundWork(t *testing.T) {
	f := newFakeSlack(t)
	a, _ := newAdapter(t, f, nil)
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	_, stop := run(t, a, func(context.Context, event.Envelope) error {
		entered <- struct{}{}
		<-release
		return nil
	})
	defer func() { _ = stop() }()

	c := f.conn(t)
	c.push(t, map[string]any{"type": "hello"})
	c.push(t, map[string]any{
		"type":        "events_api",
		"envelope_id": "env-1",
		"payload":     eventsPayloadFor("Ev1", dmEvent("hi")),
	})
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("inbound never called")
	}
	// Inbound is still blocked: the ack must already be on the wire, well
	// inside Slack's 3s window.
	if id := c.ack(t, ackDeadline); id != "env-1" {
		t.Fatalf("ack = %q, want env-1", id)
	}
	close(release)
}

func TestDisconnectEnvelopeReconnects(t *testing.T) {
	f := newFakeSlack(t)
	a, _ := newAdapter(t, f, nil)
	st, stop := run(t, a, nil)
	defer func() { _ = stop() }()

	c := f.conn(t)
	c.push(t, map[string]any{"type": "hello"})
	waitStatus(t, st, channel.StateConnected)
	c.push(t, map[string]any{"type": "disconnect", "reason": "refresh_requested"})
	waitStatus(t, st, channel.StateReconnecting)

	c2 := f.conn(t)
	c2.push(t, map[string]any{"type": "hello"})
	waitStatus(t, st, channel.StateConnected)
	if n := len(f.callsFor("apps.connections.open")); n < 2 {
		t.Fatalf("apps.connections.open called %d times, want >= 2", n)
	}
	// Identity is resolved once and reused across connections.
	if n := len(f.callsFor("auth.test")); n != 1 {
		t.Fatalf("auth.test called %d times, want 1", n)
	}
}

func TestSocketDropReconnectsWithBackoff(t *testing.T) {
	f := newFakeSlack(t)
	a, _ := newAdapter(t, f, nil)
	st, stop := run(t, a, nil)
	defer func() { _ = stop() }()

	c := f.conn(t)
	c.push(t, map[string]any{"type": "hello"})
	waitStatus(t, st, channel.StateConnected)

	start := time.Now()
	c.drop()
	waitStatus(t, st, channel.StateReconnecting)
	c2 := f.conn(t)
	c2.push(t, map[string]any{"type": "hello"})
	waitStatus(t, st, channel.StateConnected)
	if d := time.Since(start); d < backoffBase/2 {
		t.Fatalf("reconnected after %s, want at least the first backoff", d)
	}
}

func TestBackoffGrowsAndIsBounded(t *testing.T) {
	for attempt := range 12 {
		d := backoffFor(attempt)
		if d < backoffBase/2 || d > backoffMax {
			t.Fatalf("attempt %d: backoff %s out of range", attempt, d)
		}
	}
	if backoffFor(0) > 2*backoffBase {
		t.Fatalf("first backoff too large: %s", backoffFor(0))
	}
}

func TestNoGoroutineLeakAfterCancel(t *testing.T) {
	f := newFakeSlack(t)
	a, _ := newAdapter(t, f, nil)

	st, stop := run(t, a, nil)
	c := f.conn(t)
	c.push(t, map[string]any{"type": "hello"})
	waitStatus(t, st, channel.StateConnected)
	c.push(t, map[string]any{
		"type": "events_api", "envelope_id": "env-1",
		"payload": eventsPayloadFor("Ev1", dmEvent("hi")),
	})
	c.ack(t, ackDeadline)
	if err := stop(); err != nil {
		t.Fatalf("Run returned %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if adapterGoroutines() == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%d adapter goroutines still running after cancel", adapterGoroutines())
}

// adapterGoroutines counts goroutines still inside adapter code.
func adapterGoroutines() int {
	buf := make([]byte, 1<<20)
	n := runtime.Stack(buf, true)
	return strings.Count(string(buf[:n]), "channel/slack.(*Adapter)")
}

// assertNoTokens fails when a token value leaked into a string.
func assertNoTokens(t *testing.T, parts ...string) {
	t.Helper()
	for _, p := range parts {
		for _, tok := range []string{fakeBotToken, fakeAppToken} {
			if strings.Contains(p, tok) {
				t.Fatalf("token leaked into %q", p)
			}
		}
	}
}
