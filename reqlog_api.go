package main

// management handlers for the request log API.

import (
	"net/http"
	"strconv"
	"strings"
)

// reqLogListGet serves
// GET /plugins/usage-statistics/reqlog?limit=&offset=&kind=&model=&combo=&grouped=
// It returns metadata only — never bodies — so the listing stays cheap.
func reqLogListGet(req managementRequest) ([]byte, error) {
	limit := 100
	if raw := strings.TrimSpace(firstValue(req.Query, "limit")); raw != "" {
		if v, err := strconv.Atoi(raw); err == nil && v > 0 {
			limit = v
		}
	}
	offset := 0
	if raw := strings.TrimSpace(firstValue(req.Query, "offset")); raw != "" {
		if v, err := strconv.Atoi(raw); err == nil && v > 0 {
			offset = v
		}
	}
	// Grouping is opt-in and defaults to on: the raw per-attempt listing is the
	// thing that made one call look like several rows. Pass grouped=0 for it.
	grouped := true
	if raw := strings.TrimSpace(firstValue(req.Query, "grouped")); raw != "" {
		grouped = raw != "0" && !strings.EqualFold(raw, "false")
	}
	filter := reqLogFilter{
		Kind:    strings.TrimSpace(firstValue(req.Query, "kind")),
		Model:   strings.TrimSpace(firstValue(req.Query, "model")),
		Combo:   strings.TrimSpace(firstValue(req.Query, "combo")),
		Grouped: grouped,
	}
	rows, total, err := reqLogPage(limit, offset, filter)
	if err != nil {
		return okEnvelope(jsonManagementResponse(http.StatusInternalServerError,
			map[string]string{"error": "request log read failed"}))
	}
	return okEnvelope(jsonManagementResponse(http.StatusOK, map[string]any{
		"rows":    rows,
		"total":   total,
		"offset":  offset,
		"limit":   limit,
		"grouped": grouped,
		"stats":   reqLogStats(),
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
