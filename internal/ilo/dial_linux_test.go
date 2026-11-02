package ilo

import (
	"net"
	"testing"

	"golang.org/x/sys/unix"
)

func TestDialControlSetsUserTimeout(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	d := net.Dialer{Control: dialControl}
	c, err := d.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	raw, err := c.(*net.TCPConn).SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var got int
	var gerr error
	if err := raw.Control(func(fd uintptr) {
		got, gerr = unix.GetsockoptInt(int(fd), unix.IPPROTO_TCP, unix.TCP_USER_TIMEOUT)
	}); err != nil || gerr != nil {
		t.Fatal(err, gerr)
	}
	if want := int(kvmUserTimeout.Milliseconds()); got != want {
		t.Fatalf("TCP_USER_TIMEOUT %d ms, want %d", got, want)
	}
}
