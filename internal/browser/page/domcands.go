package page

import (
	"strconv"
	"strings"

	"holaclaw/internal/browser/cdp"
	"holaclaw/internal/browser/proto"
)

// The accessibility tree is not the whole supply of candidates: it under-reports
// inputs (measured by others on a plain search box, and on Google Flights here),
// it drops what a page marks aria-hidden, and it names a control by what the
// page declared rather than by what a person reads on it. So the DOMSnapshot
// observation already takes for boxes is read a second time, as a source of
// interactables in its own right, and the two sets are merged on the backend
// node id.
//
// Nothing here runs script: the node table, its attributes and the snapshot's
// own isClickable flag are all in the result the protocol already sent, so the
// stealth rule of internal/browser/cdp is untouched. Every string produced here
// is page content: it is capped and it goes into the candidate, nowhere else.

// domElem is one interactable the DOM says exists.
type domElem struct {
	backend  int64
	role     string
	name     string
	value    string
	disabled bool
	// The states the element publishes about itself, read off its attributes:
	// the accessibility tree reports them too, but not always, and a page that
	// writes aria-expanded on a div the tree calls generic still means it.
	expanded *bool
	hasPopup string
	invalid  bool
	required bool
	// editable says characters go into the element and fixed says they do not,
	// whatever the accessibility tree makes of it: a native select is answered
	// with the keyboard but nothing is typed into it.
	editable bool
	fixed    bool
	// secret is set for a field whose contents are the page's business alone;
	// value is left empty for it, here and everywhere after.
	secret bool
	// generic is set for an element that is a candidate only because it has a
	// click handler, a tabindex or a clickable box. Those are dropped when a
	// named control sits inside or around them.
	generic bool
	// pointer is the narrower case still: an element nothing but its computed
	// cursor marks as clickable. A page that delegates its clicks to a
	// container leaves its day cells with no handler, no role and no tabindex
	// of their own, and the hand drawn over them is all there is to go on.
	// Those are gated on having just arrived (observe.go).
	pointer bool
	// desc is the sentence the page wrote about how the element works, from
	// aria-describedby or a title that is not simply the name again.
	desc string
}

// DOM node types the reader cares about.
const (
	nodeElement = 1
	nodeText    = 3
)

// maxInnerTextNodes bounds the walk that builds a name out of an element's own
// text: a name is at most MaxNameBytes, so a wrapper full of prose is not read
// to the end to produce one.
const maxInnerTextNodes = 64

// inputRoles maps an input's type to its accessibility role. An unknown type is
// a textbox, which is what a browser does with it too.
var inputRoles = map[string]string{
	"button":   "button",
	"checkbox": "checkbox",
	"color":    "button",
	"file":     "button",
	"image":    "button",
	"number":   "spinbutton",
	"radio":    "radio",
	"range":    "slider",
	"reset":    "button",
	"search":   "searchbox",
	"submit":   "button",
}

// genericSkip are tags that never become a candidate on a click handler alone:
// a listener on the body or on a layout wrapper is delegation, not a control.
var genericSkip = map[string]bool{
	"html": true, "body": true, "head": true, "script": true, "style": true,
	"template": true, "form": true, "main": true, "nav": true, "header": true,
	"footer": true, "section": true, "article": true, "aside": true,
	"ul": true, "ol": true, "dl": true, "table": true, "tbody": true,
	"thead": true, "tfoot": true, "tr": true, "iframe": true, "frame": true,
	"noscript": true, "picture": true, "video": true, "audio": true,
	// A label is clickable by definition — it forwards a click to its control —
	// so it is a candidate only through labelStandIn, never on its own.
	"label": true, "optgroup": true,
}

// domIndex is one snapshot read as a DOM: the interactables it holds, the
// place of every node in document order, and the readers themselves, so a
// node can be looked up again by backend id once the candidates are known.
type domIndex struct {
	elems []domElem
	order map[int64]int
	docs  []*docReader
}

// scanDOM reads a snapshot as a DOM. boxes is what readSnapshot found, so an
// element with no layout box — display:none, a <template>'s content, a
// zero-area wrapper — never becomes a candidate. cursors is what the snapshot
// said about the pointer, one map of node index to cursor per document
// (observe.go).
func scanDOM(snap *cdp.Snapshot, boxes map[int64]rect, cursors []map[int]string) *domIndex {
	x := &domIndex{order: make(map[int64]int, 256)}
	pos := 0
	for di := range snap.Documents {
		doc := &snap.Documents[di]
		for i := range doc.Nodes.BackendNodeID {
			if _, dup := x.order[doc.Nodes.BackendNodeID[i]]; !dup {
				x.order[doc.Nodes.BackendNodeID[i]] = pos
			}
			pos++
		}
		d := newDocReader(snap, doc)
		x.docs = append(x.docs, d)
		var cursor map[int]string
		if di < len(cursors) {
			cursor = cursors[di]
		}
		x.elems = append(x.elems, d.interactables(boxes, cursor)...)
	}
	return x
}

// domInteractables is the scan's two halves on their own: the interactables in
// document order, and the position of every node in that order so the merged
// candidate list can be put back into it.
func domInteractables(snap *cdp.Snapshot, boxes map[int64]rect, cursors []map[int]string) ([]domElem, map[int64]int) {
	x := scanDOM(snap, boxes, cursors)
	return x.elems, x.order
}

// node finds a node again by its backend id: which document it belongs to and
// where in that document's table it sits.
func (x *domIndex) node(backend int64) (*docReader, int, bool) {
	for _, d := range x.docs {
		if i, ok := d.byBackend[backend]; ok {
			return d, i, true
		}
	}
	return nil, 0, false
}

// alert is one thing the page is telling the user: where it was found — which
// frame, and where in that frame's document order — which element said it, and
// whether the page raised it now or keeps it standing. The frame and the
// position put the collected messages back into the order a person would read
// them in; the element is what a later observation compares against, so a
// region that has not changed a word is not announced twice.
type alert struct {
	pos     int
	text    string
	backend int64
	session string
	doc     int
	always  bool
}

// alerts is what the page announces of its own accord: the text of role=alert
// and role=status elements and of its live regions. A region with no layout
// box is one the page is not showing — a template for a message it has not
// made yet — and says nothing.
func (x *domIndex) alerts(boxes map[int64]rect) []alert {
	var out []alert
	for _, d := range x.docs {
		for i := range d.n {
			if d.tags[i] == "" {
				continue
			}
			raised, live := d.isLive(i)
			if !live {
				continue
			}
			id := d.doc.Nodes.BackendNodeID[i]
			if box, drawn := boxes[id]; !drawn || box.empty() {
				continue
			}
			if s := capName(d.innerText(i)); s != "" {
				out = append(out, alert{pos: x.order[id], text: s, backend: id, always: raised})
			}
		}
	}
	return out
}

// isLive reports whether an element is one of the page's announcements, and
// whether it is one the page raised rather than one it keeps: a role=alert is
// something a page says now and is reported every time, while a status and a
// live region are read again only when their text has changed. aria-live="off"
// is a region the page has switched off and is not an announcement.
func (d *docReader) isLive(i int) (raised, live bool) {
	if v, ok := d.attr(i, "role"); ok {
		switch firstWord(strings.ToLower(v)) {
		case "alert":
			return true, true
		case "status":
			return false, true
		}
	}
	if v, ok := d.attr(i, "aria-live"); ok {
		s := strings.ToLower(strings.TrimSpace(v))
		return false, s != "" && s != "off"
	}
	return false, false
}

// messages is what a field points at as its own error or description: the
// page's complaint about what it has just rejected. rel is what the
// accessibility tree said about the same relations, which a page that wires
// them up in script sometimes has when the attributes do not.
func (x *domIndex) messages(backend int64, rel []cdp.AXRelatedNode, boxes map[int64]rect) []alert {
	var out []alert
	seen := make(map[int64]bool, 4)
	add := func(id int64) {
		if id == 0 || seen[id] {
			return
		}
		seen[id] = true
		if box, drawn := boxes[id]; !drawn || box.empty() {
			return // a message the page is not showing
		}
		d, i, ok := x.node(id)
		if !ok {
			return
		}
		if s := capName(d.innerText(i)); s != "" {
			// The page has rejected a field and is pointing at why: that is
			// said every time it is still true, whatever was said before.
			out = append(out, alert{pos: x.order[id], text: s, backend: id, always: true})
		}
	}
	// An idref is resolved in the document of the field that wrote it: ids are
	// unique per document and two frames may well use the same one.
	field, fi, haveField := x.node(backend)
	byRef := func(ref string) {
		if !haveField {
			return
		}
		if n, ok := field.byID[ref]; ok {
			add(field.doc.Nodes.BackendNodeID[n])
		}
	}
	if haveField {
		for _, a := range []string{"aria-errormessage", "aria-describedby"} {
			if v, ok := field.attr(fi, a); ok {
				for _, ref := range strings.Fields(v) {
					byRef(ref)
				}
			}
		}
	}
	for _, r := range rel {
		if r.BackendDOMNodeID != 0 {
			add(r.BackendDOMNodeID)
			continue
		}
		byRef(r.IDRef)
	}
	return out
}

// docReader reads one document of a snapshot: its tags, attributes, id and
// label indexes, and the text under a node.
type docReader struct {
	snap *cdp.Snapshot
	doc  *cdp.DocumentSnapshot
	n    int
	// tags holds the lowercased tag name of every element, "" for a node that
	// is not one.
	tags []string
	// byID maps an id attribute to its node, byBackend a backend node id to
	// its node, labelFor a label's for attribute to the label, and clickable
	// is the snapshot's own flag as a set.
	byID      map[string]int
	byBackend map[int64]int
	labelFor  map[string]int
	clickable map[int]bool
}

func newDocReader(snap *cdp.Snapshot, doc *cdp.DocumentSnapshot) *docReader {
	d := &docReader{
		snap:      snap,
		doc:       doc,
		n:         len(doc.Nodes.BackendNodeID),
		byID:      make(map[string]int, 64),
		byBackend: make(map[int64]int, len(doc.Nodes.BackendNodeID)),
		labelFor:  make(map[string]int, 16),
		clickable: make(map[int]bool, len(doc.Nodes.IsClickable.Index)),
	}
	for _, i := range doc.Nodes.IsClickable.Index {
		d.clickable[i] = true
	}
	d.tags = make([]string, d.n)
	for i := range d.n {
		if _, dup := d.byBackend[doc.Nodes.BackendNodeID[i]]; !dup {
			d.byBackend[doc.Nodes.BackendNodeID[i]] = i
		}
		if i >= len(doc.Nodes.NodeType) || doc.Nodes.NodeType[i] != nodeElement {
			continue
		}
		tag := strings.ToLower(d.str(d.nodeName(i)))
		d.tags[i] = tag
		if id, ok := d.attr(i, "id"); ok && id != "" {
			if _, dup := d.byID[id]; !dup {
				d.byID[id] = i
			}
		}
		if tag == "label" {
			if f, ok := d.attr(i, "for"); ok && f != "" {
				if _, dup := d.labelFor[f]; !dup {
					d.labelFor[f] = i
				}
			}
		}
	}
	return d
}

// interactables classifies every element of the document.
func (d *docReader) interactables(boxes map[int64]rect, cursor map[int]string) []domElem {
	var out []domElem
	at := make(map[int]int, 32) // node index → position in out
	// What the cursor alone marks, read off the styles the snapshot already
	// carries.
	pointers := d.pointerNodes(boxes, cursor)
	for i := range d.n {
		if d.tags[i] == "" {
			continue
		}
		e, ok := d.classify(i, boxes)
		if !ok {
			if !pointers[i] {
				continue
			}
			// A pointer candidate is a generic one: the same pruning drops it
			// when a real control sits inside it or around it, and drops the
			// container whose delegated handler made it look clickable.
			e = d.elemOf(i, "button", true)
			e.pointer = true
		}
		if box, drawn := boxes[e.backend]; !drawn || box.empty() {
			continue // not rendered: display:none, a template's content, no area
		}
		if d.inTemplate(i) {
			continue
		}
		at[i] = len(out)
		out = append(out, e)
	}
	return d.pruneGenerics(out, at)
}

// classify decides what an element is, "" when it is nothing a step can act on.
func (d *docReader) classify(i int, boxes map[int64]rect) (domElem, bool) {
	tag := d.tags[i]
	role, generic := "", false
	switch tag {
	case "input", "textarea", "select", "button":
		var ok bool
		if role, ok = d.controlRole(i); !ok {
			return domElem{}, false // an input the page keeps hidden
		}
	case "a", "area":
		if href, ok := d.attr(i, "href"); ok && href != "" {
			role = "link"
		}
	case "summary":
		role = "disclosuretriangle"
	case "label":
		// A label for a control the page hides — the styled checkbox, the
		// custom switch — is what a person actually clicks, so it stands in
		// for its control. A label around a visible one is left to the control.
		role = d.labelStandIn(i, boxes)
	}
	if r, ok := d.attr(i, "role"); ok {
		if first := firstWord(strings.ToLower(r)); interactiveRoles[first] {
			role, generic = first, false
		}
	}
	if role == "" {
		if ce, ok := d.attr(i, "contenteditable"); ok && !strings.EqualFold(ce, "false") {
			role = "textbox"
		}
	}
	if role == "" && !genericSkip[tag] && d.isClickable(i) {
		role, generic = "button", true
	}
	if role == "" {
		return domElem{}, false
	}
	return d.elemOf(i, role, generic), true
}

// elemOf reads an element whose role is already settled: the states it
// publishes, what it holds, what it is called and what the page wrote about it.
func (d *docReader) elemOf(i int, role string, generic bool) domElem {
	e := domElem{
		backend:  d.doc.Nodes.BackendNodeID[i],
		role:     role,
		generic:  generic,
		disabled: d.isDisabled(i),
	}
	d.flags(i, role, &e)
	// The one place a DOM candidate's value is filled: a secret field is read
	// as empty here, so no later path can hand its contents out.
	if !e.secret {
		e.value = capName(d.value(i))
	}
	e.name = capName(d.name(i, role))
	e.desc = d.describe(i, e.name)
	return e
}

// maxPointerNameBytes is how long a hand-styled element may be called and still
// be one thing to click: past it what the hand is drawn over is prose a page
// made clickable — a whole card, a paragraph, a row — and not a control, unless
// the page named it itself.
const maxPointerNameBytes = 40

// pointerNodes are the elements a page shows a hand over and says nothing else
// about: no role, no handler of their own, no tabindex — the day cells of a
// calendar whose clicks its container handles. One qualifies when it is drawn,
// when its tag is not one clicks are delegated through (genericSkip), when it
// is not inside a <template>, and when it is called something short enough to
// be a control. The hand is drawn over the wrapper of a cell as well as over
// the cell, and it is the pruning that keeps the innermost of them: a
// hand-styled element that holds another candidate is a wrapper like any other.
func (d *docReader) pointerNodes(boxes map[int64]rect, cursor map[int]string) map[int]bool {
	if len(cursor) == 0 {
		return nil
	}
	out := make(map[int]bool, len(cursor))
	for i := range d.n {
		if cursor[i] != "pointer" || d.tags[i] == "" || genericSkip[d.tags[i]] {
			continue
		}
		if box, drawn := boxes[d.doc.Nodes.BackendNodeID[i]]; !drawn || box.empty() {
			continue
		}
		if !d.pointerNamed(i) || d.inTemplate(i) {
			continue
		}
		out[i] = true
	}
	return out
}

// pointerNamed reports whether a hand-styled element is called something a
// reader could act on: a short name, wherever it was read off, or a name the
// page wrote itself, which is as long as the page wants it.
func (d *docReader) pointerNamed(i int) bool {
	name := strings.Join(strings.Fields(d.name(i, "button")), " ")
	if name == "" {
		return false
	}
	if len(name) <= maxPointerNameBytes {
		return true
	}
	for _, a := range []string{"aria-label", "title"} {
		if v, ok := d.attr(i, a); ok && strings.TrimSpace(v) != "" {
			return true
		}
	}
	return false
}

// flags reads the states an element publishes about itself: what it opens,
// whether it is open, whether the page has rejected it, whether it must be
// filled, and whether what it holds is nobody's business.
func (d *docReader) flags(i int, role string, e *domElem) {
	if v, ok := d.attr(i, "aria-expanded"); ok {
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "true":
			t := true
			e.expanded = &t
		case "false":
			f := false
			e.expanded = &f
		}
	}
	if v, ok := d.attr(i, "aria-haspopup"); ok {
		if s := strings.ToLower(strings.TrimSpace(v)); s != "" && s != "false" {
			e.hasPopup = s
		}
	}
	if v, ok := d.attr(i, "aria-invalid"); ok {
		if s := strings.ToLower(strings.TrimSpace(v)); s != "" && s != "false" {
			e.invalid = true
		}
	}
	if _, ok := d.attr(i, "required"); ok {
		e.required = true
	}
	if v, ok := d.attr(i, "aria-required"); ok && strings.EqualFold(strings.TrimSpace(v), "true") {
		e.required = true
	}
	// A box that suggests as it is typed in opens a list, whether the page
	// says so with aria-autocomplete or with a datalist behind an input. The
	// executor wants one signal for "this opens a list", not three.
	if e.hasPopup == "" {
		if v, ok := d.attr(i, "aria-autocomplete"); ok && listAutocomplete(v) {
			e.hasPopup = "listbox"
		}
	}
	if e.hasPopup == "" && role == "combobox" && d.tags[i] == "input" {
		if _, ok := d.attr(i, "list"); ok {
			e.hasPopup = "listbox"
		}
	}
	d.typing(i, e)
	if d.isReadonly(i) {
		// The page says its text cannot be written, and that settles it: a
		// browser calls a readonly input editable — the kind of control is —
		// and a field that only opens a calendar would then be typed into.
		e.editable, e.fixed = false, true
	}
	if v, ok := d.attr(i, "type"); ok && strings.EqualFold(strings.TrimSpace(v), "password") {
		e.secret = true
	}
	if v, ok := d.attr(i, "autocomplete"); ok && secretAutocomplete(v) {
		e.secret = true
	}
}

// typing decides whether characters can be put into an element: what a person
// types goes into a text input, a textarea and anything the page marks
// contenteditable, and nowhere else. A button, a box to tick, a file to pick
// and a native select all hold a value that is chosen rather than written.
func (d *docReader) typing(i int, e *domElem) {
	switch d.tags[i] {
	case "input":
		t := "text"
		if v, ok := d.attr(i, "type"); ok && strings.TrimSpace(v) != "" {
			t = strings.ToLower(strings.TrimSpace(v))
		}
		if notTyped[t] {
			e.fixed = true
		} else {
			e.editable = true
		}
	case "textarea":
		e.editable = true
	case "select":
		e.fixed = true
	}
	// contenteditable is the page's own word on it and has the last one: an
	// empty value is the attribute present, which means true.
	if v, ok := d.attr(i, "contenteditable"); ok && !strings.EqualFold(strings.TrimSpace(v), "false") {
		e.editable, e.fixed = true, false
	}
}

// notTyped are the input types that hold no text.
var notTyped = map[string]bool{
	"button": true, "submit": true, "reset": true, "checkbox": true,
	"radio": true, "image": true, "file": true, "range": true,
	"color": true, "hidden": true,
}

// listAutocomplete reports whether an aria-autocomplete value means the
// element offers a list of completions.
func listAutocomplete(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "list", "both", "inline":
		return true
	}
	return false
}

// secretTokens are the autocomplete tokens that name something nobody but the
// page should be told: a card, a password, a one-time code.
var secretTokens = map[string]bool{
	"cc-number": true, "cc-csc": true, "cc-exp": true, "cc-exp-month": true,
	"cc-exp-year": true, "current-password": true, "new-password": true,
	"one-time-code": true,
}

// secretAutocomplete reads an autocomplete attribute, which may carry a
// section and a billing/shipping hint before the field name itself.
func secretAutocomplete(v string) bool {
	for _, tok := range strings.Fields(strings.ToLower(v)) {
		if secretTokens[tok] {
			return true
		}
	}
	return false
}

// controlRole is the role of a form control, false when it is not rendered as
// one at all (an input the page marks hidden).
func (d *docReader) controlRole(i int) (string, bool) {
	switch d.tags[i] {
	case "input":
		t := "text"
		if v, ok := d.attr(i, "type"); ok && v != "" {
			t = strings.ToLower(v)
		}
		if t == "hidden" {
			return "", false
		}
		role := inputRoles[t]
		if role == "" {
			role = "textbox"
		}
		if _, list := d.attr(i, "list"); list && role == "textbox" {
			role = "combobox" // an input with a datalist behind it
		}
		return role, true
	case "textarea":
		return "textbox", true
	case "select":
		if _, multi := d.attr(i, "multiple"); multi {
			return "listbox", true
		}
		return "combobox", true
	case "button":
		return "button", true
	}
	return "", false
}

// labelStandIn gives a label the role of the control it names, but only when
// that control has no box of its own.
func (d *docReader) labelStandIn(i int, boxes map[int64]rect) string {
	f, ok := d.attr(i, "for")
	if !ok || f == "" {
		return ""
	}
	target, ok := d.byID[f]
	if !ok {
		return ""
	}
	if box, drawn := boxes[d.doc.Nodes.BackendNodeID[target]]; drawn && !box.empty() {
		return "" // the control itself is a candidate; the label would double it
	}
	role, _ := d.controlRole(target)
	return role
}

// name is the best name the DOM alone can give an element: what the page
// declared, then what is written next to it, then what is written on it.
func (d *docReader) name(i int, role string) string {
	if v, ok := d.attr(i, "aria-label"); ok && strings.TrimSpace(v) != "" {
		return v
	}
	if v, ok := d.attr(i, "aria-labelledby"); ok {
		if s := d.textOfIDs(v, proto.MaxNameBytes); s != "" {
			return s
		}
	}
	if s := d.labelText(i); s != "" {
		return s
	}
	for _, a := range []string{"placeholder", "title", "alt", "name"} {
		if v, ok := d.attr(i, a); ok && strings.TrimSpace(v) != "" {
			return v
		}
	}
	if role == "button" {
		if v, ok := d.attr(i, "value"); ok && strings.TrimSpace(v) != "" {
			return v
		}
	}
	// A control the page never named: what it told the reader about it, then
	// what is drawn on it, then what is written in it. A button with an icon
	// and nothing else is the common case, and "" is no use to anybody.
	if v, ok := d.attr(i, "aria-describedby"); ok {
		if s := d.textOfIDs(v, proto.MaxNameBytes); s != "" {
			return s
		}
	}
	if s := d.iconName(i); s != "" {
		return s
	}
	return d.innerText(i)
}

// describe is the page's own sentence about how an element works: what
// aria-describedby points at, else a title that says something other than the
// name. It is longer than a name and capped on its own.
func (d *docReader) describe(i int, name string) string {
	if v, ok := d.attr(i, "aria-describedby"); ok {
		if s := d.textOfIDs(v, proto.MaxDescriptionBytes); s != "" {
			return s
		}
	}
	if v, ok := d.attr(i, "title"); ok {
		if s := strings.Join(strings.Fields(v), " "); s != "" && s != name {
			return s
		}
	}
	return ""
}

// iconName is the name of a control whose contents are a picture: the <title>
// inside its svg, else the alt or the aria-label of something under it. A page
// that draws its buttons rather than writing them still says what they are,
// once, where its own reader would look.
func (d *docReader) iconName(i int) string {
	alt := ""
	for k, seen := i+1, 0; k < d.n && seen < maxInnerTextNodes; k, seen = k+1, seen+1 {
		if p := d.parentRaw(k); p < i || p == k {
			break
		}
		if d.tags[k] == "" {
			continue
		}
		if d.tags[k] == "title" {
			if s := d.innerText(k); s != "" {
				return s
			}
		}
		if alt != "" {
			continue
		}
		for _, a := range []string{"alt", "aria-label"} {
			if v, ok := d.attr(k, a); ok && strings.TrimSpace(v) != "" {
				alt = v
				break
			}
		}
	}
	return alt
}

// labelText is the text of the label that names an element: the one that points
// at its id, else the one it sits inside.
func (d *docReader) labelText(i int) string {
	if id, ok := d.attr(i, "id"); ok && id != "" {
		if l, ok := d.labelFor[id]; ok {
			if s := d.innerText(l); s != "" {
				return s
			}
		}
	}
	for p := d.parent(i); p >= 0; p = d.parent(p) {
		if d.tags[p] == "label" {
			return d.innerText(p)
		}
	}
	return ""
}

// textOfIDs resolves an idref list — an aria-labelledby, an aria-describedby —
// to the text it points at, up to limit bytes of each piece.
func (d *docReader) textOfIDs(list string, limit int) string {
	var parts []string
	for _, id := range strings.Fields(list) {
		if n, ok := d.byID[id]; ok {
			if s := d.innerTextTo(n, limit); s != "" {
				parts = append(parts, s)
			}
		}
	}
	return strings.Join(parts, " ")
}

// innerText is the text under a node, bounded like a name.
func (d *docReader) innerText(i int) string {
	return d.innerTextTo(i, proto.MaxNameBytes)
}

// innerTextTo is the text under a node, stopping at limit bytes. The node
// table is a pre-order traversal, so a node's descendants are the run that
// follows it while the parents stay inside it; the walk is bounded in nodes
// and in bytes, since nothing read here travels uncapped.
func (d *docReader) innerTextTo(i, limit int) string {
	var b strings.Builder
	for k, seen := i+1, 0; k < d.n && seen < maxInnerTextNodes; k, seen = k+1, seen+1 {
		if p := d.parentRaw(k); p < i {
			break
		}
		if k >= len(d.doc.Nodes.NodeType) || d.doc.Nodes.NodeType[k] != nodeText {
			continue
		}
		s := strings.TrimSpace(d.str(d.nodeValue(k)))
		if s == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString(" ")
		}
		b.WriteString(s)
		if b.Len() >= limit {
			break
		}
	}
	return b.String()
}

// value is what an input holds, the attribute when the snapshot says nothing.
func (d *docReader) value(i int) string {
	if idx, ok := d.doc.Nodes.InputValue.Get(i); ok {
		if s := d.str(idx); s != "" {
			return s
		}
	}
	if d.tags[i] == "input" || d.tags[i] == "textarea" {
		if v, ok := d.attr(i, "value"); ok {
			return v
		}
	}
	return ""
}

// isReadonly reports whether the page says the element's text cannot be
// written: the DOM attribute, which a browser puts on the control itself, or
// the ARIA state, which a page writes on a widget of its own making.
func (d *docReader) isReadonly(i int) bool {
	if _, ok := d.attr(i, "readonly"); ok {
		return true
	}
	v, ok := d.attr(i, "aria-readonly")
	return ok && strings.EqualFold(strings.TrimSpace(v), "true")
}

func (d *docReader) isDisabled(i int) bool {
	if _, ok := d.attr(i, "disabled"); ok {
		return true
	}
	v, ok := d.attr(i, "aria-disabled")
	return ok && strings.EqualFold(v, "true")
}

// isClickable is the last resort: the snapshot's own flag (an element that
// would respond to a click), an onclick attribute, or a tabindex that puts the
// element in the tab order.
func (d *docReader) isClickable(i int) bool {
	if d.clickable[i] {
		return true
	}
	if _, ok := d.attr(i, "onclick"); ok {
		return true
	}
	if v, ok := d.attr(i, "tabindex"); ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n >= 0 {
			return true
		}
	}
	return false
}

// pruneGenerics drops a generic clickable that wraps another candidate (a card
// around a button) or sits inside a real one (the icon inside a button): what
// is left is the innermost element worth a click.
func (d *docReader) pruneGenerics(out []domElem, at map[int]int) []domElem {
	if len(out) == 0 {
		return out
	}
	wraps := make(map[int]bool, len(at)*4)
	real := make(map[int]bool, len(at))
	for i, k := range at {
		if !out[k].generic {
			real[i] = true
		}
		for p := d.parent(i); p >= 0; p = d.parent(p) {
			if wraps[p] {
				break
			}
			wraps[p] = true
		}
	}
	drop := make(map[int]bool, 8)
	for i, k := range at {
		if !out[k].generic {
			continue
		}
		if wraps[i] {
			drop[k] = true
			continue
		}
		for p := d.parent(i); p >= 0; p = d.parent(p) {
			if real[p] {
				drop[k] = true
				break
			}
		}
	}
	keep := make([]domElem, 0, len(out))
	for k := range out {
		if !drop[k] {
			keep = append(keep, out[k])
		}
	}
	return keep
}

// inTemplate reports whether a node sits inside a <template>: its content is
// not part of the page, however laid out it looks.
func (d *docReader) inTemplate(i int) bool {
	for p := d.parent(i); p >= 0; p = d.parent(p) {
		if d.tags[p] == "template" {
			return true
		}
	}
	return false
}

// attr reads one attribute; the table is name,value,name,value as string
// indexes.
func (d *docReader) attr(i int, name string) (string, bool) {
	if i < 0 || i >= len(d.doc.Nodes.Attributes) {
		return "", false
	}
	a := d.doc.Nodes.Attributes[i]
	for k := 0; k+1 < len(a); k += 2 {
		if strings.EqualFold(d.str(a[k]), name) {
			return d.str(a[k+1]), true
		}
	}
	return "", false
}

func (d *docReader) parent(i int) int {
	p := d.parentRaw(i)
	if p == i {
		return -1
	}
	return p
}

func (d *docReader) parentRaw(i int) int {
	if i < 0 || i >= len(d.doc.Nodes.ParentIndex) {
		return -1
	}
	return d.doc.Nodes.ParentIndex[i]
}

func (d *docReader) nodeName(i int) cdp.StrIdx {
	if i < len(d.doc.Nodes.NodeName) {
		return d.doc.Nodes.NodeName[i]
	}
	return -1
}

func (d *docReader) nodeValue(i int) cdp.StrIdx {
	if i < len(d.doc.Nodes.NodeValue) {
		return d.doc.Nodes.NodeValue[i]
	}
	return -1
}

func (d *docReader) str(i cdp.StrIdx) string { return d.snap.Str(i) }

// firstWord is the first token of a space-separated attribute: a role attribute
// may list fallbacks.
func firstWord(s string) string {
	if f := strings.Fields(s); len(f) > 0 {
		return f[0]
	}
	return ""
}
