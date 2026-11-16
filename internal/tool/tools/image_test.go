package tools

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"holaclaw/internal/event"
	"holaclaw/internal/provider"
	"holaclaw/internal/tool"
)

// fakeImages is an ImageBackend with scripted answers and call counters.
type fakeImages struct {
	mu sync.Mutex

	resp  *provider.ImageResponse
	model string
	err   error
	// errResp is what comes back with err: a reply with only its usage.
	errResp *provider.ImageResponse

	takeOK  bool
	limit   int
	takeErr error

	takes     int
	generates int
	returns   int
	prompts   []string
}

func newFakeImages(mime string) *fakeImages {
	return &fakeImages{
		resp: &provider.ImageResponse{Data: []byte("PIXELDATA"), Mime: mime,
			Usage: provider.Usage{PromptTokens: 16, CompletionTokens: 715, Cost: &imageCost}},
		model:  "meta/muse-image",
		takeOK: true,
		limit:  20,
	}
}

func (f *fakeImages) GenerateImage(_ context.Context, _, prompt string) (*provider.ImageResponse, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.generates++
	f.prompts = append(f.prompts, prompt)
	if f.err != nil {
		return f.errResp, f.model, f.err
	}
	return f.resp, f.model, nil
}

func (f *fakeImages) TakeImage(context.Context, string) (bool, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.takes++
	return f.takeOK, f.limit, f.takeErr
}

func (f *fakeImages) ReturnImage(context.Context, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.returns++
}

func (f *fakeImages) counts() (takes, generates, returns int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.takes, f.generates, f.returns
}

// imageRig wires the tool with fakes and captures the events it emits.
type imageRig struct {
	tool   tool.Tool
	back   *fakeImages
	mu     sync.Mutex
	events []event.Event
}

func newImageRig(t *testing.T, back *fakeImages, blobs BlobPutter) *imageRig {
	t.Helper()
	rig := &imageRig{back: back}
	r := tool.NewRegistry()
	sink := event.SinkFunc(func(_ context.Context, e event.Event) {
		rig.mu.Lock()
		defer rig.mu.Unlock()
		rig.events = append(rig.events, e)
	})
	if err := RegisterImage(r, back, blobs, sink); err != nil {
		t.Fatalf("RegisterImage: %v", err)
	}
	tl, ok := r.Get("image.generate")
	if !ok {
		t.Fatal("image.generate is not registered")
	}
	rig.tool = tl
	return rig
}

func (rig *imageRig) run(t *testing.T, args string) tool.Result {
	t.Helper()
	res, err := rig.tool.Run(context.Background(), tool.Call{
		CallID: "c1", Name: "image.generate", Args: json.RawMessage(args),
		Agent: "muse", TraceID: event.NewID(), SessionID: event.NewID(),
	})
	if err != nil {
		t.Fatalf("image.generate returned a Go error: %v", err)
	}
	return res
}

func (rig *imageRig) only(t *testing.T) event.Event {
	t.Helper()
	rig.mu.Lock()
	defer rig.mu.Unlock()
	if len(rig.events) != 1 {
		t.Fatalf("got %d events, want 1", len(rig.events))
	}
	if rig.events[0].Type != event.ImageGenerated {
		t.Fatalf("event type %q, want %q", rig.events[0].Type, event.ImageGenerated)
	}
	return rig.events[0]
}

func (rig *imageRig) count() int {
	rig.mu.Lock()
	defer rig.mu.Unlock()
	return len(rig.events)
}

func TestImageGenerateHappyPath(t *testing.T) {
	cases := []struct {
		mime string
		ext  string
		kind event.Kind
	}{
		{"image/png", ".png", event.KindImage},
		{"image/jpeg", ".jpg", event.KindImage},
		{"image/webp", ".webp", event.KindImage},
	}
	for _, tc := range cases {
		t.Run(tc.mime, func(t *testing.T) {
			back, blobs := newFakeImages(tc.mime), &fakeBlobs{}
			rig := newImageRig(t, back, blobs)

			res := rig.run(t, `{"prompt":"  a red bicycle, watercolour  "}`)
			if res.IsError {
				t.Fatalf("image.generate: %s", res.Text)
			}
			if res.Tainted {
				t.Error("result is tainted; a generated image reads nothing")
			}
			if len(res.Attachments) != 1 {
				t.Fatalf("got %d attachments, want 1", len(res.Attachments))
			}
			a := res.Attachments[0]
			if !a.ID.Valid() {
				t.Errorf("attachment id %q is not a ULID", a.ID)
			}
			if a.BlobID == "" || a.SHA256 != a.BlobID {
				t.Errorf("blob id %q / sha256 %q", a.BlobID, a.SHA256)
			}
			if a.Name != "image"+tc.ext {
				t.Errorf("name = %q, want image%s", a.Name, tc.ext)
			}
			if a.MIME != tc.mime || a.Kind != tc.kind || a.Size != int64(len("PIXELDATA")) {
				t.Errorf("mime %q kind %q size %d", a.MIME, a.Kind, a.Size)
			}
			if string(blobs.body) != "PIXELDATA" || blobs.mime != tc.mime {
				t.Errorf("stored %q as %q", blobs.body, blobs.mime)
			}
			for _, want := range []string{a.Name, tc.mime, "9 bytes", "attached to your reply"} {
				if !strings.Contains(res.Text, want) {
					t.Errorf("text %q lacks %q", res.Text, want)
				}
			}
			// The trimmed prompt is what the backend sees.
			if got := back.prompts[0]; got != "a red bicycle, watercolour" {
				t.Errorf("prompt = %q, want it trimmed", got)
			}

			ev := rig.only(t)
			if ev.Duration <= 0 {
				t.Error("event has no duration")
			}
			var attrs map[string]any
			if err := json.Unmarshal(ev.Attrs, &attrs); err != nil {
				t.Fatalf("attrs: %v", err)
			}
			if attrs["model"] != "meta/muse-image" || attrs["mime"] != tc.mime {
				t.Errorf("attrs = %v", attrs)
			}
			if attrs["bytes"] != float64(9) || attrs["cost_nanousd"] != float64(imageCost) || attrs["prompt_bytes"] != float64(26) ||
				attrs["prompt_tokens"] != float64(16) || attrs["completion_tokens"] != float64(715) {
				t.Errorf("attrs = %v", attrs)
			}
			// The prompt must appear nowhere in the event.
			raw, err := json.Marshal(ev)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(raw), "bicycle") || strings.Contains(string(raw), "watercolour") {
				t.Errorf("event carries the prompt: %s", raw)
			}
		})
	}
}

var imageCost = provider.NanoUSD(10000000)

// An unknown price leaves no cost — the call is unpriced, not free — while
// the tokens are written either way, so the event still reads as a model
// call (event.UsageTypes).
func TestImageGenerateUnknownCostAttr(t *testing.T) {
	back := newFakeImages("image/webp")
	back.resp.Usage.Cost = nil
	rig := newImageRig(t, back, &fakeBlobs{})

	if res := rig.run(t, `{"prompt":"a cat"}`); res.IsError {
		t.Fatalf("image.generate: %s", res.Text)
	}
	e := rig.only(t)
	var attrs map[string]any
	if err := json.Unmarshal(e.Attrs, &attrs); err != nil {
		t.Fatal(err)
	}
	if _, ok := attrs["cost_nanousd"]; ok || attrs["prompt_tokens"] != float64(16) {
		t.Errorf("attrs = %v, want tokens and no cost", attrs)
	}
	if u := event.UsageOfEvent(e); u.UnpricedCalls != 1 {
		t.Errorf("counted as %+v, want one unpriced call", u)
	}
}

// A reply that held no usable image was still a call: the failure event
// carries its usage. A failure with no reply carries none.
func TestImageGenerateFailureUsage(t *testing.T) {
	back := newFakeImages("image/webp")
	back.err = &provider.Error{Kind: provider.ErrServer, Message: "unsupported image type"}
	back.errResp = &provider.ImageResponse{Usage: provider.Usage{PromptTokens: 16, Cost: &imageCost}}
	rig := newImageRig(t, back, &fakeBlobs{})
	if res := rig.run(t, `{"prompt":"a cat"}`); !res.IsError {
		t.Fatalf("image.generate succeeded: %s", res.Text)
	}
	if u := event.UsageOfEvent(rig.only(t)); u.PromptTokens != 16 || u.Cost == nil || *u.Cost != imageCost {
		t.Errorf("failure counted as %+v", u)
	}

	back = newFakeImages("image/webp")
	back.err = &provider.Error{Kind: provider.ErrTimeout}
	rig = newImageRig(t, back, &fakeBlobs{})
	_ = rig.run(t, `{"prompt":"a cat"}`)
	var attrs map[string]any
	if err := json.Unmarshal(rig.only(t).Attrs, &attrs); err != nil {
		t.Fatal(err)
	}
	if _, ok := attrs["prompt_tokens"]; ok {
		t.Errorf("a call without a reply carries usage: %v", attrs)
	}
}

func TestImageGenerateNameSanitising(t *testing.T) {
	cases := []struct {
		name string
		want string
	}{
		{"", "image.webp"},
		{"sunset", "sunset.webp"},
		{"  sunset  ", "sunset.webp"},
		{"../../etc/passwd", "passwd.webp"},
		{`C:\windows\evil`, "evil.webp"},
		{"cat.png", "cat.webp"},
		{"report.txt", "report.txt.webp"},
		{".hidden", "hidden.webp"},
		{"...", "image.webp"},
		{"bad\x00na\x07me", "badname.webp"},
		{strings.Repeat("x", 300), strings.Repeat("x", 120) + ".webp"},
	}
	for _, tc := range cases {
		rig := newImageRig(t, newFakeImages("image/webp"), &fakeBlobs{})
		args, err := json.Marshal(map[string]string{"prompt": "a cat", "name": tc.name})
		if err != nil {
			t.Fatal(err)
		}
		res := rig.run(t, string(args))
		if res.IsError {
			t.Fatalf("name %q: %s", tc.name, res.Text)
		}
		if got := res.Attachments[0].Name; got != tc.want {
			t.Errorf("name %q -> %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestImageGenerateBadPrompt(t *testing.T) {
	cases := map[string]string{
		"missing": `{}`,
		"empty":   `{"prompt":""}`,
		"blank":   `{"prompt":"   \n "}`,
		"long":    `{"prompt":"` + strings.Repeat("a", maxImagePromptBytes+1) + `"}`,
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			back := newFakeImages("image/webp")
			rig := newImageRig(t, back, &fakeBlobs{})
			res := rig.run(t, args)
			if !res.IsError {
				t.Fatalf("got a success: %s", res.Text)
			}
			takes, gens, _ := back.counts()
			if takes != 0 || gens != 0 {
				t.Errorf("backend was used: %d takes, %d generations", takes, gens)
			}
			if rig.count() != 0 {
				t.Errorf("bad input emitted %d events", rig.count())
			}
		})
	}
	// The long prompt is refused, not truncated: the model is told to shorten.
	rig := newImageRig(t, newFakeImages("image/webp"), &fakeBlobs{})
	res := rig.run(t, `{"prompt":"`+strings.Repeat("a", maxImagePromptBytes+1)+`"}`)
	if !strings.Contains(res.Text, "shorten") {
		t.Errorf("text = %q, want advice to shorten", res.Text)
	}
}

func TestImageGenerateBadArgs(t *testing.T) {
	back := newFakeImages("image/webp")
	rig := newImageRig(t, back, &fakeBlobs{})
	res := rig.run(t, `{"prompt":`)
	if !res.IsError || !strings.Contains(res.Text, "image.generate") {
		t.Fatalf("result = %+v", res)
	}
	if takes, gens, _ := back.counts(); takes != 0 || gens != 0 {
		t.Errorf("backend was used: %d takes, %d generations", takes, gens)
	}
}

func TestImageGenerateDailyLimit(t *testing.T) {
	back := newFakeImages("image/webp")
	back.takeOK, back.limit = false, 20
	rig := newImageRig(t, back, &fakeBlobs{})

	res := rig.run(t, `{"prompt":"a cat"}`)
	if !res.IsError {
		t.Fatalf("got a success: %s", res.Text)
	}
	if !strings.Contains(res.Text, "daily limit of 20 images") || !strings.Contains(res.Text, "tomorrow") {
		t.Errorf("text = %q", res.Text)
	}
	if _, gens, rets := back.counts(); gens != 0 || rets != 0 {
		t.Errorf("%d generations, %d returns; want none of either", gens, rets)
	}
}

func TestImageGenerateCounterError(t *testing.T) {
	back := newFakeImages("image/webp")
	back.takeErr = errors.New("db is gone")
	rig := newImageRig(t, back, &fakeBlobs{})

	res := rig.run(t, `{"prompt":"a cat"}`)
	if !res.IsError || strings.Contains(res.Text, "db is gone") {
		t.Fatalf("result = %+v", res)
	}
	if _, gens, _ := back.counts(); gens != 0 {
		t.Errorf("%d generations after a counter failure", gens)
	}
}

func TestImageGenerateBackendError(t *testing.T) {
	back := newFakeImages("image/webp")
	back.err = &provider.Error{
		Kind: provider.ErrRateLimit, Provider: "openrouter",
		Message: "slow down MARKER-a red bicycle",
	}
	rig := newImageRig(t, back, &fakeBlobs{})

	res := rig.run(t, `{"prompt":"a red bicycle"}`)
	if !res.IsError {
		t.Fatalf("got a success: %s", res.Text)
	}
	if !strings.Contains(res.Text, "rate_limit") || !strings.Contains(res.Text, "image service failed") {
		t.Errorf("text = %q", res.Text)
	}
	if strings.Contains(res.Text, "MARKER") || strings.Contains(res.Text, "openrouter") {
		t.Errorf("upstream message leaked into %q", res.Text)
	}
	if len(res.Attachments) != 0 {
		t.Errorf("failure attached %d files", len(res.Attachments))
	}
	if _, _, rets := back.counts(); rets != 1 {
		t.Errorf("ReturnImage called %d times, want 1", rets)
	}
	var attrs map[string]any
	if err := json.Unmarshal(rig.only(t).Attrs, &attrs); err != nil {
		t.Fatal(err)
	}
	if attrs["error"] != "rate_limit" || attrs["model"] != "meta/muse-image" {
		t.Errorf("attrs = %v", attrs)
	}
	if _, ok := attrs["bytes"]; ok {
		t.Errorf("failure recorded bytes: %v", attrs)
	}
}

func TestImageGeneratePlainError(t *testing.T) {
	back := newFakeImages("image/webp")
	back.err = errors.New("boom")
	rig := newImageRig(t, back, &fakeBlobs{})

	res := rig.run(t, `{"prompt":"a cat"}`)
	if !res.IsError || !strings.Contains(res.Text, "(failed)") || strings.Contains(res.Text, "boom") {
		t.Fatalf("text = %q", res.Text)
	}
}

func TestImageGenerateNoModel(t *testing.T) {
	back := newFakeImages("image/webp")
	back.err = ErrNoImageModel
	rig := newImageRig(t, back, &fakeBlobs{})

	res := rig.run(t, `{"prompt":"a cat"}`)
	if !res.IsError || !strings.Contains(res.Text, "no image model is configured") {
		t.Fatalf("text = %q", res.Text)
	}
	if _, _, rets := back.counts(); rets != 1 {
		t.Errorf("ReturnImage called %d times, want 1", rets)
	}
	var attrs map[string]any
	if err := json.Unmarshal(rig.only(t).Attrs, &attrs); err != nil {
		t.Fatal(err)
	}
	if attrs["error"] != "no_model" {
		t.Errorf("attrs = %v", attrs)
	}
}

// A blob failure keeps the use counted: the upstream was already paid.
func TestImageGenerateBlobFailure(t *testing.T) {
	back := newFakeImages("image/webp")
	rig := newImageRig(t, back, &fakeBlobs{err: errors.New("disk full")})

	res := rig.run(t, `{"prompt":"a cat"}`)
	if !res.IsError || strings.Contains(res.Text, "disk full") {
		t.Fatalf("result = %+v", res)
	}
	if len(res.Attachments) != 0 {
		t.Errorf("failure attached %d files", len(res.Attachments))
	}
	if _, _, rets := back.counts(); rets != 0 {
		t.Errorf("ReturnImage called %d times after a paid generation, want 0", rets)
	}
}

func TestImageGenerateEmptyImage(t *testing.T) {
	back := newFakeImages("image/webp")
	back.resp = &provider.ImageResponse{Mime: "image/webp"}
	rig := newImageRig(t, back, &fakeBlobs{})

	if res := rig.run(t, `{"prompt":"a cat"}`); !res.IsError {
		t.Fatalf("empty image succeeded: %s", res.Text)
	}
}

func TestRegisterImageValidation(t *testing.T) {
	if err := RegisterImage(tool.NewRegistry(), nil, &fakeBlobs{}, nil); err == nil {
		t.Error("a nil backend was accepted")
	}
	if err := RegisterImage(tool.NewRegistry(), newFakeImages("image/webp"), nil, nil); err == nil {
		t.Error("a nil blob store was accepted")
	}
	// A nil sink is allowed and must not panic.
	r := tool.NewRegistry()
	if err := RegisterImage(r, newFakeImages("image/webp"), &fakeBlobs{}, nil); err != nil {
		t.Fatalf("RegisterImage: %v", err)
	}
	tl, _ := r.Get("image.generate")
	if _, err := tl.Run(context.Background(), tool.Call{Name: "image.generate",
		Args: json.RawMessage(`{"prompt":"a cat"}`)}); err != nil {
		t.Fatalf("run with a nil sink: %v", err)
	}
}

// TestImageSchemaSize registers the tool in a real registry, which rejects
// anything over tool.MaxSchemaTokens, and logs the budget.
func TestImageSchemaSize(t *testing.T) {
	rig := newImageRig(t, newFakeImages("image/webp"), &fakeBlobs{})
	spec := rig.tool.Spec()
	n := tool.SchemaTokens(spec)
	t.Logf("image.generate %d tokens", n)
	if n > tool.MaxSchemaTokens {
		t.Errorf("%d tokens, cap is %d", n, tool.MaxSchemaTokens)
	}
	// The two-property JSON schema alone is 61 tokens and the name 5, so the
	// floor is ~66; this guards the description against creeping growth.
	if n > 110 {
		t.Errorf("%d tokens, the budget for this tool is 110", n)
	}
	if rig.tool.Risk() != tool.RiskExternal {
		t.Errorf("risk %q", rig.tool.Risk())
	}
}

// lockedBlobs is a BlobPutter safe for concurrent use (fakeBlobs is not).
type lockedBlobs struct {
	mu sync.Mutex
	n  int
}

func (b *lockedBlobs) PutBlob(_ context.Context, r io.Reader, _ string) (string, int64, error) {
	body, err := io.ReadAll(r)
	if err != nil {
		return "", 0, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.n++
	return strings.Repeat("cd", 32), int64(len(body)), nil
}

func TestImageGenerateConcurrent(t *testing.T) {
	back := newFakeImages("image/png")
	rig := newImageRig(t, back, &lockedBlobs{})

	var fails atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := rig.tool.Run(context.Background(), tool.Call{
				Name: "image.generate", Agent: "muse",
				Args: json.RawMessage(`{"prompt":"a cat","name":"cat"}`),
			})
			if err != nil || res.IsError || len(res.Attachments) != 1 {
				fails.Add(1)
			}
		}()
	}
	wg.Wait()
	if fails.Load() != 0 {
		t.Errorf("%d concurrent calls failed", fails.Load())
	}
	if takes, gens, _ := back.counts(); takes != 20 || gens != 20 {
		t.Errorf("%d takes, %d generations, want 20 of each", takes, gens)
	}
	if rig.count() != 20 {
		t.Errorf("%d events, want 20", rig.count())
	}
}

// A failure that can happen after the upstream rendered stays counted: a
// refund there would make the daily cap a no-op while every retry is billed
// (security review M7, finding 3).
func TestImageGenerateRefundsOnlyUnbilledFailures(t *testing.T) {
	for kind, want := range map[provider.ErrorKind]int{
		provider.ErrAuth: 1, provider.ErrRateLimit: 1, provider.ErrUsageLimit: 1, provider.ErrBadRequest: 1,
		provider.ErrServer: 0, provider.ErrTimeout: 0, provider.ErrNetwork: 0,
	} {
		back := newFakeImages("image/webp")
		back.err = &provider.Error{Kind: kind, Provider: "p"}
		rig := newImageRig(t, back, &fakeBlobs{})
		if res := rig.run(t, `{"prompt":"a cat"}`); !res.IsError {
			t.Fatalf("%s: got a success", kind)
		}
		if _, _, rets := back.counts(); rets != want {
			t.Errorf("%s: ReturnImage called %d times, want %d", kind, rets, want)
		}
	}
}
