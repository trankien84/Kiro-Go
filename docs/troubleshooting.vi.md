# Xử lý sự cố Kiro-Go

**Ngôn ngữ:** [English](troubleshooting.md) · [Tiếng Việt](troubleshooting.vi.md) · [简体中文](troubleshooting.zh-CN.md)

Bắt đầu bằng `GET /health` để kiểm tra process, `GET /v1/models` để xem các route công bố, trạng thái account trong Admin và request đang lỗi. **Process khỏe không có nghĩa model upstream dùng được.** Không đưa mật khẩu, authorization code, token thật hoặc log raw chunks chưa che thông tin vào issue.

## 1. Microsoft Enterprise SSO từ trình duyệt máy khác

**Triệu chứng:** Trình duyệt chuyển đến `http://localhost:3128` rồi báo không truy cập được.

**Microsoft SSO thủ công:** Đây là hành vi **dự kiến**, không cần SSH tunnel. Sao chép **toàn bộ** URL callback đầu tiên `http://localhost:3128/signin/callback?...` trên thanh địa chỉ và dán vào hộp Microsoft trong Admin. Mở URL đăng nhập Microsoft mà Admin trả về, hoàn tất đăng nhập/MFA, rồi sao chép **toàn bộ** URL thứ hai `http://localhost:3128/oauth/callback?...` vào Admin. Session chỉ có hiệu lực 10 phút; URL phải giữ nguyên path/query và `state` hợp lệ. Nếu session hết hạn, query thiếu hoặc state không khớp, hãy bắt đầu lại chứ không sửa tay URL.

**Kiro Hosted SSO:** Đây là tính năng **khác**. Backend mở listener tạm ở `3128` phía **server** và Admin poll kết quả. Phải dùng trình duyệt có `localhost` truy cập được listener trên Docker host/server, hoặc cấu hình tunnel tin cậy. Compose publish `127.0.0.1:3128:3128` cho luồng này; `localhost` trên máy từ xa **vẫn là máy từ xa**. Nếu không mở được listener, kiểm tra cổng `3128` có bị process khác chiếm không.

Không nhầm hai lựa chọn trên Admin. Xem [phân biệt hai luồng](architecture/vi/authentication.md#2-hai-luồng-sso-trên-trình-duyệt-và-nơi-nhận-callback). Việc Kiro CLI hỗ trợ device code cho một số loại tài khoản khác **không đồng nghĩa** Microsoft SSO thủ công trong Kiro-Go hỗ trợ device authorization.

## 2. Không import được cache Kiro IDE

File cache được đọc ở **server/container**, không phải máy của trình duyệt. Với Compose, kiểm tra mount cache, `KIRO_AWS_SSO_CACHE_DIR` và `KIRO_IDE_CACHE`. Trường `path` do client gửi bị bỏ qua có chủ đích vì bảo mật. File phải có refresh material dùng được và nằm trong giới hạn kích thước. Nếu host không có cache IDE, dùng import JSON từ helper đã được kiểm tra. Xem [authentication](architecture/vi/authentication.md).

## 3. Model có trong danh sách nhưng request thất bại

Danh sách model phụ thuộc account và route. Kiểm tra model ID chính xác, account có đủ điều kiện, provider route, token, quyền dùng model, cooldown, region Bedrock và capability riêng của tools/reasoning/Responses. `400` do thiếu khả năng giao thức không nhất thiết là credential hỏng; cạn tài khoản khả dụng có thể trả `503`. Không đoán alias rồi thay model ID tùy ý. Xem [provider routing](architecture/vi/provider-routing.md) và [agent protocol](architecture/vi/agent-protocol.md).

## 4. Request trả 401 hoặc lỗi quota

`ADMIN_PASSWORD` bảo vệ Admin, **khác** client API key. `RequireApiKey` là công tắc xác thực client; bật mà không có key dùng được thì từ chối toàn bộ request. Key bị tắt hoặc thiếu header có thể bị `401`; key vượt giới hạn token/credit có thể nhận `429`. Kiểm tra phần quản lý key trước khi kết luận SSO upstream lỗi. Không public dịch vụ với mật khẩu admin mặc định hoặc chưa bật client auth.

## 5. Stream bị ngắt hoặc tool output bị lặp

Phân biệt lỗi **trước** và **sau** khi text/tool event đã commit cho client. Kiro-Go phải ngăn replay không an toàn sau ranh giới semantic commit; không được biến stream cụt thành kết quả hoàn tất giả. Kiểm tra protocol của client, xem [stream integrity](architecture/vi/streaming-recovery.md) và [Completion Guard](architecture/vi/completion-guard.md). Ưu tiên diagnostics tổng hợp; `KIRO_DIAG_CHUNKS=1` có thể ghi nội dung nhạy cảm.

## 6. Docker không chạy hoặc mở port không an toàn

Compose không tự truyền `ADMIN_PASSWORD` của host vào container. Muốn chỉ mở API trên máy host, chạy `KIRO_PORT=127.0.0.1:8080 docker compose up -d --build`, sau đó thử `curl -fsS http://localhost:8080/health`. Cổng Hosted SSO `3128` chỉ publish trên loopback host, nhưng listener trong container có thể bị container khác cùng Docker network truy cập. Đổi mật khẩu mạnh, bật client-key auth và bảo vệ volume trước khi public. Xem [cấu hình](configuration.vi.md).

**Source tham khảo:** [`proxy/handler.go`](../proxy/handler.go), [`proxy/auth.go`](../proxy/auth.go), [`auth/microsoft_sso.go`](../auth/microsoft_sso.go), [`auth/kiro_sso.go`](../auth/kiro_sso.go), [`proxy/ide_cache_import.go`](../proxy/ide_cache_import.go), [`proxy/kiro_transport_recovery.go`](../proxy/kiro_transport_recovery.go), [`docker-compose.yml`](../docker-compose.yml).
