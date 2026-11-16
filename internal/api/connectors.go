package api

import (
	"errors"
	"net/http"
	"slices"
	"strings"

	"holaclaw/internal/api/gen"
	"holaclaw/internal/connector"
	"holaclaw/internal/connector/mcp"
	"holaclaw/internal/connector/oauth"
)

// --- connectors ---------------------------------------------------------------
//
// Thin wrappers over the connector manager (docs/connectors.md §8), all admin
// scope. Nothing here logs or answers a token, a code, a client secret, the
// PKCE verifier or the state.

// CreateConnector opens a connector: 202 with the authorization URL, or 200
// for an open server connected at once.
func (h *handlers) CreateConnector(w http.ResponseWriter, r *http.Request) {
	if !h.connectorsReady(w) {
		return
	}
	var in gen.ConnectorCreate
	if !decodeJSON(w, r, &in) {
		return
	}
	req := connector.CreateRequest{
		ID: in.Id, URL: in.Url, Scopes: optSlice(in.Scopes),
		RedirectURI: deref(in.RedirectUri), ClientID: deref(in.ClientId), ClientSecret: deref(in.ClientSecret),
		AllowInsecureTransport: deref(in.AllowInsecureTransport),
	}
	if in.Auth != nil {
		req.Auth = string(*in.Auth)
	}
	st, err := h.b.Connectors.Create(r.Context(), req)
	if err != nil {
		h.failConnector(w, "create connector", err)
		return
	}
	writeStarted(w, st)
}

// PostConnectorCode finishes the open authorization with the redirect's
// query. The manager answers once the connector is connected or degraded.
func (h *handlers) PostConnectorCode(w http.ResponseWriter, r *http.Request, id gen.Id) {
	if !h.connectorsReady(w) {
		return
	}
	var in gen.ConnectorCode
	if !decodeJSON(w, r, &in) {
		return
	}
	if in.Code == "" || in.State == "" {
		badRequest(w, "code and state are required")
		return
	}
	c, err := h.b.Connectors.Code(r.Context(), id, oauth.Callback{Code: in.Code, State: in.State, Issuer: deref(in.Iss)})
	if err != nil {
		h.failConnector(w, "connector code", err)
		return
	}
	writeJSON(w, http.StatusAccepted, toGenConnector(c))
}

// ReconnectConnector opens a new authorization on a connector; it answers
// like create.
func (h *handlers) ReconnectConnector(w http.ResponseWriter, r *http.Request, id gen.Id) {
	if !h.connectorsReady(w) {
		return
	}
	var in gen.ConnectorReconnect
	if !decodeJSON(w, r, &in) {
		return
	}
	st, err := h.b.Connectors.Reconnect(r.Context(), id, connector.ReconnectRequest{
		Scopes: optSlice(in.Scopes), RedirectURI: deref(in.RedirectUri), ClientSecret: deref(in.ClientSecret),
	})
	if err != nil {
		h.failConnector(w, "reconnect connector", err)
		return
	}
	writeStarted(w, st)
}

// ListConnectors answers every connector, by id.
func (h *handlers) ListConnectors(w http.ResponseWriter, r *http.Request) {
	if !h.connectorsReady(w) {
		return
	}
	cs, err := h.b.Connectors.List(r.Context())
	if err != nil {
		h.failConnector(w, "list connectors", err)
		return
	}
	out := make([]gen.Connector, 0, len(cs))
	for _, c := range cs {
		out = append(out, toGenConnector(c))
	}
	writeJSON(w, http.StatusOK, struct {
		Connectors []gen.Connector `json:"connectors"`
	}{out})
}

// GetConnector answers one connector with its tools, trust and the public
// auth metadata of its grant.
func (h *handlers) GetConnector(w http.ResponseWriter, r *http.Request, id gen.Id) {
	if !h.connectorsReady(w) {
		return
	}
	c, err := h.b.Connectors.Get(r.Context(), id)
	if err != nil {
		h.failConnector(w, "get connector", err)
		return
	}
	writeJSON(w, http.StatusOK, toGenConnectorDetail(c))
}

// InspectConnector asks a server what it is without a token. Nothing is
// stored, registered or announced.
func (h *handlers) InspectConnector(w http.ResponseWriter, r *http.Request) {
	if !h.connectorsReady(w) {
		return
	}
	var in gen.ConnectorInspect
	if !decodeJSON(w, r, &in) {
		return
	}
	got, err := h.b.Connectors.Inspect(r.Context(), in.Url)
	if err != nil {
		h.failConnector(w, "inspect connector", err)
		return
	}
	writeJSON(w, http.StatusOK, toGenInspection(got))
}

// DeleteConnector removes a connector in any state, an open authorization
// included; the manager announces it as deleted.
func (h *handlers) DeleteConnector(w http.ResponseWriter, r *http.Request, id gen.Id) {
	if !h.connectorsReady(w) {
		return
	}
	if err := h.b.Connectors.Delete(r.Context(), id); err != nil {
		h.failConnector(w, "delete connector", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *handlers) connectorsReady(w http.ResponseWriter) bool {
	if h.b.Connectors == nil {
		notImplemented(w, "connectors")
		return false
	}
	return true
}

// writeStarted is 202 with the authorization URL, or 200 without one.
func writeStarted(w http.ResponseWriter, st connector.Started) {
	out := gen.ConnectorStarted{Connector: toGenConnector(st.Connector)}
	if st.AuthorizationURL == "" {
		writeJSON(w, http.StatusOK, out)
		return
	}
	exp := st.ExpiresAt
	out.AuthorizationUrl, out.ExpiresAt = &st.AuthorizationURL, &exp
	writeJSON(w, http.StatusAccepted, out)
}

// failConnector maps the manager's errors. Their text is the daemon's own
// and names no credential, so it is shown.
func (h *handlers) failConnector(w http.ResponseWriter, what string, err error) {
	switch {
	case errors.Is(err, connector.ErrNotFound):
		notFound(w)
	case errors.Is(err, connector.ErrInvalid), errors.Is(err, connector.ErrStateMismatch),
		errors.Is(err, connector.ErrAuthMismatch), issMismatch(err):
		badRequest(w, err.Error())
	case errors.Is(err, connector.ErrExists), errors.Is(err, connector.ErrNoIntent), errors.Is(err, connector.ErrBusy),
		errors.Is(err, connector.ErrHandManaged), errors.Is(err, connector.ErrBadFile):
		writeError(w, http.StatusConflict, CodeConflict, err.Error())
	case errors.Is(err, connector.ErrLeftovers):
		writeError(w, http.StatusInternalServerError, CodeInternal, err.Error())
	case errors.Is(err, connector.ErrAuthorization), errors.Is(err, connector.ErrDiscovery),
		errors.Is(err, connector.ErrUnreachable):
		writeError(w, http.StatusBadGateway, CodeInternal, err.Error())
	default:
		h.b.fail(w, what, err)
	}
}

// issMismatch is a redirect whose iss is not the authorization server's
// (RFC 9207): a bad request like a state mismatch. Discovery's own issuer
// mismatch wraps ErrDiscovery and stays a 502.
func issMismatch(err error) bool {
	return errors.Is(err, connector.ErrAuthorization) && errors.Is(err, oauth.ErrIssuerMismatch)
}

func toGenConnector(c connector.Connector) gen.Connector {
	out := gen.Connector{
		Id: c.ID, Url: c.URL, State: string(c.State),
		Auth: optString(c.Auth), ClientId: optString(c.ClientID), Detail: optString(c.Detail),
		ServerName: optString(c.ServerName), ServerVersion: optString(c.ServerVersion),
		ToolCount: len(c.Tools), LastCheck: c.LastCheck,
	}
	if c.Scopes != nil {
		out.Scopes = ptr(slices.Clone(c.Scopes))
	}
	if len(c.GrantedScopes) > 0 {
		out.GrantedScopes = ptr(slices.Clone(c.GrantedScopes))
	}
	return out
}

// toGenConnectorDetail is the show answer. Of the auth metadata only what
// the authorization server publishes and the client id it knows are
// answered; the grant's credentials live in auth.toml and never get here.
func toGenConnectorDetail(c connector.Connector) gen.ConnectorDetail {
	out := gen.ConnectorDetail{Connector: toGenConnector(c), Trusted: c.Trusted, Tools: toGenTools(c.Tools)}
	if md := c.Metadata; md != nil {
		am := toGenAuthMetadata(*md)
		am.IdentityKind, am.ClientId = optString(string(c.IdentityKind)), optString(c.GrantedClientID)
		out.AuthMetadata = &am
	}
	return out
}

// toGenInspection is the inspect answer: the server's info and tools when
// it is open, else its public auth metadata and the identity a create with
// the default redirect would use ("" from the manager is a pre-registered
// client id).
func toGenInspection(in connector.Inspection) gen.ConnectorInspection {
	out := gen.ConnectorInspection{
		Open: in.Open, ServerName: optString(in.ServerName), ServerVersion: optString(in.ServerVersion),
		Tools: toGenTools(in.Tools),
	}
	if md := in.Metadata; md != nil {
		am := toGenAuthMetadata(*md)
		out.AuthMetadata = &am
		if len(md.AuthorizationServers) > 0 {
			out.AuthorizationServers = ptr(slices.Clone(md.AuthorizationServers))
		}
		if len(md.ScopesSupported) > 0 {
			out.ScopesSupported = ptr(slices.Clone(md.ScopesSupported))
		}
		kind := in.IdentityKind
		if kind == "" {
			kind = oauth.IdentityPreRegistered
		}
		out.Identity = ptr(string(kind))
	}
	return out
}

// toGenTools is the tools by name with their risk class.
func toGenTools(tools []mcp.Tool) []gen.ConnectorTool {
	tools = slices.Clone(tools)
	slices.SortFunc(tools, func(a, b mcp.Tool) int { return strings.Compare(a.Name, b.Name) })
	out := make([]gen.ConnectorTool, 0, len(tools))
	for _, t := range tools {
		out = append(out, gen.ConnectorTool{
			Name: t.Name, Description: optString(t.Description), Risk: string(connector.Risk(t)),
			AlwaysAsk: connector.AlwaysAsk(t), DescriptionTruncated: connector.DescriptionCut(t.Description),
		})
	}
	return out
}

// toGenAuthMetadata is the public part of the metadata, without the
// identity: its caller knows which one applies.
func toGenAuthMetadata(md oauth.Metadata) gen.ConnectorAuthMetadata {
	am := gen.ConnectorAuthMetadata{
		Issuer: md.Issuer, Resource: optString(md.Resource),
		AuthorizationEndpoint: optString(md.AuthorizationEndpoint), TokenEndpoint: optString(md.TokenEndpoint),
		RegistrationEndpoint: optString(md.RegistrationEndpoint),
	}
	if len(md.Scopes) > 0 {
		am.Scopes = ptr(slices.Clone(md.Scopes))
	}
	return am
}

// optSlice keeps absent (nil) apart from an empty list.
func optSlice(s *[]string) []string {
	if s == nil {
		return nil
	}
	if *s == nil {
		return []string{}
	}
	return *s
}
