package browser

import (
	"sort"
	"strings"

	"holaclaw/internal/browser/proto"
)

// The trajectory of one run, as facts rather than as the model's own account
// of itself. Two failures of the M11 loop are what this is for: a run whose
// every move "works" — a calendar opens and closes, a menu toggles — earning
// budget while the request never gets closer, and a run with no way at all to
// notice a dead end.
//
// So progress is redefined here as something that happened to the page or to
// the run — a page never seen before, a goal score higher than any before it,
// an alert or an invalid mark the move cleared, a suggestion taken — and never
// as a move's own effect score, which is the loop grading its own homework.
// Being stuck is the same kind of fact: the page keeps coming back, an element
// keeps failing, the goal has not moved in three turns, the move call found
// nothing twice.
//
// Nothing in here is rendered or emitted. The names it keys elements by are
// the page's own words: they reach the next move call fenced, as the line that
// says what not to try again, and reach no log line and no trace event.

// stuckReason is the closed set of ways a run is stuck. Each is a word of this
// package, so it is safe on a trace and in a log line.
type stuckReason string

const (
	stuckCircling  stuckReason = "circling"  // the page keeps coming back to where it was
	stuckRepeating stuckReason = "repeating" // the same element failed twice
	stuckStalled   stuckReason = "stalled"   // goal_p has not risen for N move calls
	stuckNothing   stuckReason = "nothing"   // the move call found nothing to do twice
)

const (
	// goalGain is how much the goal score has to rise above everything before
	// it to count as progress. A score that wobbles by a hundredth between two
	// readings of the same page is noise, not a run getting closer.
	goalGain = 0.1
	// stallCalls is how many move calls may pass without the goal rising
	// before the run is one that is going nowhere.
	stallCalls = 3
	// failedTwice is how often a move on one element may score below the
	// effect floor before that element is one to stop trying.
	failedTwice = 2
	// trackedPages is how far back the fingerprints go: enough to see an
	// A,B,A,B toggle and the three readings of one page that never moves.
	trackedPages = 6
	// circleCalls is how many move calls a loop has to have cost before it is
	// worth calling one. A page read twice inside a single turn — settled,
	// picked from — is not a run in circles.
	circleCalls = 2
)

// track is what the run has actually done, as distinct from what it says it
// has done.
type track struct {
	fps []fingerprint // last trackedPages observed page fingerprints, oldest first
	// at is the move call each of those fingerprints was observed on, kept
	// alongside fps: the same page twice inside one turn is not a circle.
	at      []int
	goalMax float64 // high-water mark of goal_p
	goalAt  int     // move call at which goalMax last rose
	// failed counts, by element key (role and name), the moves on it that
	// scored below the effect floor. The names never leave this struct except
	// fenced, in the line that tells the next move call what not to try.
	failed map[string]int
	seen   map[string]bool // URLs visited
	noMove int
	// fired is the reasons already reported: each holds for the rest of the
	// run once it holds at all, and reporting it twice would spend a recovery
	// on something already answered.
	fired map[stuckReason]bool
	// calls is the last move call the tracker was told about, so a reset knows
	// where the run stands without being told again.
	calls int
}

func newTrack() *track {
	return &track{failed: map[string]int{}, seen: map[string]bool{}, fired: map[stuckReason]bool{}}
}

// observed books one reading of the page and reports whether its address is
// one the run has never been at — the plainest progress there is.
func (t *track) observed(o proto.Observation, movesCall int) bool {
	t.calls = movesCall
	t.fps = append(t.fps, fingerprintOf(o))
	t.at = append(t.at, movesCall)
	if len(t.fps) > trackedPages {
		t.fps, t.at = t.fps[len(t.fps)-trackedPages:], t.at[len(t.at)-trackedPages:]
	}
	if o.URL == "" || t.seen[o.URL] {
		return false
	}
	t.seen[o.URL] = true
	return true
}

// scored books the goal the decision model read off the page and reports
// whether it is higher than anything before it by more than noise.
func (t *track) scored(v verdict, movesCall int) bool {
	t.calls = movesCall
	if v.Goal < t.goalMax+goalGain {
		return false
	}
	t.goalMax, t.goalAt = v.Goal, movesCall
	return true
}

// failedOn books a move that did not do what it was for, against the element
// it was aimed at.
func (t *track) failedOn(c proto.Candidate) {
	t.failed[elementKey(c)]++
}

// cleared reports whether the move took something off the page that the page
// was saying before it: a complaint, or a field the site had marked invalid.
// A form that stops objecting is a run getting somewhere, whatever else the
// move did or did not change.
func (t *track) cleared(d diff) bool {
	return len(d.Unsaid) > 0 || len(d.Accepted) > 0
}

// stuck is the first reason that holds and has not been reported yet.
func (t *track) stuck(movesCall int) (stuckReason, bool) {
	t.calls = movesCall
	reasons := []struct {
		why   stuckReason
		holds bool
	}{
		{stuckCircling, t.circling()},
		{stuckRepeating, t.repeating()},
		{stuckStalled, movesCall >= stallCalls && movesCall-t.goalAt >= stallCalls},
		{stuckNothing, t.noMove >= maxNoMove},
	}
	for _, r := range reasons {
		if r.holds && !t.fired[r.why] {
			t.fired[r.why] = true
			return r.why, true
		}
	}
	return "", false
}

// circling reports the page coming back to where it was: the last four
// readings alternating between two pages, or the last three being the same
// page — and either way more than one move call spent on them.
func (t *track) circling() bool {
	n := len(t.fps)
	if n < 3 {
		return false
	}
	window := 0
	switch {
	case n >= 4 && t.fps[n-1] == t.fps[n-3] && t.fps[n-2] == t.fps[n-4] && t.fps[n-1] != t.fps[n-2]:
		window = 4
	case t.fps[n-1] == t.fps[n-2] && t.fps[n-2] == t.fps[n-3]:
		window = 3
	default:
		return false
	}
	return t.calls-t.at[n-window] >= circleCalls
}

// repeating reports an element a move has failed on twice.
func (t *track) repeating() bool {
	for _, n := range t.failed {
		if n >= failedTwice {
			return true
		}
	}
	return false
}

// avoid is the elements worth telling the next move call to leave alone, as
// `role "name"`, in a fixed order. Page-derived: the caller fences it.
func (t *track) avoid() []string {
	var out []string
	for k, n := range t.failed {
		if n < failedTwice {
			continue
		}
		role, name, _ := strings.Cut(k, "\x00")
		out = append(out, oneLine(role, 40)+" "+quote(name))
	}
	sort.Strings(out)
	return out
}

// reset is what a restart does to the tracker: the run is taking another route
// from the page it began on, so the pages it walked and the elements it failed
// on say nothing about the route ahead. The reasons already reported stay
// reported — a restart is not a fresh run — and the addresses stay seen when
// the caller says so, because arriving back at one of them is not progress.
func (t *track) reset(keepSeen bool) {
	t.fps, t.at = nil, nil
	t.goalMax, t.goalAt = 0, t.calls
	t.failed = map[string]int{}
	t.noMove = 0
	if !keepSeen {
		t.seen = map[string]bool{}
	}
}

// elementKey is what identifies an element across observations: its role and
// its accessible name, the same pair rebinding uses. Refs are per observation
// and are no use for it.
func elementKey(c proto.Candidate) string {
	return strings.ToLower(strings.TrimSpace(c.Role)) + "\x00" + c.Name
}

// stuckWords is what each reason says in this package's own words: the line
// that goes into the history the next move call reads, and into the state the
// strategy question is answered on.
var stuckWords = map[stuckReason]string{
	stuckCircling:  "the run is going in circles",
	stuckRepeating: "the same element has failed twice",
	stuckStalled:   "the request is no closer than it was three turns ago",
	stuckNothing:   effectNoMove,
}
