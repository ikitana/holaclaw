package page

import (
	"errors"
	"time"

	"holaclaw/internal/browser/proto"
)

// The engine's own account of what it served, for a live viewer (docs/browser.md). It is
// the same rule as everywhere else in this layer: what a page said goes into
// the struct that travels to the viewer and nowhere else, and what the
// executor asked to be typed or pressed does not travel at all — an Activity
// carries neither Action.Text nor Action.Key, by construction rather than by
// redaction.
//
// It is a live view's companion, not a log: the ring is small, a subscriber
// that falls behind loses its oldest entries, and nothing here is persisted.

const (
	// activityRing is how many entries a session keeps for a viewer that
	// joins after the fact.
	activityRing = 50
	// activityBuffer is one subscriber's queue. A full one drops its oldest
	// entry: a viewer that cannot keep up misses the beginning, not the end.
	activityBuffer = 64
)

// record appends one entry to the session's ring and offers it to every
// subscriber. It is called after the engine call it describes has returned,
// so the act mutex is never held while an entry is sent.
func (s *session) record(a proto.Activity) {
	s.amu.Lock()
	s.acts = append(s.acts, a)
	if len(s.acts) > activityRing {
		s.acts = append(s.acts[:0], s.acts[len(s.acts)-activityRing:]...)
	}
	subs := make([]chan proto.Activity, 0, len(s.subs))
	for ch := range s.subs {
		subs = append(subs, ch)
	}
	s.amu.Unlock()

	for _, ch := range subs {
		select {
		case ch <- a:
		default:
			select {
			case <-ch: // the oldest goes, so the newest arrives
			default:
			}
			select {
			case ch <- a:
			default:
			}
		}
	}
}

// subscribe returns the entries recorded so far and a channel of the ones to
// come, plus the cancel that unsubscribes. The replay is taken under the same
// lock as the subscription, so nothing is missed or seen twice.
func (s *session) subscribe() (replay []proto.Activity, ch <-chan proto.Activity, cancel func()) {
	c := make(chan proto.Activity, activityBuffer)
	s.amu.Lock()
	replay = append(replay, s.acts...)
	if s.subs == nil {
		s.subs = map[chan proto.Activity]struct{}{}
	}
	s.subs[c] = struct{}{}
	s.amu.Unlock()
	return replay, c, func() {
		s.amu.Lock()
		delete(s.subs, c)
		s.amu.Unlock()
	}
}

// observeActivity is the account of one Observe: what was asked for and what
// the page answered, shortlist included.
func observeActivity(start time.Time, req proto.ObserveRequest, obs proto.Observation, err error) proto.Activity {
	a := proto.Activity{
		At:         start,
		Op:         "observe",
		TookMS:     time.Since(start).Milliseconds(),
		URL:        obs.URL,
		Candidates: len(obs.Candidates),
		Total:      obs.Total,
		Alerts:     obs.Alerts,
		Dialogs:    obs.Dialogs,
		Modal:      obs.Modal,
		Loading:    obs.Loading,
		Truncated:  obs.Truncated,
		TextBytes:  len(obs.Text),
		TextAround: req.TextAround,
		Hints:      req.Hints,
		Shortlist:  obs.Candidates,
		OK:         err == nil,
	}
	a.Code = codeOf(err)
	return a
}

// actActivity is the account of one Act. The action's Text and Key are not
// read here at all: they are what a person's page is being told, and the
// viewer is not part of that conversation.
func actActivity(start time.Time, action proto.Action, res proto.ActResult, target string) proto.Activity {
	a := proto.Activity{
		At:        start,
		Op:        "act",
		TookMS:    res.TookMS,
		URL:       res.URL,
		Kind:      action.Kind,
		Ref:       action.Ref,
		Target:    target,
		Until:     action.Until,
		Clear:     action.Clear,
		Submit:    action.Submit,
		OK:        res.OK,
		Code:      res.Code,
		Navigated: res.Navigated,
		Quiet:     res.Quiet,
	}
	if a.TookMS == 0 {
		a.TookMS = time.Since(start).Milliseconds()
	}
	if a.URL == "" && action.Kind == proto.ActNavigate {
		// Nothing settled, so the URL that was asked for is the only one there
		// is to show.
		a.URL = action.URL
	}
	return a
}

// codeOf is the wire code of a failure, "" when there was none or when the
// error is not one of ours.
func codeOf(err error) proto.ErrorCode {
	if err == nil {
		return ""
	}
	var pe *proto.Error
	if errors.As(err, &pe) {
		return pe.Code
	}
	return proto.ErrBrowser
}

// describeRef is how the last observation named an element, for a viewer to
// read: `button "Search"`. Page content, so it travels in the Activity and
// nowhere else. "" when the ref is not one of the current observation's.
func (a *attachment) describeRef(ref string) string {
	if ref == "" {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.refDesc[ref]
}
