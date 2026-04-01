# Repository Guidelines

## Project Structure & Module Organization

This is a monorepo containing the Nexus proxy server and backend client.

- **Module**: `github.com/AtDexters-Lab/nexus-proxy`
- **Server entry point**: `cmd/nexus-proxy-server/main.go`
- **Client entry point**: `cmd/nexus-proxy-client/main.go`
- **Shared protocol**: `protocol/` (public package — wire format types, constants, control messages)
- **Shared hostnames**: `hostnames/` (public package — IDNA normalization, wildcard matching)
- **Client library**: `client/` (public package — importable by external consumers)
- **Server internals**: `internal/` (private server packages):
  - `internal/proxy` (listeners, SNI parsing, ACME helpers)
  - `internal/hub` (backend auth, pools, load balancing)
  - `internal/peer` (inter-node tunnels)
  - `internal/config` (YAML config loading)
  - `internal/routing`, `internal/iface` (supporting utilities)
  - `internal/auth` (JWT validation)
  - `internal/bandwidth` (DRR scheduler)
  - `internal/registration` (orchestrator heartbeat)
  - `internal/stun` (RFC 5389 STUN server)
- Config examples: `config/server.example.yaml`, `config/client.example.yaml`
- TLS cache (when using ACME): `acme_certs/`
- Installer script: `scripts/install.sh` (systemd install; curl | bash friendly)

## Build, Test, and Development Commands
- Build server: `go build -o bin/nexus-proxy-server ./cmd/nexus-proxy-server`
- Build client: `go build -o bin/nexus-proxy-client ./cmd/nexus-proxy-client`
- Run server (dev): `go run ./cmd/nexus-proxy-server -config config/server.example.yaml`
- Run client (dev): `go run ./cmd/nexus-proxy-client -config config/client.example.yaml`
- Test all: `go test ./...`
- Test server only: `go test ./internal/...`
- Test client only: `go test ./client/...`
- Format: `go fmt ./...`  |  Vet: `go vet ./...`
- Minimum Go version: as specified in `go.mod` (Go 1.24 toolchain).

## Releases and Artifacts
- GitHub Actions workflows: `.github/workflows/release.yml`, `.github/workflows/ci.yml`
- Release trigger: push a git tag matching `v*` (e.g., `v0.1.2`)
- CI trigger: push or PR to main
- Release assets:
  - `nexus-proxy-server_linux_amd64.tar.gz` / `nexus-proxy-server_linux_arm64.tar.gz`
  - `nexus-proxy-client_linux_amd64.tar.gz` / `nexus-proxy-client_linux_arm64.tar.gz`
  - `SHA256SUMS`

## Installer Notes (`scripts/install.sh`)
- One-liner: `sudo bash -c 'curl -fsSL https://raw.githubusercontent.com/AtDexters-Lab/nexus-proxy/main/scripts/install.sh | bash'`
- Behavior: detects arch, downloads latest (or `NEXUS_VERSION`), verifies SHA256, prompts for FQDN, writes `/etc/nexus-proxy-server/config.yaml`, installs systemd unit.
- Re-run on existing systems: prompts to [U]pgrade (binary), [R]econfigure (backup old config), or [A]bort.
- Env overrides: `NEXUS_VERSION`, `NEXUS_HOST`, `NEXUS_SKIP_DNS`, `NEXUS_IDLE_TIMEOUT`.

## Shared Protocol (`protocol/`)
- Wire format: binary messages with `ControlByteData` (0x01) or `ControlByteControl` (0x02) prefix
- `ControlMessage` struct: JSON-encoded control messages between proxy and backends
- `EventType` constants: connect, disconnect, ping_client, pong_client, pause_stream, resume_stream
- `PeerMessage` struct: JSON control messages between peer nodes
- `DisconnectReason` type: machine-readable disconnect reason codes
- `Transport` type: "tcp" or "udp"

## Client Library (`client/`)
- `Client`: main connection lifecycle manager (WebSocket to Nexus, local connection relay)
- `ClientBackendConfig`: per-backend configuration
- `TokenProvider` interface: pluggable attestation (CommandTokenProvider, HMACTokenProvider)
- `WithConnectHandler()`: custom connection routing; return `ErrNoRoute` to fall back to port mappings
- Port mappings support exact hostname overrides taking precedence over wildcards

## Proxy Sniffing and Prelude Forwarding
- TLS SNI sniff via `crypto/tls` aborted handshake; HTTP sniff via `net/http`.
- Forward captured prelude to backends using `NewClientWithPrelude`.
- Use `WithPrelude` when handing the conn to an in-process handler that must re-see bytes (ACME, tunnels).
- ACME HTTP-01 only: intercept `/.well-known/acme-challenge/*` on port 80; do not route to backends.

## Hostname Normalization
- Always compare using canonical ASCII lower-case. Use the `normalizeHostname` helper (IDNA ToASCII, trim trailing dot).

## Coding Style & Naming Conventions
- Use `gofmt` (tabs by default); do not hand-format.
- Package names: short, lower-case (`proxy`, `hub`).
- Exported identifiers: `CamelCase` with clear nouns/verbs; unexported start lowercase.
- Files: lowercase with hyphens or underscores (e.g., `peer-manager.go`), tests end with `_test.go`.
- Prefer small, focused packages under `internal/`.

## Testing Guidelines
- Frameworks: standard `testing` with `testify/require` as needed.
- Location: alongside code (e.g., `internal/proxy/sni_test.go`, `client/client_tdd_test.go`).
- Names: `TestXxx(t *testing.T)`; table-driven where practical.
- Run locally with coverage: `go test ./... -cover`.

## Commit & Pull Request Guidelines
- Commits: short, imperative summaries (e.g., "fix peer tunneling route").
- PRs must include:
  - What changed and why, with links to issues.
  - How to test (commands, config snippets).
  - Any config or security implications (ports, TLS, JWT).
  - Logs or screenshots when behavior is user-visible.

## Security & Configuration Tips
- Routing requires SNI (TLS) or Host header (HTTP/80). ECH is not supported.
- Backend auth uses a JWT secret in config (`backendsJWTSecret`). Keep it out of git.
- ACME: HTTP-01 is supported on port 80; code uses Let's Encrypt staging for hub TLS by default. Switch to prod or manual TLS as needed.
- Limit exposed ports to those in `relayPorts` and the hub listener.
