package page

import (
	"context"
	"net/url"
	"strings"
	"time"

	"holaclaw/internal/browser/cdp"
	"holaclaw/internal/browser/proto"
)

// The allow-list is the network, not advice (docs/browser.md). Every Document request of
// every attached target pauses in Fetch and is failed when its host is not
// allowed, which covers the four ways a page can move: a navigation, a
// redirect (it pauses again), a popup, and an iframe document.

// documentPatterns pauses document requests only. Sub-resources are left
// alone: an image from a CDN is not a navigation, and pausing everything would
// put this layer in the path of every byte a page loads.
var documentPatterns = []cdp.RequestPattern{
	{URLPattern: "*", ResourceType: "Document", RequestStage: "Request"},
}

// leaveTimeout bounds the walk back off a page that left the list: it is
// cleanup, not the action, and it must not hold the action's caller up.
const leaveTimeout = 3 * time.Second

// normaliseAllow lowercases the list, drops empty entries, a leading dot and
// any port. The zero-length result means "no list": any site.
func normaliseAllow(allow []string) []string {
	out := make([]string, 0, len(allow))
	for _, h := range allow {
		h = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(h)), ".")
		if h = bareHost(h); h == "" {
			continue
		}
		out = append(out, h)
	}
	return out
}

// hostAllowed matches a host against a normalised list: the host itself or any
// subdomain of an entry. An empty list allows everything.
func hostAllowed(allow []string, host string) bool {
	if len(allow) == 0 {
		return true
	}
	host = bareHost(host)
	for _, entry := range allow {
		if host == entry || strings.HasSuffix(host, "."+entry) {
			return true
		}
	}
	return false
}

// bareHost reduces a host as somebody may write it — bracketed, with a port,
// with a root dot — to the bare form url.Hostname() produces, so an entry of
// the list and a host off the wire are compared in the same shape. An IPv6
// literal without brackets is recognised by its second colon, so its last
// group is never taken for a port.
func bareHost(host string) string {
	host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	if strings.HasPrefix(host, "[") {
		if i := strings.LastIndex(host, "]"); i > 0 {
			return host[1:i] // [::1] and [::1]:8443 alike
		}
		return ""
	}
	if strings.Count(host, ":") == 1 {
		if i := strings.LastIndex(host, ":"); i > 0 {
			host = host[:i]
		}
	}
	return host
}

// checkNavigable is the pre-flight ActNavigate makes: a URL outside the list is
// refused without a request ever being made, and a scheme this layer does not
// drive is a bad action rather than a blocked one.
func checkNavigable(allow []string, raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" && u.Scheme != "about" {
		return errf(proto.ErrBadAction, "url")
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
	case "about":
		if u.Opaque != "blank" && u.Path != "blank" {
			return errf(proto.ErrBadAction, "scheme")
		}
		return nil
	default:
		return errf(proto.ErrBadAction, "scheme")
	}
	if !hostAllowed(allow, u.Hostname()) {
		return errf(proto.ErrNotAllowed, "host")
	}
	return nil
}

// resultAllowed is the check an action makes on the URL it ended on. Only an
// http(s) URL carries a site: about:blank and Chromium's own error scheme name
// no host and are always fine.
func (s *session) resultAllowed(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return true // not a URL this layer navigated to; the wire rules stand
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
		return s.allowed(u.Hostname())
	default:
		return true
	}
}

// leaveDisallowed takes the tab off a page outside the list: back where there
// is history, about:blank where there is not. Either way the executor's next
// observation is of a page the list allows.
func (e *Engine) leaveDisallowed(ctx context.Context, at *attachment, sess *cdp.Session, t *tab) {
	// On its own deadline: the action's may already have run out, and a page
	// outside the list is left behind either way.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), leaveTimeout)
	defer cancel()

	lifecycle, unwatch := at.watch(t)
	defer unwatch()
	if err := e.back(ctx, sess); err != nil {
		if _, _, err := sess.Navigate(ctx, "about:blank"); err != nil {
			e.log.Warn("browser: leave a blocked page", "agent", at.s.agent, "err", err)
			return
		}
	}
	at.settle(ctx, lifecycle, true)
	// Going back can land on another page that left the list in the same way;
	// about:blank always ends it.
	if !at.s.resultAllowed(at.tabURL(ctx, t)) {
		if _, _, err := sess.Navigate(ctx, "about:blank"); err != nil {
			e.log.Warn("browser: leave a blocked page", "agent", at.s.agent, "err", err)
		}
	}
}

// onRequestPaused is the enforcement point. It runs on the subscription's own
// goroutine, so the CDP calls it makes cannot stall the connection.
func (a *attachment) onRequestPaused(sess *cdp.Session, t *tab, ev cdp.RequestPausedEvent) {
	ctx, cancel := context.WithTimeout(context.Background(), attachTimeout)
	defer cancel()

	if ev.ResourceType != "Document" {
		_ = sess.ContinueRequest(ctx, ev.RequestID)
		return
	}
	host := ""
	if u, err := url.Parse(ev.Request.URL); err == nil {
		host = u.Hostname()
	}
	if host == "" || a.s.allowed(host) {
		if err := sess.ContinueRequest(ctx, ev.RequestID); err != nil {
			a.s.e.log.Warn("browser: continue request", "agent", a.s.agent, "err", err)
		}
		return
	}
	// Failed, not redirected: the page sees a network error, which is what a
	// blocked navigation is. The URL is page content and is not logged.
	if err := sess.FailRequest(ctx, ev.RequestID, "BlockedByClient"); err != nil {
		a.s.e.log.Warn("browser: block request", "agent", a.s.agent, "err", err)
	}
	a.s.e.log.Info("browser: navigation blocked by the allow-list", "agent", a.s.agent)

	// A window the page opened that never reached an allowed document is a
	// window nobody asked for: close it rather than leave a blank tab behind.
	a.mu.Lock()
	orphan := t != nil && t.opener != "" && !t.navigated
	targetID := ""
	if orphan {
		targetID = t.targetID
		a.dropTabLocked(targetID)
	}
	a.mu.Unlock()
	if orphan {
		if err := a.bs.CloseTarget(ctx, targetID); err != nil {
			a.s.e.log.Warn("browser: close blocked popup", "agent", a.s.agent, "err", err)
		}
	}
}
