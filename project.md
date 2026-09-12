# Usage Statistics Plugin for CLIProxyAPI (CPA)

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
├── config.go             # Cấu hình plugin qua config.yaml
├── abi_cgo.go            # Cgo memory safety & C-ABI wrapper
├── dashboard2.html       # Giao diện Analytics Dashboard chính (nhúng trực tiếp vào binary qua go:embed)
├── dashboard.html        # Giao diện dashboard v1 (lưu trữ phục vụ chuyển đổi mục đích sau này)
├── test_e2e.py           # E2E Test Suite kiểm thử toàn bộ ABI và lifecycle của Plugin
├── test_router_source.py # Test Suite kiểm tra đọc & tính toán dữ liệu từ model-router.db
├── test_dashboard_auth.js# Test giải mã token/auth CPAMC tự động từ localStorage/sessionStorage
└── test_error_decode.js  # Test parser & HTML entity decoder cho log lỗi từ CPA
```

---

## 📊 Mô tả Giao diện Analytics Dashboard (Dashboard 2)

Dashboard được thiết kế theo phong cách tối giản, hiện đại (Linear/Vercel-inspired), giao diện đơn sắc, không sử dụng icon hay font ngoài, tự động tương thích với Theme sáng/tối của CPA Manager Plus (CPAMC).

### 1. Header & Thanh điều khiển toàn cục
- **Tên & Tiêu đề**: *Analytics Dashboard · Hệ thống phân tích & theo dõi Gateway CLIProxyAPI (Giờ Việt Nam · UTC+7)*.
- **Tự động đồng bộ Theme**: Lắng nghe `MutationObserver` từ CPAMC, tự động chuyển đổi Sáng/Tối mượt mà khi người dùng đổi theme trên thanh công cụ CPA.
- **Tự động làm mới (Auto-refresh)**: Menu chọn chu kỳ `Tắt`, `30s`, `1 phút`, `5 phút`, `15 phút`, `30 phút` kèm ghi nhớ `localStorage` và tự động tạm dừng khi rời tab.
- **Thời gian cập nhật**: Hiển thị chính xác mốc thời gian vừa kéo dữ liệu: `Cập nhật lúc: dd/MM/yyyy HH:mm:ss` (Giờ VN).
- **Bộ lọc Thời gian nhanh (Time Pills)**: Chọn nhanh khoảng thời gian `1 giờ`, `6 giờ`, `24 giờ`, `7 ngày` (mặc định), `Tất cả`.

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
- **Sắp xếp**: Luôn hiển thị bản ghi mới nhất lên đầu (`timestamp DESC`).
- **Phân trang nhanh**: Nút *Trang trước*, *Trang sau* và nhãn số trang / tổng số request.
- **Modal Chi tiết Request & Tự động Giải mã Lỗi**:
  - Click vào bất kỳ dòng request nào để mở Drawer chi tiết.
  - Hiển thị đầy đủ thông tin: Sequence ID, Thời gian UTC+7, Router Alias, Model thật, Provider, Key, Attribution, Reasoning Effort, Latency, TTFT, Tokens.
  - Đối với request lỗi: Tự động trích xuất mã lỗi, nguyên nhân, gợi ý model thay thế và thời điểm reset quota (chuyển đổi UTC → Giờ Việt Nam), đồng thời giữ nút *Xem Raw Log* nguyên bản.

---

## 🛠️ Hướng dẫn Build & Cài đặt

### 1. Biên dịch Plugin (.so)
Plugin yêu cầu Go 1.22+ và môi trường CGO bật để build file C-shared library:
```bash
docker run --rm -v $(pwd):/src -w /src golang:1.26 bash -c \
  "CGO_ENABLED=1 go build -trimpath -buildvcs=false -ldflags '-s -w' -buildmode=c-shared -o /src/usage-statistics.so ."
```

### 2. Triển khai vào CPA
Copy file `usage-statistics.so` vào thư mục `plugins/` của CPA và khởi động lại container CPA:
```bash
cp usage-statistics.so /mnt/dungchung/cliproxy/plugins/
docker compose restart cli-proxy-api
```

### 3. Chạy Test Suite
```bash
node test_dashboard_auth.js
node test_error_decode.js
python3 test_router_source.py
python3 test_e2e.py
```
