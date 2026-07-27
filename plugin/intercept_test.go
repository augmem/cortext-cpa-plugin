package main

import (
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/tidwall/gjson"
)

func TestRequestInjectsMemoryAfterIngest(t *testing.T) {
	dir := t.TempDir()
	cfg := DefaultConfig()
	cfg.DataDir = dir
	cfg.InterruptGate = false
	cfg.AutoConsolidate = false
	svc := NewService(cfg)

	// Seed via first request
	body1 := []byte(`{"messages":[{"role":"user","content":"Bailey loves tennis balls."}]}`)
	h := http.Header{}
	h.Set("X-Cortext-Session", "sess-1")
	resp := svc.HandleRequestBeforeAuth(pluginapi.RequestInterceptRequest{
		SourceFormat: fmtOpenAI,
		Body:         body1,
		Headers:      h,
	})
	// Second request should recall
	body2 := []byte(`{"messages":[{"role":"user","content":"What does Bailey love?"}]}`)
	resp = svc.HandleRequestBeforeAuth(pluginapi.RequestInterceptRequest{
		SourceFormat: fmtOpenAI,
		Body:         body2,
		Headers:      h,
	})
	if len(resp.Body) == 0 {
		t.Fatal("expected rewritten body with memory")
	}
	sys := gjson.GetBytes(resp.Body, "messages.0.content").String()
	if !strings.Contains(sys, "<cortext_memory>") {
		t.Fatalf("expected memory injection, got %s", resp.Body)
	}
	// stub engine keyword recall should surface tennis
	if !strings.Contains(strings.ToLower(sys), "tennis") {
		t.Fatalf("expected recalled tennis fact in %q", sys)
	}
}

func TestScopeIsolation(t *testing.T) {
	dir := t.TempDir()
	cfg := DefaultConfig()
	cfg.DataDir = dir
	cfg.AutoConsolidate = false
	svc := NewService(cfg)

	h1 := http.Header{}
	h1.Set("X-Cortext-Session", "A")
	h2 := http.Header{}
	h2.Set("X-Cortext-Session", "B")

	svc.HandleRequestBeforeAuth(pluginapi.RequestInterceptRequest{
		SourceFormat: fmtOpenAI,
		Headers:      h1,
		Body:         []byte(`{"messages":[{"role":"user","content":"Secret for session A only: red-balloon-42"}]}`),
	})

	resp := svc.HandleRequestBeforeAuth(pluginapi.RequestInterceptRequest{
		SourceFormat: fmtOpenAI,
		Headers:      h2,
		Body:         []byte(`{"messages":[{"role":"user","content":"What is the secret?"}]}`),
	})
	// Session B should not see A's fact via stub keyword match of "secret" alone maybe —
	// the durable text is only in scope A. Stub recall searches only its own rows.
	if len(resp.Body) > 0 && strings.Contains(string(resp.Body), "red-balloon-42") {
		t.Fatalf("session B leaked session A memory: %s", resp.Body)
	}
}

func TestResponseIngest(t *testing.T) {
	dir := t.TempDir()
	cfg := DefaultConfig()
	cfg.DataDir = dir
	cfg.AutoConsolidate = false
	svc := NewService(cfg)
	h := http.Header{}
	h.Set("X-Cortext-Session", "r1")

	svc.HandleResponse(pluginapi.ResponseInterceptRequest{
		SourceFormat:   fmtOpenAI,
		RequestHeaders: h,
		Body:           []byte(`{"choices":[{"message":{"content":"I am the assistant reply about zebras."}}]}`),
	})

	resp := svc.HandleRequestBeforeAuth(pluginapi.RequestInterceptRequest{
		SourceFormat: fmtOpenAI,
		Headers:      h,
		Body:         []byte(`{"messages":[{"role":"user","content":"zebras"}]}`),
	})
	if len(resp.Body) == 0 || !strings.Contains(strings.ToLower(string(resp.Body)), "zebra") {
		t.Fatalf("expected assistant fact recalled, got %s", resp.Body)
	}
}

func TestStreamResidualFlushOnDone(t *testing.T) {
	dir := t.TempDir()
	cfg := DefaultConfig()
	cfg.DataDir = dir
	cfg.AutoConsolidate = false
	cfg.InterruptGate = false
	svc := NewService(cfg)
	h := http.Header{}
	h.Set("X-Cortext-Session", "stream-1")

	// Short delta under segment threshold, no sentence punctuation.
	short := "unique-needle-quokka-77"
	svc.HandleStreamChunk(pluginapi.StreamChunkInterceptRequest{
		SourceFormat:   fmtOpenAI,
		RequestHeaders: h,
		ChunkIndex:     0,
		Body:           []byte(`{"choices":[{"delta":{"content":"` + short + `"}}]}`),
	})
	// Explicit SSE end must flush residual into durable store.
	svc.HandleStreamChunk(pluginapi.StreamChunkInterceptRequest{
		SourceFormat:   fmtOpenAI,
		RequestHeaders: h,
		ChunkIndex:     1,
		Body:           []byte("data: [DONE]"),
	})

	resp := svc.HandleRequestBeforeAuth(pluginapi.RequestInterceptRequest{
		SourceFormat: fmtOpenAI,
		Headers:      h,
		Body:         []byte(`{"messages":[{"role":"user","content":"quokka"}]}`),
	})
	if len(resp.Body) == 0 || !strings.Contains(string(resp.Body), short) {
		t.Fatalf("expected residual stream fact after [DONE], got %s", resp.Body)
	}
}

// A coalesced final chunk (delta + [DONE] in one payload) must not lose the
// trailing delta.
func TestStreamDoneCoalescedFinalDelta(t *testing.T) {
	dir := t.TempDir()
	cfg := DefaultConfig()
	cfg.DataDir = dir
	cfg.AutoConsolidate = false
	cfg.InterruptGate = false
	svc := NewService(cfg)
	h := http.Header{}
	h.Set("X-Cortext-Session", "stream-coal")

	svc.HandleStreamChunk(pluginapi.StreamChunkInterceptRequest{
		SourceFormat:   fmtOpenAI,
		RequestHeaders: h,
		ChunkIndex:     0,
		Body: []byte("data: {\"choices\":[{\"delta\":{\"content\":\"tail-needle-wombat-31\"}}]}\n" +
			"data: [DONE]"),
	})

	resp := svc.HandleRequestBeforeAuth(pluginapi.RequestInterceptRequest{
		SourceFormat: fmtOpenAI,
		Headers:      h,
		Body:         []byte(`{"messages":[{"role":"user","content":"wombat"}]}`),
	})
	if len(resp.Body) == 0 || !strings.Contains(string(resp.Body), "tail-needle-wombat-31") {
		t.Fatalf("coalesced final delta lost: %s", resp.Body)
	}
}

// Non-OpenAI terminators must also trigger the residual flush.
func TestStreamDoneTerminators(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"openai DONE", "data: [DONE]"},
		{"claude message_stop", "event: message_stop\ndata: {\"type\":\"message_stop\"}"},
		{"responses completed", "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_t\"}}"},
		{"responses failed", "event: response.failed\ndata: {\"type\":\"response.failed\"}"},
		{"gemini finishReason", "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"\"}]},\"finishReason\":\"STOP\"}]}"},
		{"gemini envelope finishReason", "data: {\"response\":{\"candidates\":[{\"finishReason\":\"STOP\"}]}}"},
		{"openai finish_reason", "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			cfg := DefaultConfig()
			cfg.DataDir = dir
			cfg.AutoConsolidate = false
			cfg.InterruptGate = false
			svc := NewService(cfg)
			h := http.Header{}
			h.Set("X-Cortext-Session", "term-"+tc.name)

			svc.HandleStreamChunk(pluginapi.StreamChunkInterceptRequest{
				SourceFormat:   fmtOpenAI,
				RequestHeaders: h,
				ChunkIndex:     0,
				Body:           []byte(`{"choices":[{"delta":{"content":"term-needle-` + tc.name + `-55"}}]}`),
			})
			svc.HandleStreamChunk(pluginapi.StreamChunkInterceptRequest{
				SourceFormat:   fmtOpenAI,
				RequestHeaders: h,
				ChunkIndex:     1,
				Body:           []byte(tc.body),
			})

			entries, err := os.ReadDir(dir)
			if err != nil || len(entries) == 0 {
				t.Fatalf("no store created; residual never flushed for %s", tc.name)
			}
		})
	}
}

// Stream-init chunks must route to resetStreamBuf (retry contamination wiring).
// A re-init while a byte-identical stream still holds unflushed text means
// two in-flight streams share one key and cannot be told apart: the buffer is
// poisoned (conflicted) instead of reset, and nothing interleaved is durably
// ingested. The completed response body carries the content instead.
func TestStreamHeaderInitResetsBuffer(t *testing.T) {
	dir := t.TempDir()
	cfg := DefaultConfig()
	cfg.DataDir = dir
	cfg.AutoConsolidate = false
	cfg.InterruptGate = false
	svc := NewService(cfg)
	h := http.Header{}
	h.Set("X-Cortext-Session", "init-reset")
	body := []byte(`{"messages":[{"role":"user","content":"retry question"}]}`)

	// Use the same ScopeKey production stream handling will derive.
	scope := svc.store.ScopeKey(ResolveScopeIDs(cfg, h, body, nil))
	skey := streamKey(scope, body)
	appendStreamDelta(skey, deltaText, "aborted partial")
	svc.HandleStreamChunk(pluginapi.StreamChunkInterceptRequest{
		SourceFormat:    fmtOpenAI,
		RequestHeaders:  h,
		OriginalRequest: body,
		ChunkIndex:      pluginapi.StreamChunkHeaderInitIndex,
	})
	if !streamConflicted(skey) {
		t.Fatal("re-init over unflushed text must mark the buffer conflicted")
	}
	clearStreamBuf(skey)

	// A fresh init with no in-flight buffer starts clean (no conflict).
	svc.HandleStreamChunk(pluginapi.StreamChunkInterceptRequest{
		SourceFormat:    fmtOpenAI,
		RequestHeaders:  h,
		OriginalRequest: body,
		ChunkIndex:      pluginapi.StreamChunkHeaderInitIndex,
	})
	if streamConflicted(skey) {
		t.Fatal("init with no in-flight buffer must not conflict")
	}
	clearStreamBuf(skey)
}

// Noise streams (no text deltas, no terminator) must not create a SQLite file.
func TestNoiseStreamCreatesNoStore(t *testing.T) {
	dir := t.TempDir()
	cfg := DefaultConfig()
	cfg.DataDir = dir
	cfg.AutoConsolidate = false
	svc := NewService(cfg)
	h := http.Header{}
	h.Set("X-Cortext-Session", "noise")

	svc.HandleStreamChunk(pluginapi.StreamChunkInterceptRequest{
		SourceFormat:   fmtOpenAI,
		RequestHeaders: h,
		ChunkIndex:     0,
		Body:           []byte(`{"choices":[{"delta":{"tool_calls":[{"index":0}]}}]}`),
	})
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("noise stream created store files: %v", entries)
	}
}

// Reasoning deltas must not be durable-ingested when ingest_reasoning=false,
// even though ingest_assistant=true.
func TestReasoningNotIngestedWhenDisabled(t *testing.T) {
	dir := t.TempDir()
	cfg := DefaultConfig()
	cfg.DataDir = dir
	cfg.AutoConsolidate = false
	cfg.InterruptGate = false
	cfg.IngestAssistant = true
	cfg.IngestReasoning = false
	svc := NewService(cfg)
	h := http.Header{}
	h.Set("X-Cortext-Session", "reason-off")

	svc.HandleStreamChunk(pluginapi.StreamChunkInterceptRequest{
		SourceFormat:   fmtOpenAI,
		RequestHeaders: h,
		ChunkIndex:     0,
		Body:           []byte(`{"choices":[{"delta":{"reasoning_content":"secret-reasoning-needle-88."}}]}`),
	})
	resp := svc.HandleRequestBeforeAuth(pluginapi.RequestInterceptRequest{
		SourceFormat: fmtOpenAI,
		Headers:      h,
		Body:         []byte(`{"messages":[{"role":"user","content":"reasoning-needle"}]}`),
	})
	if len(resp.Body) > 0 && strings.Contains(string(resp.Body), "secret-reasoning-needle-88") {
		t.Fatalf("reasoning ingested despite ingest_reasoning=false: %s", resp.Body)
	}
}

func TestMultiFormatInject(t *testing.T) {
	dir := t.TempDir()
	cfg := DefaultConfig()
	cfg.DataDir = dir
	cfg.AutoConsolidate = false
	cfg.InterruptGate = false
	svc := NewService(cfg)

	cases := []struct {
		name   string
		format string
		seed   []byte
		probe  []byte
		hdr    string
	}{
		{
			name:   "openai",
			format: fmtOpenAI,
			seed:   []byte(`{"messages":[{"role":"user","content":"Fact: purple-lemur-91 lives here."}]}`),
			probe:  []byte(`{"messages":[{"role":"user","content":"purple-lemur"}]}`),
			hdr:    "mf-openai",
		},
		{
			name:   "claude",
			format: fmtClaude,
			seed:   []byte(`{"system":"s","messages":[{"role":"user","content":"Fact: purple-lemur-91 lives here."}]}`),
			probe:  []byte(`{"system":"s","messages":[{"role":"user","content":"purple-lemur"}]}`),
			hdr:    "mf-claude",
		},
		{
			name:   "gemini",
			format: fmtGemini,
			seed:   []byte(`{"contents":[{"role":"user","parts":[{"text":"Fact: purple-lemur-91 lives here."}]}]}`),
			probe:  []byte(`{"contents":[{"role":"user","parts":[{"text":"purple-lemur"}]}]}`),
			hdr:    "mf-gemini",
		},
		{
			name:   "openai-response",
			format: fmtOpenAIResponse,
			seed:   []byte(`{"instructions":"s","input":"Fact: purple-lemur-91 lives here."}`),
			probe:  []byte(`{"instructions":"s","input":"purple-lemur"}`),
			hdr:    "mf-oai-resp",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := http.Header{}
			h.Set("X-Cortext-Session", tc.hdr)
			svc.HandleRequestBeforeAuth(pluginapi.RequestInterceptRequest{
				SourceFormat: tc.format,
				Headers:      h,
				Body:         tc.seed,
			})
			resp := svc.HandleRequestBeforeAuth(pluginapi.RequestInterceptRequest{
				SourceFormat: tc.format,
				Headers:      h,
				Body:         tc.probe,
			})
			if len(resp.Body) == 0 {
				t.Fatal("expected inject rewrite")
			}
			if !strings.Contains(string(resp.Body), "<cortext_memory>") {
				t.Fatalf("missing memory fence in %s", resp.Body)
			}
			if !strings.Contains(string(resp.Body), "purple-lemur-91") {
				t.Fatalf("missing recalled fact in %s", resp.Body)
			}
		})
	}
}

func TestDurabilityAcrossServiceReopen(t *testing.T) {
	dir := t.TempDir()
	cfg := DefaultConfig()
	cfg.DataDir = dir
	cfg.AutoConsolidate = false
	cfg.InterruptGate = false

	svc1 := NewService(cfg)
	h := http.Header{}
	h.Set("X-Cortext-Session", "dur-1")
	svc1.HandleRequestBeforeAuth(pluginapi.RequestInterceptRequest{
		SourceFormat: fmtOpenAI,
		Headers:      h,
		Body:         []byte(`{"messages":[{"role":"user","content":"Remember durable-token-axolotl-33."}]}`),
	})
	svc1.Shutdown()

	svc2 := NewService(cfg)
	resp := svc2.HandleRequestBeforeAuth(pluginapi.RequestInterceptRequest{
		SourceFormat: fmtOpenAI,
		Headers:      h,
		Body:         []byte(`{"messages":[{"role":"user","content":"axolotl"}]}`),
	})
	if len(resp.Body) == 0 || !strings.Contains(string(resp.Body), "axolotl-33") {
		t.Fatalf("expected durable recall after reopen, got %s", resp.Body)
	}

	h2 := http.Header{}
	h2.Set("X-Cortext-Session", "dur-2")
	svc2.HandleRequestBeforeAuth(pluginapi.RequestInterceptRequest{
		SourceFormat: fmtOpenAI,
		Headers:      h2,
		Body:         []byte(`{"messages":[{"role":"user","content":"other-session-fact-99"}]}`),
	})
	respB := svc2.HandleRequestBeforeAuth(pluginapi.RequestInterceptRequest{
		SourceFormat: fmtOpenAI,
		Headers:      h,
		Body:         []byte(`{"messages":[{"role":"user","content":"other-session"}]}`),
	})
	if len(respB.Body) > 0 && strings.Contains(string(respB.Body), "other-session-fact-99") {
		t.Fatalf("session leak after reopen: %s", respB.Body)
	}
}
