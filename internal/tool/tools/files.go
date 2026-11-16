package tools

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"holaclaw/internal/provider"
	"holaclaw/internal/tool"
)

const (
	// maxReadBytes is the cap on one files.read result.
	maxReadBytes = 16 << 10
	// maxScanBytes is how much of a file read ever touches, whatever the
	// offset/limit asked for.
	maxScanBytes = 8 << 20
	// maxWriteBytes is the cap on one files.write body.
	maxWriteBytes = 1 << 20
	// maxListEntries is the cap on one files.list result.
	maxListEntries = 200
	// sniffBytes is how much of a file is inspected for NUL bytes.
	sniffBytes = 8000
)

// filesRead reads a text file from the workspace.
type filesRead struct{}

func (filesRead) Spec() provider.ToolSpec {
	return provider.ToolSpec{
		Name:        "files.read",
		Description: "Read a text file from the workspace. Paths are relative. offset is the first line, limit the line count.",
		Parameters: object{
			Properties: map[string]prop{"path": {Type: tString}, "offset": {Type: tInteger}, "limit": {Type: tInteger}},
			Required:   []string{"path"},
		}.raw(),
	}
}

func (filesRead) Risk() tool.Risk { return tool.RiskRead }

func (filesRead) Run(_ context.Context, c tool.Call) (tool.Result, error) {
	var a struct {
		Path   string `json:"path"`
		Offset int    `json:"offset"`
		Limit  int    `json:"limit"`
	}
	if err := decodeArgs(c.Args, &a); err != nil {
		return errf("bad arguments: %v", err), nil
	}
	p, err := relPath(a.Path)
	if err != nil {
		return errf("%v", err), nil
	}
	root, err := openWorkspace(c)
	if err != nil {
		return errf("%v", err), nil
	}
	defer root.Close() //nolint:errcheck // read-only

	f, err := root.Open(p)
	if err != nil {
		return jailErr("files.read", a.Path, err), nil
	}
	defer f.Close() //nolint:errcheck // read-only
	if st, err := f.Stat(); err == nil && st.IsDir() {
		return errf("files.read: %s is a directory; use files.list", a.Path), nil
	}
	body, err := io.ReadAll(io.LimitReader(f, maxScanBytes))
	if err != nil {
		return errf("files.read: %s: %v", a.Path, err), nil
	}
	head := body
	if len(head) > sniffBytes {
		head = head[:sniffBytes]
	}
	if bytes.IndexByte(head, 0) >= 0 {
		return errf("files.read: %s looks binary; use attach to send it", a.Path), nil
	}
	if a.Offset > 1 || a.Limit > 0 {
		body = selectLines(body, a.Offset, a.Limit)
	}
	return tool.Result{Text: capText(string(body), maxReadBytes)}, nil
}

// selectLines keeps limit lines starting at the 1-based offset (0 or 1 means
// the first line; limit 0 means to the end).
func selectLines(body []byte, offset, limit int) []byte {
	if offset < 1 {
		offset = 1
	}
	lines := strings.SplitAfter(string(body), "\n")
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	if offset-1 >= len(lines) {
		return nil
	}
	lines = lines[offset-1:]
	if limit > 0 && limit < len(lines) {
		lines = lines[:limit]
	}
	return []byte(strings.Join(lines, ""))
}

// filesWrite creates or appends to a file in the workspace.
type filesWrite struct{}

func (filesWrite) Spec() provider.ToolSpec {
	return provider.ToolSpec{
		Name:        "files.write",
		Description: "Write a file in the workspace, creating parent dirs. Paths are relative. append adds to the end, not replacing.",
		Parameters: object{
			Properties: map[string]prop{"path": {Type: tString}, "content": {Type: tString}, "append": {Type: tBoolean}},
			Required:   []string{"path", "content"},
		}.raw(),
	}
}

func (filesWrite) Risk() tool.Risk { return tool.RiskWrite }

func (filesWrite) Run(_ context.Context, c tool.Call) (tool.Result, error) {
	var a struct {
		Path    string `json:"path"`
		Content string `json:"content"`
		Append  bool   `json:"append"`
	}
	if err := decodeArgs(c.Args, &a); err != nil {
		return errf("bad arguments: %v", err), nil
	}
	p, err := relPath(a.Path)
	if err != nil {
		return errf("%v", err), nil
	}
	if len(a.Content) > maxWriteBytes {
		return errf("files.write: content is %d bytes, cap is %d; write it in parts", len(a.Content), maxWriteBytes), nil
	}
	root, err := openWorkspace(c)
	if err != nil {
		return errf("%v", err), nil
	}
	defer root.Close() //nolint:errcheck // the write is fsync-free anyway

	if dir := filepath.Dir(p); dir != "." {
		if err := root.MkdirAll(dir, 0o750); err != nil {
			return jailErr("files.write", a.Path, err), nil
		}
	}
	flags := os.O_WRONLY | os.O_CREATE
	if a.Append {
		flags |= os.O_APPEND
	} else {
		flags |= os.O_TRUNC
	}
	f, err := root.OpenFile(p, flags, 0o640)
	if err != nil {
		return jailErr("files.write", a.Path, err), nil
	}
	n, werr := f.WriteString(a.Content)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return errf("files.write: %s: %v", a.Path, werr), nil
	}
	verb := "wrote"
	if a.Append {
		verb = "appended"
	}
	return tool.Result{Text: fmt.Sprintf("%s %d bytes to %s", verb, n, p)}, nil
}

// filesList lists one directory level of the workspace.
type filesList struct{}

func (filesList) Spec() provider.ToolSpec {
	return provider.ToolSpec{
		Name:        "files.list",
		Description: "List one directory level of the workspace. path defaults to the workspace root. Directories end in /.",
		Parameters:  object{Properties: map[string]prop{"path": {Type: tString}}}.raw(),
	}
}

func (filesList) Risk() tool.Risk { return tool.RiskRead }

func (filesList) Run(_ context.Context, c tool.Call) (tool.Result, error) {
	var a struct {
		Path string `json:"path"`
	}
	if err := decodeArgs(c.Args, &a); err != nil {
		return errf("bad arguments: %v", err), nil
	}
	p := "."
	if strings.TrimSpace(a.Path) != "" {
		var err error
		if p, err = relPath(a.Path); err != nil {
			return errf("%v", err), nil
		}
	}
	root, err := openWorkspace(c)
	if err != nil {
		return errf("%v", err), nil
	}
	defer root.Close() //nolint:errcheck // read-only

	d, err := root.Open(p)
	if err != nil {
		return jailErr("files.list", p, err), nil
	}
	defer d.Close() //nolint:errcheck // read-only
	ents, err := d.ReadDir(-1)
	if err != nil {
		return errf("files.list: %s is not a directory", p), nil
	}
	sort.Slice(ents, func(i, j int) bool { return ents[i].Name() < ents[j].Name() })
	total := len(ents)
	if len(ents) > maxListEntries {
		ents = ents[:maxListEntries]
	}
	var b strings.Builder
	for _, e := range ents {
		if e.IsDir() {
			fmt.Fprintf(&b, "%s/\n", e.Name())
			continue
		}
		size := int64(-1)
		if fi, err := e.Info(); err == nil {
			size = fi.Size()
		}
		fmt.Fprintf(&b, "%s\t%d\n", e.Name(), size)
	}
	if total > len(ents) {
		fmt.Fprintf(&b, "[%d more entries not listed]\n", total-len(ents))
	}
	if total == 0 {
		b.WriteString("(empty)\n")
	}
	return tool.Result{Text: capText(b.String(), maxReadBytes)}, nil
}
