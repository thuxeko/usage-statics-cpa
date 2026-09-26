package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestReqLogRoundTrip exercises the whole capture path: enqueue -> flush ->
// query, on a throwaway database, then verifies the row survives a reopen.
func TestReqLogRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "request-logs.db")

	st, err := newReqLogStore(path)
	if err != nil {
		t.Fatalf("newReqLogStore: %v", err)
	}
	defer st.close()

	st.enqueue(reqLogSample{
		RequestID: "rid-1", TraceID: "tid-1", Kind: "request",
		Model: "gpt-test", ReqPath: "/v1/chat/completions",
		SourceFmt: "openai", ToFormat: "openai", Stream: true,
		Headers:   `{"Content-Type":"application/json","Authorization":"[REDACTED]"}`,
		Body:      `{"messages":[{"role":"user","content":"hello"}]}`,
		BodyBytes: 46, Truncated: false, At: time.Now().UTC(),
	})
	st.enqueue(reqLogSample{
		RequestID: "rid-1", Kind: "response",
		Body: `{"choices":[{"delta":{"content":"hi"}}]}`,
		BodyBytes: 39, At: time.Now().UTC(),
	})
	// Force the commit; the batch threshold alone would not have fired.
	st.flush()

	rows, err := reqLogRecentForTest(st, 10)
	if err != nil {
		t.Fatalf("reqLogRecentForTest: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("want 2 rows, got %d", len(rows))
	}
	// Newest first: the response row must lead.
	if got := rows[0]["kind"]; got != "response" {
		t.Fatalf("want newest row kind=response, got %v", got)
	}
	if got := rows[1]["model"]; got != "gpt-test" {
		t.Fatalf("want model gpt-test on request row, got %v", got)
	}

	// Reopen: the data must still be there (schema is CREATE IF NOT EXISTS).
	st.close()
	st2, err := newReqLogStore(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer st2.close()
	rows2, err := reqLogRecentForTest(st2, 10)
	if err != nil {
		t.Fatalf("reqLogRecentForTest after reopen: %v", err)
	}
	if len(rows2) != 2 {
		t.Fatalf("want 2 rows after reopen, got %d", len(rows2))
	}
}

// reqLogRecentForTest is reqLogRecent against an explicit store, so the test
// does not depend on the package-level singleton.
func reqLogRecentForTest(s *reqLogStore, limit int) ([]map[string]any, error) {
	rows, err := s.db.Query(`SELECT id, request_id, kind, timestamp, model,
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

func TestMaskSecrets(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"api key", `{"api_key":"sk-abcdef1234567890abcd"}`, "[REDACTED]"},
		{"bearer", "Authorization: Bearer abcdef1234567890", "[REDACTED]"},
		{"clean stays", `{"model":"gpt-4","stream":true}`, ""},
	}
	for _, tc := range cases {
		got := maskSecrets(tc.in)
		if tc.want == "" {
			if strings.Contains(got, secretPlaceholder) {
				t.Errorf("%s: clean input was masked: %s", tc.name, got)
			}
			continue
		}
		if !strings.Contains(got, secretPlaceholder) {
			t.Errorf("%s: secret not masked, got %s", tc.name, got)
		}
		if strings.Contains(got, "sk-abcdef") || strings.Contains(got, "abcdef1234567890") {
			t.Errorf("%s: secret leaked: %s", tc.name, got)
		}
	}
}

func TestSafeHeadersJSON(t *testing.T) {
	h := map[string]any{
		"Authorization": "Bearer sk-abcdef1234567890",
		"Content-Type":  "application/json",
		"X-Api-Key":     "secretvalue123",
	}
	out := safeHeadersJSON(h)
	if strings.Contains(out, "sk-abcdef") || strings.Contains(out, "secretvalue123") {
		t.Fatalf("credential leaked into headers JSON: %s", out)
	}
	if strings.Contains(out, "application/json") == false {
		t.Fatalf("non-secret header was dropped: %s", out)
	}
}

func TestTruncateBody(t *testing.T) {
	body := strings.Repeat("x", 1000)
	got, n, trunc := truncateBody(body, 100)
	if len(got) != 100 || n != 1000 || !trunc {
		t.Fatalf("got len=%d n=%d trunc=%v", len(got), n, trunc)
	}
	got, n, trunc = truncateBody(body, 0)
	if len(got) != 1000 || n != 1000 || trunc {
		t.Fatalf("max=0 must not truncate: len=%d trunc=%v", len(got), trunc)
	}
}

// TestReqLogConfigParsing proves the YAML knobs reach the config struct and
// that the default is capture ON with sane resource caps.
func TestReqLogConfigParsing(t *testing.T) {
	cfg := parseConfig([]byte("data_dir: /tmp/x\n"))
	if !cfg.ReqLogEnabled {
		t.Fatal("request log should default to enabled")
	}
	if cfg.ReqLogMaxBytes != reqLogDefaultMaxBodyBytes {
		t.Fatalf("default max bytes = %d", cfg.ReqLogMaxBytes)
	}
	if cfg.ReqLogRetentionD != reqLogDefaultRetentionDays {
		t.Fatalf("default retention = %d", cfg.ReqLogRetentionD)
	}

	cfg = parseConfig([]byte("request_log_enabled: false\nrequest_log_max_bytes: 512\nrequest_log_retention_days: 3\n"))
	if cfg.ReqLogEnabled {
		t.Fatal("expected disabled")
	}
	if cfg.ReqLogMaxBytes != 512 || cfg.ReqLogRetentionD != 3 {
		t.Fatalf("got max=%d days=%d", cfg.ReqLogMaxBytes, cfg.ReqLogRetentionD)
	}
}
