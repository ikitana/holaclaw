package decisions

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"holaclaw/internal/provider"
)

const testKey = "sk-decisions-secret-key"

// newTestClient serves h and returns a client plus a model pointed at it.
func newTestClient(t *testing.T, h http.Handler, opts Options) (*Client, provider.Model) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	if opts.Name == "" {
		opts.Name = "openrouter"
	}
	if opts.APIKey == nil {
		opts.APIKey = func() string { return testKey }
	}
	return New(opts), provider.Model{
		Name: "jev", Provider: "openrouter", ID: "~typesafe/jev-latest",
		API: provider.APIDecisions, URL: srv.URL + "/api/alpha/decisions",
	}
}

// sampleQuestions is the measured request: one of each type.
func sampleQuestions() map[string]provider.Question {
	return map[string]provider.Question{
		"safe": {
			Type:         provider.QuestionNoul,
			Instructions: "Is the command safe to run?",
			Criteria:     map[string]string{"true": "safe", "false": "unsafe"},
		},
		"kind": {
			Type:         provider.QuestionChoice,
			Instructions: "Classify the command.",
			Criteria:     map[string]string{"benign": "reads only", "destructive": "writes or deletes"},
		},
		"sev": {
			Type:         provider.QuestionScore,
			Instructions: "How bad is the worst case?",
			Scale:        []string{"Trivial", "Annoying", "Critical"},
		},
	}
}

const sampleReply = `{"model":"typesafe/jev-1.13-20260917","answers":{
 "safe":{"type":"noul","noul":0},
 "kind":{"type":"choice","choice":"destructive","probabilities":{"destructive":1,"benign":0},"confidence":1},
 "sev":{"type":"score","score":0.85,"legend":{"0":"Trivial","1":"Annoying","2":"Critical"},"probabilities":{"0":0.18,"1":0.79,"2":0.03},"confidence":0.68}},
 "usage":{"input_tokens":426,"output_tokens":62,"cost":0.000017892},"id":"gen-dec-1","provider":"TypeSafe"}`

func TestDecideHappyPath(t *testing.T) {
	var gotPath, gotAuth, gotType string
	var gotBody map[string]any
	c, m := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		gotType = r.Header.Get("Content-Type")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = io.WriteString(w, sampleReply)
	}), Options{})

	resp, err := c.Decide(context.Background(), provider.DecisionRequest{
		Model: m, State: "rm -rf /", Questions: sampleQuestions(),
	})
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if gotPath != "/api/alpha/decisions" || gotAuth != "Bearer "+testKey || gotType != "application/json" {
		t.Errorf("path=%q auth=%q content-type=%q", gotPath, gotAuth, gotType)
	}
	if gotBody["model"] != "~typesafe/jev-latest" || gotBody["state"] != "rm -rf /" {
		t.Errorf("body = %+v", gotBody)
	}

	// Request shape: criteria is an object for noul/choice, an array for score.
	qs, _ := gotBody["questions"].(map[string]any)
	safe, _ := qs["safe"].(map[string]any)
	if safe["type"] != "noul" || safe["instructions"] == "" {
		t.Errorf("safe question = %+v", safe)
	}
	if _, ok := safe["criteria"].(map[string]any); !ok {
		t.Errorf("noul criteria = %T", safe["criteria"])
	}
	kind, _ := qs["kind"].(map[string]any)
	if _, ok := kind["criteria"].(map[string]any); !ok {
		t.Errorf("choice criteria = %T", kind["criteria"])
	}
	sev, _ := qs["sev"].(map[string]any)
	scale, ok := sev["criteria"].([]any)
	if !ok || len(scale) != 3 || scale[0] != "Trivial" {
		t.Errorf("score criteria = %#v", sev["criteria"])
	}

	if len(resp.Answers) != 3 {
		t.Fatalf("answers = %+v", resp.Answers)
	}
	if a := resp.Answers["safe"]; a.Type != provider.QuestionNoul || a.Noul != 0 {
		t.Errorf("safe = %+v", a)
	}
	if a := resp.Answers["kind"]; a.Choice != "destructive" || a.Confidence != 1 || a.Probabilities["benign"] != 0 {
		t.Errorf("kind = %+v", a)
	}
	if a := resp.Answers["sev"]; a.Score != 0.85 || a.Probabilities["1"] != 0.79 || a.Confidence != 0.68 {
		t.Errorf("sev = %+v", a)
	}
	if u := resp.Usage; u.PromptTokens != 426 || u.CompletionTokens != 62 || u.Cost == nil || *u.Cost != 17892 {
		t.Errorf("usage=%+v", resp.Usage)
	}
}

func TestDecideOmitsEmptyNoulCriteria(t *testing.T) {
	var gotBody map[string]any
	c, m := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = io.WriteString(w, `{"answers":{"safe":{"type":"noul","noul":0.5}}}`)
	}), Options{})

	_, err := c.Decide(context.Background(), provider.DecisionRequest{
		Model: m, State: "s",
		Questions: map[string]provider.Question{
			"safe": {Type: provider.QuestionNoul, Instructions: "safe?"},
		},
	})
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	qs, _ := gotBody["questions"].(map[string]any)
	safe, _ := qs["safe"].(map[string]any)
	if _, ok := safe["criteria"]; ok {
		t.Errorf("empty criteria was sent: %+v", safe)
	}
}

// newDeadClient returns a client whose endpoint would refuse a connection: any
// request that reaches it is a bug in local validation.
func newDeadClient(t *testing.T) (*Client, provider.Model) {
	t.Helper()
	c := New(Options{Name: "openrouter", APIKey: func() string { return testKey }})
	return c, provider.Model{ID: "m", URL: "http://127.0.0.1:1/decisions"}
}

func TestDecideLocalValidation(t *testing.T) {
	cases := []struct {
		name string
		req  provider.DecisionRequest
	}{
		{"no url", provider.DecisionRequest{
			Model:     provider.Model{ID: "m"},
			Questions: map[string]provider.Question{"a": {Type: provider.QuestionNoul, Instructions: "?"}},
		}},
		{"no questions", provider.DecisionRequest{}},
		{"unknown type", provider.DecisionRequest{
			Questions: map[string]provider.Question{"a": {Type: "vibes", Instructions: "?"}},
		}},
		{"no instructions", provider.DecisionRequest{
			Questions: map[string]provider.Question{"a": {Type: provider.QuestionNoul}},
		}},
		{"empty name", provider.DecisionRequest{
			Questions: map[string]provider.Question{"": {Type: provider.QuestionNoul, Instructions: "?"}},
		}},
		{"choice with one criterion", provider.DecisionRequest{
			Questions: map[string]provider.Question{"a": {
				Type: provider.QuestionChoice, Instructions: "?",
				Criteria: map[string]string{"only": "one"},
			}},
		}},
		{"choice with no criteria", provider.DecisionRequest{
			Questions: map[string]provider.Question{"a": {Type: provider.QuestionChoice, Instructions: "?"}},
		}},
		{"score with one scale entry", provider.DecisionRequest{
			Questions: map[string]provider.Question{"a": {
				Type: provider.QuestionScore, Instructions: "?", Scale: []string{"only"},
			}},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, m := newDeadClient(t)
			if tc.req.Model.URL == "" && tc.name != "no url" {
				tc.req.Model = m
			}
			_, err := c.Decide(context.Background(), tc.req)
			if provider.KindOf(err) != provider.ErrBadRequest {
				t.Fatalf("kind = %q (%v)", provider.KindOf(err), err)
			}
		})
	}
}

// decodeCase is one bad reply to a single noul/choice/score question.
type decodeCase struct {
	name  string
	qs    map[string]provider.Question
	reply string
}

func TestDecideResponseValidation(t *testing.T) {
	noul := map[string]provider.Question{"a": {Type: provider.QuestionNoul, Instructions: "?"}}
	choice := map[string]provider.Question{"a": {
		Type: provider.QuestionChoice, Instructions: "?",
		Criteria: map[string]string{"yes": "y", "no": "n"},
	}}
	score := map[string]provider.Question{"a": {
		Type: provider.QuestionScore, Instructions: "?", Scale: []string{"low", "mid", "high"},
	}}

	cases := []decodeCase{
		{"missing answer", noul, `{"answers":{}}`},
		{"no answers at all", noul, `{"usage":{"cost":1}}`},
		{"type mismatch", noul, `{"answers":{"a":{"type":"choice","choice":"yes"}}}`},
		{"noul missing", noul, `{"answers":{"a":{"type":"noul"}}}`},
		{"noul above one", noul, `{"answers":{"a":{"type":"noul","noul":1.5}}}`},
		{"noul negative", noul, `{"answers":{"a":{"type":"noul","noul":-0.1}}}`},
		{"confidence out of range", noul, `{"answers":{"a":{"type":"noul","noul":0.5,"confidence":2}}}`},
		{"choice not in criteria", choice, `{"answers":{"a":{"type":"choice","choice":"maybe"}}}`},
		{"choice empty", choice, `{"answers":{"a":{"type":"choice"}}}`},
		{"choice probability key unknown", choice,
			`{"answers":{"a":{"type":"choice","choice":"yes","probabilities":{"yes":0.9,"maybe":0.1}}}}`},
		{"choice probability out of range", choice,
			`{"answers":{"a":{"type":"choice","choice":"yes","probabilities":{"yes":1.4}}}}`},
		{"score missing", score, `{"answers":{"a":{"type":"score"}}}`},
		{"score above scale", score, `{"answers":{"a":{"type":"score","score":2.5}}}`},
		{"score negative", score, `{"answers":{"a":{"type":"score","score":-1}}}`},
		{"score probability index out of range", score,
			`{"answers":{"a":{"type":"score","score":1,"probabilities":{"3":1}}}}`},
		{"score probability index not a number", score,
			`{"answers":{"a":{"type":"score","score":1,"probabilities":{"high":1}}}}`},
		{"malformed json", noul, `{"answers":`},
		{"not json at all", noul, `<html>gateway</html>`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, m := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, tc.reply)
			}), Options{})
			resp, err := c.Decide(context.Background(), provider.DecisionRequest{Model: m, State: "s", Questions: tc.qs})
			if provider.KindOf(err) != provider.ErrServer {
				t.Fatalf("kind = %q (%v)", provider.KindOf(err), err)
			}
			// A reply that parsed comes back with its usage and no answers;
			// one that did not parse has nothing to report.
			if resp != nil && resp.Answers != nil {
				t.Fatalf("an invalid reply returned answers: %+v", resp.Answers)
			}
			if tc.name == "no answers at all" && (resp == nil || resp.Usage.Cost == nil || *resp.Usage.Cost != 1000000000) {
				t.Fatalf("usage of an invalid reply = %+v", resp)
			}
			// Never echo upstream answer text back.
			if strings.Contains(err.Error(), "maybe") || strings.Contains(err.Error(), "gateway") {
				t.Fatalf("error echoed upstream text: %v", err)
			}
		})
	}
}

func TestDecideIgnoresExtraAnswers(t *testing.T) {
	c, m := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"answers":{
			"a":{"type":"noul","noul":0.25},
			"b":{"type":"choice","choice":"whatever"}}}`)
	}), Options{})
	resp, err := c.Decide(context.Background(), provider.DecisionRequest{
		Model: m, State: "s",
		Questions: map[string]provider.Question{"a": {Type: provider.QuestionNoul, Instructions: "?"}},
	})
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if len(resp.Answers) != 1 || resp.Answers["a"].Noul != 0.25 {
		t.Fatalf("answers = %+v", resp.Answers)
	}
	if resp.Usage.Cost != nil {
		t.Errorf("a reply without a cost = %v, want nil", *resp.Usage.Cost)
	}
}

// The cost is read from its JSON text, exactly; one that is not a number, or
// is negative, leaves the call unpriced rather than failing it.
func TestDecideCost(t *testing.T) {
	cases := []struct {
		cost string
		want int64 // -1: not reported
	}{
		{`0`, 0},
		{`0.000017892`, 17892},
		{`1.7892e-05`, 17892},
		{`-0.1`, -1},
		{`"0.1"`, -1},
		{`null`, -1},
	}
	for _, tc := range cases {
		c, m := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `{"answers":{"a":{"type":"noul","noul":0.25}},"usage":{"cost":`+tc.cost+`}}`)
		}), Options{})
		resp, err := c.Decide(context.Background(), provider.DecisionRequest{
			Model: m, State: "s",
			Questions: map[string]provider.Question{"a": {Type: provider.QuestionNoul, Instructions: "?"}},
		})
		if err != nil {
			t.Fatalf("%s: decide: %v", tc.cost, err)
		}
		switch got := resp.Usage.Cost; {
		case tc.want < 0 && got != nil:
			t.Errorf("%s: cost = %d, want not reported", tc.cost, *got)
		case tc.want >= 0 && (got == nil || int64(*got) != tc.want):
			t.Errorf("%s: cost = %v, want %d", tc.cost, got, tc.want)
		}
	}
}

func TestDecideHTTPErrors(t *testing.T) {
	cases := []struct {
		status     int
		body       string
		retryAfter string
		want       provider.ErrorKind
		wantRetry  time.Duration
	}{
		{http.StatusBadRequest, `{"error":{"message":"unknown model","code":400}}`, "", provider.ErrBadRequest, 0},
		{http.StatusUnauthorized, `{"error":{"message":"no key","code":401}}`, "", provider.ErrAuth, 0},
		{http.StatusPaymentRequired, `{"error":{"message":"credits","code":402}}`, "", provider.ErrUsageLimit, 0},
		{http.StatusForbidden, `{"error":{"message":"nope","code":403}}`, "", provider.ErrAuth, 0},
		{http.StatusTooManyRequests, `{"error":{"message":"slow down","code":429}}`, "7", provider.ErrRateLimit, 7 * time.Second},
		{http.StatusInternalServerError, `{"error":{"message":"boom","code":500}}`, "", provider.ErrServer, 0},
		{http.StatusBadGateway, `upstream down`, "", provider.ErrServer, 0},
		{http.StatusTeapot, `{"error":{"message":"?","code":418}}`, "", provider.ErrBadRequest, 0},
	}
	for _, tc := range cases {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			c, m := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tc.retryAfter != "" {
					w.Header().Set("Retry-After", tc.retryAfter)
				}
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}), Options{})
			_, err := c.Decide(context.Background(), provider.DecisionRequest{
				Model: m, State: "s",
				Questions: map[string]provider.Question{"a": {Type: provider.QuestionNoul, Instructions: "?"}},
			})
			var pe *provider.Error
			if !asProviderError(err, &pe) {
				t.Fatalf("err = %v", err)
			}
			if pe.Kind != tc.want || pe.Status != tc.status || pe.RetryAfter != tc.wantRetry {
				t.Fatalf("kind=%q status=%d retry=%v", pe.Kind, pe.Status, pe.RetryAfter)
			}
		})
	}
}

func TestDecideCapsUpstreamMessage(t *testing.T) {
	long := strings.Repeat("x", 4000)
	c, m := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"message":"`+long+`","code":400}}`)
	}), Options{})
	_, err := c.Decide(context.Background(), provider.DecisionRequest{
		Model: m, State: "s",
		Questions: map[string]provider.Question{"a": {Type: provider.QuestionNoul, Instructions: "?"}},
	})
	var pe *provider.Error
	if !asProviderError(err, &pe) {
		t.Fatalf("err = %v", err)
	}
	if len(pe.Message) > MaxMessageBytes+len("…") {
		t.Fatalf("message is %d bytes", len(pe.Message))
	}
}

func TestDecideNeverLeaksKeyOrState(t *testing.T) {
	c, m := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		// A hostile upstream echoing the key and the state back at us.
		_, _ = io.WriteString(w, `{"error":{"message":"bad Authorization: Bearer `+testKey+` for secret-state","code":401}}`)
	}), Options{})
	_, err := c.Decide(context.Background(), provider.DecisionRequest{
		Model: m, State: "secret-state",
		Questions: map[string]provider.Question{"a": {Type: provider.QuestionNoul, Instructions: "?"}},
	})
	if strings.Contains(err.Error(), testKey) {
		t.Fatalf("error leaked the key: %v", err)
	}
	if strings.Contains(err.Error(), "secret-state") {
		t.Fatalf("error leaked the state: %v", err)
	}
}

func TestDecideTimeout(t *testing.T) {
	done := make(chan struct{})
	c, m := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-done:
		}
	}), Options{DefaultTimeout: 30 * time.Millisecond})
	t.Cleanup(func() { close(done) })

	_, err := c.Decide(context.Background(), provider.DecisionRequest{
		Model: m, State: "s",
		Questions: map[string]provider.Question{"a": {Type: provider.QuestionNoul, Instructions: "?"}},
	})
	if provider.KindOf(err) != provider.ErrTimeout {
		t.Fatalf("kind = %q (%v)", provider.KindOf(err), err)
	}
}

func TestDecideModelTimeoutWins(t *testing.T) {
	done := make(chan struct{})
	c, m := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-done:
		}
	}), Options{DefaultTimeout: time.Hour})
	t.Cleanup(func() { close(done) })
	m.Timeout = 30 * time.Millisecond

	_, err := c.Decide(context.Background(), provider.DecisionRequest{
		Model: m, State: "s",
		Questions: map[string]provider.Question{"a": {Type: provider.QuestionNoul, Instructions: "?"}},
	})
	if provider.KindOf(err) != provider.ErrTimeout {
		t.Fatalf("kind = %q (%v)", provider.KindOf(err), err)
	}
}

func TestDecideCallerCancel(t *testing.T) {
	done := make(chan struct{})
	c, m := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-done:
		}
	}), Options{})
	t.Cleanup(func() { close(done) })
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	_, err := c.Decide(ctx, provider.DecisionRequest{
		Model: m, State: "s",
		Questions: map[string]provider.Question{"a": {Type: provider.QuestionNoul, Instructions: "?"}},
	})
	if provider.KindOf(err) != provider.ErrNetwork {
		t.Fatalf("kind = %q (%v)", provider.KindOf(err), err)
	}
}

func TestDecideOversizedBody(t *testing.T) {
	c, m := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"answers":{"a":{"type":"noul","noul":0.5,"pad":"`)
		pad := strings.Repeat("y", 64<<10)
		for range (MaxResponseBytes / len(pad)) + 2 {
			_, _ = io.WriteString(w, pad)
		}
		_, _ = io.WriteString(w, `"}}}`)
	}), Options{})
	_, err := c.Decide(context.Background(), provider.DecisionRequest{
		Model: m, State: "s",
		Questions: map[string]provider.Question{"a": {Type: provider.QuestionNoul, Instructions: "?"}},
	})
	if provider.KindOf(err) != provider.ErrServer {
		t.Fatalf("kind = %q (%v)", provider.KindOf(err), err)
	}
}

func TestDecideDoesNotRetry(t *testing.T) {
	var calls int
	c, m := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":{"message":"boom","code":500}}`)
	}), Options{})
	_, _ = c.Decide(context.Background(), provider.DecisionRequest{
		Model: m, State: "s",
		Questions: map[string]provider.Question{"a": {Type: provider.QuestionNoul, Instructions: "?"}},
	})
	if calls != 1 {
		t.Fatalf("calls = %d", calls)
	}
}

func TestNewDefaults(t *testing.T) {
	c := New(Options{})
	if c.Name() != "decisions" || c.timeout != DefaultRequestTimeout || cap(c.sem) != DefaultMaxConcurrent {
		t.Fatalf("client = %+v", c)
	}
}

func TestDecideNoAuthHeaderWithoutKey(t *testing.T) {
	var auth string
	c, m := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		_, _ = io.WriteString(w, `{"answers":{"a":{"type":"noul","noul":0.5}}}`)
	}), Options{APIKey: func() string { return "" }})
	if _, err := c.Decide(context.Background(), provider.DecisionRequest{
		Model: m, State: "s",
		Questions: map[string]provider.Question{"a": {Type: provider.QuestionNoul, Instructions: "?"}},
	}); err != nil {
		t.Fatalf("decide: %v", err)
	}
	if auth != "" {
		t.Fatalf("authorization = %q", auth)
	}
}

// asProviderError is errors.As with the concrete type, kept short for tests.
func asProviderError(err error, target **provider.Error) bool {
	pe, ok := err.(*provider.Error)
	if ok {
		*target = pe
	}
	return ok
}

// A redirect is chosen by the upstream; the key and the state (tool argument
// excerpts) must not follow it (security review M6, finding 2).
func TestDecideNeverFollowsRedirects(t *testing.T) {
	var hits int
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits++ }))
	defer other.Close()
	c, m := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL, http.StatusTemporaryRedirect)
	}), Options{})
	_, err := c.Decide(context.Background(), provider.DecisionRequest{Model: m, State: "s", Questions: map[string]provider.Question{
		"q": {Type: provider.QuestionNoul, Instructions: "?"},
	}})
	if err == nil {
		t.Fatal("a redirect must be an error")
	}
	if hits != 0 {
		t.Fatalf("the redirect target was contacted %d times", hits)
	}
}
