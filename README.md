# Serverless Virtual LAN — v1 prototype

A small tool that connects computers over the internet **as if they
were on the same local network**, so games with LAN multiplayer work
between people in different houses. There is **no server** — not for
traffic, not for lookups, not for accounts.

Design: [`design-doc-2026-11-23.md`](design-doc-2026-11-23.md) is the
governing spec. This README maps the code to it.

## How it works (the short version)

1. A host picks a room password and gets a **room code** (like
   `XSTV3-DBSA3` — readable aloud, no look-alike characters).
2. The host's copy announces "a room lives here" on the public
   **Mainline DHT** (the same serverless network BitTorrent uses),
   under a key that is the **hash of the code** — the code itself
   never travels.
3. A friend enters the code + password. Their copy looks the hash up,
   finds the host (and any members), and knocks.
4. Knocking is a handshake that **proves the password without sending
   it**: the password runs through Argon2id to make a room key, the
   handshake is HMAC-tagged with that key and mixes in an ephemeral
   X25519 exchange, and the resulting session carries traffic with
   ChaCha20-Poly1305. Wrong password → the knock is dropped silently
   and repeated attempts are throttled with backoff.
5. Inside the room it's a **mesh**: everyone holds an encrypted
   session to everyone else, members gossip the member list (the DHT
   is only needed at the door), and broadcast/multicast frames are
   copied to all members so in-game LAN browsers light up.
6. If two members can't reach each other directly, a mutual member
   **relays** their sealed traffic — the relay passes envelopes it
   cannot read. The member list honestly shows *direct* or *via
   relay*, plus ping.

Room-local addresses live in `10.88.0.0/16` and are derived
deterministically from each member's identity hash (collisions
re-hash with a counter), so every member computes the same address
book independently.

## Running it

### Graphical app (the front door)

```sh
./dist/svlan            # Linux
dist\svlan.exe          # Windows — keep wintun.dll beside it
```

`svlan.exe` is built as a Windows GUI application — double-clicking it
opens just the window, no terminal. (For headless/CLI use on Windows,
`dist\svlan-console.exe` is the same program built with a console, so
its printed room code and logs stay visible.)

One window: **Host a Room** (set a password, get the big code) or
**Join a Room** (code + password). The member list shows who is in,
how they're connected, and their ping. Plain words only.

On Windows, `wintun.dll` (the WireGuard project's virtual adapter
driver) must sit next to `svlan.exe`; it's included in `dist/` with
its license (`WINTUN-LICENSE.txt`). Creating the adapter needs
administrator rights — run the app as administrator.

### Command line (also the test harness)

```sh
# Host a room (prints ROOMCODE):
./dist/svlan -mode host -name alice -password 'room password'

# Join it:
./dist/svlan -mode join -name bob -password 'room password' -code XXXXX-XXXXX

# A bare DHT bootstrap node (for private swarms / local testing):
./dist/svlan -mode bootstrap -dht-port 42001
```

Headless flags for scripting: `-peer host:port` (skip lookup),
`-no-dht`, `-bootstrap host:port`, `-tun` (real adapter instead of
the loopback test adapter), `-echo`, `-wait-members N`, `-icmp IP`,
`-icmp-name NAME`, `-udp-echo IP:PORT`, `-broadcast TEXT`,
`-members`, `-hold DURATION`, `-block-direct addr` (simulate an
unpunchable pair to exercise relay). Scripted actions print
`RESULT ...` lines.

## Code map

| Path | What it is | Design doc |
|---|---|---|
| `cmd/svlan/` | Entry point: UI mode + headless host/join/bootstrap harness | — |
| `internal/room/` | Room codes, DHT key (hash of code), Argon2id room key, room IPs | §3, §6 |
| `internal/session/` | Password handshake (HELLO/REPLY), X25519 + AEAD session traffic | §6 |
| `internal/node/` | The room engine: mesh sessions, gossip, routing, relay, throttling, hole-punch retries, keepalive pings | §3–§5 |
| `internal/dhtx/` | Mainline DHT wrapper (anacrolix/dht) + a corrected peer store | §4.3, §8 |
| `internal/adapter/` | Virtual adapter: Linux TUN, Windows Wintun, userspace loopback for testing | §4.1 |
| `internal/ui/` | Gio front door — Host/Join, big code, member list | §7 |
| `tests/` | Milestone verification rigs (shell) | — |
| `logs/` | Verification run logs | — |
| `screenshots/` | UI verification captures | — |

Two implementation notes worth knowing:

- **Transport**: the tunnel uses one listening UDP socket plus one
  *connected* UDP socket per peer, bound to the same local port with
  SO_REUSEADDR/SO_REUSEPORT. (This VM's sandbox blocks unconnected
  UDP sends outright; the design is also what makes the hole-punch
  retries leave from the exact socket peers reply to.)
- **Peer store**: anacrolix/dht v2.24.0's bundled in-memory peer
  store is broken (keys peers by IP only; `GetPeers` unmarshals the
  key wrongly, so `get_peers` never returns values). `internal/dhtx`
  ships its own correct store — see `peerstore.go`.

## Verification

Every milestone was verified with real runs on this machine (logs in
`logs/`): two-node tunnel traffic (ICMP + UDP echo + broadcast),
DHT announce→lookup→connect against a local bootstrap node,
wrong-password rejection + throttling, a 3-node gossip mesh with
broadcast, relay fallback for a blocked pair with ICMP across the
relay, and punch/keepalive exchange against a late-starting peer.
The UI was launched under Xvfb and driven end-to-end (screenshots in
`screenshots/`). Rerun any of it:

```sh
go test ./...
tests/m1-manual-mesh.sh   # two nodes, manual addresses
tests/m2-dht-room.sh      # DHT room via local bootstrap
tests/m3-password-gate.sh # wrong password + throttling + TUN probe
tests/m4-mesh-relay.sh    # 3-node gossip mesh + relay
tests/m5-punch-keepalive.sh # punch retries + keepalive (local only)
```

## Honest limits of this prototype

- **No real TUN in the verification VM** (no CAP_NET_ADMIN): the
  full stack is runtime-verified over a userspace loopback adapter;
  the real Linux TUN and Windows Wintun bindings are compile-verified
  and attempted at runtime (the tool reports the failure plainly and
  falls back, labelling itself "test mode").
- **Public DHT reachability is unproven from here** — this VM's
  internet goes through an HTTP proxy, so DHT verification used a
  local bootstrap node. The standard public bootstrap list
  (`router.bittorrent.com`, `router.utorrent.com`,
  `dht.transmissionbt.com`, `dht.libtorrent.org`) ships in config.
- **Real NAT hole punching is unproven** — only the punch/keepalive
  exchange logic on local addresses is verified. Symmetric-NAT pairs
  are expected to land on the relay path, which *is* verified.
- **Windows runtime is unproven** — `svlan.exe` is cross-compiled
  and `wintun.dll` is bundled, but nothing Windows-side has run yet.
- Session packet acceptance is strictly in-order (reordered UDP
  packets drop rather than buffer) — fine for a v1 prototype,
  worth a replay window in v2.
- Mesh comfort ceiling per the design doc: ~12–16 players.
