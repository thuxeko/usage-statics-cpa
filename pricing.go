package main

// Estimated cost for usage records, derived from the model-router price book.
//
// Design notes (agreed with the user, 2026-09-18):
//   - The price book lives in model-router.db (store_state.prices_json) and is
//     read-only here: the router plugin owns it, we never write to it.
//   - Manual prices always win over catalog prices; the router already enforces
//     that when it saves the book, so we only need to read what it stored.
//   - A model with no price is NEVER treated as free. Records are counted as
//     "unpriced" and reported separately so the UI can say so out loud.
//   - Cache accounting matters: the router payload records
//     accounting_mode=input_includes_cache for openai-compatible providers, so
//     the cached tokens are already part of input_tokens. Billing both would
//     double-charge (verified: glm-5.3 24h goes $70.83 -> ~$90).
//   - Resolution mirrors the router: exact key, case-insensitive key, then a
//     normalized comparison name (drop provider prefix, strip ignored suffixes
//     such as -thinking/-high, apply sync mappings). If two distinct prices
//     normalize to the same name we treat it as ambiguous and leave it
//     unpriced rather than guessing.

import (
	"context"
	"encoding/json"
	"math"
	"strings"
)

const (
	accountingModeInputIncludesCache = "input_includes_cache"
	accountingModeInputExcludesCache = "input_excludes_cache"
	reasoningModeIncluded            = "included"
	reasoningModeSeparate            = "separate"
)

// modelPrice is one entry of the router price book. Rates are USD per million
// tokens, matching the router's own modelPrice shape.
type modelPrice struct {
	Input         float64 `json:"input"`
	Output        float64 `json:"output"`
	CacheRead     float64 `json:"cache_read"`
	CacheCreation float64 `json:"cache_creation"`
	AccountingMode string `json:"accounting_mode,omitempty"`
	Source         string `json:"source,omitempty"`
}

// priceBook holds the decoded router price book plus the lookup index.
type priceBook struct {
	Revision   uint64                `json:"revision"`
	SyncedAt   string                `json:"last_sync_at,omitempty"`
	SyncedBy   string                `json:"last_sync_source,omitempty"`
	Count      int                   `json:"count"`
	exact      map[string]modelPrice
	normalized map[string]string // comparison name -> exact key
	ambiguous  map[string]bool   // comparison names with conflicting prices
	suffixes   []string             // ignored suffixes from the router sync settings
	mappings   map[string]string    // sync mappings (source -> target)
	overrides  map[string]priceOver // manual overrides from usage.db (lowercased key)
}

type priceBookFile struct {
	Revision     uint64                `json:"revision"`
	Prices       map[string]modelPrice `json:"prices"`
	SyncSettings struct {
		IgnoredSuffixes []string `json:"ignored_suffixes"`
		Mappings        []struct {
			Source string `json:"source"`
			Target string `json:"target"`
		} `json:"mappings"`
	} `json:"sync_settings"`
	LastSync *struct {
		Source      string `json:"source"`
		CompletedAt string `json:"completed_at"`
	} `json:"last_sync"`
}

// emptyPriceBook is returned when the router DB has no price book yet, so the
// dashboard can still render "chưa có giá" instead of failing.
func emptyPriceBook() *priceBook {
	return &priceBook{
		exact:      map[string]modelPrice{},
		normalized: map[string]string{},
		ambiguous:  map[string]bool{},
		overrides:  map[string]priceOver{},
	}
}

// loadPriceBook reads the router price book. Any failure degrades to an empty
// book: pricing is an estimate layer and must never break the usage dashboard.
func (r *routerStore) loadPriceBook(ctx context.Context) *priceBook {
	book := emptyPriceBook()
	if r == nil {
		return book
	}
	db, err := r.connect()
	if err != nil {
		return book
	}
	var raw []byte
	if err := db.QueryRowContext(ctx, `SELECT prices_json FROM store_state WHERE id = 1`).Scan(&raw); err != nil {
		return book
	}
	var file priceBookFile
	if err := json.Unmarshal(raw, &file); err != nil {
		return book
	}
	book.Revision = file.Revision
	book.Count = len(file.Prices)
	book.exact = file.Prices
	if book.exact == nil {
		book.exact = map[string]modelPrice{}
	}
	if file.LastSync != nil {
		book.SyncedAt = file.LastSync.CompletedAt
		book.SyncedBy = file.LastSync.Source
	}

	suffixes := file.SyncSettings.IgnoredSuffixes
	mappings := map[string]string{}
	for _, m := range file.SyncSettings.Mappings {
		if m.Source != "" && m.Target != "" {
			mappings[strings.ToLower(strings.TrimSpace(m.Source))] = strings.ToLower(strings.TrimSpace(m.Target))
		}
	}
	book.suffixes = suffixes
	book.mappings = mappings

	// Build the comparison index over sorted keys so collisions are detected
	// deterministically.
	keys := make([]string, 0, len(book.exact))
	for k := range book.exact {
		keys = append(keys, k)
	}
	sortStrings(keys)
	for _, key := range keys {
		cmp := comparisonModelName(key, suffixes, mappings)
		if cmp == "" {
			continue
		}
		if existing, ok := book.normalized[cmp]; ok {
			if !sameRates(book.exact[existing], book.exact[key]) {
				book.ambiguous[cmp] = true
			}
			continue
		}
		book.normalized[cmp] = key
	}
	return book
}

// loadEffectivePriceBook loads the router price book and merges the manual
// overrides stored in usage.db. Overrides win at resolve() time.
func (r *routerStore) loadEffectivePriceBook(ctx context.Context) *priceBook {
	book := r.loadPriceBook(ctx)
	store := currentStore()
	if store == nil {
		return book
	}
	overrides, err := store.LoadPriceOverrides(ctx)
	if err != nil {
		return book
	}
	book.overrides = overrides
	return book
}

// compareKey returns the name used to decide whether two entries are the same
// model reached through different providers: it drops the provider prefix and
// the ignored suffixes (-thinking/-high/...). Two entries with the same key are
// CANDIDATES for merging — the caller must still check that their prices agree,
// because providers do quote different rates for the same base model.
func (b *priceBook) compareKey(model string) string {
	if b == nil {
		return strings.ToLower(strings.TrimSpace(model))
	}
	return comparisonModelName(model, b.suffixes, b.mappings)
}

// bookPrice returns the price the ROUTER book holds for a name (overrides are
// ignored). Used to decide whether two spellings of a model agree.
func (b *priceBook) bookPrice(model string) (modelPrice, bool) {
	model = strings.TrimSpace(model)
	if model == "" || b == nil {
		return modelPrice{}, false
	}
	if price, ok := b.exact[model]; ok {
		return price, true
	}
	for key, price := range b.exact {
		if strings.EqualFold(key, model) {
			return price, true
		}
	}
	return modelPrice{}, false
}

// hasBookEntry reports whether the ROUTER price book itself carries an entry
// for this model, ignoring manual overrides. Used to tell "came from the book"
// apart from "the user set this by hand".
func (b *priceBook) hasBookEntry(model string) bool {
	model = strings.TrimSpace(model)
	if model == "" || b == nil {
		return false
	}
	if _, ok := b.exact[model]; ok {
		return true
	}
	for key := range b.exact {
		if strings.EqualFold(key, model) {
			return true
		}
	}
	return false
}

// resolve finds the price for a model name. ok=false means "no price known",
// which the caller must report as unpriced rather than as zero cost.
func (b *priceBook) resolve(model string) (modelPrice, bool) {
	model = strings.TrimSpace(model)
	if model == "" || b == nil {
		return modelPrice{}, false
	}
	// A manual override always wins over the router book, so a synced catalog
	// value can never silently replace a price the user set by hand.
	if ov, ok := b.overrides[strings.ToLower(model)]; ok {
		return modelPrice{
			Input:          ov.Input,
			Output:         ov.Output,
			CacheRead:      ov.CacheRead,
			CacheCreation:  ov.CacheCreation,
			AccountingMode: ov.AccountingMode,
			Source:         "manual-override",
		}, true
	}
	if price, ok := b.exact[model]; ok {
		return price, true
	}
	for key, price := range b.exact {
		if strings.EqualFold(key, model) {
			return price, true
		}
	}
	cmp := comparisonModelName(model, b.suffixes, b.mappings)
	if cmp == "" {
		return modelPrice{}, false
	}
	if b.ambiguous[cmp] {
		return modelPrice{}, false
	}
	if key, ok := b.normalized[cmp]; ok {
		return b.exact[key], true
	}
	return modelPrice{}, false
}

func sameRates(left, right modelPrice) bool {
	return left.Input == right.Input &&
		left.Output == right.Output &&
		left.CacheRead == right.CacheRead &&
		left.CacheCreation == right.CacheCreation &&
		left.AccountingMode == right.AccountingMode
}

// comparisonModelName normalizes a model name for price matching: lowercase,
// drop the provider prefix, strip ignored suffixes repeatedly, then apply the
// configured sync mappings. Mirrors the router's own comparison logic.
func comparisonModelName(value string, suffixes []string, mappings map[string]string) string {
	value = normalizeCatalogName(value)
	value = stripIgnoredSuffixes(value, suffixes)
	if mappings != nil {
		if target, ok := mappings[value]; ok {
			value = target
		}
	}
	return stripIgnoredSuffixes(value, suffixes)
}

func normalizeCatalogName(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	parts := strings.Split(value, "/")
	return strings.TrimSpace(parts[len(parts)-1])
}

func stripIgnoredSuffixes(value string, suffixes []string) string {
	if len(suffixes) == 0 {
		return value
	}
	for {
		previous := value
		for _, suffix := range suffixes {
			s := strings.ToLower(strings.TrimSpace(suffix))
			if s == "" {
				continue
			}
			if strings.HasSuffix(value, s) {
				value = strings.TrimSpace(strings.TrimSuffix(value, s))
				break
			}
		}
		if value == previous {
			return value
		}
	}
}

func sortStrings(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}

// defaultAccountingMode infers how input tokens relate to cached tokens when a
// record does not carry an explicit mode. Mirrors the router's heuristic.
func defaultAccountingMode(provider, executor string) string {
	normalizedProvider := strings.ToLower(strings.TrimSpace(provider))
	normalizedExecutor := strings.ToLower(strings.TrimSpace(executor))
	value := strings.TrimSpace(normalizedProvider + " " + normalizedExecutor)
	if normalizedExecutor == "openaicompatexecutor" ||
		normalizedProvider == "openai-compatibility" ||
		strings.HasPrefix(normalizedProvider, "openai-compatible-") {
		return accountingModeInputIncludesCache
	}
	if strings.Contains(value, "claude") || strings.Contains(value, "anthropic") {
		return accountingModeInputExcludesCache
	}
	return accountingModeInputIncludesCache
}

// resolveRecordPrice finds the price for a record, trying the provider model
// first and the provider alias second (e.g. "opr/stealth/union-alpha").
func resolveRecordPrice(rec routerRecord, book *priceBook) (modelPrice, bool) {
	price, ok := book.resolve(rec.ProviderModel)
	if ok {
		return price, true
	}
	if rec.ProviderAlias != "" && rec.ProviderAlias != rec.ProviderModel {
		if price, ok := book.resolve(rec.ProviderAlias); ok {
			return price, true
		}
	}
	return modelPrice{}, false
}

// estimateRecordCost returns the estimated USD cost for one record plus the
// source of the price used. priced is false when the model has no known price;
// the caller must not add a zero cost to a "free" bucket in that case.
func estimateRecordCost(rec routerRecord, book *priceBook) (float64, string, bool) {
	price, ok := resolveRecordPrice(rec, book)
	if !ok {
		return 0, "", false
	}

	cacheRead := rec.CacheReadTokens
	if cacheRead == 0 {
		cacheRead = rec.CachedTokens
	}
	cacheCreation := rec.CacheCreationTokens

	mode := price.AccountingMode
	if mode == "" {
		mode = rec.AccountingMode
	}
	if mode == "" {
		mode = defaultAccountingMode(rec.Provider, rec.ExecutorType)
	}

	billableInput := rec.InputTokens
	if mode == accountingModeInputIncludesCache {
		billableInput = rec.InputTokens - cacheRead - cacheCreation
		if billableInput < 0 {
			billableInput = 0
		}
	}

	outputTokens := rec.OutputTokens
	if reasoningMode(rec) == reasoningModeSeparate {
		outputTokens += rec.ReasoningTokens
	}

	cost := tokenCostUSD(billableInput, price.Input) +
		tokenCostUSD(outputTokens, price.Output) +
		tokenCostUSD(cacheRead, price.CacheRead) +
		tokenCostUSD(cacheCreation, price.CacheCreation)
	if cost < 0 {
		cost = 0
	}
	return cost, price.Source, true
}

// round4 keeps estimated costs readable without pretending to more precision
// than a token-rate estimate actually has.
func round4(value float64) float64 {
	return math.Round(value*10000) / 10000
}

func reasoningMode(rec routerRecord) string {
	switch rec.ReasoningMode {
	case reasoningModeIncluded, reasoningModeSeparate:
		return rec.ReasoningMode
	}
	provider := strings.ToLower(strings.TrimSpace(rec.Provider))
	executor := strings.ToLower(strings.TrimSpace(rec.ExecutorType))
	value := provider + " " + executor
	for _, marker := range []string{"google", "gemini", "aistudio", "antigravity", "vertex", "interaction"} {
		if strings.Contains(value, marker) {
			return reasoningModeSeparate
		}
	}
	return reasoningModeIncluded
}

func tokenCostUSD(tokens int64, perMillion float64) float64 {
	if tokens <= 0 || perMillion <= 0 {
		return 0
	}
	return float64(tokens) * perMillion / 1_000_000
}
