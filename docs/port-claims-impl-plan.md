# Nexus Port Claims (TCP/UDP) – Implementation Plan

This plan describes how to let backends “claim” whole public ports on a Nexus node (TCP and UDP) and have those ports routed through the mesh, while preserving existing hostname/SNI/Host routing as the primary mechanism.

## Goals

- Support **port claims** for **TCP** and **UDP** (e.g., `53/tcp`, `53/udp`) as first-class routes.
- Keep port claims **fallback-only**:
  - If hostname routing succeeds (SNI/Host → backend pool), use it.
  - If hostname sniffing fails (no SNI/Host), fall back to a port-claim route (if configured/claimed).
- Support **mesh routing** for port claims (tunnel to peers when no local backend exists).
- Allow **load balancing**:
  - TCP: weighted selection per connection (same as hostname pools).
  - UDP: weighted selection per flow, with flow affinity for replies.
- Make UDP behavior **generic** (not DNS-specific) via per-claimed-port policy in JWT, bounded by server-side caps.
- Maintain a **best-effort** exclusivity model at the cluster level (no global lock/consensus); operators/JWT signer can enforce exclusivity externally.

## Non-Goals

- Strong cluster-wide exclusivity guarantees (no RAFT/consensus/central coordinator in this iteration).
- Preserving true client source IP at the backend socket layer (we can provide metadata like `client_ip`).
- L7 protocol features (DNS-aware routing, EDNS-specific behavior, etc.).

## Terminology

- **Route key**: a string used for routing lookups and peer announcements.
  - Hostname routes: `app.example.com`, `*.example.com` (existing).
  - Port-claim routes: `tcp:53`, `udp:53` (new).
- **Fallback-only**: port-claim routing is attempted only after hostname routing fails.
- **UDP flow**: a 3‑tuple `(srcIP, srcPort, dstPort)` used to keep reply affinity.

## High-Level Design

### 1) Route registry and selection

- Extend the hub’s routing registries to include **port-claim pools** in addition to hostname pools.
- Add helpers to:
  - register a backend into a `tcp:<port>` or `udp:<port>` pool when it presents port claims,
  - select a backend by route key (hostname or port-key),
  - include port route keys in `GetLocalRoutes()` so they are announced to peers.

### 2) TCP fallback-only port claims

- On each accepted TCP connection:
  1. Attempt hostname routing (SNI, else HTTP Host on `:80`), including ACME interception behavior.
  2. If hostname sniffing fails (no SNI/Host), attempt `tcp:<localPort>`.
  3. If not local, look up a peer advertising `tcp:<localPort>` and tunnel the TCP stream (existing peer tunnel path).

### 3) UDP port claims (local + mesh)

- Add UDP listeners for configured `udpRelayPorts`.
- For each inbound datagram:
  - Compute flow key `(srcIP, srcPort, dstPort)`.
  - Resolve a flow owner backend for `udp:<dstPort>` using weighted selection, then keep it sticky until flow expiry.
  - Forward datagrams as framed messages (one datagram per WS frame) to the owning backend or to a peer.
- Replies from the backend must be routed back through the same flow mapping.

## Configuration Changes (Proposed)

Additions to `config.yaml` (names are proposals; exact naming TBD during implementation):

- Listener ports:
  - `udpRelayPorts: [53]` (UDP ports to bind publicly; independent of TCP `relayPorts`)
- Claim allowlists:
  - `allowedTCPPortClaims: [53]`
  - `allowedUDPPortClaims: [53]`
- UDP flow table bounds:
  - `udpMaxFlows: 200000`
  - `udpMaxDatagramBytes: 2048` (or 4096; default should be conservative)
- UDP flow idle timeout caps (server-enforced):
  - `udpFlowIdleTimeoutDefaultSeconds: 30`
  - `udpFlowIdleTimeoutMinSeconds: 5`
  - `udpFlowIdleTimeoutMaxSeconds: 300`

Notes:
- Binding `udpRelayPorts` should be optional; if unset, UDP is disabled.
- Claim allowlists prevent a compromised/misissued token from binding arbitrary ports.

## JWT Claims Changes (Proposed)

Extend backend JWT claims (additive; existing deployments remain valid):

- `tcp_ports: [53, 12345]`
- `udp_routes: [{ "port": 53, "flow_idle_timeout_seconds": 60 }]`

Server behavior:
- All claimed ports must be present in `allowed{TCP,UDP}PortClaims` or be rejected.
- `flow_idle_timeout_seconds` is clamped to the configured min/max; if omitted, use `udpFlowIdleTimeoutDefaultSeconds`.

## Wire/Protocol Changes (Proposed)

### Proxy ↔ Backend (WS multiplexing)

- Add `transport` to the connect control message so backends can interpret frames correctly:
  - `transport: "tcp" | "udp"` (default `"tcp"` if omitted by older servers)
- Optionally add `route_key` for clarity (especially if `hostname` is empty/meaningless):
  - `route_key: "udp:53"` or `route_key: "tcp:53"`

Compatibility:
- Adding fields to JSON control messages is backward compatible for clients that ignore unknown fields.

### Peer ↔ Peer (mesh)

Announcements:
- Include route keys (`tcp:<port>`, `udp:<port>`) in the existing `announce` hostnames list.

Tunneling:
- TCP: reuse existing stream tunnel implementation by using the route key in the existing `tunnel_request` message.
- UDP: add flow-level tunnel primitives:
  - `udp_flow_open` (JSON): establishes a flow ID and associates it with `udp:<port>` on the destination peer.
  - `udp_flow_close` (JSON): closes flow mapping.
  - Reuse `PeerTunnelData`/`PeerTunnelClose` binary frames to carry datagrams keyed by flow ID, or introduce dedicated UDP binary control bytes if it improves clarity.

## Routing & Load Balancing Semantics

- Local pools:
  - `tcp:<port>`: weighted WRR per new TCP connection.
  - `udp:<port>`: weighted selection per new flow, cached for affinity until idle timeout.
- Mesh:
  - If multiple peers announce the same route key, the existing peer selection behaves like load balancing across peers.
  - “Best effort exclusivity” is achieved by JWT signer/operator policy; Nexus will route to whichever peers/backends are currently advertising the route.

## Implementation Workstreams

1. **Schema & validation**
   - Extend `internal/config.Config` with UDP listener ports, allowlists, and UDP caps; update `config.example.yaml` and validation.
   - Extend `internal/auth.Claims` with port-claim fields; validate allowlists and clamp UDP policy.

2. **Hub routing registries**
   - Add registries for TCP/UDP port pools and include them in `GetLocalRoutes()`.
   - Add selection methods for `(transport, port)` and/or `routeKey`.
   - Update backend registration/unregistration to register port claims to pools.

3. **TCP fallback-only port claims**
   - Update `internal/proxy.Listener.handleConnection` to:
     - try hostname route first,
     - then attempt `tcp:<localPort>`,
     - then peer tunnel using the same route key if no local backend.
   - Ensure ACME interception remains correct for `:80` (never route ACME challenges to backends).

4. **UDP local listener**
   - Implement `internal/proxy/udp_listener.go`:
     - bind UDP ports,
     - maintain a flow table keyed by `(srcIP,srcPort,dstPort)` with last-seen timestamps,
     - enforce `udpMaxFlows` and idle eviction,
     - call `backend.AddClient(...)` once per flow and `backend.SendData(flowID, datagram)` for each datagram.
   - Add `transport` to control messages (`internal/protocol.ControlMessage` and senders).

5. **UDP mesh tunneling**
   - Extend peer protocol to open/close UDP flows and carry datagrams to the owning peer.
   - Track active UDP tunnels similarly to TCP tunnels, but keyed by flow ID and backed by a UDP-aware “connection” that writes to `net.PacketConn.WriteTo`.

6. **Logging & guardrails**
   - Log claim registration, collisions, and rejected claims (with reasons).
   - Add debug logs for UDP flow creation/eviction and per-port flow table pressure.

7. **Testing**
   - Unit tests:
     - config validation for new fields,
     - claims parsing + allowlist enforcement + timeout clamping,
     - hub pool selection for route keys.
   - Integration-ish tests (as feasible with `net` primitives):
     - TCP fallback: no SNI → routes to `tcp:<port>`,
     - UDP local: send datagram → backend receives frames and can reply.
     - Peer routing: announce `tcp:<port>`/`udp:<port>` and select correct peer.

## Open Questions / Decisions

- Should we support a mode to force **exclusive** (single-backend) port claims per node/cluster, or rely entirely on signer policy?
- Should port-claim fallback ever trigger when hostname sniff succeeds but no backend exists (i.e., a “catch-all” port route), or should fallback be limited strictly to “no SNI/Host” cases?
- Exact UDP eviction strategy under pressure (pure LRU vs random eviction when full).
- Whether to include a `route_key` field in control messages (recommended for clarity).
- Maximum safe default for `udpMaxDatagramBytes` (balance between generality and DoS resistance).

## Suggested Iteration Order

1. TCP port-claim routing (local + mesh) with fallback-only semantics.  
2. UDP local port claims (flow table + control message transport flag).  
3. UDP mesh tunneling (flow open/close + datagram forwarding).  
4. Hardening (caps, eviction, logs) + tests + docs updates.
