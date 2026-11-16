package page

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"holaclaw/internal/browser/cdp"
)

// Keys are a closed set on the action path: ActKey takes a name from the table
// below and nothing else, because a key name that reached CDP unchecked would
// be model text driving input. The live view is the other path — there a
// person's key events are translated, never validated against a list, and
// never logged.

// namedKey is one entry of the table: what CDP needs to make the key real.
type namedKey struct {
	key  string // DOM KeyboardEvent.key
	code string // DOM KeyboardEvent.code
	vk   int    // windowsVirtualKeyCode
	text string // the character it inserts, "" for a control key
}

// keyNames is the closed set ActKey accepts, lowercased on lookup.
var keyNames = map[string]namedKey{
	"enter":      {key: "Enter", code: "Enter", vk: 13, text: "\r"},
	"escape":     {key: "Escape", code: "Escape", vk: 27},
	"tab":        {key: "Tab", code: "Tab", vk: 9, text: "\t"},
	"backspace":  {key: "Backspace", code: "Backspace", vk: 8},
	"delete":     {key: "Delete", code: "Delete", vk: 46},
	"arrowup":    {key: "ArrowUp", code: "ArrowUp", vk: 38},
	"arrowdown":  {key: "ArrowDown", code: "ArrowDown", vk: 40},
	"arrowleft":  {key: "ArrowLeft", code: "ArrowLeft", vk: 37},
	"arrowright": {key: "ArrowRight", code: "ArrowRight", vk: 39},
	"home":       {key: "Home", code: "Home", vk: 36},
	"end":        {key: "End", code: "End", vk: 35},
	"pageup":     {key: "PageUp", code: "PageUp", vk: 33},
	"pagedown":   {key: "PageDown", code: "PageDown", vk: 34},
	"space":      {key: " ", code: "Space", vk: 32, text: " "},
}

// lookupKey resolves an action's key name. Unknown names are refused rather
// than guessed at.
func lookupKey(name string) (namedKey, bool) {
	k, ok := keyNames[strings.ToLower(strings.TrimSpace(name))]
	return k, ok
}

// keyEvents builds the down/up pair for one named key.
func keyEvents(k namedKey, modifiers int) []cdp.KeyEvent {
	down := cdp.KeyEvent{
		Type:                  "keyDown",
		Key:                   k.key,
		Code:                  k.code,
		WindowsVirtualKeyCode: k.vk,
		NativeVirtualKeyCode:  k.vk,
		Modifiers:             modifiers,
	}
	if k.text != "" {
		down.Text, down.UnmodifiedText = k.text, k.text
	} else {
		down.Type = "rawKeyDown"
	}
	up := cdp.KeyEvent{
		Type:                  "keyUp",
		Key:                   k.key,
		Code:                  k.code,
		WindowsVirtualKeyCode: k.vk,
		NativeVirtualKeyCode:  k.vk,
		Modifiers:             modifiers,
	}
	return []cdp.KeyEvent{down, up}
}

// viewerKey translates one live-view key message into a CDP event. The viewer
// sends DOM key and code; a printable key carries its own text, anything else
// is looked up. Nothing here is logged: this is what a person is typing.
func viewerKey(event, key, code string, modifiers int) cdp.KeyEvent {
	typ := "keyDown"
	if event == "up" {
		typ = "keyUp"
	}
	ev := cdp.KeyEvent{Type: typ, Key: key, Code: code, Modifiers: modifiers}
	if k, ok := keyNames[strings.ToLower(key)]; ok {
		ev.Key, ev.Code, ev.WindowsVirtualKeyCode, ev.NativeVirtualKeyCode = k.key, k.code, k.vk, k.vk
		if ev.Code == "" {
			ev.Code = code
		}
		if k.text != "" && typ == "keyDown" {
			ev.Text, ev.UnmodifiedText = k.text, k.text
		} else if typ == "keyDown" {
			ev.Type = "rawKeyDown"
		}
		return ev
	}
	// A single printable character: the virtual key code of the common ones is
	// the upper-case ASCII value, which is what browsers report.
	if r, n := utf8.DecodeRuneInString(key); n == len(key) && unicode.IsGraphic(r) {
		if typ == "keyDown" {
			ev.Text, ev.UnmodifiedText = key, key
		}
		if r < utf8.RuneSelf {
			ev.WindowsVirtualKeyCode = int(unicode.ToUpper(r))
			ev.NativeVirtualKeyCode = ev.WindowsVirtualKeyCode
		}
		return ev
	}
	// A modifier or a key we have no code for: send it bare rather than drop
	// it, so shift-, ctrl- and alt-held states still reach the page.
	if typ == "keyDown" {
		ev.Type = "rawKeyDown"
	}
	return ev
}
