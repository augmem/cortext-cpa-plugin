//go:build !cortext_native

package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// readStubRows loads the durable rows the stub engine persisted for the
// (single) scope created under dir.
func readStubRows(t *testing.T, dir string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, "*.stub.json"))
	if err != nil || len(matches) == 0 {
		t.Fatalf("no stub store under %s (err=%v)", dir, err)
	}
	b, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatalf("read stub store: %v", err)
	}
	var rows []string
	if err := json.Unmarshal(b, &rows); err != nil {
		t.Fatalf("stub store json: %v", err)
	}
	return rows
}

// A streamed assistant response is durably ingested segment-wise; when the
// host then fires the response interceptor with the assembled full body, the
// full text must NOT be stored a second time (near-duplicate).
func TestStreamThenFullResponseNotDoubleIngested(t *testing.T) {
	dir := t.TempDir()
	cfg := DefaultConfig()
	cfg.DataDir = dir
	cfg.AutoConsolidate = false
	cfg.InterruptGate = false
	svc := NewService(cfg)
	h := http.Header{}
	h.Set("X-Cortext-Session", "dup-1")

	seg1 := "First fact about quokkas."
	seg2 := "Second fact about wombats."
	// Pin the dedupe contract: stream chunks and the response intercept carry
	// byte-identical OriginalRequest, so both hash to the same stream key.
	orig := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	for i, d := range []string{seg1, seg2} {
		svc.HandleStreamChunk(pluginapi.StreamChunkInterceptRequest{
			SourceFormat:    fmtOpenAI,
			RequestHeaders:  h,
			OriginalRequest: orig,
			ChunkIndex:      i,
			Body:            []byte(`{"choices":[{"delta":{"content":"` + d + `"}}]}`),
		})
	}
	svc.HandleStreamChunk(pluginapi.StreamChunkInterceptRequest{
		SourceFormat:    fmtOpenAI,
		RequestHeaders:  h,
		OriginalRequest: orig,
		ChunkIndex:      2,
		Body:            []byte("data: [DONE]"),
	})

	// Host aggregates the stream and fires the response interceptor.
	full := seg1 + " " + seg2
	svc.HandleResponse(pluginapi.ResponseInterceptRequest{
		SourceFormat:    fmtOpenAI,
		RequestHeaders:  h,
		OriginalRequest: orig,
		Body:            []byte(`{"choices":[{"message":{"content":"` + full + `"}}]}`),
	})

	rows := readStubRows(t, dir)
	var hasSeg1, hasSeg2, hasFull bool
	for _, r := range rows {
		switch r {
		case seg1:
			hasSeg1 = true
		case seg2:
			hasSeg2 = true
		case full:
			hasFull = true
		}
	}
	if !hasSeg1 || !hasSeg2 {
		t.Fatalf("stream segments missing from store: %v", rows)
	}
	if hasFull {
		t.Fatalf("assembled full body re-ingested as near-duplicate: %v", rows)
	}
}

// Non-streamed responses (no stream mark) still ingest the full body.
func TestFullResponseIngestWithoutStreamMark(t *testing.T) {
	dir := t.TempDir()
	cfg := DefaultConfig()
	cfg.DataDir = dir
	cfg.AutoConsolidate = false
	cfg.InterruptGate = false
	svc := NewService(cfg)
	h := http.Header{}
	h.Set("X-Cortext-Session", "nostream-1")

	body := "Assistant reply about capybaras without any stream."
	svc.HandleResponse(pluginapi.ResponseInterceptRequest{
		SourceFormat:   fmtOpenAI,
		RequestHeaders: h,
		Body:           []byte(`{"choices":[{"message":{"content":"` + body + `"}}]}`),
	})

	rows := readStubRows(t, dir)
	found := false
	for _, r := range rows {
		if r == body {
			found = true
		}
	}
	if !found {
		t.Fatalf("non-streamed full body not ingested: %v", rows)
	}
}

// Gemini SSE has no [DONE] sentinel: the finishReason chunk is the only end
// signal and must flush the buffered tail into the durable store.
func TestGeminiFinishReasonFlushesStreamTail(t *testing.T) {
	dir := t.TempDir()
	cfg := DefaultConfig()
	cfg.DataDir = dir
	cfg.AutoConsolidate = false
	cfg.InterruptGate = false
	svc := NewService(cfg)
	h := http.Header{}
	h.Set("X-Cortext-Session", "gem-1")

	svc.HandleStreamChunk(pluginapi.StreamChunkInterceptRequest{
		SourceFormat:   fmtGemini,
		RequestHeaders: h,
		ChunkIndex:     0,
		Body:           []byte(`data: {"candidates":[{"content":{"parts":[{"text":"gemini-needle-pangolin-12"}],"role":"model"}}]}`),
	})
	svc.HandleStreamChunk(pluginapi.StreamChunkInterceptRequest{
		SourceFormat:   fmtGemini,
		RequestHeaders: h,
		ChunkIndex:     1,
		Body:           []byte(`data: {"candidates":[{"content":{"parts":[{"text":""}]},"finishReason":"STOP"}]}`),
	})

	rows := readStubRows(t, dir)
	found := false
	for _, r := range rows {
		if strings.Contains(r, "gemini-needle-pangolin-12") {
			found = true
		}
	}
	if !found {
		t.Fatalf("gemini tail never flushed on finishReason: %v", rows)
	}
}

// finishReason detection must not fire on mid-stream chunks.
func TestIsStreamDoneFinishReasonNegatives(t *testing.T) {
	for _, body := range []string{
		`data: {"choices":[{"delta":{"content":"hi"},"finish_reason":null}]}`, // mid-stream null
		`data: {"choices":[],"usage":{"total_tokens":3}}`,                     // usage-only tail
		`data: {"candidates":[{"content":{"parts":[{"text":"partial"}]}}]}`,   // gemini mid-stream
		`data: {"type":"response.output_text.delta","delta":"partial"}`,       // responses mid-stream
	} {
		if IsStreamDone([]byte(body)) {
			t.Errorf("false terminator: %s", body)
		}
	}
}

// A closed engine (eviction/reconfigure race) must not swallow a durable
// ingest: ProcessText fails with errEngineClosed and the seen-map is not
// marked, so the next history resubmit re-ingests the text.
func TestClosedEngineDoesNotSwallowIngest(t *testing.T) {
	dir := t.TempDir()
	cfg := DefaultConfig()
	cfg.DataDir = dir
	cfg.AutoConsolidate = false
	svc := NewService(cfg)
	scope := svc.store.ScopeKey(ScopeIDs{Session: "closed-1"})
	eng, err := svc.store.ForScope(scope)
	if err != nil {
		t.Fatal(err)
	}
	svc.durableIngest(eng, scope, "user", "fact-before-close")
	if err := eng.Close(); err != nil {
		t.Fatal(err)
	}
	svc.durableIngest(eng, scope, "user", "fact-after-close-needle")
	if svc.store.HasSeen(scope, hashText("fact-after-close-needle")) {
		t.Fatal("closed-engine ingest must not mark seen")
	}
	rows := readStubRows(t, dir)
	for _, r := range rows {
		if strings.Contains(r, "fact-after-close-needle") {
			t.Fatalf("closed engine stored text: %v", rows)
		}
	}
}

// Two concurrent byte-identical streams must not interleave into durable
// memory: no segment or residual from the shared key is ingested, and the
// assembled response body is the recovery path.
func TestConflictingIdenticalStreamsNotIngested(t *testing.T) {
	dir := t.TempDir()
	cfg := DefaultConfig()
	cfg.DataDir = dir
	cfg.AutoConsolidate = false
	cfg.InterruptGate = false
	svc := NewService(cfg)
	h := http.Header{}
	h.Set("X-Cortext-Session", "conf-1")
	orig := []byte(`{"model":"m","messages":[{"role":"user","content":"same prompt"}]}`)

	chunk := func(idx int, body string) {
		svc.HandleStreamChunk(pluginapi.StreamChunkInterceptRequest{
			SourceFormat:    fmtOpenAI,
			RequestHeaders:  h,
			OriginalRequest: orig,
			ChunkIndex:      idx,
			Body:            []byte(body),
		})
	}
	// Stream A starts and buffers unflushed text.
	chunk(pluginapi.StreamChunkHeaderInitIndex, "")
	chunk(0, `{"choices":[{"delta":{"content":"alpha-partial"}}]}`)
	// Stream B (identical body) starts while A holds residual: conflict.
	chunk(pluginapi.StreamChunkHeaderInitIndex, "")
	// Both streams keep delivering; all of it must be dropped.
	chunk(1, `{"choices":[{"delta":{"content":"-bravo"}}]}`)
	chunk(2, `{"choices":[{"delta":{"content":"-charlie"}}]}`)
	chunk(3, "data: [DONE]")

	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("conflicted stream ingested interleaved text: %v", entries)
	}

	// Recovery: the host fires the response intercept with the assembled body.
	full := "alpha-partial-bravo-charlie final answer."
	svc.HandleResponse(pluginapi.ResponseInterceptRequest{
		SourceFormat:    fmtOpenAI,
		RequestHeaders:  h,
		OriginalRequest: orig,
		Body:            []byte(`{"choices":[{"message":{"content":"` + full + `"}}]}`),
	})
	rows := readStubRows(t, dir)
	found := false
	for _, r := range rows {
		if r == full {
			found = true
		}
	}
	if !found {
		t.Fatalf("assembled response body not ingested after conflict: %v", rows)
	}
}

// Concurrent identical durable ingests must produce exactly one stored row.
func TestConcurrentIdenticalIngestSingleRow(t *testing.T) {
	dir := t.TempDir()
	cfg := DefaultConfig()
	cfg.DataDir = dir
	cfg.AutoConsolidate = false
	svc := NewService(cfg)
	scope := svc.store.ScopeKey(ScopeIDs{Session: "conc-ingest"})
	eng, err := svc.store.ForScope(scope)
	if err != nil {
		t.Fatal(err)
	}
	const workers = 16
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			svc.durableIngest(eng, scope, "user", "identical-needle-text")
		}()
	}
	wg.Wait()
	rows := readStubRows(t, dir)
	count := 0
	for _, r := range rows {
		if r == "identical-needle-text" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("identical concurrent ingests stored %d rows, want 1", count)
	}
}

// ingest_assistant=false must hold on the history-resubmit path too: clients
// resend full transcripts, so assistant text would otherwise re-enter the
// store through the request interceptor.
func TestAssistantHistoryNotIngestedWhenDisabled(t *testing.T) {
	dir := t.TempDir()
	cfg := DefaultConfig()
	cfg.DataDir = dir
	cfg.AutoConsolidate = false
	cfg.InterruptGate = false
	cfg.IngestAssistant = false
	svc := NewService(cfg)
	h := http.Header{}
	h.Set("X-Cortext-Session", "noasst-1")

	svc.HandleRequestBeforeAuth(pluginapi.RequestInterceptRequest{
		SourceFormat: fmtOpenAI,
		Headers:      h,
		Body: []byte(`{"messages":[` +
			`{"role":"user","content":"user fact armadillo-9"},` +
			`{"role":"assistant","content":"assistant needle zzz-123"},` +
			`{"role":"developer","content":"dev scaffolding xyz-7"},` +
			`{"role":"user","content":"current question"}` +
			`]}`),
	})

	rows := readStubRows(t, dir)
	var userStored bool
	for _, r := range rows {
		if strings.Contains(r, "zzz-123") {
			t.Fatalf("assistant history ingested despite ingest_assistant=false: %v", rows)
		}
		if strings.Contains(r, "xyz-7") {
			t.Fatalf("developer scaffolding ingested: %v", rows)
		}
		if strings.Contains(r, "armadillo-9") {
			userStored = true
		}
	}
	if !userStored {
		t.Fatalf("user history missing from store: %v", rows)
	}
}

// A scope reopened after a material reconfigure must wait for the previous
// engine's detached close: the old handle is fully closed before ForScope
// returns the new one, so a slow close cannot clobber fresh writes.
func TestReopenWaitsForPendingClose(t *testing.T) {
	dir := t.TempDir()
	cfg := DefaultConfig()
	cfg.DataDir = dir
	cfg.AutoConsolidate = false
	st := NewStore(cfg)
	defer st.DisposeAll()
	scope := st.ScopeKey(ScopeIDs{Session: "reopen-1"})
	eng1, err := st.ForScope(scope)
	if err != nil {
		t.Fatal(err)
	}
	stub1 := eng1.(*stubEngine)

	// Material change drains engines and closes them on a detached goroutine.
	cfg2 := cfg
	cfg2.Focus = 0.9
	st.Reconfigure(cfg2)

	eng2, err := st.ForScope(scope)
	if err != nil {
		t.Fatal(err)
	}
	if eng2 == eng1 {
		t.Fatal("expected a fresh engine after material reconfigure")
	}
	stub1.mu.Lock()
	closed := stub1.closed
	stub1.mu.Unlock()
	if !closed {
		t.Fatal("ForScope returned before the previous engine finished closing")
	}
}

// A ForScope open must wait for a detached EVICTION close of the same scope
// file (not just reconfigure closes): reopening while the old engine is
// mid-close can clobber fresh writes.
func TestForScopeWaitsForEvictedClose(t *testing.T) {
	dir := t.TempDir()
	cfg := DefaultConfig()
	cfg.DataDir = dir
	cfg.AutoConsolidate = false
	st := NewStore(cfg)
	defer st.DisposeAll()
	scope := st.ScopeKey(ScopeIDs{Session: "evict-close"})
	eng1, err := st.ForScope(scope)
	if err != nil {
		t.Fatal(err)
	}
	stub1 := eng1.(*stubEngine)

	// Simulate an eviction: drop from live maps, register the pending close,
	// and close slowly in the background.
	st.mu.Lock()
	for i, k := range st.order {
		if k == scope {
			st.order = append(st.order[:i], st.order[i+1:]...)
			break
		}
	}
	delete(st.engines, scope)
	st.closings[scope] = make(chan struct{})
	st.mu.Unlock()
	go func() {
		time.Sleep(100 * time.Millisecond)
		st.closeEngines(cfg, []doomedEngine{{key: scope, eng: eng1}})
	}()

	start := time.Now()
	eng2, err := st.ForScope(scope)
	if err != nil {
		t.Fatal(err)
	}
	if eng2 == eng1 {
		t.Fatal("expected a fresh engine after eviction")
	}
	stub1.mu.Lock()
	closed := stub1.closed
	stub1.mu.Unlock()
	if !closed {
		t.Fatalf("ForScope returned after %v without waiting for the pending close", time.Since(start))
	}
}

// Streamed tool-call argument JSON is buffered whole and ingested once at
// stream end — never fragmented by the prose segmenter.
func TestStreamToolArgsIngestedWhole(t *testing.T) {
	dir := t.TempDir()
	cfg := DefaultConfig()
	cfg.DataDir = dir
	cfg.AutoConsolidate = false
	cfg.InterruptGate = false
	svc := NewService(cfg)
	h := http.Header{}
	h.Set("X-Cortext-Session", "toolargs-1")

	// Argument JSON full of sentence punctuation and >120 bytes: would
	// fragment badly through the prose segmenter.
	arg := `{"query": "what is 3.14? Tell me. Now! ` + strings.Repeat("x", 150) + `"}`
	mid := len(arg) / 2
	chunk := func(idx int, part string) {
		svc.HandleStreamChunk(pluginapi.StreamChunkInterceptRequest{
			SourceFormat:   fmtClaude,
			RequestHeaders: h,
			ChunkIndex:     idx,
			Body: []byte(`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":` +
				strconv.Quote(part) + `}}`),
		})
	}
	chunk(0, arg[:mid])
	chunk(1, arg[mid:])
	svc.HandleStreamChunk(pluginapi.StreamChunkInterceptRequest{
		SourceFormat:   fmtClaude,
		RequestHeaders: h,
		ChunkIndex:     2,
		Body:           []byte("event: message_stop\ndata: {\"type\":\"message_stop\"}"),
	})

	rows := readStubRows(t, dir)
	want := "[tool call] " + arg
	found := false
	for _, r := range rows {
		if r == want {
			found = true
		}
		if strings.Contains(r, `"query"`) && r != want {
			t.Fatalf("tool args fragmented into prose segments: %v", rows)
		}
	}
	if !found {
		t.Fatalf("assembled tool args missing from store: %v", rows)
	}
}

// Two inits before ANY delta (duplicate POSTs racing) must also poison:
// both streams' deltas would otherwise interleave into durable memory.
func TestConflictingInitsBeforeDeltasNotIngested(t *testing.T) {
	dir := t.TempDir()
	cfg := DefaultConfig()
	cfg.DataDir = dir
	cfg.AutoConsolidate = false
	cfg.InterruptGate = false
	svc := NewService(cfg)
	h := http.Header{}
	h.Set("X-Cortext-Session", "conf-init-1")
	orig := []byte(`{"model":"m","messages":[{"role":"user","content":"same prompt"}]}`)

	chunk := func(idx int, body string) {
		svc.HandleStreamChunk(pluginapi.StreamChunkInterceptRequest{
			SourceFormat:    fmtOpenAI,
			RequestHeaders:  h,
			OriginalRequest: orig,
			ChunkIndex:      idx,
			Body:            []byte(body),
		})
	}
	// Both streams init before either sends a delta.
	chunk(pluginapi.StreamChunkHeaderInitIndex, "")
	chunk(pluginapi.StreamChunkHeaderInitIndex, "")
	// Interleaved deltas from two different responses.
	chunk(0, `{"choices":[{"delta":{"content":"alpha-beta-gamma-delta alpha-beta-gam"}}]}`)
	chunk(1, `{"choices":[{"delta":{"content":"ONE-TWO-THREE-FOUR-FIVE ONE-TWO-THRE"}}]}`)
	chunk(2, "data: [DONE]")

	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("init-init conflict ingested interleaved text: %v", entries)
	}
}

// Conflict is sticky past the first [DONE]: the sibling stream's late deltas
// stay dropped and the assembled full body remains the recovery path.
func TestConflictStickyAfterDone(t *testing.T) {
	dir := t.TempDir()
	cfg := DefaultConfig()
	cfg.DataDir = dir
	cfg.AutoConsolidate = false
	cfg.InterruptGate = false
	svc := NewService(cfg)
	h := http.Header{}
	h.Set("X-Cortext-Session", "sticky-1")
	orig := []byte(`{"model":"m","messages":[{"role":"user","content":"same prompt"}]}`)

	chunk := func(idx int, body string) {
		svc.HandleStreamChunk(pluginapi.StreamChunkInterceptRequest{
			SourceFormat:    fmtOpenAI,
			RequestHeaders:  h,
			OriginalRequest: orig,
			ChunkIndex:      idx,
			Body:            []byte(body),
		})
	}
	chunk(pluginapi.StreamChunkHeaderInitIndex, "") // stream A starts
	chunk(0, `{"choices":[{"delta":{"content":"HEAD-of-response "}}]}`)
	chunk(pluginapi.StreamChunkHeaderInitIndex, "")                     // stream B starts: conflict
	chunk(1, "data: [DONE]")                                            // stream A ends
	chunk(2, `{"choices":[{"delta":{"content":"TAIL-of-response."}}]}`) // B's late delta
	chunk(3, "data: [DONE]")                                            // stream B ends

	// The full assembled body is the recovery path and must be ingested.
	full := "HEAD-of-response TAIL-of-response."
	svc.HandleResponse(pluginapi.ResponseInterceptRequest{
		SourceFormat:    fmtOpenAI,
		RequestHeaders:  h,
		OriginalRequest: orig,
		Body:            []byte(`{"choices":[{"message":{"content":"` + full + `"}}]}`),
	})

	rows := readStubRows(t, dir)
	var hasFull, hasTailOnly bool
	for _, r := range rows {
		if r == full {
			hasFull = true
		}
		if r == "TAIL-of-response." {
			hasTailOnly = true
		}
	}
	if !hasFull {
		t.Fatalf("assembled full body not ingested after sticky conflict: %v", rows)
	}
	if hasTailOnly {
		t.Fatalf("late sibling delta ingested as partial fragment: %v", rows)
	}
}

// An interactions-shaped body with a staged gate block on the scope must
// pass through untouched (no invented messages array).
func TestInteractionsBodyPassesThroughWithStagedBlock(t *testing.T) {
	dir := t.TempDir()
	cfg := DefaultConfig()
	cfg.DataDir = dir
	cfg.AutoConsolidate = false
	svc := NewService(cfg)
	h := http.Header{}
	h.Set("X-Cortext-Session", "ix-1")
	scope := svc.store.ScopeKey(ScopeIDs{Session: "ix-1"})
	svc.store.Bus().Stage(scope, "- staged fact")

	body := []byte(`{"model":"m","input":[{"role":"user","content":"hi"}],"previous_interaction_id":"i-1"}`)
	resp := svc.HandleRequestBeforeAuth(pluginapi.RequestInterceptRequest{
		SourceFormat: "interactions",
		Headers:      h,
		Body:         body,
	})
	if len(resp.Body) != 0 {
		t.Fatalf("interactions body rewritten: %s", resp.Body)
	}
}

// Stream segments stored for a turn must dedupe the next turn's resubmitted
// whole message (whitespace-normalized hash): the store holds the segments
// only, never a second whole-message copy.
func TestWholeMessageResubmitDedupesAgainstSegments(t *testing.T) {
	dir := t.TempDir()
	cfg := DefaultConfig()
	cfg.DataDir = dir
	cfg.AutoConsolidate = false
	cfg.InterruptGate = false
	svc := NewService(cfg)
	h := http.Header{}
	h.Set("X-Cortext-Session", "whole-1")

	seg1 := "Sentence one about quokkas. "
	seg2 := "Sentence two about wombats."
	for i, d := range []string{seg1, seg2} {
		svc.HandleStreamChunk(pluginapi.StreamChunkInterceptRequest{
			SourceFormat:   fmtOpenAI,
			RequestHeaders: h,
			ChunkIndex:     i,
			Body:           []byte(`{"choices":[{"delta":{"content":"` + d + `"}}]}`),
		})
	}
	svc.HandleStreamChunk(pluginapi.StreamChunkInterceptRequest{
		SourceFormat:   fmtOpenAI,
		RequestHeaders: h,
		ChunkIndex:     2,
		Body:           []byte("data: [DONE]"),
	})

	// Next turn resubmits the transcript, assistant message whole (the
	// provider's assembled message is the verbatim concatenation of deltas).
	whole := seg1 + seg2
	svc.HandleRequestBeforeAuth(pluginapi.RequestInterceptRequest{
		SourceFormat: fmtOpenAI,
		Headers:      h,
		Body: []byte(`{"messages":[{"role":"user","content":"q1"},` +
			`{"role":"assistant","content":"` + whole + `"},` +
			`{"role":"user","content":"q2"}]}`),
	})

	rows := readStubRows(t, dir)
	trim1 := strings.TrimSpace(seg1)
	var hasSeg1, hasSeg2, hasWhole bool
	for _, r := range rows {
		switch r {
		case trim1:
			hasSeg1 = true
		case seg2:
			hasSeg2 = true
		case whole:
			hasWhole = true
		}
	}
	if !hasSeg1 || !hasSeg2 {
		t.Fatalf("segments missing: %v", rows)
	}
	if hasWhole {
		t.Fatalf("resubmitted whole message double-stored: %v", rows)
	}
}

// recall_limit: 0 disables injection (absence keeps the default of 12).
func TestRecallLimitZeroDisables(t *testing.T) {
	dir := t.TempDir()
	cfg := DefaultConfig()
	cfg.DataDir = dir
	cfg.AutoConsolidate = false
	cfg.InterruptGate = false
	cfg.RecallLimit = 0
	svc := NewService(cfg)
	h := http.Header{}
	h.Set("X-Cortext-Session", "rl0-1")

	svc.HandleResponse(pluginapi.ResponseInterceptRequest{
		SourceFormat:   fmtOpenAI,
		RequestHeaders: h,
		Body:           []byte(`{"choices":[{"message":{"content":"fact about capybaras rl0"}}]}`),
	})
	resp := svc.HandleRequestBeforeAuth(pluginapi.RequestInterceptRequest{
		SourceFormat: fmtOpenAI,
		Headers:      h,
		Body:         []byte(`{"messages":[{"role":"user","content":"capybaras"}]}`),
	})
	if len(resp.Body) != 0 {
		t.Fatalf("recall_limit=0 still injected: %s", resp.Body)
	}
}
