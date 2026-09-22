package main

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"
)

// TestAntigravityLabels verifies that antigravity rows resolve to the upstream
// account rather than the bare provider name. Older rows carry an empty
// auth_id but still have auth_index; those legitimately fall back to the
// provider, which is why the test asserts on rows that DO carry an account.
func TestAntigravityLabels(t *testing.T) {
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
	page, err := store.ListPage(ctx, QueryRange{}, PageFilter{Provider: "antigravity", Limit: 500})
	if err != nil {
		t.Fatalf("ListPage: %v", err)
	}
	rows := page.Rows

	seen := map[string]string{}
	for _, r := range rows {
		if r.Provider != "antigravity" {
			continue
		}
		seen[r.AuthID] = providerLabel(r)
	}
	if len(seen) == 0 {
		t.Skip("no antigravity rows in the most recent window")
	}
	fmt.Printf("\n--- antigravity auth_id -> label ---\n")
	for id, label := range seen {
		fmt.Printf("  %-46s -> %s\n", id, label)
		if id != "" && label == "antigravity" {
			t.Errorf("row with auth_id %q still renders as bare provider", id)
		}
		if id != "" && label != accountFromAuthID(id, "antigravity") {
			t.Errorf("auth_id %q -> %q, expected account %q", id, label, accountFromAuthID(id, "antigravity"))
		}
	}
	fmt.Printf("--- %d distinct antigravity identities ---\n\n", len(seen))
}