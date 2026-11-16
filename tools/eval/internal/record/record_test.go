package record

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"holaclaw/internal/provider"
)

// stub is a provider that answers every call with one canned response.
type stub struct {
	resp *provider.Response
	err  error
	reqs []provider.Request
}

func (s *stub) Name() string { return "stub" }

func (s *stub) Chat(_ context.Context, req provider.Request) (provider.Stream, error) {
	s.reqs = append(s.reqs, req)
	if s.err != nil {
		return nil, s.err
	}
	return &replayStream{resp: s.resp}, nil
}

func req(system, user string, tools ...string) provider.Request {
	r := provider.Request{
		System:   system,
		Messages: []provider.Message{provider.TextMessage(provider.RoleUser, user)},
	}
	for _, t := range tools {
		r.Tools = append(r.Tools, provider.ToolSpec{Name: t})
	}
	return r
}

func TestParseMode(t *testing.T) {
	for _, m := range []Mode{ModeLive, ModeRecord, ModeReplay} {
		if got, err := ParseMode(string(m)); err != nil || got != m {
			t.Fatalf("ParseMode(%q) = %q, %v", m, got, err)
		}
	}
	if _, err := ParseMode("dream"); err == nil {
		t.Fatal("want an error for an unknown mode")
	}
}

func TestSlugAndPath(t *testing.T) {
	if got := Slug("deepseek/deepseek-v4-flash-0731:free"); got != "deepseek-deepseek-v4-flash-0731-free" {
		t.Fatalf("Slug = %q", got)
	}
	want := filepath.Join("rec", "m1", "two-step", "3.json")
	if got := Path("rec", "m1", "two-step", 3); got != want {
		t.Fatalf("Path = %q, want %q", got, want)
	}
}

func TestDigestSensitivity(t *testing.T) {
	base, shape := Digest(req("sys", "hello", "weather"))
	if shape.Messages != 1 || len(shape.Tools) != 1 || shape.LastMessage != "hello" {
		t.Fatalf("shape = %+v", shape)
	}
	same, _ := Digest(req("sys", "hello", "weather"))
	if base != same {
		t.Fatal("the same request must digest the same")
	}
	for _, other := range []provider.Request{
		req("sys2", "hello", "weather"),
		req("sys", "hello there", "weather"),
		req("sys", "hello"),
		req("sys", "hello", "weather", "fetch"),
	} {
		if d, _ := Digest(other); d == base {
			t.Fatalf("digest did not change for %+v", other)
		}
	}
}

func TestRecordThenReplay(t *testing.T) {
	dir := t.TempDir()
	path := Path(dir, "m1", "s1", 1)
	live := &stub{resp: &provider.Response{Text: "hi there", Finish: provider.FinishStop,
		Usage: provider.Usage{PromptTokens: 7, CompletionTokens: 2}}}

	rp, err := Open(ModeRecord, live, path, Run{Model: "m1", Scenario: "s1", Run: 1})
	if err != nil {
		t.Fatal(err)
	}
	stream, err := rp.Chat(context.Background(), req("sys", "hello", "weather"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Collect(stream, nil); err != nil {
		t.Fatal(err)
	}
	if err := rp.Save(); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"Authorization", "Bearer", "api_key", "http"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("recording leaks %q:\n%s", secret, raw)
		}
	}

	pp, err := Open(ModeReplay, nil, path, Run{})
	if err != nil {
		t.Fatal(err)
	}
	stream, err = pp.Chat(context.Background(), req("sys", "hello", "weather"))
	if err != nil {
		t.Fatal(err)
	}
	var streamed string
	resp, err := provider.Collect(stream, func(d string) { streamed += d })
	if err != nil {
		t.Fatal(err)
	}
	if resp.Text != "hi there" || streamed != "hi there" || resp.Usage.PromptTokens != 7 {
		t.Fatalf("replayed %+v (streamed %q)", resp, streamed)
	}

	// A second call has no recorded step left.
	if _, err := pp.Chat(context.Background(), req("sys", "hello", "weather")); err == nil ||
		!strings.Contains(err.Error(), "re-record") {
		t.Fatalf("got %v, want an out-of-steps error", err)
	}
}

func TestReplayDigestMismatch(t *testing.T) {
	dir := t.TempDir()
	path := Path(dir, "m1", "s1", 1)
	live := &stub{resp: &provider.Response{Text: "ok", Finish: provider.FinishStop}}
	rp, _ := Open(ModeRecord, live, path, Run{})
	stream, _ := rp.Chat(context.Background(), req("sys", "hello", "weather"))
	_, _ = provider.Collect(stream, nil)
	if err := rp.Save(); err != nil {
		t.Fatal(err)
	}

	pp, err := Open(ModeReplay, nil, path, Run{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = pp.Chat(context.Background(), req("sys", "hello again", "weather"))
	var me *MismatchError
	if !errors.As(err, &me) {
		t.Fatalf("got %v, want a MismatchError", err)
	}
	msg := me.Error()
	for _, want := range []string{"does not match this prompt", "last message recorded", "hello"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("mismatch hint %q lacks %q", msg, want)
		}
	}
}

func TestRecordProviderError(t *testing.T) {
	dir := t.TempDir()
	path := Path(dir, "m1", "s1", 1)
	live := &stub{err: &provider.Error{Kind: provider.ErrRateLimit, Provider: "stub"}}
	rp, _ := Open(ModeRecord, live, path, Run{})
	if _, err := rp.Chat(context.Background(), req("sys", "hello")); err == nil {
		t.Fatal("want the provider error")
	}
	if err := rp.Save(); err != nil {
		t.Fatal(err)
	}
	pp, err := Open(ModeReplay, nil, path, Run{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = pp.Chat(context.Background(), req("sys", "hello"))
	if provider.KindOf(err) != provider.ErrRateLimit {
		t.Fatalf("replayed error = %v, want a rate_limit provider error", err)
	}
}

func TestOpenErrors(t *testing.T) {
	if _, err := Open(ModeReplay, nil, filepath.Join(t.TempDir(), "missing.json"), Run{}); err == nil {
		t.Fatal("want an error for a missing recording")
	}
	if _, err := Open(ModeRecord, nil, "x.json", Run{}); err == nil {
		t.Fatal("want an error for record mode without a live provider")
	}
	bad := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(bad, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(ModeReplay, nil, bad, Run{}); err == nil {
		t.Fatal("want a parse error")
	}
}

func TestReplayStreamEndsWithEOF(t *testing.T) {
	s := &replayStream{resp: &provider.Response{Finish: provider.FinishStop}}
	c, err := s.Next() // no text: straight to done
	if err != nil || c.Kind != provider.ChunkDone {
		t.Fatalf("chunk %v, err %v", c, err)
	}
	if _, err := s.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("err = %v, want io.EOF", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}
