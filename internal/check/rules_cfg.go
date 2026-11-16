package check

import (
	"context"

	"holaclaw/internal/config"
)

// cfgRule passes every config.Problem straight through as a Finding with the
// same code, file, key, message and hint. It is the only rule that produces
// more than one id: the ids are the CFGxxx codes documented in
// internal/config/problem.go.
func cfgRule() Rule {
	return Rule{
		ID:      "CFG",
		Summary: "config.Load / config.Validate problems, passed through unchanged",
		Run: func(_ context.Context, in *Input) []Finding {
			out := make([]Finding, 0, len(in.LoadProblems))
			for _, p := range in.LoadProblems {
				out = append(out, Finding{
					ID:       p.Code,
					Severity: cfgSeverity(p.Severity),
					File:     p.File,
					Key:      p.Key,
					Message:  p.Message,
					Fix:      p.Hint,
				})
			}
			return out
		},
	}
}

func cfgSeverity(s config.Severity) Severity {
	if s == config.SeverityWarning {
		return SeverityWarning
	}
	return SeverityError
}
