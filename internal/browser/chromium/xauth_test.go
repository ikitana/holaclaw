package chromium

import (
	"bytes"
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// The layout is fixed by libXau, so the writer is checked against bytes
// written out by hand, not against itself.
func TestWriteXauthLayout(t *testing.T) {
	cookie := []byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}
	var got bytes.Buffer
	err := writeXauth(&got, xauthEntry{
		family:  familyLocal,
		address: "host",
		number:  "7",
		name:    cookieName,
		data:    cookie,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{
		0x01, 0x00, // family 256 = FamilyLocal, big endian
		0x00, 0x04, 'h', 'o', 's', 't', // address
		0x00, 0x01, '7', // display number, decimal text
		0x00, 0x12, // 18 = len("MIT-MAGIC-COOKIE-1")
		'M', 'I', 'T', '-', 'M', 'A', 'G', 'I', 'C', '-', 'C', 'O', 'O', 'K', 'I', 'E', '-', '1',
		0x00, 0x10, // 16 cookie bytes
		0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15,
	}
	if !bytes.Equal(got.Bytes(), want) {
		t.Fatalf("layout mismatch\n got %x\nwant %x", got.Bytes(), want)
	}
}

// readXauth is an independent reader, only for the round-trip test.
func readXauth(t *testing.T, r io.Reader) []xauthEntry {
	t.Helper()
	var out []xauthEntry
	for {
		var family uint16
		if err := binary.Read(r, binary.BigEndian, &family); err != nil {
			return out
		}
		fields := make([][]byte, 4)
		for i := range fields {
			var n uint16
			if err := binary.Read(r, binary.BigEndian, &n); err != nil {
				t.Fatalf("truncated entry: %v", err)
			}
			b := make([]byte, n)
			if _, err := io.ReadFull(r, b); err != nil {
				t.Fatalf("truncated field: %v", err)
			}
			fields[i] = b
		}
		out = append(out, xauthEntry{
			family: family, address: string(fields[0]), number: string(fields[1]),
			name: string(fields[2]), data: fields[3],
		})
	}
}

func TestWriteXauthRoundTrip(t *testing.T) {
	in := []xauthEntry{
		{familyLocal, "alpha", "0", cookieName, bytes.Repeat([]byte{0xab}, 16)},
		{familyLocal, "a-host-with-dashes", "1234", cookieName, bytes.Repeat([]byte{0x00}, 16)},
	}
	var buf bytes.Buffer
	if err := writeXauth(&buf, in...); err != nil {
		t.Fatal(err)
	}
	got := readXauth(t, &buf)
	if len(got) != len(in) {
		t.Fatalf("got %d entries, want %d", len(got), len(in))
	}
	for i := range in {
		if got[i].family != in[i].family || got[i].address != in[i].address ||
			got[i].number != in[i].number || got[i].name != in[i].name ||
			!bytes.Equal(got[i].data, in[i].data) {
			t.Errorf("entry %d: got %+v, want %+v", i, got[i], in[i])
		}
	}
}

func TestWriteXauthFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "Xauthority")
	cookie, err := newCookie()
	if err != nil {
		t.Fatal(err)
	}
	if len(cookie) != cookieBytes {
		t.Fatalf("cookie is %d bytes, want %d", len(cookie), cookieBytes)
	}
	if err := writeXauthFile(path, "h", "3", cookie); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// The cookie is the only thing keeping other local users off the display.
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v, want 0600", fi.Mode().Perm())
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	got := readXauth(t, f)
	if len(got) != 1 || got[0].number != "3" || !bytes.Equal(got[0].data, cookie) {
		t.Fatalf("got %+v", got)
	}

	// Rewriting truncates: one entry, the new number, never two.
	if err := writeXauthFile(path, "h", "11", cookie); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := readXauth(t, bytes.NewReader(b)); len(got) != 1 || got[0].number != "11" {
		t.Fatalf("got %+v", got)
	}
}

func TestCookiesDiffer(t *testing.T) {
	a, err := newCookie()
	if err != nil {
		t.Fatal(err)
	}
	b, err := newCookie()
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(a, b) {
		t.Fatal("two cookies came out identical")
	}
}
