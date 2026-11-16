package page

import (
	"strings"
	"testing"

	"holaclaw/internal/browser/cdp"
)

// domBuilder writes the snapshot a real page would produce, node by node: the
// node table with its attributes, one layout box per element, and the sparse
// columns the reader uses. Backend node ids are the node index plus one.
type domBuilder struct {
	snap  cdp.Snapshot
	doc   cdp.DocumentSnapshot
	boxes map[int64]rect
	strs  map[string]cdp.StrIdx
	y     float64
}

func newDOM() *domBuilder {
	b := &domBuilder{boxes: map[int64]rect{}, strs: map[string]cdp.StrIdx{}}
	b.str("")
	return b
}

func (b *domBuilder) str(s string) cdp.StrIdx {
	if i, ok := b.strs[s]; ok {
		return i
	}
	i := cdp.StrIdx(len(b.snap.Strings))
	b.snap.Strings = append(b.snap.Strings, s)
	b.strs[s] = i
	return i
}

// el adds an element with a box, and returns its node index. attrs are
// name, value pairs.
func (b *domBuilder) el(parent int, tag string, attrs ...string) int {
	i := b.node(parent, nodeElement, strings.ToUpper(tag), "")
	var a []cdp.StrIdx
	for k := 0; k+1 < len(attrs); k += 2 {
		a = append(a, b.str(attrs[k]), b.str(attrs[k+1]))
	}
	b.doc.Nodes.Attributes[i] = a
	b.y += 40
	b.box(i, rect{x: 10, y: b.y, w: 100, h: 30})
	return i
}

// text adds a text node under a parent.
func (b *domBuilder) text(parent int, s string) int {
	return b.node(parent, nodeText, "#text", s)
}

func (b *domBuilder) node(parent, typ int, name, value string) int {
	i := len(b.doc.Nodes.BackendNodeID)
	b.doc.Nodes.ParentIndex = append(b.doc.Nodes.ParentIndex, parent)
	b.doc.Nodes.NodeType = append(b.doc.Nodes.NodeType, typ)
	b.doc.Nodes.NodeName = append(b.doc.Nodes.NodeName, b.str(name))
	b.doc.Nodes.NodeValue = append(b.doc.Nodes.NodeValue, b.str(value))
	b.doc.Nodes.BackendNodeID = append(b.doc.Nodes.BackendNodeID, int64(i+1))
	b.doc.Nodes.Attributes = append(b.doc.Nodes.Attributes, nil)
	return i
}

// box gives a node a layout box; a node without one is not rendered. Every
// laid-out node carries the computed styles the observation asks for, in the
// order it asks for them (snapshotStyles).
func (b *domBuilder) box(i int, r rect) {
	b.doc.Layout.NodeIndex = append(b.doc.Layout.NodeIndex, i)
	b.doc.Layout.Bounds = append(b.doc.Layout.Bounds, []float64{r.x, r.y, r.w, r.h})
	b.doc.Layout.Text = append(b.doc.Layout.Text, -1)
	b.doc.Layout.Styles = append(b.doc.Layout.Styles,
		[]cdp.StrIdx{b.str("block"), b.str("visible"), b.str("auto")})
	b.boxes[int64(i+1)] = r
}

// pointer draws a hand over a node: the page's own cursor:pointer, and the only
// thing that marks a cell of a delegated widget.
func (b *domBuilder) pointer(i int) {
	for k, ni := range b.doc.Layout.NodeIndex {
		if ni == i {
			b.doc.Layout.Styles[k][cursorStyle] = b.str("pointer")
		}
	}
}

// unbox takes a node's box away: display:none, or a control the page hides.
func (b *domBuilder) unbox(i int) {
	delete(b.boxes, int64(i+1))
	for k, ni := range b.doc.Layout.NodeIndex {
		if ni == i {
			b.doc.Layout.Bounds[k] = []float64{0, 0, 0, 0}
		}
	}
}

func (b *domBuilder) clickable(i int) {
	b.doc.Nodes.IsClickable.Index = append(b.doc.Nodes.IsClickable.Index, i)
}

func (b *domBuilder) inputValue(i int, v string) {
	b.doc.Nodes.InputValue.Index = append(b.doc.Nodes.InputValue.Index, i)
	b.doc.Nodes.InputValue.Value = append(b.doc.Nodes.InputValue.Value, b.str(v))
}

func (b *domBuilder) build() (*cdp.Snapshot, map[int64]rect) {
	snap := b.snap
	snap.Documents = []cdp.DocumentSnapshot{b.doc}
	return &snap, b.boxes
}

// found indexes the result of domInteractables by backend node id.
func found(elems []domElem) map[int64]domElem {
	out := make(map[int64]domElem, len(elems))
	for _, e := range elems {
		out[e.backend] = e
	}
	return out
}

func backendOf(node int) int64 { return int64(node + 1) }

// Every DOM source of a candidate, and everything that is not one.
func TestDOMInteractablesSources(t *testing.T) {
	b := newDOM()
	root := b.el(-1, "html")
	body := b.el(root, "body")

	search := b.el(body, "input", "type", "search", "placeholder", "Search the site")
	text := b.el(body, "input", "type", "text", "id", "dep")
	label := b.el(body, "label", "for", "dep")
	b.text(label, "Departure airport")
	hidden := b.el(body, "input", "type", "hidden", "name", "csrf")
	area := b.el(body, "textarea", "aria-label", "Message")
	sel := b.el(body, "select", "name", "country")
	button := b.el(body, "button", "aria-hidden", "true")
	b.text(button, "  Order   coffee ")
	submit := b.el(body, "input", "type", "submit", "value", "Send it", "disabled", "")
	link := b.el(body, "a", "href", "/other")
	b.text(link, "Other page")
	anchor := b.el(body, "a", "name", "top") // no href: not a link
	editor := b.el(body, "div", "contenteditable", "true", "aria-label", "Story")
	roled := b.el(body, "div", "role", "combobox presentation", "aria-label", "Where from")
	sum := b.el(body, "summary")
	b.text(sum, "More")
	onclick := b.el(body, "div", "onclick", "go()")
	b.text(onclick, "Fake button")
	tabbed := b.el(body, "span", "tabindex", "0", "title", "Close")
	clicked := b.el(body, "li")
	b.clickable(clicked)
	b.text(clicked, "Clickable row")
	plain := b.el(body, "div")
	b.text(plain, "just a paragraph")
	gone := b.el(body, "input", "type", "text", "placeholder", "Not rendered")
	b.unbox(gone)
	tpl := b.el(body, "template")
	inTpl := b.el(tpl, "button")
	b.text(inTpl, "In a template")
	// A checkbox the page hides behind its own label: the label is the control.
	box := b.el(body, "input", "type", "checkbox", "id", "tos")
	b.unbox(box)
	boxLabel := b.el(body, "label", "for", "tos")
	b.text(boxLabel, "I agree")
	b.inputValue(text, "Madrid")

	snap, boxes := b.build()
	elems, order := domInteractables(snap, boxes, pointerCursors(snap))
	got := found(elems)

	for _, c := range []struct {
		node int
		role string
		name string
	}{
		{search, "searchbox", "Search the site"},
		{text, "textbox", "Departure airport"},
		{area, "textbox", "Message"},
		{sel, "combobox", "country"},
		{button, "button", "Order coffee"},
		{submit, "button", "Send it"},
		{link, "link", "Other page"},
		{editor, "textbox", "Story"},
		{roled, "combobox", "Where from"},
		{sum, "disclosuretriangle", "More"},
		{onclick, "button", "Fake button"},
		{tabbed, "button", "Close"},
		{clicked, "button", "Clickable row"},
		{boxLabel, "checkbox", "I agree"},
	} {
		e, ok := got[backendOf(c.node)]
		if !ok {
			t.Errorf("node %d (%s %q) is not a candidate", c.node, c.role, c.name)
			continue
		}
		if e.role != c.role || e.name != c.name {
			t.Errorf("node %d = %s %q, want %s %q", c.node, e.role, e.name, c.role, c.name)
		}
	}
	for _, node := range []int{root, body, hidden, anchor, plain, gone, tpl, inTpl, box, label} {
		if e, ok := got[backendOf(node)]; ok {
			t.Errorf("node %d is a candidate (%s %q) and should not be", node, e.role, e.name)
		}
	}
	if e := got[backendOf(submit)]; !e.disabled {
		t.Error("the disabled submit is not marked disabled")
	}
	if e := got[backendOf(text)]; e.value != "Madrid" {
		t.Errorf("input value = %q", e.value)
	}
	// Document order is the node table's order, whatever the candidates are.
	if order[backendOf(search)] >= order[backendOf(link)] {
		t.Error("document order does not follow the node table")
	}
}

// A handler on a wrapper is not a second control, and neither is the icon
// inside a button: only the innermost element worth a click survives.
func TestDOMInteractablesPrunesWrappers(t *testing.T) {
	b := newDOM()
	root := b.el(-1, "html")
	body := b.el(root, "body")
	b.clickable(body) // a delegated listener on the body

	card := b.el(body, "div", "onclick", "open()")
	inner := b.el(card, "button")
	icon := b.el(inner, "span", "tabindex", "0")
	b.text(inner, "Buy")

	outer := b.el(body, "div", "onclick", "a()")
	nested := b.el(outer, "div", "onclick", "b()")
	b.text(nested, "Row")

	snap, boxes := b.build()
	elems, _ := domInteractables(snap, boxes, pointerCursors(snap))
	got := found(elems)

	if _, ok := got[backendOf(inner)]; !ok {
		t.Error("the real button was dropped")
	}
	for _, node := range []int{body, card, icon, outer} {
		if e, ok := got[backendOf(node)]; ok {
			t.Errorf("node %d survived as %s %q", node, e.role, e.name)
		}
	}
	if _, ok := got[backendOf(nested)]; !ok {
		t.Error("the innermost clickable row was dropped")
	}
}

// The states an element publishes about itself in its attributes: the
// accessibility tree reports most of them too, but not on a page that writes
// them on a div the tree calls generic, and never the secret ones.
func TestDOMFlags(t *testing.T) {
	b := newDOM()
	root := b.el(-1, "html")
	body := b.el(root, "body")

	open := b.el(body, "div", "role", "combobox", "aria-label", "Where to",
		"aria-expanded", "true", "aria-haspopup", "listbox")
	shut := b.el(body, "div", "role", "combobox", "aria-label", "Airline", "aria-expanded", "false")
	menu := b.el(body, "button", "aria-haspopup", "menu")
	b.text(menu, "More")
	bad := b.el(body, "input", "type", "text", "aria-label", "Email",
		"aria-invalid", "spelling", "required", "")
	fine := b.el(body, "input", "type", "text", "aria-label", "Name",
		"aria-invalid", "false", "aria-required", "true")
	suggest := b.el(body, "input", "type", "text", "aria-label", "City", "aria-autocomplete", "list")
	datalist := b.el(body, "input", "type", "text", "aria-label", "Where from", "list", "cities")
	pass := b.el(body, "input", "type", "password", "aria-label", "Password")
	b.inputValue(pass, "hunter2")
	card := b.el(body, "input", "type", "text", "aria-label", "Card number",
		"autocomplete", "cc-number", "value", "4111111111111111")
	town := b.el(body, "input", "type", "text", "aria-label", "Town", "value", "Madrid")

	snap, boxes := b.build()
	elems, _ := domInteractables(snap, boxes, pointerCursors(snap))
	got := found(elems)

	if e := got[backendOf(open)]; e.expanded == nil || !*e.expanded || e.hasPopup != "listbox" {
		t.Fatalf("open combobox = %+v", e)
	}
	if e := got[backendOf(shut)]; e.expanded == nil || *e.expanded {
		t.Fatalf("closed combobox = %+v, want an expanded state of false", e)
	}
	if e := got[backendOf(menu)]; e.hasPopup != "menu" || e.expanded != nil {
		t.Fatalf("menu button = %+v, want no expanded state at all", e)
	}
	if e := got[backendOf(bad)]; !e.invalid || !e.required {
		t.Fatalf("rejected field = %+v", e)
	}
	if e := got[backendOf(fine)]; e.invalid || !e.required {
		t.Fatalf(`aria-invalid="false" with aria-required="true" = %+v`, e)
	}
	// One signal for "this box opens a list", whichever way the page says it.
	if e := got[backendOf(suggest)]; e.hasPopup != "listbox" {
		t.Fatalf("aria-autocomplete=list gave %q", e.hasPopup)
	}
	if e := got[backendOf(datalist)]; e.role != "combobox" || e.hasPopup != "listbox" {
		t.Fatalf("<input list> = %+v", e)
	}
	// A secret is never carried, whichever source offered its contents.
	if e := got[backendOf(pass)]; !e.secret || e.value != "" {
		t.Fatalf("password = %+v", e)
	}
	if e := got[backendOf(card)]; !e.secret || e.value != "" {
		t.Fatalf("card number = %+v", e)
	}
	if e := got[backendOf(town)]; e.secret || e.value != "Madrid" {
		t.Fatalf("ordinary field = %+v", e)
	}
}

// An autocomplete attribute carries a section and a billing hint before the
// field name, and the name is what says whether the field is a secret.
func TestSecretAutocomplete(t *testing.T) {
	for _, v := range []string{"cc-number", "section-pay billing cc-csc", "CC-Exp",
		"current-password", "new-password", "one-time-code"} {
		if !secretAutocomplete(v) {
			t.Errorf("autocomplete %q was not read as a secret", v)
		}
	}
	for _, v := range []string{"", "email", "cc-name", "street-address", "off"} {
		if secretAutocomplete(v) {
			t.Errorf("autocomplete %q was read as a secret", v)
		}
	}
}

// A control the page never named: what it is called is drawn on it, or written
// beside it, or written in it. A page that ships icon buttons still says what
// they are, once, where its own reader would look.
func TestUnnamedControlNames(t *testing.T) {
	b := newDOM()
	root := b.el(-1, "html")
	body := b.el(root, "body")

	icon := b.el(body, "button")
	svg := b.el(icon, "svg")
	title := b.el(svg, "title")
	b.text(title, "Abrir calendario")
	picture := b.el(body, "button")
	b.el(picture, "img", "alt", "Close")
	labelled := b.el(body, "button")
	b.el(labelled, "span", "aria-label", "Next month")
	written := b.el(body, "button")
	b.text(written, "  Order   coffee ")
	described := b.el(body, "button", "aria-describedby", "tip")
	tip := b.el(body, "div", "id", "tip")
	b.text(tip, "Opens the menu")
	titled := b.el(body, "button", "title", "Print")
	link := b.el(body, "a", "href", "/next")
	b.text(link, "Next page")
	bare := b.el(body, "button")
	snap, boxes := b.build()

	elems, _ := domInteractables(snap, boxes, pointerCursors(snap))
	got := found(elems)
	for _, c := range []struct {
		node int
		want string
	}{
		{icon, "Abrir calendario"},
		{picture, "Close"},
		{labelled, "Next month"},
		{written, "Order coffee"},
		{described, "Opens the menu"},
		{titled, "Print"},
		{link, "Next page"},
		{bare, ""},
	} {
		if e := got[backendOf(c.node)]; e.name != c.want {
			t.Errorf("name = %q, want %q (%+v)", e.name, c.want, e)
		}
	}
}

// What a page draws a hand over and says nothing else about: the day cells of a
// calendar whose clicks its container handles have no role, no handler of their
// own and no tabindex, and the cursor is all there is to go on. Only the
// innermost one is a candidate, and never one with a real control inside it.
func TestPointerCandidatesAreTheInnermostHandStyledElement(t *testing.T) {
	b := newDOM()
	root := b.el(-1, "html")
	body := b.el(root, "body")
	b.pointer(body) // a page that styles its whole body is not one big button

	cal := b.el(body, "div", "id", "cal") // the container the clicks go to
	day := b.el(cal, "div")
	b.text(day, "26")
	b.pointer(day)

	wrap := b.el(body, "div") // a hand over a card that holds a real link
	b.pointer(wrap)
	link := b.el(wrap, "a", "href", "/other")
	b.text(link, "Other page")
	b.pointer(link)

	outer := b.el(body, "div") // nested hands: the innermost is what is clicked
	b.pointer(outer)
	b.text(outer, "Fares")
	inner := b.el(outer, "div")
	b.text(inner, "Cheapest")
	b.pointer(inner)

	inside := b.el(body, "button") // a hand over the icon inside a button
	b.text(inside, "Search")
	icon := b.el(inside, "span")
	b.text(icon, "→")
	b.pointer(icon)

	list := b.el(body, "ul") // a tag clicks are delegated through
	b.pointer(list)
	b.text(list, "Menu")
	plain := b.el(body, "div") // no hand: not a candidate at all
	b.text(plain, "Ordinary text")
	empty := b.el(body, "div") // a hand over something with no name
	b.pointer(empty)

	snap, boxes := b.build()
	got := found(elemsOf(snap, boxes))

	e, ok := got[backendOf(day)]
	if !ok {
		t.Fatalf("the day cell is not a candidate: %+v", got)
	}
	if e.role != "button" || e.name != "26" || !e.pointer || !e.generic {
		t.Fatalf("the day cell = %+v, want a generic pointer button named 26", e)
	}
	if e := got[backendOf(inner)]; !e.pointer || e.name != "Cheapest" {
		t.Fatalf("the innermost of two nested hands = %+v", e)
	}
	if e, ok := got[backendOf(link)]; !ok || e.role != "link" {
		t.Fatalf("the link inside a hand-styled card = %+v", e)
	}
	for _, c := range []struct {
		node int
		why  string
	}{
		{body, "the body is never a candidate on a cursor"},
		{cal, "the container has no hand of its own"},
		{wrap, "a card that holds a link is not the thing to click"},
		{outer, "the outer of two nested hands"},
		{icon, "the icon inside a real button"},
		{list, "a list clicks are delegated through"},
		{plain, "an element with no hand over it"},
		{empty, "an element with no name"},
	} {
		if e, ok := got[backendOf(c.node)]; ok {
			t.Errorf("%s is a candidate (%+v): %s", c.why, e, c.why)
		}
	}
}

// A hand over a paragraph is a page that made its prose clickable, not a
// control — unless the page named the element itself.
func TestPointerCandidatesNeedAShortName(t *testing.T) {
	const prose = "Todos los trenes con origen y destino en la misma estación de Madrid"
	b := newDOM()
	root := b.el(-1, "html")
	body := b.el(root, "body")

	long := b.el(body, "div")
	b.text(long, prose)
	b.pointer(long)
	labelled := b.el(body, "div", "aria-label", "Cambiar de estación")
	b.text(labelled, prose)
	b.pointer(labelled)
	titled := b.el(body, "div", "title", "Elegir estación")
	b.text(titled, prose)
	b.pointer(titled)
	short := b.el(body, "div")
	b.text(short, "Madrid")
	b.pointer(short)

	snap, boxes := b.build()
	got := found(elemsOf(snap, boxes))

	if e, ok := got[backendOf(long)]; ok {
		t.Errorf("a hand over a whole sentence is a candidate: %+v", e)
	}
	for _, c := range []struct {
		node int
		name string
	}{
		{labelled, "Cambiar de estación"},
		{titled, "Elegir estación"},
		{short, "Madrid"},
	} {
		e, ok := got[backendOf(c.node)]
		if !ok || !e.pointer {
			t.Errorf("node %d (%q) is not a pointer candidate: %+v", c.node, c.name, e)
			continue
		}
		if e.name != c.name {
			t.Errorf("node %d = %q, want %q", c.node, e.name, c.name)
		}
	}
}

// elemsOf is the DOM's own candidates, cursors and all.
func elemsOf(snap *cdp.Snapshot, boxes map[int64]rect) []domElem {
	elems, _ := domInteractables(snap, boxes, pointerCursors(snap))
	return elems
}
