package main

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/charmbracelet/bubbles/help"
	bkey "github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"

	"holaclaw/internal/api"
	"holaclaw/internal/api/gen"
)

// The skills explorer: `holaclaw skills` with nothing after it (docs/skills.md
// §7). Skills follow the agent, so the screen is the agents, then the levels
// of the one selected — bundled, agent and the trash — then the skills of
// that level as the model's index has them. The status line says why a skill
// is hidden, which is the question that usually brings somebody here.
//
// It is the memory browser's shape with the trace browser's bottom panel,
// where each version names the trace that saved it. It explores; installing
// is `skills install`, and the subcommands stay for scripts.

// skillsAPI is the slice of the daemon this explorer needs. An interface so
// the model can be driven in a test without a server; nothing else here
// reaches the network.
type skillsAPI interface {
	ListAgents(ctx context.Context) ([]gen.AgentInfo, error)
	ListSkills(ctx context.Context, agent string) ([]gen.Skill, error)
	GetSkill(ctx context.Context, name, agent string) (gen.Skill, error)
	GetSkillVersion(ctx context.Context, name, agent string, version int) (gen.Skill, error)
	RemoveSkill(ctx context.Context, name, agent string) error
	RestoreSkill(ctx context.Context, name, agent string) (gen.Skill, error)
	RevertSkill(ctx context.Context, name, agent string, to int) (gen.Skill, error)
}

// skPane is which column the keyboard is talking to.
type skPane int

const (
	skPaneAgents skPane = iota
	skPaneLevels
	skPaneSkills
)

// --- what a request answers with ------------------------------------------------

// The list and the panel carry the sequence number they were asked under, and
// an answer behind the model's is dropped: walking the agents fires a list
// per row, and walking the skills with the panel open a request per
// row, and a slow early answer must not land last.
type (
	skAgentsMsg struct {
		agents []gen.AgentInfo
		err    error
	}
	skListMsg struct {
		seq    int
		skills []gen.Skill
		err    error
	}
	skDetailMsg struct {
		seq   int
		skill gen.Skill
		err   error
	}
	skVersionMsg struct {
		seq     int
		version int
		skill   gen.Skill
		err     error
	}
	// skDoneMsg is the answer to any action: what to say, or what failed,
	// and whether the open panel is about the thing that changed.
	skDoneMsg struct {
		what   string
		said   string
		err    error
		detail bool
	}
)

// --- the model ----------------------------------------------------------------------

// skLevel is one row of the levels column: bundled, agent or trash, with how
// many skills it holds. The name is what a reload finds it again by.
type skLevel struct {
	name string
	n    int
}

// The three levels, in the order the column lists them.
const (
	skBundled = "bundled"
	skAgent   = "agent"
	skTrash   = "trash"
)

// skConfirm is the one modal every action goes through. yes is what `y` does.
type skConfirm struct {
	title string
	body  string
	yes   func() tea.Cmd
}

// skTarget is a line of the panel the cursor can stand on: a version of an
// agent skill.
type skTarget struct {
	line    int // the panel line it is drawn on
	version *gen.SkillVersion
}

// skDetail is the bottom panel: the skill as it is, or one of its versions,
// with the live skill's history under it either way so the cursor has the
// same lines to walk.
type skDetail struct {
	live    gen.Skill
	loaded  bool       // live has arrived
	old     *gen.Skill // an earlier version being shown, nil for the live file
	raw     bool
	targets []skTarget
	cur     int
}

type skillsUI struct {
	ctx   context.Context
	api   skillsAPI
	where string

	keys   skKeyMap
	styles tuiStyles
	help   help.Model
	filter textinput.Model
	detail viewport.Model
	modal  viewport.Model

	agents []gen.AgentInfo
	skills []gen.Skill // every skill of the selected agent
	levels []skLevel
	shown  []gen.Skill // the selected level, after the filter

	agentCur, levelCur, skillCur cursor
	query                        string

	// want is what a reload lands on again, by name, once the lists answer.
	want struct{ agent, level, skill string }

	listSeq, detSeq, verSeq int
	loadList                bool

	focus    skPane
	typing   bool
	showHelp bool
	showDet  bool
	det      skDetail
	confirm  *skConfirm

	status  string
	isError bool

	width, height, bodyH     int
	agentsW, levelsW, skillW int
	quitting                 bool
}

// runSkillsTUI is the whole explorer.
func runSkillsTUI(ctx context.Context, c *cli, sapi skillsAPI, where string) error {
	return runFullScreen(ctx, c, newSkillsUI(ctx, sapi, where))
}

func newSkillsUI(ctx context.Context, sapi skillsAPI, where string) *skillsUI {
	st := newTUIStyles()

	q := textinput.New()
	q.Prompt = "/ "
	q.Placeholder = "filter the skills"

	h := help.New()
	h.Styles.ShortKey, h.Styles.FullKey = st.helpKey, st.helpKey
	h.Styles.ShortDesc, h.Styles.FullDesc = st.dim, st.dim

	m := &skillsUI{
		ctx: ctx, api: sapi, where: where,
		keys: newSkKeyMap(), styles: st, help: h, filter: q,
	}
	// A size arrives a frame later; until then the columns are laid out for a
	// plausible terminal so the first frame is not drawn at zero columns.
	m.width, m.height = 100, 30
	m.layout()
	return m
}

func (m *skillsUI) Init() tea.Cmd { return m.loadAgents() }

// --- requests -------------------------------------------------------------------

// loadAgents asks for the agents.
func (m *skillsUI) loadAgents() tea.Cmd {
	ctx, sapi := m.ctx, m.api
	return func() tea.Msg {
		agents, err := sapi.ListAgents(ctx)
		if err != nil {
			return skAgentsMsg{err: err}
		}
		return skAgentsMsg{agents: agents}
	}
}

// loadSkills asks for the selected agent's skills under a fresh sequence
// number. One request feeds both the levels and the skills: picking a level
// narrows what is already here.
func (m *skillsUI) loadSkills() tea.Cmd {
	agent := m.agent()
	if agent == "" {
		m.skills, m.levels, m.shown = nil, nil, nil
		return nil
	}
	m.listSeq++
	m.loadList = true
	seq, ctx, sapi := m.listSeq, m.ctx, m.api
	return func() tea.Msg {
		skills, err := sapi.ListSkills(ctx, agent)
		return skListMsg{seq: seq, skills: skills, err: err}
	}
}

// loadDetail is the panel's request, under a sequence number so walking the
// list with the panel open never shows one skill's answer under another.
func (m *skillsUI) loadDetail(s gen.Skill) tea.Cmd {
	m.detSeq++
	m.det = skDetail{raw: m.det.raw}
	seq, ctx, sapi, agent := m.detSeq, m.ctx, m.api, m.agent()
	return func() tea.Msg {
		sk, err := sapi.GetSkill(ctx, s.Name, agent)
		return skDetailMsg{seq: seq, skill: sk, err: err}
	}
}

// reload is `R`: everything again from the agents down, landing on the same
// agent, level and skill when they are still there.
func (m *skillsUI) reload() tea.Cmd {
	m.want.agent = m.agent()
	m.keepSelection()
	return m.loadAgents()
}

// keepSelection remembers the level and skill so the next list lands on them.
func (m *skillsUI) keepSelection() {
	m.want.level, m.want.skill = "", ""
	if l, ok := m.level(); ok {
		m.want.level = l.name
	}
	if s, ok := m.skill(); ok {
		m.want.skill = s.Name
	}
}

// --- update ---------------------------------------------------------------------

func (m *skillsUI) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.layout()
		m.refreshDetail()
		return m, nil

	case tea.KeyMsg:
		return m, m.onKey(msg)

	case tea.MouseMsg:
		return m, m.onMouse(msg)

	case skAgentsMsg:
		return m, m.onAgents(msg)

	case skListMsg:
		return m, m.onList(msg)

	case skDetailMsg:
		if msg.seq != m.detSeq {
			return m, nil
		}
		if msg.err != nil {
			m.fail("skill", msg.err)
			return m, nil
		}
		m.det.live, m.det.loaded = msg.skill, true
		m.refreshDetail()
		return m, nil

	case skVersionMsg:
		if msg.seq != m.verSeq {
			return m, nil
		}
		if msg.err != nil {
			m.fail("version", msg.err)
			return m, nil
		}
		old := msg.skill
		old.Version = &msg.version // the number asked for, which the daemon echoes
		m.det.old = &old
		m.refreshDetail()
		m.detail.GotoTop()
		return m, nil

	case skDoneMsg:
		if msg.err != nil {
			m.fail(msg.what, msg.err)
			return m, nil
		}
		m.say(msg.said)
		m.keepSelection()
		cmds := []tea.Cmd{m.loadSkills()}
		if msg.detail && m.showDet {
			if s, ok := m.skill(); ok {
				cmds = append(cmds, m.loadDetail(s))
			}
		}
		return m, tea.Batch(cmds...)
	}
	return m, nil
}

func (m *skillsUI) onAgents(msg skAgentsMsg) tea.Cmd {
	if msg.err != nil {
		m.fail("agents", msg.err)
		return nil
	}
	m.agents = msg.agents
	sort.Slice(m.agents, func(i, j int) bool { return m.agents[i].Name < m.agents[j].Name })
	if want := m.want.agent; want != "" {
		m.want.agent = ""
		for i, a := range m.agents {
			if a.Name == want {
				m.agentCur.at = i
			}
		}
	}
	m.agentCur.clamp(len(m.agents), m.rowsFor(skPaneAgents))
	// One agent is the common case, and there is nothing to choose there:
	// the column shrinks to a label and the keyboard starts on the levels.
	if len(m.agents) == 1 && m.focus == skPaneAgents {
		m.focus = skPaneLevels
	}
	m.layout()
	return m.loadSkills()
}

func (m *skillsUI) onList(msg skListMsg) tea.Cmd {
	if msg.seq != m.listSeq {
		return nil
	}
	m.loadList = false
	if msg.err != nil {
		m.skills, m.levels, m.shown = nil, nil, nil
		m.fail("skills", msg.err)
		return nil
	}
	m.skills = msg.skills
	m.levels = skillLevels(m.skills)
	m.levelCur, m.skillCur = cursor{}, cursor{}
	for i, l := range m.levels {
		if l.name == m.want.level {
			m.levelCur.at = i
		}
	}
	m.levelCur.clamp(len(m.levels), m.rowsFor(skPaneLevels))
	m.applyFilter()
	for i, s := range m.shown {
		if s.Name == m.want.skill {
			m.skillCur.at = i
		}
	}
	m.skillCur.clamp(len(m.shown), m.rowsFor(skPaneSkills))
	m.want.level, m.want.skill = "", ""
	return nil
}

// skillLevels is the levels column for one agent's skills: bundled, agent and
// trash always, each with how many skills the list puts there, so an empty
// trash still says so.
func skillLevels(skills []gen.Skill) []skLevel {
	levels := []skLevel{{name: skBundled}, {name: skAgent}, {name: skTrash}}
	for _, s := range skills {
		for i := range levels {
			if levelOf(s) == levels[i].name {
				levels[i].n++
			}
		}
	}
	return levels
}

// levelOf is the level row a skill is listed under: the trash for a trashed
// one, else its level.
func levelOf(s gen.Skill) string {
	if isTrashed(s) {
		return skTrash
	}
	return string(s.Level)
}

func isTrashed(s gen.Skill) bool { return s.Trashed != nil && *s.Trashed }

func (m *skillsUI) fail(what string, err error) {
	m.status, m.isError = what+": "+err.Error(), true
}

func (m *skillsUI) say(s string) { m.status, m.isError = s, false }

func (m *skillsUI) clear() { m.status, m.isError = "", false }

// --- selection --------------------------------------------------------------------

func (m *skillsUI) agent() string {
	if m.agentCur.at >= len(m.agents) {
		return ""
	}
	return m.agents[m.agentCur.at].Name
}

func (m *skillsUI) level() (skLevel, bool) {
	if m.levelCur.at >= len(m.levels) {
		return skLevel{}, false
	}
	return m.levels[m.levelCur.at], true
}

func (m *skillsUI) skill() (gen.Skill, bool) {
	if m.skillCur.at >= len(m.shown) {
		return gen.Skill{}, false
	}
	return m.shown[m.skillCur.at], true
}

// applyFilter narrows the skills to the selected level and the `/` text. The
// daemon is not asked again: the whole list is here, and a level is a view
// of it.
func (m *skillsUI) applyFilter() {
	m.shown = nil
	l, ok := m.level()
	if !ok {
		return
	}
	for _, s := range m.skills {
		if levelOf(s) != l.name {
			continue
		}
		if matches(m.query, s.Name+" "+s.Description+" "+hiddenText(s)) {
			m.shown = append(m.shown, s)
		}
	}
	sort.SliceStable(m.shown, func(i, j int) bool { return m.shown[i].Name < m.shown[j].Name })
	m.skillCur.clamp(len(m.shown), m.rowsFor(skPaneSkills))
}

func (m *skillsUI) rows(p skPane) int {
	switch p {
	case skPaneAgents:
		return len(m.agents)
	case skPaneLevels:
		return len(m.levels)
	}
	return len(m.shown)
}

func (m *skillsUI) cursorOf(p skPane) *cursor {
	switch p {
	case skPaneAgents:
		return &m.agentCur
	case skPaneLevels:
		return &m.levelCur
	}
	return &m.skillCur
}

// --- keys ---------------------------------------------------------------------------

func (m *skillsUI) onKey(msg tea.KeyMsg) tea.Cmd {
	switch {
	case m.confirm != nil:
		return m.confirmKey(msg)
	case m.typing:
		return m.filterKey(msg)
	case m.showHelp:
		// The help is a reference, not a mode: anything shuts it.
		m.showHelp = false
		return nil
	case m.showDet:
		return m.detailKey(msg)
	}
	return m.browseKey(msg)
}

func (m *skillsUI) browseKey(msg tea.KeyMsg) tea.Cmd {
	switch {
	case bkey.Matches(msg, m.keys.Quit):
		m.quitting = true
		return tea.Quit
	case bkey.Matches(msg, m.keys.Help):
		m.showHelp = true
		return nil
	case bkey.Matches(msg, m.keys.Reload):
		m.say("reloading…")
		return m.reload()
	case bkey.Matches(msg, m.keys.Filter):
		m.typing = true
		m.focus = skPaneSkills
		m.filter.SetValue(m.query)
		m.filter.CursorEnd()
		m.filter.Focus()
		return textinput.Blink
	case bkey.Matches(msg, m.keys.Close):
		// Escape drops the filter first, then backs out a column: the way
		// out is the way in, one step at a time.
		if m.query != "" {
			m.setQuery("")
			return nil
		}
		m.movePane(-1)
		return nil
	case bkey.Matches(msg, m.keys.Open):
		return m.open()
	case bkey.Matches(msg, m.keys.Remove):
		return m.askRemove()
	case bkey.Matches(msg, m.keys.Restore):
		return m.askRestore()
	case bkey.Matches(msg, m.keys.Revert):
		if s, ok := m.skill(); ok {
			if why := noActions(s); why != "" {
				m.say(why)
				return nil
			}
			m.say("open the skill and pick a version to revert to")
		}
		return nil
	case bkey.Matches(msg, m.keys.NextPane), bkey.Matches(msg, m.keys.Right):
		m.movePane(1)
		return nil
	case bkey.Matches(msg, m.keys.PrevPane), bkey.Matches(msg, m.keys.Left):
		m.movePane(-1)
		return nil
	case bkey.Matches(msg, m.keys.Up):
		return m.moveCursor(-1)
	case bkey.Matches(msg, m.keys.Down):
		return m.moveCursor(1)
	case bkey.Matches(msg, m.keys.PageUp):
		return m.moveCursor(-m.rowsFor(m.focus))
	case bkey.Matches(msg, m.keys.PageDown):
		return m.moveCursor(m.rowsFor(m.focus))
	case bkey.Matches(msg, m.keys.HalfUp):
		return m.moveCursor(-halfPage(m.rowsFor(m.focus)))
	case bkey.Matches(msg, m.keys.HalfDown):
		return m.moveCursor(halfPage(m.rowsFor(m.focus)))
	case bkey.Matches(msg, m.keys.Top):
		return m.moveCursor(-m.rows(m.focus))
	case bkey.Matches(msg, m.keys.Bottom):
		return m.moveCursor(m.rows(m.focus))
	}
	return nil
}

// open is enter: in the left columns it moves right, where the cursor has
// already filled what it points at; on a skill it opens the panel.
func (m *skillsUI) open() tea.Cmd {
	if m.focus < skPaneSkills {
		m.movePane(1)
		return nil
	}
	s, ok := m.skill()
	if !ok {
		return nil
	}
	m.showDet = true
	m.layout()
	cmd := m.loadDetail(s)
	m.refreshDetail()
	return cmd
}

// movePane walks the focus across the columns, skipping the agents when
// there is only one.
func (m *skillsUI) movePane(d int) {
	first := skPaneAgents
	if len(m.agents) <= 1 {
		first = skPaneLevels
	}
	m.focus = min(max(m.focus+skPane(d), first), skPaneSkills)
}

// moveCursor walks the focused column. A new agent is a new list; a new
// level is the same list narrowed, which costs nothing.
func (m *skillsUI) moveCursor(d int) tea.Cmd {
	c := m.cursorOf(m.focus)
	before := c.at
	c.move(d, m.rows(m.focus), m.rowsFor(m.focus))
	if c.at == before {
		return nil
	}
	m.clear()
	switch m.focus {
	case skPaneAgents:
		m.want.level, m.want.skill = "", ""
		return m.loadSkills()
	case skPaneLevels:
		m.skillCur = cursor{}
		m.applyFilter()
	}
	return nil
}

// filterKey is the box above the skills: enter keeps it, escape drops it.
func (m *skillsUI) filterKey(msg tea.KeyMsg) tea.Cmd {
	switch msg.String() {
	case "enter":
		m.typing = false
		m.filter.Blur()
		m.setQuery(strings.TrimSpace(m.filter.Value()))
		return nil
	case "esc":
		m.typing = false
		m.filter.Blur()
		m.filter.SetValue("")
		m.setQuery("")
		return nil
	}
	var cmd tea.Cmd
	m.filter, cmd = m.filter.Update(msg)
	return cmd
}

func (m *skillsUI) setQuery(q string) {
	if m.query == q {
		return
	}
	m.query = q
	m.skillCur = cursor{}
	m.applyFilter()
	m.clear()
}

// noActions says why a skill cannot be changed from here, "" when it can:
// bundled skills ship with the binary and are never written.
func noActions(s gen.Skill) string {
	if s.Level == gen.SkillLevelBundled {
		return "bundled skills have no actions"
	}
	return ""
}

// --- the panel ----------------------------------------------------------------------

func (m *skillsUI) detailKey(msg tea.KeyMsg) tea.Cmd {
	switch {
	case bkey.Matches(msg, m.keys.Close):
		// Back from a version to the live file first, then out.
		if m.det.old != nil {
			m.verSeq++
			m.det.old = nil
			m.refreshDetail()
			return nil
		}
		m.showDet = false
		m.layout()
		return nil
	case bkey.Matches(msg, m.keys.Quit):
		m.quitting = true
		return tea.Quit
	case bkey.Matches(msg, m.keys.Help):
		m.showHelp = true
		return nil
	case bkey.Matches(msg, m.keys.Raw):
		m.det.raw = !m.det.raw
		m.refreshDetail()
		return nil
	case bkey.Matches(msg, m.keys.Up):
		m.walk(-1)
		return nil
	case bkey.Matches(msg, m.keys.Down):
		m.walk(1)
		return nil
	case bkey.Matches(msg, m.keys.Open):
		if t, ok := m.target(); ok && t.version != nil {
			return m.loadVersion(t.version.Version)
		}
		return nil
	case bkey.Matches(msg, m.keys.Revert):
		return m.askRevert()
	}
	if scrollKey(&m.detail, msg) {
		return nil
	}
	var cmd tea.Cmd
	m.detail, cmd = m.detail.Update(msg)
	return cmd
}

func (m *skillsUI) target() (skTarget, bool) {
	if m.det.cur >= len(m.det.targets) {
		return skTarget{}, false
	}
	return m.det.targets[m.det.cur], true
}

// walk moves the panel's cursor over the versions, and scrolls the text so
// the line it is on stays in sight.
func (m *skillsUI) walk(d int) {
	n := len(m.det.targets)
	if n == 0 {
		return
	}
	m.det.cur = min(max(m.det.cur+d, 0), n-1)
	m.refreshDetail()
	line := m.det.targets[m.det.cur].line
	switch {
	case line < m.detail.YOffset:
		m.detail.SetYOffset(line)
	case line >= m.detail.YOffset+m.detail.Height:
		m.detail.SetYOffset(line - m.detail.Height + 1)
	}
}

// refreshDetail redraws the panel from what it holds, keeping the scroll.
func (m *skillsUI) refreshDetail() {
	if !m.showDet {
		return
	}
	text, targets := skillDetailText(m.det, max(m.detail.Width, 10), m.styles)
	m.det.targets = targets
	m.det.cur = min(m.det.cur, max(len(targets)-1, 0))
	off := m.detail.YOffset
	m.detail.SetContent(text)
	m.detail.SetYOffset(off)
}

func (m *skillsUI) loadVersion(version int) tea.Cmd {
	m.verSeq++
	s := m.det.live
	seq, ctx, sapi, agent := m.verSeq, m.ctx, m.api, m.agent()
	return func() tea.Msg {
		sk, err := sapi.GetSkillVersion(ctx, s.Name, agent, version)
		return skVersionMsg{seq: seq, version: version, skill: sk, err: err}
	}
}

// --- the actions ------------------------------------------------------------------

// Every action is asked before it is done: each changes files an agent reads
// on its next run, and the one screen that makes them easy should not also
// make them accidental.

func (m *skillsUI) askRevert() tea.Cmd {
	s := m.det.live
	if why := noActions(s); why != "" {
		m.say(why)
		return nil
	}
	if isTrashed(s) {
		m.say(s.Name + " is in the trash: restore it first")
		return nil
	}
	t, ok := m.target()
	if !ok || t.version == nil {
		m.say("put the cursor on a version to revert to it")
		return nil
	}
	name, agent, to := s.Name, m.agent(), t.version.Version
	body := versionLine(*t.version) + "\n\nreferences and scripts come back too; the current version retires into history/, so nothing is lost"
	m.ask(fmt.Sprintf("revert %s to version %d? y/n", name, to), body, func() tea.Cmd {
		ctx, sapi := m.ctx, m.api
		return func() tea.Msg {
			_, err := sapi.RevertSkill(ctx, name, agent, to)
			return skDoneMsg{what: "revert", said: fmt.Sprintf("reverted %s to version %d", name, to), err: err, detail: true}
		}
	})
	return nil
}

func (m *skillsUI) askRemove() tea.Cmd {
	s, ok := m.skill()
	if !ok {
		return nil
	}
	if why := noActions(s); why != "" {
		m.say(why)
		return nil
	}
	name, agent := s.Name, m.agent()
	title := fmt.Sprintf("remove %s from %s? y/n", name, agent)
	body := "into the trash, with its history; r there brings it back"
	said := fmt.Sprintf("moved %s to the trash", name)
	if isTrashed(s) {
		title = fmt.Sprintf("delete %s for good? y/n", name)
		body, said = "it is in the trash already: this deletes it and its history for good", fmt.Sprintf("deleted %s for good", name)
	}
	m.ask(title, body, func() tea.Cmd {
		ctx, sapi := m.ctx, m.api
		return func() tea.Msg {
			return skDoneMsg{what: "remove", said: said, err: sapi.RemoveSkill(ctx, name, agent)}
		}
	})
	return nil
}

// askRestore is `r`: a trashed skill back among the agent's, with its
// history. The daemon refuses when a live skill has taken the name.
func (m *skillsUI) askRestore() tea.Cmd {
	s, ok := m.skill()
	if !ok {
		return nil
	}
	if !isTrashed(s) {
		m.say(s.Name + " is not in the trash")
		return nil
	}
	name, agent := s.Name, m.agent()
	m.ask(fmt.Sprintf("restore %s into %s? y/n", name, agent), "out of the trash, with its history", func() tea.Cmd {
		ctx, sapi := m.ctx, m.api
		return func() tea.Msg {
			_, err := sapi.RestoreSkill(ctx, name, agent)
			return skDoneMsg{what: "restore", said: fmt.Sprintf("restored %s", name), err: err}
		}
	})
	return nil
}

func (m *skillsUI) ask(title, body string, yes func() tea.Cmd) {
	m.confirm = &skConfirm{title: title, body: body, yes: yes}
	m.layout()
	m.fitModal()
}

func (m *skillsUI) confirmKey(msg tea.KeyMsg) tea.Cmd {
	c := *m.confirm
	switch msg.String() {
	case "esc", "ctrl+c", "n", "N":
		m.confirm = nil
		return nil
	case "y", "Y":
		m.confirm = nil
		return c.yes()
	}
	if scrollKey(&m.modal, msg) {
		return nil
	}
	var cmd tea.Cmd
	m.modal, cmd = m.modal.Update(msg)
	return cmd
}

// --- mouse ------------------------------------------------------------------------

// onMouse makes the columns clickable, as in the other two browsers: a click
// selects the row under the pointer, the wheel walks the column under it,
// and with the panel open the wheel reads the panel.
func (m *skillsUI) onMouse(msg tea.MouseMsg) tea.Cmd {
	if m.confirm != nil || m.showHelp {
		return nil
	}
	if m.showDet && msg.Y >= skHeaderRows+m.bodyH {
		var cmd tea.Cmd
		m.detail, cmd = m.detail.Update(msg)
		return cmd
	}
	pane := m.paneAt(msg.X)
	switch msg.Button {
	case tea.MouseButtonWheelUp, tea.MouseButtonWheelDown:
		m.focus = pane
		d := -memWheelRows
		if msg.Button == tea.MouseButtonWheelDown {
			d = memWheelRows
		}
		return m.moveCursor(d)
	case tea.MouseButtonLeft:
		// The release of a click must not be taken for a second click.
		if msg.Action != tea.MouseActionPress {
			return nil
		}
		m.focus = pane
		i, ok := m.rowAt(pane, msg.Y)
		if !ok {
			return nil
		}
		return m.moveCursor(i - m.cursorOf(pane).at)
	}
	return nil
}

// --- the command ---------------------------------------------------------------------

// cmdSkillsBrowse is `holaclaw skills` with no subcommand. Like the other two
// browsers it needs a terminal at both ends, and says so rather than drawing
// a screen nobody can read.
func cmdSkillsBrowse(ctx context.Context, c *cli, cn *conn) error {
	need := "the skills explorer needs a terminal; `skills list|show|install|remove|restore|revert` are the ones for scripts"
	return browseDaemon(ctx, c, cn, need, func(client *api.Client, where string) error {
		return runSkillsTUI(ctx, c, client, where)
	})
}
