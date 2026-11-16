package main

import (
	"bufio"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
)

// operation is one HTTP operation scanned out of api/openapi.yaml.
type operation struct {
	method, path, operationID, scope, summary string
}

var (
	pathLineRe   = regexp.MustCompile(`^  (/\S*):$`)
	methodLineRe = regexp.MustCompile(`^    (get|post|put|delete|patch|options):$`)
)

// scanOpenAPI is a minimal, line-oriented scanner for THIS file's regular
// layout: a `paths:` block of `  /path:` entries, each holding one or more
// `    <method>:` blocks whose own fields sit at six-space indent. It is not
// a YAML parser — flow-style (`{ ... }`) content on one line is never
// mistaken for a field at line start, and nothing outside `paths:` is read.
//
// It fails loudly (returns an error) when an operation lacks an operationId
// or an x-scope, so the generated doc doubles as a lint (ARCHITECTURE.md §7).
func scanOpenAPI(path string) ([]operation, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	var ops []operation
	var cur *operation
	var curPath string
	inPaths := false
	inDescBlock := false
	descBlockIndent := -1

	flush := func() error {
		if cur == nil {
			return nil
		}
		if cur.operationID == "" {
			return fmt.Errorf("docgen: %s %s has no operationId", cur.method, cur.path)
		}
		if cur.scope == "" {
			return fmt.Errorf("docgen: %s %s (%s) has no x-scope", cur.method, cur.path, cur.operationID)
		}
		ops = append(ops, *cur)
		cur = nil
		return nil
	}

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()

		if !inPaths {
			if strings.HasPrefix(line, "paths:") {
				inPaths = true
			}
			continue
		}
		// A top-level key (no leading space) other than "paths:" ends the
		// section we care about.
		if line != "" && line[0] != ' ' {
			break
		}

		indent := len(line) - len(strings.TrimLeft(line, " "))

		if inDescBlock {
			if strings.TrimSpace(line) == "" {
				continue
			}
			if indent > descBlockIndent {
				if cur != nil && cur.summary == "" {
					cur.summary = strings.TrimSpace(line)
				}
				continue // still inside the block scalar; only the first line is the summary
			}
			inDescBlock = false
			// fall through: this line is the next field/method/path
		}

		if m := pathLineRe.FindStringSubmatch(line); m != nil {
			if err := flush(); err != nil {
				return nil, err
			}
			curPath = m[1]
			continue
		}
		if m := methodLineRe.FindStringSubmatch(line); m != nil {
			if err := flush(); err != nil {
				return nil, err
			}
			cur = &operation{method: strings.ToUpper(m[1]), path: curPath}
			continue
		}
		if cur == nil || indent < 6 {
			continue
		}
		trim := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trim, "operationId:"):
			cur.operationID = strings.TrimSpace(strings.TrimPrefix(trim, "operationId:"))
		case strings.HasPrefix(trim, "x-scope:"):
			cur.scope = strings.TrimSpace(strings.TrimPrefix(trim, "x-scope:"))
		case strings.HasPrefix(trim, "description:"):
			val := strings.TrimSpace(strings.TrimPrefix(trim, "description:"))
			if val == "|" || val == ">" || val == "|-" || val == ">-" {
				inDescBlock = true
				descBlockIndent = indent
			} else if val != "" {
				cur.summary = strings.Trim(val, `'"`)
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if err := flush(); err != nil {
		return nil, err
	}

	sort.SliceStable(ops, func(i, j int) bool {
		if ops[i].path != ops[j].path {
			return ops[i].path < ops[j].path
		}
		return ops[i].method < ops[j].method
	})
	return ops, nil
}

// renderAPIMD renders docs/reference/api.md from api/openapi.yaml.
func renderAPIMD() (string, error) {
	ops, err := scanOpenAPI(inRepo("api/openapi.yaml"))
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("# API reference\n\n")
	b.WriteString("Generated from `api/openapi.yaml`. Every operation below declares an\n")
	b.WriteString("`operationId` and an `x-scope`; a missing one fails the generator.\n\n")
	b.WriteString("| Method | Path | Operation | Scope | Summary |\n")
	b.WriteString("|---|---|---|---|---|\n")
	for _, op := range ops {
		fmt.Fprintf(&b, "| %s | `%s` | `%s` | %s | %s |\n", op.method, op.path, op.operationID, op.scope, mdCell(op.summary))
	}
	return b.String(), nil
}
