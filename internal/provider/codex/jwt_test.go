package codex

import (
	"encoding/base64"
	"errors"
	"testing"
	"time"
)

func jwt(payload string) string {
	return jwtHeader + "." + base64.RawURLEncoding.EncodeToString([]byte(payload)) + ".FAKEsig"
}

func TestDecodeClaims(t *testing.T) {
	cases := []struct {
		name    string
		token   string
		wantErr error
		wantExp time.Time
		wantAcc string
	}{
		{
			name:    "valid",
			token:   jwt(`{"exp":1893456000,"https://api.openai.com/auth":{"chatgpt_account_id":"acct-1"}}`),
			wantExp: time.Unix(1893456000, 0).UTC(),
			wantAcc: "acct-1",
		},
		{
			name:  "missing claims",
			token: jwt(`{"iss":"https://auth.openai.com"}`),
		},
		{
			name:    "auth claim without account id",
			token:   jwt(`{"exp":1893456000,"https://api.openai.com/auth":{"user_id":"u-1"}}`),
			wantExp: time.Unix(1893456000, 0).UTC(),
		},
		{
			name:    "claims of the wrong type are tolerated",
			token:   jwt(`{"exp":"soon","https://api.openai.com/auth":"nope"}`),
			wantExp: time.Time{},
		},
		{
			name:    "garbage payload",
			token:   jwtHeader + ".not-base64-@@@.FAKEsig",
			wantErr: ErrBadToken,
		},
		{
			name:    "payload is not an object",
			token:   jwt(`["a"]`),
			wantErr: ErrBadToken,
		},
		{
			name:    "two segments",
			token:   jwtHeader + "." + jwtHeader,
			wantErr: ErrBadToken,
		},
		{
			name:    "four segments",
			token:   jwtHeader + ".a.b.c",
			wantErr: ErrBadToken,
		},
		{
			name:    "empty",
			wantErr: ErrBadToken,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := decodeClaims(tc.token)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if err != nil {
				return
			}
			if !c.ExpiresAt.Equal(tc.wantExp) {
				t.Errorf("exp = %v, want %v", c.ExpiresAt, tc.wantExp)
			}
			if c.AccountID != tc.wantAcc {
				t.Errorf("account = %q, want %q", c.AccountID, tc.wantAcc)
			}
		})
	}
}

// A decode failure must never echo the token.
func TestDecodeClaimsErrorHasNoToken(t *testing.T) {
	token := jwtHeader + ".not-base64-@@@.FAKEsigSECRET"
	_, err := decodeClaims(token)
	if err == nil {
		t.Fatal("want an error")
	}
	if got := err.Error(); containsAny(got, token, "FAKEsigSECRET") {
		t.Fatalf("error leaks the token: %q", got)
	}
}
