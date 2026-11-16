package telegram

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"holaclaw/internal/channel"
	"holaclaw/internal/event"
)

// TestMain shortens the fixed timings so the loop tests stay fast.
func TestMain(m *testing.M) {
	backoffBase, backoffMax = 2*time.Millisecond, 10*time.Millisecond
	typingInterval, typingCooldown = 10*time.Millisecond, 200*time.Millisecond
	m.Run()
}

// updateFeed emulates Telegram's getUpdates semantics: an update is delivered
// again and again until the offset moves past it.
type updateFeed struct {
	mu      sync.Mutex
	queue   []wireUpdate
	offsets []int64
}

func (u *updateFeed) install(f *fakeAPI) {
	f.onFunc("getUpdates", func(c call) reply {
		u.mu.Lock()
		off := int64(0)
		if v, ok := c.Body["offset"].(float64); ok {
			off = int64(v)
		}
		u.offsets = append(u.offsets, off)
		var keep []wireUpdate
		for _, up := range u.queue {
			if up.UpdateID >= off {
				keep = append(keep, up)
			}
		}
		u.queue = keep
		out := append([]wireUpdate(nil), keep...)
		u.mu.Unlock()
		if len(out) == 0 {
			time.Sleep(2 * time.Millisecond) // stand in for a long poll
		}
		return reply{Result: out}
	})
}

func (u *updateFeed) push(ups ...wireUpdate) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.queue = append(u.queue, ups...)
}

// lastOffset is the offset of the latest getUpdates call, or -1 before any.
func (u *updateFeed) lastOffset() int64 {
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.offsets) == 0 {
		return -1
	}
	return u.offsets[len(u.offsets)-1]
}

func (u *updateFeed) firstOffset() int64 {
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.offsets) == 0 {
		return -1
	}
	return u.offsets[0]
}

// statusLog records the lifecycle callbacks.
type statusLog struct {
	mu      sync.Mutex
	entries []struct {
		state  channel.State
		detail string
	}
}

func (s *statusLog) fn() func(channel.State, string) {
	return func(st channel.State, detail string) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.entries = append(s.entries, struct {
			state  channel.State
			detail string
		}{st, detail})
	}
}

func (s *statusLog) detail(st channel.State) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.entries {
		if e.state == st {
			return e.detail, true
		}
	}
	return "", false
}

// start runs the adapter in the background and returns a stop func.
func start(t *testing.T, a *Adapter, in channel.Inbound) (*statusLog, func() error) {
	t.Helper()
	st := &statusLog{}
	ctx, cancel := context.WithCancel(t.Context())
	errc := make(chan error, 1)
	go func() { errc <- a.Run(ctx, in, st.fn()) }()
	var (
		once   sync.Once
		runErr error
	)
	finish := func() {
		once.Do(func() {
			cancel()
			select {
			case runErr = <-errc:
			case <-time.After(5 * time.Second):
				runErr = errors.New("Run did not return after cancel")
			}
		})
	}
	t.Cleanup(finish)
	return st, func() error { finish(); return runErr }
}

// collector gathers delivered envelopes and can reject the first N.
type collector struct {
	mu     sync.Mutex
	envs   []event.Envelope
	reject int
}

func (c *collector) in(_ context.Context, env event.Envelope) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.reject > 0 {
		c.reject--
		return errors.New("store unavailable")
	}
	c.envs = append(c.envs, env)
	return nil
}

func (c *collector) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.envs)
}

func (c *collector) at(i int) event.Envelope {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.envs[i]
}

func TestGetMeUnauthorized(t *testing.T) {
	f := newFake(t)
	f.on("getMe", &fakeErr{Status: 401, Description: "Unauthorized"})
	a, _, sink := newAdapter(t, f, nil)

	err := a.Run(t.Context(), func(context.Context, event.Envelope) error { return nil }, (&statusLog{}).fn())
	if !errors.Is(err, ErrAuth) {
		t.Fatalf("err = %v, want ErrAuth", err)
	}
	if f.count("getUpdates") != 0 {
		t.Fatal("a revoked token must not start a retry loop")
	}
	if strings.Contains(err.Error(), fakeToken) || strings.Contains(sink.String(), fakeToken) {
		t.Fatal("token leaked")
	}
}

func TestGetMeReportsLoggedOut(t *testing.T) {
	f := newFake(t)
	f.on("getMe", &fakeErr{Status: 401, Description: "Unauthorized"})
	a, _, _ := newAdapter(t, f, nil)
	st := &statusLog{}
	_ = a.Run(t.Context(), func(context.Context, event.Envelope) error { return nil }, st.fn())
	if _, ok := st.detail(channel.StateLoggedOut); !ok {
		t.Fatal("expected a logged_out status")
	}
}

func TestPollAdvancesOffsetOnlyAfterAccept(t *testing.T) {
	f := newFake(t)
	feed := &updateFeed{}
	feed.install(f)
	a, state, _ := newAdapter(t, f, nil)
	c := &collector{reject: 1} // the first delivery fails

	st, stop := start(t, a, c.in)
	// Connected is reported after getMe returns, so wait for the status itself.
	waitFor(t, func() bool { _, ok := st.detail(channel.StateConnected); return ok })
	if d, ok := st.detail(channel.StateConnected); !ok || d != "@holaclaw" {
		t.Fatalf("connected detail = %q ok=%v", d, ok)
	}

	feed.push(msgUpdate(10, dmMessage(3, "hello")))
	waitFor(t, func() bool { return c.len() == 1 })
	// The rejected delivery left the offset alone, so Telegram resent it.
	waitFor(t, func() bool { return state.get(offsetKey) == "11" })
	if got := c.at(0).Message.Text; got != "hello" {
		t.Fatalf("text = %q", got)
	}
	if err := stop(); !errors.Is(err, context.Canceled) {
		t.Fatalf("stop err = %v", err)
	}
}

// TestPollAcceptsDuplicate: a message core already handled advances the
// offset like an accepted one, so Telegram does not resend it.
func TestPollAcceptsDuplicate(t *testing.T) {
	f := newFake(t)
	feed := &updateFeed{}
	feed.install(f)
	a, state, _ := newAdapter(t, f, nil)
	var calls atomic.Int32
	_, stop := start(t, a, func(context.Context, event.Envelope) error {
		calls.Add(1)
		return channel.ErrDuplicate
	})
	feed.push(msgUpdate(10, dmMessage(3, "hello")))
	waitFor(t, func() bool { return state.get(offsetKey) == "11" })
	_ = stop()
	if n := calls.Load(); n != 1 {
		t.Fatalf("Inbound called %d times, want 1", n)
	}
}

func TestRestartResumesFromStoredOffset(t *testing.T) {
	f := newFake(t)
	feed := &updateFeed{}
	feed.install(f)
	a, state, _ := newAdapter(t, f, nil)
	if err := state.Set(t.Context(), offsetKey, "500"); err != nil {
		t.Fatal(err)
	}
	c := &collector{}
	_, stop := start(t, a, c.in)
	waitFor(t, func() bool { return feed.firstOffset() >= 0 })
	if got := feed.firstOffset(); got != 500 {
		t.Fatalf("first poll offset = %d, want 500", got)
	}
	_ = stop()
}

func TestPollHonoursRetryAfter(t *testing.T) {
	f := newFake(t)
	feed := &updateFeed{}
	var once sync.Once
	f.onFunc("getUpdates", func(c call) reply {
		var limited bool
		once.Do(func() { limited = true })
		if limited {
			return reply{Err: &fakeErr{Status: 429, Description: "Too Many Requests", RetryAfter: 1}}
		}
		time.Sleep(2 * time.Millisecond)
		feed.mu.Lock()
		defer feed.mu.Unlock()
		return reply{Result: []wireUpdate{}}
	})
	a, _, _ := newAdapter(t, f, nil)
	st, stop := start(t, a, func(context.Context, event.Envelope) error { return nil })

	begin := time.Now()
	waitFor(t, func() bool { return f.count("getUpdates") >= 2 })
	if d := time.Since(begin); d < 800*time.Millisecond {
		t.Fatalf("retry_after not honoured: second poll after %v", d)
	}
	if d, ok := st.detail(channel.StateReconnecting); !ok || !strings.Contains(d, "rate limited") {
		t.Fatalf("status detail = %q", d)
	}
	_ = stop()
}

func TestPollConflictReportsReconnecting(t *testing.T) {
	f := newFake(t)
	f.on("getUpdates", &fakeErr{Status: 409, Description: "Conflict: terminated by other getUpdates request"})
	a, _, sink := newAdapter(t, f, nil)
	st, stop := start(t, a, func(context.Context, event.Envelope) error { return nil })
	waitFor(t, func() bool {
		d, ok := st.detail(channel.StateReconnecting)
		return ok && strings.Contains(d, "another poller")
	})
	_ = stop()
	if strings.Contains(sink.String(), fakeToken) {
		t.Fatal("token leaked into the log")
	}
}

// TestBackoffHonoursContext keeps the backoff long and cancels mid-sleep.
func TestBackoffHonoursContext(t *testing.T) {
	oldBase, oldMax := backoffBase, backoffMax
	backoffBase, backoffMax = 10*time.Second, 30*time.Second
	t.Cleanup(func() { backoffBase, backoffMax = oldBase, oldMax })

	f := newFake(t)
	f.on("getUpdates", &fakeErr{Status: 500, Description: "Internal Server Error"})
	a, _, _ := newAdapter(t, f, nil)
	_, stop := start(t, a, func(context.Context, event.Envelope) error { return nil })
	waitFor(t, func() bool { return f.count("getUpdates") > 0 })

	done := make(chan error, 1)
	go func() { done <- stop() }()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("backoff did not honour context cancellation")
	}
}

func TestAlbumMergedIntoOneEnvelope(t *testing.T) {
	f := newFake(t)
	f.onFunc("getFile", func(c call) reply {
		id, _ := c.Body["file_id"].(string)
		return reply{Result: wireFile{FileID: id, FilePath: "photos/" + id}}
	})
	f.file("photos/p1", []byte("one"))
	f.file("photos/p2", []byte("two"))
	feed := &updateFeed{}
	feed.install(f)
	a, state, _ := newAdapter(t, f, nil)
	c := &collector{}
	_, stop := start(t, a, c.in)
	waitFor(t, func() bool { return f.count("getMe") > 0 })

	photo := func(id int64, fileID, caption string) *wireMessage {
		m := dmMessage(id, "")
		m.MediaGroupID = "grp-1"
		m.Caption = caption
		m.Photo = []wirePhoto{{FileID: fileID, Width: 800, FileSize: 3}}
		return m
	}
	feed.push(
		msgUpdate(20, photo(30, "p1", "two pictures")),
		msgUpdate(21, photo(31, "p2", "")),
	)
	waitFor(t, func() bool { return c.len() == 1 })
	time.Sleep(50 * time.Millisecond)
	if c.len() != 1 {
		t.Fatalf("album produced %d envelopes, want 1", c.len())
	}
	env := c.at(0)
	if env.Message.Text != "two pictures" {
		t.Fatalf("caption = %q", env.Message.Text)
	}
	if len(env.Message.Attachments) != 2 {
		t.Fatalf("attachments = %d", len(env.Message.Attachments))
	}
	if env.Origin.PlatformMsgID != "1234:30" {
		t.Fatalf("platform msg id = %q", env.Origin.PlatformMsgID)
	}
	waitFor(t, func() bool { return state.get(offsetKey) == "22" })
	_ = stop()
}

func TestStartDeepLinkIsANormalEnvelope(t *testing.T) {
	f := newFake(t)
	feed := &updateFeed{}
	feed.install(f)
	a, _, _ := newAdapter(t, f, nil)
	c := &collector{}
	_, stop := start(t, a, c.in)
	waitFor(t, func() bool { return f.count("getMe") > 0 })

	m := dmMessage(4, "/start inv_abc123")
	m.Entities = []wireEntity{{Type: "bot_command", Offset: 0, Length: 6}}
	feed.push(msgUpdate(1, m))
	waitFor(t, func() bool { return c.len() == 1 })
	if got := c.at(0).Message.Text; got != "/start inv_abc123" {
		t.Fatalf("text = %q", got)
	}
	_ = stop()
}

func TestCallbackFlow(t *testing.T) {
	f := newFake(t)
	f.on("sendMessage", wireMessage{MessageID: 77, Chat: wireChat{ID: 1234, Type: "private"}})
	feed := &updateFeed{}
	feed.install(f)

	type decision struct {
		id      event.ID
		choice  string
		aliases []string
	}
	got := make(chan decision, 1)
	a, _, _ := newAdapter(t, f, func(o *Options) {
		o.OnApproval = func(_ context.Context, id event.ID, choice string, aliases []string) {
			got <- decision{id, choice, aliases}
		}
	})
	_, stop := start(t, a, func(context.Context, event.Envelope) error { return nil })
	waitFor(t, func() bool { return f.count("getMe") > 0 })

	approvalID := event.NewID()
	target := event.Origin{Conversation: "1234", ConversationKind: event.ConversationDM}
	if err := a.PromptApproval(t.Context(), target, approvalID, "Run `rm -rf /`?", []string{"allow", "deny"}); err != nil {
		t.Fatal(err)
	}
	sent := f.seen("sendMessage")
	if len(sent) != 1 {
		t.Fatalf("sendMessage calls = %d", len(sent))
	}
	markup, ok := sent[0].Body["reply_markup"].(map[string]any)
	if !ok {
		t.Fatalf("no inline keyboard: %+v", sent[0].Body)
	}
	rows := markup["inline_keyboard"].([]any)
	buttons := rows[0].([]any)
	if len(buttons) != 2 {
		t.Fatalf("buttons = %d", len(buttons))
	}
	data := buttons[0].(map[string]any)["callback_data"].(string)
	if !strings.HasPrefix(data, "a:") || !strings.HasSuffix(data, ":allow") || len(data) > 64 {
		t.Fatalf("callback_data = %q", data)
	}

	feed.push(wireUpdate{UpdateID: 40, CallbackQuery: &wireCallbackQuery{
		ID:      "cbq-1",
		From:    &wireUser{ID: 7, Username: "angel"},
		Data:    data,
		Message: &wireMessage{MessageID: 77, Chat: wireChat{ID: 1234, Type: "private"}, Text: "Run rm -rf /?"},
	}})

	select {
	case d := <-got:
		if d.id != approvalID || d.choice != "allow" {
			t.Fatalf("decision = %+v", d)
		}
		if strings.Join(d.aliases, ",") != "tg:7" {
			t.Fatalf("aliases = %v", d.aliases)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("OnApproval was not called")
	}
	waitFor(t, func() bool { return f.count("answerCallbackQuery") == 1 && f.count("editMessageText") == 1 })
	edit := f.seen("editMessageText")[0]
	if _, ok := edit.Body["reply_markup"]; ok {
		t.Fatal("the keyboard must be removed")
	}
	if !strings.Contains(edit.str("text"), "<i>allow</i>") {
		t.Fatalf("edited text = %q", edit.str("text"))
	}
	_ = stop()
}

func TestExpiredCallbackIsAnswered(t *testing.T) {
	f := newFake(t)
	a, _, _ := newAdapter(t, f, nil)
	a.handleCallback(t.Context(), &wireCallbackQuery{ID: "x", Data: "a:deadbeef:allow"})
	calls := f.seen("answerCallbackQuery")
	if len(calls) != 1 || !strings.Contains(calls[0].str("text"), "expired") {
		t.Fatalf("answer = %+v", calls)
	}
}

func TestTokenNeverAppearsInErrors(t *testing.T) {
	f := newFake(t)
	f.on("getUpdates", &fakeErr{Status: 400, Description: "Bad Request: chat not found"})
	a, _, sink := newAdapter(t, f, nil)

	// A transport failure: the server is gone, so Go quotes the URL.
	f.srv.Close()
	err := a.call(t.Context(), "getMe", nil, nil)
	if err == nil {
		t.Fatal("expected a transport error")
	}
	if strings.Contains(err.Error(), fakeToken) {
		t.Fatalf("token leaked into a transport error: %s", err)
	}
	if !errors.Is(err, ErrNetwork) {
		t.Fatalf("kind = %v", err)
	}
	if strings.Contains(sink.String(), fakeToken) {
		t.Fatal("token leaked into the log")
	}
}

func TestScrubRemovesBothHalvesOfTheToken(t *testing.T) {
	secret := "424242:AAHsUpErSeCrEtToKeNvAlUe"
	in := "Get \"https://api.telegram.org/bot" + secret + "/getMe\": dial tcp: refused; AAHsUpErSeCrEtToKeNvAlUe"
	out := scrub(in, secret)
	if strings.Contains(out, secret) || strings.Contains(out, "AAHsUpErSeCrEtToKeNvAlUe") {
		t.Fatalf("scrub left the token in: %s", out)
	}
}
