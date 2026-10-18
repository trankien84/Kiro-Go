# Tham chiếu cấu hình và triển khai

**Ngôn ngữ:** [English](configuration.md) · [Tiếng Việt](configuration.vi.md) · [简体中文](configuration.zh-CN.md)

Tài liệu vận hành bổ sung cho [README tiếng Việt](../README_VI.md). Các giá trị được đối chiếu với source và `docker-compose.yml`. Danh sách model và khả năng provider được xác định theo account, không thể suy ra chỉ từ tên model.

## 1. Khởi chạy và mở dịch vụ an toàn

- Binary: `-host` / `-port` ưu tiên hơn `HOST` / `PORT`, sau đó mới đến cấu hình đã lưu; mặc định `0.0.0.0:8080`. `CONFIG_PATH` mặc định `data/config.json`.
- Compose: `KIRO_PORT` publish cổng container `8080`; muốn chỉ mở tại máy host, đặt `KIRO_PORT=127.0.0.1:8080`. Cấu hình mặc định mở cổng API trên mọi interface. Trước khi public, nên dùng reverse proxy HTTPS.
- Admin: cấu hình mới có mật khẩu `changeme`; đổi trong Admin hoặc đặt `ADMIN_PASSWORD` **bên trong container**. Compose đi kèm **không tự truyền** `ADMIN_PASSWORD` từ host. Client API key là một lớp xác thực **riêng** (`RequireApiKey`); hãy bật và tạo key trước khi cho truy cập công khai.
- Secret: bảo vệ `data/config.json`, file import, token và bản export account. Compose bật `KIRO_IMPORT_WATCH=1` (binary thuần mặc định tắt); ai ghi được vào `data/imports` có thể kích hoạt import credential. `KIRO_IMPORT_DIR` đổi thư mục watcher.

## 2. Enterprise SSO và IDE cache

| Thiết lập | Cách hoạt động |
| --- | --- |
| `KIRO_SSO_CALLBACK_BIND` | Chỉ dành cho **Kiro Hosted SSO có listener**. Mặc định bind loopback `127.0.0.1` / `::1` tại `3128`; Compose dùng `0.0.0.0` **trong container** để nhận kết nối do Docker forward. **Microsoft SSO thủ công không đọc biến này và không cần listener.** |
| `KIRO_IDE_CACHE` | Đường dẫn cố định đến file credential IDE **trên server**; `path` do client truyền trong request import bị bỏ qua. |
| `KIRO_AWS_SSO_CACHE_DIR` | Thư mục trên host được Compose mount read-only đến `/host-aws-sso-cache`; `KIRO_IDE_CACHE` trong container trỏ tới `kiro-auth-token.json`. |
| `KIRO_PROFILE_REGIONS` | Danh sách vùng để dò profile/API key khi phù hợp; không ghi đè toàn bộ provider. |

Xem [hai luồng SSO](architecture/vi/authentication.md#2-hai-luồng-sso-trên-trình-duyệt-và-nơi-nhận-callback) trước khi cấu hình mạng. Compose giữ cổng `3128` để tương thích với **Hosted SSO**, không phải vì mọi lần đăng nhập Microsoft đều cần tunnel.

## 3. Tuỳ chọn theo provider

| Biến | Ý nghĩa và giới hạn |
| --- | --- |
| `BEDROCK_MODEL_MAP` | JSON ánh xạ alias sang Bedrock model ID. `Account.BedrockModelMap` được ưu tiên. Adapter cũng xét model đã discover, ID Bedrock trực tiếp và fallback có sẵn; alias fallback **không bảo đảm** account có quyền dùng model tại region đó. |
| `CUSTOM_API_CREDITS_PER_1K_TOKENS` | Đơn giá credit ước tính khi forward qua Custom API mà không có giá upstream; mặc định **1.0** credit/1.000 token. Đây là quy ước tính phí do operator chọn, không phải giá chính thức của model. |
| `LOG_LEVEL` | Ưu tiên hơn `logLevel` đã lưu; hỗ trợ `debug`, `info`, `warn`, `error` (cũng nhận alias `trace`, `warning`, `err`). Nếu không có giá trị hợp lệ, mặc định `info`. |

Xem [provider routing](architecture/vi/provider-routing.md) và [agent protocol](architecture/vi/agent-protocol.md). **Native fidelity của các model Kiro non-Claude thuộc nghiên cứu riêng**, không được ngầm xác nhận qua tài liệu cấu hình này.

## 4. Diagnostics, giới hạn body và Completion Guard

`KIRO_DIAGNOSTICS` bật nhóm diagnostics stream/payload/reasoning; có thể bật riêng bằng `KIRO_DIAG_STREAM`, `KIRO_DIAG_PAYLOAD`, `KIRO_DIAG_REASONING`. **`KIRO_DIAG_CHUNKS` độc lập** và có thể ghi prompt/tool content; chỉ bật trong môi trường kiểm thử được kiểm soát. Các cờ được đọc lúc process khởi động.

`KIRO_MAX_REQUEST_BODY_BYTES` mặc định 64 MiB cho body chung. Một số route có giới hạn chặt hơn, ví dụ Messages/count-tokens tối đa 32 MiB. `KIRO_AGENT_COMPLETION_GUARD` mặc định `strict` nhưng `KIRO_AGENT_COMPLETION_AUDIT` mặc định `off`; đây là hai cơ chế riêng. Xem [Completion Guard](architecture/vi/completion-guard.md) và [stream recovery](architecture/vi/streaming-recovery.md).

**Mã nguồn:** [`main.go`](../main.go), [`config/config.go`](../config/config.go), [`docker-compose.yml`](../docker-compose.yml), [`auth/kiro_sso.go`](../auth/kiro_sso.go), [`auth/microsoft_sso.go`](../auth/microsoft_sso.go), [`proxy/bedrock.go`](../proxy/bedrock.go), [`proxy/custom_api_forward.go`](../proxy/custom_api_forward.go), [`logger/logger.go`](../logger/logger.go), [`diagnostics/flags.go`](../diagnostics/flags.go).
