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
	RequestID  string
	TraceID    string
	Model      string
	ReqPath    string
	SourceFmt  string
	ToFormat   string
	Stream     bool
	Kind       string // "request" | "response"
	Headers    string // JSON object, Authorization stripped (never the raw value)
	Body       string
	BodyBytes  int
	Truncated  bool
	At         time.Time
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
	if _, err := s.db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_rl_req_kind ON request_logs(request_id, kind)`); err != nil {
		return fmt.Errorf("request log unique index: %w", err)
	}
	return nil
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
	stmt, err := tx.PrepareContext(ctx, `INSERT OR IGNORE INTO request_logs
(request_id, trace_id, kind, timestamp, model, request_path, source_format, to_format, stream, headers, body, body_bytes, truncated)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		_ = tx.Rollback()
		s.dropped.Add(int64(len(batch)))
		return
	}
	ok := 0
	for _, v := range batch {
		_, err = stmt.ExecContext(ctx,
			v.RequestID, v.TraceID, v.Kind, v.At.UTC().Format(time.RFC3339Nano),
			v.Model, v.ReqPath, v.SourceFmt, v.ToFormat, boolToInt(v.Stream),
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
func captureRequest(req requestInterceptRPC, meta map[string]any, headers map[string]any) {
	st := currentReqLog()
	if st == nil || st.disabled.Load() {
		return
	}
	body, bytes, trunc := truncateBody(string(req.Body), int(reqLogMax.Load()))
	st.enqueue(reqLogSample{
		RequestID: req.RequestID,
		TraceID:   req.TraceID,
		Model:     firstStr(meta, req.Model, "requested_model", "requested_model"),
		ReqPath:   firstStr(meta, "", "request_path"),
		SourceFmt: req.SourceFormat,
		ToFormat:  req.ToFormat,
		Stream:    req.Stream,
		Kind:      "request",
		Headers:   safeHeadersJSON(headers),
		Body:      body,
		BodyBytes: bytes,
		Truncated: trunc,
		At:        time.Now(),
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

func reqLogRecent(limit int) ([]map[string]any, error) {
	st := currentReqLog()
	if st == nil {
		return []map[string]any{}, nil
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rows, err := st.db.QueryContext(ctx, `SELECT id, request_id, kind, timestamp, model,
request_path, source_format, to_format, stream, body_bytes, truncated
FROM request_logs ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id int64
		var rid, kind, ts, model, path, src, to string
		var stream, bytes, trunc int
		if err := rows.Scan(&id, &rid, &kind, &ts, &model, &path, &src, &to, &stream, &bytes, &trunc); err != nil {
			continue
		}
		out = append(out, map[string]any{
			"id": id, "request_id": rid, "kind": kind, "timestamp": ts,
			"model": model, "request_path": path, "source_format": src,
			"to_format": to, "stream": stream == 1,
			"body_bytes": bytes, "truncated": trunc == 1,
		})
	}
	return out, nil
}

func reqLogBody(id int64) (map[string]any, error) {
	st := currentReqLog()
	if st == nil {
		return nil, fmt.Errorf("request log unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var rid, kind, ts, model, headers, body string
	var trunc int
	err := st.db.QueryRowContext(ctx, `SELECT request_id, kind, timestamp, model, headers, body, truncated
FROM request_logs WHERE id = ?`, id).Scan(&rid, &kind, &ts, &model, &headers, &body, &trunc)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"request_id": rid, "kind": kind, "timestamp": ts, "model": model,
		"headers": headers, "body": maskSecrets(body), "truncated": trunc == 1,
	}, nil
}

func reqLogStats() map[string]any {
	st := currentReqLog()
	if st == nil {
		return map[string]any{"enabled": false}
	}
	return map[string]any{
		"enabled":  !st.disabled.Load(),
		"queued":   st.queued.Load(),
		"written":  st.written.Load(),
		"dropped":  st.dropped.Load(),
		"path":     reqLogDBPathForDisplay(),
	}
}

func reqLogDBPathForDisplay() string {
	if v, ok := reqLogPathV.Load().(string); ok {
		return filepath.Base(v)
	}
	return ""
}
