package oauth

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"slices"
)

// CIMDClientID is the client id of the metadata document holaclaw publishes
// (docs/connectors.md §3).
const CIMDClientID = "https://holaclaw.ai/oauth/client-metadata.json"

// clientName is what a consent screen shows for a registered client.
const clientName = "HolaClaw"

// pinnedRedirects are the redirect URIs the metadata document lists; the
// draft requires an exact match.
var pinnedRedirects = map[string]bool{
	"http://127.0.0.1:26500/callback": true,
	"http://127.0.0.1:26501/callback": true,
	"http://127.0.0.1:26502/callback": true,
	"http://127.0.0.1:26503/callback": true,
	"http://127.0.0.1:26504/callback": true,
}

// PinnedRedirect reports whether uri is one the metadata document lists.
func PinnedRedirect(uri string) bool { return pinnedRedirects[uri] }

// PinnedRedirects is every redirect URI the metadata document lists, in
// port order: the addresses a client listens on.
func PinnedRedirects() []string {
	return slices.Sorted(maps.Keys(pinnedRedirects))
}

// IdentityKind is how the client named itself to the authorization server.
type IdentityKind string

const (
	IdentityPreRegistered IdentityKind = "pre-registered" // client_id from connectors.toml
	IdentityCIMD          IdentityKind = "cimd"           // the client id metadata document
	IdentityDCR           IdentityKind = "dcr"            // dynamic client registration
)

// Identity is the client the authorization server knows, bound to the
// issuer and the redirect it was made for. ClientSecret is a credential
// (auth.toml only) and never printed or marshalled.
//
// For a refresh the manager rebuilds it from what it persisted when the
// tokens were granted: the Kind and the ClientID they were granted to
// (store.Connector.ClientID holds the granted client id whatever its kind),
// never inferred, since a reconnect can switch kinds; Issuer is that of the
// stored AuthMeta.
type Identity struct {
	Kind         IdentityKind
	ClientID     string
	ClientSecret string `json:"-"`
	Issuer       string
	RedirectURI  string
}

func (i Identity) String() string {
	return fmt.Sprintf("{%s %s issuer:%s redirect:%s secret:%t}", i.Kind, i.ClientID, i.Issuer, i.RedirectURI, i.ClientSecret != "")
}

func (i Identity) GoString() string { return "oauth.Identity" + i.String() }

// IdentityOptions are the inputs of Identify.
type IdentityOptions struct {
	ClientID               string // pre-registered, from connectors.toml
	ClientSecret           string // of the pre-registered client, from auth.toml
	RegisteredClientID     string // registered before, from the cache row
	RegisteredClientSecret string // of the registered client, from auth.toml
	RegisteredRedirectURI  string // the redirect it registered; reused only for that one
	RedirectURI            string // the intent's redirect
	CIMDRefused            bool   // the cache row remembers a refused document
}

// Identify picks the client identity, first that applies: a pre-registered
// client id, the metadata document (advertised, a pinned redirect, not
// refused before), a client registered before, then dynamic client
// registration with the redirect.
func (c *Client) Identify(ctx context.Context, m Metadata, o IdentityOptions) (Identity, error) {
	if o.RedirectURI == "" {
		return Identity{}, fmt.Errorf("%w: no redirect uri", ErrNoRegistration)
	}
	id := Identity{Issuer: m.Issuer, RedirectURI: o.RedirectURI}
	switch IdentityKindFor(m, o) {
	case IdentityPreRegistered:
		id.Kind, id.ClientID, id.ClientSecret = IdentityPreRegistered, o.ClientID, o.ClientSecret
	case IdentityCIMD:
		id.Kind, id.ClientID = IdentityCIMD, CIMDClientID
	case IdentityDCR:
		id.Kind = IdentityDCR
		if reusesRegistration(o) {
			id.ClientID, id.ClientSecret = o.RegisteredClientID, o.RegisteredClientSecret
			break
		}
		var err error
		if id.ClientID, id.ClientSecret, err = c.register(ctx, m.RegistrationEndpoint, o.RedirectURI); err != nil {
			return Identity{}, err
		}
	default:
		return Identity{}, ErrNoRegistration
	}
	return id, nil
}

// IdentityKindFor is the kind Identify picks, without registering; ""
// means none applies: a pre-registered client id is needed. Inspect uses
// it to say what a create would do.
func IdentityKindFor(m Metadata, o IdentityOptions) IdentityKind {
	switch {
	case o.ClientID != "":
		return IdentityPreRegistered
	case m.CIMDSupported && !o.CIMDRefused && PinnedRedirect(o.RedirectURI):
		return IdentityCIMD
	case reusesRegistration(o), m.RegistrationEndpoint != "":
		return IdentityDCR
	}
	return ""
}

// reusesRegistration reports whether a client registered before serves:
// it was registered for this redirect.
func reusesRegistration(o IdentityOptions) bool {
	return o.RegisteredClientID != "" && o.RegisteredRedirectURI == o.RedirectURI
}

// register is RFC 7591 registration of a public client.
func (c *Client) register(ctx context.Context, endpoint, redirectURI string) (string, string, error) {
	if !isHTTPS(endpoint) {
		return "", "", ErrInsecureURL
	}
	body, err := json.Marshal(map[string]any{
		"client_name":                clientName,
		"redirect_uris":              []string{redirectURI},
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": "none",
	})
	if err != nil {
		return "", "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", "", fmt.Errorf("%w: bad registration endpoint", ErrBadMetadata)
	}
	req.Header.Set("Content-Type", "application/json")
	var resp struct {
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
	}
	if err := c.post(req, Identity{Kind: IdentityDCR}, &resp); err != nil {
		return "", "", err
	}
	if !validClientID(resp.ClientID) {
		return "", "", fmt.Errorf("%w: registration returned no usable client id", ErrBadResponse)
	}
	return resp.ClientID, resp.ClientSecret, nil
}
