# Log Request (nội dung request) — Implementation Plan

> **For Hermes:** Dùng subagent-driven-development để implement từng task.

**Goal:** Bro thấy được **mỗi request gửi gì / nhận gì / thành công hay thất bại**, ngay trong dashboard usage-statistics.

**Tech Stack:** Go plugin (C-ABI `.so`), SQLite (usage.db), inline HTML/JS (dashboard2.html), CPA v7.3.17.

---

## PHẦN 0 — SỰ THẬT HIỆN TẠI (đã đo đạc 2026-09-26)

### 0.1 Những gì ĐÃ CÓ sẵn
| Thứ | Trạng thái | Vị trí |
|---|---|---|
| `request-log: false` | **đang TẮT** | `/mnt/dungchung/cliproxy/config.yaml:504` |
| CPA request-log endpoints | Có sẵn | `GET/PUT /v0/management/request-log`, `GET /v0/management/request-log-by-id/:id` |
| main.log (gin_logger) | Đang chạy, 12.380 dòng / 3.26 ngày | `/CLIProxyAPI/logs/main.log` (rotate 10 MB) |
| Error log (chỉ khi fail) | Đang chạy, 10 files | `/CLIProxyAPI/logs/error-*.log` |
| usage.db | 39.021 rows, 38 MB | `/data/usage-statistics/usage.db` |
| chat_previews (payload capture) | **307 rows — GẦN NHƯ TRỐNG** | bảng `chat_previews` |

### 0.2 Vấn đề thực sự (đã chứng minh)
1. **Payload capture đang HỎNG nặng**: 09-24→09-25 có **6.330 usage records** nhưng chỉ **5 chat_previews**. Tỷ lệ lưu được = **0.08%**.
2. Nguyên nhân: `globalPayloadManager` chỉ giữ **300 items** and lưu theo `RequestID` (UUID của CPA), nhưng `usage_records.id` là **UUID tự sinh của plugin** (`uuid.NewString()` trong `toRecord()`). Hai hệ ID này **không bao giờ khớp** → drawer lookup `/payload?id=<usage id>` luôn trả `found:false`.
   - Bằng chứng: `SELECT COUNT(*) FROM chat_previews WHERE request_id IN (SELECT id FROM usage_records)` → **0**
3. **main.log rotate 10 MB** (~3-4 ngày) → không thể tra cứu lịch sử.
4. **Không có IP** (đã xác nhận `pluginapi.UsageRecord` không có field IP).

### 0.3 Chi phí nếu bật `request-log: true`
- ~3.800 request/ngày (đo từ gin_logger).
- Error log thực tế **477 KB/file** (gồm full request + response body).
- Ước tính **50–200 KB/request** → **186 MB – 742 MB/ngày**.
- Disk còn 404 GB → được, nhưng **phải set `logs-max-total-size-mb`** nếu không sẽ phình vô hạn.

---

## 3 PHƯƠNG ÁN

### 🅰️ Phương án A — Bật `request-log: true` của CPA *(native, ít code nhất)*

**Làm:** sửa 2 dòng trong `/mnt/dungchung/cliproxy/config.yaml`
```yaml
request-log: true
logs-max-total-size-mb: 20480        # 20 GB — bắt buộc, nếu không log phình vô hạn
```

**Được:**
- Log ĐẦY ĐỦ: headers, request body, response body, status — chuẩn format CPA.
- Có sẵn API `GET /v0/management/request-log-by-id/:id`.
- Không phải viết code.

**Mất:**
- **Rất nặng**: 186–742 MB/ngày, ghi đĩa liên tục.
- Log nằm rải rác thành **file .log** trong `/CLIProxyAPI/logs/` — plugin phải đọc/grep file, không query được theo thời gian/model/key.
- Muốn hiện lên dashboard thì vẫn phải viết code đọc file.

**Khuyến nghị:** ⚠️ Chỉ bật nếu bro cần **debug sâu từng request một**. Không hợp để "xem log thường xuyên".

---

### 🅱️ Phương án B — SỬA payload capture của plugin *(KHUYẾN NGHỊ)*

**Ý tưởng:** plugin đã có sẵn `RequestInterceptor` + `ResponseInterceptor` + `StreamInterceptor` (đã đăng ký `Capabilities`, CPA đã gọi). Chỉ cần **nối đúng ID** và **lưu đủ**.

**Sửa 3 chỗ:**

**B1. Lưu thêm `request_id` (UUID thật của CPA) vào `usage_records`**
- File: `main.go` — struct `usageRecord` thêm `RequestID string \`json:"RequestID"\``
- File: `main.go` `toRecord()` — map `rec.RequestID`
- File: `store.go` — thêm cột `request_id` vào schema + INSERT
- File: `types.go` — `Record` struct thêm `RequestID`

**B2. Nới ring buffer từ 300 → 3000 và lưu CẢ request lẫn response**
- File: `payload_capture.go:60` — `if len(m.history) > 300` → `3000`

**B3. Sửa lookup trong drawer**
- File: `store.go` `GetChatPayload()` — join theo `usage_records.request_id` thay vì `id`

**Được:**
- Tra cứu được **prompt + response** ngay trong drawer hiện tại.
- Nhẹ: usage.db ~1 KB/row (đang 38 MB / 39k rows).
- Có sẵn UI (`loadChatPayloadForDetail`) — chỉ cần nối đúng.
- Có thể filter theo model/key/thời gian vì nằm trong SQLite.

**Mất:**
- Chỉ lưu **preview** (2000 bytes prompt / 2000 bytes response) — không phải full body.
- Vẫn không có IP.
- Phải rebuild `.so` + deploy.

---

### 🅲️ Phương án C — Bảng `request_logs` riêng trong usage.db *(đầy đủ nhất)*

**Ý tưởng:** tạo bảng mới, lưu **toàn bộ** mỗi request: headers, body, response, status, IP (nếu lấy được qua RequestInterceptor), thời gian.

**Schema:**
```sql
CREATE TABLE request_logs (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  request_id TEXT,          -- UUID thật của CPA
  usage_id   TEXT,          -- FK -> usage_records.id
  timestamp  TEXT,
  client_ip  TEXT,          -- lấy từ RequestInterceptor (nếu CPA truyền)
  method     TEXT,
  path       TEXT,
  model      TEXT,
  api_key    TEXT,
  status_code INTEGER,
  latency_ms INTEGER,
  request_headers TEXT,     -- JSON
  request_body    TEXT,     -- cắt ngắn theo config
  response_body   TEXT,     -- cắt ngắn theo config
  truncated  INTEGER DEFAULT 0
);
CREATE INDEX idx_rl_ts ON request_logs(timestamp);
CREATE INDEX idx_rl_rid ON request_logs(request_id);
```

**Thêm config field mới** (register.go `ConfigFields`):
- `capture_bodies`: bool — có lưu body không (mặc định true)
- `max_body_bytes`: int — cắt ngắn (mặc định 8192)
- `log_retention_days`: int — dọn cũ (mặc định 7)

**Thêm tab mới "Log Request"** trên dashboard2.html:
- Bảng: thời gian, IP, model, status badge, latency, tokens
- Click vào row → drawer hiện headers + request body + response body
- Filter: theo status (all/ok/failed), model, api_key, khoảng thời gian

**Được:**
- **Đầy đủ nhất** — mọi thứ trong SQLite, query được.
- Có IP (nếu lấy được).
- Có retention tự động.
- Không phụ thuộc file log của CPA.

**Mất:**
- Nhiều code nhất (Go + UI).
- Nặng hơn B: ~8 KB/request × 3.800 = **~30 MB/ngày** (chấp nhận được).
- IP: phải xác nhận `request.intercept_before` có truyền `RemoteAddr` không — **cần test trước**.

---

## KHUYẾN NGHỊ CỦA TÔI

**Làm B trước, rồi nếu chưa đủ thì nâng lên C.**

- B sửa đúng cái đang hỏng (0.08% → 100%), tốn ít công, cho bro thấy ngay prompt/response.
- C là superset của B — nếu làm C thì B là bước đệm tự nhiên (cần `request_id` ở cả 2).

**Đề xuất lộ trình:**
1. **Task 1-4 (Phương án B)** — nối ID, nới buffer, sửa lookup → bro test drawer.
2. **Task 5+ (Phương án C)** — nếu bro muốn IP + headers + full body + tab riêng.

---

## TASKS — PHƯƠNG ÁN B (làm trước)

### Task 1: Thêm cột `request_id` vào usage.db

**Objective:** Lưu UUID thật của CPA vào mỗi usage record.

**Files:**
- Modify: `/mnt/dungchung/cpa-usage-statistics/main.go:287` (struct `usageRecord`)
- Modify: `/mnt/dungchung/cpa-usage-statistics/main.go:344` (`toRecord`)
- Modify: `/mnt/dungchung/cpa-usage-statistics/types.go` (struct `Record`)
- Modify: `/mnt/dungchung/cpa-usage-statistics/store.go` (schema + INSERT + SELECT)

**Step 1: Thêm field vào struct**
```go
type usageRecord struct {
	RequestID       string       `json:"RequestID"`   // ← THÊM
	Provider        string       `json:"Provider"`
	...
}
```

**Step 2: Map trong toRecord**
```go
return Record{
    ID:        uuid.NewString(),
    RequestID: strings.TrimSpace(rec.RequestID),   // ← THÊM
    ...
}
```

**Step 3: Thêm cột DB (migration an toàn)**
```go
// store.go — trong ensureSchema(), sau CREATE TABLE usage_records:
_, _ = s.db.Exec(`ALTER TABLE usage_records ADD COLUMN request_id TEXT`)
_, _ = s.db.Exec(`CREATE INDEX IF NOT EXISTS idx_ur_request_id ON usage_records(request_id)`)
```
*(ALTER lỗi nếu cột đã tồn tại → bỏ qua bằng `_`)*

**Step 4: Thêm vào INSERT** — thêm `request_id` vào danh sách cột + `?`.

**Step 5: Test**
```bash
USAGE_TEST_DB=/mnt/dungchung/hermes-data/usage-overview-builds/dbsnap/usage.db \
  go test -run TestInsert -v .
```

**Step 6: Commit** — `git commit -m "feat(usage): persist the CPA request id on each usage record"`

---

### Task 2: Nới ring buffer payload 300 → 3000

**Objective:** Không bị mất payload khi có nhiều request đồng thời.

**Files:**
- Modify: `/mnt/dungchung/cpa-usage-statistics/payload_capture.go:60`

**Step 1: Đổi hằng số**
```go
// Keep history bounded to 3000 items (was 300 — too small under agent-loop
// concurrency, so most previews were evicted before the response arrived).
if len(m.history) > 3000 {
```

**Step 2: Commit** — `git commit -m "fix(usage): keep 3000 in-flight payloads instead of 300"`

---

### Task 3: Sửa lookup payload theo `request_id`

**Objective:** Drawer tìm được preview (hiện tại luôn `found:false`).

**Files:**
- Modify: `/mnt/dungchung/cpa-usage-statistics/store.go` (`GetChatPayload`)
- Modify: `/mnt/dungchung/cpa-usage-statistics/dashboard2.html:3010` (gửi `request_id` thay vì `id`)

**Step 1: Thêm hàm lookup 2 chặng**
```go
// GetChatPayloadForUsage resolves the preview for one usage row: first by the
// CPA request id stored on the row, then by an approximate timestamp window.
func (s *SQLiteStore) GetChatPayloadForUsage(ctx context.Context, usageID string) (*ChatPayloadRecord, error) {
	var rid string
	_ = s.db.QueryRowContext(ctx, `SELECT request_id FROM usage_records WHERE id = ?`, usageID).Scan(&rid)
	if strings.TrimSpace(rid) != "" {
		if rec, err := s.GetChatPayload(ctx, rid, time.Time{}); err == nil && rec != nil {
			return rec, nil
		}
	}
	// fallback: timestamp window
	var ts string
	_ = s.db.QueryRowContext(ctx, `SELECT timestamp FROM usage_records WHERE id = ?`, usageID).Scan(&ts)
	if t, err := time.Parse(time.RFC3339Nano, ts); err == nil {
		return s.GetChatPayload(ctx, "", t)
	}
	return nil, nil
}
```

**Step 2: Đổi `/payload` handler** để đọc `usage_id` rồi gọi hàm trên.

**Step 3:** Dashboard gửi `?usage_id=<r.id>`.

**Step 4: Commit** — `git commit -m "fix(usage): resolve chat previews through the stored CPA request id"`

---

### Task 4: Verify + Deploy

**Step 1:** Build
```bash
cd /mnt/dungchung/cpa-usage-statistics
docker run --rm -v "$PWD":/src -w /src golang:1.26 sh -c \
  'CGO_ENABLED=1 go build -trimpath -buildvcs=false -ldflags "-s -w" -buildmode=c-shared -o /src/usage-statistics.so . && echo BUILD_OK'
```

**Step 2:** Chạy test với DB thật
```bash
docker run --rm -v /mnt/dungchung/cpa-usage-statistics:/src -w /src \
  -e USAGE_TEST_DB=/src/live-usage-copy.db golang:1.26 \
  sh -c "go test -buildvcs=false -run TestChatPayload -v ."
```

**Step 3:** Chờ bro duyệt → deploy (copy `.so`, `chown 1004:1004`, `chmod 644`, `docker compose up -d --force-recreate`)

**Step 4:** Verify sau 1 giờ
```sql
SELECT COUNT(*) FROM chat_previews;  -- phải tăng mạnh
```

---

## TASKS — PHƯƠNG ÁN C (làm sau nếu cần)

### Task 5: Thử lấy IP từ `request.intercept_before`

**Objective:** Xác nhận CPA có truyền client address không.

**Cách:** Thêm log tạm in TOÀN BỘ payload nhận được ở `handleRequestIntercept`, chạy 1 giờ, xem có field nào chứa IP.
```go
func handleRequestIntercept(raw []byte) ([]byte, error) {
    os.WriteFile("/tmp/intercept_dump.jsonl", append(raw, '\n'), 0644)  // tạm thời
    ...
}
```

**Nếu có IP** → lưu vào `client_ip`, hiện cột IP.
**Nếu không** → bỏ cột IP, hoặc lấy IP từ reverse proxy (cloudflared) ở phía trước.

---

### Task 6: Tạo bảng `request_logs`

Như schema ở trên. Thêm retention cleanup theo `log_retention_days`.

---

### Task 7: Tab "Log Request" trên dashboard

Bảng + drawer + filter. Copy pattern từ tab "Tra cứu Requests" (`loadRequestsPage` / `showDetail`).

---

## RỦI RO & LƯU Ý

1. **Disk**: Phương án C ~30 MB/ngày. Set `log_retention_days: 7` mặc định.
2. **Bảo mật**: request/response body có thể chứa API key hoặc dữ liệu nhạy cảm.
   - Phải **mask** các chuỗi `sk-...` trước khi lưu.
   - Cân nhắc chỉ lưu khi `failed = 1` nếu bro lo ngại.
3. **Hiệu năng**: Interceptor chạy TRÊN ĐƯỜNG đi của mọi request. Mọi thao tác nặng phải `go func()` async.
4. **Backup `.so` trước mỗi lần deploy** (đang có sẵn pattern `.bak-pre<name>-<date>`).
5. **Không tự động áp dụng** — mọi thay đổi config/DB phải bro duyệt.

---

## VERIFICATION CHECKLIST

- [ ] `SELECT COUNT(*) FROM chat_previews` tăng rõ rệt sau deploy
- [ ] Drawer hiện được prompt + response cho request gần nhất
- [ ] `usage_records.request_id` không rỗng với record mới
- [ ] CPU container không tăng (so sánh trước/sau)
- [ ] usage.db không phình quá 2× sau 24h
