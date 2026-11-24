// Package codex implements the unofficial ChatGPT-subscription flow: device-code
// login, strictly serialized token refresh and inference over the Responses API
// at the ChatGPT backend (docs/models.md).
//
// Everything unofficial is isolated here — endpoints, form fields, header names
// and the pinned model list — so that when upstream drifts, this one package
// and the fixtures under testdata/ are what change. The transport itself is the
// shared internal/provider/responses client; nothing in this package talks to
// the network outside the functions below, and every endpoint is injectable.
//
// Secrets: the tokens live in auth.toml only (ARCHITECTURE.md §4). They never
// appear in an event, an error or a log line.
package codex

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"holaclaw/internal/auth/secrets"
	"holaclaw/internal/event"
	"holaclaw/internal/provider"
	"holaclaw/internal/provider/responses"
)

// modelsTimeout bounds the model lookup: the endpoint is unofficial and
// unstable.
const modelsTimeout = 5 * time.Second

// Options configures a Provider. Every field has a usable default; the URL and
// client id fields exist so tests can point the flow at an httptest server.
type Options struct {
	Name          string           // provider instance name, e.g. "codex"
	Secrets       *secrets.File    // auth.toml; the one owner of the tokens
	Events        event.Sink       // auth.refreshed goes here
	HTTPClient    *http.Client     // default: a plain client
	Now           func() time.Time // default time.Now; injectable for expiry tests
	AuthBaseURL   string           // default DefaultAuthBaseURL
	InferenceURL  string           // default DefaultInferenceURL
	ClientID      string           // default DefaultClientID
	MaxConcurrent int              // in-flight requests; responses default when 0
}

// Provider is a Codex connection: the Responses transport plus the auth state
// machine that keeps it supplied with a valid access token.
type Provider struct {
	name      string
	auth      *auth
	login     *Login
	client    *responses.Client
	hc        *http.Client
	modelsURL string
	session   string // stable per Provider, used when a request carries none
}

var (
	_ provider.Provider    = (*Provider)(nil)
	_ provider.ModelLister = (*Provider)(nil)
)

// New builds a subscription-mode provider.
func New(opts Options) *Provider {
	name := opts.Name
	if name == "" {
		name = "codex"
	}
	hc := opts.HTTPClient
	if hc == nil {
		hc = &http.Client{}
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	authBase := strings.TrimRight(opts.AuthBaseURL, "/")
	if authBase == "" {
		authBase = DefaultAuthBaseURL
	}
	inference := opts.InferenceURL
	if inference == "" {
		inference = DefaultInferenceURL
	}
	clientID := opts.ClientID
	if clientID == "" {
		clientID = DefaultClientID
	}

	st := &store{sec: opts.Secrets, name: name}
	a := &auth{
		store: st, name: name, hc: hc, now: now,
		authBase: authBase, clientID: clientID, events: opts.Events,
	}
	p := &Provider{
		name:      name,
		auth:      a,
		login:     &Login{auth: a, store: st, sleep: sleepCtx},
		hc:        hc,
		modelsURL: modelsURL(inference),
		session:   randomSessionID(),
	}
	base, path := splitURL(inference)
	noStore := false // `store:false`: the backend must not retain the request
	p.client = responses.New(responses.Options{
		Name:                name,
		BaseURL:             base,
		Path:                path,
		APIKey:              a.accessToken,
		Headers:             p.headers,
		Store:               &noStore,
		OmitMaxOutputTokens: true,
		MaxConcurrent:       opts.MaxConcurrent,
		HTTPClient:          hc,
		OnUnauthorized:      a.forceRefresh,
	})
	return p
}

// NewAPIKey builds the `auth = "api-key"` mode: the same Responses transport
// against api.openai.com with the key from [providers.<name>] api_key. No
// device login, no refresh — the key does not rotate.
func NewAPIKey(name string, sec *secrets.File, opts ...Options) provider.Provider {
	var o Options
	if len(opts) > 0 {
		o = opts[0]
	}
	if name == "" {
		name = "codex"
	}
	return responses.New(responses.Options{
		Name:          name,
		BaseURL:       o.InferenceURL, // empty: the transport's api.openai.com default
		APIKey:        sec.Lookup(secrets.Providers, name, keyAPIKey),
		MaxConcurrent: o.MaxConcurrent,
		HTTPClient:    o.HTTPClient,
	})
}

// Name reports the provider instance name.
func (p *Provider) Name() string { return p.name }

// Login exposes the device-code flow so the CLI and the control API can drive
// and observe it.
func (p *Provider) Login() *Login { return p.login }

// Adopt runs write, which stores tokens obtained elsewhere in auth.toml, and
// uses them from the next request, as a completed login does.
func (p *Provider) Adopt(write func() error) error { return p.auth.adopt(write) }

// Chat ensures a valid access token before anything goes on the wire: a
// provider that is not logged in fails with a typed auth error and the login
// hint, without a request.
func (p *Provider) Chat(ctx context.Context, req provider.Request) (provider.Stream, error) {
	if _, err := p.auth.token(ctx); err != nil {
		return nil, err
	}
	return p.client.Chat(ctx, req)
}

// headers are the three the ChatGPT backend expects on every Responses call.
func (p *Provider) headers(req provider.Request) map[string]string {
	session := req.SessionID
	if session == "" {
		session = p.session
	}
	h := map[string]string{
		"originator": Originator,
		"session_id": session,
	}
	if id := p.auth.accountID(); id != "" {
		h["chatgpt-account-id"] = id
	}
	return h
}

// Models lists the model ids the account can use (provider.ModelLister). It
// sends the access token auth.toml holds as it is and never refreshes it: a
// refresh from a process that is not the daemon would spend the daemon's
// refresh token. An expired token, an answer without a single id and an
// instance that is not logged in are errors.
func (p *Provider) Models(ctx context.Context) ([]string, error) {
	t := p.auth.store.load()
	if t.Access == "" {
		return nil, p.auth.authError("codex is not logged in: " + loginHint)
	}
	ids, err := p.fetchModels(ctx, t.Access, t.AccountID)
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, &provider.Error{Kind: provider.ErrServer, Provider: p.name, Message: "the model list is empty"}
	}
	return ids, nil
}

// fetchModels is the one request to the model endpoint, under its own short
// deadline.
func (p *Provider) fetchModels(ctx context.Context, token, accountID string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, modelsTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.modelsURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("originator", Originator)
	if accountID != "" {
		req.Header.Set("chatgpt-account-id", accountID)
	}

	resp, err := p.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, &provider.Error{
			Kind: provider.ErrServer, Provider: p.name, Status: resp.StatusCode,
			Message: "model list unavailable",
		}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, err
	}
	return parseModels(body), nil
}

// parseModels accepts the two shapes seen in the wild: an OpenAI-style
// {"data":[{"id":…}]} envelope and a bare {"models":["…"]}.
func parseModels(body []byte) []string {
	var env struct {
		Data []struct {
			ID   string `json:"id"`
			Slug string `json:"slug"`
		} `json:"data"`
		Models []json.RawMessage `json:"models"`
	}
	if json.Unmarshal(body, &env) != nil {
		return nil
	}
	var out []string
	for _, d := range env.Data {
		if id := firstNonEmpty(d.ID, d.Slug); id != "" {
			out = append(out, id)
		}
	}
	for _, raw := range env.Models {
		var s string
		if json.Unmarshal(raw, &s) == nil && s != "" {
			out = append(out, s)
			continue
		}
		var m struct {
			ID   string `json:"id"`
			Slug string `json:"slug"`
		}
		if json.Unmarshal(raw, &m) == nil {
			if id := firstNonEmpty(m.ID, m.Slug); id != "" {
				out = append(out, id)
			}
		}
	}
	return out
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// modelsURL derives the model endpoint from the inference URL:
// …/backend-api/codex/responses → …/backend-api/codex/models.
func modelsURL(inference string) string {
	u := strings.TrimRight(inference, "/")
	if i := strings.LastIndex(u, "/"); i > 0 {
		u = u[:i]
	}
	return u + "/models"
}

// splitURL cuts an absolute endpoint into the base and path the Responses
// transport wants.
func splitURL(raw string) (base, path string) {
	i := strings.Index(raw, "://")
	if i < 0 {
		return raw, ""
	}
	if j := strings.Index(raw[i+3:], "/"); j >= 0 {
		return raw[:i+3+j], raw[i+3+j:]
	}
	return raw, ""
}

// randomSessionID is the per-Provider session_id used when a request carries
// none: stable for the life of the process, opaque, and not derived from
// anything about the user.
func randomSessionID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "00000000000000000000000000000000"
	}
	return hex.EncodeToString(b[:])
}

// sleepCtx waits out d unless ctx ends first.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
