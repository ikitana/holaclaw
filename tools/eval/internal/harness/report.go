package harness

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
)

// cell aggregates the runs of one model × scenario pair.
type cell struct {
	runs, passed, skipped int
	steps, prompt         int
}

func (c cell) String() string {
	if c.runs == 0 {
		return "-"
	}
	if c.passed == 0 && c.skipped == c.runs {
		return "skip"
	}
	scored := c.runs - c.skipped
	if scored == 0 {
		scored = 1
	}
	return fmt.Sprintf("%d/%d %.1f %d", c.passed, c.runs-c.skipped,
		float64(c.steps)/float64(scored), c.prompt/scored)
}

// WriteTable prints the model × scenario matrix, then every failure and every
// skipped run with its reason.
func WriteTable(w io.Writer, r Report) {
	models, scenarios := axes(r)
	cells := map[string]*cell{}
	for _, run := range r.Runs {
		k := run.Model + "\x00" + run.Scenario
		c := cells[k]
		if c == nil {
			c = &cell{}
			cells[k] = c
		}
		c.runs++
		switch run.Status {
		case StatusPass:
			c.passed++
		case StatusSkip:
			c.skipped++
		case StatusFail:
		}
		c.steps += run.Steps
		c.prompt += run.PromptTokens
	}

	tw := tabwriter.NewWriter(w, 2, 4, 2, ' ', 0)
	_, _ = fmt.Fprintf(tw, "model\t%s\n", strings.Join(scenarios, "\t"))
	for _, m := range models {
		_, _ = fmt.Fprint(tw, m)
		for _, s := range scenarios {
			c := cells[m+"\x00"+s]
			if c == nil {
				_, _ = fmt.Fprint(tw, "\t-")
				continue
			}
			_, _ = fmt.Fprintf(tw, "\t%s", c)
		}
		_, _ = fmt.Fprintln(tw)
	}
	_ = tw.Flush()
	_, _ = fmt.Fprintf(w, "\ncell: passed/runs, avg steps, avg prompt tokens (mode %s)\n", r.Mode)

	var failures, skips []string
	for _, run := range r.Runs {
		switch run.Status {
		case StatusSkip:
			skips = append(skips, fmt.Sprintf("  %s / %s run %d: %s", run.Model, run.Scenario, run.Run, run.Reason))
		case StatusFail:
			head := fmt.Sprintf("  %s / %s run %d", run.Model, run.Scenario, run.Run)
			if run.Reason != "" {
				failures = append(failures, head+": "+indent(run.Reason))
			}
			for _, t := range run.Turns {
				for _, f := range t.Failures {
					failures = append(failures, fmt.Sprintf("%s turn %d: %s", head, t.Turn, f))
				}
			}
		case StatusPass:
		}
	}
	if len(failures) > 0 {
		_, _ = fmt.Fprintf(w, "\nfailures (%d):\n%s\n", len(failures), strings.Join(failures, "\n"))
	}
	if len(skips) > 0 {
		_, _ = fmt.Fprintf(w, "\nskipped (%d):\n%s\n", len(skips), strings.Join(skips, "\n"))
	}
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

// axes returns the model and scenario names in first-seen order.
func axes(r Report) (models, scenarios []string) {
	seenM, seenS := map[string]bool{}, map[string]bool{}
	for _, run := range r.Runs {
		if !seenM[run.Model] {
			seenM[run.Model] = true
			models = append(models, run.Model)
		}
		if !seenS[run.Scenario] {
			seenS[run.Scenario] = true
			scenarios = append(scenarios, run.Scenario)
		}
	}
	sort.Strings(scenarios)
	return models, scenarios
}

// indent keeps a multi-line harness error readable under its heading.
func indent(s string) string {
	return strings.ReplaceAll(strings.TrimRight(s, "\n"), "\n", "\n    ")
}
