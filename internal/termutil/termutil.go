// Package termutil holds the terminal helpers the CLI's plain prompts share:
// is a file descriptor a terminal, and reading from one without echo.
package termutil

import "golang.org/x/sys/unix"

// IsTerminal reports whether fd is a terminal: only a terminal answers
// TCGETS.
func IsTerminal(fd int) bool {
	_, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	return err == nil
}

// EchoOff turns off the echo of the terminal fd, and returns what turns it
// back on.
func EchoOff(fd int) (func(), error) {
	t, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		return nil, err
	}
	saved := *t
	t.Lflag &^= unix.ECHO
	if err := unix.IoctlSetTermios(fd, unix.TCSETS, t); err != nil {
		return nil, err
	}
	return func() { _ = unix.IoctlSetTermios(fd, unix.TCSETS, &saved) }, nil
}
