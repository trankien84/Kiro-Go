# 配置与部署参考

**语言：** [English](configuration.md) · [Tiếng Việt](configuration.vi.md) · [简体中文](configuration.zh-CN.md)

本文补充[中文 README](../README_CN.md) 的运维说明。各项值已与源码及 `docker-compose.yml` 对照。模型能力按账号和实际发现结果判断，不能仅凭模型名称推断。

## 1. 安全启动与对外访问

- 可执行程序：CLI `-host` / `-port` 优先于 `HOST` / `PORT`，随后才使用已保存的配置；默认 `0.0.0.0:8080`。`CONFIG_PATH` 默认 `data/config.json`。
- Compose：`KIRO_PORT` 将宿主机端口映射到容器 `8080`；仅供本机使用时设置 `KIRO_PORT=127.0.0.1:8080`。默认 Compose 会在所有接口发布 API；公开部署之前应配置 HTTPS 反向代理。
- 管理员：新配置默认密码为 `changeme`，应立即在 Admin 修改，或在**容器环境中**设置 `ADMIN_PASSWORD`。随项目提供的 Compose **不会自动转发**宿主机的 `ADMIN_PASSWORD`。客户端 API Key 是另一道鉴权开关（`RequireApiKey`）；对外开放之前应启用并创建 Key。
- Secret：保护 `data/config.json`、导入文件、Token 和账号导出。Compose 默认启用 `KIRO_IMPORT_WATCH=1`（独立 Binary 默认关闭）；具备 `data/imports` 写入权限的用户可以触发凭据导入。`KIRO_IMPORT_DIR` 可修改监听目录。

## 2. Enterprise SSO 与 IDE 缓存

| 设置 | 行为 |
| --- | --- |
| `KIRO_SSO_CALLBACK_BIND` | **仅用于 Kiro Hosted SSO 的 Listener**。默认在 `3128` 绑定 `127.0.0.1` / `::1`；Compose 为了接收 Docker 端口转发而在**容器内**设置 `0.0.0.0`。**Microsoft 手动回调流程既不读取该变量，也不需要 Listener。** |
| `KIRO_IDE_CACHE` | **服务端本地** IDE 凭据文件的固定路径；导入 API 中客户端发送的 `path` 被忽略。 |
| `KIRO_AWS_SSO_CACHE_DIR` | Compose 以只读方式挂载宿主机目录到 `/host-aws-sso-cache`，容器的 `KIRO_IDE_CACHE` 指向 `kiro-auth-token.json`。 |
| `KIRO_PROFILE_REGIONS` | 适用于 Profile/API Key 发现的候选 Region 列表，并非所有 Provider 的统一覆盖设置。 |

配置前请阅读[两种 SSO 流程](architecture/zh-CN/authentication.md#2-两种浏览器-sso-流程及回调归属)。Compose 保留端口 `3128` 是为了支持 **Hosted SSO**，不代表所有 Microsoft 登录都需要 Tunnel。

## 3. Provider 专用选项

| 变量 | 含义与限制 |
| --- | --- |
| `BEDROCK_MODEL_MAP` | 模型别名到 Bedrock Model ID 的 JSON 映射。优先使用 `Account.BedrockModelMap`；还会参考已发现的模型、Bedrock 原生 ID 和内置回退映射。内置别名**不保证**该 Region/账号具有调用权限。 |
| `CUSTOM_API_CREDITS_PER_1K_TOKENS` | Custom API 转发时若缺少上游价格，采用本地 Credit 估算费率；默认每 1,000 Token **1.0** Credit。它是运营者自己的计费约定，不是模型官方价格。 |
| `LOG_LEVEL` | 覆盖已保存的 `logLevel`；支持 `debug`、`info`、`warn`、`error`（也接受 `trace`、`warning`、`err` 别名）。没有有效值时默认 `info`。 |

参阅 [Provider 路由](architecture/zh-CN/provider-routing.md)和 [Agent 协议](architecture/zh-CN/agent-protocol.md)。**非 Claude Kiro 模型的 Native Fidelity 将另行研究**，不由本配置文档做出保证。

## 4. Diagnostics、请求体限制与 Completion Guard

`KIRO_DIAGNOSTICS` 启用 Stream/Payload/Reasoning 诊断，也可分别使用 `KIRO_DIAG_STREAM`、`KIRO_DIAG_PAYLOAD`、`KIRO_DIAG_REASONING`。**`KIRO_DIAG_CHUNKS` 独立控制**，可能记录 Prompt 和 Tool 内容；仅在受控环境中开启。这些标志在进程启动时读取。

`KIRO_MAX_REQUEST_BODY_BYTES` 默认对一般 Request Body 限制为 64 MiB；部分路由更严格，例如 Messages/count-tokens 上限 32 MiB。`KIRO_AGENT_COMPLETION_GUARD` 默认 `strict`，而 `KIRO_AGENT_COMPLETION_AUDIT` 默认 `off`；两者是独立设置。参阅 [Completion Guard](architecture/zh-CN/completion-guard.md)与[流恢复](architecture/zh-CN/streaming-recovery.md)。

**源码：** [`main.go`](../main.go)、[`config/config.go`](../config/config.go)、[`docker-compose.yml`](../docker-compose.yml)、[`auth/kiro_sso.go`](../auth/kiro_sso.go)、[`auth/microsoft_sso.go`](../auth/microsoft_sso.go)、[`proxy/bedrock.go`](../proxy/bedrock.go)、[`proxy/custom_api_forward.go`](../proxy/custom_api_forward.go)、[`logger/logger.go`](../logger/logger.go)、[`diagnostics/flags.go`](../diagnostics/flags.go)。
