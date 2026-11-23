//go:build !linux && !windows

package adapter

import "fmt"

// OpenTUN is not implemented on this platform yet; the loopback
// adapter remains available for testing.
func OpenTUN(name string, ip [4]byte) (Adapter, error) {
	return nil, fmt.Errorf("%w: no TUN implementation for this platform", ErrNoTUN)
}
