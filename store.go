package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const sqliteTimestampLayout = "2006-01-02T15:04:05.000000000Z07:00"

// SQLiteStore persists usage records in a pure-Go SQLite database.
type SQLiteStore struct {
	db *sql.DB
}

// NewSQLiteStore opens (creating if needed) the usage database at path.
func NewSQLiteStore(path string) (*SQLiteStore, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("usage sqlite path is empty")
	}
	if err := prepareSQLitePath(path); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("usage sqlite open: %w", err)
	}
	db.SetMaxOpenConns(1)
	store := &SQLiteStore{db: db}
	if err := store.initSchema(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func prepareSQLitePath(path string) error {
	dir := filepath.Clean(filepath.Dir(path))
	if dir != "." && filepath.Dir(dir) != dir {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("usage sqlite mkdir: %w", err)
		}
	}
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("usage sqlite create: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("usage sqlite close created file: %w", err)
	}
	return nil
}

// initSchema creates the table and indexes. Column names are aligned with
// upstream usage.Record / usage.Detail; there is no legacy migration path.
func (s *SQLiteStore) initSchema(ctx context.Context) error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS usage_records (
	id TEXT PRIMARY KEY,
	timestamp TEXT NOT NULL,
	api_key TEXT NOT NULL DEFAULT '',
	provider TEXT NOT NULL DEFAULT '',
	model TEXT NOT NULL DEFAULT '',
	alias TEXT NOT NULL DEFAULT '',
	source TEXT NOT NULL DEFAULT '',
	auth_id TEXT NOT NULL DEFAULT '',
	auth_index TEXT NOT NULL DEFAULT '',
	auth_type TEXT NOT NULL DEFAULT '',
	base_url TEXT NOT NULL DEFAULT '',
	executor_type TEXT NOT NULL DEFAULT '',
	reasoning_effort TEXT NOT NULL DEFAULT '',
	service_tier TEXT NOT NULL DEFAULT '',
	latency_ms INTEGER NOT NULL DEFAULT 0 CHECK (latency_ms >= 0),
	ttft_ms INTEGER NOT NULL DEFAULT 0 CHECK (ttft_ms >= 0),
	input_tokens INTEGER NOT NULL DEFAULT 0 CHECK (input_tokens >= 0),
	output_tokens INTEGER NOT NULL DEFAULT 0 CHECK (output_tokens >= 0),
	reasoning_tokens INTEGER NOT NULL DEFAULT 0 CHECK (reasoning_tokens >= 0),
	cached_tokens INTEGER NOT NULL DEFAULT 0 CHECK (cached_tokens >= 0),
	cache_read_tokens INTEGER NOT NULL DEFAULT 0 CHECK (cache_read_tokens >= 0),
	cache_creation_tokens INTEGER NOT NULL DEFAULT 0 CHECK (cache_creation_tokens >= 0),
	total_tokens INTEGER NOT NULL DEFAULT 0 CHECK (total_tokens >= 0),
	failed INTEGER NOT NULL DEFAULT 0 CHECK (failed IN (0, 1)),
	failure_status_code INTEGER NOT NULL DEFAULT 0 CHECK (failure_status_code >= 0),
	failure_body TEXT NOT NULL DEFAULT ''
)`,
		`CREATE INDEX IF NOT EXISTS idx_usage_records_timestamp ON usage_records(timestamp)`,
		`CREATE INDEX IF NOT EXISTS idx_usage_records_api_model ON usage_records(api_key, provider, model)`,
		`CREATE TABLE IF NOT EXISTS chat_previews (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			request_id TEXT NOT NULL DEFAULT '',
			timestamp TEXT NOT NULL,
			model TEXT NOT NULL DEFAULT '',
			prompt_preview TEXT NOT NULL DEFAULT '',
			response_preview TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE TABLE IF NOT EXISTS price_overrides (
	model TEXT PRIMARY KEY,
	input REAL NOT NULL DEFAULT 0 CHECK (input >= 0),
	output REAL NOT NULL DEFAULT 0 CHECK (output >= 0),
	cache_read REAL NOT NULL DEFAULT 0 CHECK (cache_read >= 0),
	cache_creation REAL NOT NULL DEFAULT 0 CHECK (cache_creation >= 0),
	accounting_mode TEXT NOT NULL DEFAULT '',
	updated_at TEXT NOT NULL DEFAULT ''
)`,
		`CREATE INDEX IF NOT EXISTS idx_chat_previews_req ON chat_previews(request_id)`,
		`CREATE INDEX IF NOT EXISTS idx_chat_previews_ts ON chat_previews(timestamp)`,
	}
	for _, statement := range statements {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("usage sqlite init schema: %w", err)
		}
	}
	return s.migrateSchema(ctx)
}

// migrateSchema 让老版本 db 的字段自愈到最新：用 table_info 读出现有列，
// 对最新 schema 中缺失的列逐个 ALTER TABLE ADD COLUMN 补齐（SQLite 不能重复
// ADD，同名列跳过）。id/timestamp 是建表原始列（任何版本都有）且不可 ADD，
// 不在补齐范围。ADD COLUMN 只带 NOT NULL DEFAULT、不带 CHECK（SQLite 对
// ADD COLUMN 的约束限制），数值非负由写入侧 nonNegative 保证。
func (s *SQLiteStore) migrateSchema(ctx context.Context) error {
	existing, err := s.existingColumns(ctx)
	if err != nil {
		return err
	}
	additions := []struct {
		name string
		ddl  string
	}{
		{"api_key", `ALTER TABLE usage_records ADD COLUMN api_key TEXT NOT NULL DEFAULT ''`},
		{"provider", `ALTER TABLE usage_records ADD COLUMN provider TEXT NOT NULL DEFAULT ''`},
		{"model", `ALTER TABLE usage_records ADD COLUMN model TEXT NOT NULL DEFAULT ''`},
		{"alias", `ALTER TABLE usage_records ADD COLUMN alias TEXT NOT NULL DEFAULT ''`},
		{"source", `ALTER TABLE usage_records ADD COLUMN source TEXT NOT NULL DEFAULT ''`},
		{"auth_id", `ALTER TABLE usage_records ADD COLUMN auth_id TEXT NOT NULL DEFAULT ''`},
		{"auth_index", `ALTER TABLE usage_records ADD COLUMN auth_index TEXT NOT NULL DEFAULT ''`},
		{"auth_type", `ALTER TABLE usage_records ADD COLUMN auth_type TEXT NOT NULL DEFAULT ''`},
		{"base_url", `ALTER TABLE usage_records ADD COLUMN base_url TEXT NOT NULL DEFAULT ''`},
		{"executor_type", `ALTER TABLE usage_records ADD COLUMN executor_type TEXT NOT NULL DEFAULT ''`},
		{"reasoning_effort", `ALTER TABLE usage_records ADD COLUMN reasoning_effort TEXT NOT NULL DEFAULT ''`},
		{"service_tier", `ALTER TABLE usage_records ADD COLUMN service_tier TEXT NOT NULL DEFAULT ''`},
		{"latency_ms", `ALTER TABLE usage_records ADD COLUMN latency_ms INTEGER NOT NULL DEFAULT 0`},
		{"ttft_ms", `ALTER TABLE usage_records ADD COLUMN ttft_ms INTEGER NOT NULL DEFAULT 0`},
		{"input_tokens", `ALTER TABLE usage_records ADD COLUMN input_tokens INTEGER NOT NULL DEFAULT 0`},
		{"output_tokens", `ALTER TABLE usage_records ADD COLUMN output_tokens INTEGER NOT NULL DEFAULT 0`},
		{"reasoning_tokens", `ALTER TABLE usage_records ADD COLUMN reasoning_tokens INTEGER NOT NULL DEFAULT 0`},
		{"cached_tokens", `ALTER TABLE usage_records ADD COLUMN cached_tokens INTEGER NOT NULL DEFAULT 0`},
		{"cache_read_tokens", `ALTER TABLE usage_records ADD COLUMN cache_read_tokens INTEGER NOT NULL DEFAULT 0`},
		{"cache_creation_tokens", `ALTER TABLE usage_records ADD COLUMN cache_creation_tokens INTEGER NOT NULL DEFAULT 0`},
		{"total_tokens", `ALTER TABLE usage_records ADD COLUMN total_tokens INTEGER NOT NULL DEFAULT 0`},
		{"failed", `ALTER TABLE usage_records ADD COLUMN failed INTEGER NOT NULL DEFAULT 0`},
		{"failure_status_code", `ALTER TABLE usage_records ADD COLUMN failure_status_code INTEGER NOT NULL DEFAULT 0`},
		{"failure_body", `ALTER TABLE usage_records ADD COLUMN failure_body TEXT NOT NULL DEFAULT ''`},
	}
	for _, addition := range additions {
		if _, ok := existing[addition.name]; ok {
			continue
		}
		if _, err := s.db.ExecContext(ctx, addition.ddl); err != nil {
			return fmt.Errorf("usage sqlite migrate add %s: %w", addition.name, err)
		}
	}
	return nil
}

// existingColumns 返回 usage_records 当前已有的列名集合。
func (s *SQLiteStore) existingColumns(ctx context.Context) (map[string]struct{}, error) {
	rows, err := s.db.QueryContext(ctx, "PRAGMA table_info(usage_records)")
	if err != nil {
		return nil, fmt.Errorf("usage sqlite table_info: %w", err)
	}
	defer func() { _ = rows.Close() }()
	columns := make(map[string]struct{})
	for rows.Next() {
		var (
			cid       int
			name      string
			ctype     string
			notNull   int
			dfltValue sql.NullString
			pk        int
		)
		if err := rows.Scan(&cid, &name, &ctype, &notNull, &dfltValue, &pk); err != nil {
			return nil, fmt.Errorf("usage sqlite table_info scan: %w", err)
		}
		columns[name] = struct{}{}
	}
	return columns, rows.Err()
}

// Insert stores one usage record.
func (s *SQLiteStore) Insert(ctx context.Context, record Record) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("usage sqlite store is nil")
	}
	if strings.TrimSpace(record.ID) == "" {
		return fmt.Errorf("usage record id is empty")
	}

	tokens := nonNegativeTokenStats(record.Tokens)
	tokens.TotalTokens = normalizeTotalTokens(tokens)

	_, err := s.db.ExecContext(ctx, `
INSERT INTO usage_records (
	id, timestamp, api_key, provider, model, alias, source, auth_id, auth_index, auth_type, base_url, executor_type,
	reasoning_effort, service_tier, latency_ms, ttft_ms,
	input_tokens, output_tokens, reasoning_tokens, cached_tokens, cache_read_tokens, cache_creation_tokens, total_tokens,
	failed, failure_status_code, failure_body
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
`,
		strings.TrimSpace(record.ID),
		formatRecordTimestamp(record.Timestamp),
		strings.TrimSpace(record.APIKey),
		strings.TrimSpace(record.Provider),
		normalizeModel(record.Model),
		strings.TrimSpace(record.Alias),
		strings.TrimSpace(record.Source),
		strings.TrimSpace(record.AuthID),
		strings.TrimSpace(record.AuthIndex),
		strings.TrimSpace(record.AuthType),
		strings.TrimSpace(record.BaseURL),
		strings.TrimSpace(record.ExecutorType),
		strings.TrimSpace(record.ReasoningEffort),
		strings.TrimSpace(record.ServiceTier),
		nonNegative(record.LatencyMs),
		nonNegative(record.TTFTMs),
		tokens.InputTokens,
		tokens.OutputTokens,
		tokens.ReasoningTokens,
		tokens.CachedTokens,
		tokens.CacheReadTokens,
		tokens.CacheCreationTokens,
		tokens.TotalTokens,
		boolToInt(record.Failed),
		nonNegativeInt(record.FailureStatusCode),
		strings.TrimSpace(record.FailureBody),
	)
	if err != nil {
		return fmt.Errorf("usage sqlite insert: %w", err)
	}
	return nil
}

// Query returns usage grouped by api_key (or provider) then model.
func (s *SQLiteStore) Query(ctx context.Context, rng QueryRange) (APIUsage, error) {
	if s == nil || s.db == nil {
		return APIUsage{}, nil
	}
	query := `
SELECT id, timestamp, api_key, provider, model, alias, source, auth_id, auth_index, auth_type, base_url, executor_type,
       reasoning_effort, service_tier, latency_ms, ttft_ms,
       input_tokens, output_tokens, reasoning_tokens, cached_tokens, cache_read_tokens, cache_creation_tokens, total_tokens,
       failed, failure_status_code, failure_body
FROM usage_records`
	args := make([]any, 0, 2)
	where := make([]string, 0, 2)
	if rng.Start != nil && !rng.Start.IsZero() {
		where = append(where, "timestamp >= ?")
		args = append(args, formatTimestamp(*rng.Start))
	}
	if rng.End != nil && !rng.End.IsZero() {
		where = append(where, "timestamp < ?")
		args = append(args, formatTimestamp(*rng.End))
	}
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += " ORDER BY timestamp ASC, id ASC"

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("usage sqlite query: %w", err)
	}
	defer func() { _ = rows.Close() }()

	result := APIUsage{}
	for rows.Next() {
		var timestampText string
		var apiKey string
		var failedInt int
		detail := RequestDetail{}
		if err := rows.Scan(
			&detail.ID,
			&timestampText,
			&apiKey,
			&detail.Provider,
			&detail.Model,
			&detail.Alias,
			&detail.Source,
			&detail.AuthID,
			&detail.AuthIndex,
			&detail.AuthType,
			&detail.BaseURL,
			&detail.ExecutorType,
			&detail.ReasoningEffort,
			&detail.ServiceTier,
			&detail.LatencyMs,
			&detail.TTFTMs,
			&detail.Tokens.InputTokens,
			&detail.Tokens.OutputTokens,
			&detail.Tokens.ReasoningTokens,
			&detail.Tokens.CachedTokens,
			&detail.Tokens.CacheReadTokens,
			&detail.Tokens.CacheCreationTokens,
			&detail.Tokens.TotalTokens,
			&failedInt,
			&detail.FailureStatusCode,
			&detail.FailureBody,
		); err != nil {
			return nil, fmt.Errorf("usage sqlite scan: %w", err)
		}
		parsed, err := time.Parse(time.RFC3339Nano, timestampText)
		if err != nil {
			return nil, fmt.Errorf("usage sqlite parse timestamp: %w", err)
		}
		detail.Timestamp = parsed.UTC()
		detail.LatencyMs = nonNegative(detail.LatencyMs)
		detail.TTFTMs = nonNegative(detail.TTFTMs)
		detail.Failed = failedInt != 0

		key := groupingKey(apiKey, detail.Provider)
		modelKey := normalizeModel(detail.Model)
		if result[key] == nil {
			result[key] = map[string][]RequestDetail{}
		}
		result[key][modelKey] = append(result[key][modelKey], detail)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("usage sqlite rows: %w", err)
	}
	return result, nil
}

// Delete removes records by id and reports which ids were absent.
func (s *SQLiteStore) Delete(ctx context.Context, ids []string) (DeleteResult, error) {
	result := DeleteResult{Missing: []string{}}
	if s == nil || s.db == nil {
		result.Missing = append(result.Missing, ids...)
		return result, nil
	}
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		res, err := s.db.ExecContext(ctx, "DELETE FROM usage_records WHERE id = ?", id)
		if err != nil {
			return result, fmt.Errorf("usage sqlite delete %s: %w", id, err)
		}
		rows, err := res.RowsAffected()
		if err != nil {
			return result, fmt.Errorf("usage sqlite rows affected: %w", err)
		}
		if rows == 0 {
			result.Missing = append(result.Missing, id)
			continue
		}
		result.Deleted += rows
	}
	return result, nil
}

// DeleteBefore removes records older than cutoff and returns the deleted count.
func (s *SQLiteStore) DeleteBefore(ctx context.Context, cutoff time.Time) (int64, error) {
	if s == nil || s.db == nil {
		return 0, nil
	}
	tsStr := formatTimestamp(cutoff)
	res, err := s.db.ExecContext(ctx, "DELETE FROM usage_records WHERE timestamp < ?", tsStr)
	if err != nil {
		return 0, fmt.Errorf("usage sqlite delete before: %w", err)
	}
	// Also prune old chat previews
	_, _ = s.db.ExecContext(ctx, "DELETE FROM chat_previews WHERE timestamp < ?", tsStr)
	rows, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("usage sqlite rows affected: %w", err)
	}
	return rows, nil
}

// ChatPayloadRecord describes a captured prompt/response preview.
type ChatPayloadRecord struct {
	RequestID       string `json:"request_id"`
	Timestamp       string `json:"timestamp"`
	Model           string `json:"model"`
	PromptPreview   string `json:"prompt_preview"`
	ResponsePreview string `json:"response_preview"`
}

// SaveChatPayload records a chat preview in usage.db.
func (s *SQLiteStore) SaveChatPayload(ctx context.Context, requestID string, reqTS time.Time, model, prompt, response string) error {
	if s == nil || s.db == nil {
		return nil
	}
	tsStr := formatTimestamp(reqTS)
	query := `INSERT INTO chat_previews (request_id, timestamp, model, prompt_preview, response_preview)
		VALUES (?, ?, ?, ?, ?)`
	_, err := s.db.ExecContext(ctx, query, requestID, tsStr, model, prompt, response)
	return err
}

// GetChatPayload retrieves a chat preview by request_id or timestamp.
func (s *SQLiteStore) GetChatPayload(ctx context.Context, requestID string, approxTS time.Time) (*ChatPayloadRecord, error) {
	if s == nil || s.db == nil {
		return nil, nil
	}
	var rec ChatPayloadRecord
	var row *sql.Row
	if strings.TrimSpace(requestID) != "" {
		row = s.db.QueryRowContext(ctx, `SELECT request_id, timestamp, model, prompt_preview, response_preview 
			FROM chat_previews WHERE request_id = ? ORDER BY id DESC LIMIT 1`, strings.TrimSpace(requestID))
		err := row.Scan(&rec.RequestID, &rec.Timestamp, &rec.Model, &rec.PromptPreview, &rec.ResponsePreview)
		if err == nil {
			return &rec, nil
		}
	}
	if !approxTS.IsZero() {
		// Search within ±30 seconds of timestamp
		start := formatTimestamp(approxTS.Add(-30 * time.Second))
		end := formatTimestamp(approxTS.Add(30 * time.Second))
		row = s.db.QueryRowContext(ctx, `SELECT request_id, timestamp, model, prompt_preview, response_preview 
			FROM chat_previews WHERE timestamp >= ? AND timestamp <= ? ORDER BY id DESC LIMIT 1`, start, end)
		err := row.Scan(&rec.RequestID, &rec.Timestamp, &rec.Model, &rec.PromptPreview, &rec.ResponsePreview)
		if err == nil {
			return &rec, nil
		}
	}
	return nil, nil
}

// Close closes the underlying database.
func (s *SQLiteStore) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func formatTimestamp(timestamp time.Time) string {
	return timestamp.UTC().Format(sqliteTimestampLayout)
}

func formatRecordTimestamp(timestamp time.Time) string {
	if timestamp.IsZero() {
		timestamp = time.Now()
	}
	return formatTimestamp(timestamp)
}

func groupingKey(apiKey, provider string) string {
	if trimmed := strings.TrimSpace(apiKey); trimmed != "" {
		return trimmed
	}
	if trimmed := strings.TrimSpace(provider); trimmed != "" {
		return trimmed
	}
	return "unknown"
}

func normalizeModel(model string) string {
	if trimmed := strings.TrimSpace(model); trimmed != "" {
		return trimmed
	}
	return "unknown"
}

func normalizeTotalTokens(tokens TokenStats) int64 {
	if tokens.TotalTokens != 0 {
		return tokens.TotalTokens
	}
	total := tokens.InputTokens + tokens.OutputTokens + tokens.ReasoningTokens
	if total != 0 {
		return total
	}
	return tokens.InputTokens + tokens.OutputTokens + tokens.ReasoningTokens + tokens.CachedTokens
}

func nonNegativeTokenStats(tokens TokenStats) TokenStats {
	tokens.InputTokens = nonNegative(tokens.InputTokens)
	tokens.OutputTokens = nonNegative(tokens.OutputTokens)
	tokens.ReasoningTokens = nonNegative(tokens.ReasoningTokens)
	tokens.CachedTokens = nonNegative(tokens.CachedTokens)
	tokens.CacheReadTokens = nonNegative(tokens.CacheReadTokens)
	tokens.CacheCreationTokens = nonNegative(tokens.CacheCreationTokens)
	tokens.TotalTokens = nonNegative(tokens.TotalTokens)
	return tokens
}

func nonNegative(value int64) int64 {
	if value < 0 {
		return 0
	}
	return value
}

func nonNegativeInt(value int) int {
	if value < 0 {
		return 0
	}
	return value
}

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

// ---- dashboard queries ------------------------------------------------------

// filterWhere builds the WHERE clause for a time range and optional PageFilter.
func filterWhere(rng QueryRange, filter PageFilter, args []any) (string, []any) {
	where := make([]string, 0, 8)
	if rng.Start != nil && !rng.Start.IsZero() {
		where = append(where, "timestamp >= ?")
		args = append(args, formatTimestamp(*rng.Start))
	}
	if rng.End != nil && !rng.End.IsZero() {
		where = append(where, "timestamp < ?")
		args = append(args, formatTimestamp(*rng.End))
	}
	if filter.Model != "" {
		where = append(where, "(model = ? OR alias = ?)")
		args = append(args, filter.Model, filter.Model)
	}
	if filter.Provider != "" {
		where = append(where, "provider = ?")
		args = append(args, filter.Provider)
	}
	if filter.APIKey != "" {
		where = append(where, "api_key = ?")
		args = append(args, filter.APIKey)
	}
	if filter.Failed != nil {
		where = append(where, "failed = ?")
		args = append(args, boolToInt(*filter.Failed))
	}
	if filter.StatusCode > 0 {
		where = append(where, "failure_status_code = ?")
		args = append(args, filter.StatusCode)
	}
	if len(where) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(where, " AND "), args
}

// Summary computes the full dashboard aggregate for the time range: totals,
// hourly trend, per-model, per-alias, per-provider, per-api-key groupings and
// failure status-code counts. Aggregation happens in SQL so the payload stays
// small no matter how many raw records exist.
func (s *SQLiteStore) Summary(ctx context.Context, rng QueryRange, filter ...PageFilter) (*UsageSummary, error) {
	summary := &UsageSummary{
		Start:       rng.Start,
		End:         rng.End,
		ByHour:      []HourStat{},
		ByModel:     []GroupStat{},
		ByAlias:     []GroupStat{},
		ByProvider:  []GroupStat{},
		ByAPIKey:    []GroupStat{},
		StatusCodes: []StatusCodeStat{},
	}
	if s == nil || s.db == nil {
		return summary, nil
	}
	var f PageFilter
	if len(filter) > 0 {
		f = filter[0]
	}
	if err := s.fillTotals(ctx, rng, f, summary); err != nil {
		return nil, err
	}
	if err := s.fillGrouped(ctx, rng, f, summary); err != nil {
		return nil, err
	}
	if err := s.fillByHour(ctx, rng, f, summary); err != nil {
		return nil, err
	}
	if err := s.fillStatusCodes(ctx, rng, f, summary); err != nil {
		return nil, err
	}
	// Costs are priced last: they annotate the per-model rows built above.
	if err := s.fillCosts(ctx, rng, f, summary); err != nil {
		return nil, err
	}
	// Breakdowns the router path always had: per-executor, per-effort,
	// histograms, scatter and slowest requests.
	if err := s.fillDistributions(ctx, rng, f, summary); err != nil {
		return nil, err
	}
	// Which provider served each model, for the model performance table.
	if err := s.fillModelProviders(ctx, rng, f, summary); err != nil {
		return nil, err
	}
	// Derived counters, mirroring what the router path reports so the two
	// sources agree on the same payload shape.
	summary.Totals.Success = summary.Totals.Calls - summary.Totals.Failed
	summary.Totals.ActiveModels = int64(len(summary.ByModel))
	summary.Totals.ActiveKeys = int64(len(summary.ByAPIKey))
	return summary, nil
}

func (s *SQLiteStore) fillTotals(ctx context.Context, rng QueryRange, filter PageFilter, summary *UsageSummary) error {
	where, args := filterWhere(rng, filter, nil)
	query := `SELECT COUNT(*), COALESCE(SUM(failed),0),
		COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0),
		COALESCE(SUM(reasoning_tokens),0), COALESCE(SUM(cached_tokens),0),
		COALESCE(SUM(cache_creation_tokens),0),
		COALESCE(SUM(total_tokens),0),
		COALESCE(AVG(latency_ms),0), COALESCE(AVG(ttft_ms),0)
		FROM usage_records` + where
	row := s.db.QueryRowContext(ctx, query, args...)
	t := &summary.Totals
	return row.Scan(&t.Calls, &t.Failed, &t.InputTokens, &t.OutputTokens,
		&t.ReasoningTokens, &t.CachedTokens, &t.CacheCreationTokens, &t.TotalTokens,
		&t.AvgLatencyMs, &t.AvgTTFTMs)
}

// groupKeySQL mirrors the Go groupingKey helper in SQL so api_key grouping
// falls back to provider, then "unknown".
const groupKeySQL = "CASE WHEN TRIM(api_key) != '' THEN api_key WHEN TRIM(provider) != '' THEN provider ELSE 'unknown' END"

func (s *SQLiteStore) fillGrouped(ctx context.Context, rng QueryRange, filter PageFilter, summary *UsageSummary) error {
	type dim struct {
		selectExpr string
		subExpr    string
		target     *[]GroupStat
		hasSub     bool
	}
	dims := []dim{
		{selectExpr: "model", subExpr: "", target: &summary.ByModel, hasSub: false},
		// For alias groupings, fall back to the model name for direct calls so
		// every row still identifies what ran; Sub keeps the real model.
		{selectExpr: "COALESCE(NULLIF(alias,''), model)", subExpr: "model", target: &summary.ByAlias, hasSub: true},
		{selectExpr: "COALESCE(NULLIF(provider,''), 'unknown')", subExpr: "", target: &summary.ByProvider, hasSub: false},
		{selectExpr: groupKeySQL, subExpr: "", target: &summary.ByAPIKey, hasSub: false},
	}
	for _, d := range dims {
		where, args := filterWhere(rng, filter, nil)
		query := "SELECT " + d.selectExpr
		if d.hasSub {
			query += ", " + d.subExpr
		}
		query += `, COUNT(*), COALESCE(SUM(failed),0),
			COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0),
			COALESCE(SUM(reasoning_tokens),0), COALESCE(SUM(cached_tokens),0),
			COALESCE(SUM(cache_creation_tokens),0),
			COALESCE(SUM(total_tokens),0),
			COALESCE(AVG(latency_ms),0), COALESCE(AVG(ttft_ms),0)
			FROM usage_records` + where + `
			GROUP BY ` + d.selectExpr + `
			ORDER BY COUNT(*) DESC, ` + d.selectExpr + ` ASC`
		rows, err := s.db.QueryContext(ctx, query, args...)
		if err != nil {
			return fmt.Errorf("usage sqlite group by %s: %w", d.selectExpr, err)
		}
		out := *d.target
		for rows.Next() {
			var g GroupStat
			var scanArgs []any
			scanArgs = append(scanArgs, &g.Name)
			if d.hasSub {
				scanArgs = append(scanArgs, &g.Sub)
			}
			scanArgs = append(scanArgs, &g.Calls, &g.Failed, &g.InputTokens, &g.OutputTokens,
				&g.ReasoningTokens, &g.CachedTokens, &g.CacheCreationTokens, &g.TotalTokens,
				&g.AvgLatencyMs, &g.AvgTTFTMs)
			if err := rows.Scan(scanArgs...); err != nil {
				rows.Close()
				return fmt.Errorf("usage sqlite group scan: %w", err)
			}
			out = append(out, g)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return fmt.Errorf("usage sqlite group rows: %w", err)
		}
		rows.Close()
		*d.target = out
	}
	return nil
}

func (s *SQLiteStore) fillByHour(ctx context.Context, rng QueryRange, filter PageFilter, summary *UsageSummary) error {
	where, args := filterWhere(rng, filter, nil)
	// Timestamps are stored as "YYYY-MM-DDTHH:MM:SS.nnnnnnnnnZ" (UTC), so the
	// first 13 characters are the UTC hour bucket.
	//
	// The latency percentiles cannot be computed in SQL, so the per-hour
	// accumulator is built in Go from one ordered scan. Without the token
	// columns here the trend chart had nothing to draw, which is why the
	// plugin source used to render an empty "Token usage trend".
	//
	// The same scan also feeds the range-wide latency percentiles, hung-call
	// and cache-hit counters that the Performance tab reads: those were only
	// ever computed on the router path, so under source=plugin every one of
	// them rendered as "–". One pass serves both, because a second full scan
	// just to compute percentiles would double the cost of the summary.
	query := `SELECT substr(timestamp,1,13), failed, latency_ms, ttft_ms,
		input_tokens, output_tokens, total_tokens,
		cached_tokens, cache_read_tokens
		FROM usage_records` + where + `
		ORDER BY substr(timestamp,1,13) ASC`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("usage sqlite hour query: %w", err)
	}
	defer func() { _ = rows.Close() }()

	type hourAcc struct {
		calls    int64
		failed   int64
		inTok    int64
		outTok   int64
		totalTok int64
		latS     []float64
		ttftS    []float64
	}
	accs := map[string]*hourAcc{}
	order := []string{}

	var allLatS []float64
	var allTTFTS []float64
	var hungCalls, cacheHitCalls int64

	for rows.Next() {
		var (
			hour                    string
			failed                  int64
			latMs, ttftMs           int64
			inTok, outTok, totalTok int64
			cachedTok, cacheReadTok int64
		)
		if err := rows.Scan(&hour, &failed, &latMs, &ttftMs, &inTok, &outTok, &totalTok,
			&cachedTok, &cacheReadTok); err != nil {
			return fmt.Errorf("usage sqlite hour scan: %w", err)
		}
		acc, ok := accs[hour]
		if !ok {
			acc = &hourAcc{}
			accs[hour] = acc
			order = append(order, hour)
		}
		acc.calls++
		if failed != 0 {
			acc.failed++
		}
		acc.inTok += inTok
		acc.outTok += outTok
		acc.totalTok += totalTok
		latS := float64(latMs) / 1000
		ttftS := float64(ttftMs) / 1000
		acc.latS = append(acc.latS, latS)
		if ttftS > 0 {
			acc.ttftS = append(acc.ttftS, ttftS)
		}

		allLatS = append(allLatS, latS)
		if ttftS > 0 {
			allTTFTS = append(allTTFTS, ttftS)
		}
		if latS > 300 {
			hungCalls++
		}
		if cachedTok > 0 || cacheReadTok > 0 {
			cacheHitCalls++
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("usage sqlite hour rows: %w", err)
	}
	sort.Strings(order)
	for _, hour := range order {
		acc := accs[hour]
		errRate := float64(0)
		if acc.calls > 0 {
			errRate = round2(float64(acc.failed) / float64(acc.calls) * 100)
		}
		summary.ByHour = append(summary.ByHour, HourStat{
			Hour:         hour,
			Calls:        acc.calls,
			Success:      acc.calls - acc.failed,
			Failed:       acc.failed,
			ErrorRate:    errRate,
			InputTokens:  acc.inTok,
			OutputTokens: acc.outTok,
			TotalTokens:  acc.totalTok,
			P50LatencyS:  round2(percentile(acc.latS, 0.5)),
			P90LatencyS:  round2(percentile(acc.latS, 0.9)),
			P95LatencyS:  round2(percentile(acc.latS, 0.95)),
			P99LatencyS:  round2(percentile(acc.latS, 0.99)),
		})
	}

	// Range-wide latency stats for the Performance tab. fillTotals ran first and
	// already filled the counters and token sums; these are the fields it cannot
	// express in SQL.
	t := &summary.Totals
	sort.Float64s(allLatS)
	sort.Float64s(allTTFTS)
	maxLat := float64(0)
	if len(allLatS) > 0 {
		maxLat = allLatS[len(allLatS)-1]
	}
	t.P50LatencyS = round2(percentile(allLatS, 0.5))
	t.P75LatencyS = round2(percentile(allLatS, 0.75))
	t.P90LatencyS = round2(percentile(allLatS, 0.9))
	t.P95LatencyS = round2(percentile(allLatS, 0.95))
	t.P99LatencyS = round2(percentile(allLatS, 0.99))
	t.MaxLatencyS = round2(maxLat)
	t.P50TTFTS = round2(percentile(allTTFTS, 0.5))
	t.P90TTFTS = round2(percentile(allTTFTS, 0.9))
	t.HungCalls = hungCalls
	t.CacheHitCalls = cacheHitCalls
	return nil
}

// latencyBucketLabel maps a latency in seconds to the bucket label the
// dashboard expects, mirroring the router path's thresholds exactly.
func latencyBucketLabel(latS float64) string {
	switch {
	case latS < 1:
		return "< 1s"
	case latS < 3:
		return "1 - 3s"
	case latS < 5:
		return "3 - 5s"
	case latS < 10:
		return "5 - 10s"
	case latS < 30:
		return "10 - 30s"
	case latS < 60:
		return "30 - 60s"
	case latS < 300:
		return "1 - 5m"
	case latS < 1800:
		return "5 - 30m"
	default:
		return "> 30m"
	}
}

// tokenBucketLabel maps a context size to its bucket label. The router path
// buckets on input tokens, so the plugin path does the same and the two
// sources stay comparable.
func tokenBucketLabel(inTok int64) string {
	switch {
	case inTok < 1000:
		return "< 1k"
	case inTok < 10000:
		return "1k - 10k"
	case inTok < 50000:
		return "10k - 50k"
	case inTok < 100000:
		return "50k - 100k"
	case inTok < 200000:
		return "100k - 200k"
	case inTok < 400000:
		return "200k - 400k"
	default:
		return "> 400k"
	}
}

var (
	latencyBucketOrder = []string{"< 1s", "1 - 3s", "3 - 5s", "5 - 10s", "10 - 30s", "30 - 60s", "1 - 5m", "5 - 30m", "> 30m"}
	tokenBucketOrder   = []string{"< 1k", "1k - 10k", "10k - 50k", "50k - 100k", "100k - 200k", "200k - 400k", "> 400k"}
)

// fillDistributions fills the breakdowns that only ever existed on the router
// path: per-executor rows, per-reasoning-effort rows, the latency and context
// histograms, the latency/TTFT scatter and the slowest-request list.
//
// It is deliberately a SEPARATE scan from fillByHour rather than more work
// inside it: the summary runs on every dashboard refresh, and mixing two
// concerns in one loop makes either one harder to change safely. The scan
// returns only aggregate counters plus a bounded sample (150 scatter points,
// 30 slowest rows), so the payload stays small no matter how wide the range.
func (s *SQLiteStore) fillDistributions(ctx context.Context, rng QueryRange, filter PageFilter, summary *UsageSummary) error {
	if s == nil || s.db == nil {
		return nil
	}
	where, args := filterWhere(rng, filter, nil)
	query := `SELECT id, timestamp, api_key, provider, model, alias, source,
		auth_id, auth_index, auth_type, base_url, executor_type,
		reasoning_effort, service_tier, latency_ms, ttft_ms,
		input_tokens, output_tokens, reasoning_tokens, cached_tokens,
		cache_read_tokens, cache_creation_tokens, total_tokens,
		failed, failure_status_code, failure_body
		FROM usage_records` + where + `
		ORDER BY timestamp DESC, id DESC`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("usage sqlite distribution query: %w", err)
	}
	defer func() { _ = rows.Close() }()

	efforts := map[string]*enhancedGroupAcc{}
	executors := map[string]*enhancedGroupAcc{}
	latBuckets := map[string]int64{}
	tokBuckets := map[string]int64{}
	var scatterPoints []ScatterPoint
	var slowest []RequestDetail

	for rows.Next() {
		var timestampText string
		var apiKey string
		var failedInt int
		d := RequestDetail{}
		if err := rows.Scan(
			&d.ID, &timestampText, &apiKey, &d.Provider, &d.Model, &d.Alias, &d.Source,
			&d.AuthID, &d.AuthIndex, &d.AuthType, &d.BaseURL, &d.ExecutorType,
			&d.ReasoningEffort, &d.ServiceTier, &d.LatencyMs, &d.TTFTMs,
			&d.Tokens.InputTokens, &d.Tokens.OutputTokens, &d.Tokens.ReasoningTokens,
			&d.Tokens.CachedTokens, &d.Tokens.CacheReadTokens, &d.Tokens.CacheCreationTokens,
			&d.Tokens.TotalTokens, &failedInt, &d.FailureStatusCode, &d.FailureBody,
		); err != nil {
			return fmt.Errorf("usage sqlite distribution scan: %w", err)
		}
		parsed, err := time.Parse(time.RFC3339Nano, timestampText)
		if err != nil {
			return fmt.Errorf("usage sqlite distribution parse timestamp: %w", err)
		}
		d.Timestamp = parsed.UTC()
		d.LatencyMs = nonNegative(d.LatencyMs)
		d.TTFTMs = nonNegative(d.TTFTMs)
		d.Failed = failedInt != 0
		d.ProviderLabel = providerLabel(d)

		latS := float64(d.LatencyMs) / 1000
		ttftS := float64(d.TTFTMs) / 1000
		cached := d.Tokens.CachedTokens
		if cached == 0 && d.Tokens.CacheReadTokens > 0 {
			cached = d.Tokens.CacheReadTokens
		}
		modelName := firstNonEmpty(d.Model, "unknown")

		// Per reasoning effort.
		effortName := firstNonEmpty(d.ReasoningEffort, "Not Specified")
		enhancedAccFor(efforts, effortName).add(d.Failed, d.LatencyMs, d.TTFTMs, latS, ttftS,
			d.Tokens.InputTokens, d.Tokens.OutputTokens, d.Tokens.ReasoningTokens,
			cached, d.Tokens.TotalTokens, d.FailureStatusCode)

		// Per executor.
		executorName := firstNonEmpty(d.ExecutorType, "unknown")
		enhancedAccFor(executors, executorName).add(d.Failed, d.LatencyMs, d.TTFTMs, latS, ttftS,
			d.Tokens.InputTokens, d.Tokens.OutputTokens, d.Tokens.ReasoningTokens,
			cached, d.Tokens.TotalTokens, d.FailureStatusCode)

		latBuckets[latencyBucketLabel(latS)]++
		tokBuckets[tokenBucketLabel(d.Tokens.InputTokens)]++

		if len(scatterPoints) < 150 && (latS > 0 || ttftS > 0) {
			scatterPoints = append(scatterPoints, ScatterPoint{
				LatencyS: round2(latS),
				TTFTS:    round2(ttftS),
				Model:    modelName,
				Provider: d.Provider,
				Failed:   d.Failed,
			})
		}

		// Keep the 30 slowest, inserting in order so the list never grows
		// unbounded for a wide range.
		if len(slowest) < 30 || d.LatencyMs > slowest[len(slowest)-1].LatencyMs {
			slowest = append(slowest, d)
			sort.Slice(slowest, func(i, j int) bool {
				return slowest[i].LatencyMs > slowest[j].LatencyMs
			})
			if len(slowest) > 30 {
				slowest = slowest[:30]
			}
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("usage sqlite distribution rows: %w", err)
	}

	summary.ByEffort = enhancedToStats(efforts, nil)
	summary.ByExecutor = enhancedToStats(executors, nil)
	for _, b := range latencyBucketOrder {
		summary.LatencyDistribution = append(summary.LatencyDistribution, DistributionBucket{Label: b, Calls: latBuckets[b]})
	}
	for _, b := range tokenBucketOrder {
		summary.TokenDistribution = append(summary.TokenDistribution, DistributionBucket{Label: b, Calls: tokBuckets[b]})
		summary.ContextHistogram = append(summary.ContextHistogram, DistributionBucket{Label: b, Calls: tokBuckets[b]})
	}
	summary.ScatterPoints = scatterPoints
	summary.SlowestRequests = slowest
	return nil
}

// effectivePriceBook returns the router price book merged with the manual
// overrides stored in usage.db.
//
// Pricing normally lives on the router store, but the dashboard now reads its
// own usage.db by default, so that path needs the same book. Any failure
// degrades to a book carrying only the overrides: an estimate layer must never
// take the usage dashboard down.
func (s *SQLiteStore) effectivePriceBook(ctx context.Context) *priceBook {
	book := emptyPriceBook()
	if rs, err := newRouterStore(); err == nil {
		defer rs.close()
		book = rs.loadEffectivePriceBook(ctx)
	}
	if s != nil && s.db != nil {
		if overrides, err := s.LoadPriceOverrides(ctx); err == nil {
			book.overrides = overrides
		}
	}
	return book
}

// fillCosts prices every record in range against the effective price book and
// writes the result into the summary totals, the per-model rows and the pricing
// metadata.
//
// A model without a price is counted as unpriced and never as free: the sum in
// CostUSD only covers priced calls, and CostPartial/priced-call counts let the
// dashboard say so out loud instead of showing a confident $0.
func (s *SQLiteStore) fillCosts(ctx context.Context, rng QueryRange, filter PageFilter, summary *UsageSummary) error {
	if s == nil || s.db == nil {
		return nil
	}
	book := s.effectivePriceBook(ctx)

	where, args := filterWhere(rng, filter, nil)
	query := `SELECT model, alias, provider, executor_type,
		input_tokens, output_tokens, reasoning_tokens,
		cached_tokens, cache_read_tokens, cache_creation_tokens
		FROM usage_records` + where
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("usage sqlite cost query: %w", err)
	}
	defer func() { _ = rows.Close() }()

	byModel := map[string]*GroupStat{}
	for i := range summary.ByModel {
		byModel[strings.ToLower(strings.TrimSpace(summary.ByModel[i].Name))] = &summary.ByModel[i]
	}

	unpricedByModel := map[string]int64{}
	unpricedOrder := []string{}
	var totalCost float64
	var pricedCalls, unpricedCalls int64

	for rows.Next() {
		var (
			model, alias, provider, executor          string
			inTok, outTok, reasoningTok               int64
			cachedTok, cacheReadTok, cacheCreationTok int64
		)
		if err := rows.Scan(&model, &alias, &provider, &executor, &inTok, &outTok, &reasoningTok,
			&cachedTok, &cacheReadTok, &cacheCreationTok); err != nil {
			return fmt.Errorf("usage sqlite cost scan: %w", err)
		}
		rec := routerRecord{
			ProviderModel:       model,
			ProviderAlias:       alias,
			Provider:            provider,
			ExecutorType:        executor,
			InputTokens:         inTok,
			OutputTokens:        outTok,
			ReasoningTokens:     reasoningTok,
			CachedTokens:        cachedTok,
			CacheReadTokens:     cacheReadTok,
			CacheCreationTokens: cacheCreationTok,
		}
		cost, _, priced := estimateRecordCost(rec, book)
		stat := byModel[strings.ToLower(strings.TrimSpace(model))]
		if priced {
			pricedCalls++
			totalCost += cost
			if stat != nil {
				// Accumulate raw and round once at the end: rounding on every
				// record compounds error across thousands of calls.
				stat.CostUSD += cost
				stat.PricedCalls++
				if stat.PriceSource == "" {
					if price, ok := resolveRecordPrice(rec, book); ok {
						stat.PriceSource = firstNonEmpty(price.Source, "router")
					}
				}
			}
			continue
		}
		unpricedCalls++
		if stat != nil {
			stat.UnpricedCalls++
		}
		if _, seen := unpricedByModel[model]; !seen {
			unpricedOrder = append(unpricedOrder, model)
		}
		unpricedByModel[model]++
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("usage sqlite cost rows: %w", err)
	}

	// Round once, after every record has been added, so per-model rows and the
	// grand total agree.
	for i := range summary.ByModel {
		summary.ByModel[i].CostUSD = round4(summary.ByModel[i].CostUSD)
	}

	summary.Totals.CostUSD = round4(totalCost)
	summary.Totals.CostPricedCalls = pricedCalls
	summary.Totals.CostUnpricedCalls = unpricedCalls
	summary.Totals.CostPartial = unpricedCalls > 0

	summary.Pricing = PricingMeta{
		Available: book.Count > 0 || len(book.overrides) > 0,
		Source:    book.SyncedBy,
		Revision:  book.Revision,
		Entries:   book.Count,
		SyncedAt:  book.SyncedAt,
		Note:      "Chi phi la uoc tinh theo bang gia cua model-router; model thieu gia khong duoc tinh la mien phi.",
	}
	for _, name := range unpricedOrder {
		summary.Pricing.Unpriced = append(summary.Pricing.Unpriced, UnpricedModel{Model: name, Calls: unpricedByModel[name]})
	}
	sort.Slice(summary.Pricing.Unpriced, func(i, j int) bool {
		if summary.Pricing.Unpriced[i].Calls == summary.Pricing.Unpriced[j].Calls {
			return summary.Pricing.Unpriced[i].Model < summary.Pricing.Unpriced[j].Model
		}
		return summary.Pricing.Unpriced[i].Calls > summary.Pricing.Unpriced[j].Calls
	})
	return nil
}

// fillModelProviders attaches the serving provider to each by-model row.
//
// The "Bảng Hiệu năng Model" table shows a Provider column, but a model row is
// aggregated over every provider that served it, so there is no single provider
// on the row itself. Rather than leave the cell blank ("–", which tells the
// operator nothing), the dominant provider is resolved and shown; when a model
// was served by more than one, the extra count is reported so the row never
// pretends the model has exactly one upstream.
func (s *SQLiteStore) fillModelProviders(ctx context.Context, rng QueryRange, filter PageFilter, summary *UsageSummary) error {
	if s == nil || s.db == nil || len(summary.ByModel) == 0 {
		return nil
	}
	where, args := filterWhere(rng, filter, nil)
	query := `SELECT model, provider, auth_id, COUNT(*) AS calls
		FROM usage_records` + where + `
		GROUP BY model, provider, auth_id
		ORDER BY model ASC, calls DESC, provider ASC`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("usage sqlite model provider query: %w", err)
	}
	defer func() { _ = rows.Close() }()

	type providerCount struct {
		label string
		calls int64
	}
	byModel := map[string][]providerCount{}
	seen := map[string]bool{}
	for rows.Next() {
		var model, provider, authID string
		var calls int64
		if err := rows.Scan(&model, &provider, &authID, &calls); err != nil {
			return fmt.Errorf("usage sqlite model provider scan: %w", err)
		}
		key := strings.ToLower(strings.TrimSpace(model))
		// An OAuth account is the more specific identity, exactly as in the
		// realtime table: "antigravity" alone would not match what the
		// operator sees there.
		label := accountFromAuthID(authID, provider)
		if label == "" {
			label = providerDisplayName(provider)
		}
		if label == "" {
			label = provider
		}
		// Several auth rows collapse onto one label (e.g. the same account
		// under two executors); count them once.
		dedupe := key + "\x00" + label
		if seen[dedupe] {
			continue
		}
		seen[dedupe] = true
		byModel[key] = append(byModel[key], providerCount{label: label, calls: calls})
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("usage sqlite model provider rows: %w", err)
	}

	for i := range summary.ByModel {
		key := strings.ToLower(strings.TrimSpace(summary.ByModel[i].Name))
		counts := byModel[key]
		if len(counts) == 0 {
			continue
		}
		// Rows arrive ordered by call count desc, so the first entry is the
		// identity that served most of this model's traffic.
		dominant := counts[0]
		label := dominant.label
		if len(counts) > 1 {
			// Be explicit that this is one of several upstreams, not the only
			// one: the counts differ and hiding that would mislead.
			label += " (+" + strconv.Itoa(len(counts)-1) + ")"
		}
		summary.ByModel[i].Provider = label
	}
	return nil
}

func (s *SQLiteStore) fillStatusCodes(ctx context.Context, rng QueryRange, filter PageFilter, summary *UsageSummary) error {
	where, args := filterWhere(rng, filter, nil)
	if where != "" {
		// filterWhere already yields " WHERE <cond>"; append the extra
		// conditions to the existing AND list.
		where += " AND failed = 1 AND failure_status_code > 0"
	} else {
		where = " WHERE failed = 1 AND failure_status_code > 0"
	}
	query := `SELECT failure_status_code, COUNT(*)
		FROM usage_records` + where + `
		GROUP BY failure_status_code
		ORDER BY COUNT(*) DESC`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("usage sqlite status query: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var sc StatusCodeStat
		if err := rows.Scan(&sc.StatusCode, &sc.Calls); err != nil {
			return fmt.Errorf("usage sqlite status scan: %w", err)
		}
		summary.StatusCodes = append(summary.StatusCodes, sc)
	}
	return rows.Err()
}

// StoreLatest returns the N most recent records straight off the timestamp
// index (DESC), without scanning a time window. This backs the dashboard's
// realtime panel, which only ever needs a fixed small tail.
func (s *SQLiteStore) StoreLatest(ctx context.Context, n int) ([]RequestDetail, error) {
	if s == nil || s.db == nil {
		return []RequestDetail{}, nil
	}
	if n <= 0 {
		n = 15
	}
	if n > 200 {
		n = 200
	}
	query := `SELECT id, timestamp, api_key, provider, model, alias, source, auth_id, auth_index, auth_type, base_url, executor_type,
		reasoning_effort, service_tier, latency_ms, ttft_ms,
		input_tokens, output_tokens, reasoning_tokens, cached_tokens, cache_read_tokens, cache_creation_tokens, total_tokens,
		failed, failure_status_code, failure_body
		FROM usage_records
		ORDER BY timestamp DESC, id DESC
		LIMIT ?`
	rows, err := s.db.QueryContext(ctx, query, n)
	if err != nil {
		return nil, fmt.Errorf("usage sqlite latest query: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]RequestDetail, 0, n)
	for rows.Next() {
		var timestampText string
		var apiKey string
		var failedInt int
		detail := RequestDetail{}
		if err := rows.Scan(
			&detail.ID,
			&timestampText,
			&apiKey,
			&detail.Provider,
			&detail.Model,
			&detail.Alias,
			&detail.Source,
			&detail.AuthID,
			&detail.AuthIndex,
			&detail.AuthType,
			&detail.BaseURL,
			&detail.ExecutorType,
			&detail.ReasoningEffort,
			&detail.ServiceTier,
			&detail.LatencyMs,
			&detail.TTFTMs,
			&detail.Tokens.InputTokens,
			&detail.Tokens.OutputTokens,
			&detail.Tokens.ReasoningTokens,
			&detail.Tokens.CachedTokens,
			&detail.Tokens.CacheReadTokens,
			&detail.Tokens.CacheCreationTokens,
			&detail.Tokens.TotalTokens,
			&failedInt,
			&detail.FailureStatusCode,
			&detail.FailureBody,
		); err != nil {
			return nil, fmt.Errorf("usage sqlite latest scan: %w", err)
		}
		parsed, err := time.Parse(time.RFC3339Nano, timestampText)
		if err != nil {
			return nil, fmt.Errorf("usage sqlite latest parse timestamp: %w", err)
		}
		detail.Timestamp = parsed.UTC()
		detail.LatencyMs = nonNegative(detail.LatencyMs)
		detail.TTFTMs = nonNegative(detail.TTFTMs)
		detail.Failed = failedInt != 0
		detail.ProviderLabel = providerLabel(detail)
		out = append(out, detail)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("usage sqlite latest rows: %w", err)
	}
	return out, nil
}

// ListPage returns a paginated, filtered view of request-level records.
func (s *SQLiteStore) ListPage(ctx context.Context, rng QueryRange, filter PageFilter) (*UsagePage, error) {
	page := &UsagePage{Total: 0, Limit: filter.Limit, Offset: filter.Offset, Rows: []RequestDetail{}}
	if s == nil || s.db == nil {
		return page, nil
	}
	where := make([]string, 0, 6)
	args := make([]any, 0, 8)
	if rng.Start != nil && !rng.Start.IsZero() {
		where = append(where, "timestamp >= ?")
		args = append(args, formatTimestamp(*rng.Start))
	}
	if rng.End != nil && !rng.End.IsZero() {
		where = append(where, "timestamp < ?")
		args = append(args, formatTimestamp(*rng.End))
	}
	if filter.Model != "" {
		where = append(where, "(model = ? OR alias = ?)")
		args = append(args, filter.Model, filter.Model)
	}
	if filter.Provider != "" {
		where = append(where, "provider = ?")
		args = append(args, filter.Provider)
	}
	if filter.APIKey != "" {
		where = append(where, "api_key = ?")
		args = append(args, filter.APIKey)
	}
	if filter.Failed != nil {
		where = append(where, "failed = ?")
		args = append(args, boolToInt(*filter.Failed))
	}
	whereSQL := ""
	if len(where) > 0 {
		whereSQL = " WHERE " + strings.Join(where, " AND ")
	}

	countRow := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM usage_records"+whereSQL, args...)
	if err := countRow.Scan(&page.Total); err != nil {
		return nil, fmt.Errorf("usage sqlite count: %w", err)
	}

	limit := filter.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	page.Limit = limit
	queryArgs := append(append([]any{}, args...), limit, filter.Offset)
	query := `SELECT id, timestamp, api_key, provider, model, alias, source, auth_id, auth_index, auth_type, base_url, executor_type,
		reasoning_effort, service_tier, latency_ms, ttft_ms,
		input_tokens, output_tokens, reasoning_tokens, cached_tokens, cache_read_tokens, cache_creation_tokens, total_tokens,
		failed, failure_status_code, failure_body
		FROM usage_records` + whereSQL + `
		ORDER BY timestamp DESC, id DESC
		LIMIT ? OFFSET ?`
	rows, err := s.db.QueryContext(ctx, query, queryArgs...)
	if err != nil {
		return nil, fmt.Errorf("usage sqlite page query: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var timestampText string
		var apiKey string
		var failedInt int
		detail := RequestDetail{}
		if err := rows.Scan(
			&detail.ID,
			&timestampText,
			&apiKey,
			&detail.Provider,
			&detail.Model,
			&detail.Alias,
			&detail.Source,
			&detail.AuthID,
			&detail.AuthIndex,
			&detail.AuthType,
			&detail.BaseURL,
			&detail.ExecutorType,
			&detail.ReasoningEffort,
			&detail.ServiceTier,
			&detail.LatencyMs,
			&detail.TTFTMs,
			&detail.Tokens.InputTokens,
			&detail.Tokens.OutputTokens,
			&detail.Tokens.ReasoningTokens,
			&detail.Tokens.CachedTokens,
			&detail.Tokens.CacheReadTokens,
			&detail.Tokens.CacheCreationTokens,
			&detail.Tokens.TotalTokens,
			&failedInt,
			&detail.FailureStatusCode,
			&detail.FailureBody,
		); err != nil {
			return nil, fmt.Errorf("usage sqlite page scan: %w", err)
		}
		parsed, err := time.Parse(time.RFC3339Nano, timestampText)
		if err != nil {
			return nil, fmt.Errorf("usage sqlite parse timestamp: %w", err)
		}
		detail.Timestamp = parsed.UTC()
		detail.Failed = failedInt != 0
		detail.ProviderLabel = providerLabel(detail)
		page.Rows = append(page.Rows, detail)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("usage sqlite page rows: %w", err)
	}
	return page, nil
}
