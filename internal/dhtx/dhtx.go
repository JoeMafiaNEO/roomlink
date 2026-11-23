// Package dhtx wraps the Mainline DHT (anacrolix/dht) for room
// lookup. The DHT is the design doc's serverless phone book: a
// host announces "a room lives at the hash of this code, knock on
// this port", and a joiner asks the same hash and gets back the
// endpoints of everyone who announced.
//
// The DHT key is the hash of the room code (room.DHTKey) — the
// code itself never enters the network. Nothing about passwords
// or membership is stored here; the DHT only hands out knocking
// addresses, and the password handshake at the door does the rest.
package dhtx

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/anacrolix/dht/v2"
	alog "github.com/anacrolix/log"
)

// PublicBootstrap is the standard Mainline DHT bootstrap list
// shipped for real-world use (design doc §8: a public commons,
// independently operated, no single point of dependence).
var PublicBootstrap = []string{
	"router.bittorrent.com:6881",
	"router.utorrent.com:6881",
	"dht.transmissionbt.com:6881",
	"dht.libtorrent.org:25401",
}

// Service is a running DHT node.
type Service struct {
	server *dht.Server
	conn   net.PacketConn

	mu     sync.Mutex
	seen   map[string]bool
	closed bool
}

// StartLocal launches a DHT server with no starting nodes — it
// IS a bootstrap node. Used for the local verification rig (and
// usable by anyone who wants to seed a private swarm).
func StartLocal(port int) (*Service, error) {
	return start(port, nil, true)
}

// Start launches a DHT server on the given UDP port (0 = any free
// port). bootstrap is a list of host:port addresses used as
// starting nodes; empty means the public bootstrap list. A local
// bootstrap node for testing is just a Service whose address
// everyone else lists as their only starting node.
func Start(port int, bootstrap []string) (*Service, error) {
	return start(port, bootstrap, false)
}

func start(port int, bootstrap []string, localOnly bool) (*Service, error) {
	conn, err := newRoutedConn(port)
	if err != nil {
		return nil, fmt.Errorf("dht listen: %w", err)
	}
	if len(bootstrap) == 0 && !localOnly {
		bootstrap = PublicBootstrap
	}
	starting := func() ([]dht.Addr, error) {
		var out []dht.Addr
		var firstErr error
		for _, b := range bootstrap {
			ua, err := net.ResolveUDPAddr("udp", b)
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			out = append(out, dht.NewAddr(ua))
		}
		if len(out) == 0 && firstErr != nil {
			return nil, firstErr
		}
		return out, nil
	}
	cfg := &dht.ServerConfig{
		Conn:          conn,
		NoSecurity:    true, // prototype: node ids are not IP-bound
		StartingNodes: starting,
		// NOT optional despite appearances: with a nil PeerStore,
		// anacrolix answers get_peers with no token and no values,
		// so no announce_peer can ever happen and no peer is ever
		// returned. (Found the hard way in the local swarm probe.)
		// We supply our own store — see peerstore.go for why the
		// bundled InMemory one cannot be used.
		PeerStore: newPeerStore(),
	}
	if os.Getenv("SVLAN_DHT_DEBUG") != "" {
		cfg.Logger = alog.Default.FilterLevel(alog.Debug)
	}
	srv, err := dht.NewServer(cfg)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("dht server: %w", err)
	}
	return &Service{server: srv, conn: conn, seen: make(map[string]bool)}, nil
}

// Addr returns the DHT server's own UDP address.
func (s *Service) Addr() string { return s.conn.LocalAddr().String() }

// PeerStoreDump is a diagnostic: what this node has stored for
// each infohash (used by the local-swarm probe test).
func (s *Service) PeerStoreDump() string {
	if ps, ok := s.server.PeerStore().(*peerStore); ok {
		ps.mu.Lock()
		defer ps.mu.Unlock()
		return fmt.Sprintf("%v", ps.peers)
	}
	return "unexpected peer store type"
}

// AnnounceLoop announces this room under key and reports every
// discovered peer endpoint to onPeer, re-announcing on an interval
// (DHT peer records expire; a live room must keep its listing
// fresh). It runs until Close. announcePort is our TUNNEL port —
// the port peers should knock on — not the DHT port.
func (s *Service) AnnounceLoop(key [20]byte, announcePort int, interval time.Duration, logf func(string, ...any), onPeer func(addr string)) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	for {
		s.mu.Lock()
		closed := s.closed
		s.mu.Unlock()
		if closed {
			return
		}
		ann, err := s.server.Announce(key, announcePort, false)
		if err != nil {
			logf("dht announce failed: %v", err)
		} else {
			for pv := range ann.Peers {
				for _, pr := range pv.Peers {
					addr := net.JoinHostPort(pr.IP.String(), strconv.Itoa(pr.Port))
					s.mu.Lock()
					fresh := !s.seen[addr]
					s.seen[addr] = true
					s.mu.Unlock()
					if fresh {
						onPeer(addr)
					}
				}
			}
			logf("dht announce cycle done: contacted=%d", ann.NumContacted())
			ann.Close()
		}
		select {
		case <-time.After(interval):
		}
		s.mu.Lock()
		closed = s.closed
		s.mu.Unlock()
		if closed {
			return
		}
	}
}

// Close stops the DHT server.
func (s *Service) Close() {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	s.server.Close()
	s.conn.Close()
}
