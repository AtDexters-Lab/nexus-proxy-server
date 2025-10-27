# TPM-Backed Attestation Architecture

This document captures the proposed architecture for integrating TPM-based remote attestation into the Nexus proxy without pushing policy logic into the proxy itself. The intent is to let Nexus remain a lightweight verifier that simply enforces whatever policy the external authorizer encodes inside signed tokens.

## Components & Responsibilities

- **Backend service**
  - Runs the business logic and holds the TPM (with IMA or equivalent runtime measurement enabled).
  - Implements the attestation flow locally: when Nexus challenges, it contacts the authorizer, gathers required nonces, produces TPM quotes, and relays resulting tokens back to Nexus.
  - Must deliver a nonce-matching token within the negotiated grace periods to keep its session alive.

- **Authorizer (attestation authority)**
  - Handles enrollment, PCR baselines, policy evaluation, revocation, and nonce freshness.
  - Issues two classes of tokens (handshake and attested) signed with keys that Nexus trusts via the remote validator endpoint.
  - Optionally exposes a health endpoint that lets Nexus defer or stagger re-attestation during maintenance.
  - Never needs to contact Nexus directly; it remains unaware of proxy topology.

- **Nexus proxy**
  - Issues per-connection nonces, validates tokens via the configured remote verifier URL, and enforces the claims embedded in each JWT.
  - Maintains no local policy: defaults are only used when claims are absent.
  - Implements timers for initial attestation, re-attestation, and maintenance grace windows.
  - Drops client tunnels if a backend fails to deliver a valid token inside the advertised grace period.

- **Remote JWT verifier**
  - Existing infrastructure that Nexus consults (over HTTPS) to validate token signatures so that the proxy does not manage signing secrets.
  - Must expose consistent keys for all attestation tokens (or publish JWKS). The authorizer is responsible for keeping the verifier’s keys in sync.

## Connection Lifecycle

### Stage 0 – Handshake Token

1. Backend opens a WebSocket to `/connect`.
2. Backend immediately presents a **handshake token** (short-lived JWT) from the authorizer.
3. Nexus validates the signature via the remote verifier. If the token is syntactically valid and within its `handshake_max_age`, Nexus replies with a fresh `session_nonce` and transitions to *awaiting attestation*.
4. The token may already include a `session_nonce` claim, but if it does not match the nonce Nexus just issued (or is absent), the token is treated as Stage 0; Nexus still waits for Stage 1.
5. If the handshake token is invalid or missing, Nexus terminates the connection.

The handshake token exists to authenticate cheaply and rate-limit without forcing a fresh TPM quote for every failed attempt. All backends—TPM-based or otherwise—must still complete Stage 1 before traffic flows.

### Stage 1 – Attested Token

1. Backend calls the authorizer, supplying the Nexus-issued `session_nonce`. The authorizer responds with a pre-mixed challenge value (e.g., `combined_nonce = hash(session_nonce || authority_nonce)`) plus any correlation handle it needs to track the request. The backend feeds `combined_nonce` into the TPM quote; the authorizer records the handle → combined nonce mapping for later verification.
2. Backend invokes the local TPM (using whatever library or helper it embeds) to produce a quote with `combined_nonce` in the `extraData`, ensuring the evidence binds to the Nexus challenge and the authorizer’s freshness guarantee.
3. Backend submits the quote (and any required PCR metadata) to the authorizer along with the correlation handle (and, optionally, the original `session_nonce` if the authorizer uses it for lookup).
4. Authorizer verifies the quote against the stored `combined_nonce`, ensures PCRs match current policy (including IMA PCRs where applicable), and issues an **attested token** containing:
   - The original `session_nonce`.
   - Any reauthentication, maintenance, or routing directives.
   - An extremely short expiry (`exp`, typically a few seconds).
5. Backend sends the attested token to Nexus. Nexus verifies the signature, checks that `session_nonce` matches the one it issued, and—if everything passes—starts relaying client traffic.
6. If the backend fails to deliver a valid attested token within the `reauth_grace` duration (default from claims or Nexus fallback), Nexus closes the socket without ever forwarding client bytes.
7. Nexus treats any token missing or mismatching `session_nonce` as Stage 0. Only a nonce-matching token upgrades the connection to Stage 1 and enables traffic, regardless of how the JWT was produced.

### Ongoing Re-Attestation

1. Nexus tracks `next_reauth = connection_time + reauth_interval` if the claim is present.
2. When the timer fires, Nexus:
   - Generates a new `session_nonce`.
   - Sends a control frame (`reauth_request`) to the backend containing the nonce.
   - Starts a grace timer using `reauth_grace` (or a default).
3. Backend repeats the Stage 1 flow over the existing connection, submitting a fresh attested token before the grace timer expires. Traffic continues to flow during this window.
4. Failure to refresh the token in time causes Nexus to drop the session and tear down client tunnels.

### Maintenance & Health Awareness

- If the attested token includes `authorizer_status_uri`, Nexus polls that endpoint before initiating re-attestation. If the endpoint reports maintenance/unhealthy or is unreachable, Nexus:
  - Defers re-attestation, extending the current session up to a configurable hard ceiling (e.g., 30 minutes of cumulative deferral).
  - After the authorizer reports healthy, Nexus staggers re-attestation start times by adding jitter derived from the remaining interval to prevent a thundering herd.

## Token Schema

Both handshake and attested tokens share standard JWT fields (`iss`, `sub`, `aud`, `exp`, `nbf`) and are signed with keys available through the remote verifier URL. Additional claims:

| Claim | Applies to | Required | Description |
|-------|------------|----------|-------------|
| `session_nonce` | Attested | Yes | The exact nonce Nexus issued for the current connection or re-auth. Nexus rejects tokens when it is absent or mismatched. |
| `handshake_max_age` (seconds) | Handshake | Optional | Maximum token age Nexus should accept; defaults to 0 (must be current) if absent. |
| `reauth_interval_seconds` | Attested | Optional | When present, Nexus schedules the next re-attestation `interval` seconds after successful validation. If absent, Nexus does not request re-auth. |
| `reauth_grace_seconds` | Attested | Optional | Window in which the backend must present the refreshed token after Nexus issues a challenge. Nexus falls back to 10 seconds if absent. |
| `maintenance_grace_cap_seconds` | Attested | Optional | Maximum cumulative extension Nexus may grant during authorizer maintenance. Absent implies Nexus uses its local default cap. |
| `authorizer_status_uri` | Attested | Optional | HTTPS endpoint Nexus polls before forcing re-attestation; must be authenticated or pinned to prevent spoofing. |
| `hostnames` | Both | Yes | Array of normalized hostnames or wildcard patterns this backend may serve. |
| `weight` | Both | Optional | Backend load-balancing weight (defaults to 1). |
| `policy_version` | Attested | Optional | Identifier for the PCR baseline or policy revision used; aids auditing and rollback. |
| `issued_at_quote` | Attested | Optional | Timestamp (RFC 3339) of when the authorizer observed the TPM quote. Useful for diagnostics when combined with short `exp`. |

Authorizer-specific claims may be forwarded as opaque metadata; Nexus ignores unknown fields but MUST preserve them if the control plane ever echoes tokens.

## Defaults & Validation Logic

- **Signature**: Nexus delegates signature/expiry verification to the remote verifier URL already supported today. Tokens failing remote validation are rejected outright.
- **Session nonce**: Always required for attested tokens. Nexus generates cryptographically strong nonces per connection/re-auth attempt and tracks used values to prevent replay.
- **Re-auth defaults**: In the absence of `reauth_interval_seconds`, Nexus does not schedule re-authentication. If `reauth_grace_seconds` is missing while `reauth_interval_seconds` is present, Nexus uses 10 seconds.
- **Maintenance**: Without `authorizer_status_uri`, Nexus performs re-attestation immediately when due. If the URI is present but unreachable, Nexus temporarily extends the session until the maintenance cap is hit, then drops the connection.
- **Logging & metrics**: Nexus should emit events for handshake success/failure, re-auth attempts, extensions due to maintenance, and dropped sessions to aid operations.

## Security Considerations

- **Replay protection**: Short `exp` values combined with per-attempt nonces ensure tokens cannot be reused. The authorizer must refuse to sign tokens when the TPM quote lacks the expected nonce hash.
- **IMA enforcement**: Measuring runtime binaries via IMA (or similar) ensures PCR values change immediately after tampering, preventing the “attest-clean-then-swap” attack.
- **Rate limiting**: Nexus can enforce per-backend or per-IP limits at the handshake stage to mitigate abuse, since Stage 0 relies only on a cheap signature check.
- **Clock skew**: All parties should run NTP; Nexus may allow a small clock skew tolerance when checking `exp` and `nbf`.

## Next Steps

1. Finalize the token claim names/types and update the authorizer to emit them.
2. Extend Nexus to implement the two-stage handshake, nonce management, re-auth timers, maintenance grace logic, and remote verifier integration for the new claims.
3. Update backend SDK/tooling so services can respond to `reauth_request` frames and handle attestation refresh without disrupting active client streams.
