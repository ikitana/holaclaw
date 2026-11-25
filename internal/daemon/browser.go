package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"time"

	"holaclaw/internal/browser"
	"holaclaw/internal/browser/client"
	"holaclaw/internal/browser/live"
	"holaclaw/internal/event"
	"holaclaw/internal/tool"
)

// The daemon's half of the browser split (docs/browser.md): the executor and
// the sidecar client are assembled by internal/app, and the daemon adds the
// two things only it has — the approval framework and the live view. Nothing
// here ever sees page text: a takeover notice is built from the agent name and
// a closed-set blocker, and the screenshot travels as a blob, never as text.

// browserHealthTimeout bounds the one probe at start. A sidecar that is not
// up yet is a warning: the daemon serves everything else fine without it, and
// systemd will start the unit.
const browserHealthTimeout = 2 * time.Second

// browserWiring is what setupBrowser built, or nil when config.toml has no
// [browser] table.
type browserWiring struct {
	client *client.Client
	live   *live.Service
}

// setupBrowser wires the live view into the Control API and the approval
// framework into the executor, then says in the log what the sidecar found.
func (d *Daemon) setupBrowser(ctx context.Context) {
	c := d.App.BrowserClient()
	if c == nil {
		return
	}
	svc := live.New(live.Options{
		Sidecar: live.FromClient(c), Events: d.App.Events, Logger: d.log,
	})
	d.browser = &browserWiring{client: c, live: svc}
	d.Backend.Browser = svc
	d.App.SetBrowserHumans(humans{d})
	d.probeBrowser(ctx)
}

// probeBrowser asks the sidecar once what it found. A missing sidecar is
// never a start failure: the browser is one tool among many.
func (d *Daemon) probeBrowser(ctx context.Context) {
	hctx, cancel := context.WithTimeout(ctx, browserHealthTimeout)
	defer cancel()
	socket := d.App.Config.Browser.Socket
	h, err := d.browser.client.Health(hctx)
	switch {
	case err != nil:
		d.log.Warn("browser sidecar is not reachable", "socket", socket,
			"err", err, "fix", "systemctl start holaclaw-browser")
	case h.Problem != "":
		d.log.Warn("browser sidecar cannot run a browser", "socket", socket, "problem", h.Problem)
	default:
		d.log.Info("browser sidecar", "socket", socket, "version", h.Version,
			"binary", h.Binary, "browser", h.Browser, "headless", h.Headless)
	}
}

// humans is browser.Humans on the daemon's own machinery: a risky step is a
// once-only tool approval, so every channel and client already shows it, and a
// blocker that needs a person is a takeover request on the live view.
type humans struct{ d *Daemon }

// Approve asks owner / admins to allow one step. The summary is the
// executor's own fixed wording; kind is already part of it. The answer is
// once-only: a grant would cover every browser.do call, never the next step.
func (h humans) Approve(ctx context.Context, t browser.Task, _ browser.RiskKind, summary string) (bool, error) {
	a, err := h.d.Approvals.Request(ctx, tool.Approval{
		Tool:      "browser.do",
		Summary:   summary,
		OnceOnly:  true,
		Agent:     t.Agent,
		TraceID:   t.TraceID,
		SessionID: t.SessionID,
	})
	if err != nil {
		return false, err
	}
	// Denied, expired and timed out are one answer: no.
	return a.Status == tool.ApprovalApproved, nil
}

// Takeover asks a person to get past a blocker in the live view. false means
// nobody did in time, which fails the run (docs/browser.md).
func (h humans) Takeover(ctx context.Context, t browser.Task, why browser.Blocker) (bool, error) {
	if h.d.browser == nil {
		return false, nil
	}
	// A person can only help through the live view. For a login, captcha
	// or 2FA the request is worth announcing and waiting for; for anything
	// else, waiting on an empty viewer is a silent stall — fail at once.
	switch why {
	case browser.BlockLogin, browser.BlockCaptcha, browser.Block2FA:
	default:
		if h.d.browser.live.Watchers(t.Agent) == 0 {
			return false, nil
		}
	}
	return h.d.browser.live.RequestTakeover(ctx, t.Agent, why, t.TraceID, t.SessionID)
}

// --- chat channels ---------------------------------------------------------------

// takeoverNotice shows a takeover request in the conversation the run came
// from, the way an approval is shown: to owner / admins, on a platform we
// have an adapter for. Driving needs the GUI — this is the nudge to open it.
func (cs *channels) takeoverNotice(ctx context.Context, e event.Event) {
	var attrs struct {
		Agent string `json:"agent"`
		Why   string `json:"why"`
	}
	if json.Unmarshal(e.Attrs, &attrs) != nil || attrs.Agent == "" {
		return
	}
	sess, err := cs.d.App.Store.GetSession(ctx, e.SessionID)
	if err != nil {
		return
	}
	if _, ok := cs.byName[sess.Origin.Channel]; !ok || !cs.turnIsAdmin(ctx, sess, e.TraceID) {
		return
	}
	msg := event.Message{
		Schema: event.SchemaVersion, ID: event.NewID(), SessionID: sess.ID, TraceID: e.TraceID,
		Role: event.RoleAssistant, Text: takeoverText(attrs.Agent, attrs.Why),
		CreatedAt: time.Now().UTC(),
	}
	start := time.Now()
	if att, err := cs.d.browserShot(ctx, attrs.Agent); err == nil {
		msg.Attachments = []event.Attachment{att}
		// It goes to a person, not to the agent, and the event never carries it.
		cs.d.App.Events.Emit(ctx, event.New(event.BrowserScreenshot, e.TraceID, e.SessionID, "",
			browser.ScreenshotAttrs{Why: browser.ShotTakeover, Size: int(att.Size), TookMS: time.Since(start).Milliseconds()}))
	}
	if err := cs.send(ctx, sess.Origin, "", e.TraceID, msg); err != nil {
		cs.d.log.Warn("takeover notice not delivered", "channel", sess.Origin.Channel, "err", err)
	}
}

// takeoverText is a fixed sentence over a closed set. Nothing the page said
// is in it — that is the whole point of naming the blocker by its label.
func takeoverText(agentName, why string) string {
	return fmt.Sprintf("The %s browser is waiting for a person: %s. Open the live view and take over, "+
		"then hand it back to let the run continue. Driving needs the GUI.", agentName, blockerPhrase(why))
}

func blockerPhrase(why string) string {
	switch browser.Blocker(why) {
	case browser.BlockLogin:
		return "a site is asking it to log in"
	case browser.BlockCaptcha:
		return "a captcha is in the way"
	case browser.Block2FA:
		return "a two-factor code is needed"
	default:
		return "it cannot get past the page on its own"
	}
}

// browserShot stores one screenshot of the agent's browser as a blob and
// returns it as an attachment. Every failure is the caller's cue to send the
// notice without a picture.
func (d *Daemon) browserShot(ctx context.Context, agentName string) (event.Attachment, error) {
	if d.browser == nil {
		return event.Attachment{}, errNoBrowserWiring
	}
	shot, err := d.browser.client.Screenshot(ctx, agentName)
	if err != nil {
		return event.Attachment{}, err
	}
	const mime = "image/jpeg"
	b, err := d.App.Store.PutBlob(ctx, bytes.NewReader(shot), mime)
	if err != nil {
		return event.Attachment{}, err
	}
	return event.Attachment{
		ID:     event.NewID(),
		BlobID: b.ID,
		SHA256: b.ID, // blob ids are content addresses: hex sha256
		MIME:   mime,
		Name:   agentName + "-browser.jpg",
		Size:   b.Size,
		Kind:   event.KindFromMIME(mime),
	}, nil
}

// errNoBrowserWiring is the internal "there is no browser here" of this file.
var errNoBrowserWiring = fmt.Errorf("daemon: no browser is configured")

// WaitReleased pauses the run while a person holds the takeover lock: the
// executor makes no observation until the browser is handed back.
func (h humans) WaitReleased(ctx context.Context, t browser.Task) error {
	if h.d.browser == nil {
		return nil
	}
	return h.d.browser.live.WaitReleased(ctx, t.Agent)
}
