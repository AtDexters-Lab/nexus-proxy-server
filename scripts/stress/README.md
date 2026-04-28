# Post-deploy stress harness

A reusable validation suite to run against a deployed nexus instance after
every deploy. Drives real concurrent / sustained / slow-loris load through
the proxy at a registered backend and reports pass/fail.

## What it tests

| Test | Tool | Validates |
|---|---|---|
| Concurrent baseline | `hey` | Listener handles thousands of simultaneous in-flight connections. Specifically exercises the post-peek-release behavior — established sessions don't count against the peek cap. |
| Sustained rate | `vegeta` | Connect/disconnect churn; sustained throughput; p99 latency stays bounded. |
| Slow-loris + control | `slowhttptest` + `vegeta` | Under sustained slow-header pressure post-TLS, legitimate users can still complete requests. **Note:** on an HTTPS target this exercises post-peek behavior, not the `MaxConcurrentPeeks` bound directly (slowhttptest `-H` trickles HTTP headers *after* the TLS ClientHello, by which point peek is done). To stress the peek bound itself, point at an HTTP/80 mapping or use a tool that drips the ClientHello. |

Out of scope (would need bespoke tooling):
- Oversized ClientHello (>32 KB) — covered by unit test `TestPeekSNIAndPrelude_TruncationFlagSet`.
- Reauth-flood on the WS side — exercise by spawning many `nexus-proxy-client` processes if needed.
- Outbound TCP proxy / peer mesh.

## Quick start (recommended)

```sh
# Install OS tools once (slowhttptest needs root)
sudo apt install -y slowhttptest jq bc curl
go install github.com/rakyll/hey@latest
go install github.com/tsenart/vegeta@latest
export PATH="$HOME/go/bin:$PATH"

# DNS or /etc/hosts: point your stress hostname at the nexus IP.
echo "<NEXUS_IP>  stress.dev.example.com" | sudo tee -a /etc/hosts

# Run everything (build → spawn backend → spawn client → run harness → tear down):
scripts/stress/run-all.sh \
    --nexus-url wss://nxs.example.com:8443/connect \
    --hostname  stress.dev.example.com \
    --secret    <hmac-secret-hex>
```

Exits 0 on PASS, 1 on threshold breach, 2 on bad arguments / missing tools, 3 on
infrastructure failure (backend / client never came up). All processes spawned
by the wrapper are torn down on exit (including Ctrl-C).

Defaults to the `laptop` preset (sized for "single laptop hitting a remote
small VM over the public internet"). Use `--preset same-network` when running
from a host on the same LAN as nexus to drive much higher load.

`--no-cleanup` keeps the backend and `nexus-proxy-client` alive after the run
for follow-up inspection — useful when investigating warnings.

## Manual setup (advanced / debugging)

If you want to run the pieces by hand — e.g. to keep `nexus-proxy-client`
running across multiple harness invocations, or to inspect logs in real time
— use the manual flow below. The wrapper above just orchestrates these
steps.

### 1. Install tools

Linux / macOS — these are widely packaged:

```sh
# Debian/Ubuntu
sudo apt-get install -y slowhttptest jq bc curl
go install github.com/rakyll/hey@latest
go install github.com/tsenart/vegeta/v12/cmd/vegeta@latest

# macOS (Homebrew)
brew install hey vegeta slowhttptest jq bc
```

`hey`, `vegeta`, `slowhttptest`, `jq`, `bc`, `curl` must be on `PATH`.

### 2. Stand up the test backend

Run the HTTPS test server (self-signed cert, generates on each launch):

```sh
go run ./scripts/stress/backend -addr :18443 -hostname stress.dev.example.com
```

The backend is the *target registered with nexus*. Load testers should
always hit the public nexus URL (`https://stress.dev.example.com/`),
**not** the backend port directly — pointing them at `:18443` bypasses the
proxy and tests only the test backend, defeating the purpose.

### 3. Connect the backend to your nexus dev instance

Copy `scripts/stress/client.example.yaml`, fill in the placeholders
(`<DEV_NEXUS_URL>`, `<STRESS_HOSTNAME>`, attestation), then run:

```sh
go run ./cmd/nexus-proxy-client -config /path/to/your-stress-client.yaml
```

### 4. Resolve the hostname to the nexus instance

Either real DNS, or for a quick local test:

```sh
echo "<DEV_NEXUS_IP>  stress.dev.example.com" | sudo tee -a /etc/hosts
```

### 5. Smoke test

```sh
curl -k https://stress.dev.example.com/
# expected: ok
```

## Run after each deploy

```sh
scripts/stress/run.sh https://stress.dev.example.com/
```

Exits `0` on pass, `1` on threshold breach. Total runtime ≈ 3 min.

## Tuning thresholds

The defaults are sized for **same-network** testing (load tester on the same
LAN / data center as nexus, where round-trip latency is sub-ms and the load
tester can sustain thousands of new TLS handshakes per second).

For **single-laptop testing over the public internet**, the laptop hits its
ephemeral-port pool (~28k after `TIME_WAIT`) and CPU on TLS handshakes long
before nexus does. Use the laptop preset:

```sh
CONCURRENT_WORKERS=32 \
CONCURRENT_DURATION=15s \
SUSTAINED_RATE=10 \
SUSTAINED_DURATION=30s \
LORIS_CONNS=50 \
LORIS_DURATION=20 \
LORIS_CONTROL_RATE=5 \
THRESH_P99_MS=8000 \
THRESH_ERR_PCT=5 \
scripts/stress/run.sh https://stress.dev.example.com/
```

Override per-deploy via env vars:

```sh
CONCURRENT_WORKERS=8192 \
SUSTAINED_RATE=2000 \
THRESH_P99_MS=300 \
scripts/stress/run.sh https://stress.dev.example.com/
```

| Variable | Default | Purpose |
|---|---|---|
| `CONCURRENT_WORKERS` | `4096` | hey worker count for test 1 |
| `CONCURRENT_DURATION` | `30s` | hey duration |
| `SUSTAINED_RATE` | `1000` | vegeta requests/sec |
| `SUSTAINED_DURATION` | `60s` | vegeta duration |
| `LORIS_CONNS` | `1000` | slowhttptest connection count |
| `LORIS_DURATION` | `60` | slowhttptest seconds |
| `LORIS_CONTROL_RATE` | `100` | concurrent control traffic RPS during loris |
| `LORIS_CONTROL_DURATION` | `30s` | control traffic duration |
| `THRESH_ERR_PCT` | `1.0` | max acceptable error rate (test 1) |
| `THRESH_SUCCESS_PCT` | `99.5` | min acceptable success rate (test 2) |
| `THRESH_P99_MS` | `500` | max acceptable p99 latency (test 2) |
| `THRESH_LORIS_CTRL` | `95` | min control success during loris (test 3) |
| `INSECURE` | `-k` | curl insecure flag for the sanity probe only (vegeta uses its own `-insecure`, slowhttptest uses `-k 1`); set to empty string when targeting a real cert |

## Reading results

The script writes to a fresh `mktemp` directory each run and prints the
path at the end. Inside:

- `concurrent.txt` — full hey output
- `sustained.bin` — vegeta binary report (use `vegeta report < sustained.bin`)
- `sustained.txt` — vegeta text report
- `loris.txt` — slowhttptest output
- `loris-control.bin` — vegeta binary report from the control traffic during loris

If a test fails, inspect the corresponding file to understand the shape
of the failure (latency tail, error code distribution, etc.).

## What "pass" means and doesn't mean

**Means:** the recent listener / peek / handler changes hold up under real
OS-level concurrent load on the dimensions tested.

**Does not mean:** the system is bulletproof. The harness does not
exercise:
- Oversized ClientHellos (covered by unit tests)
- Backend-side flow-control / bandwidth-cap behavior
- Peer mesh under load (single-node)
- Authorization-token abuse paths
- Network-level DDoS (deferred to upstream)

For a more thorough validation, augment with:
- Multi-node peer-mesh testing (separate harness)
- Backend-side throughput soak via `internal/hub/sink_throughput_bench_test.go` (build tag `bench`)
- Manual tcpdump of a sample stress run if any test fails in a non-obvious way
