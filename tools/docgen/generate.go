package main

import (
	"os"
	"path/filepath"
)

// Generate writes every reference doc into outDir, creating it if needed.
// Files are written in a fixed order with fixed names; nothing here depends
// on wall-clock time, environment variables (beyond what building and running
// the holaclaw binary itself needs) or map iteration order.
func Generate(outDir string) error {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}

	config, err := renderConfigMD()
	if err != nil {
		return err
	}
	if err := write(outDir, "config.md", config); err != nil {
		return err
	}

	cli, err := renderCLIMD()
	if err != nil {
		return err
	}
	if err := write(outDir, "cli.md", cli); err != nil {
		return err
	}

	apiDoc, err := renderAPIMD()
	if err != nil {
		return err
	}
	if err := write(outDir, "api.md", apiDoc); err != nil {
		return err
	}

	checkRules, err := renderCheckRulesMD()
	if err != nil {
		return err
	}
	if err := write(outDir, "check-rules.md", checkRules); err != nil {
		return err
	}

	if err := write(outDir, "events.md", renderEventsMD()); err != nil {
		return err
	}

	return nil
}

func write(outDir, name, body string) error {
	return os.WriteFile(filepath.Join(outDir, name), []byte(generatedHeader+body), 0o644)
}
