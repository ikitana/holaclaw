package openaicompat

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"holaclaw/internal/provider"
)

func TestModels(t *testing.T) {
	var got *http.Request
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Clone(context.Background())
		_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"gemma4:e2b"},{"id":""},{"id":"qwen3:8b"}]}`)
	}), Options{APIKey: func() string { return "sk-test" }})

	ids, err := c.Models(context.Background())
	if err != nil {
		t.Fatalf("models: %v", err)
	}
	if want := []string{"gemma4:e2b", "qwen3:8b"}; !reflect.DeepEqual(ids, want) {
		t.Errorf("ids = %v, want %v", ids, want)
	}
	if got.Method != http.MethodGet || got.URL.Path != "/v1/models" {
		t.Errorf("request = %s %s", got.Method, got.URL.Path)
	}
	if got.Header.Get("Authorization") != "Bearer sk-test" {
		t.Errorf("authorization = %q", got.Header.Get("Authorization"))
	}
}

func TestModelsFailures(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		kind   provider.ErrorKind
	}{
		{"not found", http.StatusNotFound, `{"error":"no such route"}`, provider.ErrBadRequest},
		{"unauthorized", http.StatusUnauthorized, `{"error":{"message":"bad key"}}`, provider.ErrAuth},
		{"server error", http.StatusInternalServerError, `oops`, provider.ErrServer},
		{"malformed body", http.StatusOK, `<html>not json</html>`, provider.ErrServer},
		{"no data", http.StatusOK, `{"models":["a"]}`, provider.ErrServer},
		{"empty list", http.StatusOK, `{"data":[]}`, provider.ErrServer},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}), Options{})
			ids, err := c.Models(context.Background())
			if provider.KindOf(err) != tc.kind || ids != nil {
				t.Fatalf("models = (%v, %v), want kind %q", ids, err, tc.kind)
			}
		})
	}
}

func TestModelsUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close() // nothing listens there any more
	c := New(Options{Name: "local", BaseURL: srv.URL + "/v1"})
	if _, err := c.Models(context.Background()); provider.KindOf(err) != provider.ErrNetwork {
		t.Fatalf("err = %v, want a network error", err)
	}
}
