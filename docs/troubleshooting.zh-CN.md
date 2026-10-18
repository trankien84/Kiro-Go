# Kiro-Go 故障排查

**语言：** [English](troubleshooting.md) · [Tiếng Việt](troubleshooting.vi.md) · [简体中文](troubleshooting.zh-CN.md)

首先检查 `GET /health`（服务进程）、`GET /v1/models`（公开的模型路由）、Admin 账号状态及具体失败的 Request。**进程健康不代表上游模型一定可用。**请勿在 Issue 中粘贴密码、授权码、真实 Token 或未脱敏的 Raw Chunk 日志。

## 1. 从远程浏览器登录 Microsoft Enterprise SSO

**现象：** 浏览器跳转到 `http://localhost:3128` 后提示无法连接。

**Microsoft 手动 SSO：** 这是**正常现象**，不需要 SSH Tunnel。复制地址栏中**完整的**第一条 `http://localhost:3128/signin/callback?...` URL，粘贴到 Admin 的 Microsoft 登录弹窗。打开 Admin 返回的 Microsoft 授权 URL，完成登录/MFA，再将**完整的**第二条 `http://localhost:3128/oauth/callback?...` URL 粘贴到 Admin。会话只有 10 分钟有效期；路径、Query 和 `state` 必须有效。会话过期、参数缺失或 State 不匹配时应重新开始，而不是手工改写 URL。

**Kiro Hosted SSO：** 这是**另一项功能**。后端在**服务端**临时监听 `3128`，Admin 轮询收到的回调。浏览器的 `localhost` 必须能访问 Docker 宿主机/服务器上的 Listener，否则需要主动配置可信的 Tunnel。Compose 为该流程发布 `127.0.0.1:3128:3128`；远程电脑的 `localhost` **仍指远程电脑自己**。Listener 启动失败时可检查 `3128` 是否已被其他进程占用。

不要混淆 Admin 中的两种入口。参阅[两种 SSO 流程](architecture/zh-CN/authentication.md#2-两种浏览器-sso-流程及回调归属)。Kiro CLI 为某些账号提供 Device Code 登录，**不代表** Kiro-Go 的 Microsoft 手动流程支持 Device Authorization。

## 2. 无法导入 Kiro IDE 缓存

缓存文件位于**服务端/容器内**，而不是浏览器所在电脑。Compose 用户应检查缓存挂载及 `KIRO_AWS_SSO_CACHE_DIR`、`KIRO_IDE_CACHE`。导入 API 故意忽略客户端发送的 `path`，以防任意文件读取。文件必须包含可用的 Refresh 凭据并符合大小上限。宿主机没有 IDE Cache 时可改用经校验的 Helper JSON 导入。参阅[身份认证](architecture/zh-CN/authentication.md)。

## 3. 模型出现在列表中，但 Request 失败

模型列表取决于账号和路由。检查准确的 Model ID、账号可用性、Provider Route、Token、模型权限、Cooldown、Bedrock Region 以及请求所需的 Tool/Reasoning/Responses 能力。协议不兼容引发的 `400` 不一定是凭据故障；没有可用账号时可能返回 `503`。不要自行猜测或替换模型别名。参阅 [Provider 路由](architecture/zh-CN/provider-routing.md)和 [Agent 协议](architecture/zh-CN/agent-protocol.md)。

## 4. API 返回 401 或 Quota 错误

`ADMIN_PASSWORD` 用于保护 Admin，**并非**客户端 API Key。`RequireApiKey` 是客户端认证总开关；打开却没有可用的 Key 时会拒绝所有请求。禁用的 Key 或缺少 Header 可能返回 `401`；超出 Token/Credit 限额可能返回 `429`。在怀疑上游 SSO 之前，先检查管理员 Key 设置。切勿在默认管理员密码或未启用客户端认证的情况下公开服务。

## 5. Stream 被截断或 Tool 输出重复

要区分已经向客户端提交 Text/Tool Event **之前**与**之后**的故障。超过 Semantic Commit 边界后不应重放原始请求，避免重复输出或执行工具；也不能把不完整 Stream 伪装成正常完成。检查客户端使用的协议，参阅[流完整性](architecture/zh-CN/streaming-recovery.md)及 [Completion Guard](architecture/zh-CN/completion-guard.md)。优先使用汇总诊断；`KIRO_DIAG_CHUNKS=1` 可能记录敏感内容。

## 6. Docker 启动失败或端口暴露不安全

Compose 不会自动把宿主机的 `ADMIN_PASSWORD` 传入容器。仅供本机访问 API 时运行 `KIRO_PORT=127.0.0.1:8080 docker compose up -d --build`，随后执行 `curl -fsS http://localhost:8080/health`。Hosted SSO 端口 `3128` 只发布在宿主机 Loopback，但容器内 Listener 仍可能被同一 Docker 网络中的其他容器访问。对外开放前务必修改强密码、启用客户端 Key 认证并保护 Volume。参阅[配置参考](configuration.zh-CN.md)。

**源码参考：** [`proxy/handler.go`](../proxy/handler.go)、[`proxy/auth.go`](../proxy/auth.go)、[`auth/microsoft_sso.go`](../auth/microsoft_sso.go)、[`auth/kiro_sso.go`](../auth/kiro_sso.go)、[`proxy/ide_cache_import.go`](../proxy/ide_cache_import.go)、[`proxy/kiro_transport_recovery.go`](../proxy/kiro_transport_recovery.go)、[`docker-compose.yml`](../docker-compose.yml)。
