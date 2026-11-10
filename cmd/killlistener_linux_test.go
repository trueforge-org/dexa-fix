//go:build linux

package cmd

import (
	"net"
	"strconv"
	"syscall"
	"testing"
)

// killListener shuts down the listening socket of addr in this process, so that the server that
// accepts on it fails, as it would when the operating system takes its socket away.
func killListener(t *testing.T, addr string) {
	t.Helper()
	_, portText, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	port, _ := strconv.Atoi(portText)
	for fd := 3; fd < 4096; fd++ {
		sa, err := syscall.Getsockname(fd)
		if err != nil {
			continue
		}
		in4, ok := sa.(*syscall.SockaddrInet4)
		if !ok || in4.Port != port {
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
	t.Fatalf("no listening socket on %s", addr)
}
