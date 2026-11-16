package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"holaclaw/internal/app"
	"holaclaw/internal/auth/secrets"
	"holaclaw/internal/check"
	"holaclaw/internal/config"
	"holaclaw/internal/connector"
	"holaclaw/internal/provider"
	"holaclaw/internal/provider/openaicompat"
	"holaclaw/internal/skill"
	"holaclaw/internal/store"
	"holaclaw/internal/tool"
)

func newCheckCmd(c *cli) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "check",
		Short: "run the configuration checks",
		Args:  cobra.NoArgs,
	}
	o := checkOptions{dir: &c.configDir}
	o.asJSON = cmd.Flags().Bool("json", false, "print findings as JSON")
	o.only = cmd.Flags().String("only", "", "comma-separated rule ids or prefixes to run")
	o.skip = cmd.Flags().String("skip", "", "comma-separated rule ids or prefixes to skip")
	o.offline = cmd.Flags().Bool("offline", false, "skip every rule that needs the network")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error { return cmdCheck(cmd.Context(), c, o) }
	return cmd
}

// checkOptions are the flags of `check`.
type checkOptions struct {
	dir, only, skip *string
	asJSON, offline *bool
}

// cmdCheck runs the deterministic configuration checks. It is deliberately
// app-less: a broken config is exactly the case it exists for, so it loads
// the directory itself, keeps whatever parsed and hands the problems to the
// rules as findings (ARCHITECTURE.md §7).
func cmdCheck(ctx context.Context, c *cli, o checkOptions) error {
	dir, asJSON, only, skip, offline := o.dir, o.asJSON, o.only, o.skip, o.offline
	cfg, loadErr := config.Load(*dir)
	var problems config.Problems
	if loadErr != nil && !errors.As(loadErr, &problems) {
		return loadErr // not a config problem: unreadable directory, and so on
	}
	loaded := loadErr == nil

	registry, err := checkRegistry()
	if err != nil {
		return err
	}
	if loaded {
		var vps config.Problems
		if verr := config.Validate(cfg, registry.Names()); errors.As(verr, &vps) {
			problems = append(problems, vps...)
		} else if verr != nil {
			return verr
		}
	}

	secretsPath := secretsPathOf(cfg)
	// A malformed auth.toml is not a reason to skip every other rule: the
	// SEC rules read the path, and a nil file is an empty secret set.
	sec, _ := secrets.Open(secretsPath)
	dataDir := dataDirOf(cfg, *dir)
	rows := cachedConnectors(ctx, c, cfg, dataDir)

	in := &check.Input{
		ConfigDir:    *dir,
		Config:       cfg,
		LoadProblems: problems,
		Secrets:      sec,
		SecretsPath:  secretsPath,
		DataDir:      dataDir,
		KnownTools:   registry.Names(),
		Specs:        specsFor(cfg, registry, rows),
		Offline:      *offline,
		SkillRoots:   skillRootsFor(*dir),
		Connectors:   rows,
	}
	if !*offline {
		in.Probe = probeFor(cfg, sec)
	}

	findings := check.Run(ctx, in, split(*only), split(*skip))
	if *asJSON {
		if err := check.WriteJSON(c.stdout, findings); err != nil {
			return err
		}
	} else {
		writeEnvSources(c.stdout, cfg)
		check.WriteText(c.stdout, findings)
	}
	if code := check.ExitCode(findings); code != 0 {
		return exitError(code)
	}
	return nil
}

// writeEnvSources prints one line for every config key an environment
// variable set, so a value that is not in config.toml is not a surprise.
func writeEnvSources(w io.Writer, cfg *config.Config) {
	if cfg == nil {
		return
	}
	for _, o := range config.EnvOverrides {
		if cfg.Sources[o.Key] == "env" {
			_, _ = fmt.Fprintf(w, "env: %s = %s from $%s\n", o.Key, cfg.Value(o.Key), o.Env)
		}
	}
}

// checkRegistry is the tool registry `check` reasons about: the very set the
// daemon registers.
func checkRegistry() (*tool.Registry, error) { return app.NewRegistry() }

// dataDirOf mirrors app.Static: server.data_dir, else <config>/data.
func dataDirOf(cfg *config.Config, dir string) string {
	if cfg == nil {
		return app.DataDir(dir, "")
	}
	return app.DataDir(dir, cfg.Server.DataDir)
}

// secretsPathOf mirrors app.Static: auth.toml in server.secrets_dir, else in
// the default secrets directory. A relative secrets_dir is a CFG finding, and
// is not followed.
func secretsPathOf(cfg *config.Config) string {
	if cfg == nil || !filepath.IsAbs(cfg.Server.SecretsDir) {
		return app.SecretsPath("")
	}
	return app.SecretsPath(cfg.Server.SecretsDir)
}

// specsFor returns the tool schemas an agent would actually get (BUD001,
// BUD002): the registry's selection, then its connectors' cached tools. A
// connector id that is a built-in name (a CFG finding) contributes none.
func specsFor(cfg *config.Config, reg *tool.Registry, rows []store.Connector) func(string) []provider.ToolSpec {
	return func(name string) []provider.ToolSpec {
		if cfg == nil {
			return nil
		}
		a, ok := cfg.Agents[name]
		if !ok {
			return nil
		}
		var specs []provider.ToolSpec
		if len(a.Tools) > 0 {
			// An unknown tool is already a CFG finding: the connectors still
			// count, for BUD002.
			if exec, err := reg.Select(a.Tools); err == nil {
				specs = exec.Specs()
			}
		}
		ids := check.GrantedConnectors(reg.Names(), a.Connectors)
		return append(specs, connector.CachedSpecs(rows, ids)...)
	}
}

// cachedConnectors reads the connector cache rows, for the offline numbers
// of `check` and `prompt-size` and for CON002, when cfg has a connector in
// connectors.toml or some agent granted one. The store is opened
// read-only: nothing is created or migrated. A data directory without a
// database has no rows; a database that cannot be read has none either,
// and one line on stderr says why.
func cachedConnectors(ctx context.Context, c *cli, cfg *config.Config, dataDir string) []store.Connector {
	used := false
	if cfg != nil {
		used = len(cfg.Connectors) > 0
		for _, a := range cfg.Agents {
			used = used || len(a.Connectors) > 0
		}
	}
	if !used {
		return nil
	}
	rows, err := store.ReadConnectors(ctx, dataDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		_, _ = fmt.Fprintf(c.stderr, "connector cache not read: %v\n", err)
		return nil
	}
	return rows
}

// probeFor asks each provider's runtime what context it actually allocated
// (MOD002). Only OpenAI-compatible endpoints can answer.
func probeFor(cfg *config.Config, sec *secrets.File) func(context.Context, string, provider.Model) (int, string, error) {
	return func(ctx context.Context, name string, m provider.Model) (int, string, error) {
		if cfg == nil {
			return 0, "", openaicompat.ErrProbeUnsupported
		}
		pc, ok := cfg.Providers[name]
		if !ok || pc.Type != "openai-compatible" {
			return 0, "", openaicompat.ErrProbeUnsupported
		}
		return openaicompat.New(openaicompat.Options{
			Name: pc.Name, BaseURL: pc.BaseURL, APIKey: apiKeyOf(pc, sec),
		}).ProbeContext(ctx, m)
	}
}

// apiKeyOf resolves a provider key the way the daemon does: an `env:VAR`
// reference in config.toml wins, otherwise auth.toml.
func apiKeyOf(pc config.Provider, sec *secrets.File) func() string {
	if pc.APIKey != "" {
		return func() string {
			v, _ := config.ResolveEnv(pc.APIKey)
			return v
		}
	}
	if sec == nil {
		return nil
	}
	return sec.Lookup(secrets.Providers, pc.Name, "api_key")
}

// split turns a comma-separated flag value into a list.
func split(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// skillRootsFor mirrors app.SkillRoots: bundled skills plus the agent's own,
// in the config directory.
func skillRootsFor(dir string) func(string) skill.Roots {
	return func(name string) skill.Roots {
		return skill.Roots{
			Bundled: skill.Bundled,
			Agent:   filepath.Join(dir, "agents", name, "skills"),
		}
	}
}
