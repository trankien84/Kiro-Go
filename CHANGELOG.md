# Changelog

All notable changes to this project are documented here. The format loosely
follows [Keep a Changelog](https://keepachangelog.com/).

## [Unreleased]

### Added
- **Import Microsoft 365 / Entra ID (Azure AD) credentials from the login helper.**
  Three converging paths now load a `CLIProxyAPI_*.json` file produced by
  `kiro-login-helper.py`, all funnelling through one `h.importCredential` core so
  the persisted account gets the exact same Microsoft-vs-generic classification
  and the exact same account fields as an interactive Enterprise SSO login:
  - `apiImportCredentials` (`POST /admin/api/auth/credentials`) now accepts the
    helper's native snake_case keys (`token_endpoint`, `issuer_url`, `scopes`,
    `profile_arn`) in addition to the existing camelCase payload.
  - New `POST /admin/api/auth/import-cli-json` endpoint ingests a single helper
    object, a JSON array, a `{ "files": [...] }` / `{ "accounts": [...] }` wrapper,
    or raw text with several objects, returning per-item results.
- `KIRO_IMPORT_WATCH` / `KIRO_IMPORT_DIR` zero-touch drop-folder watcher
  (`data/imports/`): valid files are imported within ~15s then moved to
  `processed/`, invalid ones to `failed/` with a `.error.txt` sidecar. Enabled by
  default in `docker-compose.yml`.
- Admin panel file picker for uploading helper JSON; `app.js` credential parsing
  now maps both snake_case and camelCase.
- `testdata/CLIProxyAPI_sample_external_idp.json` sanitized fixture.
- **Import directly from the Kiro IDE cache (no browser, no helper script).** New
  `POST /admin/api/auth/import-ide-cache` endpoint and an admin-panel button read the
  credential the Kiro IDE already cached on the host
  (`~/.aws/sso/cache/kiro-auth-token.json`, overridable via `KIRO_IDE_CACHE` or a `path`
  body field) and import it through the same core. The cache's camelCase keys map
  straight onto the existing decoder; its stale `expiresAt` is ignored in favor of
  the mandatory refresh.
- **Configurable listen port/host.** New `-port` / `-host` CLI flags and `PORT` /
  `HOST` env overrides (precedence: flag > env > `config.json`), so the proxy can run
  on a port other than `8080` without editing the config. `docker-compose.yml` honors
  `KIRO_PORT` for the published host port (`KIRO_PORT=9090 docker compose up -d`).

### Fixed
- An `external_idp` import that omits `token_endpoint`/`client_id` (and has no
  `userId`/`accessToken` to derive them from) is now rejected up front with an
  actionable message instead of an opaque refresh failure.

### Security
- The account email is stored as a label only; the password is never persisted or
  sent upstream. Microsoft 365 tenants enforce MFA / Conditional Access, so a headless
  ROPC password grant is not a supported auth path — the interactive helper (browser
  PKCE) remains the canonical credential-mint path.
- Classification of an `external_idp` credential as genuinely Microsoft (strict
  tenant-bound refresh) vs. generic (loose allow-list only) happens in exactly one
  place (`h.importCredential`), shared by every import path — the legacy JSON
  endpoint, the cli-json batch endpoint, the IDE-cache endpoint, and the directory
  watcher all funnel through it, so none of them can drift from that decision.
