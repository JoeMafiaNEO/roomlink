//go:build linux

package adapter

import (
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

// tunDevice is a real Linux TUN adapter created on /dev/net/tun.
// Creating it needs CAP_NET_ADMIN; where the kernel refuses (as in
// this project's build VM), OpenTUN returns ErrNoTUN and callers
// fall back to the loopback adapter for testing.
type tunDevice struct {
	file *os.File
	name string
}

const (
	iffTun   = 0x0001
	iffNoPi  = 0x1000
	tunSetIf = 0x400454ca
)

type ifreq struct {
	name  [16]byte
	flags uint16
	_     [22]byte
}

// OpenTUN creates a TUN device with the requested name, assigns
// the room IP and brings the link up.
func OpenTUN(name string, ip [4]byte) (Adapter, error) {
	f, err := os.OpenFile("/dev/net/tun", os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("%w: open /dev/net/tun: %v", ErrNoTUN, err)
	}
	var req ifreq
	copy(req.name[:], name)
	req.flags = iffTun | iffNoPi
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, f.Fd(), uintptr(tunSetIf), uintptr(unsafe.Pointer(&req))); errno != 0 {
		f.Close()
		return nil, fmt.Errorf("%w: TUNSETIFF: %v", ErrNoTUN, errno)
	}
	actual := string(req.name[:])
	for i, c := range req.name {
		if c == 0 {
			actual = string(req.name[:i])
			break
		}
	}
	d := &tunDevice{file: f, name: actual}
	if err := d.configure(ip); err != nil {
		f.Close()
		return nil, err
	}
	return d, nil
}

// configure sets the address and MTU and raises the link, using
// the netlink-free ioctl path (SIOCSIFADDR et al.) via a datagram
// socket.
func (d *tunDevice) configure(ip [4]byte) error {
	sock, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM, 0)
	if err != nil {
		return fmt.Errorf("control socket: %w", err)
	}
	defer unix.Close(sock)

	type ifreqAddr struct {
		name [16]byte
		addr unix.RawSockaddrInet4
		_    [8]byte
	}
	var req ifreqAddr
	copy(req.name[:], d.name)
	req.addr.Family = unix.AF_INET
	req.addr.Addr = ip
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(sock), uintptr(unix.SIOCSIFADDR), uintptr(unsafe.Pointer(&req))); errno != 0 {
		return fmt.Errorf("%w: SIOCSIFADDR: %v", ErrNoTUN, errno)
	}

	type ifreqFlags struct {
		name  [16]byte
		flags uint16
		_     [22]byte
	}
	var freq ifreqFlags
	copy(freq.name[:], d.name)
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(sock), uintptr(unix.SIOCGIFFLAGS), uintptr(unsafe.Pointer(&freq))); errno != 0 {
		return fmt.Errorf("%w: SIOCGIFFLAGS: %v", ErrNoTUN, errno)
	}
	freq.flags |= unix.IFF_UP | unix.IFF_RUNNING
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(sock), uintptr(unix.SIOCSIFFLAGS), uintptr(unsafe.Pointer(&freq))); errno != 0 {
		return fmt.Errorf("%w: SIOCSIFFLAGS: %v", ErrNoTUN, errno)
	}
	return nil
}

func (d *tunDevice) ReadPacket() ([]byte, error) {
	buf := make([]byte, 65535)
	n, err := d.file.Read(buf)
	if err != nil {
		return nil, err
	}
	return buf[:n], nil
}

func (d *tunDevice) WritePacket(pkt []byte) error {
	_, err := d.file.Write(pkt)
	return err
}

func (d *tunDevice) Name() string { return d.name }

func (d *tunDevice) Close() error { return d.file.Close() }
