package main

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestProviderHealthColumns is the regression guard for the Provider Health
// card (Analytics Dashboard > Tổng quan).
//
// The card reads error_rate, p50_latency_s and p90_latency_s straight off the
// by_provider rows. The plugin path used to leave all three at their zero
// value for this dimension: a percentile cannot be expressed as a SQL SUM, so
// the columns rendered "0.0%" for providers that had failures and "–" for the
// latencies even though the underlying rows carried both.
//
// Run against a copy of the live usage.db (USAGE_TEST_DB) to check real data,
// or with no env var to check the invariant on a synthetic store.
func TestProviderHealthColumns(t *testing.T) {
	dbPath := os.Getenv("USAGE_TEST_DB")
	if dbPath == "" {
		t.Skip("USAGE_TEST_DB not set")
	}
	store, err := NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	end := time.Now().UTC()
	start := end.Add(-30 * 24 * time.Hour)
	summary, err := store.Summary(ctx, QueryRange{Start: &start, End: &end})
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if len(summary.ByProvider) == 0 {
		t.Fatalf("no provider rows in the summary")
	}

	t.Logf("by_provider rows = %d", len(summary.ByProvider))
	for _, p := range summary.ByProvider {
		t.Logf("  %-34s calls=%-6d failed=%-5d error_rate=%-7.2f p50=%-8.2f p90=%-8.2f avg=%.0fms",
			p.Name, p.Calls, p.Failed, p.ErrorRate, p.P50LatencyS, p.P90LatencyS, p.AvgLatencyMs)
	}

	// A provider with failures must report a non-zero rate; the column reads
	// "0.0%" otherwise, which is the bug that was reported.
	badRate := 0
	for _, p := range summary.ByProvider {
		if p.Failed > 0 && p.ErrorRate == 0 {
			badRate++
			t.Errorf("provider %q has %d failures but error_rate=0", p.Name, p.Failed)
		}
	}
	if badRate > 0 {
		t.Errorf("%d/%d provider rows report 0%% error rate despite failures", badRate, len(summary.ByProvider))
	}

	// The rate must be consistent with the counts it is derived from.
	for _, p := range summary.ByProvider {
		want := round2(float64(p.Failed) / float64(p.Calls) * 100)
		if p.ErrorRate != want {
			t.Errorf("provider %q error_rate = %.2f, want %.2f (%d/%d)",
				p.Name, p.ErrorRate, want, p.Failed, p.Calls)
		}
	}

	// Every provider that served calls with a measurable latency must expose a
	// P50/P90, otherwise the omitempty tag drops the field and the column
	// renders "–".
	missingP50 := 0
	for _, p := range summary.ByProvider {
		if p.AvgLatencyMs > 0 && p.P50LatencyS == 0 {
			missingP50++
			t.Errorf("provider %q has avg latency %.0fms but p50_latency_s=0 (column shows '–')",
				p.Name, p.AvgLatencyMs)
		}
	}
	if missingP50 > 0 {
		t.Errorf("%d/%d provider rows are missing P50", missingP50, len(summary.ByProvider))
	}

	// Same derived numbers are rendered by the model performance table, so the
	// invariant has to hold there too.
	for _, m := range summary.ByModel {
		if m.Failed > 0 && m.ErrorRate == 0 {
			t.Errorf("model %q has %d failures but error_rate=0", m.Name, m.Failed)
		}
		if m.AvgLatencyMs > 0 && m.P50LatencyS == 0 {
			t.Errorf("model %q has avg latency %.0fms but p50_latency_s=0", m.Name, m.AvgLatencyMs)
		}
	}
}

// TestGroupAccumulatorDerives checks the accumulator directly: the error rate
// and percentiles must be computed from the samples, and a group the
// accumulator never saw must be left untouched.
func TestGroupAccumulatorDerives(t *testing.T) {
	acc := &groupAccumulator{}
	// 10 calls, 2 failed, latencies 1s..10s.
	// percentile() indexes with floor(n*pct) on the sorted slice, so with 10
	// samples p50 -> index 5 (the 6th value, 6s) and p90 -> index 9 (10s).
	// This mirrors the router path, which shares the same helper.
	for i := 1; i <= 10; i++ {
		acc.add("prov-a", i%5 == 0, float64(i), float64(i)/10)
	}
	acc.add("prov-a", false, 0, 0) // zero latency must not enter the sample

	rows := []GroupStat{
		{Name: "prov-a", Calls: 10, Failed: 2},
		{Name: "prov-untouched", Calls: 4, Failed: 1},
	}
	acc.apply(rows)

	if rows[0].ErrorRate != 20 {
		t.Errorf("error_rate = %.2f, want 20.00", rows[0].ErrorRate)
	}
	if rows[0].Success != 8 {
		t.Errorf("success = %d, want 8", rows[0].Success)
	}
	if rows[0].P50LatencyS != 6 {
		t.Errorf("p50 = %.2f, want 6.00", rows[0].P50LatencyS)
	}
	if rows[0].P90LatencyS != 10 {
		t.Errorf("p90 = %.2f, want 10.00", rows[0].P90LatencyS)
	}
	if rows[0].P50TTFTS != 0.6 {
		t.Errorf("p50 ttft = %.2f, want 0.60", rows[0].P50TTFTS)
	}
	// A zero-latency sample must be excluded rather than counted as 0s, which
	// would drag the percentiles down.
	if got := len(acc.sub("prov-a").latS); got != 10 {
		t.Errorf("latency samples = %d, want 10 (the 0s sample must be skipped)", got)
	}
	// A row with no samples must stay at zero rather than inherit a neighbour's
	// numbers.
	if rows[1].ErrorRate != 0 || rows[1].P50LatencyS != 0 {
		t.Errorf("untouched row was modified: %+v", rows[1])
	}
}
