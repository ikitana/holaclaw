package daemon

import (
	"context"
	"io"

	"holaclaw/internal/media"
)

// normaliser returns the (cached) media normaliser of an agent, built from
// its `transcribe` and `vision` roles (docs/models.md).
func (d *Daemon) normaliser(agentName string) *media.Normaliser {
	d.mu.Lock()
	defer d.mu.Unlock()
	if n, ok := d.media[agentName]; ok {
		return n
	}
	a := d.App
	roles := a.Config.Agents[agentName].Models
	deps := media.Deps{
		Blobs: func(ctx context.Context, id string) (io.ReadCloser, int64, error) {
			rc, b, err := a.Store.OpenBlob(ctx, id)
			return rc, b.Size, err
		},
		SetDerived: a.Store.SetDerived,
		Events:     a.Events,
	}
	if chat, err := a.Config.Model(roles.Chat); err == nil {
		deps.ChatVision = chat.Vision
	}
	if ref, err := a.ModelRef(roles.Transcribe); roles.Transcribe != "" && err == nil {
		deps.Transcribe = &media.Role{Model: ref.Model, Provider: ref.Provider}
	}
	if ref, err := a.ModelRef(roles.Vision); roles.Vision != "" && err == nil {
		deps.Vision = &media.Role{Model: ref.Model, Provider: ref.Provider}
	}
	n := media.New(deps)
	d.media[agentName] = n
	return n
}
