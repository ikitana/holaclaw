package mcp

import (
	"context"
	"sync"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// On a modern server the SDK caches every tools/list page by cursor for the
// server's TTL and has no way to evict it. A connection whose listings have
// cached more than this is replaced before the next ListTools.
const (
	maxCachedPages = 2 * maxPages
	maxCachedBytes = 2 * MaxListBytes
)

// conn is one SDK connection; dead closes when it ends.
type conn struct {
	cs     *sdk.ClientSession
	dead   chan struct{}
	modern bool // stateless protocol: no session id, tools/list is cached

	mu          sync.Mutex
	cachedPages int
	cachedBytes int
}

// noteCachedPage counts a tools/list page the SDK cached.
func (cn *conn) noteCachedPage(bytes int) {
	if !cn.modern {
		return
	}
	cn.mu.Lock()
	defer cn.mu.Unlock()
	cn.cachedPages++
	cn.cachedBytes += bytes
}

func (cn *conn) overCached() bool {
	cn.mu.Lock()
	defer cn.mu.Unlock()
	return cn.cachedPages >= maxCachedPages || cn.cachedBytes >= maxCachedBytes
}

// connect runs the handshake: server/discover at protocolVersion, or the
// SDK's fallback to initialize on a legacy server. It returns as soon as
// ctx ends: a failed SDK Connect closes its session before returning, and
// that close waits for the notifications/cancelled of the handshake (and,
// on a legacy server, a DELETE), each up to 5s against a server that does
// not answer. The SDK finishes that in the background, and a session that
// connected after all is closed. Nothing tracks that tail: current lets the
// next handshake start at once, beside it, and Client.Close and
// Manager.Close do not wait for it. Accepted: it holds one connection to a
// server that stopped answering, for no longer than those 5s waits.
func (c *Client) connect(ctx context.Context) (*conn, error) {
	st := &callState{}
	t := &sdk.StreamableClientTransport{
		Endpoint:     c.opts.URL,
		HTTPClient:   c.httpClient,
		MaxEventSize: MaxResponseBytes,
	}
	type connected struct {
		cs  *sdk.ClientSession
		err error
	}
	done := make(chan connected, 1)
	go func() {
		cs, err := c.sdkClient.Connect(withState(ctx, st), t, &sdk.ClientSessionOptions{ProtocolVersion: protocolVersion})
		done <- connected{cs, err}
	}()
	var r connected
	select {
	case r = <-done:
	case <-ctx.Done():
		go func() {
			if r := <-done; r.cs != nil {
				_ = r.cs.Close()
			}
		}()
		return nil, classify(ctx, "connect", ctx.Err(), st.get())
	}
	cs, err := r.cs, r.err
	if err != nil {
		return nil, classify(ctx, "connect", err, st.get())
	}
	cn := &conn{cs: cs, dead: make(chan struct{})}
	if ir := cs.InitializeResult(); ir != nil && ir.ProtocolVersion >= protocolVersion {
		cn.modern = true
	}
	go func() {
		_ = cs.Wait()
		close(cn.dead)
		// Forget and close it: a failed SDK connection keeps draining its
		// long-lived stream until closed.
		c.forget(cn)
		_ = cs.Close()
	}()
	return cn, nil
}

// current returns the live connection, connecting when there is none or
// when lost is still the current one. One handshake runs at a time;
// waiters honour ctx.
func (c *Client) current(ctx context.Context, lost *conn) (*conn, error) {
	for {
		c.mu.Lock()
		if c.closed {
			c.mu.Unlock()
			return nil, ErrClosed
		}
		if c.live != nil && c.live != lost {
			cn := c.live
			c.mu.Unlock()
			return cn, nil
		}
		if ch := c.connecting; ch != nil {
			c.mu.Unlock()
			select {
			case <-ch:
				continue
			case <-ctx.Done():
				return nil, classify(ctx, "connect", ctx.Err(), seen{})
			}
		}
		ch := make(chan struct{})
		c.connecting = ch
		old := c.live
		c.live = nil
		c.mu.Unlock()
		if old != nil {
			go func() { _ = old.cs.Close() }()
		}
		cn, err := c.connect(ctx)
		c.mu.Lock()
		c.connecting = nil
		close(ch)
		if err == nil && c.closed {
			go func() { _ = cn.cs.Close() }()
			err = ErrClosed
		} else if err == nil {
			c.live = cn
		}
		c.mu.Unlock()
		return cn, err
	}
}

// drop forgets cn so the next request connects again, and closes it.
func (c *Client) drop(cn *conn) {
	if c.forget(cn) {
		go func() { _ = cn.cs.Close() }()
	}
}

// forget clears cn as the current connection, reporting whether it was.
func (c *Client) forget(cn *conn) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if cn != nil && c.live == cn {
		c.live = nil
		return true
	}
	return false
}

// retireOverCached drops the current connection once the SDK's tools/list
// cache on it passed maxCachedPages or maxCachedBytes.
func (c *Client) retireOverCached() {
	c.mu.Lock()
	cn := c.live
	c.mu.Unlock()
	if cn != nil && cn.overCached() {
		c.drop(cn)
	}
}

// toolsChanged is the SDK's list_changed handler. It ignores notifications
// from replaced connections and after Close, and never blocks.
func (c *Client) toolsChanged(_ context.Context, req *sdk.ToolListChangedRequest) {
	if c.closeCtx.Err() != nil {
		return
	}
	c.mu.Lock()
	current := c.live == nil || c.live.cs == req.Session
	c.mu.Unlock()
	if !current {
		return
	}
	select {
	case c.changed <- struct{}{}:
	default:
	}
}

// notifyLoop runs OnToolsChanged, one call at a time, until Close.
func (c *Client) notifyLoop() {
	defer close(c.workerDone)
	for {
		select {
		case <-c.closeCtx.Done():
			return
		case <-c.changed:
			if c.closeCtx.Err() != nil {
				return
			}
			c.opts.OnToolsChanged()
		}
	}
}
