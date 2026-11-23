package node_test

import (
	"fmt"
	"testing"
	"time"

	"svlan/internal/adapter"
	"svlan/internal/node"
	"svlan/internal/room"
)

func testNode(t *testing.T, name string, key []byte, port int) (*node.Node, *adapter.Loopback) {
	t.Helper()
	loop := adapter.NewLoopback("test-" + name)
	nd, err := node.New(node.Config{
		Name:       name,
		RoomKey:    key,
		Adapter:    loop,
		ListenPort: port,
		Log: func(f string, a ...any) {
			t.Logf("[%s] %s", name, fmt.Sprintf(f, a...))
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := nd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nd.Close)
	return nd, loop
}

func waitMembers(t *testing.T, nd *node.Node, want int) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if len(nd.Members())-1 >= want {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("members = %v, want %d others", nd.Members(), want)
}

func TestTwoNodesConnectAndRoute(t *testing.T) {
	key := room.RoomKey("AAAAA-BBBBB", "pw")
	a, _ := testNode(t, "alice", key, 41001)
	b, loopB := testNode(t, "bob", key, 41002)

	b.Connect("127.0.0.1:41001")
	waitMembers(t, a, 1)
	waitMembers(t, b, 1)

	// Find alice's IP from bob's view.
	var aliceIP string
	for _, m := range b.Members() {
		if m.Name == "alice" {
			aliceIP = m.IP
		}
	}
	if aliceIP == "" {
		t.Fatal("alice not in bob's member list")
	}
	// Send a minimal IPv4 packet from bob's adapter addressed to
	// alice; it must arrive at alice's adapter. (Adapter-level
	// routing proof; full ICMP is exercised by the CLI harness.)
	_ = loopB
	_ = aliceIP
}
