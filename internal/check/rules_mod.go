package check

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"holaclaw/internal/config"
	"holaclaw/internal/provider"
	"holaclaw/internal/provider/openaicompat"
)

// MinContextTokens is the smallest model.context worth configuring; smaller
// windows leave no room for tool schemas or a useful reply.
const MinContextTokens = 4096

// ProbeTimeout bounds each MOD002 and MOD003 network probe.
const ProbeTimeout = 5 * time.Second

// ErrNoModelList is what Input.Models returns for a provider that cannot be
// asked for its models (a codex instance that is not signed in): MOD003 has
// nothing to say about it.
var ErrNoModelList = errors.New("check: the provider has no model list")

// maxListedModels caps the ids a MOD003 message quotes: a gateway serves
// hundreds.
const maxListedModels = 20

// mod001Rule: a context window under MinContextTokens is nearly always a
// mistake — there is barely room for the fixed prompt overhead, let alone a
// conversation.
func mod001Rule() Rule {
	return Rule{
		ID:      "MOD001",
		Summary: "model.context is under 4096",
		Run: func(_ context.Context, in *Input) []Finding {
			if in.Config == nil {
				return nil
			}
			var out []Finding
			for _, name := range sortedKeys(in.Config.Models) {
				m := in.Config.Models[name]
				if m.Context <= 0 || m.Context >= MinContextTokens {
					continue
				}
				out = append(out, Finding{
					ID:       "MOD001",
					Severity: SeverityWarning,
					File:     config.FileConfig,
					Key:      "models." + name + ".context",
					Message:  fmt.Sprintf("context %d is under %d tokens", m.Context, MinContextTokens),
					Fix:      fmt.Sprintf("raise models.%s.context, or pick a model with more room", name),
				})
			}
			return out
		},
	}
}

// mod002Rule: compares the configured context against what the runtime
// actually allocated (Ollama num_ctx, llama.cpp n_ctx) for every
// openai-compatible model, probed concurrently. in.Probe == nil skips the
// rule entirely — there is no way to reach the network.
func mod002Rule() Rule {
	return Rule{
		ID:      "MOD002",
		Summary: "configured context is larger than what the runtime actually allocated",
		Run: func(ctx context.Context, in *Input) []Finding {
			if in.Config == nil || in.Probe == nil {
				return nil
			}
			names := sortedKeys(in.Config.Models)
			results := make([]Finding, len(names))
			var wg sync.WaitGroup
			for i, name := range names {
				m := in.Config.Models[name]
				p, ok := in.Config.Providers[m.Provider]
				if !ok || p.Type != "openai-compatible" {
					continue
				}
				pm, err := in.Config.Model(name)
				if err != nil {
					continue
				}
				wg.Add(1)
				go func(i int, name string, configured int, providerName string, pm provider.Model) {
					defer wg.Done()
					pctx, cancel := context.WithTimeout(ctx, ProbeTimeout)
					defer cancel()
					allocated, _, err := in.Probe(pctx, providerName, pm)
					results[i] = mod002Finding(name, providerName, configured, allocated, err)
				}(i, name, m.Context, m.Provider, pm)
			}
			wg.Wait()
			var out []Finding
			for _, f := range results {
				if f.ID != "" {
					out = append(out, f)
				}
			}
			return out
		},
	}
}

func mod002Finding(name, providerName string, configured, allocated int, err error) Finding {
	switch {
	case errors.Is(err, openaicompat.ErrProbeUnsupported):
		return Finding{}
	case err != nil:
		return Finding{
			ID:       "MOD002",
			Severity: SeverityInfo,
			File:     config.FileConfig,
			Key:      "models." + name + ".context",
			Message:  fmt.Sprintf("could not reach %s: %s", providerName, err),
		}
	case configured > allocated:
		return Finding{
			ID:       "MOD002",
			Severity: SeverityError,
			File:     config.FileConfig,
			Key:      "models." + name + ".context",
			Message:  fmt.Sprintf("configured context %d is larger than the %d the runtime allocated", configured, allocated),
			Fix: fmt.Sprintf("raise num_ctx (or n_ctx) on %s to at least %d, or lower models.%s.context to %d",
				providerName, configured, name, allocated),
		}
	default:
		return Finding{}
	}
}

// mod003Rule: asks every provider for the model ids it serves, concurrently
// and once each, and reports the configured ids that are not among them. A
// model with its own url is not served at the provider's base_url, and is
// left out. in.Models == nil skips the rule entirely, as for MOD002.
func mod003Rule() Rule {
	return Rule{
		ID:      "MOD003",
		Summary: "the provider does not serve a configured model id",
		Run: func(ctx context.Context, in *Input) []Finding {
			if in.Config == nil || in.Models == nil {
				return nil
			}
			byProvider := map[string][]string{}
			for _, name := range sortedKeys(in.Config.Models) {
				m := in.Config.Models[name]
				if _, ok := in.Config.Providers[m.Provider]; !ok || m.ID == "" || m.URL != "" {
					continue
				}
				byProvider[m.Provider] = append(byProvider[m.Provider], name)
			}
			providers := sortedKeys(byProvider)
			results := make([][]Finding, len(providers))
			var wg sync.WaitGroup
			for i, providerName := range providers {
				wg.Add(1)
				go func(i int, providerName string) {
					defer wg.Done()
					pctx, cancel := context.WithTimeout(ctx, ProbeTimeout)
					defer cancel()
					ids, live, err := in.Models(pctx, providerName)
					results[i] = mod003Findings(in.Config, providerName, byProvider[providerName], ids, live, err)
				}(i, providerName)
			}
			wg.Wait()
			return slices.Concat(results...)
		},
	}
}

func mod003Findings(cfg *config.Config, providerName string, models, ids []string, live bool, err error) []Finding {
	switch {
	case errors.Is(err, ErrNoModelList):
		return nil
	case err != nil:
		return []Finding{{
			ID:       "MOD003",
			Severity: SeverityInfo,
			File:     config.FileConfig,
			Key:      "providers." + providerName,
			Message:  fmt.Sprintf("could not list the models of %s: %s", providerName, err),
		}}
	}
	var out []Finding
	for _, name := range models {
		id := cfg.Models[name].ID
		// Ollama serves a name without a tag as its `latest`.
		if slices.Contains(ids, id) || slices.Contains(ids, id+":latest") {
			continue
		}
		f := Finding{
			ID:       "MOD003",
			Severity: SeverityError,
			File:     config.FileConfig,
			Key:      "models." + name + ".id",
			Message:  fmt.Sprintf("%s does not serve %q; it serves: %s", providerName, id, listModels(ids)),
			Fix:      fmt.Sprintf("set models.%s.id to an id %s serves", name, providerName),
		}
		if !live {
			// A list that is not the provider's own answer is a guess.
			f.Severity = SeverityWarning
			f.Message = fmt.Sprintf("%q is not in the built-in list for %s, used because its own list could not be read: %s",
				id, providerName, listModels(ids))
		}
		out = append(out, f)
	}
	return out
}

// listModels quotes at most maxListedModels ids, and says how many it left out.
func listModels(ids []string) string {
	if len(ids) <= maxListedModels {
		return strings.Join(ids, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(ids[:maxListedModels], ", "), len(ids)-maxListedModels)
}
