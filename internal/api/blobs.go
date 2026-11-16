package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"

	"holaclaw/internal/api/gen"
	"holaclaw/internal/auth"
	"holaclaw/internal/event"
)

// defaultMIME is what an upload without a usable content type becomes.
const defaultMIME = "application/octet-stream"

// sniffLen is what http.DetectContentType looks at.
const sniffLen = 512

// PutBlob stores an uploaded body content-addressed. The content type comes
// from the header; an absent or generic one is sniffed from the first bytes.
func (h *handlers) PutBlob(w http.ResponseWriter, r *http.Request) {
	body, mime, err := sniff(r.Body, r.Header.Get("Content-Type"))
	if err != nil {
		h.b.fail(w, "read blob", err)
		return
	}
	p, _ := PrincipalFrom(r.Context())
	b, err := h.b.Store.PutBlob(r.Context(), body, mime)
	if err != nil {
		h.b.fail(w, "put blob", err)
		return
	}
	// Remember the upload so the caller can read back what it just sent,
	// before any message references it.
	h.fresh.add(p.UserID, b.ID)
	writeJSON(w, http.StatusCreated, gen.Blob{Id: b.ID, Size: b.Size, Mime: b.MIME})
}

// GetBlob streams a blob back. The bytes are opaque and may be attacker
// supplied, so the browser is told not to sniff and not to render them.
func (h *handlers) GetBlob(w http.ResponseWriter, r *http.Request, id gen.Id) {
	p, _ := PrincipalFrom(r.Context())
	if !h.mayReadBlob(r.Context(), p, id) {
		notFound(w)
		return
	}
	rc, b, err := h.b.Store.OpenBlob(r.Context(), id)
	if err != nil {
		h.b.fail(w, "open blob", err)
		return
	}
	defer func() { _ = rc.Close() }()
	mime := b.MIME
	if mime == "" {
		mime = defaultMIME
	}
	w.Header().Set("Content-Type", mime)
	w.Header().Set("Content-Length", strconv.FormatInt(b.Size, 10))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Disposition", "attachment")
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	if _, err := io.Copy(w, rc); err != nil {
		h.b.logger().Warn("api: blob stream interrupted", "blob_id", b.ID, "error", err)
	}
}

// blobRefs is the read-only lookup GetBlob authorizes against; *store.SQLite
// implements it. It is an optional interface rather than a Store method so the
// persistence contract stays as it is.
type blobRefs interface {
	BlobSessions(ctx context.Context, blobID string) ([]event.ID, error)
}

// mayReadBlob is the per-identity authorization of GET /blobs/{id}: the socket
// reads everything; a token reads a blob that a message of a session it may
// see references, or one its identity uploaded and has not attached yet
// (docs/security.md). Anything else is a not_found — the existence of a blob
// is not the caller's business.
func (h *handlers) mayReadBlob(ctx context.Context, p auth.Principal, id string) bool {
	if isSocket(p) {
		return true
	}
	if p.UserID == "" {
		return false
	}
	if h.fresh.has(p.UserID, id) {
		return true
	}
	refs, ok := h.b.Store.(blobRefs)
	if !ok {
		// Without the lookup the gate cannot be applied, so it denies.
		h.b.logger().Error("api: store cannot resolve blob references")
		return false
	}
	sessions, err := refs.BlobSessions(ctx, id)
	if err != nil {
		h.b.logger().Error("api: blob sessions", "blob_id", id, "error", err)
		return false
	}
	return h.anyVisible(ctx, p, sessions)
}

// freshUploadsPerUser bounds the memory of uploads not yet attached, per user;
// freshUploadUsers bounds how many users are remembered at all.
const (
	freshUploadsPerUser = 64
	freshUploadUsers    = 256
)

// freshBlobs remembers, per user, the blobs that principal uploaded in this
// process so it can read them back before a message references them. It is
// deliberately in memory and bounded: a restart forgets, and a blob that
// matters has been attached to a message by then. The caveat is documented on
// GET /blobs/{id} in the spec.
type freshBlobs struct {
	mu sync.Mutex
	// per user, the most recent ids in arrival order.
	ids map[event.ID][]string
}

func newFreshBlobs() *freshBlobs { return &freshBlobs{ids: map[event.ID][]string{}} }

func (f *freshBlobs) add(user event.ID, id string) {
	if f == nil || user == "" || id == "" {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, known := f.ids[user]; !known && len(f.ids) >= freshUploadUsers {
		f.ids = map[event.ID][]string{}
	}
	list := append(f.ids[user], id)
	if len(list) > freshUploadsPerUser {
		list = append([]string(nil), list[len(list)-freshUploadsPerUser:]...)
	}
	f.ids[user] = list
}

func (f *freshBlobs) has(user event.ID, id string) bool {
	if f == nil {
		return false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, known := range f.ids[user] {
		if known == id {
			return true
		}
	}
	return false
}

// sniff returns the reader unchanged plus the content type to store: the
// declared one when it says something, otherwise what the first bytes look
// like. The peeked bytes are put back in front of the stream.
func sniff(r io.Reader, declared string) (io.Reader, string, error) {
	if declared != "" && declared != defaultMIME {
		return r, declared, nil
	}
	head := make([]byte, sniffLen)
	n, err := io.ReadFull(r, head)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, "", err
	}
	head = head[:n]
	return io.MultiReader(bytes.NewReader(head), r), http.DetectContentType(head), nil
}

// unmarshalStrict decodes a JSON document and refuses unknown fields, the way
// the request decoder does.
func unmarshalStrict(b []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("invalid JSON")
	}
	return nil
}

// wrapBody keeps a body-size error recognisable while hiding the reader's own
// message behind a stable one.
func wrapBody(err error, message string) error {
	if tooLarge(err) {
		return err
	}
	return errors.New(message)
}
