package main

// Data-source selection and CPA request-error-log lookup.
//
// CPA delivers each usage record to a single usage plugin. When the
// model-router plugin is active it captures the vast majority of records
// (routed + direct traffic), so this plugin's own SQLite store only sees a
// fraction. The dashboard therefore reads the model-router database
// READ-ONLY when it exists (source=router), and falls back to its own store
// (source=plugin) otherwise. The upstream Fwindy routes are untouched.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// resolveSource picks the usage source for a request. ?source= forces one;
// otherwise the router DB wins when present (it holds the complete record
// set while model-router is enabled).
func resolveSource(query map[string][]string) (string, *routerStore) {
	forced := firstValue(query, "source")
	if forced == "plugin" {
		return "plugin", nil
	}
	rs, err := newRouterStore()
	if err == nil {
		if forced == "router" {
			return "router", rs
		}
		return "router", rs // auto
	}
	return "plugin", nil
}

// usageSummaryGet / usageRequestsGet dispatch on the resolved source.

func summaryForSource(req managementRequest) ([]byte, error) {
	rng, filter := parsePageFilter(req.Query)
	ctx, cancel := context.WithTimeout(context.Background(), insertTimeout)
	defer cancel()

	source, rs := resolveSource(req.Query)
	if source == "router" && rs != nil {
		defer rs.close()
		summary, err := rs.RouterSummary(ctx, rng, filter)
		if err != nil {
			return okEnvelope(jsonManagementResponse(500, map[string]string{"error": "router db read failed"}))
		}
		summary.Source = "router"
		return okEnvelope(jsonManagementResponse(200, summary))
	}
	store := currentStore()
	if store == nil {
		return okEnvelope(jsonManagementResponse(500, map[string]string{"error": "usage store unavailable"}))
	}
	summary, err := store.Summary(ctx, rng, filter)
	if err != nil {
		return okEnvelope(jsonManagementResponse(500, map[string]string{"error": "failed to summarize usage"}))
	}
	summary.Source = "plugin"
	return okEnvelope(jsonManagementResponse(200, summary))
}

func pageForSource(req managementRequest) ([]byte, error) {
	rng, filter := parsePageFilter(req.Query)
	ctx, cancel := context.WithTimeout(context.Background(), insertTimeout)
	defer cancel()

	source, rs := resolveSource(req.Query)
	if source == "router" && rs != nil {
		defer rs.close()
		page, err := rs.RouterPage(ctx, rng, filter)
		if err != nil {
			return okEnvelope(jsonManagementResponse(500, map[string]string{"error": "router db read failed"}))
		}
		page.Source = "router"
		return okEnvelope(jsonManagementResponse(200, page))
	}
	store := currentStore()
	if store == nil {
		return okEnvelope(jsonManagementResponse(500, map[string]string{"error": "usage store unavailable"}))
	}
	page, err := store.ListPage(ctx, rng, filter)
	if err != nil {
		return okEnvelope(jsonManagementResponse(500, map[string]string{"error": "failed to list usage"}))
	}
	page.Source = "plugin"
	return okEnvelope(jsonManagementResponse(200, page))
}

// ---- CPA request-error-log lookup ---------------------------------------------

// errorLogFile is one candidate match returned to the dashboard.
type errorLogFile struct {
	Name    string `json:"name"`
	Size    int64  `json:"size"`
	ModTime string `json:"mod_time"`
}

// errorLogGet finds CPA request-error log files matching a failed request.
// CPA writes one file per failed request:
// error-v1-chat-completions-2026-09-10T200158-<trace>.log (local TZ naming).
// The dashboard passes the record timestamp (UTC RFC3339) and status; we list
// candidate files within a ±90s window and return up to 5 newest.
func errorLogGet(req managementRequest) ([]byte, error) {
	rawTS := strings.TrimSpace(firstValue(req.Query, "ts"))
	status := strings.TrimSpace(firstValue(req.Query, "status"))

	resp := map[string]any{"files": []errorLogFile{}, "matched_ts": rawTS}
	if rawTS == "" {
		return okEnvelope(jsonManagementResponse(400, map[string]string{"error": "ts required"}))
	}
	t, err := time.Parse(time.RFC3339Nano, rawTS)
	if err != nil {
		return okEnvelope(jsonManagementResponse(400, map[string]string{"error": "invalid ts"}))
	}

	logDir := errorLogDir()
	entries, err := os.ReadDir(logDir)
	if err != nil {
		resp["error"] = "cannot read " + logDir
		return okEnvelope(jsonManagementResponse(200, resp))
	}

	// CPA names files with the LOCAL timestamp; be TZ-agnostic: match any
	// file whose embedded time is within ±(window + tz offset guess).
	window := 100 * time.Second
	loc := time.Local
	tLocalName := t.In(loc).Format("2006-01-02T150405")
	tUTCName := t.UTC().Format("2006-01-02T150405")

	var candidates []errorLogFile
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, "error-") || !strings.HasSuffix(name, ".log") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		// Extract the embedded timestamp: error-<endpoint>-<ts>-<id>.log
		stamp := extractErrorLogStamp(name)
		if stamp == "" {
			continue
		}
		st, err := time.ParseInLocation("2006-01-02T150405", stamp, loc)
		if err != nil {
			continue
		}
		delta := st.Sub(t)
		if delta < 0 {
			delta = -delta
		}
		if delta <= window {
			candidates = append(candidates, errorLogFile{
				Name:    name,
				Size:    info.Size(),
				ModTime: info.ModTime().UTC().Format(time.RFC3339),
			})
		}
	}
	if candidates == nil {
		candidates = []errorLogFile{}
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].Name > candidates[j].Name })
	if len(candidates) > 5 {
		candidates = candidates[:5]
	}
	resp["files"] = candidates
	resp["local_hint"] = tLocalName
	resp["utc_hint"] = tUTCName
	resp["status_filter"] = status
	_ = status // status is informational; timestamp window is the match key
	return okEnvelope(jsonManagementResponse(200, resp))
}

// Matches any "error-<endpoint with hyphens>-<YYYY-MM-DDThhmmss>-<id>.log"
// name, e.g. error-v1-chat-completions-2026-09-10T200158-f104724e.log.
var errLogStampRe = regexp.MustCompile(`^error-.+?-(\d{4}-\d{2}-\d{2}T\d{6})-[0-9a-fA-F]+\.log$`)

func extractErrorLogStamp(name string) string {
	m := errLogStampRe.FindStringSubmatch(name)
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

// errorLogDir mirrors CPA's log dir resolution: the mounted logs dir in the
// docker image, relative ./logs for source runs.
func errorLogDir() string {
	candidates := []string{
		"/CLIProxyAPI/logs",
		"logs",
	}
	for _, dir := range candidates {
		if fi, err := os.Stat(dir); err == nil && fi.IsDir() {
			return dir
		}
	}
	return "logs"
}

// errorLogContent returns a trimmed excerpt of one error log file. Only file
// names that passed the error-log listing are accepted, and the base must
// resolve inside the log dir (no path traversal).
func errorLogContent(name string) (int, []byte) {
	clean := filepath.Base(strings.TrimSpace(name))
	if !strings.HasPrefix(clean, "error-") || !strings.HasSuffix(clean, ".log") {
		return 400, []byte(`{"error":"invalid file"}`)
	}
	path := filepath.Join(errorLogDir(), clean)
	fi, err := os.Stat(path)
	if err != nil || fi.IsDir() {
		return 404, []byte(`{"error":"not found"}`)
	}
	const maxBytes = 64 * 1024
	data, err := os.ReadFile(path)
	if err != nil {
		return 500, []byte(`{"error":"read failed"}`)
	}
	if len(data) > maxBytes {
		// Keep head and tail: request headers live at the top, the API error
		// response at the bottom.
		head := data[:maxBytes/2]
		tail := data[len(data)-maxBytes/2:]
		data = append(append([]byte{}, head...), tail...)
		data = append(data, []byte("\n\n[... truncated by usage-statistics plugin ...]")...)
	}
	return 200, data
}

// errorLogGetFull is a richer handler used when query has file=name.
func errorLogGetWithFile(req managementRequest) ([]byte, error) {
	if name := strings.TrimSpace(firstValue(req.Query, "file")); name != "" {
		status, body := errorLogContent(name)
		var payload any
		if err := json.Unmarshal(body, &payload); err != nil {
			payload = map[string]any{"content": string(body)}
		}
		return okEnvelope(jsonManagementResponse(status, payload))
	}
	return errorLogGet(req)
}

// chatPayloadGet retrieves captured prompt and response preview for a request.
func chatPayloadGet(req managementRequest) ([]byte, error) {
	reqID := strings.TrimSpace(firstValue(req.Query, "id"))
	rawTS := strings.TrimSpace(firstValue(req.Query, "ts"))
	var ts time.Time
	if rawTS != "" {
		ts, _ = time.Parse(time.RFC3339Nano, rawTS)
		if ts.IsZero() {
			ts, _ = time.Parse(time.RFC3339, rawTS)
		}
	}

	store := currentStore()
	if store == nil {
		return okEnvelope(jsonManagementResponse(200, map[string]any{"found": false}))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	rec, err := store.GetChatPayload(ctx, reqID, ts)
	if err != nil || rec == nil {
		return okEnvelope(jsonManagementResponse(200, map[string]any{"found": false}))
	}

	return okEnvelope(jsonManagementResponse(200, map[string]any{
		"found":            true,
		"request_id":       rec.RequestID,
		"timestamp":        rec.Timestamp,
		"model":            rec.Model,
		"prompt_preview":   rec.PromptPreview,
		"response_preview": rec.ResponsePreview,
	}))
}

type cpaConfigProvider struct {
	Name     string   `json:"name"`
	Prefix   string   `json:"prefix"`
	BaseURL  string   `json:"base_url"`
	Keys     []string `json:"keys"`
	Disabled bool     `json:"disabled"`
	Models   []string `json:"models"`
}

type cpaYAMLConfig struct {
	OpenAICompatibility []struct {
		Name          string   `yaml:"name"`
		Prefix        string   `yaml:"prefix"`
		BaseURL       string   `yaml:"base-url"`
		Disabled      bool     `yaml:"disabled"`
		APIKey        string   `yaml:"api-key"`
		APIKeys       []string `yaml:"api-keys"`
		APIKeyEntries []struct {
			APIKey string `yaml:"api-key"`
		} `yaml:"api-key-entries"`
		Models []any `yaml:"models"`
	} `yaml:"openai-compatibility"`
}

func findCPAConfigFile() string {
	candidates := []string{
		"/CLIProxyAPI/config.yaml",
		"/mnt/dungchung/cliproxy/config.yaml",
		filepath.Join(pluginLibraryDir(), "../config.yaml"),
		filepath.Join(pluginLibraryDir(), "../../config.yaml"),
		"config.yaml",
	}
	for _, c := range candidates {
		fi, err := os.Stat(c)
		if err != nil || fi.IsDir() || fi.Size() == 0 {
			continue
		}
		return c
	}
	return ""
}

func readCPAConfigProviders() []cpaConfigProvider {
	path := findCPAConfigFile()
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var cfg cpaYAMLConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil
	}

	var results []cpaConfigProvider
	for _, item := range cfg.OpenAICompatibility {
		name := strings.TrimSpace(item.Name)
		if name == "" {
			continue
		}
		var keys []string
		for _, e := range item.APIKeyEntries {
			k := strings.TrimSpace(e.APIKey)
			if k != "" {
				keys = append(keys, k)
			}
		}
		for _, k := range item.APIKeys {
			k = strings.TrimSpace(k)
			if k != "" {
				keys = append(keys, k)
			}
		}
		if item.APIKey != "" {
			k := strings.TrimSpace(item.APIKey)
			if k != "" {
				keys = append(keys, k)
			}
		}

		var models []string
		for _, m := range item.Models {
			switch v := m.(type) {
			case string:
				if strings.TrimSpace(v) != "" {
					models = append(models, strings.TrimSpace(v))
				}
			case map[string]any:
				if n, ok := v["name"].(string); ok && strings.TrimSpace(n) != "" {
					models = append(models, strings.TrimSpace(n))
				}
			}
		}

		results = append(results, cpaConfigProvider{
			Name:     name,
			Prefix:   item.Prefix,
			BaseURL:  strings.TrimSpace(item.BaseURL),
			Keys:     keys,
			Disabled: item.Disabled,
			Models:   models,
		})
	}
	return results
}

// providersGet returns the list of ACTIVE (non-disabled) configured providers with their Base URLs and API Keys.
func providersGet(req managementRequest) ([]byte, error) {
	all := readCPAConfigProviders()
	active := make([]cpaConfigProvider, 0, len(all))
	for _, p := range all {
		if p.Disabled {
			continue
		}
		if len(p.Keys) == 0 {
			continue
		}
		active = append(active, p)
	}
	return okEnvelope(jsonManagementResponse(200, map[string]any{
		"providers": active,
	}))
}

type proxyFetchRequest struct {
	URL     string            `json:"url"`
	Method  string            `json:"method"`
	Headers map[string]string `json:"headers"`
	Body    string            `json:"body"`
}

// proxyFetchHandler executes an HTTP request from CPA backend to upstream to prevent browser CORS issues.
func proxyFetchHandler(req managementRequest) ([]byte, error) {
	var pReq proxyFetchRequest
	if err := json.Unmarshal(req.Body, &pReq); err != nil {
		return okEnvelope(jsonManagementResponse(400, map[string]string{"error": "invalid json body"}))
	}
	targetURL := strings.TrimSpace(pReq.URL)
	if targetURL == "" {
		return okEnvelope(jsonManagementResponse(400, map[string]string{"error": "url is required"}))
	}

	method := strings.ToUpper(strings.TrimSpace(pReq.Method))
	if method == "" {
		method = "GET"
	}

	var reqBody io.Reader
	if pReq.Body != "" {
		reqBody = strings.NewReader(pReq.Body)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(ctx, method, targetURL, reqBody)
	if err != nil {
		return okEnvelope(jsonManagementResponse(400, map[string]string{"error": "failed to build request: " + err.Error()}))
	}

	for k, v := range pReq.Headers {
		httpReq.Header.Set(k, v)
	}
	if httpReq.Header.Get("User-Agent") == "" {
		httpReq.Header.Set("User-Agent", "CPA-ToolsHub/1.0")
	}

	client := &http.Client{
		Timeout: 25 * time.Second,
	}

	resp, err := client.Do(httpReq)
	if err != nil {
		return okEnvelope(jsonManagementResponse(200, map[string]any{
			"ok":          false,
			"status_code": 502,
			"error":       "upstream connection failed: " + err.Error(),
		}))
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024))

	var rawJSON any
	if err := json.Unmarshal(respBody, &rawJSON); err == nil {
		return okEnvelope(jsonManagementResponse(200, map[string]any{
			"ok":          resp.StatusCode >= 200 && resp.StatusCode < 300,
			"status_code": resp.StatusCode,
			"data":        rawJSON,
			"raw":         string(respBody),
		}))
	}

	return okEnvelope(jsonManagementResponse(200, map[string]any{
		"ok":          resp.StatusCode >= 200 && resp.StatusCode < 300,
		"status_code": resp.StatusCode,
		"raw":         string(respBody),
		"error":       "response is not valid JSON",
	}))
}

var _ = fmt.Sprintf // keep fmt import if unused by future edits
var _ = strconv.Itoa
