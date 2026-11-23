#!/bin/bash
# M3: password gate. A joiner with the WRONG password must fail the
# handshake and be dropped; repeated failures must trip throttling;
# a joiner with the right password joins normally.
# Also checks the -tun path degrades honestly in this VM.
set -u
cd "$(dirname "$0")/.."
BIN=./dist/svlan
LOG=logs/m3
mkdir -p "$LOG"
CODE="AAAAA-BBBBB"

# TUN attempt (M0 evidence): expect tun-unavailable + fallback.
"$BIN" -mode host -name tunprobe -password pw -code "$CODE" \
  -port 40011 -no-dht -tun -hold 2s > "$LOG/tunprobe.log" 2>&1
grep -o "RESULT tun-unavailable[^ ]* [^ ]*" "$LOG/tunprobe.log" | head -1

"$BIN" -mode host -name alice -password right-password -code "$CODE" \
  -port 40012 -no-dht -echo -hold 35s > "$LOG/alice.log" 2>&1 &
APID=$!
sleep 2

# Wrong password: three rapid attempts -> rejected + throttled.
for i in 1 2 3; do
  "$BIN" -mode join -name mallory -password wrong-password -code "$CODE" \
    -port $((40020+i)) -no-dht -peer 127.0.0.1:40012 \
    -wait-members 1 -timeout 6s > "$LOG/mallory$i.log" 2>&1
  echo "mallory attempt $i exit=$?"
done

# Right password joins.
"$BIN" -mode join -name bob -password right-password -code "$CODE" \
  -port 40025 -no-dht -peer 127.0.0.1:40012 -echo \
  -wait-members 1 -timeout 15s -members > "$LOG/bob.log" 2>&1
echo "bob exit=$?"
kill $APID 2>/dev/null; wait $APID 2>/dev/null

echo "alice rejections: $(grep -c 'handshake rejected' "$LOG/alice.log")"
echo "alice throttle lines: $(grep -c 'throttle:' "$LOG/alice.log")"
grep "RESULT members-ok" "$LOG/bob.log" | head -1
