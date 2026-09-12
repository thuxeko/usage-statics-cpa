package main

import (
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

//go:embed dashboard.html
var dashboardHTML string

const (
	abiVersion uint32 = 1
	pluginID          = "usage-statistics"
	pluginName        = "Usage Statistics"
	// managementUsagePath is the plugin-owned management route path. The host
	// mounts it under /v0/management, producing
	// /v0/management/plugins/usage-statistics/usage.
	managementUsagePath = "/plugins/" + pluginID + "/usage"
	// managementSummaryPath and managementRequestsPath extend the API with a
	// SQL-aggregated dashboard summary and a paginated request listing.
	managementSummaryPath  = "/plugins/" + pluginID + "/usage/summary"
	managementRequestsPath = "/plugins/" + pluginID + "/usage/requests"
	// managementErrorLogPath serves the CPA request-error log excerpt for one
	// failed request (matched by timestamp + status code).
	managementErrorLogPath = "/plugins/" + pluginID + "/error-log"
	// dashboardPath is the FULL public resource URL the browser requests. CPA
	// mounts Resource routes at /v0/resource/plugins/<id>/<registered Path>
	// and dispatches the request to the plugin's management.handle with this
	// full path (verified against the model-router plugin pattern).
	dashboardPath = "/v0/resource/plugins/" + pluginID + "/dashboard"
	// dashboardMatchPath is the normalized form compared in management.handle
	// after stripping the /v0/resource prefix from the forwarded path.
	dashboardMatchPath = "/plugins/" + pluginID + "/dashboard"
	insertTimeout      = 5 * time.Second
)

// Overridable at build time via -ldflags "-X main.pluginVersion=...".
var (
	pluginVersion    = "0.1.0"
	pluginAuthor     = "Fwindy"
	pluginRepository = "https://github.com/Fwindy/cpa-usage-statistics"
)

// ---- ABI envelope ----------------------------------------------------------

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func main() {}

func okEnvelope(v any) ([]byte, error) {
	result, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return json.Marshal(envelope{OK: true, Result: result})
}

func errorEnvelope(code, message string) []byte {
	raw, _ := json.Marshal(envelope{OK: false, Error: &envelopeError{Code: code, Message: message}})
	return raw
}

// ---- method dispatch -------------------------------------------------------

func handleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case "plugin.register", "plugin.reconfigure":
		if err := configurePlugin(request); err != nil {
			return nil, err
		}
		return okEnvelope(registerResponse())
	case "usage.handle":
		return handleUsage(request)
	case "management.register":
		return okEnvelope(managementRegistration())
	case "management.handle":
		return handleManagement(request)
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

// ---- registration metadata -------------------------------------------------

type registerResponsePayload struct {
	SchemaVersion int              `json:"schema_version"`
	Metadata      metadataInfo     `json:"metadata"`
	Capabilities  capabilitiesInfo `json:"capabilities"`
}

type metadataInfo struct {
	Name             string            `json:"Name"`
	Version          string            `json:"Version"`
	Author           string            `json:"Author"`
	GitHubRepository string            `json:"GitHubRepository"`
	Logo             string            `json:"Logo"`
	ConfigFields     []configFieldInfo `json:"ConfigFields"`
}

type configFieldInfo struct {
	Name        string `json:"Name"`
	Type        string `json:"Type"`
	Description string `json:"Description"`
}

type capabilitiesInfo struct {
	UsagePlugin   bool `json:"usage_plugin"`
	ManagementAPI bool `json:"management_api"`
}

func registerResponse() registerResponsePayload {
	return registerResponsePayload{
		SchemaVersion: 1,
		Metadata: metadataInfo{
			Name:             pluginName,
			Version:          pluginVersion,
			Author:           pluginAuthor,
			GitHubRepository: pluginRepository,
			ConfigFields: []configFieldInfo{
				{Name: "data_dir", Type: "string", Description: "Directory for usage.db. Defaults to ~/.cli-proxy-api/plugins/usage-statistics."},
				{Name: "retention_days", Type: "integer", Description: "Delete usage records older than this many days. 0 disables cleanup."},
			},
		},
		Capabilities: capabilitiesInfo{UsagePlugin: true, ManagementAPI: true},
	}
}

type managementRegistrationPayload struct {
	Routes    []managementRoute   `json:"routes"`
	Resources []resourceRouteInfo `json:"resources"`
}

type managementRoute struct {
	Method      string `json:"Method"`
	Path        string `json:"Path"`
	Description string `json:"Description"`
}

// resourceRouteInfo follows the CPA plugin resource-menu pattern (as used by
// model-router): a public HTML page mounted under /v0/resource/plugins/<id>/...
// that the management frontend embeds in its Plugins menu. Menu is the label
// shown in the sidebar.
type resourceRouteInfo struct {
	Path        string `json:"Path"`
	Menu        string `json:"Menu"`
	Description string `json:"Description"`
}

func managementRegistration() managementRegistrationPayload {
	return managementRegistrationPayload{
		Routes: []managementRoute{
			{Method: "GET", Path: managementUsagePath, Description: "Query persisted usage grouped by api key and model."},
			{Method: "GET", Path: managementSummaryPath, Description: "Dashboard aggregate: totals, per-hour, per-model, per-alias, per-provider, per-api-key."},
			{Method: "GET", Path: managementRequestsPath, Description: "Paginated request-level usage listing with filters."},
			{Method: "GET", Path: managementErrorLogPath, Description: "CPA request-error log excerpt for a failed request (ts + status match)."},
			{Method: "DELETE", Path: managementUsagePath, Description: "Delete persisted usage records by id."},
		},
		Resources: []resourceRouteInfo{
			{
				// Relative to /v0/resource/plugins/usage-statistics/. The host
				// shows "Usage Statistics" in the management Plugins menu.
				Path:        "/dashboard",
				Menu:        "Usage Statistics",
				Description: "Usage dashboard: overview, models, requests, keys & providers.",
			},
		},
	}
}

// ---- usage ingestion -------------------------------------------------------

type usageRecord struct {
	Provider        string       `json:"Provider"`
	ExecutorType    string       `json:"ExecutorType"`
	Model           string       `json:"Model"`
	Alias           string       `json:"Alias"`
	APIKey          string       `json:"APIKey"`
	AuthID          string       `json:"AuthID"`
	AuthIndex       string       `json:"AuthIndex"`
	AuthType        string       `json:"AuthType"`
	Source          string       `json:"Source"`
	ReasoningEffort string       `json:"ReasoningEffort"`
	ServiceTier     string       `json:"ServiceTier"`
	RequestedAt     time.Time    `json:"RequestedAt"`
	Latency         int64        `json:"Latency"`
	TTFT            int64        `json:"TTFT"`
	Failed          bool         `json:"Failed"`
	Failure         usageFailure `json:"Failure"`
	Detail          usageDetail  `json:"Detail"`
}

type usageFailure struct {
	StatusCode int    `json:"StatusCode"`
	Body       string `json:"Body"`
}

type usageDetail struct {
	InputTokens         int64 `json:"InputTokens"`
	OutputTokens        int64 `json:"OutputTokens"`
	ReasoningTokens     int64 `json:"ReasoningTokens"`
	CachedTokens        int64 `json:"CachedTokens"`
	CacheReadTokens     int64 `json:"CacheReadTokens"`
	CacheCreationTokens int64 `json:"CacheCreationTokens"`
	TotalTokens         int64 `json:"TotalTokens"`
}

func handleUsage(request []byte) ([]byte, error) {
	if len(request) == 0 {
		return okEnvelope(map[string]any{"ignored": true})
	}
	var rec usageRecord
	if err := json.Unmarshal(request, &rec); err != nil {
		return okEnvelope(map[string]any{"ignored": true})
	}
	store := currentStore()
	if store == nil {
		return okEnvelope(map[string]any{"ignored": true})
	}
	ctx, cancel := context.WithTimeout(context.Background(), insertTimeout)
	defer cancel()
	if err := store.Insert(ctx, toRecord(rec)); err != nil {
		return nil, err
	}
	maybeCleanup()
	return okEnvelope(map[string]any{"stored": true})
}

func toRecord(rec usageRecord) Record {
	ts := rec.RequestedAt
	if ts.IsZero() {
		ts = time.Now()
	}
	return Record{
		ID:              uuid.NewString(),
		Timestamp:       ts.UTC(),
		APIKey:          strings.TrimSpace(rec.APIKey),
		Provider:        strings.TrimSpace(rec.Provider),
		Model:           normalizeModel(rec.Model),
		Alias:           strings.TrimSpace(rec.Alias),
		Source:          strings.TrimSpace(rec.Source),
		AuthID:          strings.TrimSpace(rec.AuthID),
		AuthIndex:       strings.TrimSpace(rec.AuthIndex),
		AuthType:        strings.TrimSpace(rec.AuthType),
		ExecutorType:    strings.TrimSpace(rec.ExecutorType),
		ReasoningEffort: strings.TrimSpace(rec.ReasoningEffort),
		ServiceTier:     strings.TrimSpace(rec.ServiceTier),
		LatencyMs:       nsToMs(rec.Latency),
		TTFTMs:          nsToMs(rec.TTFT),
		Tokens: TokenStats{
			InputTokens:         rec.Detail.InputTokens,
			OutputTokens:        rec.Detail.OutputTokens,
			ReasoningTokens:     rec.Detail.ReasoningTokens,
			CachedTokens:        rec.Detail.CachedTokens,
			CacheReadTokens:     rec.Detail.CacheReadTokens,
			CacheCreationTokens: rec.Detail.CacheCreationTokens,
			TotalTokens:         rec.Detail.TotalTokens,
		},
		Failed:            rec.Failed || rec.Failure.StatusCode >= 400,
		FailureStatusCode: rec.Failure.StatusCode,
		FailureBody:       strings.TrimSpace(rec.Failure.Body),
	}
}

func nsToMs(ns int64) int64 {
	if ns <= 0 {
		return 0
	}
	return ns / int64(time.Millisecond)
}

// ---- management handling ---------------------------------------------------

type managementRequest struct {
	Method  string              `json:"Method"`
	Path    string              `json:"Path"`
	Headers map[string][]string `json:"Headers"`
	Query   map[string][]string `json:"Query"`
	Body    []byte              `json:"Body"`
}

type managementResponse struct {
	StatusCode int                 `json:"StatusCode"`
	Headers    map[string][]string `json:"Headers"`
	Body       []byte              `json:"Body"`
}

type deleteUsageRequest struct {
	IDs []string `json:"ids"`
}

func handleManagement(request []byte) ([]byte, error) {
	var req managementRequest
	if len(request) > 0 {
		if err := json.Unmarshal(request, &req); err != nil {
			return nil, err
		}
	}
	path := strings.TrimRight(strings.TrimSpace(req.Path), "/")
	// CPA forwards the FULL public path to management.handle (verified against
	// the model-router plugin source, which matches on
	// "/v0/management/plugins/..." and "/v0/resource/plugins/..."). Normalize
	// by stripping the public prefixes so both full and relative forms match.
	path = strings.TrimPrefix(path, "/v0/management")
	path = strings.TrimPrefix(path, "/v0/resource")
	switch {
	case path == strings.TrimRight(dashboardMatchPath, "/"):
		// Public inline UI page (resource route). No management key needed to
		// LOAD the page; its JS fetches data through /v0/management which the
		// host authenticates.
		if !strings.EqualFold(strings.TrimSpace(req.Method), "GET") {
			return okEnvelope(jsonManagementResponse(405, map[string]string{"error": "method not allowed"}))
		}
		return okEnvelope(jsonManagementResponseHTML(dashboardHTML))
	case path == strings.TrimRight(managementUsagePath, "/"):
		// Raw record listing / delete (upstream behaviour, kept intact).
		switch strings.ToUpper(strings.TrimSpace(req.Method)) {
		case "GET":
			return usageGet(req)
		case "DELETE":
			return usageDelete(req)
		default:
			return okEnvelope(jsonManagementResponse(405, map[string]string{"error": "method not allowed"}))
		}
	case path == strings.TrimRight(managementSummaryPath, "/"):
		if !strings.EqualFold(strings.TrimSpace(req.Method), "GET") {
			return okEnvelope(jsonManagementResponse(405, map[string]string{"error": "method not allowed"}))
		}
		return summaryForSource(req)
	case path == strings.TrimRight(managementRequestsPath, "/"):
		if !strings.EqualFold(strings.TrimSpace(req.Method), "GET") {
			return okEnvelope(jsonManagementResponse(405, map[string]string{"error": "method not allowed"}))
		}
		return pageForSource(req)
	case path == strings.TrimRight(managementErrorLogPath, "/"):
		if !strings.EqualFold(strings.TrimSpace(req.Method), "GET") {
			return okEnvelope(jsonManagementResponse(405, map[string]string{"error": "method not allowed"}))
		}
		return errorLogGetWithFile(req)
	default:
		return okEnvelope(jsonManagementResponse(404, map[string]string{"error": "not found"}))
	}
}

// parsePageFilter extracts the request-listing filters from the query params.
func parsePageFilter(query map[string][]string) (QueryRange, PageFilter) {
	rng, _ := parseUsageRange(query)
	filter := PageFilter{}
	if v := firstValue(query, "model"); strings.TrimSpace(v) != "" {
		filter.Model = strings.TrimSpace(v)
	}
	if v := firstValue(query, "provider"); strings.TrimSpace(v) != "" {
		filter.Provider = strings.TrimSpace(v)
	}
	if v := firstValue(query, "alias"); strings.TrimSpace(v) != "" {
		filter.Alias = strings.TrimSpace(v)
	}
	if v := firstValue(query, "api_key"); strings.TrimSpace(v) != "" {
		filter.APIKey = strings.TrimSpace(v)
	}
	if v := firstValue(query, "attribution"); strings.TrimSpace(v) != "" {
		filter.Attribution = strings.TrimSpace(v)
	}
	if v := firstValue(query, "reasoning_effort"); strings.TrimSpace(v) != "" {
		filter.ReasoningEffort = strings.TrimSpace(v)
	}
	if v := firstValue(query, "executor"); strings.TrimSpace(v) != "" {
		filter.Executor = strings.TrimSpace(v)
	}
	if v := firstValue(query, "status_code"); strings.TrimSpace(v) != "" {
		filter.StatusCode = toConfigInt(v)
	}
	if v := firstValue(query, "cached_only"); v == "true" || v == "1" {
		filter.CachedOnly = true
	}
	switch strings.ToLower(firstValue(query, "result")) {
	case "failed":
		failed := true
		filter.Failed = &failed
	case "success":
		failed := false
		filter.Failed = &failed
	}
	filter.Limit = toConfigInt(firstValue(query, "limit"))
	filter.Offset = toConfigInt(firstValue(query, "offset"))
	return rng, filter
}

// usageSummaryGet computes the dashboard aggregate for the requested range.
func usageSummaryGet(req managementRequest) ([]byte, error) {
	rng, errResp := parseUsageRange(req.Query)
	if errResp != nil {
		return okEnvelope(*errResp)
	}
	store := currentStore()
	if store == nil {
		return okEnvelope(jsonManagementResponse(500, map[string]string{"error": "usage store unavailable"}))
	}
	ctx, cancel := context.WithTimeout(context.Background(), insertTimeout)
	defer cancel()
	summary, err := store.Summary(ctx, rng)
	if err != nil {
		return okEnvelope(jsonManagementResponse(500, map[string]string{"error": "failed to summarize usage"}))
	}
	return okEnvelope(jsonManagementResponse(200, summary))
}

// usageRequestsGet returns one page of request-level records.
func usageRequestsGet(req managementRequest) ([]byte, error) {
	rng, filter := parsePageFilter(req.Query)
	store := currentStore()
	if store == nil {
		return okEnvelope(jsonManagementResponse(500, map[string]string{"error": "usage store unavailable"}))
	}
	ctx, cancel := context.WithTimeout(context.Background(), insertTimeout)
	defer cancel()
	page, err := store.ListPage(ctx, rng, filter)
	if err != nil {
		return okEnvelope(jsonManagementResponse(500, map[string]string{"error": "failed to list usage"}))
	}
	return okEnvelope(jsonManagementResponse(200, page))
}

func usageGet(req managementRequest) ([]byte, error) {
	rng, errResp := parseUsageRange(req.Query)
	if errResp != nil {
		return okEnvelope(*errResp)
	}
	store := currentStore()
	if store == nil {
		return okEnvelope(jsonManagementResponse(200, APIUsage{}))
	}
	ctx, cancel := context.WithTimeout(context.Background(), insertTimeout)
	defer cancel()
	result, err := store.Query(ctx, rng)
	if err != nil {
		return okEnvelope(jsonManagementResponse(500, map[string]string{"error": "failed to query usage"}))
	}
	return okEnvelope(jsonManagementResponse(200, result))
}

func usageDelete(req managementRequest) ([]byte, error) {
	store := currentStore()
	if store == nil {
		return okEnvelope(jsonManagementResponse(400, map[string]string{"error": "usage store unavailable"}))
	}
	var body deleteUsageRequest
	if len(req.Body) > 0 {
		if err := json.Unmarshal(req.Body, &body); err != nil {
			return okEnvelope(jsonManagementResponse(400, map[string]string{"error": "invalid body"}))
		}
	}
	ids := make([]string, 0, len(body.IDs))
	seen := make(map[string]struct{}, len(body.IDs))
	for _, id := range body.IDs {
		trimmed := strings.TrimSpace(id)
		if trimmed == "" {
			continue
		}
		if _, exists := seen[trimmed]; exists {
			continue
		}
		seen[trimmed] = struct{}{}
		ids = append(ids, trimmed)
	}
	if len(ids) == 0 {
		return okEnvelope(jsonManagementResponse(400, map[string]string{"error": "ids required"}))
	}
	ctx, cancel := context.WithTimeout(context.Background(), insertTimeout)
	defer cancel()
	result, err := store.Delete(ctx, ids)
	if err != nil {
		return okEnvelope(jsonManagementResponse(500, map[string]string{"error": "failed to delete usage records"}))
	}
	return okEnvelope(jsonManagementResponse(200, result))
}

func parseUsageRange(query map[string][]string) (QueryRange, *managementResponse) {
	var rng QueryRange
	if raw := strings.TrimSpace(firstValue(query, "start")); raw != "" {
		start, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			resp := jsonManagementResponse(400, map[string]string{"error": "invalid start"})
			return rng, &resp
		}
		start = start.UTC()
		rng.Start = &start
	}
	if raw := strings.TrimSpace(firstValue(query, "end")); raw != "" {
		end, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			resp := jsonManagementResponse(400, map[string]string{"error": "invalid end"})
			return rng, &resp
		}
		end = end.UTC()
		rng.End = &end
	}
	return rng, nil
}

func firstValue(values map[string][]string, key string) string {
	if len(values[key]) > 0 {
		return values[key][0]
	}
	return ""
}

func jsonManagementResponse(status int, payload any) managementResponse {
	body, err := json.Marshal(payload)
	if err != nil {
		body = []byte(`{"error":"failed to encode response"}`)
		status = 500
	}
	return managementResponse{
		StatusCode: status,
		Headers:    map[string][]string{"Content-Type": {"application/json; charset=utf-8"}},
		Body:       body,
	}
}

// jsonManagementResponseHTML returns the inline dashboard page with the same
// security headers the model-router resource page uses.
func jsonManagementResponseHTML(html string) managementResponse {
	return managementResponse{
		StatusCode: 200,
		Headers: map[string][]string{
			"Content-Type":             {"text/html; charset=utf-8"},
			"Cache-Control":            {"no-store"},
			"Referrer-Policy":          {"no-referrer"},
			"X-Content-Type-Options":   {"nosniff"},
			"Content-Security-Policy":  {"default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; connect-src 'self'"},
		},
		Body: []byte(html),
	}
}

// ---- store lifecycle -------------------------------------------------------

var (
	storeMu       sync.RWMutex
	globalStore   *SQLiteStore
	currentDBPath string

	retentionDays atomic.Int64

	cleanupMu   sync.Mutex
	lastCleanup time.Time
)

func currentStore() *SQLiteStore {
	storeMu.RLock()
	defer storeMu.RUnlock()
	return globalStore
}

func closeStore() {
	storeMu.Lock()
	defer storeMu.Unlock()
	if globalStore != nil {
		_ = globalStore.Close()
		globalStore = nil
		currentDBPath = ""
	}
}

func ensureStore(cfg pluginConfig) error {
	path, err := resolveDBPath(cfg)
	if err != nil {
		return err
	}
	storeMu.Lock()
	defer storeMu.Unlock()
	if globalStore != nil && currentDBPath == path {
		return nil
	}
	store, err := NewSQLiteStore(path)
	if err != nil {
		return err
	}
	if globalStore != nil {
		_ = globalStore.Close()
	}
	globalStore = store
	currentDBPath = path
	return nil
}

func maybeCleanup() {
	days := retentionDays.Load()
	if days <= 0 {
		return
	}
	cleanupMu.Lock()
	now := time.Now()
	if !lastCleanup.IsZero() && now.Sub(lastCleanup) < time.Hour {
		cleanupMu.Unlock()
		return
	}
	lastCleanup = now
	cleanupMu.Unlock()

	store := currentStore()
	if store == nil {
		return
	}
	cutoff := now.Add(-time.Duration(days) * 24 * time.Hour)
	ctx, cancel := context.WithTimeout(context.Background(), insertTimeout)
	defer cancel()
	_, _ = store.DeleteBefore(ctx, cutoff)
}

// ---- config ----------------------------------------------------------------

type lifecycleRequest struct {
	ConfigYAML json.RawMessage `json:"config_yaml"`
}

func configurePlugin(request []byte) error {
	var req lifecycleRequest
	if len(request) > 0 {
		if err := json.Unmarshal(request, &req); err != nil {
			return err
		}
	}
	raw, err := lifecycleConfigYAML(req.ConfigYAML)
	if err != nil {
		return err
	}
	cfg := parseConfig(raw)
	retentionDays.Store(int64(cfg.RetentionDays))
	if err := ensureStore(cfg); err != nil {
		return err
	}
	if cfg.RetentionDays > 0 {
		cleanupMu.Lock()
		lastCleanup = time.Time{}
		cleanupMu.Unlock()
		maybeCleanup()
	}
	return nil
}

// lifecycleConfigYAML accepts the host's config_yaml which may arrive as a
// base64 string, a plain YAML string, or a byte array.
func lifecycleConfigYAML(raw json.RawMessage) ([]byte, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		if decoded, errDecode := base64.StdEncoding.DecodeString(text); errDecode == nil && strings.Contains(string(decoded), ":") {
			return decoded, nil
		}
		return []byte(text), nil
	}
	var bytes []byte
	if err := json.Unmarshal(raw, &bytes); err == nil {
		return bytes, nil
	}
	return nil, errors.New("config_yaml must be a string or byte array")
}
