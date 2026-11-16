package browsereval

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"holaclaw/internal/provider"
)

// The report: a model × task matrix first, because the question is "which
// model can browse", then the per-task medians, because the second question is
// what a task costs. --json writes everything, every run included.

// ModelSummary aggregates one column of the matrix.
type ModelSummary struct {
	Model   string `json:"model"`
	Runs    int    `json:"runs"`
	Passed  int    `json:"passed"`
	Failed  int    `json:"failed"`
	Errors  int    `json:"errors"`
	Skipped int    `json:"skipped"`
	// Medians over this model's scored runs.
	Steps        int              `json:"median_steps"`
	Calls        int              `json:"median_calls"`
	PromptTokens int              `json:"median_prompt_tokens"`
	Took         time.Duration    `json:"median_took_ns"`
	TotalCost    provider.NanoUSD `json:"total_cost_nanousd"`
	// Rungs sums the ladder histogram over this model's runs.
	Rungs [8]int `json:"rungs"`
	Skip  string `json:"skip,omitempty"`
}

// TaskSummary aggregates one row of the medians table, over every model.
type TaskSummary struct {
	Task         string           `json:"task"`
	Expect       string           `json:"expect"`
	Runs         int              `json:"runs"`
	Passed       int              `json:"passed"`
	Steps        int              `json:"median_steps"`
	Generative   int              `json:"median_generative_calls"`
	Decisions    int              `json:"median_decision_calls"`
	MoveCalls    int              `json:"median_move_calls"`
	PromptTokens int              `json:"median_prompt_tokens"`
	OutputTokens int              `json:"median_output_tokens"`
	Took         time.Duration    `json:"median_took_ns"`
	MoveTook     time.Duration    `json:"median_move_took_ns"`
	Cost         provider.NanoUSD `json:"total_cost_nanousd"`
}

// Report is everything one evaluation produced.
type Report struct {
	Mode    string         `json:"mode"`
	When    time.Time      `json:"when"`
	Browser string         `json:"browser,omitempty"`
	Results []RunResult    `json:"results"`
	Models  []ModelSummary `json:"models"`
	Tasks   []TaskSummary  `json:"tasks"`
}

// Failed counts runs whose verdict did not hold. It is the CI gate.
func (r Report) Failed() int { return r.count(StatusFail) }

// Errors counts runs that produced no verdict at all.
func (r Report) Errors() int { return r.count(StatusError) }

// Skipped counts runs with no key, no recording, or a drifted one.
func (r Report) Skipped() int { return r.count(StatusSkip) }

func (r Report) count(s Status) int {
	n := 0
	for _, res := range r.Results {
		if res.Status == s {
			n++
		}
	}
	return n
}

// TotalCost sums the decision-model spend of every run.
func (r Report) TotalCost() provider.NanoUSD {
	var sum provider.NanoUSD
	for _, res := range r.Results {
		sum += res.Metrics.Cost
	}
	return sum
}

// summarise computes one row per target, in the order they were configured.
func summarise(targets []Target, results []RunResult) []ModelSummary {
	index := map[string]int{}
	out := make([]ModelSummary, 0, len(targets))
	for _, t := range targets {
		index[t.Label] = len(out)
		out = append(out, ModelSummary{Model: t.Label, Skip: t.Skip})
	}
	steps := make([][]int, len(out))
	calls := make([][]int, len(out))
	prompt := make([][]int, len(out))
	took := make([][]time.Duration, len(out))

	for _, res := range results {
		i, ok := index[res.Model]
		if !ok {
			continue
		}
		s := &out[i]
		s.Runs++
		s.TotalCost += res.Metrics.Cost
		switch res.Status {
		case StatusSkip:
			s.Skipped++
			continue
		case StatusError:
			s.Errors++
			continue
		case StatusPass:
			s.Passed++
		case StatusFail:
			s.Failed++
		}
		for r, n := range res.Metrics.Rungs {
			s.Rungs[r] += n
		}
		steps[i] = append(steps[i], res.Metrics.Steps)
		calls[i] = append(calls[i], res.Metrics.Calls())
		prompt[i] = append(prompt[i], res.Metrics.PromptTokens)
		took[i] = append(took[i], res.Metrics.Took)
	}
	for i := range out {
		out[i].Steps = medianInt(steps[i])
		out[i].Calls = medianInt(calls[i])
		out[i].PromptTokens = medianInt(prompt[i])
		out[i].Took = medianDuration(took[i])
	}
	return out
}

// taskSummaries computes the per-task medians over every model.
func taskSummaries(tasks []Task, results []RunResult) []TaskSummary {
	index := map[string]int{}
	out := make([]TaskSummary, 0, len(tasks))
	for _, t := range tasks {
		index[t.Name()] = len(out)
		out = append(out, TaskSummary{Task: t.Name(), Expect: t.expect()})
	}
	var (
		steps  = make([][]int, len(out))
		gen    = make([][]int, len(out))
		dec    = make([][]int, len(out))
		moves  = make([][]int, len(out))
		prompt = make([][]int, len(out))
		output = make([][]int, len(out))
		took   = make([][]time.Duration, len(out))
		plan   = make([][]time.Duration, len(out))
	)
	for _, res := range results {
		i, ok := index[res.Task]
		if !ok {
			continue
		}
		t := &out[i]
		t.Cost += res.Metrics.Cost
		if res.Status != StatusPass && res.Status != StatusFail {
			continue
		}
		t.Runs++
		if res.Status == StatusPass {
			t.Passed++
		}
		m := res.Metrics
		steps[i] = append(steps[i], m.Steps)
		gen[i] = append(gen[i], m.GenerativeCalls)
		dec[i] = append(dec[i], m.DecisionCalls)
		moves[i] = append(moves[i], m.MoveCalls)
		prompt[i] = append(prompt[i], m.PromptTokens)
		output[i] = append(output[i], m.OutputTokens)
		took[i] = append(took[i], m.Took)
		plan[i] = append(plan[i], m.MoveTook)
	}
	for i := range out {
		out[i].Steps = medianInt(steps[i])
		out[i].Generative = medianInt(gen[i])
		out[i].Decisions = medianInt(dec[i])
		out[i].MoveCalls = medianInt(moves[i])
		out[i].PromptTokens = medianInt(prompt[i])
		out[i].OutputTokens = medianInt(output[i])
		out[i].Took = medianDuration(took[i])
		out[i].MoveTook = medianDuration(plan[i])
	}
	return out
}

// WriteTable prints the matrix, the per-task medians and then every failed
// run's reasons.
func WriteTable(w io.Writer, r Report) {
	tw := tabwriter.NewWriter(w, 2, 4, 2, ' ', 0)
	header := []string{"model"}
	for _, t := range r.Tasks {
		header = append(header, t.Task)
	}
	_, _ = fmt.Fprintln(tw, strings.Join(header, "\t"))
	for _, m := range r.Models {
		row := []string{m.Model}
		for _, t := range r.Tasks {
			row = append(row, cell(r.Results, m.Model, t.Task))
		}
		_, _ = fmt.Fprintln(tw, strings.Join(row, "\t"))
	}
	_ = tw.Flush()
	_, _ = fmt.Fprintf(w, "\n✓ every run's verdict held, ✗ one did not, ! error, - skipped; "+
		"then median steps/model calls. mode %s", r.Mode)
	if r.Browser != "" {
		_, _ = fmt.Fprintf(w, ", %s", r.Browser)
	}
	_, _ = fmt.Fprintf(w, ". total cost $%s\n\n", r.TotalCost().Decimal().StringFixed(4))

	tw = tabwriter.NewWriter(w, 2, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "model\tpassed\tsteps\tcalls\tprompt\ttime\tcost\terrors\tskipped")
	for _, m := range r.Models {
		if m.Skip != "" && m.Passed+m.Failed == 0 {
			_, _ = fmt.Fprintf(tw, "%s\tskip: %s\t\t\t\t\t\t\t\n", m.Model, m.Skip)
			continue
		}
		_, _ = fmt.Fprintf(tw, "%s\t%d/%d\t%d\t%d\t%d\t%s\t$%s\t%d\t%d\n",
			m.Model, m.Passed, m.Passed+m.Failed, m.Steps, m.Calls, m.PromptTokens,
			secs(m.Took), m.TotalCost.Decimal().StringFixed(4), m.Errors, m.Skipped)
	}
	_ = tw.Flush()

	_, _ = fmt.Fprintln(w, "\nper task (medians over every model):")
	tw = tabwriter.NewWriter(w, 2, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "task\tpassed\tsteps\tgen\tdecide\tmoves\tprompt\toutput\tmove\ttime\tcost")
	for _, t := range r.Tasks {
		_, _ = fmt.Fprintf(tw, "%s\t%d/%d\t%d\t%d\t%d\t%d\t%d\t%d\t%s\t%s\t$%s\n",
			t.Task, t.Passed, t.Runs, t.Steps, t.Generative, t.Decisions, t.MoveCalls,
			t.PromptTokens, t.OutputTokens, secs(t.MoveTook), secs(t.Took), t.Cost.Decimal().StringFixed(4))
	}
	_ = tw.Flush()

	if rungs := rungLine(r.Models); rungs != "" {
		_, _ = fmt.Fprintf(w, "\nrungs (summed): %s\n", rungs)
	}

	var fails, errs, skips []string
	for _, res := range r.Results {
		switch res.Status {
		case StatusFail:
			fails = append(fails, fmt.Sprintf("  %s / %s run %d: %s",
				res.Model, res.Task, res.Run, strings.Join(res.Verdict.Reasons, "; ")))
		case StatusError:
			errs = append(errs, fmt.Sprintf("  %s / %s run %d: %s", res.Model, res.Task, res.Run, indent(res.Error)))
		case StatusSkip:
			skips = append(skips, fmt.Sprintf("  %s / %s run %d: %s", res.Model, res.Task, res.Run, res.Error))
		case StatusPass:
		}
	}
	section(w, "failed verdicts", fails)
	section(w, "errors", errs)
	section(w, "skipped", skips)
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

// cell is one model × task square: the mark, then median steps and calls.
func cell(results []RunResult, model, task string) string {
	var (
		pass, fail, errs, skip int
		steps, calls           []int
	)
	for _, res := range results {
		if res.Model != model || res.Task != task {
			continue
		}
		switch res.Status {
		case StatusPass:
			pass++
		case StatusFail:
			fail++
		case StatusError:
			errs++
		case StatusSkip:
			skip++
		}
		if res.Status == StatusPass || res.Status == StatusFail {
			steps = append(steps, res.Metrics.Steps)
			calls = append(calls, res.Metrics.Calls())
		}
	}
	switch {
	case pass+fail == 0 && errs > 0:
		return "!"
	case pass+fail == 0 && skip > 0:
		return "-"
	case pass+fail == 0:
		return ""
	}
	mark := "✓"
	if fail > 0 {
		mark = "✗"
	}
	if pass+fail > 1 {
		mark = fmt.Sprintf("%s %d/%d", mark, pass, pass+fail)
	}
	return fmt.Sprintf("%s %ds/%dc", mark, medianInt(steps), medianInt(calls))
}

// rungLine renders the summed ladder histogram, the metric nobody else
// publishes.
func rungLine(models []ModelSummary) string {
	names := map[int]string{2: "move", 3: "choice", 5: "vision", 6: "person", 7: "failure"}
	var total [8]int
	for _, m := range models {
		for i, n := range m.Rungs {
			total[i] += n
		}
	}
	var parts []string
	for i := 2; i < len(total); i++ {
		if total[i] > 0 {
			parts = append(parts, fmt.Sprintf("%s %d", names[i], total[i]))
		}
	}
	return strings.Join(parts, ", ")
}

func section(w io.Writer, title string, lines []string) {
	if len(lines) == 0 {
		return
	}
	_, _ = fmt.Fprintf(w, "\n%s (%d):\n%s\n", title, len(lines), strings.Join(lines, "\n"))
}

// medianInt is the upper median of v (nearest rank), 0 for no samples.
func medianInt(v []int) int {
	if len(v) == 0 {
		return 0
	}
	s := append([]int(nil), v...)
	sort.Ints(s)
	return s[len(s)/2]
}

func medianDuration(v []time.Duration) time.Duration {
	if len(v) == 0 {
		return 0
	}
	s := append([]time.Duration(nil), v...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	return s[len(s)/2]
}

func secs(d time.Duration) string {
	if d == 0 {
		return "-"
	}
	return fmt.Sprintf("%.1fs", d.Seconds())
}

func indent(s string) string {
	return strings.ReplaceAll(strings.TrimRight(s, "\n"), "\n", "\n    ")
}
