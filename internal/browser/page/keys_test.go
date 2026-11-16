package page

import "testing"

func TestLookupKeyIsAClosedSet(t *testing.T) {
	for _, name := range []string{"Enter", "escape", " Tab ", "Backspace", "Delete",
		"ArrowUp", "ArrowDown", "ArrowLeft", "ArrowRight", "Home", "End", "PageUp", "PageDown", "Space"} {
		if _, ok := lookupKey(name); !ok {
			t.Errorf("lookupKey(%q) not found", name)
		}
	}
	for _, name := range []string{"", "F13", "Meta+Q", "any key", "ctrl-c"} {
		if _, ok := lookupKey(name); ok {
			t.Errorf("lookupKey(%q) resolved; unknown names must be refused", name)
		}
	}
}

func TestKeyEventsCarryCodes(t *testing.T) {
	enter, _ := lookupKey("enter")
	evs := keyEvents(enter, 0)
	if len(evs) != 2 || evs[0].Type != "keyDown" || evs[1].Type != "keyUp" {
		t.Fatalf("events = %+v", evs)
	}
	if evs[0].WindowsVirtualKeyCode != 13 || evs[0].Text != "\r" {
		t.Fatalf("enter keyDown = %+v", evs[0])
	}
	esc, _ := lookupKey("escape")
	if got := keyEvents(esc, 0)[0]; got.Type != "rawKeyDown" || got.Text != "" {
		t.Fatalf("escape keyDown = %+v, want a rawKeyDown with no text", got)
	}
}

func TestViewerKey(t *testing.T) {
	ev := viewerKey("down", "a", "KeyA", 0)
	if ev.Type != "keyDown" || ev.Text != "a" || ev.WindowsVirtualKeyCode != 'A' {
		t.Fatalf("printable key = %+v", ev)
	}
	ev = viewerKey("up", "a", "KeyA", 0)
	if ev.Type != "keyUp" || ev.Text != "" {
		t.Fatalf("key up = %+v", ev)
	}
	ev = viewerKey("down", "Enter", "Enter", 0)
	if ev.WindowsVirtualKeyCode != 13 || ev.Key != "Enter" {
		t.Fatalf("named key = %+v", ev)
	}
	ev = viewerKey("down", "ArrowLeft", "ArrowLeft", 8)
	if ev.WindowsVirtualKeyCode != 37 || ev.Modifiers != 8 || ev.Type != "rawKeyDown" {
		t.Fatalf("arrow = %+v", ev)
	}
	// A modifier key we have no table entry for is still sent, not dropped.
	ev = viewerKey("down", "Shift", "ShiftLeft", 8)
	if ev.Key != "Shift" || ev.Code != "ShiftLeft" {
		t.Fatalf("modifier = %+v", ev)
	}
}

func TestChunks(t *testing.T) {
	got := chunks("abcdefgh", 3)
	if len(got) != 3 || got[0] != "abc" || got[2] != "gh" {
		t.Fatalf("chunks = %q", got)
	}
	if got := chunks("", 3); len(got) != 0 {
		t.Fatalf("chunks of an empty string = %q", got)
	}
	// A multi-byte rune is never split.
	for _, c := range chunks("héllo wörld", 2) {
		if !utf8Valid(c) {
			t.Fatalf("chunk %q split a rune", c)
		}
	}
}
