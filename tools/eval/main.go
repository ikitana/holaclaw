// Command eval is the holaclaw eval harness: it runs synthetic scenarios
// through the real agent library with mocked tools and either a live model or
// a recording, and prints a model × scenario matrix (tools/eval/README.md).
//
// It is a dev tool, never shipped. Scenarios are invented: no private data,
// no real conversations, names or repository contents ever go to a model.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"holaclaw/internal/agent"
	"holaclaw/internal/provider"
	"holaclaw/internal/tool"
	"holaclaw/tools/eval/internal/browsereval"
	"holaclaw/tools/eval/internal/guardeval"
	"holaclaw/tools/eval/internal/harness"
	"holaclaw/tools/eval/internal/model"
	"holaclaw/tools/eval/internal/record"
	"holaclaw/tools/eval/internal/scenario"
	"holaclaw/tools/eval/internal/tainteval"
)

// stringList is a pflag.Value: a repeatable flag that also splits each value
// on commas, e.g. --model a --model b,c yields [a b c].
type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }

func (s *stringList) Type() string { return "strings" }

func (s *stringList) Set(v string) error {
	for _, part := range strings.Split(v, ",") {
		if part = strings.TrimSpace(part); part != "" {
			*s = append(*s, part)
		}
	}
	return nil
}

type flags struct {
	scenarios   string
	models      string
	recordings  string
	only        stringList
	runs        int
	mode        string
	paid        bool
	jsonOut     string
	timeout     time.Duration
	concurrency int
	strict      bool
}

// errUsage marks an error as a usage error: main exits 2 for these (bad
// flags or arguments) and 1 for everything else, matching the old
// flag-based exit codes.
var errUsage = errors.New("usage")

func main() {
	os.Exit(execute(os.Args[1:], os.Stdout, os.Stderr))
}

// execute builds the command tree, runs it and returns the process exit
// code. It is the tool's testable entry point: main just forwards to it.
func execute(args []string, stdout, stderr io.Writer) int {
	var exitCode int
	cmd := newRootCmd(&exitCode)
	cmd.SetArgs(args)
	cmd.SetOut(stdout)
	cmd.SetErr(stderr)
	if err := cmd.Execute(); err != nil {
		_, _ = fmt.Fprintln(stderr, "eval:", err)
		if errors.Is(err, errUsage) {
			return 2
		}
		return 1
	}
	return exitCode
}

// flagErrorFunc wraps a pflag parse error as a usage error. Set once on the
// root command, it is inherited by every subcommand that does not set its
// own (cobra walks up the parent chain).
func flagErrorFunc(_ *cobra.Command, err error) error {
	return fmt.Errorf("%w: %v", errUsage, err)
}

// noArgs rejects positional arguments as a usage error: none of this tool's
// commands takes one.
func noArgs(cmd *cobra.Command, args []string) error {
	if err := cobra.NoArgs(cmd, args); err != nil {
		return fmt.Errorf("%w: %v", errUsage, err)
	}
	return nil
}

// newRootCmd builds the `eval` command: the scenario harness by default,
// plus the guard, taint and browser subcommands. exitCode receives the
// harness's own report-based exit code (1 on failures, 0 otherwise); a
// non-nil error from Execute means something went wrong before or instead
// of producing a report, and always exits 1 (or 2, for a usage error).
func newRootCmd(exitCode *int) *cobra.Command {
	var f flags
	cmd := &cobra.Command{
		Use:           "eval",
		Short:         "run the scenario harness through the real agent library",
		Args:          noArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			code, err := run(ctx, f)
			if err != nil {
				return err
			}
			*exitCode = code
			return nil
		},
	}
	cmd.SetFlagErrorFunc(flagErrorFunc)
	cmd.Flags().StringVar(&f.scenarios, "scenarios", "testdata/eval/scenarios", "directory of scenario .toml files")
	cmd.Flags().StringVar(&f.models, "models", "testdata/eval/models.toml", "model list")
	cmd.Flags().StringVar(&f.recordings, "recordings", "testdata/eval/recordings", "recording directory")
	cmd.Flags().Var(&f.only, "model", "only these models (repeatable, comma-separated)")
	cmd.Flags().IntVar(&f.runs, "runs", 1, "runs per model and scenario")
	cmd.Flags().StringVar(&f.mode, "mode", string(record.ModeReplay), "live | record | replay")
	cmd.Flags().BoolVar(&f.paid, "paid", false, "include models marked paid")
	cmd.Flags().StringVar(&f.jsonOut, "json", "", "also write the report as JSON to this file")
	cmd.Flags().DurationVar(&f.timeout, "timeout", 2*time.Minute, "per-turn timeout (0 for none)")
	cmd.Flags().IntVar(&f.concurrency, "concurrency", 1, "runs in parallel (free tiers rate-limit)")
	cmd.Flags().BoolVar(&f.strict, "strict", false, "exit non-zero on failures in live/record mode too")

	cmd.AddCommand(newGuardCmd(exitCode))
	cmd.AddCommand(newTaintCmd(exitCode))
	cmd.AddCommand(newBrowserCmd(exitCode))
	return cmd
}

func run(ctx context.Context, f flags) (int, error) {
	mode, err := record.ParseMode(f.mode)
	if err != nil {
		return 0, err
	}
	scenarios, err := scenario.Load(f.scenarios)
	if err != nil {
		return 0, err
	}
	models, err := model.Load(f.models)
	if err != nil {
		return 0, err
	}
	targets, err := targets(models, f, mode)
	if err != nil {
		return 0, err
	}

	rep, err := harness.Run(ctx, harness.Options{
		Targets:     targets,
		Scenarios:   scenarios,
		Runs:        f.runs,
		Mode:        mode,
		RecordDir:   f.recordings,
		Timeout:     f.timeout,
		Concurrency: f.concurrency,
	})
	if err != nil {
		return 0, err
	}
	harness.WriteTable(os.Stdout, rep)
	if f.jsonOut != "" {
		out, err := os.Create(f.jsonOut)
		if err != nil {
			return 0, fmt.Errorf("json report: %w", err)
		}
		defer func() { _ = out.Close() }()
		if err := harness.WriteJSON(out, rep); err != nil {
			return 0, err
		}
	}
	if rep.Failed() > 0 && (mode == record.ModeReplay || f.strict) {
		return 1, nil
	}
	return 0, nil
}

// targets turns the model list into harness targets, honouring --model and
// --paid. A model whose key env var is unset is skipped, not failed; replay
// needs no key at all.
func targets(models []model.Model, f flags, mode record.Mode) ([]harness.Target, error) {
	picked, err := selectModels(models, f.only, f.paid)
	if err != nil {
		return nil, err
	}
	out := make([]harness.Target, 0, len(picked))
	for _, m := range picked {
		t := harness.Target{
			Name: m.Name,
			Spec: func(window int) provider.Model { return m.Spec(window, f.timeout) },
			New:  func() (provider.Provider, error) { return m.Provider(), nil },
		}
		if mode != record.ModeReplay {
			t.Skip = m.Skip()
		}
		out = append(out, t)
	}
	return out, nil
}

// selectModels applies --model and --paid to the model list. It is shared by
// both subcommands so they select models the same way.
func selectModels(models []model.Model, only stringList, paid bool) ([]model.Model, error) {
	want := map[string]bool{}
	for _, n := range only {
		want[n] = false
	}
	var out []model.Model
	for _, m := range models {
		if len(want) > 0 {
			if _, ok := want[m.Name]; !ok {
				continue
			}
			want[m.Name] = true
		}
		if m.Paid && !paid {
			continue // paid models cost credits: they need --paid
		}
		out = append(out, m)
	}
	for n, found := range want {
		if !found {
			return nil, fmt.Errorf("unknown model %q in --model", n)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no models selected (paid models need --paid)")
	}
	return out, nil
}

// --- guard subcommand ----------------------------------------------------

type guardFlags struct {
	cases       string
	models      string
	recordings  string
	only        stringList
	mode        string
	paid        bool
	jsonOut     string
	timeout     time.Duration
	concurrency int
	reference   string
	noArgs      bool
	api         string
	url         string
	threshold   float64
}

// newGuardCmd builds `eval guard`: every case × model through
// internal/guard. exitCode receives the report-based exit code.
func newGuardCmd(exitCode *int) *cobra.Command {
	var f guardFlags
	cmd := &cobra.Command{
		Use:           "guard",
		Short:         "run the guard model's cases through internal/guard",
		Args:          noArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			code, err := runGuard(ctx, f)
			if err != nil {
				return err
			}
			*exitCode = code
			return nil
		},
	}
	cmd.SetFlagErrorFunc(flagErrorFunc)
	cmd.Flags().StringVar(&f.cases, "cases", "testdata/eval/guard", "directory of guard case .toml files")
	cmd.Flags().StringVar(&f.models, "models", "testdata/eval/models.toml", "model list")
	cmd.Flags().StringVar(&f.recordings, "recordings", "testdata/eval/guard-recordings", "recording directory")
	cmd.Flags().Var(&f.only, "model", "only these models (repeatable, comma-separated)")
	cmd.Flags().StringVar(&f.mode, "mode", string(record.ModeReplay), "live | record | replay")
	cmd.Flags().BoolVar(&f.paid, "paid", false, "include models marked paid")
	cmd.Flags().StringVar(&f.jsonOut, "json", "", "also write the report as JSON to this file")
	cmd.Flags().DurationVar(&f.timeout, "timeout", 0, "per-classification timeout (0 for the guard default)")
	cmd.Flags().IntVar(&f.concurrency, "concurrency", 1, "classifications in parallel (free tiers rate-limit)")
	cmd.Flags().StringVar(&f.reference, "reference", "jev-guard", "model measured as the reference guard")
	cmd.Flags().BoolVar(&f.noArgs, "no-args", false, "send metadata only, never the arguments excerpt")
	cmd.Flags().StringVar(&f.api, "api", "chat", "backend: chat | decisions (internal/provider/decide.go, EXPERIMENTAL)")
	cmd.Flags().StringVar(&f.url, "url", "", "decisions endpoint (required with --api decisions; models.toml has no base_url for it)")
	cmd.Flags().Float64Var(&f.threshold, "threshold", tool.DefaultGuardThreshold, "verdict score at or above which a call counts as allow")
	return cmd
}

// parseGuardAPI validates --api and --url and reports whether the decision
// backend was chosen. --url has no default: the decisions endpoint is
// EXPERIMENTAL and no standard base path exists to derive it from.
func parseGuardAPI(f guardFlags) (decide bool, err error) {
	switch f.api {
	case "", "chat":
		return false, nil
	case "decisions":
		// A replay never reaches the network, so it needs no endpoint.
		if f.url == "" && f.mode != string(record.ModeReplay) {
			return false, fmt.Errorf("eval: --url is required with --api decisions")
		}
		return true, nil
	default:
		return false, fmt.Errorf("eval: unknown --api %q (chat|decisions)", f.api)
	}
}

func runGuard(ctx context.Context, f guardFlags) (int, error) {
	mode, err := record.ParseMode(f.mode)
	if err != nil {
		return 0, err
	}
	decide, err := parseGuardAPI(f)
	if err != nil {
		return 0, err
	}
	cases, err := guardeval.Load(f.cases)
	if err != nil {
		return 0, err
	}
	models, err := model.Load(f.models)
	if err != nil {
		return 0, err
	}
	picked, err := selectModels(models, f.only, f.paid)
	if err != nil {
		return 0, err
	}
	targets := make([]guardeval.Target, 0, len(picked))
	for _, m := range picked {
		t := guardeval.Target{
			Name: m.Name,
			Spec: func() provider.Model {
				s := m.Spec(0, f.timeout)
				if decide {
					// decisions is EXPERIMENTAL and has no standard base path
					// (internal/provider/decisions): the endpoint is always
					// this explicit override, never derived from base_url.
					s.API, s.URL = provider.APIDecisions, f.url
				}
				return s
			},
			Decide:    decide,
			Reference: m.Name == f.reference,
		}
		if decide {
			t.NewDecider = func() (provider.Decider, error) { return m.Decider(), nil }
		} else {
			t.New = func() (provider.Provider, error) { return m.Provider(), nil }
		}
		if mode != record.ModeReplay {
			t.Skip = m.Skip()
		}
		targets = append(targets, t)
	}

	var sendArgs *bool
	if f.noArgs {
		no := false
		sendArgs = &no
	}
	rep, err := guardeval.Run(ctx, guardeval.Options{
		Targets:     targets,
		Cases:       cases,
		Mode:        mode,
		RecordDir:   f.recordings,
		Timeout:     f.timeout,
		Concurrency: f.concurrency,
		SendArgs:    sendArgs,
		Threshold:   f.threshold,
	})
	if err != nil {
		return 0, err
	}
	guardeval.WriteTable(os.Stdout, rep)
	if f.jsonOut != "" {
		out, err := os.Create(f.jsonOut)
		if err != nil {
			return 0, fmt.Errorf("json report: %w", err)
		}
		defer func() { _ = out.Close() }()
		if err := guardeval.WriteJSON(out, rep); err != nil {
			return 0, err
		}
	}
	// CI gate: the reference guard must never allow a risky call, and a
	// recording that no longer matches must not pass silently.
	if mode == record.ModeReplay && (rep.ReferenceFailed() || rep.Errors() > 0) {
		return 1, nil
	}
	return 0, nil
}

// --- taint subcommand ----------------------------------------------------

type taintFlags struct {
	cases       string
	models      string
	recordings  string
	only        stringList
	mode        string
	paid        bool
	jsonOut     string
	timeout     time.Duration
	concurrency int
	reference   string
	api         string
	url         string
	threshold   float64
}

// newTaintCmd builds `eval taint`: every snippet × model through
// internal/taint. exitCode receives the report-based exit code.
func newTaintCmd(exitCode *int) *cobra.Command {
	var f taintFlags
	cmd := &cobra.Command{
		Use:           "taint",
		Short:         "run the taint scorer's snippets through internal/taint",
		Args:          noArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			code, err := runTaint(ctx, f)
			if err != nil {
				return err
			}
			*exitCode = code
			return nil
		},
	}
	cmd.SetFlagErrorFunc(flagErrorFunc)
	cmd.Flags().StringVar(&f.cases, "cases", "testdata/eval/taint", "directory of taint case .toml files")
	cmd.Flags().StringVar(&f.models, "models", "testdata/eval/models.toml", "model list")
	cmd.Flags().StringVar(&f.recordings, "recordings", "testdata/eval/taint-recordings", "recording directory")
	cmd.Flags().Var(&f.only, "model", "only these models (repeatable, comma-separated)")
	cmd.Flags().StringVar(&f.mode, "mode", string(record.ModeReplay), "live | record | replay")
	cmd.Flags().BoolVar(&f.paid, "paid", false, "include models marked paid")
	cmd.Flags().StringVar(&f.jsonOut, "json", "", "also write the report as JSON to this file")
	cmd.Flags().DurationVar(&f.timeout, "timeout", 0, "per-window timeout (0 for the scorer default)")
	cmd.Flags().IntVar(&f.concurrency, "concurrency", 1, "cases in parallel (free tiers rate-limit)")
	cmd.Flags().StringVar(&f.reference, "reference", "jev-guard", "model measured as the reference scorer")
	cmd.Flags().StringVar(&f.api, "api", "decisions", "backend: decisions only (the question is a noul)")
	cmd.Flags().StringVar(&f.url, "url", "", "decisions endpoint (required outside replay; models.toml has no base_url for it)")
	cmd.Flags().Float64Var(&f.threshold, "threshold", agent.DefaultTaintThreshold, "score at or above which content counts as steering")
	return cmd
}

func runTaint(ctx context.Context, f taintFlags) (int, error) {
	mode, err := record.ParseMode(f.mode)
	if err != nil {
		return 0, err
	}
	// The scorer has one backend: a noul is a decision model's answer.
	if f.api != "decisions" {
		return 0, fmt.Errorf("eval: --api %q: the taint scorer needs a decision model", f.api)
	}
	if f.url == "" && mode != record.ModeReplay {
		return 0, errors.New("eval: --url is required outside replay mode")
	}
	cases, err := tainteval.Load(f.cases)
	if err != nil {
		return 0, err
	}
	models, err := model.Load(f.models)
	if err != nil {
		return 0, err
	}
	picked, err := selectModels(models, f.only, f.paid)
	if err != nil {
		return 0, err
	}
	targets := make([]tainteval.Target, 0, len(picked))
	for _, m := range picked {
		t := tainteval.Target{
			Name: m.Name,
			Spec: func() provider.Model {
				s := m.Spec(0, f.timeout)
				// decisions is EXPERIMENTAL and has no standard base path:
				// the endpoint is always this explicit override.
				s.API, s.URL = provider.APIDecisions, f.url
				return s
			},
			NewDecider: func() (provider.Decider, error) { return m.Decider(), nil },
			Reference:  m.Name == f.reference,
		}
		if mode != record.ModeReplay {
			t.Skip = m.Skip()
		}
		targets = append(targets, t)
	}

	rep, err := tainteval.Run(ctx, tainteval.Options{
		Targets:     targets,
		Cases:       cases,
		Mode:        mode,
		RecordDir:   f.recordings,
		Timeout:     f.timeout,
		Concurrency: f.concurrency,
		Threshold:   f.threshold,
	})
	if err != nil {
		return 0, err
	}
	tainteval.WriteTable(os.Stdout, rep)
	if f.jsonOut != "" {
		out, err := os.Create(f.jsonOut)
		if err != nil {
			return 0, fmt.Errorf("json report: %w", err)
		}
		defer func() { _ = out.Close() }()
		if err := tainteval.WriteJSON(out, rep); err != nil {
			return 0, err
		}
	}
	// CI gate: the reference scorer must never call steering content clean,
	// and a recording that no longer matches must not pass silently.
	if mode == record.ModeReplay && (rep.ReferenceFailed() || rep.Errors() > 0) {
		return 1, nil
	}
	return 0, nil
}

// --- browser subcommand --------------------------------------------------

const (
	defaultTasksFile = "testdata/eval/browser/tasks.toml"
	defaultRealFile  = "testdata/eval/browser/real.toml"
)

type browserFlags struct {
	tasks      string
	models     string
	recordings string
	only       stringList
	pick       stringList
	decide     string
	decideURL  string
	plan       string
	mode       string
	paid       bool
	baseline   bool
	headless   bool
	jsonOut    string
	runs       int
	timeout    time.Duration
	real       bool
}

// newBrowserCmd builds `eval browser`: every synthetic site × model through
// the real browser executor and a real Chromium. exitCode receives the
// report-based exit code.
func newBrowserCmd(exitCode *int) *cobra.Command {
	var f browserFlags
	cmd := &cobra.Command{
		Use:           "browser",
		Short:         "drive the real browser executor over synthetic (or --real) sites",
		Args:          noArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			code, err := runBrowser(ctx, f)
			if err != nil {
				return err
			}
			*exitCode = code
			return nil
		},
	}
	cmd.SetFlagErrorFunc(flagErrorFunc)
	cmd.Flags().StringVar(&f.tasks, "tasks-file", defaultTasksFile, "task list")
	cmd.Flags().StringVar(&f.models, "models", "testdata/eval/models.toml", "model list")
	cmd.Flags().StringVar(&f.recordings, "recordings", "testdata/eval/browser/recordings", "recording directory")
	cmd.Flags().Var(&f.only, "model", "only these chat models (repeatable, comma-separated)")
	cmd.Flags().Var(&f.pick, "tasks", "only these tasks (repeatable, comma-separated)")
	cmd.Flags().StringVar(&f.decide, "decide", "", "model for the decide role (default: the chat model, through a JSON prompt)")
	cmd.Flags().StringVar(&f.decideURL, "decide-url", "", "decisions endpoint (required with --decide outside replay)")
	cmd.Flags().StringVar(&f.plan, "plan", "", "model for the plan role: the one that chooses the moves (default: the chat model)")
	cmd.Flags().StringVar(&f.mode, "mode", string(record.ModeReplay), "live | record | replay")
	cmd.Flags().BoolVar(&f.paid, "paid", false, "include models marked paid")
	cmd.Flags().BoolVar(&f.baseline, "baseline", false, "also run the naive per-click chat loop")
	cmd.Flags().BoolVar(&f.headless, "headless", false, "--headless=new instead of an Xvfb display")
	cmd.Flags().StringVar(&f.jsonOut, "json", "", "also write the report as JSON to this file")
	cmd.Flags().IntVar(&f.runs, "runs", 1, "runs per model and task")
	cmd.Flags().DurationVar(&f.timeout, "timeout", browsereval.DefaultTimeout, "per-task timeout")
	cmd.Flags().BoolVar(&f.real, "real", false, "drive real public sites (testdata/eval/browser/real.toml) instead of the synthetic set; owner-run, never in CI, live model calls and live pages")
	return cmd
}

func runBrowser(ctx context.Context, f browserFlags) (int, error) {
	mode, err := record.ParseMode(f.mode)
	if err != nil {
		return 0, err
	}
	if f.real && mode == record.ModeReplay {
		return 0, errors.New("eval browser: --real is refused in replay mode: page content is live, there is nothing to replay")
	}
	tasksFile := f.tasks
	if f.real && tasksFile == defaultTasksFile {
		tasksFile = defaultRealFile
	}
	var tasks []browsereval.Task
	if f.real {
		fmt.Println("real sites: live model calls, live pages, results vary")
		tasks, err = browsereval.LoadReal(tasksFile)
	} else {
		tasks, err = browsereval.Load(tasksFile)
	}
	if err != nil {
		return 0, err
	}
	if tasks, err = browsereval.Select(tasks, f.pick); err != nil {
		return 0, err
	}
	models, err := model.Load(f.models)
	if err != nil {
		return 0, err
	}
	targets, err := browserTargets(models, f, mode)
	if err != nil {
		return 0, err
	}

	// No browser is a skip, exactly like a model whose key_env is unset: the
	// machine cannot run this eval, and that is not a failure.
	env, err := browsereval.NewEnv(browsereval.EnvOptions{Headless: f.headless, NoSites: f.real})
	if err != nil {
		if errors.Is(err, browsereval.ErrNoBrowser) {
			fmt.Printf("eval browser: skipped: %v\n", err)
			return 0, nil
		}
		return 0, err
	}
	defer func() {
		shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		env.Close(shutdown)
	}()

	rep, err := browsereval.Run(ctx, browsereval.Options{
		Targets:   targets,
		Tasks:     tasks,
		Sidecar:   env.Engine,
		StartURL:  env.StartURL,
		Host:      env.Host,
		Runs:      f.runs,
		Mode:      mode,
		RecordDir: f.recordings,
		Timeout:   f.timeout,
		Browser:   env.Browser.String(),
	})
	if err != nil {
		return 0, err
	}
	browsereval.WriteTable(os.Stdout, rep)
	if f.jsonOut != "" {
		out, err := os.Create(f.jsonOut)
		if err != nil {
			return 0, fmt.Errorf("json report: %w", err)
		}
		defer func() { _ = out.Close() }()
		if err := browsereval.WriteJSON(out, rep); err != nil {
			return 0, err
		}
	}
	if mode == record.ModeReplay && (rep.Failed() > 0 || rep.Errors() > 0) {
		return 1, nil
	}
	return 0, nil
}

// browserTargets turns the model list into one column per chat model (plus a
// baseline column with --baseline), honouring --model, --decide, --plan and
// --paid.
func browserTargets(models []model.Model, f browserFlags, mode record.Mode) ([]browsereval.Target, error) {
	picked, err := selectModels(models, f.only, f.paid)
	if err != nil {
		return nil, err
	}
	var (
		decider    *model.Model
		decideSkip string
	)
	if f.decide != "" {
		d, err := findModel(models, f.decide, f.paid)
		if err != nil {
			return nil, err
		}
		if f.decideURL == "" && mode != record.ModeReplay {
			// internal/provider/decisions is EXPERIMENTAL and has no standard
			// base path: the endpoint is always passed explicitly.
			return nil, fmt.Errorf("eval: --decide-url is required with --decide outside replay")
		}
		decider = d
		if mode != record.ModeReplay {
			decideSkip = d.Skip()
		}
	}
	var (
		planner  *model.Model
		planSkip string
	)
	if f.plan != "" {
		p, err := findModel(models, f.plan, f.paid)
		if err != nil {
			return nil, err
		}
		planner = p
		if mode != record.ModeReplay {
			planSkip = p.Skip()
		}
	}

	var out []browsereval.Target
	for _, m := range picked {
		chatSpec := func() provider.Model { return m.Spec(0, f.timeout) }
		t := browsereval.Target{
			Label:    m.Name,
			Model:    m.Name,
			ChatSpec: chatSpec,
			NewChat:  func() (provider.Provider, error) { return m.Provider(), nil },
		}
		if mode != record.ModeReplay {
			t.Skip = m.Skip()
		}
		if decider != nil {
			d := *decider
			t.Label, t.Decide = m.Name+"+"+d.Name, d.Name
			t.DecideSpec = func() provider.Model {
				s := d.Spec(0, f.timeout)
				s.API, s.URL = provider.APIDecisions, f.decideURL
				return s
			}
			t.NewDecider = func() (provider.Decider, error) { return d.Decider(), nil }
			if t.Skip == "" {
				t.Skip = decideSkip
			}
		}
		if planner != nil {
			p := *planner
			t.Label, t.Plan = t.Label+"+plan:"+p.Name, p.Name
			t.PlanSpec = func() provider.Model { return p.Spec(0, f.timeout) }
			t.NewPlan = func() (provider.Provider, error) { return p.Provider(), nil }
			if t.Skip == "" {
				t.Skip = planSkip
			}
		}
		out = append(out, t)
		if f.baseline {
			b := browsereval.Target{
				Label:    m.Name + "-baseline",
				Model:    m.Name,
				ChatSpec: chatSpec,
				NewChat:  func() (provider.Provider, error) { return m.Provider(), nil },
				Baseline: true,
				Skip:     t.Skip,
			}
			if (decider != nil || planner != nil) && mode != record.ModeReplay {
				b.Skip = m.Skip() // the baseline never asks a decision or a plan model
			}
			out = append(out, b)
		}
	}
	return out, nil
}

// findModel resolves --decide against the model list, refusing a paid model
// unless --paid was passed.
func findModel(models []model.Model, name string, paid bool) (*model.Model, error) {
	for i := range models {
		if models[i].Name != name {
			continue
		}
		if models[i].Paid && !paid {
			return nil, fmt.Errorf("eval: model %q is paid: pass --paid", name)
		}
		return &models[i], nil
	}
	return nil, fmt.Errorf("eval: unknown model %q in --decide", name)
}
