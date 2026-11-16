package page

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
	"time"
)

// A bot wall — a CAPTCHA challenge, a "verify you are human" page, Chromium's
// own network error page — is recognised and reported, never solved: nothing
// below this file clicks through one. What the executor does about a blocker
// it is told about is a decision made elsewhere, not a click from here.
//
// THE RULE of this file: the script only *finds*. It runs in a fresh isolated
// world (cdp.EvalIsolated — never the main world, never Runtime.enable), reads
// the top frame only, and returns one of a fixed set of words. Nothing on the
// page is ever activated, typed into, or written back to.

// The fixed words LastBotWall reports.
const (
	// BotWall — a bot-detection challenge is in the way.
	BotWall = "botwall"
	// Blocked — Chromium's own error page is what loaded.
	Blocked = "blocked"
)

// botWallBudget bounds one check, so an action's own timeout is not spent
// looking for a wall that is not there.
const botWallBudget = 2 * time.Second

// botWords are recognised, never acted on. They are matched against the title
// and the headings only — never against the page's body text, which is
// content this layer does not read for its own purposes.
var botWords = []string{
	"verify you are human", "verifying you are human", "are you a robot",
	"i'm not a robot", "im not a robot", "checking your browser",
	"unusual traffic", "access denied", "attention required",
}

// botSelectors are the challenge widgets themselves.
var botSelectors = []string{
	`iframe[src*="recaptcha"]`,
	`iframe[src*="hcaptcha"]`,
	`iframe[src*="challenges.cloudflare.com"]`,
	`.g-recaptcha`,
	`.h-captcha`,
	`#challenge-running`,
	`#cf-challenge-running`,
	`#px-captcha`,
}

// errorSelectors are Chromium's own error page ("This page has been
// blocked", a network error). It is reported, not repaired.
var errorSelectors = []string{`#main-frame-error`, `body.neterror`, `#error-information-popup-container`}

// botWallSpec is what the script is given. Every field of it comes from the
// tables above — never from the page.
type botWallSpec struct {
	Bot      []string `json:"bot"`
	BotSel   []string `json:"botSel"`
	ErrorSel []string `json:"errorSel"`
}

// botWallHit is what the script returns: a fixed word from the tables above
// and the rule that matched, never a string the page wrote.
type botWallHit struct {
	Kind string `json:"kind"` // "botwall", "blocked" or ""
	Rule string `json:"rule"`
}

// botWallExpr is the expression one check evaluates: the constant script
// applied to the table, as JSON.
func botWallExpr() (string, error) {
	b, err := json.Marshal(botWallSpec{Bot: botWords, BotSel: botSelectors, ErrorSel: errorSelectors})
	if err != nil {
		return "", err
	}
	return botWallJS + "(" + string(b) + ")", nil
}

// botWallJS looks at the top document only: the challenge widgets it names
// are markers in the page that holds them, not something that needs entering
// a cross-origin frame to see, and Chromium's own error page is never
// anything but the top document.
const botWallJS = `(function (spec) {
  var out = {kind: "", rule: ""};
  for (var i = 0; i < spec.botSel.length; i++) {
    var walls = [];
    try { walls = document.querySelectorAll(spec.botSel[i]); } catch (e) { walls = []; }
    for (var j = 0; j < walls.length; j++) {
      var w = walls[j];
      // An invisible reCAPTCHA or hCaptcha guards a form without ever being
      // shown: its frame says so in its own address, and a page that merely
      // carries one is not a wall. A widget that is not laid out at all is
      // not one either.
      var src = (w.getAttribute && w.getAttribute("src")) || "";
      if (src.indexOf("size=invisible") >= 0) continue;
      var box = w.getBoundingClientRect ? w.getBoundingClientRect() : null;
      if (box && (box.width < 1 || box.height < 1)) continue;
      out.kind = "botwall"; out.rule = "widget"; return out;
    }
  }
  var heads = (document.title || "");
  var hs = document.querySelectorAll("h1, h2");
  for (var i = 0; i < hs.length && i < 4; i++) heads += " " + (hs[i].textContent || "");
  heads = heads.replace(/\s+/g, " ").trim().toLowerCase();
  for (var i = 0; i < spec.bot.length; i++) {
    if (heads.indexOf(spec.bot[i]) >= 0) { out.kind = "botwall"; out.rule = "wording"; return out; }
  }
  for (var i = 0; i < spec.errorSel.length; i++) {
    if (document.querySelector(spec.errorSel[i])) { out.kind = "blocked"; out.rule = "neterror"; return out; }
  }
  return out;
})`

// ── the check ────────────────────────────────────────────────────────────────

// LastBotWall is the fixed word for what the agent's current page had in the
// way: botwall, blocked, or "" for a page that had neither. It is reset when
// a check runs on a new URL, so it always describes the page the executor is
// looking at.
func (e *Engine) LastBotWall(agent string) string {
	s, err := e.session(agent)
	if err != nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.botLast
}

// BotWalls is how many bot walls this engine has recognised since it started,
// over every agent. It is the counter the recall tool reports.
func (e *Engine) BotWalls() int64 { return e.botWallCount.Load() }

// recordBotWall files the fixed word of a check against the URL the page was
// on when it ran.
func (s *session) recordBotWall(rawURL, word string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.botURL, s.botLast = rawURL, word
}

// checkBotWall looks once at the active tab's top document and returns the
// fixed word it found, "" when the page had neither. Every failure below is
// silent by design: a page that refuses an isolated world, a frame that went
// away, are both "nothing found here", never an error the caller has to
// handle.
func (e *Engine) checkBotWall(ctx context.Context, at *attachment, s *session, t *tab) string {
	if ctx.Err() != nil {
		return ""
	}
	raw := at.tabURL(ctx, t)
	if _, ok := botWallHost(raw); !ok {
		return ""
	}
	expr, err := botWallExpr()
	if err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, botWallBudget)
	defer cancel()

	tree, err := at.conn.Session(t.sessionID).GetFrameTree(ctx)
	if err != nil {
		return ""
	}
	var hit botWallHit
	if err := at.conn.EvalIsolated(ctx, t.sessionID, tree.Frame.ID, expr, &hit); err != nil {
		return ""
	}
	word := ""
	switch hit.Kind {
	case "botwall":
		word = BotWall
	case "blocked":
		word = Blocked
	default:
		s.recordBotWall(raw, "")
		return ""
	}
	e.botWallCount.Add(1)
	e.log.Info("browser: bot wall", "agent", s.agent, "kind", word, "rule", hit.Rule)
	s.recordBotWall(raw, word)
	return word
}

// botWallHost is the host of a page worth a check: about:blank, a data URL
// and Chromium's own schemes carry no site and are left alone.
func botWallHost(raw string) (string, bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", false
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
	default:
		return "", false
	}
	host := bareHost(u.Hostname())
	return host, host != ""
}
