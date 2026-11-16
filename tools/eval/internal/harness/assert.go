package harness

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"holaclaw/tools/eval/internal/scenario"
)

// Observed is what one turn actually produced.
type Observed struct {
	Calls        []scenario.Call
	Reply        string
	Steps        int
	PromptTokens int
	Stop         string
}

// Check evaluates the expectations of one turn and returns one line per
// failure; an empty result means the turn passed.
func Check(e scenario.Expect, o Observed) []string {
	var fails []string
	add := func(format string, args ...any) { fails = append(fails, fmt.Sprintf(format, args...)) }

	if e.NoToolCalls && len(o.Calls) > 0 {
		add("expected no tool calls, got %s", strings.Join(names(o.Calls), ", "))
	}
	for _, f := range e.ForbidTools {
		for _, c := range o.Calls {
			if c.Name == f {
				add("forbidden tool %s was called with %s", f, c.Raw)
				break
			}
		}
	}
	if len(e.ToolCalls) > 0 {
		if len(o.Calls) != len(e.ToolCalls) {
			add("expected %d tool calls (%s), got %d (%s)",
				len(e.ToolCalls), strings.Join(expNames(e.ToolCalls), ", "),
				len(o.Calls), strings.Join(names(o.Calls), ", "))
		} else {
			for i, want := range e.ToolCalls {
				if why, ok := callMatches(want, o.Calls[i]); !ok {
					add("tool call %d: %s", i+1, why)
				}
			}
		}
	}
	if len(e.ToolCallsAnyOrder) > 0 {
		used := make([]bool, len(o.Calls))
		for _, want := range e.ToolCallsAnyOrder {
			found := false
			for i, got := range o.Calls {
				if used[i] {
					continue
				}
				if _, ok := callMatches(want, got); ok {
					used[i], found = true, true
					break
				}
			}
			if !found {
				add("expected a call to %s%s, got %s", want.Name, argsText(want.Args), strings.Join(names(o.Calls), ", "))
			}
		}
	}

	reply := strings.ToLower(o.Reply)
	for _, s := range e.ReplyContains {
		if !strings.Contains(reply, strings.ToLower(s)) {
			add("reply does not contain %q", s)
		}
	}
	for _, s := range e.ReplyNotContains {
		if strings.Contains(reply, strings.ToLower(s)) {
			add("reply contains %q", s)
		}
	}
	if e.ReplyRegex != "" {
		re, err := regexp.Compile(e.ReplyRegex)
		switch {
		case err != nil:
			add("reply_regex %q: %v", e.ReplyRegex, err)
		case !re.MatchString(o.Reply):
			add("reply does not match /%s/", e.ReplyRegex)
		}
	}
	if e.MaxSteps > 0 && o.Steps > e.MaxSteps {
		add("took %d steps, limit %d", o.Steps, e.MaxSteps)
	}
	if e.MaxPromptTokens > 0 && o.PromptTokens > e.MaxPromptTokens {
		add("prompt was %d tokens, limit %d", o.PromptTokens, e.MaxPromptTokens)
	}
	if e.Stop != "" && o.Stop != e.Stop {
		add("stopped with %q, expected %q", o.Stop, e.Stop)
	}
	return fails
}

// callMatches reports whether one observed call satisfies an expectation:
// same name, and every expected argument present with an equal value.
func callMatches(want scenario.CallExpect, got scenario.Call) (string, bool) {
	if want.Name != got.Name {
		return fmt.Sprintf("expected %s, got %s", want.Name, got.Name), false
	}
	for k, wv := range want.Args {
		gv, ok := got.Args[k]
		if !ok {
			return fmt.Sprintf("%s: argument %q missing from %s", got.Name, k, got.Raw), false
		}
		if !equalValue(wv, gv) {
			return fmt.Sprintf("%s: argument %q is %v, expected %v", got.Name, k, gv, wv), false
		}
	}
	return "", true
}

// equalValue compares a TOML-typed expectation with a JSON-decoded argument.
// Numbers are compared as floats, everything else by its text form.
func equalValue(want, got any) bool {
	wf, wn := toFloat(want)
	gf, gn := toFloat(got)
	if wn || gn {
		return wn && gn && wf == gf
	}
	return fmt.Sprint(want) == fmt.Sprint(got)
}

func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	default:
		return 0, false
	}
}

func names(cs []scenario.Call) []string {
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.Name)
	}
	if len(out) == 0 {
		return []string{"none"}
	}
	return out
}

func expNames(cs []scenario.CallExpect) []string {
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.Name)
	}
	return out
}

func argsText(args map[string]any) string {
	if len(args) == 0 {
		return ""
	}
	parts := make([]string, 0, len(args))
	for k, v := range args {
		parts = append(parts, fmt.Sprintf("%s=%v", k, v))
	}
	sort.Strings(parts)
	return "(" + strings.Join(parts, " ") + ")"
}
