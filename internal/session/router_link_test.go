package session

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"holaclaw/internal/event"
	"holaclaw/internal/store"
	"holaclaw/internal/store/storetest"
)

// logBuf is a log sink safe for the router's concurrent use.
type logBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func withLog(rh *routerHarness) *logBuf {
	l := &logBuf{}
	rh.r.Logger = slog.New(slog.NewTextHandler(l, nil))
	return l
}

func aliasEnvelope(channel string, aliases ...string) event.Envelope {
	return event.Envelope{
		Origin: event.Origin{
			Channel: channel, ChannelType: "whatsapp", Conversation: "c9", ConversationKind: event.ConversationDM,
		},
		Sender:  event.Sender{Aliases: aliases, DisplayName: "Angel <admin>"},
		Message: event.Message{Text: "secret text"},
	}
}

// The free aliases of a resolved envelope join the identity, with one
// identity.bound each; an envelope with nothing new emits none.
func TestRouteLinksEnvelopeAliases(t *testing.T) {
	t.Parallel()
	rh := newRouterHarness(t)
	ctx := context.Background()

	acc, err := rh.r.Route(ctx, aliasEnvelope("tg-main", "11", "33"))
	if err != nil {
		t.Fatal(err)
	}
	added := rh.ev.of(event.IdentityBound)
	if len(added) != 1 || added[0].TraceID != acc.TraceID {
		t.Fatalf("identity.bound = %+v", added)
	}
	a := attrs(t, added[0])
	if a["identity"] != string(rh.identity.ID) || a["binding"] != "tg-main:33" || a["matched"] != "11" {
		t.Fatalf("attrs = %v", a)
	}
	// The new alias alone now finds the person.
	acc, err = rh.r.Route(ctx, aliasEnvelope("tg-main", "33"))
	if err != nil || acc.User.ID != rh.identity.ID {
		t.Fatalf("route by new alias: %+v, %v", acc, err)
	}
	if _, err := rh.r.Route(ctx, aliasEnvelope("tg-main", "11", "22", "33")); err != nil {
		t.Fatal(err)
	}
	if n := len(rh.ev.of(event.IdentityBound)); n != 1 {
		t.Fatalf("identity.bound = %d after no-op envelopes, want 1", n)
	}
}

// An alias held by another identity is left alone: the message resolves
// through the first match and the conflict is a warning with ids only.
func TestRouteAliasOfAnotherIdentity(t *testing.T) {
	t.Parallel()
	rh := newRouterHarness(t)
	log := withLog(rh)
	ctx := context.Background()
	bo := store.Identity{ID: event.NewID(), Name: "bo", Role: store.RoleUser, CreatedAt: time.Now()}
	if err := rh.st.CreateIdentity(ctx, bo); err != nil {
		t.Fatal(err)
	}
	storetest.Bind(t, rh.st, "tg-main", []string{"44"}, bo.ID, "Bo")

	acc, err := rh.r.Route(ctx, aliasEnvelope("tg-main", "11", "44"))
	if err != nil || acc.User.ID != rh.identity.ID {
		t.Fatalf("route: %+v, %v", acc, err)
	}
	out := log.String()
	if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "alias=44") ||
		!strings.Contains(out, "user="+string(bo.ID)) {
		t.Fatalf("log = %q", out)
	}
	if strings.Contains(out, "Angel") || strings.Contains(out, "secret text") {
		t.Fatalf("log carries a display name or text: %q", out)
	}
	if n := len(rh.ev.of(event.IdentityBound)); n != 0 {
		t.Fatalf("identity.bound = %d, want 0", n)
	}
	if r, _ := store.ResolveAliases(ctx, rh.st.DB(), "tg-main", []string{"44"}, nil); r.Identity.ID != bo.ID {
		t.Fatalf("44 moved to %q", r.Identity.ID)
	}
}

// A recycled number: the identity behind the phone number already holds
// another LID, so the sender is unknown and nothing is linked.
func TestRouteRecycledNumberIsUnknown(t *testing.T) {
	t.Parallel()
	rh := newRouterHarness(t)
	log := withLog(rh)
	rh.r.Resolve = func(ctx context.Context, o event.Origin, s event.Sender) (store.Resolution, error) {
		return rh.st.ResolveAndLink(ctx, o.Channel, s.Aliases, s.DisplayName, []string{"wa:", "wa:lid:"})
	}
	ctx := context.Background()
	storetest.Bind(t, rh.st, "wa", []string{"wa:+1", "wa:lid:9"}, rh.identity.ID, "")

	_, err := rh.r.Route(ctx, aliasEnvelope("wa", "wa:+1", "wa:lid:5"))
	var unknown *ErrUnknownSender
	if !errors.As(err, &unknown) {
		t.Fatalf("err = %v, want ErrUnknownSender", err)
	}
	dropped := rh.ev.of(event.RouterDropped)
	if len(dropped) != 1 || attrs(t, dropped[0])["reason"] != "unknown_sender" {
		t.Fatalf("router.dropped = %+v", dropped)
	}
	out := log.String()
	if !strings.Contains(out, "not trusted") || !strings.Contains(out, "held=wa:lid:9") || !strings.Contains(out, "alias=wa:lid:5") {
		t.Fatalf("log = %q", out)
	}
	if _, err := store.ResolveAliases(ctx, rh.st.DB(), "wa", []string{"wa:lid:5"}, nil); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("recycled LID linked: %v", err)
	}
	if n := len(rh.ev.of(event.IdentityBound)); n != 0 {
		t.Fatalf("identity.bound = %d, want 0", n)
	}
	// The same person's own envelope still resolves and links nothing new.
	if _, err := rh.r.Route(ctx, aliasEnvelope("wa", "wa:+1", "wa:lid:9")); err != nil {
		t.Fatal(err)
	}
}

// Two envelopes for the same person at once: both route, one link.
func TestRouteConcurrentEnvelopesLinkOnce(t *testing.T) {
	t.Parallel()
	rh := newRouterHarness(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = rh.r.Route(ctx, aliasEnvelope("tg-main", "11", "55"))
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if n := len(rh.ev.of(event.IdentityBound)); n != 1 {
		t.Fatalf("identity.bound = %d, want 1", n)
	}
}
