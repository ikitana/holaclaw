package tainteval

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
	"time"
)

// WriteTable prints one row per model, then every false clean (the failure
// that matters), then the rest of the misses, the errors, and the per-case
// scores a threshold is chosen from.
func WriteTable(w io.Writer, r Report) {
	tw := tabwriter.NewWriter(w, 2, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "model\tcases\taccuracy\tfalse-clean\tfalse-injected\tclean range\tinjected range\tp50\tp95\tcost\terrors")
	for _, m := range r.Models {
		if m.Skip != "" && m.Scored == 0 {
			_, _ = fmt.Fprintf(tw, "%s\t%d\tskip\t-\t-\t-\t-\t-\t-\t-\t-\n", name(m), m.Cases)
			continue
		}
		_, _ = fmt.Fprintf(tw, "%s\t%d\t%.0f%%\t%d/%d %.0f%%\t%d/%d %.0f%%\t%.2f–%.2f\t%.2f–%.2f\t%s\t%s\t$%s\t%d\n",
			name(m), m.Cases, 100*m.Accuracy,
			m.FalseClean, m.Injected, 100*m.FalseCleanRate,
			m.FalseInjected, m.Clean, 100*m.FalseInjectedRate,
			m.CleanMin, m.CleanMax, m.InjectedMin, m.InjectedMax,
			ms(m.P50), ms(m.P95), m.TotalCost.Decimal().StringFixed(4), m.Errors)
	}
	_ = tw.Flush()
	_, _ = fmt.Fprintf(w, "\nfalse-clean: steering content scored below the threshold %.2f, so it arms nothing "+
		"(the number that matters). mode %s. p50/p95 are live-only. total cost: $%s\n",
		r.Threshold, r.Mode, r.TotalCost().Decimal().StringFixed(4))

	var cleans, injected, errs []string
	for _, res := range r.Results {
		line := fmt.Sprintf("  %s / %s: expected %s, scored %.2f", res.Model, res.Case, res.Expect, res.Score)
		switch {
		case res.Status == StatusError:
			errs = append(errs, fmt.Sprintf("  %s / %s: %s", res.Model, res.Case, indent(res.Error)))
		case res.FalseClean():
			cleans = append(cleans, line)
		case res.Status == StatusFail:
			injected = append(injected, line)
		}
	}
	section(w, "false cleans", cleans)
	section(w, "false injected", injected)
	section(w, "errors", errs)
	scores(w, r)
}

// scores lists every case's score, injected first and worst first: this is
// what the threshold is read off.
func scores(w io.Writer, r Report) {
	rows := append([]CaseResult(nil), r.Results...)
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].Model != rows[j].Model {
			return rows[i].Model < rows[j].Model
		}
		if rows[i].Injected != rows[j].Injected {
			return rows[i].Injected
		}
		return rows[i].Score < rows[j].Score
	})
	_, _ = fmt.Fprintln(w, "\nscores:")
	tw := tabwriter.NewWriter(w, 2, 4, 2, ' ', 0)
	for _, res := range rows {
		if res.Status == StatusSkip {
			continue
		}
		_, _ = fmt.Fprintf(tw, "  %s\t%s\t%s\t%.2f\n", res.Model, res.Expect, res.Case, res.Score)
	}
	_ = tw.Flush()
}

// WriteJSON writes the machine-readable report.
func WriteJSON(w io.Writer, r Report) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(r); err != nil {
		return fmt.Errorf("eval: write json report: %w", err)
	}
	return nil
}

func section(w io.Writer, title string, lines []string) {
	if len(lines) == 0 {
		return
	}
	_, _ = fmt.Fprintf(w, "\n%s (%d):\n%s\n", title, len(lines), strings.Join(lines, "\n"))
}

func name(m Summary) string {
	if m.Reference {
		return m.Model + " *"
	}
	return m.Model
}

func ms(d time.Duration) string {
	if d == 0 {
		return "-"
	}
	return fmt.Sprintf("%dms", d.Milliseconds())
}

func indent(s string) string {
	return strings.ReplaceAll(strings.TrimRight(s, "\n"), "\n", "\n    ")
}
