package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
)

// cliNode is one command as `holaclaw __cli-tree` prints it (cmd/holaclaw/
// clitree.go). The two are separate programs, so the shape is repeated here.
type cliNode struct {
	Path     string    `json:"path"`
	Group    string    `json:"group"`
	Short    string    `json:"short"`
	Long     string    `json:"long"`
	Usage    string    `json:"usage"`
	Flags    []cliFlag `json:"flags"`
	Commands []cliNode `json:"commands"`
}

type cliFlag struct {
	Name      string `json:"name"`
	Shorthand string `json:"shorthand"`
	Type      string `json:"type"`
	Default   string `json:"default"`
	Usage     string `json:"usage"`
}

// buildHolaClaw builds the cmd/holaclaw binary into dir and returns its path.
// It runs from the repository root regardless of the caller's working
// directory (go run vs. go test).
func buildHolaClaw(dir string) (string, error) {
	bin := dir + "/holaclaw"
	cmd := exec.Command("go", "build", "-o", bin, "./cmd/holaclaw")
	cmd.Dir = repoRoot
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("docgen: building holaclaw: %w: %s", err, stderr.String())
	}
	return bin, nil
}

// cleanEnv returns an environment with every HOLACLAW_* variable removed, so
// flag defaults come out the same regardless of who runs this generator.
func cleanEnv() []string {
	var out []string
	for _, e := range os.Environ() {
		if strings.HasPrefix(e, "HOLACLAW_") {
			continue
		}
		out = append(out, e)
	}
	return out
}

// cliTree runs the built binary's hidden `__cli-tree` and decodes it.
func cliTree(bin string) (cliNode, error) {
	cmd := exec.Command(bin, "__cli-tree")
	cmd.Env = cleanEnv()
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return cliNode{}, fmt.Errorf("docgen: holaclaw __cli-tree: %w: %s", err, stderr.String())
	}
	var root cliNode
	if err := json.Unmarshal(out.Bytes(), &root); err != nil {
		return cliNode{}, fmt.Errorf("docgen: holaclaw __cli-tree: %w", err)
	}
	return root, nil
}

// renderCLIMD builds the holaclaw binary into a temp directory and renders
// its command tree.
func renderCLIMD() (string, error) {
	tmp, err := os.MkdirTemp("", "docgen-cli-*")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(tmp) }()

	bin, err := buildHolaClaw(tmp)
	if err != nil {
		return "", err
	}
	root, err := cliTree(bin)
	if err != nil {
		return "", err
	}
	return renderCLI(root)
}

// cliConventions is the part of the reference no command declares: how the
// CLI reads its arguments and what it exits with.
const cliConventions = `Generated from the command tree of ` + "`holaclaw`" + `, grouped as ` + "`holaclaw help`" + `
groups it.

- Long flags take two dashes: ` + "`--config`" + `. The single-dash spelling (` + "`-config`" + `)
  is not accepted. One-letter shorthands take one: ` + "`-m`" + `, ` + "`-q`" + `, ` + "`-h`" + `, and
  ` + "`-v`" + `, which counts: ` + "`-vv`" + ` is two.
- Flags may come before or after the arguments, except in ` + "`auth set`" + `,
  ` + "`memory add`" + ` and ` + "`memory search`" + `: there everything after the first argument is
  taken as typed, so a value or a text may start with a dash. ` + "`--`" + ` ends the flags
  anywhere.
- ` + "`-h`" + `, ` + "`--help`" + ` and ` + "`holaclaw help <command>`" + ` print help to stdout and exit 0. A
  wrong invocation — an unknown command or flag, a missing argument — prints the
  error and the usage to stderr and exits 2; any other failure exits 1.
- The config directory is ` + "`--config`" + `, else ` + "`$HOLACLAW_CONFIG`" + `, else ` + "`/etc/holaclaw`" + `. When
  ` + "`/etc/holaclaw`" + ` does not exist, a user other than root gets ` + "`$XDG_CONFIG_HOME/holaclaw`" + `
  (` + "`~/.config/holaclaw`" + `) instead, and the control socket ` + "`$XDG_RUNTIME_DIR/holaclaw.sock`" + `.
  Commands that talk to the daemon take ` + "`--addr`" + ` and ` + "`--token`" + `, else
  ` + "`$HOLACLAW_ADDR`" + ` and ` + "`$HOLACLAW_TOKEN`" + `, else the socket of the config directory.
- ` + "`holaclaw completion bash|zsh|fish|powershell`" + ` prints a shell completion script.
`

// renderCLI renders the tree: the global flags, then one section per help
// group with its commands in name order, each followed by its subcommands.
func renderCLI(root cliNode) (string, error) {
	if len(root.Commands) == 0 {
		return "", fmt.Errorf("docgen: `holaclaw __cli-tree` listed no commands")
	}
	var b strings.Builder
	b.WriteString("# CLI reference\n\n")
	b.WriteString(cliConventions)
	b.WriteString("\n## Global flags\n\n")
	writeFlags(&b, root.Flags)

	groupsSeen := map[string]bool{}
	var groupOrder []string
	for _, c := range root.Commands {
		if !groupsSeen[c.Group] {
			groupsSeen[c.Group] = true
			groupOrder = append(groupOrder, c.Group)
		}
	}
	for _, g := range groupOrder {
		fmt.Fprintf(&b, "## %s\n\n", g)
		var inGroup []cliNode
		for _, c := range root.Commands {
			if c.Group == g {
				inGroup = append(inGroup, c)
			}
		}
		sort.SliceStable(inGroup, func(i, j int) bool { return inGroup[i].Path < inGroup[j].Path })
		for _, c := range inGroup {
			writeCommand(&b, c, 3)
		}
	}
	return b.String(), nil
}

// writeCommand writes one command under a heading of the given level, and
// its subcommands one level down.
func writeCommand(b *strings.Builder, c cliNode, level int) {
	fmt.Fprintf(b, "%s `%s`\n\n%s\n\n", strings.Repeat("#", min(level, 6)), c.Path, c.Short)
	fmt.Fprintf(b, "```\nusage: %s\n", c.Usage)
	// Help shows the long text instead of the summary; here the summary is
	// already above it.
	if long := strings.TrimSpace(strings.TrimPrefix(c.Long, c.Short)); long != "" {
		fmt.Fprintf(b, "\n%s\n", long)
	}
	b.WriteString("```\n\n")
	if len(c.Flags) > 0 {
		writeFlags(b, c.Flags)
	}
	for _, sub := range c.Commands {
		writeCommand(b, sub, level+1)
	}
}

// writeFlags writes a flag table.
func writeFlags(b *strings.Builder, flags []cliFlag) {
	b.WriteString("| Flag | Default | Description |\n|---|---|---|\n")
	for _, f := range flags {
		name := "`--" + f.Name
		if f.Type != "bool" && f.Type != "count" {
			name += " " + f.Type
		}
		name += "`"
		if f.Shorthand != "" {
			name = "`-" + f.Shorthand + "`, " + name
		}
		def := ""
		if f.Default != "" {
			def = "`" + f.Default + "`"
		}
		fmt.Fprintf(b, "| %s | %s | %s |\n", name, def, cell(f.Usage))
	}
	b.WriteString("\n")
}

// cell makes text safe inside a Markdown table cell: a pipe would end the
// cell, and <id> would be read as a tag.
var cell = strings.NewReplacer("|", `\|`, "<", `\<`, "\n", " ").Replace
