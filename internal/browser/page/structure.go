package page

import (
	"slices"
	"sort"
	"strings"

	"holaclaw/internal/browser/cdp"
	"holaclaw/internal/browser/proto"
)

// The shape of a page, beside the flat list of candidates (docs/browser.md): who contains
// whom, what points at what, and which dialogs are open. A shortlist of roles
// and names says what a page holds but not how it is put together, so an
// option arrives with no sign of the box it came out of and the buttons of a
// cookie wall compete with the controls behind it that nobody can reach.
//
// All of it is read out of the DOMSnapshot and the accessibility tree the
// observation already asked for: no extra CDP call, one pass of maps per
// frame. Every name produced here is page content and travels in the
// Observation, nowhere else.

// containerRoles are the roles that hold other controls. Candidate.In names
// the nearest of them an element belongs to. A group is one only when it has a
// name: an unnamed group is a wrapper, not a thing a reader can point at.
var containerRoles = map[string]bool{
	"listbox":     true,
	"menu":        true,
	"menubar":     true,
	"tree":        true,
	"grid":        true,
	"dialog":      true,
	"alertdialog": true,
	"tablist":     true,
	"radiogroup":  true,
	"group":       true,
}

// popupRoles are the containers a control opens and then points at through
// aria-controls or aria-owns: the listbox behind a combobox, the menu behind a
// button.
var popupRoles = map[string]bool{
	"listbox": true,
	"menu":    true,
	"tree":    true,
	"grid":    true,
	"dialog":  true,
}

// ancestorLimit bounds a parent walk: a snapshot that points at itself must
// cost a loop, not an observation.
const ancestorLimit = 256

// nodeKey names one node of one frame. Backend node ids are unique within a
// renderer and not across the frames an observation walks, so nothing here is
// keyed on the id alone.
type nodeKey struct {
	session string
	backend int64
}

// dialogNode is one dialog the page has open.
type dialogNode struct {
	pos     int
	backend int64
	name    string
	modal   bool
}

// pageStruct is the structure of one frame: the DOM parent of every node, the
// relations its elements declare, and the dialogs it has open.
type pageStruct struct {
	session string
	parent  map[int64]int64
	// controls is what an element aria-controls or aria-owns, owns only the
	// second: ownership is what makes an option belong to the control that
	// wrote it, while control is enough to tie a popup to its opener.
	controls map[int64][]int64
	owns     map[int64][]int64
	active   map[int64]int64 // aria-activedescendant
	dialogs  []dialogNode
	dlg      map[int64]*dialogNode
}

func newPageStruct(session string) *pageStruct {
	return &pageStruct{
		session:  session,
		parent:   make(map[int64]int64, 256),
		controls: make(map[int64][]int64, 8),
		owns:     make(map[int64][]int64, 8),
		active:   make(map[int64]int64, 8),
		dlg:      make(map[int64]*dialogNode, 4),
	}
}

// addDialog records one dialog, merging what the two sources said about the
// same node: a name from whichever had one, and modal if either saw it.
func (st *pageStruct) addDialog(pos int, backend int64, name string, modal bool) {
	d, ok := st.dlg[backend]
	if !ok {
		st.dlg[backend] = &dialogNode{pos: pos, backend: backend, name: name, modal: modal}
		return
	}
	if d.name == "" {
		d.name = name
	}
	d.modal = d.modal || modal
}

// finish puts the dialogs into document order and drops the ones the page is
// not showing: a dialog with no layout box is markup waiting for its turn.
func (st *pageStruct) finish(boxes map[int64]rect) {
	for _, d := range st.dlg {
		if box, drawn := boxes[d.backend]; !drawn || box.empty() {
			continue
		}
		st.dialogs = append(st.dialogs, *d)
	}
	sort.SliceStable(st.dialogs, func(i, j int) bool { return st.dialogs[i].pos < st.dialogs[j].pos })
}

// within reports whether a node is one of the given roots or sits under one.
func (st *pageStruct) within(node int64, roots map[int64]bool) bool {
	id := node
	for n := 0; n < ancestorLimit; n++ {
		if roots[id] {
			return true
		}
		p, ok := st.parent[id]
		if !ok || p == 0 || p == id {
			return false
		}
		id = p
	}
	return false
}

// structure reads the DOM half: the parent of every node, the relations its
// elements declare in attributes, and the dialogs it marks open.
func (x *domIndex) structure(session string) *pageStruct {
	st := newPageStruct(session)
	for _, d := range x.docs {
		for i := range d.n {
			id := d.doc.Nodes.BackendNodeID[i]
			if p := d.parent(i); p >= 0 && p < d.n {
				if _, dup := st.parent[id]; !dup {
					st.parent[id] = d.doc.Nodes.BackendNodeID[p]
				}
			}
			if d.tags[i] == "" {
				continue
			}
			if v, ok := d.attr(i, "aria-controls"); ok {
				st.controls[id] = append(st.controls[id], d.idrefs(v)...)
			}
			if v, ok := d.attr(i, "aria-owns"); ok {
				ids := d.idrefs(v)
				st.controls[id] = append(st.controls[id], ids...)
				st.owns[id] = append(st.owns[id], ids...)
			}
			if v, ok := d.attr(i, "aria-activedescendant"); ok {
				if ids := d.idrefs(v); len(ids) > 0 && st.active[id] == 0 {
					st.active[id] = ids[0]
				}
			}
			if name, modal, ok := d.dialog(i); ok {
				st.addDialog(x.order[id], id, name, modal)
			}
		}
	}
	return st
}

// readAX adds what the accessibility tree knows of the same structure: a page
// that wires its relations up in script has no attributes to read, and a
// dialog built out of a div is one the tree still calls a dialog.
func (st *pageStruct) readAX(x *domIndex, nodes []cdp.AXNode) {
	for i := range nodes {
		n := &nodes[i]
		if n.Ignored || n.BackendDOMNodeID == 0 {
			continue
		}
		id := n.BackendDOMNodeID
		modal := false
		for k := range n.Properties {
			p := &n.Properties[k]
			switch p.Name {
			case "controls":
				st.controls[id] = append(st.controls[id], x.related(p.Value.IDRefs())...)
			case "owns":
				ids := x.related(p.Value.IDRefs())
				st.controls[id] = append(st.controls[id], ids...)
				st.owns[id] = append(st.owns[id], ids...)
			case "activedescendant":
				if ids := x.related(p.Value.IDRefs()); len(ids) > 0 && st.active[id] == 0 {
					st.active[id] = ids[0]
				}
			case "modal":
				modal = p.Value.Bool()
			}
		}
		switch strings.ToLower(n.Role.Text()) {
		case "dialog", "alertdialog":
			st.addDialog(x.order[id], id, capName(n.Name.Text()), modal)
		}
	}
}

// related turns what a relation property points at into backend node ids: the
// node itself when the tree gave one, else the id attribute resolved in the
// document that holds it.
func (x *domIndex) related(rel []cdp.AXRelatedNode) []int64 {
	var out []int64
	for _, r := range rel {
		if r.BackendDOMNodeID != 0 {
			out = append(out, r.BackendDOMNodeID)
			continue
		}
		for _, d := range x.docs {
			if n, ok := d.byID[r.IDRef]; ok {
				out = append(out, d.doc.Nodes.BackendNodeID[n])
				break
			}
		}
	}
	return out
}

// idrefs resolves a space-separated list of ids in this document: ids are
// unique per document and two frames may well use the same one.
func (d *docReader) idrefs(list string) []int64 {
	var out []int64
	for _, ref := range strings.Fields(list) {
		if n, ok := d.byID[ref]; ok {
			out = append(out, d.doc.Nodes.BackendNodeID[n])
		}
	}
	return out
}

// dialog reports whether an element is a dialog the page has open, and what it
// is called. A <dialog> without its open attribute is closed markup; a div the
// page calls a dialog is one whenever it is laid out, which finish decides.
func (d *docReader) dialog(i int) (name string, modal, ok bool) {
	role := ""
	if v, has := d.attr(i, "role"); has {
		role = firstWord(strings.ToLower(v))
	}
	switch {
	case role == "dialog" || role == "alertdialog":
	case d.tags[i] == "dialog" && role == "":
		if _, open := d.attr(i, "open"); !open {
			return "", false, false
		}
	default:
		return "", false, false
	}
	if v, has := d.attr(i, "aria-modal"); has && strings.EqualFold(strings.TrimSpace(v), "true") {
		modal = true
	}
	return capName(d.dialogName(i)), modal, true
}

// dialogName is what a dialog is called: what the page declared about it and
// never the text inside it — a dialog's contents are a page, not a name.
func (d *docReader) dialogName(i int) string {
	if v, ok := d.attr(i, "aria-label"); ok && strings.TrimSpace(v) != "" {
		return v
	}
	if v, ok := d.attr(i, "aria-labelledby"); ok {
		if s := d.textOfIDs(v, proto.MaxNameBytes); s != "" {
			return s
		}
	}
	if v, ok := d.attr(i, "title"); ok && strings.TrimSpace(v) != "" {
		return v
	}
	return ""
}

// dialogsOf is what the whole page has open, in document order, said once
// each and capped like the alerts are. The modal is the last one that claims
// to be one: a page that stacks dialogs is asking about the top of the stack.
func dialogsOf(structs []*pageStruct) (names []string, modal string, at nodeKey) {
	for _, st := range structs {
		for _, d := range st.dialogs {
			name := d.name
			if name == "" {
				name = "dialog" // open, and the page never said what it is
			}
			if len(names) < proto.MaxAlerts && !slices.Contains(names, name) {
				names = append(names, name)
			}
			if d.modal {
				modal, at = name, nodeKey{st.session, d.backend}
			}
		}
	}
	return names, modal, at
}

// inModal is the candidates a modal dialog leaves reachable: its own
// descendants and whatever it owns. Everything behind a modal is there to be
// read and cannot be clicked, so ranking a page's whole supply of controls
// against a step would hand the executor moves the browser will not perform.
// Total is counted before this and still reports the page.
func inModal(all []candidate, st *pageStruct, root int64) []candidate {
	if st == nil {
		return all
	}
	roots := map[int64]bool{root: true}
	for _, id := range st.owns[root] {
		roots[id] = true
	}
	out := make([]candidate, 0, len(all))
	for i := range all {
		c := &all[i]
		if c.ref.session != st.session || c.ref.backend == root {
			continue
		}
		if st.within(c.ref.backend, roots) {
			out = append(out, *c)
		}
	}
	return out
}

// inIndex is what Candidate.In is decided from: the shortlist, where each of
// its entries sits in the page, and the relations that point at them.
type inIndex struct {
	out        []proto.Candidate
	structs    map[string]*pageStruct
	node       map[string]nodeKey // ref → the node it names
	onList     map[nodeKey]int    // node → its place in out
	controlled map[nodeKey]string // node → the shortlisted element that controls it
	owned      map[nodeKey]string // node → the shortlisted element that owns it
	active     map[nodeKey]string // node → the shortlisted element pointing at it
}

// setIn fills Candidate.In on a shortlist that has already been chosen and
// named. It runs last on purpose: a ref that did not make the list is no use
// to a reader, so an option whose listbox was cut names the combobox the
// listbox hangs off instead of naming nothing.
func setIn(out []proto.Candidate, all []candidate, structs map[string]*pageStruct) {
	if len(out) == 0 {
		return
	}
	x := &inIndex{
		out:        out,
		structs:    structs,
		node:       make(map[string]nodeKey, len(all)),
		onList:     make(map[nodeKey]int, len(out)),
		controlled: make(map[nodeKey]string, len(out)),
		owned:      make(map[nodeKey]string, len(out)),
		active:     make(map[nodeKey]string, len(out)),
	}
	for i := range all {
		x.node[all[i].c.Ref] = nodeKey{all[i].ref.session, all[i].ref.backend}
	}
	for i := range out {
		k, ok := x.node[out[i].Ref]
		if !ok {
			continue
		}
		if _, dup := x.onList[k]; !dup {
			x.onList[k] = i
		}
	}
	// The relations, reversed and kept only where the pointing element made
	// the shortlist. The walk is over out rather than over the map, so a page
	// with two controls over one popup answers the same way every time.
	for i := range out {
		k, ok := x.node[out[i].Ref]
		if !ok {
			continue
		}
		st := structs[k.session]
		if st == nil {
			continue
		}
		x.point(x.controlled, k.session, st.controls[k.backend], out[i].Ref)
		x.point(x.owned, k.session, st.owns[k.backend], out[i].Ref)
		if t := st.active[k.backend]; t != 0 {
			x.point(x.active, k.session, []int64{t}, out[i].Ref)
		}
	}
	for i := range out {
		out[i].In = x.of(i)
	}
}

// point records that ref is what the given nodes hang off, first claim wins.
func (x *inIndex) point(m map[nodeKey]string, session string, ids []int64, ref string) {
	for _, id := range ids {
		k := nodeKey{session, id}
		if _, dup := m[k]; !dup {
			m[k] = ref
		}
	}
}

// of is the element one candidate belongs to, "" for one at the top of the
// page. The order of the rules is what makes the answer readable: the box an
// element sits in first, then the control that opened that box, then the
// control that hands an option out without containing it, then whatever else
// it happens to sit inside.
func (x *inIndex) of(i int) string {
	c := &x.out[i]
	k, ok := x.node[c.Ref]
	if !ok {
		return ""
	}
	st := x.structs[k.session]
	if st == nil {
		return ""
	}
	if ref := x.ancestor(st, k, true); ref != "" {
		return ref
	}
	if popupRoles[c.Role] {
		if ref := x.controlled[k]; ref != "" && ref != c.Ref {
			return ref
		}
	}
	if c.Role == "option" {
		if ref := x.owned[k]; ref != "" && ref != c.Ref {
			return ref
		}
		if ref := x.active[k]; ref != "" && ref != c.Ref {
			return ref
		}
	}
	return x.ancestor(st, k, false)
}

// ancestor walks the DOM parents for the nearest node that made the shortlist.
// containers narrows that to the roles that hold other controls, so an option
// names its listbox rather than the first button above it; either way an
// ancestor that is not on the list — a dialog, which is nothing to act on — is
// walked straight past.
func (x *inIndex) ancestor(st *pageStruct, k nodeKey, containers bool) string {
	id := k.backend
	for n := 0; n < ancestorLimit; n++ {
		p, ok := st.parent[id]
		if !ok || p == 0 || p == id {
			return ""
		}
		id = p
		j, on := x.onList[nodeKey{k.session, id}]
		if !on {
			continue
		}
		if !containers {
			return x.out[j].Ref
		}
		if containerRoles[x.out[j].Role] && (x.out[j].Role != "group" || x.out[j].Name != "") {
			return x.out[j].Ref
		}
	}
	return ""
}
