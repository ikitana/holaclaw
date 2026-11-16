package daemon

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"holaclaw/internal/api"
	"holaclaw/internal/app"
	"holaclaw/internal/approval"
	"holaclaw/internal/browser"
	"holaclaw/internal/browser/client"
	"holaclaw/internal/browser/proto"
	"holaclaw/internal/channel"
	"holaclaw/internal/config"
	"holaclaw/internal/event"
	"holaclaw/internal/store"
	"holaclaw/internal/tool"
)

// The browser wiring the daemon adds: a risky step is an ordinary approval, a
// blocker is a takeover request, and a chat channel gets it as one fixed
// sentence plus a screenshot. Nothing here needs a real Chromium — the
// sidecar is a fake socket, which is the only thing this file is about.

// --- harness -------------------------------------------------------------------

// browserDaemon is a Daemon with only the parts this file uses: a store, the
// approval service and whatever browser wiring the test asks for.
func browserDaemon(t *testing.T) *Daemon {
	t.Helper()
	st, err := store.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	a := &app.App{Store: st, Events: st}
	return &Daemon{
		App:       a,
		Approvals: approval.New(st.DB(), st, approval.WithTimeout(time.Minute)),
		log:       slog.New(slog.DiscardHandler),
	}
}

// fakeSidecar serves h on a unix socket and returns a client for it.
func fakeSidecar(t *testing.T, h http.Handler) *client.Client {
	t.Helper()
	path := shortSocket(t)
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := httptest.NewUnstartedServer(h)
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
	return client.New(path, client.Options{})
}

// fakeChannel records what was sent to a conversation.
type fakeChannel struct {
	mu      sync.Mutex
	sent    []event.Message
	targets []event.Origin
}

func (*fakeChannel) Name() string           { return "fake" }
func (*fakeChannel) Type() string           { return "fake" }
func (*fakeChannel) Limits() channel.Limits { return channel.Limits{} }
func (*fakeChannel) Run(ctx context.Context, _ channel.Inbound, _ func(channel.State, string)) error {
	<-ctx.Done()
	return nil
}

func (f *fakeChannel) Send(_ context.Context, out channel.Outbound) (channel.Receipt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, out.Message)
	f.targets = append(f.targets, out.Target)
	return channel.Receipt{PlatformMsgIDs: []string{"1"}}, nil
}

func (f *fakeChannel) messages() []event.Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]event.Message(nil), f.sent...)
}

// seedConversation creates an owner and a session on the fake channel.
func seedConversation(t *testing.T, d *Daemon, role store.Role) store.Session {
	t.Helper()
	ctx := context.Background()
	i := store.Identity{ID: event.NewID(), Name: "owner", Role: role, CreatedAt: time.Now().UTC()}
	if err := d.App.Store.CreateIdentity(ctx, i); err != nil {
		t.Fatalf("CreateIdentity: %v", err)
	}
	origin := event.Origin{Channel: "fake", ChannelType: "fake", Conversation: "c1", ConversationKind: event.ConversationDM}
	sess, _, err := d.App.Store.EnsureSession(ctx, store.Session{
		ID: event.NewID(), Key: "main/fake/c1", Agent: "main", Kind: store.SessionChat,
		UserID: i.ID, Origin: origin,
	})
	if err != nil {
		t.Fatalf("EnsureSession: %v", err)
	}
	return sess
}

// --- approvals -------------------------------------------------------------------

func TestHumansApprove(t *testing.T) {
	answer := func(t *testing.T, approve bool) bool {
		t.Helper()
		d := browserDaemon(t)
		ctx := context.Background()
		type res struct {
			ok  bool
			err error
		}
		out := make(chan res, 1)
		go func() {
			ok, err := humans{d}.Approve(ctx, browser.Task{Agent: "main", TraceID: "tr1", SessionID: "se1"},
				browser.RiskPurchase, "browser: a step classified purchase: click \"Buy\"")
			out <- res{ok, err}
		}()
		var pending tool.Approval
		deadline := time.Now().Add(wait)
		for time.Now().Before(deadline) {
			list, err := d.Approvals.ListPending(ctx)
			if err != nil {
				t.Fatalf("ListPending: %v", err)
			}
			if len(list) == 1 {
				pending = list[0]
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		if pending.ID == "" {
			t.Fatal("no approval was raised")
		}
		if pending.Tool != "browser.do" || pending.Agent != "main" || pending.TraceID != "tr1" {
			t.Fatalf("approval = %+v", pending)
		}
		if !strings.Contains(pending.Summary, "purchase") {
			t.Fatalf("summary = %q", pending.Summary)
		}
		if _, err := d.Approvals.Resolve(ctx, pending.ID, approve, tool.GrantOnce, "u1"); err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		r := <-out
		if r.err != nil {
			t.Fatalf("Approve: %v", r.err)
		}
		return r.ok
	}

	t.Run("approved", func(t *testing.T) {
		if !answer(t, true) {
			t.Fatal("an approved step came back false")
		}
	})
	t.Run("denied", func(t *testing.T) {
		if answer(t, false) {
			t.Fatal("a denied step came back true")
		}
	})
}

func TestHumansApproveTimeout(t *testing.T) {
	d := browserDaemon(t)
	d.Approvals = approval.New(d.App.Store.DB(), d.App.Store, approval.WithTimeout(10*time.Millisecond))
	ok, err := humans{d}.Approve(context.Background(), browser.Task{Agent: "main"}, browser.RiskSend, "browser: a step classified send")
	if err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if ok {
		t.Fatal("a timed-out approval is a deny")
	}
}

// TestHumansTakeoverWithoutBrowser pins the answer when there is no live
// view: nobody can take over, so the run does not wait for one.
func TestHumansTakeoverWithoutBrowser(t *testing.T) {
	d := browserDaemon(t)
	ok, err := humans{d}.Takeover(context.Background(), browser.Task{Agent: "main"}, browser.BlockLogin)
	if ok || err != nil {
		t.Fatalf("Takeover with no browser = %v, %v", ok, err)
	}
}

// --- the chat-channel notice -----------------------------------------------------

func TestTakeoverText(t *testing.T) {
	for _, tc := range []struct {
		why  string
		want string
	}{
		{string(browser.BlockLogin), "log in"},
		{string(browser.BlockCaptcha), "captcha"},
		{string(browser.Block2FA), "two-factor"},
		{"something new", "on its own"},
	} {
		got := takeoverText("main", tc.why)
		if !strings.Contains(got, "main") || !strings.Contains(got, tc.want) {
			t.Fatalf("takeoverText(%q) = %q", tc.why, got)
		}
	}
}

func TestTakeoverNotice(t *testing.T) {
	newCS := func(t *testing.T, d *Daemon, ch *fakeChannel) *channels {
		t.Helper()
		return &channels{d: d, byName: map[string]channel.Channel{"fake": ch}}
	}
	notice := func(sess store.Session, why string) event.Event {
		return event.New(event.BrowserTakeoverRequest, "tr1", sess.ID, "",
			map[string]any{"agent": "main", "why": why})
	}

	t.Run("with a screenshot", func(t *testing.T) {
		d := browserDaemon(t)
		mux := http.NewServeMux()
		mux.HandleFunc("POST /sessions/{agent}/screenshot", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write([]byte("\xff\xd8\xff not really a jpeg"))
		})
		d.browser = &browserWiring{client: fakeSidecar(t, mux)}
		sess := seedConversation(t, d, store.RoleOwner)
		ch := &fakeChannel{}
		newCS(t, d, ch).takeoverNotice(context.Background(), notice(sess, string(browser.BlockLogin)))

		msgs := ch.messages()
		if len(msgs) != 1 {
			t.Fatalf("%d messages sent", len(msgs))
		}
		if !strings.Contains(msgs[0].Text, "log in") {
			t.Fatalf("text = %q", msgs[0].Text)
		}
		if len(msgs[0].Attachments) != 1 || msgs[0].Attachments[0].MIME != "image/jpeg" {
			t.Fatalf("attachments = %+v", msgs[0].Attachments)
		}
	})

	t.Run("screenshot unavailable", func(t *testing.T) {
		d := browserDaemon(t)
		mux := http.NewServeMux()
		mux.HandleFunc("POST /sessions/{agent}/screenshot", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"code":"internal","message":"no"}`))
		})
		d.browser = &browserWiring{client: fakeSidecar(t, mux)}
		sess := seedConversation(t, d, store.RoleOwner)
		ch := &fakeChannel{}
		newCS(t, d, ch).takeoverNotice(context.Background(), notice(sess, string(browser.BlockCaptcha)))

		msgs := ch.messages()
		if len(msgs) != 1 || len(msgs[0].Attachments) != 0 {
			t.Fatalf("messages = %+v", msgs)
		}
	})

	t.Run("not an admin", func(t *testing.T) {
		d := browserDaemon(t)
		sess := seedConversation(t, d, store.RoleUser)
		ch := &fakeChannel{}
		newCS(t, d, ch).takeoverNotice(context.Background(), notice(sess, string(browser.BlockLogin)))
		if msgs := ch.messages(); len(msgs) != 0 {
			t.Fatalf("a non-admin was told to take over: %+v", msgs)
		}
	})

	t.Run("no such session", func(t *testing.T) {
		d := browserDaemon(t)
		ch := &fakeChannel{}
		e := event.New(event.BrowserTakeoverRequest, "tr1", event.NewID(), "",
			map[string]any{"agent": "main", "why": "login"})
		newCS(t, d, ch).takeoverNotice(context.Background(), e)
		if msgs := ch.messages(); len(msgs) != 0 {
			t.Fatalf("messages = %+v", msgs)
		}
	})

	t.Run("malformed attrs", func(t *testing.T) {
		d := browserDaemon(t)
		sess := seedConversation(t, d, store.RoleOwner)
		ch := &fakeChannel{}
		e := event.New(event.BrowserTakeoverRequest, "tr1", sess.ID, "", map[string]any{"why": "login"})
		newCS(t, d, ch).takeoverNotice(context.Background(), e)
		if msgs := ch.messages(); len(msgs) != 0 {
			t.Fatalf("messages = %+v", msgs)
		}
	})
}

// TestTakeoverNoticeNeverCarriesPageText is the rule of docs/browser.md as a test: the
// notice is built from the agent name and a closed-set label, so whatever the
// event carries beyond them cannot reach a person's screen.
func TestTakeoverNoticeNeverCarriesPageText(t *testing.T) {
	d := browserDaemon(t)
	d.browser = &browserWiring{client: fakeSidecar(t, http.NotFoundHandler())}
	sess := seedConversation(t, d, store.RoleOwner)
	ch := &fakeChannel{}
	cs := &channels{d: d, byName: map[string]channel.Channel{"fake": ch}}
	const marker = "SECRET-PAGE-TEXT"
	attrs, err := json.Marshal(map[string]any{"agent": "main", "why": marker, "title": marker})
	if err != nil {
		t.Fatal(err)
	}
	e := event.New(event.BrowserTakeoverRequest, "tr1", sess.ID, "", nil)
	e.Attrs = attrs
	cs.takeoverNotice(context.Background(), e)

	msgs := ch.messages()
	if len(msgs) != 1 {
		t.Fatalf("%d messages", len(msgs))
	}
	if strings.Contains(msgs[0].Text, marker) {
		t.Fatalf("the notice quoted the event: %q", msgs[0].Text)
	}
}

// --- setup -----------------------------------------------------------------------

// TestSetupBrowser covers the two shapes of start: no [browser] table at all,
// and a table whose sidecar is not up yet — which is a warning, never a
// failure to start.
func TestSetupBrowser(t *testing.T) {
	t.Run("no browser table", func(t *testing.T) {
		d := browserDaemon(t)
		d.App.Config = &config.Config{}
		d.Backend = &api.Backend{}
		d.setupBrowser(context.Background())
		if d.browser != nil || d.Backend.Browser != nil {
			t.Fatal("a browser was wired with no [browser] table")
		}
	})
	t.Run("sidecar answers", func(t *testing.T) {
		d := browserDaemon(t)
		path := shortSocket(t)
		ln, err := net.Listen("unix", path)
		if err != nil {
			t.Fatal(err)
		}
		srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(proto.Health{Version: "dev", Binary: "/usr/bin/chromium", Browser: "145.0.0.1"})
		}))
		srv.Listener = ln
		srv.Start()
		defer srv.Close()

		d.App.Config = &config.Config{Browser: config.Browser{Enabled: true, Socket: path}}
		d.Backend = &api.Backend{}
		d.setupBrowser(context.Background())
		if d.browser == nil || d.browser.live == nil || d.Backend.Browser == nil {
			t.Fatal("the live view was not wired")
		}
	})
	t.Run("sidecar down", func(t *testing.T) {
		d := browserDaemon(t)
		d.App.Config = &config.Config{Browser: config.Browser{Enabled: true, Socket: shortSocket(t)}}
		d.Backend = &api.Backend{}
		d.setupBrowser(context.Background()) // a warning in the log, nothing more
		if d.browser == nil {
			t.Fatal("an unreachable sidecar stopped the wiring")
		}
	})
}

// A takeover for anything but a credential blocker is answered at once when
// nobody watches the live view: waiting on an empty viewer is a silent stall.
func TestHumansTakeoverErrorWithNoWatcherFailsFast(t *testing.T) {
	d := browserDaemon(t)
	start := time.Now()
	ok, err := humans{d}.Takeover(context.Background(), browser.Task{Agent: "main"}, browser.BlockError)
	if err != nil || ok {
		t.Fatalf("ok=%v err=%v, want false, nil", ok, err)
	}
	if time.Since(start) > time.Second {
		t.Fatalf("took %s; must not wait for a takeover", time.Since(start))
	}
}
