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
- Installer script: `setup-nexus.sh` (systemd install).

## Build, Test, and Development Commands
- Build: `go build -o bin/nexus-proxy-server ./proxy-server`
  - Produces a local binary in `bin/`.
- Run (dev): `go run ./proxy-server -config config.example.yaml`
- Test: `go test ./...`
- Format: `go fmt ./...`  |  Vet: `go vet ./...`
- Minimum Go version: as specified in `go.mod` (Go 1.24 toolchain).

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
- ACME: current code uses Let’s Encrypt staging (untrusted) for hub TLS; use manual TLS in production or adjust ACME directory if changing behavior.
- Limit exposed ports to those in `relayPorts` and the hub listener.

