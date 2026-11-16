package whatsapp

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"

	"holaclaw/internal/channel"
)

// Status details of §5.3.
const (
	detailLinking   = "waiting for the code on the phone"
	detailLoggedOut = "unlinked from the phone, or WhatsApp logged this device out; run login again"
	detailReplaced  = "another client is using this session"
	detailOutdated  = "WhatsApp rejected this client version; update holaclaw"
)

func (a *Adapter) detailUnlinked() string {
	return "not linked: run `" + cliLine("login", a.name) + " --phone …`"
}

// cliLine is the `holaclaw login|logout whatsapp` command for channel name;
// the name is left out when it is "whatsapp", the CLI's default.
func cliLine(verb, name string) string {
	if name == "whatsapp" {
		return "holaclaw " + verb + " whatsapp"
	}
	return "holaclaw " + verb + " whatsapp --name " + name
}

// linkExpired is queued when a pairing code outlives codeTTL.
type linkExpired struct{}

// Run opens the session database and, when it holds a device, connects. With
// no device it reports unlinked and waits: a Link from the API pairs the same
// client, and the Connected that follows moves the instance on, with no
// restart. Every disconnect is mapped here (§5.3); whatsmeow's own
// auto-reconnect is off.
func (a *Adapter) Run(ctx context.Context, in channel.Inbound, status func(channel.State, string)) error {
	if a.path == "" {
		return errors.New("whatsapp: no session path")
	}
	a.stateMu.Lock()
	if a.runCtx != nil {
		a.stateMu.Unlock()
		return errors.New("whatsapp: already running")
	}
	a.runCtx, a.inbound, a.status = ctx, in, status
	a.stateMu.Unlock()

	cli, err := a.open(ctx, a.path, a.log)
	if err != nil {
		a.stateMu.Lock()
		a.runCtx = nil
		a.stateMu.Unlock()
		return err
	}
	a.install(cli)
	defer a.shutdown()

	l := &loop{a: a}
	if a.linked() {
		l.connect(ctx)
	} else {
		a.report(channel.StateUnlinked, a.detailUnlinked())
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-l.retry:
			l.retry = nil
			l.connect(ctx)
		case <-a.queueWake:
			for _, it := range a.drain() {
				if err := l.handle(ctx, it); err != nil {
					return err
				}
			}
		}
	}
}

// loop is the state Run keeps between events; only Run's goroutine touches it.
type loop struct {
	a       *Adapter
	retry   <-chan time.Time
	attempt int
}

// connect dials the stored device. A failure schedules the next try.
func (l *loop) connect(ctx context.Context) {
	err := l.a.connect(ctx)
	if errors.Is(err, whatsmeow.ErrAlreadyConnected) {
		return // whatsmeow got there first, after pairing; Connected follows
	}
	if err != nil && ctx.Err() == nil {
		l.reconnect("connect failed: " + err.Error())
	}
}

// reconnect reports reconnecting and schedules a try after the backoff.
func (l *loop) reconnect(detail string) {
	l.a.report(channel.StateReconnecting, detail)
	l.retry = l.a.after(l.a.backoff(l.attempt))
	l.attempt++
}

// handle maps one lifecycle event. An error stops Run: the session store
// could not be reopened.
func (l *loop) handle(ctx context.Context, it item) error {
	a := l.a
	a.stateMu.Lock()
	cli, current := a.client, it.clientGen == a.clientGen
	pending := a.link.Status == channel.LinkPending
	a.stateMu.Unlock()
	if !current || cli == nil {
		return nil
	}
	switch ev := it.event.(type) {
	case *events.PairSuccess:
		a.finishLink(it.clientGen, "linked as +"+ev.ID.User)
		// whatsmeow reconnects by itself after pairing; should that fail, no
		// event follows, so a retry stands by until Connected clears it.
		l.retry = a.after(a.backoff(l.attempt))
		l.attempt++
	case *events.PairError:
		a.failLink(it.clientGen, "pairing failed: "+ev.Error.Error())
	case linkExpired:
		a.failLink(it.clientGen, "the code expired; run the command again")
	case *events.Connected:
		l.retry, l.attempt = nil, 0
		pn, _ := cli.OwnID()
		a.finishLink(it.clientGen, "linked as +"+pn.User)
		a.report(channel.StateConnected, "+"+pn.User)
		a.announce(ctx, cli)
	case *events.PushNameSetting:
		// Presence needs the push name, which a fresh link learns late.
		a.announce(ctx, cli)
	case *events.Disconnected:
		if pending {
			a.failLink(it.clientGen, "WhatsApp closed the linking connection; run the command again")
		} else if a.linked() {
			l.reconnect("disconnected")
		}
	case *events.KeepAliveTimeout:
		// whatsmeow keeps a socket whose keepalives fail; past its own limit
		// it would reconnect, and with auto-reconnect off that is ours to do.
		// A timeout still in flight after our own Disconnect finds a retry
		// already armed and leaves it be.
		if l.retry == nil && a.now().Sub(ev.LastSuccess) > whatsmeow.KeepAliveMaxFailTime {
			cli.Disconnect()
			l.reconnect("keepalive timed out")
		}
	case *events.ConnectFailure:
		if pending {
			a.failLink(it.clientGen, "connect failure: "+ev.Reason.String())
		} else {
			l.reconnect("connect failure: " + ev.Reason.String())
		}
	case *events.LoggedOut:
		l.retry = nil
		if pending {
			a.failLink(it.clientGen, detailLoggedOut)
		}
		a.report(channel.StateLoggedOut, detailLoggedOut)
		return a.wipe(ctx)
	case *events.StreamReplaced:
		// No reconnect: two copies fighting over one session is the pattern
		// most likely to get the number flagged.
		l.retry = nil
		cli.Disconnect()
		a.report(channel.StateLoggedOut, detailReplaced)
	case *events.TemporaryBan:
		a.report(channel.StateLoggedOut, fmt.Sprintf("WhatsApp banned this number temporarily: %s, for %s", ev.Code, ev.Expire))
		wait := ev.Expire
		if wait <= 0 {
			// No end given: back off like any failure, not every 2 s.
			wait = a.backoff(l.attempt)
			l.attempt++
		}
		l.retry = a.after(wait)
	case *events.ClientOutdated:
		l.retry = nil
		if pending {
			a.failLink(it.clientGen, detailOutdated)
		} else {
			cli.Disconnect()
		}
		a.report(channel.StateLoggedOut, detailOutdated)
	}
	return nil
}

// announce sends the "available" presence the typing indicator needs, once
// per connection.
func (a *Adapter) announce(ctx context.Context, cli waClient) {
	err := cli.SendPresence(ctx, types.PresenceAvailable)
	switch {
	case errors.Is(err, whatsmeow.ErrNoPushName):
		a.log.Debug("whatsapp presence waits for the push name", "channel", a.name)
	case err != nil:
		a.log.Warn("whatsapp presence failed", "channel", a.name, "error", err)
	}
}

// backoff is the wait before reconnect attempt n: doubling from backoffBase,
// capped at backoffMax, jittered over its upper half.
func (a *Adapter) backoff(n int) time.Duration {
	d := backoffMax
	if n < 32 {
		if s := backoffBase << n; s > 0 && s < backoffMax {
			d = s
		}
	}
	half := d / 2
	return half + time.Duration(a.jitter(int64(d-half)+1))
}

// handler returns the event handler for client generation gen. It never
// blocks: the QR that a Link waits for is signalled here, lifecycle events go
// to Run's queue, and messages go to handleMessage, whose result decides the
// ack.
func (a *Adapter) handler(gen int) func(evt any) bool {
	return func(evt any) bool {
		switch evt.(type) {
		case *events.QR:
			a.stateMu.Lock()
			if gen == a.clientGen && a.firstQR != nil {
				close(a.firstQR)
				a.firstQR = nil
			}
			a.stateMu.Unlock()
			return true
		case *events.PairSuccess, *events.PairError, *events.Connected, *events.Disconnected,
			*events.KeepAliveTimeout, *events.ConnectFailure, *events.LoggedOut,
			*events.StreamReplaced, *events.TemporaryBan, *events.ClientOutdated, *events.PushNameSetting:
			a.enqueue(gen, evt)
			return true
		}
		return a.handleMessage(gen, evt)
	}
}

func (a *Adapter) enqueue(gen int, ev any) {
	a.stateMu.Lock()
	a.queue = append(a.queue, item{clientGen: gen, event: ev})
	a.stateMu.Unlock()
	select {
	case a.queueWake <- struct{}{}:
	default:
	}
}

func (a *Adapter) drain() []item {
	a.stateMu.Lock()
	defer a.stateMu.Unlock()
	q := a.queue
	a.queue = nil
	return q
}

// install makes cli the current client under a new generation.
func (a *Adapter) install(cli waClient) {
	a.stateMu.Lock()
	a.clientGen++
	a.client = cli
	gen := a.clientGen
	a.stateMu.Unlock()
	cli.AddEventHandler(a.handler(gen))
}

// replace closes the current client and opens a fresh one, deleting the
// session database first when wipe is set. The caller holds sessionMu.
func (a *Adapter) replace(ctx context.Context, wipe bool) error {
	a.stateMu.Lock()
	old := a.client
	a.client = nil
	a.clientGen++
	a.stateMu.Unlock()
	if old != nil {
		if err := old.Close(); err != nil {
			a.log.Warn("whatsapp session close failed", "channel", a.name, "error", err)
		}
	}
	if wipe {
		if err := removeSession(a.path); err != nil {
			return fmt.Errorf("whatsapp: delete session: %w", err)
		}
	}
	cli, err := a.open(ctx, a.path, a.log)
	if err != nil {
		return err
	}
	a.install(cli)
	return nil
}

// wipe deletes the session after a logout and opens an empty one, ready for
// a new link.
func (a *Adapter) wipe(ctx context.Context) error {
	a.sessionMu.Lock()
	defer a.sessionMu.Unlock()
	a.stateMu.Lock()
	if a.link.Status == channel.LinkPending {
		a.endLinkLocked(channel.LinkState{Status: channel.LinkFailed, Detail: detailLoggedOut})
	}
	a.stateMu.Unlock()
	return a.replace(ctx, true)
}

// connect dials the current client when it holds a linked device.
func (a *Adapter) connect(ctx context.Context) error {
	a.sessionMu.Lock()
	defer a.sessionMu.Unlock()
	a.stateMu.Lock()
	cli := a.client
	a.stateMu.Unlock()
	if cli == nil {
		return nil
	}
	if pn, _ := cli.OwnID(); pn.IsEmpty() {
		return nil
	}
	return cli.Connect(ctx)
}

// linked reports whether the current client holds a paired device.
func (a *Adapter) linked() bool {
	a.stateMu.Lock()
	cli := a.client
	a.stateMu.Unlock()
	if cli == nil {
		return false
	}
	pn, _ := cli.OwnID()
	return !pn.IsEmpty()
}

func (a *Adapter) report(s channel.State, detail string) {
	a.stateMu.Lock()
	status := a.status
	a.stateMu.Unlock()
	if status != nil {
		status(s, detail)
	}
}

// shutdown closes the client when Run returns. A pending link fails.
func (a *Adapter) shutdown() {
	a.sessionMu.Lock()
	defer a.sessionMu.Unlock()
	a.stateMu.Lock()
	cli := a.client
	a.client, a.runCtx, a.firstQR, a.queue = nil, nil, nil, nil
	a.clientGen++
	if a.link.Status == channel.LinkPending {
		a.endLinkLocked(channel.LinkState{Status: channel.LinkFailed, Detail: "the channel stopped"})
	}
	a.stateMu.Unlock()
	if cli != nil {
		if err := cli.Close(); err != nil {
			a.log.Warn("whatsapp session close failed", "channel", a.name, "error", err)
		}
	}
}
