package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"holaclaw/internal/api/gen"
)

// loginReply is one answer of the fake daemon: a body, or an error.
type loginReply struct {
	status int
	login  gen.DeviceLogin
	err    gen.Error
}

func ok(l gen.DeviceLogin) loginReply { return loginReply{status: http.StatusOK, login: l} }

func refused(status int, code, msg string) loginReply {
	return loginReply{status: status, err: gen.Error{Code: code, Message: msg}}
}

// loginFake is a daemon with channels, providers that have a login, and a
// scripted channel login: polls answers in order, the last one repeated.
type loginFake struct {
	channels  []string          // whatsapp channels
	others    map[string]string // other channels: name → type
	providers []string
	polls     []loginReply
	start     loginReply
	logout    loginReply
	codex     loginReply // the provider's POST, when set; none otherwise
	stop      func()     // called on a POST, as Ctrl-C would be

	mu    sync.Mutex
	calls []string // "METHOD path", past the status probe
	phone string   // the body of the POST
}

func (f *loginFake) serve(t *testing.T) string {
	t.Helper()
	write := func(w http.ResponseWriter, r loginReply) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(r.status)
		if r.status >= 300 {
			_ = json.NewEncoder(w).Encode(r.err)
		} else if r.status != http.StatusNoContent {
			_ = json.NewEncoder(w).Encode(r.login)
		}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/status" {
			_ = json.NewEncoder(w).Encode(gen.Status{})
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls = append(f.calls, r.Method+" "+r.URL.Path)
		if r.Method == http.MethodPost && f.stop != nil {
			f.stop()
		}
		switch {
		case r.URL.Path == "/v1/channels":
			var out struct {
				Channels []gen.ChannelStatus `json:"channels"`
			}
			for _, n := range f.channels {
				out.Channels = append(out.Channels, gen.ChannelStatus{Name: n, Type: "whatsapp", State: gen.Unlinked})
			}
			for n, typ := range f.others {
				out.Channels = append(out.Channels, gen.ChannelStatus{Name: n, Type: typ, State: gen.Unlinked})
			}
			_ = json.NewEncoder(w).Encode(out)
		case strings.HasPrefix(r.URL.Path, "/v1/credentials/"):
			name := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/credentials/"), "/login")
			for _, p := range f.providers {
				if p == name && r.Method == http.MethodPost && f.codex.status != 0 {
					write(w, f.codex)
					return
				}
				if p == name {
					write(w, ok(gen.DeviceLogin{State: gen.DeviceLoginStateNone}))
					return
				}
			}
			write(w, refused(http.StatusNotFound, "not_found", "not found"))
		case r.Method == http.MethodGet:
			reply := f.polls[0]
			if len(f.polls) > 1 {
				f.polls = f.polls[1:]
			}
			write(w, reply)
		case r.Method == http.MethodPost:
			var in gen.ChannelLoginStart
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &in)
			f.phone = in.Phone
			write(w, f.start)
		case r.Method == http.MethodDelete:
			write(w, f.logout)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func (f *loginFake) posted() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if strings.HasPrefix(c, "POST /v1/channels/") {
			return true
		}
	}
	return false
}

// touched reports whether a call went to path or below it.
func (f *loginFake) touched(prefix string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if _, p, _ := strings.Cut(c, " "); strings.HasPrefix(p, prefix) {
			return true
		}
	}
	return false
}

// fastPoll makes the login loops poll at once.
func fastPoll(t *testing.T) {
	old := loginPoll
	loginPoll = time.Millisecond
	t.Cleanup(func() { loginPoll = old })
}

func ptrTo[T any](v T) *T { return &v }

var (
	linkExpiry = time.Date(2026, 10, 19, 10, 2, 40, 0, time.UTC)
	pending    = ok(gen.DeviceLogin{State: gen.DeviceLoginStatePending, UserCode: ptrTo("ABCD-EFGH"), ExpiresAt: &linkExpiry})
	linked     = ok(gen.DeviceLogin{State: gen.DeviceLoginStateDone, Detail: ptrTo("linked as +34600000000")})
	noLink     = ok(gen.DeviceLogin{State: gen.DeviceLoginStateNone})
)

func TestLoginChannelLinks(t *testing.T) {
	fastPoll(t)
	f := &loginFake{channels: []string{"whatsapp"}, polls: []loginReply{noLink, pending, pending, linked}, start: pending}
	addr := f.serve(t)
	code, out, errOut := runCLI(t, "", "login", "whatsapp", "--phone", "+34 600 000 000", "--addr", addr)
	if code != 0 {
		t.Fatalf("exit = %d\n%s", code, errOut)
	}
	want := "On the phone with +34 600 000 000, open WhatsApp → Settings → Linked devices → Link a device\n" +
		"→ Link with phone number instead, and enter:\n\n    ABCD-EFGH\n\n" +
		"The code expires at " + linkExpiry.Local().Format("15:04:05") + ". WhatsApp also shows a notification on the phone that opens this screen.\n" +
		"whatsapp: linked as +34600000000\n"
	if out != want {
		t.Errorf("stdout =\n%s\nwant\n%s", out, want)
	}
	if errOut != "waiting for the phone… (Ctrl-C stops waiting; the daemon keeps the code alive until it expires)\n" {
		t.Errorf("stderr = %q", errOut)
	}
	if f.phone != "+34 600 000 000" {
		t.Errorf("posted phone = %q, want it as typed", f.phone)
	}
}

func TestLoginChannelResumesPending(t *testing.T) {
	fastPoll(t)
	// No --phone: the pending code is resumed, and the instructions name no
	// number, as the code may be for another one.
	f := &loginFake{channels: []string{"whatsapp"}, polls: []loginReply{pending, pending, linked}}
	code, out, _ := runCLI(t, "", "login", "whatsapp", "--addr", f.serve(t))
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if f.posted() {
		t.Error("a pending link was started again")
	}
	if !strings.HasPrefix(out, "On the phone, open WhatsApp → Settings → Linked devices → Link a device\n") ||
		!strings.Contains(out, "    ABCD-EFGH\n") || !strings.HasSuffix(out, "whatsapp: linked as +34600000000\n") {
		t.Errorf("stdout = %q", out)
	}
}

// Ctrl-C while waiting stops quietly with the shell's status for SIGINT,
// for a channel and for a provider.
func TestLoginInterrupted(t *testing.T) {
	fastPoll(t)
	for _, tc := range []struct {
		name string
		f    *loginFake
		args []string
	}{
		{"channel", &loginFake{channels: []string{"whatsapp"}, polls: []loginReply{noLink, pending}, start: pending},
			[]string{"login", "whatsapp", "--phone", "34600000000"}},
		{"provider", &loginFake{providers: []string{"codex"}}, []string{"login", "codex"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			tc.f.stop = cancel
			var out, errOut strings.Builder
			code := run(ctx, append(tc.args, "--addr", tc.f.serve(t)), strings.NewReader(""), &out, &errOut)
			if code != 130 || strings.Contains(errOut.String(), "holaclaw:") || strings.Contains(errOut.String(), "context canceled") {
				t.Errorf("exit = %d, stderr = %q; want 130 and quiet", code, errOut.String())
			}
		})
	}
}

// What the daemon sends cannot drive the terminal: escape sequences and
// control characters are dropped from the code, the detail and the messages.
func TestLoginDropsControlCharacters(t *testing.T) {
	fastPoll(t)
	esc := ok(gen.DeviceLogin{State: gen.DeviceLoginStatePending, UserCode: ptrTo("ABCD\x1b]0;pwned\x07-EFGH")})
	done := ok(gen.DeviceLogin{State: gen.DeviceLoginStateDone, Detail: ptrTo("linked as \x1b[2J+34600000000\u009b")})
	f := &loginFake{channels: []string{"whatsapp"}, polls: []loginReply{esc, done}}
	code, out, _ := runCLI(t, "", "login", "whatsapp", "--addr", f.serve(t))
	if code != 0 || strings.ContainsAny(out, "\x1b\x07\u009b") ||
		!strings.Contains(out, "    ABCD-EFGH\n") || !strings.HasSuffix(out, "whatsapp: linked as +34600000000\n") {
		t.Errorf("exit = %d, stdout = %q", code, out)
	}

	f = &loginFake{channels: []string{"whatsapp"}, polls: []loginReply{noLink},
		start: refused(http.StatusConflict, "conflict", "whatsapp: a link\x1b[31m is already waiting for the phone")}
	code, _, errOut := runCLI(t, "", "login", "whatsapp", "--phone", "34600000000", "--addr", f.serve(t))
	if code != 1 || errOut != "holaclaw: whatsapp: a link is already waiting for the phone\n" {
		t.Errorf("exit = %d, stderr = %q", code, errOut)
	}

	// The names the daemon lists, too.
	f = &loginFake{channels: []string{"a\x1b[2J", "b"}}
	_, _, errOut = runCLI(t, "", "login", "whatsapp", "--addr", f.serve(t))
	if strings.Contains(errOut, "\x1b") || !strings.Contains(errOut, "(whatsapp channels: a, b)") {
		t.Errorf("missing name: stderr = %q", errOut)
	}

	f = &loginFake{providers: []string{"codex"}, codex: ok(gen.DeviceLogin{State: gen.DeviceLoginStatePending,
		VerificationUrl: ptrTo("https://auth.example/\x1b[8mdevice"), UserCode: ptrTo("WXYZ\r-1234")})}
	code, out, errOut = runCLI(t, "", "login", "codex", "--addr", f.serve(t))
	if code != 1 || out != "Open https://auth.example/device and enter the code: WXYZ-1234\n" {
		t.Errorf("codex: exit = %d, stdout = %q, stderr = %q", code, out, errOut)
	}
}

func TestLoginChannelLinkedWithoutDetail(t *testing.T) {
	fastPoll(t)
	f := &loginFake{channels: []string{"wa"}, polls: []loginReply{noLink, ok(gen.DeviceLogin{State: gen.DeviceLoginStateDone})}, start: pending}
	code, out, _ := runCLI(t, "", "login", "whatsapp", "--name", "wa", "--phone", "34600000000", "--addr", f.serve(t))
	if code != 0 || !strings.HasSuffix(out, "\nwa: linked\n") {
		t.Errorf("exit = %d, stdout = %q", code, out)
	}
}

func TestLoginChannelExpires(t *testing.T) {
	fastPoll(t)
	expired := ok(gen.DeviceLogin{State: gen.DeviceLoginStateFailed, Detail: ptrTo("the code expired; run the command again")})
	f := &loginFake{channels: []string{"whatsapp"}, polls: []loginReply{noLink, pending, expired}, start: pending}
	code, _, errOut := runCLI(t, "", "login", "whatsapp", "--phone", "34600000000", "--addr", f.serve(t))
	if code != 1 || !strings.Contains(errOut, "holaclaw: whatsapp: the code expired; run the command again\n") {
		t.Errorf("exit = %d, stderr = %q", code, errOut)
	}

	// No detail: a generic line, still an error.
	f = &loginFake{channels: []string{"whatsapp"}, polls: []loginReply{noLink, noLink}, start: pending}
	code, _, errOut = runCLI(t, "", "login", "whatsapp", "--phone", "34600000000", "--addr", f.serve(t))
	if code != 1 || !strings.Contains(errOut, "holaclaw: whatsapp: the link failed; run the command again\n") {
		t.Errorf("exit = %d, stderr = %q", code, errOut)
	}
}

// The daemon's refusals are printed as they come: the adapter's messages
// (internal/channel/whatsapp/login.go) already start with "whatsapp: ".
func TestLoginChannelRefused(t *testing.T) {
	msg := "whatsapp: already linked as +34600000000; run `holaclaw logout whatsapp` first"
	f := &loginFake{channels: []string{"whatsapp"}, polls: []loginReply{noLink}, start: refused(http.StatusConflict, "conflict", msg)}
	code, out, errOut := runCLI(t, "", "login", "whatsapp", "--phone", "34600000000", "--addr", f.serve(t))
	if code != 1 || errOut != "holaclaw: whatsapp: already linked as +34600000000; run `holaclaw logout whatsapp` first\n" || out != "" {
		t.Errorf("exit = %d, stdout = %q, stderr = %q", code, out, errOut)
	}

	f = &loginFake{channels: []string{"whatsapp"}, polls: []loginReply{noLink},
		start: refused(http.StatusBadRequest, "bad_request", "whatsapp: use the international format, with the country code")}
	code, _, errOut = runCLI(t, "", "login", "whatsapp", "--phone", "0600000000", "--addr", f.serve(t))
	if code != 1 || errOut != "holaclaw: whatsapp: use the international format, with the country code\n" {
		t.Errorf("400: exit = %d, stderr = %q", code, errOut)
	}

	f = &loginFake{channels: []string{"whatsapp"}, polls: []loginReply{noLink},
		start: refused(http.StatusBadRequest, "bad_request", "whatsapp: WhatsApp refused +34600000000: is the number on WhatsApp?")}
	code, _, errOut = runCLI(t, "", "login", "whatsapp", "--phone", "34600000000", "--addr", f.serve(t))
	if code != 1 || errOut != "holaclaw: whatsapp: WhatsApp refused +34600000000: is the number on WhatsApp?\n" {
		t.Errorf("unknown number: exit = %d, stderr = %q", code, errOut)
	}

	f = &loginFake{channels: []string{"whatsapp"}, polls: []loginReply{noLink},
		start: refused(http.StatusBadGateway, "internal", "whatsapp: WhatsApp did not open the linking connection in time")}
	code, _, errOut = runCLI(t, "", "login", "whatsapp", "--phone", "34600000000", "--addr", f.serve(t))
	if code != 1 || errOut != "holaclaw: whatsapp: WhatsApp did not open the linking connection in time\n" {
		t.Errorf("502: exit = %d, stderr = %q", code, errOut)
	}

	// Removed between the list and the login: the daemon's 404.
	f = &loginFake{channels: []string{"wa"}, polls: []loginReply{refused(http.StatusNotFound, "not_found", "not found")}}
	code, _, errOut = runCLI(t, "", "login", "whatsapp", "--name", "wa", "--phone", "1", "--addr", f.serve(t))
	if code != 1 || errOut != "holaclaw: no channel named \"wa\" on this daemon: add it to channels.toml and restart `holaclaw run`\n" {
		t.Errorf("404: exit = %d, stderr = %q", code, errOut)
	}
}

func TestLoginChannelNeedsPhone(t *testing.T) {
	for _, tc := range []struct{ channel, says string }{
		{"whatsapp", "whatsapp: --phone is required: holaclaw login whatsapp --phone <number>"},
		{"wa", "wa: --phone is required: holaclaw login whatsapp --name wa --phone <number>"},
	} {
		f := &loginFake{channels: []string{tc.channel}, polls: []loginReply{noLink}}
		args := []string{"login", "whatsapp", "--addr", f.serve(t)}
		if tc.channel != "whatsapp" {
			args = append(args, "--name", tc.channel)
		}
		code, _, errOut := runCLI(t, "", args...)
		if code != 2 || !strings.Contains(errOut, tc.says) {
			t.Errorf("%s: exit = %d, stderr = %q", tc.channel, code, errOut)
		}
		if f.posted() {
			t.Errorf("%s: started a link with no phone", tc.channel)
		}
	}
}

// `login whatsapp` and `logout whatsapp` act on the channel named with
// --name, which defaults to "whatsapp"; anything else says what is there.
func TestLoginWhatsAppPicksTheChannel(t *testing.T) {
	fastPoll(t)
	tg := map[string]string{"tg": "telegram"}

	// The default name, with another whatsapp channel also present.
	f := &loginFake{channels: []string{"whatsapp", "wa2"}, others: tg, polls: []loginReply{noLink, linked}, start: pending}
	code, out, _ := runCLI(t, "", "login", "whatsapp", "--phone", "34600000000", "--addr", f.serve(t))
	if code != 0 || !strings.HasSuffix(out, "whatsapp: linked as +34600000000\n") || !f.touched("/v1/channels/whatsapp/login") || f.touched("/v1/channels/wa2/") {
		t.Errorf("default name: exit = %d, stdout = %q, calls = %v", code, out, f.calls)
	}

	// --name picks another instance.
	f = &loginFake{channels: []string{"wa", "wa2"}, polls: []loginReply{noLink, linked}, start: pending}
	code, out, _ = runCLI(t, "", "login", "whatsapp", "--name", "wa2", "--phone", "34600000000", "--addr", f.serve(t))
	if code != 0 || !strings.HasSuffix(out, "wa2: linked as +34600000000\n") || f.touched("/v1/channels/wa/") {
		t.Errorf("named: exit = %d, stdout = %q, calls = %v", code, out, f.calls)
	}

	for _, tc := range []struct {
		name string
		f    *loginFake
		args []string
		want string
	}{
		{"missing name with others listed", &loginFake{channels: []string{"wa", "wa2"}}, []string{"login", "whatsapp"},
			"holaclaw: no whatsapp channel named \"whatsapp\" (whatsapp channels: wa, wa2); pass --name <channel>\n" +
				"add the instance to channels.toml and restart `holaclaw run`:\n\n" +
				"    [channels.whatsapp]\n    type  = \"whatsapp\"\n    agent = \"main\"\n"},
		{"none at all", &loginFake{others: tg}, []string{"login", "whatsapp"},
			"holaclaw: no whatsapp channel named \"whatsapp\" (no whatsapp channels); pass --name <channel>\n" +
				"add the instance to channels.toml and restart `holaclaw run`:\n\n" +
				"    [channels.whatsapp]\n    type  = \"whatsapp\"\n    agent = \"main\"\n"},
		{"named, missing", &loginFake{channels: []string{"wa"}}, []string{"login", "whatsapp", "--name", "nope"},
			"holaclaw: no whatsapp channel named \"nope\" (whatsapp channels: wa); pass --name <channel>\n" +
				"add the instance to channels.toml and restart `holaclaw run`:\n\n" +
				"    [channels.nope]\n    type  = \"whatsapp\"\n    agent = \"main\"\n"},
		{"wrong type", &loginFake{channels: []string{"wa"}, others: tg}, []string{"login", "whatsapp", "--name", "tg"},
			"holaclaw: \"tg\" is a telegram channel, not a whatsapp one; pass --name <channel>\n"},
		{"logout missing name with others listed", &loginFake{channels: []string{"wa", "wa2"}}, []string{"logout", "whatsapp"},
			"holaclaw: no whatsapp channel named \"whatsapp\" (whatsapp channels: wa, wa2); pass --name <channel>\n"},
		{"logout none at all", &loginFake{}, []string{"logout", "whatsapp"},
			"holaclaw: no whatsapp channel named \"whatsapp\" (no whatsapp channels); pass --name <channel>\n"},
		{"logout wrong type", &loginFake{others: tg}, []string{"logout", "whatsapp", "--name", "tg"},
			"holaclaw: \"tg\" is a telegram channel, not a whatsapp one; pass --name <channel>\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, out, errOut := runCLI(t, "", append(tc.args, "--addr", tc.f.serve(t))...)
			if code != 1 || out != "" || errOut != tc.want {
				t.Errorf("exit = %d, stdout = %q, stderr =\n%s\nwant\n%s", code, out, errOut, tc.want)
			}
			if tc.f.touched("/v1/channels/") {
				t.Errorf("acted on a channel: %v", tc.f.calls)
			}
		})
	}
}

// A positional argument is rejected with the usage, not acted on: the
// instance is always named with --name.
func TestLoginRejectsPositionalArgs(t *testing.T) {
	for _, args := range [][]string{
		{"login", "whatsapp", "extra"},
		{"login", "codex", "extra"},
		{"logout", "whatsapp", "extra"},
	} {
		code, _, errOut := runCLI(t, "", args...)
		if code != 2 || !strings.Contains(errOut, `holaclaw: unexpected argument "extra"; name the instance with --name extra`) {
			t.Errorf("%v: exit = %d, stderr = %q", args, code, errOut)
		}
	}
}

func TestLoginCodex(t *testing.T) {
	fastPoll(t)
	// No name: the provider named codex, on the device-code endpoints and
	// never the channels'. The fake's login answers none, so it fails there.
	f := &loginFake{channels: []string{"codex"}, providers: []string{"codex"}}
	code, _, errOut := runCLI(t, "", "login", "codex", "--addr", f.serve(t))
	if code != 1 || !strings.Contains(errOut, "codex: login failed") || f.touched("/v1/channels") {
		t.Errorf("default: exit = %d, stderr = %q, calls = %v", code, errOut, f.calls)
	}

	f = &loginFake{providers: []string{"work"}}
	code, _, errOut = runCLI(t, "", "login", "codex", "--name", "work", "--addr", f.serve(t))
	if code != 1 || !strings.Contains(errOut, "holaclaw: work: login failed\n") || !f.touched("/v1/credentials/work/login") {
		t.Errorf("named: exit = %d, stderr = %q, calls = %v", code, errOut, f.calls)
	}

	f = &loginFake{providers: []string{"work"}}
	code, _, errOut = runCLI(t, "", "login", "codex", "--addr", f.serve(t))
	want := "holaclaw: no codex provider named codex on this daemon; pass --name <provider>\n" +
		"add it to config.toml and restart `holaclaw run`:\n\n" +
		"    [providers.codex]\n    type = \"codex\"\n    auth = \"subscription\"\n"
	if code != 1 || errOut != want {
		t.Errorf("404: exit = %d, stderr = %q", code, errOut)
	}

	// --phone belongs to whatsapp only.
	if code, _, errOut := runCLI(t, "", "login", "codex", "--phone", "1"); code != 2 || !strings.Contains(errOut, "unknown flag: --phone") {
		t.Errorf("--phone: exit = %d, stderr = %q", code, errOut)
	}
}

func TestLogout(t *testing.T) {
	f := &loginFake{channels: []string{"whatsapp"}, logout: loginReply{status: http.StatusNoContent}}
	code, out, _ := runCLI(t, "", "logout", "whatsapp", "--addr", f.serve(t))
	if code != 0 || out != "whatsapp: unlinked\n" {
		t.Errorf("exit = %d, stdout = %q", code, out)
	}
	if len(f.calls) != 2 || f.calls[1] != "DELETE /v1/channels/whatsapp/login" {
		t.Errorf("calls = %v", f.calls)
	}

	f = &loginFake{channels: []string{"wa", "wa2"}, logout: loginReply{status: http.StatusNoContent}}
	code, out, _ = runCLI(t, "", "logout", "whatsapp", "--name", "wa2", "--addr", f.serve(t))
	if code != 0 || out != "wa2: unlinked\n" || !f.touched("/v1/channels/wa2/login") {
		t.Errorf("named: exit = %d, stdout = %q, calls = %v", code, out, f.calls)
	}

	f = &loginFake{channels: []string{"wa"}, logout: refused(http.StatusNotImplemented, "not_implemented", `channel "wa" (whatsapp) has no interactive login`)}
	code, _, errOut := runCLI(t, "", "logout", "whatsapp", "--name", "wa", "--addr", f.serve(t))
	if code != 1 || errOut != "holaclaw: wa has no interactive login to log out of\n" {
		t.Errorf("501: exit = %d, stderr = %q", code, errOut)
	}

	f = &loginFake{channels: []string{"whatsapp"}, logout: refused(http.StatusConflict, "conflict", "whatsapp: not linked")}
	code, _, errOut = runCLI(t, "", "logout", "whatsapp", "--addr", f.serve(t))
	if code != 1 || errOut != "holaclaw: whatsapp: not linked\n" {
		t.Errorf("409: exit = %d, stderr = %q", code, errOut)
	}

	msg := "whatsapp: the session is deleted, but WhatsApp was not told (offline); remove the device in Linked devices on the phone"
	f = &loginFake{channels: []string{"whatsapp"}, logout: refused(http.StatusBadGateway, "internal", msg)}
	code, _, errOut = runCLI(t, "", "logout", "whatsapp", "--addr", f.serve(t))
	if code != 1 || errOut != "holaclaw: "+msg+"\n" {
		t.Errorf("502: exit = %d, stderr = %q", code, errOut)
	}
}

func TestPrintable(t *testing.T) {
	r := func(c rune) string { return string(c) }
	zwj, zwnj := r(0x200d), r(0x200c)
	family := r(0x1f468) + zwj + r(0x1f469) + zwj + r(0x1f467)
	persian := r(0x645) + r(0x6cc) + zwnj + r(0x62e) + r(0x648) + r(0x627) + r(0x647) + r(0x645)
	for _, tc := range []struct{ name, in, want string }{
		{"plain", "hola, món", "hola, món"},
		{"zwj keeps an emoji sequence", family, family},
		{"zwnj keeps Persian", persian, persian},
		{"other format runes are dropped", "a" + r(0xad) + "b" + r(0x2060) + r(0x2064) + "c" + r(0x206a) + r(0x180e) + r(0xfff9) + "d",
			"abcd"},
		{"controls", "a\x00b\tc\nd\x7fe" + r(0x85) + "f", "abcdef"},
		{"separators", "a" + r(0x2028) + "b" + r(0x2029) + "c", "abc"},
		{"zero-width space and BOM", "a" + r(0x200b) + "b" + r(0xfeff) + "c", "abc"},
		{"tags", "a" + r(0xe0001) + r(0xe0041) + r(0xe007f) + "b", "ab"},
		{"CSI", "a\x1b[31mb\x1b[0m\x1b[2;5Hc", "abc"},
		{"CSI cut by a control", "a\x1b[31\nb", "ab"},
		{"CSI unterminated", "a\x1b[31", "a"},
		{"OSC with BEL", "a\x1b]0;owned\x07b", "ab"},
		{"OSC with ESC \\", "a\x1b]8;;https://evil.example\x1b\\b", "ab"},
		{"OSC with 8-bit ST", "a\x1b]0;owned" + r(0x9c) + "b", "ab"},
		{"OSC unterminated", "a\x1b]0;owned", "a"},
		{"OSC ended by another escape", "a\x1b]0;title\x1b[31mred text", "ared text"},
		{"OSC cancelled by CAN", "a\x1b]0;title\x18b", "ab"},
		{"OSC cancelled by SUB", "a\x1b]0;title\x1ab", "ab"},
		{"DCS", "a\x1bP1$qm\x1b\\b", "ab"},
		{"SOS", "a\x1bXhidden\x1b\\b", "ab"},
		{"PM", "a\x1b^hidden\x1b\\b", "ab"},
		{"APC", "a\x1b_hidden" + r(0x9c) + "b", "ab"},
		{"8-bit DCS", "a" + r(0x90) + "hidden" + r(0x9c) + "b", "ab"},
		{"8-bit SOS", "a" + r(0x98) + "hidden\x1b\\b", "ab"},
		{"8-bit PM", "a" + r(0x9e) + "hidden\x07b", "ab"},
		{"8-bit APC", "a" + r(0x9f) + "hidden\x1b\\b", "ab"},
		{"8-bit CSI", "a" + r(0x9b) + "31mb", "ab"},
		{"8-bit OSC", "a" + r(0x9d) + "0;owned\x07b", "ab"},
		{"a lone ESC", "a\x1bb", "ab"},
		{"invalid UTF-8", "a\xffb", "a" + r(0xfffd) + "b"},
	} {
		if got := printable(tc.in); got != tc.want {
			t.Errorf("%s: printable(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}
	// Every bidi control is dropped.
	for _, c := range []rune{0x061c, 0x200e, 0x200f, 0x202a, 0x202b, 0x202c, 0x202d, 0x202e, 0x2066, 0x2067, 0x2068, 0x2069} {
		if got := printable("a" + r(c) + "b"); got != "ab" {
			t.Errorf("printable kept %U: %q", c, got)
		}
	}
}
