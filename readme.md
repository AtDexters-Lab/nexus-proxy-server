# Nexus: A Geo-Distributed, Stateless L4/L7 TCP Proxy

[![Go Version](https://img.shields.io/badge/go-1.21+-blue.svg)](https://golang.org)
[![Build Status](https://img.shields.io/badge/build-passing-brightgreen.svg)](https://github.com/AtDexters-Lab/global-access-relay)
[![License](https://img.shields.io/badge/license-Apache%202.0-blue.svg)](https://opensource.org/licenses/Apache-2.0)

**Nexus** is a high-performance, stateless, geo-distributed reverse proxy designed for modern infrastructure. It routes traffic at both Layer 4 (TCP) and Layer 7 (HTTP) without requiring TLS termination, making it a secure and transparent gateway for your backend services.

The core mission of Nexus is to intelligently route client requests to the appropriate backend server cluster based on the requested **hostname (FQDN)**, even when those clusters are spread across the globe. It achieves this while keeping the proxy layer itself completely stateless regarding user sessions, which dramatically simplifies deployment and scaling.

## Core Philosophy & Key Features

-   **Stateless at the Edge:** The proxy does not maintain any user session state (like sticky sessions). This makes the proxy nodes themselves trivial to scale and highly resilient. State is managed by the backend application layer.
-   **Opaque by Default:** For generic TCP traffic (e.g., on port 443), Nexus operates at Layer 4, forwarding byte streams without terminating or inspecting the encrypted payload. This enhances security and simplifies certificate management.
-   **Hybrid L4/L7 Operation:** Nexus is a pure L4 proxy for standard traffic but also operates as a lightweight L7 proxy on port 80. This provides a practical solution for backend services that need to solve the Let's Encrypt `HTTP-01` challenge for certificate automation.
-   **Intelligent Geo-Routing:** Nexus nodes form a mesh, allowing them to tunnel traffic to one another. This enables efficient geo-distribution where backends only need to connect to their nearest regional proxies, not the entire global fleet.
-   **Efficient Backend Communication:** Backends connect to the proxy mesh via a persistent WebSocket connection, over which many concurrent client streams are multiplexed. This is highly efficient and resilient to network firewalls.
-   **Simple, Weighted Load Balancing:** Uses a straightforward Weighted Round Robin (WRR) algorithm to distribute load across available backend instances for a given service.

## Architectural Decisions & Rationale

| Decision                           | Rationale                                                                                                                                                                                                                                                                                                                                                                           |
| :--------------------------------- | :---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| **No Session Stickiness**          | We initially explored IP-based stickiness and consistent hashing. Both introduced significant complexity and state-synchronization challenges. The most robust and scalable modern architecture is to make the edge stateless and delegate session management to the backend services themselves (e.g., using a shared Redis or database). This simplifies the proxy immeasurably.  |
| **WebSockets for Backends**        | Instead of requiring backends to accept raw TCP, they connect *outbound* to the proxy via WebSockets. This is more firewall-friendly, uses a standard, well-supported protocol, and provides a natural framing mechanism for our multiplexing protocol.                                                                                                                             |
| **No TLS Termination**             | The proxy never terminates the client's TLS connection. This provides maximum security and privacy, as the unencrypted application data never touches the proxy. It also delegates the responsibility of certificate management entirely to the backend services, where it belongs.                                                                                                 |
| **Hybrid L4/L7 Proxy**             | While a pure L4 proxy is architecturally "cleaner," it's not practical. To support automated certificate renewal via the standard `HTTP-01` challenge, the proxy must be able to route plain HTTP traffic from port 80. We chose this over the `DNS-01` challenge to avoid creating a dependency on external DNS provider APIs, making Nexus more self-contained.                   |
| **Inter-Proxy Mesh for Tunneling** | Requiring every backend to connect to every proxy globally does not scale. The mesh design allows backends to connect to a small regional "pod" of proxies. If a client connects to a different region, the proxy can tunnel the traffic to the correct region. This optimizes for backend connection resources and uses tunneling as a cross-region failover or routing mechanism. |


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
    -   **Control Messages (JSON):** `{"event": "connect", "client_id": "..."}` signals a new client.
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


## Getting Started (Example)

Nexus is configured via a `config.yaml` file.

```yaml
# config.yaml

# Address for the backend hub to listen on
listenAddress: ":8080"

# Ports to listen on for public client traffic
proxyPorts:
  - 443   # L4 TCP with SNI
  - 80    # L7 HTTP with Host header

# A shared secret used to authenticate connections between peer nodes.
# Must be the same on all nodes in the mesh.
peerSecret: "a-very-strong-secret-for-the-mesh"

# A map of peer Nexus nodes to form the mesh
peers:
  - "wss://[nexus-eu.example.com/mesh](https://nexus-eu.example.com/mesh)"
  - "wss://[nexus-ap.example.com/mesh](https://nexus-ap.example.com/mesh)"

# A map of allowed backend hostnames (FQDNs) and the secret to validate their JWT
backends:
  "api.nexus.dev": "a-very-strong-jwt-secret-for-the-api-service"
  "customer-a.com": "another-secret-for-a-different-service"
```