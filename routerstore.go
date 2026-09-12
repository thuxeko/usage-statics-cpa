package main

// Read-only access to the model-router plugin's SQLite database
// (/CLIProxyPlugins/plugins/model-router.db — same plugins dir as this
// library). CPA delivers usage records for routed/direct traffic to the
// model-router plugin (single-consumer delivery), so its DB is the complete
// realtime source. We open it read-only via URI mode=ro and map its JSON
// payloads onto the rich 4-view dashboard aggregates.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const routerDBFile = "model-router.db"

// routerRecord mirrors the JSON payload rows model-router stores in its
// requests table (verified against v0.5.0 payloads).
type routerRecord struct {
	Sequence            int64  `json:"sequence"`
	RequestedAt         string `json:"requested_at"`
	Attribution         string `json:"attribution"`
	RouterModel         string `json:"router_model"`
	Provider            string `json:"provider"`
	ExecutorType        string `json:"executor_type"`
	ProviderModel       string `json:"provider_model"`
	ProviderAlias       string `json:"provider_alias"`
	Source              string `json:"source"`
	ReasoningEffort     string `json:"reasoning_effort"`
	ServiceTier         string `json:"service_tier"`
	MaskedAPIKey        string `json:"masked_api_key"`
	Failed              bool   `json:"failed"`
	StatusCode          int    `json:"status_code"`
	LatencyNS           int64  `json:"latency_ns"`
	TTFTNS              int64  `json:"ttft_ns"`
	InputTokens         int64  `json:"input_tokens"`
	OutputTokens        int64  `json:"output_tokens"`
	ReasoningTokens     int64  `json:"reasoning_tokens"`
	CachedTokens        int64  `json:"cached_tokens"`
	CacheReadTokens     int64  `json:"cache_read_tokens"`
	CacheCreationTokens int64  `json:"cache_creation_tokens"`
	TotalTokens         int64  `json:"total_tokens"`
}

// routerStore is a lazy read-only handle to the model-router database.
type routerStore struct {
	path string
	db   *sql.DB
}

func newRouterStore() (*routerStore, error) {
	dir := pluginLibraryDir()
	if dir == "" {
		dir = "plugins"
	}
	path := filepath.Join(dir, routerDBFile)
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("router db not found at %s: %w", path, err)
	}
	return &routerStore{path: path}, nil
}

func (r *routerStore) connect() (*sql.DB, error) {
	if r.db != nil {
		return r.db, nil
	}
	// mode=ro: never locks the writer.
	db, err := sql.Open("sqlite", "file:"+r.path+"?mode=ro&_pragma=busy_timeout(3000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	r.db = db
	return db, nil
}

func (r *routerStore) close() {
	if r.db != nil {
		_ = r.db.Close()
		r.db = nil
	}
}

func pluginLibraryDir() string {
	dirIfDB := func(dir string) string {
		if dir == "" {
			return ""
		}
		if _, err := os.Stat(filepath.Join(dir, routerDBFile)); err == nil {
			return dir
		}
		return ""
	}
	candidates := []string{
		filepath.Join("plugins"),
		".",
		"/CLIProxyAPI/plugins",
	}
	for _, dir := range candidates {
		if dir := dirIfDB(dir); dir != "" {
			return dir
		}
	}
	if maps, err := os.ReadFile("/proc/self/maps"); err == nil {
		for _, line := range strings.Split(string(maps), "\n") {
			parts := strings.Fields(line)
			if len(parts) >= 6 && strings.HasSuffix(parts[5], "usage-statistics.so") {
				if d := dirIfDB(filepath.Dir(parts[5])); d != "" {
					return d
				}
			}
		}
	}
	return ""
}

func (r *routerStore) scanRouterRows(ctx context.Context, start, end *time.Time, fn func(routerRecord) error) error {
	db, err := r.connect()
	if err != nil {
		return err
	}
	var startNS, endNS int64
	if start != nil {
		startNS = start.UnixNano()
	}
	if end != nil {
		endNS = end.UnixNano()
	}
	query := `SELECT payload FROM requests`
	var args []any
	switch {
	case start != nil && end != nil:
		query += ` WHERE requested_at_ns >= ? AND requested_at_ns < ?`
		args = []any{startNS, endNS}
	case start != nil:
		query += ` WHERE requested_at_ns >= ?`
		args = []any{startNS}
	case end != nil:
		query += ` WHERE requested_at_ns < ?`
		args = []any{endNS}
	}
	query += ` ORDER BY requested_at_ns ASC`
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("router db query: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			return fmt.Errorf("router db scan: %w", err)
		}
		var rec routerRecord
		if err := json.Unmarshal(payload, &rec); err != nil {
			continue
		}
		if err := fn(rec); err != nil {
			return err
		}
	}
	return rows.Err()
}

// matchRouterRecord checks if a router record passes the given PageFilter.
func matchRouterRecord(rec routerRecord, filter PageFilter) bool {
	if filter.Model != "" &&
		rec.ProviderModel != filter.Model &&
		rec.ProviderAlias != filter.Model &&
		rec.RouterModel != filter.Model {
		return false
	}
	if filter.Alias != "" && rec.RouterModel != filter.Alias && (filter.Alias != "Direct / No Router" || rec.RouterModel != "") {
		return false
	}
	if filter.Provider != "" && rec.Provider != filter.Provider {
		return false
	}
	if filter.APIKey != "" && rec.MaskedAPIKey != filter.APIKey && rec.Provider != filter.APIKey {
		return false
	}
	if filter.Attribution != "" && !strings.EqualFold(rec.Attribution, filter.Attribution) {
		return false
	}
	if filter.ReasoningEffort != "" {
		if filter.ReasoningEffort == "Not Specified" {
			if rec.ReasoningEffort != "" {
				return false
			}
		} else if !strings.EqualFold(rec.ReasoningEffort, filter.ReasoningEffort) {
			return false
		}
	}
	if filter.Executor != "" && !strings.EqualFold(rec.ExecutorType, filter.Executor) {
		return false
	}
	if filter.ServiceTier != "" && !strings.EqualFold(rec.ServiceTier, filter.ServiceTier) {
		return false
	}
	if filter.StatusCode > 0 && rec.StatusCode != filter.StatusCode {
		return false
	}
	if filter.CachedOnly {
		cached := rec.CachedTokens
		if cached == 0 {
			cached = rec.CacheReadTokens
		}
		if cached <= 0 {
			return false
		}
	}
	if filter.Failed != nil && rec.Failed != *filter.Failed {
		return false
	}
	return true
}

// RouterSummary computes the complete 4-view UsageSummary shape.
func (r *routerStore) RouterSummary(ctx context.Context, rng QueryRange, filter ...PageFilter) (*UsageSummary, error) {
	summary := &UsageSummary{
		Start:               rng.Start,
		End:                 rng.End,
		ByHour:              []HourStat{},
		ByModel:             []GroupStat{},
		ByAlias:             []GroupStat{},
		ByProvider:          []GroupStat{},
		ByAPIKey:            []GroupStat{},
		ByAttribution:       []GroupStat{},
		ByEffort:            []GroupStat{},
		ByExecutor:          []GroupStat{},
		ByPrefix:            []GroupStat{},
		StatusCodes:         []StatusCodeStat{},
		RouterMappings:      []RouterMapping{},
		LatencyDistribution: []DistributionBucket{},
		TokenDistribution:   []DistributionBucket{},
		ContextHistogram:    []DistributionBucket{},
		ScatterPoints:       []ScatterPoint{},
		SlowestRequests:     []RequestDetail{},
	}
	if r == nil {
		return summary, nil
	}

	var activeFilter PageFilter
	if len(filter) > 0 {
		activeFilter = filter[0]
	}

	models := map[string]*enhancedGroupAcc{}
	aliases := map[string]*enhancedGroupAcc{}
	aliasSubs := map[string]map[string]int64{}
	providers := map[string]*enhancedGroupAcc{}
	keys := map[string]*enhancedGroupAcc{}
	attributions := map[string]*enhancedGroupAcc{}
	efforts := map[string]*enhancedGroupAcc{}
	executors := map[string]*enhancedGroupAcc{}
	prefixes := map[string]*enhancedGroupAcc{}
	hours := map[string]*richHourAcc{}
	codes := map[int]int64{}
	routerMaps := map[string]map[string]int64{}

	latBuckets := map[string]int64{
		"< 1s":    0,
		"1 - 3s":  0,
		"3 - 5s":  0,
		"5 - 10s": 0,
		"10 - 30s": 0,
		"30 - 60s": 0,
		"1 - 5m":  0,
		"5 - 30m": 0,
		"> 30m":   0,
	}

	tokBuckets := map[string]int64{
		"< 1k":       0,
		"1k - 10k":   0,
		"10k - 50k":  0,
		"50k - 100k": 0,
		"100k - 200k": 0,
		"200k - 400k": 0,
		"> 400k":      0,
	}

	var allSlowest []routerRecord
	var allRecordsCount int64
	var totals enhancedGroupAcc
	var scatterPoints []ScatterPoint

	err := r.scanRouterRows(ctx, rng.Start, rng.End, func(rec routerRecord) error {
		if !matchRouterRecord(rec, activeFilter) {
			return nil
		}
		allRecordsCount++
		latS := float64(rec.LatencyNS) / 1e9
		ttftS := float64(rec.TTFTNS) / 1e9
		latMs := rec.LatencyNS / int64(time.Millisecond)
		ttftMs := rec.TTFTNS / int64(time.Millisecond)

		cached := rec.CachedTokens
		if cached == 0 && rec.CacheReadTokens > 0 {
			cached = rec.CacheReadTokens
		}

		totals.add(rec.Failed, latMs, ttftMs, latS, ttftS, rec.InputTokens, rec.OutputTokens, rec.ReasoningTokens, cached, rec.TotalTokens, rec.StatusCode)

		modelName := firstNonEmpty(rec.ProviderModel, "unknown")
		enhancedAccFor(models, modelName).add(rec.Failed, latMs, ttftMs, latS, ttftS, rec.InputTokens, rec.OutputTokens, rec.ReasoningTokens, cached, rec.TotalTokens, rec.StatusCode)

		aliasName := firstNonEmpty(rec.RouterModel, "Direct / No Router")
		enhancedAccFor(aliases, aliasName).add(rec.Failed, latMs, ttftMs, latS, ttftS, rec.InputTokens, rec.OutputTokens, rec.ReasoningTokens, cached, rec.TotalTokens, rec.StatusCode)
		if _, ok := aliasSubs[aliasName]; !ok {
			aliasSubs[aliasName] = map[string]int64{}
		}
		aliasSubs[aliasName][modelName]++

		providerName := firstNonEmpty(rec.Provider, "unknown")
		enhancedAccFor(providers, providerName).add(rec.Failed, latMs, ttftMs, latS, ttftS, rec.InputTokens, rec.OutputTokens, rec.ReasoningTokens, cached, rec.TotalTokens, rec.StatusCode)

		keyName := firstNonEmpty(rec.MaskedAPIKey, rec.Provider, "unknown")
		enhancedAccFor(keys, keyName).add(rec.Failed, latMs, ttftMs, latS, ttftS, rec.InputTokens, rec.OutputTokens, rec.ReasoningTokens, cached, rec.TotalTokens, rec.StatusCode)

		attrName := firstNonEmpty(rec.Attribution, "unattributed")
		enhancedAccFor(attributions, attrName).add(rec.Failed, latMs, ttftMs, latS, ttftS, rec.InputTokens, rec.OutputTokens, rec.ReasoningTokens, cached, rec.TotalTokens, rec.StatusCode)

		effortName := firstNonEmpty(rec.ReasoningEffort, "Not Specified")
		enhancedAccFor(efforts, effortName).add(rec.Failed, latMs, ttftMs, latS, ttftS, rec.InputTokens, rec.OutputTokens, rec.ReasoningTokens, cached, rec.TotalTokens, rec.StatusCode)

		executorName := firstNonEmpty(rec.ExecutorType, "unknown")
		enhancedAccFor(executors, executorName).add(rec.Failed, latMs, ttftMs, latS, ttftS, rec.InputTokens, rec.OutputTokens, rec.ReasoningTokens, cached, rec.TotalTokens, rec.StatusCode)

		prefix := "none"
		if rec.ProviderAlias != "" && strings.Contains(rec.ProviderAlias, "/") {
			prefix = strings.Split(rec.ProviderAlias, "/")[0] + "/"
		} else if rec.Provider != "" {
			prefix = rec.Provider + "/"
		}
		enhancedAccFor(prefixes, prefix).add(rec.Failed, latMs, ttftMs, latS, ttftS, rec.InputTokens, rec.OutputTokens, rec.ReasoningTokens, cached, rec.TotalTokens, rec.StatusCode)

		if rec.RouterModel != "" {
			if _, ok := routerMaps[rec.RouterModel]; !ok {
				routerMaps[rec.RouterModel] = map[string]int64{}
			}
			routerMaps[rec.RouterModel][modelName]++
		}

		// Latency Distribution
		switch {
		case latS < 1:
			latBuckets["< 1s"]++
		case latS < 3:
			latBuckets["1 - 3s"]++
		case latS < 5:
			latBuckets["3 - 5s"]++
		case latS < 10:
			latBuckets["5 - 10s"]++
		case latS < 30:
			latBuckets["10 - 30s"]++
		case latS < 60:
			latBuckets["30 - 60s"]++
		case latS < 300:
			latBuckets["1 - 5m"]++
		case latS < 1800:
			latBuckets["5 - 30m"]++
		default:
			latBuckets["> 30m"]++
		}

		// Token Distribution
		inTok := rec.InputTokens
		switch {
		case inTok < 1000:
			tokBuckets["< 1k"]++
		case inTok < 10000:
			tokBuckets["1k - 10k"]++
		case inTok < 50000:
			tokBuckets["10k - 50k"]++
		case inTok < 100000:
			tokBuckets["50k - 100k"]++
		case inTok < 200000:
			tokBuckets["100k - 200k"]++
		case inTok < 400000:
			tokBuckets["200k - 400k"]++
		default:
			tokBuckets["> 400k"]++
		}

		// Scatter points sample (take up to 150 points for chart)
		if len(scatterPoints) < 150 {
			if latS > 0 || ttftS > 0 {
				scatterPoints = append(scatterPoints, ScatterPoint{
					LatencyS: round2(latS),
					TTFTS:    round2(ttftS),
					Model:    modelName,
					Provider: providerName,
					Failed:   rec.Failed,
				})
			}
		}

		// Slowest requests collection
		if len(allSlowest) < 30 || rec.LatencyNS > allSlowest[len(allSlowest)-1].LatencyNS {
			allSlowest = append(allSlowest, rec)
			sort.Slice(allSlowest, func(i, j int) bool {
				return allSlowest[i].LatencyNS > allSlowest[j].LatencyNS
			})
			if len(allSlowest) > 30 {
				allSlowest = allSlowest[:30]
			}
		}

		// Hourly stats
		hour := rec.RequestedAt
		if len(hour) >= 13 {
			hour = hour[:13]
			richHourAccFor(hours, hour).add(rec.Failed, rec.TotalTokens, rec.InputTokens, rec.OutputTokens, rec.Attribution == "routed", rec.Attribution == "direct", rec.ReasoningEffort == "high" || rec.ReasoningEffort == "max", latS)
		}

		if rec.Failed && rec.StatusCode > 0 {
			codes[rec.StatusCode]++
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	p50Lat := percentile(totals.latS, 0.5)
	p75Lat := percentile(totals.latS, 0.75)
	p90Lat := percentile(totals.latS, 0.9)
	p95Lat := percentile(totals.latS, 0.95)
	p99Lat := percentile(totals.latS, 0.99)
	maxLat := float64(0)
	if len(totals.latS) > 0 {
		maxLat = totals.latS[len(totals.latS)-1]
	}

	p50TTFT := percentile(totals.ttftS, 0.5)
	p90TTFT := percentile(totals.ttftS, 0.9)

	var hungCount int64
	for _, l := range totals.latS {
		if l > 300 {
			hungCount++
		}
	}

	errRate := float64(0)
	if totals.calls > 0 {
		errRate = round2(float64(totals.failed) / float64(totals.calls) * 100)
	}

	summary.Totals = SummaryTotals{
		Calls:               totals.calls,
		Success:             totals.calls - totals.failed,
		Failed:              totals.failed,
		ErrorRate:           errRate,
		InputTokens:         totals.inTok,
		OutputTokens:        totals.outTok,
		ReasoningTokens:     totals.reasoningTok,
		CachedTokens:        totals.cachedTok,
		CacheCreationTokens: 0,
		TotalTokens:         totals.totalTok,
		AvgLatencyMs:        avgOrZero(totals.latSum, totals.calls),
		AvgTTFTMs:           avgOrZero(totals.ttftSum, totals.calls),
		P50LatencyS:         round2(p50Lat),
		P75LatencyS:         round2(p75Lat),
		P90LatencyS:         round2(p90Lat),
		P95LatencyS:         round2(p95Lat),
		P99LatencyS:         round2(p99Lat),
		MaxLatencyS:         round2(maxLat),
		P50TTFTS:            round2(p50TTFT),
		P90TTFTS:            round2(p90TTFT),
		HungCalls:           hungCount,
		CacheHitCalls:       totals.cacheHitCalls,
		ActiveKeys:          int64(len(keys)),
		ActiveModels:        int64(len(models)),
	}

	summary.ByModel = enhancedToStats(models, nil)
	summary.ByAlias = enhancedToStats(aliases, aliasSubs)
	summary.ByProvider = enhancedToStats(providers, nil)
	summary.ByAPIKey = enhancedToStats(keys, nil)
	summary.ByAttribution = enhancedToStats(attributions, nil)
	summary.ByEffort = enhancedToStats(efforts, nil)
	summary.ByExecutor = enhancedToStats(executors, nil)
	summary.ByPrefix = enhancedToStats(prefixes, nil)
	summary.ByHour = richHourAccsToStats(hours)
	summary.ScatterPoints = scatterPoints

	// Slowest requests
	for _, rec := range allSlowest {
		summary.SlowestRequests = append(summary.SlowestRequests, routerToDetail(rec))
	}

	for code, calls := range codes {
		summary.StatusCodes = append(summary.StatusCodes, StatusCodeStat{StatusCode: code, Calls: calls})
	}
	sortStatusCodeStats(summary.StatusCodes)

	for alias, mapModels := range routerMaps {
		for model, calls := range mapModels {
			summary.RouterMappings = append(summary.RouterMappings, RouterMapping{
				Alias: alias,
				Model: model,
				Calls: calls,
			})
		}
	}
	sort.Slice(summary.RouterMappings, func(i, j int) bool {
		if summary.RouterMappings[i].Alias == summary.RouterMappings[j].Alias {
			return summary.RouterMappings[i].Calls > summary.RouterMappings[j].Calls
		}
		return summary.RouterMappings[i].Alias < summary.RouterMappings[j].Alias
	})

	latOrder := []string{"< 1s", "1 - 3s", "3 - 5s", "5 - 10s", "10 - 30s", "30 - 60s", "1 - 5m", "5 - 30m", "> 30m"}
	for _, b := range latOrder {
		summary.LatencyDistribution = append(summary.LatencyDistribution, DistributionBucket{
			Label: b,
			Calls: latBuckets[b],
		})
	}

	tokOrder := []string{"< 1k", "1k - 10k", "10k - 50k", "50k - 100k", "100k - 200k", "200k - 400k", "> 400k"}
	for _, b := range tokOrder {
		summary.TokenDistribution = append(summary.TokenDistribution, DistributionBucket{
			Label: b,
			Calls: tokBuckets[b],
		})
		summary.ContextHistogram = append(summary.ContextHistogram, DistributionBucket{
			Label: b,
			Calls: tokBuckets[b],
		})
	}

	return summary, nil
}

func (r *routerStore) RouterPage(ctx context.Context, rng QueryRange, filter PageFilter) (*UsagePage, error) {
	page := &UsagePage{Limit: filter.Limit, Offset: filter.Offset, Rows: []RequestDetail{}}
	if r == nil {
		return page, nil
	}
	var matched []routerRecord
	err := r.scanRouterRows(ctx, rng.Start, rng.End, func(rec routerRecord) error {
		if !matchRouterRecord(rec, filter) {
			return nil
		}
		matched = append(matched, rec)
		return nil
	})
	if err != nil {
		return nil, err
	}
	page.Total = int64(len(matched))
	for i, j := 0, len(matched)-1; i < j; i, j = i+1, j-1 {
		matched[i], matched[j] = matched[j], matched[i]
	}
	limit := filter.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	page.Limit = limit
	if filter.Offset < 0 {
		filter.Offset = 0
	}
	if filter.Offset >= len(matched) {
		return page, nil
	}
	end := filter.Offset + limit
	if end > len(matched) {
		end = len(matched)
	}
	for _, rec := range matched[filter.Offset:end] {
		detail := routerToDetail(rec)
		page.Rows = append(page.Rows, detail)
	}
	return page, nil
}

func routerToDetail(rec routerRecord) RequestDetail {
	cached := rec.CachedTokens
	if cached == 0 && rec.CacheReadTokens > 0 {
		cached = rec.CacheReadTokens
	}
	return RequestDetail{
		ID:              "mr:" + strconv.FormatInt(rec.Sequence, 10),
		Sequence:        rec.Sequence,
		Timestamp:       parseRouterTimestampOrZero(rec.RequestedAt),
		APIKey:          rec.MaskedAPIKey,
		Provider:        rec.Provider,
		Model:           firstNonEmpty(rec.ProviderModel, rec.ProviderAlias),
		Alias:           firstNonEmpty(rec.RouterModel, "Direct / No Router"),
		Source:          firstNonEmpty(rec.Source, rec.Attribution),
		AuthIndex:       "",
		AuthType:        "",
		ExecutorType:    firstNonEmpty(rec.ExecutorType, ""),
		ReasoningEffort: firstNonEmpty(rec.ReasoningEffort, "Not Specified"),
		ServiceTier:     firstNonEmpty(rec.ServiceTier, "auto"),
		LatencyMs:       rec.LatencyNS / int64(time.Millisecond),
		TTFTMs:          rec.TTFTNS / int64(time.Millisecond),
		Failed:          rec.Failed,
		FailureStatusCode: func() int {
			if rec.StatusCode > 0 {
				return rec.StatusCode
			}
			if rec.Failed {
				return 500
			}
			return 200
		}(),
		Tokens: TokenStats{
			InputTokens:         rec.InputTokens,
			OutputTokens:        rec.OutputTokens,
			ReasoningTokens:     rec.ReasoningTokens,
			CachedTokens:        cached,
			CacheReadTokens:     rec.CacheReadTokens,
			CacheCreationTokens: rec.CacheCreationTokens,
			TotalTokens:         rec.TotalTokens,
		},
	}
}

func parseRouterTimestampOrZero(raw string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(raw))
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}

// ---- enhanced accumulator helpers -------------------------------------------

type enhancedGroupAcc struct {
	calls         int64
	failed        int64
	inTok         int64
	outTok        int64
	reasoningTok  int64
	cachedTok     int64
	cacheHitCalls int64
	totalTok      int64
	latSum        int64
	ttftSum       int64
	latS          []float64
	ttftS         []float64
	errors        map[int]int64
}

func (g *enhancedGroupAcc) add(failed bool, latMs, ttftMs int64, latS, ttftS float64, inTok, outTok, reasoningTok, cachedTok, totalTok int64, statusCode int) {
	g.calls++
	if failed {
		g.failed++
		if statusCode > 0 {
			if g.errors == nil {
				g.errors = map[int]int64{}
			}
			g.errors[statusCode]++
		}
	}
	g.latSum += latMs
	g.ttftSum += ttftMs
	if latS > 0 {
		g.latS = append(g.latS, latS)
	}
	if ttftS > 0 {
		g.ttftS = append(g.ttftS, ttftS)
	}
	g.inTok += inTok
	g.outTok += outTok
	g.reasoningTok += reasoningTok
	g.cachedTok += cachedTok
	if cachedTok > 0 {
		g.cacheHitCalls++
	}
	g.totalTok += totalTok
}

func enhancedAccFor(m map[string]*enhancedGroupAcc, key string) *enhancedGroupAcc {
	g, ok := m[key]
	if !ok {
		g = &enhancedGroupAcc{}
		m[key] = g
	}
	return g
}

func enhancedToStats(m map[string]*enhancedGroupAcc, subMaps map[string]map[string]int64) []GroupStat {
	out := make([]GroupStat, 0, len(m))
	for name, g := range m {
		p50Lat := percentile(g.latS, 0.5)
		p75Lat := percentile(g.latS, 0.75)
		p90Lat := percentile(g.latS, 0.9)
		p95Lat := percentile(g.latS, 0.95)
		p99Lat := percentile(g.latS, 0.99)
		p50TTFT := percentile(g.ttftS, 0.5)
		p90TTFT := percentile(g.ttftS, 0.9)

		topErr := 0
		var maxErrCount int64
		for code, cnt := range g.errors {
			if cnt > maxErrCount {
				topErr, maxErrCount = code, cnt
			}
		}

		sub := ""
		if subMaps != nil {
			if sm, ok := subMaps[name]; ok {
				var bestM string
				var bestC int64
				for mName, c := range sm {
					if c > bestC {
						bestM, bestC = mName, c
					}
				}
				sub = bestM
			}
		}

		errRate := float64(0)
		if g.calls > 0 {
			errRate = round2(float64(g.failed) / float64(g.calls) * 100)
		}

		out = append(out, GroupStat{
			Name:            name,
			Sub:             sub,
			Calls:           g.calls,
			Success:         g.calls - g.failed,
			Failed:          g.failed,
			ErrorRate:       errRate,
			InputTokens:     g.inTok,
			OutputTokens:    g.outTok,
			ReasoningTokens: g.reasoningTok,
			CachedTokens:    g.cachedTok,
			CacheHitCalls:   g.cacheHitCalls,
			TotalTokens:     g.totalTok,
			AvgLatencyMs:    avgOrZero(g.latSum, g.calls),
			AvgTTFTMs:       avgOrZero(g.ttftSum, g.calls),
			P50LatencyS:     round2(p50Lat),
			P75LatencyS:     round2(p75Lat),
			P90LatencyS:     round2(p90Lat),
			P95LatencyS:     round2(p95Lat),
			P99LatencyS:     round2(p99Lat),
			P50TTFTS:        round2(p50TTFT),
			P90TTFTS:        round2(p90TTFT),
			TopError:        topErr,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Calls == out[j].Calls {
			return out[i].Name < out[j].Name
		}
		return out[i].Calls > out[j].Calls
	})
	return out
}

type richHourAcc struct {
	calls        int64
	failed       int64
	totalTokens  int64
	inputTokens  int64
	outputTokens int64
	routed       int64
	direct       int64
	highEffort   int64
	latS         []float64
}

func (h *richHourAcc) add(failed bool, total, in, out int64, isRouted, isDirect, isHighEffort bool, latS float64) {
	h.calls++
	if failed {
		h.failed++
	}
	h.totalTokens += total
	h.inputTokens += in
	h.outputTokens += out
	if isRouted {
		h.routed++
	}
	if isDirect {
		h.direct++
	}
	if isHighEffort {
		h.highEffort++
	}
	if latS > 0 {
		h.latS = append(h.latS, latS)
	}
}

func richHourAccFor(m map[string]*richHourAcc, key string) *richHourAcc {
	h, ok := m[key]
	if !ok {
		h = &richHourAcc{}
		m[key] = h
	}
	return h
}

func richHourAccsToStats(m map[string]*richHourAcc) []HourStat {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]HourStat, 0, len(keys))
	for _, k := range keys {
		h := m[k]
		errRate := float64(0)
		if h.calls > 0 {
			errRate = round2(float64(h.failed) / float64(h.calls) * 100)
		}
		out = append(out, HourStat{
			Hour:         k,
			Calls:        h.calls,
			Success:      h.calls - h.failed,
			Failed:       h.failed,
			ErrorRate:    errRate,
			TotalTokens:  h.totalTokens,
			InputTokens:  h.inputTokens,
			OutputTokens: h.outputTokens,
			RoutedCalls:  h.routed,
			DirectCalls:  h.direct,
			HighEffort:   h.highEffort,
			P50LatencyS:  round2(percentile(h.latS, 0.5)),
			P90LatencyS:  round2(percentile(h.latS, 0.9)),
			P95LatencyS:  round2(percentile(h.latS, 0.95)),
			P99LatencyS:  round2(percentile(h.latS, 0.99)),
		})
	}
	return out
}

func percentile(arr []float64, pct float64) float64 {
	if len(arr) == 0 {
		return 0
	}
	sort.Float64s(arr)
	idx := int(math.Floor(float64(len(arr)) * pct))
	if idx >= len(arr) {
		idx = len(arr) - 1
	}
	return arr[idx]
}

func round2(val float64) float64 {
	return math.Round(val*100) / 100
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func avgOrZero(sum, count int64) float64 {
	if count == 0 {
		return 0
	}
	return float64(sum) / float64(count)
}

func sortStatusCodeStats(s []StatusCodeStat) {
	sort.Slice(s, func(i, j int) bool {
		if s[i].Calls == s[j].Calls {
			return s[i].StatusCode < s[j].StatusCode
		}
		return s[i].Calls > s[j].Calls
	})
}
