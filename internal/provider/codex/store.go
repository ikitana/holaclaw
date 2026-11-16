package codex

import (
	"fmt"
	"time"

	"holaclaw/internal/auth/secrets"
)

// Keys of the [providers.<name>] entry in auth.toml (ARCHITECTURE.md §4). The
// daemon is their single owner: they rotate on every refresh.
const (
	keyAccessToken  = "access_token"
	keyRefreshToken = "refresh_token"
	keyIDToken      = "id_token"
	keyExpiresAt    = "expires_at" // RFC3339
	keyAccountID    = "account_id"
	keyAPIKey       = "api_key" // auth = "api-key" mode only
)

// secretStore is the seam over *secrets.File: a narrow interface so tests can
// make a write fail and assert that the old state is not discarded silently.
type secretStore interface {
	Get(section secrets.Section, name, key string) (string, bool)
	Set(section secrets.Section, name string, kv map[string]string) error
}

// tokens is one set of ChatGPT credentials. Values are secrets: they are never
// put in an error, a log line or an event.
type tokens struct {
	Access    string
	Refresh   string
	ID        string // id_token, optional
	ExpiresAt time.Time
	AccountID string
}

// store reads and writes the [providers.<name>] entry.
type store struct {
	sec  secretStore
	name string
}

// load reads the stored tokens and overlays what the access token itself says:
// the JWT is authoritative for expiry and account id, the stored values are the
// fallback when a claim is missing or the token is not decodable.
func (s *store) load() tokens {
	t := tokens{
		Access:    s.get(keyAccessToken),
		Refresh:   s.get(keyRefreshToken),
		ID:        s.get(keyIDToken),
		AccountID: s.get(keyAccountID),
	}
	if v := s.get(keyExpiresAt); v != "" {
		if at, err := time.Parse(time.RFC3339, v); err == nil {
			t.ExpiresAt = at.UTC()
		}
	}
	if t.Access == "" {
		return t
	}
	c, err := decodeClaims(t.Access)
	if err != nil {
		return t
	}
	if !c.ExpiresAt.IsZero() {
		t.ExpiresAt = c.ExpiresAt
	}
	if c.AccountID != "" {
		t.AccountID = c.AccountID
	}
	return t
}

// save writes the whole token set back. secrets.File re-reads under a lock and
// changes only this entry, so a concurrent writer of another entry is safe.
func (s *store) save(t tokens) error {
	kv := map[string]string{
		keyAccessToken:  t.Access,
		keyRefreshToken: t.Refresh,
		keyAccountID:    t.AccountID,
		keyExpiresAt:    "",
	}
	if !t.ExpiresAt.IsZero() {
		kv[keyExpiresAt] = t.ExpiresAt.UTC().Format(time.RFC3339)
	}
	if t.ID != "" {
		kv[keyIDToken] = t.ID
	}
	if err := s.sec.Set(secrets.Providers, s.name, kv); err != nil {
		// The error comes from the secrets package, which never quotes values.
		return fmt.Errorf("codex: persisting tokens: %w", err)
	}
	return nil
}

func (s *store) get(key string) string {
	v, _ := s.sec.Get(secrets.Providers, s.name, key)
	return v
}
