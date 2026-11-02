//go:build !linux

package ilo

import "syscall"

// dialControl is only implemented on Linux, see dial_linux.go.
var dialControl func(network, address string, c syscall.RawConn) error
