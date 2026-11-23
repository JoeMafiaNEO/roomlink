//go:build !windows

package node

import (
	"errors"
	"syscall"
)

// isConnRefused reports whether err is the one-shot ICMP
// port-unreachable report a connected UDP socket gets when a
// packet raced the peer's startup.
func isConnRefused(err error) bool {
	return errors.Is(err, syscall.ECONNREFUSED)
}
