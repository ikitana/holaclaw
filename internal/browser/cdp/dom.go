package cdp

import "context"

// NodeRef names a node the way DOM methods accept: exactly one of the three.
// BackendNodeID is the one that survives across calls and domains (DOMSnapshot
// and Accessibility both speak it), so it is what observation carries around.
type NodeRef struct {
	NodeID        int64  `json:"nodeId,omitempty"`
	BackendNodeID int64  `json:"backendNodeId,omitempty"`
	ObjectID      string `json:"objectId,omitempty"`
}

// Node is one DOM node. Attributes are flat name/value pairs, as the protocol
// sends them.
type Node struct {
	NodeID          int64    `json:"nodeId"`
	ParentID        int64    `json:"parentId,omitempty"`
	BackendNodeID   int64    `json:"backendNodeId"`
	NodeType        int      `json:"nodeType"`
	NodeName        string   `json:"nodeName"`
	NodeValue       string   `json:"nodeValue"`
	ChildNodeCount  int      `json:"childNodeCount,omitempty"`
	Children        []Node   `json:"children,omitempty"`
	Attributes      []string `json:"attributes,omitempty"`
	FrameID         string   `json:"frameId,omitempty"`
	ContentDocument *Node    `json:"contentDocument,omitempty"`
	ShadowRoots     []Node   `json:"shadowRoots,omitempty"`
	ShadowRootType  string   `json:"shadowRootType,omitempty"`
}

// Attr returns an attribute's value.
func (n *Node) Attr(name string) (string, bool) {
	for i := 0; i+1 < len(n.Attributes); i += 2 {
		if n.Attributes[i] == name {
			return n.Attributes[i+1], true
		}
	}
	return "", false
}

// Quad is eight numbers: four corners, clockwise from top-left.
type Quad []float64

// Center is where a click goes.
func (q Quad) Center() (x, y float64) {
	if len(q) < 8 {
		return 0, 0
	}
	for i := 0; i < 8; i += 2 {
		x += q[i]
		y += q[i+1]
	}
	return x / 4, y / 4
}

// BoxModel is the geometry of a node, in viewport coordinates.
type BoxModel struct {
	Content Quad `json:"content"`
	Padding Quad `json:"padding"`
	Border  Quad `json:"border"`
	Margin  Quad `json:"margin"`
	Width   int  `json:"width"`
	Height  int  `json:"height"`
}

// GetDocument fetches the root. depth -1 is the whole tree; pierce follows
// shadow roots and iframe documents.
func (s *Session) GetDocument(ctx context.Context, depth int, pierce bool) (*Node, error) {
	var r struct {
		Root Node `json:"root"`
	}
	err := s.Call(ctx, "DOM.getDocument", map[string]any{"depth": depth, "pierce": pierce}, &r)
	if err != nil {
		return nil, err
	}
	return &r.Root, nil
}

// DescribeNode reads a node without adding it to the agent's node map.
func (s *Session) DescribeNode(ctx context.Context, ref NodeRef, depth int, pierce bool) (*Node, error) {
	params := refParams(ref)
	params["depth"] = depth
	params["pierce"] = pierce
	var r struct {
		Node Node `json:"node"`
	}
	if err := s.Call(ctx, "DOM.describeNode", params, &r); err != nil {
		return nil, err
	}
	return &r.Node, nil
}

// GetBoxModel returns a node's boxes. It fails for a node with no layout,
// which is how invisibility is detected.
func (s *Session) GetBoxModel(ctx context.Context, ref NodeRef) (*BoxModel, error) {
	var r struct {
		Model BoxModel `json:"model"`
	}
	if err := s.Call(ctx, "DOM.getBoxModel", refParams(ref), &r); err != nil {
		return nil, err
	}
	return &r.Model, nil
}

// GetContentQuads returns every box of a node — a link broken across lines has
// more than one — which is the honest place to aim a click.
func (s *Session) GetContentQuads(ctx context.Context, ref NodeRef) ([]Quad, error) {
	var r struct {
		Quads []Quad `json:"quads"`
	}
	if err := s.Call(ctx, "DOM.getContentQuads", refParams(ref), &r); err != nil {
		return nil, err
	}
	return r.Quads, nil
}

// ScrollIntoViewIfNeeded scrolls a node into the viewport.
func (s *Session) ScrollIntoViewIfNeeded(ctx context.Context, ref NodeRef) error {
	return s.Call(ctx, "DOM.scrollIntoViewIfNeeded", refParams(ref), nil)
}

// Focus focuses a node.
func (s *Session) Focus(ctx context.Context, ref NodeRef) error {
	return s.Call(ctx, "DOM.focus", refParams(ref), nil)
}

// ResolveNode returns a Runtime object id for a node in an execution context.
// contextID must be an isolated world's: resolving into the main world, and
// then calling into it, is what the rule of this package forbids.
func (s *Session) ResolveNode(ctx context.Context, ref NodeRef, contextID int64) (string, error) {
	params := refParams(ref)
	if contextID != 0 {
		params["executionContextId"] = contextID
	}
	var r struct {
		Object struct {
			ObjectID string `json:"objectId"`
		} `json:"object"`
	}
	if err := s.Call(ctx, "DOM.resolveNode", params, &r); err != nil {
		return "", err
	}
	return r.Object.ObjectID, nil
}

// GetNodeForLocation hit-tests a viewport point: what a click there would hit.
func (s *Session) GetNodeForLocation(ctx context.Context, x, y int) (backendNodeID, nodeID int64, frameID string, err error) {
	var r struct {
		BackendNodeID int64  `json:"backendNodeId"`
		NodeID        int64  `json:"nodeId"`
		FrameID       string `json:"frameId"`
	}
	err = s.Call(ctx, "DOM.getNodeForLocation", map[string]any{
		"x": x, "y": y,
		"includeUserAgentShadowDOM": false,
	}, &r)
	if err != nil {
		return 0, 0, "", err
	}
	return r.BackendNodeID, r.NodeID, r.FrameID, nil
}

func refParams(ref NodeRef) map[string]any {
	p := map[string]any{}
	switch {
	case ref.NodeID != 0:
		p["nodeId"] = ref.NodeID
	case ref.BackendNodeID != 0:
		p["backendNodeId"] = ref.BackendNodeID
	case ref.ObjectID != "":
		p["objectId"] = ref.ObjectID
	}
	return p
}
