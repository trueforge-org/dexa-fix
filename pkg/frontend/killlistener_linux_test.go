//go:build linux

package frontend

import (
	"strconv"
	"syscall"
	"testing"
)

// killListener shuts down the listening socket on port in this process, so that the server that
// accepts on it fails, as it would when the operating system takes its socket away.
func killListener(t *testing.T, port string) {
	t.Helper()
	want, err := strconv.Atoi(port)
	if err != nil {
		t.Fatal(err)
	}
	for fd := 3; fd < 4096; fd++ {
		sa, err := syscall.Getsockname(fd)
		if err != nil {
			continue
		}
		got := -1
		switch a := sa.(type) {
		case *syscall.SockaddrInet4:
			got = a.Port
		case *syscall.SockaddrInet6:
			got = a.Port
		}
		if got != want {
			continue
		}
		if listening, err := syscall.GetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_ACCEPTCONN); err != nil || listening != 1 {
			continue
		}
		if err := syscall.Shutdown(fd, syscall.SHUT_RDWR); err != nil {
			t.Fatal(err)
		}
		return
	}
	t.Fatalf("no listening socket on port %s", port)
}
