package ilo

import (
	"errors"
	"syscall"

	"golang.org/x/sys/unix"
)

// dialControl makes the kernel drop the KVM connection when sent data
// stays unacknowledged for kvmUserTimeout, e.g. when the iLO hangs or
// disappears. Otherwise TCP retransmits for about 15 minutes, keepalives
// do not cover unacknowledged data, and a read timeout does not work
// because the iLO may legitimately send nothing for minutes.
func dialControl(_, _ string, c syscall.RawConn) error {
	var serr error
	err := c.Control(func(fd uintptr) {
		serr = unix.SetsockoptInt(int(fd), unix.IPPROTO_TCP, unix.TCP_USER_TIMEOUT, int(kvmUserTimeout.Milliseconds()))
	})
	return errors.Join(err, serr)
}
