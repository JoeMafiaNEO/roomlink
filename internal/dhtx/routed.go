package dhtx

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"time"
)

// routedConn is a net.PacketConn for the DHT server that sends
// through per-destination CONNECTED sockets instead of WriteTo.
//
// Reason: some sandboxes / strict egress firewalls reject
// unconnected sendto() with EPERM while permitting connected
// sockets (measured in this project's build VM). anacrolix/dht
// only needs the PacketConn interface, so we present one whose
// WriteTo dials (once per destination, SO_REUSEADDR on our port)
// and whose ReadFrom is fed by pumps on the listener and on every
// connected socket.
type routedConn struct {
	base net.PacketConn
	port int

	mu       sync.Mutex
	senders  map[string]*net.UDPConn
	deadline time.Time

	queue  chan recvItem
	closed chan struct{}
	once   sync.Once
}

type recvItem struct {
	data []byte
	addr net.Addr
	err  error
}

func newRoutedConn(port int) (*routedConn, error) {
	lc := net.ListenConfig{Control: reuseControl}
	base, err := lc.ListenPacket(context.Background(), "udp4", fmt.Sprintf("0.0.0.0:%d", port))
	if err != nil {
		return nil, err
	}
	c := &routedConn{
		base:    base,
		port:    base.LocalAddr().(*net.UDPAddr).Port,
		senders: make(map[string]*net.UDPConn),
		queue:   make(chan recvItem, 256),
		closed:  make(chan struct{}),
	}
	go c.basePump()
	return c, nil
}

func (c *routedConn) basePump() {
	buf := make([]byte, 65535)
	for {
		n, addr, err := c.base.ReadFrom(buf)
		if err != nil {
			select {
			case <-c.closed:
				return
			default:
				continue
			}
		}
		data := make([]byte, n)
		copy(data, buf[:n])
		select {
		case c.queue <- recvItem{data: data, addr: addr}:
		case <-c.closed:
			return
		}
	}
}

func (c *routedConn) senderPump(conn *net.UDPConn, remote net.Addr) {
	buf := make([]byte, 65535)
	for {
		n, err := conn.Read(buf)
		if err != nil {
			// One-shot ICMP port-unreachable (peer still
			// starting) must not kill the pump: the error clears
			// after delivery and the socket stays usable.
			if isConnRefused(err) {
				continue
			}
			return
		}
		data := make([]byte, n)
		copy(data, buf[:n])
		select {
		case c.queue <- recvItem{data: data, addr: remote}:
		case <-c.closed:
			return
		}
	}
}

func (c *routedConn) ReadFrom(b []byte) (int, net.Addr, error) {
	c.mu.Lock()
	dl := c.deadline
	c.mu.Unlock()
	var timer *time.Timer
	var timeout <-chan time.Time
	if !dl.IsZero() {
		timer = time.NewTimer(time.Until(dl))
		timeout = timer.C
		defer timer.Stop()
	}
	select {
	case item := <-c.queue:
		if item.err != nil {
			return 0, nil, item.err
		}
		return copy(b, item.data), item.addr, nil
	case <-c.closed:
		return 0, nil, errors.New("routedConn closed")
	case <-timeout:
		return 0, nil, os.ErrDeadlineExceeded
	}
}

func (c *routedConn) WriteTo(b []byte, addr net.Addr) (int, error) {
	ua, ok := addr.(*net.UDPAddr)
	if !ok {
		var err error
		ua, err = net.ResolveUDPAddr("udp4", addr.String())
		if err != nil {
			return 0, err
		}
	}
	key := ua.String()
	c.mu.Lock()
	sender, ok := c.senders[key]
	c.mu.Unlock()
	if !ok {
		dialer := net.Dialer{
			Control:   reuseControl,
			LocalAddr: &net.UDPAddr{IP: net.IPv4zero, Port: c.port},
		}
		dc, err := dialer.DialContext(context.Background(), "udp4", key)
		if err != nil {
			return 0, err
		}
		sender = dc.(*net.UDPConn)
		c.mu.Lock()
		if existing, raced := c.senders[key]; raced {
			sender.Close()
			sender = existing
		} else {
			c.senders[key] = sender
			go c.senderPump(sender, ua)
		}
		c.mu.Unlock()
	}
	return sender.Write(b)
}

func (c *routedConn) Close() error {
	c.once.Do(func() {
		close(c.closed)
		c.base.Close()
		c.mu.Lock()
		for _, s := range c.senders {
			s.Close()
		}
		c.mu.Unlock()
	})
	return nil
}

func (c *routedConn) LocalAddr() net.Addr { return c.base.LocalAddr() }

func (c *routedConn) SetDeadline(t time.Time) error {
	c.SetReadDeadline(t)
	return nil
}

func (c *routedConn) SetReadDeadline(t time.Time) error {
	c.mu.Lock()
	c.deadline = t
	c.mu.Unlock()
	return nil
}

func (c *routedConn) SetWriteDeadline(t time.Time) error { return nil }
