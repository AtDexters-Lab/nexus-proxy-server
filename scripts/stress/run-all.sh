#!/usr/bin/env bash
#
# One-command post-deploy stress validation for a nexus-proxy instance.
#
# Builds the test backend + nexus-proxy-client, starts both as background
# processes, waits for the client to authenticate against the target nexus,
# then runs the stress harness. Tears everything down on exit (including
# Ctrl-C and failures).
#
# Usage:
#   scripts/stress/run-all.sh \
#       --nexus-url wss://nxs.example.com:8443/connect \
#       --hostname  stress.example.com \
#       --secret    <hmac-secret-hex>
#
# Or via env vars (any combination):
#   NEXUS_URL=... TARGET_HOSTNAME=... HMAC_SECRET=... scripts/stress/run-all.sh
# (TARGET_HOSTNAME, not HOSTNAME — the latter is reserved by bash for the
# system hostname.)
#
# Required: --nexus-url, --hostname, and one of --secret / --secret-file.
# Optional: --preset {laptop|same-network} (default laptop), --target,
#           --backend-port (default 18443), --no-cleanup, --help.

set -euo pipefail

# Lock numeric locale: passed through to run.sh and ensures any printf/awk
# float formatting in this wrapper is stable across operator locales.
export LC_ALL=C

NEXUS_URL="${NEXUS_URL:-}"
# Note: bash reserves $HOSTNAME for the system hostname, so we use
# TARGET_HOSTNAME (or HOSTNAME_ARG as legacy alias) for the env-var form.
HOSTNAME_ARG="${TARGET_HOSTNAME:-${HOSTNAME_ARG:-}}"
HMAC_SECRET="${HMAC_SECRET:-}"
HMAC_SECRET_FILE="${HMAC_SECRET_FILE:-}"
PRESET="${PRESET:-laptop}"
TARGET_URL="${TARGET_URL:-}"
BACKEND_PORT="${BACKEND_PORT:-18443}"
NO_CLEANUP=false

print_help() {
  sed -n '2,21p' "$0" | sed 's/^# \?//'
  cat <<'EOF'

Examples:
  # Laptop preset against a 1-vCPU dev VM:
  scripts/stress/run-all.sh \
      -n wss://nxs.example.com:8443/connect \
      -h stress.example.com \
      -s 0123456789abcdef...

  # Same-network preset (defaults sized for low RTT):
  scripts/stress/run-all.sh -p same-network -n ... -h ... -s ...

  # Use a secret file instead of a literal:
  scripts/stress/run-all.sh -n ... -h ... -f /etc/nexus/stress-hmac.key
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    -n|--nexus-url)    NEXUS_URL="$2"; shift 2 ;;
    -h|--hostname)     HOSTNAME_ARG="$2"; shift 2 ;;
    -s|--secret)       HMAC_SECRET="$2"; shift 2 ;;
    -f|--secret-file)  HMAC_SECRET_FILE="$2"; shift 2 ;;
    -p|--preset)       PRESET="$2"; shift 2 ;;
    -t|--target)       TARGET_URL="$2"; shift 2 ;;
    -d|--backend-port) BACKEND_PORT="$2"; shift 2 ;;
    --no-cleanup)      NO_CLEANUP=true; shift ;;
    --help|-?)         print_help; exit 0 ;;
    *) echo "unknown argument: $1"; print_help; exit 2 ;;
  esac
done

red()    { printf '\033[31m%s\033[0m\n' "$*"; }
green()  { printf '\033[32m%s\033[0m\n' "$*"; }
yellow() { printf '\033[33m%s\033[0m\n' "$*"; }

[[ -n "$NEXUS_URL" ]]   || { red "missing --nexus-url (or NEXUS_URL env)"; exit 2; }
[[ -n "$HOSTNAME_ARG" ]] || { red "missing --hostname (or HOSTNAME_ARG env)"; exit 2; }
if [[ -z "$HMAC_SECRET" && -z "$HMAC_SECRET_FILE" ]]; then
  red "missing --secret or --secret-file (or HMAC_SECRET / HMAC_SECRET_FILE env)"; exit 2
fi

TARGET_URL="${TARGET_URL:-https://${HOSTNAME_ARG}/}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
WORKDIR="$(mktemp -d -t nexus-stress-run-XXXXXX)"
BACKEND_PID=""
CLIENT_PID=""

cleanup() {
  local rc=$?
  if [[ -n "$CLIENT_PID" ]]; then
    if $NO_CLEANUP; then
      yellow "==> --no-cleanup: client pid=$CLIENT_PID still running"
    else
      kill "$CLIENT_PID" 2>/dev/null || true
      wait "$CLIENT_PID" 2>/dev/null || true
    fi
  fi
  if [[ -n "$BACKEND_PID" ]]; then
    if $NO_CLEANUP; then
      yellow "==> --no-cleanup: backend pid=$BACKEND_PID still running"
    else
      kill "$BACKEND_PID" 2>/dev/null || true
      wait "$BACKEND_PID" 2>/dev/null || true
    fi
  fi
  # Always shred the HMAC secret unless the operator explicitly opted into
  # preserving it for follow-up debugging via --no-cleanup. The workdir
  # itself is preserved either way for the harness's text logs.
  if [[ -f "$WORKDIR/hmac" ]] && ! $NO_CLEANUP; then
    shred -u "$WORKDIR/hmac" 2>/dev/null || rm -f "$WORKDIR/hmac"
  fi
  if $NO_CLEANUP; then
    yellow "==> workdir preserved: $WORKDIR (CONTAINS HMAC SECRET — clean up manually)"
  else
    echo  "==> workdir: $WORKDIR (logs kept, secret shredded; prune when ready)"
  fi
  exit $rc
}
trap cleanup EXIT INT TERM

echo "==> workdir: $WORKDIR"
echo "==> target:  $TARGET_URL"
echo "==> nexus:   $NEXUS_URL"
echo "==> hostname:$HOSTNAME_ARG"
echo "==> preset:  $PRESET"

# Materialize the HMAC secret file.
if [[ -n "$HMAC_SECRET_FILE" ]]; then
  cp "$HMAC_SECRET_FILE" "$WORKDIR/hmac"
else
  printf '%s' "$HMAC_SECRET" > "$WORKDIR/hmac"
fi
chmod 600 "$WORKDIR/hmac"

# Generate the runtime nexus-proxy-client config.
cat > "$WORKDIR/client.yaml" <<EOF
backends:
  - name: "nexus-stress"
    hostnames: ["$HOSTNAME_ARG"]
    nexusAddresses: ["$NEXUS_URL"]
    weight: 1
    attestation:
      hmacSecretFile: "$WORKDIR/hmac"
      tokenTTLSeconds: 60
      reauthIntervalSeconds: 600
      reauthGraceSeconds: 30
    portMappings:
      443:
        default: "localhost:$BACKEND_PORT"
    healthChecks:
      enabled: true
      inactivityTimeout: 60
      pongTimeout: 5
EOF

echo "==> building binaries"
( cd "$REPO_ROOT" && go build -o "$WORKDIR/backend" ./scripts/stress/backend )
( cd "$REPO_ROOT" && go build -o "$WORKDIR/client"  ./cmd/nexus-proxy-client )

echo "==> starting test backend on :$BACKEND_PORT"
"$WORKDIR/backend" -addr ":$BACKEND_PORT" -hostname "$HOSTNAME_ARG" \
    > "$WORKDIR/backend.log" 2>&1 &
BACKEND_PID=$!

# Wait until the backend's TLS port responds.
backend_ready=false
for _ in {1..40}; do
  if curl -sk -o /dev/null -m 2 "https://localhost:$BACKEND_PORT/"; then
    backend_ready=true; break
  fi
  sleep 0.25
done
if ! $backend_ready; then
  red "backend did not come up; see $WORKDIR/backend.log"
  exit 3
fi

echo "==> starting nexus-proxy-client"
"$WORKDIR/client" -config "$WORKDIR/client.yaml" > "$WORKDIR/client.log" 2>&1 &
CLIENT_PID=$!

# Wait until the client authenticates with the target nexus.
echo "    waiting up to 30s for client→nexus authentication..."
client_ready=false
for _ in {1..60}; do
  if grep -q "Connection established and authenticated" "$WORKDIR/client.log" 2>/dev/null; then
    client_ready=true; break
  fi
  if ! kill -0 "$CLIENT_PID" 2>/dev/null; then
    red "nexus-proxy-client exited early; tail of log:"
    tail -20 "$WORKDIR/client.log"
    exit 3
  fi
  sleep 0.5
done
if ! $client_ready; then
  red "client did not authenticate; tail of log:"
  tail -20 "$WORKDIR/client.log"
  exit 3
fi
green "==> path is up"

# Sanity probe through the full chain.
if ! curl -sk -m 5 "$TARGET_URL" >/dev/null; then
  red "sanity probe to $TARGET_URL failed (DNS? routing?)"
  exit 3
fi
green "==> sanity probe via nexus ok"

# Apply preset → env vars consumed by run.sh.
case "$PRESET" in
  laptop)
    # Sized for "single laptop hitting a small VM (~1 vCPU) over the public
    # internet". Sustained-rate test is no-keepalive churn against ~46ms
    # RTT, so per-request cost is ~3-4 RTTs (TLS handshake) + VM CPU. 8s
    # p99 leaves headroom over typical observed ~5s while catching gross
    # regressions.
    export CONCURRENT_WORKERS="${CONCURRENT_WORKERS:-32}"
    export CONCURRENT_DURATION="${CONCURRENT_DURATION:-15s}"
    export SUSTAINED_RATE="${SUSTAINED_RATE:-10}"
    export SUSTAINED_DURATION="${SUSTAINED_DURATION:-30s}"
    export LORIS_CONNS="${LORIS_CONNS:-50}"
    export LORIS_DURATION="${LORIS_DURATION:-20}"
    export LORIS_CONTROL_RATE="${LORIS_CONTROL_RATE:-5}"
    export THRESH_P99_MS="${THRESH_P99_MS:-8000}"
    export THRESH_ERR_PCT="${THRESH_ERR_PCT:-5}"
    ;;
  same-network)
    : # use run.sh defaults
    ;;
  *)
    red "unknown preset: $PRESET (use: laptop | same-network)"; exit 2 ;;
esac

# Hand off to the harness.
"$SCRIPT_DIR/run.sh" "$TARGET_URL"
