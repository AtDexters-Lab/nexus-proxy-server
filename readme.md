# Nexus Proxy: A Privacy-First Passthrough Proxy

**Nexus Proxy** is a **privacy-first passthrough proxy**, built on the principle of zero-trust networking. It intelligently routes encrypted traffic **without ever terminating the TLS connection**, preserving true end-to-end encryption between your clients and backend services. The data stream remains completely **opaque and private** to Nexus, making it a secure-by-default gateway for your infrastructure.

The core mission of Nexus is to intelligently route client requests to the appropriate backend server cluster based on the requested hostname (FQDN), even when those clusters are spread across the globe. It achieves this while keeping the proxy layer itself completely stateless regarding user sessions, which dramatically simplifies deployment and scaling.

## Core Philosophy & Key Features

-   **Privacy by Design:** The proxy's primary security feature is that it *cannot* see your data. By passing the encrypted TLS stream through without termination, it guarantees privacy from the proxy layer itself.
-   **Stateless at the Edge:** The proxy does not maintain any user session state (like sticky sessions). This makes the proxy nodes themselves trivial to scale and highly resilient. State is managed by the backend application layer.
-   **Hybrid L4/L7 Operation:** Nexus is a pure passthrough proxy for standard traffic but also operates as a lightweight L7 proxy on port 80. This provides a practical solution for backend services that need to solve the Let's Encrypt `HTTP-01` challenge for certificate automation.
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

### Hub TLS (Mandatory)
The hub server (for backend and peer connections) requires TLS. For local development, you can [generate a self-signed certificate](https://mkcert.dev)

```yaml
# config.yaml

# Address for the backend hub to listen on for WebSocket connections.
backendListenAddress: ":8443"

# TLS certificate and key for the hub server. (Mandatory)
hubTlsCertFile: "hub.crt"
hubTlsKeyFile: "hub.key"

# Ports to listen on for public client traffic.
# Port 80 is treated as L7 HTTP, all others as L4 TCP.
relayPorts:
  - 443
  - 80

# Time in seconds that a client connection can be idle before being closed.
idleTimeoutSeconds: 60

# A shared secret used to authenticate connections between peer nodes.
# Must be the same on all nodes in the mesh.
peerSecret: "a-very-strong-secret-for-the-mesh"

# A secret used to authenticate connections with backends.
# This secret is used to validate JWTs issued by a central authorizer.
backendsJWTSecret: "a-very-strong-jwt-secret-from-the-central-authorizer"

# A map of peer Nexus nodes to form the mesh
peers:
  - "wss://[nexus-eu.example.com/mesh](https://nexus-eu.example.com/mesh)"
```

## Reference Backend Client

A complete, working reference implementation for a backend client that connects to Nexus Proxy can be found here:

-   **[Nexus Proxy Backend Client](https://github.com/AtDexters-Lab/nexus-proxy-backend-client)**

This backend client demonstrates how to handle the WebSocket connection, authentication, and multiplexing protocol required to serve traffic from Nexus.

## The Piccolo Ecosystem: A Perfect Backend for Nexus

Nexus Proxy was designed to be the ideal gateway for personal, self-hosted services that prioritize privacy and data ownership. A perfect reference implementation for a Nexus-compatible backend is the **[Piccolo](https://piccolospace.com/)**.

Piccolo is a palm-sized personal server that gives you global access to your files and applications while ensuring total privacy. It's designed to offer the convenience of cloud services without sacrificing control over your digital life.

By connecting a Piccolo device as a backend to Nexus, you can securely expose your self-hosted services to the world without compromising your privacy.

Learn more about running your own personal server here: **[Get Piccolo](https://piccolospace.com/getpiccolo/)**
