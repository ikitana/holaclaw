package check

import (
	"context"
	"fmt"
	"path"

	"holaclaw/internal/config"
)

// pol001Rule: a blanket exec = "allow" runs any command with no gate at all;
// "ask" plus a narrow exec_allow is the intended shape (docs/security.md).
func pol001Rule() Rule {
	return Rule{
		ID:      "POL001",
		Summary: "agent policy has exec = allow",
		Run: func(_ context.Context, in *Input) []Finding {
			if in.Config == nil {
				return nil
			}
			var out []Finding
			for _, name := range sortedKeys(in.Config.Agents) {
				a := in.Config.Agents[name]
				if a.Policy.Exec != "allow" {
					continue
				}
				out = append(out, Finding{
					ID:       "POL001",
					Severity: SeverityWarning,
					File:     path.Join("agents", name, config.FileAgent),
					Key:      "policy.exec",
					Message:  "policy.exec = \"allow\" runs any command with no approval",
					Fix:      "set policy.exec = \"ask\" and list allowed commands in policy.exec_allow",
				})
			}
			return out
		},
	}
}

// pol002Rule: listing the exec tool while the exec class is denied costs
// prompt tokens for a tool the gate will never let run.
func pol002Rule() Rule {
	return Rule{
		ID:      "POL002",
		Summary: "agent lists the exec tool but policy exec = deny",
		Run: func(_ context.Context, in *Input) []Finding {
			if in.Config == nil {
				return nil
			}
			var out []Finding
			for _, name := range sortedKeys(in.Config.Agents) {
				a := in.Config.Agents[name]
				if a.Policy.Exec != "deny" || !contains(a.Tools, "exec") {
					continue
				}
				out = append(out, Finding{
					ID:       "POL002",
					Severity: SeverityInfo,
					File:     path.Join("agents", name, config.FileAgent),
					Key:      "tools",
					Message:  fmt.Sprintf("agent %q lists the exec tool, but policy.exec = \"deny\" means it can never run", name),
					Fix:      "remove \"exec\" from tools to save prompt tokens",
				})
			}
			return out
		},
	}
}
