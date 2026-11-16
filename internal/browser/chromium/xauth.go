package chromium

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"io"
	"os"
)

// Xvfb creates its socket in /tmp/.X11-unix, where every local user can reach
// it. Without access control any of them could watch or drive the agent's
// browser, so the display is started with an MIT-MAGIC-COOKIE-1 authority file
// that only the sidecar (and the Chromium it starts) can read.
//
// The Xauthority format is small enough to write ourselves rather than shell
// out to xauth: a sequence of entries, each big-endian
//
//	uint16 family, uint16 len + address, uint16 len + display number,
//	uint16 len + auth name, uint16 len + auth data
const (
	// familyLocal is FamilyLocal from X11/Xauth.h: the address is a host name
	// and the connection is a unix socket.
	familyLocal = 256
	cookieName  = "MIT-MAGIC-COOKIE-1"
	cookieBytes = 16
)

// xauthEntry is one record of an Xauthority file.
type xauthEntry struct {
	family  uint16
	address string // host name for familyLocal
	number  string // display number as decimal text, "7" for ":7"
	name    string // "MIT-MAGIC-COOKIE-1"
	data    []byte
}

// writeXauth writes entries in the Xauthority binary format.
func writeXauth(w io.Writer, entries ...xauthEntry) error {
	var buf bytes.Buffer
	for _, e := range entries {
		if err := binary.Write(&buf, binary.BigEndian, e.family); err != nil {
			return err
		}
		for _, field := range [][]byte{[]byte(e.address), []byte(e.number), []byte(e.name), e.data} {
			if len(field) > 0xffff {
				return fmt.Errorf("xauth: field of %d bytes does not fit", len(field))
			}
			if err := binary.Write(&buf, binary.BigEndian, uint16(len(field))); err != nil {
				return err
			}
			buf.Write(field)
		}
	}
	_, err := w.Write(buf.Bytes())
	return err
}

// writeXauthFile writes a single-entry authority file at path, 0600. It is
// called twice per display: once before Xvfb starts (the server loads the
// cookie data from it and does not care which display the entry names) and
// once after -displayfd reports the number, so the client side — Chromium's
// Xlib, which does match on the number — finds it.
func writeXauthFile(path, host, number string, cookie []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	err = writeXauth(f, xauthEntry{
		family:  familyLocal,
		address: host,
		number:  number,
		name:    cookieName,
		data:    cookie,
	})
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// newCookie returns 16 random bytes.
func newCookie() ([]byte, error) {
	b := make([]byte, cookieBytes)
	if _, err := rand.Read(b); err != nil {
		return nil, fmt.Errorf("xauth: %w", err)
	}
	return b, nil
}
