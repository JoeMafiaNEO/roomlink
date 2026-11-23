//go:build windows

package node

import (
	"syscall"

	"golang.org/x/sys/windows"
)

// reuseControl on Windows: SO_REUSEADDR lets the listener and the
// per-peer connected sockets share the local port.
func reuseControl(network, address string, c syscall.RawConn) error {
	var err error
	cerr := c.Control(func(fd uintptr) {
		err = windows.SetsockoptInt(windows.Handle(fd), windows.SOL_SOCKET, windows.SO_REUSEADDR, 1)
	})
	if cerr != nil {
		return cerr
	}
	return err
}
