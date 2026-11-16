package images

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"holaclaw/internal/provider"
)

const testKey = "sk-images-secret-key"

// Tiny fixtures: only the magic bytes matter to the sniffer.
var (
	pngBytes  = append([]byte("\x89PNG\r\n\x1a\n"), []byte("IHDRnot-a-real-image")...)
	jpegBytes = append([]byte{0xFF, 0xD8, 0xFF, 0xE0}, []byte("JFIF-ish payload")...)
	webpBytes = append([]byte("RIFF\x24\x00\x00\x00WEBP"), []byte("VP8 payload here")...)
	gifBytes  = []byte("GIF89a still an image, just not one we take")
)

// newTestClient serves h and returns a client plus a model pointed at it.
func newTestClient(t *testing.T, h http.Handler, opts Options) (*Client, provider.Model) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	if opts.BaseURL == "" {
		opts.BaseURL = srv.URL + "/api/v1"
	}
	if opts.Name == "" {
		opts.Name = "openrouter"
	}
	if opts.APIKey == nil {
		opts.APIKey = func() string { return testKey }
	}
	return New(opts), provider.Model{
		Name: "muse", Provider: "openrouter", ID: "meta/muse-image", API: provider.APIImages,
	}
}

// replyWith is the measured 200 body carrying img.
func replyWith(img []byte, enc *base64.Encoding, prefix string) string {
	return `{"created":1789809290,"data":[{"b64_json":"` + prefix + enc.EncodeToString(img) +
		`","media_type":"image/webp"}],"usage":{"prompt_tokens":16,"completion_tokens":715,"total_tokens":731,"cost":0.01}}`
}

func TestGenerateImageHappyPath(t *testing.T) {
	var gotPath, gotAuth, gotType string
	var gotBody wireRequest
	c, m := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		gotType = r.Header.Get("Content-Type")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = io.WriteString(w, replyWith(webpBytes, base64.StdEncoding, ""))
	}), Options{})

	resp, err := c.GenerateImage(context.Background(), provider.ImageRequest{Model: m, Prompt: "a cat"})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if gotPath != "/api/v1/images" || gotAuth != "Bearer "+testKey || gotType != "application/json" {
		t.Errorf("path=%q auth=%q content-type=%q", gotPath, gotAuth, gotType)
	}
	if gotBody.Model != "meta/muse-image" || gotBody.Prompt != "a cat" {
		t.Errorf("body = %+v", gotBody)
	}
	if resp.Mime != "image/webp" || string(resp.Data) != string(webpBytes) {
		t.Errorf("resp = mime %q len %d", resp.Mime, len(resp.Data))
	}
	if u := resp.Usage; u.PromptTokens != 16 || u.CompletionTokens != 715 || u.Cost == nil || *u.Cost != 10000000 {
		t.Errorf("usage = %+v", resp.Usage)
	}
}

// Both namings of the usage are read: OpenAI's input/output tokens with
// details, OpenRouter's prompt/completion tokens with a cost. What a reply
// leaves out stays nil.
func TestGenerateImageUsage(t *testing.T) {
	cached := 3
	fourCents := provider.NanoUSD(40000000)
	cases := []struct {
		name, usage string
		want        provider.Usage
	}{
		{"openai", `{"input_tokens":50,"output_tokens":4160,"total_tokens":4210,"input_tokens_details":{"text_tokens":40,"image_tokens":10,"cached_tokens":3}}`,
			provider.Usage{PromptTokens: 50, CompletionTokens: 4160, CachedTokens: &cached}},
		{"free", `{"prompt_tokens":16,"completion_tokens":715,"cost":0}`,
			provider.Usage{PromptTokens: 16, CompletionTokens: 715, Cost: new(provider.NanoUSD)}},
		{"exponent", `{"prompt_tokens":16,"completion_tokens":715,"cost":4e-2}`,
			provider.Usage{PromptTokens: 16, CompletionTokens: 715, Cost: &fourCents}},
		{"malformed", `{"prompt_tokens":16,"completion_tokens":715,"cost":"lots"}`,
			provider.Usage{PromptTokens: 16, CompletionTokens: 715}},
		{"none", `null`, provider.Usage{}},
	}
	for _, tc := range cases {
		c, m := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `{"data":[{"b64_json":"`+base64.StdEncoding.EncodeToString(pngBytes)+`"}],"usage":`+tc.usage+`}`)
		}), Options{})
		resp, err := c.GenerateImage(context.Background(), provider.ImageRequest{Model: m, Prompt: "x"})
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if !reflect.DeepEqual(resp.Usage, tc.want) {
			t.Errorf("%s: usage = %+v, want %+v", tc.name, resp.Usage, tc.want)
		}
	}
}

func TestGenerateImageModelURLOverridesBase(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = io.WriteString(w, replyWith(pngBytes, base64.StdEncoding, ""))
	}))
	t.Cleanup(srv.Close)
	c := New(Options{Name: "openrouter", BaseURL: "http://127.0.0.1:1/api/v1"})
	m := provider.Model{ID: "m", URL: srv.URL + "/custom/render"}
	if _, err := c.GenerateImage(context.Background(), provider.ImageRequest{Model: m, Prompt: "x"}); err != nil {
		t.Fatalf("generate: %v", err)
	}
	if gotPath != "/custom/render" {
		t.Fatalf("path = %q", gotPath)
	}
}

func TestGenerateImageMimeSniffing(t *testing.T) {
	cases := []struct {
		name string
		img  []byte
		want string
	}{
		{"png", pngBytes, "image/png"},
		{"jpeg", jpegBytes, "image/jpeg"},
		{"webp", webpBytes, "image/webp"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, m := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				// media_type always lies here; the bytes decide.
				_, _ = io.WriteString(w, replyWith(tc.img, base64.StdEncoding, ""))
			}), Options{})
			resp, err := c.GenerateImage(context.Background(), provider.ImageRequest{Model: m, Prompt: "x"})
			if err != nil {
				t.Fatalf("generate: %v", err)
			}
			if resp.Mime != tc.want {
				t.Fatalf("mime = %q, want %q", resp.Mime, tc.want)
			}
			// The standard sniffer agrees, webp included: our own table is a
			// closed set, not a workaround.
			if got := http.DetectContentType(tc.img); got != tc.want {
				t.Fatalf("http.DetectContentType = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestGenerateImageUnsupportedType(t *testing.T) {
	c, m := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, replyWith(gifBytes, base64.StdEncoding, ""))
	}), Options{})
	resp, err := c.GenerateImage(context.Background(), provider.ImageRequest{Model: m, Prompt: "x"})
	if provider.KindOf(err) != provider.ErrServer || !strings.Contains(err.Error(), "unsupported image type") {
		t.Fatalf("err = %v (kind %q)", err, provider.KindOf(err))
	}
	// The image was unusable but the call was made: its usage comes back.
	if resp == nil || resp.Data != nil || resp.Usage.Cost == nil || *resp.Usage.Cost != 10000000 {
		t.Fatalf("resp = %+v, want only the usage", resp)
	}
}

func TestGenerateImageBase64Variants(t *testing.T) {
	cases := []struct {
		name   string
		enc    *base64.Encoding
		prefix string
	}{
		{"std", base64.StdEncoding, ""},
		{"raw", base64.RawStdEncoding, ""},
		{"data uri", base64.StdEncoding, "data:image/webp;base64,"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, m := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, replyWith(webpBytes, tc.enc, tc.prefix))
			}), Options{})
			resp, err := c.GenerateImage(context.Background(), provider.ImageRequest{Model: m, Prompt: "x"})
			if err != nil {
				t.Fatalf("generate: %v", err)
			}
			if string(resp.Data) != string(webpBytes) {
				t.Fatalf("data = %q", resp.Data)
			}
		})
	}
}

func TestGenerateImageLocalValidation(t *testing.T) {
	cases := []struct {
		name string
		opts Options
		req  provider.ImageRequest
	}{
		{"empty prompt", Options{BaseURL: "http://127.0.0.1:1/api/v1"}, provider.ImageRequest{Prompt: ""}},
		{"prompt too long", Options{BaseURL: "http://127.0.0.1:1/api/v1"},
			provider.ImageRequest{Prompt: strings.Repeat("p", MaxPromptBytes+1)}},
		{"no endpoint", Options{}, provider.ImageRequest{Prompt: "x"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := New(tc.opts)
			_, err := c.GenerateImage(context.Background(), tc.req)
			if provider.KindOf(err) != provider.ErrBadRequest {
				t.Fatalf("kind = %q (%v)", provider.KindOf(err), err)
			}
		})
	}
}

func TestGenerateImagePromptAtCapIsAccepted(t *testing.T) {
	c, m := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, replyWith(pngBytes, base64.StdEncoding, ""))
	}), Options{})
	if _, err := c.GenerateImage(context.Background(), provider.ImageRequest{
		Model: m, Prompt: strings.Repeat("p", MaxPromptBytes),
	}); err != nil {
		t.Fatalf("generate: %v", err)
	}
}

func TestGenerateImageBadResponses(t *testing.T) {
	cases := []struct{ name, body string }{
		{"no data", `{"created":1,"data":[],"usage":{"cost":0.01}}`},
		{"no data field", `{"created":1}`},
		{"empty b64", `{"data":[{"b64_json":"","media_type":"image/webp"}]}`},
		{"not base64", `{"data":[{"b64_json":"!!! not base64 !!!"}]}`},
		{"decodes to nothing", `{"data":[{"b64_json":"===="}]}`},
		{"malformed json", `{"data":[`},
		{"html", `<html>proxy error</html>`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, m := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, tc.body)
			}), Options{})
			_, err := c.GenerateImage(context.Background(), provider.ImageRequest{Model: m, Prompt: "x"})
			if provider.KindOf(err) != provider.ErrServer {
				t.Fatalf("kind = %q (%v)", provider.KindOf(err), err)
			}
		})
	}
}

func TestGenerateImageHTTPErrors(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		body       string
		retryAfter string
		want       provider.ErrorKind
		wantRetry  time.Duration
	}{
		{"zod", http.StatusBadRequest,
			`{"success":false,"error":{"name":"ZodError","message":"prompt: Required"}}`, "",
			provider.ErrBadRequest, 0},
		{"auth", http.StatusUnauthorized, `{"error":{"message":"no key","code":401}}`, "", provider.ErrAuth, 0},
		{"credits", http.StatusPaymentRequired, `{"error":{"message":"credits","code":402}}`, "", provider.ErrUsageLimit, 0},
		{"forbidden", http.StatusForbidden, `{"error":{"message":"nope","code":403}}`, "", provider.ErrAuth, 0},
		{"not found", http.StatusNotFound, `{"error":{"message":"No endpoints found","code":404}}`, "", provider.ErrBadRequest, 0},
		{"rate limit", http.StatusTooManyRequests, `{"error":{"message":"slow","code":429}}`, "3",
			provider.ErrRateLimit, 3 * time.Second},
		{"server", http.StatusInternalServerError, `{"error":{"message":"boom","code":500}}`, "", provider.ErrServer, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, m := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tc.retryAfter != "" {
					w.Header().Set("Retry-After", tc.retryAfter)
				}
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}), Options{})
			_, err := c.GenerateImage(context.Background(), provider.ImageRequest{Model: m, Prompt: "x"})
			pe, ok := err.(*provider.Error)
			if !ok {
				t.Fatalf("err = %v", err)
			}
			if pe.Kind != tc.want || pe.Status != tc.status || pe.RetryAfter != tc.wantRetry {
				t.Fatalf("kind=%q status=%d retry=%v", pe.Kind, pe.Status, pe.RetryAfter)
			}
			if pe.Message == "" {
				t.Errorf("no message: %v", pe)
			}
		})
	}
}

func TestGenerateImageZodMessageIsKept(t *testing.T) {
	c, m := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"success":false,"error":{"name":"ZodError","message":"prompt: Required"}}`)
	}), Options{})
	_, err := c.GenerateImage(context.Background(), provider.ImageRequest{Model: m, Prompt: "x"})
	if !strings.Contains(err.Error(), "prompt: Required") {
		t.Fatalf("err = %v", err)
	}
}

func TestGenerateImageCapsUpstreamMessage(t *testing.T) {
	c, m := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"message":"`+strings.Repeat("x", 5000)+`","code":400}}`)
	}), Options{})
	_, err := c.GenerateImage(context.Background(), provider.ImageRequest{Model: m, Prompt: "x"})
	pe, ok := err.(*provider.Error)
	if !ok {
		t.Fatalf("err = %v", err)
	}
	if len(pe.Message) > MaxMessageBytes+len("…") {
		t.Fatalf("message is %d bytes", len(pe.Message))
	}
}

func TestGenerateImageNeverLeaksKeyOrPrompt(t *testing.T) {
	c, m := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"message":"bad Authorization: Bearer `+testKey+`","code":401}}`)
	}), Options{})
	_, err := c.GenerateImage(context.Background(), provider.ImageRequest{Model: m, Prompt: "secret prompt"})
	if strings.Contains(err.Error(), testKey) {
		t.Fatalf("error leaked the key: %v", err)
	}
	if strings.Contains(err.Error(), "secret prompt") {
		t.Fatalf("error leaked the prompt: %v", err)
	}
}

func TestGenerateImageOversizedImage(t *testing.T) {
	// A payload that decodes past MaxImageBytes but stays under the body cap.
	big := strings.Repeat("QUJD", (MaxImageBytes/3)+16) // "ABC" repeated
	c, m := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"data":[{"b64_json":"`+big+`"}],"usage":{"cost":0.01}}`)
	}), Options{})
	_, err := c.GenerateImage(context.Background(), provider.ImageRequest{Model: m, Prompt: "x"})
	if provider.KindOf(err) != provider.ErrServer || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("err = %v (kind %q)", err, provider.KindOf(err))
	}
}

func TestGenerateImageOversizedBody(t *testing.T) {
	c, m := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"data":[{"b64_json":"`)
		pad := strings.Repeat("A", 1<<20)
		for range (MaxResponseBytes / len(pad)) + 2 {
			_, _ = io.WriteString(w, pad)
		}
		_, _ = io.WriteString(w, `"}]}`)
	}), Options{})
	_, err := c.GenerateImage(context.Background(), provider.ImageRequest{Model: m, Prompt: "x"})
	if provider.KindOf(err) != provider.ErrServer || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("err = %v (kind %q)", err, provider.KindOf(err))
	}
}

func TestGenerateImageTimeout(t *testing.T) {
	done := make(chan struct{})
	c, m := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-done:
		}
	}), Options{DefaultTimeout: 30 * time.Millisecond})
	t.Cleanup(func() { close(done) })

	_, err := c.GenerateImage(context.Background(), provider.ImageRequest{Model: m, Prompt: "x"})
	if provider.KindOf(err) != provider.ErrTimeout {
		t.Fatalf("kind = %q (%v)", provider.KindOf(err), err)
	}
}

func TestGenerateImageModelTimeoutWins(t *testing.T) {
	done := make(chan struct{})
	c, m := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-done:
		}
	}), Options{DefaultTimeout: time.Hour})
	t.Cleanup(func() { close(done) })
	m.Timeout = 30 * time.Millisecond

	_, err := c.GenerateImage(context.Background(), provider.ImageRequest{Model: m, Prompt: "x"})
	if provider.KindOf(err) != provider.ErrTimeout {
		t.Fatalf("kind = %q (%v)", provider.KindOf(err), err)
	}
}

func TestGenerateImageDoesNotRetry(t *testing.T) {
	var calls int
	c, m := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":{"message":"boom","code":500}}`)
	}), Options{})
	_, _ = c.GenerateImage(context.Background(), provider.ImageRequest{Model: m, Prompt: "x"})
	if calls != 1 {
		t.Fatalf("calls = %d", calls)
	}
}

func TestGenerateImageNoAuthHeaderWithoutKey(t *testing.T) {
	var auth string
	c, m := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		_, _ = io.WriteString(w, replyWith(pngBytes, base64.StdEncoding, ""))
	}), Options{APIKey: func() string { return "" }})
	if _, err := c.GenerateImage(context.Background(), provider.ImageRequest{Model: m, Prompt: "x"}); err != nil {
		t.Fatalf("generate: %v", err)
	}
	if auth != "" {
		t.Fatalf("authorization = %q", auth)
	}
}

func TestNewDefaults(t *testing.T) {
	c := New(Options{BaseURL: "http://x/api/v1/"})
	if c.Name() != "images" || c.timeout != DefaultRequestTimeout || cap(c.sem) != DefaultMaxConcurrent {
		t.Fatalf("client = %+v", c)
	}
	if got := c.endpoint(provider.Model{}); got != "http://x/api/v1/images" {
		t.Fatalf("endpoint = %q", got)
	}
}

// A redirect is chosen by the upstream; the key and the prompt must not
// follow it (security review M7, finding 2).
func TestGenerateImageNeverFollowsRedirects(t *testing.T) {
	var hits int
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits++ }))
	defer other.Close()
	c, m := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL, http.StatusTemporaryRedirect)
	}), Options{})
	if _, err := c.GenerateImage(context.Background(), provider.ImageRequest{Model: m, Prompt: "a cat"}); err == nil {
		t.Fatal("a redirect must be an error")
	}
	if hits != 0 {
		t.Fatalf("the redirect target was contacted %d times", hits)
	}
}
