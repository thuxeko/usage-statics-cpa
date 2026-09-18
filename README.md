# CPA Usage Statistics & Analytics Dashboard

## Tổng quan gọn — cập nhật 2026-09-17 (chờ deploy)
- Đúng 4 card theo CAP: Tổng token (Vào/Ra/Cache), Chi phí ước tính (Chưa cấu hình), Tổng lượt gọi, Model dùng nhiều nhất theo lượt gọi.
- Hàng biểu đồ: Token usage trend (cột xếp chồng đầu vào/đầu ra, làm tròn đỉnh, bật tắt từng thành phần, tooltip chung khi rê/chạm) cạnh Model usage share (vòng tròn có khe hở, rê hoặc bấm để làm nổi bật một model, số ở tâm đổi theo model đang chọn).
- Hàng dưới: Sức khỏe Nhà cung cấp cạnh Phân bố Mã lỗi HTTP; Mức sử dụng từng model chiếm một hàng riêng đủ rộng. Cột TC/TB gộp Thành công/Thất bại và luôn tô màu hai bên: thành công xanh, thất bại đỏ, kể cả khi có lỗi. Bảng model có gợi ý cuộn ngang khi tràn cột.
- Biểu đồ vẫn tự vẽ bằng SVG thuần, không thêm thư viện ngoài; trục số làm tròn đẹp, nhãn trục hai dòng không đè nhau, giữ đúng tỷ lệ theo thời gian thực tế.
- Trend dùng dữ liệu `by_hour` hiện có, trục giờ Việt Nam. Cache read / cache hit rate tạm vô hiệu hoá vì chưa có dữ liệu cache và quy ước tính cache theo giờ; không suy diễn số liệu. Không port zoom/phân giải phút từ CAP.
- Hàng dưới: Mức sử dụng từng model cạnh Phân bố Mã lỗi HTTP. Trên mobile, các panel xếp dọc; bảng model cuộn ngang trong card.
- Bỏ chọn ngày giờ cụ thể, chỉ giữ preset 1h / 6h / 24h / 7d / tất cả; mặc định 1h. Bỏ biểu đồ lưu lượng request khỏi Tổng quan.
- Chọn cách hiển thị token đầy đủ / k / m / B; giữ sắp xếp, chọn cột và phân trang bảng model.
- Tab Hiệu năng & Độ trễ ẩn nút điều hướng, giữ mã và nội dung. Giữ nguyên Tools, API, xác thực, SQLite/model-router và chi tiết request; chưa làm phần giá.
- Bản sửa bố cục/trend chỉ build vào staging; chờ người dùng duyệt trước khi copy plugin live và restart Docker.
- Attribution: xem THIRD_PARTY_NOTICES.md.


Plugin nội tuyến (C-ABI Shared Object `.so`) dành cho **CLIProxyAPI (CPA)** tích hợp sẵn giao diện **Analytics Dashboard** hiện đại, nhẹ và không phụ thuộc asset ngoài. Plugin cung cấp các công cụ theo dõi lưu lượng, phân tích hiệu năng đa phân vị độ trễ (P50/P90/P99), thống kê tiêu thụ token/cache và tự động bóc tách nguyên nhân lỗi từ log hệ thống.

---

## 🌟 Tính năng nổi bật

- 📊 **Giao diện Analytics Dashboard nội tuyến (Zero External Assets)**: Nhúng trực tiếp vào binary `.so` qua Go `embed`, truy cập trực tiếp từ menu Plugins của CPA Manager Plus (CPAMC) hoặc qua Resource Route `/v0/resource/plugins/usage-statistics/dashboard2`.
- 🌓 **Tự động đồng bộ Theme (Dark / Light)**: Lắng nghe trạng thái giao diện của CPA Manager cha qua `MutationObserver` để tự động chuyển đổi Sáng/Tối mượt mà.
- ⏱️ **Thời gian thực & Giờ Việt Nam (UTC+7)**: Tự động chuyển đổi toàn bộ mốc thời gian sang múi giờ Việt Nam (`Asia/Ho_Chi_Minh`), hỗ trợ chế độ **Tự động làm mới (Auto-refresh)** linh hoạt (30s, 1m, 5m, 15m, 30m) kèm hiển thị mốc thời gian cập nhật chính xác.
- 🔀 **Tương thích hoàn hảo với Model Router**: Tự động nhận diện và đọc trực tiếp từ `model-router.db` (SQLite Read-only) để lấy đầy đủ 100% dữ liệu request và router alias.
- 🔍 **Tự động giải mã nguyên nhân lỗi**: Đọc log lỗi CPA, decode HTML entities (`&#34;`, `&#39;`), bóc tách mã lỗi, trích xuất thời điểm reset quota và model gợi ý thay thế.
- 📈 **Hệ thống phân tích 5 Tab chuyên sâu**:
  1. **Tổng quan**: Thẻ KPI hợp nhất (Tổng request, % lỗi & trạng thái, Input/Output tokens), biểu đồ lưu lượng theo giờ và phân bố mã lỗi HTTP.
  2. **Lưu lượng & Model**: Cơ cấu Provider, Router Alias, Client API Keys và bảng hiệu năng Model thật.
  3. **Hiệu năng & Độ trễ**: Phân vị P50, P75, P90, P95, P99, Max, TTFT, biểu đồ Histogram và Scatter Plot (Latency vs TTFT).
  4. **Token & Cache**: Thống kê Input/Output/Reasoning Tokens, Cache Hit Rate %, phân bố token theo model và reasoning effort.
  5. **Tra cứu Requests**: Phân trang theo số lượng dòng (15, 25, 50, 100 dòng), lọc nhanh Thành công/Thất bại, sắp xếp mới nhất, Drawer xem chi tiết request và log lỗi.
- 🧰 **Tools Hub — Reasoning Effort Inspector**: Menu **Tools** (trước đây là dashboard v1) là trung tâm công cụ chẩn đoán. Công cụ đầu tiên kiểm tra một model hỗ trợ những mức `reasoning_effort` nào bằng cách probe thực tế `none` / `low` / `medium` / `high` / `xhigh` và đo reasoning tokens + độ trễ từng mức. Chọn Provider trực tiếp từ CPA sẽ tự động điền Base URL + API Key (nhiều key thì có dropdown chọn), hoặc nhập Base URL tùy chỉnh.
- 💬 **Chat Preview Capture**: Tự động lưu prompt/response (tối đa 1.000 ký tự mỗi phần) vào bảng phụ `chat_previews` trong `usage.db` cho mọi request, phục vụ tra cứu nội dung ngay trên dashboard.
- 🔐 **Không lộ bí mật**: API Key chỉ được trả về cho client đã xác thực Management Key; không ghi log hay hiển thị key dạng thô.

---

## 📦 Cài đặt & Triển khai

### 1. Biên dịch Plugin (.so)
Plugin được xây dựng dưới dạng C-shared library (`.so`) bằng Go 1.22+ và CGO:

```bash
docker run --rm -v $(pwd):/src -w /src golang:1.26 bash -c \
  "CGO_ENABLED=1 go build -trimpath -buildvcs=false -ldflags '-s -w' -buildmode=c-shared -o /src/usage-statistics.so ."
```

### 2. Cài đặt vào CLIProxyAPI
Copy file `usage-statistics.so` vào thư mục `plugins/` của CPA và cấp quyền đọc:

```bash
cp usage-statistics.so /path/to/cliproxy/plugins/
chmod 644 /path/to/cliproxy/plugins/usage-statistics.so
```

### 3. Cấu hình trong `config.yaml` của CPA
Thêm cấu hình kích hoạt plugin trong file `config.yaml`:

```yaml
plugins:
  enabled: true
  configs:
    usage-statistics:
      enabled: true
      priority: 100
      # Tùy chọn: Thư mục chứa usage.db (nếu không dùng model-router)
      data_dir: ""
      # Tùy chọn: Số ngày lưu trữ bản ghi (0 = không tự xóa)
      retention_days: 0
```

### 4. Khởi động lại CPA
```bash
docker compose restart cli-proxy-api
```

---

## 🚀 Truy cập Dashboard

Sau khi cài đặt thành công, bro có thể truy cập dashboard theo 2 cách:
1. **Qua giao diện CPA Manager Plus**: Mở mục **Plugins** trên menu điều hướng → Chọn **Tools** hoặc **Analytics Dashboard**.
2. **Truy cập trực tiếp qua đường dẫn**:
   ```text
   # Analytics Dashboard (5 tab phân tích)
   http://<cpa-host>:<cpa-port>/v0/resource/plugins/usage-statistics/dashboard2

   # Tools Hub (Reasoning Effort Inspector)
   http://<cpa-host>:<cpa-port>/v0/resource/plugins/usage-statistics/dashboard
   ```

---

## 📡 API Endpoints

Tất cả các API quản trị đều yêu cầu xác thực Management Key của CPA:

| Method | Endpoint | Mô tả |
|---|---|---|
| `GET` | `/v0/management/plugins/usage-statistics/usage/summary` | Trả về dữ liệu thống kê tổng hợp (KPI, phân vị, biểu đồ, bảng model/provider). |
| `GET` | `/v0/management/plugins/usage-statistics/usage/requests` | Danh sách chi tiết request có phân trang và lọc (limit, offset, result, start, end). |
| `GET` | `/v0/management/plugins/usage-statistics/error-log` | Tìm và đọc log lỗi CPA chi tiết theo timestamp và mã trạng thái. |
| `GET` | `/v0/management/plugins/usage-statistics/payload` | Lấy nội dung chat preview (prompt/response) đã lưu cho một request. |
| `GET` | `/v0/management/plugins/usage-statistics/providers` | Danh sách Provider **đang active** trong CPA kèm Base URL & API Key (dùng cho Tools Hub). |
| `POST` | `/v0/management/plugins/usage-statistics/proxy-fetch` | Proxy request ra upstream từ backend, tránh CORS (fetch models / probe reasoning). |
| `GET` | `/v0/management/plugins/usage-statistics/usage` | Lấy danh sách bản ghi thô (tương thích ngược). |
| `DELETE` | `/v0/management/plugins/usage-statistics/usage` | Xóa các bản ghi usage theo ID. |
| `GET` | `/v0/resource/plugins/usage-statistics/dashboard2` | Giao diện web Analytics Dashboard v2. |
| `GET` | `/v0/resource/plugins/usage-statistics/dashboard` | Giao diện web Tools Hub (Reasoning Effort Inspector). |

---

## 🧪 Kiểm thử (Testing)

Project đi kèm bộ kiểm thử tự động toàn diện:

```bash
# Kiểm tra tự động giải mã token xác thực CPAMC
node test_dashboard_auth.js

# Kiểm tra parser và decode mã lỗi CPA
node test_error_decode.js

# Kiểm tra đọc và tổng hợp dữ liệu từ model-router.db
python3 test_router_source.py

# Kiểm tra interceptor lưu prompt/response preview
python3 test_payload_capture.py

# Kiểm tra toàn bộ ABI C-shared và lifecycle của Plugin
python3 test_e2e.py

# Go test: /providers chỉ trả về provider đang active & có key
docker run --rm -v $(pwd):/src -w /src golang:1.26 bash -c \
  "CGO_ENABLED=1 go test -run TestProvidersActiveOnly -v ./..."
```

---

## 📄 Giấy phép (License)

Dự án được phân phối dưới giấy phép [MIT License](LICENSE).
