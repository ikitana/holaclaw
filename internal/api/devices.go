package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"

	"holaclaw/internal/api/gen"
	"holaclaw/internal/auth"
	"holaclaw/internal/config"
	"holaclaw/internal/event"
	"holaclaw/internal/store"
)

// PairLink is the link a device opens to pair: the app reads the server and
// the code from it (docs/identity.md §3).
func PairLink(server, code string) string {
	return "holaclaw://pair?server=" + url.QueryEscape(server) + "&code=" + url.QueryEscape(code)
}

// ServerURL is the address devices reach the control API at, as the config
// says it: server.public_url, else server.listen when it names a host another
// device can reach, over https with TLS configured and http without. A
// wildcard, loopback or missing listen names none, and the answer is "": a
// link to 127.0.0.1 would look valid and reach no phone.
func ServerURL(s config.Server) string {
	if s.PublicURL != "" {
		return s.PublicURL
	}
	host, port, err := net.SplitHostPort(s.Listen)
	if err != nil || !reachableHost(host) {
		return ""
	}
	scheme := "http"
	if s.TLSCert != "" && s.TLSKey != "" {
		scheme = "https"
	}
	return scheme + "://" + net.JoinHostPort(host, port)
}

// reachableHost reports whether host names a machine other devices can reach:
// not empty, a wildcard, localhost (or a name under it, RFC 6761) or a
// loopback address.
func reachableHost(host string) bool {
	name := strings.ToLower(strings.TrimSuffix(host, "."))
	if name == "" || name == "localhost" || strings.HasSuffix(name, ".localhost") {
		return false
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		ip = ip.Unmap()
		return !ip.IsUnspecified() && !ip.IsLoopback()
	}
	return true
}

// serverURL is the address a pairing link minted by r carries:
// server.public_url, else the address r itself reached over TCP when other
// devices can reach it too, else what ServerURL derives from the config. A
// request on the unix socket, or through a tunnel to loopback, reached no
// address a device could use.
func (b *Backend) serverURL(r *http.Request) string {
	var s config.Server
	if b.Config != nil {
		s = b.Config.Server
	}
	if s.PublicURL != "" || IsSocket(r.Context()) {
		return ServerURL(s)
	}
	host := r.Host
	if h, _, err := net.SplitHostPort(r.Host); err == nil {
		host = h
	}
	if !reachableHost(strings.Trim(host, "[]")) {
		return ServerURL(s)
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// isPair matches the pairing call before routing has run, so the auth
// middleware can let the code answer for it: POST and the exact path.
func isPair(r *http.Request) bool {
	return r.Method == http.MethodPost && r.URL.EscapedPath() == BaseURL+"/pair"
}

// sourceOf is the network address failed pairings are limited by: the peer's
// IP, its /64 for IPv6, where one host usually holds the whole prefix, and
// "unix" on the socket.
func sourceOf(r *http.Request) string {
	if IsSocket(r.Context()) {
		return "unix"
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return host
	}
	ip = ip.Unmap().WithZone("")
	if ip.Is6() {
		p, _ := ip.Prefix(64) // never fails: a zoneless IPv6 address has 128 bits
		return p.String()
	}
	return ip.String()
}

// PairDevice redeems a holaclaw invite for a device. The request carries no
// token: the code is the credential, and the secret is shown once, here.
func (h *handlers) PairDevice(w http.ResponseWriter, r *http.Request) {
	if h.b.Auth == nil {
		notImplemented(w, "pairing")
		return
	}
	var in gen.PairRequest
	if !decodeJSON(w, r, &in) {
		return
	}
	secret, t, err := h.b.Auth.Pair(r.Context(), in.Code, in.Device, in.Platform, sourceOf(r))
	switch {
	case errors.Is(err, auth.ErrInvalidDevice):
		badRequest(w, strings.TrimPrefix(err.Error(), "auth: "))
	case errors.Is(err, auth.ErrInviteInvalid):
		badRequest(w, "the code is invalid or has expired")
	case errors.Is(err, auth.ErrRateLimited):
		writeError(w, http.StatusTooManyRequests, CodeRateLimited, "too many failed codes; try again later")
	case err != nil:
		h.b.fail(w, "pair device", err)
	default:
		writeJSON(w, http.StatusCreated, gen.TokenCreated{Token: toGenToken(t), Secret: secret})
	}
}

// GetMe tells the caller who it is, as this very request resolved: the role
// and the scope it acts with, its binding and its token. The token's own
// scope may be higher than the effective one, and its prefix older than both.
func (h *handlers) GetMe(w http.ResponseWriter, r *http.Request) {
	if h.b.Auth == nil || h.b.Store == nil {
		notImplemented(w, "identities")
		return
	}
	p, _ := PrincipalFrom(r.Context())
	i, err := h.b.Store.GetIdentity(r.Context(), p.UserID)
	if errors.Is(err, store.ErrNotFound) {
		// Deleted since this request authenticated: the token is gone with it.
		unauthorized(w)
		return
	}
	if err != nil {
		h.b.fail(w, "get me", err)
		return
	}
	out := gen.Me{Binding: event.ChannelHolaclaw + ":" + p.Binding, Scope: gen.MeScope(p.Scope)}
	out.Identity.Id, out.Identity.Name, out.Identity.Role = string(i.ID), i.Name, gen.MeIdentityRole(p.Role)
	if p.TokenID != "" {
		t, err := h.b.Auth.GetToken(r.Context(), p.TokenID)
		if errors.Is(err, store.ErrNotFound) {
			unauthorized(w)
			return
		}
		if err != nil {
			h.b.fail(w, "get me", err)
			return
		}
		out.Token = ptr(toGenToken(t))
	}
	writeJSON(w, http.StatusOK, out)
}

// ListDevices lists the holaclaw bindings of the caller's own identity.
func (h *handlers) ListDevices(w http.ResponseWriter, r *http.Request) {
	if h.b.Auth == nil {
		notImplemented(w, "devices")
		return
	}
	p, _ := PrincipalFrom(r.Context())
	devices, err := h.b.Auth.Devices(r.Context(), p)
	if err != nil {
		h.b.fail(w, "list devices", err)
		return
	}
	out := make([]gen.Device, 0, len(devices))
	for _, d := range devices {
		out = append(out, gen.Device{
			Binding:    event.ChannelHolaclaw + ":" + d.Alias,
			Display:    optString(d.Display),
			Platform:   optString(d.Platform),
			Tokens:     d.Tokens,
			Current:    d.Alias == p.Binding,
			CreatedAt:  d.CreatedAt,
			LastUsedAt: d.LastUsedAt,
		})
	}
	writeJSON(w, http.StatusOK, struct {
		Devices []gen.Device `json:"devices"`
	}{out})
}

// CreateDeviceLink mints a pairing link onto the caller's own identity, at
// most at the caller's own scope. The code is shown once, here.
func (h *handlers) CreateDeviceLink(w http.ResponseWriter, r *http.Request) {
	if h.b.Auth == nil {
		notImplemented(w, "devices")
		return
	}
	// The body is optional: none at all, chunked or not, is the defaults.
	var in gen.DeviceLinkCreate
	body, err := io.ReadAll(r.Body)
	switch {
	case tooLarge(err):
		writeError(w, http.StatusRequestEntityTooLarge, CodeTooLarge, "payload too large")
		return
	case err != nil:
		badRequest(w, "invalid JSON body")
		return
	case len(bytes.TrimSpace(body)) > 0:
		dec := json.NewDecoder(bytes.NewReader(body))
		dec.DisallowUnknownFields()
		if dec.Decode(&in) != nil {
			badRequest(w, "invalid JSON body")
			return
		}
	}
	var scope auth.Scope
	if in.Scope != nil {
		scope = auth.Scope(*in.Scope)
	}
	p, _ := PrincipalFrom(r.Context())
	code, inv, err := h.b.Auth.InviteDevice(r.Context(), p, scope)
	var bad *auth.InviteSpecError
	switch {
	case errors.Is(err, auth.ErrInvalidScope):
		badRequest(w, "invalid scope")
	case errors.Is(err, auth.ErrScopeAboveCaller):
		writeError(w, http.StatusForbidden, CodeForbidden, "a pairing link never carries more than the caller's own scope")
	case errors.Is(err, auth.ErrPending):
		writeError(w, http.StatusConflict, CodeConflict, "the identity is pending; an admin confirms it first")
	case errors.As(err, &bad):
		badRequest(w, "invalid invite spec: "+bad.Reason)
	case err != nil:
		h.b.fail(w, "create device link", err)
	default:
		writeJSON(w, http.StatusCreated, h.invite(r, code, inv))
	}
}

// UnbindDevice removes one holaclaw binding of the caller's own identity and
// revokes its tokens. Anyone else's binding is a 404, as if it did not exist.
func (h *handlers) UnbindDevice(w http.ResponseWriter, r *http.Request, binding string) {
	if h.b.Auth == nil {
		notImplemented(w, "devices")
		return
	}
	alias, ok := holaclawAlias(binding)
	if !ok {
		badRequest(w, "binding must be holaclaw:<alias>")
		return
	}
	p, _ := PrincipalFrom(r.Context())
	u, err := h.b.Auth.UnbindDevice(r.Context(), p, alias)
	switch {
	case errors.Is(err, auth.ErrSocketBinding):
		badRequest(w, "holaclaw:cli is the unix socket's binding and stays")
	case errors.Is(err, store.ErrNotFound):
		notFound(w)
	case err != nil:
		h.b.fail(w, "unbind device", err)
	default:
		writeJSON(w, http.StatusOK, gen.BindingUnbound{
			Identity: string(u.IdentityID), PendingDeleted: u.PendingDeleted, MemoryForgotten: u.Memory, TokensRevoked: u.Tokens,
		})
	}
}
