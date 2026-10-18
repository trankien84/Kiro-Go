# Troubleshooting Kiro-Go

**Languages:** [English](troubleshooting.md) · [Tiếng Việt](troubleshooting.vi.md) · [简体中文](troubleshooting.zh-CN.md)

Start by checking `GET /health` (process readiness), `GET /v1/models` (advertised routes), the account status in Admin and the actual failing request. **A healthy process does not prove an upstream model is usable.** Never paste passwords, authorization codes, raw tokens, or unredacted diagnostic chunks into issue reports.

## 1. Microsoft Enterprise SSO from a remote browser

**Symptom:** browser redirects to `http://localhost:3128` and says the page cannot be reached.

**Manual Microsoft SSO:** this is **expected**. Do not open a tunnel. Copy the **complete** first `http://localhost:3128/signin/callback?...` URL from the address bar into the Admin Microsoft sign-in dialog. Open the Microsoft authorization URL returned by Admin; after sign-in/MFA, copy the **complete** second `http://localhost:3128/oauth/callback?...` URL into Admin. The 10-minute session, callback path and (when supplied) state must be valid. A missing/wrong second-leg state, expired session, incorrect issuer or incomplete query string is not recoverable by editing the URL; restart login.

**Kiro Hosted SSO:** this is a *different* feature. It binds a temporary listener on port `3128` on the **server** and polls for the captured callback. Use a browser on the Docker host/server whose `localhost` resolves to that listener, or explicitly set up a trusted network tunnel. Docker Compose publishes `127.0.0.1:3128:3128` for this mode; **a remote browser's localhost still means the remote machine**. Check for an already occupied `3128` port if listener startup fails.

Do not confuse the two Admin buttons. See [two-flow explanation](architecture/authentication.md#2-two-browser-sso-flows-and-callback-ownership). Device-code login documented for other Kiro CLI identities does **not** imply that Kiro-Go's Microsoft manual flow implements device authorization.

## 2. IDE cache import does not find credentials

The cache is read from **the server/container**, not from the browser's computer. In Compose, verify the host cache mount and `KIRO_AWS_SSO_CACHE_DIR` / `KIRO_IDE_CACHE` path. An HTTP request's `path` field is ignored for security. The file must contain usable refresh material and is read with a bounded size; use a validated CLI JSON import instead if the host has no IDE cache. See [authentication](architecture/authentication.md).

## 3. A model appears in the list but the request fails

Model listing is account- and route-dependent. Check the exact model ID and eligible account, provider route, token validity, model permissions, cooldown, Bedrock region and request-specific tool/reasoning/Responses capabilities. A `400` for incompatible features is not automatically a broken credential; an exhausted pool may return `503`. Do not replace provider IDs with guessed aliases. Consult [provider routing](architecture/provider-routing.md) and [agent protocols](architecture/agent-protocol.md).

## 4. API requests return 401 or quota errors

`ADMIN_PASSWORD` protects Admin; it is **not** the client API key. `RequireApiKey` is the client-auth master switch; when enabled without any usable keys, requests fail closed. A disabled key or missing header can result in `401`; a key exceeding its token/credit limit may return `429`. Check Admin key settings before diagnosing upstream SSO. Exposing the service with default admin password or disabled client auth is unsafe.

## 5. Incomplete streaming or repeated tool output

Distinguish a transport interruption **before** output from an interruption **after** content/tool events have been committed to a client. Kiro-Go prevents unsafe replay after the semantic commit boundary; it should not hide truncation by emitting a normal completion sentinel. Verify the requested protocol and read [stream integrity](architecture/streaming-recovery.md) and [Completion Guard](architecture/completion-guard.md). Prefer summary diagnostics; `KIRO_DIAG_CHUNKS=1` can log sensitive content.

## 6. Docker startup and exposure checks

The supplied Compose file does not automatically pass the host's `ADMIN_PASSWORD` into the container. For local-only API access use `KIRO_PORT=127.0.0.1:8080 docker compose up -d --build`, then test `curl -fsS http://localhost:8080/health`. The Hosted SSO `3128` host binding remains loopback-only, but the in-container listener may be visible to containers on the same Docker network. Set a strong password, enable client-key enforcement and secure volumes before enabling public access. Read [configuration](configuration.md).

**Source entry points:** [`proxy/handler.go`](../proxy/handler.go), [`proxy/auth.go`](../proxy/auth.go), [`auth/microsoft_sso.go`](../auth/microsoft_sso.go), [`auth/kiro_sso.go`](../auth/kiro_sso.go), [`proxy/ide_cache_import.go`](../proxy/ide_cache_import.go), [`proxy/kiro_transport_recovery.go`](../proxy/kiro_transport_recovery.go), [`docker-compose.yml`](../docker-compose.yml).
