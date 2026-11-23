// Package node is the room engine. One Node is one room member: it
// owns the UDP socket, runs the password handshakes, keeps the
// mesh of encrypted sessions to every other member, gossips the
// member list, routes IP packets between the virtual adapter and
// the right session, relays sealed traffic for unreachable pairs,
// and keeps NAT paths warm.
//
// Design-doc mapping:
//   - §3 joining: handshake, then member-list exchange (gossip).
//   - §4 mesh fabric: every member holds a session to every other.
//   - §5 peer relay: InnerRelay forwards a sealed session packet
//     through a mutual member; the relay cannot read it.
//   - §6 password gate: handshake tags prove the room key; failed
//     attempts are dropped silently and throttled with backoff.
package node

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"sort"
	"strings"
	"sync"
	"time"

	"svlan/internal/adapter"
	"svlan/internal/room"
	"svlan/internal/session"
)

// MemberView is what the UI and tests see about one member.
type MemberView struct {
	PeerID    string
	Name      string
	IP        string
	Self      bool
	Connected bool   // an established session exists
	Direct    bool   // session is direct (not relayed)
	Via       string // relay member's name when not direct
	RTTMillis int    // -1 when unknown
	Endpoint  string // UDP endpoint when direct
}

// Config configures a Node.
type Config struct {
	Name       string // display name inside the room
	RoomKey    []byte // from room.RoomKey(code, password)
	Adapter    adapter.Adapter
	ListenPort int // UDP port for the tunnel socket; 0 = ephemeral
	Log        func(format string, args ...any)
	OnMembers  func([]MemberView) // called after any membership change
	// BlockDirectAddrs simulates unpunchable NAT pairs in tests:
	// handshakes to or from these UDP addresses are ignored, so
	// the relay path is exercised for real.
	BlockDirectAddrs map[string]bool
}

type peer struct {
	id          []byte
	name        string
	ip          [4]byte
	sess        *session.Session
	direct      bool
	via         string // peer-id hex of relay, "" when direct
	endpoint    *net.UDPAddr
	established bool
	created     time.Time
	sentInfo    bool
	lastSeen    time.Time
	rttMillis   int
	pingNonce   uint64
	pingSent    time.Time
}

type pendingOut struct {
	key       string
	hello     session.Hello
	ephPriv   [32]byte
	localSID  uint64
	addr      *net.UDPAddr // direct target (nil when relayed)
	via       string       // relay peer-id hex ("" when direct)
	targetID  []byte       // set when relayed
	created   time.Time
	attempts  int
	nextRetry time.Time
}

type throttle struct {
	failures     int
	blockedUntil time.Time
}

// Node is one room member.
type Node struct {
	cfg     Config
	peerID  []byte
	selfIP  [4]byte
	tp      *transport
	adapter adapter.Adapter

	mu        sync.Mutex
	peers     map[string]*peer           // by peer-id hex
	sessions  map[uint64]*peer           // by local session id addressed to us
	pending   map[string]*pendingOut     // outbound handshakes
	tentative map[*session.Session]*peer // sessions awaiting peerinfo
	throttles map[string]*throttle
	known     map[string]knownMember // gossiped members we may not be connected to
	blocked   map[string]bool
	started   time.Time
	closed    chan struct{}
	closeOnce sync.Once

	// Counters, exported for tests and the UI details view.
	HandshakeFailures int
	PacketsSent       int
	PacketsReceived   int
	RelayedPackets    int
}

type knownMember struct {
	id        []byte
	name      string
	endpoints []string
	from      string // peer-id hex of the member who told us
	learned   time.Time
}

// New creates a Node. Call Start to bind and run.
func New(cfg Config) (*Node, error) {
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		return nil, err
	}
	if cfg.Log == nil {
		cfg.Log = func(string, ...any) {}
	}
	n := &Node{
		cfg:       cfg,
		peerID:    id,
		adapter:   cfg.Adapter,
		peers:     make(map[string]*peer),
		sessions:  make(map[uint64]*peer),
		pending:   make(map[string]*pendingOut),
		tentative: make(map[*session.Session]*peer),
		throttles: make(map[string]*throttle),
		known:     make(map[string]knownMember),
		blocked:   cfg.BlockDirectAddrs,
		closed:    make(chan struct{}),
	}
	if n.blocked == nil {
		n.blocked = make(map[string]bool)
	}
	n.selfIP = room.IPForPeer(id, nil)
	return n, nil
}

// SetAdapter swaps the adapter before Start (used to substitute a
// real TUN device once our room IP is known).
func (n *Node) SetAdapter(a adapter.Adapter) {
	n.adapter = a
	n.cfg.Adapter = a
}

// PeerIDHex returns our peer id.
func (n *Node) PeerIDHex() string { return hex.EncodeToString(n.peerID) }

// SelfIP returns our room-local IP.
func (n *Node) SelfIP() string { return room.IPString(n.selfIP) }

// ListenAddr returns the tunnel UDP address (valid after Start).
func (n *Node) ListenAddr() string {
	if n.tp == nil {
		return ""
	}
	return fmt.Sprintf("0.0.0.0:%d", n.tp.port)
}

// Start binds the UDP transport and launches the pumps.
func (n *Node) Start() error {
	tp, err := newTransport(n.cfg.ListenPort, func(pkt []byte, src *net.UDPAddr) {
		n.handleDatagram(pkt, src, "")
	}, n.cfg.Log)
	if err != nil {
		return err
	}
	n.tp = tp
	n.started = time.Now()
	go n.adapterLoop()
	go n.maintenanceLoop()
	n.logf("node started: name=%q peer=%s ip=%s listen=%s", n.cfg.Name, n.PeerIDHex()[:8], n.SelfIP(), n.ListenAddr())
	return nil
}

// Close shuts the node down.
func (n *Node) Close() {
	n.closeOnce.Do(func() {
		close(n.closed)
		if n.tp != nil {
			n.tp.close()
		}
		if n.adapter != nil {
			n.adapter.Close()
		}
	})
}

func (n *Node) logf(format string, args ...any) { n.cfg.Log(format, args...) }

// --- Connection initiation ---------------------------------------

// Connect initiates a handshake to a peer at a known UDP endpoint
// (from the DHT or manual entry). Safe to call repeatedly.
func (n *Node) Connect(addrStr string) {
	addr, err := net.ResolveUDPAddr("udp", addrStr)
	if err != nil {
		n.logf("connect: bad address %q: %v", addrStr, err)
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	// Note: a test-blocked address is still attempted. The block
	// simulates an unpunchable NAT pair — our packets leave, the
	// far side's handler drops them (see handleHello), no reply
	// ever comes — which is exactly what drives the relay
	// fallback after the attempt times out.
	for _, p := range n.peers {
		if p.endpoint != nil && p.endpoint.String() == addr.String() && p.established {
			return
		}
	}
	key := "addr:" + addr.String()
	if _, exists := n.pending[key]; exists {
		return
	}
	n.startHandshakeLocked(key, addr, "", nil)
}

// connectViaRelay initiates a handshake to targetID through the
// established peer viaHex.
func (n *Node) connectViaRelay(targetID []byte, viaHex string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if _, exists := n.peers[hex.EncodeToString(targetID)]; exists {
		return
	}
	key := "via:" + viaHex + ":" + hex.EncodeToString(targetID)
	if _, exists := n.pending[key]; exists {
		return
	}
	n.startHandshakeLocked(key, nil, viaHex, targetID)
}

func (n *Node) startHandshakeLocked(key string, addr *net.UDPAddr, via string, targetID []byte) {
	localSID := session.RandomSessionID()
	var myID [16]byte
	copy(myID[:], n.peerID)
	dgram, ephPriv, hello, err := session.BuildHello(n.cfg.RoomKey, localSID, myID)
	if err != nil {
		n.logf("handshake build failed: %v", err)
		return
	}
	p := &pendingOut{
		key: key, hello: hello, ephPriv: ephPriv, localSID: localSID,
		addr: addr, via: via, targetID: targetID, created: time.Now(),
	}
	n.pending[key] = p
	n.sendHelloLocked(p, dgram)
	n.logf("handshake started: key=%s", key)
}

// sendHelloLocked transmits (or retransmits) a HELLO, wrapped for
// relay delivery when the handshake is relayed.
func (n *Node) sendHelloLocked(p *pendingOut, dgram []byte) {
	p.attempts++
	if p.via == "" {
		n.writeRaw(dgram, p.addr)
		return
	}
	relay, ok := n.peers[p.via]
	if !ok || !relay.established || relay.endpoint == nil {
		return
	}
	relayID, _ := hex.DecodeString(p.via)
	wrapped := wrapDatagram(n.peerID, p.targetID, relayID, dgram)
	n.writeRaw(wrapped, relay.endpoint)
}

// wrapDatagram builds a TypeRelayWrap envelope:
// type | srcPeerID(16) | dstPeerID(16) | relayPeerID(16) | inner...
func wrapDatagram(srcID, dstID, relayID, inner []byte) []byte {
	out := make([]byte, 0, 1+48+len(inner))
	out = append(out, session.TypeRelayWrap)
	out = append(out, srcID...)
	out = append(out, dstID...)
	out = append(out, relayID...)
	out = append(out, inner...)
	return out
}

// --- Socket pumps -------------------------------------------------

func (n *Node) writeRaw(b []byte, addr *net.UDPAddr) {
	if n.tp == nil || addr == nil {
		return
	}
	n.tp.send(b, addr)
}

// handleDatagram dispatches one received datagram. via is the
// relay peer-id hex when the datagram arrived wrapped, else "".
func (n *Node) handleDatagram(dgram []byte, src *net.UDPAddr, via string) {
	if len(dgram) < 1 {
		return
	}
	switch dgram[0] {
	case session.TypeHello:
		n.logf("recv HELLO from %s via=%q len=%d", src, via, len(dgram))
		n.handleHello(dgram, src, via)
	case session.TypeReply:
		n.logf("recv REPLY from %s via=%q len=%d", src, via, len(dgram))
		n.handleReply(dgram, src, via)
	case session.TypeData:
		n.handleData(dgram, src, via)
	case session.TypeRelayWrap:
		n.handleRelayWrap(dgram, src)
	}
}

func (n *Node) handleRelayWrap(dgram []byte, src *net.UDPAddr) {
	if len(dgram) < 1+48+1 {
		return
	}
	srcID := dgram[1:17]
	dstID := dgram[17:33]
	relayID := dgram[33:49]
	inner := dgram[49:]
	if hex.EncodeToString(dstID) == n.PeerIDHex() {
		// For us: process the inner datagram as arriving via the
		// named relay peer.
		n.handleDatagram(inner, src, hex.EncodeToString(relayID))
		return
	}
	// Forward to the destination member, untouched inside.
	n.mu.Lock()
	dst, ok := n.peers[hex.EncodeToString(dstID)]
	var endpoint *net.UDPAddr
	if ok && dst.endpoint != nil {
		endpoint = dst.endpoint
	}
	n.mu.Unlock()
	if endpoint == nil {
		n.logf("relay-wrap: no route to %s", hex.EncodeToString(dstID)[:8])
		return
	}
	// Rewrite the relay id to us (we are the carrier on this hop).
	out := wrapDatagram(srcID, dstID, n.peerID, inner)
	n.writeRaw(out, endpoint)
	n.mu.Lock()
	n.RelayedPackets++
	n.mu.Unlock()
}

// --- Handshake: responder side ------------------------------------

func (n *Node) handleHello(dgram []byte, src *net.UDPAddr, via string) {
	// Throttle per source endpoint (IP:port), not per IP: an
	// IP-wide block would lock out legitimate roommates behind
	// the same NAT (and did, on loopback, in testing). Online
	// password guessing is already slow on the attacker's side —
	// every guess costs them an Argon2id run to build the tag —
	// so per-endpoint backoff is a sound second belt for v1.
	throttleKey := src.String()
	if via != "" {
		// Relayed handshakes are vouched for by a member, but the
		// claimed source still gets throttled on failure.
		throttleKey = "relay:" + src.String()
	}
	n.mu.Lock()
	if th, ok := n.throttles[throttleKey]; ok && time.Now().Before(th.blockedUntil) {
		n.mu.Unlock()
		return
	}
	if via == "" && n.blocked[src.String()] {
		n.mu.Unlock()
		return
	}
	n.mu.Unlock()

	hello, ok := session.ParseHello(n.cfg.RoomKey, dgram)
	if !ok {
		n.recordThrottleFailure(throttleKey)
		n.mu.Lock()
		n.HandshakeFailures++
		n.mu.Unlock()
		n.logf("handshake rejected: bad tag from %s (wrong password or foreign room)", src)
		return
	}
	n.resetThrottle(throttleKey)

	ourSID := session.RandomSessionID()
	reply, ephPriv, randR, err := session.BuildReply(n.cfg.RoomKey, hello, ourSID)
	if err != nil {
		n.logf("reply build failed: %v", err)
		return
	}
	sess, err := session.NewSession(ephPriv, hello.EphPub, n.cfg.RoomKey, hello.Rand, randR, false, ourSID, hello.SessionID)
	if err != nil {
		n.logf("session build failed: %v", err)
		return
	}
	p := &peer{
		sess: sess, direct: via == "", via: via, lastSeen: time.Now(), rttMillis: -1,
		created: time.Now(),
		// The HELLO is tag-authenticated and carries the
		// initiator's peer id, so we know who is knocking.
		id: append([]byte{}, hello.PeerID[:]...),
	}
	// Return path: direct to the source, or back through the
	// relay peer that carried the HELLO (src is its endpoint).
	p.endpoint = src
	n.mu.Lock()
	n.sessions[ourSID] = p
	n.tentative[sess] = p
	n.mu.Unlock()

	if via == "" {
		n.writeRaw(reply, src)
	} else {
		relayID, _ := hex.DecodeString(via)
		wrapped := wrapDatagram(n.peerID, hello.PeerID[:], relayID, reply)
		n.writeRaw(wrapped, src)
	}
	n.logf("handshake accepted from %s (via=%q); reply sent", src, via)
}

// --- Handshake: initiator side ------------------------------------

func (n *Node) handleReply(dgram []byte, src *net.UDPAddr, via string) {
	n.mu.Lock()
	var p *pendingOut
	for _, cand := range n.pending {
		if cand.via == "" && via == "" && cand.addr != nil && cand.addr.String() == src.String() {
			p = cand
			break
		}
		if cand.via != "" && via == cand.via {
			p = cand
			break
		}
	}
	n.mu.Unlock()
	if p == nil {
		return
	}
	sidR, ephPubR, randR, ok := session.VerifyReply(n.cfg.RoomKey, p.hello, dgram)
	if !ok {
		n.logf("handshake failed: reply tag mismatch from %s (wrong password on one side)", src)
		n.mu.Lock()
		n.HandshakeFailures++
		delete(n.pending, p.key)
		n.mu.Unlock()
		return
	}
	sess, err := session.NewSession(p.ephPriv, ephPubR, n.cfg.RoomKey, p.hello.Rand, randR, true, p.localSID, sidR)
	if err != nil {
		n.logf("session build failed: %v", err)
		return
	}
	np := &peer{
		sess: sess, direct: via == "", via: via, lastSeen: time.Now(), rttMillis: -1,
		created: time.Now(),
	}
	if via == "" {
		np.endpoint = src
	} else {
		np.endpoint = src // the relay's endpoint; traffic wraps through the relay peer
		np.id = p.targetID
	}
	n.mu.Lock()
	n.sessions[p.localSID] = np
	n.tentative[sess] = np
	delete(n.pending, p.key)
	n.mu.Unlock()
	n.sendPeerInfo(np)
	n.logf("handshake complete with %s (via=%q); peerinfo sent", src, via)
}

// --- Session data --------------------------------------------------

func (n *Node) handleData(dgram []byte, src *net.UDPAddr, via string) {
	sid, ok := session.DataSessionID(dgram)
	if !ok {
		return
	}
	n.mu.Lock()
	p, known := n.sessions[sid]
	n.mu.Unlock()
	if !known {
		return
	}
	innerType, payload, err := p.sess.Open(dgram)
	if err != nil {
		return // replay, corruption, or not for us: drop silently
	}
	n.mu.Lock()
	p.lastSeen = time.Now()
	n.PacketsReceived++
	n.mu.Unlock()

	switch innerType {
	case session.InnerIP:
		if n.adapter != nil {
			n.adapter.WritePacket(payload)
		}
	case session.InnerPing:
		n.sendInner(p, session.InnerPong, payload)
	case session.InnerPong:
		if len(payload) == 16 {
			n.mu.Lock()
			sent := int64(binary.BigEndian.Uint64(payload[8:16]))
			rtt := time.Since(time.Unix(0, sent)).Milliseconds()
			p.rttMillis = int(rtt)
			n.mu.Unlock()
		}
	case session.InnerPeerInfo:
		n.handlePeerInfo(p, payload)
	case session.InnerGossip:
		n.handleGossip(p, payload)
	case session.InnerRelay:
		n.handleRelay(p, payload)
	}
}

// sendInner seals and dispatches one inner payload to a peer,
// wrapping through its relay when the peer is relay-reached.
func (n *Node) sendInner(p *peer, innerType byte, payload []byte) {
	dgram := p.sess.Seal(innerType, payload)
	n.mu.Lock()
	n.PacketsSent++
	via := p.via
	endpoint := p.endpoint
	n.mu.Unlock()
	if via == "" {
		n.writeRaw(dgram, endpoint)
		return
	}
	// Wrap for the relay: payload = dstPeerID | raw datagram.
	n.mu.Lock()
	relay, ok := n.peers[via]
	n.mu.Unlock()
	if !ok || relay == nil || relay.sess == nil {
		return
	}
	wrappedPayload := make([]byte, 0, 16+len(dgram))
	wrappedPayload = append(wrappedPayload, p.id...)
	wrappedPayload = append(wrappedPayload, dgram...)
	outer := relay.sess.Seal(session.InnerRelay, wrappedPayload)
	n.writeRaw(outer, relay.endpoint)
}

// handleRelay forwards a sealed datagram to its target member.
func (n *Node) handleRelay(from *peer, payload []byte) {
	if len(payload) < 16+1 {
		return
	}
	dstID := payload[:16]
	raw := payload[16:]
	n.mu.Lock()
	dst, ok := n.peers[hex.EncodeToString(dstID)]
	var endpoint *net.UDPAddr
	if ok {
		endpoint = dst.endpoint
	}
	n.RelayedPackets++
	n.mu.Unlock()
	if endpoint == nil {
		return
	}
	n.writeRaw(raw, endpoint)
}

// --- Peer info & gossip --------------------------------------------

type peerInfoMsg struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type gossipMsg struct {
	Members []gossipMember `json:"members"`
}

type gossipMember struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Endpoints []string `json:"endpoints"`
}

func (n *Node) sendPeerInfo(p *peer) {
	n.mu.Lock()
	if p.sentInfo {
		n.mu.Unlock()
		return
	}
	p.sentInfo = true
	n.mu.Unlock()
	msg, _ := json.Marshal(peerInfoMsg{ID: n.PeerIDHex(), Name: n.cfg.Name})
	n.sendInner(p, session.InnerPeerInfo, msg)
}

func (n *Node) handlePeerInfo(p *peer, payload []byte) {
	var msg peerInfoMsg
	if err := json.Unmarshal(payload, &msg); err != nil {
		return
	}
	idBytes, err := hex.DecodeString(msg.ID)
	if err != nil || len(idBytes) != 16 {
		return
	}
	n.mu.Lock()
	_, wasTentative := n.tentative[p.sess]
	if wasTentative {
		delete(n.tentative, p.sess)
	}
	p.id = idBytes
	p.name = msg.Name
	p.established = true
	n.peers[msg.ID] = p
	n.recomputeIPsLocked()
	n.mu.Unlock()
	n.logf("member established: %q (%s) ip=%s direct=%v", msg.Name, msg.ID[:8], room.IPString(p.ip), p.direct)
	// Answer with our info if we have not sent it yet (the
	// responder's peerinfo is how the initiator learns us; the
	// initiator already sent theirs and must not answer again,
	// or the two sides would bounce peerinfo forever).
	n.sendPeerInfo(p)
	n.broadcastGossip()
	n.notifyMembers()
}

func (n *Node) broadcastGossip() {
	n.mu.Lock()
	msg := gossipMsg{}
	for _, p := range n.peers {
		if !p.established {
			continue
		}
		gm := gossipMember{ID: hex.EncodeToString(p.id), Name: p.name}
		if p.endpoint != nil {
			gm.Endpoints = []string{p.endpoint.String()}
		}
		msg.Members = append(msg.Members, gm)
	}
	// Include ourselves so a joiner learns the full list from any
	// single member.
	self := gossipMember{ID: n.PeerIDHex(), Name: n.cfg.Name}
	if n.tp != nil {
		// ListenAddr is on the wildcard address; present the
		// loopback form so same-machine peers can use it. On real
		// networks, peers learn our reachable endpoint from the
		// source address of our datagrams instead.
		self.Endpoints = []string{strings.Replace(n.ListenAddr(), "0.0.0.0", "127.0.0.1", 1)}
	}
	msg.Members = append(msg.Members, self)
	data, _ := json.Marshal(msg)
	targets := make([]*peer, 0)
	for _, p := range n.peers {
		if p.established {
			targets = append(targets, p)
		}
	}
	n.mu.Unlock()
	for _, p := range targets {
		n.sendInner(p, session.InnerGossip, data)
	}
}

func (n *Node) handleGossip(from *peer, payload []byte) {
	var msg gossipMsg
	if err := json.Unmarshal(payload, &msg); err != nil {
		return
	}
	n.mu.Lock()
	fromHex := ""
	if from.id != nil {
		fromHex = hex.EncodeToString(from.id)
	}
	var toConnect []gossipMember
	for _, gm := range msg.Members {
		if gm.ID == n.PeerIDHex() {
			continue
		}
		idBytes, err := hex.DecodeString(gm.ID)
		if err != nil || len(idBytes) != 16 {
			continue
		}
		if _, connected := n.peers[gm.ID]; connected {
			continue
		}
		n.known[gm.ID] = knownMember{id: idBytes, name: gm.Name, endpoints: gm.Endpoints, from: fromHex, learned: time.Now()}
		toConnect = append(toConnect, gm)
	}
	n.mu.Unlock()
	for _, gm := range toConnect {
		for _, ep := range gm.Endpoints {
			n.Connect(ep)
			break // one endpoint is enough to start; retries handle the rest
		}
	}
}

// recomputeIPsLocked assigns room IPs deterministically: members
// sorted by peer id each take the first free derived slot, so
// every member computes the same assignment independently.
func (n *Node) recomputeIPsLocked() {
	ids := [][]byte{n.peerID}
	for _, p := range n.peers {
		if p.established && p.id != nil {
			ids = append(ids, p.id)
		}
	}
	sort.Slice(ids, func(i, j int) bool {
		return hex.EncodeToString(ids[i]) < hex.EncodeToString(ids[j])
	})
	taken := make(map[[4]byte]bool)
	assign := make(map[string][4]byte)
	for _, id := range ids {
		ip := room.IPForPeer(id, taken)
		taken[ip] = true
		assign[hex.EncodeToString(id)] = ip
	}
	n.selfIP = assign[n.PeerIDHex()]
	for _, p := range n.peers {
		if p.id != nil {
			p.ip = assign[hex.EncodeToString(p.id)]
		}
	}
}

// --- Adapter pump & routing ----------------------------------------

func (n *Node) adapterLoop() {
	if n.adapter == nil {
		return
	}
	for {
		pkt, err := n.adapter.ReadPacket()
		if err != nil {
			select {
			case <-n.closed:
				return
			default:
				return
			}
		}
		n.routeOutbound(pkt)
	}
}

// routeOutbound sends one IP packet from the adapter into the room.
func (n *Node) routeOutbound(pkt []byte) {
	if len(pkt) < 20 || pkt[0]>>4 != 4 {
		return
	}
	var dst [4]byte
	copy(dst[:], pkt[16:20])
	n.mu.Lock()
	targets := make([]*peer, 0, 1)
	if room.IsBroadcast(dst) || (dst[0] >= 224 && dst[0] <= 239) {
		for _, p := range n.peers {
			if p.established {
				targets = append(targets, p)
			}
		}
	} else {
		for _, p := range n.peers {
			if p.established && p.ip == dst {
				targets = append(targets, p)
				break
			}
		}
	}
	n.mu.Unlock()
	for _, p := range targets {
		n.sendInner(p, session.InnerIP, pkt)
	}
}

// --- Maintenance: retries, pings, pruning, relay fallback ----------

func (n *Node) maintenanceLoop() {
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	var lastPing, lastGossip time.Time
	for {
		select {
		case <-n.closed:
			return
		case now := <-tick.C:
			n.retryPending(now)
			if now.Sub(lastPing) > 5*time.Second {
				lastPing = now
				n.pingAll(now)
			}
			if now.Sub(lastGossip) > 20*time.Second {
				lastGossip = now
				n.broadcastGossip()
			}
		}
	}
}

// retryPending retransmits HELLOs — this retransmission burst is
// the NAT hole punch: repeated outbound packets from our socket
// open the local NAT mapping while the peer's replies open theirs.
// Expired direct attempts fall back to a relayed attempt through
// any mutual member.
func (n *Node) retryPending(now time.Time) {
	n.mu.Lock()
	// Prune responder-side sessions whose initiator never
	// completed (no peerinfo within 30s): they hold a session id
	// and nothing else.
	for sess, p := range n.tentative {
		if !p.established && now.Sub(p.created) > 30*time.Second {
			delete(n.tentative, sess)
			delete(n.sessions, sess.ID)
		}
	}
	var expired []*pendingOut
	for _, p := range n.pending {
		if now.Sub(p.created) > 20*time.Second {
			expired = append(expired, p)
			delete(n.pending, p.key)
			continue
		}
		// Punch cadence: fast for the first 2 seconds, then steady.
		interval := 1200 * time.Millisecond
		if now.Sub(p.created) < 2*time.Second {
			interval = 250 * time.Millisecond
		}
		if now.After(p.nextRetry) {
			p.nextRetry = now.Add(interval)
			// Rebuild the datagram from the stored hello fields by
			// re-sending the identical HELLO (same tag/ephemeral):
			dgram := helloDatagram(n.cfg.RoomKey, p)
			if dgram != nil {
				n.sendHelloLocked(p, dgram)
			}
		}
	}
	n.mu.Unlock()
	// Relay fallback for expired direct attempts.
	for _, p := range expired {
		if p.via != "" || p.addr == nil {
			continue
		}
		n.tryRelayFallback(p.addr)
	}
}

// helloDatagram reconstructs the exact HELLO bytes for a pending
// handshake (retransmissions must be identical: same ephemeral key
// and tag, or the peer's REPLY won't match our stored state).
func helloDatagram(roomKey []byte, p *pendingOut) []byte {
	// Rebuild from stored hello: type | sid | peerID | ephPub | rand | tag.
	tag := room.Proof(roomKey, []byte("svlan-hello-v1"), uint64Bytes(p.hello.SessionID), p.hello.PeerID[:], p.hello.EphPub[:], p.hello.Rand[:])
	out := make([]byte, 0, 105)
	out = append(out, session.TypeHello)
	out = append(out, uint64Bytes(p.hello.SessionID)...)
	out = append(out, p.hello.PeerID[:]...)
	out = append(out, p.hello.EphPub[:]...)
	out = append(out, p.hello.Rand[:]...)
	out = append(out, tag...)
	return out
}

func uint64Bytes(v uint64) []byte {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], v)
	return b[:]
}

// tryRelayFallback finds a gossiped member whose endpoint matches
// the failed address, and retries the handshake relayed through
// any established mutual member.
func (n *Node) tryRelayFallback(addr *net.UDPAddr) {
	n.mu.Lock()
	var targetID []byte
	for _, km := range n.known {
		for _, ep := range km.endpoints {
			if ep == addr.String() {
				targetID = km.id
			}
		}
	}
	var viaHex string
	if targetID != nil {
		for hexID, p := range n.peers {
			if p.established && p.direct {
				viaHex = hexID
				break
			}
		}
	}
	n.mu.Unlock()
	if targetID != nil && viaHex != "" {
		n.logf("direct handshake to %s timed out; falling back to relay via %s", addr, viaHex[:8])
		n.connectViaRelay(targetID, viaHex)
	}
}

func (n *Node) pingAll(now time.Time) {
	n.mu.Lock()
	targets := make([]*peer, 0)
	for _, p := range n.peers {
		if p.established {
			targets = append(targets, p)
		}
	}
	n.mu.Unlock()
	for _, p := range targets {
		var payload [16]byte
		binary.BigEndian.PutUint64(payload[0:8], uint64(now.UnixNano()))
		binary.BigEndian.PutUint64(payload[8:16], uint64(now.UnixNano()))
		n.sendInner(p, session.InnerPing, payload[:])
	}
}

// --- Throttling ------------------------------------------------------

func (n *Node) recordThrottleFailure(key string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	th, ok := n.throttles[key]
	if !ok {
		th = &throttle{}
		n.throttles[key] = th
	}
	th.failures++
	if th.failures >= 3 {
		backoff := time.Duration(1<<(th.failures-3)) * 5 * time.Second
		if backoff > 120*time.Second {
			backoff = 120 * time.Second
		}
		th.blockedUntil = time.Now().Add(backoff)
		n.logf("throttle: %s blocked for %s after %d failed handshakes", key, backoff, th.failures)
	}
}

func (n *Node) resetThrottle(key string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	delete(n.throttles, key)
}

// --- Views -----------------------------------------------------------

// Members returns a snapshot of the member list (self first).
func (n *Node) Members() []MemberView {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := []MemberView{{
		PeerID: n.PeerIDHex(), Name: n.cfg.Name, IP: n.SelfIP(),
		Self: true, Connected: true, Direct: true, RTTMillis: 0,
	}}
	ids := make([]string, 0, len(n.peers))
	for id := range n.peers {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		p := n.peers[id]
		if !p.established {
			continue
		}
		mv := MemberView{
			PeerID: id, Name: p.name, IP: room.IPString(p.ip),
			Connected: true, Direct: p.direct, RTTMillis: p.rttMillis,
		}
		if p.endpoint != nil {
			mv.Endpoint = p.endpoint.String()
		}
		if !p.direct {
			if relay, ok := n.peers[p.via]; ok && relay != nil {
				mv.Via = relay.name
			} else {
				mv.Via = p.via[:8]
			}
		}
		out = append(out, mv)
	}
	return out
}

func (n *Node) notifyMembers() {
	if n.cfg.OnMembers != nil {
		n.cfg.OnMembers(n.Members())
	}
}
