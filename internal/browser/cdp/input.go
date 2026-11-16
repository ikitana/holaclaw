package cdp

import (
	"context"
	"encoding/json"
	"fmt"
)

// Input is the only way this package touches a page: dispatched events are
// isTrusted, which no script injection can claim (docs/browser.md). Pacing —
// curved pointer paths, key jitter, dwell — belongs a layer up.

// MouseEvent is one Input.dispatchMouseEvent. Type is "mousePressed",
// "mouseReleased", "mouseMoved" or "mouseWheel"; Button is "none", "left",
// "middle", "right", "back" or "forward".
type MouseEvent struct {
	Type       string  `json:"type"`
	X          float64 `json:"x"`
	Y          float64 `json:"y"`
	Button     string  `json:"button,omitempty"`
	Buttons    int     `json:"buttons,omitempty"`
	ClickCount int     `json:"clickCount,omitempty"`
	Modifiers  int     `json:"modifiers,omitempty"`
	DeltaX     float64 `json:"deltaX,omitempty"` // mouseWheel only
	DeltaY     float64 `json:"deltaY,omitempty"` // mouseWheel only
	Timestamp  float64 `json:"timestamp,omitempty"`
}

// KeyEvent is one Input.dispatchKeyEvent. Type is "keyDown", "keyUp",
// "rawKeyDown" or "char". A printable key needs Text as well as Key/Code.
type KeyEvent struct {
	Type                  string `json:"type"`
	Key                   string `json:"key,omitempty"`
	Code                  string `json:"code,omitempty"`
	Text                  string `json:"text,omitempty"`
	UnmodifiedText        string `json:"unmodifiedText,omitempty"`
	WindowsVirtualKeyCode int    `json:"windowsVirtualKeyCode,omitempty"`
	NativeVirtualKeyCode  int    `json:"nativeVirtualKeyCode,omitempty"`
	Modifiers             int    `json:"modifiers,omitempty"`
	Location              int    `json:"location,omitempty"`
	AutoRepeat            bool   `json:"autoRepeat,omitempty"`
	IsKeypad              bool   `json:"isKeypad,omitempty"`
}

// DispatchMouseEvent sends one pointer or wheel event at viewport coordinates.
func (s *Session) DispatchMouseEvent(ctx context.Context, ev MouseEvent) error {
	if ev.Type != "mouseWheel" {
		return s.Call(ctx, "Input.dispatchMouseEvent", ev, nil)
	}
	// A wheel event must carry both deltas even when one is zero, which the
	// struct's omitempty would drop.
	b, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("cdp: mouse event: %w", err)
	}
	var params map[string]any
	if err := json.Unmarshal(b, &params); err != nil {
		return fmt.Errorf("cdp: mouse event: %w", err)
	}
	params["deltaX"] = ev.DeltaX
	params["deltaY"] = ev.DeltaY
	return s.Call(ctx, "Input.dispatchMouseEvent", params, nil)
}

// DispatchKeyEvent sends one key event to the focused element.
func (s *Session) DispatchKeyEvent(ctx context.Context, ev KeyEvent) error {
	return s.Call(ctx, "Input.dispatchKeyEvent", ev, nil)
}

// InsertText commits text as if it came from an input method: one event for a
// whole string, used for pasted or non-typeable content.
func (s *Session) InsertText(ctx context.Context, text string) error {
	return s.Call(ctx, "Input.insertText", map[string]any{"text": text}, nil)
}
