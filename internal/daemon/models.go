package daemon

import (
	"context"
	"sort"
	"time"

	"holaclaw/internal/provider"
)

// modelCheckInterval is how often the daemon asks its providers what they
// serve: a provider retires ids while the daemon runs.
const modelCheckInterval = 24 * time.Hour

// modelListTimeout bounds one provider's answer.
const modelListTimeout = 30 * time.Second

// modelChecks runs checkModels once at start and then every
// modelCheckInterval until ctx ends.
func (d *Daemon) modelChecks(ctx context.Context) {
	t := time.NewTicker(modelCheckInterval)
	defer t.Stop()
	for {
		d.checkModels(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// checkModels asks every provider that can list its models
// (provider.ModelLister) and warns about each configured model whose id is
// not served, as `holaclaw check` reports it (MOD003). It only warns: a turn
// on such a model fails with the provider's own error. A model with its own
// url is not served at the provider's base_url, and is left out.
func (d *Daemon) checkModels(ctx context.Context) {
	cfg := d.App.Config
	byProvider := map[string][]string{}
	for name, m := range cfg.Models {
		if m.ID != "" && m.URL == "" {
			byProvider[m.Provider] = append(byProvider[m.Provider], name)
		}
	}
	providers := make([]string, 0, len(byProvider))
	for name := range byProvider {
		providers = append(providers, name)
	}
	sort.Strings(providers)
	for _, name := range providers {
		lister, ok := d.App.Providers[name].(provider.ModelLister)
		if !ok {
			continue
		}
		lctx, cancel := context.WithTimeout(ctx, modelListTimeout)
		ids, err := lister.Models(lctx)
		cancel()
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			d.log.Info("model list not read", "provider", name, "err", err)
			continue
		}
		models := byProvider[name]
		sort.Strings(models)
		for _, model := range models {
			if id := cfg.Models[model].ID; !provider.Serves(ids, id) {
				d.log.Warn("the provider does not serve a configured model id: turns on it will fail",
					"model", model, "id", id, "provider", name)
			}
		}
	}
}
