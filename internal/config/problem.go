package config

import (
	"sort"
	"strings"
)

// Problem codes. Stable strings: they appear in `holaclaw check` output, in the
// JSON report and in documentation, so an existing code never changes meaning
// and a retired one is never reused (ARCHITECTURE.md §7).
//
//	CFG001  unknown key, with a "did you mean" over the valid keys of that table
//	CFG002  key in the wrong file; the message names the right one
//	CFG003  wrong value type for the key
//	CFG004  value outside the allowed set for the key
//	CFG005  required key missing
//	CFG006  dangling reference: model→provider, role→model, channel→agent, agent→connector
//	CFG007  `version` missing, not an integer, or not CurrentVersion
//	CFG008  files of one directory carry different versions
//	CFG009  a literal secret where only an `env:VAR` reference is allowed
//	CFG010  unknown tool name (only checked by Validate, which knows the registry)
//	CFG011  malformed value: unparseable TOML, bad duration, non-positive count,
//	        a bad connector id or one that is a built-in tool or group name,
//	        a connector granted twice
//	CFG012  missing or unreadable file in the config directory
const (
	CodeUnknownKey      = "CFG001"
	CodeWrongFile       = "CFG002"
	CodeWrongType       = "CFG003"
	CodeBadEnum         = "CFG004"
	CodeMissingRequired = "CFG005"
	CodeDanglingRef     = "CFG006"
	CodeBadVersion      = "CFG007"
	CodeVersionMismatch = "CFG008"
	CodeLiteralSecret   = "CFG009"
	CodeUnknownTool     = "CFG010"
	CodeBadValue        = "CFG011"
	CodeMissingFile     = "CFG012"
)

// Severity of a problem. Only Error makes Load fail.
type Severity string

const (
	SeverityError   Severity = "error"
	SeverityWarning Severity = "warning"
)

// Problem is one validation finding: a stable id, where it is and how to fix
// it — the shape `holaclaw check` reports (ARCHITECTURE.md §7).
type Problem struct {
	Code     string
	Severity Severity
	File     string // path relative to the config directory
	Key      string // dotted key path, empty for file-level problems
	Message  string
	Hint     string
}

func (p Problem) String() string {
	var b strings.Builder
	b.WriteString(p.Code)
	b.WriteString(" ")
	b.WriteString(string(p.Severity))
	b.WriteString(": ")
	b.WriteString(p.File)
	if p.Key != "" {
		b.WriteString(": ")
		b.WriteString(p.Key)
	}
	b.WriteString(": ")
	b.WriteString(p.Message)
	if p.Hint != "" {
		b.WriteString(" (")
		b.WriteString(p.Hint)
		b.WriteString(")")
	}
	return b.String()
}

// Problems is the error type Load and Validate return: every problem found,
// never just the first.
type Problems []Problem

func (ps Problems) Error() string {
	lines := make([]string, len(ps))
	for i, p := range ps {
		lines[i] = p.String()
	}
	return strings.Join(lines, "\n")
}

// HasErrors reports whether any problem has Error severity.
func (ps Problems) HasErrors() bool {
	for _, p := range ps {
		if p.Severity == SeverityError {
			return true
		}
	}
	return false
}

// Codes returns the codes in order, for tests and reports.
func (ps Problems) Codes() []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.Code
	}
	return out
}

// sortStable orders problems by file, then key, then code so output does not
// depend on map iteration order.
func (ps Problems) sortStable() {
	sort.SliceStable(ps, func(i, j int) bool {
		a, b := ps[i], ps[j]
		if a.File != b.File {
			return a.File < b.File
		}
		if a.Key != b.Key {
			return a.Key < b.Key
		}
		return a.Code < b.Code
	})
}

// collector accumulates problems for one file, and the cross-file references
// seen while walking it (nil when the caller only reports problems).
type collector struct {
	file string
	ps   *Problems
	refs *[]refUse
}

// at is a collector that only reports problems.
func at(file string, ps *Problems) collector { return collector{file: file, ps: ps} }

func (c collector) add(code, key, msg, hint string) {
	*c.ps = append(*c.ps, Problem{
		Code: code, Severity: SeverityError, File: c.file, Key: key, Message: msg, Hint: hint,
	})
}
