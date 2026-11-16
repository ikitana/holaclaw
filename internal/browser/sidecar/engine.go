package sidecar

import (
	"context"
	"io"

	"holaclaw/internal/browser/proto"
)

// Engine is what the sidecar server serves: the page layer
// (internal/browser/page) implements it, the server never touches CDP itself.
// Every method is safe for concurrent use across agents; calls for one agent
// are serialised by the engine. Errors that map onto proto.ErrorCode are
// *proto.Error values; anything else is 500 `browser`.
type Engine interface {
	// Health reports the resolved binary, its version and the fixed problem
	// sentence when no browser can run. It never starts a browser.
	Health(ctx context.Context) proto.Health
	// Sessions lists running browsers.
	Sessions(ctx context.Context) []proto.Session
	// Open starts the agent's browser or returns the running one, applying
	// req.Allow to it from now on; with req.Fresh it starts on a new blank
	// tab.
	Open(ctx context.Context, agent string, req proto.OpenRequest) (proto.Session, error)
	// Close stops the agent's browser and deletes its quarantined downloads.
	Close(ctx context.Context, agent string) error
	Observe(ctx context.Context, agent string, req proto.ObserveRequest) (proto.Observation, error)
	Act(ctx context.Context, agent string, a proto.Action) (proto.ActResult, error)
	// Screenshot is one JPEG of the active tab.
	Screenshot(ctx context.Context, agent string) ([]byte, error)
	// Download opens one quarantined file for reading and removes it from the
	// quarantine once the reader is closed. ErrBadAction for an unknown id.
	Download(ctx context.Context, agent, id string) (io.ReadCloser, proto.Download, error)
	// Stream runs the live view of the agent's browser until ctx ends: frames
	// and meta go to v, input from in is dispatched as it arrives. One Stream
	// per agent at a time; a second one fails with ErrHeld. Nothing read from
	// in is ever logged.
	Stream(ctx context.Context, agent string, in <-chan proto.StreamMessage, v Viewer) error
}

// Viewer receives the live view. Frame blocks until the viewer has room for
// the next frame — that block is the screencast's backpressure: the engine
// acknowledges the frame to Chromium only when Frame returns.
type Viewer interface {
	Frame(ctx context.Context, jpeg []byte, width, height int) error
	Meta(ctx context.Context, m proto.StreamMessage) error
}

// Errorf builds a *proto.Error for the wire.
func Errorf(code proto.ErrorCode, detail string) *proto.Error {
	return &proto.Error{Code: code, Detail: detail}
}
