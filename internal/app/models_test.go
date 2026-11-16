package app

import (
	"strings"
	"testing"

	"holaclaw/internal/config"
	"holaclaw/internal/provider"
)

// decisionModel is a model served by the decisions endpoint.
func decisionModel(name string) config.Model {
	return config.Model{Name: name, Provider: "p", ID: "d", API: string(provider.APIDecisions)}
}

// TestTaintScorer: the `guard` role when it is a decision model, else
// `decide`, else nothing at all (docs/security.md).
func TestTaintScorer(t *testing.T) {
	for _, tc := range []struct {
		name  string
		roles config.Roles
		// models added on top of the chat model
		extra map[string]config.Model
		want  bool
		err   string
	}{
		{name: "no roles", roles: config.Roles{Chat: "chat"}},
		{
			name:  "decision guard",
			roles: config.Roles{Chat: "chat", Guard: "g"},
			extra: map[string]config.Model{"g": decisionModel("g")},
			want:  true,
		},
		{
			// A chat guard cannot answer a noul: fall through to `decide`.
			name:  "chat guard, decision decide",
			roles: config.Roles{Chat: "chat", Guard: "chat", Decide: "d"},
			extra: map[string]config.Model{"d": decisionModel("d")},
			want:  true,
		},
		{
			name:  "chat guard alone",
			roles: config.Roles{Chat: "chat", Guard: "chat"},
		},
		{
			name:  "unknown role",
			roles: config.Roles{Chat: "chat", Guard: "missing"},
			err:   "missing",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := testApp(t, "")
			for n, m := range tc.extra {
				a.Config.Models[n] = m
			}
			a.Config.Agents["main"] = config.Agent{Name: "main", Models: tc.roles}
			a.Deciders["p"] = fakeDecider{}

			s, err := a.TaintScorer("main")
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("err = %v, want it to name %q", err, tc.err)
				}
				return
			}
			if err != nil {
				t.Fatalf("TaintScorer: %v", err)
			}
			if (s != nil) != tc.want {
				t.Fatalf("scorer = %v, want one: %v", s, tc.want)
			}
		})
	}
}

// A decisions model whose provider has no decision transport is a wiring
// mistake worth stopping for, exactly as it is for the guard.
func TestTaintScorerWithoutADecisionTransport(t *testing.T) {
	a := testApp(t, "")
	a.Config.Models["g"] = decisionModel("g")
	a.Config.Agents["main"] = config.Agent{Name: "main", Models: config.Roles{Chat: "chat", Guard: "g"}}

	if _, err := a.TaintScorer("main"); err == nil ||
		!strings.Contains(err.Error(), "does not serve decision models") {
		t.Fatalf("err = %v", err)
	}
}

// The Runner is wired with both knobs: the scorer and the threshold.
func TestRunnerCarriesTheTaintWiring(t *testing.T) {
	a := testApp(t, "")
	a.Config.Models["g"] = decisionModel("g")
	a.Config.Agents["main"] = config.Agent{
		Name:   "main",
		Models: config.Roles{Chat: "chat", Guard: "g"},
		Policy: config.Policy{TaintThreshold: 0.4, TaintDecayCalls: 2},
	}
	a.Deciders["p"] = fakeDecider{}

	r, err := a.Runner("main", nil, nil)
	if err != nil {
		t.Fatalf("Runner: %v", err)
	}
	if r.Taint == nil || r.TaintThreshold != 0.4 || r.TaintDecay != 2 {
		t.Fatalf("runner taint wiring: scorer=%v threshold=%v decay=%d", r.Taint, r.TaintThreshold, r.TaintDecay)
	}
}
