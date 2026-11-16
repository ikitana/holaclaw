package cdp

import (
	"context"
	"encoding/base64"
	"fmt"
)

// Page events we listen to.
const (
	EventFrameNavigated          = "Page.frameNavigated"
	EventLifecycleEvent          = "Page.lifecycleEvent"
	EventLoadEventFired          = "Page.loadEventFired"
	EventJavaScriptDialogOpening = "Page.javascriptDialogOpening"
	EventScreencastFrame         = "Page.screencastFrame"
)

// Frame is one frame of a page.
type Frame struct {
	ID             string `json:"id"`
	ParentID       string `json:"parentId,omitempty"`
	LoaderID       string `json:"loaderId,omitempty"`
	Name           string `json:"name,omitempty"`
	URL            string `json:"url"`
	SecurityOrigin string `json:"securityOrigin,omitempty"`
	UnreachableURL string `json:"unreachableUrl,omitempty"`
}

// FrameTree is Page.getFrameTree's answer, children included.
type FrameTree struct {
	Frame       Frame       `json:"frame"`
	ChildFrames []FrameTree `json:"childFrames,omitempty"`
}

// FrameNavigatedEvent is Page.frameNavigated.
type FrameNavigatedEvent struct {
	Frame Frame  `json:"frame"`
	Type  string `json:"type,omitempty"`
}

// LifecycleEvent is Page.lifecycleEvent; Name is "init", "load",
// "DOMContentLoaded", "networkIdle", …
type LifecycleEvent struct {
	FrameID   string  `json:"frameId"`
	LoaderID  string  `json:"loaderId"`
	Name      string  `json:"name"`
	Timestamp float64 `json:"timestamp"`
}

// JavaScriptDialogOpeningEvent is Page.javascriptDialogOpening. A dialog blocks
// the renderer until it is handled.
type JavaScriptDialogOpeningEvent struct {
	URL               string `json:"url"`
	Message           string `json:"message"`
	Type              string `json:"type"`
	HasBrowserHandler bool   `json:"hasBrowserHandler"`
	DefaultPrompt     string `json:"defaultPrompt,omitempty"`
}

// ScreencastFrameEvent is Page.screencastFrame. Data is base64 JPEG; the frame
// must be acked with its SessionID or the stream stalls.
type ScreencastFrameEvent struct {
	Data      string              `json:"data"`
	Metadata  ScreencastFrameMeta `json:"metadata"`
	SessionID int                 `json:"sessionId"`
}

// ScreencastFrameMeta places a frame in the page.
type ScreencastFrameMeta struct {
	OffsetTop       float64 `json:"offsetTop"`
	PageScaleFactor float64 `json:"pageScaleFactor"`
	DeviceWidth     float64 `json:"deviceWidth"`
	DeviceHeight    float64 `json:"deviceHeight"`
	ScrollOffsetX   float64 `json:"scrollOffsetX"`
	ScrollOffsetY   float64 `json:"scrollOffsetY"`
	Timestamp       float64 `json:"timestamp,omitempty"`
}

// NavigationEntry is one step of the back/forward history.
type NavigationEntry struct {
	ID             int    `json:"id"`
	URL            string `json:"url"`
	UserTypedURL   string `json:"userTypedURL,omitempty"`
	Title          string `json:"title"`
	TransitionType string `json:"transitionType,omitempty"`
}

// PageEnable turns the Page domain on. Unlike Runtime it tells the page
// nothing: it only asks the browser process for navigation notifications.
func (s *Session) PageEnable(ctx context.Context) error {
	return s.Call(ctx, "Page.enable", nil, nil)
}

// SetLifecycleEventsEnabled turns Page.lifecycleEvent on, which is how a load
// is awaited without polling.
func (s *Session) SetLifecycleEventsEnabled(ctx context.Context, enabled bool) error {
	return s.Call(ctx, "Page.setLifecycleEventsEnabled", map[string]any{"enabled": enabled}, nil)
}

// Navigate loads url. A navigation the browser refused comes back as errorText
// with no error: the call itself succeeded.
func (s *Session) Navigate(ctx context.Context, url string) (frameID, errorText string, err error) {
	var r struct {
		FrameID   string `json:"frameId"`
		ErrorText string `json:"errorText,omitempty"`
	}
	if err := s.Call(ctx, "Page.navigate", map[string]any{"url": url}, &r); err != nil {
		return "", "", err
	}
	return r.FrameID, r.ErrorText, nil
}

// GetFrameTree returns the page's frames.
func (s *Session) GetFrameTree(ctx context.Context) (*FrameTree, error) {
	var r struct {
		FrameTree FrameTree `json:"frameTree"`
	}
	if err := s.Call(ctx, "Page.getFrameTree", nil, &r); err != nil {
		return nil, err
	}
	return &r.FrameTree, nil
}

// CaptureScreenshot returns the decoded image. format is "jpeg" or "png";
// quality applies to jpeg only.
func (s *Session) CaptureScreenshot(ctx context.Context, format string, quality int) ([]byte, error) {
	params := map[string]any{"format": format}
	if format == "jpeg" && quality > 0 {
		params["quality"] = quality
	}
	var r struct {
		Data string `json:"data"`
	}
	if err := s.Call(ctx, "Page.captureScreenshot", params, &r); err != nil {
		return nil, err
	}
	b, err := base64.StdEncoding.DecodeString(r.Data)
	if err != nil {
		return nil, fmt.Errorf("cdp: screenshot: %w", err)
	}
	return b, nil
}

// StartScreencast begins the JPEG stream behind the live view (docs/browser.md). Frames
// arrive as EventScreencastFrame and every one must be acked.
func (s *Session) StartScreencast(ctx context.Context, format string, quality, maxWidth, maxHeight, everyNthFrame int) error {
	params := map[string]any{"format": format}
	if quality > 0 {
		params["quality"] = quality
	}
	if maxWidth > 0 {
		params["maxWidth"] = maxWidth
	}
	if maxHeight > 0 {
		params["maxHeight"] = maxHeight
	}
	if everyNthFrame > 0 {
		params["everyNthFrame"] = everyNthFrame
	}
	return s.Call(ctx, "Page.startScreencast", params, nil)
}

// StopScreencast ends the stream.
func (s *Session) StopScreencast(ctx context.Context) error {
	return s.Call(ctx, "Page.stopScreencast", nil, nil)
}

// ScreencastFrameAck acknowledges one frame; this is the backpressure.
func (s *Session) ScreencastFrameAck(ctx context.Context, sessionID int) error {
	return s.Call(ctx, "Page.screencastFrameAck", map[string]any{"sessionId": sessionID}, nil)
}

// HandleJavaScriptDialog answers an open alert/confirm/prompt/beforeunload.
func (s *Session) HandleJavaScriptDialog(ctx context.Context, accept bool, promptText string) error {
	params := map[string]any{"accept": accept}
	if promptText != "" {
		params["promptText"] = promptText
	}
	return s.Call(ctx, "Page.handleJavaScriptDialog", params, nil)
}

// BringToFront raises the page's window.
func (s *Session) BringToFront(ctx context.Context) error {
	return s.Call(ctx, "Page.bringToFront", nil, nil)
}

// GetNavigationHistory returns the history and the index of the current entry.
func (s *Session) GetNavigationHistory(ctx context.Context) (current int, entries []NavigationEntry, err error) {
	var r struct {
		CurrentIndex int               `json:"currentIndex"`
		Entries      []NavigationEntry `json:"entries"`
	}
	if err := s.Call(ctx, "Page.getNavigationHistory", nil, &r); err != nil {
		return 0, nil, err
	}
	return r.CurrentIndex, r.Entries, nil
}

// NavigateToHistoryEntry goes back or forward to an entry id.
func (s *Session) NavigateToHistoryEntry(ctx context.Context, entryID int) error {
	return s.Call(ctx, "Page.navigateToHistoryEntry", map[string]any{"entryId": entryID}, nil)
}

// CreateIsolatedWorld makes a world in frameID and returns its execution
// context id. Prefer EvalIsolated, which does this and the evaluate together.
func (s *Session) CreateIsolatedWorld(ctx context.Context, frameID, worldName string, grantUniversalAccess bool) (int64, error) {
	var r struct {
		ExecutionContextID int64 `json:"executionContextId"`
	}
	err := s.Call(ctx, "Page.createIsolatedWorld", map[string]any{
		"frameId":             frameID,
		"worldName":           worldName,
		"grantUniveralAccess": grantUniversalAccess, // the protocol's own spelling
	}, &r)
	if err != nil {
		return 0, err
	}
	return r.ExecutionContextID, nil
}
