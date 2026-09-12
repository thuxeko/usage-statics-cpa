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
	Calls           int64   `json:"calls"`
	Failed          int64   `json:"failed"`
	InputTokens     int64   `json:"input_tokens"`
	OutputTokens    int64   `json:"output_tokens"`
	ReasoningTokens int64   `json:"reasoning_tokens"`
	CachedTokens    int64   `json:"cached_tokens"`
	TotalTokens     int64   `json:"total_tokens"`
	AvgLatencyMs    float64 `json:"avg_latency_ms"`
	AvgTTFTMs       float64 `json:"avg_ttft_ms"`
	P50LatencyS     float64 `json:"p50_latency_s"`
	P90LatencyS     float64 `json:"p90_latency_s"`
	P50TTFTS        float64 `json:"p50_ttft_s"`
	P90TTFTS        float64 `json:"p90_ttft_s"`
	HungCalls       int64   `json:"hung_calls"`       // > 300s
	CacheHitCalls   int64   `json:"cache_hit_calls"` // cached_tokens > 0
	ActiveKeys      int64   `json:"active_keys"`
	ActiveModels    int64   `json:"active_models"`
}

// GroupStat is one aggregated row for a named dimension (model, alias,
// provider, attribution, reasoning_effort, executor, or masked api key).
type GroupStat struct {
	Name            string  `json:"name"`
	Sub             string  `json:"sub,omitempty"`
	Calls           int64   `json:"calls"`
	Failed          int64   `json:"failed"`
	InputTokens     int64   `json:"input_tokens"`
	OutputTokens    int64   `json:"output_tokens"`
	ReasoningTokens int64   `json:"reasoning_tokens,omitempty"`
	CachedTokens    int64   `json:"cached_tokens,omitempty"`
	CacheHitCalls   int64   `json:"cache_hit_calls,omitempty"`
	TotalTokens     int64   `json:"total_tokens"`
	AvgLatencyMs    float64 `json:"avg_latency_ms"`
	AvgTTFTMs       float64 `json:"avg_ttft_ms"`
	P50LatencyS     float64 `json:"p50_latency_s,omitempty"`
	P90LatencyS     float64 `json:"p90_latency_s,omitempty"`
	P50TTFTS        float64 `json:"p50_ttft_s,omitempty"`
	P90TTFTS        float64 `json:"p90_ttft_s,omitempty"`
	TopError        int     `json:"top_error,omitempty"`
}

// HourStat is one hourly bucket of the trend chart with multi-dimension counts.
type HourStat struct {
	Hour         string `json:"hour"`
	Calls        int64  `json:"calls"`
	Failed       int64  `json:"failed"`
	TotalTokens  int64  `json:"total_tokens"`
	InputTokens  int64  `json:"input_tokens,omitempty"`
	OutputTokens int64  `json:"output_tokens,omitempty"`
	RoutedCalls  int64  `json:"routed_calls,omitempty"`
	DirectCalls  int64  `json:"direct_calls,omitempty"`
	HighEffort   int64  `json:"high_effort_calls,omitempty"`
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

// ContextBucket is one histogram bucket for prompt context token sizes.
type ContextBucket struct {
	Label string `json:"label"`
	Calls int64  `json:"calls"`
}

// UsageSummary is the complete dashboard aggregate for a time range.
type UsageSummary struct {
	Start            *time.Time       `json:"start,omitempty"`
	End              *time.Time       `json:"end,omitempty"`
	Source           string           `json:"source,omitempty"`
	Totals           SummaryTotals    `json:"totals"`
	ByHour           []HourStat       `json:"by_hour"`
	ByModel          []GroupStat      `json:"by_model"`
	ByAlias          []GroupStat      `json:"by_alias"`
	ByProvider       []GroupStat      `json:"by_provider"`
	ByAPIKey         []GroupStat      `json:"by_api_key"`
	ByAttribution    []GroupStat      `json:"by_attribution"`
	ByEffort         []GroupStat      `json:"by_effort"`
	ByExecutor       []GroupStat      `json:"by_executor"`
	ByPrefix         []GroupStat      `json:"by_prefix"`
	StatusCodes      []StatusCodeStat `json:"status_codes"`
	RouterMappings   []RouterMapping  `json:"router_mappings"`
	ContextHistogram []ContextBucket  `json:"context_histogram"`
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
