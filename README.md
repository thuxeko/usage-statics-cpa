# CPA Usage Statistics & Analytics Dashboard

Plugin nội tuyến (C-ABI Shared Object `.so`) dành cho **CLIProxyAPI (CPA)** tích hợp sẵn giao diện **Analytics Dashboard** hiện đại, nhẹ và không phụ thuộc asset ngoài. Plugin cung cấp các công cụ theo dõi lưu lượng, phân tích hiệu năng đa phân vị độ trễ (P50/P90/P99), thống kê tiêu thụ token/cache, ghi lại payload request/response và tự động bóc tách nguyên nhân lỗi từ log hệ thống.

Thiết kế cho máy yếu: mọi truy vấn đều có index hỗ trợ, không scan toàn bảng trên hot path, dashboard poll ~24 ms/lần.

---

## 🌟 Tính năng nổi bật

- 📊 **Giao diện Analytics Dashboard nội tuyến (Zero External Assets)**: Nhúng trực tiếp vào binary `.so` qua Go `embed`, truy cập từ menu Plugins của CPA Manager Plus (CPAMC) hoặc qua Resource Route `/v0/resource/plugins/usage-statistics/dashboard2`.
- 🌓 **Tự động đồng bộ Theme (Dark / Light)**: Lắng nghe trạng thái giao diện của CPA Manager cha qua `MutationObserver`.
- ⏱️ **Thời gian thực & Giờ Việt Nam (UTC+7)**: Chuyển đổi toàn bộ mốc thời gian sang `Asia/Ho_Chi_Minh`, hỗ trợ **Tự động làm mới** (30s / 1m / 5m / 15m / 30m) kèm mốc thời gian cập nhật.
- 🔀 **Tương thích Model Router**: Tự động nhận diện và đọc `model-router.db` (SQLite read-only) để lấy đầy đủ dữ liệu request và router alias.
- 🔍 **Tự động giải mã nguyên nhân lỗi**: Đọc log lỗi CPA, decode HTML entities (`&#34;`, `&#39;`), bóc tách mã lỗi, trích xuất thời điểm reset quota và model gợi ý thay thế.
- 📈 **7 Tab phân tích chuyên sâu**:
  1. **Tổng quan**: Thẻ KPI (tổng token, chi phí ước tính, tổng lượt gọi, model dùng nhiều nhất), biểu đồ token theo giờ, sức khỏe Provider, phân bố mã lỗi HTTP, bảng hiệu năng model, và khối **Requests (Realtime Log)** hiển thị các request mới nhất.
  2. **Lưu lượng & Model**: Cơ cấu Provider, Router Alias, Client API Keys và bảng hiệu năng Model thật.
  3. **Hiệu năng & Độ trễ**: Phân vị P50/P75/P90/P95/P99, Max, TTFT, histogram, scatter plot (Latency vs TTFT), top request chậm nhất.
  4. **Token & Cache**: Input/Output/Reasoning Tokens, Cache Hit Rate %, phân bố token theo model và reasoning effort.
  5. **Tra cứu Requests**: Phân trang (15/25/50/100 dòng), lọc Thành công/Thất bại, drawer chi tiết request và log lỗi.
  6. **Log Request**: Xem payload request/response đã ghi lại, tách khối có nhãn (Tổng quan / Tool được gọi / Suy luận / Nội dung trả về / Headers), raw thu gọn trong `<details>`.
  7. **Giá Model**: Bảng giá thủ công + đối chiếu giá từ models.dev, phục vụ lớp ước tính chi phí.
- 🧰 **Tools Hub — Reasoning Effort Inspector**: Menu **Tools** (trước đây là dashboard v1) là trung tâm công cụ chẩn đoán. Công cụ đầu tiên kiểm tra một model hỗ trợ những mức `reasoning_effort` nào bằng cách probe thực tế `none` / `low` / `medium` / `high` / `xhigh` và đo reasoning tokens + độ trễ từng mức. Chọn Provider trực tiếp từ CPA sẽ tự động điền Base URL + API Key (nhiều key thì có dropdown chọn), hoặc nhập Base URL tùy chỉnh.
- 💬 **Chat Preview Capture**: Tự động lưu prompt/response (tối đa 1.000 ký tự mỗi phần) vào bảng phụ `chat_previews` trong `usage.db`.
- 📝 **Request / Response Payload Log**: Ghi payload vào SQLite **riêng** (`request-logs.db`) — không bật `request-log` của CPA. Response dạng stream được **gom chunk thành một row** và trích sẵn `content` / `reasoning` / `tool_calls` / `finish_reason`.
- 🔐 **Không lộ bí mật**: API Key chỉ trả về cho client đã xác thực Management Key; body đã ghi được scrub credential bằng `maskSecrets` trước khi lưu.

---

## 📦 Cài đặt & Triển khai

### 1. Biên dịch Plugin (.so)

Plugin được xây dựng dưới dạng C-shared library (`.so`) bằng Go 1.22+ và CGO:

```bash
docker run --rm -v $(pwd):/src -w /src golang:1.26 bash -c \
  "CGO_ENABLED=1 go build -trimpath -buildvcs=false -ldflags '-s -w' -buildmode=c-shared -o /src/usage-statistics.so ."
```

### 2. Cài đặt vào CLIProxyAPI

Copy file `usage-statistics.so` vào thư mục `plugins/` của CPA. Container CPA chạy bằng user `1004:1004` nên phải set đúng owner, nếu không plugin sẽ không load được:

```bash
cp usage-statistics.so /path/to/cliproxy/plugins/
chown 1004:1004 /path/to/cliproxy/plugins/usage-statistics.so
chmod 644 /path/to/cliproxy/plugins/usage-statistics.so
```

### 3. Cấu hình trong `config.yaml` của CPA

```yaml
plugins:
  enabled: true
  configs:
    usage-statistics:
      enabled: true
      priority: 100
      # Tùy chọn: thư mục chứa usage.db (mặc định ~/.cli-proxy-api/plugins/usage-statistics)
      data_dir: ""
      # Tùy chọn: số ngày lưu bản ghi usage (0 = không tự xóa)
      retention_days: 0
      # Ghi payload request/response vào request-logs.db (mặc định: bật)
      request_log_enabled: true
      # Cắt mỗi body về tối đa bấy nhiêu byte (mặc định 8192)
      request_log_max_bytes: 8192
      # Số ngày lưu request log (mặc định 3, 0 = không tự xóa)
      request_log_retention_days: 3
```

> ⚠️ **Lưu ý về bind-mount**: nếu `config.yaml` được mount dạng **file đơn** (`./config.yaml:/CLIProxyAPI/config.yaml`), sửa file ở host bằng editor/tool tạo inode mới sẽ **không** được container nhìn thấy. Phải sửa in-place từ trong container, ví dụ `sed ... > /tmp/cfg.new && cat /tmp/cfg.new > /CLIProxyAPI/config.yaml`. CPA tự hot-reload, không cần restart.

### 4. Khởi động lại CPA

```bash
docker compose up -d --force-recreate cli-proxy-api
```

---

## 🚀 Truy cập Dashboard

1. **Qua giao diện CPA Manager Plus**: mở mục **Plugins** → chọn **Tools** hoặc **Analytics Dashboard**.
2. **Truy cập trực tiếp**:
   ```text
   # Analytics Dashboard (7 tab phân tích)
   http://<cpa-host>:<cpa-port>/v0/resource/plugins/usage-statistics/dashboard2

   # Tools Hub (Reasoning Effort Inspector)
   http://<cpa-host>:<cpa-port>/v0/resource/plugins/usage-statistics/dashboard
   ```

> 🔄 HTML được nhúng trong `.so`, nên sau mỗi lần deploy UI phải **hard-refresh (Ctrl+F5)** — nếu không browser sẽ dùng bản cache cũ và bạn sẽ tưởng bản sửa không có tác dụng.

---

## 📡 API Endpoints

Các API quản trị yêu cầu xác thực Management Key của CPA:

| Method | Endpoint | Mô tả |
|---|---|---|
| `GET` | `/v0/management/plugins/usage-statistics/usage/summary` | Dữ liệu thống kê tổng hợp (KPI, phân vị, biểu đồ, bảng model/provider). |
| `GET` | `/v0/management/plugins/usage-statistics/usage/requests` | Danh sách request chi tiết, phân trang + lọc (limit, offset, result, start, end). |
| `GET` | `/v0/management/plugins/usage-statistics/usage/latest` | N bản ghi mới nhất cho khối **Realtime Log** (đọc theo `rowid DESC`, không scan cửa sổ thời gian). |
| `GET` | `/v0/management/plugins/usage-statistics/usage` | Danh sách bản ghi thô (tương thích ngược). |
| `DELETE` | `/v0/management/plugins/usage-statistics/usage` | Xóa bản ghi usage theo ID. |
| `GET` | `/v0/management/plugins/usage-statistics/error-log` | Tìm và đọc log lỗi CPA theo timestamp + mã trạng thái. |
| `GET` | `/v0/management/plugins/usage-statistics/payload` | Chat preview (prompt/response) đã lưu cho một request. |
| `GET` | `/v0/management/plugins/usage-statistics/providers` | Danh sách Provider **đang active** kèm Base URL & API Key (Tools Hub). |
| `POST` | `/v0/management/plugins/usage-statistics/proxy-fetch` | Proxy request ra upstream từ backend, tránh CORS (fetch models / probe reasoning). |
| `GET` | `/v0/management/plugins/usage-statistics/pricing` | Đọc/ghi bảng giá ước tính (giá router + override thủ công trong `usage.db`). |
| `GET` | `/v0/management/plugins/usage-statistics/pricing/sync` | Trả về ứng viên giá từ models.dev cho các model trong khoảng thời gian; **không tự áp dụng**. |
| `GET` | `/v0/management/plugins/usage-statistics/reqlog` | Danh sách row payload đã ghi (metadata, không kèm body). Tham số: `limit`, `kind`, `model`. |
| `GET` | `/v0/management/plugins/usage-statistics/reqlog/body` | Trả về một body đã ghi theo `id`. |
| `GET` | `/v0/management/plugins/usage-statistics/reqlog/stats` | Tình trạng ghi log (queued / written / dropped). |
| `GET` | `/v0/resource/plugins/usage-statistics/dashboard2` | Giao diện Analytics Dashboard v2. |
| `GET` | `/v0/resource/plugins/usage-statistics/dashboard` | Giao diện Tools Hub (Reasoning Effort Inspector). |

---

## 🗄️ Lưu trữ dữ liệu

Hai database SQLite tách biệt, đều bật WAL:

- **`usage.db`** — bản ghi usage (`usage_records`), chat preview (`chat_previews`), override bảng giá. Retention theo `retention_days`.
- **`request-logs.db`** — payload request/response (`request_logs`), mỗi request 1 row `request` + 1 row `response`. Retention theo `request_log_retention_days`.

Ghi theo batch (32 row/lần) qua một worker nền duy nhất để không chặn đường request. Aggregator stream có giới hạn cứng: 64 stream đồng thời, 512 KiB text, 1 MiB carry, sweep stream idle sau 45 giây.

> Chi phí đo được trên máy 2-core Celeron 3865U: ghi payload ~1,4 ms/request; mỗi chunk stream ~795 ns; finalize một response ~863 ns. Dashboard poll ~24 ms.

---

## ⚙️ Yêu cầu về `schema_version`

`registerResponse()` khai `SchemaVersion: 6` (`pluginabi.SchemaVersionRawManagementResponse`).

**Đây không phải chi tiết vô hại.** Với `schema_version < 6`, CPA core chạy `html.EscapeString` lên **mọi string** trong JSON trả về từ management API của plugin (`internal/pluginhost/management.go` → `htmlsanitize.JSONBodyIfLikely`), biến `"` thành `&#34;` **trước khi** dữ liệu tới browser. Hậu quả: `JSON.parse` thất bại, drawer Log Request rơi vào nhánh dump raw và hiển thị một "bức tường" entity. Đã từng gây ra 3 vòng sửa nhầm ở tầng renderer.

Có test khoá hành vi này (`schema_version_test.go`) để không tụt version trở lại.

---

## 🧪 Kiểm thử (Testing)

```bash
# Go test toàn bộ (một số test cần config.yaml thật của CPA)
docker run --rm -v $(pwd):/src -w /src golang:1.26 \
  bash -c "CGO_ENABLED=1 go test -buildvcs=false ./..."

# Riêng test lọc provider đang active (cần mount config.yaml thật)
docker run --rm -v $(pwd):/src -v /path/to/cliproxy/config.yaml:/src/config.yaml:ro \
  -w /src golang:1.26 bash -c "CGO_ENABLED=1 go test -run TestProvidersActiveOnly -v ./..."

# Script kiểm tra phụ trợ
node test_dashboard_auth.js      # Giải mã token xác thực CPAMC
node test_error_decode.js        # Parser + decode mã lỗi CPA
python3 test_router_source.py    # Đọc & tổng hợp dữ liệu từ model-router.db
python3 test_payload_capture.py  # Interceptor lưu prompt/response preview
python3 test_e2e.py              # ABI C-shared và lifecycle của plugin
```

Test Go đáng chú ý:

- `stream_agg_test.go` — aggregator stream: nối chunk, `tool_calls`, cap text (không cap raw bytes), concatenated JSON, terminator.
- `store_latest_order_test.go` — khối Realtime Log phải mới-nhất-trước. `usage_records.id` là **TEXT/UUID**, nên `ORDER BY id DESC` sắp theo alphabet và làm bảng trông loạn; phải dùng `rowid DESC`.
- `schema_version_test.go` — khoá `schema_version >= 6`.
- `reqlog_test.go` — ghi/đọc payload, chống trùng row.

---

## 📄 Giấy phép (License)

Dự án được phân phối dưới giấy phép [MIT License](LICENSE). Attribution: xem [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md).
