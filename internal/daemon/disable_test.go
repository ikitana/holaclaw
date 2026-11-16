package daemon

import (
	"errors"
	"testing"
	"time"

	"holaclaw/internal/api"
	"holaclaw/internal/api/gen"
	"holaclaw/internal/event"
	"holaclaw/internal/testmodel"
)

// user creates a confirmed user with a client token, and a client over TCP
// that acts as them.
func (e *env) user(addr, name string) (gen.User, *api.Client) {
	e.t.Helper()
	u, err := e.client.CreateIdentity(e.ctx, name, "user")
	if err != nil {
		e.t.Fatalf("create identity: %v", err)
	}
	tok, err := e.client.CreateToken(e.ctx, gen.TokenCreate{Name: name, Scope: ptrOf(gen.TokenCreateScopeClient), UserId: ptrOf(u.Id)})
	if err != nil {
		e.t.Fatalf("create token: %v", err)
	}
	c, err := api.Dial("http://"+addr, tok.Secret)
	if err != nil {
		e.t.Fatalf("dial: %v", err)
	}
	return u, c
}

// ended waits for the stream to close.
func (s *stream) ended() {
	s.t.Helper()
	deadline := time.After(wait)
	for {
		select {
		case ev, ok := <-s.ch:
			if !ok {
				return
			}
			s.seen = append(s.seen, ev.Type)
		case <-deadline:
			s.t.Fatalf("the stream is still open (saw %v)", s.seen)
		}
	}
}

// Disabling an identity stops what it already had running: its event stream
// closes and its turn is aborted, while another user's stream and turn go on.
// A reconnect is refused until the identity is enabled again.
func TestDisableCutsOffStreamsAndRunningTurns(t *testing.T) {
	e := start(t, opts{listen: true},
		testmodel.Step{Text: "too late", Delay: 10 * time.Second},
		testmodel.Step{Text: "bob's answer", Delay: 300 * time.Millisecond},
	)
	addr := e.tcpAddr()
	alice, ac := e.user(addr, "alice")
	_, bc := e.user(addr, "bob")
	conversation := "mine"
	as, err := ac.CreateSession(e.ctx, gen.SessionCreate{Conversation: &conversation})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	bs, err := bc.CreateSession(e.ctx, gen.SessionCreate{Conversation: &conversation})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	aliceStream, bobStream := watchWith(t, ac, ""), watchWith(t, bc, "")

	calls := func(n int) {
		t.Helper()
		deadline := time.Now().Add(wait)
		for e.model.Calls() < n {
			if time.Now().After(deadline) {
				t.Fatalf("the model got %d calls, want %d", e.model.Calls(), n)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	accepted := sendWith(t, ac, as.Id, string(event.NewID()), "a long one")
	calls(1)
	// Accepted while her turn runs: it waits for the next turn.
	queued := sendWith(t, ac, as.Id, string(event.NewID()), "and another")
	sendWith(t, bc, bs.Id, string(event.NewID()), "a short one")
	calls(2)

	if _, err := e.client.DisableIdentity(e.ctx, alice.Id); err != nil {
		t.Fatalf("disable: %v", err)
	}
	aliceStream.ended()
	if got := bobStream.await(event.Reply); got.SessionId == nil || *got.SessionId != bs.Id {
		t.Fatalf("bob's stream got a reply for %v", got.SessionId)
	}
	if m, ok := lastAssistant(e.history(bs.Id)); !ok || text(m) != "bob's answer" {
		t.Fatalf("bob's history: %v", roles(e.history(bs.Id)))
	}
	// The running turn is aborted, and the queued one is dropped rather than
	// run once the first is out of the way.
	for _, trace := range []string{accepted.TraceId, queued.TraceId} {
		deadline := time.Now().Add(wait)
		for e.traceTypes(trace)[string(event.RunAborted)] == 0 {
			if time.Now().After(deadline) {
				t.Fatalf("alice's turn was not aborted: %v", e.traceTypes(trace))
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	if m, ok := lastAssistant(e.history(as.Id)); ok {
		t.Fatalf("alice's aborted turn answered %q", text(m))
	}
	if n := e.model.Calls(); n != 2 {
		t.Fatalf("the model got %d calls, want only the first two", n)
	}

	_, err = ac.Events(e.ctx, "", "")
	var ce *api.ClientError
	if !errors.As(err, &ce) || ce.Status != 403 || ce.Code != api.CodeIdentityDisabled {
		t.Fatalf("reconnect while disabled: err = %v", err)
	}
	if _, err := e.client.EnableIdentity(e.ctx, alice.Id); err != nil {
		t.Fatalf("enable: %v", err)
	}
	s, err := ac.Events(e.ctx, "", "")
	if err != nil {
		t.Fatalf("reconnect after the enable: %v", err)
	}
	_ = s.Close()
}
