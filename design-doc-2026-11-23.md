# Serverless Virtual LAN — Design Document

*Working title — no name chosen yet. Drafted 2026-11-23 from Jesse's design rulings.*

## 1. What it is

A small desktop tool that connects two or more computers over the internet **as if they were on the same local network**, so games with LAN multiplayer work between people in different houses. It runs **no server** — not for traffic, not for lookups, not for accounts. A host creates a room and gets a **code**; friends join with the code and a **room password**; from then on, every machine in the room sees the others as local network neighbors.

## 2. Hard constraints (the design's identity)

- **No server, anywhere.** No machine of ours carries traffic, stores rooms, or resolves codes. If our infrastructure vanished, existing copies of the tool keep working.
- **Direct peer-to-peer traffic.** Game data flows straight between players' machines, encrypted. The only exception is a player relaying for another player (see §5) — still no server.
- **The game never knows.** To the game, this is a normal LAN. No game-side configuration, no mods, no port forwarding.

## 3. How a session works

**Hosting**
1. The host clicks *Host a Room* and sets a room password.
2. The tool generates a room code, hashes it into a lookup key, and announces the room on the public DHT network under that key (re-announcing periodically while the room lives).
3. The host screen shows the code, large, and an empty member list.

**Joining**
1. The joiner clicks *Join a Room* and enters the code and the password.
2. The tool hashes the code the same way and looks the key up on the DHT — a few seconds of asking around returns the host's (and any current members') contact info.
3. The joiner hole-punches a direct connection to each member and completes the password handshake with them. Wrong password: the handshake fails, the connection is dropped, nothing is revealed.
4. Once in, members exchange the member list with each other directly. The DHT is only needed at the door; a room in progress no longer depends on it.

**Playing**
- The tool's virtual network adapter presents the room as a LAN. Games find each other and connect exactly as they would in the same house — including LAN server browsers, because broadcast/multicast discovery traffic is deliberately forwarded to every member (see §4).
- The member list shows each player with a plain status: *direct* or *via relay*, plus ping.

**Leaving / closing**
- A member leaving is announced to the mesh; their routes simply disappear. If the host leaves, the room persists among the remaining members (mesh — no single point of failure); the DHT announcement lapses, so no *new* joiners can find the room by code unless a remaining member re-announces it.

## 4. Architecture

Five pieces, each replaceable:

1. **Virtual network adapter.** A TUN/TAP-style driver (on Windows: Wintun, the WireGuard project's driver) gives the room its own network interface with room-local IP addresses. Games bind to it like any LAN adapter.
2. **Tunnel.** Every frame the game puts on the virtual adapter is encrypted and wrapped in a UDP packet addressed to the owning peer; on arrival it's decrypted and delivered to that machine's adapter. Encryption keys derive from the room password plus per-session ephemeral keys, so sessions can't be replayed into each other.
3. **Serverless lookup (DHT).** A Kademlia DHT (the design BitTorrent's Mainline DHT has run at global scale for ~20 years) keyed by the hash of the room code. Stores only "the room at this key can be knocked on, here is contact info" — never traffic, never passwords, never member data beyond what's needed to knock.
4. **NAT traversal.** UDP hole punching per pair of peers, using contact info from the DHT record and from each other. Each pair tries to open a direct path; the pair's state (direct / relayed) shows in the member list.
5. **Mesh fabric + broadcast forwarding.** Every member holds a connection to every other member. Broadcast and multicast frames from the adapter are copied to all members — this is what makes in-game LAN browsers light up, and it's a deliberate feature, not a side effect. Membership changes propagate peer-to-peer.

## 5. When a direct connection won't open (peer relay)

Some router pairs (symmetric NAT, carrier-grade NAT) cannot be hole-punched. In a mesh there is a serverless answer: if player C can reach both A and B, but A and B can't reach each other, C **relays** A↔B traffic (encrypted end-to-end between A and B — C passes sealed envelopes it cannot read). The member list marks such connections *via relay* with the honest ping cost. A pair with no common reachable peer at all cannot connect; the UI says so in plain words rather than spinning forever.

## 6. Security model

- **Code = locator, password = key.** The code is effectively public: anyone who knows or guesses it can find that a room exists and knock. The password is the actual gate.
- **The password is never sent.** It proves itself inside the join handshake (challenge–response / PAKE-style), and keys the room's encryption. A wrong password yields nothing — not even confirmation of what went wrong.
- **Guessing resistance.** Password keys are derived with a slow KDF (Argon2/scrypt class), and each member throttles repeated failed joins with backoff. A findable room plus a modest password still holds up, because every guess is slow and rate-limited.
- **No accounts, no telemetry.** There is no server to hold them. Identity inside a room is a display name the user picks locally.

## 7. The UI (clean is a requirement, not a polish step)

One small window. No jargon on the surface — the words DHT, NAT, and adapter never appear.

- **Front door:** two buttons — *Host a Room*, *Join a Room*.
- **Host view:** password field → *Start Hosting* → the room code displayed large enough to read aloud over voice chat; member list below as people arrive (name, direct/via relay, ping); *Close Room*.
- **Join view:** code field, password field, *Join*. Success drops you into the same member-list view; failure is one plain sentence ("That code doesn't match a live room", "Wrong room password", "Couldn't reach the room — a router is blocking direct connections").
- **Everything fits on the one screen.** No scrolling, no settings maze. Advanced detail, if it ever exists, lives behind a single "Details" disclosure and is never required to play.

## 8. Honest limits

- **The first knock needs the commons.** Joining the DHT requires contacting a public bootstrap node once. These are long-running, independently operated public addresses shipped as a list in the tool — not our server, and no single one is a dependency — but "zero infrastructure touched, ever" is not a claim this design makes.
- **Mesh has a ceiling.** Every member connects to every member, so comfort ends around **12–16 players**: more pairs to punch, and broadcast copies multiply upload use. That's well above how LAN gaming groups actually run (2–8), and it's the accepted price of no-server.
- **Lookup takes seconds.** A DHT lookup is a few seconds of asking around. Fine for joining a lobby; the design never puts the DHT in the play path.
- **Host-grade connections matter.** A relayed player rides on a fellow player's upload. The UI shows it honestly instead of hiding it.

## 9. Decisions

**Locked (Jesse, 2026-11-23)**
- No server — traffic, lookup, and accounts all serverless.
- Room code for joining; plenty of players per room.
- Serverless (DHT) lookup for the code.
- Mesh topology; peer relay as the fallback for unpunchable pairs.
- Room password on top of the code.
- Clean, single-window, jargon-free UI.

**Still open**
- Who it's for first: Jesse's own circle, or a public product (affects how paranoid the code/password defaults need to be).
- Platform order (Windows first is the natural assumption — unconfirmed).
- A name.

## 10. What building it looks like (sketch, not a spec)

Proven parts exist for every hard piece: Wintun for the adapter, the Mainline DHT for lookup, WireGuard-class cryptography for the tunnel. The original work is the glue — the room protocol (announce, knock, handshake, member exchange, relay) and the UI. A two-machine prototype (host + joiner playing one LAN game) is the first milestone; the mesh, relay, and polish follow it.
