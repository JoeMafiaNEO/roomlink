// Package adapter abstracts the virtual network adapter the room
// lives on. The node pumps packets between an Adapter and the
// encrypted tunnel:
//
//	ReadPacket  — a packet the local OS (or test harness) wants
//	              sent into the room.
//	WritePacket — a packet received from the room, delivered to
//	              the local OS (or test harness).
//
// Two implementations exist: a real TUN device (Linux /dev/net/tun,
// Windows Wintun) and a userspace loopback used for automated
// verification where no TUN device can be created.
package adapter

import "errors"

// Adapter is a packet source/sink carrying raw IPv4 packets.
type Adapter interface {
	ReadPacket() ([]byte, error)
	WritePacket(pkt []byte) error
	Name() string
	Close() error
}

// ErrNoTUN is returned when a real TUN device cannot be created
// (no device, no permission). Callers may fall back to loopback
// for testing, and must say so — a loopback adapter carries no
// real OS traffic.
var ErrNoTUN = errors.New("cannot create TUN device")

// Loopback is a userspace adapter: packets are passed through
// channels. It lets the full protocol stack run — and be verified
// — on machines where a real TUN device is unavailable.
type Loopback struct {
	name   string
	fromOS chan []byte
	toOS   chan []byte
	closed chan struct{}
}

// NewLoopback creates a loopback adapter with the given label.
func NewLoopback(name string) *Loopback {
	return &Loopback{
		name:   name,
		fromOS: make(chan []byte, 256),
		toOS:   make(chan []byte, 256),
		closed: make(chan struct{}),
	}
}

// SendFromOS injects a packet as if the local OS had sent it into
// the room (test harness / echo responder use).
func (l *Loopback) SendFromOS(pkt []byte) {
	cp := make([]byte, len(pkt))
	copy(cp, pkt)
	select {
	case l.fromOS <- cp:
	case <-l.closed:
	}
}

// RecvToOS returns the channel of packets delivered from the room
// to the local OS side (test harness use).
func (l *Loopback) RecvToOS() <-chan []byte { return l.toOS }

func (l *Loopback) ReadPacket() ([]byte, error) {
	select {
	case pkt := <-l.fromOS:
		return pkt, nil
	case <-l.closed:
		return nil, errors.New("adapter closed")
	}
}

func (l *Loopback) WritePacket(pkt []byte) error {
	cp := make([]byte, len(pkt))
	copy(cp, pkt)
	select {
	case l.toOS <- cp:
		return nil
	case <-l.closed:
		return errors.New("adapter closed")
	}
}

func (l *Loopback) Name() string { return l.name }

func (l *Loopback) Close() error {
	select {
	case <-l.closed:
	default:
		close(l.closed)
	}
	return nil
}
