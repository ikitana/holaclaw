// Package guard is the optional guard model of the policy pipeline
// (docs/models.md, docs/security.md): a classifier that scores how safe one tool call is
// to run unasked. It never sees a Deny, and what its verdict may change is
// decided by the gate from Policy.GuardMode, never here.
//
// Two backends, same verdict. A **chat** guard is one short, tool-less model
// request with a fixed system prompt and a compact record of the call; the
// answer is parsed leniently down to a single word and reported as a score of
// 1 or 0. A **decision** guard (Options.Decider, recommended) is one
// provider.Decider call carrying the same record and two narrow questions —
// `safe` (a noul: the probability that the call is safe to run without asking
// a human) and `kind` (one of a small closed set) — which gives a graded
// score and a sharper label for a fraction of the latency.
//
// The model's own text never leaves this package in either backend. A verdict
// label reaches a trace event and an approval prompt, and free text there
// could carry instructions injected through the arguments: every label is a
// fixed string from this package's closed set, and a `kind` that is not one of
// the constants below is reported as unparseable rather than passed through.
//
// # Privacy
//
// A cloud guard sends the excerpt off the machine. Options.SendArgs is the
// switch: with arguments (the default) the classifier sees what the call
// would actually do — paths, URLs, commands — which is what makes it useful,
// but those bytes reach the guard provider. Without them it sees only
// metadata (tool, what it does, risk class, run kind, role, argument size),
// which
// keeps everything local to the machine and makes the verdict much coarser:
// it can no longer tell `rm -rf /` from `rm -rf ./build`. A guard pointed at
// a loopback or otherwise trusted endpoint wants SendArgs; a guard pointed at
// a third-party API and fed untrusted workspaces may not.
package guard

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"time"

	"holaclaw/internal/provider"
	"holaclaw/internal/tool"
)

// Defaults for Options.
const (
	// DefaultTimeout bounds one classification on a chat guard, which may be a
	// reasoning model and spends seconds before its one word.
	DefaultTimeout = 8 * time.Second
	// DefaultDecideTimeout bounds one classification on a decision model. It is
	// short on purpose: the guard sits on the path of every tool call, the
	// measured round trip is ~0.4 s, and docs/security.md makes a timeout a deny in auto
	// mode — waiting longer only delays that.
	DefaultDecideTimeout = 2 * time.Second
	DefaultMaxExcerpt    = 1500 // bytes of arguments sent to the classifier
	// MaxAnswerTokens is the chat reply reserve. The answer is one word, but
	// reasoning models spend hidden tokens before it: with 16 every answer came
	// back empty (finish=length) and was read as "ask" (measured in the evals).
	MaxAnswerTokens = 512
)

// Reasons are the only strings a chat guard ever reports, and
// ReasonUnparseable is what either backend reports for an answer it cannot
// read. They are fixed labels, never model text: a reason reaches the trace,
// and model text could carry instructions read out of untrusted content.
const (
	ReasonAllow       = "guard: allow"
	ReasonAsk         = "guard: ask"
	ReasonUnparseable = "guard: unparseable"
)

// Options configures a guard. Model plus exactly one backend are required:
// Decider if set, else Provider.
type Options struct {
	Model    provider.Model
	Provider provider.Provider
	// Decider is the decision-model backend. When set it is used instead of
	// Provider and one classification is one Decide call (see decide.go).
	Decider provider.Decider
	// Timeout bounds one classification; 0 means DefaultDecideTimeout for the
	// decision backend and DefaultTimeout for the chat one.
	Timeout time.Duration
	// MaxExcerpt caps the argument excerpt in bytes; 0 means
	// DefaultMaxExcerpt. Ignored when SendArgs is off.
	MaxExcerpt int
	// SendArgs decides whether the arguments excerpt is sent at all. nil means
	// true: point it at a false to keep arguments on the machine and classify
	// from metadata alone (see the package comment).
	SendArgs *bool
	// Now is the clock the cache ages entries by; nil means time.Now.
	Now func() time.Time
}

// New builds a guard. The returned value is safe for concurrent use.
func New(o Options) tool.Guard {
	if o.Timeout <= 0 {
		o.Timeout = DefaultTimeout
		if o.Decider != nil {
			o.Timeout = DefaultDecideTimeout
		}
	}
	if o.MaxExcerpt <= 0 {
		o.MaxExcerpt = DefaultMaxExcerpt
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	sendArgs := true
	if o.SendArgs != nil {
		sendArgs = *o.SendArgs
	}
	return &guard{o: o, sendArgs: sendArgs, cache: newCache(cacheEntries, cacheTTL, o.Now)}
}

type guard struct {
	o        Options
	sendArgs bool
	cache    *cache
}

var _ tool.Guard = (*guard)(nil)

// Classify scores one call. A provider or timeout error is returned as is and
// never cached; what an error means is the gate's decision (docs/security.md: an ask in
// recommend mode, a deny in auto mode).
func (g *guard) Classify(ctx context.Context, c tool.Call, t tool.Tool) (tool.Verdict, error) {
	key := g.cacheKey(c, t)
	if v, ok := g.cache.get(key); ok {
		return v, nil
	}
	var (
		v   tool.Verdict
		err error
	)
	if g.o.Decider != nil {
		v, err = g.decide(ctx, c, t)
	} else {
		v, err = g.chat(ctx, c, t)
	}
	if err != nil {
		return tool.Verdict{Usage: v.Usage}, err // a reply that failed was still paid for
	}
	g.cache.put(key, v)
	return v, nil
}

// chat is the one-word backend: a tool-less request with the fixed system
// prompt, parsed down to allow (score 1) or anything else (score 0).
func (g *guard) chat(ctx context.Context, c tool.Call, t tool.Tool) (tool.Verdict, error) {
	req := provider.Request{
		Model:     g.o.Model,
		System:    SystemPrompt,
		Messages:  []provider.Message{provider.TextMessage(provider.RoleUser, g.record(c, t))},
		MaxTokens: MaxAnswerTokens,
	}
	ctx, cancel := context.WithTimeout(ctx, g.o.Timeout)
	defer cancel()

	stream, err := g.o.Provider.Chat(ctx, req)
	if err != nil {
		return tool.Verdict{}, err // no retry: the gate decides what a failure means
	}
	resp, err := provider.Collect(stream, nil)
	if err != nil {
		return tool.Verdict{}, err
	}
	d, reason := Parse(resp.Text)
	u := resp.Usage
	v := tool.Verdict{Label: reason, Usage: &u}
	if d == tool.Allow {
		v.Score = 1
	}
	return v, nil
}

// record is the state the classifier is shown: metadata, and the argument
// excerpt when SendArgs is on. Both backends send exactly this.
func (g *guard) record(c tool.Call, t tool.Tool) string {
	return buildRecord(c, t.Risk(), t.Spec().Description, g.sendArgs, g.o.MaxExcerpt)
}

// cacheKey identifies a call for the cache: what the model is actually shown
// that can change the verdict, plus the identity of the model that would
// answer, so switching backends or models never replays an old verdict.
// Arguments are canonicalised so that the same call formatted differently
// hits the same entry.
func (g *guard) cacheKey(c tool.Call, t tool.Tool) string {
	h := sha256.New()
	write := func(parts ...string) {
		for _, s := range parts {
			_, _ = h.Write([]byte(s))
			_, _ = h.Write([]byte{0})
		}
	}
	backend := "chat"
	if g.o.Decider != nil {
		backend = "decide"
	}
	write(backend, g.o.Model.Name, g.o.Model.Provider, g.o.Model.ID, g.o.Model.URL)
	// Role and run kind are shown to the model too: a verdict computed for the
	// owner must never be replayed for a guest or a detached run. Taint is not
	// part of the key because it is not part of the record any more (see
	// buildRecord): the same call would get the same answer either way, and
	// what the taint changes is the gate's, not the guard's.
	write(c.Name, string(t.Risk()), t.Spec().Description, c.Role, string(c.RunKind), canonicalArgs(c.Args))
	return hex.EncodeToString(h.Sum(nil))
}
