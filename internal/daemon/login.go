package daemon

import (
	"context"
	"fmt"
	"time"

	"holaclaw/internal/api/gen"
	"holaclaw/internal/provider/codex"
	"holaclaw/internal/store"
)

// providerLogin exposes the Codex device-code flow to the Control API. The
// poll loop runs in the daemon, the single owner of the rotating tokens.
type providerLogin struct{ d *Daemon }

func (p providerLogin) Start(ctx context.Context, name string) (gen.DeviceLogin, error) {
	cp, ok := p.d.App.Codex[name]
	if !ok {
		return gen.DeviceLogin{}, fmt.Errorf("provider %q: %w", name, store.ErrNotFound)
	}
	l := cp.Login()
	if _, err := l.Start(ctx); err != nil {
		return gen.DeviceLogin{}, err
	}
	go func() {
		// Detached from the request: the login outlives the HTTP call.
		bg, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Minute)
		defer cancel()
		for {
			done, err := l.Poll(bg)
			if done || (err != nil && l.State().Status != codex.LoginPending) || bg.Err() != nil {
				return
			}
		}
	}()
	return deviceLogin(l.State()), nil
}

func (p providerLogin) Status(_ context.Context, name string) (gen.DeviceLogin, error) {
	cp, ok := p.d.App.Codex[name]
	if !ok {
		return gen.DeviceLogin{}, fmt.Errorf("provider %q: %w", name, store.ErrNotFound)
	}
	return deviceLogin(cp.Login().State()), nil
}

// storeCredentials is api.Backend.StoreCredentials: a subscription Codex
// provider serves its tokens from memory, so it adopts the ones write stores;
// every other provider reads auth.toml on each request.
func (d *Daemon) storeCredentials(name string, write func() error) error {
	if cp, ok := d.App.Codex[name]; ok {
		return cp.Adopt(write)
	}
	return write()
}

func deviceLogin(s codex.LoginState) gen.DeviceLogin {
	out := gen.DeviceLogin{State: gen.DeviceLoginState(s.Status)}
	if s.DeviceCode.UserCode != "" {
		out.UserCode = &s.DeviceCode.UserCode
		out.VerificationUrl = &s.DeviceCode.VerificationURL
		out.ExpiresAt = &s.DeviceCode.ExpiresAt
	}
	if s.Detail != "" {
		out.Detail = &s.Detail
	}
	return out
}
