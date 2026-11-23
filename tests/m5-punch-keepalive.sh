#!/bin/bash
# M5 (LOCAL EXCHANGE LOGIC ONLY): the joiner starts three seconds
# BEFORE the host exists. Its repeated handshake sends are the
# hole-punch burst; when the host appears, the pair must connect
# without restarting anything. The pair then sits together long
# enough for the 5-second keepalive ping/pong to produce a real
# RTT reading in the member list.
#
# This proves the punch + keepalive EXCHANGE LOGIC on local
# addresses only. It does not prove real NAT traversal.
set -u
cd "$(dirname "$0")/.."
BIN=./dist/svlan
LOG=logs/m5
mkdir -p "$LOG"
PW="punch-password"
CODE="NNNNN-PPPPP"

"$BIN" -mode join -name early -password "$PW" -code "$CODE" \
  -port 40072 -no-dht -peer 127.0.0.1:40071 \
  -wait-members 1 -timeout 20s -hold 7s -members > "$LOG/early.log" 2>&1 &
J=$!
sleep 3
"$BIN" -mode host -name late -password "$PW" -code "$CODE" \
  -port 40071 -no-dht -echo -hold 20s > "$LOG/late.log" 2>&1 &
H=$!
wait $J
echo "early exit=$?"
kill $H 2>/dev/null; wait 2>/dev/null
grep "RESULT members-ok" "$LOG/early.log"
grep -o '"Name":"late"[^}]*' "$LOG/early.log" | head -1
