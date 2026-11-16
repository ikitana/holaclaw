package googlechat

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

func TestEveryAPIKind(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   error
		kind   Kind
	}{
		{400, ErrBadRequest, KindBadRequest},
		{401, ErrAuth, KindAuth},
		{403, ErrForbidden, KindForbidden},
		{404, ErrNotFound, KindNotFound},
		{409, ErrConflict, KindConflict},
		{422, ErrBadRequest, KindBadRequest},
		{429, ErrRateLimited, KindRateLimited},
		{500, ErrServer, KindServer},
		{503, ErrServer, KindServer},
	} {
		f := newFake(t)
		f.handle("spaces.list", func(w http.ResponseWriter, _ *http.Request) {
			if tc.status == 429 {
				w.Header().Set("Retry-After", "7")
			}
			writeJSON(w, tc.status, googleErr(tc.status, "detail"))
		})
		a, _ := newAdapter(t, f, nil)
		err := a.call(context.Background(), request{op: "spaces.list", method: http.MethodGet,
			url: func(string) string { return a.chat + "/v1/spaces" }}, nil)
		var e *Error
		if !errors.Is(err, tc.want) || !errors.As(err, &e) || e.Kind != tc.kind || e.Status != tc.status || e.Project != testProject {
			t.Errorf("status %d: err = %#v", tc.status, err)
			continue
		}
		if tc.status == 429 && e.RetryAfter.Seconds() != 7 {
			t.Errorf("RetryAfter = %v", e.RetryAfter)
		}
	}
}

func TestMalformedSuccessIsServer(t *testing.T) {
	f := newFake(t)
	f.handle("spaces.list", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("<html>")) })
	a, _ := newAdapter(t, f, nil)
	var out map[string]any
	err := a.call(context.Background(), request{op: "spaces.list", method: http.MethodGet,
		url: func(string) string { return a.chat + "/v1/spaces" }}, &out)
	if !errors.Is(err, ErrServer) {
		t.Fatalf("err = %v", err)
	}
}

func TestErrorString(t *testing.T) {
	e := &Error{Op: "pull", Kind: KindForbidden, Status: 403, Message: "PERMISSION_DENIED: nope"}
	if got := e.Error(); got != "googlechat pull: forbidden (403): PERMISSION_DENIED: nope" {
		t.Fatalf("Error() = %q", got)
	}
	if !errors.Is(e, ErrForbidden) || errors.Is(e, ErrAuth) {
		t.Fatal("sentinel mismatch")
	}
	ctxErr := transportError(context.Background(), "pull", "p", context.DeadlineExceeded, func(s string) string { return s })
	if !errors.Is(ctxErr, ErrNetwork) || !errors.Is(ctxErr, context.DeadlineExceeded) {
		t.Fatalf("transport error lost its cause: %v", ctxErr)
	}
}
