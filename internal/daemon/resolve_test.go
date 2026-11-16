package daemon

import (
	"context"
	"errors"
	"strings"
	"testing"

	"holaclaw/internal/agent"
	"holaclaw/internal/auth"
	"holaclaw/internal/channel"
	"holaclaw/internal/event"
	"holaclaw/internal/store"
	"holaclaw/internal/store/storetest"
)

// nsChannel is a fake adapter that declares the WhatsApp namespaces.
type nsChannel struct{ fakeChannel }

func (*nsChannel) AliasNamespaces() []string { return []string{"wa:", "wa:lid:"} }

// The daemon's resolver links a channel sender's free aliases and applies the
// namespaces the channel instance declares; API principals link nothing.
func TestResolveUsesChannelNamespaces(t *testing.T) {
	ctx := context.Background()
	d := browserDaemon(t)
	st := d.App.Store
	d.Auth = auth.New(st)
	d.Auth.Namespaces = d.aliasNamespaces
	if ns := d.aliasNamespaces("wa"); ns != nil {
		t.Fatalf("namespaces before channels = %v", ns)
	}
	d.channels = &channels{d: d, byName: map[string]channel.Channel{"wa": &nsChannel{}, "plain": &fakeChannel{}}}

	old, err := d.Auth.CreateIdentity(ctx, "old", store.RoleUser, auth.EmptyEventID)
	if err != nil {
		t.Fatal(err)
	}
	for _, ch := range []string{"wa", "plain"} {
		storetest.Bind(t, st, ch, []string{"wa:+1", "wa:lid:9"}, old.ID, "")
	}
	sender := event.Sender{Aliases: []string{"wa:+1", "wa:lid:5"}}
	if _, err := d.resolve(ctx, event.Origin{Channel: "wa"}, sender); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("declared namespaces: err = %v, want ErrNotFound", err)
	}
	r, err := d.resolve(ctx, event.Origin{Channel: "plain"}, sender)
	if err != nil || r.Identity.ID != old.ID || len(r.Free) != 1 {
		t.Fatalf("no namespaces: %+v, %v", r, err)
	}

	// A holaclaw client arrives from its binding, and nothing else links.
	_, tok, err := d.Auth.CreateToken(ctx, "phone", auth.ScopeClient, old.ID)
	if err != nil {
		t.Fatal(err)
	}
	hc := event.Origin{Channel: event.ChannelHolaclaw, ChannelType: event.ChannelHolaclaw}
	r, err = d.resolve(ctx, hc, event.Sender{Aliases: []string{tok.Binding, "stray"}})
	if err != nil || r.Identity.ID != old.ID {
		t.Fatalf("holaclaw client: %+v, %v", r, err)
	}
	if _, err := d.Auth.Resolve(ctx, event.ChannelHolaclaw, []string{"stray"}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("holaclaw alias linked: %v", err)
	}
	for _, alias := range []string{"user:" + string(old.ID), "owner", "nobody"} {
		if _, err := d.resolve(ctx, hc, event.Sender{Aliases: []string{alias}}); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("holaclaw sender %q: err = %v, want ErrNotFound", alias, err)
		}
	}
}

// A new alias linked on the router's path carries the sender's display name
// sanitized, the way every other binding stores it.
func TestResolveLinksSanitizedDisplay(t *testing.T) {
	ctx := context.Background()
	d := browserDaemon(t)
	d.Auth = auth.New(d.App.Store)
	i, err := d.Auth.CreateIdentity(ctx, "ana", store.RoleUser, auth.EmptyEventID)
	if err != nil {
		t.Fatal(err)
	}
	storetest.Bind(t, d.App.Store, "tg", []string{"known"}, i.ID, "")
	sender := event.Sender{Aliases: []string{"known", "fresh"}, DisplayName: "ev" + string(rune(0x202e)) + "il\nname"}
	if r, err := d.resolve(ctx, event.Origin{Channel: "tg"}, sender); err != nil || r.Identity.ID != i.ID {
		t.Fatalf("resolve: %+v, %v", r, err)
	}
	var got string
	if err := d.App.Store.DB().QueryRowContext(ctx,
		`SELECT display FROM bindings WHERE channel = 'tg' AND alias = 'fresh'`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if want := agent.SanitizeDisplay(sender.DisplayName); got == "" || got != want || strings.ContainsAny(got, string(rune(0x202e))+"\n") {
		t.Fatalf("stored display %q, want sanitized %q", got, want)
	}
}
