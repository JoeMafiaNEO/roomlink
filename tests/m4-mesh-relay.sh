#!/bin/bash
# M4a: 3-node mesh via gossip. Carol is only ever told Bob's
# address; she must learn Alice from Bob's gossip and connect to
# her directly. A broadcast from Carol must reach both others.
# M4b: relay. Erin and Frank both block each other's addresses
# (simulated unpunchable pair); they must end up connected via
# Dave as relay, and ICMP must cross the relay path.
set -u
cd "$(dirname "$0")/.."
BIN=./dist/svlan
LOG=logs/m4
mkdir -p "$LOG"
PW="mesh-password"

echo "=== M4a gossip mesh ==="
CODE="EEEEE-FFFFF"
"$BIN" -mode host -name alice -password "$PW" -code "$CODE" \
  -port 40041 -no-dht -echo -hold 30s > "$LOG/a-alice.log" 2>&1 &
A=$!
"$BIN" -mode join -name bob -password "$PW" -code "$CODE" \
  -port 40042 -no-dht -peer 127.0.0.1:40041 -echo -hold 30s > "$LOG/a-bob.log" 2>&1 &
B=$!
sleep 3
"$BIN" -mode join -name carol -password "$PW" -code "$CODE" \
  -port 40043 -no-dht -peer 127.0.0.1:40042 -echo \
  -wait-members 2 -timeout 25s -broadcast "bcast-from-carol" -members -hold 2s \
  > "$LOG/a-carol.log" 2>&1
echo "carol exit=$?"
sleep 1
kill $A $B 2>/dev/null; wait 2>/dev/null
grep "RESULT members-ok" "$LOG/a-carol.log"
echo "broadcast at alice: $(grep -c 'broadcast-received' "$LOG/a-alice.log")  at bob: $(grep -c 'broadcast-received' "$LOG/a-bob.log")"
grep -o '"Name":"alice"[^}]*' "$LOG/a-carol.log" | head -1

echo "=== M4b relay ==="
CODE="GGGGG-HHHHH"
"$BIN" -mode host -name dave -password "$PW" -code "$CODE" \
  -port 40051 -no-dht -echo -hold 120s > "$LOG/b-dave.log" 2>&1 &
D=$!
"$BIN" -mode join -name erin -password "$PW" -code "$CODE" \
  -port 40052 -no-dht -peer 127.0.0.1:40051 -block-direct 127.0.0.1:40053 \
  -echo -hold 120s > "$LOG/b-erin.log" 2>&1 &
E=$!
sleep 3
"$BIN" -mode join -name frank -password "$PW" -code "$CODE" \
  -port 40053 -no-dht -peer 127.0.0.1:40051 -block-direct 127.0.0.1:40052 \
  -echo -wait-members 2 -timeout 90s -icmp-name erin -members \
  > "$LOG/b-frank.log" 2>&1
echo "frank exit=$?"
kill $D $E 2>/dev/null; wait 2>/dev/null
grep "RESULT members-ok" "$LOG/b-frank.log"
grep "RESULT icmp-ok" "$LOG/b-frank.log"
grep -o '"Name":"erin"[^}]*' "$LOG/b-frank.log" | head -1
echo "relay fallback lines:"; grep -h "falling back to relay" "$LOG/b-frank.log" "$LOG/b-erin.log" | head -2
