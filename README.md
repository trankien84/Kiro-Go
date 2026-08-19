# Kiro-Go

[![Go Version](https://img.shields.io/badge/Go-1.21+-00ADD8?style=flat&logo=go)](https://go.dev/)
[![Docker](https://img.shields.io/badge/Docker-Ready-2496ED?style=flat&logo=docker)](https://www.docker.com/)
[![License](https://img.shields.io/badge/License-MIT-green.svg)](LICENSE)

Convert Kiro accounts to OpenAI / Anthropic compatible API service.

[English](README.md) | [中文](README_CN.md) | [Tiếng Việt](README_VI.md)

If this project helps you, a Star would mean a lot.

## Features

- Anthropic `/v1/messages`, OpenAI `/v1/chat/completions` & OpenAI `/v1/responses`
- Multi-account pool with round-robin load balancing
- Auto token refresh, SSE streaming, Web admin panel
- Multiple auth: AWS Builder ID, IAM Identity Center (Enterprise SSO), Microsoft Enterprise SSO, SSO Token, local cache, credentials JSON, Kiro API Key
- Usage tracking, account import/export, i18n (CN / EN / VI)
- Support configuring outbound proxy (SOCKS5 / HTTP)

## Quick Start

### Docker Compose (Recommended)

```bash
git clone https://github.com/zsecducna/Kiro-Go.git
cd Kiro-Go
mkdir -p data
docker-compose up -d
```

### Docker Run

```bash
docker run -d \
  --name kiro-go \
  -p 8080:8080 \
  -e ADMIN_PASSWORD=your_secure_password \
  -v /path/to/data:/app/data \
  --restart unless-stopped \
  ghcr.io/zsecducna/kiro-go:latest
```

### Build from Source

```bash
git clone https://github.com/zsecducna/Kiro-Go.git
cd Kiro-Go
go build -o kiro-go .
./kiro-go

# Run on a different port (flag > PORT env > config.json):
./kiro-go -port 9090
# or: PORT=9090 ./kiro-go
```

### Deploy on Zeabur

The repo already includes a `Dockerfile`, so it builds and runs on Zeabur out of the box.

**Option 1: Dashboard (one-click)**

1. Fork this repo to your GitHub account.
2. In Zeabur, create a new service and choose **Deploy from GitHub**, then select your fork.
3. Zeabur auto-detects the `Dockerfile` and builds the image.
4. In the **Networking** tab, expose port `8080` and bind a domain.
5. In the **Variables** tab, set at least `ADMIN_PASSWORD` (admin panel password).
6. Mount a Volume at `/app/data` if you want accounts / config to survive redeploys.

**Option 2: CLI**

```bash
npm i -g zeabur
zeabur auth login
zeabur deploy
```

> Run the commands from the project root. The CLI writes `.zeabur/context.json` to remember the target project / service — it contains personal IDs, so don't commit it.

Once the service is up, open `https://<your-domain>/admin` to log in.

Config is auto-created at `data/config.json`. Mount `/app/data` for persistence. The default admin password is `changeme` — override it via the `ADMIN_PASSWORD` env var or change it in the admin panel before going to production.

## Usage

Open `http://localhost:8080/admin`, log in, add accounts, then call the API:

```bash
# Claude
curl http://localhost:8080/v1/messages \
  -H "Content-Type: application/json" \
  -H "anthropic-version: 2023-06-01" \
  -d '{"model":"claude-sonnet-4.5","max_tokens":1024,"messages":[{"role":"user","content":"Hello!"}]}'

# OpenAI
curl http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer any" \
  -d '{"model":"gpt-4o","messages":[{"role":"user","content":"Hello!"}]}'
```

### Add a Kiro API Key account

In the admin panel, choose **API Key** when adding an account and paste `ksk_...` (or `ksk_...|region`).

You can also import via the credentials API:

```bash
curl -X POST http://localhost:8080/admin/api/auth/credentials \
  -H "Content-Type: application/json" \
  -H "Cookie: <admin-session>" \
  -d '{"kiroApiKey":"ksk_your_key|us-east-1","authMethod":"api_key","nickname":"cli-key"}'
```

API Key accounts call the Kiro CLI runtime (`https://runtime.{region}.kiro.dev/`) with `tokentype: API_KEY`. They skip OAuth refresh and do not use `profileArn`.

## Importing Microsoft 365 / Entra ID (Azure AD) credentials

Enterprise SSO (Microsoft 365 / Entra ID) accounts are neither AWS Builder ID nor
IAM Identity Center accounts. Besides the interactive **Enterprise SSO** flow in the
admin panel, credentials can also arrive as a `CLIProxyAPI_<user>.json` file minted by
an external helper (`auth_method: external_idp`). There are four ways to get an
account in:

1. **Interactive Enterprise SSO** (admin panel → Add Account → Enterprise SSO). The
   backend drives the hosted sign-in portal and polls until the account is created.

2. **Paste / upload the helper JSON in the admin panel.** The Enterprise SSO card's
   file picker and paste box accept the helper's native `CLIProxyAPI_*.json`
   verbatim — snake_case keys (`token_endpoint`, `issuer_url`, `scopes`,
   `profile_arn`) are understood, alongside the existing camelCase API shape.

3. **API.** `POST /admin/api/auth/import-cli-json` accepts a single helper object, a
   JSON array, a `{ "files": ["<json>", ...] }` / `{ "accounts": [...] }` wrapper, or
   raw text with several objects. It returns per-item results.

   ```bash
   curl -X POST http://localhost:8080/admin/api/auth/import-cli-json \
     -H "X-Admin-Password: $ADMIN_PASSWORD" \
     --data-binary @CLIProxyAPI_user.json
   ```

4. **Zero-touch drop folder (Docker).** With `KIRO_IMPORT_WATCH=1` (set by default in
   `docker-compose.yml`), any `CLIProxyAPI_*.json` placed in `data/imports/` is imported
   within ~15s, then moved to `data/imports/processed/` (or `failed/` with a `.error.txt`
   sidecar). Imports go through the same persisted path the running server owns, so they
   never race the in-memory config.

5. **Import from the Kiro IDE cache (no browser, no helper).** If the Kiro IDE is already
   signed in on the same host as the proxy, it keeps a live credential at
   `~/.aws/sso/cache/kiro-auth-token.json`. The admin panel's Enterprise SSO card has an
   **Import from Kiro IDE (this host)** button, or call the API directly:

   ```bash
   curl -X POST http://localhost:8080/admin/api/auth/import-ide-cache \
     -H "X-Admin-Password: $ADMIN_PASSWORD"
   # custom location: -d '{"path":"/path/to/kiro-auth-token.json"}'
   ```

   The proxy reads the file **server-side**, so this works only when the IDE and the proxy
   share a host — or, in Docker, when the host AWS SSO cache directory is mounted into the
   container. The Compose file does this portably for Linux/macOS with
   `${HOME}/.aws/sso/cache:/host-aws-sso-cache:ro`; override `KIRO_AWS_SSO_CACHE_DIR` if
   your Kiro IDE uses a different location. The cache's stale `expiresAt` is ignored: the
   import performs a mandatory refresh, so the persisted expiry always comes from a fresh
   upstream response.

Every path above funnels through the same internal core, so an account imported via
JSON, the IDE cache, or the drop folder gets identical Microsoft-vs-generic
classification (strict tenant-bound refresh vs. loose allow-list-only refresh) to one
created through the interactive flow.

> The account email is stored as a label only. The password is **never** persisted or
> sent upstream — Microsoft 365 tenants enforce MFA / Conditional Access, so a headless
> password (ROPC) grant is not a reliable auth path.

## Thinking Mode

Append a suffix (default `-thinking`) to the model name, e.g. `claude-sonnet-4.5-thinking`. Claude-compatible requests that include a top-level `thinking` config such as `{"type":"enabled","budget_tokens":2048}` or `{"type":"adaptive"}` also enable thinking mode automatically. Configure output format in the admin panel under Settings - Thinking Mode.

## Outbound Proxy

For users in restricted network regions, configure an outbound proxy in the admin panel under **Settings - Outbound Proxy Settings**. Supports SOCKS5 and HTTP proxies.

The setting takes effect immediately without restarting.

## Environment Variables

| Variable | Description | Default |
|----------|-------------|---------|
| `CONFIG_PATH` | Config file path | `data/config.json` |
| `ADMIN_PASSWORD` | Admin panel password (overrides config) | - |
| `PORT` | HTTP listen port (overrides config; `-port` flag wins over this) | `8080` |
| `HOST` | HTTP bind host (overrides config; `-host` flag wins over this) | `0.0.0.0` |
| `KIRO_IMPORT_WATCH` | Enable the `data/imports/` auto-ingest watcher (`1`/`true`) | off (on in Docker) |
| `KIRO_IMPORT_DIR` | Directory the watcher scans for `CLIProxyAPI_*.json` | `data/imports` |
| `KIRO_IDE_CACHE` | Path to the Kiro IDE credential cache for `import-ide-cache` | `~/.aws/sso/cache/kiro-auth-token.json` (Docker: `/host-aws-sso-cache/kiro-auth-token.json`) |
| `KIRO_AWS_SSO_CACHE_DIR` | Host AWS SSO cache directory mounted by Docker Compose for IDE-cache import | `$HOME/.aws/sso/cache` |
| `KIRO_PROFILE_REGIONS` | Comma-separated fallback regions for external_idp profile probing | `us-east-1,eu-central-1` |
| `KIRO_DIAGNOSTICS` | Enables stream, payload, and reasoning diagnostics | `false` |
| `KIRO_DIAG_STREAM` | Logs aggregate stream metrics | `false` |
| `KIRO_DIAG_PAYLOAD` | Logs payload size and truncation metrics | `false` |
| `KIRO_DIAG_REASONING` | Logs reasoning capabilities and forwarded fields | `false` |
| `KIRO_DIAG_CHUNKS` | Logs raw and emitted stream chunks; may contain sensitive content | `false` |
| `KIRO_ASSISTANT_CHUNK_MODE` | Assistant text decoder: `delta`, `cumulative`, or opt-in `legacy` compatibility | `delta` |
| `KIRO_REASONING_CHUNK_MODE` | Reasoning text decoder: `delta`, `cumulative`, or opt-in `legacy` compatibility | `delta` |
| `KIRO_SYNTHETIC_THINKING_SIGNATURE` | Synthetic compatibility signature fallback: `auto`, `always`, or `never` | `auto` |
| `KIRO_AGENT_COMPLETION_GUARD` | Completion guard policy: `off`, `observe`, `retry`, or `strict` | `strict` |
| `KIRO_AGENT_COMPLETION_DELIVERY` | Response delivery: `auto`, `buffered`, or `deferred-stream` | `auto` |
| `KIRO_AGENT_COMPLETION_GUARD_MAX_RETRIES` | Maximum recovery retries for unusable or truncated responses | `2` |
| `KIRO_AGENT_COMPLETION_GUARD_HEARTBEAT_MS` | SSE heartbeat interval while a non-stream guard waits on an internal request | `10000` |
| `KIRO_AGENT_COMPLETION_GUARD_MAX_BUFFER_BYTES` | Shared retained-data accounting budget for each guard attempt, including pending/raw SSE frames and parsed semantic state | `16777216` |
| `KIRO_MAX_REQUEST_BODY_BYTES` | General JSON front-door ceiling. Messages and token counting are additionally capped at 32 MiB, and the selected transport may impose a lower effective limit (for example Bedrock). | `67108864` |
| `KIRO_NATIVE_TOOL_CHOICE` | Kiro-native forced tool choice: `never` never sends the field, `auto` uses prompt-only, `always` opts into the experimental field with prompt-only fallback on rejection | `auto` |
| `KIRO_AGENT_COMPLETION_AUDIT` | Semantic completion audit: `off`, `observe`, or `enforce` | `off` |
| `KIRO_AGENT_COMPLETION_AUDIT_MIN_CONFIDENCE` | Minimum confidence required before audit continuation | `95` |

Completion guard delivery behavior:

- `auto`: use deferred-finalization streaming only when the model is known not to support mid-stream refusal, and buffered verification for `stream=false`. Claude 4+ requests, unrecognized Claude model aliases, and requests using server-side `fallbacks` are forced to buffered delivery so a terminal `refusal` can discard partial output before any bytes reach the client. Known pre-Claude-4 models may still use deferred delivery when no fallback is configured.
- `deferred-stream`: for `stream=true`, forward ordinary text and thinking immediately until a structured tool turn begins, then retain the complete structured remainder until every block, tool ID, tool input, server-result pair, and terminal field has been validated. Mixed server/client, server-tool, and MCP turns are therefore committed or discarded as one unit; a malformed later client tool cannot leave an earlier server-tool block visible. Non-stream requests remain buffered.
- `buffered`: keep the original strict behavior even when the client requested streaming; the complete response is verified first and then replayed as SSE.
- Deferred streaming validates the complete SSE lifecycle (`message_start`, sequential content blocks, one or more cumulative `message_delta` events, and one `message_stop`). Malformed JSON, incomplete frames, invalid delta types, reused or parallel block indexes, invalid tool input, out-of-order terminal events, and content after an error are not forwarded as successful completion.
- Client `tool_use` blocks remain atomic. Server-tool calls and results are paired by `id`/`tool_use_id`; a pending server tool is accepted only for a mixed client-tool turn ending in `tool_use`, or is continued safely from matching prior history.
- Before any visible content is committed, deferred streaming may retry HTTP 408, HTTP 429, HTTP 5xx, retryable SSE errors, and malformed upstream streams. Deterministic 4xx errors are not retried. After visible content is committed, recovery continues the open stream instead of replaying the response from the beginning.
- Deferred streaming drops the pre-message raw HTTP/SSE body as soon as `message_start` is validated. Pending frames, deferred `message_start`, the atomic structured-turn buffer, partial tool JSON, and parsed semantic/raw metadata share one configurable retained-memory budget. Ownership is transferred instead of copied where possible; parser-only maps/builders are released immediately after classification, and every terminal/retry/error path clears its remaining buffers and response references.
- Recovery suppresses incomplete internal thinking, trims only meaningful token-boundary overlap, and builds ordinary recovery history solely from content already committed to the client. `pause_turn` and paused compaction replay the complete assistant state exactly as required, including mixed server/client turns, pending server tools, MCP blocks, signed thinking, the response `container`, provider-specific block metadata, and unknown top-level Messages request fields. Server-side fallback boundaries are preserved in place; pre-boundary thinking, redacted thinking, connector narration, client tool calls, and unpaired server-tool calls are removed according to the fallback continuation contract, while all blocks after the final fallback boundary remain ordered. Signed-thinking continuations and Anthropic-defined client tools such as Bash, Computer, Text Editor, and Memory may use transparent `custom_api` or Claude-native Bedrock transports. Server-tool, MCP, unknown structured continuations, and unmodeled protocol-bearing Messages fields are direct-Messages-only: native Kiro and Bedrock accounts are skipped unless a future per-account capability matrix explicitly proves support. This is required for MCP because the Anthropic MCP connector is unavailable through Amazon Bedrock, and it avoids silently routing other server tools to a platform that may not expose that tool family.
- Usage from rejected hidden retries and audit attempts is logged internally and is not added to client-visible usage. When a continuation or recovery contributes blocks that are actually emitted, its input/cache usage and structured usage metadata are merged, while visible output is capped by a bounded token estimate. Fallback `usage.iterations`, server-tool usage, citations metadata, response `stop_details`, and current/future unknown usage fields survive buffering and SSE replay. Recorders transfer ownership with `takeResult`, release backing storage immediately, and every streaming attempt clears parser state and deferred buffers on all terminal paths. The limit is an accounting guard for retained payload data, not an exact Go heap ceiling: maps, strings, and runtime object overhead may consume additional memory.
- Kiro assistant history has no wire representation for Claude `thinking`, server-tool, or MCP blocks. The translator therefore marks the payload for Claude-native routing, retains a non-serialized reference to the parsed request plus per-turn sidecars for diagnostics/tests, and transparent transports receive the original raw Messages request. Custom JSON preservation on request, response, usage, tool, and content-block types prevents guard clones or buffered SSE replay from dropping fields such as `mcp_servers`, `server_name`, `is_error`, `max_uses`, `container`, `stop_details`, fallback `iterations`, citations, server-tool usage, or future provider metadata. The current documented streaming matrix covers text, connector narration, citations, tool input, thinking/signature, compaction, fallback boundaries, terminal metadata, and errors; unknown future protocol events fail closed or remain opaque rather than being silently reinterpreted. Ordinary malformed-stream recovery still restarts from committed visible output rather than replaying incomplete signed thinking.
- Guard mode `observe` is a transparent passthrough: it does not defer, validate, retry, or rewrite the response. Use diagnostics or upstream logs when byte-for-byte behavior is required.

Completion audit behavior:

- `off`: prose responses are classified from their protocol stop reason only.
- `observe`: an internal audit request is made and logged, but it never triggers a continuation.
- `enforce`: a high-confidence `needs_action` audit may trigger one internal continuation. If that continuation fails or does not return a real tool call, Kiro-Go returns the original candidate response instead of returning a retry-exhausted error.

Diagnostic logging is disabled by default.

Accepted enabled values: `1`, `true`, `yes`, `on`, `enabled`.

`KIRO_DIAGNOSTICS` does not enable `KIRO_DIAG_CHUNKS`.

Chunk modes are explicit because chunk content alone cannot reliably distinguish a new delta from a replayed or cumulative snapshot:

- `delta`: emit every chunk unchanged. This is the non-destructive default for `assistantResponseEvent`.
- `cumulative`: emit only monotonic snapshot growth. If the stream violates that contract, the decoder logs a warning, switches to `delta`, and preserves the current chunk.
- `legacy`: retain the previous prefix/suffix overlap heuristic as an opt-in compatibility mode. Assistant and reasoning text both default to `delta` so valid short chunks are not removed by heuristic overlap detection.

Transparent Claude-native transports preserve provider-issued thinking signatures unchanged. Kiro-native responses use a proxy-owned provenance signature instead, because provider-signed thinking state is not serialized into Kiro assistant history. This keeps a tool-result continuation on Kiro instead of misclassifying it as foreign provider state or forwarding a proxy signature to Direct Anthropic/Bedrock.

`KIRO_SYNTHETIC_THINKING_SIGNATURE` controls only the local compatibility fallback:

- `auto`: synthesize only when visible thinking text exists.
- `always`: also allow a fallback for empty or omitted thinking.
- `never`: never synthesize a local signature.

The setting does not disable Kiro provenance localization when Kiro itself emitted a provider signature; that localization is required for transport affinity. Both provenance and fallback signatures are generated by Kiro-Go and are not provider-issued Claude signatures.

Structured JSON output (`output_config.format`) is never silently dropped on Kiro. Kiro-Go forwards it through `additionalModelRequestFields` only when a live model snapshot from a genuine Kiro credential advertises `output_config.format`; otherwise Kiro is removed as a candidate and a transparent/native transport must handle the request.

## Contributing

Friendly discussion is welcome. If you run into issues, try asking Claude Code, Codex, or similar tools for help first — most problems can be solved that way. PRs are even better.

## Friend Links

- [LINUX DO](https://linux.do)

## Disclaimer

For educational and research purposes only. Not affiliated with Amazon, AWS, or Kiro. Users are responsible for complying with applicable terms of service and laws. Use at your own risk.

## License

[MIT](LICENSE)
