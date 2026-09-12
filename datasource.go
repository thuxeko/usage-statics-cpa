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
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
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
	rng, errResp := parseUsageRange(req.Query)
	if errResp != nil {
		return okEnvelope(*errResp)
	}
	ctx, cancel := context.WithTimeout(context.Background(), insertTimeout)
	defer cancel()

	source, rs := resolveSource(req.Query)
	if source == "router" && rs != nil {
		defer rs.close()
		summary, err := rs.RouterSummary(ctx, rng)
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
	summary, err := store.Summary(ctx, rng)
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

var _ = fmt.Sprintf // keep fmt import if unused by future edits
var _ = strconv.Itoa
