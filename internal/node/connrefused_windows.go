//go:build windows

package node

import (
	"errors"

	"golang.org/x/sys/windows"
)

// isConnRefused: on Windows the same ICMP report surfaces as
// WSAECONNRESET on connected UDP sockets.
func isConnRefused(err error) bool {
	return errors.Is(err, windows.WSAECONNRESET) || errors.Is(err, windows.WSAECONNREFUSED)
}
