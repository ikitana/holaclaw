// Package browsereval is the browser executor eval
// (docs/browser.md): every synthetic site of tools/eval/internal/sites is
// driven by the real internal/browser executor against a real Chromium, and
// the run is scored on outcome, blocker, approval, server-side state and the
// answer it came back with.
//
// The measurement is the point: success, steps, generative calls, decision
// calls, move calls, tokens, cost, wall time and the rung histogram per run,
// with an optional naive per-click chat loop (baseline.go) as the column the
// ladder is compared against.
//
// Only invented sites and intents ever reach a model here — the hard rule of
// tools/eval/README.md. API keys come from the environment (key_env) and are
// never read from a file, printed or recorded.
package browsereval

import (
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/pelletier/go-toml/v2"

	"holaclaw/tools/eval/internal/sites"
)

// Task is one entry of the task list: either a synthetic site (Site, driven
// against the loopback server sites.Handler serves) or a real public site
// (Real, driven live, --real only). Exactly one of the two is set; Site's
// zero value is never a valid synthetic task on its own, so Real being
// non-nil is what every accessor below switches on.
type Task struct {
	Site sites.Site
	Real *RealTask
	// Approve is what the scripted Humans answers to this task's approval
	// request. False everywhere but the sites whose success needs the risky
	// step carried out. Real tasks never grant one: nothing behind --real is
	// ever a purchase.
	Approve bool
}

// RealTask is one entry of testdata/eval/browser/real.toml: a real public
// site, complete in itself (unlike Task.Site, which only names a synthetic
// site the runner already knows everything about). There is no server-side
// Check possible for a site the eval does not own, and no Blocker/Risk
// expectation either — a real task is judged on outcome and answer alone.
type RealTask struct {
	Name   string
	Intent string
	URL    string
	Allow  []string
	Expect string // "done" or "blocked"
	Answer *regexp.Regexp
	Note   string
}

// Name is the task's short id: the site's for a synthetic task, the entry's
// own for a real one.
func (t Task) Name() string {
	if t.Real != nil {
		return t.Real.Name
	}
	return t.Site.Name
}

// intent is what the runner hands to browser.Task.Intent.
func (t Task) intent() string {
	if t.Real != nil {
		return t.Real.Intent
	}
	return t.Site.Intent
}

// expect is the human line the outcome word is read out of.
func (t Task) expect() string {
	if t.Real != nil {
		return t.Real.Expect
	}
	return t.Site.Expect
}

// answer is the pattern a passing run's extracted answer must match, nil
// when the task carries none.
func (t Task) answer() *regexp.Regexp {
	if t.Real != nil {
		return t.Real.Answer
	}
	return t.Site.Answer
}

// allow is the agent's navigation allow-list: a real task's own hosts, or the
// synthetic loopback host every site is served on.
func (t Task) allow(host string) []string {
	if t.Real != nil {
		return t.Real.Allow
	}
	return []string{host}
}

// startURL is where one run of the task begins: a real task's fixed URL, or
// the synthetic site's URL for this run id.
func (t Task) startURL(build func(sites.Site, string) string, run string) string {
	if t.Real != nil {
		return t.Real.URL
	}
	return build(t.Site, run)
}

type taskFile struct {
	Tasks []taskEntry `toml:"tasks"`
}

type taskEntry struct {
	Name    string `toml:"name"`
	Approve bool   `toml:"approve"`
}

// Load reads the task list and binds every entry to its site. A name with no
// site behind it is an error: a typo must not silently shrink the eval.
func Load(path string) ([]Task, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("eval: read tasks: %w", err)
	}
	var f taskFile
	if err := toml.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("eval: parse %s: %w", path, err)
	}
	if len(f.Tasks) == 0 {
		return nil, fmt.Errorf("eval: no tasks in %s", path)
	}
	byName := map[string]sites.Site{}
	for _, s := range sites.Sites() {
		byName[s.Name] = s
	}
	seen := map[string]bool{}
	out := make([]Task, 0, len(f.Tasks))
	for _, e := range f.Tasks {
		s, ok := byName[e.Name]
		switch {
		case e.Name == "":
			return nil, fmt.Errorf("eval: %s: task with empty name", path)
		case !ok:
			return nil, fmt.Errorf("eval: %s: no synthetic site named %q", path, e.Name)
		case seen[e.Name]:
			return nil, fmt.Errorf("eval: %s: task %q listed twice", path, e.Name)
		}
		seen[e.Name] = true
		out = append(out, Task{Site: s, Approve: e.Approve})
	}
	return out, nil
}

// realFile is the shape of testdata/eval/browser/real.toml.
type realFile struct {
	Tasks []realEntry `toml:"tasks"`
}

type realEntry struct {
	Name   string   `toml:"name"`
	Intent string   `toml:"intent"`
	URL    string   `toml:"url"`
	Allow  []string `toml:"allow"`
	Expect string   `toml:"expect"`
	Answer string   `toml:"answer"`
	Note   string   `toml:"note"`
}

// LoadReal reads the real-sites task list (--real): every entry is
// self-contained, unlike Load's synthetic tasks, which only reference the
// site table by name. A run against these sites reaches the live internet, so
// an incomplete entry — a missing intent, an empty allow-list, an
// unparseable expectation — is rejected here rather than failing loudly mid
// run.
func LoadReal(path string) ([]Task, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("eval: read %s: %w", path, err)
	}
	var f realFile
	if err := toml.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("eval: parse %s: %w", path, err)
	}
	if len(f.Tasks) == 0 {
		return nil, fmt.Errorf("eval: no tasks in %s", path)
	}
	seen := map[string]bool{}
	out := make([]Task, 0, len(f.Tasks))
	for _, e := range f.Tasks {
		switch {
		case e.Name == "":
			return nil, fmt.Errorf("eval: %s: entry with empty name", path)
		case seen[e.Name]:
			return nil, fmt.Errorf("eval: %s: %q listed twice", path, e.Name)
		case e.Intent == "":
			return nil, fmt.Errorf("eval: %s: %q has no intent", path, e.Name)
		case e.URL == "":
			return nil, fmt.Errorf("eval: %s: %q has no url", path, e.Name)
		case len(e.Allow) == 0:
			return nil, fmt.Errorf("eval: %s: %q has an empty allow list", path, e.Name)
		}
		if _, ok := expectedOutcome(e.Expect); !ok {
			return nil, fmt.Errorf("eval: %s: %q expect %q holds no done|blocked word", path, e.Name, e.Expect)
		}
		seen[e.Name] = true
		rt := &RealTask{
			Name: e.Name, Intent: e.Intent, URL: e.URL,
			Allow: append([]string(nil), e.Allow...), Expect: e.Expect, Note: e.Note,
		}
		if e.Answer != "" {
			re, err := regexp.Compile(e.Answer)
			if err != nil {
				return nil, fmt.Errorf("eval: %s: %q answer: %w", path, e.Name, err)
			}
			rt.Answer = re
		}
		out = append(out, Task{Real: rt})
	}
	return out, nil
}

// Select applies --tasks: the named tasks in the list's own order, or all of
// them when nothing was named.
func Select(tasks []Task, only []string) ([]Task, error) {
	if len(only) == 0 {
		return tasks, nil
	}
	want := map[string]bool{}
	for _, n := range only {
		if n = strings.TrimSpace(n); n != "" {
			want[n] = false
		}
	}
	var out []Task
	for _, t := range tasks {
		if _, ok := want[t.Name()]; ok {
			want[t.Name()] = true
			out = append(out, t)
		}
	}
	for n, found := range want {
		if !found {
			return nil, fmt.Errorf("eval: unknown task %q in --tasks", n)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("eval: no tasks selected")
	}
	return out, nil
}
