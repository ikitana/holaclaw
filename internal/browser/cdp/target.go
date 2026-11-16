package cdp

import "context"

// Target events we listen to. Everything below the browser level arrives with
// the session id of the target it belongs to.
const (
	EventTargetCreated      = "Target.targetCreated"
	EventTargetDestroyed    = "Target.targetDestroyed"
	EventTargetInfoChanged  = "Target.targetInfoChanged"
	EventAttachedToTarget   = "Target.attachedToTarget"
	EventDetachedFromTarget = "Target.detachedFromTarget"
)

// TargetInfo describes one target: a page, an iframe out of process, a worker.
type TargetInfo struct {
	TargetID         string `json:"targetId"`
	Type             string `json:"type"`
	Title            string `json:"title"`
	URL              string `json:"url"`
	Attached         bool   `json:"attached"`
	OpenerID         string `json:"openerId,omitempty"`
	BrowserContextID string `json:"browserContextId,omitempty"`
}

// TargetCreatedEvent is Target.targetCreated.
type TargetCreatedEvent struct {
	TargetInfo TargetInfo `json:"targetInfo"`
}

// TargetInfoChangedEvent is Target.targetInfoChanged. Title and URL changes of
// an attached page arrive here — which is how a page can report state without
// any main-world script.
type TargetInfoChangedEvent struct {
	TargetInfo TargetInfo `json:"targetInfo"`
}

// TargetDestroyedEvent is Target.targetDestroyed.
type TargetDestroyedEvent struct {
	TargetID string `json:"targetId"`
}

// AttachedToTargetEvent is Target.attachedToTarget: with flatten, SessionID is
// the id to put on commands for that target.
type AttachedToTargetEvent struct {
	SessionID          string     `json:"sessionId"`
	TargetInfo         TargetInfo `json:"targetInfo"`
	WaitingForDebugger bool       `json:"waitingForDebugger"`
}

// DetachedFromTargetEvent is Target.detachedFromTarget.
type DetachedFromTargetEvent struct {
	SessionID string `json:"sessionId"`
	TargetID  string `json:"targetId,omitempty"`
}

// GetTargets lists the browser's targets.
func (s *Session) GetTargets(ctx context.Context) ([]TargetInfo, error) {
	var r struct {
		TargetInfos []TargetInfo `json:"targetInfos"`
	}
	if err := s.Call(ctx, "Target.getTargets", nil, &r); err != nil {
		return nil, err
	}
	return r.TargetInfos, nil
}

// CreateTarget opens a new page and returns its target id.
func (s *Session) CreateTarget(ctx context.Context, url string) (string, error) {
	var r struct {
		TargetID string `json:"targetId"`
	}
	if err := s.Call(ctx, "Target.createTarget", map[string]any{"url": url}, &r); err != nil {
		return "", err
	}
	return r.TargetID, nil
}

// AttachToTarget attaches flat and returns the session id to use on commands.
func (s *Session) AttachToTarget(ctx context.Context, targetID string) (string, error) {
	var r struct {
		SessionID string `json:"sessionId"`
	}
	err := s.Call(ctx, "Target.attachToTarget", map[string]any{
		"targetId": targetID,
		"flatten":  true,
	}, &r)
	if err != nil {
		return "", err
	}
	return r.SessionID, nil
}

// SetAutoAttach attaches to targets this one spawns — popups and out-of-process
// iframes — as they appear, flat. Called on the browser session it catches
// whole new windows; called on a page session, that page's own frames.
//
// waitForDebuggerOnStart pauses each new target until RunIfWaitingForDebugger
// releases it, which is the only way to configure a popup (allow-list, download
// behaviour) before it can load anything. Measured on Chromium 153: with
// browser-level auto-attach the opener's `window.open` does not return until
// the new target has been handled either way, so the Input call that triggered
// it acknowledges late — handlers must not block, which Subscribe guarantees.
func (s *Session) SetAutoAttach(ctx context.Context, autoAttach, waitForDebuggerOnStart bool) error {
	return s.Call(ctx, "Target.setAutoAttach", map[string]any{
		"autoAttach":             autoAttach,
		"waitForDebuggerOnStart": waitForDebuggerOnStart,
		"flatten":                true,
	}, nil)
}

// RunIfWaitingForDebugger releases a target paused by auto-attach. It is the
// one Runtime method this package uses besides evaluate: it starts the page,
// it does not enable the domain, and the page cannot observe it.
func (s *Session) RunIfWaitingForDebugger(ctx context.Context) error {
	return s.Call(ctx, "Runtime.runIfWaitingForDebugger", nil, nil)
}

// SetDiscoverTargets turns the targetCreated / Destroyed / InfoChanged events on.
func (s *Session) SetDiscoverTargets(ctx context.Context, discover bool) error {
	return s.Call(ctx, "Target.setDiscoverTargets", map[string]any{"discover": discover}, nil)
}

// CloseTarget closes a page.
func (s *Session) CloseTarget(ctx context.Context, targetID string) error {
	return s.Call(ctx, "Target.closeTarget", map[string]any{"targetId": targetID}, nil)
}

// ActivateTarget brings a page to the front.
func (s *Session) ActivateTarget(ctx context.Context, targetID string) error {
	return s.Call(ctx, "Target.activateTarget", map[string]any{"targetId": targetID}, nil)
}

// GetTargetInfo reads a target's current title and URL. An empty targetID means
// this session's own target.
func (s *Session) GetTargetInfo(ctx context.Context, targetID string) (*TargetInfo, error) {
	params := map[string]any{}
	if targetID != "" {
		params["targetId"] = targetID
	}
	var r struct {
		TargetInfo TargetInfo `json:"targetInfo"`
	}
	if err := s.Call(ctx, "Target.getTargetInfo", params, &r); err != nil {
		return nil, err
	}
	return &r.TargetInfo, nil
}
