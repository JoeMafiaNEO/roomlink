//go:build windows

package dhtx

import (
	"syscall"

	"golang.org/x/sys/windows"
)

// reuseControl on Windows: SO_REUSEADDR for shared-port sockets.
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
