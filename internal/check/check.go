// Package check implements `holaclaw check`: a fixed set of deterministic
// rules over a config directory, secrets file and data directory. Every rule
// has a stable id, a severity, an exact file/key and — where one exists — a
// copy-pasteable fix. No rule ever writes to the config directory or
// auth.toml; there is no `--fix` (ARCHITECTURE.md §1, §7). DAT001 is the one
// exception that touches disk at all: proving the data directory is
// writable means creating it and a probe file, exactly what the daemon
// itself would do on first run.
package check

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"holaclaw/internal/auth/secrets"
	"holaclaw/internal/config"
	"holaclaw/internal/provider"
	"holaclaw/internal/skill"
	"holaclaw/internal/store"
)

// Severity of a finding. Unlike config.Severity, check also has Info: a
// finding worth mentioning that never changes the exit code beyond 0.
type Severity string

const (
	SeverityError   Severity = "error"
	SeverityWarning Severity = "warning"
	SeverityInfo    Severity = "info"
)

// Finding is one thing a rule found: a stable id, where it is and how to fix
// it, mirroring config.Problem so `holaclaw check` and config errors read the
// same way.
type Finding struct {
	ID       string
	Severity Severity
	File     string // path relative to the config directory, or "-" for none
	Key      string // dotted key path, empty when the finding is file-level
	Message  string
	Fix      string // exact, copy-pasteable; empty when there is none
}

func (f Finding) String() string {
	var b strings.Builder
	b.WriteString(f.ID)
	b.WriteString(" ")
	b.WriteString(string(f.Severity))
	b.WriteString(": ")
	b.WriteString(f.File)
	if f.Key != "" {
		b.WriteString(": ")
		b.WriteString(f.Key)
	}
	b.WriteString(": ")
	b.WriteString(f.Message)
	if f.Fix != "" {
		b.WriteString(" (fix: ")
		b.WriteString(f.Fix)
		b.WriteString(")")
	}
	return b.String()
}

// Rule is one named check. Run returns every finding for that rule; a rule
// producing several distinct ids (the CFG pass-through) is the one exception
// to "one rule, one id" and is documented as such in RULES.md.
type Rule struct {
	ID      string
	Summary string
	Run     func(ctx context.Context, in *Input) []Finding
}

// Input is everything the rules need. Config and LoadProblems may describe a
// directory that failed to load: Config is filled as far as parsing got
// (config.Load's contract), and rules that need a field it doesn't have skip
// silently rather than panic.
type Input struct {
	ConfigDir string
	Config    *config.Config // may be partial or nil when loading failed
	// LoadProblems is every config.Problem found by config.Load and
	// config.Validate, merged by the caller.
	LoadProblems config.Problems
	Secrets      *secrets.File
	SecretsPath  string
	DataDir      string
	// Probe reports the context window a provider's runtime actually
	// allocated for a model (docs/models.md). nil skips MOD002 entirely —
	// the caller passes nil for an offline check.
	Probe func(ctx context.Context, providerName string, m provider.Model) (int, string, error)
	// KnownTools is the registered tool and group names, for rules that need
	// to reason about tool selection beyond what config.Validate already
	// checked.
	KnownTools []string
	// Specs returns the tool schemas an agent would actually get, connector
	// tools included, for BUD001 and BUD002.
	// nil means "no tools configured" rather than "skip the rule".
	Specs func(agent string) []provider.ToolSpec
	// Offline skips the rules that touch a socket (BRW004); the network
	// rules skip themselves by having no Probe.
	Offline bool
	// SkillRoots returns the two skill roots of an agent (app.SkillRoots);
	// nil skips every SKL rule.
	SkillRoots func(agent string) skill.Roots
	// Connectors are the connector cache rows of the store, for CON002.
	// nil means none were read: no database, or one that could not be.
	Connectors []store.Connector
}

// Rules returns every rule, in the fixed, documented order (RULES.md). The
// order is part of the contract: it is the primary sort key of Run's output.
func Rules() []Rule {
	return []Rule{
		cfgRule(),
		sec001Rule(), sec002Rule(), sec003Rule(), sec004Rule(), sec005Rule(), sec006Rule(), sec007Rule(),
		net001Rule(), net002Rule(), net003Rule(),
		acc001Rule(), acc002Rule(), acc003Rule(),
		pol001Rule(), pol002Rule(),
		mod001Rule(), mod002Rule(),
		bud001Rule(), bud002Rule(),
		dat001Rule(), dat002Rule(),
		brw001Rule(), brw002Rule(), brw003Rule(), brw004Rule(), brw005Rule(), brw006Rule(),
		con001Rule(), con002Rule(), con003Rule(), con004Rule(),
		sklProblemsRule(), skl001Rule(), skl002Rule(), skl006Rule(),
	}
}

// Run executes every rule not excluded by only/skip, against in. Findings are
// ordered deterministically: rule order first (Rules()'s order), then by
// file, then by key. only and skip match a finding id by exact value or by
// prefix (so "SEC" selects every SEC00x id); an empty only means "every
// rule".
func Run(ctx context.Context, in *Input, only, skip []string) []Finding {
	var out []Finding
	for _, r := range Rules() {
		fs := r.Run(ctx, in)
		sort.SliceStable(fs, func(i, j int) bool {
			if fs[i].File != fs[j].File {
				return fs[i].File < fs[j].File
			}
			return fs[i].Key < fs[j].Key
		})
		for _, f := range fs {
			if matches(f.ID, only, true) && !matches(f.ID, skip, false) {
				out = append(out, f)
			}
		}
	}
	return out
}

// matches reports whether id is selected by patterns: empty-and-"only" always
// matches (no filter given); otherwise a pattern matches by exact value or as
// a prefix followed directly by more of the id (so "SEC" matches "SEC001").
func matches(id string, patterns []string, emptyMatches bool) bool {
	if len(patterns) == 0 {
		return emptyMatches
	}
	for _, p := range patterns {
		if id == p || strings.HasPrefix(id, p) {
			return true
		}
	}
	return false
}

// ExitCode is `holaclaw check`'s process exit status: 0 when there is nothing
// or only info, 1 when the worst finding is a warning, 2 when any is an
// error (ARCHITECTURE.md §7).
func ExitCode(fs []Finding) int {
	warn := false
	for _, f := range fs {
		switch f.Severity {
		case SeverityError:
			return 2
		case SeverityWarning:
			warn = true
		}
	}
	if warn {
		return 1
	}
	return 0
}

// WriteText prints one line per finding, in the order given.
func WriteText(w io.Writer, fs []Finding) {
	if len(fs) == 0 {
		_, _ = fmt.Fprintln(w, "ok: no findings")
		return
	}
	for _, f := range fs {
		_, _ = fmt.Fprintln(w, f.String())
	}
}

// jsonFinding is the wire shape of a Finding: lower-case, stable field names.
type jsonFinding struct {
	ID       string   `json:"id"`
	Severity Severity `json:"severity"`
	File     string   `json:"file"`
	Key      string   `json:"key,omitempty"`
	Message  string   `json:"message"`
	Fix      string   `json:"fix,omitempty"`
}

// WriteJSON prints fs as a JSON array, in the order given.
func WriteJSON(w io.Writer, fs []Finding) error {
	out := make([]jsonFinding, len(fs))
	for i, f := range fs {
		out[i] = jsonFinding(f)
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}
