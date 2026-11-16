package slack

import (
	"context"
	"encoding/json"
	"errors"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/coder/websocket"

	"holaclaw/internal/channel"
)

const (
	readLimit    = 4 << 20
	pingInterval = 30 * time.Second
	pingTimeout  = 10 * time.Second
	backoffBase  = time.Second
	backoffMax   = 60 * time.Second
	ackDeadline  = 3 * time.Second // Slack drops the connection past this
	stateBotUser = "bot_user_id"
	stateTeamID  = "team_id"
)

// errDisconnect is the server asking us to reconnect now (no backoff).
var errDisconnect = errors.New("slack: disconnect requested")

// sockEnvelope is one Socket Mode frame.
type sockEnvelope struct {
	Type       string          `json:"type"`
	EnvelopeID string          `json:"envelope_id"`
	Reason     string          `json:"reason"`
	Payload    json.RawMessage `json:"payload"`
}

// Run keeps one Socket Mode connection up until ctx ends. Credentials that
// are rejected end the loop; everything else reconnects with backoff.
func (a *Adapter) Run(ctx context.Context, in channel.Inbound, status func(channel.State, string)) error {
	if status == nil {
		status = func(channel.State, string) {}
	}
	attempt := 0
	for {
		if ctx.Err() != nil {
			return nil
		}
		err := a.session(ctx, in, status)
		switch {
		case ctx.Err() != nil:
			return nil
		case isAuthError(err):
			status(channel.StateLoggedOut, a.redact(err.Error()))
			return err
		case errors.Is(err, errDisconnect):
			attempt = 0
			status(channel.StateReconnecting, "server asked to reconnect")
			continue
		}
		reason := "connection closed"
		if err != nil {
			reason = a.redact(err.Error())
		}
		status(channel.StateReconnecting, reason)
		a.log.Info("slack reconnecting", "reason", reason, "attempt", attempt+1)
		if err := sleepCtx(ctx, backoffFor(attempt)); err != nil {
			return nil
		}
		attempt++
	}
}

// backoffFor is exponential backoff with jitter, 1s to 60s.
func backoffFor(attempt int) time.Duration {
	d := backoffBase << min(attempt, 6)
	if d > backoffMax {
		d = backoffMax
	}
	// full jitter over the upper half keeps a floor under the delay
	return d/2 + time.Duration(rand.Int64N(int64(d/2)+1))
}

// session runs one connection: identity, wss URL, dial, envelope loop.
func (a *Adapter) session(ctx context.Context, in channel.Inbound, status func(channel.State, string)) error {
	if bot, _ := a.identity(); bot == "" {
		auth, err := a.authTest(ctx)
		if err != nil {
			return err
		}
		a.setIdentity(auth.UserID, auth.TeamID)
		a.saveIdentity(ctx, auth)
	}
	wssURL, err := a.connectionsOpen(ctx)
	if err != nil {
		return err
	}
	conn, _, err := websocket.Dial(ctx, wssURL, &websocket.DialOptions{HTTPClient: a.hc})
	if err != nil {
		return &Error{Op: "socket.dial", Message: a.redact(err.Error()), Err: err}
	}
	conn.SetReadLimit(readLimit)
	defer conn.CloseNow() //nolint:errcheck // best effort on teardown

	sctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	defer wg.Wait()
	defer cancel()

	wg.Add(1)
	go func() {
		defer wg.Done()
		a.keepalive(sctx, conn)
	}()

	for {
		_, data, err := conn.Read(sctx)
		if err != nil {
			if sctx.Err() != nil {
				return nil
			}
			return &Error{Op: "socket.read", Message: a.redact(err.Error()), Err: err}
		}
		var env sockEnvelope
		if err := json.Unmarshal(data, &env); err != nil {
			a.log.Warn("slack: undecodable envelope", "err", a.redact(err.Error()))
			continue
		}
		// Ack first: Slack disconnects us if an ack takes more than 3s, so
		// nothing that can block happens before this write.
		if env.EnvelopeID != "" {
			if err := a.ack(sctx, conn, env.EnvelopeID); err != nil {
				return err
			}
		}
		switch env.Type {
		case "hello":
			bot, _ := a.identity()
			status(channel.StateConnected, "connected as "+bot)
		case "disconnect":
			a.log.Info("slack disconnect envelope", "reason", env.Reason)
			return errDisconnect
		case "events_api":
			wg.Add(1)
			go func(p json.RawMessage) {
				defer wg.Done()
				a.handleEventsAPI(sctx, in, p)
			}(env.Payload)
		case "interactive":
			wg.Add(1)
			go func(p json.RawMessage) {
				defer wg.Done()
				a.handleInteractive(sctx, p)
			}(env.Payload)
		default:
			a.log.Debug("slack: ignored envelope", "type", env.Type)
		}
	}
}

// ack confirms one envelope well inside Slack's 3s window.
func (a *Adapter) ack(ctx context.Context, conn *websocket.Conn, envelopeID string) error {
	body, err := json.Marshal(struct {
		EnvelopeID string `json:"envelope_id"`
	}{envelopeID})
	if err != nil {
		return &Error{Op: "socket.ack", Message: a.redact(err.Error()), Err: err}
	}
	actx, cancel := context.WithTimeout(ctx, ackDeadline)
	defer cancel()
	if err := conn.Write(actx, websocket.MessageText, body); err != nil {
		return &Error{Op: "socket.ack", Message: a.redact(err.Error()), Err: err}
	}
	return nil
}

// keepalive pings until the connection or the session ends.
func (a *Adapter) keepalive(ctx context.Context, conn *websocket.Conn) {
	t := time.NewTicker(pingInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			pctx, cancel := context.WithTimeout(ctx, pingTimeout)
			err := conn.Ping(pctx)
			cancel()
			if err != nil {
				a.log.Debug("slack ping failed", "err", a.redact(err.Error()))
				conn.CloseNow() //nolint:errcheck // forces Read to return
				return
			}
		}
	}
}

// saveIdentity records who we are in the per-instance state store (ARCHITECTURE.md §6).
func (a *Adapter) saveIdentity(ctx context.Context, auth authTestResult) {
	if a.opt.State == nil {
		return
	}
	for k, v := range map[string]string{stateBotUser: auth.UserID, stateTeamID: auth.TeamID} {
		if err := a.opt.State.Set(ctx, k, v); err != nil {
			a.log.Warn("slack: state write failed", "key", k, "err", a.redact(err.Error()))
		}
	}
}
