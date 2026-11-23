#!/bin/bash
# M2: DHT room flow with a LOCAL bootstrap node (the public DHT is
# unreachable through this VM's egress proxy — the shipped config
# carries the standard public bootstrap list for real-world use).
# Host announces under hash(code); joiner looks up, finds the host,
# connects — no manual peer address anywhere.
set -u
cd "$(dirname "$0")/.."
BIN=./dist/svlan
LOG=logs/m2
mkdir -p "$LOG"
CODE="CCCCC-DDDDD"
PW="dht-password"

"$BIN" -mode bootstrap -name boot -dht-port 42001 > "$LOG/boot.log" 2>&1 &
BPID=$!
sleep 1

"$BIN" -mode host -name alice -password "$PW" -code "$CODE" \
  -port 40031 -dht-port 42011 -bootstrap 127.0.0.1:42001 \
  -announce-interval 5s -echo -hold 45s > "$LOG/alice.log" 2>&1 &
APID=$!
sleep 2
AIP=$(grep -o 'ip=[0-9.]*' "$LOG/alice.log" | head -1 | cut -d= -f2)
echo "alice ip: $AIP"

"$BIN" -mode join -name bob -password "$PW" -code "$CODE" \
  -port 40032 -dht-port 42012 -bootstrap 127.0.0.1:42001 \
  -announce-interval 5s -echo \
  -wait-members 1 -timeout 40s \
  -icmp "$AIP" -timeout 15s \
  -members > "$LOG/bob.log" 2>&1
BEXIT=$?
kill $APID $BPID 2>/dev/null; wait 2>/dev/null

echo "bob exit: $BEXIT"
echo "dht-discovered lines: $(grep -c 'dht discovered peer' "$LOG/bob.log" "$LOG/alice.log" | paste -sd+ )"
grep "RESULT members-ok" "$LOG/bob.log"
grep "RESULT icmp-ok" "$LOG/bob.log"
