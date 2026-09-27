# Usage Statistics Plugin for CLIProxyAPI (CPA)

## Tổng quan gọn — cập nhật 2026-09-27 (đã deploy live)
- Đúng 4 card theo CAP: Tổng token (Vào/Ra/Cache), Chi phí ước tính, Tổng lượt gọi, Model dùng nhiều nhất theo lượt gọi.
- Hàng biểu đồ: Token usage trend (cột xếp chồng đầu vào/đầu ra, làm tròn đỉnh, bật tắt từng thành phần, tooltip chung khi rê/chạm) cạnh Model usage share (vòng tròn có khe hở, rê hoặc bấm để làm nổi bật một model, số ở tâm đổi theo model đang chọn).
- Hàng dưới: Sức khỏe Nhà cung cấp cạnh Phân bố Mã lỗi HTTP; Mức sử dụng từng model chiếm một hàng riêng đủ rộng. Cột TC/TB gộp Thành công/Thất bại và luôn tô màu hai bên: thành công xanh, thất bại đỏ, kể cả khi có lỗi. Bảng model có gợi ý cuộn ngang khi tràn cột.
- Biểu đồ vẫn tự vẽ bằng SVG thuần, không thêm thư viện ngoài; trục số làm tròn đẹp, nhãn trục hai dòng không đè nhau, giữ đúng tỷ lệ theo thời gian thực tế.
- Trend dùng dữ liệu `by_hour` hiện có, trục giờ Việt Nam. Cache read / cache hit rate tạm vô hiệu hoá vì chưa có dữ liệu cache và quy ước tính cache theo giờ; không suy diễn số liệu. Không port zoom/phân giải phút từ CAP.
- Hàng dưới: Mức sử dụng từng model cạnh Phân bố Mã lỗi HTTP. Trên mobile, các panel xếp dọc; bảng model cuộn ngang trong card.
- Bỏ chọn ngày giờ cụ thể, chỉ giữ preset 1h / 6h / 24h / 7d / tất cả; mặc định 1h. Bỏ biểu đồ lưu lượng request khỏi Tổng quan.
- Chọn cách hiển thị token đầy đủ / k / m / B; giữ sắp xếp, chọn cột và phân trang bảng model.
- Tab Hiệu năng & Độ trễ ẩn nút điều hướng, giữ mã và nội dung. Giữ nguyên Tools, API, xác thực, SQLite/model-router và chi tiết request.
- Đã bổ sung 2 tab: **Log Request** (xem payload đã ghi) và **Giá Model** (bảng giá ước tính chi phí).
- Khối **Requests (Realtime Log)** ở tab Tổng quan đọc theo `rowid DESC` — `usage_records.id` là TEXT/UUID nên **không** được `ORDER BY id`, sẽ sắp theo alphabet và làm bảng trông loạn.
- Attribution: xem THIRD_PARTY_NOTICES.md.


Plugin nội tuyến (Go C-ABI Shared Object `.so`) dành cho **CLIProxyAPI (CPA)** để thu thập số liệu, thống kê lưu lượng, độ trễ và phân tích chi tiết hoạt động của các LLM Models / Providers qua Router Gateway.

---

## 🏗️ Kiến trúc Project & Thành phần Code

```text
/mnt/dungchung/cpa-usage-statistics/
├── main.go               # C-ABI Entrypoint: đăng ký capability (usage_plugin, management_api), định tuyến /v0/...
├── datasource.go         # Tầng phân giải nguồn dữ liệu (ưu tiên đọc model-router.db qua SQLite read-only)
├── routerstore.go        # Engine phân tích, aggregate SQL/In-memory cho toàn bộ metrics từ Router DB
├── store.go              # SQLite Store nội bộ (fallback khi không dùng model-router)
├── types.go              # Định nghĩa struct dữ liệu: UsageSummary, HourStat, DistributionBucket, RequestDetail...
├── payload_capture.go    # Interceptor bắt prompt/response chat, ghi vào bảng phụ chat_previews
├── reqlog.go             # Ghi payload request/response vào request-logs.db (batch 32, worker nền, prune)
├── reqlog_api.go         # Management handler cho /reqlog, /reqlog/body, /reqlog/stats
├── stream_agg.go         # Gộp chunk SSE/JSON thành 1 row response; trích content/reasoning/tool_calls
├── config.go             # Cấu hình plugin qua config.yaml
├── abi_cgo.go            # Cgo memory safety & C-ABI wrapper
├── dashboard2.html       # Giao diện Analytics Dashboard chính (nhúng trực tiếp vào binary qua go:embed)
├── dashboard.html        # Giao diện Tools Hub — Reasoning Effort Inspector (nhúng qua go:embed)
├── schema_version_test.go    # Go test: khoá schema_version >= 6 (raw management response)
├── store_latest_order_test.go # Go test: khối Realtime Log phải mới-nhất-trước (rowid, không dùng id)
├── stream_agg_test.go        # Go test: aggregator stream (tool_calls, cap text, concatenated JSON)
├── reqlog_test.go            # Go test: ghi/đọc payload, chống trùng row
├── providers_active_test.go  # Go test: /providers chỉ trả về provider đang active & có key
├── test_e2e.py           # E2E Test Suite kiểm thử toàn bộ ABI và lifecycle của Plugin
├── test_router_source.py # Test Suite kiểm tra đọc & tính toán dữ liệu từ model-router.db
├── test_payload_capture.py   # Test Suite cho interceptor lưu prompt/response preview
├── test_dashboard_auth.js# Test giải mã token/auth CPAMC tự động từ localStorage/sessionStorage
└── test_error_decode.js  # Test parser & HTML entity decoder cho log lỗi từ CPA
```

### Hai Resource Menu hiển thị trong CPA Manager Plus

| Menu (sidebar) | Path | Nội dung |
|---|---|---|
| **Tools** | `/v0/resource/plugins/usage-statistics/dashboard` | CPA Tools Hub: Reasoning Effort Inspector |
| **Analytics Dashboard** | `/v0/resource/plugins/usage-statistics/dashboard2` | Analytics Dashboard v2 (7 tab phân tích) |

> Menu `Tools` trước đây là `dashboard v1` (Usage Statistics) — đã được đổi mục đích thành trung tâm công cụ, sẽ tiếp tục bổ sung thêm tool mới.

---

## 📊 Mô tả Giao diện Analytics Dashboard (Dashboard 2)

Dashboard được thiết kế theo phong cách tối giản, hiện đại (Linear/Vercel-inspired), giao diện đơn sắc, không sử dụng icon hay font ngoài, tự động tương thích với Theme sáng/tối của CPA Manager Plus (CPAMC).

### 1. Header & Thanh điều khiển toàn cục
- **Tên & Tiêu đề**: *Analytics Dashboard · Hệ thống phân tích & theo dõi Gateway CLIProxyAPI (Giờ Việt Nam · UTC+7)*.
- **Tự động đồng bộ Theme**: Lắng nghe `MutationObserver` từ CPAMC, tự động chuyển đổi Sáng/Tối mượt mà khi người dùng đổi theme trên thanh công cụ CPA.
- **Tự động làm mới (Auto-refresh)**: Menu chọn chu kỳ `Tắt`, `30s`, `1 phút`, `5 phút`, `15 phút`, `30 phút` kèm ghi nhớ `localStorage` và tự động tạm dừng khi rời tab.
- **Thời gian cập nhật**: Hiển thị chính xác mốc thời gian vừa kéo dữ liệu: `Cập nhật lúc: dd/MM/yyyy HH:mm:ss` (Giờ VN).
- **Bộ lọc Thời gian nhanh (Time Pills)**: Chọn nhanh khoảng thời gian `1 giờ` (mặc định), `6 giờ`, `24 giờ`, `7 ngày`, `Tất cả`.

### 2. Các Tab chức năng chính

#### 📋 Tab 1 — Tổng quan (Overview)
- **4 Thẻ số liệu cốt lõi**:
  - **Tổng số Request**: Tổng lượt gọi đến gateway.
  - **Tỷ lệ Lỗi & Trạng thái**: Tỷ lệ % thất bại nổi bật (đổi màu xanh/cam/đỏ theo độ nghiêm trọng) kèm chi tiết `X thành công · Y lỗi`.
  - **Tổng Input Tokens**: Context prompt nạp vào model.
  - **Tổng Output Tokens**: Số token câu trả lời model sinh ra.
- **Biểu đồ Lưu lượng theo thời gian (Volume Timeline)**: Biểu đồ diện tích xếp chồng (Stacked Area) phân tách rõ ràng lưu lượng *Thành công (Xanh lá)* và *Thất bại (Đỏ)* theo giờ (UTC+7).
- **Biểu đồ Phân bố Mã lỗi HTTP**: Thống kê số lượng các lỗi 429 (Rate limit), 503 (Unavailable), 502, 524, 400...
- **Bảng Sức khỏe Nhà cung cấp (Provider Health)**: Bảng chi tiết từng Provider: Requests, Thành công, Thất bại, % Lỗi, P50, P90.

#### 🚦 Tab 2 — Lưu lượng & Model (Traffic & Models)
- **Phân bố theo Provider**: Biểu đồ thanh ngang tỷ trọng lưu lượng giữa các Provider upstream.
- **Phân bố theo Router Alias**: Thống kê lưu lượng qua các định tuyến alias (`high-com`, `medium-com`, `low-com`, `fast`...).
- **Client API Keys**: Thống kê tỷ trọng lưu lượng theo từng API Key client (mã hóa ẩn danh dạng `sk******xx`).
- **Bảng Hiệu năng Model thật**: So sánh chi tiết từng Model: số request, số lỗi, tỷ lệ lỗi %, độ trễ P50/P90, tổng Input/Output tokens (hỗ trợ click tiêu đề cột để sắp xếp).
- **Bảng Ánh xạ Router Alias → Model thật**: Giúp theo dõi tỷ lệ phân bổ thực tế từ Alias sang các Model đích.

#### ⚡ Tab 3 — Hiệu năng & Độ trễ (Performance)
- **Hệ thống phân vị độ trễ (Latency Percentiles)**:
  - **P50 Độ trễ**: Mức trung bình lúc mạng bình thường (kèm note: *Càng thấp càng tốt / mượt*).
  - **P75 / P90 / P95 / P99 Độ trễ**: Đo SLA và phát hiện các trường hợp nghẽn/lag bất thường.
  - **Độ trễ Lớn nhất**: Request chạy chậm kỷ lục.
  - **P50 / P90 TTFT (Time To First Token)**: Thời gian chờ để nhận được token đầu tiên.
- **Biểu đồ Xu hướng Độ trễ Đa phân vị**: Trực quan hóa đường P50, P90, P95, P99 theo thời gian.
- **Biểu đồ Phân bố Độ trễ (Histogram)**: Phân nhóm trực quan từ `< 1s`, `1-3s`, `3-5s`, `5-10s` ... `> 30m`.
- **Latency vs TTFT (Scatter Plot)**: Biểu đồ phân tán so sánh tương quan giữa tổng thời gian xử lý và thời gian chờ chữ đầu.
- **Hiệu năng Trình thực thi (Executors)**: Thống kê hiệu năng theo Executor (`OpenAICompat`, `Antigravity`, `openai`, `claude`...).
- **Top 15 Request Chậm nhất**: Bảng liệt kê các request có độ trễ cao nhất kèm thời gian UTC+7, Model, Provider, Tokens và trạng thái.

#### 💰 Tab 4 — Token & Cache (Tokens & Efficiency)
- **KPI Tokens & Cache**:
  - Tổng Input Tokens, Output Tokens, Reasoning Tokens (token suy nghĩ ngầm của các model tư duy).
  - Tổng Token từ Cache, Lượt gọi có dùng cache, Tỷ lệ Cache Hit % (*Càng cao càng tiết kiệm chi phí & chạy nhanh*).
- **Biểu đồ Xu hướng Token theo thời gian**: So sánh Input Context vs Output Tokens.
- **Biểu đồ Phân bố Token (Histogram)**: Phân bố lượng token từ `< 1k` đến `> 400k`.
- **Bảng Tiêu thụ Token theo Model**: So sánh chi tiết Input, Output, Reasoning, Cached Tokens, TB In/Req, TB Out/Req, % Cache Hit của từng model.
- **Hiệu năng theo Mức độ Suy nghĩ (Reasoning Effort)**: Phân tích tương quan giữa mức độ reasoning (`high`, `medium`, `low`, `Not Specified`) với độ trễ và lượng token tiêu thụ.

#### 🔍 Tab 5 — Tra cứu Requests (Realtime Log & Drawer)
- **Tùy chọn số lượng bản ghi**: Dropdown chọn hiển thị `15 dòng` (mặc định), `25 dòng`, `50 dòng`, `100 dòng`.
- **Bộ lọc Trạng thái**: Lọc nhanh `Kết quả: Tất cả`, `Chỉ Thành công`, `Chỉ Thất bại`.
- **Sắp xếp**: Luôn hiển thị bản ghi mới nhất lên đầu (`ORDER BY timestamp DESC, id DESC`).
- **Phân trang nhanh**: Nút *Trang trước*, *Trang sau* và nhãn số trang / tổng số request.
- **Modal Chi tiết Request & Tự động Giải mã Lỗi**:
  - Click vào bất kỳ dòng request nào để mở Drawer chi tiết.
  - Hiển thị đầy đủ thông tin: Sequence ID, Thời gian UTC+7, Router Alias, Model thật, Provider, Key, Attribution, Reasoning Effort, Latency, TTFT, Tokens.
  - Đối với request lỗi: Tự động trích xuất mã lỗi, nguyên nhân, gợi ý model thay thế và thời điểm reset quota (chuyển đổi UTC → Giờ Việt Nam), đồng thời giữ nút *Xem Raw Log* nguyên bản.

> **Khối "Requests (Realtime Log)"** ở tab Tổng quan là danh sách riêng, đọc từ `/usage/latest` theo **`rowid DESC`**. Không dùng `ORDER BY id DESC` ở đây: `usage_records.id` là TEXT chứa UUID/hex digest nên sắp theo alphabet, khiến các tháng trộn lẫn nhau và bản ghi mới nhất không nằm đầu bảng. `rowid` mới là cột đơn điệu theo thứ tự chèn. (Đã từng là bug thật — xem `store_latest_order_test.go`.)

#### 📝 Tab 6 — Log Request (Payload Capture)
- **Danh sách row đã ghi**: mỗi request sinh 1 row `request` + 1 row `response`, hiển thị thời gian, model, kind, số byte, cờ `cắt` nếu body bị truncate.
- **Drawer tách khối có nhãn** (không dump JSON thô):
  - *Request*: mỗi lượt chat trong `messages` là một khối riêng kèm badge role; headers in bảng 2 cột `Name | value`.
  - *Response*: **Tổng quan** (badge `N chunk`, `X B gốc`, `finish_reason`, tên tool), **Tool được gọi**, **Suy luận**, **Nội dung trả về**, **Raw đầu stream** (thu gọn trong `<details>`).
- **Response dạng stream được gom lại**: `stream_agg.go` nối các chunk rời thành một row duy nhất rồi trích sẵn `content` / `reasoning` / `tool_calls` / `finish_reason` — thay vì lưu hàng trăm chunk riêng lẻ.
- **Body bị cắt ở `request_log_max_bytes`** (mặc định 8192, thực tế ~8157 byte), nên JSON không luôn parse được: drawer có bộ đọc "lenient" quét textual từng cặp `role`/`content` và giải mã đầy đủ escape (`\"` `\n` `\t` `\r` `\b` `\f` `\uXXXX`), đánh dấu phần bị cắt bằng badge.

#### 💰 Tab 7 — Giá Model
- Bảng giá thủ công lưu trong `usage.db`, cộng với đối chiếu ứng viên giá từ models.dev qua `/pricing/sync`.
- **Không tự áp giá**: model chưa có giá hiển thị *chưa có giá* thay vì số 0, để không tạo cảm giác số liệu đúng.

---

## 🧰 Mô tả Tab Tools — CPA Tools Hub

Tab `Tools` là trung tâm công cụ chẩn đoán, đồng bộ phong cách Slate Theme tối giản với Dashboard 2 (không icon, không emoji, không màu mè). Công cụ đầu tiên là **Reasoning Effort Inspector**.

### Reasoning Effort Inspector

Kiểm tra một model LLM thực sự hỗ trợ những mức `reasoning_effort` nào (không có API chuẩn để hỏi trực tiếp, phải probe thực tế).

**Luồng hoạt động:**

1. **Chọn nguồn Provider** — 2 chế độ (mode pill):
   - **Chọn Provider từ CPA**: Đọc `openai-compatibility` trong `config.yaml` của CPA, **chỉ liệt kê provider đang active** (bỏ qua provider có `disabled: true` hoặc không có API key nào). Chọn provider nào sẽ **tự động điền Base URL + API Key**. Nếu provider có nhiều key, hiện thêm dropdown chọn key.
   - **Nhập Base URL tùy chỉnh**: Dùng cho provider thứ 3 chưa khai báo trong CPA.

2. **Tải danh sách Model thủ công** — người dùng chủ động bấm nút *Tải danh sách Model*. Request đi qua endpoint nội bộ `/proxy-fetch` của plugin (backend gọi upstream) để tránh CORS, **không** lấy danh sách model sẵn có của CPA. Cũng có thể gõ tay tên model bất kỳ (model mới/thử nghiệm).

3. **Probe các mức thinking** — Gửi lần lượt request probe với `none`, `low`, `medium`, `high`, `xhigh`, đo **reasoning tokens**, **độ trễ** và **trạng thái** từng mức.

4. **Bảng kết quả** — Mức effort / Trạng thái (`Hỗ trợ (Thinking Active)`, `Chấp nhận (No Thinking)`, `Lỗi N`) / Reasoning Tokens / Độ trễ / Ghi chú.

5. **Khuyến nghị cấu hình Model-Router** — Sinh sẵn đoạn YAML gợi ý `thinking-levels` + `default-thinking` để copy vào cấu hình router.

> ⚠️ **Lưu ý về xác thực**: probe đi thẳng tới upstream bằng API Key của provider (qua backend proxy), **không** đi qua cổng client `/v1/chat/completions` của CPA — nên không cần Client API Key của CPA, cũng không bị CPA ghi đè `reasoning_effort`.

### Lưu ý kỹ thuật khi phát triển Tools Hub

- **Giải mã Management Key**: CPAMC lưu key trong `localStorage` dưới dạng mã hóa `enc::v1::<base64>`; phải decode bằng XOR với `SALT = "cli-proxy-api-webui::secure-storage"` kèm `host` + `userAgent`, rồi bóc JSON `state.managementKey`. `dashboard.html` dùng đúng thuật toán của `dashboard2.html` (`decodeObfuscated` + `extractKeyFromJson`), quét cả `localStorage` và `sessionStorage`.
- **Tránh CORS**: mọi request ra upstream (fetch models, probe chat) đều phải đi qua `POST /v0/management/plugins/usage-statistics/proxy-fetch` để backend thực hiện.
- **Đọc `config.yaml`**: hàm `findCPAConfigFile()` duyệt danh sách đường dẫn theo thứ tự ưu tiên và **bỏ qua file rỗng 0 byte** (từng gây lỗi đọc nhầm file rác).

---

## ⚠️ Ba cái bẫy đã từng gây bug thật

### 1. `schema_version` phải ≥ 6, nếu không CPA sẽ HTML-escape toàn bộ JSON

`registerResponse()` khai `SchemaVersion: 6` (`pluginabi.SchemaVersionRawManagementResponse`). Với `schema_version < 6`, CPA core gọi `htmlsanitize.JSONBodyIfLikely` (`internal/pluginhost/management.go`) lên response management của plugin, chạy `html.EscapeString` trên **mọi string** → `"` biến thành `&#34;` **trước khi** tới browser. Hệ quả: `JSON.parse` thất bại, drawer rơi vào nhánh dump raw.

Dấu hiệu phân biệt để không sửa nhầm tầng:
- Hàm `esc()` của dashboard sinh **`&quot;`**
- CPA core sinh **`&#34;`**

Thấy `&#34;` nghĩa là lỗi ở upstream (schema version), **không phải** ở renderer. Đã từng đốt 3 vòng sửa nhầm dashboard vì không phân biệt điểm này. Có test khoá: `schema_version_test.go`.

### 2. `usage_records.id` là TEXT/UUID — không bao giờ `ORDER BY id`

Bảng dùng `id TEXT PRIMARY KEY` chứa UUID lẫn hex digest (`fffb0a5b-…`, `fffda9230c76…`), nên `ORDER BY id DESC` sắp theo **alphabet**, trộn tháng 7/8/9 vào nhau và đẩy bản ghi mới nhất xuống giữa bảng. Dùng `ORDER BY rowid DESC` — `rowid` là INTEGER ẩn, đơn điệu theo thứ tự chèn, và đo trên DB thật 40k row còn **nhanh hơn** (0,03 ms so với 0,05 ms) vì vẫn là walk ngược bảng, không cần sort.

Các query phân trang khác dùng `ORDER BY timestamp DESC, id DESC` (timestamp dẫn đầu) nên không bị lỗi này. Riêng `chat_previews.id` và `request_logs.id` là INTEGER AUTOINCREMENT nên `ORDER BY id` ở đó vẫn đúng.

### 3. `config.yaml` bind-mount dạng file đơn

Nếu compose mount `./config.yaml:/CLIProxyAPI/config.yaml`, sửa file ở host bằng tool tạo **inode mới** (editor, patch, write) sẽ không được container nhìn thấy. Phải sửa in-place từ trong container:

```bash
docker exec cli-proxy-api sh -c 'sed ... /CLIProxyAPI/config.yaml > /tmp/cfg.new && cat /tmp/cfg.new > /CLIProxyAPI/config.yaml'
```

CPA tự hot-reload, không cần restart.

---

## 🗄️ Lưu trữ dữ liệu

Hai SQLite tách biệt, đều bật WAL (`journal_mode=wal`, `busy_timeout=5000`, `synchronous=NORMAL`, pool 4):

- **`usage.db`** — `usage_records`, `chat_previews`, override bảng giá. Retention theo `retention_days`.
- **`request-logs.db`** — `request_logs`, mỗi request 1 row `request` + 1 row `response`. Retention theo `request_log_retention_days` (mặc định 3 ngày).

Ghi theo batch 32 row qua **một** worker nền duy nhất để không chặn đường request. Aggregator stream có giới hạn cứng: 64 stream đồng thời, 512 KiB text, 1 MiB carry, sweep stream idle sau 45 giây.

Chi phí đo trên máy 2-core Celeron 3865U: ghi payload ~1,4 ms/request; mỗi chunk stream ~795 ns; finalize một response ~863 ns; dashboard poll ~24 ms.

> **Vì sao không bật `request-log` của CPA**: CPA ghi ~480 KB/request (186–742 MB/ngày) và chỉ grep được. Plugin ghi ~20–30 MB/ngày, có cấu trúc và đọc được bằng SQL.

---

## 🛠️ Hướng dẫn Build & Cài đặt

### 1. Biên dịch Plugin (.so)
Plugin yêu cầu Go 1.22+ và môi trường CGO bật để build file C-shared library:
```bash
docker run --rm -v $(pwd):/src -w /src golang:1.26 bash -c \
  "CGO_ENABLED=1 go build -trimpath -buildvcs=false -ldflags '-s -w' -buildmode=c-shared -o /src/usage-statistics.so ."
```

### 2. Triển khai vào CPA
Copy file `usage-statistics.so` vào thư mục `plugins/` của CPA. Container CPA chạy user `1004:1004`, phải set đúng owner nếu không plugin không load:
```bash
cp usage-statistics.so /mnt/dungchung/cliproxy/plugins/
chown 1004:1004 /mnt/dungchung/cliproxy/plugins/usage-statistics.so
chmod 644 /mnt/dungchung/cliproxy/plugins/usage-statistics.so
docker compose up -d --force-recreate cli-proxy-api
```

> Nên backup `.so` đang chạy trước khi ghi đè, và verify sau khi deploy bằng cách so `md5sum` host ↔ container.

### 3. Chạy Test Suite
```bash
# Go test toàn bộ (một số test cần config.yaml thật của CPA)
docker run --rm -v $(pwd):/src -w /src golang:1.26 \
  bash -c "CGO_ENABLED=1 go test -buildvcs=false ./..."

# Riêng test lọc provider đang active (cần mount config.yaml thật)
docker run --rm -v $(pwd):/src -v /path/to/cliproxy/config.yaml:/src/config.yaml:ro \
  -w /src golang:1.26 bash -c "CGO_ENABLED=1 go test -run TestProvidersActiveOnly -v ./..."

# Script kiểm tra phụ trợ
node test_dashboard_auth.js
node test_error_decode.js
python3 test_router_source.py
python3 test_payload_capture.py
python3 test_e2e.py
```
