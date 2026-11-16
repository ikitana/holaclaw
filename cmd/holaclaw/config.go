package main

import (
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"holaclaw/internal/config"
)

// newConfigCmd is the config file group. `config migrate` is the single
// writer of the config directory (ARCHITECTURE.md §4); everything else here only
// reads.
func newConfigCmd(c *cli) *cobra.Command {
	cmd := &cobra.Command{Use: "config", Short: "migrate the config files, print their schema, or the keys the environment sets"}
	cmd.AddCommand(newConfigMigrateCmd(c), newConfigSchemaCmd(c), newConfigEnvCmd(c))
	return cmd
}

// newConfigEnvCmd prints the keys an environment variable may set, with
// their value and where it came from: the variable, config.toml or the
// default.
func newConfigEnvCmd(c *cli) *cobra.Command {
	return &cobra.Command{
		Use:   "env",
		Short: "print the keys an environment variable may set, their value and where it came from",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			cfg, err := config.Load(c.configDir)
			if err != nil {
				return err
			}
			tw := tabwriter.NewWriter(c.stdout, 0, 0, 2, ' ', 0)
			_, _ = fmt.Fprintln(tw, "KEY\tVALUE\tSOURCE")
			for _, o := range config.EnvOverrides {
				src := cfg.Sources[o.Key]
				if src == "env" {
					src = "$" + o.Env
				}
				_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\n", o.Key, cfg.Value(o.Key), src)
			}
			return tw.Flush()
		},
	}
}

// newConfigMigrateCmd prints the diff of migrating the config directory to
// the current version. Without --write nothing on disk changes.
func newConfigMigrateCmd(c *cli) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "migrate",
		Short: "print the migration of the config directory to the current version",
		Args:  cobra.NoArgs,
	}
	write := cmd.Flags().Bool("write", false, "write the migration (one timestamped backup is made first)")
	cmd.RunE = func(*cobra.Command, []string) error {
		diff, err := config.Migrate(c.configDir, *write)
		if err != nil {
			return err
		}
		if diff == "" {
			_, _ = fmt.Fprintf(c.stdout, "config is already at version %d; nothing to migrate\n", config.CurrentVersion)
			return nil
		}
		_, _ = fmt.Fprint(c.stdout, diff)
		if !*write {
			_, _ = fmt.Fprintln(c.stdout, "\n(dry run: nothing was written; re-run with --write)")
		}
		return nil
	}
	return cmd
}

// newConfigSchemaCmd prints the published JSON Schema of one config file.
func newConfigSchemaCmd(c *cli) *cobra.Command {
	return &cobra.Command{
		Use:   "schema <" + strings.Join(config.SchemaFiles(), "|") + ">",
		Short: "print the JSON Schema of one config file",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			body, err := config.Schema(args[0])
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintf(c.stdout, "%s\n", body)
			return nil
		},
	}
}
