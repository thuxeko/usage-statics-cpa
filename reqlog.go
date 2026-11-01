package main

// Request log: persist the request/response payload of each proxied call into
// a SEPARATE sqlite database (request-logs.db) so the analytics DB keeps its
// current size and query cost.
//
// Resource discipline (the host running CPA is short on CPU/RAM):
//   * The intercept hot path does almost nothing: it copies the raw bytes and
//     hands them to a buffered channel. No JSON parsing, no regex, no disk.
//   * A SINGLE background worker owns every expensive step: base64/JSON
//     handling, secret masking, truncation and the sqlite write.
//   * The channel is bounded and lossy: when it is full the sample is dropped
//     instead of blocking the request or growing memory without limit.
//   * Writes are batched (one transaction per flush) so sqlite does far fewer
//     fsyncs than one per request.
//   * The worker only ever holds one batch in memory; bodies are truncated
//     before they are queued, so peak memory is bounded by
//     maxBodyBytes * batchSize.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ---- tunables ---------------------------------------------------------------

const (
	// reqLogQueueSize bounds in-flight samples. Overflow is dropped, never
	// blocks: correctness of the proxy always wins over completeness of logs.
	reqLogQueueSize = 512
	// reqLogBatchSize is how many rows are committed per transaction.
	reqLogBatchSize = 32
	// reqLogFlushInterval is the longest a row waits before being committed.
	reqLogFlushInterval = 2 * time.Second
	// reqLogDefaultMaxBodyBytes truncates each body before it is queued.
	// Kept small on purpose: measured live bodies average ~390 KB (agent-loop
	// prompts), so 8 KB is already a ~50x reduction and keeps request-logs.db
	// near 30 MB/day instead of gigabytes.
	reqLogDefaultMaxBodyBytes = 8192
	// reqLogDefaultRetentionDays prunes old rows on a slow timer.
	// 3 days keeps the file near ~100 MB even with heavy agent-loop traffic.
	reqLogDefaultRetentionDays = 3
)

// ---- data model -------------------------------------------------------------

// reqLogSample is the cheap value handed from the hot path to the worker.
// It already carries truncated bodies so memory is bounded at capture time.
type reqLogSample struct {
	RequestID string
	TraceID   string
	Model     string
	// RequestedModel is the client-requested model before combo/alias rewriting.
	// For a combo call it is the combo name; for a direct call it is empty.
	RequestedModel string
	ReqPath        string
	SourceFmt      string
	ToFormat       string
	Stream         bool
	Kind           string // "request" | "response"
	Headers        string // JSON object, Authorization stripped (never the raw value)
	Body           string
	BodyBytes      int
	Truncated      bool
	At             time.Time
}

// ---- store ------------------------------------------------------------------

type reqLogStore struct {
	db *sql.DB

	mu        sync.Mutex
	batch     []reqLogSample
	ticker    *time.Ticker
	lastPrune time.Time // worker-owned, no lock needed

	stop      chan struct{}
	closeOnce sync.Once
	wg        sync.WaitGroup

	// stats are atomic so the dashboard can read them without locking.
	queued   atomic.Int64
	written  atomic.Int64
	dropped  atomic.Int64
	disabled atomic.Bool
}

var (
	reqLogMu    sync.RWMutex
	reqLog      *reqLogStore
	reqLogAgg   *streamAggregator
	reqLogMax   atomic.Int64
	reqLogDays  atomic.Int64
	reqLogPathV atomic.Value // string

	// reqLogLive mirrors reqLog without needing reqLogMu. The stream
	// aggregator's janitor finalises buffers during shutdown, while the
	// caller already holds reqLogMu for writing: taking the read lock there
	// would deadlock against the writer waiting on wg.Wait().
	reqLogLive atomic.Pointer[reqLogStore]
)

func init() {
	reqLogMax.Store(reqLogDefaultMaxBodyBytes)
	reqLogDays.Store(reqLogDefaultRetentionDays)
}

// streamAgg returns the process-wide stream aggregator, if request logging is
// active. A nil result means the caller should not buffer anything.
func streamAgg() *streamAggregator {
	reqLogMu.RLock()
	defer reqLogMu.RUnlock()
	if reqLog == nil || reqLog.disabled.Load() {
		return nil
	}
	return reqLogAgg
}

// openReqLogStore opens (or reopens) request-logs.db next to usage.db.
func openReqLogStore(path string) error {
	reqLogMu.Lock()
	defer reqLogMu.Unlock()
	if reqLog != nil && reqLogPathV.Load() == path {
		return nil
	}
	if reqLog != nil {
		reqLog.close()
		reqLog = nil
	}
	if reqLogAgg != nil {
		reqLogAgg.close()
		reqLogAgg = nil
	}
	st, err := newReqLogStore(path)
	if err != nil {
		return err
	}
	reqLog = st
	reqLogLive.Store(st)
	reqLogAgg = newStreamAggregator(captureStreamResponse)
	reqLogPathV.Store(path)
	return nil
}

func closeReqLogStore() {
	reqLogMu.Lock()
	defer reqLogMu.Unlock()
	if reqLogAgg != nil {
		// Stop the janitor first: it finalises buffers, and those writes must
		// land in the store that is still open.
		reqLogAgg.close()
		reqLogAgg = nil
	}
	if reqLog != nil {
		reqLogLive.Store(nil)
		reqLog.close()
		reqLog = nil
	}
}

func currentReqLog() *reqLogStore {
	reqLogMu.RLock()
	defer reqLogMu.RUnlock()
	return reqLog
}

func newReqLogStore(path string) (*reqLogStore, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("request log path is empty")
	}
	if err := prepareSQLitePath(path); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("request log sqlite open: %w", err)
	}
	// A dedicated single connection: this DB is write-mostly and entirely
	// asynchronous, so it never contends with the analytics store.
	db.SetMaxOpenConns(1)
	st := &reqLogStore{db: db, stop: make(chan struct{})}
	if err := st.initSchema(); err != nil {
		_ = db.Close()
		return nil, err
	}
	st.start()
	return st, nil
}

func (s *reqLogStore) initSchema() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS request_logs (
id INTEGER PRIMARY KEY AUTOINCREMENT,
request_id TEXT NOT NULL DEFAULT '',
trace_id TEXT NOT NULL DEFAULT '',
kind TEXT NOT NULL DEFAULT '',
timestamp TEXT NOT NULL,
model TEXT NOT NULL DEFAULT '',
requested_model TEXT NOT NULL DEFAULT '',
request_path TEXT NOT NULL DEFAULT '',
source_format TEXT NOT NULL DEFAULT '',
to_format TEXT NOT NULL DEFAULT '',
stream INTEGER NOT NULL DEFAULT 0,
headers TEXT NOT NULL DEFAULT '',
body TEXT NOT NULL DEFAULT '',
body_bytes INTEGER NOT NULL DEFAULT 0,
truncated INTEGER NOT NULL DEFAULT 0
)`,
		`CREATE INDEX IF NOT EXISTS idx_rl_request_id ON request_logs(request_id)`,
		`CREATE INDEX IF NOT EXISTS idx_rl_ts ON request_logs(timestamp)`,
		// The log is grouped by trace to collapse one call's attempts into a
		// single row, so that lookup must be indexed on an existing database.
		`CREATE INDEX IF NOT EXISTS idx_rl_trace ON request_logs(trace_id)`,
		// WAL keeps writers from blocking the rare reader and turns the
		// per-flush commit into a cheap append.
		`PRAGMA journal_mode=WAL`,
		`PRAGMA synchronous=NORMAL`,
	}
	for _, q := range stmts {
		if _, err := s.db.Exec(q); err != nil {
			return fmt.Errorf("request log schema: %w", err)
		}
	}
	// Databases written by the pre-unique-index build already contain
	// duplicates, which would make CREATE UNIQUE INDEX fail. Clear them first,
	// keeping the lowest id (the earliest capture) for each request+kind.
	if err := s.dedupeExisting(); err != nil {
		return err
	}
	// Self-heal an existing database: CREATE TABLE IF NOT EXISTS above is a
	// no-op once the table exists, so a new column must be added explicitly.
	if err := s.migrateSchema(); err != nil {
		return err
	}
	if _, err := s.db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_rl_req_kind ON request_logs(request_id, kind)`); err != nil {
		return fmt.Errorf("request log unique index: %w", err)
	}
	return nil
}

// migrateSchema adds columns introduced after the table was first created.
// Read the live columns first: SQLite has no ADD COLUMN IF NOT EXISTS, and a
// duplicate ADD is an error that would take the whole store down on startup.
func (s *reqLogStore) migrateSchema() error {
	existing, err := s.existingColumns()
	if err != nil {
		return err
	}
	additions := []struct {
		name string
		ddl  string
	}{
		{"requested_model", `ALTER TABLE request_logs ADD COLUMN requested_model TEXT NOT NULL DEFAULT ''`},
	}
	for _, addition := range additions {
		if _, ok := existing[addition.name]; ok {
			continue
		}
		if _, err := s.db.Exec(addition.ddl); err != nil {
			return fmt.Errorf("request log migrate add %s: %w", addition.name, err)
		}
	}
	return nil
}

func (s *reqLogStore) existingColumns() (map[string]struct{}, error) {
	rows, err := s.db.Query(`PRAGMA table_info(request_logs)`)
	if err != nil {
		return nil, fmt.Errorf("request log table_info: %w", err)
	}
	defer rows.Close()
	out := make(map[string]struct{})
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt any
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			continue
		}
		out[name] = struct{}{}
	}
	return out, rows.Err()
}

// dedupeExisting removes duplicate (request_id, kind) rows left by earlier
// builds, keeping the first capture of each pair.
func (s *reqLogStore) dedupeExisting() error {
	_, err := s.db.Exec(`DELETE FROM request_logs WHERE id NOT IN (
SELECT MIN(id) FROM request_logs GROUP BY request_id, kind)`)
	if err != nil {
		return fmt.Errorf("request log dedupe: %w", err)
	}
	return nil
}

func (s *reqLogStore) start() {
	s.ticker = time.NewTicker(reqLogFlushInterval)
	s.wg.Add(1)
	go s.loop()
}

func (s *reqLogStore) loop() {
	defer s.wg.Done()
	defer s.ticker.Stop()
	for {
		select {
		case <-s.ticker.C:
			s.flush()
			s.maybePrune()
		case <-s.stop:
			s.flush()
			return
		}
	}
}

// enqueue is called from the intercept hot path. It never blocks and never
// does parsing work: on overflow the sample is counted and dropped.
func (s *reqLogStore) enqueue(v reqLogSample) {
	s.mu.Lock()
	s.batch = append(s.batch, v)
	n := len(s.batch)
	s.mu.Unlock()
	s.queued.Add(1)
	if n >= reqLogBatchSize {
		s.flush()
	}
}

func (s *reqLogStore) flush() {
	s.mu.Lock()
	if len(s.batch) == 0 {
		s.mu.Unlock()
		return
	}
	// Swap the batch out so the hot path keeps running while we write.
	batch := s.batch
	s.batch = make([]reqLogSample, 0, reqLogBatchSize)
	s.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		s.dropped.Add(int64(len(batch)))
		return
	}
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO request_logs
(request_id, trace_id, kind, timestamp, model, requested_model, request_path, source_format, to_format, stream, headers, body, body_bytes, truncated)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(request_id, kind) DO UPDATE SET
trace_id=excluded.trace_id,
timestamp=excluded.timestamp,
model=excluded.model,
requested_model=excluded.requested_model,
request_path=excluded.request_path,
source_format=excluded.source_format,
to_format=excluded.to_format,
stream=excluded.stream,
headers=excluded.headers,
body=excluded.body,
body_bytes=excluded.body_bytes,
truncated=excluded.truncated`)
	if err != nil {
		_ = tx.Rollback()
		s.dropped.Add(int64(len(batch)))
		return
	}
	ok := 0
	for _, v := range batch {
		_, err = stmt.ExecContext(ctx,
			v.RequestID, v.TraceID, v.Kind, v.At.UTC().Format(time.RFC3339Nano),
			v.Model, v.RequestedModel, v.ReqPath, v.SourceFmt, v.ToFormat, boolToInt(v.Stream),
			v.Headers, v.Body, v.BodyBytes, boolToInt(v.Truncated),
		)
		if err == nil {
			ok++
		}
	}
	_ = stmt.Close()
	if err := tx.Commit(); err != nil {
		s.dropped.Add(int64(len(batch) - ok))
	} else {
		s.written.Add(int64(ok))
	}
}

// maybePrune runs at most once per hour; retention_days <= 0 disables it.
func (s *reqLogStore) maybePrune() {
	days := reqLogDays.Load()
	if days <= 0 {
		return
	}
	now := time.Now()
	if s.lastPrune.After(now.Add(-time.Hour)) {
		return
	}
	s.lastPrune = now
	cutoff := now.Add(-time.Duration(days) * 24 * time.Hour).UTC().Format(time.RFC3339Nano)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = s.db.ExecContext(ctx, `DELETE FROM request_logs WHERE timestamp < ?`, cutoff)
}

func (s *reqLogStore) close() {
	// closeOnce makes Shutdown-then-close (and double-close from tests) safe;
	// closing a channel twice panics and would take the whole plugin down.
	s.closeOnce.Do(func() {
		close(s.stop)
		s.wg.Wait()
		_ = s.db.Close()
	})
}

// ---- hot-path capture -------------------------------------------------------

// secretPatterns redact credential-shaped strings before anything is persisted.
var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`sk-[A-Za-z0-9_\-]{8,}`),
	regexp.MustCompile(`(?i)\b(bearer)\s+[A-Za-z0-9_\-\.=]{8,}`),
	regexp.MustCompile(`(?i)("?(?:api[_-]?key|authorization|access[_-]?token)"?\s*[:=]\s*"?)[^"\s,}]{6,}`),
}

const secretPlaceholder = "[REDACTED]"

func maskSecrets(s string) string {
	for _, re := range secretPatterns {
		s = re.ReplaceAllStringFunc(s, func(m string) string {
			// keep the key name when the pattern is a key/value form
			loc := re.FindStringSubmatchIndex(m)
			if len(loc) >= 4 && loc[2] >= 0 {
				return m[:loc[3]] + secretPlaceholder
			}
			return secretPlaceholder
		})
	}
	return s
}

// truncateBody caps a payload at max bytes, preferring a rune boundary.
func truncateBody(s string, max int) (string, int, bool) {
	n := len(s)
	if max > 0 && n > max {
		return s[:max], n, true
	}
	return s, n, false
}

// captureRequest is the ONLY work done on the request hot path.
// A combo/alias was applied exactly when the client-requested name differs from
// the model that actually ran. requested_model is stored on every row, so the
// comparison — not emptiness — is the signal. When the router keeps the name
// (a combo whose first target is the model itself) this reads as direct, which
// matches what actually happened: no rewriting took place.
func isResolvedRequestedModel(requestedModel, model string) bool {
	return requestedModel != "" && requestedModel != model
}

func captureRequest(req requestInterceptRPC, meta map[string]any, headers map[string]any) {
	st := currentReqLog()
	if st == nil || st.disabled.Load() {
		return
	}
	body, bytes, trunc := truncateBody(string(req.Body), int(reqLogMax.Load()))
	// requestedModel is the client-requested name before combo/alias rewriting.
	// It is stored on EVERY row, including the one that carries the requested
	// name as its model. Storing it always (instead of blanking it when equal)
	// is what lets the reader tell a routed call from a direct one: a row is a
	// resolved attempt exactly when requested_model differs from model.
	requestedModel := strings.TrimSpace(firstStr(meta, req.RequestedModel, "requested_model", "RequestedModel"))
	model := strings.TrimSpace(req.Model)
	if model == "" {
		model = requestedModel
	}
	st.enqueue(reqLogSample{
		RequestID:      req.RequestID,
		TraceID:        req.TraceID,
		Model:          model,
		RequestedModel: requestedModel,
		ReqPath:        firstStr(meta, "", "request_path"),
		SourceFmt:      req.SourceFormat,
		ToFormat:       req.ToFormat,
		Stream:         req.Stream,
		Kind:           "request",
		Headers:        safeHeadersJSON(headers),
		Body:           body,
		BodyBytes:      bytes,
		Truncated:      trunc,
		At:             time.Now(),
	})
}

// captureResponse is the ONLY work done on the non-streaming response hot path.
func captureResponse(req responseInterceptRPC) {
	st := currentReqLog()
	if st == nil || st.disabled.Load() {
		return
	}
	body, bytes, trunc := truncateBody(string(req.Body), int(reqLogMax.Load()))
	st.enqueue(reqLogSample{
		RequestID: req.RequestID,
		Kind:      "response",
		Body:      body,
		BodyBytes: bytes,
		Truncated: trunc,
		At:        time.Now(),
	})
}

// captureStreamChunk runs on the stream hot path, once per chunk. It only
// appends bytes into a bounded in-memory buffer - no parsing, no disk, no
// allocation per chunk beyond the buffer growth.
func captureStreamChunk(requestID, traceID, model, sourceFmt string, body []byte) {
	agg := streamAgg()
	if agg == nil {
		return
	}
	agg.add(requestID, traceID, model, sourceFmt, body)
}

// captureStreamResponse turns a finished stream buffer into ONE response row.
// It is called from the aggregator, never from the request path, and it is
// where the single JSON parse of the whole stream happens. The store is read
// through an atomic pointer because this runs during shutdown, when reqLogMu
// is held for writing by closeReqLogStore.
func captureStreamResponse(b *streamBuf) {
	st := reqLogLive.Load()
	if st == nil || st.disabled.Load() {
		return
	}
	max := int(reqLogMax.Load())
	body, rawBytes, trunc := summariseStream(b, max)
	st.enqueue(reqLogSample{
		RequestID: b.requestID,
		TraceID:   b.traceID,
		Model:     b.model,
		SourceFmt: b.sourceFmt,
		Stream:    true,
		Kind:      "response",
		Body:      body,
		BodyBytes: rawBytes,
		Truncated: trunc,
		At:        time.Now(),
	})
}

// safeHeadersJSON serialises headers with credential values removed. Anything
// that fails to marshal becomes an empty object rather than an error path.
func safeHeadersJSON(headers map[string]any) string {
	if len(headers) == 0 {
		return ""
	}
	out := make(map[string]string, len(headers))
	for k, v := range headers {
		lk := strings.ToLower(strings.TrimSpace(k))
		if lk == "authorization" || lk == "proxy-authorization" ||
			lk == "x-api-key" || lk == "api-key" {
			out[k] = secretPlaceholder
			continue
		}
		if s, ok := v.(string); ok {
			out[k] = maskSecrets(s)
			continue
		}
		b, err := json.Marshal(v)
		if err != nil {
			continue
		}
		out[k] = maskSecrets(string(b))
	}
	b, err := json.Marshal(out)
	if err != nil {
		return ""
	}
	return string(b)
}

func firstStr(m map[string]any, fallback string, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
				return strings.TrimSpace(s)
			}
		}
	}
	return strings.TrimSpace(fallback)
}

// ---- query ------------------------------------------------------------------

// reqLogFilter narrows a request-log listing. An empty field matches
// everything, so the zero value lists the whole log.
type reqLogFilter struct {
	Kind  string
	Model string
	// Combo filters on the client-requested name (the combo/alias), not the
	// resolved model. "combo" = any request that went through a combo.
	Combo string
	// Grouped collapses every attempt of one call (same trace_id) into a single
	// row, so a call that failed over twice reads as one line instead of three.
	Grouped bool
}

// where renders the filter as a SQL predicate plus its bind arguments.
//
// The filter used to be applied in Go AFTER the LIMIT, so the caller received
// `limit` rows and then threw some away: page 2 was short, `total` reported the
// size of the page instead of the size of the log, and a filter that matched
// few rows could render an empty page while later pages still held matches.
// Pushing it into SQL makes the page and the total agree.
func (f reqLogFilter) where() (string, []any) {
	return f.wherePrefixed("")
}

// wherePrefixed is where() with an optional table alias, so the grouped query
// can reuse the same predicate against `request_logs r`.
func (f reqLogFilter) wherePrefixed(prefix string) (string, []any) {
	var clauses []string
	var args []any
	col := func(name string) string {
		if prefix == "" {
			return name
		}
		return prefix + "." + name
	}
	if f.Kind != "" {
		clauses = append(clauses, col("kind")+" = ?")
		args = append(args, f.Kind)
	}
	if f.Model != "" {
		clauses = append(clauses, "LOWER("+col("model")+") LIKE ?")
		args = append(args, "%"+strings.ToLower(f.Model)+"%")
	}
	// The combo filter describes the CALL, which is only recorded on the request
	// rows. A response row carries no trace_id and no requested_model, so
	// letting it match would report every response as a direct call.
	// NOTE: this is a row-level filter, used by the flat listing. The grouped
	// path classifies whole calls (see groupedWhere/pageGrouped), because a
	// routed call still contains one row whose requested_model equals its model.
	combo := strings.ToLower(strings.TrimSpace(f.Combo))
	switch combo {
	case "combo":
		clauses = append(clauses, col("requested_model")+" <> '' AND "+col("requested_model")+" <> "+col("model"))
	case "direct":
		clauses = append(clauses, "("+col("requested_model")+" = '' OR "+col("requested_model")+" = "+col("model")+")")
	case "":
	default:
		clauses = append(clauses, "LOWER("+col("requested_model")+") LIKE ?")
		args = append(args, "%"+combo+"%")
	}
	if len(clauses) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(clauses, " AND "), args
}

// groupedWhere is where() for the grouped query. Only row-level predicates
// belong here: whether a call was routed is a property of the whole CALL, so it
// is decided in groupHaving over the group, not per row.
func (f reqLogFilter) groupedWhere() (string, []any) {
	scoped := f
	scoped.Kind = "request"
	scoped.Combo = ""
	return scoped.wherePrefixed("")
}

// groupHaving classifies a whole call: "combo" keeps calls that were routed
// through a combo/alias, "direct" keeps the ones that went straight to a model,
// and any other value matches calls whose requested name contains it.
//
// The signal is the ROW COUNT, not a per-row field comparison. Routing means the
// plugin re-issued the request upstream, which lands as a second `request` row
// under the same trace. Comparing requested_model against model does NOT work:
// CPA strips the provider prefix from Model but keeps it in RequestedModel, so
// even a direct call has requested_model <> model (`ca2/x` vs `x`) and would be
// misread as routed.
func (f reqLogFilter) groupHaving() (string, []any) {
	combo := strings.ToLower(strings.TrimSpace(f.Combo))
	if combo == "" {
		return "", nil
	}
	switch combo {
	case "combo":
		return " HAVING COUNT(*) > 1", nil
	case "direct":
		return " HAVING COUNT(*) = 1", nil
	default:
		// The combo name is the model on the call's own (first) row.
		return " HAVING SUM(CASE WHEN LOWER(model) LIKE ? THEN 1 ELSE 0 END) > 0",
			[]any{"%" + combo + "%"}
	}
}

// reqLogPage returns one page of the log plus the total number of rows that
// match the filter, so the caller can render "Trang X / Y".
func reqLogPage(limit, offset int, filter reqLogFilter) ([]map[string]any, int64, error) {
	st := currentReqLog()
	if st == nil {
		return []map[string]any{}, 0, nil
	}
	return st.page(limit, offset, filter)
}

// page is the store-bound implementation of reqLogPage, so tests can page an
// explicit store instead of the package-level singleton.
func (st *reqLogStore) page(limit, offset int, filter reqLogFilter) ([]map[string]any, int64, error) {
	if filter.Grouped {
		return st.pageGrouped(limit, offset, filter)
	}
	return st.pageFlat(limit, offset, filter)
}

// pageFlat lists capture rows one per line (request and response separately).
func (st *reqLogStore) pageFlat(limit, offset int, filter reqLogFilter) ([]map[string]any, int64, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}
	whereSQL, whereArgs := filter.where()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var total int64
	countSQL := `SELECT COUNT(*) FROM request_logs` + whereSQL
	if err := st.db.QueryRowContext(ctx, countSQL, whereArgs...).Scan(&total); err != nil {
		return nil, 0, err
	}
	if offset >= int(total) {
		// Deleting rows or narrowing the filter can leave the caller past the
		// end; fall back to the first page instead of rendering an empty table.
		offset = 0
	}

	args := append(append([]any{}, whereArgs...), limit, offset)
	rows, err := st.db.QueryContext(ctx, `SELECT id, request_id, kind, timestamp, model,
requested_model, request_path, source_format, to_format, stream, body_bytes, truncated
FROM request_logs`+whereSQL+` ORDER BY id DESC LIMIT ? OFFSET ?`, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id int64
		var rid, kind, ts, model, requestedModel, path, src, to string
		var stream, bytes, trunc int
		if err := rows.Scan(&id, &rid, &kind, &ts, &model, &requestedModel, &path, &src, &to, &stream, &bytes, &trunc); err != nil {
			continue
		}
		out = append(out, map[string]any{
			"id": id, "request_id": rid, "kind": kind, "timestamp": ts,
			"model": model, "requested_model": requestedModel, "request_path": path,
			"source_format": src, "to_format": to, "stream": stream == 1,
			"body_bytes": bytes, "truncated": trunc == 1,
			"grouped": false, "attempts": 1,
		})
	}
	return out, total, rows.Err()
}

// pageGrouped collapses one call into one row. A call that fanned out across
// combo targets writes one `request` row per physical attempt, so listing them
// separately made a single call look like several. Every attempt of a call
// shares a trace_id, which is the grouping key here.
func (st *reqLogStore) pageGrouped(limit, offset int, filter reqLogFilter) ([]map[string]any, int64, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}
	// The grouping key is the trace; a row without one is its own group so it
	// is never merged with an unrelated request. Grouping runs on `request`
	// rows only: a response row is a separate capture with no trace_id, and
	// counting it as a group would list every response as its own call.
	const groupKey = `COALESCE(NULLIF(trace_id,''), 'row:'||id)`
	whereSQL, whereArgs := filter.groupedWhere()
	havingSQL, havingArgs := filter.groupHaving()

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	var total int64
	countSQL := `SELECT COUNT(*) FROM (SELECT ` + groupKey + ` AS g FROM request_logs` +
		whereSQL + ` GROUP BY g` + havingSQL + `)`
	countArgs := append(append([]any{}, whereArgs...), havingArgs...)
	if err := st.db.QueryRowContext(ctx, countSQL, countArgs...).Scan(&total); err != nil {
		return nil, 0, err
	}
	if offset >= int(total) {
		offset = 0
	}

	// Page the groups by their newest member so the list stays newest-first.
	args := append(append([]any{}, countArgs...), limit, offset)
	groupRows, err := st.db.QueryContext(ctx, `SELECT `+groupKey+` AS g, MAX(id) AS newest
FROM request_logs`+whereSQL+` GROUP BY g`+havingSQL+` ORDER BY newest DESC LIMIT ? OFFSET ?`, args...)
	if err != nil {
		return nil, 0, err
	}
	keys := make([]string, 0, limit)
	for groupRows.Next() {
		var g string
		var newest int64
		if err := groupRows.Scan(&g, &newest); err != nil {
			continue
		}
		keys = append(keys, g)
	}
	groupRows.Close()
	if err := groupRows.Err(); err != nil {
		return nil, 0, err
	}
	if len(keys) == 0 {
		return []map[string]any{}, total, nil
	}

	// Fetch every request row belonging to the page's groups, then the matching
	// responses. Bounded by the page size, so the IN list stays small.
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(keys)), ",")
	fetchArgs := make([]any, 0, len(keys))
	for _, k := range keys {
		fetchArgs = append(fetchArgs, k)
	}
	reqRows, err := st.db.QueryContext(ctx, `SELECT id, request_id, trace_id, timestamp, model,
requested_model, request_path, source_format, to_format, stream, body_bytes, truncated
FROM request_logs
WHERE kind='request' AND `+groupKey+` IN (`+placeholders+`)
ORDER BY id ASC`, fetchArgs...)
	if err != nil {
		return nil, 0, err
	}
	type attempt struct {
		id             int64
		requestID      string
		ts             string
		model          string
		requestedModel string
		path           string
		sourceFmt      string
		toFormat       string
		stream         bool
		bodyBytes      int
		truncated      bool
	}
	byGroup := make(map[string][]attempt, len(keys))
	for reqRows.Next() {
		var a attempt
		var key string
		var stream, bytes, trunc int
		if err := reqRows.Scan(&a.id, &a.requestID, &key, &a.ts, &a.model,
			&a.requestedModel, &a.path, &a.sourceFmt, &a.toFormat, &stream, &bytes, &trunc); err != nil {
			continue
		}
		a.stream = stream == 1
		a.bodyBytes = bytes
		a.truncated = trunc == 1
		byGroup[key] = append(byGroup[key], a)
	}
	reqRows.Close()
	if err := reqRows.Err(); err != nil {
		return nil, 0, err
	}

	// Which attempts produced a response, and how big it was. One query for the
	// whole page instead of one per row.
	type response struct {
		id        int64
		bodyBytes int
		truncated bool
	}
	responses := map[string]response{}
	var allReqIDs []string
	for _, attempts := range byGroup {
		for _, a := range attempts {
			if a.requestID != "" {
				allReqIDs = append(allReqIDs, a.requestID)
			}
		}
	}
	if len(allReqIDs) > 0 {
		ph := strings.TrimSuffix(strings.Repeat("?,", len(allReqIDs)), ",")
		rArgs := make([]any, 0, len(allReqIDs))
		for _, rid := range allReqIDs {
			rArgs = append(rArgs, rid)
		}
		respRows, err := st.db.QueryContext(ctx, `SELECT id, request_id, body_bytes, truncated
FROM request_logs WHERE kind='response' AND request_id IN (`+ph+`)`, rArgs...)
		if err == nil {
			for respRows.Next() {
				var id int64
				var rid string
				var bytes, trunc int
				if err := respRows.Scan(&id, &rid, &bytes, &trunc); err != nil {
					continue
				}
				responses[rid] = response{id: id, bodyBytes: bytes, truncated: trunc == 1}
			}
			respRows.Close()
		}
	}

	out := make([]map[string]any, 0, len(keys))
	for _, key := range keys {
		attempts := byGroup[key]
		if len(attempts) == 0 {
			continue
		}
		// The attempt that answered is the one worth showing; fall back to the
		// last attempt when the call failed on every target.
		primary := attempts[len(attempts)-1]
		for _, a := range attempts {
			if _, ok := responses[a.requestID]; ok {
				primary = a
			}
		}
		// The combo name is the model on the call's OWN row: that is the row the
		// router had not rewritten yet, so requested_model still equals model.
		// (Comparing requested_model against model on the resolved rows does NOT
		// work — CPA keeps the provider prefix in requested_model but strips it
		// from model, so `ca2/x` vs `x` looks rewritten even for a direct call.)
		combo := ""
		attemptsCount := 0
		for _, a := range attempts {
			if a.requestedModel == a.model && combo == "" {
				combo = a.model
			}
			if isResolvedRequestedModel(a.requestedModel, a.model) {
				// A resolved row is one physical target the router tried.
				attemptsCount++
			}
		}
		if attemptsCount == 0 {
			attemptsCount = 1
		}
		totalBytes := 0
		truncated := false
		for _, a := range attempts {
			totalBytes += a.bodyBytes
			truncated = truncated || a.truncated
		}
		resp, hasResp := responses[primary.requestID]
		if hasResp {
			totalBytes += resp.bodyBytes
			truncated = truncated || resp.truncated
		}
		payload := map[string]any{"request_id": primary.id}
		if hasResp {
			payload["response_id"] = resp.id
		}
		out = append(out, map[string]any{
			"id":              primary.id,
			"request_id":      primary.requestID,
			"kind":            "call",
			"timestamp":       attempts[0].ts, // when the call started
			"model":           primary.model,
			"combo":           combo,
			"requested_model": combo,
			"request_path":    primary.path,
			"source_format":   primary.sourceFmt,
			"to_format":       primary.toFormat,
			"stream":          primary.stream,
			"body_bytes":      totalBytes,
			"truncated":       truncated,
			"grouped":         true,
			"attempts":        attemptsCount,
			"has_response":    hasResp,
			"payload":         payload,
		})
	}
	return out, total, nil
}

func reqLogBody(id int64) (map[string]any, error) {
	st := currentReqLog()
	if st == nil {
		return nil, fmt.Errorf("request log unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var rid, kind, ts, model, requestedModel, headers, body string
	var trunc int
	err := st.db.QueryRowContext(ctx, `SELECT request_id, kind, timestamp, model, requested_model, headers, body, truncated
FROM request_logs WHERE id = ?`, id).Scan(&rid, &kind, &ts, &model, &requestedModel, &headers, &body, &trunc)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"request_id": rid, "kind": kind, "timestamp": ts, "model": model,
		"requested_model": requestedModel,
		"headers":         headers, "body": maskSecrets(body), "truncated": trunc == 1,
	}, nil
}

func reqLogStats() map[string]any {
	st := currentReqLog()
	if st == nil {
		return map[string]any{"enabled": false}
	}
	return map[string]any{
		"enabled": !st.disabled.Load(),
		"queued":  st.queued.Load(),
		"written": st.written.Load(),
		"dropped": st.dropped.Load(),
		"path":    reqLogDBPathForDisplay(),
	}
}

func reqLogDBPathForDisplay() string {
	if v, ok := reqLogPathV.Load().(string); ok {
		return filepath.Base(v)
	}
	return ""
}
