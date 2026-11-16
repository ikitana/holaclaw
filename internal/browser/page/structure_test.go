package page

import (
	"testing"

	"holaclaw/internal/browser/proto"
)

// observeOf is one whole observation over a fake browser: the collection, the
// refs, and the assembly that the shortlist, Candidate.In and the dialogs come
// out of. Nothing here starts a browser or a session.
func observeOf(t *testing.T, nodes []map[string]any, snap any, req proto.ObserveRequest) proto.Observation {
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
	all, _, _, st, err := at.collect(t.Context(), "S1", 0, 0)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	nameRefs(all, 1)
	var obs proto.Observation
	assemble(&obs, all, []*pageStruct{st}, req)
	return obs
}

// listByName indexes a shortlist by accessible name.
func listByName(list []proto.Candidate) map[string]proto.Candidate {
	out := make(map[string]proto.Candidate, len(list))
	for _, c := range list {
		out[c.Name] = c
	}
	return out
}

// What can be typed into and what cannot: the flag describes the element, so a
// div the page calls a combobox is a button that opens a list and a native
// select is answered with the keyboard but never written into.
func TestEditableIsWhatTakesCharacters(t *testing.T) {
	b := newDOM()
	root := b.el(-1, "html")
	body := b.el(root, "body")
	b.el(body, "input", "type", "text", "aria-label", "Departure")
	b.el(body, "input", "type", "checkbox", "aria-label", "Direct only")
	b.el(body, "textarea", "aria-label", "Notes")
	b.el(body, "div", "contenteditable", "", "aria-label", "Story")
	b.el(body, "div", "role", "combobox", "aria-label", "Round trip")
	settable := b.el(body, "div", "role", "combobox", "aria-label", "Where from")
	native := b.el(body, "select", "aria-label", "Country")
	axOnly := b.el(body, "div", "role", "textbox", "aria-label", "Rich text")
	snap, _ := b.build()

	// The tree calls the native select settable too, and it would be wrong.
	obs := observeOf(t, []map[string]any{
		axProp(axNode("combobox", "Where from", backendOf(settable), nil), "settable", true),
		axProp(axNode("combobox", "Country", backendOf(native), nil), "settable", true),
		axProp(axNode("textbox", "Rich text", backendOf(axOnly), nil), "editable", "richtext"),
	}, snap, proto.ObserveRequest{})

	got := listByName(obs.Candidates)
	for _, name := range []string{"Departure", "Notes", "Story", "Where from", "Rich text"} {
		if c, ok := got[name]; !ok || !c.Editable {
			t.Errorf("%q: editable = %v (found %v), want true", name, c.Editable, ok)
		}
	}
	for _, name := range []string{"Direct only", "Round trip", "Country"} {
		if c, ok := got[name]; !ok || c.Editable {
			t.Errorf("%q: editable = %v (found %v), want false", name, c.Editable, ok)
		}
	}
}

// The chain a person reads off a page that has just opened a list: the options
// belong to the listbox, the listbox to the combobox that controls it, and the
// combobox to nothing.
func TestInNamesTheListboxAndItsCombobox(t *testing.T) {
	b := newDOM()
	root := b.el(-1, "html")
	body := b.el(root, "body")
	b.el(body, "div", "role", "combobox", "aria-controls", "tt", "aria-label", "Trip type")
	list := b.el(body, "ul", "role", "listbox", "id", "tt", "aria-label", "Ticket type")
	one := b.el(list, "li", "role", "option")
	b.text(one, "Round trip")
	two := b.el(list, "li", "role", "option")
	b.text(two, "One way")
	snap, _ := b.build()

	obs := observeOf(t, nil, snap, proto.ObserveRequest{})
	got := listByName(obs.Candidates)
	combo, listbox := got["Trip type"], got["Ticket type"]
	if combo.Ref == "" || listbox.Ref == "" {
		t.Fatalf("shortlist = %+v", obs.Candidates)
	}
	if listbox.In != combo.Ref {
		t.Errorf("listbox.In = %q, want the combobox %q", listbox.In, combo.Ref)
	}
	for _, c := range obs.Candidates {
		if c.Role == "option" && c.In != listbox.Ref {
			t.Errorf("option %q is in %q, want the listbox %q", c.Name, c.In, listbox.Ref)
		}
	}
	if combo.In != "" {
		t.Errorf("the combobox is at the top of the page but is in %q", combo.In)
	}
}

// A menu item belongs to the menu it sits in, by ancestry alone: no page ever
// wires an aria-controls between them.
func TestInNamesTheMenuAMenuItemSitsIn(t *testing.T) {
	b := newDOM()
	root := b.el(-1, "html")
	body := b.el(root, "body")
	open := b.el(body, "button", "aria-haspopup", "menu")
	b.text(open, "Actions")
	menu := b.el(body, "div", "role", "menu", "aria-label", "Actions menu")
	item := b.el(menu, "div", "role", "menuitem")
	b.text(item, "Rename")
	snap, _ := b.build()

	obs := observeOf(t, nil, snap, proto.ObserveRequest{})
	got := listByName(obs.Candidates)
	if got["Rename"].In != got["Actions menu"].Ref || got["Actions menu"].Ref == "" {
		t.Fatalf("menu item = %+v, menu = %+v", got["Rename"], got["Actions menu"])
	}
	if got["Actions menu"].In != "" {
		t.Errorf("the menu is at the top of the page but is in %q", got["Actions menu"].In)
	}
}

// A ref nobody was given is no use to a reader: when the listbox an option sits
// in was cut from the shortlist, the option names the combobox that owns it
// instead, and no In ever names an element that did not make the list.
func TestInFallsThroughToTheOwnerWhenTheListboxIsCut(t *testing.T) {
	b := newDOM()
	root := b.el(-1, "html")
	body := b.el(root, "body")
	b.el(body, "input", "type", "text", "role", "combobox", "aria-owns", "opt1", "aria-label", "Where from")
	list := b.el(body, "ul", "role", "listbox", "aria-label", "Suggestions")
	opt := b.el(list, "li", "role", "option", "id", "opt1")
	b.text(opt, "Seville")
	snap, _ := b.build()

	// Two slots and a hint the option alone matches: the listbox is cut.
	obs := observeOf(t, nil, snap, proto.ObserveRequest{Hints: []string{"seville"}, MaxCandidates: 2})
	got := listByName(obs.Candidates)
	if _, cut := got["Suggestions"]; cut {
		t.Fatalf("the listbox was meant to be cut: %+v", obs.Candidates)
	}
	if got["Seville"].In != got["Where from"].Ref || got["Where from"].Ref == "" {
		t.Fatalf("option = %+v, combobox = %+v", got["Seville"], got["Where from"])
	}
	refs := make(map[string]bool, len(obs.Candidates))
	for _, c := range obs.Candidates {
		refs[c.Ref] = true
	}
	for _, c := range obs.Candidates {
		if c.In != "" && !refs[c.In] {
			t.Errorf("candidate %q is in %q, which is not on the shortlist", c.Ref, c.In)
		}
	}
}

// What the page has open, by name: a dialog it never named is "dialog", and one
// with no layout box is markup waiting for its turn.
func TestDialogsAreTheOpenOnes(t *testing.T) {
	b := newDOM()
	root := b.el(-1, "html")
	body := b.el(root, "body")
	picker := b.el(body, "div", "role", "dialog", "aria-label", "Choose date")
	day := b.el(picker, "button")
	b.text(day, "14")
	plain := b.el(body, "div", "role", "dialog")
	b.el(plain, "button", "aria-label", "Close")
	later := b.el(body, "div", "role", "dialog", "aria-label", "Not yet")
	b.unbox(later)
	b.el(body, "dialog", "aria-label", "Never opened") // no open attribute
	b.el(body, "input", "type", "search", "aria-label", "Search")
	snap, _ := b.build()

	obs := observeOf(t, nil, snap, proto.ObserveRequest{})
	want := []string{"Choose date", "dialog"}
	if len(obs.Dialogs) != len(want) {
		t.Fatalf("dialogs = %q, want %q", obs.Dialogs, want)
	}
	for i, s := range want {
		if obs.Dialogs[i] != s {
			t.Fatalf("dialogs = %q, want %q", obs.Dialogs, want)
		}
	}
	if obs.Modal != "" {
		t.Errorf("nothing here claims to be modal, but modal = %q", obs.Modal)
	}
	// A dialog is not something to act on: the buttons in it are.
	if len(obs.Candidates) != 3 {
		t.Errorf("shortlist = %+v, want the two buttons and the search box", obs.Candidates)
	}
}

// A modal is the page: the shortlist holds its own controls and nothing else,
// while Total still counts everything the page has.
func TestModalRestrictsTheShortlistButNotTotal(t *testing.T) {
	b := newDOM()
	root := b.el(-1, "html")
	body := b.el(root, "body")
	b.el(body, "input", "type", "search", "aria-label", "Search")
	b.el(body, "a", "href", "/help")
	wall := b.el(body, "div", "role", "dialog", "aria-modal", "true", "aria-label", "Cookies")
	accept := b.el(wall, "button")
	b.text(accept, "Accept")
	reject := b.el(wall, "button")
	b.text(reject, "Reject")
	_, _ = accept, reject
	snap, _ := b.build()

	obs := observeOf(t, nil, snap, proto.ObserveRequest{})
	if obs.Modal != "Cookies" {
		t.Fatalf("modal = %q, want Cookies", obs.Modal)
	}
	if obs.Total != 4 {
		t.Errorf("total = %d, want the whole page's 4", obs.Total)
	}
	if len(obs.Candidates) != 2 {
		t.Fatalf("shortlist = %+v, want the modal's two buttons", obs.Candidates)
	}
	got := listByName(obs.Candidates)
	if _, ok := got["Accept"]; !ok {
		t.Errorf("shortlist = %+v, want Accept", obs.Candidates)
	}
	if _, ok := got["Reject"]; !ok {
		t.Errorf("shortlist = %+v, want Reject", obs.Candidates)
	}
}

// A cookie banner that never said it was modal is a dialog like any other: its
// buttons compete with the page's, they do not replace it.
func TestNonModalDialogDoesNotRestrict(t *testing.T) {
	b := newDOM()
	root := b.el(-1, "html")
	body := b.el(root, "body")
	b.el(body, "input", "type", "search", "aria-label", "Search")
	banner := b.el(body, "div", "role", "dialog", "aria-label", "Cookies")
	accept := b.el(banner, "button")
	b.text(accept, "Accept")
	snap, _ := b.build()

	obs := observeOf(t, nil, snap, proto.ObserveRequest{})
	if obs.Modal != "" {
		t.Fatalf("modal = %q, want none", obs.Modal)
	}
	if len(obs.Dialogs) != 1 || obs.Dialogs[0] != "Cookies" {
		t.Fatalf("dialogs = %q, want [Cookies]", obs.Dialogs)
	}
	if obs.Total != 2 || len(obs.Candidates) != 2 {
		t.Fatalf("total = %d, shortlist = %+v, want both controls", obs.Total, obs.Candidates)
	}
}
