// Package scenario loads eval scenarios: synthetic conversations with mocked
// tools and per-turn assertions (tools/eval/README.md). Scenario content is
// invented; no private data ever enters a scenario file.
package scenario

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/pelletier/go-toml/v2"

	"holaclaw/internal/provider"
	"holaclaw/internal/tool"
)

// DefaultContext is the model window a scenario runs with when it sets none.
const DefaultContext = 8192

// Scenario is one synthetic conversation.
type Scenario struct {
	Name        string `toml:"name"`
	Description string `toml:"description"`
	Persona     string `toml:"persona"`
	Context     int    `toml:"context"`   // model window override
	MaxSteps    int    `toml:"max_steps"` // agent.Definition.MaxSteps
	// Skills is the skill index the agent sees (agent.Definition.Skills);
	// loading one is a mocked `skill.load` tool like any other.
	Skills []Skill `toml:"skills"`
	Tools  []Tool  `toml:"tools"`
	Turns  []Turn  `toml:"turns"`

	Path string `toml:"-"` // file the scenario was loaded from
}

// Skill is one line of the skills fragment.
type Skill struct {
	Name        string `toml:"name"`
	Description string `toml:"description"`
}

// Tool is one mocked tool: a schema the model sees plus canned responses.
type Tool struct {
	Name        string     `toml:"name"`
	Description string     `toml:"description"`
	Parameters  string     `toml:"parameters"` // JSON Schema object, as text
	Risk        string     `toml:"risk"`       // read | write | external | exec
	Responses   []Response `toml:"responses"`
}

// Response is one canned tool result. The first response whose When matches
// wins; a response without When is the default.
type Response struct {
	When    map[string]string `toml:"when"` // arg name → exact value or "~substring"
	Text    string            `toml:"text"`
	IsError bool              `toml:"is_error"`
	Tainted bool              `toml:"tainted"`
}

// Turn is one user message and what the reply to it must look like.
type Turn struct {
	User   string `toml:"user"`
	Expect Expect `toml:"expect"`
}

// Expect is the assertion set of one turn. Zero fields are not asserted.
type Expect struct {
	ToolCalls         []CallExpect `toml:"tool_calls"`           // exact sequence
	ToolCallsAnyOrder []CallExpect `toml:"tool_calls_any_order"` // same multiset, any order
	NoToolCalls       bool         `toml:"no_tool_calls"`
	ForbidTools       []string     `toml:"forbid_tools"`
	ReplyContains     []string     `toml:"reply_contains"`     // case-insensitive
	ReplyNotContains  []string     `toml:"reply_not_contains"` // case-insensitive
	ReplyRegex        string       `toml:"reply_regex"`
	MaxSteps          int          `toml:"max_steps"`
	MaxPromptTokens   int          `toml:"max_prompt_tokens"` // context.built total, first step
	Stop              string       `toml:"stop"`              // agent.StopReason
}

// CallExpect is one expected tool call: the name plus a subset of its
// arguments that must match.
type CallExpect struct {
	Name string         `toml:"name"`
	Args map[string]any `toml:"args"`
}

// Window is the model window this scenario runs with.
func (s Scenario) Window() int {
	if s.Context > 0 {
		return s.Context
	}
	return DefaultContext
}

// Load reads every *.toml scenario in dir, sorted by name.
func Load(dir string) ([]Scenario, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*.toml"))
	if err != nil {
		return nil, fmt.Errorf("eval: scan %s: %w", dir, err)
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("eval: no scenarios in %s", dir)
	}
	sort.Strings(paths)
	out := make([]Scenario, 0, len(paths))
	seen := map[string]string{}
	for _, p := range paths {
		s, err := LoadFile(p)
		if err != nil {
			return nil, err
		}
		if prev, dup := seen[s.Name]; dup {
			return nil, fmt.Errorf("eval: scenario %q defined twice (%s and %s)", s.Name, prev, p)
		}
		seen[s.Name] = p
		out = append(out, s)
	}
	return out, nil
}

// LoadFile reads and validates one scenario file.
func LoadFile(path string) (Scenario, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Scenario{}, fmt.Errorf("eval: read scenario: %w", err)
	}
	var s Scenario
	if err := toml.Unmarshal(b, &s); err != nil {
		return Scenario{}, fmt.Errorf("eval: parse %s: %w", path, err)
	}
	s.Path = path
	if s.Name == "" {
		s.Name = strings.TrimSuffix(filepath.Base(path), ".toml")
	}
	if err := s.validate(); err != nil {
		return Scenario{}, fmt.Errorf("eval: %s: %w", path, err)
	}
	return s, nil
}

// validate rejects scenarios that could only fail at run time: bad schemas,
// oversized tool schemas, unknown tool names in assertions, bad regexps.
func (s Scenario) validate() error {
	if len(s.Turns) == 0 {
		return fmt.Errorf("scenario %s has no turns", s.Name)
	}
	names := map[string]bool{}
	for _, t := range s.Tools {
		if t.Name == "" {
			return fmt.Errorf("scenario %s: tool with empty name", s.Name)
		}
		if names[t.Name] {
			return fmt.Errorf("scenario %s: tool %s declared twice", s.Name, t.Name)
		}
		names[t.Name] = true
		var obj map[string]any
		if err := json.Unmarshal([]byte(t.Parameters), &obj); err != nil {
			return fmt.Errorf("scenario %s: tool %s parameters are not a JSON object: %w", s.Name, t.Name, err)
		}
		if n := tool.SchemaTokens(t.spec()); n > tool.MaxSchemaTokens {
			return fmt.Errorf("scenario %s: tool %s schema is %d tokens, cap is %d",
				s.Name, t.Name, n, tool.MaxSchemaTokens)
		}
		if _, err := riskOf(t.Risk); err != nil {
			return fmt.Errorf("scenario %s: tool %s: %w", s.Name, t.Name, err)
		}
	}
	for _, sk := range s.Skills {
		if sk.Name == "" || sk.Description == "" {
			return fmt.Errorf("scenario %s: a skill needs a name and a description", s.Name)
		}
	}
	for i, tn := range s.Turns {
		if strings.TrimSpace(tn.User) == "" {
			return fmt.Errorf("scenario %s: turn %d has no user text", s.Name, i+1)
		}
		e := tn.Expect
		if e.ReplyRegex != "" {
			if _, err := regexp.Compile(e.ReplyRegex); err != nil {
				return fmt.Errorf("scenario %s: turn %d: reply_regex: %w", s.Name, i+1, err)
			}
		}
		if e.NoToolCalls && (len(e.ToolCalls) > 0 || len(e.ToolCallsAnyOrder) > 0) {
			return fmt.Errorf("scenario %s: turn %d: no_tool_calls with expected calls", s.Name, i+1)
		}
		for _, c := range append(append([]CallExpect{}, e.ToolCalls...), e.ToolCallsAnyOrder...) {
			if !names[c.Name] {
				return fmt.Errorf("scenario %s: turn %d expects unmocked tool %q", s.Name, i+1, c.Name)
			}
		}
	}
	return nil
}

// Specs lists the tool schemas of this scenario, in declaration order.
func (s Scenario) Specs() []provider.ToolSpec {
	out := make([]provider.ToolSpec, 0, len(s.Tools))
	for _, t := range s.Tools {
		out = append(out, t.spec())
	}
	return out
}

func (t Tool) spec() provider.ToolSpec {
	return provider.ToolSpec{
		Name:        t.Name,
		Description: t.Description,
		Parameters:  json.RawMessage(t.Parameters),
	}
}

func riskOf(s string) (tool.Risk, error) {
	switch tool.Risk(s) {
	case "", tool.RiskRead:
		return tool.RiskRead, nil
	case tool.RiskWrite:
		return tool.RiskWrite, nil
	case tool.RiskExternal:
		return tool.RiskExternal, nil
	case tool.RiskExec:
		return tool.RiskExec, nil
	default:
		return "", fmt.Errorf("unknown risk %q", s)
	}
}
