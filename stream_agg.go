package main

// Stream response aggregation.
//
// CPA hands the plugin one callback per stream chunk. Storing a row per chunk
// would be thousands of rows for a single request, so the chunks are folded
// into ONE "response" row when the stream ends. This is what makes streamed
// responses visible at all: ~99% of the traffic on this host is streamed, and
// until now only the request half of those exchanges was captured.
//
// Two measured facts shape the design:
//
//  1. Providers here do NOT frame streams as SSE. The bytes arrive as JSON
//     objects concatenated back to back with no separator and no "data:"
//     prefix ("}{"), so the stream is walked by brace balance.
//
//  2. Reasoning models emit hundreds of reasoning_content chunks BEFORE the
//     first content chunk, and a single stream can exceed 60 KB while the
//     stored body is capped at 8 KB. Capping the RAW bytes therefore threw the
//     actual answer away: every captured stream was truncated and stored an
//     empty content (30/30 live rows). Text is now extracted incrementally as
//     chunks arrive and only the extracted TEXT is capped, so the answer
//     survives no matter how long the preamble is.
//
// Cost model: the hot path appends bytes and scans only the new ones for
// complete JSON objects. Memory per in-flight stream is bounded by the carry
// buffer (one partial object), the accumulated text, and a small preview.

import (
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/tidwall/gjson"
)

const (
	// streamAggMaxTracked bounds concurrent in-flight streams. Older entries
	// are evicted (and written out) when the cap is hit, so a burst cannot
	// grow memory without limit.
	streamAggMaxTracked = 64
	// streamAggIdleTTL is how long a buffer may go without a new chunk before
	// the janitor flushes it. Providers that emit no explicit terminator
	// (Gemini) are covered by this path.
	streamAggIdleTTL = 45 * time.Second
	// streamAggSweepInterval is the janitor tick. One goroutine, one timer.
	streamAggSweepInterval = 15 * time.Second
	// streamAggMaxChunks caps how many chunks a single stream may accumulate.
	// Guards against a runaway/looping stream.
	streamAggMaxChunks = 20000
	// streamAggMaxCarry bounds the partial-object buffer. A well-formed stream
	// never holds more than one incomplete object; exceeding this means the
	// framing is not JSON at all, so the carry is dropped rather than grown.
	streamAggMaxCarry = 1 << 20 // 1 MiB
	// streamAggMaxText caps the accumulated answer text. Well above the stored
	// body cap so the final truncation, not this, decides what is stored.
	streamAggMaxText = 512 << 10 // 512 KiB
)

// streamBuf accumulates one in-flight streamed response.
type streamBuf struct {
	requestID string
	traceID   string
	model     string
	sourceFmt string

	// carry holds the incomplete JSON tail of the previous chunk.
	carry []byte
	// content and reasoning are the extracted text, capped at streamAggMaxText.
	content   strings.Builder
	reasoning strings.Builder
	// toolNames collects the tools the model asked to call. On this host most
	// streamed turns are agent-loop turns that emit tool_calls instead of
	// text, so without this the captured response looks empty when it is not.
	toolNames []string
	// preview keeps the first raw bytes so an unexpected framing stays
	// diagnosable from the stored row alone.
	preview []byte

	rawBytes  int // total bytes seen, before any cap
	chunks    int
	truncated bool
	finish    string
	startedAt time.Time
	lastChunk time.Time
}

// streamAggregator owns the in-flight buffers. A single mutex guards the map;
// the critical section is a map lookup, a bounded append and a brace scan over
// the newly arrived bytes.
type streamAggregator struct {
	mu   sync.Mutex
	bufs map[string]*streamBuf

	stop      chan struct{}
	closeOnce sync.Once
	wg        sync.WaitGroup

	// finalize is called (outside the lock) with a completed buffer.
	finalize func(*streamBuf)
}

func newStreamAggregator(finalize func(*streamBuf)) *streamAggregator {
	a := &streamAggregator{
		bufs:     make(map[string]*streamBuf),
		stop:     make(chan struct{}),
		finalize: finalize,
	}
	a.wg.Add(1)
	go a.janitor()
	return a
}

// add appends one chunk. Called from the intercept hot path; never blocks on
// I/O. Parsing is limited to the objects completed by this chunk.
func (a *streamAggregator) add(requestID, traceID, model, sourceFmt string, body []byte) {
	if a == nil || requestID == "" || len(body) == 0 {
		return
	}
	now := time.Now()

	a.mu.Lock()
	b := a.bufs[requestID]
	if b == nil {
		if len(a.bufs) >= streamAggMaxTracked {
			// Evict the least recently active buffer. Done inline because the
			// callback must not run while holding the lock.
			if victim := a.evictOldestLocked(); victim != nil {
				a.mu.Unlock()
				a.emit(victim)
				a.mu.Lock()
				b = a.bufs[requestID]
			}
		}
		if b == nil {
			b = &streamBuf{requestID: requestID, startedAt: now}
			a.bufs[requestID] = b
		}
	}
	// Model/source format can be empty on the first payload chunk; keep the
	// first non-empty value we are given.
	if b.model == "" {
		b.model = model
	}
	if b.sourceFmt == "" {
		b.sourceFmt = sourceFmt
	}
	if b.traceID == "" {
		b.traceID = traceID
	}
	b.chunks++
	b.rawBytes += len(body)
	b.lastChunk = now
	b.consume(body)

	// Terminator detection is a cheap substring scan on the chunk we just
	// appended, not on the whole buffer.
	done := b.chunks >= streamAggMaxChunks || isStreamTerminator(body)
	var out *streamBuf
	if done {
		out = b
		delete(a.bufs, requestID)
	}
	a.mu.Unlock()

	if out != nil {
		a.emit(out)
	}
}

// consume appends raw bytes and folds every JSON object they complete into the
// buffer's extracted text. Caller must hold a.mu.
func (b *streamBuf) consume(chunk []byte) {
	// Keep a small raw preview for diagnostics.
	if len(b.preview) < streamPreviewBytes {
		room := streamPreviewBytes - len(b.preview)
		if len(chunk) > room {
			b.preview = append(b.preview, chunk[:room]...)
		} else {
			b.preview = append(b.preview, chunk...)
		}
	}

	b.carry = append(b.carry, chunk...)

	consumed := 0
	for i := 0; i < len(b.carry); {
		if b.carry[i] != '{' {
			i++
			continue
		}
		end := scanObjectEnd(b.carry, i)
		if end < 0 {
			// Incomplete object: wait for the next chunk.
			break
		}
		b.absorb(b.carry[i:end])
		i = end
		consumed = i
	}
	if consumed > 0 {
		b.carry = append(b.carry[:0], b.carry[consumed:]...)
	}
	// Refuse to grow without bound on non-JSON framing.
	if len(b.carry) > streamAggMaxCarry {
		b.carry = b.carry[:0]
		b.truncated = true
	}
}

// absorb extracts text and the finish reason from one complete JSON object.
func (b *streamBuf) absorb(obj []byte) {
	s := string(obj)

	if b.content.Len() < streamAggMaxText {
		b.content.WriteString(extractChunkText(s))
	}
	// Reasoning models emit a separate reasoning_content stream; keep it so a
	// reply that is entirely reasoning is not stored as empty.
	if b.reasoning.Len() < streamAggMaxText {
		if r := gjson.Get(s, "choices.0.delta.reasoning_content"); r.Exists() && r.String() != "" {
			b.reasoning.WriteString(r.String())
		}
	}
	if fr := gjson.Get(s, "choices.0.finish_reason"); fr.Exists() && fr.String() != "" {
		b.finish = fr.String()
	}
	if fr := gjson.Get(s, "delta.stop_reason"); fr.Exists() && fr.String() != "" {
		b.finish = fr.String()
	}
	// Tool calls are streamed in pieces: the name usually arrives on one chunk
	// and the arguments on later ones. Only the names are collected, and
	// duplicates are collapsed, which keeps this cheap and bounded.
	if tc := gjson.Get(s, "choices.0.delta.tool_calls"); tc.IsArray() {
		for _, call := range tc.Array() {
			if name := call.Get("function.name"); name.Exists() && name.String() != "" {
				if !containsString(b.toolNames, name.String()) && len(b.toolNames) < 32 {
					b.toolNames = append(b.toolNames, name.String())
				}
			}
		}
	}
}

func containsString(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

// evictOldestLocked removes and returns the least recently active buffer.
// Caller must hold a.mu.
func (a *streamAggregator) evictOldestLocked() *streamBuf {
	var (
		oldestID string
		oldest   *streamBuf
	)
	for id, b := range a.bufs {
		if oldest == nil || b.lastChunk.Before(oldest.lastChunk) {
			oldestID, oldest = id, b
		}
	}
	if oldest != nil {
		delete(a.bufs, oldestID)
	}
	return oldest
}

func (a *streamAggregator) janitor() {
	defer a.wg.Done()
	t := time.NewTicker(streamAggSweepInterval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			a.sweep()
		case <-a.stop:
			a.flushAll()
			return
		}
	}
}

// sweep flushes buffers whose stream has gone quiet.
func (a *streamAggregator) sweep() {
	cutoff := time.Now().Add(-streamAggIdleTTL)
	var ready []*streamBuf
	a.mu.Lock()
	for id, b := range a.bufs {
		if b.lastChunk.Before(cutoff) {
			ready = append(ready, b)
			delete(a.bufs, id)
		}
	}
	a.mu.Unlock()
	for _, b := range ready {
		a.emit(b)
	}
}

// flushAll drains every buffer; used on shutdown.
func (a *streamAggregator) flushAll() {
	a.mu.Lock()
	ready := make([]*streamBuf, 0, len(a.bufs))
	for id, b := range a.bufs {
		ready = append(ready, b)
		delete(a.bufs, id)
	}
	a.mu.Unlock()
	for _, b := range ready {
		a.emit(b)
	}
}

func (a *streamAggregator) emit(b *streamBuf) {
	if a.finalize != nil && b != nil && b.chunks > 0 {
		a.finalize(b)
	}
}

func (a *streamAggregator) close() {
	a.closeOnce.Do(func() {
		close(a.stop)
		a.wg.Wait()
	})
}

// isStreamTerminator reports whether this chunk ends the stream. Covers the
// OpenAI (`[DONE]`) and Anthropic (`message_stop`) markers, plus a chunk that
// actually carries a stop reason.
//
// The stop-reason check MUST match the quoted value, not the bare key: OpenAI
// sends `"finish_reason":null` on every single chunk, so a substring test for
// "finish_reason" matched the first chunk and finalised the stream before any
// content arrived. Gemini sends no marker at all and is handled by the idle
// sweep.
func isStreamTerminator(chunk []byte) bool {
	if len(chunk) == 0 {
		return false
	}
	s := string(chunk)
	if strings.Contains(s, "[DONE]") || strings.Contains(s, "message_stop") {
		return true
	}
	return strings.Contains(s, `"finish_reason":"`) || strings.Contains(s, `"stop_reason":"`)
}

// ---- parsing helpers --------------------------------------------------------

// scanObjectEnd returns the index just past the JSON object starting at s[start],
// or -1 when the object is incomplete. Braces inside string literals are
// ignored, which is what makes this safe on arbitrary model output.
func scanObjectEnd(s []byte, start int) int {
	depth := 0
	inStr := false
	esc := false
	for i := start; i < len(s); i++ {
		c := s[i]
		if inStr {
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i + 1
			}
		}
	}
	return -1
}

// extractChunkText pulls assistant text out of one decoded JSON object, trying
// every shape the providers in use are known to emit.
func extractChunkText(payload string) string {
	// OpenAI / OpenAI-compatible: choices[].delta.content
	if d := gjson.Get(payload, "choices.0.delta.content"); d.Exists() && d.String() != "" {
		return d.String()
	}
	// Some gateways send the full message rather than a delta.
	if d := gjson.Get(payload, "choices.0.message.content"); d.Exists() && d.String() != "" {
		return d.String()
	}
	// Anthropic: delta.text
	if d := gjson.Get(payload, "delta.text"); d.Exists() && d.String() != "" {
		return d.String()
	}
	// Anthropic non-delta shape: content[].text
	if arr := gjson.Get(payload, "content"); arr.IsArray() {
		var sb strings.Builder
		for _, part := range arr.Array() {
			if t := part.Get("text"); t.Exists() && t.String() != "" {
				sb.WriteString(t.String())
			}
		}
		if sb.Len() > 0 {
			return sb.String()
		}
	}
	// Gemini: candidates[].content.parts[].text
	if parts := gjson.Get(payload, "candidates.0.content.parts"); parts.IsArray() {
		var sb strings.Builder
		for _, p := range parts.Array() {
			if t := p.Get("text"); t.Exists() && t.String() != "" {
				sb.WriteString(t.String())
			}
		}
		if sb.Len() > 0 {
			return sb.String()
		}
	}
	return ""
}

// streamSummary is the JSON envelope stored in the response row. The dashboard
// renders bodies as JSON, so keeping it self-describing means no special
// casing on the client.
type streamSummary struct {
	Aggregated   bool     `json:"aggregated"`
	Chunks       int      `json:"chunks"`
	RawBytes     int      `json:"raw_bytes"`
	FinishReason string   `json:"finish_reason,omitempty"`
	Content      string   `json:"content"`
	ToolCalls    []string `json:"tool_calls,omitempty"`
	Reasoning    string   `json:"reasoning,omitempty"`
	Truncated    bool     `json:"truncated"`
	Note         string   `json:"note,omitempty"`
	// Preview keeps the first bytes of the raw stream. It is what makes a
	// provider whose framing differs from the assumed shape debuggable
	// without another deploy cycle.
	Preview string `json:"preview,omitempty"`
}

// streamPreviewBytes caps the retained raw preview.
const streamPreviewBytes = 400

// summariseStream renders an accumulated buffer as the stored envelope. Runs
// ONCE per request, off the hot path: the text was already extracted chunk by
// chunk, so this only applies the size cap.
func summariseStream(b *streamBuf, max int) (string, int, bool) {
	text := b.content.String()
	// Fall back to reasoning when the model produced no visible answer, which
	// is exactly the case that previously stored an empty body.
	reasoning := b.reasoning.String()
	if text == "" && reasoning != "" {
		text = reasoning
	}

	truncated := b.truncated || b.content.Len() >= streamAggMaxText || b.reasoning.Len() >= streamAggMaxText
	if max > 0 && len(text) > max {
		text = text[:max]
		truncated = true
	}
	// Keep the reasoning as a separate, bounded field so a tool-call turn is
	// still explainable from the stored row.
	reasoningOut := reasoning
	if max > 0 && len(reasoningOut) > max {
		reasoningOut = reasoningOut[:max]
	}

	env := streamSummary{
		Aggregated:   true,
		Chunks:       b.chunks,
		RawBytes:     b.rawBytes,
		FinishReason: b.finish,
		Content:      text,
		ToolCalls:    b.toolNames,
		Reasoning:    reasoningOut,
		Truncated:    truncated,
		Preview:      maskSecrets(string(b.preview)),
	}
	if b.chunks == 0 {
		env.Note = "no chunks observed"
	}
	raw, err := json.Marshal(env)
	if err != nil {
		return text, b.rawBytes, truncated
	}
	return maskSecrets(string(raw)), b.rawBytes, truncated
}
