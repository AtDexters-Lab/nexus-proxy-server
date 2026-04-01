# Nexus Proxy: A Privacy-First Passthrough Proxy

![Stage: Beta](https://img.shields.io/badge/Stage-Beta-blue)

**Nexus Proxy** is a **privacy-first passthrough proxy**, built on the principle of zero-trust networking. It intelligently routes encrypted traffic **without ever terminating the TLS connection**, preserving true end-to-end encryption between your clients and backend services. The data stream remains completely **opaque and private** to Nexus, making it a secure-by-default gateway for your infrastructure.

The core mission of Nexus is to intelligently route client requests to the appropriate backend server cluster based on the requested hostname (FQDN), even when those clusters are spread across the globe. It achieves this while keeping the proxy layer itself completely stateless regarding user sessions, which dramatically simplifies deployment and scaling.

This repository contains both the **proxy server** and the **backend client** (library + CLI).

## Core Philosophy & Key Features

-   **Privacy by Design:** The proxy's primary security feature is that it *cannot* see your data. By passing the encrypted TLS stream through without termination, it guarantees privacy from the proxy layer itself.
-   **Stateless at the Edge:** The proxy does not maintain any user session state (like sticky sessions). This makes the proxy nodes themselves trivial to scale and highly resilient. State is managed by the backend application layer.
-   **Hybrid L4/L7 Operation:** Nexus is a pure passthrough proxy for standard traffic but also operates as a lightweight L7 proxy on port 80. This provides a practical solution for backend services that need to solve the Let's Encrypt `HTTP-01` challenge for certificate automation.
-   **Automatic TLS for the Hub:** Nexus can automatically obtain and renew its own TLS certificates for its backend and peer hub endpoints using Let's Encrypt (`HTTP-01` challenge). This enables a zero-configuration TLS setup.
-   **Intelligent Geo-Routing:** Nexus nodes form a mesh, allowing them to tunnel traffic to one another. This enables efficient geo-distribution where backends only need to connect to their nearest regional proxies, not the entire global fleet.
-   **Efficient Backend Communication:** Backends connect to the proxy mesh via a persistent WebSocket connection, over which many concurrent client streams are multiplexed. This is highly efficient and resilient to network firewalls.
-   **Simple, Weighted Load Balancing:** Uses a straightforward Weighted Round Robin (WRR) algorithm to distribute load across available backend instances for a given service.

## Architecture Deep Dive
A client connects to their nearest Nexus node. That node identifies the target service and routes the connection, either directly to a locally connected backend or by tunneling to a peer Nexus node.

```ascii
              DNS Geo-Routing
                    |
+-------------------+-------------------+
|                   |                   |
|  [Nexus Proxy A]  |  [Nexus Proxy B]  |
|   (US-West)       |   (EU-Central)    |
+-------------------+-------------------+
      ^       \      (Peer Mesh Conn)   ^
      |        \                        |
      |         `-----> Tunnel <-------`
(Client in US) |                          | (Backend in EU)
      |        +-------------------+    |
      `------> | Backend (US)      |    |
               +-------------------+    `------> WebSocket Conn

```

#### Components

-   **The Nexus Node:** A single running instance of the proxy. It listens for client traffic on multiple ports (L4/L7) and for backend connections on a dedicated "hub" port. It also connects to other Nexus nodes to form the mesh.
-   **The Hub:** The component within a Nexus node that manages authenticating backends (via JWT) and maintaining their WebSocket connections.
-   **The Peer Manager:** Manages outbound WebSocket connections to all other peer Nexus nodes in the fleet, and exchanges routing information with them.
-   **The Load Balancer:** A simple, stateless module that performs Weighted Round Robin selection from a list of healthy, locally-connected backends.
-   **Backend Multiplexing Protocol:** All communication with a backend happens over a single WebSocket.
    -   **Control Messages (JSON):** `{"event": "connect", "client_id": "...", "hostname": "example.com"}` signals a new client and the target virtual host.
    -   **Data Messages (Binary):** `[1-byte Control][16-byte ClientID][Payload]` carries the actual proxied data.

#### Routing Logic & Edge Cases

The routing logic is designed to be simple and explicit.

-   **For traffic on L4 ports (e.g., 443):**
    -   The proxy **must** successfully parse a **Server Name Indication (SNI)** header from the client's initial TLS handshake.
    -   If an SNI is present, it is used to look up the corresponding backend pool.
    -   If **no SNI is present**, the connection is considered ambiguous and is **immediately terminated**.
    -   The use of **Encrypted Client Hello (ECH)** is incompatible with this routing model. For Nexus to function, backend services must not enable ECH.

-   **For traffic on the L7 port (80):**
    -   The proxy **must** successfully parse an HTTP `Host` header.
    -   If the `Host` header is missing or malformed, the request cannot be routed and the connection is closed.


## Install

### Quick Install (Linux, Server)

- Requirements:
  - Public ports `80` and `443` reachable from the internet (HTTP-01 ACME).
  - A DNS A record you can point to this server.
  - Systemd-based Linux (for service installation).

- One-liner installer:

```
sudo bash -c 'curl -fsSL https://raw.githubusercontent.com/AtDexters-Lab/nexus-proxy/main/scripts/install.sh | bash'
```

- What the script does:
  - Detects CPU arch and downloads the latest release.
  - Prompts for your Nexus hostname (FQDN) and guides DNS A record setup.
  - Writes `/etc/nexus-proxy-server/config.yaml` with a generated JWT secret.
  - Installs and starts a `systemd` service named `nexus-proxy-server`.

- Environment overrides:
  - Pin version: `NEXUS_VERSION=v0.1.2`
  - Provide hostname non-interactively: `NEXUS_HOST=nexus.example.com`
  - Skip DNS wait (CI/testing): `NEXUS_SKIP_DNS=skip`

Example:

```
sudo NEXUS_VERSION=v0.1.2 NEXUS_HOST=nexus.example.com NEXUS_SKIP_DNS=skip \
  bash -c 'curl -fsSL https://raw.githubusercontent.com/AtDexters-Lab/nexus-proxy/main/scripts/install.sh | bash'
```

### Releases (Binaries)

- We publish Linux binaries for `amd64` and `arm64` on every Git tag `v*`.
- Artifacts include the server binary, client binary, and their respective config examples.
- See the GitHub Releases page for download links and `SHA256SUMS`.

### Build From Source

```bash
# Server
go build -o bin/nexus-proxy-server ./cmd/nexus-proxy-server

# Client
go build -o bin/nexus-proxy-client ./cmd/nexus-proxy-client

# Test
go test ./...
```

## Configure

### Server

Nexus is configured via a `config.yaml` file. See the [example config](config/server.example.yaml) for all available options.

#### Hub TLS (Automatic or Manual)

The hub server (for backend and peer connections) requires TLS. You can choose between two modes:

1.  **Automatic (Recommended):** Simply provide your server's public hostname in the `config.yaml`. Nexus will automatically obtain and renew a free TLS certificate from Let's Encrypt.

    ```yaml
    hubPublicHostname: "nexus.example.com"
    ```

2.  **Manual:** Manually specify the path to your own certificate and key files.

    ```yaml
    hubTlsCertFile: "/path/to/your/fullchain.pem"
    hubTlsKeyFile: "/path/to/your/privkey.pem"
    ```

#### Managing the Service

- Check status: `systemctl status nexus-proxy-server`
- Restart: `sudo systemctl restart nexus-proxy-server`
- Logs: `journalctl -u nexus-proxy-server -f`

### Backend Clients

Backends authenticate to the Hub using a JWT signed with the shared secret from `config.yaml`.

- Preferred claim: `hostnames` (array of FQDNs this backend serves)

```json
{
  "hostnames": ["app.example.com", "api.example.com"],
  "weight": 5,
  "exp": 1735689600
}
```

At runtime, the proxy sends a `connect` control message to your backend for each client with the resolved `hostname` field so a multi-tenant backend can route appropriately.

#### Wildcard Hostnames (Single-Label)

Backends may register wildcard hostnames using a single leftmost label (TLS-style), e.g. `*.example.com`.

- Matching: `a.example.com` matches; `a.b.example.com` does not. Exact hostnames always take precedence over wildcard.

#### Port Claims (TCP/UDP)

Backends may optionally claim whole ports (e.g. an authoritative DNS service on 53) in addition to (or instead of) hostnames.

- Enable UDP listeners with `udpRelayPorts` (e.g. `[53]`).
- Allowlist claimable ports with `allowedTCPPortClaims` / `allowedUDPPortClaims`.
- JWT claims: `"tcp_ports": [53]`, `"udp_routes": [{"port": 53, "flow_idle_timeout_seconds": 60}]`

### Client Configuration

See the [client example config](config/client.example.yaml) for all available options.

Key config fields:

- `hostnames`: FQDNs or wildcards this backend serves
- `nexusAddresses`: WebSocket URLs to connect to (e.g., `wss://nexus.example.com:8443/connect`)
- `attestation`: Either `command` (external) or `hmacSecret`/`hmacSecretFile` (built-in HS256)
- `portMappings`: Maps Nexus ports to local targets with optional hostname overrides
- `healthChecks`: Active ping/pong for connection liveness

### Client Library Usage

The client can be used as a Go library for custom integrations:

```go
import "github.com/AtDexters-Lab/nexus-proxy/client"

router := func(ctx context.Context, req client.ConnectRequest) (net.Conn, error) {
    if strings.HasSuffix(req.Hostname, ".preview.example.com") {
        return net.Dial("tcp", "localhost:5080")
    }
    return nil, client.ErrNoRoute  // Fall back to YAML config
}
c, _ := client.New(cfg, client.WithConnectHandler(router))
go c.Start(ctx)
```

## The Piccolo Ecosystem

| Component | Role |
|-----------|------|
| [piccolo-os](https://github.com/AtDexters-Lab/piccolo-os) | OS images, install guides, and project hub |
| [piccolod](https://github.com/AtDexters-Lab/piccolod) | On-device daemon -- portal, app management, encryption |
| [namek-server](https://github.com/AtDexters-Lab/namek-server) | Orchestrator -- device auth, DNS, certificates |
| [nexus-proxy](https://github.com/AtDexters-Lab/nexus-proxy) | Edge relay -- remote access with device-terminated TLS |
| [piccolo-store](https://github.com/AtDexters-Lab/piccolo-store) | App catalog -- manifests for installable apps |

## License

AGPL-3.0 -- see [LICENSE](./LICENSE).
