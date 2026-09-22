package main

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestLiveSummaryHasTokenTrendAndCost is the regression guard for the two
// dashboard panels that rendered empty under source=plugin:
//
//   - "Token usage trend" needs per-hour input/output tokens. The old
//     fillByHour selected only total_tokens, so the chart had nothing to draw.
//   - "Tổng chi phí ước tính" needs the price book applied to usage.db rows.
//     Pricing used to exist only on the router store, so cost_usd stayed 0 and
//     pricing.available stayed false.
//
// It runs against a real copy of usage.db plus the live model-router.db (which
// carries the price book). USAGE_TEST_DB points at the copy; ROUTER_TEST_DIR
// points at a directory holding model-router.db.
func TestLiveSummaryHasTokenTrendAndCost(t *testing.T) {
	dbPath := os.Getenv("USAGE_TEST_DB")
	if dbPath == "" {
		t.Skip("USAGE_TEST_DB not set")
	}
	store, err := NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	end := time.Now().UTC()
	start := end.Add(-24 * time.Hour)
	rng := QueryRange{Start: &start, End: &end}

	summary, err := store.Summary(ctx, rng)
	if err != nil {
		t.Fatalf("summary: %v", err)
	}

	// --- Token usage trend -------------------------------------------------
	if len(summary.ByHour) == 0 {
		t.Fatalf("by_hour is empty; the trend chart has nothing to draw")
	}
	var hourInput, hourOutput, hourTotal int64
	hoursWithTokens := 0
	for _, h := range summary.ByHour {
		hourInput += h.InputTokens
		hourOutput += h.OutputTokens
		hourTotal += h.TotalTokens
		if h.InputTokens > 0 || h.OutputTokens > 0 {
			hoursWithTokens++
		}
	}
	t.Logf("by_hour: %d buckets, %d with tokens, input=%d output=%d total=%d",
		len(summary.ByHour), hoursWithTokens, hourInput, hourOutput, hourTotal)

	if hoursWithTokens == 0 {
		t.Fatalf("every by_hour bucket has zero input/output tokens; the chart would be empty")
	}
	if hourInput != summary.Totals.InputTokens {
		t.Errorf("hour input %d != totals input %d: hourly buckets must sum to the totals",
			hourInput, summary.Totals.InputTokens)
	}
	if hourOutput != summary.Totals.OutputTokens {
		t.Errorf("hour output %d != totals output %d", hourOutput, summary.Totals.OutputTokens)
	}

	// --- Estimated cost ----------------------------------------------------
	t.Logf("cost: usd=%.4f priced=%d unpriced=%d partial=%v available=%v entries=%d",
		summary.Totals.CostUSD, summary.Totals.CostPricedCalls, summary.Totals.CostUnpricedCalls,
		summary.Totals.CostPartial, summary.Pricing.Available, summary.Pricing.Entries)

	if !summary.Pricing.Available {
		t.Fatalf("pricing.available is false; the dashboard would say 'Chưa có bảng giá'")
	}
	if summary.Totals.CostPricedCalls == 0 {
		t.Fatalf("no priced calls, yet the price book is available: the book is not being applied")
	}
	if summary.Totals.CostUSD <= 0 {
		t.Fatalf("cost_usd is %.4f with %d priced calls: pricing silently produced zero",
			summary.Totals.CostUSD, summary.Totals.CostPricedCalls)
	}
	// Unpriced calls must be visible, not folded into the sum as free.
	if summary.Totals.CostUnpricedCalls != summary.Totals.Calls-summary.Totals.CostPricedCalls {
		t.Errorf("priced %d + unpriced %d != total calls %d",
			summary.Totals.CostPricedCalls, summary.Totals.CostUnpricedCalls, summary.Totals.Calls)
	}

	// Per-model rows must carry the cost so the overview table can show it.
	withCost := 0
	var seenCost float64
	for _, m := range summary.ByModel {
		if m.PricedCalls > 0 {
			withCost++
			seenCost += m.CostUSD
		}
	}
	if withCost == 0 {
		t.Fatalf("no per-model row carries a cost")
	}
	t.Logf("by_model: %d rows, %d priced, sum=%.4f (totals %.4f)", len(summary.ByModel), withCost, seenCost, summary.Totals.CostUSD)
	if diff := seenCost - summary.Totals.CostUSD; diff > 0.01 || diff < -0.01 {
		t.Errorf("per-model cost sum %.4f disagrees with totals %.4f (diff %.4f)",
			seenCost, summary.Totals.CostUSD, diff)
	}

	// --- Reasoning / cache columns (previously missing, showed as 0) --------
	if summary.Totals.ReasoningTokens == 0 {
		t.Logf("warning: totals reasoning_tokens is 0")
	}
	if summary.Totals.CachedTokens == 0 {
		t.Errorf("totals cached_tokens is 0; cache column would read empty")
	}
	reasoningInModels := int64(0)
	cachedInModels := int64(0)
	for _, m := range summary.ByModel {
		reasoningInModels += m.ReasoningTokens
		cachedInModels += m.CachedTokens
	}
	if reasoningInModels != summary.Totals.ReasoningTokens {
		t.Errorf("per-model reasoning %d != totals %d", reasoningInModels, summary.Totals.ReasoningTokens)
	}
	if cachedInModels != summary.Totals.CachedTokens {
		t.Errorf("per-model cached %d != totals %d", cachedInModels, summary.Totals.CachedTokens)
	}

	// --- Latency trend needs per-hour percentiles now that we scan rows -----
	latencyHours := 0
	for _, h := range summary.ByHour {
		if h.P50LatencyS > 0 {
			latencyHours++
		}
	}
	if latencyHours == 0 {
		t.Errorf("no by_hour bucket carries a latency percentile; the latency trend chart stays empty")
	}

	// --- Performance tab fields (same omission class) ----------------------
	t.Logf("perf: p50=%.2f p90=%.2f p99=%.2f max=%.2f ttft_p50=%.2f ttft_p90=%.2f hung=%d cache_hit=%d",
		summary.Totals.P50LatencyS, summary.Totals.P90LatencyS, summary.Totals.P99LatencyS,
		summary.Totals.MaxLatencyS, summary.Totals.P50TTFTS, summary.Totals.P90TTFTS,
		summary.Totals.HungCalls, summary.Totals.CacheHitCalls)
	if summary.Totals.P50LatencyS <= 0 || summary.Totals.P90LatencyS <= 0 {
		t.Errorf("latency percentiles are zero; the Performance tab would show '–'")
	}
	if summary.Totals.P90LatencyS < summary.Totals.P50LatencyS {
		t.Errorf("p90 %.2f < p50 %.2f", summary.Totals.P90LatencyS, summary.Totals.P50LatencyS)
	}
	if summary.Totals.P99LatencyS < summary.Totals.P90LatencyS {
		t.Errorf("p99 %.2f < p90 %.2f", summary.Totals.P99LatencyS, summary.Totals.P90LatencyS)
	}
	if summary.Totals.MaxLatencyS < summary.Totals.P99LatencyS {
		t.Errorf("max %.2f < p99 %.2f", summary.Totals.MaxLatencyS, summary.Totals.P99LatencyS)
	}
	if summary.Totals.CacheHitCalls <= 0 {
		t.Errorf("cache_hit_calls is 0 although the range has cached tokens; the cache rate would read 0%%")
	}
	if summary.Totals.CacheHitCalls > summary.Totals.Calls {
		t.Errorf("cache_hit_calls %d exceeds total calls %d", summary.Totals.CacheHitCalls, summary.Totals.Calls)
	}
	if summary.Totals.Success != summary.Totals.Calls-summary.Totals.Failed {
		t.Errorf("success %d != calls %d - failed %d", summary.Totals.Success, summary.Totals.Calls, summary.Totals.Failed)
	}
	if summary.Totals.ActiveModels != int64(len(summary.ByModel)) {
		t.Errorf("active_models %d != len(by_model) %d", summary.Totals.ActiveModels, len(summary.ByModel))
	}

	// --- "Bảng Hiệu năng Model": the Provider column ------------------------
	//
	// The column used to render the `sub` field, which is only ever set for the
	// alias dimension, so it showed "–" for every model row.
	missingProvider := 0
	multiProvider := 0
	for _, m := range summary.ByModel {
		if m.Provider == "" {
			missingProvider++
			continue
		}
		t.Logf("  model %-24s provider=%-34s calls=%d", m.Name, m.Provider, m.Calls)
		if contains(m.Provider, "(+") {
			multiProvider++
		}
	}
	if missingProvider > 0 {
		t.Errorf("%d/%d by_model rows have no provider; the column would show '–'",
			missingProvider, len(summary.ByModel))
	}
	t.Logf("by_model: %d rows, %d served by multiple providers", len(summary.ByModel), multiProvider)

	// --- Tab "Token & Cache": the two cards the user reported as empty -----
	//
	// "Phân bố Kích thước Context" reads token_distribution, "Mức suy nghĩ
	// (Reasoning) × Hiệu năng" reads by_effort. Both had data in usage.db but
	// were never filled on the plugin path.
	var distTotal int64
	for _, b := range summary.TokenDistribution {
		distTotal += b.Calls
	}
	t.Logf("context distribution: %d buckets, %d calls total", len(summary.TokenDistribution), distTotal)
	if len(summary.TokenDistribution) == 0 {
		t.Errorf("token_distribution is empty; 'Phân bố Kích thước Context' renders nothing")
	}
	if distTotal != summary.Totals.Calls {
		t.Errorf("context distribution covers %d calls but the range has %d", distTotal, summary.Totals.Calls)
	}
	for _, b := range summary.TokenDistribution {
		if b.Calls > 0 {
			t.Logf("  context %-12s %d", b.Label, b.Calls)
		}
	}

	t.Logf("by_effort: %d rows", len(summary.ByEffort))
	if len(summary.ByEffort) == 0 {
		t.Errorf("by_effort is empty; 'Mức suy nghĩ (Reasoning) × Hiệu năng' renders nothing")
	}
	var effortCalls int64
	for _, e := range summary.ByEffort {
		effortCalls += e.Calls
		t.Logf("  effort %-14s calls=%d p50=%.1fs in=%d out=%d reasoning=%d",
			e.Name, e.Calls, e.P50LatencyS, e.InputTokens, e.OutputTokens, e.ReasoningTokens)
	}
	if effortCalls != summary.Totals.Calls {
		t.Errorf("by_effort covers %d calls but the range has %d", effortCalls, summary.Totals.Calls)
	}
	// The card shows p50/p90 per effort, so they must be populated.
	withP50 := 0
	for _, e := range summary.ByEffort {
		if e.P50LatencyS > 0 {
			withP50++
		}
	}
	if withP50 == 0 {
		t.Errorf("no by_effort row carries a p50 latency; the card's latency columns stay empty")
	}

	// --- Same omission class on the Performance tab ------------------------
	t.Logf("by_executor: %d rows, latency_distribution: %d buckets, scatter: %d points, slowest: %d rows",
		len(summary.ByExecutor), len(summary.LatencyDistribution), len(summary.ScatterPoints), len(summary.SlowestRequests))
	if len(summary.ByExecutor) == 0 {
		t.Errorf("by_executor is empty; the executor table renders nothing")
	}
	if len(summary.LatencyDistribution) == 0 {
		t.Errorf("latency_distribution is empty; the latency histogram renders nothing")
	}
	if len(summary.ScatterPoints) == 0 {
		t.Errorf("scatter_points is empty; the latency/TTFT scatter renders nothing")
	}
	if len(summary.SlowestRequests) == 0 {
		t.Errorf("slowest_requests is empty; the slowest table renders nothing")
	}
	if len(summary.SlowestRequests) > 1 {
		for i := 1; i < len(summary.SlowestRequests); i++ {
			if summary.SlowestRequests[i-1].LatencyMs < summary.SlowestRequests[i].LatencyMs {
				t.Errorf("slowest_requests is not sorted by latency desc at %d", i)
				break
			}
		}
	}
}
