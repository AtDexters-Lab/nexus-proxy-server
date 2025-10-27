# Nexus TPM Attestation – Implementation Plan

This plan describes the code changes required to bring the Nexus proxy in line with the attestation architecture documented in `docs/tpm-attestation-architecture.md`. Backward compatibility with the legacy shared-secret JWT path is not required; new code can assume every backend participates in the dual-stage flow.

## Goals

- Replace the current single-message JWT auth with a two-stage handshake:
  1. Handshake token (Stage 0) → Nexus issues a `session_nonce`.
  2. Attested token (Stage 1) → Nexus validates nonce match and unlocks traffic.
- Support periodic re-attestation driven entirely by token claims (`reauth_interval_seconds`, `reauth_grace_seconds`, etc.).
- Integrate remote signature validation (HTTP-based verifier) for all tokens.
- Surface maintenance-awareness (`authorizer_status_uri`, `maintenance_grace_cap_seconds`) and stagger re-auth retries.
- Emit structured logs/metrics for handshake success, failures, and maintenance deferrals.

## High-Level Workstreams

1. **Configuration & bootstrap**  
   - Add new config entries for the remote verifier URL, request timeouts, and maintenance grace defaults.  
   - Remove the `backendsJWTSecret` requirement once the remote verifier is mandatory.  
   - Update `config.example.yaml` and validation logic accordingly.

2. **Protocol & messaging updates**  
   - Define control frames for `reauth_request` and the Stage 0 → Stage 1 transition. Candidates:
     - JSON messages on the WebSocket prior to pump start.
     - New binary control byte (e.g., `ControlByteChallenge`) if we prefer the existing frame scheme.
   - Document the wire format in the repo (README or protocol doc).

3. **Hub authentication pipeline (`internal/hub`)**  
   - Refactor `handleBackendConnect` and `authenticateBackend`:
     - Accept the initial Stage 0 token, call the remote verifier, and extract hostnames/weight claims.
     - Generate a cryptographically strong `session_nonce` and send it back to the backend (e.g., JSON message `{ "type": "challenge", "nonce": "..." }`).
     - Await a Stage 1 attested token message; validate via remote verifier and ensure `session_nonce` matches.
   - Only instantiate `Backend` (and register it) after Stage 1 succeeds.
   - Add timeout handling for both stages so stalled connections are closed.
   - Store per-backend metadata: `reauth_interval`, `reauth_grace`, `maintenance_cap`, `authorizer_status_uri`, `policy_version`, `hostnames`.

4. **Remote verifier client**  
   - Introduce a reusable package (e.g., `internal/auth/validator`) that POSTs tokens to the configured verifier endpoint and returns parsed claims.  
   - Handle retries/timeouts and map errors to actionable log messages.  
   - Cache JWKS or rely purely on the remote response, depending on the verifier API.

5. **Backend lifecycle enhancements (`internal/hub/backend.go`)**  
   - Add fields/timers to track `nextReauth` and outstanding challenges.  
   - Implement a control loop that, when `nextReauth` elapses:
     - Sends a re-auth request message to the backend with a fresh `session_nonce`.
     - Waits up to `reauth_grace` for a Stage 1 token on the control channel.
     - Drops the backend on timeout or failed validation.
   - Integrate maintenance deferral: when `authorizer_status_uri` is provided, poll it (respecting `maintenance_grace_cap_seconds` and jittered scheduling).

6. **Logging & metrics**  
   - Add structured log entries for:
     - Stage 0 validation success/failure.
     - Challenge sent / Stage 1 success.
     - Re-auth scheduled, deferred, succeeded, failed.
     - Maintenance window extensions.
   - Hook into existing metrics (if present) or add counters/gauges (e.g., `reauth_in_progress`, `maintenance_defer_total`).

7. **Testing**  
   - Unit tests for the remote verifier client (success, failure, timeout).  
   - Tests for Stage 0/Stage 1 flow using fake WebSocket connections.  
   - Re-auth timer tests ensuring grace windows and maintenance deferrals behave as expected.  
   - Hostname normalization and weight handling should continue to pass existing tests.

8. **Documentation & rollout**  
   - Update README / operator guides to explain the new handshake, config fields, and operational controls.  
   - Provide migration notes (even though backward compatibility isn’t required) for clarity.  
   - Optionally add sequence diagrams or state machine visuals.

## Detailed Task Breakdown

| Area | Task |
|------|------|
| Config | Extend `internal/config.Config` with `RemoteVerifierURL`, `VerifierTimeout`, `MaintenanceGraceDefault`, etc. Fail validation if URL is empty. |
| Claims parsing | Define a claims struct (new package) matching the doc (`session_nonce`, `reauth_interval_seconds`, etc.). Include helpers for optional defaults. |
| WebSocket handshake | Implement a mini state machine in `handleBackendConnect` that consumes text frames in order: Stage 0 token → challenge response. |
| Challenge generation | Add helper to create cryptographically random nonces (32 bytes base64) and prevent reuse within an active session. |
| Backend struct | Extend `Backend` to keep attestation metadata and timers. Possibly create a new struct (`AttestedBackend`) to wrap these fields. |
| Re-auth scheduling | Use `time.Timer` or `time.AfterFunc` per backend; ensure timers stop when backend is unregistered. |
| Maintenance poller | Implement asynchronous polling (with jitter and backoff) of `authorizer_status_uri`, respecting `maintenance_grace_cap_seconds`. |
| Grace enforcement | On re-auth failure, ensure all client connections are terminated and the backend is unregistered to avoid dangling routes. |
| Metrics/logs | Standardize log keys (`backend_id`, `nonce`, `reauth_deadline`, etc.). Add metrics instrumentation if the project already collects them. |

## Open Questions / Decisions

- **Token transport format**: Stage 0/Stage 1 messages can be plain text JWTs on the WebSocket, but consider wrapping them in JSON (`{ "type": "token", "stage": 0, "jwt": "..." }`) for extensibility.
- **Remote verifier API contract**: Clarify whether the verifier returns decoded claims, a verified token with embedded claims, or simply a success/failure; adjust client accordingly.
- **Authorizer health endpoint semantics**: Need a concrete schema (e.g., `{"status":"healthy"}`) and authentication method (mTLS, bearer token, or pinned cert).
- **State cleanup**: Decide how long to keep attestation metadata after backend disconnects; likely drop immediately.
- **Backpressure during re-auth**: Currently we plan to let traffic continue; verify no extra buffering is required.

## Suggested Iteration Order

1. Config & remote verifier client.  
2. Stage 0/Stage 1 handshake code, including nonce generation and initial backend registration.  
3. Attested backend metadata + routing integration.  
4. Re-auth timers and maintenance defer logic.  
5. Logging/metrics polish.  
6. Tests and docs updates.

Following this plan should transition Nexus to the new attestation model while keeping responsibilities aligned with the architecture document.

