package whatsapp

import (
	"bytes"
	"errors"
	"log/slog"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"

	"holaclaw/internal/channel"
)

const statusLinking = "linking|" + detailLinking

// pairable scripts a fresh device that opens the linking socket and hands out
// fakeCode.
func pairable(f *fakeClient) {
	f.pairCode = fakeCode
	f.onConnect = func(f *fakeClient) { f.emit(&events.QR{Codes: []string{"2@qr"}}) }
}

// linking starts an unlinked adapter and asks for a code.
func linking(t *testing.T) (*env, *fakeClient, channel.LinkState) {
	t.Helper()
	e := newEnv(t, "", pairable)
	e.run()
	e.waitStatus(statusUnlinked)
	st, err := e.a.Link(t.Context(), "+34 600-000 000")
	if err != nil {
		t.Fatalf("link: %v", err)
	}
	return e, e.client(0), st
}

func TestLinkSuccess(t *testing.T) {
	e, f, st := linking(t)
	want := channel.LinkState{Status: channel.LinkPending, Code: fakeCode, Expires: e.clock.Now().Add(160 * time.Second), Detail: detailLinking}
	if st != want {
		t.Fatalf("link state = %+v, want %+v", st, want)
	}
	if got := e.a.LinkState(); got != want {
		t.Fatalf("LinkState = %+v, want %+v", got, want)
	}
	f.mu.Lock()
	args := f.pairArgs
	f.mu.Unlock()
	if wantArgs := []any{number, true, whatsmeow.PairClientChrome, "Chrome (Linux)"}; !reflect.DeepEqual(args, wantArgs) {
		t.Fatalf("PairPhone args = %v, want %v", args, wantArgs)
	}
	e.waitStatus(statusLinking)

	f.pair(t, number)
	f.emit(&events.PairSuccess{ID: types.NewJID(number, types.DefaultUserServer)})
	f.emit(&events.Connected{})
	e.waitStatus(statusConnected)
	if got := e.a.LinkState(); got.Status != channel.LinkDone || got.Code != "" {
		t.Fatalf("LinkState after pairing = %+v, want done without the code", got)
	}
	// The code's timer firing late changes nothing.
	e.clock.fire(t, codeTTL)
	time.Sleep(20 * time.Millisecond)
	if got := e.status.last(); got != statusConnected {
		t.Fatalf("status after a late expiry = %q", got)
	}
	if n := f.count(&f.deletes); n != 0 {
		t.Fatalf("a linked device was deleted %d times", n)
	}
}

// failed asserts a failed link: the reason, the half-made device deleted, a
// fresh client open and the instance back in unlinked.
func failed(t *testing.T, e *env, f *fakeClient, reason string) {
	t.Helper()
	e.waitStatus(statusUnlinked)
	if got := e.a.LinkState(); got.Status != channel.LinkFailed || got.Detail != reason || got.Code != "" {
		t.Fatalf("LinkState = %+v, want failed with %q", got, reason)
	}
	// The device never paired, so it was never saved: nothing to delete,
	// and no spurious warning from whatsmeow refusing to.
	if n := f.count(&f.deletes); n != 0 || strings.Contains(e.logs.String(), "half-made") {
		t.Fatalf("an unsaved device was deleted %d times:\n%s", n, e.logs.String())
	}
	if !f.isClosed() || e.opened() != 2 {
		t.Fatalf("the failed client was not replaced (closed %v, opened %d)", f.isClosed(), e.opened())
	}
}

func TestLinkCodeExpires(t *testing.T) {
	e, f, _ := linking(t)
	e.clock.fire(t, codeTTL)
	failed(t, e, f, "the code expired; run the command again")
	// A fresh link works on the new client.
	if _, err := e.a.Link(t.Context(), "+"+number); err != nil {
		t.Fatalf("relink: %v", err)
	}
}

func TestLinkPairError(t *testing.T) {
	e, f, _ := linking(t)
	f.emit(&events.PairError{Error: errors.New("signature mismatch")})
	failed(t, e, f, "pairing failed: signature mismatch")
}

func TestLinkSocketClosedByWhatsApp(t *testing.T) {
	e, f, _ := linking(t)
	f.emit(&events.Disconnected{})
	failed(t, e, f, "WhatsApp closed the linking connection; run the command again")
}

func TestLinkNumberNotOnWhatsApp(t *testing.T) {
	e := newEnv(t, "", func(f *fakeClient) {
		pairable(f)
		f.pairErr = whatsmeow.ErrIQNotFound
	})
	e.run()
	e.waitStatus(statusUnlinked)
	_, err := e.a.Link(t.Context(), "+"+number)
	if err == nil || err.Error() != "whatsapp: WhatsApp refused +"+number+": is the number on WhatsApp?" ||
		!errors.Is(err, channel.ErrBadPhone) {
		t.Fatalf("link: %v, want the refusal as ErrBadPhone", err)
	}
	failed(t, e, e.client(0), "WhatsApp refused +"+number+": is the number on WhatsApp?")
}

func TestLinkConnectFails(t *testing.T) {
	e := newEnv(t, "", func(f *fakeClient) { f.connectErr = []error{errors.New("dial: refused")} })
	e.run()
	e.waitStatus(statusUnlinked)
	if _, err := e.a.Link(t.Context(), "+"+number); err == nil || errors.Is(err, channel.ErrBadPhone) || errors.Is(err, channel.ErrLinkConflict) {
		t.Fatalf("link: %v, want an unclassified failure", err)
	}
	failed(t, e, e.client(0), "connect: dial: refused")
}

func TestLinkNoQR(t *testing.T) {
	e := newEnv(t, "", nil)
	e.run()
	e.waitStatus(statusUnlinked)
	errc := make(chan error, 1)
	go func() {
		_, err := e.a.Link(t.Context(), "+"+number)
		errc <- err
	}()
	e.clock.fire(t, qrWait)
	if err := <-errc; err == nil {
		t.Fatal("link succeeded without a QR event")
	}
	failed(t, e, e.client(0), "WhatsApp did not open the linking connection in time")
}

func TestLinkRefusals(t *testing.T) {
	if _, err := New(Options{Name: "wa", SessionPath: "x"}).Link(t.Context(), "+"+number); err == nil ||
		!strings.Contains(err.Error(), "not running") || !errors.Is(err, channel.ErrLinkConflict) {
		t.Fatalf("link before Run: %v", err)
	}

	e, _ := connected(t)
	_, err := e.a.Link(t.Context(), "+"+number)
	if want := "already linked as +34600000000; run `holaclaw logout whatsapp --name wa` first"; err == nil || !strings.Contains(err.Error(), want) ||
		!errors.Is(err, channel.ErrLinkConflict) {
		t.Fatalf("relink: %v, want %q", err, want)
	}

	e2, f2, _ := linking(t)
	if _, err := e2.a.Link(t.Context(), "+"+number); err == nil || !errors.Is(err, channel.ErrLinkConflict) {
		t.Fatalf("a second link started while one was pending: %v", err)
	}
	if n := f2.count(&f2.connects); n != 1 {
		t.Fatalf("connects = %d, want 1", n)
	}

	e3 := newEnv(t, "", pairable)
	e3.run()
	e3.waitStatus(statusUnlinked)
	if _, err := e3.a.Link(t.Context(), "0600 000 000"); err == nil || !strings.Contains(err.Error(), "international format, with the country code") ||
		!errors.Is(err, channel.ErrBadPhone) {
		t.Fatalf("national number: %v", err)
	}
	if n := e3.client(0).count(&e3.client(0).connects); n != 0 {
		t.Fatal("a refused number reached WhatsApp")
	}
}

func TestNormalizePhone(t *testing.T) {
	for in, want := range map[string]string{
		"+34 600 000 000": "34600000000",
		"34-600-000-000":  "34600000000",
		" +14155550123 ":  "14155550123",
		"447700900123":    "447700900123",
	} {
		if got, err := normalizePhone(in); err != nil || got != want {
			t.Errorf("normalizePhone(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for in, msg := range map[string]string{
		"":                  "a phone number is needed",
		"+":                 "a phone number is needed",
		"0034600000000":     "use the international format, with the country code",
		"600 000 000a":      "digits only",
		"+34 (600) 000 000": "digits only",
		"34+600000000":      "digits only",
		"12345":             "not a phone number",
		"1234567890123456":  "not a phone number",
	} {
		if _, err := normalizePhone(in); err == nil || !strings.Contains(err.Error(), msg) || !errors.Is(err, channel.ErrBadPhone) {
			t.Errorf("normalizePhone(%q) error = %v, want %q and ErrBadPhone", in, err, msg)
		}
	}
}

func TestUnlink(t *testing.T) {
	e, f := connected(t)
	if err := e.a.Unlink(t.Context()); err != nil {
		t.Fatalf("unlink: %v", err)
	}
	e.waitStatus(statusUnlinked)
	if n := f.count(&f.logouts); n != 1 {
		t.Fatalf("logouts = %d, want 1", n)
	}
	if raw, _ := os.ReadFile(e.path); len(raw) != 0 {
		t.Fatalf("session file after unlink = %q", raw)
	}
	if pn, _ := e.client(1).OwnID(); !pn.IsEmpty() {
		t.Fatal("the new client still holds a device")
	}
	if err := e.a.Unlink(t.Context()); err == nil || !strings.Contains(err.Error(), "not linked") || !errors.Is(err, channel.ErrLinkConflict) {
		t.Fatalf("second unlink: %v", err)
	}
}

func TestUnlinkWhenWhatsAppCannotBeTold(t *testing.T) {
	e, f := connected(t)
	f.mu.Lock()
	f.logoutErr = errors.New("not connected")
	f.mu.Unlock()
	err := e.a.Unlink(t.Context())
	if err == nil || !strings.Contains(err.Error(), "remove the device in Linked devices on the phone") {
		t.Fatalf("unlink: %v", err)
	}
	e.waitStatus(statusUnlinked)
	if raw, _ := os.ReadFile(e.path); len(raw) != 0 {
		t.Fatalf("session kept after a failed logout: %q", raw)
	}
}

func TestUnlinkCancelsAPendingLink(t *testing.T) {
	e, f, _ := linking(t)
	if err := e.a.Unlink(t.Context()); err != nil {
		t.Fatalf("unlink: %v", err)
	}
	e.waitStatus(statusUnlinked)
	if got := e.a.LinkState(); got.Status != channel.LinkNone || got.Code != "" {
		t.Fatalf("LinkState = %+v, want none", got)
	}
	// The old socket finishing the pairing late moves nothing.
	f.emit(&events.PairSuccess{ID: types.NewJID(number, types.DefaultUserServer)})
	f.emit(&events.Connected{})
	time.Sleep(20 * time.Millisecond)
	if got := e.status.last(); got != statusUnlinked {
		t.Fatalf("status = %q after a stale pairing", got)
	}
}

// TestPairingCodeNeverLogged runs a link that succeeds and one that expires
// with every log level captured: the code reaches the caller and LinkState
// only, never a log line or a status detail (which become events).
func TestPairingCodeNeverLogged(t *testing.T) {
	e, f, st := linking(t)
	if st.Code != fakeCode {
		t.Fatalf("code = %q", st.Code)
	}
	f.pair(t, number)
	f.emit(&events.PairSuccess{ID: types.NewJID(number, types.DefaultUserServer)})
	f.emit(&events.Connected{})
	e.waitStatus(statusConnected)

	e2, f2, _ := linking(t)
	f2.emit(&events.PairError{Error: errors.New("boom")})
	e2.waitStatus(statusUnlinked)

	for _, out := range []string{e.logs.String(), e.status.joined(), e2.logs.String(), e2.status.joined()} {
		if strings.Contains(out, fakeCode) || strings.Contains(out, strings.ReplaceAll(fakeCode, "-", "")) {
			t.Fatalf("pairing code leaked:\n%s", out)
		}
	}
}

func TestLogBridgeKeepsWarnAndAbove(t *testing.T) {
	var buf bytes.Buffer
	l := newLogBridge(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})), "whatsmeow").Sub("Client")
	l.Debugf("<message>%s</message>", "decrypted secret")
	l.Infof("info %d", 1)
	l.Warnf("warned %d", 2)
	l.Errorf("failed %d", 3)
	out := buf.String()
	if strings.Contains(out, "decrypted secret") || strings.Contains(out, "info 1") {
		t.Fatalf("debug or info reached slog:\n%s", out)
	}
	for _, want := range []string{"level=WARN msg=\"warned 2\" module=whatsmeow/Client", "level=ERROR msg=\"failed 3\""} {
		if !strings.Contains(out, want) {
			t.Fatalf("log = %s, want %s", out, want)
		}
	}
}
