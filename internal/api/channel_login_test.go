package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"holaclaw/internal/api/gen"
	"holaclaw/internal/auth"
	"holaclaw/internal/channel"
)

const testPairingCode = "ABCD-EFGH"

// wrapped pairs msg, shown verbatim, with sentinel for errors.Is, the way
// the real whatsapp adapter classifies its own refusals.
type wrapped struct {
	msg      string
	sentinel error
}

func (e wrapped) Error() string { return e.msg }
func (e wrapped) Unwrap() error { return e.sentinel }

// fakeLinker plays the WhatsApp adapter: a link waits for the phone until
// done is called, and the refusals are the adapter's own.
type fakeLinker struct {
	mu        sync.Mutex
	state     channel.LinkState
	linked    string          // the number once linked
	phone     string          // the number the last Link asked for
	ctx       context.Context // the context the last Link got
	unlinkErr error
}

func (f *fakeLinker) Link(ctx context.Context, phone string) (channel.LinkState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.phone, f.ctx = phone, ctx
	switch {
	case strings.HasPrefix(phone, "0"):
		return channel.LinkState{}, wrapped{"whatsapp: use the international format, with the country code", channel.ErrBadPhone}
	case f.state.Status == channel.LinkPending:
		return channel.LinkState{}, wrapped{"whatsapp: a link is already waiting for the phone", channel.ErrLinkConflict}
	case f.linked != "":
		return channel.LinkState{}, wrapped{"whatsapp: already linked as " + f.linked + "; run `holaclaw logout whatsapp --name wa` first", channel.ErrLinkConflict}
	case phone == "+999":
		f.state = channel.LinkState{Status: channel.LinkFailed, Detail: "WhatsApp refused +999: is the number on WhatsApp?"}
		return f.state, wrapped{"whatsapp: " + f.state.Detail, channel.ErrBadPhone}
	case phone == "+500":
		f.state = channel.LinkState{Status: channel.LinkFailed, Detail: "connect: offline"}
		return f.state, errors.New("whatsapp: " + f.state.Detail)
	}
	f.state = channel.LinkState{Status: channel.LinkPending, Code: testPairingCode, Expires: time.Unix(1_800_000_000, 0).UTC(), Detail: "waiting for the phone"}
	return f.state, nil
}

func (f *fakeLinker) LinkState() channel.LinkState {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state
}

func (f *fakeLinker) Unlink(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.unlinkErr != nil {
		return f.unlinkErr
	}
	if f.linked == "" && f.state.Status != channel.LinkPending {
		return wrapped{"whatsapp: not linked", channel.ErrLinkConflict}
	}
	f.linked, f.state = "", channel.LinkState{Status: channel.LinkNone}
	return nil
}

// done finishes a pending link as the phone would.
func (f *fakeLinker) done(number string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.linked, f.state = number, channel.LinkState{Status: channel.LinkDone, Detail: "linked as " + number}
}

// linkHarness has a WhatsApp instance "wa" served by a fakeLinker and a
// Telegram instance "tg" that cannot link.
func linkHarness(t *testing.T) (*harness, *fakeLinker, *Client) {
	t.Helper()
	h := newHarness(t)
	configChannel(h, "wa", "whatsapp", "approved")
	configChannel(h, "tg", "telegram", "approved")
	f := &fakeLinker{}
	h.b.ChannelLinker = func(name string) channel.Linker {
		if name == "wa" {
			return f
		}
		return nil
	}
	c, err := Dial(h.srv.URL, h.adminToken)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	return h, f, c
}

// clientErr asserts err is a ClientError with status and code, and returns
// its message.
func clientErr(t *testing.T, err error, status int, code string) string {
	t.Helper()
	var ce *ClientError
	if !errors.As(err, &ce) || ce.Status != status || ce.Code != code {
		t.Fatalf("err = %v, want %d %s", err, status, code)
	}
	return ce.Message
}

func TestChannelLoginHappyPath(t *testing.T) {
	h, f, c := linkHarness(t)
	ctx := context.Background()

	st, err := c.ChannelLogin(ctx, "wa")
	if err != nil || st.State != gen.DeviceLoginStateNone || st.UserCode != nil {
		t.Fatalf("before: %+v, %v; want none", st, err)
	}

	st, err = c.StartChannelLogin(ctx, "wa", "+34 600 000 000")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if f.phone != "+34 600 000 000" {
		t.Errorf("the linker got phone %q", f.phone)
	}
	if st.State != gen.DeviceLoginStatePending || st.UserCode == nil || *st.UserCode != testPairingCode ||
		st.ExpiresAt == nil || !st.ExpiresAt.Equal(time.Unix(1_800_000_000, 0)) || st.VerificationUrl != nil {
		t.Fatalf("start = %+v, want pending with the code and expiry and no URL", st)
	}

	st, err = c.ChannelLogin(ctx, "wa")
	if err != nil || st.State != gen.DeviceLoginStatePending || st.UserCode == nil || *st.UserCode != testPairingCode {
		t.Fatalf("poll = %+v, %v; want pending with the code", st, err)
	}

	f.done("+34600000000")
	st, err = c.ChannelLogin(ctx, "wa")
	if err != nil || st.State != gen.DeviceLoginStateDone || st.UserCode != nil || st.Detail == nil || *st.Detail != "linked as +34600000000" {
		t.Fatalf("done = %+v, %v; want done, no code, the detail", st, err)
	}

	if err := c.ChannelLogout(ctx, "wa"); err != nil {
		t.Fatalf("logout: %v", err)
	}
	if st, _ := c.ChannelLogin(ctx, "wa"); st.State != gen.DeviceLoginStateNone {
		t.Fatalf("after logout = %+v, want none", st)
	}
	// The code went back to the admin and nowhere else.
	if strings.Contains(h.logs.String(), testPairingCode) {
		t.Fatalf("the pairing code reached the log:\n%s", h.logs.String())
	}
}

func TestChannelLoginNotImplemented(t *testing.T) {
	h, _, c := linkHarness(t)
	ctx := context.Background()

	// A channel whose adapter has no Linker.
	_, err := c.StartChannelLogin(ctx, "tg", "+34600000000")
	if msg := clientErr(t, err, http.StatusNotImplemented, CodeNotImplemented); !strings.Contains(msg, "(telegram)") {
		t.Errorf("message = %q, want the channel type", msg)
	}
	_, err = c.ChannelLogin(ctx, "tg")
	clientErr(t, err, http.StatusNotImplemented, CodeNotImplemented)
	clientErr(t, c.ChannelLogout(ctx, "tg"), http.StatusNotImplemented, CodeNotImplemented)

	// A build with no channel login at all.
	h.b.ChannelLinker = nil
	_, err = c.StartChannelLogin(ctx, "wa", "+34600000000")
	clientErr(t, err, http.StatusNotImplemented, CodeNotImplemented)
	_, err = c.ChannelLogin(ctx, "wa")
	clientErr(t, err, http.StatusNotImplemented, CodeNotImplemented)
	clientErr(t, c.ChannelLogout(ctx, "wa"), http.StatusNotImplemented, CodeNotImplemented)
}

func TestChannelLoginUnknownChannel(t *testing.T) {
	_, _, c := linkHarness(t)
	ctx := context.Background()
	_, err := c.StartChannelLogin(ctx, "nope", "+34600000000")
	clientErr(t, err, http.StatusNotFound, CodeNotFound)
	_, err = c.ChannelLogin(ctx, "nope")
	clientErr(t, err, http.StatusNotFound, CodeNotFound)
	clientErr(t, c.ChannelLogout(ctx, "nope"), http.StatusNotFound, CodeNotFound)
}

// The linker's refusals reach the operator verbatim: a state the instance is
// in is a conflict, the number a bad request. A failure that is not about
// the instance's state (WhatsApp not told of a logout) goes through the
// ordinary logging-safe path instead.
func TestChannelLoginRefusals(t *testing.T) {
	h, f, c := linkHarness(t)
	ctx := context.Background()

	_, err := c.StartChannelLogin(ctx, "wa", "0600000000")
	if msg := clientErr(t, err, http.StatusBadRequest, CodeBadRequest); !strings.Contains(msg, "international format") {
		t.Errorf("bad phone message = %q", msg)
	}

	_, err = c.StartChannelLogin(ctx, "wa", "+999")
	if msg := clientErr(t, err, http.StatusBadRequest, CodeBadRequest); !strings.Contains(msg, "is the number on WhatsApp") {
		t.Errorf("failed link message = %q", msg)
	}
	if st, _ := c.ChannelLogin(ctx, "wa"); st.State != gen.DeviceLoginStateFailed || st.Detail == nil {
		t.Errorf("after a failed link = %+v, want failed with a detail", st)
	}

	// Any other failure is WhatsApp's side, still in the adapter's words.
	_, err = c.StartChannelLogin(ctx, "wa", "+500")
	if msg := clientErr(t, err, http.StatusBadGateway, CodeInternal); msg != "whatsapp: connect: offline" {
		t.Errorf("upstream failure message = %q", msg)
	}

	if _, err := c.StartChannelLogin(ctx, "wa", "+34600000000"); err != nil {
		t.Fatalf("start: %v", err)
	}
	_, err = c.StartChannelLogin(ctx, "wa", "+34600000000")
	if msg := clientErr(t, err, http.StatusConflict, CodeConflict); !strings.Contains(msg, "already waiting") {
		t.Errorf("pending message = %q", msg)
	}

	f.done("+34600000000")
	channelsAre(h, gen.ChannelStatus{Name: "wa", Type: "whatsapp", Agent: "main", State: gen.Connected, Detail: ptr("+34600000000")})
	_, err = c.StartChannelLogin(ctx, "wa", "+34600000000")
	if msg := clientErr(t, err, http.StatusConflict, CodeConflict); !strings.Contains(msg, "already linked as +34600000000; run `holaclaw logout whatsapp --name wa` first") {
		t.Errorf("linked message = %q", msg)
	}

	f.unlinkErr = errors.New("whatsapp: the session is deleted, but WhatsApp was not told (offline); remove the device in Linked devices on the phone")
	// The session is gone but WhatsApp was not told: the operator gets the
	// adapter's instruction verbatim.
	if msg := clientErr(t, c.ChannelLogout(ctx, "wa"), http.StatusBadGateway, CodeInternal); !strings.Contains(msg, "remove the device in Linked devices") {
		t.Errorf("unlink message = %q", msg)
	}
	f.unlinkErr = nil
	if err := c.ChannelLogout(ctx, "wa"); err != nil {
		t.Fatalf("logout: %v", err)
	}
	if msg := clientErr(t, c.ChannelLogout(ctx, "wa"), http.StatusConflict, CodeConflict); !strings.Contains(msg, "not linked") {
		t.Errorf("second logout message = %q", msg)
	}
}

func TestChannelLoginBody(t *testing.T) {
	h, _, _ := linkHarness(t)
	errorBody(t, h.do(http.MethodPost, "/channels/wa/login", h.adminToken, nil), http.StatusBadRequest, CodeBadRequest)
	errorBody(t, h.do(http.MethodPost, "/channels/wa/login", h.adminToken, map[string]string{"number": "+34600000000"}),
		http.StatusBadRequest, CodeBadRequest)
}

// A client that hangs up does not cancel the attempt: the adapter would fail
// it and register a fresh device on the next try.
func TestChannelLoginOutlivesTheRequest(t *testing.T) {
	h, f, _ := linkHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/channels/wa/login",
		strings.NewReader(`{"phone":"+34600000000"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	(&handlers{b: h.b}).LoginChannel(rec, req, "wa")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body)
	}
	if f.ctx == nil || f.ctx.Err() != nil {
		t.Fatalf("the linker's context = %v, want one the request's cancel does not reach", f.ctx)
	}
}

// Linking puts the bot on an account: admin scope only.
func TestChannelLoginNeedsAdmin(t *testing.T) {
	h, f, _ := linkHarness(t)
	tokens := map[string]string{
		"client": h.ownerToken,
	}
	for name, tok := range tokens {
		for _, m := range []string{http.MethodPost, http.MethodGet, http.MethodDelete} {
			t.Run(name+" "+m, func(t *testing.T) {
				errorBody(t, h.do(m, "/channels/wa/login", tok, gen.ChannelLoginStart{Phone: "+34600000000"}),
					http.StatusForbidden, CodeForbidden)
			})
		}
	}
	if f.phone != "" {
		t.Fatalf("a refused request reached the linker with %q", f.phone)
	}
	if scopes["loginChannel"] != auth.ScopeAdmin || scopes["getChannelLogin"] != auth.ScopeAdmin || scopes["logoutChannel"] != auth.ScopeAdmin {
		t.Fatal("the channel login operations are not admin scope")
	}
}
