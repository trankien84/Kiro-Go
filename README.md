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
| `KIRO_DIAG_CHUNKS` | Logs raw and normalized stream chunks; may contain sensitive content | `false` |

Diagnostic logging is disabled by default.

Accepted enabled values: `1`, `true`, `yes`, `on`, `enabled`.

`KIRO_DIAGNOSTICS` does not enable `KIRO_DIAG_CHUNKS`.

## Contributing

Friendly discussion is welcome. If you run into issues, try asking Claude Code, Codex, or similar tools for help first — most problems can be solved that way. PRs are even better.

## Friend Links

- [LINUX DO](https://linux.do)

## Disclaimer

For educational and research purposes only. Not affiliated with Amazon, AWS, or Kiro. Users are responsible for complying with applicable terms of service and laws. Use at your own risk.

## License

[MIT](LICENSE)
