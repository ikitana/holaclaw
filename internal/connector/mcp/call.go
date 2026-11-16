package mcp

import (
	"context"
	"errors"
	"fmt"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// deadWait is how long an unsent call waits to learn whether the SDK
// connection died under it.
const deadWait = 250 * time.Millisecond

// call runs f under the call timeout, Close and the in-flight limit. A lost
// connection (a session 404, or a dead connection before anything was
// sent) is replaced once and f retried once.
func (c *Client) call(ctx context.Context, op string, f func(context.Context, *conn) error) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return fmt.Errorf("mcp: %s: %w", op, ErrClosed)
	}
	c.active++
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.active--
		c.mu.Unlock()
	}()
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	stop := context.AfterFunc(c.closeCtx, cancel)
	defer stop()
	err := c.attempts(ctx, op, f)
	if err != nil && c.closeCtx.Err() != nil {
		return fmt.Errorf("mcp: %s: %w", op, ErrClosed)
	}
	return err
}

// attempts takes a slot, then tries f on the current connection, and once
// more on a replacement when the first was lost.
func (c *Client) attempts(ctx context.Context, op string, f func(context.Context, *conn) error) error {
	select {
	case c.slots <- struct{}{}:
		defer func() { <-c.slots }()
	case <-ctx.Done():
		return classify(ctx, op, ctx.Err(), seen{})
	}
	cn, err := c.current(ctx, nil)
	if err != nil {
		return err
	}
	for retried := false; ; retried = true {
		s, err := attempt(ctx, cn, f)
		if err == nil {
			return nil
		}
		// A session 404 only means anything on the legacy protocol.
		if (!s.gone || cn.modern) && !lostUnsent(ctx, err, s, cn) {
			if errors.Is(err, sdk.ErrConnectionClosed) {
				c.drop(cn)
			}
			return classify(ctx, op, err, s)
		}
		if retried {
			c.drop(cn)
			return fmt.Errorf("mcp: %s: %w", op, ErrSessionLost)
		}
		if cn, err = c.current(ctx, cn); err != nil {
			return err
		}
	}
}

// attempt is one try of f on cn, returning what the transport saw.
func attempt(ctx context.Context, cn *conn, f func(context.Context, *conn) error) (seen, error) {
	st := &callState{}
	err := f(withState(ctx, st), cn)
	return st.get(), err
}

// lostUnsent reports a call that never reached the network because the SDK
// connection had already failed (e.g. its stream saw a 404 or an oversized
// event).
func lostUnsent(ctx context.Context, err error, s seen, cn *conn) bool {
	if s.sent || s.unauthorized || s.tokenErr != nil || ctx.Err() != nil {
		return false
	}
	if errors.Is(err, sdk.ErrConnectionClosed) || errors.Is(err, sdk.ErrSessionMissing) {
		return true
	}
	t := time.NewTimer(deadWait)
	defer t.Stop()
	select {
	case <-cn.dead:
		return true
	case <-t.C:
		return false
	}
}

// classify maps an SDK error to this package's errors, using what the
// transport recorded. It never includes a token.
func classify(ctx context.Context, op string, err error, s seen) error {
	switch {
	case s.unauthorized:
		return &UnauthorizedError{WWWAuthenticate: s.wwwAuth}
	case s.tokenErr != nil:
		return fmt.Errorf("mcp: %s: token: %w", op, s.tokenErr)
	case s.tooLarge:
		return fmt.Errorf("mcp: %s: %w", op, ErrTooLarge)
	case s.gone:
		return fmt.Errorf("mcp: %s: %w", op, errSessionGone)
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return fmt.Errorf("mcp: %s: timed out: %w", op, context.DeadlineExceeded)
	case ctx.Err() != nil:
		return fmt.Errorf("mcp: %s: %w", op, ctx.Err())
	}
	return fmt.Errorf("mcp: %s: %w", op, err)
}
