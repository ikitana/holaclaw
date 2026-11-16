package check

import (
	"context"
	"fmt"

	"holaclaw/internal/config"
)

// acc001Rule: open access plus a guest policy that allows exec or write is
// exactly the combination docs/identity.md refuses — an unauthenticated
// sender would get more than session-only, safe-tier tools.
func acc001Rule() Rule {
	return Rule{
		ID:      "ACC001",
		Summary: "channel has open dm/rooms while its agent's guest policy allows exec or write",
		Run: func(_ context.Context, in *Input) []Finding {
			if in.Config == nil {
				return nil
			}
			var out []Finding
			for _, name := range sortedKeys(in.Config.Channels) {
				ch := in.Config.Channels[name]
				if ch.Access.DM != "open" && ch.Access.Rooms != "open" {
					continue
				}
				a, ok := in.Config.Agents[ch.Agent]
				if !ok {
					continue // dangling ref: already reported as CFG006
				}
				g := a.Policy.Guest
				if g.Exec == "deny" && g.Write == "deny" && g.External == "deny" {
					continue
				}
				out = append(out, Finding{
					ID:       "ACC001",
					Severity: SeverityError,
					File:     config.FileChannels,
					Key:      "channels." + name + ".access",
					Message: fmt.Sprintf(
						"channel %q allows open access but agent %q's guest policy allows exec=%q, write=%q, external=%q",
						name, ch.Agent, g.Exec, g.Write, g.External),
					Fix: fmt.Sprintf(
						"set policy.guest.exec, policy.guest.write and policy.guest.external to \"deny\" in agents/%s/agent.toml, "+
							"or change channels.%s.access away from \"open\"", ch.Agent, name),
				})
			}
			return out
		},
	}
}

// acc002Rule: replying to every message in an open room is noisy and, on a
// paid model, expensive — it is allowed, but worth a warning.
func acc002Rule() Rule {
	return Rule{
		ID:      "ACC002",
		Summary: "reply = always with rooms = open",
		Run: func(_ context.Context, in *Input) []Finding {
			if in.Config == nil {
				return nil
			}
			var out []Finding
			for _, name := range sortedKeys(in.Config.Channels) {
				ch := in.Config.Channels[name]
				if ch.Access.Reply != "always" || ch.Access.Rooms != "open" {
					continue
				}
				out = append(out, Finding{
					ID:       "ACC002",
					Severity: SeverityWarning,
					File:     config.FileChannels,
					Key:      "channels." + name + ".access.reply",
					Message:  fmt.Sprintf("channel %q replies to every message in an open room: noisy and costly", name),
					Fix:      "set channels." + name + ".access.reply = \"mention\"",
				})
			}
			return out
		},
	}
}

// acc003Rule: Google Chat only delivers @-mentions in a space (docs/channels/
// googlechat.md §6), so reply = "always" on a googlechat channel can never
// fire in a room — it only looks configured.
func acc003Rule() Rule {
	return Rule{
		ID:      "ACC003",
		Summary: "reply = always on a googlechat channel",
		Run: func(_ context.Context, in *Input) []Finding {
			if in.Config == nil {
				return nil
			}
			var out []Finding
			for _, name := range sortedKeys(in.Config.Channels) {
				ch := in.Config.Channels[name]
				if ch.Type != "googlechat" || ch.Access.Reply != "always" {
					continue
				}
				out = append(out, Finding{
					ID:       "ACC003",
					Severity: SeverityWarning,
					File:     config.FileChannels,
					Key:      "channels." + name + ".access.reply",
					Message:  fmt.Sprintf("channel %q has reply = \"always\" but Chat only delivers @-mentions in spaces: it can never fire in a room", name),
					Fix:      "set channels." + name + ".access.reply = \"mention\"",
				})
			}
			return out
		},
	}
}
