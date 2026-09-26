package main

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
)

// collectFinalized returns an aggregator plus a channel of everything it emits,
// so tests can assert on the summary envelope without touching sqlite. The
// buffer is generous: the eviction test legitimately emits more than a handful
// of buffers, and a small channel would block the janitor.
func collectFinalized() (*streamAggregator, chan *streamBuf) {
	ch := make(chan *streamBuf, 512)
	return newStreamAggregator(func(b *streamBuf) { ch <- b }), ch
}

func TestStreamAggregatorJoinsOpenAIChunks(t *testing.T) {
	agg, out := collectFinalized()
	defer agg.close()

	agg.add("req-1", "trace-1", "gpt-4o", "openai", []byte("data: {\"choices\":[{\"delta\":{\"content\":\"Hel\"}}]}\n\n"))
	agg.add("req-1", "", "", "", []byte("data: {\"choices\":[{\"delta\":{\"content\":\"lo\"}}]}\n\n"))
	agg.add("req-1", "", "", "", []byte("data: [DONE]\n\n"))

	select {
	case b := <-out:
		body, rawBytes, _ := summariseStream(b, reqLogDefaultMaxBodyBytes)
		var got streamSummary
		if err := json.Unmarshal([]byte(body), &got); err != nil {
			t.Fatalf("summary is not valid JSON: %v (%s)", err, body)
		}
		if got.Content != "Hello" {
			t.Errorf("content = %q, want %q", got.Content, "Hello")
		}
		if got.Chunks != 3 {
			t.Errorf("chunks = %d, want 3", got.Chunks)
		}
		if !got.Aggregated {
			t.Error("aggregated flag not set")
		}
		// rawBytes counts every byte of every chunk, not the capped buffer.
		if rawBytes != len("data: {\"choices\":[{\"delta\":{\"content\":\"Hel\"}}]}\n\n")+
			len("data: {\"choices\":[{\"delta\":{\"content\":\"lo\"}}]}\n\n")+
			len("data: [DONE]\n\n") {
			t.Errorf("rawBytes = %d, want the full pre-truncation size", rawBytes)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stream was never finalised on the [DONE] terminator")
	}
}

func TestStreamAggregatorHandlesAnthropicAndGemini(t *testing.T) {
	agg, out := collectFinalized()
	defer agg.close()

	// Anthropic style
	agg.add("req-a", "", "claude", "anthropic", []byte("data: {\"delta\":{\"text\":\"Bon\"}}\n\n"))
	agg.add("req-a", "", "", "", []byte("data: {\"delta\":{\"text\":\"jour\"}}\n\n"))
	agg.add("req-a", "", "", "", []byte("data: {\"delta\":{\"stop_reason\":\"end_turn\"}}\n\n"))

	// Gemini style: no terminator, so the idle sweep must pick it up.
	agg.add("req-g", "", "gemini", "gemini", []byte("data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"Xin\"}]}}]}\n\n"))

	seen := map[string]streamSummary{}
	deadline := time.After(2 * time.Second)
	for len(seen) < 1 {
		select {
		case b := <-out:
			body, _, _ := summariseStream(b, reqLogDefaultMaxBodyBytes)
			var got streamSummary
			if err := json.Unmarshal([]byte(body), &got); err != nil {
				t.Fatalf("bad JSON: %v", err)
			}
			seen[b.requestID] = got
		case <-deadline:
			t.Fatal("anthropic stream was not finalised")
		}
	}
	if seen["req-a"].Content != "Bonjour" {
		t.Errorf("anthropic content = %q, want %q", seen["req-a"].Content, "Bonjour")
	}
	if seen["req-a"].FinishReason != "end_turn" {
		t.Errorf("finish_reason = %q, want end_turn", seen["req-a"].FinishReason)
	}

	// Force the idle sweep instead of waiting 45s of wall clock.
	agg.mu.Lock()
	for _, b := range agg.bufs {
		b.lastChunk = time.Now().Add(-2 * streamAggIdleTTL)
	}
	agg.mu.Unlock()
	agg.sweep()

	select {
	case b := <-out:
		if b.requestID != "req-g" {
			t.Fatalf("expected the gemini buffer, got %q", b.requestID)
		}
		body, _, _ := summariseStream(b, reqLogDefaultMaxBodyBytes)
		var got streamSummary
		if err := json.Unmarshal([]byte(body), &got); err != nil {
			t.Fatalf("bad JSON: %v", err)
		}
		if got.Content != "Xin" {
			t.Errorf("gemini content = %q, want %q", got.Content, "Xin")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("idle sweep did not flush the gemini buffer")
	}
}

func TestStreamAggregatorCapsTextNotRawBytes(t *testing.T) {
	// The regression this covers: capping RAW bytes threw the answer away,
	// because reasoning preambles exceed the cap before content starts. Text
	// must be extracted as it streams and only the TEXT is capped.
	agg, out := collectFinalized()
	defer agg.close()

	big := strings.Repeat("x", 4096)
	// Two large reasoning chunks first, then the real answer.
	agg.add("req-cap", "", "m", "openai", []byte(`{"choices":[{"delta":{"reasoning_content":"`+big+`"},"finish_reason":null}]}`))
	agg.add("req-cap", "", "", "", []byte(`{"choices":[{"delta":{"reasoning_content":"`+big+`"},"finish_reason":null}]}`))
	agg.add("req-cap", "", "", "", []byte(`{"choices":[{"delta":{"content":"THE ANSWER"},"finish_reason":"stop"}]}`))

	select {
	case b := <-out:
		body, rawBytes, _ := summariseStream(b, reqLogDefaultMaxBodyBytes)
		var got streamSummary
		if err := json.Unmarshal([]byte(body), &got); err != nil {
			t.Fatalf("bad JSON: %v", err)
		}
		if got.Content != "THE ANSWER" {
			t.Errorf("content = %q, want %q - the answer must survive a long preamble", got.Content, "THE ANSWER")
		}
		if rawBytes <= reqLogDefaultMaxBodyBytes {
			t.Errorf("rawBytes = %d should reflect the full stream, not the cap", rawBytes)
		}
		if b.content.Len() != len("THE ANSWER") {
			t.Errorf("accumulated text = %d bytes, want exactly the answer", b.content.Len())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stream was never finalised")
	}
}

func TestStreamAggregatorTruncatesTextAtStoredCap(t *testing.T) {
	agg, out := collectFinalized()
	defer agg.close()

	// Content far beyond the 8KB stored cap.
	chunk := `{"choices":[{"delta":{"content":"` + strings.Repeat("y", 2000) + `"},"finish_reason":null}]}`
	for i := 0; i < 10; i++ {
		agg.add("req-big", "", "m", "openai", []byte(chunk))
	}
	agg.add("req-big", "", "", "", []byte(`{"choices":[{"delta":{},"finish_reason":"stop"}]}`))

	select {
	case b := <-out:
		body, _, _ := summariseStream(b, reqLogDefaultMaxBodyBytes)
		var got streamSummary
		if err := json.Unmarshal([]byte(body), &got); err != nil {
			t.Fatalf("bad JSON: %v", err)
		}
		if len(got.Content) != reqLogDefaultMaxBodyBytes {
			t.Errorf("stored content = %d bytes, want the %d cap", len(got.Content), reqLogDefaultMaxBodyBytes)
		}
		if !got.Truncated {
			t.Error("truncated flag not set")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("oversized stream was never finalised")
	}
}

func TestStreamAggregatorEvictsOldestWhenSaturated(t *testing.T) {
	agg, out := collectFinalized()
	defer agg.close()

	// Fill past the tracking cap with streams that never terminate.
	for i := 0; i < streamAggMaxTracked+5; i++ {
		id := string(rune('a' + i%26))
		id = id + "-" + strings.Repeat("z", i%7)
		agg.add(id, "", "m", "openai", []byte("data: {\"choices\":[{\"delta\":{\"content\":\"q\"}}]}\n\n"))
	}

	agg.mu.Lock()
	tracked := len(agg.bufs)
	agg.mu.Unlock()
	if tracked > streamAggMaxTracked {
		t.Errorf("tracked %d buffers, cap is %d", tracked, streamAggMaxTracked)
	}

	// Eviction must have emitted something rather than silently dropping it.
	select {
	case <-out:
	case <-time.After(2 * time.Second):
		t.Error("eviction dropped a buffer without emitting it")
	}
}

func TestStreamAggregatorIgnoresEmptyAndConcurrentUse(t *testing.T) {
	agg, out := collectFinalized()

	agg.add("", "", "", "", []byte("data: x\n\n"))  // no request id
	agg.add("req-x", "", "", "", nil)                // no body
	agg.add("req-x", "", "", "", []byte{})           // empty body

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				agg.add("req-race", "", "m", "openai", []byte("data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n"))
			}
		}()
	}
	wg.Wait()
	agg.add("req-race", "", "", "", []byte("data: [DONE]\n\n"))

	select {
	case <-out:
	case <-time.After(2 * time.Second):
		t.Fatal("concurrent adds never produced a finalised buffer")
	}
	agg.close()

	// Nothing should be emitted for the malformed inputs.
	select {
	case b := <-out:
		t.Errorf("unexpected emit for request %q", b.requestID)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestStreamAggregatorCloseFlushesInFlight(t *testing.T) {
	agg, out := collectFinalized()
	agg.add("req-open", "", "m", "openai", []byte("data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n"))

	agg.close()

	select {
	case b := <-out:
		if b.requestID != "req-open" {
			t.Errorf("flushed %q, want req-open", b.requestID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("close did not flush the in-flight buffer")
	}
	// Double close must be safe.
	agg.close()
}

func TestSummariseStreamMasksSecrets(t *testing.T) {
	agg, out := collectFinalized()
	defer agg.close()

	agg.add("req-sec", "", "m", "openai",
		[]byte("data: {\"choices\":[{\"delta\":{\"content\":\"sk-abcdefghijklmnop\"}}]}\n\n"))
	agg.add("req-sec", "", "", "", []byte("data: [DONE]\n\n"))

	select {
	case b := <-out:
		body, _, _ := summariseStream(b, reqLogDefaultMaxBodyBytes)
		if strings.Contains(body, "sk-abcdefghijklmnop") {
			t.Errorf("secret leaked into stored body: %s", body)
		}
		if !strings.Contains(body, secretPlaceholder) {
			t.Errorf("expected the redaction placeholder, got: %s", body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stream was never finalised")
	}
}

func TestStreamAggregatorJoinsRealisticOpenAIStream(t *testing.T) {
	agg, out := collectFinalized()
	defer agg.close()

	// Shaped exactly like real OpenAI SSE: every chunk carries
	// "finish_reason":null, and only the last one carries a real reason.
	chunks := []string{
		`data: {"id":"c1","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}` + "\n\n",
		`data: {"id":"c1","choices":[{"index":0,"delta":{"content":"The"},"finish_reason":null}]}` + "\n\n",
		`data: {"id":"c1","choices":[{"index":0,"delta":{"content":" answer"},"finish_reason":null}]}` + "\n\n",
		`data: {"id":"c1","choices":[{"index":0,"delta":{"content":" is 42."},"finish_reason":null}]}` + "\n\n",
		`data: {"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n",
		"data: [DONE]\n\n",
	}
	for _, ch := range chunks {
		agg.add("req-real", "", "gpt-4o", "openai", []byte(ch))
	}

	select {
	case b := <-out:
		body, _, _ := summariseStream(b, reqLogDefaultMaxBodyBytes)
		var got streamSummary
		if err := json.Unmarshal([]byte(body), &got); err != nil {
			t.Fatalf("bad JSON: %v", err)
		}
		if got.Content != "The answer is 42." {
			t.Errorf("content = %q, want %q", got.Content, "The answer is 42.")
		}
		// Finalisation happens on the chunk carrying the real finish_reason,
		// so the trailing [DONE] is not part of this buffer.
		if got.Chunks != 5 {
			t.Errorf("chunks = %d, want 5 (finalised on the finish_reason chunk)", got.Chunks)
		}
		if got.FinishReason != "stop" {
			t.Errorf("finish_reason = %q, want stop", got.FinishReason)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("realistic OpenAI stream was never finalised")
	}
}

func TestStreamAggregatorHandlesConcatenatedJSON(t *testing.T) {
	// Exactly the shape measured live on this host: OpenAI-style chunks
	// concatenated with NO separator and NO "data:" prefix. The old
	// line-splitting parser stored empty content for every one of these.
	stream := `{"id":"cmb-1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}],"usage":null}` +
		`{"id":"cmb-1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"Xin"},"finish_reason":null}]}` +
		`{"id":"cmb-1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":" chao"},"finish_reason":null}]}` +
		`{"id":"cmb-1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":null}`

	agg, out := collectFinalized()
	defer agg.close()
	agg.add("req-cat", "", "fast", "openai", []byte(stream))

	select {
	case b := <-out:
		body, _, _ := summariseStream(b, reqLogDefaultMaxBodyBytes)
		var got streamSummary
		if err := json.Unmarshal([]byte(body), &got); err != nil {
			t.Fatalf("bad JSON: %v", err)
		}
		if got.Content != "Xin chao" {
			t.Errorf("content = %q, want %q", got.Content, "Xin chao")
		}
		if got.FinishReason != "stop" {
			t.Errorf("finish_reason = %q, want stop", got.FinishReason)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("concatenated JSON stream was not finalised")
	}
}

func TestStreamAggregatorKeepsReasoningWhenNoVisibleContent(t *testing.T) {
	// Reasoning models on this host stream reasoning_content before (and
	// sometimes instead of) content. Storing an empty body for those was the
	// live symptom this covers.
	stream := `{"choices":[{"delta":{"reasoning_content":"Let me think"},"finish_reason":null}]}` +
		`{"choices":[{"delta":{"reasoning_content":" about this"},"finish_reason":null}]}` +
		`{"choices":[{"delta":{},"finish_reason":"stop"}]}`

	agg, out := collectFinalized()
	defer agg.close()
	agg.add("req-reason", "", "fast", "openai", []byte(stream))

	select {
	case b := <-out:
		body, _, _ := summariseStream(b, reqLogDefaultMaxBodyBytes)
		var got streamSummary
		if err := json.Unmarshal([]byte(body), &got); err != nil {
			t.Fatalf("bad JSON: %v", err)
		}
		if got.Content != "Let me think about this" {
			t.Errorf("content = %q, want the reasoning fallback", got.Content)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("reasoning-only stream was not finalised")
	}
}

func TestScanObjectEnd(t *testing.T) {
	// Braces inside strings must not confuse the scanner.
	got := []string{}
	raw := []byte(`{"a":"}{"} {"b":1} {"c":"x"} [DONE]`)
	for i := 0; i < len(raw); {
		if raw[i] != '{' {
			i++
			continue
		}
		end := scanObjectEnd(raw, i)
		if end < 0 {
			break
		}
		got = append(got, string(raw[i:end]))
		i = end
	}
	want := []string{`{"a":"}{"}`, `{"b":1}`, `{"c":"x"}`}
	if len(got) != len(want) {
		t.Fatalf("got %d objects %v, want %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("object %d = %q, want %q", i, got[i], want[i])
		}
	}

	// An incomplete trailing object must report -1, not a bogus end.
	if end := scanObjectEnd([]byte(`{"partial":`), 0); end != -1 {
		t.Errorf("scanObjectEnd on an incomplete object = %d, want -1", end)
	}
}

func TestStreamAggregatorCarriesPartialObjectsAcrossChunks(t *testing.T) {
	// A JSON object split across two chunks must be parsed once complete, not
	// dropped. This is the case a naive per-chunk parse would lose.
	agg, out := collectFinalized()
	defer agg.close()

	agg.add("req-split", "", "m", "openai", []byte(`{"choices":[{"delta":{"cont`))
	agg.add("req-split", "", "", "", []byte(`ent":"split ok"},"finish_reason":null}]}`))
	agg.add("req-split", "", "", "", []byte(`{"choices":[{"delta":{},"finish_reason":"stop"}]}`))

	select {
	case b := <-out:
		body, _, _ := summariseStream(b, reqLogDefaultMaxBodyBytes)
		var got streamSummary
		if err := json.Unmarshal([]byte(body), &got); err != nil {
			t.Fatalf("bad JSON: %v", err)
		}
		if got.Content != "split ok" {
			t.Errorf("content = %q, want %q", got.Content, "split ok")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("split stream was never finalised")
	}
}

func TestStreamAggregatorCapturesToolCalls(t *testing.T) {
	// Measured live: the majority of streamed turns on this host are agent-loop
	// turns that finish with finish_reason=tool_calls and carry no text at all.
	// The response must still record which tools were requested.
	stream := `{"choices":[{"delta":{"role":"assistant"},"finish_reason":null}]}` +
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"terminal","arguments":""}}]},"finish_reason":null}]}` +
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"cmd\":\"ls\"}"}}]},"finish_reason":null}]}` +
		`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`

	agg, out := collectFinalized()
	defer agg.close()
	agg.add("req-tool", "", "fast", "openai", []byte(stream))

	select {
	case b := <-out:
		body, _, _ := summariseStream(b, reqLogDefaultMaxBodyBytes)
		var got streamSummary
		if err := json.Unmarshal([]byte(body), &got); err != nil {
			t.Fatalf("bad JSON: %v", err)
		}
		if len(got.ToolCalls) != 1 || got.ToolCalls[0] != "terminal" {
			t.Errorf("tool_calls = %v, want [terminal]", got.ToolCalls)
		}
		if got.FinishReason != "tool_calls" {
			t.Errorf("finish_reason = %q, want tool_calls", got.FinishReason)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("tool-call stream was never finalised")
	}
}

func TestIsStreamTerminator(t *testing.T) {
	cases := map[string]bool{
		"data: [DONE]\n\n":                      true,
		"data: {\"type\":\"message_stop\"}\n\n": true,
		"data: {\"delta\":{\"stop_reason\":\"end_turn\"}}\n": true,
		"data: {\"choices\":[{\"finish_reason\":\"stop\"}]}\n": true,
		"data: {\"delta\":{\"text\":\"a\"}}\n":   false,
		"":                                      false,
		"data: [DONEISH]\n":                     false,
		// Regression: OpenAI puts "finish_reason":null on EVERY chunk. Matching
		// the bare key finalised the stream on chunk 1 and stored empty content.
		"data: {\"choices\":[{\"delta\":{\"content\":\"Hi\"},\"finish_reason\":null}]}\n": false,
		"data: {\"delta\":{\"stop_reason\":null}}\n":                                    false,
	}
	for in, want := range cases {
		if got := isStreamTerminator([]byte(in)); got != want {
			t.Errorf("isStreamTerminator(%q) = %v, want %v", in, got, want)
		}
	}
}
