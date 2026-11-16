package main

import (
	"os"
	"strings"
)

// checkRulesRel is the single source of truth for the check rules; this
// file only copies it under a generated header.
const checkRulesRel = "internal/check/RULES.md"

// renderCheckRulesMD copies internal/check/RULES.md verbatim under a
// generated-doc header. The single source stays RULES.md; this is a mirror
// for the docs/reference tree.
func renderCheckRulesMD() (string, error) {
	b, err := os.ReadFile(inRepo(checkRulesRel))
	if err != nil {
		return "", err
	}
	body := strings.TrimRight(string(b), "\n")
	var out strings.Builder
	out.WriteString("Mirrors `" + checkRulesRel + "`, the single source of truth.\n\n")
	out.WriteString(body)
	out.WriteString("\n")
	return out.String(), nil
}
