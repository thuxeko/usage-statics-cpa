package main

import "time"

// TokenStats holds token accounting for one request, aligned with upstream
// usage.Detail field semantics.
type TokenStats struct {
	InputTokens         int64 `json:"input_tokens"`
	OutputTokens        int64 `json:"output_tokens"`
	ReasoningTokens     int64 `json:"reasoning_tokens"`
	CachedTokens        int64 `json:"cached_tokens"`
	CacheReadTokens     int64 `json:"cache_read_tokens"`
	CacheCreationTokens int64 `json:"cache_creation_tokens"`
	TotalTokens         int64 `json:"total_tokens"`
}

// Record is one persisted usage row. Column names mirror upstream
// usage.Record / usage.Detail naming to avoid ambiguity.
type Record struct {
	ID                string
	Timestamp         time.Time
	APIKey            string
	Provider          string
	Model             string
	Alias             string
	Source            string
	AuthID            string
	AuthIndex         string
	AuthType          string
	ExecutorType      string
	BaseURL           string
	ReasoningEffort   string
	ServiceTier       string
	LatencyMs         int64
	TTFTMs            int64
	Tokens            TokenStats
	Failed            bool
	FailureStatusCode int
	FailureBody       string
}

// RequestDetail is the outward JSON shape returned by the usage API. It omits
// storage-only grouping fields (api_key/model live in the map keys).
type RequestDetail struct {
	ID                string     `json:"id"`
	Sequence          int64      `json:"sequence,omitempty"`
	Timestamp         time.Time  `json:"timestamp"`
	Provider          string     `json:"provider,omitempty"`
	Model             string     `json:"model,omitempty"`
	Alias             string     `json:"alias,omitempty"`
	Source            string     `json:"source"`
	APIKey            string     `json:"api_key,omitempty"`
	AuthID            string     `json:"auth_id,omitempty"`
	AuthIndex         string     `json:"auth_index"`
	AuthType          string     `json:"auth_type,omitempty"`
	ExecutorType      string     `json:"executor_type,omitempty"`
	BaseURL           string     `json:"base_url,omitempty"`
	ProviderLabel     string     `json:"provider_label,omitempty"`
	ReasoningEffort   string     `json:"reasoning_effort"`
	ServiceTier       string     `json:"service_tier"`
	LatencyMs         int64      `json:"latency_ms"`
	TTFTMs            int64      `json:"ttft_ms"`
	Tokens            TokenStats `json:"tokens"`
	Failed            bool       `json:"failed"`
	FailureStatusCode int        `json:"failure_status_code,omitempty"`
	FailureBody       string     `json:"failure_body,omitempty"`
}

// APIUsage groups details by grouping key (api_key or provider) then by model.
type APIUsage map[string]map[string][]RequestDetail

// QueryRange bounds a usage query by timestamp.
type QueryRange struct {
	Start *time.Time
	End   *time.Time
}

// DeleteResult reports the outcome of a delete-by-id request.
type DeleteResult struct {
	Deleted int64    `json:"deleted"`
	Missing []string `json:"missing"`
}

// ---- dashboard aggregation types -------------------------------------------

// SummaryTotals is the overall aggregate for one time range.
type SummaryTotals struct {
	Calls               int64   `json:"calls"`
	Success             int64   `json:"success"`
	Failed              int64   `json:"failed"`
	ErrorRate           float64 `json:"error_rate"`
	InputTokens         int64   `json:"input_tokens"`
	OutputTokens        int64   `json:"output_tokens"`
	ReasoningTokens     int64   `json:"reasoning_tokens"`
	CachedTokens        int64   `json:"cached_tokens"`
	CacheCreationTokens int64   `json:"cache_creation_tokens"`
	TotalTokens         int64   `json:"total_tokens"`
	AvgLatencyMs        float64 `json:"avg_latency_ms"`
	AvgTTFTMs           float64 `json:"avg_ttft_ms"`
	P50LatencyS         float64 `json:"p50_latency_s"`
	P75LatencyS         float64 `json:"p75_latency_s"`
	P90LatencyS         float64 `json:"p90_latency_s"`
	P95LatencyS         float64 `json:"p95_latency_s"`
	P99LatencyS         float64 `json:"p99_latency_s"`
	MaxLatencyS         float64 `json:"max_latency_s"`
	P50TTFTS            float64 `json:"p50_ttft_s"`
	P90TTFTS            float64 `json:"p90_ttft_s"`
	HungCalls           int64   `json:"hung_calls"` // > 300s
	CacheHitCalls       int64   `json:"cache_hit_calls"`
	ActiveKeys          int64   `json:"active_keys"`
	ActiveModels        int64   `json:"active_models"`

	// Estimated cost. Priced calls are those whose model resolved to a price in
	// the router price book; unpriced calls have no known price and are NEVER
	// counted as free.
	CostUSD           float64 `json:"cost_usd"`
	CostPricedCalls   int64   `json:"cost_priced_calls"`
	CostUnpricedCalls int64   `json:"cost_unpriced_calls"`
	CostPartial       bool    `json:"cost_partial"`
}

// GroupStat is one aggregated row for a named dimension.
type GroupStat struct {
	Name                string  `json:"name"`
	Sub                 string  `json:"sub,omitempty"`
	Provider            string  `json:"provider,omitempty"`
	Calls               int64   `json:"calls"`
	Success             int64   `json:"success"`
	Failed              int64   `json:"failed"`
	ErrorRate           float64 `json:"error_rate"`
	InputTokens         int64   `json:"input_tokens"`
	OutputTokens        int64   `json:"output_tokens"`
	ReasoningTokens     int64   `json:"reasoning_tokens,omitempty"`
	CachedTokens        int64   `json:"cached_tokens,omitempty"`
	CacheCreationTokens int64   `json:"cache_creation_tokens,omitempty"`
	CacheHitCalls       int64   `json:"cache_hit_calls,omitempty"`
	TotalTokens         int64   `json:"total_tokens"`
	AvgLatencyMs        float64 `json:"avg_latency_ms"`
	AvgTTFTMs           float64 `json:"avg_ttft_ms"`
	P50LatencyS         float64 `json:"p50_latency_s,omitempty"`
	P75LatencyS         float64 `json:"p75_latency_s,omitempty"`
	P90LatencyS         float64 `json:"p90_latency_s,omitempty"`
	P95LatencyS         float64 `json:"p95_latency_s,omitempty"`
	P99LatencyS         float64 `json:"p99_latency_s,omitempty"`
	P50TTFTS            float64 `json:"p50_ttft_s,omitempty"`
	P90TTFTS            float64 `json:"p90_ttft_s,omitempty"`
	TopError            int     `json:"top_error,omitempty"`

	// Estimated cost for this group (models only). CostUSD covers PricedCalls;
	// UnpricedCalls had no price and are excluded from the sum.
	CostUSD       float64 `json:"cost_usd,omitempty"`
	PricedCalls   int64   `json:"priced_calls,omitempty"`
	UnpricedCalls int64   `json:"unpriced_calls,omitempty"`
	PriceSource   string  `json:"price_source,omitempty"`
}

// UnpricedModel reports a model seen in the period that has no price in the
// price book, so the dashboard can list it instead of silently showing $0.
type UnpricedModel struct {
	Model string `json:"model"`
	Calls int64  `json:"calls"`
}

// PricingMeta describes the price book the estimates were computed from.
type PricingMeta struct {
	Available bool            `json:"available"`
	Source    string          `json:"source,omitempty"`
	Revision  uint64          `json:"revision"`
	Entries   int             `json:"entries"`
	SyncedAt  string          `json:"synced_at,omitempty"`
	Unpriced  []UnpricedModel `json:"unpriced_models,omitempty"`
	Note      string          `json:"note,omitempty"`
}

// HourStat is one bucket of the trend chart with multi-dimension counts.
type HourStat struct {
	Hour         string  `json:"hour"`
	Calls        int64   `json:"calls"`
	Success      int64   `json:"success"`
	Failed       int64   `json:"failed"`
	ErrorRate    float64 `json:"error_rate"`
	TotalTokens  int64   `json:"total_tokens"`
	InputTokens  int64   `json:"input_tokens,omitempty"`
	OutputTokens int64   `json:"output_tokens,omitempty"`
	RoutedCalls  int64   `json:"routed_calls,omitempty"`
	DirectCalls  int64   `json:"direct_calls,omitempty"`
	HighEffort   int64   `json:"high_effort_calls,omitempty"`
	P50LatencyS  float64 `json:"p50_latency_s,omitempty"`
	P90LatencyS  float64 `json:"p90_latency_s,omitempty"`
	P95LatencyS  float64 `json:"p95_latency_s,omitempty"`
	P99LatencyS  float64 `json:"p99_latency_s,omitempty"`
}

// StatusCodeStat counts failed calls per upstream status code.
type StatusCodeStat struct {
	StatusCode int   `json:"status_code"`
	Calls      int64 `json:"calls"`
}

// RouterMapping represents an alias -> provider_model routing distribution.
type RouterMapping struct {
	Alias string `json:"alias"`
	Model string `json:"model"`
	Calls int64  `json:"calls"`
}

// DistributionBucket represents a histogram bin for latency or tokens.
type DistributionBucket struct {
	Label string `json:"label"`
	Calls int64  `json:"calls"`
}

// ScatterPoint is one point for the Latency vs TTFT scatter plot.
type ScatterPoint struct {
	LatencyS float64 `json:"latency_s"`
	TTFTS    float64 `json:"ttft_s"`
	Model    string  `json:"model"`
	Provider string  `json:"provider"`
	Failed   bool    `json:"failed"`
}

// UsageSummary is the complete dashboard aggregate for a time range.
type UsageSummary struct {
	Start               *time.Time           `json:"start,omitempty"`
	End                 *time.Time           `json:"end,omitempty"`
	Source              string               `json:"source,omitempty"`
	Totals              SummaryTotals        `json:"totals"`
	ByHour              []HourStat           `json:"by_hour"`
	ByModel             []GroupStat          `json:"by_model"`
	ByAlias             []GroupStat          `json:"by_alias"`
	ByProvider          []GroupStat          `json:"by_provider"`
	ByAPIKey            []GroupStat          `json:"by_api_key"`
	ByAttribution       []GroupStat          `json:"by_attribution"`
	ByEffort            []GroupStat          `json:"by_effort"`
	ByExecutor          []GroupStat          `json:"by_executor"`
	ByPrefix            []GroupStat          `json:"by_prefix"`
	StatusCodes         []StatusCodeStat     `json:"status_codes"`
	RouterMappings      []RouterMapping      `json:"router_mappings"`
	LatencyDistribution []DistributionBucket `json:"latency_distribution"`
	TokenDistribution   []DistributionBucket `json:"token_distribution"`
	ContextHistogram    []DistributionBucket `json:"context_histogram"` // backward compat
	ScatterPoints       []ScatterPoint       `json:"scatter_points"`
	SlowestRequests     []RequestDetail      `json:"slowest_requests"`
	Pricing             PricingMeta          `json:"pricing"`
}

// PageFilter narrows the paginated request listing.
type PageFilter struct {
	Model           string
	Provider        string
	Alias           string
	APIKey          string
	Attribution     string
	ReasoningEffort string
	Executor        string
	ServiceTier     string
	Failed          *bool
	StatusCode      int
	CachedOnly      bool
	Limit           int
	Offset          int
}

// UsagePage is one page of request-level records.
type UsagePage struct {
	Total  int64           `json:"total"`
	Limit  int             `json:"limit"`
	Offset int             `json:"offset"`
	Source string          `json:"source,omitempty"`
	Rows   []RequestDetail `json:"rows"`
}
