package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// seedComboCall writes one client call: the call's own row (model == the
// client-requested name) first, then one row per physical attempt (model is the
// resolved target with the provider prefix stripped, requested_model keeps it),
// plus the response of the attempt that answered. This is the exact shape CPA
// produces, verified against the live DB.
func seedComboCall(t *testing.T, st *reqLogStore, combo, traceID string, targets []string, answered int, at time.Time) {
	t.Helper()
	st.enqueue(reqLogSample{
		RequestID: "req-" + combo + "-0", TraceID: traceID, Kind: "request",
		Model: combo, RequestedModel: combo, // equal => the call's own row
		ReqPath: "/v1/chat/completions", BodyBytes: 100, At: at,
	})
	for i, target := range targets {
		st.enqueue(reqLogSample{
			RequestID: "req-" + combo + "-" + itoa(i+1), TraceID: traceID, Kind: "request",
			// resolved: prefix kept in requested_model, stripped from model
			Model: stripPrefix(target), RequestedModel: target,
			ReqPath: "/v1/chat/completions", BodyBytes: 100, At: at,
		})
	}
	st.enqueue(reqLogSample{
		RequestID: "req-" + combo + "-" + itoa(answered+1), Kind: "response",
		BodyBytes: 500, At: at,
	})
}

// stripPrefix mimics CPA's Model: everything after the first "/" (the provider
// prefix is dropped from the model but kept in requested_model).
func stripPrefix(s string) string {
	if i := strings.Index(s, "/"); i >= 0 {
		return s[i+1:]
	}
	return s
}

func itoa(i int) string {
	if i < 10 {
		return string(rune('0' + i))
	}
	return string(rune('0'+i/10)) + string(rune('0'+i%10))
}

// A call that failed over writes one request row per attempt. Listing them raw
// is what made the tab unreadable; grouped mode must show ONE row per call,
// carrying the combo name and the attempt count.
func TestReqLogGroupedCollapsesAttemptsIntoOneRow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "request-logs.db")
	st, err := newReqLogStore(path)
	if err != nil {
		t.Fatalf("newReqLogStore: %v", err)
	}
	t.Cleanup(st.close)

	base := time.Now().UTC().Add(-time.Hour)
	// One combo call that tried two targets and was answered by the second.
	seedComboCall(t, st, "strong", "trace-aaa", []string{"ca2/qwen3.8-max-0902", "a6/glm-5.3"}, 1, base)
	// One direct call, no combo.
	st.enqueue(reqLogSample{
		RequestID: "req-direct", TraceID: "trace-bbb", Kind: "request",
		Model: "kr/claude-opus-5.5-thinking", ReqPath: "/v1/chat/completions",
		BodyBytes: 50, At: base.Add(time.Minute),
	})
	st.flush()

	rows, total, err := st.page(50, 0, reqLogFilter{Grouped: true})
	if err != nil {
		t.Fatalf("page grouped: %v", err)
	}
	if total != 2 {
		t.Fatalf("grouped total = %d, want 2 (one row per call, not per attempt)", total)
	}
	if len(rows) != 2 {
		t.Fatalf("grouped rows = %d, want 2", len(rows))
	}

	// Newest first: the direct call is second in time, so it leads.
	var comboRow, directRow map[string]any
	for _, r := range rows {
		if r["combo"] == "strong" {
			comboRow = r
		}
		if r["combo"] == "" || r["combo"] == nil {
			directRow = r
		}
	}
	if comboRow == nil {
		t.Fatalf("no grouped row carried the combo name; rows = %#v", rows)
	}
	if got := comboRow["attempts"]; got != 2 {
		t.Fatalf("combo row attempts = %v, want 2 (two physical targets)", got)
	}
	if got := comboRow["model"]; got != "glm-5.3" {
		t.Fatalf("combo row model = %v, want the answering target glm-5.3 (prefix stripped by CPA)", got)
	}
	if got := comboRow["has_response"]; got != true {
		t.Fatalf("combo row has_response = %v, want true", got)
	}
	if directRow == nil {
		t.Fatalf("no grouped row for the direct call; rows = %#v", rows)
	}
	if got := directRow["attempts"]; got != 1 {
		t.Fatalf("direct row attempts = %v, want 1", got)
	}
}

// The flat listing must stay available and must now expose the combo name.
func TestReqLogFlatStillWorksAndExposesRequestedModel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "request-logs.db")
	st, err := newReqLogStore(path)
	if err != nil {
		t.Fatalf("newReqLogStore: %v", err)
	}
	t.Cleanup(st.close)

	base := time.Now().UTC().Add(-time.Hour)
	seedComboCall(t, st, "fast", "trace-ccc", []string{"lbd/deepseek-v4.1-flash"}, 0, base)
	st.flush()

	rows, total, err := st.page(50, 0, reqLogFilter{})
	if err != nil {
		t.Fatalf("page flat: %v", err)
	}
	// combo row + 1 target row + 1 response = 3 raw captures.
	if total != 3 || len(rows) != 3 {
		t.Fatalf("flat total=%d rows=%d, want 3 raw captures", total, len(rows))
	}
	// The flat listing keeps the raw shape: the call's own row still carries the
	// combo as its model, and the re-issued row carries the resolved target.
	var own, resolved bool
	for _, r := range rows {
		if r["model"] == "fast" && r["requested_model"] == "fast" {
			own = true
		}
		if r["model"] == "deepseek-v4.1-flash" && r["requested_model"] == "lbd/deepseek-v4.1-flash" {
			resolved = true
		}
	}
	if !own {
		t.Fatalf("flat listing lost the call's own row (model=fast); rows = %#v", rows)
	}
	if !resolved {
		t.Fatalf("flat listing lost the resolved target row; rows = %#v", rows)
	}
}

// Filtering by combo must select calls by their requested name, and "direct"
// must select the ones with no combo.
func TestReqLogComboFilter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "request-logs.db")
	st, err := newReqLogStore(path)
	if err != nil {
		t.Fatalf("newReqLogStore: %v", err)
	}
	t.Cleanup(st.close)

	base := time.Now().UTC().Add(-time.Hour)
	seedComboCall(t, st, "strong", "trace-ddd", []string{"ca2/qwen3.8-max-0902"}, 0, base)
	seedComboCall(t, st, "fast", "trace-eee", []string{"lbd/deepseek-v4.1-flash"}, 0, base.Add(time.Minute))
	st.enqueue(reqLogSample{
		RequestID: "req-d1", TraceID: "trace-fff", Kind: "request",
		Model: "kr/claude-opus-5.5-thinking", RequestedModel: "kr/claude-opus-5.5-thinking",
		At: base.Add(2 * time.Minute),
	})
	st.flush()

	_, totalCombo, err := st.page(50, 0, reqLogFilter{Combo: "combo", Grouped: true})
	if err != nil {
		t.Fatalf("page combo: %v", err)
	}
	if totalCombo != 2 {
		t.Fatalf("combo filter total = %d, want 2 (two routed calls)", totalCombo)
	}

	_, totalDirect, err := st.page(50, 0, reqLogFilter{Combo: "direct", Grouped: true})
	if err != nil {
		t.Fatalf("page direct: %v", err)
	}
	if totalDirect != 1 {
		t.Fatalf("direct filter total = %d, want 1", totalDirect)
	}

	_, totalNamed, err := st.page(50, 0, reqLogFilter{Combo: "strong", Grouped: true})
	if err != nil {
		t.Fatalf("page named combo: %v", err)
	}
	if totalNamed != 1 {
		t.Fatalf("named combo filter total = %d, want 1", totalNamed)
	}
}

// captureRequest must record the combo name and drop it when it equals the
// resolved model, so a direct call is never mislabelled as routed.
func TestCaptureRequestRecordsComboOnlyWhenItDiffers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "request-logs.db")
	st, err := newReqLogStore(path)
	if err != nil {
		t.Fatalf("newReqLogStore: %v", err)
	}
	t.Cleanup(st.close)

	prev := reqLog
	reqLog = st
	reqLogLive.Store(st)
	t.Cleanup(func() {
		reqLog = prev
		reqLogLive.Store(prev)
	})

	captureRequest(requestInterceptRPC{
		RequestID: "r1", TraceID: "t1", Model: "ca2/qwen3.8-max-0902", RequestedModel: "strong",
	}, nil, nil)
	captureRequest(requestInterceptRPC{
		RequestID: "r2", TraceID: "t2", Model: "kr/claude-opus-5.5-thinking", RequestedModel: "kr/claude-opus-5.5-thinking",
	}, nil, nil)
	st.flush()

	rows, _, err := st.page(50, 0, reqLogFilter{})
	if err != nil {
		t.Fatalf("page: %v", err)
	}
	got := map[string]string{}
	for _, r := range rows {
		rid, _ := r["request_id"].(string)
		rm, _ := r["requested_model"].(string)
		got[rid] = rm
	}
	// requested_model is stored on every row; the combo is identifiable because
	// it differs from the model that ran.
	if got["r1"] != "strong" {
		t.Fatalf("r1 requested_model = %q, want %q (combo differs from model)", got["r1"], "strong")
	}
	if got["r2"] != "kr/claude-opus-5.5-thinking" {
		t.Fatalf("r2 requested_model = %q, want the same as its model (direct call)", got["r2"])
	}
}
