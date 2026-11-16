package openaicompat

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"testing"

	"holaclaw/internal/provider"
)

func TestEmbedReturnsInputOrder(t *testing.T) {
	var got embedRequest
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/embeddings" {
			t.Errorf("path = %q", r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		// Deliberately out of order.
		_, _ = io.WriteString(w, `{"data":[
			{"index":2,"embedding":[3,3]},
			{"index":0,"embedding":[1,1]},
			{"index":1,"embedding":[2,2]}]}`)
	}), Options{})

	vecs, usage, err := c.Embed(context.Background(), provider.Model{ID: "embed-1"}, []string{"a", "b", "c"})
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	if !reflect.DeepEqual(usage, provider.Usage{}) {
		t.Errorf("no usage object = %s, want zero", usageText(usage))
	}
	if len(got.Input) != 3 || got.Model != "embed-1" {
		t.Errorf("request = %+v", got)
	}
	for i, v := range vecs {
		if len(v) != 2 || v[0] != float32(i+1) {
			t.Fatalf("vector %d = %v", i, v)
		}
	}
}

// The reply's usage: prompt tokens, and OpenRouter's cost when it sends one.
// A cost that is absent stays nil; a reported zero stays zero.
func TestEmbedUsage(t *testing.T) {
	cases := []struct {
		name, usage string
		want        provider.Usage
	}{
		{"priced", `{"prompt_tokens":12,"total_tokens":12,"cost":0.000001}`, provider.Usage{PromptTokens: 12, Cost: nano(1000)}},
		{"exponent", `{"prompt_tokens":12,"total_tokens":12,"cost":1e-6}`, provider.Usage{PromptTokens: 12, Cost: nano(1000)}},
		{"free", `{"prompt_tokens":12,"total_tokens":12,"cost":0}`, provider.Usage{PromptTokens: 12, Cost: nano(0)}},
		{"null", `{"prompt_tokens":12,"total_tokens":12,"cost":null}`, provider.Usage{PromptTokens: 12}},
		{"unpriced", `{"prompt_tokens":12,"total_tokens":12}`, provider.Usage{PromptTokens: 12}},
	}
	for _, tc := range cases {
		c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `{"data":[{"index":0,"embedding":[1]}],"usage":`+tc.usage+`}`)
		}), Options{})
		_, usage, err := c.Embed(context.Background(), provider.Model{ID: "e"}, []string{"a"})
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if !reflect.DeepEqual(usage, tc.want) {
			t.Errorf("%s: usage = %s, want %s", tc.name, usageText(usage), usageText(tc.want))
		}
	}
}

func TestEmbedEmptyInput(t *testing.T) {
	c := New(Options{BaseURL: "http://127.0.0.1:1/v1"})
	vecs, _, err := c.Embed(context.Background(), provider.Model{ID: "e"}, nil)
	if err != nil || vecs != nil {
		t.Fatalf("vecs=%v err=%v", vecs, err)
	}
}

// A reply with the wrong number of vectors is an error, but it was answered:
// its usage comes back with the error.
func TestEmbedCountMismatchIsServerError(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"data":[{"index":0,"embedding":[1]}],"usage":{"prompt_tokens":4,"cost":0.1}}`)
	}), Options{})
	_, usage, err := c.Embed(context.Background(), provider.Model{ID: "e"}, []string{"a", "b"})
	if provider.KindOf(err) != provider.ErrServer {
		t.Fatalf("kind = %q (%v)", provider.KindOf(err), err)
	}
	if usage.PromptTokens != 4 || usage.Cost == nil || *usage.Cost != 100000000 {
		t.Errorf("usage = %s", usageText(usage))
	}
}

func TestEmbedHTTPError(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"message":"bad key"}}`)
	}), Options{})
	_, _, err := c.Embed(context.Background(), provider.Model{ID: "e"}, []string{"a"})
	if provider.KindOf(err) != provider.ErrAuth {
		t.Fatalf("kind = %q (%v)", provider.KindOf(err), err)
	}
}
