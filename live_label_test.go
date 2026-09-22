package main

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"
)

// TestLiveProviderLabels maps the most recent real rows in usage.db through
// providerLabel, so the operator-facing strings can be eyeballed against real
// traffic. Skipped automatically when the DB is not present.
func TestLiveProviderLabels(t *testing.T) {
	dbPath := os.Getenv("USAGE_TEST_DB")
	if dbPath == "" {
		t.Skip("USAGE_TEST_DB not set")
	}
	store, err := NewSQLiteStore(dbPath)
	if err != nil {
		t.Skipf("cannot open %s: %v", dbPath, err)
	}
	defer func() { _ = store.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rows, err := store.StoreLatest(ctx, 60)
	if err != nil {
		t.Fatalf("StoreLatest: %v", err)
	}
	if len(rows) == 0 {
		t.Skip("no rows in db")
	}
	fmt.Printf("\n--- %d real rows: provider -> label ---\n", len(rows))
	seen := map[string]string{}
	for _, r := range rows {
		label := providerLabel(r)
		if old, ok := seen[r.Provider]; !ok || old != label {
			fmt.Printf("  %-34s -> %-46s (base_url=%q auth_id=%q)\n", r.Provider, label, r.BaseURL, r.AuthID)
			seen[r.Provider] = label
		}
	}
	fmt.Printf("--- distinct providers: %d ---\n\n", len(seen))
}