package page

import (
	"context"
	"encoding/json"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"holaclaw/internal/browser/cdp"
	"holaclaw/internal/browser/proto"
)

// Observation is small by construction (docs/browser.md): the accessibility tree says what
// each element is and is called, DOMSnapshot says where it is and whether it
// can be seen, and the shortlist keeps what a decision model can judge. Every
// string here is page content and goes into the returned struct only.

// interactiveRoles is the closed set of accessibility roles a step can act on.
// Anything else — headings, paragraphs, generic containers — is not a
// candidate, however clickable the page makes it look.
var interactiveRoles = map[string]bool{
	"button":           true,
	"link":             true,
	"textbox":          true,
	"searchbox":        true,
	"combobox":         true,
	"checkbox":         true,
	"radio":            true,
	"menuitem":         true,
	"menuitemcheckbox": true,
	"menuitemradio":    true,
	"tab":              true,
	"option":           true,
	"switch":           true,
	"slider":           true,
	"spinbutton":       true,
	"listbox":          true,
	// A menu is on the list for the same reason a listbox is: it is the thing
	// a menu item came out of, and a reader that cannot see it cannot tell
	// which button opened the items under it.
	"menu":               true,
	"textarea":           true,
	"disclosuretriangle": true,
	"treeitem":           true,
}

// rect is a box in document coordinates.
type rect struct{ x, y, w, h float64 }

func (r rect) empty() bool { return r.w <= 0 || r.h <= 0 }

// intersects reports whether two boxes overlap at all.
func (r rect) intersects(o rect) bool {
	return r.x < o.x+o.w && r.x+r.w > o.x && r.y < o.y+o.h && r.y+r.h > o.y
}

// sameLine reports whether two laid-out text boxes sit on one visual line:
// their vertical ranges overlap and the second does not start back to the
// left of the first. It is what puts a newline between "256 GB" and the
// price under it — without it a page arrives as one unbroken string and the
// extraction's span-cutting, which splits on line breaks, sees one span.
func sameLine(prev, cur rect) bool {
	if prev.empty() || cur.empty() {
		return false
	}
	if cur.y >= prev.y+prev.h || prev.y >= cur.y+cur.h {
		return false // no vertical overlap: a new line
	}
	return cur.x+lineWrapSlack >= prev.x
}

// lineWrapSlack is how far left of the previous box a box may start and still
// count as the same line: glyph boxes of one run are not exactly aligned, and
// a real wrap goes back to the margin, not by a pixel.
const lineWrapSlack = 2.0

// Observe reports the active tab: URL, title, loading, a shortlist of
// candidates with opaque refs, and optionally the visible text. What it
// answered is also recorded as an Activity for whoever is watching the live
// view (activity.go).
func (e *Engine) Observe(ctx context.Context, agent string, req proto.ObserveRequest) (proto.Observation, error) {
	start := time.Now()
	s, err := e.session(agent)
	if err != nil {
		return proto.Observation{}, err
	}
	obs, err := e.observe(ctx, s, req)
	s.record(observeActivity(start, req, obs, err))
	return obs, err
}

// observe is Observe on a session that exists.
func (e *Engine) observe(ctx context.Context, s *session, req proto.ObserveRequest) (proto.Observation, error) {
	s.act.Lock()
	defer s.act.Unlock()

	at, release, err := s.ensure(ctx)
	if err != nil {
		return proto.Observation{}, err
	}
	defer release()

	t, err := at.activeTab()
	if err != nil {
		return proto.Observation{}, err
	}
	// A bot wall or Chromium's own error page is recognised before the
	// observation, so the executor's blocker classification has the fixed
	// word to work with rather than having to read it off the candidates. A
	// JavaScript dialog blocks the renderer, so nothing is asked of the page
	// while one is open.
	if at.dialogKind() == "" {
		e.checkBotWall(ctx, at, s, t)
	}

	obs := proto.Observation{Dialog: at.dialogKind()}
	if info, err := at.bs.GetTargetInfo(ctx, t.targetID); err == nil {
		obs.URL, obs.Title = info.URL, info.Title
	} else {
		at.mu.Lock()
		obs.URL, obs.Title = t.url, t.title
		at.mu.Unlock()
	}
	at.mu.Lock()
	obs.Loading = t.loading
	sessions := []string{t.sessionID}
	for sid := range t.frames {
		sessions = append(sessions, sid)
	}
	at.mu.Unlock()

	if obs.Dialog != "" {
		// A modal dialog blocks the renderer: the tree and the snapshot would
		// not answer. The executor is told what is in the way and the next
		// action dismisses it.
		return obs, nil
	}

	// TextAround names an element of the *previous* observation, so it is
	// resolved against the ref map that this one is about to replace.
	var anchorRef elemRef
	haveAnchor := false
	if req.TextAround != "" {
		at.mu.Lock()
		anchorRef, haveAnchor = at.refs[req.TextAround]
		at.mu.Unlock()
	}

	// The text budget is carried down rather than applied at the end: a page
	// of any size would otherwise be assembled in full and then thrown away.
	budget := textBudget(req.TextBytes)
	if budget > 0 && haveAnchor {
		// A window is cut around an element that may sit anywhere on the page,
		// so the walk cannot stop at the size of the excerpt itself. It stops
		// at a bound of its own instead; past that the anchor is not found and
		// the text is the beginning of the page, as it is without one.
		budget = anchorScan
	}

	var all []candidate
	var structs []*pageStruct
	var text strings.Builder
	var said []alert
	anchorAt, cutShort := -1, false
	for fi, sid := range sessions {
		want := budget
		if want > 0 && text.Len() >= budget {
			want = 0 // the budget is spent; this frame's text is not collected
		}
		var anchorNode int64
		if haveAnchor && anchorRef.session == sid {
			anchorNode = anchorRef.backend
		}
		part, txt, says, st, err := at.collect(ctx, sid, want, anchorNode)
		if err != nil {
			if sid == t.sessionID {
				return proto.Observation{}, err
			}
			continue // a frame that went away mid-observation
		}
		for i := range part {
			part[i].ref.tab = t.targetID
		}
		all = append(all, part...)
		structs = append(structs, st)
		// Which frame an announcement came from and which element said it: the
		// first keeps the frames in the order they were read, the second is what
		// tells a region that has changed its text from one that has been saying
		// the same thing all along.
		for i := range says {
			says[i].session, says[i].doc = sid, fi
		}
		said = append(said, says...)
		cutShort = cutShort || txt.more
		if want > 0 && txt.text != "" {
			if text.Len() > 0 {
				// A frame is its own document, never a continuation of the
				// line the previous one ended on.
				text.WriteString("\n")
			}
			if txt.anchor >= 0 && anchorAt < 0 {
				anchorAt = text.Len() + txt.anchor
			}
			text.WriteString(txt.text)
		}
	}

	// Refs are handed out in document order over every candidate, so the
	// seventh interactive element of the page is the seventh whether or not it
	// is shortlisted — and every ref carries the number of the observation
	// that minted it.
	//
	// The generation is what makes a stale ref stale. Without it the names are
	// positional and the map is replaced wholesale, so "r9" exists in the next
	// observation too and resolves to whatever is ninth *now*: open a select
	// and twenty options enter document order, and a queued move meant for the
	// search button silently clicks an option instead. A ref that names an
	// observation that has been superseded cannot be found at all, which is
	// the error the executor already knows how to recover from.
	at.mu.Lock()
	at.gen++
	gen := at.gen
	at.mu.Unlock()

	// What this observation has that the previous one of the same tab did not:
	// the elements the last move put on the screen, and the announcements the
	// page was not already making. Both are decided before the shortlist, which
	// ranks a new element first when it was given nothing to look for.
	all, alerts := at.remember(t.targetID, obs.URL, all, said)

	refs, desc := nameRefs(all, gen)
	at.mu.Lock()
	at.refs = refs
	at.refDesc = desc
	at.mu.Unlock()

	assemble(&obs, all, structs, req)
	obs.Alerts = alerts
	if req.TextBytes > 0 {
		if anchorAt >= 0 {
			obs.Text, obs.Truncated = windowText(text.String(), anchorAt, req.TextBytes)
		} else {
			obs.Text, obs.Truncated = capText(text.String(), req.TextBytes)
		}
		obs.Truncated = obs.Truncated || cutShort
	}
	return obs, nil
}

// seen is what the previous observation of a tab found: the nodes of its whole
// pool — the shortlist is a cut, and an element that did not make it is not
// news when it comes back — and what each of its announcements said. Both are
// dropped on a navigation: on a new page everything is new and nothing is.
type seen struct {
	tab  string
	url  string
	pool map[nodeKey]bool
	said map[nodeKey]string
	// pointer is the pointer-styled candidates this observation let through, so
	// the next one keeps them: a calendar is new the moment it opens and never
	// again, and a day cell that vanished from the shortlist on the very next
	// observation would be a widget nobody can finish using.
	pointer map[nodeKey]bool
}

// maxPointerCandidates bounds what a cursor alone may add to one observation: a
// page that styles half of itself as clickable costs a bounded number of
// candidates, not a shortlist made of wrappers.
const maxPointerCandidates = 40

// remember compares this observation with the previous one of the same tab and
// then replaces it: it marks the candidates that were not there before, and
// answers with what the page is announcing now as opposed to what it was
// announcing already. A tab observed for the first time, and a page that has
// navigated since, have nothing to compare against: nothing is new and
// everything the page says is said. It also answers with the candidates that
// survive the pointer gate — the rest never reach a ref or the Total.
func (a *attachment) remember(tab, url string, all []candidate, said []alert) ([]candidate, []string) {
	pool := make(map[nodeKey]bool, len(all))
	for i := range all {
		pool[nodeKey{all[i].ref.session, all[i].ref.backend}] = true
	}
	a.mu.Lock()
	prev := a.prev
	a.mu.Unlock()

	var before map[nodeKey]string
	same := prev.pool != nil && prev.tab == tab && prev.url == url
	if same {
		for i := range all {
			all[i].c.New = !prev.pool[nodeKey{all[i].ref.session, all[i].ref.backend}]
		}
		before = prev.said
	}
	kept, admitted := keepPointers(all, prev, same)
	texts, now := announce(said, before)

	a.mu.Lock()
	a.prev = seen{tab: tab, url: url, pool: pool, said: now, pointer: admitted}
	a.mu.Unlock()
	return kept, texts
}

// keepPointers is the gate a candidate that is one on its cursor alone has to
// pass: it is kept when the last move put it there — the calendar that has just
// opened — or when it was kept before and is still on the page. On the first
// observation of a tab, and after a navigation, a page styles as much as it
// likes and none of it is a candidate: what makes a pointer-styled div worth
// reporting is that it arrived, not that it exists. It answers with the
// candidates to keep, in document order, and with what it admitted, for the
// next observation to go on.
func keepPointers(all []candidate, prev seen, same bool) ([]candidate, map[nodeKey]bool) {
	admitted := make(map[nodeKey]bool, 8)
	out := make([]candidate, 0, len(all))
	for i := range all {
		if !all[i].pointer {
			out = append(out, all[i])
			continue
		}
		if !same || len(admitted) >= maxPointerCandidates {
			continue
		}
		k := nodeKey{all[i].ref.session, all[i].ref.backend}
		if !all[i].c.New && !prev.pointer[k] {
			continue
		}
		admitted[k] = true
		out = append(out, all[i])
	}
	return out, admitted
}

// announce puts the collected announcements into the order a person would read
// them in — frame by frame, document order inside each — and keeps the ones
// worth telling the executor about: what the page raised (role=alert, the
// message a rejected field points at) every time, and a live region or a status
// only when its text is not what the same element said last time, so a cookie
// wall that is a live region is reported once and not on every observation
// after it. It also returns what every element said, reported or not, for the
// next observation to compare against. Nothing is said twice; the cap is
// MaxAlerts.
func announce(found []alert, before map[nodeKey]string) ([]string, map[nodeKey]string) {
	sort.SliceStable(found, func(i, j int) bool {
		if found[i].doc != found[j].doc {
			return found[i].doc < found[j].doc
		}
		return found[i].pos < found[j].pos
	})
	out := make([]string, 0, len(found))
	now := make(map[nodeKey]string, len(found))
	for _, f := range found {
		if f.text == "" {
			continue
		}
		k := nodeKey{f.session, f.backend}
		repeat := !f.always && before[k] == f.text
		if _, dup := now[k]; !dup {
			now[k] = f.text
		}
		if repeat || len(out) >= proto.MaxAlerts || slices.Contains(out, f.text) {
			continue
		}
		out = append(out, f.text)
	}
	return out, now
}

// nameRefs hands every candidate the ref it is known by until the next
// observation, and returns the two maps a later action resolves one through:
// the element itself, and how the observation described it.
func nameRefs(all []candidate, gen uint64) (map[string]elemRef, map[string]string) {
	refs := make(map[string]elemRef, len(all))
	desc := make(map[string]string, len(all))
	for i := range all {
		all[i].c.Ref = refName(gen, i+1)
		all[i].order = i
		refs[all[i].c.Ref] = all[i].ref
		desc[all[i].c.Ref] = all[i].c.Role + " " + strconv.Quote(all[i].c.Name)
	}
	return refs, desc
}

// assemble is the half of an observation that can only be decided once every
// frame has been read and every candidate named: what the page has open, which
// candidates a modal leaves reachable, the shortlist itself, and what each of
// its entries belongs to. Total is the page, always, whatever the modal cut.
func assemble(obs *proto.Observation, all []candidate, structs []*pageStruct, req proto.ObserveRequest) {
	obs.Total = len(all)
	byID := make(map[string]*pageStruct, len(structs))
	for _, st := range structs {
		byID[st.session] = st
	}
	var at nodeKey
	obs.Dialogs, obs.Modal, at = dialogsOf(structs)
	pool := all
	if obs.Modal != "" {
		pool = inModal(all, byID[at.session], at.backend)
	}
	obs.Candidates = shortlist(pool, req.Hints, req.MaxCandidates)
	setIn(obs.Candidates, all, byID)
}

// refName is one opaque handle: which observation minted it, and where in
// document order the element was. Short, because every candidate of every
// observation carries one into a model's state.
func refName(gen uint64, i int) string {
	return "g" + strconv.FormatUint(gen, 10) + "r" + strconv.Itoa(i)
}

// snapshotStyles are the computed styles every snapshot is taken with: what
// the page draws and whether a hand is shown over it. The values come back per
// laid-out node in this order, so cursorStyle is where the cursor sits.
var snapshotStyles = []string{"display", "visibility", "cursor"}

const cursorStyle = 2

// pointerCursors reads back the one style the candidates need: the laid-out
// nodes the page shows a hand over, per document, by node index. Only pointer
// is kept — the map would otherwise hold every node of the page to say
// "auto" — and it rides on the snapshot the observation already took.
func pointerCursors(snap *cdp.Snapshot) []map[int]string {
	out := make([]map[int]string, len(snap.Documents))
	for di := range snap.Documents {
		doc := &snap.Documents[di]
		m := make(map[int]string, 16)
		for li, ni := range doc.Layout.NodeIndex {
			if li >= len(doc.Layout.Styles) {
				break
			}
			st := doc.Layout.Styles[li]
			if cursorStyle >= len(st) {
				continue
			}
			if v := snap.Str(st[cursorStyle]); v == "pointer" {
				m[ni] = v
			}
		}
		out[di] = m
	}
	return out
}

// candidate is one interactive element on its way to the shortlist.
type candidate struct {
	c     proto.Candidate
	ref   elemRef
	order int
	// pointer marks an element that is a candidate on its cursor alone
	// (domcands.go): one of those is kept only when the last move put it on
	// the screen, which is decided in remember.
	pointer bool
	// pos is the element's place in the snapshot's node table: the two sources
	// are merged and then put back into document order by it.
	pos   int
	score int
}

// collect reads one target: its accessibility tree joined with its snapshot,
// and the snapshot's own interactables merged in. budget is how many bytes of
// page text to gather, 0 for none; anchor is the backend node id the text is
// to be cut around, 0 when the text is read from the top. It also returns what
// the page is announcing right now.
func (a *attachment) collect(ctx context.Context, sessionID string, budget int, anchor int64) ([]candidate, snapText, []alert, *pageStruct, error) {
	sess := a.conn.Session(sessionID)
	// Measured on Chromium 153 (cdp): getFullAXTree answers without
	// Accessibility.enable, so the observation enables no domain of its own.
	nodes, err := sess.GetFullAXTree(ctx, 0, "")
	if err != nil {
		return nil, snapText{}, nil, nil, browserErr(ctx, err, "axtree")
	}
	// Measured on Chromium 153: the tree of a page stops at its iframe
	// elements, so each frame of the tree is asked for its own. An
	// out-of-process frame is a target of its own and is collected separately.
	if tree, err := sess.GetFrameTree(ctx); err == nil {
		for _, id := range childFrameIDs(tree) {
			more, err := sess.GetFullAXTree(ctx, 0, id)
			if err != nil {
				continue
			}
			nodes = append(nodes, more...)
		}
	}
	snap, err := sess.CaptureSnapshot(ctx, cdp.SnapshotOptions{ComputedStyles: snapshotStyles})
	if err != nil {
		return nil, snapText{}, nil, nil, browserErr(ctx, err, "snapshot")
	}
	view := viewport(ctx, a.conn, sessionID)
	boxes, values, text := readSnapshot(snap, budget, anchor)
	idx := scanDOM(snap, boxes, pointerCursors(snap))
	// The same two reads, taken apart a second time: how the frame is put
	// together, so a candidate can say which box it came out of and a modal
	// dialog can say which candidates it leaves reachable.
	st := idx.structure(sessionID)
	st.readAX(idx, nodes)
	st.finish(boxes)
	doms, order := idx.elems, idx.order
	byNode := make(map[int64]*domElem, len(doms))
	for i := range doms {
		if _, dup := byNode[doms[i].backend]; !dup {
			byNode[doms[i].backend] = &doms[i]
		}
	}

	out := make([]candidate, 0, 32)
	seen := make(map[int64]bool, 32)
	// What the tree said each field's error message and description are, kept
	// by node: only a field the page has rejected is asked for them, and that
	// is not known until the DOM has had its say too.
	rel := make(map[int64][]cdp.AXRelatedNode, 4)
	for i := range nodes {
		n := &nodes[i]
		if n.Ignored || n.BackendDOMNodeID == 0 || seen[n.BackendDOMNodeID] {
			continue
		}
		role := strings.ToLower(n.Role.Text())
		if !interactiveRoles[role] {
			continue
		}
		box, drawn := boxes[n.BackendDOMNodeID]
		if !drawn || box.empty() {
			continue // no layout box: the element is not rendered at all
		}
		dom := byNode[n.BackendDOMNodeID]
		c := proto.Candidate{
			Ref:       "", // assigned once the whole document is walked
			Role:      role,
			Name:      capName(n.Name.Text()),
			Offscreen: !view.empty() && !box.intersects(view),
		}
		// A field the page never named — the common case for a search box —
		// is called what a person reads on it: its label, placeholder or title.
		if c.Name == "" && dom != nil {
			c.Name = dom.name
		}
		// The tree first, the DOM for what it left unsaid: a page that writes
		// aria-expanded on an element the tree reports without a state still
		// means it, and the tree never says which field is a secret.
		r, readonly := axFlags(&c, n)
		if len(r) > 0 {
			rel[n.BackendDOMNodeID] = r
		}
		desc := n.Description.Text()
		if dom != nil {
			mergeFlags(&c, dom)
			if strings.TrimSpace(desc) == "" {
				desc = dom.desc
			}
		}
		if readonly {
			// Chromium marks a readonly input editable — the kind of control
			// is — so the state the page wrote has the last word, after both
			// sources have had theirs. A field nothing can be typed into is
			// still a candidate: it is what is clicked to open the calendar.
			c.Editable = false
		}
		setDescription(&c, desc)
		v := n.Value.Text()
		if v == "" {
			v = values[n.BackendDOMNodeID]
		}
		setValue(&c, v)
		seen[n.BackendDOMNodeID] = true
		out = append(out, candidate{
			c:   c,
			ref: elemRef{session: sessionID, frame: n.FrameID, backend: n.BackendDOMNodeID},
			pos: order[n.BackendDOMNodeID],
		})
	}
	// What the tree did not report: inputs it under-reports, elements the page
	// hid from it, and the clickables that carry a handler and no role.
	for i := range doms {
		d := &doms[i]
		if seen[d.backend] {
			continue
		}
		box, drawn := boxes[d.backend]
		if !drawn || box.empty() {
			continue
		}
		seen[d.backend] = true
		c := proto.Candidate{
			Role:      d.role,
			Name:      d.name,
			Disabled:  d.disabled,
			Offscreen: !view.empty() && !box.intersects(view),
		}
		mergeFlags(&c, d)
		setValue(&c, d.value)
		setDescription(&c, d.desc)
		out = append(out, candidate{
			c:       c,
			ref:     elemRef{session: sessionID, backend: d.backend},
			pos:     order[d.backend],
			pointer: d.pointer,
		})
	}
	// One document order over both sources, so a ref still counts elements
	// down the page and the shortlist's last key means what it says.
	sort.SliceStable(out, func(i, j int) bool { return out[i].pos < out[j].pos })

	// What the page is announcing: its own live regions, plus the message
	// every field it has rejected points at.
	said := idx.alerts(boxes)
	for i := range out {
		if !out[i].c.Invalid {
			continue
		}
		id := out[i].ref.backend
		said = append(said, idx.messages(id, rel[id], boxes)...)
	}
	return out, text, said, st, nil
}

// setValue is the one place a candidate's value is filled: a secret field is
// reported empty, whichever source offered its contents.
func setValue(c *proto.Candidate, v string) {
	if c.Secret {
		c.Value = ""
		return
	}
	c.Value = capName(v)
}

// setDescription is the one place a candidate's description is filled: the
// sentence the page wrote about how the element works. A secret field carries
// none — nothing about what it holds travels — and a description that only
// repeats the name says nothing twice.
func setDescription(c *proto.Candidate, s string) {
	if c.Secret {
		return
	}
	if s = capDescription(s); s != "" && s != c.Name {
		c.Description = s
	}
}

// axFlags reads the states the accessibility tree publishes about an element.
// It returns the nodes its error message and description point at, which are
// read only if the page turns out to have rejected the field, and whether the
// tree says the element is readonly.
func axFlags(c *proto.Candidate, n *cdp.AXNode) (rel []cdp.AXRelatedNode, readonly bool) {
	autocomplete := ""
	settable := false
	for i := range n.Properties {
		p := &n.Properties[i]
		switch p.Name {
		case "focused":
			c.Focused = p.Value.Bool()
		case "disabled":
			c.Disabled = p.Value.Bool()
		case "expanded":
			// Only an element that has the state at all gets one: the pointer
			// is how "closed" is told apart from "not a thing that opens".
			if b, ok := p.Value.Value.(bool); ok {
				c.Expanded = &b
			}
		case "hasPopup":
			if v := strings.ToLower(p.Value.Text()); v != "" && v != "false" {
				c.HasPopup = v
			}
		case "invalid":
			c.Invalid = axTruthy(&p.Value)
		case "required":
			c.Required = p.Value.Bool()
		case "autocomplete":
			autocomplete = p.Value.Text()
		case "editable":
			// A string on most builds ("plaintext", "richtext"), a bool on
			// some: anything but "false" is a node characters go into.
			if b, ok := p.Value.Value.(bool); ok {
				c.Editable = c.Editable || b
			} else if v := strings.ToLower(strings.TrimSpace(p.Value.Text())); v != "" && v != "false" {
				c.Editable = true
			}
		case "settable":
			settable = p.Value.Bool()
		case "readonly":
			// A control whose text cannot be written, whatever the tree makes
			// of the element being an editable kind of thing.
			readonly = axTruthy(&p.Value)
		case "errormessage", "describedby":
			rel = append(rel, p.Value.IDRefs()...)
		}
	}
	// A box that completes what is typed in it opens a list, whatever the page
	// calls it: the executor gets one signal rather than two to join up.
	if c.HasPopup == "" && listAutocomplete(autocomplete) {
		c.HasPopup = "listbox"
	}
	// A value the tree says can be set is a value typed in, but only on the
	// roles that take characters: a checkbox is settable too, and typing into
	// one is nothing.
	if settable && settableRoles[c.Role] {
		c.Editable = true
	}
	return rel, readonly
}

// settableRoles are the roles whose value is a string a person types: the
// tree's settable state means something else on the rest.
var settableRoles = map[string]bool{
	"textbox":    true,
	"searchbox":  true,
	"combobox":   true,
	"spinbutton": true,
}

// axTruthy reads a state the tree publishes as a bool on some builds and as the
// ARIA token itself on others ("true", "spelling", "grammar"): anything but
// false is the state being set.
func axTruthy(v *cdp.AXValue) bool {
	if b, ok := v.Value.(bool); ok {
		return b
	}
	s := strings.ToLower(strings.TrimSpace(v.Text()))
	return s != "" && s != "false"
}

// mergeFlags fills from the DOM what the accessibility tree did not say. The
// tree wins wherever it said anything — it is the page as a person's software
// is told it — except for Secret, which only the DOM knows and which is never
// unset once something has claimed it.
func mergeFlags(c *proto.Candidate, d *domElem) {
	if c.Expanded == nil {
		c.Expanded = d.expanded
	}
	if c.HasPopup == "" {
		c.HasPopup = d.hasPopup
	}
	if !c.Invalid {
		c.Invalid = d.invalid
	}
	if !c.Required {
		c.Required = d.required
	}
	if d.secret {
		c.Secret = true
	}
	if d.editable {
		c.Editable = true
	}
	if d.fixed {
		// What the element is settles it: a native select answers to the
		// keyboard, but nothing is typed into it, and a div the page calls a
		// combobox is a button that opens a list.
		c.Editable = false
	}
}

// childFrameIDs flattens a frame tree, the root excluded.
func childFrameIDs(tree *cdp.FrameTree) []string {
	var out []string
	var walk func(t *cdp.FrameTree)
	walk = func(t *cdp.FrameTree) {
		for i := range t.ChildFrames {
			out = append(out, t.ChildFrames[i].Frame.ID)
			walk(&t.ChildFrames[i])
		}
	}
	walk(tree)
	return out
}

// readSnapshot indexes a snapshot by backend node id: the box of every laid-out
// node, the value of every input, and — when asked for — the visible text.
// Iframe documents are offset by their own frame's box, so one coordinate
// space covers the page.
//
// budget bounds the text in bytes (0 = none): the builder stops once it is
// past it, so a page of any size costs the cap and not the page. The exact cut
// is still capText's, which is why the budget carries a margin.
//
// anchor is a backend node id to remember the place of in the text, 0 for
// none: the excerpt is then cut around that element instead of from the top.
func readSnapshot(snap *cdp.Snapshot, budget int, anchor int64) (map[int64]rect, map[int64]string, snapText) {
	boxes := make(map[int64]rect, 128)
	values := make(map[int64]string, 8)
	var text strings.Builder
	// The box the last piece of text came from, so the next one knows whether
	// it continues that line or begins a new one.
	var prev rect
	var wrote bool
	out := snapText{anchor: -1}

	offsets := documentOffsets(snap)
	for di := range snap.Documents {
		doc := &snap.Documents[di]
		off := offsets[di]
		// Where the anchor sits in this document's node table, -1 when it is
		// not here. The table is a pre-order traversal, so the element's own
		// runs and its descendants' are the ones at or after it.
		from := -1
		if anchor != 0 && out.anchor < 0 {
			for i, id := range doc.Nodes.BackendNodeID {
				if id == anchor {
					from = i
					break
				}
			}
		}
		for li, ni := range doc.Layout.NodeIndex {
			if ni < 0 || ni >= len(doc.Nodes.BackendNodeID) || li >= len(doc.Layout.Bounds) {
				continue
			}
			b := doc.Layout.Bounds[li]
			if len(b) < 4 {
				continue
			}
			r := rect{x: b[0] + off.x, y: b[1] + off.y, w: b[2], h: b[3]}
			id := doc.Nodes.BackendNodeID[ni]
			if old, ok := boxes[id]; !ok || old.empty() {
				boxes[id] = r
			}
			if budget > 0 && li < len(doc.Layout.Text) && !r.empty() {
				if s := strings.TrimSpace(snap.Str(doc.Layout.Text[li])); s != "" {
					if text.Len() >= budget {
						out.more = true // the walk stopped short of the page
						continue
					}
					if wrote {
						if sameLine(prev, r) {
							text.WriteString(" ")
						} else {
							text.WriteString("\n")
						}
					}
					if from >= 0 && ni >= from && out.anchor < 0 {
						out.anchor = text.Len()
					}
					text.WriteString(s)
					prev, wrote = r, true
				}
			}
		}
		for k, ni := range doc.Nodes.InputValue.Index {
			if k >= len(doc.Nodes.InputValue.Value) || ni < 0 || ni >= len(doc.Nodes.BackendNodeID) {
				continue
			}
			values[doc.Nodes.BackendNodeID[ni]] = snap.Str(doc.Nodes.InputValue.Value[k])
		}
	}
	out.text = text.String()
	return boxes, values, out
}

// snapText is the visible text of one target, with where the anchor element's
// own text begins in it (-1 when there is no anchor or it was never reached)
// and whether the walk stopped before the end of the page.
type snapText struct {
	text   string
	anchor int
	more   bool
}

// documentOffsets places every document of a snapshot in the top document's
// coordinates: an iframe's boxes are relative to its own document, so they are
// shifted by the box of the frame element that holds it.
func documentOffsets(snap *cdp.Snapshot) []rect {
	offsets := make([]rect, len(snap.Documents))
	// One pass over the owners: an iframe node points at its document index.
	type owner struct{ doc, node int }
	owners := map[int]owner{}
	for di := range snap.Documents {
		doc := &snap.Documents[di]
		for k, ni := range doc.Nodes.ContentDocumentIndex.Index {
			if k >= len(doc.Nodes.ContentDocumentIndex.Value) {
				continue
			}
			owners[doc.Nodes.ContentDocumentIndex.Value[k]] = owner{doc: di, node: ni}
		}
	}
	// Documents are ordered parent before child, so one forward pass composes
	// nested frames correctly.
	for di := 1; di < len(snap.Documents); di++ {
		o, ok := owners[di]
		if !ok {
			continue
		}
		doc := &snap.Documents[o.doc]
		for li, ni := range doc.Layout.NodeIndex {
			if ni != o.node || li >= len(doc.Layout.Bounds) || len(doc.Layout.Bounds[li]) < 4 {
				continue
			}
			b := doc.Layout.Bounds[li]
			offsets[di] = rect{x: offsets[o.doc].x + b[0], y: offsets[o.doc].y + b[1]}
			break
		}
	}
	return offsets
}

// viewport reads the part of the document that is on screen, in the same
// coordinates DOMSnapshot uses. Measured on Chromium 153: snapshot bounds are
// device pixels (a 180 px button is 188.4 wide at a device pixel ratio of
// 1.047), which is what layoutViewport reports — the css* members are CSS
// pixels and would compare against the wrong scale.
//
// internal/browser/cdp has no typed helper for Page.getLayoutMetrics; the raw
// call goes through the same Conn.Call, so the stealth rule still applies. A
// failure means "no viewport known", and then nothing is marked offscreen.
func viewport(ctx context.Context, conn *cdp.Conn, sessionID string) rect {
	var r map[string]json.RawMessage
	if err := rawCall(ctx, conn, sessionID, "Page.getLayoutMetrics", nil, &r); err != nil {
		return rect{}
	}
	var vp map[string]json.RawMessage
	var raw json.RawMessage
	var ok bool
	for _, key := range []string{"layoutViewport", "cssLayoutViewport", "visualViewport", "cssVisualViewport"} {
		if raw, ok = r[key]; ok {
			break
		}
	}
	if !ok {
		return rect{}
	}
	if err := json.Unmarshal(raw, &vp); err != nil {
		return rect{}
	}
	return rect{
		x: jsonNumber(vp, "pageX"),
		y: jsonNumber(vp, "pageY"),
		w: jsonNumber(vp, "clientWidth"),
		h: jsonNumber(vp, "clientHeight"),
	}
}

// shortlist scores, orders and caps the candidates: hint matches first, then —
// when the hints matched nothing at all — what the last move put on the screen,
// then what can be seen, then the kind of control, and last document order.
func shortlist(all []candidate, hints []string, max int) []proto.Candidate {
	if max <= 0 {
		max = proto.DefaultMaxCandidates
	}
	if max > proto.MaxCandidates {
		max = proto.MaxCandidates
	}
	words := hintWords(hints)
	hinted := false
	for i := range all {
		all[i].score = score(&all[i].c, words)
		if all[i].score > 0 {
			hinted = true
		}
	}
	ranked := make([]candidate, len(all))
	copy(ranked, all)
	sort.SliceStable(ranked, func(i, j int) bool {
		a, b := ranked[i], ranked[j]
		if a.score != b.score {
			return a.score > b.score
		}
		// Nothing was asked for by name, and something is here that was not
		// here before: what the last move opened is what the move was for, and
		// the calendar it opened goes on the list before the page it opened
		// over.
		if !hinted && a.c.New != b.c.New {
			return a.c.New
		}
		if a.c.Offscreen != b.c.Offscreen {
			return !a.c.Offscreen
		}
		// Nothing was asked for by name: a page is reached through its form
		// controls far more often than through its links, and a page of links
		// would otherwise push the one field off the list.
		if !hinted {
			if ra, rb := kindRank(a.c.Role), kindRank(b.c.Role); ra != rb {
				return ra < rb
			}
		}
		return a.order < b.order
	})
	if len(ranked) > max {
		ranked = ranked[:max]
	}
	out := make([]proto.Candidate, 0, len(ranked))
	for _, c := range ranked {
		out = append(out, c.c)
	}
	return out
}

// kindRank orders the roles by how likely a step is to want one when it did
// not say: something to fill in, then something to press, then a link.
func kindRank(role string) int {
	switch role {
	case "textbox", "searchbox", "combobox", "textarea", "listbox",
		"checkbox", "radio", "switch", "slider", "spinbutton":
		return 0
	case "link":
		return 2
	default:
		return 1
	}
}

// hintWords splits the step's hints into lowercase words.
func hintWords(hints []string) []string {
	var out []string
	for _, h := range hints {
		for _, w := range strings.FieldsFunc(strings.ToLower(h), func(r rune) bool {
			return !isWordRune(r)
		}) {
			if len(w) >= 2 {
				out = append(out, w)
			}
		}
	}
	return out
}

func isWordRune(r rune) bool {
	return r == '_' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r > 127
}

// score is the hint match: a whole word is worth more than a prefix, a name
// match more than a role match. Nothing else about the element enters here —
// visibility and order are separate, later keys.
func score(c *proto.Candidate, words []string) int {
	if len(words) == 0 {
		return 0
	}
	name := strings.ToLower(c.Name)
	nameWords := strings.FieldsFunc(name, func(r rune) bool { return !isWordRune(r) })
	total := 0
	for _, w := range words {
		switch {
		case containsWord(nameWords, w):
			total += 4
		case strings.Contains(name, w):
			total += 3
		case hasPrefixWord(nameWords, w):
			total += 2
		case strings.ToLower(c.Role) == w:
			total += 1
		}
	}
	return total
}

func containsWord(words []string, w string) bool {
	for _, got := range words {
		if got == w {
			return true
		}
	}
	return false
}

// hasPrefixWord is the fuzzy half: "depart" matches "departure", and a word of
// at least four letters matches a hint it starts with ("departures" ~ "depart").
func hasPrefixWord(words []string, w string) bool {
	for _, got := range words {
		if strings.HasPrefix(got, w) || len(got) >= 4 && strings.HasPrefix(w, got) {
			return true
		}
	}
	return false
}

// capName truncates an accessible name to MaxNameBytes without splitting a
// rune. Names are page content: they are capped, never logged.
func capName(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= proto.MaxNameBytes {
		return s
	}
	return truncate(s, proto.MaxNameBytes)
}

// capDescription truncates a description to MaxDescriptionBytes without
// splitting a rune. Page content like a name: capped, never logged.
func capDescription(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= proto.MaxDescriptionBytes {
		return s
	}
	return truncate(s, proto.MaxDescriptionBytes)
}

// textMargin is how far past the cap the text is allowed to grow before
// collection stops: enough that the string handed to capText is longer than
// the cap whenever anything was left out, so Truncated is never a lie.
const textMargin = 256

// textBudget turns the request's cap into the number of bytes worth
// collecting, 0 when no text was asked for.
func textBudget(want int) int {
	if want <= 0 {
		return 0
	}
	if want > proto.MaxTextBytes {
		want = proto.MaxTextBytes
	}
	return want + textMargin
}

// anchorScan bounds the walk when the text is to be cut around an element: the
// page before the anchor is gathered only so the anchor's place in it is
// known, and a page whose anchor never arrives costs this much and not the
// page.
const anchorScan = 4 * proto.MaxTextBytes

// windowText cuts the excerpt around a place in the text rather than from its
// beginning: want bytes centred on at, moved inwards when the anchor sits near
// an end so the whole budget is spent on text that exists. The bool is whether
// anything was left out on either side.
func windowText(s string, at, want int) (string, bool) {
	if want > proto.MaxTextBytes {
		want = proto.MaxTextBytes
	}
	if want <= 0 || len(s) <= want {
		return s, false
	}
	start := at - want/2
	if start > len(s)-want {
		start = len(s) - want
	}
	if start < 0 {
		start = 0
	}
	// Both ends land on a rune boundary: the window is a cut through a page,
	// not a string the page ever wrote.
	for start > 0 && !utf8.RuneStart(s[start]) {
		start--
	}
	end := start + want
	for end < len(s) && !utf8.RuneStart(s[end]) {
		end--
	}
	return s[start:end], start > 0 || end < len(s)
}

// capText caps the visible text at the smaller of the request and MaxTextBytes.
func capText(s string, want int) (string, bool) {
	if want > proto.MaxTextBytes {
		want = proto.MaxTextBytes
	}
	if want <= 0 || len(s) <= want {
		return s, false
	}
	return truncate(s, want), true
}

// truncate cuts to n bytes at a rune boundary.
func truncate(s string, n int) string {
	if n >= len(s) {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
