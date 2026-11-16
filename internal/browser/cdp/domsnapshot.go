package cdp

import "context"

// DOMSnapshot.captureSnapshot is the other half of the observation: one call
// for the whole document with layout, instead of a DOM walk per node. Its
// result is index-encoded — every string is an offset into one shared table,
// and the parallel arrays are read by position — so the structs below are
// faithful to that shape rather than convenient. Only the fields observation
// needs are decoded.

// StrIdx indexes Snapshot.Strings. -1 means "no string".
type StrIdx int

// Snapshot is DOMSnapshot.captureSnapshot's result.
type Snapshot struct {
	Documents []DocumentSnapshot `json:"documents"`
	Strings   []string           `json:"strings"`
}

// Str resolves an index against the shared table.
func (s *Snapshot) Str(i StrIdx) string {
	if i < 0 || int(i) >= len(s.Strings) {
		return ""
	}
	return s.Strings[i]
}

// DocumentSnapshot is one document — the page, then one per iframe.
type DocumentSnapshot struct {
	DocumentURL   StrIdx             `json:"documentURL"`
	Title         StrIdx             `json:"title"`
	BaseURL       StrIdx             `json:"baseURL"`
	ContentWidth  float64            `json:"contentWidth,omitempty"`
	ContentHeight float64            `json:"contentHeight,omitempty"`
	ScrollOffsetX float64            `json:"scrollOffsetX,omitempty"`
	ScrollOffsetY float64            `json:"scrollOffsetY,omitempty"`
	Nodes         NodeTreeSnapshot   `json:"nodes"`
	Layout        LayoutTreeSnapshot `json:"layout"`
	TextBoxes     TextBoxSnapshot    `json:"textBoxes"`
}

// NodeTreeSnapshot holds every node of one document in parallel arrays, all of
// the same length; the index into them is a node's identity within the
// document. ParentIndex[i] is -1 for the root.
type NodeTreeSnapshot struct {
	ParentIndex   []int    `json:"parentIndex"`
	NodeType      []int    `json:"nodeType"`
	NodeName      []StrIdx `json:"nodeName"`
	NodeValue     []StrIdx `json:"nodeValue"`
	BackendNodeID []int64  `json:"backendNodeId"`
	// Attributes[i] is name,value,name,value,… as string indexes.
	Attributes [][]StrIdx `json:"attributes"`
	// ContentDocumentIndex points an iframe node at its document in Documents.
	ContentDocumentIndex RareIntegerData `json:"contentDocumentIndex"`
	InputValue           RareStringData  `json:"inputValue"`
	InputChecked         RareBooleanData `json:"inputChecked"`
	IsClickable          RareBooleanData `json:"isClickable"`
	CurrentSourceURL     RareStringData  `json:"currentSourceURL"`
}

// LayoutTreeSnapshot holds the nodes that have a box. NodeIndex[i] indexes
// NodeTreeSnapshot, Bounds[i] is [x, y, width, height] in document
// coordinates, Text[i] is the laid-out text. Styles[i] is the computed styles
// that were asked for, by position in SnapshotOptions.ComputedStyles, as
// indexes into the shared table: the protocol answers one array per laid-out
// node, never per document node.
type LayoutTreeSnapshot struct {
	NodeIndex []int       `json:"nodeIndex"`
	Bounds    [][]float64 `json:"bounds"`
	Text      []StrIdx    `json:"text"`
	Styles    [][]StrIdx  `json:"styles"`
}

// TextBoxSnapshot holds the individual text runs; LayoutIndex points into
// LayoutTreeSnapshot.
type TextBoxSnapshot struct {
	LayoutIndex []int       `json:"layoutIndex"`
	Bounds      [][]float64 `json:"bounds"`
	Start       []int       `json:"start"`
	Length      []int       `json:"length"`
}

// RareStringData is the protocol's sparse column: Value[k] belongs to node
// Index[k].
type RareStringData struct {
	Index []int    `json:"index"`
	Value []StrIdx `json:"value"`
}

// Get returns the value for a node index.
func (d RareStringData) Get(node int) (StrIdx, bool) {
	for k, i := range d.Index {
		if i == node && k < len(d.Value) {
			return d.Value[k], true
		}
	}
	return -1, false
}

// RareIntegerData is the sparse column for integers.
type RareIntegerData struct {
	Index []int `json:"index"`
	Value []int `json:"value"`
}

// Get returns the value for a node index.
func (d RareIntegerData) Get(node int) (int, bool) {
	for k, i := range d.Index {
		if i == node && k < len(d.Value) {
			return d.Value[k], true
		}
	}
	return 0, false
}

// RareBooleanData is the sparse column for flags: listed means true.
type RareBooleanData struct {
	Index []int `json:"index"`
}

// Has reports whether the flag is set for a node index.
func (d RareBooleanData) Has(node int) bool {
	for _, i := range d.Index {
		if i == node {
			return true
		}
	}
	return false
}

// SnapshotOptions are captureSnapshot's flags.
type SnapshotOptions struct {
	// ComputedStyles are the properties to include per node; usually a short
	// list such as display and visibility.
	ComputedStyles []string
	// IncludeDOMRects adds scroll and client rects; off by default, it roughly
	// doubles the result.
	IncludeDOMRects bool
	// IncludePaintOrder says which element is on top where they overlap.
	IncludePaintOrder bool
}

// CaptureSnapshot takes the whole document, iframes included.
func (s *Session) CaptureSnapshot(ctx context.Context, opts SnapshotOptions) (*Snapshot, error) {
	styles := opts.ComputedStyles
	if styles == nil {
		styles = []string{}
	}
	params := map[string]any{"computedStyles": styles}
	if opts.IncludeDOMRects {
		params["includeDOMRects"] = true
	}
	if opts.IncludePaintOrder {
		params["includePaintOrder"] = true
	}
	var snap Snapshot
	if err := s.Call(ctx, "DOMSnapshot.captureSnapshot", params, &snap); err != nil {
		return nil, err
	}
	return &snap, nil
}
