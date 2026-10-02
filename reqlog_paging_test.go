package main

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

// seedReqLog writes n request rows into a throwaway request-logs.db.
func seedReqLog(t *testing.T, n int) *reqLogStore {
	t.Helper()
	path := filepath.Join(t.TempDir(), "request-logs.db")
	st, err := newReqLogStore(path)
	if err != nil {
		t.Fatalf("newReqLogStore: %v", err)
	}
	t.Cleanup(st.close)
	base := time.Now().UTC().Add(-time.Duration(n) * time.Minute)
	for i := 0; i < n; i++ {
		st.enqueue(reqLogSample{
			RequestID: fmt.Sprintf("rid-%03d", i),
			Kind:      "request",
			Model:     "model-a",
			ReqPath:   "/v1/chat/completions",
			Body:      fmt.Sprintf(`{"i":%d}`, i),
			BodyBytes: 10,
			At:        base.Add(time.Duration(i) * time.Minute),
		})
	}
	st.flush()
	return st
}

// TestReqLogPagination covers the regression the Log Request tab had: no
// offset support meant page 2 could never be requested, and `total` reported
// the page size instead of the size of the log.
func TestReqLogPagination(t *testing.T) {
	st := seedReqLog(t, 120)

	// Page 1.
	rows, total, err := st.page(50, 0, reqLogFilter{})
	if err != nil {
		t.Fatalf("page 1: %v", err)
	}
	if len(rows) != 50 {
		t.Fatalf("page 1 rows = %d, want 50", len(rows))
	}
	if total != 120 {
		t.Fatalf("total = %d, want 120 (the size of the log, not of the page)", total)
	}
	firstID := rows[0]["id"]

	// Page 2 must return DIFFERENT rows, not the same first page again.
	rows2, total2, err := st.page(50, 50, reqLogFilter{})
	if err != nil {
		t.Fatalf("page 2: %v", err)
	}
	if len(rows2) != 50 {
		t.Fatalf("page 2 rows = %d, want 50", len(rows2))
	}
	if total2 != 120 {
		t.Fatalf("page 2 total = %d, want 120", total2)
	}
	if rows2[0]["id"] == firstID {
		t.Fatalf("page 2 starts at the same row as page 1 (id %v): offset is ignored", firstID)
	}

	// Page 3 is the short tail.
	rows3, _, err := st.page(50, 100, reqLogFilter{})
	if err != nil {
		t.Fatalf("page 3: %v", err)
	}
	if len(rows3) != 20 {
		t.Fatalf("page 3 rows = %d, want 20", len(rows3))
	}

	// Past the end: fall back to page 1 rather than an empty table.
	rows4, _, err := st.page(50, 5000, reqLogFilter{})
	if err != nil {
		t.Fatalf("past the end: %v", err)
	}
	if len(rows4) != 50 {
		t.Fatalf("past-the-end rows = %d, want 50 (fallback to the first page)", len(rows4))
	}
}

// TestReqLogFilterTotals is the other half of the bug: the kind/model filter
// ran in Go after the LIMIT, so `total` described the filtered page instead of
// the filtered log, and a narrow filter could return an empty page.
func TestReqLogFilterTotals(t *testing.T) {
	st := seedReqLog(t, 60)
	st.enqueue(reqLogSample{
		RequestID: "resp-1", Kind: "response", Model: "model-b",
		Body: "{}", BodyBytes: 2, At: time.Now().UTC(),
	})
	st.flush()

	rows, total, err := st.page(10, 0, reqLogFilter{Kind: "response"})
	if err != nil {
		t.Fatalf("filtered page: %v", err)
	}
	if total != 1 {
		t.Fatalf("filtered total = %d, want 1 (only the response row)", total)
	}
	if len(rows) != 1 {
		t.Fatalf("filtered rows = %d, want 1", len(rows))
	}
	if got := rows[0]["kind"]; got != "response" {
		t.Fatalf("filtered row kind = %v, want response", got)
	}

	// A model filter must match case-insensitively and report its own total.
	_, totalModel, err := st.page(10, 0, reqLogFilter{Model: "MODEL-A"})
	if err != nil {
		t.Fatalf("model filter: %v", err)
	}
	if totalModel != 60 {
		t.Fatalf("model-filtered total = %d, want 60", totalModel)
	}

	// No match: total 0, no rows, no error.
	rowsNone, totalNone, err := st.page(10, 0, reqLogFilter{Model: "does-not-exist"})
	if err != nil {
		t.Fatalf("no-match filter: %v", err)
	}
	if totalNone != 0 || len(rowsNone) != 0 {
		t.Fatalf("no-match filter returned total=%d rows=%d, want 0/0", totalNone, len(rowsNone))
	}
}

// TestReqLogFilterWhereSQL guards the predicate builder itself: every filter
// value must be bound, never interpolated.
func TestReqLogFilterWhereSQL(t *testing.T) {
	clause, args := reqLogFilter{Kind: "request", Model: "glm"}.where()
	if clause == "" {
		t.Fatal("expected a WHERE clause")
	}
	if len(args) != 2 {
		t.Fatalf("args = %v, want 2 bound values", args)
	}
	if got := args[0]; got != "request" {
		t.Fatalf("args[0] = %v, want request", got)
	}
	if got := args[1]; got != "%glm%" {
		t.Fatalf("args[1] = %v, want %%glm%%", got)
	}
	empty, emptyArgs := reqLogFilter{}.where()
	if empty != "" || len(emptyArgs) != 0 {
		t.Fatalf("zero filter = (%q, %v), want empty", empty, emptyArgs)
	}
}
