package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"

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

// errInContainer is what `restart` says in the container image, where the
// entrypoint decides, before the daemon starts, what a restart of the daemon
// alone would leave as it was: the owners of the volumes, the browser sidecar.
var errInContainer = errors.New("this is a container: restart it from the host instead (`docker restart <container>`), so that its entrypoint runs again")

func newRestartCmd(c *cli) *cobra.Command {
	cmd := &cobra.Command{Use: "restart", Short: "restart the daemon, which reads its config again", Args: cobra.NoArgs}
	cn := c.connFlags(cmd)
	cmd.RunE = func(cmd *cobra.Command, _ []string) error { return cmdRestart(cmd.Context(), c, cn) }
	return cmd
}

// cmdRestart has the running daemon restart in its process. The container
// image sets $HOLACLAW_CONTAINER, and there it refuses.
func cmdRestart(ctx context.Context, c *cli, cn *conn) error {
	if os.Getenv("HOLACLAW_CONTAINER") != "" {
		return errInContainer
	}
	client, err := cn.dial(ctx)
	if err != nil {
		return err
	}
	if err := client.Restart(ctx); err != nil {
		return err
	}
	_, _ = fmt.Fprintln(c.stdout, "the daemon is restarting")
	return nil
}
