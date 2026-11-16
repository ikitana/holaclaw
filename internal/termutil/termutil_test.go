package termutil

import (
	"fmt"
	"os"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// openPTY opens a pty pair, or skips the test where there is none.
func openPTY(t *testing.T) (master, slave *os.File) {
	t.Helper()
	m, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		t.Skipf("no pty available: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })
	n, err := unix.IoctlGetInt(int(m.Fd()), unix.TIOCGPTN)
	if err != nil {
		t.Skipf("TIOCGPTN: %v", err)
	}
	if err := unix.IoctlSetPointerInt(int(m.Fd()), unix.TIOCSPTLCK, 0); err != nil {
		t.Skipf("TIOCSPTLCK: %v", err)
	}
	s, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("open the pty slave: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return m, s
}

func TestIsTerminal(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "f")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if IsTerminal(int(f.Fd())) {
		t.Error("a regular file is a terminal")
	}
	_, s := openPTY(t)
	if !IsTerminal(int(s.Fd())) {
		t.Error("a pty is not a terminal")
	}
}

func TestEchoOff(t *testing.T) {
	_, s := openPTY(t)
	fd := int(s.Fd())
	echo := func() bool {
		tio, err := unix.IoctlGetTermios(fd, unix.TCGETS)
		if err != nil {
			t.Fatal(err)
		}
		return tio.Lflag&unix.ECHO != 0
	}
	if !echo() {
		t.Fatal("a new pty has no echo")
	}
	restore, err := EchoOff(fd)
	if err != nil {
		t.Fatal(err)
	}
	if echo() {
		t.Error("echo still on")
	}
	restore()
	if !echo() {
		t.Error("echo not restored")
	}
}

func TestEchoOffNotATerminal(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "f")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if restore, err := EchoOff(int(f.Fd())); err == nil || restore != nil {
		t.Fatalf("EchoOff on a file: %v, want an error and no restore", err)
	}
}
