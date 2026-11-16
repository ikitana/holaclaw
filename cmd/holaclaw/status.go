package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"holaclaw/internal/api"
	"holaclaw/internal/daemon"
)

func newStatusCmd(c *cli) *cobra.Command {
	cmd := &cobra.Command{Use: "status", Short: "daemon status and channels", Args: cobra.NoArgs}
	cn := c.connFlags(cmd)
	cmd.RunE = func(cmd *cobra.Command, _ []string) error { return cmdStatus(cmd.Context(), c, cn) }
	return cmd
}

// cmdStatus prints what the daemon reports about itself and its channels.
func cmdStatus(ctx context.Context, c *cli, cn *conn) error {
	client, err := cn.dial(ctx)
	if err != nil {
		return err
	}
	st, err := client.Status(ctx)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(c.stdout, "version          %s\nuptime           %s\nsessions active  %d\n",
		st.Version, (time.Duration(st.UptimeSeconds) * time.Second).String(), st.SessionsActive)
	if st.PendingApprovals != nil {
		_, _ = fmt.Fprintf(c.stdout, "approvals        %d pending\n", *st.PendingApprovals)
	}

	channels, err := client.ListChannels(ctx)
	if err != nil {
		return err
	}
	if len(channels) == 0 {
		_, _ = fmt.Fprintln(c.stdout, "\nno channels configured")
	} else {
		_, _ = fmt.Fprintln(c.stdout)
		w := tabwriter.NewWriter(c.stdout, 0, 0, 2, ' ', 0)
		_, _ = fmt.Fprintln(w, "CHANNEL\tTYPE\tAGENT\tSTATE\tDETAIL")
		for _, ch := range channels {
			_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", ch.Name, ch.Type, ch.Agent, ch.State, deref(ch.Detail))
		}
		if err := w.Flush(); err != nil {
			return err
		}
	}
	return statusConnectors(ctx, c, client)
}

// statusConnectors adds one line per connector that is not connected
// (docs/connectors.md §8). These lines are optional: a token without the
// admin scope, a daemon without the route or without connectors prints
// nothing, and any other failure is one line on stderr, never the
// command's error.
func statusConnectors(ctx context.Context, c *cli, client *api.Client) error {
	cs, err := client.ListConnectors(ctx)
	var ce *api.ClientError
	if errors.As(err, &ce) && (ce.Status == http.StatusForbidden || ce.Status == http.StatusNotFound ||
		ce.Status == http.StatusNotImplemented) {
		return nil
	}
	if err != nil {
		if ctx.Err() != nil {
			return interrupted(ctx, err)
		}
		_, _ = fmt.Fprintln(c.stderr, "connectors: "+printable(err.Error()))
		return nil
	}
	var w *tabwriter.Writer
	for _, k := range cs {
		if k.State == "connected" {
			continue
		}
		if w == nil {
			_, _ = fmt.Fprintln(c.stdout)
			w = tabwriter.NewWriter(c.stdout, 0, 0, 2, ' ', 0)
			_, _ = fmt.Fprintln(w, "CONNECTOR\tSTATE\tDETAIL")
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\n", printable(k.Id), printable(k.State), orDash(printable(deref(k.Detail))))
	}
	if w == nil {
		return nil
	}
	return w.Flush()
}

// newVersionCmd prints the version stamped into the build.
func newVersionCmd(c *cli) *cobra.Command {
	return &cobra.Command{
		Use: "version", Short: "print the version", Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			_, _ = fmt.Fprintf(c.stdout, "holaclaw %s\n", daemon.Version)
			return nil
		},
	}
}
