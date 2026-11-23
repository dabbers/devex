#!/usr/bin/env bash
# Check the egress policy does what it claims, on the machine it is loaded on.
#
# Read-only apart from three short-lived listeners on loopback. Run as root:
#   sudo bash deploy/nftables/verify-egress.sh
#
# Every line reports what happened, and the summary says whether the result
# matches the policy. A FAIL here means the rules are not doing their job --
# investigate before trusting them.

set -uo pipefail

RUN_AS="${1:-dabberz}"
pass=0; fail=0

if ! id "$RUN_AS" >/dev/null 2>&1; then
  echo "No such account: $RUN_AS -- pass the instance account as the first argument." >&2
  exit 2
fi
if ! nft list table inet dabberz_egress >/dev/null 2>&1; then
  echo "Table inet dabberz_egress is not loaded. Nothing to verify." >&2
  exit 2
fi

probe=$(mktemp /tmp/probe.XXXXXX.py)
cat > "$probe" <<'PY'
import socket, sys, time
host, port = sys.argv[1], int(sys.argv[2])
s = socket.socket(); s.settimeout(5)
t = time.time()
try:
    s.connect((host, port)); print("ALLOWED %d" % ((time.time()-t)*1000)); s.close()
except Exception as e:
    print("BLOCKED %d %s" % ((time.time()-t)*1000, type(e).__name__))
PY
chmod 644 "$probe"

# Short-lived stand-ins so a blocked result means the policy blocked it,
# rather than nothing having been listening in the first place.
listeners=()
for p in 41005 5432; do
  python3 -c "
import socket,sys
s=socket.socket(); s.setsockopt(socket.SOL_SOCKET,socket.SO_REUSEADDR,1)
try: s.bind(('127.0.0.1',$p)); s.listen(8)
except OSError: sys.exit(0)
import time; time.sleep(60)
" & listeners+=($!)
done
sleep 1

check() { # description host port expected
  local desc="$1" host="$2" port="$3" want="$4"
  local out got ms
  out=$(setpriv --reuid "$RUN_AS" --regid "$(id -g "$RUN_AS")" --clear-groups python3 "$probe" "$host" "$port" 2>/dev/null)
  got=${out%% *}; ms=$(echo "$out" | awk '{print $2}')
  if [ "$got" = "$want" ]; then
    pass=$((pass+1)); printf '  ok   %-44s %s (%sms)\n' "$desc" "$got" "$ms"
  else
    fail=$((fail+1)); printf '  FAIL %-44s %s, wanted %s\n' "$desc" "$got" "$want"
  fi
  # A block that takes the full timeout is a hang, not a refusal: a build
  # that trips it stalls instead of erroring. Worth knowing about.
  if [ "$got" = "BLOCKED" ] && [ "${ms:-0}" -ge 4500 ]; then
    printf '       note: timed out rather than refused -- see README\n'
  fi
}

echo "Egress policy, as $RUN_AS:"
check "a neighbour's preview port (41005)"  127.0.0.1       41005 BLOCKED
check "the control plane (:80)"             127.0.0.1       80    BLOCKED
check "its own database (:5432)"            127.0.0.1       5432  ALLOWED
check "SMTP relay (1.1.1.1:25)"             1.1.1.1         25    BLOCKED
check "private network (192.168.1.1:80)"    192.168.1.1     80    BLOCKED
check "link-local / metadata"               169.254.169.254 80    BLOCKED
check "public HTTPS (1.1.1.1:443)"          1.1.1.1         443   ALLOWED
check "an unusual port (1.1.1.1:4444)"      1.1.1.1         4444  BLOCKED

echo
echo "As root (the daemon must be unaffected):"
out=$(python3 "$probe" 127.0.0.1 41005); got=${out%% *}
if [ "$got" = "ALLOWED" ]; then
  pass=$((pass+1)); printf '  ok   %-44s %s\n' "reaches a preview port" "$got"
else
  fail=$((fail+1)); printf '  FAIL %-44s %s, wanted ALLOWED\n' "reaches a preview port" "$got"
  echo "       The daemon is being filtered. It routes every preview; this breaks the platform."
fi

for pid in "${listeners[@]}"; do kill "$pid" 2>/dev/null; done
rm -f "$probe"
echo
echo "$pass passed, $fail failed"
[ "$fail" -eq 0 ] || exit 1
