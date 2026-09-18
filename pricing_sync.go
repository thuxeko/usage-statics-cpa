package main

// models.dev price lookup for the Sync dialog.
//
// The catalog is fetched by the CPA backend (the browser cannot: CORS), cached
// in memory, and turned into a *candidate list* per model. Nothing is applied
// automatically: the user picks a candidate in the dialog and the choice is
// saved as a manual override, so a wrong or zero-price match can never silently
// rewrite the estimated cost.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	modelsDevCatalogURL = "https://models.dev/api.json"
	modelsDevTimeout    = 25 * time.Second
	modelsDevMaxBytes   = 32 << 20
	modelsDevCacheTTL   = 30 * time.Minute
)

type modelsDevCost struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cache_read"`
	CacheWrite float64 `json:"cache_write"`
}

type modelsDevModel struct {
	ID   string        `json:"id"`
	Name string        `json:"name"`
	Cost *modelsDevCost `json:"cost"`
}

type modelsDevProvider struct {
	ID     string                    `json:"id"`
	Name   string                    `json:"name"`
	Models map[string]modelsDevModel `json:"models"`
}

// priceCandidate is one selectable models.dev price for a model.
type priceCandidate struct {
	Provider      string  `json:"provider"`
	CatalogModel  string  `json:"catalog_model"`
	Input         float64 `json:"input"`
	Output        float64 `json:"output"`
	CacheRead     float64 `json:"cache_read"`
	CacheCreation float64 `json:"cache_creation"`
	ZeroPrice     bool    `json:"zero_price"`
}

type candidateGroup struct {
	Model      string           `json:"model"`
	Candidates []priceCandidate `json:"candidates"`
}

var (
	modelsDevMu      sync.Mutex
	modelsDevCache   []candidateGroup // keyed by normalized model name
	modelsDevFetched time.Time
	modelsDevErr     string
)

// fetchModelsDevCatalog downloads and indexes models.dev by normalized model id.
func fetchModelsDevCatalog(ctx context.Context) (map[string][]priceCandidate, error) {
	modelsDevMu.Lock()
	if time.Since(modelsDevFetched) < modelsDevCacheTTL && modelsDevCache != nil {
		cached := modelsDevCache
		modelsDevMu.Unlock()
		return indexCandidates(cached), nil
	}
	modelsDevMu.Unlock()

	client := &http.Client{Timeout: modelsDevTimeout}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, modelsDevCatalogURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "usage-statistics-plugin")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, modelsDevMaxBytes))
	if err != nil {
		return nil, err
	}

	var catalog map[string]modelsDevProvider
	if err := json.Unmarshal(body, &catalog); err != nil {
		return nil, err
	}

	grouped := map[string][]priceCandidate{}
	for providerID, provider := range catalog {
		for _, model := range provider.Models {
			if model.Cost == nil {
				continue
			}
			name := strings.TrimSpace(model.ID)
			if name == "" {
				name = model.Name
			}
			key := normalizeCatalogName(name)
			if key == "" {
				continue
			}
			grouped[key] = append(grouped[key], priceCandidate{
				Provider:      providerID,
				CatalogModel:  name,
				Input:         model.Cost.Input,
				Output:        model.Cost.Output,
				CacheRead:     model.Cost.CacheRead,
				CacheCreation: model.Cost.CacheWrite,
				ZeroPrice:     model.Cost.Input == 0 && model.Cost.Output == 0,
			})
		}
	}

	list := make([]candidateGroup, 0, len(grouped))
	for key, candidates := range grouped {
		sort.Slice(candidates, func(i, j int) bool {
			if candidates[i].Input != candidates[j].Input {
				return candidates[i].Input < candidates[j].Input
			}
			return candidates[i].Provider < candidates[j].Provider
		})
		list = append(list, candidateGroup{Model: key, Candidates: candidates})
	}

	modelsDevMu.Lock()
	modelsDevCache = list
	modelsDevFetched = time.Now()
	modelsDevErr = ""
	modelsDevMu.Unlock()
	return grouped, nil
}

func indexCandidates(list []candidateGroup) map[string][]priceCandidate {
	out := make(map[string][]priceCandidate, len(list))
	for _, group := range list {
		out[group.Model] = group.Candidates
	}
	return out
}

// candidatesFor finds the models.dev entries for one observed model name. It
// tries the bare name first, then the name with ignored suffixes stripped.
func candidatesFor(model string, index map[string][]priceCandidate) []priceCandidate {
	if index == nil {
		return nil
	}
	key := normalizeCatalogName(model)
	if candidates, ok := index[key]; ok {
		return candidates
	}
	stripped := stripIgnoredSuffixes(key, defaultIgnoredSuffixes())
	if candidates, ok := index[stripped]; ok {
		return candidates
	}
	return nil
}

func defaultIgnoredSuffixes() []string {
	return []string{"-thinking", "-preview", "-xhigh", "-high", "-low", "(thinking)", "(xhigh)", "(high)", "(low)"}
}

// pricingSyncGet returns, for every model seen in the range, the models.dev
// candidates the user can pick from. It never writes anything.
func pricingSyncGet(req managementRequest) ([]byte, error) {
	rng, filter := parsePageFilter(req.Query)
	ctx, cancel := context.WithTimeout(context.Background(), modelsDevTimeout+5*time.Second)
	defer cancel()

	seen := map[string]int64{}
	if store, err := newRouterStore(); err == nil {
		defer store.close()
		_ = store.scanRouterRows(ctx, rng.Start, rng.End, func(rec routerRecord) error {
			if !matchRouterRecord(rec, filter) {
				return nil
			}
			seen[firstNonEmpty(rec.ProviderModel, "unknown")]++
			return nil
		})
	}
	// Models already in the price book but idle in this range are included too,
	// so the dialog can fix a price before it is used again.
	if store, err := newRouterStore(); err == nil {
		book := store.loadEffectivePriceBook(ctx)
		for name := range book.exact {
			if _, ok := seen[name]; !ok {
				seen[name] = 0
			}
		}
		store.close()
	}

	index, err := fetchModelsDevCatalog(ctx)
	if err != nil {
		return okEnvelope(jsonManagementResponse(200, map[string]any{
			"available": false,
			"error":     "models.dev không truy cập được: " + err.Error(),
			"groups":    []candidateGroup{},
		}))
	}

	groups := make([]candidateGroup, 0, len(seen))
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		groups = append(groups, candidateGroup{
			Model:      name,
			Candidates: candidatesFor(name, index),
		})
	}
	return okEnvelope(jsonManagementResponse(200, map[string]any{
		"available": true,
		"source":    "models.dev",
		"groups":    groups,
	}))
}
