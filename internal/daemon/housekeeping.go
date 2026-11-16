package daemon

import (
	"context"
	"time"

	"holaclaw/internal/event"
	"holaclaw/internal/store"
)

// inboundKeep is how long an inbound dedupe key is kept: WhatsApp's server
// buffer, the longest any platform resends a message (#101).
const inboundKeep = 14 * 24 * time.Hour

// housekeeping prunes once at start and then daily.
func (d *Daemon) housekeeping(ctx context.Context) {
	t := time.NewTicker(24 * time.Hour)
	defer t.Stop()
	for {
		d.prune(ctx, time.Now())
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// prune applies retention (config: retention.events_days, 0 keeps events
// forever; docs/traceability.md) and drops the inbound dedupe keys older than
// inboundKeep, which no setting turns off: the table must stay bounded.
func (d *Daemon) prune(ctx context.Context, now time.Time) {
	if days := d.App.Config.Retention.EventsDays; days > 0 {
		cutoff := now.Add(-time.Duration(days) * 24 * time.Hour)
		if n, err := d.App.Store.PruneEvents(ctx, cutoff); err != nil {
			d.log.Warn("retention: prune events", "err", err)
		} else if n > 0 {
			d.log.Info("retention: pruned events", "count", n, "older_than_days", days)
		}
		// The ids of sent messages, which reactions are routed by, age the
		// same way; a reaction to an older message is dropped.
		if _, err := d.App.Store.PruneSent(ctx, cutoff); err != nil {
			d.log.Warn("retention: prune sent message ids", "err", err)
		}
	}
	if n, err := d.App.Store.PruneInbound(ctx, now.Add(-inboundKeep)); err != nil {
		d.log.Warn("prune inbound dedupe keys", "err", err)
	} else if n > 0 {
		d.log.Debug("pruned inbound dedupe keys", "count", n)
	}
}

// backupCheck is how often the daemon looks whether an automatic backup is
// due. The first look is one check in, not at start: a daemon that is being
// restarted over and over must not answer each start with a backup.
const backupCheck = time.Minute

// automaticBackups takes the daemon's own backups: one every backups.every,
// the newest backups.keep kept (ARCHITECTURE.md §4). keep = 0 turns them off.
func (d *Daemon) automaticBackups(ctx context.Context) {
	if d.backups == nil || d.App.Config.Backups.Keep <= 0 {
		return
	}
	t := time.NewTicker(backupCheck)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			if rec, started, err := d.backups.Automatic(now); err != nil {
				d.log.Warn("automatic backup", "err", err)
			} else if started {
				d.log.Info("automatic backup started", "id", rec.ID)
			}
		}
	}
}

// forgetSession removes what was derived from a session before the session
// itself goes: its recall memory. A failure aborts the deletion.
func (d *Daemon) forgetSession(ctx context.Context, id event.ID) error {
	_, err := d.App.Memory.ForgetSession(ctx, id)
	return err
}

// beforeDeleteUser stops everything that would keep acting for the user.
func (d *Daemon) beforeDeleteUser(ctx context.Context, id event.ID) error {
	if d.tasks == nil {
		return nil
	}
	n, err := d.tasks.CancelByUser(ctx, id)
	if n > 0 {
		d.log.Info("cancelled the tasks of a deleted user", "user", id, "count", n)
	}
	return err
}

// beforeEnable applies the missed-run policy to what came due while the
// identity was disabled, as a restart does for the daemon's downtime.
func (d *Daemon) beforeEnable(ctx context.Context, id event.ID) error {
	if d.tasks == nil {
		return nil
	}
	return d.tasks.RecoverCreator(ctx, id)
}

// afterEnable wakes the scheduler for the enabled identity's re-armed tasks.
func (d *Daemon) afterEnable(context.Context, event.ID) {
	if d.tasks != nil {
		d.tasks.Wake()
	}
}

// forgetUserMemory forgets a deleted identity's own partition in every agent,
// a user's DMs or a room's; the shared notes written there stay with the agent.
func (d *Daemon) forgetUserMemory(ctx context.Context, id event.ID) (int64, error) {
	i, err := d.App.Store.GetIdentity(ctx, id)
	if err != nil {
		return 0, err
	}
	if i.Kind == store.KindRoom {
		return d.App.Memory.ForgetPartition(ctx, RoomPartition(id))
	}
	return d.App.Memory.ForgetPartition(ctx, UserPartition(id))
}
