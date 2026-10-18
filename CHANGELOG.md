# Changelog

Notable changes to Kiro-Go are recorded here for people deploying, using, and reviewing the project. The entries
follow [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) conventions; they describe **final behavior**,
not every intermediate commit or refactor.

## Unreleased

> **Implementation history:** This entry consolidates **33 commits** from the reviewed
> implementation snapshot, after merge base
> [`76a663a`](https://github.com/trankien84/Kiro-Go/commit/76a663a6992e409d0381ec3ba7161b96611afd90),
> through implementation HEAD
> [`63d4e2bb`](https://github.com/trankien84/Kiro-Go/commit/63d4e2bb4375d85aed46e72d1d7c5df25138fa41).
> This includes incorporated upstream work and subsequent fork-specific hardening. No new release is implied;
> `version.json` currently reads `1.1.5`.
>
> **Compatibility:** API endpoint availability is not the same as feature compatibility. Tool protocols,
> thinking, structured output, stateful Responses, and streaming semantics are checked against the **selected
> account and transport**. Requests with unsupported required semantics are rejected or routed to a compatible
> transport instead of silently dropping meaning.

### Added

- **Account routing policies:** `round-robin` (default), `priority`, and `fill-first`, including per-account
  priority and order controls in the admin UI. The scheduler applies these policies **only after** credential,
  quota, model, cooldown, circuit-breaker, and protocol eligibility checks. `priority` shares the best eligible
  tier through the existing selector; `fill-first` chooses the first eligible account in that tier. ([routing work](https://github.com/trankien84/Kiro-Go/commit/63d4e2bb4375))
- **Provider-aware model registry:** immutable per-request provider/model evidence, exact upstream model IDs,
  capability-based public model availability, and account-lifecycle-scoped discovery/denial/cooldown state. A
  selected model does not change underneath an in-flight request because a background refresh published a new
  registry. ([provider hardening](https://github.com/trankien84/Kiro-Go/commit/fc9093294f48),
  [follow-up](https://github.com/trankien84/Kiro-Go/commit/63d4e2bb4375))
- **Background account reconciler:** independently scheduled credential and account-info maintenance with
  bounded concurrency, jitter/backoff, typed refresh-failure classification, and lifecycle-aware result
  application. Operator-disabled accounts can retain eligible credential maintenance without being silently made
  routable again. ([account follow-up](https://github.com/trankien84/Kiro-Go/commit/63d4e2bb4375))
- **Lifecycle-aware request leases:** an `AccountSelection` deep snapshot captures the account and its
  lifecycle/statistics generations; async refresh, quota/usage recording, administrative mutations, background
  workers, and cleanup must not mutate a deleted/recreated or replaced account lifecycle using stale
  observations. ([state-safety work](https://github.com/trankien84/Kiro-Go/commit/fc9093294f48),
  [account follow-up](https://github.com/trankien84/Kiro-Go/commit/63d4e2bb4375))
- **Graceful application draining:** stop admitting new HTTP requests before shutdown, wait for admitted work
  within a shared deadline, and avoid tearing down handler/pool state underneath requests that did not drain.
  ([implementation](https://github.com/trankien84/Kiro-Go/commit/fc9093294f48))
- **Claude semantic transport planning:** analyze requested tool, thinking, beta, content, model, and metadata
  semantics before selecting Kiro native, Direct Anthropic, custom relay, Bedrock Invoke, or Bedrock Converse.
  Preserve opaque provider-owned fields on supported transparent paths and reject unsupported non-transparent
  projections. ([implementation](https://github.com/trankien84/Kiro-Go/commit/fc9093294f48))
- **Completion Guard for Claude Messages:** `off`, `observe`, `retry`, and `strict` modes; tool
  history/schema/protocol validation; buffer and deferred-stream delivery; terminal SSE validation; bounded
  recovery with different behavior before and after client-visible commitment. The default guard policy is
  `strict`, but enabled guard behavior is not a guarantee of automatic recovery for every malformed response.
  ([initial design](https://github.com/trankien84/Kiro-Go/commit/fc9093294f48),
  [protocol follow-up](https://github.com/trankien84/Kiro-Go/commit/63d4e2bb4375))
- **Optional semantic completion audit:** `KIRO_AGENT_COMPLETION_AUDIT=off|observe|enforce` (`off` by default)
  classifies candidate responses as `complete`, `waiting_for_user`, `needs_action`, or `uncertain`. In enforce
  mode, only a valid `needs_action` finding **and** an independent verifier's `continue_autonomously` approval
  can authorize a continuation. ([audit hardening](https://github.com/trankien84/Kiro-Go/commit/5cfb26eb5b77),
  [visibility](https://github.com/trankien84/Kiro-Go/commit/a210d6ca634c))
- **Richer agent protocol compatibility:** request/response handling for supported Inline Tools,
  client-executable tools, server-tool history, MCP tools and Tool Search reference/availability states,
  deferred loading, structured tool catalogs, and model-specific Claude feature constraints. Feature-specific
  routing still fails closed when the selected transport cannot preserve the requested semantics. ([protocol hardening](https://github.com/trankien84/Kiro-Go/commit/fc9093294f48),
  [follow-up](https://github.com/trankien84/Kiro-Go/commit/63d4e2bb4375))
- **Provider-gated structured JSON and reasoning:** schema-derived Kiro reasoning capabilities, model-family
  capability snapshots, thinking-signature provenance and transport affinity, and `output_config.format` on Kiro
  only when **that exact account/model** advertises live schema support. ([reasoning integration](https://github.com/trankien84/Kiro-Go/commit/a420b21da351),
  [fidelity work](https://github.com/trankien84/Kiro-Go/commit/fc9093294f48))
- **Safer OpenAI-compatible projection and Responses routing:** explicit fidelity checks, tool-choice handling,
  transport-aware `/v1/responses` selection, local/external/transient `previous_response_id` ownership, and
  commit-boundary-aware local state publication. A locally owned response ID is not forwarded as if it were an
  external upstream ID. ([implementation](https://github.com/trankien84/Kiro-Go/commit/fc9093294f48))
- **Stream integrity and transport observability:** typed Kiro transport/EventStream errors; terminal
  stop-reason checks; bounded retries where replay is safe; cancellation propagation; idle/progress watchdogs;
  and diagnostic traces for DNS, TCP/TLS, connections, reads, framing, and failure classification. ([upstream stream work](https://github.com/trankien84/Kiro-Go/commit/f8f6071c9298),
  [fork recovery](https://github.com/trankien84/Kiro-Go/commit/fc9093294f48))
- **Explicit stream text modes:** `KIRO_ASSISTANT_CHUNK_MODE` and `KIRO_REASONING_CHUNK_MODE` support `delta`
  (default), `cumulative`, and opt-in `legacy`, avoiding heuristic loss of legitimate repeated text by default.
  ([stream fix](https://github.com/trankien84/Kiro-Go/commit/95c85db31f80),
  [decoder](https://github.com/trankien84/Kiro-Go/commit/fc9093294f48))
- **Microsoft 365 / Entra ID Enterprise SSO and Kiro API Key accounts**, alongside existing supported Kiro
  sign-in methods. Kiro API keys have their own credential and region handling rather than being treated as
  OAuth refresh tokens. ([Microsoft SSO](https://github.com/trankien84/Kiro-Go/commit/82247ca7e215),
  [Kiro API Key](https://github.com/trankien84/Kiro-Go/commit/1e674324e48e))
- **Credential import surfaces:** helper JSON and authenticated admin imports, CLI JSON batch import,
  server-host Kiro IDE credential-cache import, admin file picker, and optional `CLIProxyAPI_*.json`
  import-folder watcher. These converge on one credential-classification/persistence path. The watcher is
  disabled for a bare executable and **enabled** by the checked-in Docker Compose file. ([import integration](https://github.com/trankien84/Kiro-Go/commit/4329d2a00781),
  [hardening](https://github.com/trankien84/Kiro-Go/commit/a4abc853f745))
- **Anthropic native `web_search` through Kiro MCP** for eligible Kiro requests, with tool-turn handling and
  regression coverage; not a guarantee that every configured transport implements native web search.
  ([feature](https://github.com/trankien84/Kiro-Go/commit/9409c3f755c6),
  [fixes](https://github.com/trankien84/Kiro-Go/commit/40f5e25f0864))
- **Vietnamese (`vi`) admin-panel localization**, including later uptime/model-label corrections; English and
  Chinese UI resources remain available.
  ([localization](https://github.com/trankien84/Kiro-Go/commit/5ee1de172fc6),
  [follow-up](https://github.com/trankien84/Kiro-Go/commit/d1a1730d3d05))
- **Configurable listening address and port:** `-host` / `-port` CLI flags, `HOST` / `PORT` environment
  overrides, and `KIRO_PORT` for the Compose-published host port. Precedence is **CLI flag > environment > saved
  config**. ([import/operability integration](https://github.com/trankien84/Kiro-Go/commit/4329d2a00781))
- **Optional response-language policy:** preserve the original request behavior by default, match the user's
  language, or request a fixed language for Kiro-native responses; explicit client/user language instructions
  take priority. ([implementation](https://github.com/trankien84/Kiro-Go/commit/fc9093294f48))
- **Developer documentation:** Expanded English/Vietnamese/Chinese READMEs and seven architecture guides for
  routing, account lifecycle, Completion Guard, agent protocols, streaming recovery and authentication. These
  documentation changes were prepared **after** implementation snapshot `63d4e2bb` and are not attributed to
  the historical source commits in the provenance table below. ([architecture index](docs/architecture/overview.md),
  [English](README.md), [Tiếng Việt](README_VI.md), [简体中文](README_CN.md))

### Changed

- **Provider routing now prioritizes actual capability evidence over optimistic aliases.** Kiro-native, custom
  relay, Direct Anthropic, Bedrock Invoke and Bedrock Converse have different supported feature sets;
  availability and compatibility are checked at candidate selection, not inferred from the public model name.
  ([registry](https://github.com/trankien84/Kiro-Go/commit/fc9093294f48))
- **Account configuration writes use copy-on-write publication:** stage a deep clone, persist via same-directory
  replacement, then publish the new live state. If the destination file was already replaced but a later
  durability step fails, the in-memory state still follows the published disk image.
  ([implementation](https://github.com/trankien84/Kiro-Go/commit/fc9093294f48))
- **Provider discovery and runtime status are lifecycle-scoped:** account/model denials, cooldowns, discovery
  snapshots, Bedrock region/inference-profile evidence and cleanup cannot be inherited blindly by a new account
  lifecycle reusing an ID. ([implementation](https://github.com/trankien84/Kiro-Go/commit/fc9093294f48),
  [follow-up](https://github.com/trankien84/Kiro-Go/commit/63d4e2bb4375))
- **Claude tool and reasoning fields are preserved or explicitly gated:** a beta activation alone does not imply
  a feature is in use; required unsupported semantics are not silently discarded. Agent tool-steering prompt
  injection is **opt-in and disabled by default**. ([reasoning correction](https://github.com/trankien84/Kiro-Go/commit/d635d4474d0e),
  [semantic planner](https://github.com/trankien84/Kiro-Go/commit/fc9093294f48))
- **Kiro region rediscovery commits only after a successful runtime retry:** a successful account/control-plane
  probe is insufficient to overwrite the persisted region.
  ([reprobe](https://github.com/trankien84/Kiro-Go/commit/d1a1730d3d05),
  [persistence fix](https://github.com/trankien84/Kiro-Go/commit/726a5a906228))
- **Kiro stream retry follows semantic commit boundaries:** bounded replay before committed semantic output;
  after a visible output boundary, only safe continuation/rejection paths apply rather than concatenating a
  fresh replay onto partial output. ([upstream retry](https://github.com/trankien84/Kiro-Go/commit/a18015d18bb9),
  [recovery hardening](https://github.com/trankien84/Kiro-Go/commit/fc9093294f48))
- **Responses persistence is state- and transport-aware:** external `previous_response_id` and local state
  follow different paths, and `store=false` uses transient local identifiers rather than unintended durable
  publication. ([implementation](https://github.com/trankien84/Kiro-Go/commit/fc9093294f48))

### Removed

- **Removed the intermediate deterministic completion-action heuristic and confidence-style action gating.**
  Final continuation decisions use categorical semantic audit plus independent verification when enabled;
  incomplete or invalid audit results do not authorize autonomous work. This does **not** mean Completion Guard
  itself was removed. ([removal](https://github.com/trankien84/Kiro-Go/commit/5e0e4d19cdc3),
  [verification](https://github.com/trankien84/Kiro-Go/commit/5cfb26eb5b77))

### Fixed

- **Incomplete streams are no longer presented as successful turns:** use upstream stop reasons and tool
  evidence to distinguish clean completion from empty/truncated output; preserve `max_tokens` and other
  available terminal reasons rather than guessing all endings. ([stop reasons/cancellation](https://github.com/trankien84/Kiro-Go/commit/2b93ffb86936),
  [integrity](https://github.com/trankien84/Kiro-Go/commit/f8f6071c9298))
- **Repeated streaming text is not dropped merely because chunks overlap or repeat**, preventing missing digits,
  words and thinking fragments when Kiro emits incremental deltas. ([upstream fix](https://github.com/trankien84/Kiro-Go/commit/95c85db31f80),
  [decoder follow-up](https://github.com/trankien84/Kiro-Go/commit/fc9093294f48))
- **Cancellation and tool/stop metadata are propagated** through Kiro stream handling, rather than leaving
  abandoned upstream work active or replacing provider terminal state with a generic finish reason. ([upstream fix](https://github.com/trankien84/Kiro-Go/commit/2b93ffb86936))
- **Claude major-only model identifiers such as `opus-5` use the intended context-window classification**,
  instead of falling through to a smaller default based on missing minor-version digits. ([upstream fix](https://github.com/trankien84/Kiro-Go/commit/df13a66e71af))
- **Reasoning request validation avoids premature rejection of compatible non-Kiro candidates**, normalizes
  Claude Code thinking requests where supported, and rejects unsupported explicit shapes rather than silently
  dropping them. ([thinking compatibility](https://github.com/trankien84/Kiro-Go/commit/99258f0bc369),
  [integration correction](https://github.com/trankien84/Kiro-Go/commit/d635d4474d0e))
- **Completion Guard better handles mixed client/server tool turns, historical tool availability, MCP
  references, deferred tools, and malformed structured output** without mistaking an internal audit/tool step
  for a user-visible answer. ([guard hardening](https://github.com/trankien84/Kiro-Go/commit/ac608362932d),
  [protocol compatibility](https://github.com/trankien84/Kiro-Go/commit/63d4e2bb4375))
- **Hidden recovery/audit narration stays hidden**, while valid real client tool calls and their eligible
  visible usage accounting are retained. Failed autonomous continuation does not erase an otherwise usable
  candidate answer. ([visibility](https://github.com/trankien84/Kiro-Go/commit/a210d6ca634c),
  [audit robustness](https://github.com/trankien84/Kiro-Go/commit/ac608362932d))
- **OAuth/SSO refresh and import edge cases:** classify retryable versus terminal failures; avoid stale refresh
  writes; surface rotated-token persistence failures; classify generic `external_idp` credentials independently
  of Microsoft-specific assertions. ([import fixes](https://github.com/trankien84/Kiro-Go/commit/a4abc853f745),
  [account recovery](https://github.com/trankien84/Kiro-Go/commit/63d4e2bb4375))
- **Imported JSON batches no longer stop after the first accepted item**, and malformed trailing objects produce
  a warning/failure record instead of silently discarding the rest. ([import fixes](https://github.com/trankien84/Kiro-Go/commit/a4abc853f745))
- **More robust Bedrock discovery/region handling and request cancellation**, including controlled
  model/inference-profile discovery and scoped concurrency instead of cross-lifecycle discovery state. ([Bedrock follow-up](https://github.com/trankien84/Kiro-Go/commit/63d4e2bb4375))
- **Administrative account changes and usage/statistics writes reject stale observations** to avoid undoing a
  concurrent update, re-enabling the wrong lifecycle, or resurrecting counters after a reset. ([state safety](https://github.com/trankien84/Kiro-Go/commit/fc9093294f48),
  [admin follow-up](https://github.com/trankien84/Kiro-Go/commit/63d4e2bb4375))

### Security

- **IDE cache imports cannot use an arbitrary path supplied in the HTTP request body.** The legacy `path` JSON
  field is deliberately ignored; the server uses `KIRO_IDE_CACHE` or its default cache path, with a **1 MiB**
  read limit. ([import hardening](https://github.com/trankien84/Kiro-Go/commit/a4abc853f745))
- **Credential import normalization uses one canonical classifier and trust boundary** across admin JSON, CLI
  JSON, IDE cache and directory watcher. Microsoft-specific versus generic `external_idp` behavior is not
  guessed solely from common token field names. ([Microsoft boundaries](https://github.com/trankien84/Kiro-Go/commit/b0ebb56e6113),
  [shared import core](https://github.com/trankien84/Kiro-Go/commit/4329d2a00781))
- **Private config/credential file handling is hardened** through atomic replacement, restricted Unix
  permissions, protected Windows owner-only ACLs, bounded temporary-file handling and stale credential lifecycle
  checks. ([implementation](https://github.com/trankien84/Kiro-Go/commit/fc9093294f48))
- **Import-watcher access is explicitly security-sensitive:** under the checked-in Compose deployment, write
  access to `./data/imports` effectively permits account import **without an admin HTTP password**. Keep this
  mount as trusted as `data/config.json`; disable `KIRO_IMPORT_WATCH` if mounted with weaker access control.
  ([boundary documentation](https://github.com/trankien84/Kiro-Go/commit/7ee2ea4cd04f))
- **Bounded HTTP/request payload and import handling** limits large or adversarial input, including nested media
  and credential documents, before they are projected upstream. Effective provider transport limits can be
  stricter than the public HTTP entry limit.
  ([implementation](https://github.com/trankien84/Kiro-Go/commit/fc9093294f48))
- **Direct Anthropic credentials are kept separate from generic Kiro bearer-token state**, with compatibility
  migration and validation for legacy stored account shapes.
  ([implementation](https://github.com/trankien84/Kiro-Go/commit/fc9093294f48))

### Upgrade and operational notes

- **Deploy to a private interface initially.** The checked-in Compose file publishes the API on the host's
  available interfaces by default unless you override `KIRO_PORT`; use e.g. `KIRO_PORT=127.0.0.1:8080` for a
  local-only deployment. Change the fresh-config admin password (`changeme`), enable client API-key enforcement
  before public exposure, and review the import-watcher mount. Compose does **not** automatically pass a host
  `ADMIN_PASSWORD` variable into the container; configure that override explicitly if needed. See
  [README](README.md).
- **For IDE credential import**, configure `KIRO_IDE_CACHE` on the **server** (and mount that path inside
  containers). Passing `{"path":"..."}` to `/admin/api/auth/import-ide-cache` does not select a file.
- **Completion Guard defaults:** `KIRO_AGENT_COMPLETION_GUARD=strict` by default; optional semantic audit stays
  `off` until explicitly configured. `deferred-stream` and `buffered` are not equivalent: capability and
  terminal conditions can require buffering even for a streaming client. See
  [Completion Guard architecture](docs/architecture/completion-guard.md).
- **No release/tag implied:** the `1.1.5` value in `version.json` comes from the integrated version change, not
  from a new fork release created by this documentation update. This section stays `Unreleased` until the
  maintainer chooses a release/tag.

### Change provenance and review trail

The above entries combine contributions from the **full merge graph**, not just the first-parent history. The
GitHub *commit author* identifies the recorded author of a commit, **not** exclusive ownership of its contents.
Merge commits may contain adapted work, conflict resolutions, and changes by others. The links below account for
**all 33 commits** in the comparison so maintainers can audit the origin of each change without turning the
user-facing entries into a raw commit log.

| Area | Commits in this comparison | Recorded authors / integration context |
| --- | --- | --- |
| Fork-wide state, routing, guard and protocol hardening | [`fc9093294f48`](https://github.com/trankien84/Kiro-Go/commit/fc9093294f48), [`63d4e2bb4375`](https://github.com/trankien84/Kiro-Go/commit/63d4e2bb4375) | `trankien84`; two broad feature commits covering many independently documented subsystems |
| Follow-up Completion Guard removal, audit, verifier and visibility | [`5e0e4d19cdc3`](https://github.com/trankien84/Kiro-Go/commit/5e0e4d19cdc3), [`ac608362932d`](https://github.com/trankien84/Kiro-Go/commit/ac608362932d), [`5cfb26eb5b77`](https://github.com/trankien84/Kiro-Go/commit/5cfb26eb5b77), [`a210d6ca634c`](https://github.com/trankien84/Kiro-Go/commit/a210d6ca634c) | `trankien84`; final categorical verification replaces intermediate heuristic |
| Reasoning documentation and thinking-compatibility fix | [`5562b0bd646b`](https://github.com/trankien84/Kiro-Go/commit/5562b0bd646b), [`99258f0bc369`](https://github.com/trankien84/Kiro-Go/commit/99258f0bc369) | `trankien84` |
| Native reasoning / schema foundation | [`793795e7d80b`](https://github.com/trankien84/Kiro-Go/commit/793795e7d80b), [`bdbdd7f32d86`](https://github.com/trankien84/Kiro-Go/commit/bdbdd7f32d86), [`696c80a298d6`](https://github.com/trankien84/Kiro-Go/commit/696c80a298d6) | Recorded author `root`; one commit has merge ancestry—do not infer a personal identity from this string |
| Microsoft Enterprise SSO and credential trust boundaries | [`82247ca7e215`](https://github.com/trankien84/Kiro-Go/commit/82247ca7e215), [`b0ebb56e6113`](https://github.com/trankien84/Kiro-Go/commit/b0ebb56e6113) | `Quorinex` |
| Kiro API-key support and import/UI corrections | [`1e674324e48e`](https://github.com/trankien84/Kiro-Go/commit/1e674324e48e), [`f0e2e61cc9e2`](https://github.com/trankien84/Kiro-Go/commit/f0e2e61cc9e2) | `Quorinex` |
| Model-context and upstream stream correctness | [`df13a66e71af`](https://github.com/trankien84/Kiro-Go/commit/df13a66e71af), [`95c85db31f80`](https://github.com/trankien84/Kiro-Go/commit/95c85db31f80), [`a18015d18bb9`](https://github.com/trankien84/Kiro-Go/commit/a18015d18bb9), [`2b93ffb86936`](https://github.com/trankien84/Kiro-Go/commit/2b93ffb86936), [`f8f6071c9298`](https://github.com/trankien84/Kiro-Go/commit/f8f6071c9298) | `haveanorder`, `yphungtrac9223-lang`, `jsjm1986`, `asuan-dev` |
| Kiro MCP native web search | [`9409c3f755c6`](https://github.com/trankien84/Kiro-Go/commit/9409c3f755c6), [`40f5e25f0864`](https://github.com/trankien84/Kiro-Go/commit/40f5e25f0864) | `Quorinex` |
| Version, auth/profile region split and Vietnamese UI | [`ec4ba566b478`](https://github.com/trankien84/Kiro-Go/commit/ec4ba566b478), [`4192f1074db6`](https://github.com/trankien84/Kiro-Go/commit/4192f1074db6), [`5ee1de172fc6`](https://github.com/trankien84/Kiro-Go/commit/5ee1de172fc6) | `Quorinex`; version-change commit alone does not establish a new release here |
| Upstream merge and schema-driven reasoning integration | [`f422b6c59212`](https://github.com/trankien84/Kiro-Go/commit/f422b6c59212), [`a420b21da351`](https://github.com/trankien84/Kiro-Go/commit/a420b21da351) | `zsecducna` as merger/integrator; incorporated commits have their own recorded authors |
| Reasoning tool-steering integration correction | [`d635d4474d0e`](https://github.com/trankien84/Kiro-Go/commit/d635d4474d0e) | `zsecducna` |
| Kiro API-key region reprobe and durable confirmation | [`d1a1730d3d05`](https://github.com/trankien84/Kiro-Go/commit/d1a1730d3d05), [`726a5a906228`](https://github.com/trankien84/Kiro-Go/commit/726a5a906228) | `zsecducna`; commits also document adaptation of earlier contributions |
| CLI/IDE import integration and watcher security | [`4329d2a00781`](https://github.com/trankien84/Kiro-Go/commit/4329d2a00781), [`a4abc853f745`](https://github.com/trankien84/Kiro-Go/commit/a4abc853f745), [`7ee2ea4cd04f`](https://github.com/trankien84/Kiro-Go/commit/7ee2ea4cd04f) | `zsecducna`; adapted from earlier contributor work (notably the import PR), not exclusive original feature ownership |

**Deep implementation and test maps:** [Architecture overview](docs/architecture/overview.md) ·
[Routing](docs/architecture/provider-routing.md) · [Account lifecycle](docs/architecture/account-lifecycle.md) ·
[Agent protocol](docs/architecture/agent-protocol.md) ·
[Completion Guard](docs/architecture/completion-guard.md) ·
[Streaming and recovery](docs/architecture/streaming-recovery.md) ·
[Authentication](docs/architecture/authentication.md).
