package page

import (
	"slices"
	"strconv"
	"strings"
	"testing"

	"holaclaw/internal/browser/cdp"
	"holaclaw/internal/browser/proto"
)

// axNode is the shape the fake answers Accessibility.getFullAXTree with.
func axNode(role, name string, backend int64, props map[string]bool) map[string]any {
	n := map[string]any{
		"nodeId":           name,
		"ignored":          false,
		"role":             map[string]any{"type": "role", "value": role},
		"name":             map[string]any{"type": "computedString", "value": name},
		"backendDOMNodeId": backend,
	}
	var properties []map[string]any
	for k, v := range props {
		properties = append(properties, map[string]any{
			"name": k, "value": map[string]any{"type": "booleanOrUndefined", "value": v},
		})
	}
	if properties != nil {
		n["properties"] = properties
	}
	return n
}

// snapshotOf builds a one-document snapshot with a box per backend node id.
func snapshotOf(boxes map[int64][]float64, text string) map[string]any {
	strs := []string{"", "DIV", text}
	var parents []int
	var names []int
	var backend []int64
	var layoutNodes []int
	var bounds [][]float64
	var layoutText []int
	i := 0
	// Sorted by backend node id, so the document order the snapshot implies
	// is stable from run to run.
	ids := make([]int64, 0, len(boxes))
	for id := range boxes {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		b := boxes[id]
		parents = append(parents, -1)
		names = append(names, 1)
		backend = append(backend, id)
		layoutNodes = append(layoutNodes, i)
		bounds = append(bounds, b)
		layoutText = append(layoutText, 2)
		i++
	}
	return map[string]any{
		"strings": strs,
		"documents": []map[string]any{{
			"documentURL": 0,
			"title":       0,
			"baseURL":     0,
			"nodes": map[string]any{
				"parentIndex":   parents,
				"nodeType":      make([]int, len(parents)),
				"nodeName":      names,
				"nodeValue":     names,
				"backendNodeId": backend,
			},
			"layout": map[string]any{
				"nodeIndex": layoutNodes,
				"bounds":    bounds,
				"text":      layoutText,
			},
		}},
	}
}

// collect is the join: roles and names from the accessibility tree, boxes and
// visibility from the snapshot, and nothing from a script.
func TestCollectJoinsTreeAndSnapshot(t *testing.T) {
	f := newFakeCDP(t)
	e := testEngineNoBrowser(t)
	s := &session{e: e, agent: "alpha"}
	at := &attachment{s: s, conn: f.conn, bs: f.conn.Session(""), tabs: map[string]*tab{}, refs: map[string]elemRef{}}
	s.at = at

	f.result("Accessibility.getFullAXTree", map[string]any{"nodes": []map[string]any{
		axNode("heading", "Booking", 1, nil),
		axNode("button", "Search flights", 2, nil),
		axNode("textbox", "Departure airport", 3, map[string]bool{"focused": true}),
		axNode("button", "Far away", 4, nil),
		axNode("link", "Hidden link", 5, nil), // no box: not rendered
		axNode("button", "Disabled", 6, map[string]bool{"disabled": true}),
	}})
	f.result("DOMSnapshot.captureSnapshot", snapshotOf(map[int64][]float64{
		1: {0, 0, 300, 40},
		2: {10, 100, 120, 30},
		3: {10, 200, 200, 30},
		4: {10, 3000, 120, 30}, // below the viewport
		6: {10, 260, 120, 30},
	}, "Booking"))
	f.result("Page.getLayoutMetrics", map[string]any{
		"layoutViewport": map[string]any{"pageX": 0, "pageY": 0, "clientWidth": 1280, "clientHeight": 800},
	})

	got, txt, _, _, err := at.collect(t.Context(), "S1", textBudget(1<<10), 0)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("collected %d candidates, want 4 (heading is not interactive, the box-less link is not rendered)", len(got))
	}
	if got[0].c.Role != "button" || got[0].c.Name != "Search flights" {
		t.Fatalf("first candidate = %+v", got[0].c)
	}
	if !got[1].c.Focused || got[1].c.Role != "textbox" {
		t.Fatalf("textbox = %+v", got[1].c)
	}
	if !got[2].c.Offscreen {
		t.Error("a box 3000px down is not marked offscreen")
	}
	if !got[3].c.Disabled {
		t.Error("the disabled button is not marked disabled")
	}
	if got[0].ref.backend != 2 || got[0].ref.session != "S1" {
		t.Fatalf("ref = %+v", got[0].ref)
	}
	if !strings.Contains(txt.text, "Booking") {
		t.Fatalf("text = %q", txt.text)
	}
	// No domain was enabled for the observation, and nothing was evaluated.
	for _, m := range f.methods() {
		switch m {
		case "Accessibility.enable", "DOM.enable", "Runtime.evaluate", "Page.createIsolatedWorld":
			t.Errorf("observation called %s", m)
		}
	}
}

func TestShortlistOrderAndCap(t *testing.T) {
	all := []candidate{
		{c: proto.Candidate{Ref: "r1", Role: "link", Name: "Home"}, order: 0},
		{c: proto.Candidate{Ref: "r2", Role: "button", Name: "Search flights", Offscreen: true}, order: 1},
		{c: proto.Candidate{Ref: "r3", Role: "textbox", Name: "Departure airport"}, order: 2},
		{c: proto.Candidate{Ref: "r4", Role: "button", Name: "Cancel"}, order: 3},
	}
	got := shortlist(all, []string{"departure"}, 0)
	if len(got) != 4 || got[0].Ref != "r3" {
		t.Fatalf("hinted shortlist = %+v, want the departure field first", got)
	}
	// Without hints: visible before offscreen, then form controls before
	// buttons before links, then document order.
	got = shortlist(all, nil, 0)
	if got[0].Ref != "r3" || got[1].Ref != "r4" || got[2].Ref != "r1" || got[3].Ref != "r2" {
		t.Fatalf("unhinted shortlist = %+v", got)
	}
	// A fuzzy prefix still matches.
	got = shortlist(all, []string{"depart"}, 0)
	if got[0].Ref != "r3" {
		t.Fatalf("prefix hint = %+v", got)
	}
	if got := shortlist(all, nil, 2); len(got) != 2 {
		t.Fatalf("cap 2 gave %d", len(got))
	}
	// The request cannot ask for more than a decision model takes.
	many := make([]candidate, proto.MaxCandidates+20)
	for i := range many {
		many[i] = candidate{c: proto.Candidate{Ref: "r", Role: "button"}, order: i}
	}
	if got := shortlist(many, nil, proto.MaxCandidates+20); len(got) != proto.MaxCandidates {
		t.Fatalf("cap = %d, want %d", len(got), proto.MaxCandidates)
	}
}

func TestCapNameAndText(t *testing.T) {
	long := strings.Repeat("é", proto.MaxNameBytes)
	got := capName(long)
	if len(got) > proto.MaxNameBytes {
		t.Fatalf("name is %d bytes, want at most %d", len(got), proto.MaxNameBytes)
	}
	if !utf8Valid(got) {
		t.Fatal("the cap split a rune")
	}
	if capName("  spaced   out \n") != "spaced out" {
		t.Fatalf("capName = %q", capName("  spaced   out \n"))
	}

	text, truncated := capText("hello world", 5)
	if text != "hello" || !truncated {
		t.Fatalf("capText = %q, %v", text, truncated)
	}
	if _, truncated := capText("short", 100); truncated {
		t.Error("short text reported truncated")
	}
	// Never more than the protocol cap, whatever the request asks for.
	big := strings.Repeat("x", proto.MaxTextBytes+100)
	text, truncated = capText(big, proto.MaxTextBytes+100)
	if len(text) != proto.MaxTextBytes || !truncated {
		t.Fatalf("capText gave %d bytes, truncated=%v", len(text), truncated)
	}
}

// The page's text is bounded while it is gathered, not after: a page of any
// size costs the cap. The boxes are still indexed in full — they are what the
// candidates are joined against.
func TestReadSnapshotStopsAtTheTextBudget(t *testing.T) {
	const chunk = "some visible words on the page"
	const nodes = 2000

	snap := &cdp.Snapshot{Strings: []string{"", "DIV", chunk}}
	doc := cdp.DocumentSnapshot{}
	for i := range nodes {
		doc.Nodes.ParentIndex = append(doc.Nodes.ParentIndex, -1)
		doc.Nodes.NodeName = append(doc.Nodes.NodeName, 1)
		doc.Nodes.BackendNodeID = append(doc.Nodes.BackendNodeID, int64(i+1))
		doc.Layout.NodeIndex = append(doc.Layout.NodeIndex, i)
		doc.Layout.Bounds = append(doc.Layout.Bounds, []float64{0, float64(i * 20), 100, 18})
		doc.Layout.Text = append(doc.Layout.Text, 2)
	}
	snap.Documents = []cdp.DocumentSnapshot{doc}
	whole := nodes * (len(chunk) + 1)

	// No text asked for: none gathered, boxes all the same.
	boxes, _, txt := readSnapshot(snap, 0, 0)
	if txt.text != "" {
		t.Fatalf("text without a budget = %d bytes", len(txt.text))
	}
	if len(boxes) != nodes {
		t.Fatalf("boxes = %d, want %d", len(boxes), nodes)
	}

	budget := textBudget(1 << 10)
	boxes, _, txt = readSnapshot(snap, budget, 0)
	if len(boxes) != nodes {
		t.Fatalf("boxes with a budget = %d, want %d", len(boxes), nodes)
	}
	// One chunk may straddle the budget; the whole page never lands here.
	if max := budget + len(chunk) + 1; len(txt.text) > max {
		t.Fatalf("text = %d bytes, want at most %d (the page is %d)", len(txt.text), max, whole)
	}
	// Long enough that capText still reports the truncation honestly.
	if len(txt.text) <= 1<<10 {
		t.Fatalf("text = %d bytes, want more than the cap so Truncated is set", len(txt.text))
	}
	if _, truncated := capText(txt.text, 1<<10); !truncated {
		t.Fatal("a page cut short was not reported truncated")
	}
}

// An iframe's boxes are relative to its own document; they are placed in the
// page's coordinates so "offscreen" means the same thing everywhere.
func TestDocumentOffsets(t *testing.T) {
	snap := &cdp.Snapshot{
		Strings: []string{"", "IFRAME"},
		Documents: []cdp.DocumentSnapshot{
			{
				Nodes: cdp.NodeTreeSnapshot{
					ParentIndex:          []int{-1},
					NodeName:             []cdp.StrIdx{1},
					BackendNodeID:        []int64{10},
					ContentDocumentIndex: cdp.RareIntegerData{Index: []int{0}, Value: []int{1}},
				},
				Layout: cdp.LayoutTreeSnapshot{
					NodeIndex: []int{0},
					Bounds:    [][]float64{{50, 400, 300, 120}},
					Text:      []cdp.StrIdx{0},
				},
			},
			{
				Nodes: cdp.NodeTreeSnapshot{
					ParentIndex:   []int{-1},
					NodeName:      []cdp.StrIdx{1},
					BackendNodeID: []int64{20},
				},
				Layout: cdp.LayoutTreeSnapshot{
					NodeIndex: []int{0},
					Bounds:    [][]float64{{5, 5, 100, 20}},
					Text:      []cdp.StrIdx{0},
				},
			},
		},
	}
	boxes, _, _ := readSnapshot(snap, 0, 0)
	got, ok := boxes[20]
	if !ok {
		t.Fatal("the iframe's button has no box")
	}
	if got.x != 55 || got.y != 405 {
		t.Fatalf("frame child box = %+v, want it offset by the frame", got)
	}
}

func utf8Valid(s string) bool {
	for _, r := range s {
		if r == 0xFFFD {
			return false
		}
	}
	return true
}

// The two sources are merged on the backend node id: the tree keeps what it
// knows, the DOM supplies what it never reported, an element named by neither
// is called what a person reads on it, and nothing is listed twice.
func TestCollectMergesDOMWithTheTree(t *testing.T) {
	f := newFakeCDP(t)
	e := testEngineNoBrowser(t)
	s := &session{e: e, agent: "alpha"}
	at := &attachment{s: s, conn: f.conn, bs: f.conn.Session(""), tabs: map[string]*tab{}, refs: map[string]elemRef{}}
	s.at = at

	b := newDOM()
	root := b.el(-1, "html")
	body := b.el(root, "body")
	search := b.el(body, "input", "type", "search", "placeholder", "Search flights")
	editor := b.el(body, "div", "contenteditable", "true", "aria-label", "Notes")
	button := b.el(body, "button")
	b.text(button, "Book it")
	snap, _ := b.build()

	// The tree reports the search box with no name at all and the button by
	// name; it never mentions the editor.
	f.result("Accessibility.getFullAXTree", map[string]any{"nodes": []map[string]any{
		axNode("combobox", "", backendOf(search), nil),
		axNode("button", "Book it", backendOf(button), nil),
	}})
	f.result("DOMSnapshot.captureSnapshot", snap)
	f.result("Page.getLayoutMetrics", map[string]any{
		"layoutViewport": map[string]any{"pageX": 0, "pageY": 0, "clientWidth": 1280, "clientHeight": 800},
	})

	got, _, _, _, err := at.collect(t.Context(), "S1", 0, 0)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("collected %d candidates, want 3: %+v", len(got), got)
	}
	// Document order, over both sources.
	if got[0].ref.backend != backendOf(search) || got[1].ref.backend != backendOf(editor) ||
		got[2].ref.backend != backendOf(button) {
		t.Fatalf("candidates are not in document order: %+v", got)
	}
	// The tree's role is kept; the name comes from the placeholder.
	if got[0].c.Role != "combobox" || got[0].c.Name != "Search flights" {
		t.Fatalf("search box = %+v", got[0].c)
	}
	if got[1].c.Role != "textbox" || got[1].c.Name != "Notes" {
		t.Fatalf("editor = %+v", got[1].c)
	}
	if got[2].c.Role != "button" || got[2].c.Name != "Book it" {
		t.Fatalf("button = %+v", got[2].c)
	}
	for _, m := range f.methods() {
		switch m {
		case "Accessibility.enable", "DOM.enable", "Runtime.evaluate", "Page.createIsolatedWorld":
			t.Errorf("observation called %s", m)
		}
	}
}

// axProp adds one property of any shape to a node built by axNode: the tree
// publishes strings ("hasPopup", "invalid", "autocomplete") as well as the
// booleans axNode already writes.
func axProp(n map[string]any, name string, value any) map[string]any {
	props, _ := n["properties"].([]map[string]any)
	n["properties"] = append(props, map[string]any{
		"name": name, "value": map[string]any{"type": "token", "value": value},
	})
	return n
}

// axRelated adds a relation property: what the tree says an element's error
// message or description points at, as Chromium sends it.
func axRelated(n map[string]any, name string, backend int64, idref string) map[string]any {
	props, _ := n["properties"].([]map[string]any)
	n["properties"] = append(props, map[string]any{
		"name": name,
		"value": map[string]any{"type": "idrefList", "relatedNodes": []map[string]any{
			{"backendDOMNodeId": backend, "idref": idref},
		}},
	})
	return n
}

// collectOf runs one collection against a fake browser: the tree and the
// snapshot as given, and nothing else.
func collectOf(t *testing.T, nodes []map[string]any, snap any, budget int, anchor int64) ([]candidate, snapText, []string) {
	t.Helper()
	f := newFakeCDP(t)
	e := testEngineNoBrowser(t)
	s := &session{e: e, agent: "alpha"}
	at := &attachment{s: s, conn: f.conn, bs: f.conn.Session(""), tabs: map[string]*tab{}, refs: map[string]elemRef{}}
	s.at = at

	f.result("Accessibility.getFullAXTree", map[string]any{"nodes": nodes})
	f.result("DOMSnapshot.captureSnapshot", snap)
	f.result("Page.getLayoutMetrics", map[string]any{
		"layoutViewport": map[string]any{"pageX": 0, "pageY": 0, "clientWidth": 1280, "clientHeight": 800},
	})
	got, txt, said, _, err := at.collect(t.Context(), "S1", budget, anchor)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	// What one collection would be announced as, with nothing remembered of a
	// previous observation: the order, the dedupe and the cap.
	texts, _ := announce(said, nil)
	return got, txt, texts
}

// byName indexes candidates by accessible name.
func byName(all []candidate) map[string]proto.Candidate {
	out := make(map[string]proto.Candidate, len(all))
	for _, c := range all {
		out[c.c.Name] = c.c
	}
	return out
}

// expanded reads an Expanded pointer for a message: "unset" is a state of its
// own and is not the same answer as "closed".
func expanded(c proto.Candidate) string {
	if c.Expanded == nil {
		return "unset"
	}
	if *c.Expanded {
		return "true"
	}
	return "false"
}

// The states the accessibility tree publishes: what an element opens, whether
// it is open, and what the page has made of what it holds.
func TestCollectReadsTheTreesStates(t *testing.T) {
	nodes := []map[string]any{
		axProp(axProp(axNode("combobox", "Where to", 1,
			map[string]bool{"expanded": true, "required": true}),
			"hasPopup", "listbox"), "invalid", "spelling"),
		// A box that completes as it is typed in is one that opens a list,
		// even when the page never said hasPopup.
		axProp(axNode("textbox", "Where from", 2, nil), "autocomplete", "list"),
		axProp(axNode("button", "Search", 3, nil), "hasPopup", "false"),
		axProp(axNode("textbox", "Nickname", 4, nil), "invalid", "false"),
	}
	got, _, _ := collectOf(t, nodes, snapshotOf(map[int64][]float64{
		1: {10, 10, 200, 30},
		2: {10, 50, 200, 30},
		3: {10, 90, 120, 30},
		4: {10, 130, 200, 30},
	}, "Booking"), 0, 0)

	all := byName(got)
	to := all["Where to"]
	if expanded(to) != "true" || to.HasPopup != "listbox" || !to.Invalid || !to.Required {
		t.Fatalf("combobox = %+v (expanded %s)", to, expanded(to))
	}
	if from := all["Where from"]; from.HasPopup != "listbox" {
		t.Fatalf("aria-autocomplete=list gave HasPopup %q", from.HasPopup)
	}
	search := all["Search"]
	if expanded(search) != "unset" || search.HasPopup != "" {
		t.Fatalf("a plain button = %+v (expanded %s)", search, expanded(search))
	}
	if all["Nickname"].Invalid {
		t.Error(`invalid="false" was read as a rejection`)
	}
}

// The tree first, the DOM for what it left unsaid — and a secret field is
// reported empty however loudly the page offers its contents.
func TestCollectMergesStatesAndKeepsSecrets(t *testing.T) {
	b := newDOM()
	root := b.el(-1, "html")
	body := b.el(root, "body")
	// The DOM disagrees with the tree about both states and adds one of its own.
	combo := b.el(body, "input", "type", "text", "aria-label", "Where to",
		"aria-expanded", "false", "aria-haspopup", "dialog", "required", "")
	pass := b.el(body, "input", "type", "password", "aria-label", "Password")
	b.inputValue(pass, "hunter2")
	card := b.el(body, "input", "type", "text", "aria-label", "Card number",
		"autocomplete", "section-pay billing cc-number", "value", "4111111111111111")
	town := b.el(body, "input", "type", "text", "aria-label", "Town")
	b.inputValue(town, "Madrid")
	snap, _ := b.build()

	nodes := []map[string]any{
		axProp(axNode("combobox", "Where to", backendOf(combo), map[string]bool{"expanded": true}), "hasPopup", "menu"),
		axNode("textbox", "Password", backendOf(pass), nil),
		axNode("textbox", "Card number", backendOf(card), nil),
		axNode("textbox", "Town", backendOf(town), nil),
	}
	got, _, _ := collectOf(t, nodes, snap, 0, 0)
	all := byName(got)

	to := all["Where to"]
	if expanded(to) != "true" || to.HasPopup != "menu" {
		t.Fatalf("the DOM overruled the tree: %+v (expanded %s)", to, expanded(to))
	}
	if !to.Required {
		t.Error("the required attribute the tree never mentioned was lost")
	}
	if p := all["Password"]; !p.Secret || p.Value != "" {
		t.Fatalf("password = %+v, want secret and empty", p)
	}
	if c := all["Card number"]; !c.Secret || c.Value != "" {
		t.Fatalf("card number = %+v, want secret and empty", c)
	}
	if town := all["Town"]; town.Secret || town.Value != "Madrid" {
		t.Fatalf("an ordinary field = %+v", town)
	}
}

// What the page is telling the user right now: its own announcements, and the
// message every field it has rejected points at.
func TestCollectReportsWhatThePageSays(t *testing.T) {
	b := newDOM()
	root := b.el(-1, "html")
	body := b.el(root, "body")
	shout := b.el(body, "div", "role", "alert")
	b.text(shout, "Your card was declined")
	status := b.el(body, "p", "role", "status")
	b.text(status, "Saved")
	live := b.el(body, "div", "aria-live", "polite")
	b.text(live, "3 results")
	off := b.el(body, "div", "aria-live", "off")
	b.text(off, "not an announcement")
	hidden := b.el(body, "div", "role", "alert")
	b.text(hidden, "a message the page is not showing")
	b.unbox(hidden)
	again := b.el(body, "div", "role", "status")
	b.text(again, "Saved") // the same thing twice is said once
	// A field the page has rejected, and the message it points at by
	// attribute; another whose relation only the tree knows.
	b.el(body, "input", "type", "email", "aria-label", "Email",
		"aria-invalid", "true", "aria-errormessage", "err")
	msg := b.el(body, "div", "id", "err")
	b.text(msg, "Enter a valid email")
	phone := b.el(body, "input", "type", "text", "aria-label", "Phone")
	note := b.el(body, "div", "id", "phone-err")
	b.text(note, "We need a phone number")
	fine := b.el(body, "input", "type", "text", "aria-label", "Town")
	quiet := b.el(body, "div", "id", "town-help")
	b.text(quiet, "Where you live")
	snap, _ := b.build()

	nodes := []map[string]any{
		axRelated(axProp(axNode("textbox", "Phone", backendOf(phone), nil), "invalid", "true"),
			"errormessage", backendOf(note), "phone-err"),
		// A valid field's description is not an announcement.
		axRelated(axNode("textbox", "Town", backendOf(fine), nil), "describedby", backendOf(quiet), "town-help"),
	}
	_, _, said := collectOf(t, nodes, snap, 0, 0)

	want := []string{"Your card was declined", "Saved", "3 results",
		"Enter a valid email", "We need a phone number"}
	if !slices.Equal(said, want) {
		t.Fatalf("alerts = %q, want %q", said, want)
	}
}

// A page that never stops talking still costs MaxAlerts.
func TestCollectCapsTheAlerts(t *testing.T) {
	b := newDOM()
	root := b.el(-1, "html")
	body := b.el(root, "body")
	for i := range proto.MaxAlerts + 3 {
		n := b.el(body, "div", "role", "alert")
		b.text(n, "message "+strconv.Itoa(i))
	}
	snap, _ := b.build()

	_, _, said := collectOf(t, nil, snap, 0, 0)
	if len(said) != proto.MaxAlerts {
		t.Fatalf("%d alerts, want %d: %q", len(said), proto.MaxAlerts, said)
	}
	if said[0] != "message 0" {
		t.Fatalf("alerts are not in document order: %q", said)
	}
}

// The anchor of a text window: where in the page text the element the
// executor named begins.
func TestReadSnapshotAnchorsTheText(t *testing.T) {
	_, _, got := readSnapshot(productSnapshot(), textBudget(1<<10), 7)
	if got.anchor < 0 {
		t.Fatalf("the anchor was not found in %q", got.text)
	}
	if !strings.HasPrefix(got.text[got.anchor:], "1.219,00 €") {
		t.Fatalf("the anchor points at %q", got.text[got.anchor:])
	}
	// A ref the snapshot knows nothing about: the text is the page from the
	// top, exactly as it is without one.
	_, _, none := readSnapshot(productSnapshot(), textBudget(1<<10), 9999)
	if none.anchor != -1 {
		t.Fatalf("an unknown anchor resolved to %d", none.anchor)
	}
	if none.text != got.text {
		t.Fatal("an unknown anchor changed the text")
	}
}

// An element with no text run of its own — a wrapper around the paragraph, an
// empty input — is anchored on the nearest run after it.
func TestReadSnapshotAnchorFallsForward(t *testing.T) {
	snap := &cdp.Snapshot{Strings: []string{"", "DIV", "before", "inside"}}
	doc := cdp.DocumentSnapshot{
		Nodes: cdp.NodeTreeSnapshot{
			ParentIndex:   []int{-1, -1, 1},
			NodeName:      []cdp.StrIdx{1, 1, 1},
			BackendNodeID: []int64{1, 2, 3},
		},
		Layout: cdp.LayoutTreeSnapshot{
			NodeIndex: []int{0, 2},
			Bounds:    [][]float64{{0, 0, 100, 20}, {0, 40, 100, 20}},
			Text:      []cdp.StrIdx{2, 3},
		},
	}
	snap.Documents = []cdp.DocumentSnapshot{doc}

	_, _, got := readSnapshot(snap, textBudget(1<<10), 2)
	if got.anchor < 0 || !strings.HasPrefix(got.text[got.anchor:], "inside") {
		t.Fatalf("anchor %d in %q", got.anchor, got.text)
	}
}

// The window is cut around the anchor rather than from the top, and says so
// whenever anything was left out on either side.
func TestWindowText(t *testing.T) {
	s := strings.Repeat("a", 500) + "NEEDLE" + strings.Repeat("b", 500)
	got, truncated := windowText(s, 500, 100)
	if !truncated {
		t.Error("a cut window was not reported truncated")
	}
	if len(got) != 100 {
		t.Fatalf("window is %d bytes, want 100", len(got))
	}
	if !strings.Contains(got, "NEEDLE") {
		t.Fatalf("window = %q, want the anchor in it", got)
	}
	// Centred: about as much before the anchor as after it.
	if before := strings.Index(got, "NEEDLE"); before < 40 || before > 50 {
		t.Fatalf("the anchor sits at %d of %d, want it centred", before, len(got))
	}
	// An anchor near an end spends the whole budget on text that exists.
	got, _ = windowText(s, len(s)-3, 100)
	if len(got) != 100 || !strings.HasSuffix(got, "b") {
		t.Fatalf("window at the end = %d bytes ending %q", len(got), got[len(got)-1:])
	}
	// Whole text shorter than the window: nothing is cut and nothing is claimed.
	if got, truncated := windowText("short page", 4, 100); got != "short page" || truncated {
		t.Fatalf("windowText on a short page = %q, %v", got, truncated)
	}
	// The cut lands on a rune boundary.
	runes := strings.Repeat("é", 200)
	got, _ = windowText(runes, 100, 51)
	if !utf8Valid(got) {
		t.Fatal("the window split a rune")
	}
}

// axDesc gives a node the description the tree computed for it: what the page
// wrote about how the element works, which Chromium resolves for us.
func axDesc(n map[string]any, text string) map[string]any {
	n["description"] = map[string]any{"type": "computedString", "value": text}
	return n
}

// A readonly field is a field whose text cannot be written: Chromium calls
// such an input editable — the kind of control is — so the state the page
// wrote vetoes it, whichever source said it. It is still a candidate: it is
// what is clicked to open the calendar.
func TestReadonlyVetoesEditable(t *testing.T) {
	b := newDOM()
	root := b.el(-1, "html")
	body := b.el(root, "body")
	b.el(body, "input", "type", "text", "aria-label", "Fecha vuelta", "readonly", "")
	b.el(body, "div", "role", "textbox", "contenteditable", "true", "aria-readonly", "true",
		"aria-label", "Locked note")
	fromAX := b.el(body, "input", "type", "text", "aria-label", "Fecha ida")
	b.el(body, "input", "type", "text", "aria-label", "Passengers")
	snap, _ := b.build()

	// The tree's own state, on an input the DOM says nothing special about.
	obs := observeOf(t, []map[string]any{
		axProp(axProp(axNode("textbox", "Fecha ida", backendOf(fromAX), nil),
			"editable", "plaintext"), "readonly", true),
	}, snap, proto.ObserveRequest{MaxCandidates: proto.MaxCandidates})

	got := listByName(obs.Candidates)
	for _, name := range []string{"Fecha vuelta", "Locked note", "Fecha ida"} {
		c, ok := got[name]
		if !ok {
			t.Fatalf("%q is not a candidate: a readonly field is still clicked", name)
		}
		if c.Editable {
			t.Errorf("%q: editable = true, want false", name)
		}
	}
	if c := got["Passengers"]; !c.Editable {
		t.Errorf("a plain input = %+v, want it editable", c)
	}
}

// The sentence a page writes about how a widget works, from wherever it wrote
// it — and never from a field whose contents are its own business.
func TestCandidateDescription(t *testing.T) {
	const long = "Viaje de ida y vuelta, para cambiar a viaje de sólo ida pulsar flecha " +
		"hacia arriba. Para cambiar fechas usar las flechas de dirección izquierda y derecha, " +
		"y para elegir el día pulsar intro sobre el día que se quiera y cerrar el calendario."
	b := newDOM()
	root := b.el(-1, "html")
	body := b.el(root, "body")
	fromAX := b.el(body, "input", "type", "text", "aria-label", "Fecha vuelta")
	b.el(body, "input", "type", "text", "aria-label", "Fecha ida", "aria-describedby", "howto")
	howto := b.el(body, "div", "id", "howto")
	b.text(howto, "Usar las flechas de dirección para cambiar el día")
	b.el(body, "input", "type", "text", "aria-label", "Passengers", "title", "Up to nine")
	b.el(body, "input", "type", "text", "title", "Search")
	capped := b.el(body, "input", "type", "text", "aria-label", "Notes")
	b.el(body, "input", "type", "password", "aria-label", "Password", "aria-describedby", "howto")
	snap, _ := b.build()

	obs := observeOf(t, []map[string]any{
		axDesc(axNode("textbox", "Fecha vuelta", backendOf(fromAX), nil),
			"Para cambiar fechas usar las flechas"),
		axDesc(axNode("textbox", "Notes", backendOf(capped), nil), long),
	}, snap, proto.ObserveRequest{MaxCandidates: proto.MaxCandidates})

	got := listByName(obs.Candidates)
	for _, c := range []struct{ name, want string }{
		{"Fecha vuelta", "Para cambiar fechas usar las flechas"},
		{"Fecha ida", "Usar las flechas de dirección para cambiar el día"},
		{"Passengers", "Up to nine"},
		// A title that is the name again says nothing twice, and a secret
		// field carries nothing at all.
		{"Search", ""},
		{"Password", ""},
	} {
		if d := got[c.name].Description; d != c.want {
			t.Errorf("%q: description = %q, want %q", c.name, d, c.want)
		}
	}
	notes := got["Notes"].Description
	if len(notes) > proto.MaxDescriptionBytes || len(notes) < proto.MaxDescriptionBytes-8 {
		t.Errorf("a long description is %d bytes, want it capped at %d",
			len(notes), proto.MaxDescriptionBytes)
	}
	if !strings.HasPrefix(long, notes) {
		t.Errorf("the capped description = %q", notes)
	}
}

// pool is the candidates of one observation, by backend node id.
func pool(ids ...int64) []candidate {
	out := make([]candidate, 0, len(ids))
	for _, id := range ids {
		out = append(out, candidate{ref: elemRef{session: "S1", backend: id}})
	}
	return out
}

// isNew reads back which of a pool the observation called new.
func isNew(all []candidate) []int64 {
	var out []int64
	for i := range all {
		if all[i].c.New {
			out = append(out, all[i].ref.backend)
		}
	}
	return out
}

// What the last move put on the screen: the whole pool of the previous
// observation is remembered, not the shortlist it was cut down to, and a
// navigation is a page where everything is new and nothing is.
func TestRememberMarksWhatIsNew(t *testing.T) {
	at := &attachment{}
	const url = "https://example.test/search"

	first := pool(1, 2, 3)
	_, _ = at.remember("T1", url, first, nil)
	if got := isNew(first); got != nil {
		t.Fatalf("the first observation of a tab called %v new", got)
	}

	second := pool(1, 2, 3, 4)
	_, _ = at.remember("T1", url, second, nil)
	if got := isNew(second); !slices.Equal(got, []int64{4}) {
		t.Fatalf("new = %v, want the element that arrived", got)
	}

	// The element that arrived is old now, whether or not it made a shortlist:
	// what is remembered is the pool.
	third := pool(1, 2, 3, 4)
	_, _ = at.remember("T1", url, third, nil)
	if got := isNew(third); got != nil {
		t.Fatalf("new = %v on an unchanged page", got)
	}

	fourth := pool(1, 2, 3, 4, 5)
	_, _ = at.remember("T1", "https://example.test/results", fourth, nil)
	if got := isNew(fourth); got != nil {
		t.Fatalf("new = %v after a navigation, want nothing", got)
	}
}

// With nothing asked for by name, what the last move opened goes first: a
// calendar of day cells ranks above the fields and buttons it opened over. A
// hint outranks it — the step said what it was looking for.
func TestShortlistRanksNewFirst(t *testing.T) {
	all := []candidate{
		{c: proto.Candidate{Role: "link", Name: "Home"}, order: 0},
		{c: proto.Candidate{Role: "textbox", Name: "Search"}, order: 1},
		{c: proto.Candidate{Role: "button", Name: "23", New: true}, order: 2},
		{c: proto.Candidate{Role: "button", Name: "24", New: true}, order: 3},
	}
	names := func(list []proto.Candidate) []string {
		var out []string
		for _, c := range list {
			out = append(out, c.Name)
		}
		return out
	}
	if got, want := names(shortlist(all, nil, 10)), []string{"23", "24", "Search", "Home"}; !slices.Equal(got, want) {
		t.Fatalf("unhinted order = %v, want %v", got, want)
	}
	want := []string{"Search", "Home", "23", "24"}
	if got := names(shortlist(all, []string{"search"}, 10)); !slices.Equal(got, want) {
		t.Fatalf("hinted order = %v, want %v", got, want)
	}
}

// A page that keeps a banner standing says it once: a live region is read
// again only when its text has changed. What the page raises — a role=alert,
// the message a rejected field points at — is said every time.
func TestLiveRegionIsSaidOnceUntilItChanges(t *testing.T) {
	at := &attachment{}
	const url = "https://example.test/search"
	say := func(banner string) []string {
		_, texts := at.remember("T1", url, nil, []alert{
			{pos: 0, text: banner, backend: 7, session: "S1"},
			{pos: 1, text: "Your card was declined", backend: 8, session: "S1", always: true},
		})
		return texts
	}

	want := []string{"We use cookies", "Your card was declined"}
	if got := say("We use cookies"); !slices.Equal(got, want) {
		t.Fatalf("first observation = %q, want %q", got, want)
	}
	if got := say("We use cookies"); !slices.Equal(got, []string{"Your card was declined"}) {
		t.Fatalf("second observation = %q, want the raised alert alone", got)
	}
	want = []string{"We use fewer cookies", "Your card was declined"}
	if got := say("We use fewer cookies"); !slices.Equal(got, want) {
		t.Fatalf("a region that changed its text = %q, want %q", got, want)
	}
}

// observer is one tab observed again and again over a single attachment: the
// collection, the pointer gate and the assembly, so what a candidate does from
// one observation to the next can be read off the shortlist.
func observer(t *testing.T) func(snap any, url string) proto.Observation {
	t.Helper()
	f := newFakeCDP(t)
	e := testEngineNoBrowser(t)
	s := &session{e: e, agent: "alpha"}
	at := &attachment{s: s, conn: f.conn, bs: f.conn.Session(""), tabs: map[string]*tab{}, refs: map[string]elemRef{}}
	s.at = at
	f.result("Accessibility.getFullAXTree", map[string]any{"nodes": []map[string]any{}})
	f.result("Page.getLayoutMetrics", map[string]any{
		"layoutViewport": map[string]any{"pageX": 0, "pageY": 0, "clientWidth": 1280, "clientHeight": 800},
	})
	return func(snap any, url string) proto.Observation {
		t.Helper()
		f.result("DOMSnapshot.captureSnapshot", snap)
		all, _, said, st, err := at.collect(t.Context(), "S1", 0, 0)
		if err != nil {
			t.Fatalf("collect: %v", err)
		}
		all, _ = at.remember("T1", url, all, said)
		nameRefs(all, 1)
		var obs proto.Observation
		assemble(&obs, all, []*pageStruct{st}, proto.ObserveRequest{MaxCandidates: proto.MaxCandidates})
		return obs
	}
}

// calendarPage is a date field and a container of hand-styled day cells with no
// role, no handler of their own and no tabindex: the picker a click opens.
func calendarPage(days int) *cdp.Snapshot {
	b := newDOM()
	root := b.el(-1, "html")
	body := b.el(root, "body")
	b.el(body, "input", "type", "text", "aria-label", "Fecha ida")
	cal := b.el(body, "div", "id", "cal")
	for d := 1; d <= days; d++ {
		cell := b.el(cal, "div")
		b.text(cell, strconv.Itoa(d))
		b.pointer(cell)
	}
	snap, _ := b.build()
	return snap
}

// dayCells counts the day cells of a shortlist, wherever they sit on it.
func dayCells(list []proto.Candidate) int {
	n := 0
	for _, c := range list {
		if c.Role != "button" {
			continue
		}
		if d, err := strconv.Atoi(c.Name); err == nil && d > 0 {
			n++
		}
	}
	return n
}

// A page styles as much of itself as it likes: what makes a hand-styled div
// worth reporting is that the last move put it there. It is kept for as long as
// it stays on the page after that, and forgotten on a navigation.
func TestPointerCandidatesAreKeptOnlyWhenTheyArrive(t *testing.T) {
	const url = "https://example.test/search"
	see := observer(t)

	// The first observation of a tab: everything is new and nothing is, so the
	// cells of a calendar that was open all along are not candidates.
	obs := see(calendarPage(3), url)
	if n := dayCells(obs.Candidates); n != 0 {
		t.Fatalf("the first observation shortlisted %d day cells: %+v", n, obs.Candidates)
	}
	if obs.Total != 1 {
		t.Fatalf("total = %d, want the field alone", obs.Total)
	}

	// The picker opens: the cells arrived, so they are candidates, and with
	// nothing asked for by name they are what the shortlist starts with.
	obs = see(calendarPage(0), url)
	obs = see(calendarPage(3), url)
	if n := dayCells(obs.Candidates); n != 3 {
		t.Fatalf("the observation after the click shortlisted %d day cells: %+v", n, obs.Candidates)
	}
	for i, c := range obs.Candidates[:3] {
		// With nothing asked for by name, what the last move opened is what
		// the shortlist starts with.
		if c.Name != strconv.Itoa(i+1) || !c.New {
			t.Fatalf("candidate %d = %+v, want the day cell %d, new and first", i, c, i+1)
		}
	}
	if obs.Total != 4 {
		t.Fatalf("total = %d, want the field and the three cells", obs.Total)
	}

	// And they stay: a cell that vanished from the shortlist the moment it
	// stopped being new would be a calendar nobody can finish using.
	obs = see(calendarPage(3), url)
	if n := dayCells(obs.Candidates); n != 3 {
		t.Fatalf("the next observation shortlisted %d day cells: %+v", n, obs.Candidates)
	}
	if obs.Candidates[0].New {
		t.Fatalf("a cell that was there before is marked new: %+v", obs.Candidates[0])
	}

	// A navigation forgets it: on a new page nothing arrived.
	obs = see(calendarPage(3), "https://example.test/results")
	if n := dayCells(obs.Candidates); n != 0 {
		t.Fatalf("after a navigation the shortlist holds %d day cells: %+v", n, obs.Candidates)
	}
	if obs.Total != 1 {
		t.Fatalf("total = %d after a navigation, want the field alone", obs.Total)
	}
}

// A page that styles half of itself as clickable costs a bounded number of
// candidates: the first maxPointerCandidates in document order and no more.
func TestPointerCandidatesAreCapped(t *testing.T) {
	const url = "https://example.test/search"
	see := observer(t)
	see(calendarPage(0), url)
	obs := see(calendarPage(maxPointerCandidates+10), url)
	if obs.Total != maxPointerCandidates+1 {
		t.Fatalf("total = %d, want the field and %d cells", obs.Total, maxPointerCandidates)
	}
	if n := dayCells(obs.Candidates); n != maxPointerCandidates {
		t.Fatalf("the shortlist starts with %d cells, want %d", n, maxPointerCandidates)
	}
}
