// Package proto is the contract between the two halves of the browser
// (docs/browser.md): the executor in the daemon and the sidecar process that
// owns Chromium. They talk HTTP/JSON over a unix socket, plus one WebSocket per
// live view. Both halves import these types; nothing else does.
//
// The sidecar holds no keys, no database and no policy. Everything it is told
// here comes from the daemon, which has already authenticated and authorised
// the caller; everything it returns about a page is untrusted content.
package proto

import "time"

// Paths of the sidecar API. {agent} is the agent name: one Chromium process
// and one profile per agent.
const (
	PathHealth     = "/health"
	PathSessions   = "/sessions"                    // GET: []Session
	PathOpen       = "/sessions/{agent}/open"       // POST OpenRequest → Session
	PathClose      = "/sessions/{agent}/close"      // POST → 204
	PathObserve    = "/sessions/{agent}/observe"    // POST ObserveRequest → Observation
	PathAct        = "/sessions/{agent}/act"        // POST Action → ActResult
	PathScreenshot = "/sessions/{agent}/screenshot" // POST → image/jpeg
	PathStream     = "/sessions/{agent}/stream"     // GET, WebSocket (see StreamMessage)
	// PathDownload serves one quarantined file (application/octet-stream) and
	// deletes it afterwards: the two halves run as different users, so files
	// cross the socket, never the filesystem.
	PathDownload = "/sessions/{agent}/downloads/{id}" // GET → bytes
)

// Health is what `check` and the daemon read at start.
type Health struct {
	Version  string `json:"version"`
	Binary   string `json:"binary"`  // resolved browser binary, "" when none was found
	Browser  string `json:"browser"` // its version string
	Headless bool   `json:"headless"`
	// Problem is a fixed, actionable sentence when the sidecar cannot run a
	// browser (no binary, no Xvfb, sandbox unusable), else "".
	Problem string `json:"problem,omitempty"`
}

// OpenRequest starts (or reuses) the agent's browser.
type OpenRequest struct {
	// Allow limits navigation to these hosts and their subdomains. Empty means
	// no list: any site. Enforced by the sidecar on every navigation and
	// redirect, never by a prompt.
	Allow []string `json:"allow,omitempty"`
	// Fresh starts the session on a page of its own: one new about:blank tab
	// becomes the active one and every tab before it is closed. The profile —
	// cookies, logins, storage — is kept; the page, its history, a dialog it
	// left open and the popups it opened are not.
	Fresh bool `json:"fresh,omitempty"`
}

// Session is one agent's running browser.
type Session struct {
	Agent   string    `json:"agent"`
	Started time.Time `json:"started"`
	Tabs    []Tab     `json:"tabs"`
}

// Tab is one page target. The active tab is the one observed and acted on.
type Tab struct {
	ID     string `json:"id"`
	URL    string `json:"url"`
	Title  string `json:"title"`
	Active bool   `json:"active"`
}

// ObserveRequest asks for a compact view of the active tab.
type ObserveRequest struct {
	// Hints are words from the current step ("departure", "search"); candidates
	// matching them rank first. Optional.
	Hints []string `json:"hints,omitempty"`
	// MaxCandidates caps the shortlist; 0 means DefaultMaxCandidates.
	MaxCandidates int `json:"max_candidates,omitempty"`
	// TextBytes caps the visible-text excerpt; 0 means none is returned.
	TextBytes int `json:"text_bytes,omitempty"`
	// TextAround is a ref of the previous observation of the same tab. When it
	// is set and the page's text is longer than TextBytes, the excerpt is the
	// window of the page around that element rather than its beginning: what
	// a move just did shows next to the element it was done to, and a site's
	// complaint about a field sits beside the field. Ignored when the ref is
	// unknown or the whole text fits.
	TextAround string `json:"text_around,omitempty"`
}

// Limits of an observation. A decision model takes at most 64 options.
const (
	DefaultMaxCandidates = 25 // M10: recall@25 == recall@40 on 20 real targets (tools/recall)
	MaxCandidates        = 64
	MaxTextBytes         = 32 << 10
	MaxNameBytes         = 120
	// MaxAlerts caps Observation.Alerts; each entry is capped at MaxNameBytes.
	MaxAlerts = 5
	// MaxDescriptionBytes caps Candidate.Description: long enough for the
	// sentence a page writes to explain a widget, short enough that a page
	// cannot write a prompt there.
	MaxDescriptionBytes = 240
)

// Observation is the state a step is decided on: small by construction.
// Every string in it that came from the page (Title, Name, Value, Text) is
// untrusted content.
type Observation struct {
	URL        string      `json:"url"`
	Title      string      `json:"title"`
	Loading    bool        `json:"loading"`
	Candidates []Candidate `json:"candidates"`
	// Total is how many interactive elements the page had before the shortlist.
	Total int `json:"total"`
	// Text is the visible text of the page, capped; "" unless asked for.
	Text      string `json:"text,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
	// Dialog is an open JavaScript dialog's kind ("alert", "confirm", "prompt",
	// "beforeunload"), else "". The sidecar dismisses dialogs on the next action.
	Dialog string `json:"dialog,omitempty"`
	// Alerts is what the page is telling the user right now, in document
	// order: the text of role=alert and role=status elements and of aria-live
	// regions, and the message an invalid field points at through
	// aria-errormessage or aria-describedby. At most MaxAlerts entries of
	// MaxNameBytes each, deduplicated; empty when the page says nothing. Page
	// content like everything else here.
	Alerts []string `json:"alerts,omitempty"`
	// Dialogs are the open dialog and alertdialog elements of the page — a
	// date picker, a passengers panel, a cookie banner — by accessible name
	// ("dialog" when unnamed), in document order, capped at MaxAlerts. A
	// dialog that appears after a move is a page the rest of a queue was not
	// decided on.
	Dialogs []string `json:"dialogs,omitempty"`
	// Modal is the name of the dialog that is modal (aria-modal), else "". When
	// it is set the shortlist holds only the modal's own controls: everything
	// behind it is there to be looked at and cannot be clicked.
	Modal string `json:"modal,omitempty"`
}

// Candidate is one interactive element. Ref is opaque and valid only until
// the next observation of the same tab.
type Candidate struct {
	Ref       string `json:"ref"`
	Role      string `json:"role"` // accessibility role: button, link, textbox, combobox, …
	Name      string `json:"name"` // accessible name, capped at MaxNameBytes
	Value     string `json:"value,omitempty"`
	Focused   bool   `json:"focused,omitempty"`
	Disabled  bool   `json:"disabled,omitempty"`
	Offscreen bool   `json:"offscreen,omitempty"`
	// Expanded is the element's own aria-expanded state — a combobox whose
	// suggestion list is open, a disclosure that is open. nil when the element
	// has no such state, which is most elements.
	Expanded *bool `json:"expanded,omitempty"`
	// HasPopup says what the element opens when acted on, in the page's own
	// terms: "listbox", "menu", "dialog", "grid", "tree" or "true". "" when it
	// opens nothing it admits to.
	HasPopup string `json:"haspopup,omitempty"`
	// Invalid is the page's own verdict on a field (aria-invalid, or the
	// accessibility tree's invalid state other than "false"): what it holds
	// was rejected. Required is the field's own required flag.
	Invalid  bool `json:"invalid,omitempty"`
	Required bool `json:"required,omitempty"`
	// Secret marks a field whose contents are nobody's business but the
	// page's: an input of type password, or one whose autocomplete names a
	// card number, a card security code, a password or a one-time code. Value
	// is never populated for it.
	Secret bool `json:"secret,omitempty"`
	// Editable says characters can be typed into the element: an input, a
	// textarea, a contenteditable, or a node the accessibility tree marks
	// editable. A combobox that is not editable is a button that opens a
	// list: it is clicked, never typed into.
	Editable bool `json:"editable,omitempty"`
	// In is the ref of the element this one belongs to, when that element is
	// on the shortlist too: an option's listbox, a listbox's combobox
	// (through aria-controls or aria-owns), a menu item's menu, a control's
	// dialog. "" for an element at the top of the page. It is what lets a
	// reader — a person or a model — tell which box a list of options came
	// out of.
	In string `json:"in,omitempty"`
	// Description is the element's accessible description (aria-describedby,
	// title): the page's own sentence about how the element works, when it
	// wrote one — "use the arrow keys to change the date". Capped at
	// MaxDescriptionBytes; page content like the name.
	Description string `json:"description,omitempty"`
	// New says the element was not on the previous observation of the same
	// tab: it is what the last move put on the screen — a calendar, a panel,
	// a menu — whatever it calls itself. The shortlist ranks new elements
	// first when no hints are given. False on the first observation of a tab
	// and after a navigation, where everything is new and nothing is.
	New bool `json:"new,omitempty"`
}

// ActionKind is the closed set of things the executor may ask for.
type ActionKind string

const (
	ActNavigate ActionKind = "navigate" // URL
	ActClick    ActionKind = "click"    // Ref
	ActType     ActionKind = "type"     // Ref, Text, Clear, Submit
	ActKey      ActionKind = "key"      // Key: "Enter", "Escape", "Tab", "ArrowDown", …
	ActScroll   ActionKind = "scroll"   // DeltaY in CSS pixels; Ref optional
	ActWait     ActionKind = "wait"     // until the page is quiet; see Until
	ActBack     ActionKind = "back"
)

// WaitUntil is the closed set of things an ActWait waits for. The browser
// itself says when: Chromium's own lifecycle events first, then a window in
// which no resource arrived and the document did not change. Nothing here is a
// fixed sleep — TimeoutMS is only the point at which a page that never stops
// is decided on as it stands.
type WaitUntil string

const (
	// WaitQuiet returns as soon as the page has finished loading and gone
	// quiet, at once if it already has. It is the default.
	WaitQuiet WaitUntil = "quiet"
	// WaitChange waits for the page to change first and then go quiet: what a
	// caller wants when it can see that content it expects has not arrived,
	// on a page Chromium already considers loaded. A page that never changes
	// costs the timeout and comes back with Quiet false.
	WaitChange WaitUntil = "change"
)

// Action is one step on the active tab.
type Action struct {
	Kind   ActionKind `json:"kind"`
	Ref    string     `json:"ref,omitempty"`
	URL    string     `json:"url,omitempty"`
	Text   string     `json:"text,omitempty"`
	Key    string     `json:"key,omitempty"`
	Until  WaitUntil  `json:"until,omitempty"`  // wait: what to wait for; "" = WaitQuiet
	Clear  bool       `json:"clear,omitempty"`  // type: replace what is there
	Submit bool       `json:"submit,omitempty"` // type: press Enter afterwards
	DeltaY int        `json:"delta_y,omitempty"`
	// TimeoutMS bounds the action including the settle wait; 0 = default.
	TimeoutMS int `json:"timeout_ms,omitempty"`
}

// ErrorCode is the closed set of reasons an action fails. The executor acts
// on the code; Detail is for the trace and never holds page text.
type ErrorCode string

const (
	ErrStaleRef   ErrorCode = "stale_ref"   // observe again
	ErrNotAllowed ErrorCode = "not_allowed" // navigation outside the allow-list
	ErrTimeout    ErrorCode = "timeout"
	ErrBadAction  ErrorCode = "bad_action" // malformed or unsupported
	ErrHeld       ErrorCode = "held"       // a person holds the takeover lock
	ErrNoSession  ErrorCode = "no_session"
	ErrBrowser    ErrorCode = "browser" // crash, CDP failure
)

// ActResult reports what an action did.
type ActResult struct {
	OK        bool      `json:"ok"`
	Code      ErrorCode `json:"code,omitempty"`
	Detail    string    `json:"detail,omitempty"`
	URL       string    `json:"url"` // after the action settled
	Navigated bool      `json:"navigated,omitempty"`
	// Quiet is set by ActWait: true when the page reached the state that was
	// waited for, false when the timeout came first. Other actions leave it
	// unset — their settle is best-effort and says nothing.
	Quiet bool `json:"quiet,omitempty"`
	// Downloads finished during the action, already in the quarantine
	// directory; the daemon moves them into blobs.
	Downloads []Download `json:"downloads,omitempty"`
	TookMS    int64      `json:"took_ms"`
}

// Download is a file the browser saved. It stays in the sidecar.s quarantine
// until fetched through PathDownload, the session closes, or MaxDownloadAge
// passes; the daemon moves the bytes into blobs.
type Download struct {
	ID   string `json:"id"`   // opaque, one path segment
	Name string `json:"name"` // suggested file name, sanitised
	Size int64  `json:"size"`
}

// Download caps: per file, per agent in quarantine, and how long an unfetched
// file is kept.
const (
	MaxDownloadBytes = 50 << 20
	MaxDownloadTotal = 200 << 20
	MaxDownloadAge   = time.Hour
)

// Error is the JSON body of every non-2xx sidecar response.
type Error struct {
	Code   ErrorCode `json:"code"`
	Detail string    `json:"detail,omitempty"`
}

// ── Live view ────────────────────────────────────────────────────────────────
//
// On the stream WebSocket a binary message from the sidecar is one JPEG frame
// of the active tab; the viewer answers each with a StreamAck so a slow link
// slows the screencast instead of queueing frames. Text messages are JSON
// StreamMessage values in both directions.

// StreamType discriminates a StreamMessage.
type StreamType string

const (
	StreamMeta    StreamType = "meta"    // → viewer: size and tabs (sidecar), holder (added by the daemon)
	StreamAck     StreamType = "ack"     // viewer → sidecar: last frame shown
	StreamPointer StreamType = "pointer" // viewer → sidecar
	StreamWheel   StreamType = "wheel"
	StreamKey     StreamType = "key"
	StreamText    StreamType = "text" // committed text: IME, paste
	StreamTab     StreamType = "tab"  // viewer → sidecar: activate Tab
	StreamQuality StreamType = "quality"
	// StreamActivity is the sidecar's own account of one observe or act the
	// engine served, so a live view can show what is being done to the page it
	// is showing.
	StreamActivity StreamType = "activity"
)

// Activity is the sidecar's own account of one call it served, for a live
// viewer. It carries what the engine did and what the page reported — never
// the text an action typed (Action.Text) and never a key an action pressed.
type Activity struct {
	At     time.Time `json:"at"`
	Op     string    `json:"op"` // "observe" | "act"
	TookMS int64     `json:"took_ms"`
	URL    string    `json:"url,omitempty"`

	// observe
	Candidates int         `json:"candidates,omitempty"` // shortlist length
	Total      int         `json:"total,omitempty"`
	Alerts     []string    `json:"alerts,omitempty"`
	Dialogs    []string    `json:"dialogs,omitempty"` // the dialogs the page has open, by name
	Modal      string      `json:"modal,omitempty"`   // the one of them the shortlist was cut down to
	Loading    bool        `json:"loading,omitempty"`
	Truncated  bool        `json:"truncated,omitempty"`
	TextBytes  int         `json:"text_bytes,omitempty"`
	TextAround string      `json:"text_around,omitempty"`
	Hints      []string    `json:"hints,omitempty"`
	Shortlist  []Candidate `json:"shortlist,omitempty"` // the candidates as returned; Value is already "" for Secret

	// act
	Kind      ActionKind `json:"kind,omitempty"`
	Ref       string     `json:"ref,omitempty"`
	Target    string     `json:"target,omitempty"` // role + name of the ref as of the last observation, for reading
	Until     WaitUntil  `json:"until,omitempty"`
	Clear     bool       `json:"clear,omitempty"`
	Submit    bool       `json:"submit,omitempty"`
	OK        bool       `json:"ok"`
	Code      ErrorCode  `json:"code,omitempty"`
	Navigated bool       `json:"navigated,omitempty"`
	Quiet     bool       `json:"quiet,omitempty"`
}

// StreamMessage is one control or input message. The takeover lock lives in
// the daemon, which relays the stream: it forwards input only from the lock
// holder and drops everyone else's, so whatever reaches the sidecar is
// dispatched. **Input is never logged, by either half.**
type StreamMessage struct {
	Type StreamType `json:"type"`

	// meta
	Width  int    `json:"width,omitempty"`
	Height int    `json:"height,omitempty"`
	Tabs   []Tab  `json:"tabs,omitempty"`
	Holder string `json:"holder,omitempty"`

	// pointer / wheel: coordinates in page CSS pixels of the streamed frame
	Event  string  `json:"event,omitempty"` // pointer: move | down | up; key: down | up
	X      float64 `json:"x,omitempty"`
	Y      float64 `json:"y,omitempty"`
	Button string  `json:"button,omitempty"` // left | middle | right
	Clicks int     `json:"clicks,omitempty"`
	DeltaX float64 `json:"delta_x,omitempty"`
	DeltaY float64 `json:"delta_y,omitempty"`

	// key: DOM KeyboardEvent.key and .code; Modifiers is the CDP bit field
	// (1 alt, 2 ctrl, 4 meta, 8 shift)
	Key       string `json:"key,omitempty"`
	Code      string `json:"code,omitempty"`
	Modifiers int    `json:"modifiers,omitempty"`

	// text
	Text string `json:"text,omitempty"`

	// tab
	Tab string `json:"tab,omitempty"`

	// quality: viewer hint, clamped by the sidecar
	JPEGQuality int `json:"jpeg_quality,omitempty"`
	MaxFPS      int `json:"max_fps,omitempty"`

	// activity: the sidecar's account of one call it served
	Activity *Activity `json:"activity,omitempty"`
}

func (e *Error) Error() string {
	if e.Detail == "" {
		return string(e.Code)
	}
	return string(e.Code) + ": " + e.Detail
}
