package record

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"holaclaw/internal/provider"
)

// deciderStub answers every Decide call with one canned response, and counts
// calls so a replay can be proved to have touched no decider at all.
type deciderStub struct {
	resp *provider.DecisionResponse
	err  error
	reqs []provider.DecisionRequest
}

func (s *deciderStub) Decide(_ context.Context, req provider.DecisionRequest) (*provider.DecisionResponse, error) {
	s.reqs = append(s.reqs, req)
	if s.err != nil {
		return nil, s.err
	}
	return s.resp, nil
}

func decisionReq(state string) provider.DecisionRequest {
	return provider.DecisionRequest{
		State: state,
		Questions: map[string]provider.Question{
			"safe": {
				Type: provider.QuestionNoul, Instructions: "is it safe",
				Criteria: map[string]string{"true": "yes", "false": "no"},
			},
			"kind": {
				Type: provider.QuestionChoice, Instructions: "what kind",
				Criteria: map[string]string{"benign": "b", "destructive": "d"},
			},
		},
	}
}

func TestDecisionDigestStableAcrossMapOrder(t *testing.T) {
	d1, shape1 := DecisionDigest(decisionReq("state"))

	// Same content, different map insertion order in both the question set
	// and a criteria set — Go randomises map iteration, so a digest that
	// forgot to sort would flake here.
	req2 := provider.DecisionRequest{
		State: "state",
		Questions: map[string]provider.Question{
			"kind": {Type: provider.QuestionChoice, Instructions: "what kind",
				Criteria: map[string]string{"destructive": "d", "benign": "b"}},
			"safe": {Type: provider.QuestionNoul, Instructions: "is it safe",
				Criteria: map[string]string{"false": "no", "true": "yes"}},
		},
	}
	d2, shape2 := DecisionDigest(req2)
	if d1 != d2 {
		t.Fatalf("digest changed with map order: %s vs %s", d1, d2)
	}
	if strings.Join(shape1.Questions, ",") != strings.Join(shape2.Questions, ",") {
		t.Fatalf("shape question order differs: %v vs %v", shape1.Questions, shape2.Questions)
	}

	if d3, _ := DecisionDigest(decisionReq("other state")); d3 == d1 {
		t.Fatal("digest did not change for a different state")
	}
	if d4, _ := DecisionDigest(provider.DecisionRequest{State: "state"}); d4 == d1 {
		t.Fatal("digest did not change when the questions were dropped")
	}
}

var recordedCost = provider.NanoUSD(200000)

// A recording made before the cost moved into Usage keeps it at the top
// level, a USD float; a replay still reads it, exactly.
func TestDecisionReplayReadsTheOldCost(t *testing.T) {
	dir := t.TempDir()
	path := Path(dir, "m1", "c1", 1)
	digest, shape := DecisionDigest(decisionReq("state"))
	run := DecisionRun{Steps: []DecisionStep{{Step: 1, Digest: digest, Shape: shape, Response: &DecisionResponse{
		Answers: map[string]provider.Answer{"safe": {Type: provider.QuestionNoul, Noul: 0.9}}, Cost: json.RawMessage("0.000046242"),
	}}}}
	raw, err := json.Marshal(run)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	pp, err := OpenDecider(ModeReplay, nil, path, DecisionRun{})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := pp.Decide(context.Background(), decisionReq("state"))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Usage.Cost == nil || *resp.Usage.Cost != 46242 {
		t.Fatalf("replayed usage %+v", resp.Usage)
	}
}

func TestDecisionRecordThenReplay(t *testing.T) {
	dir := t.TempDir()
	path := Path(dir, "m1", "c1", 1)
	live := &deciderStub{resp: &provider.DecisionResponse{
		Answers: map[string]provider.Answer{"safe": {Type: provider.QuestionNoul, Noul: 0.9, Confidence: 0.5}},
		Usage:   provider.Usage{PromptTokens: 5, CompletionTokens: 1, Cost: &recordedCost},
	}}

	rp, err := OpenDecider(ModeRecord, live, path, DecisionRun{Model: "m1", Case: "c1"})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := rp.Decide(context.Background(), decisionReq("state"))
	if err != nil {
		t.Fatal(err)
	}
	if *resp.Usage.Cost != recordedCost || resp.Answers["safe"].Noul != 0.9 {
		t.Fatalf("live resp = %+v", resp)
	}
	if err := rp.Save(); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"Authorization", "Bearer", "api_key", "http"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("recording leaks %q:\n%s", secret, raw)
		}
	}

	// Replay serves the same answer without touching the live decider.
	pp, err := OpenDecider(ModeReplay, nil, path, DecisionRun{})
	if err != nil {
		t.Fatal(err)
	}
	resp2, err := pp.Decide(context.Background(), decisionReq("state"))
	if err != nil {
		t.Fatal(err)
	}
	if resp2.Answers["safe"].Noul != 0.9 || resp2.Usage.Cost == nil || *resp2.Usage.Cost != recordedCost {
		t.Fatalf("replayed %+v", resp2)
	}
	if len(live.reqs) != 1 {
		t.Fatalf("replay touched the live decider (%d calls)", len(live.reqs))
	}

	// A second call has no recorded step left.
	if _, err := pp.Decide(context.Background(), decisionReq("state")); err == nil ||
		!strings.Contains(err.Error(), "re-record") {
		t.Fatalf("got %v, want an out-of-steps error", err)
	}
}

func TestDecisionReplayDigestMismatch(t *testing.T) {
	dir := t.TempDir()
	path := Path(dir, "m1", "c1", 1)
	live := &deciderStub{resp: &provider.DecisionResponse{Answers: map[string]provider.Answer{}}}
	rp, err := OpenDecider(ModeRecord, live, path, DecisionRun{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rp.Decide(context.Background(), decisionReq("state")); err != nil {
		t.Fatal(err)
	}
	if err := rp.Save(); err != nil {
		t.Fatal(err)
	}

	pp, err := OpenDecider(ModeReplay, nil, path, DecisionRun{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = pp.Decide(context.Background(), decisionReq("different state"))
	var me *DecisionMismatchError
	if !errors.As(err, &me) {
		t.Fatalf("got %v, want a DecisionMismatchError", err)
	}
	msg := me.Error()
	for _, want := range []string{"does not match this request", "state recorded", "state"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("mismatch hint %q lacks %q", msg, want)
		}
	}
}

func TestDecisionRecordError(t *testing.T) {
	dir := t.TempDir()
	path := Path(dir, "m1", "c1", 1)
	live := &deciderStub{err: &provider.Error{Kind: provider.ErrRateLimit, Provider: "stub"}}
	rp, err := OpenDecider(ModeRecord, live, path, DecisionRun{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rp.Decide(context.Background(), decisionReq("state")); err == nil {
		t.Fatal("want the decider error")
	}
	if err := rp.Save(); err != nil {
		t.Fatal(err)
	}
	pp, err := OpenDecider(ModeReplay, nil, path, DecisionRun{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = pp.Decide(context.Background(), decisionReq("state"))
	if provider.KindOf(err) != provider.ErrRateLimit {
		t.Fatalf("replayed error = %v, want a rate_limit provider error", err)
	}
}

func TestDecisionOpenErrors(t *testing.T) {
	if _, err := OpenDecider(ModeReplay, nil, filepath.Join(t.TempDir(), "missing.json"), DecisionRun{}); err == nil {
		t.Fatal("want an error for a missing recording")
	}
	if _, err := OpenDecider(ModeRecord, nil, "x.json", DecisionRun{}); err == nil {
		t.Fatal("want an error for record mode without a live decider")
	}
	bad := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(bad, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenDecider(ModeReplay, nil, bad, DecisionRun{}); err == nil {
		t.Fatal("want a parse error")
	}
}

func TestDecisionLiveModeDoesNotRecord(t *testing.T) {
	dir := t.TempDir()
	path := Path(dir, "m1", "c1", 1)
	live := &deciderStub{resp: &provider.DecisionResponse{Answers: map[string]provider.Answer{}}}
	rp, err := OpenDecider(ModeLive, live, path, DecisionRun{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rp.Decide(context.Background(), decisionReq("state")); err != nil {
		t.Fatal(err)
	}
	if err := rp.Save(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("live mode must not write a recording, stat err = %v", err)
	}
}
