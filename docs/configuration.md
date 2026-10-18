# Configuration and deployment reference

**Languages:** [English](configuration.md) · [Tiếng Việt](configuration.vi.md) · [简体中文](configuration.zh-CN.md)

This is an operator-focused supplement to the [README](../README.md). Values below come from the source implementation and checked-in `docker-compose.yml`; the available model list and provider capabilities are discovered per account rather than guaranteed by a static model name.

## 1. Start and expose the service safely

- Executable: CLI `-host` / `-port` overrides `HOST` / `PORT`, which overrides saved configuration; defaults are `0.0.0.0:8080`. `CONFIG_PATH` defaults to `data/config.json`.
- Checked-in Compose: `KIRO_PORT` publishes container port `8080`; set `KIRO_PORT=127.0.0.1:8080` for a local-only API. By default the host port is exposed on all interfaces. Configure an HTTPS reverse proxy before exposing the API publicly.
- Admin: fresh config uses password `changeme`; override using the **container** `ADMIN_PASSWORD` or change it through Admin. The checked-in Compose file does **not** pass through a host `ADMIN_PASSWORD` automatically. Client API keys are a **separate** gate (`RequireApiKey`); switch it on and create keys before public exposure.
- Secrets: `data/config.json`, imports, cookies/tokens and copies of account exports must remain private. `KIRO_IMPORT_WATCH=1` (enabled in Compose, disabled for bare executable) makes write access to `data/imports` equivalent to credential-import privilege; `KIRO_IMPORT_DIR` chooses the directory.

## 2. SSO and IDE cache variables

| Setting | Behavior |
| --- | --- |
| `KIRO_SSO_CALLBACK_BIND` | **Kiro Hosted SSO listener only**. Default binds `127.0.0.1` / `::1` on port `3128`; Compose uses `0.0.0.0` inside its container so Docker forwarding can reach the listener. **Manual Microsoft SSO does not read or require this setting.** |
| `KIRO_IDE_CACHE` | Fixed **server-side** path of a Kiro IDE credential file; client-provided `path` in the import API is ignored. |
| `KIRO_AWS_SSO_CACHE_DIR` | Host directory mounted read-only to `/host-aws-sso-cache` by Compose, whose container-side `KIRO_IDE_CACHE` targets `kiro-auth-token.json`. |
| `KIRO_PROFILE_REGIONS` | Comma-separated candidate regions for applicable profile/API-key discovery, not a universal override of every provider. |

See [authentication](architecture/authentication.md#2-two-browser-sso-flows-and-callback-ownership) before deciding whether the `3128` port is necessary. The existing Compose port mapping is **preserved for Hosted SSO compatibility**, not because every Microsoft login needs a tunnel.

## 3. Provider-specific options

| Setting | Behavior and caveats |
| --- | --- |
| `BEDROCK_MODEL_MAP` | Optional JSON mapping from a requested model alias to a Bedrock model ID. `Account.BedrockModelMap` takes priority. The adapter also considers account-discovered models, exact-looking Bedrock IDs and built-in convenience fallbacks; a fallback alias does **not** prove model entitlement in the target region. |
| `CUSTOM_API_CREDITS_PER_1K_TOKENS` | Local quota/credit estimate for Custom API forwarding when an upstream credit price is unavailable; default **1.0** credit per 1,000 tokens. This is an operator billing convention, not the upstream's official model price. |
| `LOG_LEVEL` | Overrides saved `logLevel`; supports `debug`, `info`, `warn`, `error` (also accepts `trace`, `warning`, `err` aliases). Default `info` if no recognized value was supplied. |

Provider selection and feature gating are documented in [routing](architecture/provider-routing.md) and [agent protocols](architecture/agent-protocol.md). **Non-Claude Kiro native protocol fidelity is a separate investigation**, not established by this configuration reference.

## 4. Diagnostics, body limits and guard

`KIRO_DIAGNOSTICS` enables the aggregate stream/payload/reasoning diagnostic switches. The individual switches are `KIRO_DIAG_STREAM`, `KIRO_DIAG_PAYLOAD`, `KIRO_DIAG_REASONING`. **`KIRO_DIAG_CHUNKS` is independent** and may reveal prompts or tool content; keep it off unless debugging in a controlled environment. These flags are read at process startup.

`KIRO_MAX_REQUEST_BODY_BYTES` defaults to 64 MiB for the general request body. Some routes enforce stricter limits, including the 32 MiB Messages/count-tokens limit. `KIRO_AGENT_COMPLETION_GUARD` defaults to `strict`, while `KIRO_AGENT_COMPLETION_AUDIT` defaults to `off`; these are independent. See [Completion Guard](architecture/completion-guard.md) and [stream recovery](architecture/streaming-recovery.md).

**Implementation references:** [`main.go`](../main.go), [`config/config.go`](../config/config.go), [`docker-compose.yml`](../docker-compose.yml), [`auth/kiro_sso.go`](../auth/kiro_sso.go), [`auth/microsoft_sso.go`](../auth/microsoft_sso.go), [`proxy/bedrock.go`](../proxy/bedrock.go), [`proxy/custom_api_forward.go`](../proxy/custom_api_forward.go), [`logger/logger.go`](../logger/logger.go), [`diagnostics/flags.go`](../diagnostics/flags.go).
