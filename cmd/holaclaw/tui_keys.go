package main

import bkey "github.com/charmbracelet/bubbles/key"

// The keyboard of the full-screen chat, in one place so the help bar and the
// Update switch can never drift apart: every binding below is both what the
// key does and what the bar says it does.

type keyMap struct {
	Send     bkey.Binding
	Newline  bkey.Binding
	Focus    bkey.Binding
	Activity bkey.Binding
	Detail   bkey.Binding
	Palette  bkey.Binding
	Help     bkey.Binding
	Clear    bkey.Binding
	Raw      bkey.Binding
	Trace    bkey.Binding
	Abort    bkey.Binding
	Quit     bkey.Binding
	Close    bkey.Binding
	PageUp   bkey.Binding
	PageDown bkey.Binding
	Top      bkey.Binding
	Bottom   bkey.Binding
}

// ShortHelp is the single line under the prompt: the handful of keys somebody
// needs before they have read anything.
func (k keyMap) ShortHelp() []bkey.Binding {
	return []bkey.Binding{k.Send, k.Newline, k.Focus, k.Detail, k.Palette, k.Help, k.Quit}
}

// FullHelp is what f1 opens: everything, grouped the way the screen is.
func (k keyMap) FullHelp() [][]bkey.Binding {
	return [][]bkey.Binding{
		{k.Send, k.Newline, k.Palette, k.Clear},
		{k.Focus, k.Activity, k.Detail, k.Close},
		{k.PageUp, k.PageDown, k.Top, k.Bottom},
		{k.Raw, k.Trace, k.Abort, k.Quit},
	}
}

func newKeyMap() keyMap {
	return keyMap{
		Send: bkey.NewBinding(
			bkey.WithKeys("enter"),
			bkey.WithHelp("enter", "send"),
		),
		Newline: bkey.NewBinding(
			bkey.WithKeys("ctrl+j", "alt+enter"),
			bkey.WithHelp("ctrl+j", "newline"),
		),
		Focus: bkey.NewBinding(
			bkey.WithKeys("tab"),
			bkey.WithHelp("tab", "move focus"),
		),
		Activity: bkey.NewBinding(
			bkey.WithKeys("ctrl+o"),
			bkey.WithHelp("ctrl+o", "activity pane"),
		),
		Detail: bkey.NewBinding(
			bkey.WithKeys("enter"),
			bkey.WithHelp("enter/click", "details"),
		),
		Palette: bkey.NewBinding(
			bkey.WithKeys("ctrl+p"),
			bkey.WithHelp("ctrl+p", "commands"),
		),
		Help: bkey.NewBinding(
			bkey.WithKeys("f1", "?"),
			bkey.WithHelp("f1", "help"),
		),
		Clear: bkey.NewBinding(
			bkey.WithKeys("ctrl+l"),
			bkey.WithHelp("ctrl+l", "clear view"),
		),
		Raw: bkey.NewBinding(
			bkey.WithKeys("ctrl+r"),
			bkey.WithHelp("ctrl+r", "raw/markdown"),
		),
		Trace: bkey.NewBinding(
			bkey.WithKeys("ctrl+t"),
			bkey.WithHelp("ctrl+t", "trace ids"),
		),
		Abort: bkey.NewBinding(
			bkey.WithKeys("ctrl+c"),
			bkey.WithHelp("ctrl+c", "abort turn"),
		),
		Quit: bkey.NewBinding(
			bkey.WithKeys("ctrl+d"),
			bkey.WithHelp("ctrl+d", "quit"),
		),
		Close: bkey.NewBinding(
			bkey.WithKeys("esc"),
			bkey.WithHelp("esc", "close panel"),
		),
		PageUp: bkey.NewBinding(
			bkey.WithKeys("pgup", "ctrl+b", "alt+v"),
			bkey.WithHelp("pgup/ctrl+b/alt+v", "scroll up"),
		),
		PageDown: bkey.NewBinding(
			bkey.WithKeys("pgdown", "ctrl+f", "ctrl+v"),
			bkey.WithHelp("pgdn/ctrl+f/ctrl+v", "scroll down"),
		),
		Top: bkey.NewBinding(
			bkey.WithKeys("home", "alt+<"),
			bkey.WithHelp("home/alt+<", "top"),
		),
		Bottom: bkey.NewBinding(
			bkey.WithKeys("end", "alt+>"),
			bkey.WithHelp("end/alt+>", "bottom"),
		),
	}
}

// --- the command palette ---------------------------------------------------------

// tuiCommand is one entry of the palette, which is also the list of slash
// commands the prompt accepts: typing `/new` and picking "new" from ctrl+p do
// the same thing.
type tuiCommand struct {
	name string
	arg  string // what an argument means, for the help text; "" when it takes none
	desc string
}

// tuiCommands is the whole vocabulary, in the order the palette shows it.
var tuiCommands = []tuiCommand{
	{name: "new", arg: "name", desc: "start a fresh conversation"},
	{name: "clear", desc: "empty the transcript on screen"},
	{name: "activity", desc: "show or hide the activity pane"},
	{name: "details", desc: "show or hide the details panel"},
	{name: "raw", desc: "toggle markdown rendering of replies"},
	{name: "trace", desc: "toggle the trace id under each reply"},
	{name: "info", desc: "print the agent, model and session"},
	{name: "abort", desc: "stop the running turn"},
	{name: "help", desc: "show every key"},
	{name: "quit", desc: "leave the chat"},
}

// Title, Description and FilterValue make a command a bubbles list item.
func (c tuiCommand) Title() string {
	if c.arg == "" {
		return "/" + c.name
	}
	return "/" + c.name + " <" + c.arg + ">"
}

func (c tuiCommand) Description() string { return c.desc }

func (c tuiCommand) FilterValue() string { return c.name + " " + c.desc }
