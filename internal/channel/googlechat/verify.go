package googlechat

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
)

// OpSubscriptionCheck is the Error.Op of Verify's subscription step, so
// callers can tell a missing or unreadable subscription from other failures.
const OpSubscriptionCheck = "subscriptions.testIamPermissions"

// consumePermission is what pulling and acknowledging need, and what the
// Pub/Sub Subscriber role grants.
const consumePermission = "pubsub.subscriptions.consume"

// VerifyOptions configures Verify. Only Key is required.
type VerifyOptions struct {
	Key        string // the service-account key JSON
	BaseURL    string // tests only, as in Options
	HTTPClient *http.Client
}

// VerifyResult is what a working key belongs to.
type VerifyResult struct {
	Project     string
	ClientEmail string
}

// Verify runs the checks `auth set` makes before storing a key
// (docs/channels/googlechat.md §3), in order, stopping at the first failure:
//
//  1. the JSON is a service-account key (ErrBadKey);
//  2. Google issues a token for it (ErrAuth);
//  3. the key may pull from projects/<p>/subscriptions/holaclaw, asked with
//     testIamPermissions (ErrNotFound, ErrForbidden);
//  4. a Chat API call as the app (ErrNotConfigured).
//
// ErrNetwork and ErrServer mean Google could not be asked; the caller stores
// the key unchecked with a warning. Every error is an *Error, already
// scrubbed, with Op naming the step and Project the key's project. The result
// carries Project and ClientEmail from step 1 on, even with an error.
func Verify(ctx context.Context, o VerifyOptions) (VerifyResult, error) {
	k, err := readKey(o.Key)
	if err != nil {
		return VerifyResult{}, err
	}
	res := VerifyResult{Project: k.ProjectID, ClientEmail: k.ClientEmail}
	if _, err := k.signer(); err != nil {
		return res, err
	}
	hc := o.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: DefaultVerifyTimeout}
	}
	a := New(Options{Key: func() string { return o.Key }, BaseURL: o.BaseURL, HTTPClient: hc})

	if _, err := a.credentials(ctx); err != nil {
		return res, err
	}
	if _, err := a.checkSubscription(ctx); err != nil {
		return res, err
	}
	err = a.call(ctx, request{op: "spaces.list", method: http.MethodGet,
		url: func(string) string { return a.chat + "/v1/spaces?pageSize=1" }}, nil)
	if err != nil {
		return res, chatNotConfigured(err)
	}
	return res, nil
}

// checkSubscription asks whether the key may pull from the subscription and
// returns the key's project. It is quick, unlike a pull, which Pub/Sub holds
// open while no event is waiting.
//
// testIamPermissions rather than subscriptions.get: the key is granted
// Pub/Sub Subscriber alone, which can pull but not get. Asking needs no role,
// so this checks the one permission the pull loop uses.
func (a *Adapter) checkSubscription(ctx context.Context) (string, error) {
	var granted struct {
		Permissions []string `json:"permissions"`
	}
	project := ""
	err := a.call(ctx, request{op: OpSubscriptionCheck, method: http.MethodPost,
		url: func(p string) string {
			project = p
			return a.pubsub + "/v1/" + subscriptionPath(p) + ":testIamPermissions"
		},
		body: map[string]any{"permissions": []string{consumePermission}}}, &granted)
	if err != nil {
		return project, err
	}
	if !slices.Contains(granted.Permissions, consumePermission) {
		return project, &Error{Kind: KindForbidden, Op: OpSubscriptionCheck, Status: http.StatusForbidden, Project: project,
			Message: "the service account lacks " + consumePermission + " on the subscription"}
	}
	return project, nil
}

// chatNotConfigured recognises Chat refusing a call because the app is not
// configured (§4 step 5).
//
// Unverified: the exact error Chat returns for an unconfigured app must be
// confirmed in the manual test. The assumption is a 404 whose message reads
// like "Google Chat app not found. To create a Chat app, you must turn on the
// Chat API and configure the app in the Google Cloud console.", so a 404, or
// any error whose message says the Chat app is not found or not configured,
// maps to KindNotConfigured.
func chatNotConfigured(err error) error {
	var e *Error
	if !errors.As(err, &e) {
		return err
	}
	msg := strings.ToLower(e.Message)
	if e.Kind == KindNotFound ||
		(strings.Contains(msg, "chat app") && (strings.Contains(msg, "not found") || strings.Contains(msg, "configure"))) {
		ne := *e
		ne.Kind = KindNotConfigured
		return &ne
	}
	return err
}
