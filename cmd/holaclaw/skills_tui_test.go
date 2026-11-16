package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"holaclaw/internal/api"
	"holaclaw/internal/api/gen"
)

// The skills explorer is a plain Bubble Tea model, so every one of these
// drives it the way the runtime would — a window size, then messages — and
// reads what it drew. No daemon and no terminal are involved: the model talks
// to a fake skillsAPI that records what it was asked for.

// fakeSkills answers every call the explorer makes and remembers each one.
type fakeSkills struct {
	agents   []gen.AgentInfo
	byAgent  map[string][]gen.Skill
	full     map[string]gen.Skill // GetSkill, by name
	versions map[string]gen.Skill // GetSkillVersion, by name@version

	calls    []string
	removed  []string
	restored []string
	reverted []string

	listErr    error
	getErr     map[string]error // by name
	restoreErr error
}

func (f *fakeSkills) ListAgents(context.Context) ([]gen.AgentInfo, error) {
	f.calls = append(f.calls, "agents")
	return f.agents, nil
}

func (f *fakeSkills) ListSkills(_ context.Context, agent string) ([]gen.Skill, error) {
	f.calls = append(f.calls, "skills:"+agent)
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.byAgent[agent], nil
}

func (f *fakeSkills) GetSkill(_ context.Context, name, _ string) (gen.Skill, error) {
	f.calls = append(f.calls, "get:"+name)
	if err := f.getErr[name]; err != nil {
		return gen.Skill{}, err
	}
	s, ok := f.full[name]
	if !ok {
		return gen.Skill{}, &api.ClientError{Status: http.StatusNotFound, Code: "not_found", Message: "no skill " + name}
	}
	return s, nil
}

func (f *fakeSkills) GetSkillVersion(_ context.Context, name, _ string, version int) (gen.Skill, error) {
	key := fmt.Sprintf("%s@%d", name, version)
	f.calls = append(f.calls, "version:"+key)
	return f.versions[key], nil
}

func (f *fakeSkills) RemoveSkill(_ context.Context, name, agent string) error {
	f.calls = append(f.calls, "remove")
	f.removed = append(f.removed, agent+"/"+name)
	return nil
}

func (f *fakeSkills) RestoreSkill(_ context.Context, name, agent string) (gen.Skill, error) {
	f.calls = append(f.calls, "restore")
	if f.restoreErr != nil {
		return gen.Skill{}, f.restoreErr
	}
	f.restored = append(f.restored, agent+"/"+name)
	return gen.Skill{Name: name}, nil
}

func (f *fakeSkills) RevertSkill(_ context.Context, name, agent string, to int) (gen.Skill, error) {
	f.calls = append(f.calls, "revert")
	f.reverted = append(f.reverted, fmt.Sprintf("%s/%s>%d", agent, name, to))
	return gen.Skill{Name: name}, nil
}

// skillsOf reads a fixture the way the daemon would send it: the learned and
// requires blocks are anonymous structs in the generated code, and JSON is
// the one spelling of them a test can write.
func skillsOf(js string) []gen.Skill {
	var out []gen.Skill
	if err := json.Unmarshal([]byte(js), &out); err != nil {
		panic(err)
	}
	return out
}

func skillOf(js string) gen.Skill { return skillsOf("[" + js + "]")[0] }

// mainSkills is the fixture most of these open on: two bundled skills, four
// agent skills (one hidden, one the agent saved) and one in the trash.
const mainSkills = `[
 {"name":"release","description":"Cut a release of a Go module","level":"bundled","eligible":true},
 {"name":"pr-review","description":"Review a pull request","level":"bundled","eligible":true},
 {"name":"deploy","description":"Deploy a service","level":"agent","eligible":false,
  "hidden":{"reason":"missing_tool","subject":"exec","message":"requires tool exec, not in the agent's tools"}},
 {"name":"notes","description":"Keep meeting notes","level":"agent","eligible":true},
 {"name":"gif-search","description":"Find a GIF for a mood","level":"agent","eligible":true,
  "learned":{"on":"2026-09-20T09:15:00Z","by":"U1","by_name":"Angel","from":"T1","origin":"correction"}},
 {"name":"standup","description":"Run the standup","level":"agent","eligible":true},
 {"name":"old-trick","description":"Something retired","level":"agent","eligible":false,
  "hidden":{"reason":"trashed","message":"in the trash"},"trashed":true}
]`

const gifSearchFull = `{"name":"gif-search","description":"Find a GIF for a mood","level":"agent","version":3,
 "eligible":true,"learned":{"on":"2026-09-20T09:15:00Z","by":"U1","by_name":"Angel","from":"T1","origin":"correction"},
 "requires":{"tools":["http_fetch"],"bins":["curl"]},"references":["refs/moods.md","refs/sites.md"],
 "body":"# GIF search\nAsk for the mood, then search.\n",
 "raw":"---\nname: gif-search\ndescription: Find a GIF for a mood\n---\n# GIF search\nAsk for the mood, then search.\n",
 "versions":[{"version":2,"on":"2026-09-18T16:40:00Z","by":"U1","by_name":"Angel","trace":"T1"},{"version":1}]}`

func skillsFixture() *fakeSkills {
	return &fakeSkills{
		agents: []gen.AgentInfo{{Name: "scribe"}, {Name: "main"}},
		byAgent: map[string][]gen.Skill{
			"main":   skillsOf(mainSkills),
			"scribe": skillsOf(`[{"name":"release","description":"Cut a release","level":"bundled","eligible":true}]`),
		},
		full: map[string]gen.Skill{
			"gif-search": skillOf(gifSearchFull),
			"release":    skillOf(`{"name":"release","description":"Cut a release of a Go module","level":"bundled","eligible":true,"body":"# Release\n"}`),
			"deploy":     skillOf(`{"name":"deploy","description":"Deploy a service","level":"agent","version":1,"eligible":false,"body":"# Deploy\n","versions":[]}`),
			"old-trick": skillOf(`{"name":"old-trick","description":"Something retired","level":"agent","version":2,"trashed":true,"eligible":false,
				"body":"# Old\n","versions":[{"version":1}]}`),
		},
		versions: map[string]gen.Skill{
			"gif-search@2": skillOf(`{"name":"gif-search","description":"Find a GIF, the old way","level":"agent","version":2,
				"eligible":true,"body":"# GIF search, as it was\n","raw":"---\nname: gif-search\n---\nthe old raw file\n"}`),
		},
		getErr: map[string]error{},
	}
}

// openSkills builds a sized model and runs it through its opening requests,
// which is the state every test below starts from. The window is tall so the
// panel holds a whole skill.
func openSkills(t *testing.T, f *fakeSkills) *skillsUI {
	t.Helper()
	m := newSkillsUI(context.Background(), f, "config socket")
	m.Update(tea.WindowSizeMsg{Width: 150, Height: 70})
	driveSkills(t, m, m.Init())
	return m
}

// driveSkills runs a command and everything it leads to, the way the runtime
// would, batches included.
func driveSkills(t *testing.T, m *skillsUI, cmd tea.Cmd) {
	t.Helper()
	queue := []tea.Cmd{cmd}
	for n := 0; len(queue) > 0; n++ {
		if n > 100 {
			t.Fatal("the explorer kept issuing commands; something loops")
		}
		c := queue[0]
		queue = queue[1:]
		if c == nil {
			continue
		}
		switch msg := c().(type) {
		case nil, tea.QuitMsg:
		case tea.BatchMsg:
			queue = append(queue, msg...)
		default:
			_, next := m.Update(msg)
			queue = append(queue, next)
		}
	}
}

// pressSk is one keystroke, followed by whatever it set off.
func pressSk(t *testing.T, m *skillsUI, s string) {
	t.Helper()
	_, cmd := m.Update(memKeyMsg(s))
	driveSkills(t, m, cmd)
}

func skScreen(m *skillsUI) string { return stripStyle(m.View()) }

// onMain is the explorer with the main agent selected and the focus on the
// skills column, at the named level and skill.
func onMain(t *testing.T, f *fakeSkills, level, skill string) *skillsUI {
	t.Helper()
	m := openSkills(t, f)
	if m.agent() != "main" {
		t.Fatalf("selected agent %q, want main", m.agent())
	}
	pick(t, m, level, skill)
	return m
}

// pick puts the cursors on a level (by name) and a skill (by name).
func pick(t *testing.T, m *skillsUI, level, skill string) {
	t.Helper()
	found := false
	for i, l := range m.levels {
		if l.name == level {
			m.levelCur.at, found = i, true
		}
	}
	if !found {
		t.Fatalf("no level %q in %+v", level, m.levels)
	}
	m.applyFilter()
	for i, s := range m.shown {
		if s.Name == skill {
			m.skillCur.at = i
			m.focus = skPaneSkills
			return
		}
	}
	t.Fatalf("no skill %q at %s", skill, level)
}

func TestSkillsOpeningWalksAgentsThenSkillsOnce(t *testing.T) {
	f := skillsFixture()
	m := openSkills(t, f)

	want := []string{"agents", "skills:main"}
	if strings.Join(f.calls, ",") != strings.Join(want, ",") {
		t.Fatalf("calls = %v, want %v", f.calls, want)
	}
	if m.focus != skPaneAgents {
		t.Errorf("focus = %v, want the agents with two to choose from", m.focus)
	}
	view := skScreen(m)
	for _, s := range []string{"agents", "levels", "skills", "main", "scribe", "holaclaw · skills · config socket · main › bundled"} {
		if !strings.Contains(view, s) {
			t.Errorf("the screen does not mention %q:\n%s", s, view)
		}
	}
}

func TestSkillsASingleAgentStartsOnTheLevels(t *testing.T) {
	f := skillsFixture()
	f.agents = []gen.AgentInfo{{Name: "main"}}
	m := openSkills(t, f)
	if m.focus != skPaneLevels {
		t.Fatalf("focus = %v, want the levels", m.focus)
	}
	if m.agentsW >= m.levelsW {
		t.Errorf("the agents column is %d wide against %d for the levels", m.agentsW, m.levelsW)
	}
}

func TestSkillsMovingTheAgentLoadsItsSkills(t *testing.T) {
	f := skillsFixture()
	m := openSkills(t, f)
	pressSk(t, m, "j")
	if f.calls[len(f.calls)-1] != "skills:scribe" {
		t.Fatalf("calls = %v, want the scribe's skills last", f.calls)
	}
	if len(m.levels) != 3 || m.levels[0].n != 1 || m.levels[1].n != 0 || m.levels[2].n != 0 {
		t.Fatalf("levels = %+v, want bundled (1), agent (0), trash (0)", m.levels)
	}
}

func TestSkillsTheLevelsAreBundledAgentAndTrashWithCounts(t *testing.T) {
	m := onMain(t, skillsFixture(), "bundled", "release")
	var got []string
	for _, l := range m.levels {
		got = append(got, fmt.Sprintf("%s(%d)", l.name, l.n))
	}
	if strings.Join(got, " ") != "bundled(2) agent(4) trash(1)" {
		t.Fatalf("levels = %v", got)
	}
	view := skScreen(m)
	for _, s := range []string{"bundled", "(2)", "agent", "(4)", "trash", "(1)"} {
		if !strings.Contains(view, s) {
			t.Errorf("the levels column does not show %q:\n%s", s, view)
		}
	}
}

func TestSkillsSelectingALevelFiltersWithoutARequest(t *testing.T) {
	f := skillsFixture()
	m := onMain(t, f, "bundled", "release")
	m.focus = skPaneLevels
	before := len(f.calls)
	pressSk(t, m, "j")
	if len(f.calls) != before {
		t.Fatalf("selecting a level asked the daemon: %v", f.calls[before:])
	}
	var names []string
	for _, s := range m.shown {
		names = append(names, s.Name)
	}
	if strings.Join(names, ",") != "deploy,gif-search,notes,standup" {
		t.Fatalf("the agent level shows %v", names)
	}
	if !strings.Contains(skScreen(m), "4 skills · 3 eligible") {
		t.Errorf("the header does not count the level:\n%s", skScreen(m))
	}
}

func TestSkillsARowIsTheDotAndTheIndexLine(t *testing.T) {
	m := onMain(t, skillsFixture(), "agent", "deploy")
	view := skScreen(m)
	if !strings.Contains(view, "○ deploy: Deploy a service") {
		t.Errorf("a hidden skill has no hollow dot or index line:\n%s", view)
	}
	if !strings.Contains(view, "● notes: Keep meeting notes") {
		t.Errorf("an eligible skill has no full dot:\n%s", view)
	}
}

func TestSkillsTheStatusLineSaysWhyASkillIsHidden(t *testing.T) {
	m := onMain(t, skillsFixture(), "agent", "deploy")
	if !strings.Contains(skScreen(m), "main / agent / deploy · hidden: requires tool exec, not in the agent's tools") {
		t.Errorf("the status line does not carry the reason:\n%s", skScreen(m))
	}
	pick(t, m, "agent", "gif-search")
	if !strings.Contains(skScreen(m), "main / agent / gif-search · eligible · taught by Angel on 2026-09-20") {
		t.Errorf("the status line does not say who taught the skill:\n%s", skScreen(m))
	}
}

func TestSkillsATrashedSkillIsMarked(t *testing.T) {
	m := onMain(t, skillsFixture(), "trash", "old-trick")
	view := skScreen(m)
	if !strings.Contains(view, "old-trick: Something retired") {
		t.Errorf("the trashed row is not there:\n%s", view)
	}
	if !strings.Contains(view, "main / trash / old-trick · in the trash") {
		t.Errorf("the status line does not say it is in the trash:\n%s", view)
	}
	if len(m.shown) != 1 {
		t.Errorf("the trash shows %d skills, want the one trashed", len(m.shown))
	}
}

func TestSkillsTheFilterNarrowsTheSkillsByNameDescriptionAndReason(t *testing.T) {
	m := onMain(t, skillsFixture(), "agent", "deploy")
	pressSk(t, m, "/")
	if !m.typing {
		t.Fatal("`/` did not open the filter")
	}
	pressSk(t, m, "exec")
	pressSk(t, m, "enter")
	if len(m.shown) != 1 || m.shown[0].Name != "deploy" {
		t.Fatalf("the filter kept %d skills, want deploy alone by its hidden reason", len(m.shown))
	}
	if !strings.Contains(skScreen(m), "/ exec") {
		t.Errorf("the filter is not shown over the column:\n%s", skScreen(m))
	}
	pressSk(t, m, "esc")
	if m.query != "" || len(m.shown) != 4 {
		t.Fatalf("escape left the filter: %q, %d rows", m.query, len(m.shown))
	}
}

func TestSkillsAnAnswerFromAnEarlierRequestIsDropped(t *testing.T) {
	m := onMain(t, skillsFixture(), "bundled", "release")
	m.Update(skListMsg{seq: m.listSeq - 1, skills: skillsOf(`[{"name":"ghost","description":"x","level":"bundled","eligible":true}]`)})
	if strings.Contains(skScreen(m), "ghost") {
		t.Fatalf("a stale list overwrote the columns:\n%s", skScreen(m))
	}
	pressSk(t, m, "enter")
	m.Update(skDetailMsg{seq: m.detSeq - 1, skill: skillOf(`{"name":"ghost","description":"x","level":"bundled","eligible":true}`)})
	if m.det.live.Name != "release" {
		t.Fatalf("a stale skill landed in the panel: %q", m.det.live.Name)
	}
}

func TestSkillsEnterOpensTheWholeSkill(t *testing.T) {
	f := skillsFixture()
	m := onMain(t, f, "agent", "gif-search")
	pressSk(t, m, "enter")

	if !m.showDet {
		t.Fatal("enter did not open the panel")
	}
	if !strings.Contains(strings.Join(f.calls, ","), "get:gif-search") {
		t.Fatalf("calls = %v, want the skill", f.calls)
	}
	view := skScreen(m)
	for _, s := range []string{
		"gif-search · version 3",
		"description    Find a GIF for a mood", "requires tools http_fetch", "requires bins  curl",
		"learned by     Angel", "learned from   T1", "origin         correction",
		"references     refs/moods.md", "refs/sites.md",
		"body", "# GIF search", "Ask for the mood, then search.",
		"versions", "› 2  2026-09-18  by Angel  trace T1", "  1",
	} {
		if !strings.Contains(view, s) {
			t.Errorf("the panel does not show %q:\n%s", s, view)
		}
	}
	if strings.Contains(view, "space") {
		t.Errorf("the panel still talks about spaces:\n%s", view)
	}
	if lines := strings.Count(view, "\n") + 1; lines != m.height {
		t.Errorf("the screen is %d rows tall with the panel open, want %d", lines, m.height)
	}
	pressSk(t, m, "esc")
	if m.showDet {
		t.Fatal("escape did not close the panel")
	}
}

func TestSkillsAPanelOnABundledSkillHasNoVersions(t *testing.T) {
	m := onMain(t, skillsFixture(), "bundled", "release")
	pressSk(t, m, "o")
	view := skScreen(m)
	if !strings.Contains(view, "# Release") {
		t.Fatalf("the panel did not open on the bundled skill:\n%s", view)
	}
	if strings.Contains(view, "versions") || strings.Contains(view, "· version") {
		t.Errorf("a bundled skill shows a history or a number:\n%s", view)
	}
}

func TestSkillsAnAgentSkillWithNoHistorySaysNone(t *testing.T) {
	m := onMain(t, skillsFixture(), "agent", "deploy")
	pressSk(t, m, "enter")
	view := skScreen(m)
	if !strings.Contains(view, "versions") || !strings.Contains(view, "none") {
		t.Errorf("an agent skill without history does not say so:\n%s", view)
	}
}

func TestSkillsEnterOnAVersionShowsItAndEscapeGoesBack(t *testing.T) {
	f := skillsFixture()
	m := onMain(t, f, "agent", "gif-search")
	pressSk(t, m, "enter")
	pressSk(t, m, "enter") // the cursor starts on the newest version
	if f.calls[len(f.calls)-1] != "version:gif-search@2" {
		t.Fatalf("calls = %v, want the version read", f.calls)
	}
	view := skScreen(m)
	for _, s := range []string{"gif-search @ version 2", "# GIF search, as it was", "Find a GIF, the old way", "versions"} {
		if !strings.Contains(view, s) {
			t.Errorf("the version panel does not show %q:\n%s", s, view)
		}
	}
	if strings.Contains(view, "version 3") {
		t.Errorf("the version panel still names the live number:\n%s", view)
	}
	pressSk(t, m, "esc")
	if !m.showDet || m.det.old != nil {
		t.Fatalf("escape from a version did not go back to the live file (open=%v, old=%v)", m.showDet, m.det.old != nil)
	}
	if v := skScreen(m); !strings.Contains(v, "Ask for the mood") || !strings.Contains(v, "gif-search · version 3") {
		t.Errorf("the live body and number are not back:\n%s", v)
	}
	pressSk(t, m, "esc")
	if m.showDet {
		t.Fatal("the second escape did not close the panel")
	}
}

func TestSkillsJShowsTheRawFile(t *testing.T) {
	m := onMain(t, skillsFixture(), "agent", "gif-search")
	pressSk(t, m, "enter")
	pressSk(t, m, "J")
	view := skScreen(m)
	if !strings.Contains(view, "gif-search · version 3 · raw") || !strings.Contains(view, "description: Find a GIF for a mood") {
		t.Errorf("J did not show the raw file:\n%s", view)
	}
	if strings.Contains(view, "requires tools") {
		t.Errorf("the parsed facts are still there in raw:\n%s", view)
	}
	pressSk(t, m, "J")
	if strings.Contains(skScreen(m), "· raw") {
		t.Error("a second J did not go back to the parsed view")
	}
}

func TestSkillsRevertIsAskedAndOnlyAYesDoesIt(t *testing.T) {
	f := skillsFixture()
	m := onMain(t, f, "agent", "gif-search")
	pressSk(t, m, "enter")

	pressSk(t, m, "v")
	view := skScreen(m)
	if !strings.Contains(view, "revert gif-search to version 2? y/n") || !strings.Contains(view, "2  2026-09-18  by Angel  trace T1") {
		t.Fatalf("v did not ask about the version under the cursor with who taught it:\n%s", view)
	}
	pressSk(t, m, "n")
	if len(f.reverted) != 0 {
		t.Fatalf("answering no reverted %v", f.reverted)
	}

	pressSk(t, m, "j") // the older version
	pressSk(t, m, "v")
	pressSk(t, m, "y")
	if len(f.reverted) != 1 || f.reverted[0] != "main/gif-search>1" {
		t.Fatalf("reverted %v, want gif-search to 1", f.reverted)
	}
	tail := strings.Join(f.calls[len(f.calls)-2:], ",")
	for _, c := range []string{"skills:main", "get:gif-search"} {
		if !strings.Contains(tail, c) {
			t.Errorf("after the revert %q was not asked again: %v", c, f.calls)
		}
	}
	if !strings.Contains(m.status, "reverted gif-search to version 1") {
		t.Errorf("status = %q", m.status)
	}
}

func TestSkillsRevertOfATrashedSkillIsRefused(t *testing.T) {
	f := skillsFixture()
	m := onMain(t, f, "trash", "old-trick")
	pressSk(t, m, "enter")
	pressSk(t, m, "v")
	if m.confirm != nil || !strings.Contains(m.status, "old-trick is in the trash: restore it first") {
		t.Fatalf("confirm=%v status=%q", m.confirm != nil, m.status)
	}
	if len(f.reverted) != 0 {
		t.Fatalf("reverted %v", f.reverted)
	}
}

func TestSkillsRemoveMovesALiveSkillToTheTrash(t *testing.T) {
	f := skillsFixture()
	m := onMain(t, f, "agent", "gif-search")
	pressSk(t, m, "d")
	view := skScreen(m)
	if !strings.Contains(view, "remove gif-search from main? y/n") || !strings.Contains(view, "into the trash, with its history") {
		t.Fatalf("the question does not say where it goes:\n%s", view)
	}
	pressSk(t, m, "n")
	if len(f.removed) != 0 {
		t.Fatalf("answering no removed %v", f.removed)
	}
	pressSk(t, m, "d")
	pressSk(t, m, "y")
	if len(f.removed) != 1 || f.removed[0] != "main/gif-search" {
		t.Fatalf("removed %v", f.removed)
	}
	if f.calls[len(f.calls)-1] != "skills:main" {
		t.Errorf("the list was not reloaded: %v", f.calls)
	}
	if !strings.Contains(m.status, "moved gif-search to the trash") {
		t.Errorf("status = %q", m.status)
	}
}

func TestSkillsRemoveOnATrashedSkillDeletesForGood(t *testing.T) {
	f := skillsFixture()
	m := onMain(t, f, "trash", "old-trick")
	pressSk(t, m, "d")
	if !strings.Contains(skScreen(m), "delete old-trick for good? y/n") {
		t.Fatalf("the question does not say it is for good:\n%s", skScreen(m))
	}
	pressSk(t, m, "y")
	if len(f.removed) != 1 || !strings.Contains(m.status, "deleted old-trick for good") {
		t.Fatalf("removed %v, status %q", f.removed, m.status)
	}
}

func TestSkillsRestoreBringsATrashedSkillBack(t *testing.T) {
	f := skillsFixture()
	m := onMain(t, f, "trash", "old-trick")
	pressSk(t, m, "r")
	if !strings.Contains(skScreen(m), "restore old-trick into main? y/n") {
		t.Fatalf("r did not ask:\n%s", skScreen(m))
	}
	pressSk(t, m, "n")
	if len(f.restored) != 0 {
		t.Fatalf("answering no restored %v", f.restored)
	}
	pressSk(t, m, "r")
	pressSk(t, m, "y")
	if len(f.restored) != 1 || f.restored[0] != "main/old-trick" {
		t.Fatalf("restored %v", f.restored)
	}
	if f.calls[len(f.calls)-1] != "skills:main" || !strings.Contains(m.status, "restored old-trick") {
		t.Errorf("calls %v, status %q", f.calls, m.status)
	}
}

func TestSkillsRestoreOfALiveSkillIsRefused(t *testing.T) {
	f := skillsFixture()
	m := onMain(t, f, "agent", "notes")
	pressSk(t, m, "r")
	if m.confirm != nil || m.status != "notes is not in the trash" {
		t.Fatalf("confirm=%v status=%q", m.confirm != nil, m.status)
	}
}

func TestSkillsRestoreOverALiveNameFailsInTheStatusLine(t *testing.T) {
	f := skillsFixture()
	f.restoreErr = &api.ClientError{Status: http.StatusConflict, Code: "conflict", Message: "a live old-trick exists"}
	m := onMain(t, f, "trash", "old-trick")
	pressSk(t, m, "r")
	pressSk(t, m, "y")
	if !m.isError || !strings.Contains(m.status, "restore:") || !strings.Contains(m.status, "a live old-trick exists") {
		t.Fatalf("status = %q", m.status)
	}
}

func TestSkillsBundledSkillsHaveNoActions(t *testing.T) {
	f := skillsFixture()
	m := onMain(t, f, "bundled", "release")
	for _, k := range []string{"d", "v"} {
		m.clear()
		pressSk(t, m, k)
		if m.confirm != nil || m.status != "bundled skills have no actions" {
			t.Errorf("%s on a bundled skill: confirm=%v status=%q", k, m.confirm != nil, m.status)
		}
	}
	// The panel works, and v there says the same.
	pressSk(t, m, "enter")
	pressSk(t, m, "v")
	if m.confirm != nil || m.status != "bundled skills have no actions" {
		t.Errorf("v in a bundled panel: confirm=%v status=%q", m.confirm != nil, m.status)
	}
	pressSk(t, m, "esc")
	pressSk(t, m, "p")
	if m.confirm != nil {
		t.Error("p still asks something; promotion is gone")
	}
	if len(f.removed)+len(f.restored)+len(f.reverted) != 0 {
		t.Fatalf("an action went out: %v %v %v", f.removed, f.restored, f.reverted)
	}
}

func TestSkillsAnApiErrorLandsInTheStatusLine(t *testing.T) {
	f := skillsFixture()
	f.listErr = errors.New("the skills dir is gone")
	m := openSkills(t, f)
	if !m.isError || !strings.Contains(m.status, "skills: the skills dir is gone") {
		t.Fatalf("status = %q (error=%v)", m.status, m.isError)
	}
	pressSk(t, m, "j")
	if skScreen(m) == "" {
		t.Fatal("the explorer stopped drawing after an error")
	}

	f = skillsFixture()
	f.getErr["gif-search"] = errors.New("unreadable SKILL.md")
	m = onMain(t, f, "agent", "gif-search")
	pressSk(t, m, "enter")
	if !strings.Contains(m.status, "skill: unreadable SKILL.md") {
		t.Fatalf("status = %q, want the panel's failure", m.status)
	}
}

func TestSkillsReloadKeepsTheSelection(t *testing.T) {
	f := skillsFixture()
	m := onMain(t, f, "agent", "standup")
	before := len(f.calls)
	pressSk(t, m, "R")
	if got := strings.Join(f.calls[before:], ","); got != "agents,skills:main" {
		t.Fatalf("R asked for %s, want everything from the agents down", got)
	}
	s, _ := m.skill()
	l, _ := m.level()
	if m.agent() != "main" || l.name != "agent" || s.Name != "standup" {
		t.Fatalf("after R: %s / %s / %s, want main / agent / standup", m.agent(), l.name, s.Name)
	}
}

func TestSkillsAClickSelectsTheRowUnderThePointer(t *testing.T) {
	m := onMain(t, skillsFixture(), "bundled", "release")
	m.Update(tea.MouseMsg{X: m.agentsW + 2, Y: skRowTop + 1, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
	if m.focus != skPaneLevels || m.levelCur.at != 1 {
		t.Fatalf("focus = %v, level = %d; want the levels column, second row", m.focus, m.levelCur.at)
	}
	if s, _ := m.skill(); s.Name != "deploy" {
		t.Errorf("the click did not narrow the skills: %q", s.Name)
	}
	m.Update(tea.MouseMsg{X: m.agentsW + 2, Y: skRowTop, Button: tea.MouseButtonWheelDown})
	if m.levelCur.at != 2 {
		t.Errorf("the wheel did not walk the column: %d", m.levelCur.at)
	}
}

func TestSkillsQuitLeaves(t *testing.T) {
	m := openSkills(t, skillsFixture())
	_, cmd := m.Update(memKeyMsg("q"))
	if cmd == nil {
		t.Fatal("`q` issued no command")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatal("`q` did not quit")
	}
	if m.View() != "" {
		t.Error("the screen was not handed back on the way out")
	}
}

func TestSkillsHelpPanelListsTheActions(t *testing.T) {
	m := openSkills(t, skillsFixture())
	pressSk(t, m, "?")
	view := skScreen(m)
	for _, s := range []string{"remove", "restore", "revert to version", "raw file"} {
		if !strings.Contains(view, s) {
			t.Errorf("the help does not list %q:\n%s", s, view)
		}
	}
	// An explorer: installing is `skills install`, and a trace is an id.
	for _, s := range []string{"promote", "install", "trace"} {
		if strings.Contains(view, s) {
			t.Errorf("the help still lists %s:\n%s", s, view)
		}
	}
	pressSk(t, m, "x")
	if m.showHelp {
		t.Error("a key did not shut the help")
	}
}

// --- the command ---------------------------------------------------------------

func TestSkillsExplorerNeedsATerminal(t *testing.T) {
	code, _, errOut := runCLI(t, "", "skills")
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.Contains(errOut, "needs a terminal") || !strings.Contains(errOut, "skills list|show|install|remove|restore|revert") {
		t.Fatalf("the hint does not point at the subcommands:\n%s", errOut)
	}
}

func TestSkillsHelpListsTheExplorerAndTheSubcommands(t *testing.T) {
	code, out, _ := runCLI(t, "", "skills", "--help")
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	for _, s := range []string{"browse the skills of an agent", "list the skills of an agent", "--addr"} {
		if !strings.Contains(out, s) {
			t.Errorf("`skills --help` does not mention %q:\n%s", s, out)
		}
	}
}

// The skills column walks and pages on the same keys as the other browsers.
func TestSkillsWalkOnVimAndEmacsKeys(t *testing.T) {
	m := onMain(t, skillsFixture(), "agent", "deploy")
	n := m.rows(skPaneSkills)
	if n < 3 {
		t.Fatalf("%d agent skills; the test needs a few to walk", n)
	}
	m.skillCur = cursor{}
	for _, tc := range []struct {
		key  string
		want int
	}{
		{"ctrl+n", 1},
		{"ctrl+p", 0},
		{"alt+>", n - 1},
		{"ctrl+b", 0}, // a page is more than the column holds
		{"ctrl+f", n - 1},
		{"alt+<", 0},
	} {
		pressSk(t, m, tc.key)
		if m.skillCur.at != tc.want {
			t.Fatalf("%s left the cursor on %d, want %d", tc.key, m.skillCur.at, tc.want)
		}
	}
}
