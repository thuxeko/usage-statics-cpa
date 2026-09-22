package main

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestLiveSummaryLatency guards the dashboard against a summary that becomes
// too expensive to serve. The plugin source now scans rows in Go (for hourly
// percentiles and pricing) instead of aggregating purely in SQL, so the cost of
// a wide range has to be measured, not assumed.
func TestLiveSummaryLatency(t *testing.T) {
	dbPath := os.Getenv("USAGE_TEST_DB")
	if dbPath == "" {
		t.Skip("USAGE_TEST_DB not set")
	}
	store, err := NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	end := time.Now().UTC()

	for _, days := range []int{1, 7, 30} {
		start := end.Add(-time.Duration(days) * 24 * time.Hour)
		rng := QueryRange{Start: &start, End: &end}

		// Warm the page cache so the number reflects CPU work, not disk.
		if _, err := store.Summary(ctx, rng); err != nil {
			t.Fatalf("warm %dd: %v", days, err)
		}
		begin := time.Now()
		summary, err := store.Summary(ctx, rng)
		elapsed := time.Since(begin)
		if err != nil {
			t.Fatalf("summary %dd: %v", days, err)
		}
		t.Logf("%2dd: %6.0f ms · %5d calls · %3d model rows · %3d hour buckets · cost $%.2f",
			days, float64(elapsed.Microseconds())/1000, summary.Totals.Calls,
			len(summary.ByModel), len(summary.ByHour), summary.Totals.CostUSD)
		if elapsed > 3*time.Second {
			t.Errorf("%dd summary took %v; too slow for an interactive dashboard", days, elapsed)
		}
	}
}
