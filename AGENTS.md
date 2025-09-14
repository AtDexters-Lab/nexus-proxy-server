# Repository Guidelines

## Project Structure & Module Organization
- Root entrypoint: `proxy-server/main.go` (binary server).
- Core packages live under `internal/`:
  - `internal/proxy` (listeners, SNI parsing, ACME helpers)
  - `internal/hub` (backend auth, pools, load balancing)
  - `internal/peer` (inter‑node tunnels)
  - `internal/config` (YAML config loading)
  - `internal/routing`, `internal/iface` (supporting utilities)
- Config: `config.example.yaml` (copy to `config.yaml`).
- TLS cache (when using ACME): `acme_certs/`.
- Installer script: `scripts/install.sh` (systemd install; curl | bash friendly).

## Build, Test, and Development Commands
- Build: `go build -o bin/nexus-proxy-server ./proxy-server`
  - Produces a local binary in `bin/`.
- Run (dev): `go run ./proxy-server -config config.example.yaml`
- Test: `go test ./...`
- Format: `go fmt ./...`  |  Vet: `go vet ./...`
- Minimum Go version: as specified in `go.mod` (Go 1.24 toolchain).

## Releases and Artifacts
- GitHub Actions workflow: `.github/workflows/release.yml`
- Trigger: push a git tag matching `v*` (e.g., `v0.1.2`)
- Release assets:
  - `nexus-proxy-server_linux_amd64.tar.gz`
  - `nexus-proxy-server_linux_arm64.tar.gz`
  - `SHA256SUMS`

## Installer Notes (`scripts/install.sh`)
- One‑liner: `sudo bash -c 'curl -fsSL https://raw.githubusercontent.com/AtDexters-Lab/nexus-proxy-server/main/scripts/install.sh | bash'`
- Behavior: detects arch, downloads latest (or `NEXUS_VERSION`), verifies SHA256, prompts for FQDN, writes `/etc/nexus-proxy-server/config.yaml`, installs systemd unit.
- Re‑run on existing systems: prompts to [U]pgrade (binary), [R]econfigure (backup old config), or [A]bort.
- Env overrides: `NEXUS_VERSION`, `NEXUS_HOST`, `NEXUS_SKIP_DNS`, `NEXUS_IDLE_TIMEOUT`.

## Proxy Sniffing and Prelude Forwarding
- TLS SNI sniff via `crypto/tls` aborted handshake; HTTP sniff via `net/http`.
- Forward captured prelude to backends using `NewClientWithPrelude`.
- Use `WithPrelude` when handing the conn to an in‑process handler that must re‑see bytes (ACME, tunnels).
- ACME HTTP‑01 only: intercept `/.well-known/acme-challenge/*` on port 80; do not route to backends.

## Hostname Normalization
- Always compare using canonical ASCII lower‑case. Use the `normalizeHostname` helper (IDNA ToASCII, trim trailing dot).

## Coding Style & Naming Conventions
- Use `gofmt` (tabs by default); do not hand‑format.
- Package names: short, lower‑case (`proxy`, `hub`).
- Exported identifiers: `CamelCase` with clear nouns/verbs; unexported start lowercase.
- Files: snake_case like `peer-manager.go`, tests end with `_test.go`.
- Prefer small, focused packages under `internal/`.

## Testing Guidelines
- Frameworks: standard `testing` with `testify/require` as needed.
- Location: alongside code (e.g., `internal/proxy/sni_test.go`).
- Names: `TestXxx(t *testing.T)`; table‑driven where practical.
- Run locally with coverage: `go test ./... -cover`.

## Commit & Pull Request Guidelines
- Commits: short, imperative summaries (e.g., "fix peer tunneling route").
- PRs must include:
  - What changed and why, with links to issues.
  - How to test (commands, config snippets).
  - Any config or security implications (ports, TLS, JWT).
  - Logs or screenshots when behavior is user‑visible.

## Security & Configuration Tips
- Routing requires SNI (TLS) or Host header (HTTP/80). ECH is not supported.
- Backend auth uses a JWT secret in config (`backendsJWTSecret`). Keep it out of git.
- ACME: HTTP‑01 is supported on port 80; code uses Let’s Encrypt staging for hub TLS by default. Switch to prod or manual TLS as needed.
- Limit exposed ports to those in `relayPorts` and the hub listener.
