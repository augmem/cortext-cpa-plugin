package main

import (
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/tidwall/gjson"
)

// controllableEngine lets unit tests force interrupt-gate and durable rows
// without reimplementing production intercept logic.
type controllableEngine struct {
	mu             sync.Mutex
	durable        []string
	interruptOn    bool
	interruptItems []MemoryItem
	ephemeralHits  []MemoryItem
	lastEphemeralQ string
	processCalls   int
}

func (e *controllableEngine) ProcessText(text, source string, retention Retention) (ContextPacket, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.processCalls++
	text = strings.TrimSpace(text)
	if retention == RetentionDurable || retention == RetentionNatural || retention == RetentionBoundary {
		if text != "" {
			e.durable = append(e.durable, text)
		}
	}
	if retention == RetentionEphemeral || retention == RetentionNatural {
		e.lastEphemeralQ = text
		out := ContextPacket{RetrievedMemory: append([]MemoryItem{}, e.ephemeralHits...)}
		if e.interruptOn {
			out.ShouldInterrupt = true
			if len(e.interruptItems) > 0 {
				out.RetrievedMemory = append([]MemoryItem{}, e.interruptItems...)
			}
		}
		return out, nil
	}
	return ContextPacket{}, nil
}

func (e *controllableEngine) Consolidate() error { return nil }
func (e *controllableEngine) Flush() error       { return nil }
func (e *controllableEngine) Close() error       { return nil }

func (e *controllableEngine) durableContains(substr string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, d := range e.durable {
		if strings.Contains(d, substr) {
			return true
		}
	}
	return false
}

func TestAssistantResponseIngestThenNextTurnRecall(t *testing.T) {
	// Proves HandleResponse durable-ingests assistant text and a later
	// request assemble (no assistant text in request body) can inject it.
	dir := t.TempDir()
	cfg := DefaultConfig()
	cfg.DataDir = dir
	cfg.InterruptGate = false
	cfg.AutoConsolidate = false
	cfg.IngestAssistant = true
	svc := NewService(cfg)

	// Use real stub engine path for this case (keyword recall).
	h := http.Header{}
	h.Set("X-Cortext-Session", "asst-gap-1")
	needle := "assistant-needle-zx9q7"
	svc.HandleResponse(pluginapi.ResponseInterceptRequest{
		SourceFormat:   fmtOpenAI,
		RequestHeaders: h,
		Body:           []byte(`{"choices":[{"message":{"content":"Noted: the lab passphrase is ` + needle + `."}}]}`),
	})

	// Next user turn: body does NOT include the assistant needle — only a probe.
	resp := svc.HandleRequestBeforeAuth(pluginapi.RequestInterceptRequest{
		SourceFormat: fmtOpenAI,
		Headers:      h,
		Body:         []byte(`{"messages":[{"role":"user","content":"What is the lab passphrase?"}]}`),
	})
	if len(resp.Body) == 0 {
		t.Fatal("expected inject rewrite after assistant ingest")
	}
	if !strings.Contains(string(resp.Body), needle) {
		t.Fatalf("expected assistant needle in inject, got %s", resp.Body)
	}
	if !strings.Contains(string(resp.Body), "<cortext_memory>") {
		t.Fatal("expected memory fence")
	}
}

func TestStreamResidualAndInterruptGateStagesNextTurn(t *testing.T) {
	// Structural proof of HandleStreamChunk residual flush + InterruptBus stage
	// for the *next* request (not mid-turn revise).
	dir := t.TempDir()
	cfg := DefaultConfig()
	cfg.DataDir = dir
	cfg.InterruptGate = true
	cfg.AutoConsolidate = false
	cfg.IngestAssistant = true
	cfg.IngestReasoning = true

	eng := &controllableEngine{
		interruptOn: true,
		interruptItems: []MemoryItem{
			{Text: "stream-gate-needle-k4m2", Modality: "text"},
		},
	}
	SetEngineFactory(func(dbPath string, c PluginConfig) (Engine, error) {
		_ = dbPath
		_ = c
		return eng, nil
	})
	t.Cleanup(func() { SetEngineFactory(nil) })

	svc := NewService(cfg)
	h := http.Header{}
	h.Set("X-Cortext-Session", "stream-gap-1")

	// Short residual under segment threshold, then DONE flush.
	short := "stream-resid-needle-p8w1"
	svc.HandleStreamChunk(pluginapi.StreamChunkInterceptRequest{
		SourceFormat:   fmtOpenAI,
		RequestHeaders: h,
		ChunkIndex:     0,
		Body:           []byte(`{"choices":[{"delta":{"content":"` + short + `"}}]}`),
	})
	// Segment long enough to flush mid-stream and hit interrupt path.
	long := strings.Repeat("stream segment about operations. ", 8) + "gate-trigger."
	svc.HandleStreamChunk(pluginapi.StreamChunkInterceptRequest{
		SourceFormat:   fmtOpenAI,
		RequestHeaders: h,
		ChunkIndex:     1,
		Body:           []byte(`{"choices":[{"delta":{"content":"` + long + `"}}]}`),
	})
	svc.HandleStreamChunk(pluginapi.StreamChunkInterceptRequest{
		SourceFormat:   fmtOpenAI,
		RequestHeaders: h,
		ChunkIndex:     2,
		Body:           []byte("data: [DONE]"),
	})

	if !eng.durableContains(short) && !eng.durableContains("stream segment") {
		// residual and/or segment must have been durable-ingested
		t.Fatalf("expected stream durable text in engine, durable=%v", eng.durable)
	}

	// Next request should take staged interrupt block into inject.
	resp := svc.HandleRequestBeforeAuth(pluginapi.RequestInterceptRequest{
		SourceFormat: fmtOpenAI,
		Headers:      h,
		Body:         []byte(`{"messages":[{"role":"user","content":"continue"}]}`),
	})
	body := string(resp.Body)
	if !strings.Contains(body, "stream-gate-needle-k4m2") {
		t.Fatalf("expected staged interrupt memory on next turn, got %s", body)
	}
	sys := gjson.GetBytes(resp.Body, "messages.0.content").String()
	if !strings.Contains(sys, "<cortext_memory>") {
		t.Fatalf("expected memory fence from gate stage, got %s", sys)
	}
}

func TestHistoryResubmitIngestsPriorUserAndAssistant(t *testing.T) {
	// Clients resend full transcripts. Prior user + assistant turns must
	// durable-ingest even when the latest user message is unrelated.
	dir := t.TempDir()
	cfg := DefaultConfig()
	cfg.DataDir = dir
	cfg.InterruptGate = false
	cfg.AutoConsolidate = false
	cfg.IngestAssistant = true
	svc := NewService(cfg)
	h := http.Header{}
	h.Set("X-Cortext-Session", "hist-resubmit-1")

	userTok := "hist-user-needle-a7k2"
	asstTok := "hist-asst-needle-b9m3"
	body := []byte(`{"messages":[` +
		`{"role":"user","content":"Prior note: ` + userTok + `."},` +
		`{"role":"assistant","content":"Prior reply records ` + asstTok + `."},` +
		`{"role":"user","content":"Continue setup without repeating secrets."}` +
		`]}`)
	svc.HandleRequestBeforeAuth(pluginapi.RequestInterceptRequest{
		SourceFormat: fmtOpenAI,
		Headers:      h,
		Body:         body,
	})

	// Fresh probe body has neither token — inject must supply them.
	resp := svc.HandleRequestBeforeAuth(pluginapi.RequestInterceptRequest{
		SourceFormat: fmtOpenAI,
		Headers:      h,
		Body:         []byte(`{"messages":[{"role":"user","content":"What prior needles were recorded?"}]}`),
	})
	got := string(resp.Body)
	if !strings.Contains(got, userTok) {
		t.Fatalf("expected prior user token in inject, got %s", got)
	}
	if !strings.Contains(got, asstTok) {
		t.Fatalf("expected prior assistant token in inject, got %s", got)
	}
}

func TestCorrectionSurfacesNewAfterUpdate(t *testing.T) {
	// Two durable user writes; assemble on "current" probe must include the new token.
	// Stub engine is keyword-based — both may surface; new must be present.
	dir := t.TempDir()
	cfg := DefaultConfig()
	cfg.DataDir = dir
	cfg.InterruptGate = false
	cfg.AutoConsolidate = false
	svc := NewService(cfg)
	h := http.Header{}
	h.Set("X-Cortext-Session", "corr-1")
	oldTok := "oldname-unit-111"
	newTok := "newname-unit-222"
	svc.HandleRequestBeforeAuth(pluginapi.RequestInterceptRequest{
		SourceFormat: fmtOpenAI,
		Headers:      h,
		Body:         []byte(`{"messages":[{"role":"user","content":"pet name is ` + oldTok + `"}]}`),
	})
	svc.HandleRequestBeforeAuth(pluginapi.RequestInterceptRequest{
		SourceFormat: fmtOpenAI,
		Headers:      h,
		Body:         []byte(`{"messages":[{"role":"user","content":"correction: pet name is actually ` + newTok + ` not ` + oldTok + `"}]}`),
	})
	resp := svc.HandleRequestBeforeAuth(pluginapi.RequestInterceptRequest{
		SourceFormat: fmtOpenAI,
		Headers:      h,
		Body:         []byte(`{"messages":[{"role":"user","content":"what is the current pet name?"}]}`),
	})
	if !strings.Contains(string(resp.Body), newTok) {
		t.Fatalf("expected new token after correction ingest, got %s", resp.Body)
	}
}

func TestStreamDoneFlushesResidualOnly(t *testing.T) {
	dir := t.TempDir()
	cfg := DefaultConfig()
	cfg.DataDir = dir
	cfg.InterruptGate = false
	cfg.AutoConsolidate = false
	svc := NewService(cfg)
	h := http.Header{}
	h.Set("X-Cortext-Session", "stream-resid-2")
	needle := "resid-only-needle-qq3"
	svc.HandleStreamChunk(pluginapi.StreamChunkInterceptRequest{
		SourceFormat:   fmtOpenAI,
		RequestHeaders: h,
		ChunkIndex:     0,
		Body:           []byte(`{"choices":[{"delta":{"content":"` + needle + `"}}]}`),
	})
	svc.HandleStreamChunk(pluginapi.StreamChunkInterceptRequest{
		SourceFormat:   fmtOpenAI,
		RequestHeaders: h,
		ChunkIndex:     1,
		Body:           []byte("data: [DONE]"),
	})
	resp := svc.HandleRequestBeforeAuth(pluginapi.RequestInterceptRequest{
		SourceFormat: fmtOpenAI,
		Headers:      h,
		Body:         []byte(`{"messages":[{"role":"user","content":"resid-only"}]}`),
	})
	if !strings.Contains(string(resp.Body), needle) {
		t.Fatalf("expected residual stream needle after DONE, got %s", resp.Body)
	}
}
