// svlan — the Serverless Virtual LAN tool.
//
// Modes:
//
//	svlan                          graphical UI (front door)
//	svlan -mode bootstrap          a bare DHT bootstrap node
//	svlan -mode host [actions]     host a room (headless)
//	svlan -mode join [actions]     join a room (headless)
//
// The headless modes double as the verification harness: scripted
// actions (wait for members, ICMP echo across the tunnel, UDP
// echo, broadcast) print RESULT lines that test scripts grep.
package main

import (
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"svlan/internal/adapter"
	"svlan/internal/dhtx"
	"svlan/internal/node"
	"svlan/internal/room"
	"svlan/internal/ui"
)

func main() {
	var (
		mode        = flag.String("mode", "ui", "ui | host | join | bootstrap")
		name        = flag.String("name", "", "display name in the room")
		password    = flag.String("password", "", "room password")
		code        = flag.String("code", "", "room code (host: set or generate; join: required)")
		port        = flag.Int("port", 0, "tunnel UDP port (0 = any)")
		dhtPort     = flag.Int("dht-port", 0, "DHT UDP port (0 = any)")
		bootstrap   = flag.String("bootstrap", "", "comma list of DHT bootstrap addrs (empty = public list)")
		peers       = flag.String("peer", "", "comma list of peer tunnel addrs to connect to directly")
		noDHT       = flag.Bool("no-dht", false, "skip DHT entirely (manual peers only)")
		useTUN      = flag.Bool("tun", false, "use a real TUN adapter instead of loopback")
		echo        = flag.Bool("echo", false, "answer ICMP/UDP echo requests arriving from the room (test harness)")
		waitMembers = flag.Int("wait-members", -1, "wait until at least N other members are connected")
		icmpTarget  = flag.String("icmp", "", "send an ICMP echo to this room IP and await the reply")
		icmpName    = flag.String("icmp-name", "", "send an ICMP echo to the member with this display name")
		udpEcho     = flag.String("udp-echo", "", "send a UDP echo to ip:port with -payload and await the reply")
		broadcast   = flag.String("broadcast", "", "send this payload as a room broadcast (UDP port 42424)")
		payload     = flag.String("payload", "svlan-test", "payload for udp-echo")
		membersOut  = flag.Bool("members", false, "print the member list as JSON before exiting")
		hold        = flag.Duration("hold", 0, "keep running this long after actions complete")
		timeout     = flag.Duration("timeout", 45*time.Second, "timeout for wait actions")
		blockDirect = flag.String("block-direct", "", "comma list of UDP addrs to treat as unreachable-direct (relay testing)")
		announceInt = flag.Duration("announce-interval", 30*time.Second, "DHT re-announce interval")
	)
	flag.Parse()

	if *mode == "ui" {
		ui.Run()
		return
	}
	if *name == "" {
		*name = *mode
	}
	logf := func(format string, args ...any) {
		fmt.Printf("%s [%s] %s\n", time.Now().Format("15:04:05.000"), *name, fmt.Sprintf(format, args...))
	}

	if *mode == "bootstrap" {
		svc, err := dhtx.StartLocal(*dhtPort)
		if err != nil {
			logf("bootstrap failed: %v", err)
			os.Exit(1)
		}
		logf("RESULT bootstrap-ready addr=%s", svc.Addr())
		select {}
	}

	if *mode != "host" && *mode != "join" {
		fmt.Fprintf(os.Stderr, "unknown mode %q\n", *mode)
		os.Exit(2)
	}
	if *password == "" {
		fmt.Fprintf(os.Stderr, "a room password is required\n")
		os.Exit(2)
	}
	if *mode == "host" && *code == "" {
		c, err := room.GenerateCode()
		if err != nil {
			logf("code generation failed: %v", err)
			os.Exit(1)
		}
		*code = c
	}
	if !room.ValidCode(*code) {
		fmt.Fprintf(os.Stderr, "room code %q is not valid\n", *code)
		os.Exit(2)
	}
	logf("room code: %s", *code)
	fmt.Printf("ROOMCODE %s\n", room.NormalizeCode(*code))

	blocked := make(map[string]bool)
	for _, a := range splitComma(*blockDirect) {
		blocked[a] = true
	}

	// Start with a loopback adapter so the node exists (its peer
	// id determines our room IP); swap in a real TUN before Start
	// when one was requested and can be created.
	loop := adapter.NewLoopback("loopback-" + *name)
	nd, err := node.New(node.Config{
		Name:             *name,
		RoomKey:          room.RoomKey(*code, *password),
		Adapter:          loop,
		ListenPort:       *port,
		Log:              logf,
		BlockDirectAddrs: blocked,
	})
	if err != nil {
		logf("node create failed: %v", err)
		os.Exit(1)
	}
	usingLoop := true
	if *useTUN {
		tunAdp, terr := adapter.OpenTUN("svlan0", mustParseIP(nd.SelfIP()))
		if terr != nil {
			logf("RESULT tun-unavailable: %v (falling back to loopback)", terr)
		} else {
			logf("RESULT tun-up name=%s ip=%s", tunAdp.Name(), nd.SelfIP())
			nd.SetAdapter(tunAdp)
			usingLoop = false
		}
	}
	if err := nd.Start(); err != nil {
		logf("node start failed: %v", err)
		os.Exit(1)
	}
	defer nd.Close()
	logf("RESULT node-ready peer=%s ip=%s listen=%s", nd.PeerIDHex(), nd.SelfIP(), nd.ListenAddr())

	// OS-side harness for the loopback adapter: answers echo
	// requests, notes broadcasts, and hands received packets to
	// any waiting action.
	var osSide *osSide
	if usingLoop {
		osSide = newOSSide(loop, *echo, logf)
		go osSide.run()
	}

	// DHT room flow.
	if !*noDHT {
		var bootList []string
		if *bootstrap != "" {
			bootList = splitComma(*bootstrap)
		}
		svc, err := dhtx.Start(*dhtPort, bootList)
		if err != nil {
			logf("dht start failed: %v", err)
			os.Exit(1)
		}
		defer svc.Close()
		logf("dht ready at %s", svc.Addr())
		tunnelPort := mustPort(nd.ListenAddr())
		key := room.DHTKey(*code)
		go svc.AnnounceLoop(key, tunnelPort, *announceInt, logf, func(addr string) {
			if mustPort(addr) == tunnelPort && isLocalAddr(addr) {
				return // ourselves, via the DHT mirror
			}
			logf("dht discovered peer %s", addr)
			nd.Connect(addr)
		})
	}
	for _, p := range splitComma(*peers) {
		nd.Connect(p)
	}

	failed := false
	// --- Scripted actions ---
	if *waitMembers >= 0 {
		if waitForMembers(nd, *waitMembers, *timeout) {
			logf("RESULT members-ok count=%d", len(nd.Members())-1)
		} else {
			logf("RESULT members-timeout count=%d", len(nd.Members())-1)
			failed = true
		}
	}
	if *icmpName != "" {
		for _, m := range nd.Members() {
			if m.Name == *icmpName {
				*icmpTarget = m.IP
			}
		}
		if *icmpTarget == "" {
			logf("RESULT icmp-fail no member named %q", *icmpName)
			failed = true
		}
	}
	if *icmpTarget != "" {
		if osSide == nil {
			logf("RESULT icmp-unavailable (needs loopback adapter)")
			failed = true
		} else if rtt, err := osSide.icmpEcho(nd.SelfIP(), *icmpTarget, *timeout); err != nil {
			logf("RESULT icmp-fail target=%s err=%v", *icmpTarget, err)
			failed = true
		} else {
			logf("RESULT icmp-ok target=%s rtt=%s", *icmpTarget, rtt)
		}
	}
	if *udpEcho != "" {
		if osSide == nil {
			logf("RESULT udp-echo-unavailable (needs loopback adapter)")
			failed = true
		} else if err := osSide.udpEcho(nd.SelfIP(), *udpEcho, *payload, *timeout); err != nil {
			logf("RESULT udp-echo-fail target=%s err=%v", *udpEcho, err)
			failed = true
		} else {
			logf("RESULT udp-echo-ok target=%s payload=%q", *udpEcho, *payload)
		}
	}
	if *broadcast != "" {
		if osSide == nil {
			logf("RESULT broadcast-unavailable (needs loopback adapter)")
			failed = true
		} else {
			osSide.sendBroadcast(nd.SelfIP(), *broadcast)
			logf("RESULT broadcast-sent payload=%q", *broadcast)
		}
	}
	if *membersOut {
		data, _ := json.Marshal(nd.Members())
		logf("RESULT members %s", data)
	}
	if *hold > 0 {
		logf("holding for %s", *hold)
		time.Sleep(*hold)
		if *membersOut {
			data, _ := json.Marshal(nd.Members())
			logf("RESULT members-after-hold %s", data)
		}
	}
	if failed {
		os.Exit(1)
	}
}

func splitComma(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func mustPort(addr string) int {
	_, ps, err := net.SplitHostPort(addr)
	if err != nil {
		return -1
	}
	p, _ := strconv.Atoi(ps)
	return p
}

func isLocalAddr(addr string) bool {
	h, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(h)
	return ip != nil && (ip.IsLoopback() || ip.IsUnspecified())
}

func mustParseIP(s string) [4]byte {
	ip, ok := room.ParseIP(s)
	if !ok {
		panic("bad ip " + s)
	}
	return ip
}

func waitForMembers(nd *node.Node, want int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if len(nd.Members())-1 >= want {
			return true
		}
		time.Sleep(200 * time.Millisecond)
	}
	return len(nd.Members())-1 >= want
}

// --- OS-side test harness (loopback adapter only) -----------------

const (
	echoUDPPort      = 4242
	broadcastUDPPort = 42424
)

// osSide plays the role of the local OS on a loopback adapter:
// it answers echo requests from the room, logs broadcasts, and
// routes other received packets to waiting scripted actions.
type osSide struct {
	loop    *adapter.Loopback
	echo    bool
	logf    func(string, ...any)
	results chan []byte
}

func newOSSide(loop *adapter.Loopback, echo bool, logf func(string, ...any)) *osSide {
	return &osSide{loop: loop, echo: echo, logf: logf, results: make(chan []byte, 64)}
}

func (o *osSide) run() {
	for pkt := range o.loop.RecvToOS() {
		o.handle(pkt)
	}
}

func (o *osSide) handle(pkt []byte) {
	if len(pkt) < 20 || pkt[0]>>4 != 4 {
		return
	}
	proto := pkt[9]
	src := pkt[12:16]
	dst := pkt[16:20]
	ihl := int(pkt[0]&0x0f) * 4
	if proto == 1 && len(pkt) >= ihl+8 { // ICMP
		icmp := pkt[ihl:]
		if icmp[0] == 8 && o.echo { // echo request -> reply
			reply := buildICMPReply(pkt)
			o.loop.SendFromOS(reply)
			o.logf("echo: answered icmp echo from %s", ipStr(src))
			return
		}
		if icmp[0] == 0 { // echo reply -> waiting action
			o.deliver(pkt)
			return
		}
	}
	if proto == 17 && len(pkt) >= ihl+8 { // UDP
		udp := pkt[ihl:]
		dstPort := binary.BigEndian.Uint16(udp[2:4])
		payload := udp[8:]
		if isBroadcastIP(dst) && dstPort == broadcastUDPPort {
			o.logf("RESULT broadcast-received from=%s payload=%q", ipStr(src), string(payload))
			return
		}
		if dstPort == echoUDPPort && o.echo {
			reply := buildUDPReply(pkt)
			o.loop.SendFromOS(reply)
			o.logf("echo: answered udp echo from %s payload=%q", ipStr(src), string(payload))
			return
		}
		o.deliver(pkt)
	}
}

func (o *osSide) deliver(pkt []byte) {
	select {
	case o.results <- pkt:
	default:
	}
}

// awaitPacket waits for a received packet matching pred.
func (o *osSide) awaitPacket(pred func([]byte) bool, timeout time.Duration) ([]byte, error) {
	deadline := time.After(timeout)
	for {
		select {
		case pkt := <-o.results:
			if pred(pkt) {
				return pkt, nil
			}
		case <-deadline:
			return nil, fmt.Errorf("timed out waiting for packet")
		}
	}
}

func (o *osSide) icmpEcho(selfIP, target string, timeout time.Duration) (time.Duration, error) {
	id := uint16(time.Now().Unix() & 0xffff)
	pkt := buildICMPRequest(selfIP, target, id, 1)
	start := time.Now()
	o.loop.SendFromOS(pkt)
	want := mustParseIP(target)
	_, err := o.awaitPacket(func(p []byte) bool {
		if len(p) < 28 || p[9] != 1 {
			return false
		}
		ihl := int(p[0]&0x0f) * 4
		return p[12] == want[0] && p[13] == want[1] && p[14] == want[2] && p[15] == want[3] &&
			p[ihl] == 0 && binary.BigEndian.Uint16(p[ihl+4:ihl+6]) == id
	}, timeout)
	if err != nil {
		return 0, err
	}
	return time.Since(start), nil
}

func (o *osSide) udpEcho(selfIP, targetAddr, payload string, timeout time.Duration) error {
	host, ps, err := net.SplitHostPort(targetAddr)
	if err != nil {
		return err
	}
	port, _ := strconv.Atoi(ps)
	srcPort := 40000 + int(time.Now().UnixNano()%20000)
	pkt := buildUDPPacket(selfIP, host, srcPort, port, []byte(payload))
	o.loop.SendFromOS(pkt)
	want := mustParseIP(host)
	_, err = o.awaitPacket(func(p []byte) bool {
		if len(p) < 28 || p[9] != 17 {
			return false
		}
		ihl := int(p[0]&0x0f) * 4
		if !(p[12] == want[0] && p[13] == want[1] && p[14] == want[2] && p[15] == want[3]) {
			return false
		}
		return string(p[ihl+8:]) == payload
	}, timeout)
	return err
}

func (o *osSide) sendBroadcast(selfIP, payload string) {
	pkt := buildUDPPacket(selfIP, "10.88.255.255", 40001, broadcastUDPPort, []byte(payload))
	o.loop.SendFromOS(pkt)
}

// --- Packet crafting ------------------------------------------------

func ipStr(b []byte) string { return fmt.Sprintf("%d.%d.%d.%d", b[0], b[1], b[2], b[3]) }

func isBroadcastIP(b []byte) bool { return b[0] == 10 && b[1] == 88 && b[2] == 255 && b[3] == 255 }

func checksum(data []byte) uint16 {
	var sum uint32
	for len(data) >= 2 {
		sum += uint32(binary.BigEndian.Uint16(data))
		data = data[2:]
	}
	if len(data) == 1 {
		sum += uint32(data[0]) << 8
	}
	for sum > 0xffff {
		sum = (sum & 0xffff) + (sum >> 16)
	}
	return ^uint16(sum)
}

func buildIPPacket(srcIP, dstIP string, proto byte, payload []byte) []byte {
	src := mustParseIP(srcIP)
	dst := mustParseIP(dstIP)
	pkt := make([]byte, 20+len(payload))
	pkt[0] = 0x45
	binary.BigEndian.PutUint16(pkt[2:4], uint16(len(pkt)))
	pkt[8] = 64 // TTL
	pkt[9] = proto
	copy(pkt[12:16], src[:])
	copy(pkt[16:20], dst[:])
	copy(pkt[20:], payload)
	binary.BigEndian.PutUint16(pkt[10:12], checksum(pkt[:20]))
	return pkt
}

func buildICMPRequest(srcIP, dstIP string, id, seq uint16) []byte {
	icmp := make([]byte, 16)
	icmp[0] = 8 // echo request
	binary.BigEndian.PutUint16(icmp[4:6], id)
	binary.BigEndian.PutUint16(icmp[6:8], seq)
	copy(icmp[8:], []byte("svlanping"))
	binary.BigEndian.PutUint16(icmp[2:4], checksum(icmp))
	return buildIPPacket(srcIP, dstIP, 1, icmp)
}

func buildICMPReply(req []byte) []byte {
	ihl := int(req[0]&0x0f) * 4
	icmp := make([]byte, len(req)-ihl)
	copy(icmp, req[ihl:])
	icmp[0] = 0 // echo reply
	icmp[2], icmp[3] = 0, 0
	binary.BigEndian.PutUint16(icmp[2:4], checksum(icmp))
	src := ipStr(req[12:16])
	dst := ipStr(req[16:20])
	return buildIPPacket(dst, src, 1, icmp)
}

func buildUDPPacket(srcIP, dstIP string, srcPort, dstPort int, payload []byte) []byte {
	udp := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint16(udp[0:2], uint16(srcPort))
	binary.BigEndian.PutUint16(udp[2:4], uint16(dstPort))
	binary.BigEndian.PutUint16(udp[4:6], uint16(len(udp)))
	copy(udp[8:], payload)
	return buildIPPacket(srcIP, dstIP, 17, udp)
}

func buildUDPReply(req []byte) []byte {
	ihl := int(req[0]&0x0f) * 4
	udp := req[ihl:]
	srcPort := binary.BigEndian.Uint16(udp[0:2])
	dstPort := binary.BigEndian.Uint16(udp[2:4])
	payload := udp[8:]
	src := ipStr(req[12:16])
	dst := ipStr(req[16:20])
	return buildUDPPacket(dst, src, int(dstPort), int(srcPort), payload)
}
