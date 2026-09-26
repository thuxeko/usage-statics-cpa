package main

// management handlers for the request log API.

import (
	"net/http"
	"strconv"
	"strings"
)

// reqLogListGet serves GET /plugins/usage-statistics/reqlog?limit=&kind=&model=
// It returns metadata only — never bodies — so the listing stays cheap.
func reqLogListGet(req managementRequest) ([]byte, error) {
	limit := 100
	if raw := strings.TrimSpace(firstValue(req.Query, "limit")); raw != "" {
		if v, err := strconv.Atoi(raw); err == nil && v > 0 {
			limit = v
		}
	}
	rows, err := reqLogRecent(limit)
	if err != nil {
		return okEnvelope(jsonManagementResponse(http.StatusInternalServerError,
			map[string]string{"error": "request log read failed"}))
	}
	kind := strings.TrimSpace(firstValue(req.Query, "kind"))
	model := strings.TrimSpace(firstValue(req.Query, "model"))
	if kind != "" || model != "" {
		filtered := rows[:0]
		for _, r := range rows {
			if kind != "" && r["kind"] != kind {
				continue
			}
			if model != "" && !strings.Contains(strings.ToLower(stringVal(r["model"])), strings.ToLower(model)) {
				continue
			}
			filtered = append(filtered, r)
		}
		rows = filtered
	}
	return okEnvelope(jsonManagementResponse(http.StatusOK, map[string]any{
		"rows":  rows,
		"total": len(rows),
		"stats": reqLogStats(),
	}))
}

// reqLogBodyGet serves GET /plugins/usage-statistics/reqlog/body?id=<row id>
func reqLogBodyGet(req managementRequest) ([]byte, error) {
	raw := strings.TrimSpace(firstValue(req.Query, "id"))
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return okEnvelope(jsonManagementResponse(http.StatusBadRequest,
			map[string]string{"error": "missing or invalid id"}))
	}
	body, err := reqLogBody(id)
	if err != nil {
		return okEnvelope(jsonManagementResponse(http.StatusNotFound,
			map[string]string{"error": "request log row not found"}))
	}
	return okEnvelope(jsonManagementResponse(http.StatusOK, body))
}

func stringVal(v any) string {
	s, _ := v.(string)
	return s
}
