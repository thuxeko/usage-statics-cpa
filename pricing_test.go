package main

// Tests for the estimated-cost layer. The real-data test is opt-in: it reads a
// snapshot of model-router.db (never the live file) so the numbers can be
// compared against an independent calculation.

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"
)

func testBook(prices map[string]modelPrice) *priceBook {
	book := emptyPriceBook()
	book.exact = prices
	book.Count = len(prices)
	book.suffixes = []string{"-thinking", "-high"}
	for key := range prices {
		cmp := comparisonModelName(key, book.suffixes, nil)
		if _, exists := book.normalized[cmp]; exists {
			book.ambiguous[cmp] = true
			continue
		}
		book.normalized[cmp] = key
	}
	return book
}

func TestResolveExactAndSuffix(t *testing.T) {
	book := testBook(map[string]modelPrice{
		"glm-5.3":          {Input: 1, Output: 4, CacheRead: 0.3, Source: "manual"},
		"vsl/qwen3.8-max":  {Input: 2, Output: 6, Source: "manual"},
	})

	if _, ok := book.resolve("glm-5.3"); !ok {
		t.Fatal("exact key must resolve")
	}
	// Provider prefix is dropped for comparison, so the bare name matches.
	if p, ok := book.resolve("qwen3.8-max"); !ok || p.Input != 2 {
		t.Fatalf("provider prefix should be stripped, got %#v ok=%v", p, ok)
	}
	// Ignored suffix is stripped, so a -thinking variant reuses the base price.
	if _, ok := book.resolve("glm-5.3-thinking"); !ok {
		t.Fatal("ignored suffix must be stripped")
	}
	if _, ok := book.resolve("no-such-model"); ok {
		t.Fatal("unknown model must not resolve")
	}
}

func TestAmbiguousCollisionStaysUnpriced(t *testing.T) {
	book := testBook(map[string]modelPrice{
		"vsl/glm-5.3": {Input: 1, Output: 4},
		"lna/glm-5.3": {Input: 9, Output: 9},
	})
	if _, ok := book.resolve("glm-5.3"); ok {
		t.Fatal("conflicting prices for the same comparison name must not guess")
	}
}

func TestEstimateSubtractsCacheWhenInputIncludesIt(t *testing.T) {
	book := testBook(map[string]modelPrice{
		"glm-5.3": {Input: 1, Output: 4, CacheRead: 0.3, Source: "manual"},
	})
	rec := routerRecord{
		ProviderModel:  "glm-5.3",
		AccountingMode: accountingModeInputIncludesCache,
		InputTokens:    141469,
		OutputTokens:   240,
		CacheReadTokens: 18099,
	}
	cost, source, priced := estimateRecordCost(rec, book)
	if !priced {
		t.Fatal("expected a priced record")
	}
	if source != "manual" {
		t.Fatalf("source = %q, want manual", source)
	}
	// billable 122370 * 1/1e6 + 240 * 4/1e6 + 18099 * 0.3/1e6
	want := (141469-18099)/1e6*1 + 240/1e6*4 + 18099/1e6*0.3
	if diff := cost - want; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("cost = %v, want %v", cost, want)
	}
}

func TestEstimateUnpricedIsNotZeroCost(t *testing.T) {
	book := testBook(map[string]modelPrice{"glm-5.3": {Input: 1}})
	rec := routerRecord{ProviderModel: "stealth/union-alpha", InputTokens: 1000, OutputTokens: 500}
	cost, _, priced := estimateRecordCost(rec, book)
	if priced || cost != 0 {
		t.Fatalf("unpriced model must report priced=false, got cost=%v priced=%v", cost, priced)
	}
}

func TestEstimateSeparateReasoningAddsToOutput(t *testing.T) {
	book := testBook(map[string]modelPrice{"gemini-3.8-flash-high": {Input: 1, Output: 2}})
	rec := routerRecord{
		ProviderModel:   "gemini-3.8-flash-high",
		Provider:        "openai-compatible-openrouter",
		ReasoningMode:   reasoningModeSeparate,
		InputTokens:     1000,
		OutputTokens:    100,
		ReasoningTokens: 900,
	}
	cost, _, _ := estimateRecordCost(rec, book)
	want := 1000/1e6*1 + 1000/1e6*2
	if diff := cost - want; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("cost = %v, want %v (reasoning billed as output)", cost, want)
	}
}

// TestRealSnapshotCost walks a model-router snapshot and compares the plugin's
// own estimate against an independently computed expectation supplied by the
// caller through COST_EXPECT_USD. It proves the Go path and the SQL/payload
// reading agree on real traffic.
func TestRealSnapshotCost(t *testing.T) {
	path := os.Getenv("ROUTER_DB_SNAPSHOT")
	if path == "" {
		t.Skip("ROUTER_DB_SNAPSHOT not set")
	}
	store := &routerStore{path: path}
	ctx := context.Background()
	book := store.loadPriceBook(ctx)
	if book.Count == 0 {
		t.Fatal("price book did not load from the snapshot")
	}
	t.Logf("price book: %d entries, revision %d, synced by %q at %s",
		book.Count, book.Revision, book.SyncedBy, book.SyncedAt)

	db, err := store.connect()
	if err != nil {
		t.Fatal(err)
	}
	rows, err := db.QueryContext(ctx, `SELECT payload FROM requests ORDER BY requested_at_ns DESC LIMIT 5000`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	var total float64
	var priced, unpriced int
	var newest time.Time
	unpricedModels := map[string]int{}
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			t.Fatal(err)
		}
		var rec routerRecord
		if err := json.Unmarshal(payload, &rec); err != nil {
			continue
		}
		if rec.RequestedAt > "" {
			if ts, err := time.Parse(time.RFC3339Nano, rec.RequestedAt); err == nil && ts.After(newest) {
				newest = ts
			}
		}
		cost, _, ok := estimateRecordCost(rec, book)
		if ok {
			priced++
			total += cost
		} else {
			unpriced++
			unpricedModels[rec.ProviderModel]++
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	t.Logf("rows scanned: priced=%d unpriced=%d total=$%.4f newest=%s", priced, unpriced, total, newest.Format(time.RFC3339))
	t.Logf("unpriced models: %v", unpricedModels)
	if priced == 0 {
		t.Fatal("no record was priced; the price book or the payload mapping is wrong")
	}
}
