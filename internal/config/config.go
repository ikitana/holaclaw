package config

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"

	"holaclaw/internal/agent"
	"holaclaw/internal/event"
	"holaclaw/internal/provider"
	"holaclaw/internal/skill"
)

// Config is one parsed config directory. Every value is user intent: runtime
// state lives in the DB, never here (ARCHITECTURE.md §4).
type Config struct {
	Dir        string
	Version    int
	Server     Server
	Providers  map[string]Provider
	Models     map[string]Model
	Retention  Retention
	Backups    Backups
	Channels   map[string]Channel
	Connectors map[string]Connector
	Agents     map[string]Agent
	Browser    Browser
	// Sources says where each EnvOverrides key got its value: "env",
	// "config.toml" or "default".
	Sources map[string]string
}

// Connector is one remote MCP server as connectors.toml describes it
// (docs/connectors.md §4). What the server told us is in the store's cache
// row, its tokens in auth.toml.
type Connector struct {
	ID       string
	URL      string
	Auth     string   // "none" | "oauth"
	Scopes   []string // nil = every scope the server offers
	ClientID string   // pre-registered client id; "" = CIMD or DCR
	// Trusted results are still wrapped as untrusted content but never scored,
	// so they never arm the taint (docs/sessions.md).
	Trusted bool
	// AllowInsecureTransport reaches the server over plain http on any host,
	// or https with a certificate that does not verify.
	AllowInsecureTransport bool
}

// connectorID is the shape of a connector id: it becomes the prefix of every
// tool name the connector registers, `<id>.<tool>` (docs/connectors.md §5).
var connectorID = regexp.MustCompile(`^[a-z0-9-]{1,32}$`)

// ValidConnectorID reports whether id may be used as a connector id.
func ValidConnectorID(id string) bool { return connectorID.MatchString(id) }

// Browser is the sidecar connection (docs/browser.md). Enabled is
// browser.enabled: true by default when the `[browser]` table is present,
// false without it, and $HOLACLAW_BROWSER_ENABLED wins over both. Off means
// `browser.do` is refused for every agent; on, every agent that does not opt
// out gets it.
type Browser struct {
	Enabled  bool
	Socket   string
	Binary   string
	Headless bool
}

// Server is where the control API listens and where data lives.
type Server struct {
	Socket  string
	Listen  string // optional TCP address
	TLSCert string
	TLSKey  string
	// AllowInsecureTransport lets a non-loopback Listen serve plain http
	// without TLS; a token is still required.
	AllowInsecureTransport bool
	// PublicURL is the address devices reach the control API at, which
	// pairing links carry; "" derives one from Listen.
	PublicURL string
	DataDir   string
	// SecretsDir holds auth.toml: an absolute path, or "" for the default,
	// /etc/holaclaw-secrets.
	SecretsDir string
}

// Provider is one connection: wire format, auth and concurrency (docs/models.md).
type Provider struct {
	Name          string
	Type          string // "openai-compatible" | "codex"
	BaseURL       string
	Auth          string // "none" | "api-key" | "subscription"
	APIKey        string // `env:VAR` reference; resolve with ResolveEnv
	MaxConcurrent int
	// Sort is the routing preference for a multi-provider gateway (OpenRouter
	// provider.sort): throughput, latency or price; "" sends nothing. Measured:
	// the same model planned in 1.9 s by throughput vs 7-22 s on the default
	// route. Single-provider endpoints ignore the field.
	Sort string
}

// Model is a model capability profile as configured.
type Model struct {
	Name          string
	Provider      string
	ID            string
	API           string // "chat" | "decisions" | "images"; never empty after Load
	URL           string // absolute endpoint override; set for every decisions model
	Context       int
	Tools         bool
	Vision        bool
	Strict        bool
	ThinkingTags  []string
	SchemaProfile string
	Reasoning     string // "" | one of reasoningEfforts
	Timeout       time.Duration
}

// The [backups] defaults: one automatic backup a day, a week of them kept.
const (
	DefaultBackupsEvery = 24 * time.Hour
	DefaultBackupsKeep  = 7
)

// Retention is how long traceability data is kept (docs/traceability.md).
type Retention struct {
	EventsDays int
}

// Backups is the daemon's own backup schedule (ARCHITECTURE.md §4).
type Backups struct {
	// Every is how often the daemon takes a backup on its own.
	Every time.Duration
	// Keep is how many of those automatic backups are kept; 0 takes none.
	Keep int
}

// Channel is one channel instance and its access knobs (docs/identity.md).
type Channel struct {
	Name   string
	Type   string // "telegram" | "slack" | "googlechat" | "whatsapp"
	Agent  string
	Access Access
}

// Access is the two knobs plus reply mode of a channel instance.
type Access struct {
	DM    string // "invite" | "open"
	Rooms string // "off" | "approved" | "open"
	Reply string // "mention" | "always"
	// ReplyTo says where an answer is a native reply to the message that
	// asked: "rooms" | "always" | "off".
	ReplyTo string
}

// Classes is a policy decision per risk class (docs/security.md).
type Classes struct {
	Read     string
	Write    string
	External string
	Exec     string
}

// Decision returns the verdict for one risk class, "" for an unknown class.
func (c Classes) Decision(risk string) string {
	switch risk {
	case "read":
		return c.Read
	case "write":
		return c.Write
	case "external":
		return c.External
	case "exec":
		return c.Exec
	}
	return ""
}

// Policy is an agent's tool policy: per class, per tool, and the restricted
// variants for guests and detached task runs.
type Policy struct {
	Classes
	Tools          map[string]string // per-tool overrides
	Guest          Classes
	Task           Classes
	ExecAllow      []string
	TaintEscalates bool
	// GuardMode is "recommend" (default) or "auto"; GuardThreshold is the
	// score at or above which the guard calls a tool call safe (docs/security.md).
	GuardMode      string
	GuardThreshold float64
	// TaintDecayCalls is how many tool calls after untrusted content entered
	// the turn still count as tainted (docs/security.md); 0 means the built-in default.
	TaintDecayCalls int
	// TaintThreshold is the taint score at or above which untrusted content
	// escalates policy (docs/security.md): below it the content is still wrapped, it just
	// arms nothing. 0 means the built-in default.
	TaintThreshold float64
}

// Roles are the model roles of an agent (docs/models.md). Only Chat is required.
type Roles struct {
	Chat       string
	Fallback   string
	Guard      string
	Summarize  string
	Transcribe string
	Vision     string
	Embed      string
	Image      string
	Decide     string
	Plan       string // browser planner; "" = the chat role
}

// Limits are per-turn caps; zero means the built-in default. ImagesPerDay is
// the exception: 0 is a real value (the image tool refuses every call), so the
// default is applied when the key is absent.
type Limits struct {
	MaxSteps     int
	ReplyReserve int
	ImagesPerDay int
}

// Agent is one agents/<name>/ unit: agent.toml plus prompt.md.
type Agent struct {
	Name    string
	Version int
	Tools   []string
	// Connectors are the connector ids granted to this agent; each one
	// behaves like a tool group in Tools (docs/connectors.md §5).
	Connectors []string
	Models     Roles
	Policy     Policy
	Limits     Limits
	Browser    AgentBrowser
	Skills     AgentSkills
	Prompt     string // agents/<name>/prompt.md
}

// AgentSkills is the per-agent skill switch (docs/skills.md §3).
type AgentSkills struct {
	// Disabled names skills kept out of the index at every level.
	Disabled []string
}

// AgentBrowser is per-agent navigation limits for the browser executor (docs/browser.md).
type AgentBrowser struct {
	// Enabled is false when the agent opts out of the browser; it is true by
	// default.
	Enabled bool
	// Allow is a host allow-list; unset or empty means any site may be opened.
	Allow []string
}

// refUse is one reference found while walking, checked once every file is in.
type refUse struct {
	file, key string
	kind      ref
	value     string
}

// Load reads and validates a config directory. It collects every problem it
// finds; the returned error is a Problems list when any has Error severity.
// The Config is still returned, filled as far as parsing got, so callers that
// only want to report can inspect it.
func Load(dir string) (*Config, error) {
	var ps Problems
	var refs []refUse
	cfg := &Config{
		Dir:        dir,
		Providers:  map[string]Provider{},
		Models:     map[string]Model{},
		Channels:   map[string]Channel{},
		Connectors: map[string]Connector{},
		Agents:     map[string]Agent{},
		Sources:    map[string]string{},
	}

	root, ok := readTOML(dir, FileConfig, &ps)
	if !ok {
		ps.sortStable()
		return cfg, ps
	}
	cfg.Version = checkVersion(at(FileConfig, &ps), root, 0)
	walkTable(collector{FileConfig, &ps, &refs}, "", root, fieldsOf(FileConfig), true)
	loadConfigFile(cfg, root)

	if chans, present := optionalTOML(dir, FileChannels, &ps); present {
		checkVersion(at(FileChannels, &ps), chans, cfg.Version)
		walkTable(collector{FileChannels, &ps, &refs}, "", chans, fieldsOf(FileChannels), true)
		loadChannelsFile(cfg, chans)
		if _, ok := cfg.Channels[event.ChannelHolaclaw]; ok {
			at(FileChannels, &ps).add(CodeBadValue, "channels."+event.ChannelHolaclaw,
				"holaclaw is the built-in channel of the CLI and the apps", "give the channel instance another name")
		}
	}

	if conns, present := optionalTOML(dir, FileConnectors, &ps); present {
		checkVersion(at(FileConnectors, &ps), conns, cfg.Version)
		walkTable(collector{FileConnectors, &ps, &refs}, "", conns, fieldsOf(FileConnectors), true)
		loadConnectorsFile(cfg, conns, &ps)
	}

	for _, name := range agentDirs(dir, &ps) {
		rel := path.Join("agents", name, FileAgent)
		m, ok := readTOML(dir, rel, &ps)
		if !ok {
			continue
		}
		c := collector{rel, &ps, &refs}
		checkVersion(at(rel, &ps), m, cfg.Version)
		walkTable(c, "", m, fieldsOf(FileAgent), true)
		a := loadAgentFile(name, m)
		prompt, err := os.ReadFile(filepath.Join(dir, "agents", name, FilePrompt))
		if err != nil {
			at(path.Join("agents", name, FilePrompt), &ps).add(CodeMissingFile, "",
				"agent prompt is missing or unreadable", "create agents/"+name+"/"+FilePrompt)
		}
		a.Prompt = string(prompt)
		cfg.Agents[name] = a
	}

	applyEnv(cfg, root, &ps)
	checkRefs(cfg, refs, &ps)
	checkRoles(cfg, &ps)
	checkBrowserTool(cfg, &ps)
	grantBrowser(cfg)
	checkSkillNames(cfg, &ps)
	checkConnectorGrants(cfg, &ps)
	ps.sortStable()
	if ps.HasErrors() {
		return cfg, ps
	}
	return cfg, nil
}

// Validate adds the checks that need the tool registry: tool names and
// per-tool policy overrides must name a registered tool or tool group.
func Validate(cfg *Config, knownTools []string) error {
	var ps Problems
	groups := map[string]bool{}
	for _, t := range knownTools {
		if i := strings.IndexByte(t, '.'); i > 0 {
			groups[t[:i]] = true
		}
	}
	known := func(name string) bool {
		for _, t := range knownTools {
			if t == name {
				return true
			}
		}
		return groups[name]
	}
	candidates := append(append([]string{}, knownTools...), sortedKeys(groups)...)

	for _, name := range sortedKeys(cfg.Agents) {
		a := cfg.Agents[name]
		file := path.Join("agents", name, FileAgent)
		c := at(file, &ps)
		for _, t := range a.Tools {
			if known(t) {
				continue
			}
			hint := didYouMean(suggest(t, candidates))
			if _, isConnector := cfg.Connectors[t]; isConnector {
				hint = fmt.Sprintf("%q is a connector: grant it in `connectors`, not `tools`", t)
			}
			c.add(CodeUnknownTool, "tools", fmt.Sprintf("unknown tool or group %q", t), hint)
		}
		for _, t := range sortedKeys(a.Policy.Tools) {
			if !known(t) && !connectorKey(cfg, t) {
				c.add(CodeUnknownTool, "policy.tools."+t, fmt.Sprintf("unknown tool or group %q", t), didYouMean(suggest(t, candidates)))
			}
		}
	}
	// A connector's tools register as <id>.<tool> and are selected by id like
	// a group (docs/connectors.md §5), so an id must not be a built-in name.
	for _, id := range sortedKeys(cfg.Connectors) {
		if known(id) {
			at(FileConnectors, &ps).add(CodeBadValue, "connectors."+id,
				fmt.Sprintf("connector id %q is a built-in tool or group name", id), "choose another id")
		}
	}
	ps.sortStable()
	if ps.HasErrors() {
		return ps
	}
	return nil
}

// connectorKey reports whether a policy.tools key names a defined connector
// or one of its tools, `<id>.<tool>` (docs/connectors.md §6). The tool part
// is not checked: the server's tool list is runtime state, not config.
func connectorKey(cfg *Config, key string) bool {
	id, tool, dotted := strings.Cut(key, ".")
	if dotted && tool == "" {
		return false
	}
	_, ok := cfg.Connectors[id]
	return ok
}

// Definition returns the agent library's view of one agent. Core is left empty
// on purpose: the built-in core prompt fragment belongs to the agent package.
func (c *Config) Definition(name string) agent.Definition {
	a, ok := c.Agents[name]
	if !ok {
		return agent.Definition{}
	}
	return agent.Definition{
		Name:         a.Name,
		Persona:      a.Prompt,
		Tools:        append([]string(nil), a.Tools...),
		Connectors:   append([]string(nil), a.Connectors...),
		MaxSteps:     a.Limits.MaxSteps,
		ReplyReserve: a.Limits.ReplyReserve,
	}
}

// Model returns the provider-facing profile of a configured model.
func (c *Config) Model(name string) (provider.Model, error) {
	m, ok := c.Models[name]
	if !ok {
		return provider.Model{}, fmt.Errorf("config: unknown model %q", name)
	}
	return provider.Model{
		Name:          m.Name,
		Provider:      m.Provider,
		ID:            m.ID,
		API:           provider.API(m.API),
		URL:           m.URL,
		Context:       m.Context,
		Tools:         m.Tools,
		Vision:        m.Vision,
		Strict:        m.Strict,
		ThinkingTags:  append([]string(nil), m.ThinkingTags...),
		SchemaProfile: m.SchemaProfile,
		Reasoning:     provider.Reasoning(m.Reasoning),
		Timeout:       m.Timeout,
	}, nil
}

// ResolveEnv resolves an `env:VAR` reference. Anything else is returned
// unchanged, so callers can pass a plain value through.
func ResolveEnv(v string) (string, error) {
	rest, ok := strings.CutPrefix(v, "env:")
	if !ok {
		return v, nil
	}
	if rest == "" {
		return "", fmt.Errorf("config: %q names no environment variable", v)
	}
	val, set := os.LookupEnv(rest)
	if !set || val == "" {
		return "", fmt.Errorf("config: environment variable %s is not set", rest)
	}
	return val, nil
}

// --- environment ------------------------------------------------------------

// DefaultBrowserSocket is browser.socket when unset: the path the systemd
// unit and the container image use.
const DefaultBrowserSocket = "/run/holaclaw-browser/browser.sock"

// EnvOverride is an environment variable that sets a config.toml key and
// wins over it, so a container image can carry its defaults.
type EnvOverride struct {
	Env, Key string
}

// EnvOverrides are every key an environment variable may set.
var EnvOverrides = []EnvOverride{
	{"HOLACLAW_LISTEN", "server.listen"},
	{"HOLACLAW_ALLOW_INSECURE_TRANSPORT", "server.allow_insecure_transport"},
	{"HOLACLAW_PUBLIC_URL", "server.public_url"},
	{"HOLACLAW_BROWSER_ENABLED", "browser.enabled"},
}

// Value is the effective value of an EnvOverrides key, as TOML would write
// it.
func (c *Config) Value(key string) string {
	switch key {
	case "server.listen":
		return strconv.Quote(c.Server.Listen)
	case "server.allow_insecure_transport":
		return strconv.FormatBool(c.Server.AllowInsecureTransport)
	case "server.public_url":
		return strconv.Quote(c.Server.PublicURL)
	case "browser.enabled":
		return strconv.FormatBool(c.Browser.Enabled)
	}
	return ""
}

// applyEnv sets the EnvOverrides keys from the environment, and records in
// cfg.Sources where every one of them came from. An empty variable is unset.
func applyEnv(cfg *Config, root map[string]any, ps *Problems) {
	for _, o := range EnvOverrides {
		tbl, k, _ := strings.Cut(o.Key, ".")
		cfg.Sources[o.Key] = "default"
		if _, ok := table(root, tbl)[k]; ok {
			cfg.Sources[o.Key] = FileConfig
		}
		v := os.Getenv(o.Env)
		if v == "" {
			continue
		}
		var on bool
		if o.Key != "server.listen" && o.Key != "server.public_url" {
			var err error
			if on, err = strconv.ParseBool(v); err != nil {
				at(FileConfig, ps).add(CodeBadValue, o.Key,
					fmt.Sprintf("$%s is %q, not a boolean", o.Env, v), "set it to true or false")
				continue
			}
		}
		switch o.Key {
		case "server.listen":
			cfg.Server.Listen = v
		case "server.allow_insecure_transport":
			cfg.Server.AllowInsecureTransport = on
		case "server.public_url":
			if !absoluteURL(v) {
				at(FileConfig, ps).add(CodeBadValue, o.Key,
					fmt.Sprintf("$%s is %q, not an absolute http(s) URL", o.Env, v), `e.g. "https://holaclaw.example.com"`)
				continue
			}
			cfg.Server.PublicURL = strings.TrimSuffix(v, "/")
		case "browser.enabled":
			cfg.Browser.Enabled = on
		}
		cfg.Sources[o.Key] = "env"
	}
}

// --- reading ----------------------------------------------------------------

func fieldsOf(file string) []field {
	s, _ := schemaFor(file)
	return s.fields
}

func readTOML(dir, rel string, ps *Problems) (map[string]any, bool) {
	b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
	if err != nil {
		at(rel, ps).add(CodeMissingFile, "", "missing or unreadable file", "expected "+rel+" in the config directory")
		return nil, false
	}
	var m map[string]any
	if err := toml.Unmarshal(b, &m); err != nil {
		at(rel, ps).add(CodeBadValue, "", "invalid TOML: "+err.Error(), "")
		return nil, false
	}
	return m, true
}

func optionalTOML(dir, rel string, ps *Problems) (map[string]any, bool) {
	if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(rel))); err != nil {
		return nil, false
	}
	m, ok := readTOML(dir, rel, ps)
	return m, ok
}

// agentDirs lists agents/<name>/ directories; at least one is required.
// agentName is the shape of an agent name. A name becomes a directory (its
// workspace, its browser profile) and a URL path segment, so it is a closed
// alphabet, not "whatever the directory is called".
var agentName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)

// ValidAgentName reports whether name may be used as an agent name.
func ValidAgentName(name string) bool { return agentName.MatchString(name) }

func agentDirs(dir string, ps *Problems) []string {
	ents, err := os.ReadDir(filepath.Join(dir, "agents"))
	if err != nil {
		at("agents", ps).add(CodeMissingFile, "", "no agents directory", "a config directory needs at least one agents/<name>/")
		return nil
	}
	var out []string
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		if !ValidAgentName(e.Name()) {
			at(filepath.Join("agents", e.Name()), ps).add(CodeBadValue, "", fmt.Sprintf("agent name %q is not valid", e.Name()),
				"use lower-case letters, digits, `-` and `_`, starting with a letter or digit, at most 63 characters")
			continue
		}
		out = append(out, e.Name())
	}
	if len(out) == 0 {
		at("agents", ps).add(CodeMissingFile, "", "no agent defined", "a config directory needs at least one agents/<name>/")
	}
	sort.Strings(out)
	return out
}

// checkVersion validates a file's `version` against CurrentVersion and, for
// files after config.toml, against the directory version (want, 0 to skip).
func checkVersion(c collector, m map[string]any, want int) int {
	raw, present := m["version"]
	if !present {
		c.add(CodeBadVersion, "version", "version is missing", "add `version = "+fmt.Sprint(CurrentVersion)+"`")
		return 0
	}
	v, ok := raw.(int64)
	if !ok {
		c.add(CodeBadVersion, "version", "version must be an integer", "add `version = "+fmt.Sprint(CurrentVersion)+"`")
		return 0
	}
	got := int(v)
	switch {
	case got > CurrentVersion:
		c.add(CodeBadVersion, "version", fmt.Sprintf("version %d is newer than this build (%d)", got, CurrentVersion), "upgrade holaclaw")
	case got < 1:
		c.add(CodeBadVersion, "version", fmt.Sprintf("version %d is not a valid config version", got), "the first version is 1")
	case got < CurrentVersion:
		c.add(CodeBadVersion, "version", fmt.Sprintf("version %d is older than %d", got, CurrentVersion), "run `holaclaw config migrate`")
	}
	if want != 0 && got != want {
		c.add(CodeVersionMismatch, "version", fmt.Sprintf("version %d differs from %s (%d)", got, FileConfig, want),
			"every file of a config directory carries the same version")
	}
	return got
}

// --- walking ----------------------------------------------------------------

func walkTable(c collector, prefix string, m map[string]any, fs []field, top bool) {
	for _, k := range sortedKeys(m) {
		key := join(prefix, k)
		f, ok := findField(fs, k)
		if !ok {
			if top {
				if owner, known := topLevelKeys()[k]; known && owner != baseName(c.file) {
					c.add(CodeWrongFile, key, fmt.Sprintf("`%s` belongs in %s", k, owner), "move this table to "+owner)
					continue
				}
			}
			c.add(CodeUnknownKey, key, fmt.Sprintf("unknown key %q", k), didYouMean(suggest(k, names(fs))))
			continue
		}
		checkValue(c, key, m[k], f)
	}
	for _, f := range fs {
		if !f.required && !conditionMet(m, fs, f.reqIf) {
			continue
		}
		if top && f.name == "version" {
			continue // checkVersion owns it
		}
		if _, present := m[f.name]; !present {
			c.add(CodeMissingRequired, join(prefix, f.name), fmt.Sprintf("required key %q is missing", f.name), f.desc)
		}
	}
}

// conditionMet reports whether a requiredIf holds in the table being walked.
// An absent sibling counts as its documented default.
func conditionMet(m map[string]any, fs []field, r requiredIf) bool {
	if r.key == "" {
		return false
	}
	if s, ok := m[r.key].(string); ok {
		return s == r.value
	}
	sib, ok := findField(fs, r.key)
	return ok && sib.def == r.value
}

func checkValue(c collector, key string, v any, f field) {
	switch f.kind {
	case kInt:
		n, ok := v.(int64)
		if !ok {
			c.add(CodeWrongType, key, wrongType(v, "integer"), "")
			return
		}
		if f.nonNeg && n < 0 {
			c.add(CodeBadValue, key, fmt.Sprintf("%d is negative", n), "use 0 or more")
		}
	case kFloat:
		x, ok := floatValue(v)
		if !ok {
			c.add(CodeWrongType, key, wrongType(v, "float"), "")
			return
		}
		if x <= 0 || x > 1 {
			c.add(CodeBadValue, key, fmt.Sprintf("%v is out of range", x), "a score is greater than 0 and at most 1")
		}
	case kURL:
		s, ok := v.(string)
		if !ok {
			c.add(CodeWrongType, key, wrongType(v, "string"), "")
			return
		}
		if !absoluteURL(s) {
			c.add(CodeBadValue, key, fmt.Sprintf("%q is not an absolute http(s) URL", s), `e.g. "https://openrouter.ai/api/alpha/decisions"`)
		}
	case kPath:
		s, ok := v.(string)
		if !ok {
			c.add(CodeWrongType, key, wrongType(v, "string"), "")
			return
		}
		if !path.IsAbs(s) {
			hint := `e.g. "/run/holaclaw-browser/browser.sock"`
			if f.def != "" {
				hint = fmt.Sprintf("e.g. %q", f.def)
			}
			c.add(CodeBadValue, key, fmt.Sprintf("%q is not an absolute path", s), hint)
		}
	case kHostList:
		list, ok := v.([]any)
		if !ok {
			c.add(CodeWrongType, key, wrongType(v, "list of host names"), "")
			return
		}
		for i, e := range list {
			s, ok := e.(string)
			if !ok {
				c.add(CodeWrongType, fmt.Sprintf("%s[%d]", key, i), wrongType(e, "string"), "")
				continue
			}
			if !validHost(s) {
				c.add(CodeBadValue, fmt.Sprintf("%s[%d]", key, i), fmt.Sprintf("%q is not a valid host name", s),
					"a bare lower-case host name, e.g. \"example.com\"; no scheme, path, port, space or IP literal")
			}
		}
	case kBool:
		if _, ok := v.(bool); !ok {
			c.add(CodeWrongType, key, wrongType(v, "boolean"), "")
		}
	case kString:
		s, ok := v.(string)
		if !ok {
			c.add(CodeWrongType, key, wrongType(v, "string"), "")
			return
		}
		if len(f.enum) > 0 && !contains(f.enum, s) {
			c.add(CodeBadEnum, key, fmt.Sprintf("%q is not a valid value", s), "one of: "+quoted(f.enum))
			return
		}
		if f.envOnly && !strings.HasPrefix(s, "env:") {
			c.add(CodeLiteralSecret, key, "a literal secret must not appear in a config file",
				"put the secret in "+FileAuth+" or use an `env:VAR` reference")
			return
		}
		if f.ref != refNone && s != "" && c.refs != nil {
			*c.refs = append(*c.refs, refUse{file: c.file, key: key, kind: f.ref, value: s})
		}
	case kDuration:
		s, ok := v.(string)
		if !ok {
			c.add(CodeWrongType, key, wrongType(v, "duration string"), `e.g. "120s"`)
			return
		}
		if _, err := time.ParseDuration(s); err != nil {
			c.add(CodeBadValue, key, fmt.Sprintf("%q is not a duration", s), `e.g. "120s", "2m"`)
		}
	case kStringList:
		list, ok := v.([]any)
		if !ok {
			c.add(CodeWrongType, key, wrongType(v, "list of strings"), "")
			return
		}
		for i, e := range list {
			s, ok := e.(string)
			if !ok {
				c.add(CodeWrongType, fmt.Sprintf("%s[%d]", key, i), wrongType(e, "string"), "")
				continue
			}
			if f.ref != refNone && s != "" && c.refs != nil {
				*c.refs = append(*c.refs, refUse{file: c.file, key: fmt.Sprintf("%s[%d]", key, i), kind: f.ref, value: s})
			}
		}
	case kTable:
		sub, ok := v.(map[string]any)
		if !ok {
			c.add(CodeWrongType, key, wrongType(v, "table"), "")
			return
		}
		walkTable(c, key, sub, f.fields, false)
	case kMapTable:
		sub, ok := v.(map[string]any)
		if !ok {
			c.add(CodeWrongType, key, wrongType(v, "table"), "")
			return
		}
		for _, name := range sortedKeys(sub) {
			inner, ok := sub[name].(map[string]any)
			if !ok {
				c.add(CodeWrongType, join(key, name), wrongType(sub[name], "table"), "")
				continue
			}
			walkTable(c, join(key, name), inner, f.fields, false)
		}
	case kMapEnum:
		sub, ok := v.(map[string]any)
		if !ok {
			c.add(CodeWrongType, key, wrongType(v, "table"), "")
			return
		}
		for _, name := range sortedKeys(sub) {
			s, ok := sub[name].(string)
			if !ok {
				c.add(CodeWrongType, join(key, name), wrongType(sub[name], "string"), "quote dotted tool names")
				continue
			}
			if !contains(f.enum, s) {
				c.add(CodeBadEnum, join(key, name), fmt.Sprintf("%q is not a valid value", s), "one of: "+quoted(f.enum))
			}
		}
	}
}

func checkRefs(cfg *Config, refs []refUse, ps *Problems) {
	for _, r := range refs {
		var (
			pool []string
			what string
		)
		switch r.kind {
		case refProvider:
			pool, what = sortedKeys(cfg.Providers), "provider"
		case refModel:
			pool, what = sortedKeys(cfg.Models), "model"
		case refAgent:
			pool, what = sortedKeys(cfg.Agents), "agent"
		case refConnector:
			pool, what = sortedKeys(cfg.Connectors), "connector"
		default:
			continue
		}
		if contains(pool, r.value) {
			continue
		}
		hint := didYouMean(suggest(r.value, pool))
		switch {
		case hint != "":
		case len(pool) == 0:
			hint = "no " + what + " is defined"
		default:
			hint = "defined " + what + "s: " + quoted(pool)
		}
		at(r.file, ps).add(CodeDanglingRef, r.key, fmt.Sprintf("no %s named %q is defined", what, r.value), hint)
	}
}

// checkRoles checks what a reference alone cannot: a role only accepts the
// wire APIs it can use (docs/models.md), and `guard_mode = "auto"` needs something to
// decide with — auto with no guard would silently deny or allow every call.
func checkRoles(cfg *Config, ps *Problems) {
	for _, name := range sortedKeys(cfg.Agents) {
		a := cfg.Agents[name]
		c := at(path.Join("agents", name, FileAgent), ps)
		for _, r := range []struct{ role, model string }{
			{"chat", a.Models.Chat}, {"fallback", a.Models.Fallback}, {"guard", a.Models.Guard},
			{"summarize", a.Models.Summarize}, {"transcribe", a.Models.Transcribe},
			{"vision", a.Models.Vision}, {"embed", a.Models.Embed}, {"image", a.Models.Image},
			{"decide", a.Models.Decide}, {"plan", a.Models.Plan},
		} {
			m, ok := cfg.Models[r.model]
			if r.model == "" || !ok {
				continue // absent, or already reported as a dangling reference
			}
			if want := roleAPIs[r.role]; !contains(want, m.API) {
				c.add(CodeBadEnum, "models."+r.role,
					fmt.Sprintf("role %q cannot use model %q: its api is %q", r.role, m.Name, m.API),
					"this role needs a model with api "+quoted(want))
			}
		}
		if a.Policy.GuardMode == "auto" && a.Models.Guard == "" {
			c.add(CodeMissingRequired, "models.guard", `guard_mode = "auto" without a guard role`,
				`set models.guard, or use guard_mode = "recommend"`)
		}
	}
}

// checkBrowserTool is the one cross-file rule docs/browser.md needs: an agent that lists
// the browser tool needs the browser on, and must not have opted out of it.
// It works off the agent's own `tools` list, never the tool registry — the
// registry check belongs to Validate, and `browser.do` need not be registered
// yet for this rule to hold.
func checkBrowserTool(cfg *Config, ps *Problems) {
	for _, name := range sortedKeys(cfg.Agents) {
		a := cfg.Agents[name]
		t, listed := browserTool(a)
		switch {
		case !listed:
		case !a.Browser.Enabled:
			at(path.Join("agents", name, FileAgent), ps).add(CodeBadValue, "tools",
				fmt.Sprintf("agent %q lists tool %q but sets browser.enabled = false", name, t),
				"remove one of the two")
		case !cfg.Browser.Enabled:
			at(path.Join("agents", name, FileAgent), ps).add(CodeMissingRequired, "tools",
				fmt.Sprintf("agent %q lists tool %q but the browser is off", name, t),
				"add a [browser] table to config.toml, or set $HOLACLAW_BROWSER_ENABLED=true")
		}
	}
}

// browserTool returns the browser tool an agent lists, if it lists one.
func browserTool(a Agent) (string, bool) {
	for _, t := range a.Tools {
		if t == "browser" || t == "browser.do" {
			return t, true
		}
	}
	return "", false
}

// grantBrowser gives `browser.do` to every agent that does not opt out, when
// the browser is on: nobody has to list it.
func grantBrowser(cfg *Config) {
	if !cfg.Browser.Enabled {
		return
	}
	for name, a := range cfg.Agents {
		if _, listed := browserTool(a); listed || !a.Browser.Enabled {
			continue
		}
		a.Tools = append(slices.Clip(a.Tools), "browser.do")
		cfg.Agents[name] = a
	}
}

// checkSkillNames refuses a skills.disabled entry that can never name a
// skill, so a typo is caught instead of silently disabling nothing.
func checkSkillNames(cfg *Config, ps *Problems) {
	for _, name := range sortedKeys(cfg.Agents) {
		for i, s := range cfg.Agents[name].Skills.Disabled {
			if !skill.ValidName(s) {
				at(path.Join("agents", name, FileAgent), ps).add(CodeBadValue, fmt.Sprintf("skills.disabled[%d]", i),
					fmt.Sprintf("%q is not a skill name", s), "lower-case letters, digits and dashes, at most 64")
			}
		}
	}
}

// checkConnectorGrants refuses a connector granted twice to one agent: a
// duplicate would register its tools twice.
func checkConnectorGrants(cfg *Config, ps *Problems) {
	for _, name := range sortedKeys(cfg.Agents) {
		seen := map[string]bool{}
		for i, id := range cfg.Agents[name].Connectors {
			if seen[id] {
				at(path.Join("agents", name, FileAgent), ps).add(CodeBadValue, fmt.Sprintf("connectors[%d]", i),
					fmt.Sprintf("connector %q is listed twice", id), "list each connector once")
			}
			seen[id] = true
		}
	}
}

// --- assignment -------------------------------------------------------------
//
// Values are read back from the decoded map with tolerant getters: anything
// ill-typed already produced a problem, so a zero value here is harmless.

func loadConfigFile(cfg *Config, m map[string]any) {
	s := table(m, "server")
	cfg.Server = Server{
		Socket:     str(s, "socket"),
		Listen:     str(s, "listen"),
		TLSCert:    str(s, "tls_cert"),
		TLSKey:     str(s, "tls_key"),
		DataDir:    str(s, "data_dir"),
		SecretsDir: str(s, "secrets_dir"),

		AllowInsecureTransport: boolean(s, "allow_insecure_transport"),
		PublicURL:              strings.TrimSuffix(str(s, "public_url"), "/"),
	}
	for name, raw := range table(m, "providers") {
		p, _ := raw.(map[string]any)
		auth := str(p, "auth")
		if auth == "" {
			auth = "none"
		}
		cfg.Providers[name] = Provider{
			Name:          name,
			Type:          str(p, "type"),
			BaseURL:       str(p, "base_url"),
			Auth:          auth,
			APIKey:        str(p, "api_key"),
			MaxConcurrent: integer(p, "max_concurrent"),
			Sort:          str(p, "sort"),
		}
	}
	for name, raw := range table(m, "models") {
		md, _ := raw.(map[string]any)
		cfg.Models[name] = Model{
			Name:          name,
			Provider:      str(md, "provider"),
			ID:            str(md, "id"),
			API:           or(str(md, "api"), apiChat),
			URL:           str(md, "url"),
			Context:       integer(md, "context"),
			Tools:         boolean(md, "tools"),
			Vision:        boolean(md, "vision"),
			Strict:        boolean(md, "strict"),
			ThinkingTags:  strList(md, "thinking_tags"),
			SchemaProfile: str(md, "schema_profile"),
			Reasoning:     str(md, "reasoning"),
			Timeout:       duration(md, "timeout"),
		}
	}
	days := integer(table(m, "retention"), "events_days")
	if days == 0 {
		days = 30 // default retention; deleting a session still cascades
	}
	cfg.Retention = Retention{EventsDays: days}
	cfg.Backups = Backups{
		Every: duration(table(m, "backups"), "every"),
		Keep:  integerOr(table(m, "backups"), "keep", DefaultBackupsKeep),
	}
	if cfg.Backups.Every <= 0 {
		cfg.Backups.Every = DefaultBackupsEvery
	}

	b := table(m, "browser")
	_, present := m["browser"]
	cfg.Browser = Browser{
		Enabled:  present && boolOr(b, "enabled", true),
		Socket:   or(str(b, "socket"), DefaultBrowserSocket),
		Binary:   str(b, "binary"),
		Headless: boolean(b, "headless"),
	}
}

func loadChannelsFile(cfg *Config, m map[string]any) {
	for name, raw := range table(m, "channels") {
		ch, _ := raw.(map[string]any)
		acc := table(ch, "access")
		cfg.Channels[name] = Channel{
			Name:  name,
			Type:  str(ch, "type"),
			Agent: str(ch, "agent"),
			Access: Access{
				DM:      or(str(acc, "dm"), "invite"),
				Rooms:   or(str(acc, "rooms"), "off"),
				Reply:   or(str(acc, "reply"), "mention"),
				ReplyTo: or(str(acc, "reply_to"), "rooms"),
			},
		}
	}
}

// loadConnectorsFile fills cfg.Connectors and checks the one thing the schema
// cannot: the shape of an id. A bad id is still loaded, so an agent naming it
// is not reported twice.
func loadConnectorsFile(cfg *Config, m map[string]any, ps *Problems) {
	for _, id := range sortedKeys(table(m, "connectors")) {
		cn, _ := table(m, "connectors")[id].(map[string]any)
		if !ValidConnectorID(id) {
			// Quoted: an id holding a dot would otherwise read as a nested key.
			at(FileConnectors, ps).add(CodeBadValue, fmt.Sprintf("connectors.%q", id), fmt.Sprintf("connector id %q is not valid", id),
				"use lower-case letters, digits and `-`, at most 32 characters")
		}
		cfg.Connectors[id] = Connector{
			ID:       id,
			URL:      str(cn, "url"),
			Auth:     str(cn, "auth"),
			Scopes:   strList(cn, "scopes"),
			ClientID: str(cn, "client_id"),
			Trusted:  boolean(cn, "trusted"),

			AllowInsecureTransport: boolean(cn, "allow_insecure_transport"),
		}
	}
}

// ParseConnectors reads a connectors.toml on its own, with the checks Load
// runs on it; the error is a Problems list. For the daemon, which writes the
// file, and its tests.
func ParseConnectors(b []byte) (map[string]Connector, error) {
	var ps Problems
	cfg := &Config{Connectors: map[string]Connector{}}
	var m map[string]any
	if err := toml.Unmarshal(b, &m); err != nil {
		at(FileConnectors, &ps).add(CodeBadValue, "", "invalid TOML: "+err.Error(), "")
		return nil, ps
	}
	checkVersion(at(FileConnectors, &ps), m, 0)
	walkTable(at(FileConnectors, &ps), "", m, fieldsOf(FileConnectors), true)
	loadConnectorsFile(cfg, m, &ps)
	ps.sortStable()
	if ps.HasErrors() {
		return cfg.Connectors, ps
	}
	return cfg.Connectors, nil
}

// ConnectorEntry is c as a connectors.toml entry, the keys loadConnectorsFile
// reads; a zero value is left out.
func ConnectorEntry(c Connector) map[string]any {
	e := map[string]any{"url": c.URL, "auth": c.Auth}
	if c.Scopes != nil {
		e["scopes"] = c.Scopes
	}
	if c.ClientID != "" {
		e["client_id"] = c.ClientID
	}
	if c.Trusted {
		e["trusted"] = true
	}
	if c.AllowInsecureTransport {
		e["allow_insecure_transport"] = true
	}
	return e
}

func loadAgentFile(name string, m map[string]any) Agent {
	roles := table(m, "models")
	pol := table(m, "policy")
	lim := table(m, "limits")
	classes := Classes{
		Read:     or(str(pol, "read"), "allow"),
		Write:    or(str(pol, "write"), "allow"),
		External: or(str(pol, "external"), "allow"),
		Exec:     or(str(pol, "exec"), "deny"),
	}
	guest := table(pol, "guest")
	task := table(pol, "task")
	tools := map[string]string{}
	for k, v := range table(pol, "tools") {
		if s, ok := v.(string); ok {
			tools[k] = s
		}
	}
	return Agent{
		Name:       name,
		Version:    integer(m, "version"),
		Tools:      strList(m, "tools"),
		Connectors: strList(m, "connectors"),
		Models: Roles{
			Chat:       str(roles, "chat"),
			Fallback:   str(roles, "fallback"),
			Guard:      str(roles, "guard"),
			Summarize:  str(roles, "summarize"),
			Transcribe: str(roles, "transcribe"),
			Vision:     str(roles, "vision"),
			Embed:      str(roles, "embed"),
			Image:      str(roles, "image"),
			Decide:     str(roles, "decide"),
			Plan:       str(roles, "plan"),
		},
		Policy: Policy{
			Classes: classes,
			Tools:   tools,
			// Guests get read only unless the agent says otherwise; `check`
			// refuses open access combined with a permissive guest policy.
			Guest: Classes{
				Read:     or(str(guest, "read"), "allow"),
				Write:    or(str(guest, "write"), "deny"),
				External: or(str(guest, "external"), "deny"),
				Exec:     or(str(guest, "exec"), "deny"),
			},
			// Detached runs default to the agent's own policy; the gate turns
			// `ask` into `deny` there unless the task carries a grant (docs/security.md).
			Task: Classes{
				Read:     or(str(task, "read"), classes.Read),
				Write:    or(str(task, "write"), classes.Write),
				External: or(str(task, "external"), classes.External),
				Exec:     or(str(task, "exec"), classes.Exec),
			},
			ExecAllow:       strList(pol, "exec_allow"),
			TaintEscalates:  boolean(pol, "taint_escalates"),
			GuardMode:       or(str(pol, "guard_mode"), "recommend"),
			GuardThreshold:  floating(pol, "guard_threshold", 0.7),
			TaintDecayCalls: integerOr(pol, "taint_decay_calls", 3),
			TaintThreshold:  floating(pol, "taint_threshold", 0.5),
		},
		Limits: Limits{
			MaxSteps:     integer(lim, "max_steps"),
			ReplyReserve: integer(lim, "reply_reserve"),
			// 0 is a configured refusal, absent is the default.
			ImagesPerDay: integerOr(lim, "images_per_day", 20),
		},
		Browser: AgentBrowser{
			Enabled: boolOr(table(m, "browser"), "enabled", true),
			Allow:   strList(table(m, "browser"), "allow"),
		},
		Skills: AgentSkills{
			Disabled: strList(table(m, "skills"), "disabled"),
		},
	}
}

// --- helpers ----------------------------------------------------------------

func findField(fs []field, name string) (field, bool) {
	for _, f := range fs {
		if f.name == name {
			return f, true
		}
	}
	return field{}, false
}

// floatValue accepts a TOML float, or an integer for the round values a float
// key can legitimately take (`guard_threshold = 1`).
func floatValue(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case int64:
		return float64(t), true
	}
	return 0, false
}

// absoluteURL reports whether s is an absolute http(s) URL with a host.
func absoluteURL(s string) bool {
	u, err := url.Parse(s)
	if err != nil {
		return false
	}
	return (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

// validHost reports whether s is a bare lower-case host name: dot-separated
// labels of letters, digits and internal hyphens, no scheme, path, port,
// space or IP literal (docs/browser.md restricts `browser.allow` to v1's
// simplest case; subdomains are always included without a wildcard).
func validHost(s string) bool {
	if s == "" || s != strings.ToLower(s) {
		return false
	}
	if net.ParseIP(s) != nil {
		return false
	}
	for _, label := range strings.Split(s, ".") {
		if label == "" || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, r := range label {
			if r != '-' && (r < 'a' || r > 'z') && (r < '0' || r > '9') {
				return false
			}
		}
	}
	return true
}

func table(m map[string]any, k string) map[string]any {
	v, _ := m[k].(map[string]any)
	return v
}

func str(m map[string]any, k string) string {
	v, _ := m[k].(string)
	return v
}

func integer(m map[string]any, k string) int {
	if v, ok := m[k].(int64); ok {
		return int(v)
	}
	return 0
}

// integerOr is integer with a default for an absent (or ill-typed) key, so a
// configured 0 stays 0.
func integerOr(m map[string]any, k string, def int) int {
	if v, ok := m[k].(int64); ok {
		return int(v)
	}
	return def
}

func floating(m map[string]any, k string, def float64) float64 {
	if x, ok := floatValue(m[k]); ok {
		return x
	}
	return def
}

func boolOr(m map[string]any, k string, def bool) bool {
	if v, ok := m[k].(bool); ok {
		return v
	}
	return def
}

func boolean(m map[string]any, k string) bool {
	v, _ := m[k].(bool)
	return v
}

func strList(m map[string]any, k string) []string {
	list, ok := m[k].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(list))
	for _, e := range list {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func duration(m map[string]any, k string) time.Duration {
	s, ok := m[k].(string)
	if !ok {
		return 0
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0
	}
	return d
}

func or(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func join(prefix, k string) string {
	if prefix == "" {
		return k
	}
	return prefix + "." + k
}

// baseName is the file name a schema is keyed by ("agents/main/agent.toml" is
// described by agent.toml).
func baseName(file string) string { return path.Base(file) }

func contains(ss []string, s string) bool {
	for _, e := range ss {
		if e == s {
			return true
		}
	}
	return false
}

func quoted(ss []string) string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = fmt.Sprintf("%q", s)
	}
	return strings.Join(out, ", ")
}

func didYouMean(s string) string {
	if s == "" {
		return ""
	}
	return fmt.Sprintf("did you mean %q?", s)
}

func wrongType(v any, want string) string {
	return fmt.Sprintf("expected %s, got %s", want, tomlType(v))
}

func tomlType(v any) string {
	switch v.(type) {
	case string:
		return "string"
	case int64:
		return "integer"
	case float64:
		return "float"
	case bool:
		return "boolean"
	case []any:
		return "list"
	case map[string]any:
		return "table"
	case time.Time:
		return "datetime"
	case nil:
		return "nothing"
	}
	return fmt.Sprintf("%T", v)
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
