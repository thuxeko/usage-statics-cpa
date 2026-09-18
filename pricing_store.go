package main

// Persistence for manual price overrides (the "Sync" dialog).
//
// These live in the plugin's own usage.db, never in model-router.db: the router
// plugin owns its database and we only read it. An override is applied on top
// of the router price book at resolve() time, so it always wins.

import (
	"context"
	"database/sql"
	"strings"
)

// LoadPriceOverrides returns every manual override keyed by lowercased model
// name, matching how resolve() looks them up.
func (s *SQLiteStore) LoadPriceOverrides(ctx context.Context) (map[string]priceOver, error) {
	out := map[string]priceOver{}
	if s == nil {
		return out, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT model, input, output, cache_read, cache_creation, accounting_mode, updated_at FROM price_overrides`)
	if err != nil {
		return out, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var ov priceOver
		var mode, updated sql.NullString
		if err := rows.Scan(&ov.Model, &ov.Input, &ov.Output, &ov.CacheRead, &ov.CacheCreation, &mode, &updated); err != nil {
			return out, err
		}
		ov.AccountingMode = mode.String
		ov.UpdatedAt = updated.String
		out[strings.ToLower(strings.TrimSpace(ov.Model))] = ov
	}
	return out, rows.Err()
}

// SavePriceOverrides replaces the whole override set in one transaction. The
// caller sends the authoritative list, so removing an entry in the dialog also
// removes it here (falling back to the router book price).
func (s *SQLiteStore) SavePriceOverrides(ctx context.Context, overrides []priceOver) error {
	if s == nil {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM price_overrides`); err != nil {
		return err
	}
	statement, err := tx.PrepareContext(ctx, `INSERT INTO price_overrides (model, input, output, cache_read, cache_creation, accounting_mode, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer func() { _ = statement.Close() }()
	for _, ov := range overrides {
		model := strings.TrimSpace(ov.Model)
		if model == "" {
			continue
		}
		if _, err := statement.ExecContext(ctx, model, ov.Input, ov.Output, ov.CacheRead, ov.CacheCreation, ov.AccountingMode, ov.UpdatedAt); err != nil {
			return err
		}
	}
	return tx.Commit()
}
