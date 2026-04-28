#!/usr/bin/env bash
#
# Post-deploy stress validation for a nexus-proxy instance.
#
# Drives a real backend (registered with nexus via nexus-proxy-client) at
# concurrent / sustained / slow-loris loads against the public nexus URL
# and reports pass/fail on operator-tunable thresholds. Output directory is
# preserved on both pass and fail for post-mortem review.
#
# Prerequisites:
#   - A backend is connected to the nexus instance and serves the target
#     hostname (use scripts/stress/backend + nexus-proxy-client; see README).
#   - DNS or /etc/hosts resolves the target hostname to the nexus instance.
#   - Tools on PATH: hey, vegeta, slowhttptest, jq, bc.
#
# Usage:
#   scripts/stress/run.sh https://stress.example.com/
#
# Tunables (env vars):
#   INSECURE             curl/load-tester insecure flag (default: -k for self-signed backend)
#   CONCURRENT_WORKERS   hey worker count for test 1   (default: 4096)
#   CONCURRENT_DURATION  hey duration for test 1       (default: 30s)
#   SUSTAINED_RATE       vegeta rate for test 2        (default: 1000)
#   SUSTAINED_DURATION   vegeta duration for test 2    (default: 60s)
#   LORIS_CONNS          slowhttptest conn count       (default: 1000)
#   LORIS_DURATION       slowhttptest duration         (default: 60)
#   THRESH_ERR_PCT       max acceptable error rate %   (default: 1.0)
#   THRESH_SUCCESS_PCT   min acceptable success rate % (default: 99.5)
#   THRESH_P99_MS        max acceptable p99 latency ms (default: 500)
#   THRESH_LORIS_CTRL    min control success during loris % (default: 95)

set -euo pipefail

# Lock numeric locale so bash printf '%.2f' on jq's '.'-decimal output and
# any awk float formatting produce stable text regardless of operator locale.
export LC_ALL=C

TARGET="${1:?usage: $0 <https://hostname/>}"
INSECURE="${INSECURE:--k}"
CONCURRENT_WORKERS="${CONCURRENT_WORKERS:-4096}"
CONCURRENT_DURATION="${CONCURRENT_DURATION:-30s}"
SUSTAINED_RATE="${SUSTAINED_RATE:-1000}"
SUSTAINED_DURATION="${SUSTAINED_DURATION:-60s}"
LORIS_CONNS="${LORIS_CONNS:-1000}"
LORIS_DURATION="${LORIS_DURATION:-60}"
LORIS_CONTROL_RATE="${LORIS_CONTROL_RATE:-100}"
LORIS_CONTROL_DURATION="${LORIS_CONTROL_DURATION:-30s}"
THRESH_ERR_PCT="${THRESH_ERR_PCT:-1.0}"
THRESH_SUCCESS_PCT="${THRESH_SUCCESS_PCT:-99.5}"
THRESH_P99_MS="${THRESH_P99_MS:-500}"
THRESH_LORIS_CTRL="${THRESH_LORIS_CTRL:-95}"

RESULTS="$(mktemp -d -t nexus-stress-XXXXXX)"
PASS=true

red()    { printf '\033[31m%s\033[0m\n' "$*"; }
green()  { printf '\033[32m%s\033[0m\n' "$*"; }
yellow() { printf '\033[33m%s\033[0m\n' "$*"; }
hdr()    { printf '\n\033[1m%s\033[0m\n' "$*"; }

require() {
  for tool in "$@"; do
    command -v "$tool" >/dev/null 2>&1 || { red "missing tool: $tool"; exit 2; }
  done
}

# slowhttptest is optional — test 3 is skipped if it's missing.
require hey vegeta jq bc curl
HAVE_SLOWHTTPTEST=true
command -v slowhttptest >/dev/null 2>&1 || HAVE_SLOWHTTPTEST=false

hdr "==> Target:  $TARGET"
echo  "    Results: $RESULTS"
echo  "    Tools:   hey $(hey --version 2>/dev/null || true) | vegeta $(vegeta -version 2>/dev/null || true)"

# Sanity probe: fail fast with exit 3 (infra failure, distinct from threshold-
# fail exit 1) if the backend isn't reachable through nexus.
# INSECURE is unquoted intentionally so multi-flag overrides ("-k --tlsv1.3")
# word-split correctly. Note: INSECURE only affects this probe — vegeta and
# slowhttptest hardcode their own insecure flags (-insecure / -k 1).
if ! curl --silent --fail --max-time 5 $INSECURE -o /dev/null "$TARGET"; then
  red "Sanity probe failed; is the backend reachable through nexus?"
  exit 3
fi
green "Sanity probe ok"

# ----------------------------------------------------------------------
# Test 1 — Concurrent baseline
#   What it validates: the listener handles ${CONCURRENT_WORKERS} simultaneous
#   in-flight conns. With slot release at peek-complete, established sessions
#   no longer count against MaxConcurrentPeeks, so a 4k worker pool should
#   flow cleanly.
# ----------------------------------------------------------------------
hdr "[1/3] Concurrent baseline (-c $CONCURRENT_WORKERS, -z $CONCURRENT_DURATION)"
hey -c "$CONCURRENT_WORKERS" -z "$CONCURRENT_DURATION" \
    -disable-keepalive -disable-redirects \
    "$TARGET" > "$RESULTS/concurrent.txt" 2>&1 || true

grep -E "Summary:|Total:|Slowest:|Fastest:|Average:|Requests/sec:|Status code distribution:|^[[:space:]]+\[" \
  "$RESULTS/concurrent.txt" | sed 's/^/    /'

# Compute error rate as (non-2xx responses + network errors) / total attempts.
# hey output format:
#   Status code distribution:
#     [200]\t526 responses           ← count is $2, status code is bracketed $1
#   Error distribution:
#     [6478]\tGet "...": EOF         ← count is bracketed $1, $2+ is the message
# Without counting Error distribution, transport-level failures (RST,
# timeout, port exhaustion) would silently false-pass the test.
HEY_RESPS=$(awk '
  /Status code distribution:/ { in_sc=1; next }
  in_sc && /^[[:space:]]*$/ { in_sc=0 }
  in_sc && /^[[:space:]]*\[/ { sum += $2 }
  END { print sum+0 }
' "$RESULTS/concurrent.txt")
NON_2XX=$(awk '
  /Status code distribution:/ { in_sc=1; next }
  in_sc && /^[[:space:]]*$/ { in_sc=0 }
  in_sc && /^[[:space:]]*\[/ {
    code=$1; gsub(/[\[\]]/, "", code)
    if (code+0 < 200 || code+0 >= 300) sum += $2
  }
  END { print sum+0 }
' "$RESULTS/concurrent.txt")
NET_ERRS=$(awk '
  /Error distribution:/ { in_ed=1; next }
  in_ed && /^[[:space:]]*$/ { in_ed=0 }
  in_ed && /^[[:space:]]*\[/ {
    cnt=$1; gsub(/[\[\]]/, "", cnt)
    sum += (cnt+0)
  }
  END { print sum+0 }
' "$RESULTS/concurrent.txt")
TOTAL_REQS=$((HEY_RESPS + NET_ERRS))
ERR_COUNT=$((NON_2XX + NET_ERRS))
ERR_PCT=$(awk -v n="$ERR_COUNT" -v t="$TOTAL_REQS" 'BEGIN { if (t==0) print 100; else printf "%.2f", (n*100.0)/t }')
echo "    requests=$TOTAL_REQS errors=$ERR_COUNT (net=$NET_ERRS, non2xx=$NON_2XX)"
echo "    error_rate = ${ERR_PCT}% (threshold: <= ${THRESH_ERR_PCT}%)"
if (( $(echo "$ERR_PCT > $THRESH_ERR_PCT" | bc -l) )); then
  red "    FAIL: error rate above threshold"
  PASS=false
else
  green "    PASS"
fi

# ----------------------------------------------------------------------
# Test 2 — Sustained rate (connect/disconnect churn)
#   What it validates: nexus handles ${SUSTAINED_RATE} new TCP conns per
#   second for ${SUSTAINED_DURATION}, with low latency tail.
# ----------------------------------------------------------------------
hdr "[2/3] Sustained rate (rate=$SUSTAINED_RATE, duration=$SUSTAINED_DURATION)"
echo "GET $TARGET" | vegeta attack \
    -insecure -keepalive=false -http2=false \
    -rate "$SUSTAINED_RATE" -duration "$SUSTAINED_DURATION" \
    -timeout 10s > "$RESULTS/sustained.bin" 2>/dev/null
vegeta report < "$RESULTS/sustained.bin" | sed 's/^/    /' | tee "$RESULTS/sustained.txt" >/dev/null
sed 's/^/    /' "$RESULTS/sustained.txt"

VEG_JSON=$(vegeta report -type json < "$RESULTS/sustained.bin")
SUCCESS_PCT=$(echo "$VEG_JSON" | jq '.success * 100')
P99_MS=$(echo "$VEG_JSON" | jq '.latencies."99th" / 1e6')
echo "    success_rate = $(printf '%.2f' "$SUCCESS_PCT")% (threshold: >= ${THRESH_SUCCESS_PCT}%)"
echo "    p99_latency  = $(printf '%.1f' "$P99_MS")ms (threshold: <= ${THRESH_P99_MS}ms)"

if (( $(echo "$SUCCESS_PCT < $THRESH_SUCCESS_PCT" | bc -l) )); then
  red "    FAIL: success rate below threshold"
  PASS=false
elif (( $(echo "$P99_MS > $THRESH_P99_MS" | bc -l) )); then
  red "    FAIL: p99 latency above threshold"
  PASS=false
else
  green "    PASS"
fi

# ----------------------------------------------------------------------
# Test 3 — Slow-loris (post-handshake) with concurrent control traffic
#   What it validates: under sustained slow-header pressure on the HTTP
#   request side, legitimate users can still complete requests. NOTE:
#   slowhttptest -H trickles bytes AFTER the TLS ClientHello completes,
#   so on an HTTPS target this exercises post-peek behavior — not the
#   MaxConcurrentPeeks bound itself. To stress the peek bound directly,
#   use an HTTP/80 target with a mapped backend, or a tool that drips
#   the TLS ClientHello.
# ----------------------------------------------------------------------
hdr "[3/3] Slow-loris ($LORIS_CONNS conns) + control traffic"

if ! $HAVE_SLOWHTTPTEST; then
  yellow "    SKIP: slowhttptest not on PATH (install with apt/brew to enable)"
else

# Start slow-loris in background.
slowhttptest -c "$LORIS_CONNS" -H -i 10 -r 200 -t GET -u "$TARGET" \
    -l "$LORIS_DURATION" -k 1 > "$RESULTS/loris.txt" 2>&1 &
LORIS_PID=$!

# Give loris time to ramp up and saturate the bound.
sleep 5

echo "GET $TARGET" | vegeta attack \
    -insecure -keepalive=false -http2=false \
    -rate "$LORIS_CONTROL_RATE" -duration "$LORIS_CONTROL_DURATION" \
    -timeout 10s > "$RESULTS/loris-control.bin" 2>/dev/null
vegeta report < "$RESULTS/loris-control.bin" | sed 's/^/    /'

CTRL_JSON=$(vegeta report -type json < "$RESULTS/loris-control.bin")
CTRL_SUCCESS=$(echo "$CTRL_JSON" | jq '.success * 100')
echo "    control_success = $(printf '%.2f' "$CTRL_SUCCESS")% (threshold: >= ${THRESH_LORIS_CTRL}%)"

# Wait for loris to finish so we capture its summary.
wait "$LORIS_PID" 2>/dev/null || true

if (( $(echo "$CTRL_SUCCESS < $THRESH_LORIS_CTRL" | bc -l) )); then
  red "    FAIL: control traffic squeezed out by loris"
  PASS=false
else
  green "    PASS"
fi

fi  # HAVE_SLOWHTTPTEST

# ----------------------------------------------------------------------
# Summary
# ----------------------------------------------------------------------
hdr "Summary"
echo "    Results dir: $RESULTS"
if $PASS; then
  green "==> PASS"
  exit 0
else
  red "==> FAIL"
  exit 1
fi
