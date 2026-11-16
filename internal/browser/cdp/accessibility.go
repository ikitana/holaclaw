package cdp

import "context"

// The accessibility tree is half of the observation (docs/browser.md): role and name for
// every element, which is what a decision model can judge, at a fraction of
// the tokens a DOM dump costs.

// AXValue is a property value. Value is whatever the protocol sent: a string
// for a name, a bool for "focusable", a list of node ids for relations.
type AXValue struct {
	Type  string `json:"type"`
	Value any    `json:"value,omitempty"`
	// RelatedNodes is what a relation property ("errormessage",
	// "describedby", "controls") points at. Chromium sends it beside the
	// value on those properties.
	RelatedNodes []AXRelatedNode `json:"relatedNodes,omitempty"`
}

// AXRelatedNode is one end of a relation. IDRef is the id attribute the page
// wrote; BackendDOMNodeID is the node itself and is what a reader should
// prefer, since it survives a document that reuses ids across frames.
type AXRelatedNode struct {
	BackendDOMNodeID int64  `json:"backendDOMNodeId,omitempty"`
	IDRef            string `json:"idref,omitempty"`
	Text             string `json:"text,omitempty"`
}

// Text is the string form of a value, "" for anything else.
func (v *AXValue) Text() string {
	if v == nil {
		return ""
	}
	s, _ := v.Value.(string)
	return s
}

// Bool is the boolean form of a value.
func (v *AXValue) Bool() bool {
	if v == nil {
		return false
	}
	b, _ := v.Value.(bool)
	return b
}

// IDRefs is the nodes a relation property points at. Chromium reports them in
// relatedNodes, but the same list also arrives inside the value of an
// "idrefList" on some builds, so both shapes are read: a caller asking what an
// invalid field's errormessage names should not depend on which one it got.
func (v *AXValue) IDRefs() []AXRelatedNode {
	if v == nil {
		return nil
	}
	out := v.RelatedNodes
	list, ok := v.Value.([]any)
	if !ok {
		return out
	}
	for _, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		var n AXRelatedNode
		// A generic decode: the numbers came through encoding/json as
		// float64, and "value" is what an idref item calls its id.
		if f, ok := m["backendDOMNodeId"].(float64); ok {
			n.BackendDOMNodeID = int64(f)
		}
		for _, key := range []string{"idref", "value"} {
			if s, ok := m[key].(string); ok && s != "" {
				n.IDRef = s
				break
			}
		}
		if s, ok := m["text"].(string); ok {
			n.Text = s
		}
		if n.BackendDOMNodeID != 0 || n.IDRef != "" {
			out = append(out, n)
		}
	}
	return out
}

// AXProperty is one named property of a node ("focusable", "disabled",
// "checked", …).
type AXProperty struct {
	Name  string  `json:"name"`
	Value AXValue `json:"value"`
}

// AXNode is one node of the accessibility tree. BackendDOMNodeID ties it back
// to DOM and DOMSnapshot.
type AXNode struct {
	NodeID           string       `json:"nodeId"`
	Ignored          bool         `json:"ignored"`
	Role             *AXValue     `json:"role,omitempty"`
	Name             *AXValue     `json:"name,omitempty"`
	Description      *AXValue     `json:"description,omitempty"`
	Value            *AXValue     `json:"value,omitempty"`
	Properties       []AXProperty `json:"properties,omitempty"`
	ChildIDs         []string     `json:"childIds,omitempty"`
	ParentID         string       `json:"parentId,omitempty"`
	BackendDOMNodeID int64        `json:"backendDOMNodeId,omitempty"`
	FrameID          string       `json:"frameId,omitempty"`
}

// AccessibilityEnable turns the domain on. Measured against Chromium 153:
// getFullAXTree answers without it, so this is not called on the hot path; it
// exists for the incremental events we do not use yet.
func (s *Session) AccessibilityEnable(ctx context.Context) error {
	return s.Call(ctx, "Accessibility.enable", nil, nil)
}

// GetFullAXTree returns the whole tree. depth 0 means no limit; frameID "" is
// the session's own frame.
func (s *Session) GetFullAXTree(ctx context.Context, depth int, frameID string) ([]AXNode, error) {
	params := map[string]any{}
	if depth > 0 {
		params["depth"] = depth
	}
	if frameID != "" {
		params["frameId"] = frameID
	}
	var r struct {
		Nodes []AXNode `json:"nodes"`
	}
	if err := s.Call(ctx, "Accessibility.getFullAXTree", params, &r); err != nil {
		return nil, err
	}
	return r.Nodes, nil
}

// QueryAXTree finds nodes by accessible name and/or role under a root.
func (s *Session) QueryAXTree(ctx context.Context, root NodeRef, accessibleName, role string) ([]AXNode, error) {
	params := refParams(root)
	if accessibleName != "" {
		params["accessibleName"] = accessibleName
	}
	if role != "" {
		params["role"] = role
	}
	var r struct {
		Nodes []AXNode `json:"nodes"`
	}
	if err := s.Call(ctx, "Accessibility.queryAXTree", params, &r); err != nil {
		return nil, err
	}
	return r.Nodes, nil
}
