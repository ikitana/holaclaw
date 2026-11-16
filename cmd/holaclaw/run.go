package main

import (
	"log/slog"
	"net"

	"github.com/spf13/cobra"

	"holaclaw/internal/daemon"
)

// newRunCmd starts the daemon and serves until interrupted.
func newRunCmd(c *cli) *cobra.Command {
	cmd := &cobra.Command{Use: "run", Short: "start the daemon", Args: cobra.NoArgs}
	debug := cmd.Flags().Bool("debug", false, "verbose logging")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		level := slog.LevelInfo
		if *debug {
			level = slog.LevelDebug
		}
		log := slog.New(slog.NewTextHandler(c.stderr, &slog.HandlerOptions{Level: level}))
		return daemon.Serve(cmd.Context(), daemon.Options{
			ConfigDir: c.configDir, Logger: log,
			OnListen: func(kind string, addr net.Addr) { log.Info("listening", "kind", kind, "addr", addr.String()) },
		})
	}
	return cmd
}
