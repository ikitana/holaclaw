package telegram

import (
	"net/http"
	"runtime"
	"strings"
	"testing"
	"time"

	"holaclaw/internal/event"
)

func dmTarget() event.Origin {
	return event.Origin{Conversation: "1234", ConversationKind: event.ConversationDM}
}

func TestTypingRefreshesAndStops(t *testing.T) {
	f := newFake(t)
	f.on("sendChatAction", true)
	a, _, _ := newAdapter(t, f, nil)

	stop := a.Typing(t.Context(), dmTarget())
	waitFor(t, func() bool { return f.count("sendChatAction") >= 3 })
	if got := f.seen("sendChatAction")[0].str("action"); got != "typing" {
		t.Fatalf("action = %q", got)
	}
	stop()
	n := f.count("sendChatAction")
	time.Sleep(5 * typingInterval)
	if f.count("sendChatAction") != n {
		t.Fatal("the indicator kept refreshing after stop")
	}
}

func TestTypingStopIsIdempotentAndLeaksNoGoroutine(t *testing.T) {
	f := newFake(t)
	f.on("sendChatAction", true)
	// A client of its own, so the keep-alive connection a sendChatAction that
	// beat stop() left open can be closed and not be counted as a leak.
	hc := &http.Client{Transport: &http.Transport{}}
	a, _, _ := newAdapter(t, f, func(o *Options) { o.HTTPClient = hc })

	before := runtime.NumGoroutine()
	for range 20 {
		stop := a.Typing(t.Context(), dmTarget())
		stop()
		stop() // idempotent
	}
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > before+2 && time.Now().Before(deadline) {
		hc.CloseIdleConnections()
		time.Sleep(5 * time.Millisecond)
	}
	if after := runtime.NumGoroutine(); after > before+2 {
		t.Fatalf("goroutines leaked: %d -> %d", before, after)
	}
}

func TestTypingFailureStartsCooldown(t *testing.T) {
	f := newFake(t)
	f.on("sendChatAction", &fakeErr{Status: 400, Description: "Bad Request: chat not found"})
	a, _, sink := newAdapter(t, f, nil)

	stop := a.Typing(t.Context(), dmTarget())
	waitFor(t, func() bool { return f.count("sendChatAction") == 1 })
	time.Sleep(5 * typingInterval)
	if n := f.count("sendChatAction"); n != 1 {
		t.Fatalf("a failing indicator must not retry: %d calls", n)
	}
	stop()

	// A new Typing inside the cooldown window does not call the API at all.
	stop2 := a.Typing(t.Context(), dmTarget())
	time.Sleep(3 * typingInterval)
	stop2()
	if n := f.count("sendChatAction"); n != 1 {
		t.Fatalf("cooldown not honoured: %d calls", n)
	}
	if !strings.Contains(sink.String(), "typing indicator disabled") {
		t.Fatal("the cooldown should be logged")
	}
	if strings.Contains(sink.String(), fakeToken) {
		t.Fatal("token leaked")
	}
}

func TestTypingUsesThread(t *testing.T) {
	f := newFake(t)
	f.on("sendChatAction", true)
	a, _, _ := newAdapter(t, f, nil)
	target := dmTarget()
	target.Thread = "42"
	stop := a.Typing(t.Context(), target)
	waitFor(t, func() bool { return f.count("sendChatAction") >= 1 })
	stop()
	if got := f.seen("sendChatAction")[0].str("message_thread_id"); got != "42" {
		t.Fatalf("message_thread_id = %q", got)
	}
}
