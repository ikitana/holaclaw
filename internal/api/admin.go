package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"holaclaw/internal/api/gen"
	"holaclaw/internal/approval"
	"holaclaw/internal/auth"
	"holaclaw/internal/auth/secrets"
	"holaclaw/internal/channel"
	"holaclaw/internal/channel/telegram"
	"holaclaw/internal/config"
	"holaclaw/internal/event"
	"holaclaw/internal/store"
	"holaclaw/internal/tool"
)

// --- approvals ----------------------------------------------------------------

// ListApprovals returns everything still waiting for a human (docs/security.md).
func (h *handlers) ListApprovals(w http.ResponseWriter, r *http.Request) {
	if h.b.Approvals == nil {
		notImplemented(w, "approvals")
		return
	}
	pending, err := h.b.Approvals.ListPending(r.Context())
	if err != nil {
		h.b.fail(w, "list approvals", err)
		return
	}
	out := make([]gen.Approval, 0, len(pending))
	for _, a := range pending {
		out = append(out, toGenApproval(a))
	}
	writeJSON(w, http.StatusOK, struct {
		Approvals []gen.Approval `json:"approvals"`
	}{out})
}

// ResolveApproval records the human decision. The first decision stands: a
// second attempt is a conflict, never a silent overwrite.
func (h *handlers) ResolveApproval(w http.ResponseWriter, r *http.Request, id gen.Id) {
	if h.b.Approvals == nil {
		notImplemented(w, "approvals")
		return
	}
	var in gen.ApprovalResolve
	if !decodeJSON(w, r, &in) {
		return
	}
	choice := tool.GrantOnce
	if in.Choice != nil {
		choice = tool.GrantScope(*in.Choice)
	}
	p, _ := PrincipalFrom(r.Context())
	a, err := h.b.Approvals.Resolve(r.Context(), event.ID(id), in.Approve, choice, p.UserID)
	switch {
	case errors.Is(err, approval.ErrNotPending):
		writeError(w, http.StatusConflict, CodeConflict, "approval is already resolved")
		return
	case errors.Is(err, approval.ErrNotFound):
		notFound(w)
		return
	case err != nil:
		h.b.fail(w, "resolve approval", err)
		return
	}
	writeJSON(w, http.StatusOK, toGenApproval(a))
}

func toGenApproval(a tool.Approval) gen.Approval {
	return gen.Approval{
		Id:        string(a.ID),
		Status:    gen.ApprovalStatus(a.Status),
		TraceId:   optID(a.TraceID),
		SessionId: optID(a.SessionID),
		Agent:     optString(a.Agent),
		Tool:      optString(a.Tool),
		Summary:   optString(a.Summary),
		CreatedAt: a.CreatedAt,
		ExpiresAt: a.ExpiresAt,
	}
}

// --- identities, invites, tokens ---------------------------------------------

// ListIdentities is the users with their channel IDs; pending keeps only the
// guests nobody has confirmed.
func (h *handlers) ListIdentities(w http.ResponseWriter, r *http.Request, params gen.ListIdentitiesParams) {
	if h.b.Auth == nil {
		notImplemented(w, "identities")
		return
	}
	identities, err := h.b.Auth.ListIdentities(r.Context(), params.Pending != nil && *params.Pending)
	if err != nil {
		h.b.fail(w, "list identities", err)
		return
	}
	out := make([]gen.User, 0, len(identities))
	for _, i := range identities {
		out = append(out, h.user(r, i))
	}
	writeJSON(w, http.StatusOK, struct {
		Identities []gen.User `json:"identities"`
	}{out})
}

func (h *handlers) GetIdentity(w http.ResponseWriter, r *http.Request, id gen.Id) {
	if h.b.Auth == nil {
		notImplemented(w, "identities")
		return
	}
	i, err := h.b.Store.GetIdentity(r.Context(), event.ID(id))
	if err != nil {
		h.b.fail(w, "get identity", err)
		return
	}
	writeJSON(w, http.StatusOK, h.user(r, i))
}

// UpdateIdentity confirms a pending (guest) identity, or renames one or
// changes its role. The IDs never move here.
func (h *handlers) UpdateIdentity(w http.ResponseWriter, r *http.Request, id gen.Id) {
	if h.b.Auth == nil {
		notImplemented(w, "identities")
		return
	}
	var in gen.IdentityUpdate
	if !decodeJSON(w, r, &in) {
		return
	}
	if in.Role != nil && !in.Role.Valid() {
		badRequest(w, "invalid role")
		return
	}
	i, err := h.b.Store.GetIdentity(r.Context(), event.ID(id))
	if err != nil {
		h.b.fail(w, "get identity", err)
		return
	}
	var role *store.Role
	if in.Role != nil {
		role = ptr(store.Role(*in.Role))
	}
	if i.Pending() {
		if in.Name == nil {
			badRequest(w, "name is required to confirm a pending identity")
			return
		}
		p, _ := PrincipalFrom(r.Context())
		var confirm store.Role
		if role != nil {
			confirm = *role
		}
		i, err = h.b.Auth.Confirm(r.Context(), i.ID, *in.Name, confirm, p.UserID)
	} else {
		p, _ := PrincipalFrom(r.Context())
		i, err = h.b.Auth.Update(r.Context(), i.ID, in.Name, role, h.b.Auth.Actor(r.Context(), p))
	}
	switch {
	case errors.Is(err, auth.ErrInvalidRole):
		badRequest(w, "invalid role")
	case errors.Is(err, auth.ErrInvalidIdentity):
		badRequest(w, "name must not be empty, and something must change")
	case errors.Is(err, auth.ErrNotPending):
		writeError(w, http.StatusConflict, CodeConflict, "the identity is not pending")
	case errors.Is(err, auth.ErrPending):
		writeError(w, http.StatusConflict, CodeConflict, "the identity is pending; confirm it first")
	case errors.Is(err, auth.ErrOwnerRole):
		writeError(w, http.StatusConflict, CodeConflict, "the owner's role cannot change")
	case err != nil:
		h.b.fail(w, "update identity", err)
	default:
		writeJSON(w, http.StatusOK, h.user(r, i))
	}
}

// CreateIdentity makes a confirmed identity with no bindings.
func (h *handlers) CreateIdentity(w http.ResponseWriter, r *http.Request) {
	if h.b.Auth == nil {
		notImplemented(w, "identities")
		return
	}
	var in gen.IdentityCreate
	if !decodeJSON(w, r, &in) {
		return
	}
	if strings.TrimSpace(in.Name) == "" {
		badRequest(w, "name is required")
		return
	}
	var role store.Role
	if in.Role != nil {
		role = store.Role(*in.Role)
	}
	p, _ := PrincipalFrom(r.Context())
	i, err := h.b.Auth.CreateIdentity(r.Context(), in.Name, role, h.b.Auth.Actor(r.Context(), p))
	switch {
	case errors.Is(err, auth.ErrInvalidRole):
		badRequest(w, "invalid role")
	case errors.Is(err, auth.ErrInvalidIdentity):
		badRequest(w, "name must not be empty")
	case err != nil:
		h.b.fail(w, "create identity", err)
	default:
		writeJSON(w, http.StatusCreated, h.user(r, i))
	}
}

// AssignIdentity moves a pending identity's bindings, DM memory and sessions
// onto a confirmed one and deletes the pending identity (docs/identity.md §2).
func (h *handlers) AssignIdentity(w http.ResponseWriter, r *http.Request, id gen.Id) {
	if h.b.Auth == nil {
		notImplemented(w, "identities")
		return
	}
	var in gen.IdentityAssign
	if !decodeJSON(w, r, &in) {
		return
	}
	if in.To == "" {
		badRequest(w, "to is required")
		return
	}
	p, _ := PrincipalFrom(r.Context())
	a, err := h.b.Auth.Assign(r.Context(), event.ID(id), event.ID(in.To), p)
	switch {
	case errors.Is(err, auth.ErrAssignSelf):
		badRequest(w, "an identity cannot be assigned to itself")
		return
	case errors.Is(err, auth.ErrNotPending):
		writeError(w, http.StatusConflict, CodeConflict, "only a pending identity can be assigned")
		return
	case errors.Is(err, auth.ErrPending):
		writeError(w, http.StatusConflict, CodeConflict, "the target is pending; confirm it first")
		return
	case errors.Is(err, auth.ErrDisabled):
		writeError(w, http.StatusConflict, CodeConflict, "the target is disabled; enable it first")
		return
	case errors.Is(err, auth.ErrOwnerOnly):
		writeError(w, http.StatusForbidden, CodeForbidden, "only the owner can assign to the owner")
		return
	case errors.Is(err, auth.ErrRoomIdentity):
		badRequest(w, "a room identity cannot be assigned")
		return
	case err != nil:
		h.b.fail(w, "assign identity", err)
		return
	}
	i, err := h.b.Store.GetIdentity(r.Context(), a.To)
	if err != nil {
		h.b.fail(w, "get identity", err)
		return
	}
	writeJSON(w, http.StatusOK, gen.IdentityAssignment{
		Identity: h.user(r, i), Bindings: a.IDs, Memory: a.Memory, Dropped: a.Dropped, Sessions: a.Sessions,
	})
}

// UnbindBinding removes one `<channel>:<alias>` binding, wherever it is held.
func (h *handlers) UnbindBinding(w http.ResponseWriter, r *http.Request, ref string) {
	if h.b.Auth == nil {
		notImplemented(w, "identities")
		return
	}
	channel, alias, ok := strings.Cut(ref, ":")
	if !ok || channel == "" || alias == "" {
		badRequest(w, "the binding must be <channel>:<alias>")
		return
	}
	p, _ := PrincipalFrom(r.Context())
	u, err := h.b.Auth.Unbind(r.Context(), channel, alias, p)
	switch {
	case errors.Is(err, auth.ErrOwnerOnly):
		writeError(w, http.StatusForbidden, CodeForbidden, "only the owner can unbind the owner")
	case errors.Is(err, auth.ErrRoomIdentity):
		badRequest(w, "a room keeps its one binding")
	case errors.Is(err, auth.ErrSocketBinding):
		badRequest(w, "holaclaw:cli is the unix socket's binding and stays")
	case err != nil:
		h.b.fail(w, "unbind binding", err)
	default:
		writeJSON(w, http.StatusOK, gen.BindingUnbound{
			Identity: string(u.IdentityID), PendingDeleted: u.PendingDeleted, MemoryForgotten: u.Memory, TokensRevoked: u.Tokens,
		})
	}
}

// DisableIdentity switches a confirmed identity or a room off.
func (h *handlers) DisableIdentity(w http.ResponseWriter, r *http.Request, id gen.Id) {
	h.switchIdentity(w, r, id, func(ctx context.Context, id, by event.ID) (store.Identity, error) {
		return h.b.Auth.Disable(ctx, id, by)
	}, "disable identity")
}

// EnableIdentity switches a disabled identity back on.
func (h *handlers) EnableIdentity(w http.ResponseWriter, r *http.Request, id gen.Id) {
	h.switchIdentity(w, r, id, func(ctx context.Context, id, by event.ID) (store.Identity, error) {
		return h.b.Auth.Enable(ctx, id, by)
	}, "enable identity")
}

func (h *handlers) switchIdentity(w http.ResponseWriter, r *http.Request, id gen.Id,
	do func(ctx context.Context, id, by event.ID) (store.Identity, error), what string) {
	if h.b.Auth == nil {
		notImplemented(w, "identities")
		return
	}
	p, _ := PrincipalFrom(r.Context())
	i, err := do(r.Context(), event.ID(id), h.b.Auth.Actor(r.Context(), p))
	switch {
	case errors.Is(err, auth.ErrOwnerProtected):
		writeError(w, http.StatusConflict, CodeConflict, "the owner cannot be disabled")
	case errors.Is(err, auth.ErrPending):
		writeError(w, http.StatusConflict, CodeConflict, "a pending user is not disabled; delete or unbind it instead")
	case err != nil:
		h.b.fail(w, what, err)
	default:
		writeJSON(w, http.StatusOK, h.user(r, i))
	}
}

// DeleteIdentity removes an identity: its tasks are cancelled, its bindings,
// tokens and invites go, and its sessions stay with no user. Unless
// keep_memory, its own memory is forgotten first. The owner is refused.
func (h *handlers) DeleteIdentity(w http.ResponseWriter, r *http.Request, id gen.Id, params gen.DeleteIdentityParams) {
	if h.b.Auth == nil {
		notImplemented(w, "identities")
		return
	}
	keep := params.KeepMemory != nil && *params.KeepMemory
	p, _ := PrincipalFrom(r.Context())
	deleted, err := h.b.Auth.DeleteIdentity(r.Context(), event.ID(id), keep, h.b.Auth.Actor(r.Context(), p))
	switch {
	case errors.Is(err, auth.ErrOwner):
		writeError(w, http.StatusConflict, CodeConflict, "the owner cannot be deleted")
		return
	case err != nil:
		h.b.fail(w, "delete identity", err)
		return
	}
	aliases := make([]string, 0, len(deleted.Aliases))
	for _, i := range deleted.Aliases {
		aliases = append(aliases, i.Channel+":"+i.Alias)
	}
	out := gen.IdentityDeleted{
		Id:              string(deleted.User.ID),
		Aliases:         aliases,
		TokensRevoked:   deleted.TokensRevoked,
		MemoryForgotten: int(deleted.Forgotten),
		MemoryKept:      keep,
	}
	if role := wireRole(deleted.User); role != "" {
		out.Role = ptr(gen.IdentityDeletedRole(role))
	}
	writeJSON(w, http.StatusOK, out)
}

// wireRole is the role the API shows for an identity, "" for none. The wire
// format has no pending user role: a pending user is a guest. A room has no
// role, and a disabled user keeps its own.
func wireRole(i store.Identity) store.Role {
	switch {
	case i.Kind == store.KindRoom:
		return ""
	case i.Pending():
		return store.RoleGuest
	}
	return i.Role
}

func (h *handlers) user(r *http.Request, ident store.Identity) gen.User {
	out := gen.User{
		Id:         string(ident.ID),
		Kind:       gen.UserKind(ident.Kind),
		Name:       ident.Name,
		Status:     gen.UserStatus(ident.Status),
		ApprovedAt: ident.ApprovedAt,
		CreatedAt:  ident.CreatedAt,
	}
	if ident.ApprovedBy != "" {
		out.ApprovedBy = ptr(string(ident.ApprovedBy))
	}
	if role := wireRole(ident); role != "" {
		out.Role = ptr(gen.UserRole(role))
	}
	if h.b.Auth == nil {
		return out
	}
	ids, err := h.b.Auth.Bindings(r.Context(), ident.ID)
	if err != nil {
		h.b.logger().Error("api: list identities", "user", ident.ID, "error", err)
		return out
	}
	if len(ids) == 0 {
		return out
	}
	tokens, err := h.b.Auth.ActiveTokens(r.Context(), ident.ID)
	if err != nil {
		h.b.logger().Error("api: list identities", "user", ident.ID, "error", err)
		return out
	}
	type binding = struct {
		Alias     string    `json:"alias"`
		Channel   string    `json:"channel"`
		CreatedAt time.Time `json:"created_at"`
		Display   *string   `json:"display,omitempty"`
		Tokens    *int      `json:"tokens,omitempty"`
	}
	list := make([]binding, 0, len(ids))
	for _, b := range ids {
		item := binding{Alias: b.Alias, Channel: b.Channel, CreatedAt: b.CreatedAt, Display: optString(b.Display)}
		if b.Channel == event.ChannelHolaclaw {
			item.Tokens = ptr(tokens[b.Alias])
		}
		list = append(list, item)
	}
	out.Identities = &list
	return out
}

// CreateInvite mints a one-shot code. It is shown once, here.
func (h *handlers) CreateInvite(w http.ResponseWriter, r *http.Request) {
	if h.b.Auth == nil {
		notImplemented(w, "invites")
		return
	}
	var in gen.InviteCreate
	if !decodeJSON(w, r, &in) {
		return
	}
	p, _ := PrincipalFrom(r.Context())
	spec := auth.InviteSpec{Channel: in.Channel, CreatedBy: p.UserID, CreatorRole: p.Role}
	if in.UserId != nil {
		spec.UserID = event.ID(*in.UserId)
	}
	if in.TtlSeconds != nil {
		// Checked before multiplying: a huge count would wrap the Duration.
		if s := *in.TtlSeconds; s < 60 || s > int(auth.MaxFirstInviteTTL.Seconds()) {
			badRequest(w, fmt.Sprintf("ttl_seconds must be between 60 and %d", int(auth.MaxFirstInviteTTL.Seconds())))
			return
		}
		spec.TTL = time.Duration(*in.TtlSeconds) * time.Second
	}
	if in.Kind != nil {
		if !in.Kind.Valid() {
			badRequest(w, "invalid kind")
			return
		}
		spec.Kind = auth.InviteKind(*in.Kind)
	}
	if in.Scope != nil {
		if !in.Scope.Valid() {
			badRequest(w, "invalid scope")
			return
		}
		spec.Scope = auth.Scope(*in.Scope)
	}
	switch {
	case spec.Channel == "":
		badRequest(w, "channel is required")
		return
	case spec.Kind == auth.InviteRoom:
		if msg := h.roomInviteRefusal(spec.Channel); msg != "" {
			badRequest(w, msg)
			return
		}
	case spec.Channel != event.ChannelHolaclaw && !h.knownChannel(spec.Channel):
		// A code bound to a typo would never redeem, and nobody would know why.
		badRequest(w, fmt.Sprintf("unknown channel %q", spec.Channel))
		return
	}
	code, inv, err := h.b.Auth.CreateInvite(r.Context(), spec)
	if err != nil {
		var bad *auth.InviteSpecError
		switch {
		case errors.Is(err, auth.ErrInvalidScope), errors.Is(err, auth.ErrPending):
			badRequest(w, strings.TrimPrefix(err.Error(), "auth: "))
		case errors.Is(err, auth.ErrOwnerOnly):
			writeError(w, http.StatusForbidden, CodeForbidden, "only the owner can invite the owner")
		case errors.Is(err, auth.ErrRoomIdentity):
			badRequest(w, "user_id names a room identity")
		case errors.As(err, &bad):
			badRequest(w, "invalid invite spec: "+bad.Reason)
		default:
			h.b.fail(w, "create invite", err)
		}
		return
	}
	writeJSON(w, http.StatusCreated, h.invite(r, code, inv))
}

// invite renders a new invite with its code, and its link or, when it has
// none, how to redeem it.
func (h *handlers) invite(r *http.Request, code string, inv auth.Invite) gen.Invite {
	out := gen.Invite{Id: string(inv.ID), Code: code, Channel: inv.Channel, ExpiresAt: inv.ExpiresAt}
	if inv.UserID != "" {
		out.UserId = ptr(string(inv.UserID))
	}
	if inv.Scope != "" {
		out.Scope = ptr(gen.InviteScope(inv.Scope))
	}
	if link := h.inviteLink(r, inv.Channel, inv.Kind, code); link != "" {
		out.Link = &link
	} else {
		out.Instruction = ptr(h.inviteInstruction(inv.Channel, inv.Kind))
	}
	return out
}

// roomInviteRefusal says why a channel instance cannot take a room invite,
// or "" when it can. Only telegram, googlechat and whatsapp redeem one for
// now, and only a channel whose rooms knob waits for approval has a use for
// it: with rooms off the code would approve a room nobody may talk in, with
// rooms open there is nothing to approve.
func (h *handlers) roomInviteRefusal(name string) string {
	var ch config.Channel
	ok := false
	if h.b.Config != nil {
		ch, ok = h.b.Config.Channels[name]
	}
	switch {
	case name == event.ChannelHolaclaw:
		return fmt.Sprintf("channel %q has no rooms", name)
	case !ok:
		return fmt.Sprintf("unknown channel %q", name)
	case ch.Type != "telegram" && ch.Type != "googlechat" && ch.Type != "whatsapp":
		return fmt.Sprintf("channel %q (%s) does not support room invites", name, ch.Type)
	case ch.Access.Rooms != "approved":
		return fmt.Sprintf("channel %q has rooms = %q; room invites need rooms = \"approved\"", name, ch.Access.Rooms)
	}
	return ""
}

// knownChannel reports whether name is a configured channel instance.
func (h *handlers) knownChannel(name string) bool {
	if h.b.Config == nil {
		return false
	}
	_, ok := h.b.Config.Channels[name]
	return ok
}

// inviteLink is the deep link of the channel an invite is bound to, when that
// adapter has one: telegram's start and startgroup links, whatsapp's wa.me.
// The bot identity it is built from comes from the platform (getMe for
// telegram) and reaches us as the instance status detail, so an instance that
// is not connected yet has nothing to link to and the invite goes out with an
// instruction instead. A holaclaw invite links to the app, with the address
// the daemon is reached at, when it knows one (serverURL).
func (h *handlers) inviteLink(r *http.Request, channelName string, kind auth.InviteKind, code string) string {
	if channelName == event.ChannelHolaclaw {
		if server := h.b.serverURL(r); server != "" {
			return PairLink(server, code)
		}
		return ""
	}
	if h.b.Channels == nil {
		return ""
	}
	for _, ch := range h.b.Channels() {
		if ch.Name != channelName {
			continue
		}
		if ch.State != gen.Connected || ch.Detail == nil {
			return ""
		}
		switch {
		case ch.Type == "whatsapp" && kind == auth.InviteDM:
			return whatsappInviteLink(*ch.Detail, code)
		case ch.Type != "telegram":
			return ""
		case kind == auth.InviteRoom:
			return telegram.GroupInviteLink(*ch.Detail, code)
		}
		return telegram.InviteLink(*ch.Detail, code)
	}
	return ""
}

// inviteInstruction is the one line that tells how to redeem a code that has
// no link.
func (h *handlers) inviteInstruction(channelName string, kind auth.InviteKind) string {
	var typ string
	if h.b.Config != nil {
		typ = h.b.Config.Channels[channelName].Type
	}
	switch {
	case channelName == event.ChannelHolaclaw:
		return "Enter this code in the app to pair a device; set server.public_url for a link."
	case kind == auth.InviteDM:
		return "Send this code to the bot in a direct message."
	case typ == "googlechat":
		return "Mention the app with this code in the space."
	case typ == "whatsapp":
		return "Add the bot's number to the group and send @bot with this code there."
	}
	return "Add the bot to the group and send this code there."
}

// whatsappInviteLink opens a chat with the bot's number with the code already
// typed (docs/channels/whatsapp.md §3). number is the connected instance's
// status detail, "+34600000000"; anything else has no link. A group has no
// wa.me equivalent: someone adds the number and sends the code there.
func whatsappInviteLink(number, code string) string {
	digits := strings.TrimPrefix(number, "+")
	if code == "" || len(digits) < 7 || len(digits) > 15 || strings.Trim(digits, "0123456789") != "" {
		return ""
	}
	return "https://wa.me/" + digits + "?text=" + url.QueryEscape(code)
}

// ListTokens lists the active tokens, or every one with all, narrowed to an
// identity or one holaclaw binding.
func (h *handlers) ListTokens(w http.ResponseWriter, r *http.Request, params gen.ListTokensParams) {
	if h.b.Auth == nil {
		notImplemented(w, "tokens")
		return
	}
	var f auth.TokenFilter
	if params.Identity != nil {
		f.IdentityID = event.ID(*params.Identity)
	}
	if params.Binding != nil {
		alias, ok := holaclawAlias(*params.Binding)
		if !ok {
			badRequest(w, "binding must be holaclaw:<alias>")
			return
		}
		f.Binding = alias
	}
	f.All = params.All != nil && *params.All
	tokens, err := h.b.Auth.ListTokens(r.Context(), f)
	if err != nil {
		h.b.fail(w, "list tokens", err)
		return
	}
	out := make([]gen.Token, 0, len(tokens))
	for _, t := range tokens {
		out = append(out, toGenToken(t))
	}
	writeJSON(w, http.StatusOK, struct {
		Tokens []gen.Token `json:"tokens"`
	}{out})
}

// holaclawAlias reads the alias out of a holaclaw:<alias> ref.
func holaclawAlias(ref string) (string, bool) {
	channel, alias, ok := strings.Cut(ref, ":")
	return alias, ok && channel == event.ChannelHolaclaw && alias != ""
}

// CreateToken mints an API token. The secret exists exactly once, in this
// response; the database keeps only its hash.
//
// A token gets a new holaclaw binding on the user it names, or joins the
// binding it names, and acts as that binding's identity: nothing defaults to
// the owner, so a request naming neither is refused (docs/identity.md §3).
func (h *handlers) CreateToken(w http.ResponseWriter, r *http.Request) {
	if h.b.Auth == nil {
		notImplemented(w, "tokens")
		return
	}
	var in gen.TokenCreate
	if !decodeJSON(w, r, &in) {
		return
	}
	var scope auth.Scope
	if in.Scope != nil {
		scope = auth.Scope(*in.Scope)
	}
	// A token acts with its identity's role: MintToken and AddToken refuse one
	// for the owner unless the caller is the owner.
	p, _ := PrincipalFrom(r.Context())
	var (
		secret string
		t      auth.Token
		err    error
	)
	switch {
	case (in.UserId == nil) == (in.Binding == nil):
		badRequest(w, "exactly one of user_id and binding is required: the token acts as that identity")
		return
	case in.Binding != nil:
		alias, ok := holaclawAlias(*in.Binding)
		if !ok {
			badRequest(w, "binding must be holaclaw:<alias>")
			return
		}
		secret, t, err = h.b.Auth.AddToken(r.Context(), p, alias, in.Name, scope)
		if errors.Is(err, store.ErrNotFound) {
			badRequest(w, "binding must name an existing holaclaw binding")
			return
		}
	default:
		secret, t, err = h.b.Auth.MintToken(r.Context(), p, in.Name, scope, event.ID(*in.UserId))
		if errors.Is(err, store.ErrNotFound) {
			badRequest(w, "user_id must name an existing user")
			return
		}
	}
	switch {
	case errors.Is(err, auth.ErrInvalidIdentity):
		badRequest(w, "user_id is required: the token acts as that user")
	case errors.Is(err, auth.ErrSocketBinding):
		badRequest(w, "holaclaw:cli is the unix socket and holds no tokens")
	case err != nil:
		h.tokenFail(w, "create token", err)
	default:
		writeJSON(w, http.StatusCreated, gen.TokenCreated{Token: toGenToken(t), Secret: secret})
	}
}

// UpdateToken raises or lowers a token's scope.
func (h *handlers) UpdateToken(w http.ResponseWriter, r *http.Request, id gen.Id) {
	if h.b.Auth == nil {
		notImplemented(w, "tokens")
		return
	}
	var in gen.TokenUpdate
	if !decodeJSON(w, r, &in) {
		return
	}
	p, _ := PrincipalFrom(r.Context())
	t, err := h.b.Auth.SetTokenScope(r.Context(), p, event.ID(id), auth.Scope(in.Scope))
	if err != nil {
		h.tokenFail(w, "update token", err)
		return
	}
	writeJSON(w, http.StatusOK, toGenToken(t))
}

// RotateToken replaces any token; the new secret is shown once, here.
func (h *handlers) RotateToken(w http.ResponseWriter, r *http.Request, id gen.Id) {
	if h.b.Auth == nil {
		notImplemented(w, "tokens")
		return
	}
	p, _ := PrincipalFrom(r.Context())
	secret, t, err := h.b.Auth.RotateToken(r.Context(), p, event.ID(id))
	if err != nil {
		h.tokenFail(w, "rotate token", err)
		return
	}
	writeJSON(w, http.StatusCreated, gen.TokenCreated{Token: toGenToken(t), Secret: secret})
}

// RotateOwnToken replaces the token that signs the request, for a client that
// rotates its own on a schedule.
func (h *handlers) RotateOwnToken(w http.ResponseWriter, r *http.Request) {
	if h.b.Auth == nil {
		notImplemented(w, "tokens")
		return
	}
	p, _ := PrincipalFrom(r.Context())
	secret, t, err := h.b.Auth.RotateOwnToken(r.Context(), p)
	if err != nil {
		h.tokenFail(w, "rotate token", err)
		return
	}
	writeJSON(w, http.StatusCreated, gen.TokenCreated{Token: toGenToken(t), Secret: secret})
}

func (h *handlers) RevokeToken(w http.ResponseWriter, r *http.Request, id gen.Id) {
	if h.b.Auth == nil {
		notImplemented(w, "tokens")
		return
	}
	p, _ := PrincipalFrom(r.Context())
	if err := h.b.Auth.RevokeToken(r.Context(), p, event.ID(id)); err != nil {
		h.tokenFail(w, "revoke token", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// tokenFail renders the refusals the token calls share.
func (h *handlers) tokenFail(w http.ResponseWriter, what string, err error) {
	var rotated *auth.RotatedError
	switch {
	case errors.As(err, &rotated):
		writeError(w, http.StatusConflict, CodeConflict,
			"the token was rotated into "+string(rotated.Into)+", which still works; revoke that one")
	case errors.Is(err, auth.ErrInvalidScope):
		badRequest(w, "invalid scope")
	case errors.Is(err, auth.ErrScopeAboveRole):
		badRequest(w, "admin scope needs the identity's role to be owner or admin")
	case errors.Is(err, auth.ErrRoomIdentity):
		badRequest(w, "a room identity holds no tokens")
	case errors.Is(err, auth.ErrNoToken):
		badRequest(w, "the caller has no token to rotate")
	case errors.Is(err, auth.ErrOwnerOnly):
		writeError(w, http.StatusForbidden, CodeForbidden, "only the owner can do this to the owner's tokens")
	case errors.Is(err, auth.ErrDisabled):
		writeError(w, http.StatusConflict, CodeConflict, "the identity is disabled; enable it first")
	case errors.Is(err, auth.ErrPending):
		writeError(w, http.StatusConflict, CodeConflict, "the identity is pending; confirm it first")
	case errors.Is(err, auth.ErrRevoked):
		writeError(w, http.StatusConflict, CodeConflict, "the token is revoked")
	default:
		h.b.fail(w, what, err)
	}
}

func toGenToken(t auth.Token) gen.Token {
	return gen.Token{
		Id:         string(t.ID),
		Name:       t.Name,
		Scope:      gen.TokenScope(t.Scope),
		Binding:    event.ChannelHolaclaw + ":" + t.Binding,
		UserId:     optID(t.IdentityID),
		Origin:     gen.TokenOrigin(t.Origin),
		OriginRef:  optID(t.OriginRef),
		CreatedBy:  optID(t.CreatedBy),
		CreatedAt:  t.CreatedAt,
		LastUsedAt: t.LastUsedAt,
		RevokedAt:  t.RevokedAt,
		RevokedBy:  optID(t.RevokedBy),
	}
}

// --- tasks --------------------------------------------------------------------

func (h *handlers) ListTasks(w http.ResponseWriter, r *http.Request) {
	if h.b.Tasks == nil {
		notImplemented(w, "tasks")
		return
	}
	tasks, err := h.b.Tasks.ListTasks(r.Context())
	if err != nil {
		h.b.fail(w, "list tasks", err)
		return
	}
	if tasks == nil {
		tasks = []gen.Task{}
	}
	writeJSON(w, http.StatusOK, struct {
		Tasks []gen.Task `json:"tasks"`
	}{tasks})
}

func (h *handlers) GetTask(w http.ResponseWriter, r *http.Request, id gen.Id) {
	if h.b.Tasks == nil {
		notImplemented(w, "tasks")
		return
	}
	t, err := h.b.Tasks.GetTask(r.Context(), id)
	if err != nil {
		h.b.fail(w, "get task", err)
		return
	}
	writeJSON(w, http.StatusOK, t)
}

func (h *handlers) CreateTask(w http.ResponseWriter, r *http.Request) {
	if h.b.Tasks == nil {
		notImplemented(w, "tasks")
		return
	}
	var in gen.TaskCreate
	if !decodeJSON(w, r, &in) {
		return
	}
	p, _ := PrincipalFrom(r.Context())
	t, err := h.b.Tasks.CreateTask(r.Context(), in, p.UserID)
	if err != nil {
		h.b.fail(w, "create task", err)
		return
	}
	writeJSON(w, http.StatusCreated, t)
}

func (h *handlers) DeleteTask(w http.ResponseWriter, r *http.Request, id gen.Id) {
	if h.b.Tasks == nil {
		notImplemented(w, "tasks")
		return
	}
	if err := h.b.Tasks.DeleteTask(r.Context(), id); err != nil {
		h.b.fail(w, "delete task", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- channels -----------------------------------------------------------------

func (h *handlers) ListChannels(w http.ResponseWriter, _ *http.Request) {
	out := []gen.ChannelStatus{}
	if h.b.Channels != nil {
		if list := h.b.Channels(); list != nil {
			out = list
		}
	}
	writeJSON(w, http.StatusOK, struct {
		Channels []gen.ChannelStatus `json:"channels"`
	}{out})
}

// LoginChannel starts a pairing-code login and answers once the code is
// there (docs/channels/whatsapp.md §5.4). The attempt is detached from the
// request: a client that hangs up does not fail it, which would register a
// fresh device on the next try; the adapter bounds it, and the client picks
// it up again with GET. The code goes back in this answer and the GET only:
// nothing here logs it.
func (h *handlers) LoginChannel(w http.ResponseWriter, r *http.Request, name string) {
	l := h.linker(w, name)
	if l == nil {
		return
	}
	var in gen.ChannelLoginStart
	if !decodeJSON(w, r, &in) {
		return
	}
	st, err := l.Link(context.WithoutCancel(r.Context()), in.Phone)
	if err != nil {
		// A malformed or unknown number is a bad request; a state the
		// instance was already in (already linked, a link already pending,
		// not running) a conflict; any other failure (the socket, a pairing
		// error) is WhatsApp's side. The message never holds the code.
		switch {
		case errors.Is(err, channel.ErrBadPhone):
			badRequest(w, err.Error())
		case errors.Is(err, channel.ErrLinkConflict):
			writeError(w, http.StatusConflict, CodeConflict, err.Error())
		default:
			writeError(w, http.StatusBadGateway, CodeInternal, err.Error())
		}
		return
	}
	writeJSON(w, http.StatusOK, deviceLogin(st))
}

// GetChannelLogin polls the current or last login of a channel.
func (h *handlers) GetChannelLogin(w http.ResponseWriter, _ *http.Request, name string) {
	if l := h.linker(w, name); l != nil {
		writeJSON(w, http.StatusOK, deviceLogin(l.LinkState()))
	}
}

// LogoutChannel unlinks a channel from its account. An instance that is not
// linked, or not running, is a conflict; any other failure (never one that
// could hold a pairing code) goes through the usual logging-safe path.
func (h *handlers) LogoutChannel(w http.ResponseWriter, r *http.Request, name string) {
	l := h.linker(w, name)
	if l == nil {
		return
	}
	if err := l.Unlink(r.Context()); err != nil {
		if errors.Is(err, channel.ErrLinkConflict) {
			writeError(w, http.StatusConflict, CodeConflict, err.Error())
			return
		}
		// The local session is gone but the platform was not told; the
		// message says what to do on the phone and holds no secret.
		writeError(w, http.StatusBadGateway, CodeInternal, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// linker is the Linker of a configured channel instance, or nil once the
// answer is written: 501 without a backend or for an adapter that cannot
// link, 404 for an unknown name.
func (h *handlers) linker(w http.ResponseWriter, name string) channel.Linker {
	if h.b.ChannelLinker == nil {
		notImplemented(w, "channel login")
		return nil
	}
	if !h.knownChannel(name) {
		notFound(w)
		return nil
	}
	l := h.b.ChannelLinker(name)
	if l == nil {
		writeError(w, http.StatusNotImplemented, CodeNotImplemented,
			fmt.Sprintf("channel %q (%s) has no interactive login", name, h.b.Config.Channels[name].Type))
	}
	return l
}

// deviceLogin renders a link on the DeviceLogin model the Codex login uses.
// The code is shown only while the link is pending.
func deviceLogin(s channel.LinkState) gen.DeviceLogin {
	out := gen.DeviceLogin{State: gen.DeviceLoginStateNone, Detail: optString(s.Detail)}
	switch s.Status {
	case channel.LinkPending:
		out.State = gen.DeviceLoginStatePending
		out.UserCode = optString(s.Code)
		if !s.Expires.IsZero() {
			out.ExpiresAt = ptr(s.Expires)
		}
	case channel.LinkDone:
		out.State = gen.DeviceLoginStateDone
	case channel.LinkFailed:
		out.State = gen.DeviceLoginStateFailed
	}
	return out
}

// --- credentials --------------------------------------------------------------

// PutCredentials writes provider secrets to auth.toml. Write-only by design:
// there is no GET, the values are never echoed and the body is never logged
// (ARCHITECTURE.md §4).
func (h *handlers) PutCredentials(w http.ResponseWriter, r *http.Request, provider string) {
	if h.b.Secrets == nil {
		notImplemented(w, "credentials")
		return
	}
	if h.b.Config == nil || h.b.Config.Providers[provider].Name == "" {
		notFound(w)
		return
	}
	var in gen.CredentialsPut
	if !decodeJSON(w, r, &in) {
		return
	}
	if len(in) == 0 {
		badRequest(w, "no credential fields given")
		return
	}
	if err := h.b.Secrets.Set(secrets.Providers, provider, in); err != nil {
		// The error is from the secrets package, which never quotes a value.
		h.b.fail(w, "store credentials", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *handlers) LoginProvider(w http.ResponseWriter, r *http.Request, provider string) {
	if h.b.ProviderLogin == nil {
		notImplemented(w, "provider login")
		return
	}
	state, err := h.b.ProviderLogin.Start(r.Context(), provider)
	if err != nil {
		h.b.fail(w, "provider login", err)
		return
	}
	writeJSON(w, http.StatusOK, state)
}

func (h *handlers) GetProviderLogin(w http.ResponseWriter, r *http.Request, provider string) {
	if h.b.ProviderLogin == nil {
		notImplemented(w, "provider login")
		return
	}
	state, err := h.b.ProviderLogin.Status(r.Context(), provider)
	if err != nil {
		h.b.fail(w, "provider login", err)
		return
	}
	writeJSON(w, http.StatusOK, state)
}
