// Package tainteval evaluates the taint scorer (docs/security.md, tools/eval/README.md): each
// case is one snippet of untrusted content with the verdict a human would
// give it — ordinary content, or text that tries to steer the agent reading
// it. The number that matters is the false-clean rate: an injected page scored
// below the threshold arms nothing, and the turn walks into it.
//
// Snippets are invented, like every other eval fixture: no private data, no
// real names, hosts or pages ever reach these models.
package tainteval

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

// Expectations a case can carry.
const (
	ExpectClean    = "clean"
	ExpectInjected = "injected"
)

// Case is one snippet the scorer must grade.
type Case struct {
	Name   string `toml:"name"`
	Text   string `toml:"text"`
	Expect string `toml:"expect"` // clean | injected

	Path string `toml:"-"` // file the case was loaded from
}

type file struct {
	Cases []Case `toml:"cases"`
}

// Injected reports whether this case must score at or above the threshold.
// Those are the cases the false-clean rate is computed over.
func (c Case) Injected() bool { return c.Expect == ExpectInjected }

// Load reads every *.toml case file in dir, sorted by path.
func Load(dir string) ([]Case, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*.toml"))
	if err != nil {
		return nil, fmt.Errorf("eval: scan %s: %w", dir, err)
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("eval: no taint cases in %s", dir)
	}
	sort.Strings(paths)
	var out []Case
	seen := map[string]string{}
	for _, p := range paths {
		cases, err := LoadFile(p)
		if err != nil {
			return nil, err
		}
		for _, c := range cases {
			if prev, dup := seen[c.Name]; dup {
				return nil, fmt.Errorf("eval: taint case %q defined twice (%s and %s)", c.Name, prev, p)
			}
			seen[c.Name] = p
			out = append(out, c)
		}
	}
	return out, nil
}

// LoadFile reads and validates one case file.
func LoadFile(path string) ([]Case, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("eval: read taint cases: %w", err)
	}
	var f file
	if err := toml.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("eval: parse %s: %w", path, err)
	}
	if len(f.Cases) == 0 {
		return nil, fmt.Errorf("eval: %s: no cases", path)
	}
	for i := range f.Cases {
		c := &f.Cases[i]
		c.Path = path
		if err := c.normalise(); err != nil {
			return nil, fmt.Errorf("eval: %s: %w", path, err)
		}
	}
	return f.Cases, nil
}

// normalise rejects a case that could only fail at run time.
func (c *Case) normalise() error {
	if strings.TrimSpace(c.Name) == "" {
		return fmt.Errorf("case with empty name")
	}
	if strings.TrimSpace(c.Text) == "" {
		return fmt.Errorf("case %s: no text", c.Name)
	}
	switch c.Expect {
	case ExpectClean, ExpectInjected:
	default:
		return fmt.Errorf("case %s: expect must be clean or injected, got %q", c.Name, c.Expect)
	}
	return nil
}
