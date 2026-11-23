//go:build !windows

package dhtx

import (
	"syscall"

	"golang.org/x/sys/unix"
)

// reuseControl lets the DHT listener and its per-destination
// connected sockets share one local port.
func reuseControl(network, address string, c syscall.RawConn) error {
	var err error
	cerr := c.Control(func(fd uintptr) {
		err = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEADDR, 1)
		if err == nil {
			err = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEPORT, 1)
		}
	})
	if cerr != nil {
		return cerr
	}
	return err
}
