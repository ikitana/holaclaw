// Command pagedump is a development-only diagnostic: it drives a real
// Chromium exactly the way the browser sidecar's page engine does (open,
// navigate, wait for quiet, observe) and prints what the executor's
// extraction helpers would make of the result — the spans they offer a
// decision model and the ranked shortlist offered for selection, and the page
// text as the chat rung reads it. No model is called; nothing here decides anything.
//
// It is not built into `holaclaw` and ships nothing: `go run ./tools/pagedump
// --url ... --intent ...`. Chromium and, unless --headless=false, Xvfb must
// be on PATH, which is why it is meant to be run through the dev shell.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"unicode"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"holaclaw/internal/browser/chromium"
	"holaclaw/internal/browser/page"
	"holaclaw/internal/browser/proto"
)

// agent is the fixed session name pagedump opens its one browser under.
const agent = "pagedump"

// errUsage marks an error as a usage error: main exits 2 for these (bad
// flags, missing required ones) and 1 for everything else, matching the old
// flag-based exit codes.
var errUsage = errors.New("usage")

func main() {
	os.Exit(execute(os.Args[1:], os.Stdout, os.Stderr))
}

// execute builds the command, runs it and returns the process exit code. It
// is the tool's testable entry point: main just forwards to it.
func execute(args []string, stdout, stderr io.Writer) int {
	cmd := newRootCmd()
	cmd.SetArgs(args)
	cmd.SetOut(stdout)
	cmd.SetErr(stderr)
	if err := cmd.Execute(); err != nil {
		_, _ = fmt.Fprintln(stderr, "pagedump:", err)
		if errors.Is(err, errUsage) {
			return 2
		}
		return 1
	}
	return 0
}

// newRootCmd builds the (single) pagedump command.
func newRootCmd() *cobra.Command {
	var (
		url      string
		intent   string
		headless bool
		waitMS   int
		settle   int
		binary   string
	)
	cmd := &cobra.Command{
		Use:           "pagedump",
		Short:         "dump what the browser executor's extraction helpers see for a page",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			if url == "" || intent == "" {
				return fmt.Errorf("%w: --url and --intent are required", errUsage)
			}
			return run(url, intent, headless, waitMS, settle, binary)
		},
	}
	cmd.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return fmt.Errorf("%w: %v", errUsage, err) })
	cmd.Flags().StringVar(&url, "url", "", "the page to open")
	cmd.Flags().StringVar(&intent, "intent", "", "the request the extraction helpers rank the page against")
	cmd.Flags().BoolVar(&headless, "headless", true, "run Chromium headless instead of under Xvfb")
	cmd.Flags().IntVar(&waitMS, "wait", 10000, "milliseconds the initial ActWait(WaitQuiet) is given")
	cmd.Flags().IntVar(&settle, "settle", 2, "extra ActWait(WaitQuiet) rounds after the first, 5000ms each — client-rendered pages need it")
	cmd.Flags().StringVar(&binary, "binary", "", "a browser path; empty searches PATH (chromium.Find)")
	return cmd
}

func run(url, intent string, headless bool, waitMS, settleRounds int, binary string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	bin, err := chromium.Find(binary)
	if err != nil {
		return fmt.Errorf("no browser: %w", err)
	}
	dir, err := os.MkdirTemp("", "holaclaw-pagedump-")
	if err != nil {
		return fmt.Errorf("state dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	eng, err := page.New(page.Options{
		Binary:   bin,
		StateDir: dir,
		Headless: headless,
		Logger:   log,
	})
	if err != nil {
		return fmt.Errorf("page engine: %w", err)
	}
	defer func() { _ = eng.Shutdown(context.Background()) }()

	if _, err := eng.Open(ctx, agent, proto.OpenRequest{}); err != nil {
		return fmt.Errorf("open: %w", err)
	}
	if _, err := eng.Act(ctx, agent, proto.Action{Kind: proto.ActNavigate, URL: url}); err != nil {
		return fmt.Errorf("navigate: %w", err)
	}
	if _, err := eng.Act(ctx, agent, proto.Action{Kind: proto.ActWait, Until: proto.WaitQuiet, TimeoutMS: waitMS}); err != nil {
		return fmt.Errorf("wait: %w", err)
	}
	for i := 0; i < settleRounds; i++ {
		if _, err := eng.Act(ctx, agent, proto.Action{Kind: proto.ActWait, Until: proto.WaitQuiet, TimeoutMS: 5000}); err != nil {
			return fmt.Errorf("settle wait %d: %w", i+1, err)
		}
	}

	obs, err := eng.Observe(ctx, agent, proto.ObserveRequest{
		TextBytes:     proto.MaxTextBytes,
		MaxCandidates: proto.DefaultMaxCandidates,
	})
	if err != nil {
		return fmt.Errorf("observe: %w", err)
	}

	printReport(obs, intent)
	return nil
}

func printReport(obs proto.Observation, intent string) {
	fmt.Println("=== observation ===")
	fmt.Printf("url:        %s\n", obs.URL)
	fmt.Printf("title:      %s\n", obs.Title)
	fmt.Printf("loading:    %v\n", obs.Loading)
	fmt.Printf("truncated:  %v\n", obs.Truncated)
	fmt.Printf("text bytes: %d\n", len(obs.Text))
	fmt.Printf("candidates: %d total, %d in shortlist\n", obs.Total, len(obs.Candidates))
	fmt.Printf("dialog:     %q\n", obs.Dialog)
	fmt.Printf("alerts:     %v\n", obs.Alerts)
	fmt.Printf("dialogs:    %v\n", obs.Dialogs)
	fmt.Printf("modal:      %q\n", obs.Modal)

	fmt.Println()
	fmt.Println("=== text (first 1500 bytes) ===")
	fmt.Println(head(obs.Text, 1500))
	fmt.Println()
	fmt.Println("=== text tail (last 800 bytes) ===")
	fmt.Println(tail(obs.Text, 800))

	terms := requestTerms(intent)
	spans := spansOf(obs.Text)
	ranked := rankSpans(spans, terms, maxSpans)

	fmt.Println()
	fmt.Printf("=== spans: %d total (spansOf), first 40 ===\n", len(spans))
	for i, s := range spans {
		if i >= 40 {
			break
		}
		fmt.Printf("%3d: %s\n", i+1, s)
	}

	fmt.Println()
	fmt.Printf("=== ranked spans: %d of %d (rankSpans against the request terms) ===\n", len(ranked), len(spans))
	for i, s := range ranked {
		fmt.Printf("%3d: %s\n", i+1, s)
	}

	fmt.Println()
	fmt.Println()
	fmt.Printf("=== terms (requestTerms) ===\n%v\n", terms)
}

// head returns the first n bytes of s, cut on a rune boundary.
func head(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

// tail returns the last n bytes of s, cut on a rune boundary.
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := len(s) - n
	for cut < len(s) && !utf8.RuneStart(s[cut]) {
		cut++
	}
	return "…" + s[cut:]
}

// ---------------------------------------------------------------------------
// Everything below this line is copied, unmodified in behaviour, from
// internal/browser/extract.go and internal/browser/relevant.go so this
// diagnostic sees exactly what the executor's extraction rung would: those
// helpers are unexported, and this tool must not import an internal package
// just to reach into it. Do not let these drift from the originals; if they
// change there, change here too.
// ---------------------------------------------------------------------------

const (
	maxSpanBytes = 120
	maxSpans     = 200
	maxPageSpans = 5000

	minTermLen = 2
	spanBefore = 2
	spanAfter  = 8
)

// spansOf cuts the page text into the short pieces the choice is offered:
// lines, then table cells, then sentences, each collapsed to one line and
// capped. Duplicates and one-character scraps are dropped, and the whole is
// bounded by maxPageSpans.
func spansOf(text string) []string {
	out := make([]string, 0, 32)
	seen := map[string]bool{}
	for _, line := range strings.Split(text, "\n") {
		for _, cell := range strings.FieldsFunc(line, isCellBreak) {
			for _, s := range sentences(cell) {
				s = capAnswer(strings.Join(strings.Fields(s), " "), maxSpanBytes)
				if utf8.RuneCountInString(s) < 2 || seen[s] {
					continue
				}
				seen[s] = true
				out = append(out, s)
				if len(out) == maxPageSpans {
					return out
				}
			}
		}
	}
	return out
}

func isCellBreak(r rune) bool { return r == '\t' || r == '|' }

// sentences splits one line where a sentence visibly ends.
func sentences(s string) []string {
	var out []string
	start := 0
	for i := 0; i+1 < len(s); i++ {
		if strings.IndexByte(".!?;", s[i]) < 0 || s[i+1] != ' ' {
			continue
		}
		out = append(out, s[start:i+1])
		start = i + 2
	}
	return append(out, s[start:])
}

// capAnswer truncates on a rune boundary.
func capAnswer(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

// requestTerms are the words of the request worth matching a page against.
func requestTerms(request string) []string {
	out := make([]string, 0, 12)
	seen := map[string]bool{}
	for _, w := range strings.Fields(foldText(request)) {
		if len(w) < minTermLen || commonWords[w] || seen[w] {
			continue
		}
		seen[w] = true
		out = append(out, w)
	}
	return out
}

var commonWords = map[string]bool{
	"the": true, "and": true, "for": true, "with": true, "from": true, "into": true,
	"that": true, "this": true, "what": true, "whats": true, "which": true, "give": true,
	"show": true, "find": true, "get": true, "tell": true, "please": true, "much": true,
	"how": true, "does": true, "cost": true, "are": true, "you": true, "can": true,
	"de": true, "del": true, "la": true, "el": true, "los": true, "las": true, "un": true,
	"una": true, "en": true, "por": true, "para": true, "que": true, "con": true,
	"cuanto": true, "cual": true, "dame": true, "busca": true, "es": true, "al": true,
}

// foldText lowercases, strips accents and turns non-alphanumerics into spaces.
func foldText(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range strings.ToLower(s) {
		if f, ok := folded[r]; ok {
			b.WriteRune(f)
			continue
		}
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
			continue
		}
		b.WriteByte(' ')
	}
	return b.String()
}

var folded = map[rune]rune{
	'á': 'a', 'à': 'a', 'ä': 'a', 'â': 'a', 'ã': 'a', 'å': 'a',
	'é': 'e', 'è': 'e', 'ë': 'e', 'ê': 'e',
	'í': 'i', 'ì': 'i', 'ï': 'i', 'î': 'i',
	'ó': 'o', 'ò': 'o', 'ö': 'o', 'ô': 'o', 'õ': 'o',
	'ú': 'u', 'ù': 'u', 'ü': 'u', 'û': 'u',
	'ñ': 'n', 'ç': 'c', 'ý': 'y', 'ÿ': 'y', 'ø': 'o', 'æ': 'a', 'ß': 's',
}

// matches counts how many of the request's terms a piece of page text carries.
func matches(text string, terms []string) int {
	if len(terms) == 0 {
		return 0
	}
	words := map[string]bool{}
	for _, w := range strings.Fields(foldText(text)) {
		words[w] = true
	}
	n := 0
	for _, t := range terms {
		if words[t] {
			n++
		}
	}
	return n
}

// rankSpans cuts a shortlist of spans out of a longer list: the ones the
// request's own words appear in first, and page order within that.
func rankSpans(spans []string, terms []string, max int) []string {
	if len(spans) <= max || len(terms) == 0 {
		if len(spans) > max {
			return spans[:max]
		}
		return spans
	}
	type scored struct {
		idx, hits int
	}
	all := make([]scored, 0, len(spans))
	for i, s := range spans {
		all = append(all, scored{i, matches(s, terms)})
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].hits > all[j].hits })

	keep := make([]bool, len(spans))
	kept := 0
	mark := func(i int) {
		if i >= 0 && i < len(spans) && !keep[i] && kept < max {
			keep[i] = true
			kept++
		}
	}
	for _, s := range all {
		if s.hits == 0 || kept >= max {
			break
		}
		mark(s.idx)
		for d := 1; d <= spanBefore; d++ {
			mark(s.idx - d)
		}
		for d := 1; d <= spanAfter; d++ {
			mark(s.idx + d)
		}
	}
	for _, s := range all {
		if kept >= max {
			break
		}
		mark(s.idx)
	}
	out := make([]string, 0, max)
	for i, s := range spans {
		if keep[i] {
			out = append(out, s)
		}
	}
	return out
}
