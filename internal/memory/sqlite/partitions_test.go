package sqlite

import (
	"context"
	"errors"
	"testing"
	"time"

	"holaclaw/internal/memory"
)

// Partitions is what a person browsing memory sees before they have chosen
// anything, so these pin the three things that browser depends on: the counts
// are per layer, the order is newest written first, and an agent nobody has
// written for is an empty list rather than an error.

func TestPartitionsCountsEachLayerAndOrdersByLastWritten(t *testing.T) {
	at := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	s, _ := newStore(t, Options{Now: func() time.Time { return at }})

	write(t, s, memory.LayerNote, "dm/U1", "angel prefers tabs", memory.VisOrigin)
	at = at.Add(time.Minute)
	write(t, s, memory.LayerNote, "dm/U1", "angel runs on nix", memory.VisOrigin)
	at = at.Add(time.Minute)
	write(t, s, memory.LayerRecall, "dm/U1", "talked about the deploy", memory.VisOrigin)
	at = at.Add(time.Minute)
	// The later write puts this partition first, however few items it holds.
	write(t, s, memory.LayerRecall, "room/slack/eng", "standup summary", memory.VisOrigin)
	newest := at

	parts, err := s.Partitions(context.Background(), "main")
	if err != nil {
		t.Fatalf("Partitions: %v", err)
	}
	if len(parts) != 2 {
		t.Fatalf("got %d partitions, want 2: %+v", len(parts), parts)
	}
	if parts[0].Name != "room/slack/eng" || parts[1].Name != "dm/U1" {
		t.Fatalf("order = %q, %q; want the newest written first", parts[0].Name, parts[1].Name)
	}
	if parts[0].Notes != 0 || parts[0].Recall != 1 {
		t.Errorf("room/slack/eng = %d notes, %d recall; want 0 and 1", parts[0].Notes, parts[0].Recall)
	}
	if parts[1].Notes != 2 || parts[1].Recall != 1 {
		t.Errorf("dm/U1 = %d notes, %d recall; want 2 and 1", parts[1].Notes, parts[1].Recall)
	}
	if !parts[0].LastWritten.Equal(newest) {
		t.Errorf("last written = %v, want %v", parts[0].LastWritten, newest)
	}
}

func TestPartitionsOfAnotherAgentAreNeverMixedIn(t *testing.T) {
	s, _ := newStore(t, Options{})
	write(t, s, memory.LayerNote, "dm/U1", "a note of main", memory.VisOrigin)
	if _, err := s.Write(context.Background(), memory.Item{
		Layer: memory.LayerNote, Text: "a note of the other agent", Visibility: memory.VisOrigin,
		Prov: memory.Provenance{Agent: "other", Partition: "dm/U9", Role: "owner"},
	}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	parts, err := s.Partitions(context.Background(), "main")
	if err != nil {
		t.Fatalf("Partitions: %v", err)
	}
	if len(parts) != 1 || parts[0].Name != "dm/U1" {
		t.Fatalf("got %+v, want only main's own partition", parts)
	}
}

func TestPartitionsNeedsAnAgent(t *testing.T) {
	s, _ := newStore(t, Options{})
	_, err := s.Partitions(context.Background(), "  ")
	if !errors.Is(err, memory.ErrProvenance) {
		t.Fatalf("err = %v, want ErrProvenance", err)
	}
}

func TestPartitionsOfAnUnknownAgentIsAnEmptyList(t *testing.T) {
	s, _ := newStore(t, Options{})
	parts, err := s.Partitions(context.Background(), "nobody")
	if err != nil {
		t.Fatalf("Partitions: %v", err)
	}
	// Empty rather than nil: a caller ranging over it reads "no partitions",
	// and a JSON encoding of it is `[]` and not `null`.
	if parts == nil {
		t.Fatal("an unknown agent returned nil instead of an empty list")
	}
	if len(parts) != 0 {
		t.Fatalf("got %+v, want nothing", parts)
	}
}
