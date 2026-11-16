// Command recall measures the shortlist of the page layer against real pages:
// for every page in a list it opens the URL with the page
// engine, observes it once, and reports how many interactive elements the page
// had, how many candidates came back, and — for the pages that name an
// expected target — whether the target is among the candidates and at which
// rank. Recall at the pruning stage is the ceiling for everything downstream,
// so this is the number the phase-2 gate is read from.
//
// It is a dev tool, never shipped, and it runs against the live web: it prints
// counts, the target lines of its own input, and the role and name of the
// candidate that matched one. No other page content is read out or stored.
//
//	go run ./tools/recall [--headless] [--cap 40] tools/recall/testdata/pages.txt
//
// One observation per page is taken at the protocol's maximum cap, which is
// why one run answers for every cap at once: the shortlist sorts and then
// truncates, so a candidate's rank does not depend on where the cut is.
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
	"time"

	"github.com/spf13/cobra"

	"holaclaw/internal/browser/chromium"
	"holaclaw/internal/browser/page"
	"holaclaw/internal/browser/proto"
)

// errUsage marks an error as a usage error: main exits 2 for these (bad
// flags, the wrong number of arguments) and 1 for everything else, matching
// the old flag-based exit codes.
var errUsage = errors.New("usage")

// agent is the one browser profile this tool uses.
const agent = "recall"

// caps are the shortlist sizes the summary reports recall for.
var caps = []int{25, 40, 64}

// target is one expected control: any of the roles, and a name that contains
// the substring (both matched case-insensitively; an empty substring matches
// any name).
type target struct {
	roles []string
	name  string
	line  string
}

// pageEntry is one URL and everything expected on it.
type pageEntry struct {
	url     string
	targets []target
}

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
		_, _ = fmt.Fprintln(stderr, "recall:", err)
		if errors.Is(err, errUsage) {
			return 2
		}
		return 1
	}
	return 0
}

// newRootCmd builds the (single) recall command.
func newRootCmd() *cobra.Command {
	var (
		headless bool
		shortCap int
		settle   time.Duration
		timeout  time.Duration
	)
	cmd := &cobra.Command{
		Use:   "recall pages.txt",
		Short: "measure the page layer's shortlist recall against real pages",
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) != 1 {
				return fmt.Errorf("%w: usage: recall [--headless] [--cap N] pages.txt", errUsage)
			}
			return nil
		},
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(_ *cobra.Command, args []string) error {
			return run(args[0], headless, shortCap, settle, timeout)
		},
	}
	cmd.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return fmt.Errorf("%w: %v", errUsage, err) })
	cmd.Flags().BoolVar(&headless, "headless", false, "run the browser headless")
	cmd.Flags().IntVar(&shortCap, "cap", proto.DefaultMaxCandidates, "shortlist cap to report candidate counts for")
	cmd.Flags().DurationVar(&settle, "settle", 2*time.Second, "extra wait after load before observing")
	cmd.Flags().DurationVar(&timeout, "timeout", 45*time.Second, "per-page budget")
	return cmd
}

func run(file string, headless bool, shortCap int, settle, timeout time.Duration) error {
	pages, err := readPages(file)
	if err != nil {
		return err
	}
	bin, err := chromium.Find("")
	if err != nil {
		return err
	}
	dir, err := os.MkdirTemp("", "recall-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()

	zero := 0.0
	e, err := page.New(page.Options{
		Binary:   bin,
		StateDir: dir,
		Headless: headless,
		Pacing:   &zero,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	defer func() {
		down, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = e.Shutdown(down)
	}()

	// No allow-list: this tool visits whatever its input names.
	if _, err := e.Open(ctx, agent, proto.OpenRequest{}); err != nil {
		return err
	}

	fmt.Printf("%s, cap %d, %d pages\n\n", bin.String(), shortCap, len(pages))
	var (
		latencies []time.Duration
		hits      = map[int]int{}
		targets   int
		failed    int
		// What the page layer recognised on each page before observing it:
		// fixed words, counted per page.
		botWalls = map[string]int{}
	)
	for _, p := range pages {
		if ctx.Err() != nil {
			break
		}
		obs, took, err := visit(ctx, e, p.url, settle, timeout)
		if err != nil {
			failed++
			fmt.Printf("%s\n  load failed: %v\n", p.url, err)
			continue
		}
		latencies = append(latencies, took)
		shown := len(obs.Candidates)
		if shown > shortCap {
			shown = shortCap
		}
		landed := ""
		if obs.URL != p.url {
			// Where the page actually ended up: a redirect or a bot wall is a
			// different URL, and a miss there is not a recall miss.
			landed = " → " + obs.URL
		}
		botWall := ""
		if word := e.LastBotWall(agent); word != "" {
			botWalls[word]++
			botWall = " botwall=" + word
		}
		fmt.Printf("%s%s\n  total=%d candidates=%d observe=%dms%s\n",
			p.url, landed, obs.Total, shown, took.Milliseconds(), botWall)
		for _, t := range p.targets {
			targets++
			rank, c := rankOf(obs.Candidates, t)
			if rank == 0 {
				fmt.Printf("  [%s] MISS\n", t.line)
				continue
			}
			for _, n := range caps {
				if rank <= n {
					hits[n]++
				}
			}
			fmt.Printf("  [%s] hit at rank %d (%s %q)\n", t.line, rank, c.Role, c.Name)
		}
	}

	fmt.Printf("\n%d pages, %d failed, %d targets\n", len(pages), failed, targets)
	for _, n := range caps {
		fmt.Printf("  recall@%-2d %d/%d (%.0f%%)\n", n, hits[n], targets, pct(hits[n], targets))
	}
	fmt.Printf("  observe p50 %dms  p95 %dms\n", percentile(latencies, 50).Milliseconds(), percentile(latencies, 95).Milliseconds())
	fmt.Printf("  botwalls %d", e.BotWalls())
	for _, word := range []string{page.BotWall, page.Blocked} {
		fmt.Printf("  %s=%d", word, botWalls[word])
	}
	fmt.Println()
	return nil
}

// visit loads one URL and observes it once, at the protocol's cap.
func visit(ctx context.Context, e *page.Engine, url string, settle, timeout time.Duration) (proto.Observation, time.Duration, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	res, err := e.Act(ctx, agent, proto.Action{Kind: proto.ActNavigate, URL: url})
	if err != nil {
		return proto.Observation{}, 0, err
	}
	if !res.OK {
		return proto.Observation{}, 0, fmt.Errorf("%s: %s", res.Code, res.Detail)
	}
	if _, err := e.Act(ctx, agent, proto.Action{Kind: proto.ActWait}); err != nil {
		return proto.Observation{}, 0, err
	}
	select {
	case <-time.After(settle):
	case <-ctx.Done():
		return proto.Observation{}, 0, ctx.Err()
	}
	start := time.Now()
	obs, err := e.Observe(ctx, agent, proto.ObserveRequest{MaxCandidates: proto.MaxCandidates})
	if err != nil {
		return proto.Observation{}, 0, err
	}
	return obs, time.Since(start), nil
}

// rankOf finds a target among the candidates: its 1-based rank, 0 for a miss.
func rankOf(cands []proto.Candidate, t target) (int, proto.Candidate) {
	for i, c := range cands {
		if !matchesRole(c.Role, t.roles) {
			continue
		}
		if !matchesName(c.Name, t.name) {
			continue
		}
		return i + 1, c
	}
	return 0, proto.Candidate{}
}

// matchesName matches a name substring, or the whole name when the pattern
// is written "=exact". An empty pattern matches any name.
func matchesName(name, want string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	switch {
	case want == "":
		return true
	case strings.HasPrefix(want, "="):
		return name == strings.TrimPrefix(want, "=")
	default:
		return strings.Contains(name, want)
	}
}

func matchesRole(role string, want []string) bool {
	for _, w := range want {
		if w == "*" || w == strings.ToLower(role) {
			return true
		}
	}
	return false
}

// readPages parses the input: blank lines and # comments are skipped, and a
// line is "url" or "url<TAB>role[|role…]<TAB>name-substring". Repeated URLs
// are loaded once and carry every target named for them.
func readPages(file string) ([]pageEntry, error) {
	b, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	var out []pageEntry
	at := map[string]int{}
	for n, line := range strings.Split(string(b), "\n") {
		line = strings.TrimRight(line, " \r")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Split(line, "\t")
		url := strings.TrimSpace(f[0])
		if url == "" {
			return nil, fmt.Errorf("%s:%d: no URL", file, n+1)
		}
		i, ok := at[url]
		if !ok {
			i = len(out)
			at[url] = i
			out = append(out, pageEntry{url: url})
		}
		if len(f) < 2 {
			continue
		}
		roles := strings.Split(strings.ToLower(strings.TrimSpace(f[1])), "|")
		name := ""
		if len(f) > 2 {
			name = strings.ToLower(strings.TrimSpace(f[2]))
		}
		out[i].targets = append(out[i].targets, target{
			roles: roles,
			name:  name,
			line:  strings.TrimSpace(f[1]) + " ~ " + name,
		})
	}
	return out, nil
}

func pct(n, total int) float64 {
	if total == 0 {
		return 0
	}
	return 100 * float64(n) / float64(total)
}

// percentile is the nearest-rank percentile of a set of durations.
func percentile(d []time.Duration, p int) time.Duration {
	if len(d) == 0 {
		return 0
	}
	s := append([]time.Duration(nil), d...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	i := (p*len(s) + 99) / 100
	if i < 1 {
		i = 1
	}
	if i > len(s) {
		i = len(s)
	}
	return s[i-1]
}
