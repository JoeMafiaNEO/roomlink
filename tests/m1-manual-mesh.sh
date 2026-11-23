#!/bin/bash
# M1: two instances, manual peer address, no DHT.
# Proves: handshake + encrypted tunnel carries ICMP echo, UDP echo,
# and a broadcast frame between two virtual adapters.
set -u
cd "$(dirname "$0")/.."
BIN=./dist/svlan
LOG=logs/m1
mkdir -p "$LOG"
CODE="AAAAA-BBBBB"
PW="test-password-1"

"$BIN" -mode host -name alice -password "$PW" -code "$CODE" \
  -port 40001 -no-dht -echo -hold 40s > "$LOG/alice.log" 2>&1 &
APID=$!
sleep 2
AIP=$(grep -o 'ip=[0-9.]*' "$LOG/alice.log" | head -1 | cut -d= -f2)
echo "alice ip: $AIP"

"$BIN" -mode join -name bob -password "$PW" -code "$CODE" \
  -port 40002 -no-dht -peer 127.0.0.1:40001 -echo \
  -wait-members 1 -timeout 20s \
  -icmp "$AIP" -timeout 15s \
  -udp-echo "$AIP:4242" -payload "hello-room" -timeout 15s \
  -broadcast "bcast-from-bob" \
  -members > "$LOG/bob.log" 2>&1
BEXIT=$?
sleep 1
kill $APID 2>/dev/null; wait $APID 2>/dev/null

echo "--- bob exit: $BEXIT"
grep -c "RESULT icmp-ok" "$LOG/bob.log" | xargs echo "icmp-ok:"
grep -c "RESULT udp-echo-ok" "$LOG/bob.log" | xargs echo "udp-echo-ok:"
grep -c "RESULT members-ok" "$LOG/bob.log" | xargs echo "members-ok:"
grep -c "RESULT broadcast-received" "$LOG/alice.log" | xargs echo "broadcast-received-by-alice:"
