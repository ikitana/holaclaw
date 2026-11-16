package slack

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

func TestRetryAfterIsHonoured(t *testing.T) {
	f := newFakeSlack(t)
	var n atomic.Int32
	f.set("chat.postMessage", func(w http.ResponseWriter, _ *http.Request) {
		if n.Add(1) == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true,"ts":"1.1"}`)
	})
	a := mappingAdapter(t, f, nil)

	ts, err := a.postMessage(context.Background(), postMessageReq{Channel: "C1", Text: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if ts != "1.1" || n.Load() != 2 {
		t.Fatalf("ts = %q after %d attempts", ts, n.Load())
	}
}

func TestRetryAfterGivesUpTyped(t *testing.T) {
	f := newFakeSlack(t)
	var n atomic.Int32
	f.set("chat.postMessage", func(w http.ResponseWriter, _ *http.Request) {
		n.Add(1)
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	a := mappingAdapter(t, f, nil)

	_, err := a.postMessage(context.Background(), postMessageReq{Channel: "C1", Text: "hi"})
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("err = %v, want ErrRateLimited", err)
	}
	if n.Load() != maxRetries+1 {
		t.Fatalf("attempts = %d, want %d", n.Load(), maxRetries+1)
	}
	assertNoTokens(t, err.Error())
}

func TestRetryAfterRespectsContext(t *testing.T) {
	f := newFakeSlack(t)
	f.set("chat.postMessage", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	a := mappingAdapter(t, f, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := a.postMessage(ctx, postMessageReq{Channel: "C1", Text: "hi"})
	if err == nil {
		t.Fatal("want an error")
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("waited %s: the sleep ignored the context", d)
	}
}

func TestParseRetryAfter(t *testing.T) {
	cases := map[string]time.Duration{"7": 7 * time.Second, " 3 ": 3 * time.Second, "": time.Second, "nonsense": time.Second}
	for in, want := range cases {
		if got := parseRetryAfter(in); got != want {
			t.Errorf("parseRetryAfter(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestHTTPErrorIsTypedAndRedacted(t *testing.T) {
	f := newFakeSlack(t)
	f.set("chat.postMessage", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		// a server that echoes the token back must not make us log it
		_, _ = io.WriteString(w, "boom "+fakeBotToken)
	})
	a := mappingAdapter(t, f, nil)

	_, err := a.postMessage(context.Background(), postMessageReq{Channel: "C1", Text: "hi"})
	var se *Error
	if !errors.As(err, &se) || se.HTTP != http.StatusInternalServerError {
		t.Fatalf("err = %v", err)
	}
	assertNoTokens(t, err.Error())
}

func TestTransportErrorIsRedacted(t *testing.T) {
	f := newFakeSlack(t)
	a, _ := newAdapter(t, f, func(o *Options) {
		// an unreachable base URL that carries the token in its path
		o.APIBaseURL = "http://127.0.0.1:1/" + fakeBotToken
	})
	err := a.callForm(context.Background(), "auth.test", fakeBotToken, url.Values{}, nil)
	if err == nil {
		t.Fatal("want an error")
	}
	assertNoTokens(t, err.Error())
}

func TestDisplayNameCachedAndDegrades(t *testing.T) {
	f := newFakeSlack(t)
	a := mappingAdapter(t, f, nil)

	for range 3 {
		if got := a.displayName(context.Background(), "U2"); got != "Alice" {
			t.Fatalf("display name = %q", got)
		}
	}
	if n := len(f.callsFor("users.info")); n != 1 {
		t.Fatalf("users.info called %d times, want 1 (cached)", n)
	}

	f.set("users.info", fail("user_not_found"))
	if got := a.displayName(context.Background(), "U9"); got != "U9" {
		t.Fatalf("failed lookup = %q, want the user id", got)
	}
}

func TestSentinelMapping(t *testing.T) {
	cases := map[string]error{
		"invalid_auth":      ErrInvalidAuth,
		"account_inactive":  ErrInvalidAuth,
		"channel_not_found": ErrChannelNotFound,
		"not_in_channel":    ErrNotInChannel,
		"ratelimited":       ErrRateLimited,
		"something_else":    nil,
	}
	for code, want := range cases {
		if got := sentinelFor(code); !errors.Is(got, want) {
			t.Errorf("sentinelFor(%q) = %v, want %v", code, got, want)
		}
	}
}

func TestTTLSetDeduplicates(t *testing.T) {
	s := newTTLSet(time.Hour)
	if s.add("k") {
		t.Fatal("first add reported a duplicate")
	}
	if !s.add("k") {
		t.Fatal("second add not reported as a duplicate")
	}
	expired := newTTLSet(time.Nanosecond)
	expired.add("k")
	time.Sleep(time.Millisecond)
	if expired.add("k") {
		t.Fatal("expired key still counted as a duplicate")
	}
}

func TestTSTime(t *testing.T) {
	got := tsTime("1727090000.000200")
	if got.Unix() != 1727090000 || got.Nanosecond() != 200*1000 { // ts fractions are microseconds
		t.Fatalf("tsTime = %v (%d)", got, got.Nanosecond())
	}
	if tsTime("nonsense").IsZero() {
		t.Fatal("an unparsable ts should fall back to now")
	}
}

func TestIdentityPersisted(t *testing.T) {
	f := newFakeSlack(t)
	st := newMemState()
	a, _ := newAdapter(t, f, func(o *Options) { o.State = st })
	_, stop := run(t, a, nil)
	c := f.conn(t)
	c.push(t, map[string]any{"type": "hello"})
	f.waitCalls(t, "auth.test", 1)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if v, ok, _ := st.Get(context.Background(), stateBotUser); ok && v == "UBOT" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if v, _, _ := st.Get(context.Background(), stateBotUser); v != "UBOT" {
		t.Fatalf("stored bot user = %q", v)
	}
	if v, _, _ := st.Get(context.Background(), stateTeamID); v != "T1" {
		t.Fatalf("stored team = %q", v)
	}
	_ = stop()
}

func TestOptionDefaults(t *testing.T) {
	a := New(Options{Name: "x"})
	if a.opt.APIBaseURL != defaultAPIBase || a.opt.MaxDownload != defaultMaxDownload {
		t.Fatalf("defaults = %q / %d", a.opt.APIBaseURL, a.opt.MaxDownload)
	}
	if a.hc == nil || a.log == nil {
		t.Fatal("client and logger must have defaults")
	}
	if a.botToken() != "" || a.appToken() != "" {
		t.Fatal("missing token funcs must read as empty")
	}
	if strconv.Itoa(a.Limits().TextChars) != "4000" {
		t.Fatal("text limit")
	}
}
