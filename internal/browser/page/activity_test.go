package page

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"holaclaw/internal/browser/proto"
)

// The engine's account of an observation is what it answered: the counts, the
// page's own announcements, and the shortlist as the executor got it.
func TestObserveActivityIsWhatWasAnswered(t *testing.T) {
	start := time.Now()
	obs := proto.Observation{
		URL:     "https://example.test/search",
		Loading: true,
		Total:   61,
		Text:    strings.Repeat("x", 4096),
		Alerts:  []string{"Pick a date"},
		Candidates: []proto.Candidate{
			{Ref: "g3r7", Role: "button", Name: "Search"},
			{Ref: "g3r9", Role: "textbox", Name: "Card number", Secret: true},
		},
		Truncated: true,
	}
	req := proto.ObserveRequest{Hints: []string{"search"}, TextAround: "g2r4", TextBytes: 4096}
	a := observeActivity(start, req, obs, nil)

	if a.Op != "observe" || !a.OK || a.Code != "" {
		t.Fatalf("activity = %+v", a)
	}
	if a.Candidates != 2 || a.Total != 61 || a.TextBytes != 4096 || !a.Truncated || !a.Loading {
		t.Fatalf("activity = %+v", a)
	}
	if a.TextAround != "g2r4" || len(a.Hints) != 1 || len(a.Alerts) != 1 || a.URL != obs.URL {
		t.Fatalf("activity = %+v", a)
	}
	if len(a.Shortlist) != 2 || a.Shortlist[1].Value != "" || !a.Shortlist[1].Secret {
		t.Fatalf("shortlist = %+v", a.Shortlist)
	}

	// A failure is recorded too, with the code and nothing invented.
	bad := observeActivity(start, req, proto.Observation{}, errf(proto.ErrNoSession, "not open"))
	if bad.OK || bad.Code != proto.ErrNoSession {
		t.Fatalf("failed observe = %+v", bad)
	}
}

// An action's account says what was done to which element, and never what was
// typed or which key was pressed — the Activity has nowhere to put either.
func TestActActivityNeverCarriesTypedTextOrKeys(t *testing.T) {
	start := time.Now()
	action := proto.Action{
		Kind: proto.ActType, Ref: "g3r9", Text: "hunter2-secret", Key: "Enter",
		Clear: true, Submit: true,
	}
	res := proto.ActResult{OK: true, URL: "https://example.test/search", TookMS: 812}
	a := actActivity(start, action, res, `combobox "From"`)

	if a.Op != "act" || a.Kind != proto.ActType || a.Ref != "g3r9" || a.Target != `combobox "From"` {
		t.Fatalf("activity = %+v", a)
	}
	if !a.Clear || !a.Submit || !a.OK || a.TookMS != 812 {
		t.Fatalf("activity = %+v", a)
	}
	raw, err := json.Marshal(proto.StreamMessage{Type: proto.StreamActivity, Activity: &a})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), "hunter2") || strings.Contains(string(raw), "Enter") {
		t.Fatalf("the action's text or key reached the viewer: %s", raw)
	}
	// Not by redaction but by construction: the type has no field for either.
	for _, name := range []string{"Text", "Key"} {
		if _, ok := reflect.TypeOf(proto.Activity{}).FieldByName(name); ok {
			t.Fatalf("proto.Activity has a %s field; a live viewer must not be able to show one", name)
		}
	}
}

// A navigate that never settled still says where it was going, and a failed
// action carries the code.
func TestActActivityOfAFailedNavigate(t *testing.T) {
	a := actActivity(time.Now(),
		proto.Action{Kind: proto.ActNavigate, URL: "https://elsewhere.test/"},
		proto.ActResult{OK: false, Code: proto.ErrNotAllowed}, "")
	if a.URL != "https://elsewhere.test/" || a.OK || a.Code != proto.ErrNotAllowed {
		t.Fatalf("activity = %+v", a)
	}
}

// The ring keeps the last activityRing entries and no more.
func TestActivityRingCaps(t *testing.T) {
	s := &session{}
	for i := 0; i < activityRing+25; i++ {
		s.record(proto.Activity{Op: "act", Ref: refName(1, i)})
	}
	got, _, cancel := s.subscribe()
	defer cancel()
	if len(got) != activityRing {
		t.Fatalf("ring holds %d entries, want %d", len(got), activityRing)
	}
	if got[0].Ref != refName(1, 25) || got[len(got)-1].Ref != refName(1, activityRing+24) {
		t.Fatalf("ring = %s … %s, want the newest %d", got[0].Ref, got[len(got)-1].Ref, activityRing)
	}
}

// A subscriber is replayed what happened before it joined and then gets what
// comes after; unsubscribing stops it.
func TestActivitySubscriberSeesTheRingThenTheNewEntries(t *testing.T) {
	s := &session{}
	s.record(proto.Activity{Op: "observe", Total: 1})

	replay, ch, cancel := s.subscribe()
	if len(replay) != 1 || replay[0].Total != 1 {
		t.Fatalf("replay = %+v", replay)
	}
	s.record(proto.Activity{Op: "act", Ref: "g1r2"})
	select {
	case a := <-ch:
		if a.Ref != "g1r2" {
			t.Fatalf("subscriber got %+v", a)
		}
	case <-time.After(time.Second):
		t.Fatal("the subscriber was not told")
	}

	cancel()
	s.record(proto.Activity{Op: "act", Ref: "g1r3"})
	select {
	case a := <-ch:
		t.Fatalf("an unsubscribed channel still got %+v", a)
	case <-time.After(20 * time.Millisecond):
	}
}

// A subscriber that does not read loses its oldest entries, never the newest:
// a viewer that falls behind is caught up with what is happening now.
func TestActivityFullSubscriberDropsTheOldest(t *testing.T) {
	s := &session{}
	_, ch, cancel := s.subscribe()
	defer cancel()

	total := activityBuffer + 10
	for i := 0; i < total; i++ {
		s.record(proto.Activity{Op: "act", Ref: refName(2, i)})
	}
	if len(ch) != activityBuffer {
		t.Fatalf("queue holds %d, want the buffer %d", len(ch), activityBuffer)
	}
	first := <-ch
	if first.Ref != refName(2, total-activityBuffer) {
		t.Fatalf("oldest queued = %s, want %s", first.Ref, refName(2, total-activityBuffer))
	}
	var last proto.Activity
	for len(ch) > 0 {
		last = <-ch
	}
	if last.Ref != refName(2, total-1) {
		t.Fatalf("newest queued = %s, want %s", last.Ref, refName(2, total-1))
	}
}

// Observe and Act record when they return, whether they worked or not: a
// session with no browser behind it still leaves the trail that says so.
func TestObserveAndActRecordEvenWhenTheyFail(t *testing.T) {
	e := testEngineNoBrowser(t)
	s := &session{e: e, agent: "alpha"}
	e.sessions["alpha"] = s

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if _, err := e.Observe(ctx, "alpha", proto.ObserveRequest{Hints: []string{"search"}}); err == nil {
		t.Fatal("Observe with no browser succeeded")
	}
	if res, err := e.Act(ctx, "alpha", proto.Action{Kind: proto.ActType, Ref: "g1r1", Text: "hunter2-secret"}); err == nil || res.OK {
		t.Fatalf("Act with no browser = %+v, %v", res, err)
	}

	got, _, unsubscribe := s.subscribe()
	defer unsubscribe()
	if len(got) != 2 || got[0].Op != "observe" || got[1].Op != "act" {
		t.Fatalf("recorded %+v", got)
	}
	if got[0].OK || got[0].Code == "" || got[1].OK || got[1].Code == "" {
		t.Fatalf("failures were recorded as successes: %+v", got)
	}
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), "hunter2") {
		t.Fatalf("the typed text was recorded: %s", raw)
	}
}

// describeRef is how an action's target is named for a reader: the role and
// the name of the last observation, and nothing for a ref that is not one.
func TestDescribeRef(t *testing.T) {
	at := &attachment{refDesc: map[string]string{"g1r1": `button "Search"`}}
	if got := at.describeRef("g1r1"); got != `button "Search"` {
		t.Fatalf("describeRef = %q", got)
	}
	if got := at.describeRef("g1r9"); got != "" {
		t.Fatalf("describeRef of an unknown ref = %q", got)
	}
	if got := at.describeRef(""); got != "" {
		t.Fatalf("describeRef of no ref = %q", got)
	}
}

// One entry on the wire is a StreamMessage of its own type, so the viewer can
// tell the engine's account apart from the tab list.
func TestStreamActivityMessage(t *testing.T) {
	v := &recordViewer{}
	st := &stream{v: v}
	if err := st.activity(context.Background(), proto.Activity{Op: "observe", Total: 3}); err != nil {
		t.Fatalf("activity: %v", err)
	}
	if len(v.metas) != 1 {
		t.Fatalf("%d messages", len(v.metas))
	}
	m := v.metas[0]
	if m.Type != proto.StreamActivity || m.Activity == nil || m.Activity.Total != 3 {
		t.Fatalf("message = %+v", m)
	}
	if m.Tabs != nil || m.Width != 0 {
		t.Fatalf("an activity message carried view state: %+v", m)
	}
}

// recordViewer is a Viewer that keeps what it was sent.
type recordViewer struct {
	metas []proto.StreamMessage
}

func (v *recordViewer) Frame(context.Context, []byte, int, int) error { return nil }

func (v *recordViewer) Meta(_ context.Context, m proto.StreamMessage) error {
	v.metas = append(v.metas, m)
	return nil
}
