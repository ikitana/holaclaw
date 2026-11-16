package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"

	"holaclaw/internal/event"
	"holaclaw/internal/provider"
	"holaclaw/internal/tokens"
	"holaclaw/internal/tool"
)

// Budget constants. All of them are part of the prompt contract: they are
// documented here, reported by `holaclaw prompt-size` and asserted by tests
// (docs/context.md).
const (
	// DefaultReplyReserve is the reply reserve when the definition sets none.
	DefaultReplyReserve = 1024
	// MinTailTokens is the smallest history tail worth building a prompt for.
	// If system + tool schemas + reply reserve leave less than this, the
	// window is too small for this agent and we fail loudly instead of
	// thrashing the compactor.
	MinTailTokens = 512
	// PersonaWindowNum/Den cap the persona at a quarter of the window.
	PersonaWindowNum, PersonaWindowDen = 1, 4
	// SkillsWindowNum/Den cap the skills index at an eighth of the window.
	SkillsWindowNum, SkillsWindowDen = 1, 8
	// ImageTokens is the flat budget charged for one inline image part. The
	// estimator cannot see image bytes, so vision cost is a constant.
	ImageTokens = 800
	// CompactionPct is the share of the available history budget above which
	// we ask for compaction even though the transcript still fits.
	CompactionPct = 70
	// MinResultTokens is the least a tool result of the newest turn keeps
	// when the turn does not fit: below it a result says nothing useful.
	MinResultTokens = 128
	// MinUserTokens is the least the text of the newest user turn keeps when
	// the turn does not fit, on top of its images: a question and some of
	// what it is about.
	MinUserTokens = 2 * MinResultTokens
	// TruncMarker replaces the middle of a user turn that had to be cut to fit.
	TruncMarker = "[…truncated]"
	// VolatileHeader opens the volatile tail block appended to the last user
	// message (never to the stable system prefix).
	VolatileHeader = "--- context ---"
	// SummaryPrefix marks a transcript system message (compaction summary or
	// notice) once it is replayed as a user-role message: no provider gets a
	// second system message from us.
	SummaryPrefix = "[summary] "
)

// BudgetError is returned when a prompt cannot be built within the model's
// window. It names the offending part so `holaclaw check` can point at it.
type BudgetError struct {
	What   string // "core", "persona", "skills", "tool schema <name>", "window too small"
	Tokens int
	Cap    int
}

func (e *BudgetError) Error() string {
	return fmt.Sprintf("context budget: %s is %d tokens, cap %d", e.What, e.Tokens, e.Cap)
}

type builder struct{}

// NewBuilder returns the default context builder. It is stateless and safe
// for concurrent use.
func NewBuilder() Builder { return builder{} }

// Build assembles the prompt for one model call.
//
// The system prompt holds nothing volatile: no clock, not even a date line —
// a fragment that varies within a session would break the byte-stable prefix
// and its cache. Precise time is supplied by the caller as an Injection and
// lands in the volatile tail (docs/context.md).
func (builder) Build(ctx context.Context, in BuildInput) (Prompt, error) {
	var p Prompt
	window := in.Model.Context

	// 1. System fragments, in order.
	core := strings.TrimSpace(in.Def.Core)
	if core == "" {
		core = CorePrompt()
	}
	if n := tokens.Estimate(core); n > MaxCoreTokens {
		return p, &BudgetError{What: "core", Tokens: n, Cap: MaxCoreTokens}
	}
	texts := []string{core}
	frags := []Fragment{fragment("core", core)}
	if persona := strings.TrimSpace(in.Def.Persona); persona != "" {
		limit := window * PersonaWindowNum / PersonaWindowDen
		if n := tokens.Estimate(persona); n > limit {
			return p, &BudgetError{What: "persona", Tokens: n, Cap: limit}
		}
		texts = append(texts, persona)
		frags = append(frags, fragment("persona", persona))
	}
	if len(in.Def.Skills) > 0 {
		text := SkillsFragment(in.Def.Skills)
		limit := window * SkillsWindowNum / SkillsWindowDen
		if n := tokens.Estimate(text); n > limit {
			return p, &BudgetError{What: "skills", Tokens: n, Cap: limit}
		}
		texts = append(texts, text)
		frags = append(frags, fragment("skills", text))
	}
	sysTokens := 0
	for _, f := range frags {
		sysTokens += f.Tokens
	}

	// 2. One fragment per tool schema. Tools are described once, by schema.
	// A granted connector's schema is the server's: outside the cap, and
	// counted on its own (docs/connectors.md §5).
	schemaTokens, connTokens := 0, 0
	var toolFrags []Fragment
	byConn := map[string][]string{}
	for _, s := range in.Tools {
		n := tool.SchemaTokens(s)
		name := "tool:" + s.Name
		if id, server, ok := connectorTool(in.Def.Connectors, s.Name); ok {
			byConn[id] = append(byConn[id], server)
			name = "connector:" + s.Name
			connTokens += n
		} else if n > tool.MaxSchemaTokens {
			return p, &BudgetError{What: "tool schema " + s.Name, Tokens: n, Cap: tool.MaxSchemaTokens}
		} else {
			schemaTokens += n
		}
		toolFrags = append(toolFrags, Fragment{
			Name:   name,
			Tokens: n,
			Hash:   hashOf(s.Name, s.Description, string(s.Parameters)),
		})
	}
	if len(byConn) > 0 {
		text := connectorsFragment(byConn)
		texts = append(texts, text)
		f := fragment("connectors", text)
		frags = append(frags, f)
		connTokens += f.Tokens
	}
	frags = append(frags, toolFrags...)
	p.System = strings.Join(texts, "\n\n")
	p.Tools = in.Tools
	p.PrefixHash = prefixHash(p.System, in.Tools)

	// 3. Reply reserve: definition or default, never more than a quarter of
	// the window.
	reserve := in.Def.ReplyReserve
	if reserve <= 0 {
		reserve = DefaultReplyReserve
	}
	if q := window / 4; reserve > q {
		reserve = q
	}

	fixed := sysTokens + schemaTokens + connTokens + reserve
	if fixed+MinTailTokens >= window {
		return p, &BudgetError{What: "window too small", Tokens: fixed + MinTailTokens, Cap: window}
	}

	// 4. Volatile tail: one block, appended to the last user message.
	block, memIDs := volatileBlock(in.Injections)
	volatile := tokens.Estimate(block)
	p.MemoryIDs = memIDs
	available := window - fixed - volatile

	// 5. Convert and group the transcript, then fit it newest-first.
	items := make([]built, 0, len(in.History))
	for _, m := range in.History {
		if silentAssistant(m) {
			continue
		}
		items = append(items, convert(ctx, in, m))
	}
	kept, dropped := fit(groupMessages(pairResults(items)), available)
	kept = repairPairing(kept)
	if len(kept) != len(items) {
		dropped = true
	}

	history := 0
	for _, it := range kept {
		history += it.tokens
		p.Messages = append(p.Messages, it.msg)
		if it.id != "" {
			p.MessageIDs = append(p.MessageIDs, it.id)
		}
	}
	p.Messages = appendVolatile(p.Messages, block)

	p.Breakdown = Breakdown{
		Window:       window,
		Fragments:    frags,
		System:       sysTokens,
		ToolSchemas:  schemaTokens,
		Connectors:   connTokens,
		History:      history,
		Volatile:     volatile,
		ReplyReserve: reserve,
		Total:        sysTokens + schemaTokens + connTokens + history + volatile,
	}
	p.NeedsCompaction = dropped || history*100 > available*CompactionPct
	return p, nil
}

// built is one converted transcript message with its budgeted cost.
type built struct {
	id     event.ID
	msg    provider.Message
	tokens int
	// result is a tool result's text before wrapping, so fit can cut it and
	// wrap it again; empty for every other message.
	result  string
	tainted bool
}

// group is an atomic unit of history: either a single message, or an
// assistant message requesting tools plus every tool result that follows it.
// A tool call is never split from its result (docs/context.md).
type group struct {
	items   []built
	tokens  int
	hasUser bool
}

func groupMessages(items []built) []group {
	var gs []group
	for i := 0; i < len(items); i++ {
		g := group{items: []built{items[i]}, tokens: items[i].tokens}
		g.hasUser = items[i].msg.Role == provider.RoleUser
		if len(items[i].msg.ToolCalls) > 0 {
			for i+1 < len(items) && items[i+1].msg.Role == provider.RoleTool {
				i++
				g.items = append(g.items, items[i])
				g.tokens += items[i].tokens
			}
		}
		gs = append(gs, g)
	}
	return gs
}

// pairResults moves each tool call's results up to directly after the call.
// Group chatter stored while a tool ran sits between the two in the
// transcript; it goes after the results instead, which is also when the bot
// could have read it. Providers reject a call not followed by its results.
// The transcript is untouched; only the prompt order changes.
func pairResults(items []built) []built {
	out := make([]built, 0, len(items))
	for i := 0; i < len(items); i++ {
		out = append(out, items[i])
		want := map[string]bool{}
		for _, tc := range items[i].msg.ToolCalls {
			want[tc.ID] = true
		}
		var between []built
		j := i + 1
		for ; j < len(items) && len(want) > 0; j++ {
			it := items[j]
			if it.msg.Role == provider.RoleTool && want[it.msg.ToolCallID] {
				delete(want, it.msg.ToolCallID)
				out = append(out, it)
				continue
			}
			if it.msg.Role == provider.RoleAssistant {
				break // the turn moved on: what is still missing is an orphan
			}
			between = append(between, it)
		}
		out = append(out, between...)
		i = j - 1
	}
	return out
}

// fit keeps whole groups from newest to oldest while they fit. The newest
// user turn and everything after it is always kept; if that does not fit, the
// turn is cut by shrinkTurn rather than failing.
func fit(gs []group, available int) ([]built, bool) {
	if len(gs) == 0 {
		return nil, false
	}
	forced := -1
	for i := len(gs) - 1; i >= 0; i-- {
		if gs[i].hasUser {
			forced = i
			break
		}
	}
	if forced < 0 {
		forced = len(gs) - 1 // no user turn at all: keep at least the newest group
	}
	used, cut := 0, false
	for i := forced; i < len(gs); i++ {
		used += gs[i].tokens
	}
	if used > available {
		used -= shrinkTurn(gs[forced:], used-available)
		cut = true
	}
	first := forced
	for i := forced - 1; i >= 0; i-- {
		if used+gs[i].tokens > available {
			break
		}
		used += gs[i].tokens
		first = i
	}
	var kept []built
	for _, g := range gs[first:] {
		kept = append(kept, g.items...)
	}
	return kept, first > 0 || cut
}

func newestUser(gs []group) (int, int) {
	for i := len(gs) - 1; i >= 0; i-- {
		for j := len(gs[i].items) - 1; j >= 0; j-- {
			if gs[i].items[j].msg.Role == provider.RoleUser {
				return i, j
			}
		}
	}
	return -1, -1
}

// shrinkTurn cuts the bulk of the turn in gs, largest first, down to a common
// cap that reclaims over tokens: every tool result, never below
// MinResultTokens, and the user's text, never below MinUserTokens. A pasted
// document is data like a result, so neither starves the other. The user's
// text loses its middle: the question sits before or after a paste, and cut
// to its head alone the model can reply to a message it cannot see. Floors
// hold even when the turn still does not fit. It returns how many tokens were
// reclaimed.
func shrinkTurn(gs []group, over int) int {
	type ref struct{ g, i, floor int }
	var refs []ref
	hi := 0
	add := func(g, i, floor int) {
		if n := gs[g].items[i].tokens; n > floor {
			refs = append(refs, ref{g, i, floor})
			hi = max(hi, n)
		}
	}
	for g := range gs {
		for i, it := range gs[g].items {
			if it.result != "" {
				add(g, i, MinResultTokens)
			}
		}
	}
	if g, i := newestUser(gs); g >= 0 {
		it := gs[g].items[i]
		text, _ := splitText(it.msg.Parts)
		add(g, i, it.tokens-tokens.Estimate(text)+MinUserTokens)
	}
	target := func(r ref, c int) int {
		return min(gs[r.g].items[r.i].tokens, max(c, r.floor))
	}
	excess := func(c int) int {
		n := 0
		for _, r := range refs {
			n += gs[r.g].items[r.i].tokens - target(r, c)
		}
		return n
	}
	// The highest cap that still reclaims enough; every floor when none does.
	c := 0
	for c < hi {
		mid := (c + hi + 1) / 2
		if excess(mid) >= over {
			c = mid
		} else {
			hi = mid - 1
		}
	}
	reclaimed := 0
	for _, r := range refs {
		it := gs[r.g].items[r.i]
		limit := target(r, c)
		if limit >= it.tokens {
			continue
		}
		before := it.tokens
		if it.msg.Role == provider.RoleUser {
			text, rest := splitText(it.msg.Parts)
			text = truncateMiddle(text, limit-(before-tokens.Estimate(text)), "\n"+TruncMarker+"\n")
			it.msg.Parts = append([]provider.Part{{Text: text}}, rest...)
		} else {
			body, mark := splitMark(it.result)
			overhead := before - tokens.Estimate(body)
			text := capResult(context.Background(), body, max(limit-overhead, 0), nil) + mark
			if it.tainted {
				text = wrapUntrusted(text)
			}
			it.msg.Parts = []provider.Part{{Text: text}}
		}
		it.tokens = msgTokens(it.msg)
		gs[r.g].items[r.i] = it
		gs[r.g].tokens += it.tokens - before
		reclaimed += before - it.tokens
	}
	return reclaimed
}

// resultMarks open the line that ends a result already cut before it was
// stored: the stage-1 pointer (capResult), the connector byte cap
// (internal/connector/tools.go) and the built-in tool cap (capText in
// internal/tool/tools/tools.go).
var resultMarks = []string{"[… full result", "[result cut at ", "[truncated "}

// splitMark splits a result into its body and the mark line that ends it, the
// newline before the mark included. A cut keeps the mark: it holds the blob
// id, or says the source itself was cut, which the prompt pointer cannot.
func splitMark(s string) (string, string) {
	i := strings.LastIndexByte(s, '\n')
	if i < 0 || !strings.HasSuffix(s, "]") {
		return s, ""
	}
	for _, m := range resultMarks {
		if strings.HasPrefix(s[i+1:], m) {
			return s[:i], s[i:]
		}
	}
	return s, ""
}

func splitText(parts []provider.Part) (string, []provider.Part) {
	var text strings.Builder
	var rest []provider.Part
	for _, p := range parts {
		if p.Image != nil {
			rest = append(rest, p)
			continue
		}
		text.WriteString(p.Text)
	}
	return text.String(), rest
}

// repairPairing is the final pass over the trimmed history: it drops tool
// results whose call is gone and any leading message that is not a user turn
// (an orphan tool-call assistant among them).
func repairPairing(items []built) []built {
	seen := map[string]bool{}
	out := items[:0:0]
	for _, it := range items {
		switch it.msg.Role {
		case provider.RoleTool:
			if !seen[it.msg.ToolCallID] {
				continue
			}
		case provider.RoleAssistant:
			for _, tc := range it.msg.ToolCalls {
				seen[tc.ID] = true
			}
		}
		out = append(out, it)
	}
	for len(out) > 0 && out[0].msg.Role != provider.RoleUser {
		out = out[1:]
	}
	return out
}

// silentAssistant is the stored answer of a turn that only reacted: nothing
// to replay, and an empty assistant content is a 400 on some upstreams
// (Anthropic and Gemini through OpenRouter).
func silentAssistant(m event.Message) bool {
	return m.Role == event.RoleAssistant && strings.TrimSpace(m.Text) == "" &&
		len(m.ToolCalls) == 0 && len(m.Attachments) == 0
}

// convert turns one transcript message into a provider message. Transcript
// system messages (compaction summaries and notices) are replayed as user
// messages prefixed with "[summary] ".
func convert(ctx context.Context, in BuildInput, m event.Message) built {
	var lines []string
	var images []provider.Part

	text := m.Text
	// The model's own answer is not wrapped even when it drew on untrusted
	// content: replayed with the tags, the model learns to emit them.
	if m.Tainted && m.Role != event.RoleAssistant {
		text = wrapUntrusted(text)
	}
	if m.Role == event.RoleSystem {
		text = SummaryPrefix + text
	}
	if m.Sender != "" {
		text = "[" + SanitizeSender(m.Sender) + "] " + text
	}
	if text != "" {
		lines = append(lines, text)
	}
	for _, a := range m.Attachments {
		lines = append(lines, attachmentStub(a))
		switch a.Kind {
		case event.KindImage:
			if in.Model.Vision && in.Images != nil {
				b, err := in.Images(ctx, a)
				if err == nil {
					images = append(images, provider.Part{Image: &provider.Image{MIME: a.MIME, Data: b}})
					continue
				}
				lines = append(lines, "(image unavailable)")
			}
			if a.Derived != nil && a.Derived.Text != "" {
				lines = append(lines, a.Derived.Text)
			}
		case event.KindAudio:
			if a.Derived != nil && a.Derived.Text != "" {
				lines = append(lines, a.Derived.Text)
			}
		case event.KindVideo, event.KindDocument:
			// Stub only; inlining documents is the caller's job.
		}
	}

	msg := provider.Message{Role: providerRole(m.Role)}
	if s := strings.Join(lines, "\n"); s != "" || len(images) == 0 {
		msg.Parts = append(msg.Parts, provider.Part{Text: s})
	}
	msg.Parts = append(msg.Parts, images...)
	for _, tc := range m.ToolCalls {
		msg.ToolCalls = append(msg.ToolCalls, provider.ToolCall{ID: tc.ID, Name: tc.Name, Arguments: tc.Arguments})
	}
	b := built{id: m.ID, msg: msg}
	if m.Role == event.RoleTool {
		b.msg.ToolCallID, b.msg.ToolName = m.ToolCallID, m.ToolName
		if len(m.Attachments) == 0 && m.Sender == "" {
			b.result, b.tainted = m.Text, m.Tainted
		}
	}
	b.tokens = msgTokens(b.msg)
	return b
}

func providerRole(r event.Role) provider.Role {
	switch r {
	case event.RoleAssistant:
		return provider.RoleAssistant
	case event.RoleTool:
		return provider.RoleTool
	default: // user and system-as-summary
		return provider.RoleUser
	}
}

// attachmentStub is the one line every attachment yields towards the model.
func attachmentStub(a event.Attachment) string {
	return fmt.Sprintf("[attachment: %s (%s, %s, %s) id=%s]",
		sanitizeLabel(a.Name, "file"), a.Kind, sanitizeLabel(a.MIME, "application/octet-stream"),
		humanSize(a.Size), a.ID)
}

func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit && exp < 2; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMG"[exp])
}

func msgTokens(m provider.Message) int {
	n := tokens.PerMessageOverhead
	for _, p := range m.Parts {
		if p.Image != nil {
			n += ImageTokens
			continue
		}
		n += tokens.Estimate(p.Text)
	}
	for _, tc := range m.ToolCalls {
		n += tokens.Estimate(tc.Name) + tokens.Estimate(tc.Arguments)
	}
	return n + tokens.Estimate(m.ToolName)
}

// volatileBlock renders the injections into the single block that is appended
// to the last user message, and collects the memory ids for context.built.
func volatileBlock(in []Injection) (string, []event.ID) {
	var lines []string
	var ids []event.ID
	for _, inj := range in {
		if inj.ID != "" {
			ids = append(ids, inj.ID)
		}
		if inj.Text != "" {
			lines = append(lines, inj.Text)
		}
	}
	if len(lines) == 0 {
		return "", ids
	}
	return VolatileHeader + "\n" + strings.Join(lines, "\n"), ids
}

// appendVolatile puts the block in the text of the last user message. With no
// user message left it becomes one, so the tail is never lost.
func appendVolatile(msgs []provider.Message, block string) []provider.Message {
	if block == "" {
		return msgs
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role != provider.RoleUser {
			continue
		}
		for j := range msgs[i].Parts {
			if msgs[i].Parts[j].Image == nil {
				msgs[i].Parts[j].Text += "\n\n" + block
				return msgs
			}
		}
		msgs[i].Parts = append([]provider.Part{{Text: block}}, msgs[i].Parts...)
		return msgs
	}
	return append(msgs, provider.TextMessage(provider.RoleUser, block))
}

// connectorsHeading opens the `connectors` fragment.
const connectorsHeading = "Connectors (remote services; their tools are named <connector>.<tool>):"

// connectorToolName is a tool name the connector package lets follow
// "<id>."; anything else under a granted id is not a connector tool.
var connectorToolName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// connectorTool splits "<id>.<tool>" when id is one of the granted ids and
// tool a name a connector can register.
func connectorTool(granted []string, name string) (id, server string, ok bool) {
	id, server, ok = strings.Cut(name, ".")
	if !ok || !connectorToolName.MatchString(server) || !slices.Contains(granted, id) {
		return "", "", false
	}
	return id, server, true
}

// connectorsFragment renders the `connectors` system fragment: the heading
// and one "- id: tool, tool" line per connector, ids and names sorted, so it
// changes only when the tools the model is given do. Names only: the
// descriptions are in the schemas.
func connectorsFragment(byConn map[string][]string) string {
	ids := make([]string, 0, len(byConn))
	for id := range byConn {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	var b strings.Builder
	b.WriteString(connectorsHeading)
	for _, id := range ids {
		names := slices.Clone(byConn[id])
		slices.Sort(names)
		b.WriteString("\n- " + id + ": " + strings.Join(slices.Compact(names), ", "))
	}
	return b.String()
}

func fragment(name, text string) Fragment {
	return Fragment{Name: name, Tokens: tokens.Estimate(text), Hash: hashOf(text)}
}

func hashOf(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		_, _ = io.WriteString(h, p)
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// prefixHash hashes the byte-stable prefix: the system text plus every tool
// spec in order. History and injections never enter it.
func prefixHash(system string, specs []provider.ToolSpec) string {
	parts := make([]string, 0, 1+3*len(specs))
	parts = append(parts, system)
	for _, s := range specs {
		parts = append(parts, s.Name, s.Description, string(s.Parameters))
	}
	return hashOf(parts...)
}

// BuiltAttrs returns the attrs of the context.built event: everything needed
// to reconstruct the prompt (docs/traceability.md).
func BuiltAttrs(p Prompt) map[string]any {
	msgIDs := make([]string, 0, len(p.MessageIDs))
	for _, id := range p.MessageIDs {
		msgIDs = append(msgIDs, string(id))
	}
	memIDs := make([]string, 0, len(p.MemoryIDs))
	for _, id := range p.MemoryIDs {
		memIDs = append(memIDs, string(id))
	}
	frags := make([]map[string]any, 0, len(p.Breakdown.Fragments))
	for _, f := range p.Breakdown.Fragments {
		frags = append(frags, map[string]any{"name": f.Name, "tokens": f.Tokens, "hash": f.Hash})
	}
	b := p.Breakdown
	return map[string]any{
		"message_ids":      msgIDs,
		"memory_ids":       memIDs,
		"fragments":        frags,
		"prefix_hash":      p.PrefixHash,
		"needs_compaction": p.NeedsCompaction,
		"window":           b.Window,
		"system":           b.System,
		"tool_schemas":     b.ToolSchemas,
		"connectors":       b.Connectors,
		"history":          b.History,
		"volatile":         b.Volatile,
		"reply_reserve":    b.ReplyReserve,
		"total":            b.Total,
	}
}

// FormatBreakdown prints the aligned table `holaclaw prompt-size` shows: the
// same numbers context.built records.
func FormatBreakdown(w io.Writer, b Breakdown) {
	width := len("fixed overhead")
	for _, f := range b.Fragments {
		if len(f.Name) > width {
			width = len(f.Name)
		}
	}
	row := func(name string, n int, pct bool) {
		if !pct {
			_, _ = fmt.Fprintf(w, "%-*s  %7d\n", width, name, n)
			return
		}
		_, _ = fmt.Fprintf(w, "%-*s  %7d  %7s\n", width, name, n, percent(n, b.Window))
	}
	_, _ = fmt.Fprintf(w, "%-*s  %7s  %7s\n", width, "fragment", "tokens", "% window")
	for _, f := range b.Fragments {
		row(f.Name, f.Tokens, true)
	}
	_, _ = fmt.Fprintf(w, "%s\n", strings.Repeat("-", width+20))
	row("system", b.System, true)
	row("tool schemas", b.ToolSchemas, true)
	row("connectors", b.Connectors, true)
	row("fixed overhead", b.System+b.ToolSchemas, true)
	row("history", b.History, true)
	row("volatile", b.Volatile, true)
	row("reply reserve", b.ReplyReserve, true)
	row("total", b.Total, true)
	row("window", b.Window, false)
}

func percent(n, window int) string {
	if window <= 0 {
		return "-"
	}
	return fmt.Sprintf("%.1f%%", float64(n)*100/float64(window))
}
