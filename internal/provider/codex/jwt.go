package codex

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// ErrBadToken is returned when the access token is not a three-segment JWT or
// its payload is not a JSON object. It never carries any part of the token.
var ErrBadToken = errors.New("codex: malformed access token")

// authClaim is the namespaced claim the ChatGPT backend puts the account id in.
const authClaim = "https://api.openai.com/auth"

// claims is the little of the access token we read: when it expires and which
// ChatGPT account it belongs to.
type claims struct {
	ExpiresAt time.Time
	AccountID string
}

// decodeClaims reads the payload of a JWT access token.
//
// The signature is deliberately NOT verified: we are the client, not the
// verifier. The issuer checks it on every request; here the payload is only a
// hint about expiry and account id, and a forged one would buy nothing but a
// rejected request. Missing or oddly typed claims are tolerated — the caller
// falls back to the stored account_id/expires_at. Nothing decoded here is ever
// logged.
func decodeClaims(token string) (claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return claims{}, ErrBadToken
	}
	// JWT payloads are base64url without padding.
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return claims{}, ErrBadToken
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(raw, &payload); err != nil {
		return claims{}, ErrBadToken
	}

	var c claims
	if b, ok := payload["exp"]; ok {
		var exp int64
		if err := json.Unmarshal(b, &exp); err == nil && exp > 0 {
			c.ExpiresAt = time.Unix(exp, 0).UTC()
		}
	}
	if b, ok := payload[authClaim]; ok {
		var auth struct {
			ChatGPTAccountID string `json:"chatgpt_account_id"`
		}
		if err := json.Unmarshal(b, &auth); err == nil {
			c.AccountID = auth.ChatGPTAccountID
		}
	}
	return c, nil
}
