package main

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"holaclaw/internal/api"
	"holaclaw/internal/api/gen"
)

// The memory browser is a plain Bubble Tea model, so every one of these drives
// it the way the runtime would — a window size, then messages — and reads what
// it drew. No daemon and no terminal are involved: the model talks to a fake
// memoryAPI that records what it was asked for.

// fakeMemory answers the five calls the browser makes and remembers every one.
type fakeMemory struct {
	agents []gen.AgentInfo
	parts  []gen.MemoryPartition
	byPart map[string][]gen.MemoryItem

	calls      []string
	lastSearch gen.SearchMemoryParams
	written    []gen.MemoryWrite
	forgot     []string

	partsErr  error
	searchErr error
}

func (f *fakeMemory) ListAgents(context.Context) ([]gen.AgentInfo, error) {
	f.calls = append(f.calls, "agents")
	return f.agents, nil
}

func (f *fakeMemory) ListMemoryPartitions(_ context.Context, agent string) ([]gen.MemoryPartition, error) {
	f.calls = append(f.calls, "partitions:"+agent)
	if f.partsErr != nil {
		return nil, f.partsErr
	}
	return f.parts, nil
}

func (f *fakeMemory) SearchMemory(_ context.Context, p gen.SearchMemoryParams) ([]gen.MemoryItem, error) {
	f.calls = append(f.calls, "search:"+p.Partition)
	f.lastSearch = p
	if f.searchErr != nil {
		return nil, f.searchErr
	}
	return f.byPart[p.Partition], nil
}

func (f *fakeMemory) WriteMemory(_ context.Context, in gen.MemoryWrite) (gen.MemoryItem, error) {
	f.calls = append(f.calls, "write")
	f.written = append(f.written, in)
	return gen.MemoryItem{Id: "M-new", Text: in.Text}, nil
}

func (f *fakeMemory) ForgetMemory(_ context.Context, id, _, _ string) error {
	f.calls = append(f.calls, "forget")
	f.forgot = append(f.forgot, id)
	return nil
}

// twoAgents is the fixture most of these open on: two agents, two partitions
// under the first, and a memory in each.
func twoAgents() *fakeMemory {
	now := time.Now()
	return &fakeMemory{
		agents: []gen.AgentInfo{{Name: "main"}, {Name: "scribe"}},
		parts: []gen.MemoryPartition{
			{Partition: "dm/U1", Notes: 2, Recall: 7, LastWritten: &now},
			{Partition: "room/01JROOM", Recall: 1, LastWritten: &now},
		},
		byPart: map[string][]gen.MemoryItem{
			"dm/U1": {
				{Id: "M1", Layer: "note", Text: "angel prefers tabs", Partition: "dm/U1", CreatedAt: now},
				{Id: "M2", Layer: "recall", Text: "talked about the deploy", Partition: "dm/U1", CreatedAt: now},
			},
			"room/01JROOM": {
				{Id: "M3", Layer: "recall", Text: "standup summary", Partition: "room/01JROOM", CreatedAt: now},
			},
		},
	}
}

// openBrowser builds a sized model and runs it through its opening requests,
// which is the state every test below starts from.
func openBrowser(t *testing.T, f *fakeMemory) *memoryUI {
	t.Helper()
	m := newMemoryUI(context.Background(), f, "config socket")
	m.Update(tea.WindowSizeMsg{Width: 130, Height: 40})
	drive(t, m, m.Init())
	return m
}

// drive runs a command and everything it leads to, the way the runtime would.
func drive(t *testing.T, m *memoryUI, cmd tea.Cmd) {
	t.Helper()
	for range 20 {
		if cmd == nil {
			return
		}
		msg := cmd()
		if msg == nil {
			return
		}
		_, cmd = m.Update(msg)
	}
	t.Fatal("the browser kept issuing commands; something loops")
}

// press is one keystroke, followed by whatever it set off.
func press(t *testing.T, m *memoryUI, s string) {
	t.Helper()
	_, cmd := m.Update(memKeyMsg(s))
	drive(t, m, cmd)
}

func memKeyMsg(s string) tea.KeyMsg {
	switch s {
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEscape}
	case "tab":
		return tea.KeyMsg{Type: tea.KeyTab}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	case "ctrl+s":
		return tea.KeyMsg{Type: tea.KeyCtrlS}
	case "space":
		return tea.KeyMsg{Type: tea.KeySpace}
	}
	if t, ok := ctrlKeys[s]; ok {
		return tea.KeyMsg{Type: t}
	}
	if r, ok := strings.CutPrefix(s, "alt+"); ok {
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(r), Alt: true}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

// ctrlKeys are the keys the runtime delivers as types of their own rather
// than as runes, so a test spells them the way the bindings do.
var ctrlKeys = map[string]tea.KeyType{
	"up": tea.KeyUp, "pgup": tea.KeyPgUp, "pgdown": tea.KeyPgDown, "home": tea.KeyHome, "end": tea.KeyEnd,
	"ctrl+b": tea.KeyCtrlB, "ctrl+f": tea.KeyCtrlF, "ctrl+u": tea.KeyCtrlU, "ctrl+d": tea.KeyCtrlD,
	"ctrl+v": tea.KeyCtrlV, "ctrl+n": tea.KeyCtrlN, "ctrl+p": tea.KeyCtrlP,
}

func screen(m *memoryUI) string { return stripStyle(m.View()) }

// A partition with an identity behind it is labelled by that name, the kind
// badge stays, and everything asked of the daemon still carries the raw string.
func TestPartitionsAreLabelledByIdentityName(t *testing.T) {
	f := twoAgents()
	f.parts[0].Name = ptrOf("Ada Lovelace")
	m := openBrowser(t, f)

	view := screen(m)
	if !strings.Contains(view, "Ada Lovelace") {
		t.Errorf("the partitions pane does not show the name:\n%s", view)
	}
	// One without a name keeps its raw id, and both keep their badge.
	if !strings.Contains(view, "01JROOM") || !strings.Contains(view, "room") || !strings.Contains(view, "dm") {
		t.Errorf("the nameless room or a badge is missing:\n%s", view)
	}
	if m.partition() != "dm/U1" || f.lastSearch.Partition != "dm/U1" {
		t.Fatalf("selected %q, searched %q; want the raw partition", m.partition(), f.lastSearch.Partition)
	}
}

func TestOpeningWalksAgentsThenPartitionsThenMemories(t *testing.T) {
	f := twoAgents()
	m := openBrowser(t, f)

	want := []string{"agents", "partitions:main", "search:dm/U1"}
	if strings.Join(f.calls, ",") != strings.Join(want, ",") {
		t.Fatalf("calls = %v, want %v", f.calls, want)
	}
	view := screen(m)
	for _, s := range []string{"agents", "partitions", "memories", "main", "scribe", "dm", "U1", "angel prefers tabs"} {
		if !strings.Contains(view, s) {
			t.Errorf("the screen does not mention %q:\n%s", s, view)
		}
	}
}

func TestASingleAgentStartsOnThePartitions(t *testing.T) {
	f := twoAgents()
	f.agents = []gen.AgentInfo{{Name: "main"}}
	m := openBrowser(t, f)
	if m.focus != paneParts {
		t.Fatalf("focus = %v, want the partitions pane", m.focus)
	}
	// And the agent column is only wide enough to read the one name in it.
	if m.agentsW >= m.partsW {
		t.Errorf("the agents column is %d wide against %d for the partitions", m.agentsW, m.partsW)
	}
}

func TestMovingThePartitionCursorLoadsItsMemories(t *testing.T) {
	f := twoAgents()
	m := openBrowser(t, f)
	m.focus = paneParts
	press(t, m, "j")

	if m.partition() != "room/01JROOM" {
		t.Fatalf("selected %q, want the second partition", m.partition())
	}
	if f.lastSearch.Partition != "room/01JROOM" {
		t.Fatalf("searched %q, want the partition the cursor moved to", f.lastSearch.Partition)
	}
	if !strings.Contains(screen(m), "standup summary") {
		t.Errorf("the memories pane still shows the old partition:\n%s", screen(m))
	}
}

func TestAnAnswerFromAnEarlierRequestIsDropped(t *testing.T) {
	m := openBrowser(t, twoAgents())
	stale := memItemsMsg{seq: m.itemSeq - 1, items: []gen.MemoryItem{{Id: "OLD", Text: "an answer nobody waited for"}}}
	m.Update(stale)
	if strings.Contains(screen(m), "an answer nobody waited for") {
		t.Fatalf("a stale answer overwrote the pane:\n%s", screen(m))
	}
	if !strings.Contains(screen(m), "angel prefers tabs") {
		t.Fatal("the current memories are gone")
	}
}

func TestSearchAsksWithQAndShowsTheScore(t *testing.T) {
	f := twoAgents()
	f.byPart["dm/U1"] = []gen.MemoryItem{
		{Id: "M1", Layer: "note", Text: "angel prefers tabs", Partition: "dm/U1", Score: ptrOf(float32(0.87))},
	}
	m := openBrowser(t, f)

	press(t, m, "/")
	if !m.typing {
		t.Fatal("`/` did not open the search box")
	}
	press(t, m, "tabs")
	press(t, m, "enter")

	if f.lastSearch.Q == nil || *f.lastSearch.Q != "tabs" {
		t.Fatalf("q = %v, want \"tabs\"", f.lastSearch.Q)
	}
	view := screen(m)
	if !strings.Contains(view, "0.87") {
		t.Errorf("the score is not on the row:\n%s", view)
	}
	if !strings.Contains(view, `search "tabs"`) {
		t.Errorf("the header does not name the search:\n%s", view)
	}

	// Escape drops the search and puts the recent list back.
	press(t, m, "esc")
	if m.query != "" || f.lastSearch.Q != nil {
		t.Fatalf("escape left the search running: query=%q q=%v", m.query, f.lastSearch.Q)
	}
}

func TestNarrowsTheLayerToNotes(t *testing.T) {
	f := twoAgents()
	m := openBrowser(t, f)
	press(t, m, "n")
	if f.lastSearch.Layer == nil || string(*f.lastSearch.Layer) != "note" {
		t.Fatalf("layer = %v, want note", f.lastSearch.Layer)
	}
	press(t, m, "b")
	if f.lastSearch.Layer != nil {
		t.Fatalf("layer = %v, want both layers again", *f.lastSearch.Layer)
	}
}

func TestEnterOpensTheWholeMemoryAndEscapeCloses(t *testing.T) {
	f := twoAgents()
	f.byPart["dm/U1"] = []gen.MemoryItem{{
		Id: "M1", Layer: "note", Visibility: "shared", Partition: "dm/U1",
		Text:      "angel prefers tabs\nand a trailing sentence no column would ever fit",
		CreatedAt: time.Date(2026, 9, 20, 8, 30, 0, 0, time.UTC),
	}}
	m := openBrowser(t, f)
	m.focus = paneItems
	press(t, m, "enter")

	view := screen(m)
	for _, s := range []string{"no column would ever fit", "M1", "shared", "dm/U1", "2026-09-20"} {
		if !strings.Contains(view, s) {
			t.Errorf("the detail does not show %q:\n%s", s, view)
		}
	}
	press(t, m, "esc")
	if m.showDet {
		t.Fatal("escape did not close the detail")
	}
}

func TestForgetIsAskedForBeforeItHappens(t *testing.T) {
	f := twoAgents()
	m := openBrowser(t, f)
	m.focus = paneItems

	press(t, m, "d")
	if m.confirm == nil {
		t.Fatal("`d` did not ask")
	}
	if !strings.Contains(screen(m), "forget M1? y/n") {
		t.Errorf("the question does not name the memory:\n%s", screen(m))
	}
	press(t, m, "n")
	if len(f.forgot) != 0 {
		t.Fatalf("answering no forgot %v", f.forgot)
	}

	press(t, m, "d")
	press(t, m, "y")
	if len(f.forgot) != 1 || f.forgot[0] != "M1" {
		t.Fatalf("forgot %v, want M1 alone", f.forgot)
	}
	if f.calls[len(f.calls)-1] != "search:dm/U1" {
		t.Errorf("the pane was not reloaded after the forget: %v", f.calls)
	}
	if !strings.Contains(screen(m), "forgot M1") {
		t.Errorf("the status line does not say so:\n%s", screen(m))
	}
}

func TestAddWritesANoteIntoTheSelectedPartition(t *testing.T) {
	f := twoAgents()
	m := openBrowser(t, f)

	press(t, m, "a")
	press(t, m, "remember the nix shell")
	press(t, m, "ctrl+s")

	if len(f.written) != 1 {
		t.Fatalf("wrote %d notes, want 1", len(f.written))
	}
	got := f.written[0]
	if got.Agent != "main" || got.Partition != "dm/U1" || got.Text != "remember the nix shell" {
		t.Fatalf("wrote %+v, want the selected agent and partition", got)
	}
	// Nothing was asked for, so the API's own default stands.
	if got.Visibility != nil {
		t.Errorf("visibility = %q, want it left to the API", *got.Visibility)
	}
	if !strings.Contains(screen(m), "saved M-new") {
		t.Errorf("the status line does not confirm the save:\n%s", screen(m))
	}
}

func TestTheSharedToggleSetsTheVisibility(t *testing.T) {
	f := twoAgents()
	m := openBrowser(t, f)

	press(t, m, "a")
	press(t, m, "everyone should know this")
	press(t, m, "tab") // about
	press(t, m, "tab") // visibility
	press(t, m, "space")
	press(t, m, "ctrl+s")

	if len(f.written) != 1 {
		t.Fatalf("wrote %d notes, want 1", len(f.written))
	}
	if v := f.written[0].Visibility; v == nil || string(*v) != "shared" {
		t.Fatalf("visibility = %v, want shared", v)
	}
}

func TestAnApiErrorLandsInTheStatusLineAndTheBrowserLivesOn(t *testing.T) {
	f := twoAgents()
	f.searchErr = errors.New("the database is locked")
	m := openBrowser(t, f)

	if !m.isError || !strings.Contains(m.status, "the database is locked") {
		t.Fatalf("status = %q (error=%v), want the failure", m.status, m.isError)
	}
	// And the panes are still there to move around in.
	press(t, m, "j")
	if screen(m) == "" {
		t.Fatal("the browser stopped drawing after an error")
	}
}

func TestADaemonThatCannotListPartitionsSaysSo(t *testing.T) {
	f := twoAgents()
	f.partsErr = &api.ClientError{Status: http.StatusNotImplemented, Code: "not_implemented", Message: "memory partitions is not available in this build"}
	m := openBrowser(t, f)

	if !m.isError || !strings.Contains(m.status, "cannot list partitions") {
		t.Fatalf("status = %q, want the not-implemented explanation", m.status)
	}
	if !strings.Contains(m.status, "memory search --partition") {
		t.Errorf("the message does not say what to do instead: %q", m.status)
	}
}

func TestQuitLeaves(t *testing.T) {
	m := openBrowser(t, twoAgents())
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

func TestAClickSelectsTheRowUnderThePointer(t *testing.T) {
	f := twoAgents()
	m := openBrowser(t, f)
	x := m.agentsW + 2 // over the partitions column
	m.Update(tea.MouseMsg{X: x, Y: memRowTop + 1, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
	if m.focus != paneParts {
		t.Fatalf("focus = %v, want the partitions column", m.focus)
	}
	if m.partCur.at != 1 {
		t.Fatalf("selected row %d, want the second", m.partCur.at)
	}
}

// --- the command ---------------------------------------------------------------

func TestMemoryBrowserNeedsATerminal(t *testing.T) {
	code, _, errOut := runCLI(t, "", "memory")
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.Contains(errOut, "needs a terminal") || !strings.Contains(errOut, "memory search|add|forget") {
		t.Fatalf("the hint does not point at the subcommands:\n%s", errOut)
	}
}

func TestMemoryHelpListsTheBrowserAndTheSubcommands(t *testing.T) {
	code, out, _ := runCLI(t, "", "memory", "-h")
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	for _, s := range []string{"browse the agent memories", "search|add|forget", "--addr"} {
		if !strings.Contains(out, s) {
			t.Errorf("`memory -h` does not mention %q:\n%s", s, out)
		}
	}
}

// Paging keys are motion only while the browser has the keyboard: in the
// search box and the note editor they are the text field's own.
func TestTextFieldsKeepTheirEditingKeys(t *testing.T) {
	m := openBrowser(t, twoAgents())
	m.focus = paneItems
	press(t, m, "/")
	press(t, m, "tabs")
	press(t, m, "ctrl+b")
	press(t, m, "x")
	if got := m.search.Value(); got != "tabxs" {
		t.Fatalf("the search box holds %q, want ctrl+b to have moved its cursor: \"tabxs\"", got)
	}
	if m.itemCur.at != 0 {
		t.Fatalf("typing in the search box moved the memories to %d", m.itemCur.at)
	}
	press(t, m, "esc")

	press(t, m, "a")
	press(t, m, "note")
	press(t, m, "ctrl+b")
	press(t, m, "x")
	if got := m.noteIn.Value(); got != "notxe" {
		t.Fatalf("the note holds %q, want ctrl+b to have moved its cursor: \"notxe\"", got)
	}
}

// The memories column walks on emacs' line keys as on the arrows.
func TestMemoriesWalkOnEmacsKeys(t *testing.T) {
	m := openBrowser(t, twoAgents())
	m.focus = paneItems
	press(t, m, "ctrl+n")
	if m.itemCur.at != 1 {
		t.Fatalf("ctrl+n left the cursor on %d, want 1", m.itemCur.at)
	}
	press(t, m, "alt+<")
	if m.itemCur.at != 0 {
		t.Fatalf("alt+< left the cursor on %d, want the first memory", m.itemCur.at)
	}
}
