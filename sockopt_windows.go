//go:build windows

package main

import "syscall"

// setSockopts mirrors the Python server's SO_REUSEADDR + SO_BROADCAST setup.
func setSockopts(network, address string, c syscall.RawConn) error {
	var sockErr error
	err := c.Control(func(fd uintptr) {
		h := syscall.Handle(fd)
		_ = syscall.SetsockoptInt(h, syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1)
		sockErr = syscall.SetsockoptInt(h, syscall.SOL_SOCKET, syscall.SO_BROADCAST, 1)
	})
	if err != nil {
		return err
	}
	return sockErr
}
