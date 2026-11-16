package cdp

import "context"

// Browser-level commands: they take no session id, so call them on
// Conn.Session("").

// Download events, on for a browser context once SetDownloadBehavior asked
// for them.
const (
	EventDownloadWillBegin = "Browser.downloadWillBegin"
	EventDownloadProgress  = "Browser.downloadProgress"
)

// Version is Browser.getVersion: what `check` reports.
type Version struct {
	ProtocolVersion string `json:"protocolVersion"`
	Product         string `json:"product"`
	Revision        string `json:"revision"`
	UserAgent       string `json:"userAgent"`
	JSVersion       string `json:"jsVersion"`
}

// DownloadWillBeginEvent is Browser.downloadWillBegin.
type DownloadWillBeginEvent struct {
	FrameID           string `json:"frameId"`
	GUID              string `json:"guid"`
	URL               string `json:"url"`
	SuggestedFilename string `json:"suggestedFilename"`
}

// DownloadProgressEvent is Browser.downloadProgress; State is "inProgress",
// "completed" or "canceled".
type DownloadProgressEvent struct {
	GUID          string  `json:"guid"`
	TotalBytes    float64 `json:"totalBytes"`
	ReceivedBytes float64 `json:"receivedBytes"`
	State         string  `json:"state"`
}

// GetVersion reads the browser's version and user agent.
func (s *Session) GetVersion(ctx context.Context) (*Version, error) {
	var v Version
	if err := s.Call(ctx, "Browser.getVersion", nil, &v); err != nil {
		return nil, err
	}
	return &v, nil
}

// SetDownloadBehavior points downloads at the quarantine directory. behavior is
// "allow", "deny", "default" or "allowAndName"; browserContextID "" is the
// default context. eventsEnabled turns the two events above on.
func (s *Session) SetDownloadBehavior(ctx context.Context, behavior, browserContextID, downloadPath string, eventsEnabled bool) error {
	params := map[string]any{"behavior": behavior, "eventsEnabled": eventsEnabled}
	if browserContextID != "" {
		params["browserContextId"] = browserContextID
	}
	if downloadPath != "" {
		params["downloadPath"] = downloadPath
	}
	return s.Call(ctx, "Browser.setDownloadBehavior", params, nil)
}

// BrowserClose asks the browser to exit. The read loop ends shortly after.
func (s *Session) BrowserClose(ctx context.Context) error {
	return s.Call(ctx, "Browser.close", nil, nil)
}
