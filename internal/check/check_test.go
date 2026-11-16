package check

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"holaclaw/internal/config"
)

// fullFixture is a config that trips several rules at once, in different
// files/keys, to exercise ordering.
func fullFixture(t *testing.T, dir, dataDir string) *Input {
	t.Helper()
	cfg := minimalConfig()
	cfg.Channels["tg-main"] = config.Channel{
		Name: "tg-main", Type: "telegram", Agent: "main",
		Access: config.Access{DM: "open", Rooms: "off", Reply: "mention"},
	}
	a := cfg.Agents["main"]
	a.Policy.Guest.Exec = "ask" // trips ACC001
	a.Policy.Exec = "allow"     // trips POL001
	cfg.Agents["main"] = a
	return &Input{
		ConfigDir: dir,
		Config:    cfg,
		DataDir:   dataDir,
		LoadProblems: config.Problems{
			{Code: "CFG001", Severity: config.SeverityError, File: "config.toml", Key: "z", Message: "unknown key"},
		},
	}
}

func TestRunOrderAndFiltering(t *testing.T) {
	dir := t.TempDir()
	in := fullFixture(t, dir, t.TempDir())

	all := Run(context.Background(), in, nil, nil)
	if len(all) < 3 {
		t.Fatalf("expected several findings, got %+v", all)
	}
	// Rule order first: CFG's finding (from the first rule) must precede
	// SEC/ACC/POL findings (later rules) regardless of file/key ordering.
	firstCFG, firstOther := -1, -1
	for i, f := range all {
		if strings.HasPrefix(f.ID, "CFG") && firstCFG < 0 {
			firstCFG = i
		}
		if !strings.HasPrefix(f.ID, "CFG") && firstOther < 0 {
			firstOther = i
		}
	}
	if firstCFG < 0 || firstOther < 0 || firstCFG > firstOther {
		t.Fatalf("CFG findings must sort before later rules' findings: %+v", all)
	}

	only := Run(context.Background(), in, []string{"ACC"}, nil)
	for _, f := range only {
		if !strings.HasPrefix(f.ID, "ACC") {
			t.Errorf("--only ACC leaked %s", f.ID)
		}
	}
	if len(only) == 0 {
		t.Fatal("--only ACC found nothing")
	}

	skipped := Run(context.Background(), in, nil, []string{"ACC", "POL", "CFG"})
	for _, f := range skipped {
		if strings.HasPrefix(f.ID, "ACC") || strings.HasPrefix(f.ID, "POL") || strings.HasPrefix(f.ID, "CFG") {
			t.Errorf("--skip left %s", f.ID)
		}
	}
}

func TestExitCode(t *testing.T) {
	cases := []struct {
		name string
		fs   []Finding
		want int
	}{
		{"none", nil, 0},
		{"info only", []Finding{{Severity: SeverityInfo}}, 0},
		{"warning", []Finding{{Severity: SeverityInfo}, {Severity: SeverityWarning}}, 1},
		{"error", []Finding{{Severity: SeverityWarning}, {Severity: SeverityError}}, 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ExitCode(c.fs); got != c.want {
				t.Errorf("ExitCode = %d, want %d", got, c.want)
			}
		})
	}
}

func TestWriteTextGolden(t *testing.T) {
	fs := []Finding{
		{ID: "SEC001", Severity: SeverityError, File: "auth.toml", Message: "world readable", Fix: "chmod 600 auth.toml"},
		{ID: "POL001", Severity: SeverityWarning, File: "agents/main/agent.toml", Key: "policy.exec", Message: "allow runs anything"},
	}
	var buf bytes.Buffer
	WriteText(&buf, fs)
	want := "SEC001 error: auth.toml: world readable (fix: chmod 600 auth.toml)\n" +
		"POL001 warning: agents/main/agent.toml: policy.exec: allow runs anything\n"
	if buf.String() != want {
		t.Errorf("WriteText =\n%s\nwant\n%s", buf.String(), want)
	}

	var empty bytes.Buffer
	WriteText(&empty, nil)
	if empty.String() != "ok: no findings\n" {
		t.Errorf("WriteText(nil) = %q", empty.String())
	}
}

func TestWriteJSONGolden(t *testing.T) {
	fs := []Finding{
		{ID: "SEC001", Severity: SeverityError, File: "auth.toml", Message: "world readable", Fix: "chmod 600 auth.toml"},
	}
	var buf bytes.Buffer
	if err := WriteJSON(&buf, fs); err != nil {
		t.Fatal(err)
	}
	var got []map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, buf.String())
	}
	if len(got) != 1 {
		t.Fatalf("got %d entries, want 1", len(got))
	}
	want := map[string]any{
		"id": "SEC001", "severity": "error", "file": "auth.toml",
		"message": "world readable", "fix": "chmod 600 auth.toml",
	}
	for k, v := range want {
		if got[0][k] != v {
			t.Errorf("%s = %v, want %v", k, got[0][k], v)
		}
	}
	if _, ok := got[0]["key"]; ok {
		t.Errorf("empty key should be omitted, got %v", got[0]["key"])
	}
}

func TestNoMutation(t *testing.T) {
	dir := t.TempDir()
	// A config file present on disk, plus an auth.toml — SEC005 and SEC001
	// read this tree without altering it.
	writeFile(t, dir, "config.toml", "version = 1\n")
	writeFile(t, dir, "auth.toml", "version = 1\n")
	before := hashTree(t, dir)

	in := fullFixture(t, dir, t.TempDir())
	_ = Run(context.Background(), in, nil, nil)

	after := hashTree(t, dir)
	if before != after {
		t.Fatalf("running the rules changed the config directory tree")
	}
}

func writeFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	if err := writeFileErr(dir, rel, content); err != nil {
		t.Fatal(err)
	}
}
