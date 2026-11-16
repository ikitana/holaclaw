package whatsapp

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types/events"
)

const (
	number          = "34600000000"
	statusUnlinked  = "unlinked|not linked: run `holaclaw login whatsapp --name wa --phone …`"
	statusConnected = "connected|+" + number
)

// connected starts a linked adapter and brings it to connected.
func connected(t *testing.T) (*env, *fakeClient) {
	t.Helper()
	e := newEnv(t, number, nil)
	e.run()
	f := e.client(0)
	eventually(t, "connect", func() bool { return f.count(&f.connects) == 1 })
	f.emit(&events.Connected{})
	e.waitStatus(statusConnected)
	return e, f
}

func TestRunWithoutDeviceReportsUnlinked(t *testing.T) {
	e := newEnv(t, "", nil)
	e.run()
	e.waitStatus(statusUnlinked)
	f := e.client(0)
	if n := f.count(&f.connects); n != 0 {
		t.Fatalf("an unlinked device connected %d times", n)
	}
}

// The suggested command leaves out a channel named after its type, which
// the CLI picks by default.
func TestCLILine(t *testing.T) {
	if got := cliLine("login", "whatsapp"); got != "holaclaw login whatsapp" {
		t.Errorf("default name: %q", got)
	}
	if got := cliLine("logout", "wa"); got != "holaclaw logout whatsapp --name wa" {
		t.Errorf("other name: %q", got)
	}
}

func TestRunConnectsAndReportsTheNumber(t *testing.T) {
	connected(t)
}

func TestRunNeedsASessionPath(t *testing.T) {
	a := New(Options{Name: "wa"})
	if err := a.Run(t.Context(), nil, nil); err == nil {
		t.Fatal("Run without a session path succeeded")
	}
}

func TestDisconnectReconnectsWithBackoff(t *testing.T) {
	e, f := connected(t)
	f.mu.Lock()
	f.connectErr = []error{errors.New("dial: boom")}
	f.mu.Unlock()

	f.emit(&events.Disconnected{})
	e.waitStatus("reconnecting|disconnected")
	e.clock.fire(t, 2*time.Second)
	e.waitStatus("reconnecting|connect failed: dial: boom")
	e.clock.fire(t, 4*time.Second) // doubled
	eventually(t, "third connect", func() bool { return f.count(&f.connects) == 3 })
	f.emit(&events.Connected{})
	e.waitStatus(statusConnected)

	// Connected resets the backoff.
	f.emit(&events.Disconnected{})
	e.waitStatus("reconnecting|disconnected")
	e.clock.fire(t, 2*time.Second)
	eventually(t, "fourth connect", func() bool { return f.count(&f.connects) == 4 })
}

func TestInitialConnectFailureRetries(t *testing.T) {
	e := newEnv(t, number, func(f *fakeClient) { f.connectErr = []error{errors.New("no route")} })
	e.run()
	e.waitStatus("reconnecting|connect failed: no route")
	e.clock.fire(t, 2*time.Second)
	f := e.client(0)
	eventually(t, "retry", func() bool { return f.count(&f.connects) == 2 })
}

func TestKeepAliveTimeout(t *testing.T) {
	e, f := connected(t)
	// A recent success: whatsmeow keeps trying, and so do we.
	f.emit(&events.KeepAliveTimeout{ErrorCount: 1, LastSuccess: e.clock.Now().Add(-time.Minute)})
	time.Sleep(20 * time.Millisecond)
	if n := f.count(&f.disconnect); n != 0 || e.status.last() != statusConnected {
		t.Fatalf("a fresh keepalive failure disconnected %d times", n)
	}
	stale := &events.KeepAliveTimeout{ErrorCount: 5, LastSuccess: e.clock.Now().Add(-whatsmeow.KeepAliveMaxFailTime - time.Second)}
	f.emit(stale)
	e.waitStatus("reconnecting|keepalive timed out")
	// Timeouts still in flight after our Disconnect leave the armed retry be.
	f.emit(stale)
	f.emit(stale)
	time.Sleep(20 * time.Millisecond)
	if n := f.count(&f.disconnect); n != 1 || e.clock.armed(2*time.Second) != 1 || e.clock.armed(4*time.Second) != 0 {
		t.Fatalf("disconnects = %d, retries armed %d/%d; want 1, one 2 s retry", n, e.clock.armed(2*time.Second), e.clock.armed(4*time.Second))
	}
}

func TestConnectFailureReconnects(t *testing.T) {
	e, f := connected(t)
	f.emit(&events.ConnectFailure{Reason: events.ConnectFailureGeneric})
	e.waitStatus("reconnecting|connect failure: 400: unknown error")
}

// TestTemporaryBanWithoutEndBacksOff: a ban with no expiry grows the wait
// like any failure instead of retrying every 2 s.
func TestTemporaryBanWithoutEndBacksOff(t *testing.T) {
	e, f := connected(t)
	f.emit(&events.TemporaryBan{Code: events.TempBanBlockedByUsers})
	e.clock.fire(t, 2*time.Second)
	eventually(t, "retry", func() bool { return f.count(&f.connects) == 2 })
	f.emit(&events.TemporaryBan{Code: events.TempBanBlockedByUsers})
	e.clock.fire(t, 4*time.Second)
	eventually(t, "second retry", func() bool { return f.count(&f.connects) == 3 })
}

func TestLoggedOutWipesTheSession(t *testing.T) {
	e, f := connected(t)
	f.emit(&events.LoggedOut{Reason: events.ConnectFailureLoggedOut})
	e.waitStatus("logged_out|" + detailLoggedOut)
	fresh := e.client(1)
	if pn, _ := fresh.OwnID(); !pn.IsEmpty() {
		t.Fatal("the reopened session still holds a device")
	}
	raw, err := os.ReadFile(e.path)
	if err != nil || len(raw) != 0 {
		t.Fatalf("session file = %q, %v; want a new, empty one", raw, err)
	}
	if !f.isClosed() {
		t.Fatal("the old client was not closed")
	}
	// Events from the old client are ignored from now on.
	f.emit(&events.Connected{})
	time.Sleep(20 * time.Millisecond)
	if got := e.status.last(); got != "logged_out|"+detailLoggedOut {
		t.Fatalf("a stale event moved the status to %q", got)
	}
	// A new link works without a restart.
	fresh.mu.Lock()
	fresh.pairCode = fakeCode
	fresh.onConnect = func(f *fakeClient) { go f.emit(&events.QR{Codes: []string{"x"}}) }
	fresh.mu.Unlock()
	if _, err := e.a.Link(t.Context(), "+"+number); err != nil {
		t.Fatalf("link after logout: %v", err)
	}
}

func TestStreamReplacedStopsWithoutReconnecting(t *testing.T) {
	e, f := connected(t)
	f.emit(&events.StreamReplaced{})
	e.waitStatus("logged_out|" + detailReplaced)
	if n := f.count(&f.disconnect); n != 1 {
		t.Fatalf("disconnects = %d, want 1", n)
	}
	if n := e.clock.armed(2 * time.Second); n != 0 {
		t.Fatalf("%d reconnect timers armed after StreamReplaced", n)
	}
	time.Sleep(20 * time.Millisecond)
	if n := f.count(&f.connects); n != 1 {
		t.Fatalf("connects = %d, want 1", n)
	}
}

func TestTemporaryBanWaitsForTheBanToEnd(t *testing.T) {
	e, f := connected(t)
	f.emit(&events.TemporaryBan{Code: events.TempBanBlockedByUsers, Expire: time.Hour})
	e.waitStatus("logged_out|WhatsApp banned this number temporarily: 102: too many people blocked you, for 1h0m0s")
	if n := e.clock.armed(2 * time.Second); n != 0 {
		t.Fatal("a ban retried on the backoff")
	}
	e.clock.fire(t, time.Hour)
	eventually(t, "reconnect after the ban", func() bool { return f.count(&f.connects) == 2 })
	f.emit(&events.Connected{})
	e.waitStatus(statusConnected)
}

func TestClientOutdatedStops(t *testing.T) {
	e, f := connected(t)
	f.emit(&events.ClientOutdated{})
	e.waitStatus("logged_out|" + detailOutdated)
	if n := e.clock.armed(2 * time.Second); n != 0 {
		t.Fatal("an outdated client retried")
	}
}

func TestBackoffCapAndJitter(t *testing.T) {
	a := New(Options{})
	a.jitter = func(n int64) int64 { return n - 1 }
	want := []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second}
	for n, w := range want {
		if got := a.backoff(n); got != w {
			t.Errorf("backoff(%d) top = %s, want %s", n, got, w)
		}
	}
	for _, n := range []int{8, 9, 40, 1000} {
		if got := a.backoff(n); got != backoffMax {
			t.Errorf("backoff(%d) = %s, want the cap %s", n, got, backoffMax)
		}
	}
	a.jitter = func(int64) int64 { return 0 }
	if got := a.backoff(0); got != time.Second {
		t.Errorf("backoff(0) bottom = %s, want 1s", got)
	}
	if got := a.backoff(100); got != backoffMax/2 {
		t.Errorf("backoff(100) bottom = %s, want %s", got, backoffMax/2)
	}
	// The real jitter stays inside [d/2, d].
	b := New(Options{})
	for range 200 {
		if got := b.backoff(3); got < 8*time.Second || got > 16*time.Second {
			t.Fatalf("backoff(3) = %s, outside [8s, 16s]", got)
		}
	}
}

func TestRunTwiceIsRefused(t *testing.T) {
	e := newEnv(t, "", nil)
	e.run()
	e.waitStatus(statusUnlinked)
	if err := e.a.Run(t.Context(), nil, nil); err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("second Run: %v", err)
	}
}
