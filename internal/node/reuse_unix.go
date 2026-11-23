//go:build !windows

package node

import (
	"syscall"

	"golang.org/x/sys/unix"
)

// reuseControl lets the listening socket and the per-peer
// connected sockets share one local port: the kernel delivers a
// peer's packets to the connected socket matching the 4-tuple
// first, and everything else to the listener.
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
