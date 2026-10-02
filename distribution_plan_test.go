package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestDistributionQueryHasNoTempSort guards the plan, not the output.
//
// The distribution scan runs on every dashboard poll. It used to end with
// `ORDER BY timestamp DESC, id DESC`. Because `id` is a UUID there is no index
// that satisfies both terms, so SQLite fell back to
// "USE TEMP B-TREE FOR LAST TERM OF ORDER BY" — materialising and sorting the
// whole result set. On the live 50k-row database that was ~19% of the summary's
// CPU (pprof: _vdbeSorterSort/_vdbeSorterMerge/_vdbeSorterCompareText).
//
// This test asserts the plan directly, so a future edit that reintroduces a
// second ORDER BY term fails here instead of quietly costing every dashboard
// refresh a full sort.
func TestDistributionQueryHasNoTempSort(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	defer store.Close()

	plan := explainPlan(t, store, distributionQuery(""))

	for _, line := range plan {
		if strings.Contains(strings.ToUpper(line), "TEMP B-TREE") {
			t.Errorf("distribution query uses a temp b-tree (%q); plan:\n  %s",
				line, strings.Join(plan, "\n  "))
		}
	}
	// The scan must still be driven by the timestamp index, otherwise dropping
	// the tie-break traded a sort for a full table scan.
	indexed := false
	for _, line := range plan {
		if strings.Contains(line, "idx_usage_records_timestamp") {
			indexed = true
		}
	}
	if !indexed {
		t.Errorf("distribution query no longer uses idx_usage_records_timestamp; plan:\n  %s",
			strings.Join(plan, "\n  "))
	}
}

// TestDistributionScanIsNewestFirst pins the semantics the tie-break was there
// for: the bounded samples (scatter points, slowest requests) must describe the
// most recent traffic, so the scan has to lead with the newest rows.
func TestDistributionScanIsNewestFirst(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	base := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 20; i++ {
		if err := store.Insert(ctx, Record{
			ID:        uuidForTest(i),
			Timestamp: base.Add(time.Duration(i) * time.Minute),
			Model:     "m",
			Provider:  "p",
			LatencyMs: 100,
		}); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}

	got, err := store.distributionTimestamps(ctx, QueryRange{}, 5)
	if err != nil {
		t.Fatalf("distributionTimestamps: %v", err)
	}
	if len(got) != 5 {
		t.Fatalf("got %d timestamps, want 5", len(got))
	}
	// Compare parsed instants rather than strings: the stored format is
	// RFC3339Nano with a fixed 9-digit fraction, while time.RFC3339Nano trims
	// trailing zeros, so the two spellings of the same instant differ.
	parsed := make([]time.Time, len(got))
	for i, s := range got {
		ts, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			t.Fatalf("parse %q: %v", s, err)
		}
		parsed[i] = ts.UTC()
	}
	// The newest row is the last one inserted (base + 19m).
	wantNewest := base.Add(19 * time.Minute).UTC()
	if !parsed[0].Equal(wantNewest) {
		t.Errorf("first row = %s, want the newest %s", parsed[0], wantNewest)
	}
	for i := 1; i < len(parsed); i++ {
		if !parsed[i].Before(parsed[i-1]) {
			t.Errorf("scan is not descending: row %d = %s, previous = %s", i, parsed[i], parsed[i-1])
		}
	}
}

// uuidForTest builds a distinct UUID-shaped id; the store treats the id as an
// opaque primary key, and real ids are UUIDs (which is why ORDER BY id was
// meaningless as a tie-break).
func uuidForTest(i int) string {
	const hex = "0123456789abcdef"
	return "00000000-0000-4000-8000-0000000000" + string([]byte{hex[(i>>4)&0xf], hex[i&0xf]})
}

// explainPlan returns SQLite's query plan for a statement, one line per step.
func explainPlan(t *testing.T, store *SQLiteStore, query string) []string {
	t.Helper()
	rows, err := store.db.Query("EXPLAIN QUERY PLAN " + query)
	if err != nil {
		t.Fatalf("EXPLAIN QUERY PLAN: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id, parent, notUsed int
		var detail string
		if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
			t.Fatalf("scan plan row: %v", err)
		}
		out = append(out, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("plan rows: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("empty query plan")
	}
	return out
}
