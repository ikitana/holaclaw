package googlechat

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func verify(t *testing.T, f *fakeGoogle, key string) (VerifyResult, error) {
	t.Helper()
	return Verify(context.Background(), VerifyOptions{Key: key, BaseURL: f.srv.URL})
}

func TestVerifyOK(t *testing.T) {
	f := newFake(t)
	res, err := verify(t, f, keyJSON(nil))
	if err != nil {
		t.Fatal(err)
	}
	if res.Project != testProject || res.ClientEmail != testEmail {
		t.Fatalf("result = %+v", res)
	}
	sc := f.records("subcheck")
	if len(sc) != 1 || !strings.Contains(string(sc[0].Body), `"pubsub.subscriptions.consume"`) {
		t.Fatalf("testIamPermissions = %+v", sc)
	}
	sl := f.records("spaces.list")
	if len(sl) != 1 || sl[0].Query.Get("pageSize") != "1" {
		t.Fatalf("spaces.list = %+v", sl)
	}
}

func TestVerifySteps(t *testing.T) {
	for _, tc := range []struct {
		name  string
		key   string
		route string
		code  int
		msg   string
		want  error
		op    string
	}{
		{name: "not a service account", key: `{"type":"authorized_user"}`, want: ErrBadKey, op: "key"},
		{name: "bad pem", key: keyJSON(func(m map[string]any) { m["private_key"] = "nope" }), want: ErrBadKey, op: "key"},
		{name: "key deleted", route: "token", code: 400, msg: "invalid_grant", want: ErrAuth, op: "token"},
		{name: "no subscription", route: "subcheck", code: 404, want: ErrNotFound, op: OpSubscriptionCheck},
		{name: "cannot check subscription", route: "subcheck", code: 403, want: ErrForbidden, op: OpSubscriptionCheck},
		{name: "chat app not configured (404)", route: "spaces.list", code: 404, want: ErrNotConfigured, op: "spaces.list"},
		{name: "chat app not configured (message)", route: "spaces.list", code: 400,
			msg:  "Google Chat app not found. To create a Chat app, you must turn on the Chat API and configure the app in the Google Cloud console.",
			want: ErrNotConfigured, op: "spaces.list"},
		{name: "chat api disabled", route: "spaces.list", code: 403, msg: "Google Chat API has not been used in project", want: ErrForbidden, op: "spaces.list"},
		{name: "server", route: "subcheck", code: 500, want: ErrServer, op: OpSubscriptionCheck},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(t)
			if tc.route != "" {
				f.handle(tc.route, func(w http.ResponseWriter, _ *http.Request) {
					if tc.route == "token" {
						writeJSON(w, tc.code, map[string]any{"error": tc.msg})
						return
					}
					msg := tc.msg
					if msg == "" {
						msg = "scripted"
					}
					writeJSON(w, tc.code, googleErr(tc.code, msg))
				})
			}
			key := tc.key
			if key == "" {
				key = keyJSON(nil)
			}
			res, err := verify(t, f, key)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			var e *Error
			if !errors.As(err, &e) || e.Op != tc.op {
				t.Fatalf("op = %+v, want %s", e, tc.op)
			}
			if tc.op != "key" && (e.Project != testProject || res.Project != testProject) {
				t.Fatalf("project missing: err %+v res %+v", e, res)
			}
		})
	}
}

// TestVerifyWithoutConsume is a key that may not pull: Google answers
// testIamPermissions with 200 and leaves the permission out.
func TestVerifyWithoutConsume(t *testing.T) {
	f := newFake(t)
	f.handle("subcheck", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, 200, map[string]any{})
	})
	_, err := verify(t, f, keyJSON(nil))
	var e *Error
	if !errors.Is(err, ErrForbidden) || !errors.As(err, &e) || e.Op != OpSubscriptionCheck || e.Project != testProject {
		t.Fatalf("err = %+v, want forbidden on %s", err, OpSubscriptionCheck)
	}
	if len(f.records("spaces.list")) != 0 {
		t.Fatal("Chat was asked after the subscription check failed")
	}
}

func TestVerifyNetwork(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	_, err := Verify(context.Background(), VerifyOptions{Key: keyJSON(nil), BaseURL: url})
	if !errors.Is(err, ErrNetwork) {
		t.Fatalf("err = %v, want ErrNetwork", err)
	}
}
