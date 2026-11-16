package record

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"holaclaw/internal/provider"
)

// Decider records or replays provider.Decider calls: the decision-model twin
// of Provider above. The wire shape differs from chat (a State plus named
// Questions, one answer back, no streaming) so it gets its own Run/Step
// types, but the same directory layout, the same digest-then-serve
// contract, and the same fail-loud-on-a-missing-recording behaviour.

// DecisionRun is the recording of one (model × case) decision call.
type DecisionRun struct {
	Model string         `json:"model"`
	Case  string         `json:"case"`
	Steps []DecisionStep `json:"steps"`
}

// DecisionStep is one Decide call: the request digest plus what came back.
type DecisionStep struct {
	Step   int           `json:"step"`
	Digest string        `json:"digest"`
	Shape  DecisionShape `json:"shape"` // human hint for digest mismatches
	// Response is set instead of ErrorKind when the call succeeded.
	Response  *DecisionResponse `json:"response,omitempty"`
	ErrorKind string            `json:"error_kind,omitempty"`
}

// DecisionShape is the coarse form of a decision request, enough to point at
// what changed between a recording and a replay attempt.
type DecisionShape struct {
	Questions  []string `json:"questions"` // question names, sorted
	StateChars int      `json:"state_chars"`
	StateTail  string   `json:"state_tail"` // tail of the state, truncated
}

// DecisionResponse is the recorded provider.DecisionResponse. The cost is in
// Usage; Cost is where recordings made before that kept it, and is read
// only when Usage has none: a USD number, read as text (provider.WireCost).
type DecisionResponse struct {
	Answers map[string]provider.Answer `json:"answers"`
	Usage   provider.Usage             `json:"usage"`
	Cost    json.RawMessage            `json:"cost,omitempty"`
}

// Decider records or replays the Decide calls of one run.
type Decider struct {
	mode  Mode
	inner provider.Decider
	path  string
	run   DecisionRun
	n     int // calls served so far
}

var _ provider.Decider = (*Decider)(nil)

// OpenDecider builds the decider for one run. In replay mode inner may be nil
// and path must name an existing recording; in record mode path is where the
// recording is written by Save.
func OpenDecider(mode Mode, inner provider.Decider, path string, meta DecisionRun) (*Decider, error) {
	d := &Decider{mode: mode, inner: inner, path: path, run: meta}
	switch mode {
	case ModeReplay:
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("eval: no recording (run with --mode record): %w", err)
		}
		var r DecisionRun
		if err := json.Unmarshal(b, &r); err != nil {
			return nil, fmt.Errorf("eval: parse recording %s: %w", path, err)
		}
		d.run.Steps = r.Steps
	default:
		if inner == nil {
			return nil, fmt.Errorf("eval: %s mode needs a live decider", mode)
		}
		d.run.Steps = nil
	}
	return d, nil
}

// Decide serves one call from the recording, or makes it and records it.
func (d *Decider) Decide(ctx context.Context, req provider.DecisionRequest) (*provider.DecisionResponse, error) {
	digest, shape := DecisionDigest(req)
	d.n++
	if d.mode == ModeReplay {
		return d.replay(digest, shape)
	}
	resp, err := d.inner.Decide(ctx, req)
	if err != nil {
		d.record(DecisionStep{Step: d.n, Digest: digest, Shape: shape, ErrorKind: string(provider.KindOf(err))})
		return resp, err
	}
	if d.mode == ModeLive {
		return resp, nil
	}
	d.record(DecisionStep{Step: d.n, Digest: digest, Shape: shape, Response: &DecisionResponse{
		Answers: resp.Answers, Usage: resp.Usage,
	}})
	return resp, nil
}

// replay serves the n-th recorded response, refusing a changed request.
func (d *Decider) replay(digest string, shape DecisionShape) (*provider.DecisionResponse, error) {
	if d.n > len(d.run.Steps) {
		return nil, fmt.Errorf("eval: recording %s has %d steps, model asked for step %d; re-record",
			d.path, len(d.run.Steps), d.n)
	}
	st := d.run.Steps[d.n-1]
	if st.Digest != digest {
		return nil, &DecisionMismatchError{Path: d.path, Step: d.n, Want: st.Digest, Got: digest, WantShape: st.Shape, GotShape: shape}
	}
	if st.ErrorKind != "" {
		return nil, &provider.Error{Kind: provider.ErrorKind(st.ErrorKind), Provider: "replay", Message: "recorded failure"}
	}
	if st.Response == nil {
		return nil, fmt.Errorf("eval: recording %s step %d has neither response nor error", d.path, d.n)
	}
	u := st.Response.Usage
	if c := provider.WireCost(st.Response.Cost); u.Cost == nil && c != nil && *c != 0 {
		u.Cost = c
	}
	return &provider.DecisionResponse{Answers: st.Response.Answers, Usage: u}, nil
}

func (d *Decider) record(s DecisionStep) { d.run.Steps = append(d.run.Steps, s) }

// Save writes the recording (record mode only; a no-op otherwise).
func (d *Decider) Save() error {
	if d.mode != ModeRecord {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(d.path), 0o755); err != nil {
		return fmt.Errorf("eval: recording dir: %w", err)
	}
	b, err := json.MarshalIndent(d.run, "", "  ")
	if err != nil {
		return fmt.Errorf("eval: encode recording: %w", err)
	}
	if err := os.WriteFile(d.path, append(b, '\n'), 0o644); err != nil {
		return fmt.Errorf("eval: write recording: %w", err)
	}
	return nil
}

// DecisionMismatchError says the replayed request is no longer the recorded
// one.
type DecisionMismatchError struct {
	Path      string
	Step      int
	Want, Got string
	WantShape DecisionShape
	GotShape  DecisionShape
}

func (e *DecisionMismatchError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "eval: recording %s step %d does not match this request (re-record)\n", e.Path, e.Step)
	fmt.Fprintf(&b, "  recorded %s  questions [%s], %d state chars\n",
		short(e.Want), strings.Join(e.WantShape.Questions, " "), e.WantShape.StateChars)
	fmt.Fprintf(&b, "  now      %s  questions [%s], %d state chars",
		short(e.Got), strings.Join(e.GotShape.Questions, " "), e.GotShape.StateChars)
	if e.WantShape.StateTail != e.GotShape.StateTail {
		fmt.Fprintf(&b, "\n  state recorded: %q\n  state now:      %q", e.WantShape.StateTail, e.GotShape.StateTail)
	}
	return b.String()
}

// DecisionDigest hashes the part of a decision request that must not drift
// between record and replay: the state and the questions asked. Both the
// question names and each question's criteria keys are visited in sorted
// order, so map iteration order — Go randomises it — never changes the hash.
func DecisionDigest(req provider.DecisionRequest) (string, DecisionShape) {
	h := sha256.New()
	write := func(parts ...string) {
		for _, s := range parts {
			_, _ = io.WriteString(h, s)
			_, _ = h.Write([]byte{0})
		}
	}
	write("state", req.State)

	names := make([]string, 0, len(req.Questions))
	for name := range req.Questions {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		q := req.Questions[name]
		write("q", name, string(q.Type), q.Instructions)
		keys := make([]string, 0, len(q.Criteria))
		for k := range q.Criteria {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			write("crit", k, q.Criteria[k])
		}
		for _, s := range q.Scale {
			write("scale", s)
		}
	}
	return hex.EncodeToString(h.Sum(nil)), DecisionShape{
		Questions:  names,
		StateChars: len(req.State),
		StateTail:  tail(req.State, maxShapeText),
	}
}
