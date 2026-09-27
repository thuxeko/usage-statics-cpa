package main

import (
	"context"
	"fmt"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// TestStoreLatestIsNewestFirstByTime guards the realtime panel's ordering.
//
// usage_records.id is a TEXT primary key holding UUIDs and hex digests, so
// "ORDER BY id DESC" sorts alphabetically and interleaves months at random.
// The implicit rowid is monotonic and matches insertion order, which is what
// the panel needs. The bug shipped once (the CPU commit switched
// "ORDER BY timestamp, id" to "ORDER BY id DESC" to dodge a temp b-tree) and
// showed up as a shuffled "Requests (Realtime Log)" table.
func TestStoreLatestIsNewestFirstByTime(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 9, 27, 7, 0, 0, 0, time.UTC)

	// Insert in chronological order with ids chosen so alphabetical order is the
	// EXACT REVERSE of insertion order. A correct implementation returns the
	// newest row first; an "ORDER BY id DESC" implementation returns the oldest.
	ids := []string{
		"ffffffff-ffff-4fff-8fff-ffffffffffff", // oldest
		"eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee",
		"dddddddd-dddd-4ddd-8ddd-dddddddddddd",
		"cccccccc-cccc-4ccc-8ccc-cccccccccccc",
		"bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb",
		"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", // newest
	}
	for i, id := range ids {
		rec := Record{
			ID:        id,
			Timestamp: base.Add(time.Duration(i) * time.Minute),
			Provider:  "test",
			Model:     fmt.Sprintf("model-%d", i),
		}
		if err := store.Insert(ctx, rec); err != nil {
			t.Fatalf("Insert %s: %v", id, err)
		}
	}

	got, err := store.StoreLatest(ctx, 6)
	if err != nil {
		t.Fatalf("StoreLatest: %v", err)
	}
	if len(got) != 6 {
		t.Fatalf("StoreLatest returned %d rows, want 6", len(got))
	}

	// The newest inserted record must come first.
	if got[0].ID != ids[len(ids)-1] {
		t.Errorf("first row = %q, want newest %q (ordering is not newest-first)",
			got[0].ID, ids[len(ids)-1])
	}
	if got[len(got)-1].ID != ids[0] {
		t.Errorf("last row = %q, want oldest %q", got[len(got)-1].ID, ids[0])
	}

	// And the timestamps must be strictly descending, which is the property the
	// UI actually renders.
	for i := 1; i < len(got); i++ {
		if !got[i-1].Timestamp.After(got[i].Timestamp) {
			t.Errorf("row %d timestamp %v is not after row %d timestamp %v",
				i-1, got[i-1].Timestamp, i, got[i].Timestamp)
		}
	}
}

// TestStoreLatestLimitTakesNewest pins the interaction between ordering and
// LIMIT: with a wrong ORDER BY the limit silently keeps the OLDEST rows.
func TestStoreLatestLimitTakesNewest(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 9, 27, 7, 0, 0, 0, time.UTC)

	for i := 0; i < 30; i++ {
		rec := Record{
			ID:        fmt.Sprintf("ffffffff-%04d-4fff-8fff-ffffffffffff", i),
			Timestamp: base.Add(time.Duration(i) * time.Minute),
			Provider:  "test",
			Model:     fmt.Sprintf("model-%d", i),
		}
		if err := store.Insert(ctx, rec); err != nil {
			t.Fatalf("Insert %d: %v", i, err)
		}
	}

	got, err := store.StoreLatest(ctx, 5)
	if err != nil {
		t.Fatalf("StoreLatest: %v", err)
	}
	if len(got) != 5 {
		t.Fatalf("StoreLatest returned %d rows, want 5", len(got))
	}
	// Newest of all 30 is model-29.
	if got[0].Model != "model-29" {
		t.Errorf("first row model = %q, want %q", got[0].Model, "model-29")
	}
}
