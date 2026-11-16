package telegram

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestInviteLink(t *testing.T) {
	cases := []struct {
		name, user, code, want string
	}{
		{"plain", "holaclaw", "ABC123", "https://t.me/holaclaw?start=ABC123"},
		{"at prefix is dropped", "@holaclaw", "ABC123", "https://t.me/holaclaw?start=ABC123"},
		{"surrounding space", "  holaclaw  ", "ABC123", "https://t.me/holaclaw?start=ABC123"},
		{"code is escaped", "holaclaw", "a b&c", "https://t.me/holaclaw?start=a+b%26c"},
		{"no user", "", "ABC123", ""},
		{"no code", "holaclaw", "", ""},
		// Detail reaches the caller as free text: a state string, or an error,
		// must never turn into a link.
		{"not a username", "connecting to telegram", "ABC123", ""},
		{"path traversal", "../botfather", "ABC123", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := InviteLink(tc.user, tc.code); got != tc.want {
				t.Fatalf("InviteLink(%q, %q) = %q, want %q", tc.user, tc.code, got, tc.want)
			}
		})
	}
}

func TestGroupInviteLink(t *testing.T) {
	if got, want := GroupInviteLink("@holaclaw", "ABC123"), "https://t.me/holaclaw?startgroup=ABC123"; got != want {
		t.Fatalf("GroupInviteLink = %q, want %q", got, want)
	}
	// The same guards as the DM link: free text never becomes a link.
	for _, user := range []string{"", "connecting to telegram", "../botfather"} {
		if got := GroupInviteLink(user, "ABC123"); got != "" {
			t.Fatalf("GroupInviteLink(%q) = %q, want none", user, got)
		}
	}
	if got := GroupInviteLink("holaclaw", ""); got != "" {
		t.Fatalf("GroupInviteLink without a code = %q, want none", got)
	}
}

func TestLooksLikeToken(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{fakeToken, true},
		{"424242:AAF-daemon-e2e-fake-bot-token-padded", true},
		{"", false},
		{"424242", false},                       // no secret half
		{"424242:short", false},                 // truncated paste
		{"xoxb-1234-5678-abcdefghij", false},    // a Slack token
		{"424242:AAHsUpErSeCrEt\nToKeN", false}, // a line break survived the paste
	}
	for _, tc := range cases {
		if got := LooksLikeToken(tc.in); got != tc.want {
			t.Fatalf("LooksLikeToken(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestVerifyTokenReportsTheIdentity(t *testing.T) {
	f := newFake(t)
	f.on("getMe", wireUser{ID: 99, IsBot: true, Username: "holaclaw", FirstName: "Hola", LastName: "Claw"})

	id, err := VerifyToken(context.Background(), VerifyOptions{Token: fakeToken, BaseURL: f.srv.URL})
	if err != nil {
		t.Fatalf("VerifyToken: %v", err)
	}
	if id.ID != 99 || id.Username != "holaclaw" || id.Name != "Hola Claw" {
		t.Fatalf("identity = %+v, want {99 holaclaw Hola Claw}", id)
	}
}

func TestVerifyTokenRejectedTokenIsAuthKind(t *testing.T) {
	f := newFake(t)
	f.on("getMe", &fakeErr{Status: 401, Description: "Unauthorized"})

	_, err := VerifyToken(context.Background(), VerifyOptions{Token: fakeToken, BaseURL: f.srv.URL})
	if !errors.Is(err, ErrAuth) {
		t.Fatalf("err = %v, want ErrAuth", err)
	}
}

// An unreachable endpoint must be distinguishable from a rejected token: the
// caller stores the secret in the first case and refuses in the second.
func TestVerifyTokenUnreachableIsNetworkKind(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	addr := srv.URL
	srv.Close()

	_, err := VerifyToken(context.Background(), VerifyOptions{Token: fakeToken, BaseURL: addr})
	if !errors.Is(err, ErrNetwork) {
		t.Fatalf("err = %v, want ErrNetwork", err)
	}
}

// The token sits in the request path, so anything a caller may print must be
// scrubbed — including a description Telegram itself echoed back.
func TestVerifyTokenErrorNeverQuotesTheToken(t *testing.T) {
	f := newFake(t)
	f.on("getMe", &fakeErr{Status: 401, Description: "Unauthorized: " + fakeToken})

	_, err := VerifyToken(context.Background(), VerifyOptions{Token: fakeToken, BaseURL: f.srv.URL})
	if err == nil {
		t.Fatal("want an error")
	}
	if strings.Contains(err.Error(), fakeToken) {
		t.Fatalf("the token leaked into %q", err.Error())
	}
}

func TestVerifyTokenHonoursContextCancellation(t *testing.T) {
	f := newFake(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := VerifyToken(ctx, VerifyOptions{Token: fakeToken, BaseURL: f.srv.URL})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}
