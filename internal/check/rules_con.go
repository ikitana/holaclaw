package check

import (
	"context"
	"fmt"
	"net/url"

	"holaclaw/internal/auth/secrets"
	"holaclaw/internal/config"
	"holaclaw/internal/store"
)

// con001Rule: an oauth connector without a grant cannot call its server; the
// daemon would only find out on the first call (docs/connectors.md §9).
// Every stored grant has an access token, so that key is the grant; a lone
// client_secret is left over from registration and is not one.
func con001Rule() Rule {
	return Rule{
		ID:      "CON001",
		Summary: "an oauth connector has no grant in auth.toml",
		Run: func(_ context.Context, in *Input) []Finding {
			if in.Config == nil {
				return nil
			}
			var out []Finding
			for _, id := range connectorIDs(in.Config) {
				if in.Config.Connectors[id].Auth != "oauth" ||
					secretHas(in.Secrets, secrets.Connectors, id, secrets.ConnectorAccessToken) {
					continue
				}
				out = append(out, Finding{
					ID:       "CON001",
					Severity: SeverityError,
					File:     config.FileConnectors,
					Key:      "connectors." + id + ".auth",
					Message:  fmt.Sprintf("connector %q has auth = \"oauth\" but no grant in auth.toml", id),
					Fix:      fmt.Sprintf("run `holaclaw connectors reconnect %s`", id),
				})
			}
			return out
		},
	}
}

// connectorIDs is cfg's connector ids, sorted, without the invalid ones: a
// bad id is CFG011 already, and printed raw in a key or fix it could carry
// control characters to the terminal.
func connectorIDs(cfg *config.Config) []string {
	var out []string
	for _, id := range sortedKeys(cfg.Connectors) {
		if config.ValidConnectorID(id) {
			out = append(out, id)
		}
	}
	return out
}

// con002Rule: the daemon's health check marks a connector expired when its
// refresh token is dead and degraded when checks keep failing; both are worth
// seeing before an agent's call fails. Only rows of configured connectors
// count: a row without config is the daemon's to clean up. Of the row, only
// Detail goes into the message: it is a closed set of daemon strings, while
// every other field may be server text.
func con002Rule() Rule {
	return Rule{
		ID:      "CON002",
		Summary: "a connector's cache row says expired or degraded",
		Run: func(_ context.Context, in *Input) []Finding {
			if in.Config == nil {
				return nil
			}
			rows := make(map[string]store.Connector, len(in.Connectors))
			for _, row := range in.Connectors {
				rows[row.ID] = row
			}
			var out []Finding
			for _, id := range connectorIDs(in.Config) {
				row, ok := rows[id]
				if !ok {
					continue
				}
				f := Finding{
					ID:       "CON002",
					Severity: SeverityWarning,
					File:     config.FileConnectors,
					Key:      "connectors." + row.ID,
				}
				switch row.State {
				case store.ConnectorExpired:
					f.Message = fmt.Sprintf("connector %q is expired: its sign-in can no longer be refreshed", row.ID)
					f.Fix = fmt.Sprintf("run `holaclaw connectors reconnect %s`", row.ID)
				case store.ConnectorDegraded:
					f.Message = fmt.Sprintf("connector %q is degraded", row.ID)
					if row.Detail != "" {
						f.Message += ": " + row.Detail
					}
					f.Fix = fmt.Sprintf("run `holaclaw connectors show %s`; reconnect if it persists", row.ID)
				default:
					continue
				}
				out = append(out, f)
			}
			return out
		},
	}
}

// con003Rule: a connector's results, and for an oauth one its bearer token,
// travel on every call. Over plain http to another machine they are readable
// on the wire, whatever the auth. A URL that does not parse, has no host or
// is not http at all is not this rule's: `connectors create` refuses it. A
// connector with allow_insecure_transport asked for exactly this, by hand
// or with --allow-insecure-transport (docs/connectors.md §11).
func con003Rule() Rule {
	return Rule{
		ID:      "CON003",
		Summary: "a connector URL is plain http on a non-loopback host",
		Run: func(_ context.Context, in *Input) []Finding {
			if in.Config == nil {
				return nil
			}
			var out []Finding
			for _, id := range connectorIDs(in.Config) {
				cn := in.Config.Connectors[id]
				if cn.AllowInsecureTransport {
					continue
				}
				raw := cn.URL
				u, err := url.Parse(raw)
				if err != nil || u.Scheme != "http" || u.Host == "" || isLoopback(u.Hostname()) {
					continue
				}
				out = append(out, Finding{
					ID:       "CON003",
					Severity: SeverityError,
					File:     config.FileConnectors,
					Key:      "connectors." + id + ".url",
					Message:  fmt.Sprintf("%q sends the connector's results and tokens in clear text", raw),
					Fix:      "use an https URL, or a loopback address",
				})
			}
			return out
		},
	}
}

// con004Rule: a trusted connector's results never arm the taint (§9), which
// switches off the escalation for everything it returns. That is a choice to
// make on purpose, so it is said on every run rather than set and forgotten.
func con004Rule() Rule {
	return Rule{
		ID:      "CON004",
		Summary: "a connector is trusted: its results never arm the taint",
		Run: func(_ context.Context, in *Input) []Finding {
			if in.Config == nil {
				return nil
			}
			var out []Finding
			for _, id := range connectorIDs(in.Config) {
				if !in.Config.Connectors[id].Trusted {
					continue
				}
				out = append(out, Finding{
					ID:       "CON004",
					Severity: SeverityInfo,
					File:     config.FileConnectors,
					Key:      "connectors." + id + ".trusted",
					Message:  fmt.Sprintf("connector %q is trusted: its results never arm the taint", id),
					Fix:      "confirm it, or set `trusted = false`",
				})
			}
			return out
		},
	}
}
