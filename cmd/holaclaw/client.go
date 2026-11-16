package main

import (
	"context"
	"errors"
	"io/fs"
	"net"
	"os"
	"syscall"

	"github.com/spf13/cobra"

	"holaclaw/internal/api"
	"holaclaw/internal/app"
	"holaclaw/internal/daemon"
)

// errNoDaemon is the one thing to say when nothing is listening. Commands
// that need the daemon print it and stop; `chat` falls back to its in-process
// mode on it.
var errNoDaemon = errors.New("daemon is not running (start it with `holaclaw run`)")

// conn is the address flags every command that talks to the daemon carries:
// an explicit --addr/--token, else the environment, else the socket of the
// config directory — which is admin by file permission (docs/identity.md).
type conn struct {
	dir   *string
	addr  *string
	token *string
}

// connFlags declares --addr and --token on cmd; --config is the root's.
func (c *cli) connFlags(cmd *cobra.Command) *conn {
	fs := cmd.Flags()
	cn := &conn{
		dir:  &c.configDir,
		addr: fs.String("addr", os.Getenv("HOLACLAW_ADDR"), "daemon base URL or socket path ($HOLACLAW_ADDR)"),
		// argv is world-readable in "ps": the environment variable is the way
		// to pass a token, and --token stays for scripts that have no choice.
		token: fs.String("token", os.Getenv("HOLACLAW_TOKEN"),
			`API token, for a TCP address; prefer $HOLACLAW_TOKEN — an argument is visible to every process in "ps"`),
	}
	// A default from the environment is a secret: help must not print it.
	fs.Lookup("token").DefValue = ""
	return cn
}

// dial connects and proves the daemon answers, so every command reports a
// dead daemon the same way instead of failing on its first operation.
func (cn *conn) dial(ctx context.Context) (*api.Client, error) {
	addr := *cn.addr
	if addr == "" {
		a, err := app.Static(*cn.dir)
		if err != nil {
			return nil, err
		}
		addr = daemon.SocketPath(a)
	}
	c, err := api.Dial(addr, *cn.token)
	if err != nil {
		return nil, err
	}
	if _, err := c.Status(ctx); err != nil {
		return nil, notRunning(err)
	}
	return c, nil
}

// notRunning turns a transport failure into errNoDaemon. An answer from the
// daemon — a 401 from a wrong token, say — is passed through unchanged: it
// would be wrong to tell the user the daemon is down when it just refused.
func notRunning(err error) error {
	var ce *api.ClientError
	if errors.As(err, &ce) {
		return err
	}
	switch {
	case errors.Is(err, syscall.ECONNREFUSED), errors.Is(err, fs.ErrNotExist), errors.Is(err, syscall.ECONNRESET):
		return errNoDaemon
	}
	var oe *net.OpError
	if errors.As(err, &oe) {
		return errNoDaemon
	}
	return err
}
