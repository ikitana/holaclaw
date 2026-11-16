package config

import (
	"errors"
	"path/filepath"
	"testing"
)

// TestChannelTypeWhatsApp loads a whatsapp instance: the type joins the enum
// with no new keys (docs/channels/whatsapp.md §3).
func TestChannelTypeWhatsApp(t *testing.T) {
	d := t.TempDir()
	copyDir(t, dir("minimal"), d)
	write(t, filepath.Join(d, FileChannels), `version = 1

[channels.whatsapp]
type  = "whatsapp"
agent = "main"
[channels.whatsapp.access]
dm     = "invite"
rooms = "approved"
`)
	cfg, err := Load(d)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := cfg.Channels["whatsapp"].Type; got != "whatsapp" {
		t.Fatalf("type = %q, want whatsapp", got)
	}

	write(t, filepath.Join(d, FileChannels), "version = 1\n[channels.wa]\ntype = \"whatsap\"\nagent = \"main\"\n")
	_, err = Load(d)
	var ps Problems
	if !errors.As(err, &ps) {
		t.Fatalf("misspelt type: err = %v, want problems", err)
	}
	if _, ok := has(ps, CodeBadEnum); !ok {
		t.Fatalf("misspelt type: problems = %v, want %s", ps.Codes(), CodeBadEnum)
	}
}

// holaclaw is the built-in channel of the CLI and the apps: a configured
// instance can't take its name, or its senders would speak as its bindings.
func TestChannelNameHolaclawReserved(t *testing.T) {
	d := t.TempDir()
	copyDir(t, dir("minimal"), d)
	write(t, filepath.Join(d, FileChannels), "version = 1\n[channels.holaclaw]\ntype = \"telegram\"\nagent = \"main\"\n")
	_, err := Load(d)
	var ps Problems
	if !errors.As(err, &ps) {
		t.Fatalf("err = %v, want problems", err)
	}
	if p, ok := has(ps, CodeBadValue); !ok || p.Key != "channels.holaclaw" {
		t.Fatalf("problems = %v, want %s on channels.holaclaw", ps, CodeBadValue)
	}
}
