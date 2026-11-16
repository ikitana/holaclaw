package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"holaclaw/internal/api/gen"
	"holaclaw/internal/auth"
	"holaclaw/internal/config"
	"holaclaw/internal/event"
	"holaclaw/internal/store"
)

// pair posts a pairing with no bearer, the way a new device does.
func pair(h *harness, code, device, platform string) *http.Response {
	h.t.Helper()
	return h.do(http.MethodPost, "/pair", "", gen.PairRequest{Code: code, Device: device, Platform: platform})
}

// pairingInvite mints a holaclaw invite for user through the API, as an admin.
func pairingInvite(h *harness, user event.ID, scope gen.InviteCreateScope) gen.Invite {
	h.t.Helper()
	id := string(user)
	in := gen.InviteCreate{Channel: event.ChannelHolaclaw, UserId: &id}
	if scope != "" {
		in.Scope = &scope
	}
	return createInvite(h, in)
}

// A device pairs with the code alone and gets a token that works at the
// invite's scope; the code is spent.
func TestPairDevice(t *testing.T) {
	h := newHarness(t)
	inv := pairingInvite(h, h.other.ID, "")

	got := decode[gen.TokenCreated](t, pair(h, inv.Code, "iPhone 15", "ios"), http.StatusCreated)
	if got.Secret == "" || got.Token.Origin != gen.TokenOriginPair || got.Token.OriginRef == nil ||
		*got.Token.OriginRef != inv.Id || got.Token.Scope != gen.TokenScopeClient || got.Token.Name != "iPhone 15" ||
		got.Token.UserId == nil || *got.Token.UserId != string(h.other.ID) {
		t.Fatalf("paired = %+v", got.Token)
	}
	list := decode[struct{ Devices []gen.Device }](t, h.do(http.MethodGet, "/me/devices", got.Secret, nil), http.StatusOK)
	var current *gen.Device
	for i, d := range list.Devices {
		if d.Current {
			current = &list.Devices[i]
		}
	}
	if current == nil || current.Binding != got.Token.Binding || current.Display == nil || *current.Display != "iPhone 15" ||
		current.Platform == nil || *current.Platform != "ios" || current.Tokens != 1 {
		t.Fatalf("devices = %+v, want the new one marked current", list.Devices)
	}
	// A client token never reaches an admin call.
	decode[gen.Error](t, h.do(http.MethodGet, "/identities", got.Secret, nil), http.StatusForbidden)

	e := decode[gen.Error](t, pair(h, inv.Code, "iPad", "ipados"), http.StatusBadRequest)
	if e.Code != CodeBadRequest || !strings.Contains(e.Message, "invalid or has expired") {
		t.Fatalf("second pairing = %+v", e)
	}
	if !strings.Contains(h.logs.String(), "/v1/pair") || strings.Contains(h.logs.String(), inv.Code) ||
		strings.Contains(h.logs.String(), got.Secret) {
		t.Fatal("the access log carries the code or the token, or no pairing line")
	}
}

// An admin invite pairs an admin device; a role that does not allow admin
// gets no admin invite at all.
func TestPairDeviceRespectsTheInviteScope(t *testing.T) {
	h := newHarness(t)
	inv := pairingInvite(h, h.admin.ID, gen.InviteCreateScopeAdmin)
	got := decode[gen.TokenCreated](t, pair(h, inv.Code, "Mac", "macos"), http.StatusCreated)
	if got.Token.Scope != gen.TokenScopeAdmin {
		t.Fatalf("scope = %s, want admin", got.Token.Scope)
	}
	decode[struct{}](t, h.do(http.MethodGet, "/identities", got.Secret, nil), http.StatusOK)
}

func TestPairDeviceRefusals(t *testing.T) {
	h := newHarness(t)
	inv := pairingInvite(h, h.other.ID, "")
	for _, c := range []struct{ name, device, platform, want string }{
		{"no device", "", "ios", "device name"},
		{"bad platform", "Mac", "Mac OS", "platform"},
	} {
		e := decode[gen.Error](t, pair(h, inv.Code, c.device, c.platform), http.StatusBadRequest)
		if !strings.Contains(e.Message, c.want) {
			t.Errorf("%s: %+v", c.name, e)
		}
	}
	resp := h.do(http.MethodPost, "/pair", "", map[string]any{"code": inv.Code, "device": "Mac", "platform": "macos", "scope": "admin"})
	decode[gen.Error](t, resp, http.StatusBadRequest)
	decode[gen.TokenCreated](t, pair(h, inv.Code, "Mac", "macos"), http.StatusCreated)
}

// Failed codes from one address are limited: past the limit even a good code
// is refused, and stays unspent.
func TestPairDeviceRateLimit(t *testing.T) {
	h := newHarness(t)
	inv := pairingInvite(h, h.other.ID, "")
	for range 5 {
		decode[gen.Error](t, pair(h, "WRONGCODE0", "Mac", "macos"), http.StatusBadRequest)
	}
	e := decode[gen.Error](t, pair(h, inv.Code, "Mac", "macos"), http.StatusTooManyRequests)
	if e.Code != CodeRateLimited {
		t.Fatalf("error = %+v", e)
	}
	// From another address the same code still pairs.
	body, _ := json.Marshal(gen.PairRequest{Code: inv.Code, Device: "Mac", Platform: "macos"})
	r := httptest.NewRequest(http.MethodPost, BaseURL+"/pair", bytes.NewReader(body))
	r.RemoteAddr = "[2001:db8::1]:4242"
	w := httptest.NewRecorder()
	h.h.ServeHTTP(w, r)
	if w.Code != http.StatusCreated {
		t.Fatalf("another address: %d %s", w.Code, w.Body)
	}
}

// Only POST /v1/pair goes without a token: no other method, spelling or
// neighbouring path reaches a handler unauthenticated.
func TestPairExemptionIsExact(t *testing.T) {
	h := newHarness(t)
	for _, c := range []struct{ method, path string }{
		{http.MethodGet, "/v1/pair"},
		{http.MethodPut, "/v1/pair"},
		{http.MethodPost, "/v1/pair/"},
		{http.MethodPost, "/v1/pair%2F"},
		{http.MethodPost, "/v1/%70air"},
		{http.MethodPost, "//v1/pair"},
		{http.MethodPost, "/v1/pair/../me/devices"},
		{http.MethodPost, "/v1/me/devices"},
	} {
		r := httptest.NewRequest(c.method, "http://x"+c.path, strings.NewReader(`{}`))
		w := httptest.NewRecorder()
		h.h.ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without a token: %d, want 401", c.method, c.path, w.Code)
		}
	}
	if !isPair(httptest.NewRequest(http.MethodPost, BaseURL+"/pair", nil)) {
		t.Fatal("POST /v1/pair is not the pairing call")
	}
}

// The body of a pairing-link request is optional, chunked or not.
func TestCreateDeviceLinkEmptyBody(t *testing.T) {
	h := newHarness(t)
	for _, body := range []string{"", "  \n"} {
		req, err := http.NewRequest(http.MethodPost, h.srv.URL+BaseURL+"/me/devices", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.ContentLength = -1 // sent chunked
		req.Header.Set("Authorization", "Bearer "+h.otherToken)
		resp, err := h.srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if inv := decode[gen.Invite](t, resp, http.StatusCreated); inv.Scope == nil || *inv.Scope != gen.InviteScopeClient {
			t.Fatalf("body %q: invite = %+v", body, inv)
		}
	}
	decode[gen.Error](t, h.do(http.MethodPost, "/me/devices", h.otherToken, map[string]string{"ttl": "1h"}), http.StatusBadRequest)
}

func TestSourceOf(t *testing.T) {
	for addr, want := range map[string]string{
		"192.0.2.7:1234":                "192.0.2.7",
		"[::ffff:192.0.2.7]:1234":       "192.0.2.7",
		"[2001:db8:1:2:aaaa::1]:1234":   "2001:db8:1:2::/64",
		"[2001:db8:1:2:bbbb::9]:1":      "2001:db8:1:2::/64",
		"not an address":                "not an address",
		"[fe80::1%eth0]:1234":           "fe80::/64",
		"[2001:db8:1:3:aaaa::1]:65535":  "2001:db8:1:3::/64",
		"203.0.113.250:65535":           "203.0.113.250",
		"[2001:db8:ffff:ffff::1]:65535": "2001:db8:ffff:ffff::/64",
	} {
		r := httptest.NewRequest(http.MethodPost, "/", nil)
		r.RemoteAddr = addr
		if got := sourceOf(r); got != want {
			t.Errorf("sourceOf(%q) = %q, want %q", addr, got, want)
		}
	}
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	if got := sourceOf(r.WithContext(markSocket(r.Context()))); got != "unix" {
		t.Errorf("socket = %q", got)
	}
}

// /v1/me/devices lists, links and unbinds only the caller's own bindings.
func TestMeDevicesTouchOnlyTheCallersBindings(t *testing.T) {
	h := newHarness(t)
	ownerBefore, err := h.auth.Bindings(t.Context(), h.owner.ID)
	if err != nil {
		t.Fatal(err)
	}

	list := decode[struct{ Devices []gen.Device }](t, h.do(http.MethodGet, "/me/devices", h.otherToken, nil), http.StatusOK)
	if len(list.Devices) != 1 || !list.Devices[0].Current || list.Devices[0].Tokens != 1 {
		t.Fatalf("devices = %+v, want nadia's one binding", list.Devices)
	}

	// A pairing link pairs onto the caller's own identity, at client scope.
	link := decode[gen.Invite](t, h.do(http.MethodPost, "/me/devices", h.otherToken, nil), http.StatusCreated)
	if link.UserId == nil || *link.UserId != string(h.other.ID) || link.Scope == nil || *link.Scope != gen.InviteScopeClient ||
		link.Link != nil {
		t.Fatalf("link = %+v, want no link on loopback", link)
	}
	paired := decode[gen.TokenCreated](t, pair(h, link.Code, "Phone", "android"), http.StatusCreated)
	if paired.Token.UserId == nil || *paired.Token.UserId != string(h.other.ID) {
		t.Fatalf("paired onto %v, want nadia", paired.Token.UserId)
	}
	// Never above the caller's own scope.
	e := decode[gen.Error](t, h.do(http.MethodPost, "/me/devices", h.otherToken, gen.DeviceLinkCreate{Scope: ptr(gen.DeviceLinkCreateScopeAdmin)}),
		http.StatusForbidden)
	if e.Code != CodeForbidden {
		t.Fatalf("admin from a client = %+v", e)
	}
	admin := decode[gen.Invite](t, h.do(http.MethodPost, "/me/devices", h.adminToken, gen.DeviceLinkCreate{Scope: ptr(gen.DeviceLinkCreateScopeAdmin)}),
		http.StatusCreated)
	if admin.Scope == nil || *admin.Scope != gen.InviteScopeAdmin || *admin.UserId != string(h.admin.ID) {
		t.Fatalf("admin link = %+v", admin)
	}

	// Someone else's binding is not found, and stays.
	for _, b := range ownerBefore {
		resp := h.do(http.MethodDelete, "/me/devices/holaclaw:"+b.Alias, h.otherToken, nil)
		decode[gen.Error](t, resp, http.StatusNotFound)
	}
	if after, _ := h.auth.Bindings(t.Context(), h.owner.ID); len(after) != len(ownerBefore) {
		t.Fatalf("the owner's bindings went from %d to %d", len(ownerBefore), len(after))
	}
	decode[gen.Error](t, h.do(http.MethodDelete, "/me/devices/telegram:1", h.otherToken, nil), http.StatusBadRequest)
	decode[gen.Error](t, h.do(http.MethodDelete, "/me/devices/holaclaw:cli", h.ownerToken, nil), http.StatusBadRequest)

	// Its own goes, with its token.
	u := decode[gen.BindingUnbound](t, h.do(http.MethodDelete, "/me/devices/"+paired.Token.Binding, h.otherToken, nil), http.StatusOK)
	if u.Identity != string(h.other.ID) || u.TokensRevoked != 1 {
		t.Fatalf("unbound = %+v", u)
	}
	decode[gen.Error](t, h.do(http.MethodGet, "/me/devices", paired.Secret, nil), http.StatusUnauthorized)
}

// The link carries server.public_url when it is set, over TCP or the socket;
// on the socket without it, the address listen names, or no link at all.
func TestPairingLinkServer(t *testing.T) {
	h := newHarness(t)
	socketLink := func() gen.Invite {
		t.Helper()
		r := httptest.NewRequest(http.MethodPost, BaseURL+"/me/devices", nil)
		r = r.WithContext(markSocket(r.Context()))
		w := httptest.NewRecorder()
		h.h.ServeHTTP(w, r)
		if w.Code != http.StatusCreated {
			t.Fatalf("socket link: %d %s", w.Code, w.Body)
		}
		var inv gen.Invite
		if err := json.Unmarshal(w.Body.Bytes(), &inv); err != nil {
			t.Fatal(err)
		}
		return inv
	}
	if inv := socketLink(); inv.Link != nil || inv.Instruction == nil || !strings.Contains(*inv.Instruction, "public_url") {
		t.Fatalf("socket, no address = %+v", inv)
	}
	h.b.Config.Server.Listen = "192.0.2.10:8484"
	if inv := socketLink(); inv.Link == nil || *inv.Link != PairLink("http://192.0.2.10:8484", inv.Code) {
		t.Fatalf("socket, listen = %+v", inv)
	}
	h.b.Config.Server.PublicURL = "https://claw.example"
	if inv := socketLink(); inv.Link == nil || *inv.Link != PairLink("https://claw.example", inv.Code) {
		t.Fatalf("socket, public url = %+v", inv)
	}
	inv := decode[gen.Invite](t, h.do(http.MethodPost, "/me/devices", h.ownerToken, nil), http.StatusCreated)
	if inv.Link == nil || *inv.Link != PairLink("https://claw.example", inv.Code) {
		t.Fatalf("tcp, public url = %+v", inv)
	}
}

// Without public_url a link minted over TCP carries the address the request
// reached, unless that address is loopback (a tunnel, or the host itself):
// then it falls back to listen, and with a loopback listen there is no link.
func TestPairingLinkRequestHost(t *testing.T) {
	h := newHarness(t)
	tcpLink := func(host string) gen.Invite {
		t.Helper()
		r := httptest.NewRequest(http.MethodPost, BaseURL+"/me/devices", nil)
		r.Host = host
		r.Header.Set("Authorization", "Bearer "+h.ownerToken)
		w := httptest.NewRecorder()
		h.h.ServeHTTP(w, r)
		if w.Code != http.StatusCreated {
			t.Fatalf("link via %s: %d %s", host, w.Code, w.Body)
		}
		var inv gen.Invite
		if err := json.Unmarshal(w.Body.Bytes(), &inv); err != nil {
			t.Fatal(err)
		}
		return inv
	}
	if inv := tcpLink("claw.lan:8484"); inv.Link == nil || *inv.Link != PairLink("http://claw.lan:8484", inv.Code) {
		t.Fatalf("reachable host = %+v", inv)
	}
	if inv := tcpLink("[2001:db8::1]:8484"); inv.Link == nil || *inv.Link != PairLink("http://[2001:db8::1]:8484", inv.Code) {
		t.Fatalf("reachable v6 host = %+v", inv)
	}
	for _, host := range []string{"127.0.0.1:8484", "localhost:8484", "[::1]:8484", "localhost"} {
		if inv := tcpLink(host); inv.Link != nil || inv.Instruction == nil || !strings.Contains(*inv.Instruction, "public_url") {
			t.Fatalf("loopback host %s = %+v, want no link", host, inv)
		}
	}
	h.b.Config.Server.Listen = "192.0.2.10:8484"
	if inv := tcpLink("127.0.0.1:8484"); inv.Link == nil || *inv.Link != PairLink("http://192.0.2.10:8484", inv.Code) {
		t.Fatalf("loopback host, reachable listen = %+v", inv)
	}
}

func TestServerURL(t *testing.T) {
	for _, c := range []struct {
		s    config.Server
		want string
	}{
		{config.Server{}, ""},
		{config.Server{Listen: "0.0.0.0:8484"}, ""},
		{config.Server{Listen: "[::]:8484"}, ""},
		{config.Server{Listen: ":8484"}, ""},
		{config.Server{Listen: "nonsense"}, ""},
		// Loopback reaches no other device: no link, as for a wildcard.
		{config.Server{Listen: "127.0.0.1:8484"}, ""},
		{config.Server{Listen: "127.8.0.1:8484"}, ""},
		{config.Server{Listen: "[::1]:8484"}, ""},
		{config.Server{Listen: "[::ffff:127.0.0.1]:8484"}, ""},
		{config.Server{Listen: "localhost:8484"}, ""},
		{config.Server{Listen: "LOCALHOST:8484"}, ""},
		{config.Server{Listen: "localhost.:8484"}, ""},
		{config.Server{Listen: "app.localhost:8484"}, ""},
		{config.Server{Listen: "notlocalhost:8484"}, "http://notlocalhost:8484"},
		{config.Server{Listen: "192.0.2.10:8484"}, "http://192.0.2.10:8484"},
		{config.Server{Listen: "[2001:db8::1]:8484"}, "http://[2001:db8::1]:8484"},
		// An explicit public_url is taken as it is, even a loopback one.
		{config.Server{Listen: "127.0.0.1:8484", PublicURL: "http://127.0.0.1:9000"}, "http://127.0.0.1:9000"},
		{config.Server{Listen: "claw.lan:8484", AllowInsecureTransport: true}, "http://claw.lan:8484"},
		{config.Server{Listen: "claw.lan:443", TLSCert: "c", TLSKey: "k"}, "https://claw.lan:443"},
		{config.Server{Listen: "0.0.0.0:8484", PublicURL: "https://claw.example"}, "https://claw.example"},
	} {
		if got := ServerURL(c.s); got != c.want {
			t.Errorf("ServerURL(%+v) = %q, want %q", c.s, got, c.want)
		}
	}
	if got := PairLink("https://claw.example:8443/x", "AB12"); got != "holaclaw://pair?server=https%3A%2F%2Fclaw.example%3A8443%2Fx&code=AB12" {
		t.Errorf("PairLink = %q", got)
	}
}

// Before `holaclaw init` pairing answers 503 like every other call; after it,
// the owner's first device pairs with an invite minted over the socket.
func TestPairBeforeAndAfterInit(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "data")
	g := newGated(t, dataDir)
	post := func(code string) *httptest.ResponseRecorder {
		body, _ := json.Marshal(gen.PairRequest{Code: code, Device: "Mac", Platform: "macos"})
		r := httptest.NewRequest(http.MethodPost, BaseURL+"/pair", bytes.NewReader(body))
		w := httptest.NewRecorder()
		g.h.ServeHTTP(w, r)
		return w
	}
	if w := post("WRONGCODE0"); w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), CodeNotInitialised) {
		t.Fatalf("before init: %d %s", w.Code, w.Body)
	}
	other, err := store.Open(t.Context(), dataDir)
	if err != nil {
		t.Fatal(err)
	}
	svc := auth.New(other)
	b, err := svc.CreateOwner(t.Context(), false, nil)
	if err != nil {
		t.Fatal(err)
	}
	socket, err := svc.SocketPrincipal(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	code, _, err := svc.InviteDevice(t.Context(), socket, "")
	_ = other.Close()
	if err != nil {
		t.Fatal(err)
	}
	w := post(code)
	if w.Code != http.StatusCreated {
		t.Fatalf("after init: %d %s", w.Code, w.Body)
	}
	var got gen.TokenCreated
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || got.Token.UserId == nil || *got.Token.UserId != string(b.Owner.ID) {
		t.Fatalf("paired = %+v, %v; want the owner", got, err)
	}
}
