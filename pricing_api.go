package main

// Management endpoints for the estimated-cost layer.
//
// GET  /plugins/usage-statistics/pricing -> the COMPLETE price catalogue:
//      every model in the router price book, every model ever seen in traffic,
//      and every manual override — deduplicated. Usage counters are scoped to
//      the selected range, but the model LIST is not: a price is a property of
//      the model, not of whether it happened to run in the last hour.
//
//      Filtering the list by the range was wrong twice over: the tab looked
//      empty during a quiet window while its own header advertised 69 prices,
//      and a model that last ran outside the window could not be priced at all.
//
// PUT  /plugins/usage-statistics/pricing -> save manual price overrides into
//      usage.db. Overrides always win over the router price book, and the
//      router DB itself is never written to.

import (
	"context"
	"fmt"
	"encoding/json"
	"sort"
	"strings"
	"time"
)

// priceGroup collects every spelling of one model (the bare name plus its
// provider-prefixed variants). The group only merges into a single row when all
// spellings agree on a price; otherwise the members stay separate so each keeps
// its own rate.
type priceGroup struct {
	key   string
	names []string // every spelling seen, in insertion order
}

func (g *priceGroup) add(name, src string) {
	for _, n := range g.names {
		if strings.EqualFold(n, name) {
			return
		}
	}
	g.names = append(g.names, name)
}

// fingerprint reduces a book price to a comparable string, or "" when unknown.
func (g *priceGroup) fingerprint(book *priceBook, name string) string {
	price, ok := book.bookPrice(name)
	if !ok {
		return ""
	}
	return fmt.Sprintf("%.6f|%.6f|%.6f|%.6f", price.Input, price.Output, price.CacheRead, price.CacheCreation)
}

// displayName picks the name to file the price under: the shortest spelling,
// which is the bare model name rather than a provider-prefixed variant.
func (g *priceGroup) displayName() string {
	best := ""
	for _, n := range g.names {
		if best == "" || len(n) < len(best) || (len(n) == len(best) && n < best) {
			best = n
		}
	}
	return best
}

type pricingModelRow struct {
	Model           string       `json:"model"`
	AlsoKnownAs     []string     `json:"also_known_as,omitempty"` // merged provider-prefixed spellings
	Calls           int64        `json:"calls"`           // calls inside the selected range
	LifetimeCalls   int64        `json:"lifetime_calls"`  // calls ever recorded
	LastSeen        string       `json:"last_seen,omitempty"`
	InRange         bool         `json:"in_range"`        // ran inside the selected range
	InBook          bool         `json:"in_book"`         // present in the router price book
	PricedCalls     int64        `json:"priced_calls"`
	UnpricedCalls   int64        `json:"unpriced_calls"`
	InputTokens     int64        `json:"input_tokens"`
	OutputTokens    int64        `json:"output_tokens"`
	CacheReadTokens int64        `json:"cache_read_tokens"`
	CostUSD         float64      `json:"cost_usd"`
	Price           *modelPrice  `json:"price,omitempty"`
	PriceSource     string       `json:"price_source"` // manual-override | router source | none
	Override        *priceOver   `json:"override,omitempty"`
}

type priceOver struct {
	Model          string  `json:"model"`
	Input          float64 `json:"input"`
	Output         float64 `json:"output"`
	CacheRead      float64 `json:"cache_read"`
	CacheCreation  float64 `json:"cache_creation"`
	AccountingMode string  `json:"accounting_mode,omitempty"`
	UpdatedAt      string  `json:"updated_at,omitempty"`
}

type pricingResponse struct {
	Available   bool              `json:"available"`
	BookSource  string            `json:"book_source"`
	BookEntries int               `json:"book_entries"`
	Revision    uint64            `json:"revision"`
	SyncedAt    string            `json:"synced_at,omitempty"`
	SyncedBy    string            `json:"synced_by,omitempty"`
	Overrides   int               `json:"overrides"`
	Total       int               `json:"total"`        // rows listed (grouped)
	RawTotal    int               `json:"raw_total"`    // rows before grouping
	InRange     int               `json:"in_range"`     // of those, how many ran in the range
	TrafficOnly int               `json:"traffic_only"` // seen in traffic but absent from the book
	Models      []pricingModelRow `json:"models"`
	Note        string            `json:"note"`
}

type savePriceOverridesRequest struct {
	Overrides []priceOver `json:"overrides"`
}

// pricingGet renders the price catalogue. One full pass over the traffic gives
// both the in-range counters and the lifetime last-seen, so the list stays
// complete without a second expensive scan.
func pricingGet(req managementRequest) ([]byte, error) {
	rng, filter := parsePageFilter(req.Query)
	ctx, cancel := context.WithTimeout(context.Background(), insertTimeout)
	defer cancel()

	store, err := newRouterStore()
	if err != nil {
		return okEnvelope(jsonManagementResponse(200, pricingResponse{
			Available:  false,
			BookSource: "none",
			Models:     []pricingModelRow{},
			Note:       "Không tìm thấy model-router.db; chưa có bảng giá để tính chi phí.",
		}))
	}
	defer store.close()

	book := store.loadEffectivePriceBook(ctx)
	overrides := book.overrides

	type acc struct {
		calls       int64
		priced      int64
		unpriced    int64
		in, out, cr int64
		cost        float64
		price       *modelPrice
	}
	byModel := map[string]*acc{}

	// Range-scoped usage counters. The model LIST is built separately below.
	err = store.scanRouterRows(ctx, rng.Start, rng.End, func(rec routerRecord) error {
		if !matchRouterRecord(rec, filter) {
			return nil
		}
		name := firstNonEmpty(rec.ProviderModel, "unknown")
		a, ok := byModel[name]
		if !ok {
			a = &acc{}
			byModel[name] = a
		}
		a.calls++
		a.in += rec.InputTokens
		a.out += rec.OutputTokens
		cr := rec.CacheReadTokens
		if cr == 0 {
			cr = rec.CachedTokens
		}
		a.cr += cr
		cost, _, priced := estimateRecordCost(rec, book)
		if priced {
			a.priced++
			a.cost += cost
			if a.price == nil {
				if price, ok := resolveRecordPrice(rec, book); ok {
					p := price
					a.price = &p
				}
			}
		} else {
			a.unpriced++
		}
		return nil
	})
	if err != nil {
		return okEnvelope(jsonManagementResponse(500, map[string]string{"error": "router db read failed"}))
	}

	// Lifetime view: which models exist at all, and when each last ran. Without
	// this a model that has not run inside the selected range is invisible and
	// therefore impossible to price.
	idx, err := store.scanRouterModelIndex(ctx)
	if err != nil {
		return okEnvelope(jsonManagementResponse(500, map[string]string{"error": "router db read failed"}))
	}

	// The catalogue is the UNION of the price book, every model ever seen in
	// traffic, and the manual overrides.
	//
	// Entries that are the same model reached through different providers
	// ("vsl/glm-5.3" and "glm-5.3") collapse into ONE row, because a price is a
	// property of the model, not of the route it arrived on. They are only
	// merged when their prices AGREE: providers do quote different rates for the
	// same base model (glm-5.3 is $1/$4 on one and $1.4/$4.4 on another), and
	// merging those would silently bill one provider's traffic at the other's
	// rate. Ambiguous groups stay as separate rows.
	groups := map[string]*priceGroup{}
	order := []string{}

	add := func(n string, src string) {
		n = strings.TrimSpace(n)
		if n == "" {
			return
		}
		key := book.compareKey(n)
		if key == "" {
			key = strings.ToLower(n)
		}
		g, ok := groups[key]
		if !ok {
			g = &priceGroup{key: key}
			groups[key] = g
			order = append(order, key)
		}
		g.add(n, src)
	}
	for name := range book.exact {
		add(name, "book")
	}
	for _, display := range idx.display {
		add(display, "traffic")
	}
	for name := range byModel {
		add(name, "traffic")
	}
	for _, ov := range overrides {
		add(ov.Model, "override")
	}
	sort.Strings(order)

	resp := pricingResponse{
		Available:   book.Count > 0 || len(overrides) > 0,
		BookSource:  "model-router",
		BookEntries: book.Count,
		Revision:    book.Revision,
		SyncedAt:    book.SyncedAt,
		SyncedBy:    book.SyncedBy,
		Overrides:   len(overrides),
		Models:      []pricingModelRow{},
		Note:        "Danh sách gồm toàn bộ bảng giá, mọi model từng chạy và giá nhập tay. Số lượt gọi tính theo khoảng thời gian đã chọn. Model không có giá hiện 'chưa có giá', không tính là miễn phí.",
	}

	for _, key := range order {
		g := groups[key]

		// Decide whether the group can collapse into one row. Every spelling
		// must agree on a price; a disagreement (two providers quoting
		// different rates for the same base model) keeps them apart so no
		// traffic is silently billed at the wrong rate.
		merged := []string{g.displayName()}
		if len(g.names) > 1 {
			seen := map[string]bool{}
			consistent := true
			for _, n := range g.names {
				fp := g.fingerprint(book, n)
				if fp == "" {
					continue // unpriced spellings never block a merge
				}
				if len(seen) > 0 && !seen[fp] {
					consistent = false
					break
				}
				seen[fp] = true
			}
			if consistent {
				merged = []string{g.displayName()}
			} else {
				// Ambiguous: keep every spelling as its own row, ordered so the
				// bare name (if any) comes first.
				merged = append([]string{}, g.names...)
				sort.Slice(merged, func(i, j int) bool {
					pi, pj := strings.Contains(merged[i], "/"), strings.Contains(merged[j], "/")
					if pi != pj {
						return !pi
					}
					return merged[i] < merged[j]
				})
			}
		}

		for mi, name := range merged {
			row := pricingModelRow{
				Model:         name,
				LifetimeCalls: idx.counts[strings.ToLower(name)],
				LastSeen:      idx.lastSeen[strings.ToLower(name)],
				PriceSource:   "none",
			}
			// InBook means "the router price book has an entry", NOT "a price
			// resolves" — resolve() also consults manual overrides.
			row.InBook = book.hasBookEntry(name)
			if a, ok := byModel[name]; ok {
				row.InRange = true
				row.Calls = a.calls
				row.PricedCalls = a.priced
				row.UnpricedCalls = a.unpriced
				row.InputTokens = a.in
				row.OutputTokens = a.out
				row.CacheReadTokens = a.cr
				row.CostUSD = round4(a.cost)
				if a.price != nil {
					pr := *a.price
					row.Price = &pr
					row.PriceSource = firstNonEmpty(pr.Source, "router")
				}
			}
			// A model that never ran in the range still needs a price shown.
			if row.Price == nil {
				if price, ok := book.resolve(name); ok {
					pr := price
					row.Price = &pr
					row.PriceSource = firstNonEmpty(pr.Source, "router")
				}
			}
			if ov, ok := overrides[strings.ToLower(name)]; ok {
				o := ov
				row.Override = &o
			}
			// Show the other spellings so the user knows what was merged in.
			if len(merged) == 1 {
				for _, n := range g.names {
					if !strings.EqualFold(n, name) {
						row.AlsoKnownAs = append(row.AlsoKnownAs, n)
					}
				}
			}
			if !row.InBook && row.Price == nil {
				resp.TrafficOnly++
			}
			if row.InRange {
				resp.InRange++
			}
			resp.Models = append(resp.Models, row)
			_ = mi
		}
	}
	resp.Total = len(resp.Models)
	resp.RawTotal = len(groups)
	if resp.Total > resp.RawTotal {
		resp.RawTotal = resp.Total
	}
	return okEnvelope(jsonManagementResponse(200, resp))
}

// pricingPut persists manual overrides. The full list is authoritative: models
// left out are removed, so the dialog can also clear a price.
func pricingPut(req managementRequest) ([]byte, error) {
	var input savePriceOverridesRequest
	if err := json.Unmarshal(req.Body, &input); err != nil {
		return okEnvelope(jsonManagementResponse(400, map[string]string{"error": "invalid json body"}))
	}
	store := currentStore()
	if store == nil {
		return okEnvelope(jsonManagementResponse(503, map[string]string{"error": "usage store unavailable"}))
	}
	ctx, cancel := context.WithTimeout(context.Background(), insertTimeout)
	defer cancel()

	cleaned := make([]priceOver, 0, len(input.Overrides))
	seen := map[string]bool{}
	for _, ov := range input.Overrides {
		model := strings.TrimSpace(ov.Model)
		if model == "" || seen[strings.ToLower(model)] {
			continue
		}
		if ov.Input < 0 || ov.Output < 0 || ov.CacheRead < 0 || ov.CacheCreation < 0 {
			return okEnvelope(jsonManagementResponse(400, map[string]string{"error": "price must not be negative"}))
		}
		seen[strings.ToLower(model)] = true
		ov.Model = model
		ov.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
		cleaned = append(cleaned, ov)
	}
	if err := store.SavePriceOverrides(ctx, cleaned); err != nil {
		return okEnvelope(jsonManagementResponse(500, map[string]string{"error": "failed to save prices"}))
	}
	return okEnvelope(jsonManagementResponse(200, map[string]any{
		"saved": len(cleaned),
	}))
}
