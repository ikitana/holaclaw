// Package config loads and validates the fixed set of TOML files that make up
// a holaclaw config directory (ARCHITECTURE.md §4): `config.toml` (required),
// `channels.toml` and `connectors.toml` (optional) and one or more
// `agents/<name>/` units. Parsing is strict: every problem carries a stable
// code, the file, the key path and a fix hint, and Load collects all of them
// instead of stopping at the first.
//
// `auth.toml` holds every secret and is owned by another package; its name is
// reserved here so the migration ladder never touches it.
package config

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// The fixed file names of a config directory.
const (
	FileConfig   = "config.toml"
	FileChannels = "channels.toml"
	// FileConnectors is the one config file the daemon also writes, over the
	// API (docs/connectors.md §4): a machine file, comments not preserved.
	FileConnectors = "connectors.toml"
	FileAgent      = "agent.toml"
	FilePrompt     = "prompt.md"
	FileAuth       = "auth.toml" // reserved: secrets, never parsed or migrated here
)

// kind is the value shape of one config key. It drives type checking, the
// JSON Schema export and the key count — all from this one description.
type kind string

const (
	kInt        kind = "integer"
	kFloat      kind = "float" // float in (0, 1]: the only floats here are scores
	kString     kind = "string"
	kBool       kind = "boolean"
	kDuration   kind = "duration" // Go duration string, e.g. "90s"
	kURL        kind = "url"      // absolute http(s) URL
	kPath       kind = "path"     // absolute filesystem path; existence is check's job
	kStringList kind = "list"
	kHostList   kind = "host-list" // list of bare lower-case host names, no scheme/path/port/IP
	kTable      kind = "table"     // fixed sub-table
	kMapTable   kind = "map-table" // user-named sub-tables, all of one shape
	kMapEnum    kind = "map-enum"  // user-named keys with an enum value
)

// ref is the kind of cross-file reference a string key holds, checked after
// every file is parsed.
type ref string

const (
	refNone      ref = ""
	refProvider  ref = "provider"
	refModel     ref = "model"
	refAgent     ref = "agent"
	refConnector ref = "connector"
)

// requiredIf makes a key required only when a sibling key of the same table
// holds a given value. A sibling that is absent counts as its documented
// default, so `models.<name>.context` is required for a model that says
// nothing about `api` (docs/models.md).
type requiredIf struct {
	key   string
	value string
}

// field is one key of the schema.
type field struct {
	name     string
	kind     kind
	enum     []string // allowed values for a string or map-enum key
	required bool
	reqIf    requiredIf // required only under this condition
	ref      ref
	envOnly  bool   // only `env:VAR` references allowed (never a literal secret)
	nonNeg   bool   // kInt: a negative value is a problem
	def      string // documented default, for the schema export
	desc     string
	fields   []field // kTable / kMapTable members
}

// fileSchema describes the root table of one config file.
type fileSchema struct {
	file   string
	desc   string
	fields []field
}

// decisions are the policy verdicts of docs/security.md.
var decisions = []string{"allow", "ask", "deny"}

// The wire APIs a model can speak (docs/models.md). A model that says nothing
// speaks "chat"; loadConfigFile fills that in, so Model.API is never empty.
const (
	apiChat      = "chat"
	apiDecisions = "decisions"
	apiImages    = "images"
)

var modelAPIs = []string{apiChat, apiDecisions, apiImages}

// reasoningEfforts is the union of the efforts OpenAI, OpenRouter and Ollama
// accept. Which of them a model supports varies (Qwen3.8 on Ollama refuses
// xhigh), so a value is not checked against the model: its server refuses
// one the model does not know.
var reasoningEfforts = []string{"", "none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra"}

// roleAPIs is the APIs each model role accepts. The guard is the one role with
// a choice: a decision model (recommended) or any chat model.
var roleAPIs = map[string][]string{
	"chat":       {apiChat},
	"fallback":   {apiChat},
	"guard":      {apiChat, apiDecisions},
	"summarize":  {apiChat},
	"transcribe": {apiChat},
	"vision":     {apiChat},
	"embed":      {apiChat},
	"image":      {apiImages},
	"decide":     {apiDecisions},
	"plan":       {apiChat},
}

// The guard modes of docs/security.md.
var guardModes = []string{"recommend", "auto"}

// classFields are the four risk classes a policy table decides on. Used for
// the agent policy and, with different defaults, for guests and task runs.
func classFields(read, write, external, exec string) []field {
	return []field{
		{name: "read", kind: kString, enum: decisions, def: read, desc: "read-class tools"},
		{name: "write", kind: kString, enum: decisions, def: write, desc: "write-class tools"},
		{name: "external", kind: kString, enum: decisions, def: external, desc: "external-class tools"},
		{name: "exec", kind: kString, enum: decisions, def: exec, desc: "exec-class tools"},
	}
}

// schemas is the single description every other part of this package reads.
var schemas = []fileSchema{
	{
		file: FileConfig,
		desc: "Server, providers and models.",
		fields: []field{
			{name: "version", kind: kInt, required: true, desc: "config directory version"},
			{name: "server", kind: kTable, desc: "control API and storage locations", fields: []field{
				{name: "socket", kind: kString, desc: "unix socket path for the control API"},
				{name: "listen", kind: kString, desc: "optional TCP address; off-loopback needs a token and TLS; $HOLACLAW_LISTEN wins"},
				{name: "tls_cert", kind: kString, desc: "TLS certificate file for listen"},
				{name: "tls_key", kind: kString, desc: "TLS key file for listen"},
				{name: "allow_insecure_transport", kind: kBool, def: "false", desc: "serve an off-loopback listen over plain http, without TLS; a token is still required; $HOLACLAW_ALLOW_INSECURE_TRANSPORT wins"},
				{name: "public_url", kind: kURL, desc: "the address devices reach the control API at, for pairing links; unset, they use listen unless it is loopback or a wildcard; $HOLACLAW_PUBLIC_URL wins"},
				{name: "data_dir", kind: kString, desc: "directory holding the SQLite file and the blob store; relative to the config directory, which holds it by default"},
				{name: "secrets_dir", kind: kPath, def: "/etc/holaclaw-secrets", desc: "absolute path of the directory holding auth.toml"},
			}},
			{name: "providers", kind: kMapTable, desc: "named provider connections", fields: []field{
				{name: "type", kind: kString, enum: []string{"openai-compatible", "codex"}, required: true, desc: "compiled-in provider type"},
				{name: "base_url", kind: kString, desc: "endpoint base URL"},
				{name: "auth", kind: kString, enum: []string{"none", "api-key", "subscription"}, def: "none", desc: "auth strategy"},
				{name: "api_key", kind: kString, envOnly: true, desc: "`env:VAR` reference; literal secrets belong in auth.toml"},
				{name: "max_concurrent", kind: kInt, desc: "in-flight request limit for this provider"},
				{name: "sort", kind: kString, enum: []string{"throughput", "latency", "price"}, desc: "routing preference for a multi-provider gateway (OpenRouter provider.sort); throughput is the fast choice"},
			}},
			{name: "models", kind: kMapTable, desc: "named model capability profiles", fields: []field{
				{name: "provider", kind: kString, required: true, ref: refProvider, desc: "provider instance serving this model"},
				{name: "id", kind: kString, required: true, desc: "model id on the wire"},
				{name: "api", kind: kString, enum: modelAPIs, def: apiChat, desc: "wire API this model speaks"},
				{name: "url", kind: kURL, reqIf: requiredIf{"api", apiDecisions}, desc: "absolute endpoint override; required for a decisions model"},
				{name: "context", kind: kInt, reqIf: requiredIf{"api", apiChat}, desc: "context window in tokens; required for a chat model, ignored otherwise"},
				{name: "tools", kind: kBool, desc: "supports native tool calling"},
				{name: "vision", kind: kBool, desc: "accepts image parts"},
				{name: "strict", kind: kBool, desc: "promote text-leaked tool calls"},
				{name: "thinking_tags", kind: kStringList, desc: "extra reasoning-tag names to scrub"},
				{name: "schema_profile", kind: kString, enum: []string{"", "llamacpp"}, desc: "strips grammar-hostile JSON-schema keywords"},
				{name: "reasoning", kind: kString, enum: reasoningEfforts, desc: "reasoning effort of every call that sets none: none turns thinking off, the others are passed through and the model may refuse one it does not support; unset = the model's default"},
				{name: "timeout", kind: kDuration, desc: "whole-request timeout, e.g. \"120s\""},
			}},
			{name: "retention", kind: kTable, desc: "how long traceability data is kept", fields: []field{
				{name: "events_days", kind: kInt, def: "30", desc: "days of trace events to keep; the ids of sent messages, which route reactions, are kept as long"},
			}},
			{name: "backups", kind: kTable, desc: "the daemon's own backups (ARCHITECTURE.md §4)", fields: []field{
				{name: "every", kind: kDuration, def: "24h", desc: "how often the daemon takes an automatic backup, e.g. \"24h\""},
				{name: "keep", kind: kInt, def: "7", nonNeg: true, desc: "automatic backups to keep, the oldest deleted first; 0 takes none. Backups somebody asked for are never deleted"},
			}},
			{name: "browser", kind: kTable, desc: "browser sidecar connection (docs/browser.md); absent = no browser unless $HOLACLAW_BROWSER_ENABLED is true", fields: []field{
				{name: "enabled", kind: kBool, def: "true", desc: "turn the browser on, giving every agent browser.do; $HOLACLAW_BROWSER_ENABLED wins"},
				{name: "socket", kind: kPath, def: DefaultBrowserSocket, desc: "unix socket path where `holaclaw browser` listens"},
				{name: "binary", kind: kPath, desc: "absolute path to the browser binary; default: discovered at runtime"},
				{name: "headless", kind: kBool, def: "false", desc: "run without a visible window; default: a window on Xvfb"},
			}},
		},
	},
	{
		file: FileChannels,
		desc: "Channel instances and their access knobs. Absent = no external channels.",
		fields: []field{
			{name: "version", kind: kInt, required: true, desc: "config directory version"},
			{name: "channels", kind: kMapTable, desc: "named channel instances", fields: []field{
				{name: "type", kind: kString, enum: []string{"telegram", "slack", "googlechat", "whatsapp"}, required: true, desc: "channel adapter"},
				{name: "agent", kind: kString, required: true, ref: refAgent, desc: "agent serving this channel"},
				{name: "access", kind: kTable, desc: "who may talk (docs/identity.md)", fields: []field{
					{name: "dm", kind: kString, enum: []string{"invite", "open"}, def: "invite", desc: "direct messages from unknown senders"},
					{name: "rooms", kind: kString, enum: []string{"off", "approved", "open"}, def: "off", desc: "room conversations"},
					{name: "reply", kind: kString, enum: []string{"mention", "always"}, def: "mention", desc: "when to reply in a conversation"},
					{name: "reply_to", kind: kString, enum: []string{"rooms", "always", "off"}, def: "rooms", desc: "where an answer is a native reply to the message that asked"},
				}},
			}},
		},
	},
	{
		file: FileConnectors,
		desc: "Connectors to remote MCP servers (docs/connectors.md). Written by the daemon over the API, or by hand. Absent = no connectors.",
		fields: []field{
			{name: "version", kind: kInt, required: true, desc: "config directory version"},
			{name: "connectors", kind: kMapTable, desc: "named connectors; an id is [a-z0-9-], at most 32 characters", fields: []field{
				{name: "url", kind: kURL, required: true, desc: "the server's Streamable HTTP endpoint"},
				{name: "auth", kind: kString, enum: []string{"none", "oauth"}, required: true, desc: "how the daemon authenticates; tokens live in auth.toml"},
				{name: "scopes", kind: kStringList, desc: "OAuth scopes to request; absent = every scope the server offers"},
				{name: "client_id", kind: kString, desc: "pre-registered client id, only for a server that requires one"},
				{name: "trusted", kind: kBool, def: "false", desc: "results are still wrapped as untrusted content but never scored or arm the taint"},
				{name: "allow_insecure_transport", kind: kBool, def: "false", desc: "reach the server over plain http on any host, or https with a certificate that does not verify"},
			}},
		},
	},
	{
		file: FileAgent,
		desc: "One agent: tools, model roles, policy and limits.",
		fields: []field{
			{name: "version", kind: kInt, required: true, desc: "config directory version"},
			{name: "tools", kind: kStringList, desc: "tool names or groups this agent may use"},
			{name: "connectors", kind: kStringList, ref: refConnector, desc: "connector ids from connectors.toml whose tools this agent may use"},
			{name: "models", kind: kTable, desc: "model roles (docs/models.md)", fields: []field{
				{name: "chat", kind: kString, required: true, ref: refModel, desc: "the agent's main model"},
				{name: "fallback", kind: kString, ref: refModel, desc: "opt-in second model; every use is traced"},
				{name: "guard", kind: kString, ref: refModel, desc: "classifier for tool calls"},
				{name: "summarize", kind: kString, ref: refModel, desc: "compaction and recall condensing"},
				{name: "transcribe", kind: kString, ref: refModel, desc: "audio to text"},
				{name: "vision", kind: kString, ref: refModel, desc: "describes images for a blind chat model"},
				{name: "embed", kind: kString, ref: refModel, desc: "vectors for recall and notes"},
				{name: "image", kind: kString, ref: refModel, desc: "image generation for the image.generate tool"},
				{name: "decide", kind: kString, ref: refModel, desc: "decision model that binds elements and verifies each browser move"},
				{name: "plan", kind: kString, ref: refModel, desc: "chat model that chooses the browser's moves on the page it has read; unset = the chat role (a fast small model is enough)"},
			}},
			{name: "policy", kind: kTable, desc: "tool policy per risk class (docs/security.md)", fields: append(
				classFields("allow", "allow", "allow", "deny"),
				field{name: "tools", kind: kMapEnum, enum: decisions, desc: "per-tool overrides; quote dotted names"},
				field{name: "guest", kind: kTable, desc: "policy for guests", fields: classFields("allow", "deny", "deny", "deny")},
				field{name: "task", kind: kTable, desc: "policy for detached task runs", fields: classFields("allow", "allow", "allow", "deny")},
				field{name: "exec_allow", kind: kStringList, desc: "exec command allow-list, e.g. \"git status\""},
				field{name: "taint_escalates", kind: kBool, desc: "untrusted content moves exec/external from allow to ask"},
				field{name: "guard_mode", kind: kString, enum: guardModes, def: "recommend", desc: "how a guard verdict is used; \"auto\" needs a guard role"},
				field{name: "guard_threshold", kind: kFloat, def: "0.7", desc: "score at or above which the guard calls a tool call safe"},
				field{name: "taint_decay_calls", kind: kInt, nonNeg: true, def: "3", desc: "tool calls after untrusted content enters the turn that still count as tainted"},
				field{name: "taint_threshold", kind: kFloat, def: "0.5", desc: "taint score at or above which untrusted content escalates policy"},
			)},
			{name: "limits", kind: kTable, desc: "per-turn limits; unset means the built-in default", fields: []field{
				{name: "max_steps", kind: kInt, desc: "hard step limit per turn"},
				{name: "reply_reserve", kind: kInt, desc: "tokens reserved for the reply"},
				{name: "images_per_day", kind: kInt, nonNeg: true, def: "20", desc: "generated images per day; 0 refuses every call"},
			}},
			{name: "browser", kind: kTable, desc: "browser executor navigation limits (docs/browser.md)", fields: []field{
				{name: "enabled", kind: kBool, def: "true", desc: "false keeps browser.do from this agent when the browser is on"},
				{name: "allow", kind: kHostList, desc: "host allow-list this agent's browser may navigate to; unset or empty = any site"},
			}},
			{name: "skills", kind: kTable, desc: "skills (docs/skills.md)", fields: []field{
				{name: "disabled", kind: kStringList, desc: "skill names kept out of this agent's index at every level"},
			}},
		},
	},
}

// schemaFor returns the description of one config file.
func schemaFor(file string) (fileSchema, bool) {
	for _, s := range schemas {
		if s.file == file {
			return s, true
		}
	}
	return fileSchema{}, false
}

// KeyCount is the total number of distinct config keys across all files: every
// leaf key once, a user-named table counted through its template rather than
// per instance. ARCHITECTURE.md §1 tracks this number in CI.
func KeyCount() int {
	n := 0
	for _, s := range schemas {
		n += countFields(s.fields)
	}
	return n
}

func countFields(fs []field) int {
	n := 0
	for _, f := range fs {
		switch f.kind {
		case kTable, kMapTable:
			n += countFields(f.fields)
		default:
			n++
		}
	}
	return n
}

// Schema returns the JSON Schema (draft 2020-12) of one config file, generated
// from the same description that drives parsing (ARCHITECTURE.md §7).
func Schema(file string) ([]byte, error) {
	s, ok := schemaFor(file)
	if !ok {
		return nil, fmt.Errorf("config: no schema for %q", file)
	}
	doc := jsonObject(s.fields)
	doc["$schema"] = "https://json-schema.org/draft/2020-12/schema"
	doc["$id"] = "https://holaclaw.dev/schema/v" + fmt.Sprint(CurrentVersion) + "/" + file
	doc["title"] = file
	doc["description"] = s.desc
	return json.MarshalIndent(doc, "", "  ")
}

// SchemaFiles lists the files Schema accepts, in load order.
func SchemaFiles() []string {
	out := make([]string, 0, len(schemas))
	for _, s := range schemas {
		out = append(out, s.file)
	}
	return out
}

// CodeMeaning is one problem code this package defines and its one-line
// meaning, for documentation (internal/check/RULES.md and the generated
// config reference).
type CodeMeaning struct {
	Code    string
	Meaning string
}

// Codes lists every CFGxxx problem code, in id order, with a one-line
// meaning. Kept in sync with the doc comment above the code constants in
// problem.go; this is the one exported list a generator can read instead of
// duplicating the codes by hand.
func Codes() []CodeMeaning {
	return []CodeMeaning{
		{CodeUnknownKey, `Unknown key, with a "did you mean" over the valid keys of that table`},
		{CodeWrongFile, "Key in the wrong file; the message names the right one"},
		{CodeWrongType, "Wrong value type for the key"},
		{CodeBadEnum, "Value outside the allowed set for the key"},
		{CodeMissingRequired, "Required key missing"},
		{CodeDanglingRef, "Dangling reference: model to provider, role to model, channel to agent, agent to connector"},
		{CodeBadVersion, "`version` missing, not an integer, or not CurrentVersion"},
		{CodeVersionMismatch, "Files of one directory carry different versions"},
		{CodeLiteralSecret, "A literal secret where only an `env:VAR` reference is allowed"},
		{CodeUnknownTool, "Unknown tool name (only checked by Validate, which knows the registry)"},
		{CodeBadValue, "Malformed value: unparseable TOML, bad duration, non-positive count, a bad connector id or one that is a built-in tool or group name, a connector granted twice"},
		{CodeMissingFile, "Missing or unreadable file in the config directory"},
	}
}

func jsonObject(fs []field) map[string]any {
	props := map[string]any{}
	var required []string
	for _, f := range fs {
		props[f.name] = jsonField(f)
		if f.required {
			required = append(required, f.name)
		}
	}
	obj := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		sort.Strings(required)
		obj["required"] = required
	}
	if cond := jsonConditional(fs); len(cond) > 0 {
		obj["allOf"] = cond
	}
	return obj
}

// jsonConditional renders every requiredIf as an if/then pair, so a validator
// applies the same rule the parser does. When the condition matches the
// sibling's own default, an absent sibling matches too.
func jsonConditional(fs []field) []any {
	var out []any
	for _, f := range fs {
		if f.reqIf.key == "" {
			continue
		}
		sib, ok := findField(fs, f.reqIf.key)
		if !ok {
			continue
		}
		var cond any = map[string]any{
			"properties": map[string]any{f.reqIf.key: map[string]any{"const": f.reqIf.value}},
			"required":   []any{f.reqIf.key},
		}
		if sib.def == f.reqIf.value {
			cond = map[string]any{"anyOf": []any{cond, map[string]any{"not": map[string]any{"required": []any{f.reqIf.key}}}}}
		}
		out = append(out, map[string]any{"if": cond, "then": map[string]any{"required": []any{f.name}}})
	}
	return out
}

func jsonField(f field) map[string]any {
	var m map[string]any
	switch f.kind {
	case kTable:
		m = jsonObject(f.fields)
	case kMapTable:
		m = map[string]any{"type": "object", "additionalProperties": jsonObject(f.fields)}
	case kMapEnum:
		m = map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string", "enum": toAny(f.enum)}}
	case kStringList:
		m = map[string]any{"type": "array", "items": map[string]any{"type": "string"}}
	case kInt:
		m = map[string]any{"type": "integer"}
		if f.nonNeg {
			m["minimum"] = 0
		}
	case kFloat:
		m = map[string]any{"type": "number", "exclusiveMinimum": 0, "maximum": 1}
	case kBool:
		m = map[string]any{"type": "boolean"}
	case kDuration:
		m = map[string]any{"type": "string", "pattern": `^[0-9]+(\.[0-9]+)?(ns|us|ms|s|m|h)$`}
	case kURL:
		m = map[string]any{"type": "string", "format": "uri", "pattern": `^https?://`}
	case kPath:
		m = map[string]any{"type": "string", "pattern": `^/`}
	case kHostList:
		m = map[string]any{"type": "array", "items": map[string]any{"type": "string", "pattern": `^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*$`}}
	default:
		m = map[string]any{"type": "string"}
		if len(f.enum) > 0 {
			m["enum"] = toAny(f.enum)
		}
	}
	if f.desc != "" {
		m["description"] = f.desc
	}
	if f.def != "" {
		m["default"] = f.def
	}
	return m
}

func toAny(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

// topLevelKeys maps every root key name to the file it belongs in, so a key in
// the wrong file can name the right one (CodeWrongFile).
func topLevelKeys() map[string]string {
	out := map[string]string{}
	for _, s := range schemas {
		for _, f := range s.fields {
			if f.name == "version" {
				continue // every file carries it
			}
			out[f.name] = s.file
		}
	}
	return out
}

// names returns the field names of a table, for "did you mean".
func names(fs []field) []string {
	out := make([]string, 0, len(fs))
	for _, f := range fs {
		out = append(out, f.name)
	}
	return out
}

// suggest returns the closest candidate to key, or "" if none is close enough.
func suggest(key string, candidates []string) string {
	best, bestD := "", 0
	limit := len(key)/2 + 1
	if limit > 3 {
		limit = 3
	}
	for _, c := range candidates {
		d := levenshtein(strings.ToLower(key), strings.ToLower(c))
		if d <= limit && (best == "" || d < bestD) {
			best, bestD = c, d
		}
	}
	return best
}

// levenshtein is the plain edit distance, two rows.
func levenshtein(a, b string) int {
	ar, br := []rune(a), []rune(b)
	prev := make([]int, len(br)+1)
	cur := make([]int, len(br)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ar); i++ {
		cur[0] = i
		for j := 1; j <= len(br); j++ {
			cost := 1
			if ar[i-1] == br[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, min(cur[j-1]+1, prev[j-1]+cost))
		}
		prev, cur = cur, prev
	}
	return prev[len(br)]
}
