package main

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/tidwall/gjson"
)

const (
	maxCapturePromptBytes   = 2000
	maxCaptureResponseBytes = 2000
)

// In-flight capture state
type inFlightPrompt struct {
	requestID      string
	traceID        string
	model          string
	prompt         string
	timestamp      time.Time
}

type payloadCaptureManager struct {
	mu        sync.Mutex
	inFlight  map[string]*inFlightPrompt // requestID / traceID -> prompt
	history   []*inFlightPrompt          // FIFO ring for fast lookup
}

var globalPayloadManager = &payloadCaptureManager{
	inFlight: make(map[string]*inFlightPrompt),
}

func (m *payloadCaptureManager) recordInboundRequest(requestID, traceID, model string, body []byte) {
	if len(body) == 0 {
		return
	}
	prompt := extractPromptPreview(body)
	if prompt == "" {
		return
	}

	item := &inFlightPrompt{
		requestID: strings.TrimSpace(requestID),
		traceID:   strings.TrimSpace(traceID),
		model:     strings.TrimSpace(model),
		prompt:    prompt,
		timestamp: time.Now().UTC(),
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if item.requestID != "" {
		m.inFlight[item.requestID] = item
	}
	if item.traceID != "" {
		m.inFlight[item.traceID] = item
	}

	// Keep history bounded to 300 items
	m.history = append(m.history, item)
	if len(m.history) > 300 {
		old := m.history[0]
		m.history = m.history[1:]
		if old.requestID != "" {
			delete(m.inFlight, old.requestID)
		}
		if old.traceID != "" {
			delete(m.inFlight, old.traceID)
		}
	}
}

func (m *payloadCaptureManager) recordResponse(requestID string, responseBody []byte) {
	respText := extractResponsePreview(responseBody)
	m.mu.Lock()
	var item *inFlightPrompt
	if requestID != "" {
		item = m.inFlight[requestID]
	}
	m.mu.Unlock()

	var promptText, model string
	var reqTS time.Time
	if item != nil {
		promptText = item.prompt
		model = item.model
		reqTS = item.timestamp
	} else {
		reqTS = time.Now().UTC()
	}

	if promptText == "" && respText == "" {
		return
	}

	// Persist to usage.db asynchronously
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		store := currentStore()
		if store != nil {
			_ = store.SaveChatPayload(ctx, requestID, reqTS, model, promptText, respText)
		}
	}()
}

func (m *payloadCaptureManager) recordStreamChunk(requestID string, chunk []byte) {
	// For streaming, extract delta content and append
	delta := extractStreamDelta(chunk)
	if delta == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	// Optional stream accumulation if needed
}

// Helper: extract prompt from chat completion / completion JSON
func extractPromptPreview(body []byte) string {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return ""
	}
	res := gjson.ParseBytes(body)
	
	// 1. OpenAI Chat: messages array
	msgs := res.Get("messages")
	if msgs.IsArray() {
		arr := msgs.Array()
		if len(arr) > 0 {
			// Get last user message or last message
			var lastUserContent string
			for i := len(arr) - 1; i >= 0; i-- {
				role := arr[i].Get("role").String()
				content := arr[i].Get("content").String()
				if content == "" {
					// Check if content is array (multimodal)
					if arr[i].Get("content").IsArray() {
						for _, part := range arr[i].Get("content").Array() {
							if part.Get("type").String() == "text" {
								content = part.Get("text").String()
								break
							}
						}
					}
				}
				if role == "user" && content != "" {
					lastUserContent = content
					break
				}
			}
			if lastUserContent == "" {
				lastUserContent = arr[len(arr)-1].Get("content").String()
			}
			if len(lastUserContent) > maxCapturePromptBytes {
				return lastUserContent[:maxCapturePromptBytes] + "..."
			}
			return lastUserContent
		}
	}

	// 2. Legacy / Anthropic / Gemini prompt format
	if p := res.Get("prompt"); p.Exists() {
		s := p.String()
		if len(s) > maxCapturePromptBytes {
			return s[:maxCapturePromptBytes] + "..."
		}
		return s
	}
	if p := res.Get("contents"); p.Exists() && p.IsArray() {
		// Gemini contents array
		arr := p.Array()
		if len(arr) > 0 {
			parts := arr[len(arr)-1].Get("parts")
			if parts.IsArray() && len(parts.Array()) > 0 {
				s := parts.Array()[0].Get("text").String()
				if len(s) > maxCapturePromptBytes {
					return s[:maxCapturePromptBytes] + "..."
				}
				return s
			}
		}
	}
	return ""
}

// Helper: extract response text from response JSON
func extractResponsePreview(body []byte) string {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return ""
	}
	res := gjson.ParseBytes(body)

	// 1. OpenAI choices[0].message.content
	if c := res.Get("choices.0.message.content"); c.Exists() && c.String() != "" {
		s := c.String()
		if len(s) > maxCaptureResponseBytes {
			return s[:maxCaptureResponseBytes] + "..."
		}
		return s
	}

	// 2. Legacy choices[0].text
	if c := res.Get("choices.0.text"); c.Exists() && c.String() != "" {
		s := c.String()
		if len(s) > maxCaptureResponseBytes {
			return s[:maxCaptureResponseBytes] + "..."
		}
		return s
	}

	// 3. Anthropic content[0].text
	if c := res.Get("content.0.text"); c.Exists() && c.String() != "" {
		s := c.String()
		if len(s) > maxCaptureResponseBytes {
			return s[:maxCaptureResponseBytes] + "..."
		}
		return s
	}

	// 4. Gemini candidates[0].content.parts[0].text
	if c := res.Get("candidates.0.content.parts.0.text"); c.Exists() && c.String() != "" {
		s := c.String()
		if len(s) > maxCaptureResponseBytes {
			return s[:maxCaptureResponseBytes] + "..."
		}
		return s
	}
	return ""
}

func extractStreamDelta(chunk []byte) string {
	if len(chunk) == 0 {
		return ""
	}
	s := string(chunk)
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "data: ") {
			data := strings.TrimPrefix(line, "data: ")
			if data == "[DONE]" {
				continue
			}
			if gjson.Valid(data) {
				delta := gjson.Get(data, "choices.0.delta.content").String()
				if delta != "" {
					return delta
				}
			}
		}
	}
	return ""
}
