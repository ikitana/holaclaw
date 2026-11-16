package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"holaclaw/internal/api/gen"
	"holaclaw/internal/config"
	"holaclaw/internal/event"
	"holaclaw/internal/store"
	"holaclaw/internal/store/storetest"
)

// channelsAre makes the backend report one channel instance.
func channelsAre(h *harness, st gen.ChannelStatus) {
	h.b.Channels = func() []gen.ChannelStatus { return []gen.ChannelStatus{st} }
}

func telegramChannel(name, detail string, state gen.ChannelStatusState) gen.ChannelStatus {
	s := gen.ChannelStatus{Name: name, Type: "telegram", Agent: "main", State: state}
	if detail != "" {
		d := detail
		s.Detail = &d
	}
	return s
}

// createInvite posts one invite and decodes it.
func createInvite(h *harness, in gen.InviteCreate) gen.Invite {
	h.t.Helper()
	resp := h.do(http.MethodPost, "/invites", h.adminToken, in)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		h.t.Fatalf("create invite: status = %d, want 201", resp.StatusCode)
	}
	var out gen.Invite
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		h.t.Fatalf("decode invite: %v", err)
	}
	return out
}

// dmInvite is an invite for h.other on channel.
func dmInvite(h *harness, channel string) gen.InviteCreate {
	return gen.InviteCreate{Channel: channel, UserId: ptr(string(h.other.ID))}
}

// identity creates a confirmed identity with no bindings.
func identity(h *harness, name string, role store.Role) event.ID {
	h.t.Helper()
	i, err := h.auth.CreateIdentity(context.Background(), name, role, h.owner.ID)
	if err != nil {
		h.t.Fatal(err)
	}
	return i.ID
}

// badInvite posts in and wants a bad_request mentioning want.
func badInvite(t *testing.T, h *harness, in gen.InviteCreate, want string) {
	t.Helper()
	e := decode[gen.Error](t, h.do(http.MethodPost, "/invites", h.adminToken, in), http.StatusBadRequest)
	if e.Code != CodeBadRequest || !strings.Contains(e.Message, want) {
		t.Fatalf("error = %+v, want a bad_request mentioning %q", e, want)
	}
}

func TestCreateInviteCarriesTheDeepLink(t *testing.T) {
	h := newHarness(t)
	configChannel(h, "tg-main", "telegram", "off")
	channelsAre(h, telegramChannel("tg-main", "@holaclaw", gen.Connected))

	inv := createInvite(h, dmInvite(h, "tg-main"))
	if inv.Link == nil || inv.Instruction != nil {
		t.Fatalf("invite = %+v, want a link and no instruction", inv)
	}
	if want := "https://t.me/holaclaw?start=" + inv.Code; *inv.Link != want {
		t.Fatalf("link = %q, want %q", *inv.Link, want)
	}
	if inv.Channel != "tg-main" || inv.UserId == nil || *inv.UserId != string(h.other.ID) || inv.Scope != nil {
		t.Fatalf("invite = %+v", inv)
	}
	if ttl := time.Until(inv.ExpiresAt); ttl > 15*time.Minute || ttl < 14*time.Minute {
		t.Fatalf("expires in %v, want 15m", ttl)
	}
}

// The username is only known once getMe has answered. Until then the invite
// still works: it goes out with how to redeem it.
func TestCreateInviteBeforeTheChannelConnectsHasAnInstruction(t *testing.T) {
	h := newHarness(t)
	configChannel(h, "tg-main", "telegram", "off")
	channelsAre(h, telegramChannel("tg-main", "", gen.Unlinked))

	inv := createInvite(h, dmInvite(h, "tg-main"))
	if inv.Code == "" || inv.Link != nil {
		t.Fatalf("invite = %+v, want a code and no link", inv)
	}
	if inv.Instruction == nil || !strings.Contains(*inv.Instruction, "direct message") {
		t.Fatalf("instruction = %v", inv.Instruction)
	}
}

// A detail that is not a username (a reconnect reason, say) must not be
// pasted into a URL.
func TestCreateInviteIgnoresANonUsernameDetail(t *testing.T) {
	h := newHarness(t)
	configChannel(h, "tg-main", "telegram", "off")
	channelsAre(h, telegramChannel("tg-main", "reconnecting in 4s", gen.Connected))

	if inv := createInvite(h, dmInvite(h, "tg-main")); inv.Link != nil {
		t.Fatalf("link = %q, want none", *inv.Link)
	}
}

// Only adapters with a deep link get one; slack gets the one-line instruction.
func TestCreateInviteForANonTelegramChannelHasAnInstruction(t *testing.T) {
	h := newHarness(t)
	configChannel(h, "slack-ops", "slack", "off")
	detail := "@holaclaw"
	channelsAre(h, gen.ChannelStatus{
		Name: "slack-ops", Type: "slack", Agent: "main", State: gen.Connected, Detail: &detail,
	})

	inv := createInvite(h, dmInvite(h, "slack-ops"))
	if inv.Link != nil || inv.Instruction == nil || *inv.Instruction != "Send this code to the bot in a direct message." {
		t.Fatalf("invite = %+v, want the DM instruction and no link", inv)
	}
}

// An invite bound to a channel the daemon does not run could never redeem:
// it is refused, rather than borrowing another instance's bot or printing a
// dead code. An invite bound to no channel is refused too.
func TestCreateInviteNeedsAKnownChannel(t *testing.T) {
	h := newHarness(t)
	configChannel(h, "tg-main", "telegram", "off")
	channelsAre(h, telegramChannel("tg-main", "@holaclaw", gen.Connected))

	badInvite(t, h, dmInvite(h, "tg-other"), `unknown channel "tg-other"`)
	badInvite(t, h, dmInvite(h, ""), "channel is required")
}

// A DM invite names one confirmed user identity, which the binding gets its
// role from.
func TestCreateInviteNeedsAnIdentity(t *testing.T) {
	h := newHarness(t)
	configChannel(h, "tg-main", "telegram", "off")
	g := h.guest("111")

	badInvite(t, h, gen.InviteCreate{Channel: "tg-main"}, "must name an identity")
	badInvite(t, h, gen.InviteCreate{Channel: "tg-main", UserId: ptr("nosuchuser")}, "is not an identity id")
	badInvite(t, h, gen.InviteCreate{Channel: "tg-main", UserId: ptr(string(g.ID))}, "pending")
	errorBody(t, h.do(http.MethodPost, "/invites", h.adminToken,
		gen.InviteCreate{Channel: "tg-main", UserId: ptr(string(event.NewID()))}), http.StatusNotFound, CodeNotFound)
}

// Only the owner mints an invite for the owner: the redeemer would act with
// the owner's role and memory.
func TestOwnerInviteIsOwnerOnly(t *testing.T) {
	h := newHarness(t)
	in := gen.InviteCreate{Channel: "holaclaw", UserId: ptr(string(h.owner.ID))}

	errorBody(t, h.do(http.MethodPost, "/invites", h.adminToken, in), http.StatusForbidden, CodeForbidden)
	inv := decode[gen.Invite](t, h.do(http.MethodPost, "/invites", h.ownerAdmin, in), http.StatusCreated)
	if inv.UserId == nil || *inv.UserId != string(h.owner.ID) {
		t.Fatalf("invite = %+v", inv)
	}
}

// The TTL is refused above an hour, or above a day onto an identity with no
// bindings yet.
func TestCreateInviteTTL(t *testing.T) {
	h := newHarness(t)
	configChannel(h, "tg-main", "telegram", "off")
	fresh := string(identity(h, "fresh", store.RoleUser))
	bound := identity(h, "bound", store.RoleUser)
	storetest.Bind(t, h.st, "tg-main", []string{"77"}, bound, "")
	ttl := func(d time.Duration) *int { return ptr(int(d.Seconds())) }

	day := createInvite(h, gen.InviteCreate{Channel: "tg-main", UserId: &fresh, TtlSeconds: ttl(24 * time.Hour)})
	if got := time.Until(day.ExpiresAt); got < 23*time.Hour {
		t.Fatalf("expires in %v, want a day", got)
	}
	badInvite(t, h, gen.InviteCreate{Channel: "tg-main", UserId: &fresh, TtlSeconds: ttl(25 * time.Hour)}, "between 60 and 86400")
	b := string(bound)
	createInvite(h, gen.InviteCreate{Channel: "tg-main", UserId: &b, TtlSeconds: ttl(time.Hour)})
	badInvite(t, h, gen.InviteCreate{Channel: "tg-main", UserId: &b, TtlSeconds: ttl(2 * time.Hour)}, "at most 1h")
	// Out of range before it becomes a Duration: too short, or so long it would wrap.
	for _, s := range []int{1, 59, 18446744074} {
		badInvite(t, h, gen.InviteCreate{Channel: "tg-main", UserId: &fresh, TtlSeconds: &s}, "ttl_seconds must be between 60 and 86400")
	}
}

// A holaclaw invite pairs a device: the channel is built in, the scope is
// client unless asked, and admin only for a role that allows it. No other
// channel takes a scope.
func TestCreatePairingInvite(t *testing.T) {
	h := newHarness(t)
	configChannel(h, "tg-main", "telegram", "off")
	admin := string(identity(h, "ada", store.RoleAdmin))
	user := string(h.other.ID)

	// The test server is on loopback, which no other device reaches: the code
	// goes out with how to get a link instead.
	inv := createInvite(h, gen.InviteCreate{Channel: "holaclaw", UserId: &user})
	if inv.Scope == nil || *inv.Scope != gen.InviteScopeClient || inv.Link != nil || inv.Instruction == nil ||
		!strings.Contains(*inv.Instruction, "server.public_url") {
		t.Fatalf("invite = %+v", inv)
	}
	inv = createInvite(h, gen.InviteCreate{Channel: "holaclaw", UserId: &admin, Scope: ptr(gen.InviteCreateScopeAdmin)})
	if inv.Scope == nil || *inv.Scope != gen.InviteScopeAdmin {
		t.Fatalf("scope = %v, want admin", inv.Scope)
	}
	badInvite(t, h, gen.InviteCreate{Channel: "holaclaw", UserId: &user, Scope: ptr(gen.InviteCreateScopeAdmin)}, "has user")
	badInvite(t, h, gen.InviteCreate{Channel: "tg-main", UserId: &admin, Scope: ptr(gen.InviteCreateScopeClient)}, "only to holaclaw")
	badInvite(t, h, gen.InviteCreate{Channel: "holaclaw", UserId: &admin, Scope: ptr(gen.InviteCreateScope("channel"))}, "invalid scope")
	badInvite(t, h, gen.InviteCreate{Channel: "holaclaw", Kind: ptr(gen.InviteCreateKindRoom)}, "has no rooms")
}

// configChannel declares a channel instance in the backend config, which is
// what decides whether a room invite may be minted for it.
func configChannel(h *harness, name, typ, rooms string) {
	if h.b.Config.Channels == nil {
		h.b.Config.Channels = map[string]config.Channel{}
	}
	h.b.Config.Channels[name] = config.Channel{
		Name: name, Type: typ, Agent: "main",
		Access: config.Access{DM: "invite", Rooms: rooms, Reply: "mention"},
	}
}

func TestCreateRoomInviteCarriesTheStartgroupLink(t *testing.T) {
	h := newHarness(t)
	configChannel(h, "tg-main", "telegram", "approved")
	channelsAre(h, telegramChannel("tg-main", "@holaclaw", gen.Connected))
	kind := gen.InviteCreateKindRoom

	inv := createInvite(h, gen.InviteCreate{Channel: "tg-main", Kind: &kind})
	if inv.Link == nil {
		t.Fatal("no link on a room invite for a connected telegram channel")
	}
	if want := "https://t.me/holaclaw?startgroup=" + inv.Code; *inv.Link != want {
		t.Fatalf("link = %q, want %q", *inv.Link, want)
	}
	if inv.UserId != nil || inv.Scope != nil {
		t.Fatalf("invite = %+v, want no identity or scope", inv)
	}
}

// Google Chat has no deep link, so a room invite for it goes out with how to
// redeem it, but, unlike slack, it is not refused up front.
func TestCreateRoomInviteGoogleChatAllowed(t *testing.T) {
	h := newHarness(t)
	configChannel(h, "chat-main", "googlechat", "approved")
	kind := gen.InviteCreateKindRoom

	inv := createInvite(h, gen.InviteCreate{Channel: "chat-main", Kind: &kind})
	if inv.Link != nil {
		t.Fatalf("link = %q, want none: googlechat has no deep link", *inv.Link)
	}
	if inv.Instruction == nil || !strings.Contains(*inv.Instruction, "Mention the app") {
		t.Fatalf("instruction = %v", inv.Instruction)
	}
}

// A room invite nobody could redeem is refused up front, with a message that
// says why, instead of printing a dead code.
func TestCreateRoomInviteRefusals(t *testing.T) {
	h := newHarness(t)
	configChannel(h, "tg-main", "telegram", "approved")
	configChannel(h, "tg-off", "telegram", "off")
	configChannel(h, "tg-open", "telegram", "open")
	configChannel(h, "slack-ops", "slack", "approved")
	room := gen.InviteCreateKindRoom
	user := string(h.other.ID)

	cases := map[string]struct {
		in   gen.InviteCreate
		want string
	}{
		"no channel":      {gen.InviteCreate{Kind: &room}, "channel is required"},
		"unknown channel": {gen.InviteCreate{Kind: &room, Channel: "tg-other"}, `unknown channel "tg-other"`},
		"slack":           {gen.InviteCreate{Kind: &room, Channel: "slack-ops"}, "(slack) does not support room invites"},
		"rooms off":       {gen.InviteCreate{Kind: &room, Channel: "tg-off"}, `rooms = "off"`},
		"rooms open":      {gen.InviteCreate{Kind: &room, Channel: "tg-open"}, `rooms = "open"`},
		"an identity":     {gen.InviteCreate{Kind: &room, Channel: "tg-main", UserId: &user}, "name no identity"},
		"bad kind":        {gen.InviteCreate{Kind: ptr(gen.InviteCreateKind("group")), Channel: "tg-main"}, "invalid kind"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) { badInvite(t, h, tc.in, tc.want) })
	}
}
