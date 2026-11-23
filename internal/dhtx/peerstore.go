package dhtx

import (
	"sync"
	"time"

	"github.com/anacrolix/dht/v2/krpc"
	peer_store "github.com/anacrolix/dht/v2/peer-store"
)

// peerStore is a correct in-memory peer_store.Interface.
//
// Why not anacrolix's bundled peer_store.InMemory? In v2.24.0 it
// is broken for this use: AddPeer keys entries by IP string only
// (two peers behind one IP clobber each other), and GetPeers
// unmarshals that IP-string key as if it were a compact node
// address, producing garbage addresses that the server's peer
// filter then discards — so get_peers NEVER returns values. This
// implementation keys by the full marshaled node address and
// expires stale peers.
type peerStore struct {
	mu    sync.Mutex
	peers map[peer_store.InfoHash]map[string]peerEntry
}

type peerEntry struct {
	addr krpc.NodeAddr
	seen time.Time
}

const peerTTL = 45 * time.Minute

func newPeerStore() *peerStore {
	return &peerStore{peers: make(map[peer_store.InfoHash]map[string]peerEntry)}
}

func (s *peerStore) AddPeer(ih peer_store.InfoHash, na krpc.NodeAddr) {
	key, err := na.MarshalBinary()
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	bucket, ok := s.peers[ih]
	if !ok {
		bucket = make(map[string]peerEntry)
		s.peers[ih] = bucket
	}
	bucket[string(key)] = peerEntry{addr: na, seen: time.Now()}
}

func (s *peerStore) GetPeers(ih peer_store.InfoHash) []krpc.NodeAddr {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []krpc.NodeAddr
	for k, e := range s.peers[ih] {
		if time.Since(e.seen) > peerTTL {
			delete(s.peers[ih], k)
			continue
		}
		out = append(out, e.addr)
	}
	return out
}
