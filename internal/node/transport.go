package node

import (
	"context"
	"fmt"
	"net"
	"sync"
)

// transport owns the node's UDP presence: one listening socket on
// the published port (the "knocking port" the DHT advertises),
// plus one CONNECTED socket per peer endpoint, bound to the same
// local port with SO_REUSEADDR.
//
// Why connected sockets instead of plain WriteToUDP on the
// listener: some sandboxes and strict egress firewalls reject
// unconnected sendto() while permitting connected-socket sends
// (measured in this project's build VM: WriteToUDP -> EPERM,
// DialUDP -> works). Connected sockets are also exactly the
// NAT-traversal shape: our datagrams to a peer always leave from
// our published port, and the kernel delivers that peer's replies
// to the matching connected socket first.
type transport struct {
	port     int
	listen   *net.UDPConn
	onPacket func(pkt []byte, src *net.UDPAddr)
	logf     func(string, ...any)

	mu     sync.Mutex
	conns  map[string]*net.UDPConn // by remote address
	closed chan struct{}
	once   sync.Once
}

func newTransport(port int, onPacket func([]byte, *net.UDPAddr), logf func(string, ...any)) (*transport, error) {
	lc := net.ListenConfig{Control: reuseControl}
	pc, err := lc.ListenPacket(context.Background(), "udp4", fmt.Sprintf("0.0.0.0:%d", port))
	if err != nil {
		return nil, fmt.Errorf("listen: %w", err)
	}
	t := &transport{
		port:     pc.LocalAddr().(*net.UDPAddr).Port,
		listen:   pc.(*net.UDPConn),
		onPacket: onPacket,
		logf:     logf,
		conns:    make(map[string]*net.UDPConn),
		closed:   make(chan struct{}),
	}
	go t.listenPump()
	return t, nil
}

func (t *transport) listenPump() {
	buf := make([]byte, 65535)
	for {
		n, src, err := t.listen.ReadFromUDP(buf)
		if err != nil {
			select {
			case <-t.closed:
				return
			default:
				continue
			}
		}
		pkt := make([]byte, n)
		copy(pkt, buf[:n])
		t.onPacket(pkt, src)
	}
}

func (t *transport) connPump(c *net.UDPConn, remote *net.UDPAddr) {
	buf := make([]byte, 65535)
	for {
		n, err := c.Read(buf)
		if err != nil {
			select {
			case <-t.closed:
				return
			default:
			}
			// A connected UDP socket reports ICMP port-unreachable
			// (ECONNREFUSED) once when an early packet raced the
			// peer's startup. The error clears after being
			// delivered; the socket is fine. Dying here was a real
			// bug: writes kept working (handshakes sent) while the
			// receive path silently starved.
			if isConnRefused(err) {
				continue
			}
			t.logf("conn pump to %s exiting: %v", remote, err)
			return
		}
		pkt := make([]byte, n)
		copy(pkt, buf[:n])
		t.onPacket(pkt, remote)
	}
}

// send transmits one datagram to addr over the connected socket
// for that endpoint, creating it on first use.
func (t *transport) send(b []byte, addr *net.UDPAddr) {
	if addr == nil {
		return
	}
	key := addr.String()
	t.mu.Lock()
	c, ok := t.conns[key]
	t.mu.Unlock()
	if !ok {
		dialer := net.Dialer{
			Control:   reuseControl,
			LocalAddr: &net.UDPAddr{IP: net.IPv4zero, Port: t.port},
		}
		dc, err := dialer.DialContext(context.Background(), "udp4", addr.String())
		if err != nil {
			t.logf("dial %s failed: %v", addr, err)
			return
		}
		c = dc.(*net.UDPConn)
		t.mu.Lock()
		if existing, raced := t.conns[key]; raced {
			c.Close()
			c = existing
		} else {
			t.conns[key] = c
			go t.connPump(c, addr)
		}
		t.mu.Unlock()
	}
	if _, err := c.Write(b); err != nil {
		t.logf("write to %s failed: %v", addr, err)
	}
}

func (t *transport) close() {
	t.once.Do(func() {
		close(t.closed)
		t.listen.Close()
		t.mu.Lock()
		for _, c := range t.conns {
			c.Close()
		}
		t.mu.Unlock()
	})
}
