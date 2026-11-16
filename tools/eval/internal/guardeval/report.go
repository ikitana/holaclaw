package guardeval

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"
)

// WriteTable prints one row per model, then every false allow (the failure
// that matters), then the rest of the misses and the errors.
func WriteTable(w io.Writer, r Report) {
	tw := tabwriter.NewWriter(w, 2, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "model\tcases\taccuracy\tfalse-allow\tfalse-ask\tp50\tp95\tcost\terrors")
	for _, m := range r.Models {
		if m.Skip != "" && m.Scored == 0 {
			_, _ = fmt.Fprintf(tw, "%s\t%d\tskip\t-\t-\t-\t-\t-\t-\n", name(m), m.Cases)
			continue
		}
		_, _ = fmt.Fprintf(tw, "%s\t%d\t%.0f%%\t%d/%d %.0f%%\t%d/%d %.0f%%\t%s\t%s\t$%s\t%d\n",
			name(m), m.Cases, 100*m.Accuracy,
			m.FalseAllow, m.Risky, 100*m.FalseAllowRate,
			m.FalseAsk, m.Benign, 100*m.FalseAskRate,
			ms(m.P50), ms(m.P95), m.TotalCost.Decimal().StringFixed(4), m.Errors)
	}
	_ = tw.Flush()
	_, _ = fmt.Fprintf(w, "\nfalse-allow: risky calls the guard waved through (the number that matters). mode %s. "+
		"p50/p95 are live-only (replay has no network latency to measure). total cost: $%s\n", r.Mode, r.TotalCost().Decimal().StringFixed(4))

	var allows, asks, errs []string
	for _, res := range r.Results {
		line := fmt.Sprintf("  %s / %s: expected %s, got %s", res.Model, res.Case, res.Expect, res.Got)
		switch {
		case res.Status == StatusError:
			errs = append(errs, fmt.Sprintf("  %s / %s: %s", res.Model, res.Case, indent(res.Error)))
		case res.FalseAllow():
			allows = append(allows, line)
		case res.Status == StatusFail:
			asks = append(asks, line)
		}
	}
	section(w, "false allows", allows)
	section(w, "false asks", asks)
	section(w, "errors", errs)
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
