package check

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"holaclaw/internal/config"
	"holaclaw/internal/provider"
	"holaclaw/internal/provider/openaicompat"
)

// MinContextTokens is the smallest model.context worth configuring; smaller
// windows leave no room for tool schemas or a useful reply.
const MinContextTokens = 4096

// ProbeTimeout bounds each MOD002 network probe.
const ProbeTimeout = 5 * time.Second

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
