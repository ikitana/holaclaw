package oauth

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"unicode"
)

// Metadata is what discovery learned about a server and its authorization
// server. It marshals to the JSON of store.Connector.AuthMeta. An Issuer
// that differs from the stored one means a reconnect: the client identity
// and the tokens belong to the old issuer.
type Metadata struct {
	Resource              string   `json:"resource"`          // the server URL tokens are bound to (RFC 8707)
	ResourceMetadata      string   `json:"resource_metadata"` // where the resource metadata was read
	Issuer                string   `json:"issuer"`
	AuthorizationEndpoint string   `json:"authorization_endpoint"`
	TokenEndpoint         string   `json:"token_endpoint"`
	RegistrationEndpoint  string   `json:"registration_endpoint,omitempty"`
	Scopes                []string `json:"scopes,omitempty"` // requested by default: the challenge's scope, else the resource's scopes_supported
	CodeChallengeMethods  []string `json:"code_challenge_methods_supported"`
	TokenAuthMethods      []string `json:"token_endpoint_auth_methods_supported,omitempty"`
	CIMDSupported         bool     `json:"client_id_metadata_document_supported,omitempty"`
	IssParameterSupported bool     `json:"authorization_response_iss_parameter_supported,omitempty"` // RFC 9207

	// What the resource metadata lists, for inspect only: never cached.
	// AuthorizationServers keeps the https ones within the caps, the first
	// (Issuer) included; ScopesSupported is as published.
	AuthorizationServers []string `json:"-"`
	ScopesSupported      []string `json:"-"`
}

// resourceMetadata is the part of RFC 9728 metadata we read.
type resourceMetadata struct {
	Resource             string   `json:"resource"`
	AuthorizationServers []string `json:"authorization_servers"`
	ScopesSupported      []string `json:"scopes_supported"`
}

// serverMetadata is the part of RFC 8414 (or OpenID) metadata we read.
type serverMetadata struct {
	Issuer                string   `json:"issuer"`
	AuthorizationEndpoint string   `json:"authorization_endpoint"`
	TokenEndpoint         string   `json:"token_endpoint"`
	RegistrationEndpoint  string   `json:"registration_endpoint"`
	CodeChallengeMethods  []string `json:"code_challenge_methods_supported"`
	TokenAuthMethods      []string `json:"token_endpoint_auth_methods_supported"`
	CIMDSupported         bool     `json:"client_id_metadata_document_supported"`
	IssParameterSupported bool     `json:"authorization_response_iss_parameter_supported"`
}

// Discover reads the metadata of serverURL starting from the WWW-Authenticate
// header of its 401. The resource_metadata it names is tried first, then the
// well-known paths; finding none is ErrNotProtected when the header named
// none. The first authorization server listed is used. Every fetch is https
// only.
func (c *Client) Discover(ctx context.Context, serverURL, wwwAuthenticate string) (Metadata, error) {
	server, err := url.Parse(serverURL)
	if err != nil || server.Host == "" || (server.Scheme != "https" && server.Scheme != "http") {
		return Metadata{}, fmt.Errorf("%w: bad server url", ErrBadMetadata)
	}
	challengeScopes := strings.Fields(bearerParam(wwwAuthenticate, "scope"))
	if !validScopes(challengeScopes) {
		return Metadata{}, fmt.Errorf("%w: bad challenge scope", ErrBadMetadata)
	}

	var candidates []string
	named := bearerParam(wwwAuthenticate, "resource_metadata")
	if named != "" {
		if len(named) > maxURL {
			return Metadata{}, fmt.Errorf("%w: resource metadata url too long", ErrBadMetadata)
		}
		candidates = append(candidates, named)
	}
	for _, u := range resourceMetadataURLs(server) {
		if u != named {
			candidates = append(candidates, u)
		}
	}
	var rm resourceMetadata
	rmURL, err := c.firstFound(ctx, candidates, &rm)
	switch {
	case err != nil:
		return Metadata{}, err
	case rmURL == "" && named != "":
		return Metadata{}, fmt.Errorf("%w: no resource metadata", ErrBadMetadata)
	case rmURL == "":
		return Metadata{}, ErrNotProtected
	}
	if !Covers(rm.Resource, serverURL) {
		return Metadata{}, ErrResourceMismatch
	}
	if len(rm.AuthorizationServers) == 0 {
		return Metadata{}, fmt.Errorf("%w: no authorization server", ErrBadMetadata)
	}
	if !validScopes(rm.ScopesSupported) {
		return Metadata{}, fmt.Errorf("%w: bad scopes_supported", ErrBadMetadata)
	}

	issuer := rm.AuthorizationServers[0]
	if !isHTTPS(issuer) {
		return Metadata{}, ErrInsecureURL
	}
	if !listable(issuer) {
		return Metadata{}, fmt.Errorf("%w: bad issuer", ErrBadMetadata)
	}
	iss, _ := url.Parse(issuer) // listable parsed it
	if iss.RawQuery != "" || iss.Fragment != "" {
		return Metadata{}, fmt.Errorf("%w: bad issuer", ErrBadMetadata)
	}
	var sm serverMetadata
	found, err := c.firstFound(ctx, serverMetadataURLs(iss), &sm)
	if err != nil {
		return Metadata{}, err
	}
	if found == "" {
		return Metadata{}, fmt.Errorf("%w: no authorization server metadata", ErrBadMetadata)
	}
	if sm.Issuer != issuer {
		return Metadata{}, ErrIssuerMismatch
	}
	if sm.AuthorizationEndpoint == "" || sm.TokenEndpoint == "" {
		return Metadata{}, fmt.Errorf("%w: missing endpoints", ErrBadMetadata)
	}
	for _, e := range []string{sm.AuthorizationEndpoint, sm.TokenEndpoint, sm.RegistrationEndpoint} {
		if e != "" && !isHTTPS(e) {
			return Metadata{}, ErrInsecureURL
		}
		if len(e) > maxURL {
			return Metadata{}, fmt.Errorf("%w: endpoint url too long", ErrBadMetadata)
		}
	}
	if !boundedList(sm.CodeChallengeMethods) || !boundedList(sm.TokenAuthMethods) {
		return Metadata{}, fmt.Errorf("%w: list too long", ErrBadMetadata)
	}
	if !slices.Contains(sm.CodeChallengeMethods, "S256") {
		return Metadata{}, ErrNoS256
	}

	scopes := challengeScopes
	if len(scopes) == 0 {
		scopes = rm.ScopesSupported
	}
	return Metadata{
		Resource:              rm.Resource,
		ResourceMetadata:      rmURL,
		Issuer:                sm.Issuer,
		AuthorizationEndpoint: sm.AuthorizationEndpoint,
		TokenEndpoint:         sm.TokenEndpoint,
		RegistrationEndpoint:  sm.RegistrationEndpoint,
		Scopes:                scopes,
		CodeChallengeMethods:  sm.CodeChallengeMethods,
		TokenAuthMethods:      sm.TokenAuthMethods,
		CIMDSupported:         sm.CIMDSupported,
		IssParameterSupported: sm.IssParameterSupported,
		AuthorizationServers:  listedServers(rm.AuthorizationServers),
		ScopesSupported:       rm.ScopesSupported,
	}, nil
}

// listedServers is the listable authorization servers a resource lists, at
// most maxList: only the first is used, so the rest are not refused, only
// left out. The issuer passed listable, so it is the first.
func listedServers(list []string) []string {
	var out []string
	for _, s := range list {
		if len(out) == maxList {
			break
		}
		if listable(s) {
			out = append(out, s)
		}
	}
	return out
}

// listable is an https URL with a host, no whitespace and within maxURL.
func listable(s string) bool {
	if len(s) > maxURL || strings.ContainsFunc(s, unicode.IsSpace) {
		return false
	}
	u, err := url.Parse(s)
	return err == nil && u.Scheme == "https" && u.Host != ""
}

// firstFound fetches the candidates in order into v and returns the URL that
// answered 200. A 4xx moves on to the next; "" means none was found.
func (c *Client) firstFound(ctx context.Context, candidates []string, v any) (string, error) {
	for _, u := range candidates {
		status, err := c.getJSON(ctx, u, v)
		if err != nil {
			return "", err
		}
		if status == http.StatusOK {
			return u, nil
		}
		if status < 400 || status > 499 {
			return "", fmt.Errorf("%w: metadata status %d", ErrBadMetadata, status)
		}
	}
	return "", nil
}

// resourceMetadataURLs are the RFC 9728 well-known forms the MCP spec tries:
// at the server's path, then at the root.
func resourceMetadataURLs(server *url.URL) []string {
	const wk = "/.well-known/oauth-protected-resource"
	origin := server.Scheme + "://" + server.Host
	var out []string
	if p := strings.TrimSuffix(server.EscapedPath(), "/"); p != "" {
		out = append(out, origin+wk+p)
	}
	return append(out, origin+wk)
}

// serverMetadataURLs are the forms the MCP spec tries for an issuer: RFC 8414
// path insertion, OpenID path insertion, OpenID path appending; without a
// path only the first two at the root.
func serverMetadataURLs(issuer *url.URL) []string {
	origin := "https://" + issuer.Host
	p := strings.TrimSuffix(issuer.EscapedPath(), "/")
	if p == "" {
		return []string{
			origin + "/.well-known/oauth-authorization-server",
			origin + "/.well-known/openid-configuration",
		}
	}
	return []string{
		origin + "/.well-known/oauth-authorization-server" + p,
		origin + "/.well-known/openid-configuration" + p,
		origin + p + "/.well-known/openid-configuration",
	}
}

// CanonicalURL is raw as the MCP spec's canonical server URI, for
// comparison: scheme and host lower case, default port and trailing slash
// dropped, no user info or fragment, the query kept as is. ok is false when
// raw has no host.
func CanonicalURL(raw string) (s string, ok bool) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", false
	}
	scheme := strings.ToLower(u.Scheme)
	host := strings.ToLower(u.Hostname())
	if strings.Contains(host, ":") { // IPv6 literal
		host = "[" + host + "]"
	}
	defaultPort := map[string]string{"https": "443", "http": "80"}[scheme]
	if port := u.Port(); port != "" && port != defaultPort {
		host += ":" + port
	}
	s = scheme + "://" + host + strings.TrimSuffix(u.EscapedPath(), "/")
	if u.RawQuery != "" {
		s += "?" + u.RawQuery
	}
	return s, true
}

// Covers reports whether tokens for resource serve serverURL: the same
// canonical URL, or a resource with no query whose path is a whole-segment
// prefix of the server's. Home Assistant names its origin as the resource
// of /api/mcp, which the MCP SDKs accept the same way.
func Covers(resource, serverURL string) bool {
	res, okR := CanonicalURL(resource)
	srv, okS := CanonicalURL(serverURL)
	if !okR || !okS {
		return false
	}
	return res == srv || (!strings.Contains(res, "?") && strings.HasPrefix(srv, res+"/"))
}
