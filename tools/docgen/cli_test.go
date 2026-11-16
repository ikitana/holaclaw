package main

import (
	"strings"
	"testing"
)

func TestRenderCLI(t *testing.T) {
	root := cliNode{
		Path:  "holaclaw",
		Flags: []cliFlag{{Name: "config", Type: "string", Default: "/etc/holaclaw", Usage: "config directory"}},
		Commands: []cliNode{
			{Path: "holaclaw run", Group: "daemon", Short: "start the daemon", Usage: "holaclaw run [--debug]",
				Flags: []cliFlag{{Name: "debug", Type: "bool", Usage: "verbose logging"}}},
			{Path: "holaclaw chat", Group: "chat", Short: "talk to an agent", Usage: "holaclaw chat [-m text]",
				Flags: []cliFlag{{Name: "message", Shorthand: "m", Type: "string", Usage: "send one message | exit"}}},
			{Path: "holaclaw auth", Group: "admin", Short: "manage the secrets", Usage: "holaclaw auth [flags]",
				Commands: []cliNode{{Path: "holaclaw auth set", Short: "store one secret", Usage: "holaclaw auth set <entry> <key>",
					Long: "store one secret\n\nwith no value the secret is read from stdin"}}},
			{Path: "holaclaw approve", Group: "admin", Short: "approve one", Usage: "holaclaw approve <id>"},
		},
	}
	md, err := renderCLI(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"| `--config string` | `/etc/holaclaw` | config directory |",
		"## daemon\n\n### `holaclaw run`\n\nstart the daemon\n\n```\nusage: holaclaw run [--debug]\n```\n\n| Flag | Default | Description |\n|---|---|---|\n| `--debug` |  | verbose logging |",
		"| `-m`, `--message string` |  | send one message \\| exit |",
		"#### `holaclaw auth set`\n\nstore one secret\n\n```\nusage: holaclaw auth set <entry> <key>\n\nwith no value the secret is read from stdin\n```",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("cli.md is missing %q:\n%s", want, md)
		}
	}
	// Groups keep the tree's order; commands inside one are sorted.
	if strings.Index(md, "## daemon") > strings.Index(md, "## chat") || strings.Index(md, "## chat") > strings.Index(md, "## admin") {
		t.Errorf("groups out of order:\n%s", md)
	}
	if strings.Index(md, "`holaclaw approve`") > strings.Index(md, "`holaclaw auth`") {
		t.Errorf("commands of a group are not sorted:\n%s", md)
	}

	if _, err := renderCLI(cliNode{Path: "holaclaw"}); err == nil {
		t.Error("an empty tree rendered")
	}
}

// TestBuildAndRenderCLI builds the real binary and renders cli.md against it.
// Skipped under -short: it invokes `go build`.
func TestBuildAndRenderCLI(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the holaclaw binary")
	}
	t.Setenv("HOLACLAW_CONFIG", "/home/someone/holaclaw")
	dir := t.TempDir()
	bin, err := buildHolaClaw(dir)
	if err != nil {
		t.Fatal(err)
	}
	root, err := cliTree(bin)
	if err != nil {
		t.Fatal(err)
	}
	md, err := renderCLI(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"## daemon", "### `holaclaw run`", "### `holaclaw check`", "### `holaclaw chat`",
		"#### `holaclaw auth set`", "#### `holaclaw skills revert`", "`-v`, `--verbose`", "## Global flags"} {
		if !strings.Contains(md, want) {
			t.Errorf("cli.md is missing %q", want)
		}
	}
	if strings.Contains(md, "/root/") || strings.Contains(md, "/home/") {
		t.Errorf("cli.md leaked an absolute home path")
	}
	if strings.Contains(md, "__cli-tree") {
		t.Errorf("cli.md lists the hidden command")
	}

	if _, err := cliTree(dir + "/missing"); err == nil {
		t.Error("a missing binary gave a tree")
	}
	root2, err := cliTree(bin)
	if err != nil {
		t.Fatal(err)
	}
	if md2, _ := renderCLI(root2); md != md2 {
		t.Error("rendering is not deterministic across two runs against the same binary")
	}
}
