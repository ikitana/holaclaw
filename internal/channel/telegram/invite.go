package telegram

import (
	"context"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// DefaultVerifyTimeout bounds the one getMe a token check costs.
const DefaultVerifyTimeout = 10 * time.Second

// userShape is what Telegram allows in a bot username. The identity a link is
// built from comes from getMe, but it reaches this package as free text, so
// the shape is checked rather than trusted.
var userShape = regexp.MustCompile(`^[A-Za-z0-9_]{1,32}$`)

// tokenShape is the token BotFather issues: "<bot id>:<secret>". Checking it
// costs nothing and catches the common paste accident — a truncated token, or
// one with a line break in the middle — without reaching the network.
var tokenShape = regexp.MustCompile(`^[0-9]{5,}:[A-Za-z0-9_-]{30,}$`)

// LooksLikeToken reports whether s has the shape of a bot token. A true here
// says nothing about whether the token is live; only getMe answers that.
func LooksLikeToken(s string) bool { return tokenShape.MatchString(s) }

// InviteLink is the deep link that pairs a sender with the bot in one tap.
// Telegram hands the payload to the bot as "/start <code>", which the access
// gate already reads as an invite code (docs/identity.md), so the sender never
// types the code and never has to find the bot by name.
//
// user is the bot username getMe reported, with or without the leading "@".
// An empty result means there is no link to offer.
func InviteLink(user, code string) string { return deepLink(user, "start", code) }

// GroupInviteLink is the deep link that adds the bot to a group and enables
// that group in one tap. Telegram asks which group, adds the bot, and posts
// "/start <code>" there on the tapper's behalf, which the access gate reads as
// a room invite.
func GroupInviteLink(user, code string) string { return deepLink(user, "startgroup", code) }

func deepLink(user, param, code string) string {
	user = strings.TrimPrefix(strings.TrimSpace(user), "@")
	if code == "" || !userShape.MatchString(user) {
		return ""
	}
	return "https://t.me/" + user + "?" + param + "=" + url.QueryEscape(code)
}

// Identity is what getMe reports about a bot.
type Identity struct {
	ID       int64
	Username string // without the leading "@"
	Name     string
}

// VerifyOptions configures VerifyToken. Only Token is required.
type VerifyOptions struct {
	Token      string
	BaseURL    string // default DefaultBaseURL
	HTTPClient *http.Client
}

// VerifyToken asks Telegram who a token belongs to. It is the cheapest proof
// that a freshly pasted token works: getMe takes no argument and changes
// nothing, and every error out of it is already scrubbed of the token.
//
// The error is an *Error, so a caller tells a rejected token (ErrAuth) from an
// unreachable network (ErrNetwork) without parsing strings.
func VerifyToken(ctx context.Context, opts VerifyOptions) (Identity, error) {
	hc := opts.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: DefaultVerifyTimeout}
	}
	a := New(Options{
		Token:      func() string { return opts.Token },
		BaseURL:    opts.BaseURL,
		HTTPClient: hc,
	})
	var me wireUser
	if err := a.call(ctx, "getMe", nil, &me); err != nil {
		return Identity{}, err
	}
	return Identity{
		ID:       me.ID,
		Username: me.Username,
		Name:     strings.TrimSpace(me.FirstName + " " + me.LastName),
	}, nil
}
