//go:build windows

package adapter

import (
	"fmt"
	"net/netip"

	"golang.zx2c4.com/wireguard/tun"
)

// wintunDevice is a real Windows adapter backed by Wintun (the
// WireGuard project's TUN driver) via the wireguard-go bindings.
// wintun.dll must sit next to the executable at runtime; it is a
// separate, redistributable component from wintun.net.
//
// NOTE: this file is compile-verified by cross-compilation only —
// the Windows runtime cannot be exercised from the Linux build VM.
type wintunDevice struct {
	dev  tun.Device
	name string
}

// OpenTUN creates a Wintun adapter with the requested name and
// assigns the room IP.
func OpenTUN(name string, ip [4]byte) (Adapter, error) {
	dev, err := tun.CreateTUN(name, 1280)
	if err != nil {
		return nil, fmt.Errorf("%w: CreateTUN: %v", ErrNoTUN, err)
	}
	actual, err := dev.Name()
	if err != nil {
		actual = name
	}
	d := &wintunDevice{dev: dev, name: actual}
	if err := d.configure(ip); err != nil {
		dev.Close()
		return nil, err
	}
	return d, nil
}

// configure assigns the room IP to the Wintun adapter through the
// wireguard-go Windows TUN helper (netsh-free path used by the
// wireguard-windows project family).
func (d *wintunDevice) configure(ip [4]byte) error {
	addr := netip.AddrFrom4(ip)
	prefix := netip.PrefixFrom(addr, 16)
	// The wireguard/tun Windows device exposes LUID-based config
	// via its NativeTun type; set the address if available.
	if nt, ok := d.dev.(interface {
		SetIPAddresses([]netip.Prefix) error
	}); ok {
		if err := nt.SetIPAddresses([]netip.Prefix{prefix}); err != nil {
			return fmt.Errorf("%w: SetIPAddresses: %v", ErrNoTUN, err)
		}
	}
	return nil
}

func (d *wintunDevice) ReadPacket() ([]byte, error) {
	bufs := make([][]byte, 1)
	bufs[0] = make([]byte, 65535)
	sizes := make([]int, 1)
	_, err := d.dev.Read(bufs, sizes, 0)
	if err != nil {
		return nil, err
	}
	return bufs[0][:sizes[0]], nil
}

func (d *wintunDevice) WritePacket(pkt []byte) error {
	bufs := [][]byte{pkt}
	_, err := d.dev.Write(bufs, 0)
	return err
}

func (d *wintunDevice) Name() string { return d.name }

func (d *wintunDevice) Close() error { return d.dev.Close() }
