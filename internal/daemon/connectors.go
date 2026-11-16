package daemon

import (
	"context"
	"fmt"
	"strings"
	"time"

	"holaclaw/internal/connector"
)

// setupConnectors starts the connector manager over connectors.toml as
// loaded (docs/connectors.md §2). It makes no network call: connectors
// settle on the first check, which Run starts. The agents reach it through
// the app, the API through reservedIDs.
func (d *Daemon) setupConnectors(ctx context.Context) error {
	a := d.App
	m, err := connector.New(ctx, connector.Options{
		Dir: a.Config.Dir, Connectors: a.Config.Connectors, Store: a.Store, Secrets: a.Secrets,
		Events: a.Events, Logger: d.log,
	})
	if err != nil {
		return err
	}
	d.connectors = m
	a.Connectors = m
	d.Backend.Connectors = reservedIDs{Manager: m, names: builtinNames(a.Registry.Names())}
	return nil
}

// connectorChecks runs the health check once at start and then every
// connector.CheckInterval until ctx ends (docs/connectors.md §7).
func (d *Daemon) connectorChecks(ctx context.Context) {
	t := time.NewTicker(connector.CheckInterval)
	defer t.Stop()
	for {
		d.connectors.Check(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// builtinNames is every built-in tool name and every group before a dot
// ("memory" for "memory.save"): a connector's tools are named <id>.<tool>
// next to the built-in ones (docs/connectors.md §5), so a new id must be
// none of them. config.Validate refuses the same in a file.
func builtinNames(tools []string) map[string]bool {
	out := map[string]bool{}
	for _, t := range tools {
		out[t] = true
		if group, _, ok := strings.Cut(t, "."); ok {
			out[group] = true
		}
	}
	return out
}

// reservedIDs is the manager as the API sees it, refusing to create a
// connector under a built-in name.
type reservedIDs struct {
	*connector.Manager
	names map[string]bool
}

func (r reservedIDs) Create(ctx context.Context, req connector.CreateRequest) (connector.Started, error) {
	if r.names[req.ID] {
		return connector.Started{}, fmt.Errorf("%w: id %q is a built-in tool or group name", connector.ErrInvalid, req.ID)
	}
	return r.Manager.Create(ctx, req)
}
