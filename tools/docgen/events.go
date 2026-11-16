package main

import (
	"fmt"
	"strings"

	"holaclaw/internal/event"
)

// renderEventsMD lists every event type (internal/event.Types), which are
// ephemeral (streamed but never stored) and which count towards a trace's and
// a session's usage totals (internal/event.UsageTypes).
func renderEventsMD() string {
	var b strings.Builder
	b.WriteString("# Event types\n\n")
	b.WriteString("Generated from `internal/event.Types`. Types are added, not removed (docs/traceability.md, ARCHITECTURE.md §7).\n")
	b.WriteString("#151 renamed the `identity.alias_*` pair to `identity.bound` and `identity.unbound`, once;\n")
	b.WriteString("rows stored before it keep the old type.\n\n")
	b.WriteString("Every event that records a model call carries its usage: `prompt_tokens` and\n")
	b.WriteString("`completion_tokens` always (`0` when the upstream reported none), and\n")
	b.WriteString("`cost_nanousd` (an integer of USD × 10⁻⁹, so sums are exact), `cached_tokens`\n")
	b.WriteString("and `reasoning_tokens` only when the upstream reported them. Events stored\n")
	b.WriteString("before `cost_nanousd` carry `cost`, a float in USD, read in its place. A call\n")
	b.WriteString("without a cost is unpriced (a local engine, a subscription), never free; a\n")
	b.WriteString("free model reports `0`. `unpriced_calls` is always written: how\n")
	b.WriteString("many of the event's calls reported no cost, `0` or `1` for one call and more\n")
	b.WriteString("for a sum (`browser.task.finished`, a scored `tool.result`, `run.finished`,\n")
	b.WriteString("`browser.move`). A call that was not made — a cached guard verdict, a\n")
	b.WriteString("failure before any reply — writes no usage at all; a reply that was\n")
	b.WriteString("answered and then failed (a dropped stream on `model.error`, an empty\n")
	b.WriteString("description on `media.refused`) still does. The totals of a trace and of a\n")
	b.WriteString("session sum those attributes over the types marked *counted*\n")
	b.WriteString("(`internal/event.UsageTypes`), each of which reports its calls exactly once;\n")
	b.WriteString("`run.finished` and `browser.move` repeat numbers already counted and are left\n")
	b.WriteString("out. A total's cost covers the priced calls; its `unpriced_calls` says how\n")
	b.WriteString("many it does not cover.\n\n")
	b.WriteString("| Type | Ephemeral | Usage |\n|---|---|---|\n")
	for _, t := range event.Types {
		eph := ""
		if t.Ephemeral() {
			eph = "yes — streamed only, never written to the store"
		}
		usage := ""
		if event.CountsUsage(t) {
			usage = "counted"
		}
		fmt.Fprintf(&b, "| `%s` | %s | %s |\n", t, eph, usage)
	}
	return b.String()
}
