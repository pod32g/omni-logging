# Omni-logging Security Audit

Audit date: 2026-06-15
Targets: `http://192.168.68.34:8080` and `http://100.101.214.34:8080`
Scope: repository review plus non-destructive runtime HTTP checks

## Remediation Status

Remediation implemented on 2026-06-15:

- `SEC-003` fixed: builds now require Go 1.25.11; `govulncheck` reports zero reachable vulnerabilities.
- `SEC-004` fixed: explicit header/read/idle timeouts and a 64 KiB header limit are configured.
- `SEC-006` fixed: strict CSP and browser security headers are applied; inline script and remote font dependencies were removed.
- `SEC-007` fixed: metrics are loopback-only by default and liveness output is minimal.
- `SEC-008` fixed in configuration: the image uses distroless nonroot, Compose uses a read-only root filesystem, drops all capabilities, enables no-new-privileges, and limits PIDs. The deployment migrates existing volume ownership. An image build could not be executed on the audit workstation because its Docker daemon was stopped.
- `SEC-009` fixed: formula-like CSV cells are prefixed before export.
- `SEC-010` fixed: base images, the deployment utility image, and GitHub checkout action use immutable digests/SHAs.
- `SEC-011` fixed: public ingest ignores caller-supplied canonical IDs and assigns server IDs; far-future timestamps are rejected.
- `SEC-002` resource-exhaustion risk reduced: admission controls now enforce request-rate, byte-rate, concurrency, and daily-byte quotas.

Deferred with the in-progress authentication design:

- `SEC-001` and the authorization portion of `SEC-002`: anonymous read/ingest remain intentional development behavior.
- `SEC-005`: browser token storage and SSE token transport should be replaced as part of the eventual session/authentication implementation.

## Executive Summary

The deployed service currently allows anonymous log search, statistics, export,
live-tail access, and ingestion. Any device permitted to reach port 8080 on the
LAN or Tailscale network can read potentially sensitive logs and inject data.
This is the highest-priority issue and should be treated as an active exposure,
not merely a hardening recommendation.

The original build used Go 1.25.4 and `govulncheck` found 13 reachable
standard-library vulnerabilities. The remediated build requires Go 1.25.11 and
the follow-up scan found no reachable vulnerabilities. HTTP connection and
header limits are now explicit.

No SQL injection or direct DOM XSS was found in the reviewed paths. SQL values
are parameterized, request bodies are capped at 32 MiB, UI log values are
rendered with text nodes, tokens are compared in constant time, and the runtime
image is minimal. All repository tests passed.

## Critical Findings

### SEC-001: Anonymous log read, export, and live-tail access

- Rule ID: `GO-AUTHZ-001`
- Severity: Critical
- Location: `internal/api/middleware.go:119-143`, `internal/api/server.go:110-113`, `docker-compose.yml:14-17`
- Evidence: `requireAdmin` bypasses authentication when `AdminToken` is empty. Compose defaults `OMNILOG_ADMIN_TOKEN` to an empty value. Runtime requests to `/api/v1/search?limit=1` and `/api/v1/search/stats?last=15m` returned `200` without credentials on both reachable addresses.
- Impact: Any reachable client can retrieve, export, and continuously monitor logs. Logs commonly contain credentials, personal data, internal addresses, stack traces, and operational details.
- Fix: Require a non-empty admin credential at startup whenever the listener is not loopback. Remove the empty compose default, provision the secret through a protected secret source, and fail closed when it is missing.
- Mitigation: Until redeployed, restrict port 8080 with host firewall and Tailscale ACLs to specific administrator identities/devices.
- False positive notes: Network ACLs may reduce who can connect, but they do not provide application-level authorization. Runtime access from this audit host was confirmed.

### SEC-011: Client-supplied IDs permit silent overwrite of existing logs

- Rule ID: `GO-AUTHZ-001`, `GO-INTEGRITY-001`
- Severity: Critical
- Location: `internal/model/event.go:73-75`, `internal/store/sqlite/sqlite.go:132-144`
- Evidence: Ingest accepts the caller's `id` field. Storage uses `ON CONFLICT(id) DO UPDATE` and replaces every substantive event field. A live test created an audit-owned event, reposted the same ID with different timestamp, source, service, level, message, and attributes, and confirmed that the original was no longer searchable while the replacement was returned.
- Impact: Because anonymous search exposes event IDs, any reachable client can select an existing record and silently rewrite historical evidence. This defeats log integrity and can conceal incidents or frame trusted systems.
- Fix: Do not accept externally supplied canonical IDs on the public ingest API. Generate IDs server-side and use a separate internal idempotency key scoped to an authenticated producer. Reject duplicate IDs instead of updating historical rows unless the write is an authenticated WAL recovery operation.
- Mitigation: Immediately require ingest authentication and restrict network access. Add append-only audit records for duplicate/rejected IDs and integrity-sensitive operations.
- False positive notes: The live test modified only audit-created IDs. The same code path applies to any existing ID returned by the anonymous search endpoint.

## High Findings

### SEC-002: Anonymous ingestion enables log poisoning and storage exhaustion

- Rule ID: `GO-AUTHZ-001`, `GO-RES-001`
- Severity: High
- Location: `internal/api/middleware.go:97-117`, `internal/api/server.go:106-109`, `docker-compose.yml:14-17`
- Evidence: `requireIngestKey` bypasses authentication when no keys are configured. Compose defaults `OMNILOG_INGEST_KEYS` to empty. Empty POST requests to both ingest endpoints returned `200` without credentials. At audit time there was no client/key rate limit or quota; the remediated branch now includes admission controls, while anonymous ingest remains intentionally deferred.
- Impact: A reachable attacker can forge operational evidence, pollute searches and alerts, consume disk/WAL capacity, and spend CPU on parsing/indexing. The 32 MiB per-request cap does not bound repeated requests.
- Fix: Require at least one ingest key for non-loopback listeners and add per-key request/byte quotas. Return a startup error when production-facing auth is absent.
- Mitigation: Restrict network access and add reverse-proxy request-rate and body-rate limits.
- False positive notes: The audit sent only empty bodies and did not modify stored data; endpoint authorization failure is still confirmed by the `200` responses.

### SEC-003: Reachable vulnerabilities in Go 1.25.4 standard library

- Rule ID: `GO-DEPLOY-001`
- Severity: High
- Location: `go.mod:3`, `Dockerfile:2`, HTTP entry point `cmd/omnilog/main.go:192-209`
- Evidence: `go version` reported `go1.25.4`. `govulncheck ./...` found 13 reachable standard-library vulnerabilities. Notable server-relevant findings include `GO-2026-4341` (query parsing memory exhaustion), `GO-2026-5039` (`net/textproto` error handling), and TLS/HTTP DoS issues. The latest required fix among the reported findings is Go 1.25.11.
- Impact: Crafted network input may cause excessive memory/CPU use, connection retention, incorrect parsing, or process instability depending on the enabled HTTP/TLS path.
- Fix: Build and deploy with Go 1.25.11 or newer supported patched release, then rerun `govulncheck ./...`. Pin the exact approved patch version in `go.mod`, Dockerfile, and CI.
- Mitigation: Limit network access and apply proxy-level header/query limits until the rebuilt binary is deployed.
- False positive notes: Some reported traces are client-side or TLS-only, but `govulncheck` confirmed reachable symbols and `GO-2026-4341` applies to request query parsing used by the server.

### SEC-004: HTTP server lacks timeouts and header limits

- Rule ID: `GO-HTTP-001`
- Severity: High
- Location: `cmd/omnilog/main.go:192`
- Evidence: The server is constructed only with `Addr` and `Handler`; `ReadHeaderTimeout`, `ReadTimeout`, `IdleTimeout`, and `MaxHeaderBytes` remain zero/default. Long-lived SSE and exports need deliberate exceptions rather than globally unbounded settings.
- Impact: Slow headers, idle connections, and oversized request headers can retain file descriptors, goroutines, and memory, causing denial of service.
- Fix: Configure `ReadHeaderTimeout`, `IdleTimeout`, and `MaxHeaderBytes`. Use endpoint-aware response handling for SSE/export so a global `WriteTimeout` does not break legitimate streams.
- Mitigation: Enforce connection, header, and idle limits at a trusted reverse proxy.
- False positive notes: No reverse proxy configuration is present in the repository; verify whether one exists outside this deployment.

## Medium Findings

### SEC-005: Admin token is exposed to browser JavaScript and URL-based transport

- Rule ID: `JS-STORAGE-001`, `GO-CONFIG-001`
- Severity: Medium
- Location: `internal/web/dist/app.js:27-28`, `internal/web/dist/app.js:314-321`, `internal/api/middleware.go:129-137`
- Evidence: The admin token is persisted in `localStorage`. Live tail sends it as a `token` query parameter because `EventSource` cannot set an Authorization header. The server also accepts token query parameters and cookies.
- Impact: Any same-origin XSS can read the long-lived admin token. Query tokens may appear in proxy/network tooling and request URLs. A leaked token grants complete log read/export access.
- Fix: Replace the static browser token with an `HttpOnly`, `SameSite` session cookie and authenticated session establishment. For SSE, authenticate with the cookie or use a short-lived, narrowly scoped stream ticket.
- Mitigation: Rotate the current token, deploy CSP, avoid query-token support outside a short-lived SSE ticket, and keep the UI on a dedicated origin.
- False positive notes: The current UI renders log fields with text nodes, so no direct stored-XSS sink was found. This finding concerns defense against future or third-party script compromise.

### SEC-006: Browser security headers are absent

- Rule ID: `GO-HTTP-004`, `JS-CSP-001`
- Severity: Medium
- Location: `internal/api/server.go:102-122`, `internal/web/dist/index.html:8-18`
- Evidence: Runtime `/` responses had no `Content-Security-Policy`, `X-Content-Type-Options`, `X-Frame-Options`, `Referrer-Policy`, or `Permissions-Policy`. The page includes an inline theme script and remote Google Fonts, which must be accounted for in CSP.
- Impact: A future injection bug has fewer browser-enforced containment controls; framing and content-type confusion protections are also absent.
- Fix: Add centralized security-header middleware. Move or hash/nonce the inline script and deploy a restrictive CSP such as same-origin scripts, explicit font/style sources, `object-src 'none'`, and `frame-ancestors 'none'`.
- Mitigation: Set equivalent headers at the reverse proxy.
- False positive notes: Headers may be supplied by an unseen edge, but direct runtime responses on port 8080 did not contain them.

### SEC-007: Metrics and detailed health data are publicly exposed

- Rule ID: `GO-DEPLOY-002`
- Severity: Medium
- Location: `internal/api/server.go:114-116`, `internal/api/handlers.go:62-93`
- Evidence: `/metrics` and `/api/v1/healthz` require no authorization. Runtime `/metrics` returned `200` with roughly 12 KiB of build, traffic, ingest, query latency, queue, and subscriber metrics.
- Impact: Reachable clients can profile traffic volume, software version, load, queue pressure, and operational behavior, improving reconnaissance and timing of resource-exhaustion attacks.
- Fix: Bind metrics to an internal listener or require a separate monitoring credential/network policy. Keep liveness output minimal.
- Mitigation: Restrict these paths at the proxy/firewall.
- False positive notes: Unauthenticated liveness is normal, but detailed ingest and subscriber metrics are unnecessary for a public probe.

### SEC-008: Container runs without explicit non-root or runtime hardening

- Rule ID: `GO-DEPLOY-003`
- Severity: Medium
- Location: `Dockerfile:12-18`, `docker-compose.yml:5-26`
- Evidence: The Dockerfile comment says non-root, but it uses `gcr.io/distroless/static-debian12` without a `USER` directive or the `:nonroot` image variant. Compose does not set `read_only`, `cap_drop`, `no-new-privileges`, PID, memory, or CPU limits, and publishes port 8080 on all host interfaces by default.
- Impact: A process compromise has root identity inside the container and fewer containment barriers; resource exhaustion can affect the host more directly.
- Fix: Use the distroless non-root variant or explicit numeric `USER`, make the root filesystem read-only, drop all capabilities, enable `no-new-privileges`, and add appropriate resource limits. Bind the published port to the intended interface only.
- Mitigation: Enforce equivalent restrictions in the container runtime and host firewall.
- False positive notes: Runtime policy outside Compose may add controls, but none are visible in the repository.

### SEC-009: CSV export permits spreadsheet formula injection

- Rule ID: `GO-OUTPUT-001`
- Severity: Medium
- Location: `internal/api/export.go:81-105`
- Evidence: Untrusted `service`, `source`, `message`, and serialized attribute values are written directly as CSV cells. CSV quoting does not neutralize leading `=`, `+`, `-`, `@`, tab, or carriage-return formula markers in spreadsheet applications.
- Impact: Opening an exported CSV in a spreadsheet may execute attacker-controlled formulas, potentially causing data exfiltration or unsafe external links depending on client settings.
- Fix: For CSV output, prefix formula-like cells with a single quote or provide a safe export mode that neutralizes spreadsheet formulas. Document that NDJSON/JSON preserve raw values.
- Mitigation: Warn administrators not to open untrusted CSV exports directly in spreadsheet software.
- False positive notes: Exploitability depends on the spreadsheet application and security settings.

## Low Findings

### SEC-010: CI and image dependencies are not immutable

- Rule ID: `GO-SUPPLY-002`
- Severity: Low
- Location: `.github/workflows/cicd.yml:23,35`, `Dockerfile:2,13`
- Evidence: GitHub Actions use the mutable `actions/checkout@v5` tag, and build/runtime images are referenced by mutable tags rather than digests.
- Impact: An upstream tag compromise or unexpected image update can change trusted build inputs.
- Fix: Pin actions to full commit SHAs and container bases to reviewed digests, with an automated update process.
- Mitigation: Keep branch protection and review dependency updates.
- False positive notes: Mutable official tags are common operationally, but they are weaker than immutable references for a self-hosted deployment pipeline.

## Verification Performed

- Confirmed both LAN and Tailscale health endpoints return `200`.
- Confirmed anonymous `200` responses for search, stats, metrics, structured ingest, and raw ingest.
- Confirmed arbitrary `source`, `service`, `level`, timestamp, and attributes are persisted from unauthenticated requests.
- Confirmed a second ingest with the same client-supplied ID silently overwrites the prior event.
- Confirmed CSV export preserves a leading `=` formula marker in an attacker-controlled message.
- Confirmed common browser security headers are absent on `/`.
- Confirmed `/debug/pprof/` and `/debug/vars` return `404`.
- Ran `go test ./...`: all tests passed.
- Ran `govulncheck ./...`: 13 reachable standard-library vulnerabilities found; command exited non-zero as expected.
- Reviewed SQL construction: user values are bound parameters; fixed internal identifiers are the only formatted SQL identifiers.
- Reviewed frontend rendering: persisted log values use `textContent`/text nodes; no `innerHTML`, `eval`, or `new Function` sink was found.

## Recommended Remediation Order

1. Immediately restrict port 8080 and configure non-empty admin and ingest credentials.
2. Stop public ingest from accepting canonical IDs and reject attempts to overwrite existing records.
3. Upgrade and redeploy with Go 1.25.11 or a newer supported patched release.
4. Add HTTP server limits and application/proxy rate limiting.
5. Replace browser static-token storage/query transport with secure sessions or short-lived SSE tickets.
6. Protect metrics, add security headers, harden the container runtime, and neutralize CSV formulas.
