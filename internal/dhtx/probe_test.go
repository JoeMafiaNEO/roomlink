package dhtx

import (
	"testing"
	"time"

	"svlan/internal/room"
)

// TestProbeLocalSwarm: bootstrap + two services; both announce the
// same room key; each must discover the other's announced port.
func TestProbeLocalSwarm(t *testing.T) {
	boot, err := StartLocal(43001)
	if err != nil {
		t.Fatal(err)
	}
	defer boot.Close()
	t.Logf("bootstrap at %s", boot.Addr())

	a, err := Start(43011, []string{"127.0.0.1:43001"})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := Start(43012, []string{"127.0.0.1:43001"})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	key := room.DHTKey("CCCCC-DDDDD")
	foundA := make(chan string, 10)
	foundB := make(chan string, 10)
	logf := func(f string, args ...any) { t.Logf(f, args...) }
	go a.AnnounceLoop(key, 40031, 3*time.Second, logf, func(addr string) { foundA <- addr })
	go b.AnnounceLoop(key, 40032, 3*time.Second, logf, func(addr string) { foundB <- addr })

	deadline := time.After(30 * time.Second)
	var gotA, gotB bool
loop:
	for !(gotA && gotB) {
		select {
		case addr := <-foundA:
			t.Logf("A discovered %s", addr)
			if addr == "127.0.0.1:40032" {
				gotA = true
			}
		case addr := <-foundB:
			t.Logf("B discovered %s", addr)
			if addr == "127.0.0.1:40031" {
				gotB = true
			}
		case <-deadline:
			t.Logf("bootstrap peer store: %s", boot.PeerStoreDump())
			t.Logf("A peer store: %s", a.PeerStoreDump())
			t.Fatalf("timeout: gotA=%v gotB=%v", gotA, gotB)
		case <-time.After(10 * time.Second):
			t.Logf("bootstrap peer store @10s: %s", boot.PeerStoreDump())
			break loop
		}
	}
	if !(gotA && gotB) {
		t.Logf("bootstrap peer store: %s", boot.PeerStoreDump())
		t.Fatalf("incomplete: gotA=%v gotB=%v", gotA, gotB)
	}
}
