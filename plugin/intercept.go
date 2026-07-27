package main

import (
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// Service holds shared plugin state.
type Service struct {
	store *Store
}

func NewService(cfg PluginConfig) *Service {
	return &Service{store: NewStore(cfg)}
}

func (s *Service) Reconfigure(cfg PluginConfig) {
	s.store.Reconfigure(cfg)
}

// Prewarm triggers first-open engine costs (native asset download/assembly)
// at registration rather than on the first request.
func (s *Service) Prewarm() {
	s.store.Prewarm()
}

func (s *Service) Shutdown() {
	s.store.DisposeAll()
}

// HandleRequestBeforeAuth implements the OpenClaw assemble+ingest loop at the
// proxy edge for every client SourceFormat.
func (s *Service) HandleRequestBeforeAuth(req pluginapi.RequestInterceptRequest) pluginapi.RequestInterceptResponse {
	cfg := s.store.Config()
	if !cfg.Enabled || len(req.Body) == 0 {
		return pluginapi.RequestInterceptResponse{}
	}

	ids := ResolveScopeIDs(cfg, req.Headers, req.Body, req.Metadata)
	scope := s.store.ScopeKey(ids)
	if scope == "" {
		// Identity-less under session/agent scope: pass through without durable store.
		metrics.SkipNoIdentity.Add(1)
		return pluginapi.RequestInterceptResponse{}
	}

	msgs := ExtractMessages(req.SourceFormat, req.Body)
	query := LatestUserText(msgs)

	// Durable-ingest prior turns first (clients resend full history).
	// Defer the *current* user query until after assemble so recall is not
	// dominated by a just-written copy of the probe itself.
	var pending []Message
	for _, m := range msgs {
		text := strings.TrimSpace(m.Text)
		if text == "" {
			continue
		}
		role := strings.ToLower(m.Role)
		if role == "system" || role == "developer" {
			// System/developer prompts are host scaffolding; never durable-store.
			continue
		}
		if role == "assistant" && !cfg.IngestAssistant {
			// The ingest_assistant knob must hold on every path — history
			// resubmit re-delivers everything the response path was blocked
			// from storing.
			continue
		}
		if query != "" && text == query && strings.EqualFold(role, "user") {
			continue
		}
		pending = append(pending, Message{Role: role, Text: text})
	}

	staged := s.store.Bus().Take(scope)

	// Window first so recall dedupe compares against what the model will
	// actually see, not the full resent history.
	out := req.Body
	if cfg.WindowMessages > 0 {
		updated, err := WindowMessages(req.SourceFormat, out, cfg.WindowMessages)
		if err != nil {
			log.Printf("cortext: window: %v", err)
		} else {
			out = updated
		}
	}
	windowTexts := messageTexts(ExtractMessages(req.SourceFormat, out))

	// Engine opens lazily: only recall and durable ingest need one. A request
	// that only receives a staged gate block (or nothing) creates no store.
	var eng Engine
	if query != "" || len(pending) > 0 {
		var err error
		eng, err = s.store.ForScope(scope)
		if err != nil {
			// Re-stage the gate block so the next assemble can still use it.
			if staged != "" {
				s.store.Bus().Stage(scope, staged)
			}
			logOpenFail(scope, err)
			return pluginapi.RequestInterceptResponse{}
		}
		for _, m := range pending {
			s.durableIngest(eng, scope, m.Role, m.Text)
		}
	}

	var body strings.Builder
	if staged != "" {
		// Drop staged lines the model can already see verbatim in the window.
		body.WriteString(filterStagedAgainstWindow(staged, windowTexts))
	}
	if query != "" {
		ctx, err := eng.ProcessText(query, sourceID(scope, "agent", "assemble"), RetentionEphemeral)
		if err != nil {
			// A closed engine (eviction/reconfigure race) degrades to empty
			// recall; it is not a process failure.
			if !errors.Is(err, errEngineClosed) {
				logFail(&metrics.ProcessFail, "process_fail", fmt.Sprintf("recall scope=%s err=%v", scope, err))
			}
		} else {
			// Prefer LTM hits; include working memory when non-empty.
			items := append([]MemoryItem{}, ctx.RetrievedMemory...)
			items = append(items, ctx.WorkingMemory...)
			items = dropQueryEchoes(items, query)
			// Drop memories already verbatim in the outbound transcript.
			items = dedupeAgainstWindow(items, windowTexts)
			recalled := formatMemories(items, cfg.RecallLimit)
			if recalled != "" {
				if body.Len() > 0 {
					body.WriteByte('\n')
				}
				body.WriteString(recalled)
			}
		}
		// Now durable-store the current user turn for the *next* request.
		s.durableIngest(eng, scope, "user", query)
	}

	// dedupeLines: the staged gate block and fresh recall may share a line.
	if block := memoryBlock(dedupeLines(body.String())); block != "" {
		updated, err := InjectMemoryBlock(req.SourceFormat, out, block)
		if err != nil {
			// Re-stage the gate block so the next assemble can still use it.
			if staged != "" {
				s.store.Bus().Stage(scope, staged)
			}
			logFail(&metrics.InjectFail, "inject_fail", fmt.Sprintf("scope=%s err=%v", scope, err))
		} else {
			if string(updated) == string(out) && staged != "" {
				// Injection target missing (unrecognized format): keep the
				// staged block for a later request that can use it.
				s.store.Bus().Stage(scope, staged)
			}
			out = updated
		}
	}

	if string(out) == string(req.Body) {
		return pluginapi.RequestInterceptResponse{}
	}
	return pluginapi.RequestInterceptResponse{Body: out}
}

// logOpenFail counts/logs engine-open failures. A store closed by shutdown
// is expected pass-through, not a failure — keep it out of the alert counter.
func logOpenFail(scope string, err error) {
	if errors.Is(err, errStoreClosed) || errors.Is(err, errConfigChanged) {
		return
	}
	logFail(&metrics.OpenFail, "open_fail", fmt.Sprintf("scope=%s err=%v", scope, err))
}

// HandleRequestAfterAuth is a no-op pass-through today (memory already injected
// on the client body before auth). Kept so the capability is complete.
func (s *Service) HandleRequestAfterAuth(req pluginapi.RequestInterceptRequest) pluginapi.RequestInterceptResponse {
	_ = req
	return pluginapi.RequestInterceptResponse{}
}

// HandleResponse durable-ingests the assistant completion.
func (s *Service) HandleResponse(req pluginapi.ResponseInterceptRequest) pluginapi.ResponseInterceptResponse {
	cfg := s.store.Config()
	if !cfg.Enabled || len(req.Body) == 0 {
		return pluginapi.ResponseInterceptResponse{}
	}
	ids := ResolveScopeIDs(cfg, req.RequestHeaders, req.OriginalRequest, req.Metadata)
	scope := s.store.ScopeKey(ids)
	if scope == "" {
		metrics.SkipNoIdentity.Add(1)
		return pluginapi.ResponseInterceptResponse{}
	}
	// Chain the response id so later previous_response_id requests find us.
	if id := ExtractResponseID(req.Body); id != "" {
		s.store.RememberResponseScope(id, scope, ownerHash(ids))
	}
	skey := streamKey(scope, req.OriginalRequest)
	text := ExtractAssistantText(req.SourceFormat, req.Body)
	// streamed reports that this request's assistant text was already durably
	// ingested from stream deltas; re-storing the assembled full body would
	// persist a near-duplicate of those segments. Peeked (not taken) so a
	// downstream engine-open failure keeps the mark for the retry.
	// Dedupe contract (pinned by TestStreamThenFullResponseNotDoubleIngested):
	// the host must deliver the same OriginalRequest bytes on stream chunks
	// and on this response intercept. If a host diverges, the mark misses and
	// the full body is ingested once as a near-duplicate of the segments —
	// bounded, and masked at inject time by line dedupe.
	streamed := peekStreamIngested(skey)
	// Avoid creating an engine/SQLite for noise responses: only open one when
	// there is a matching stream residual to flush or text to ingest.
	if !hasStreamBuf(skey) && (streamed || !cfg.IngestAssistant || strings.TrimSpace(text) == "") {
		dropStreamIngested(skey)
		return pluginapi.ResponseInterceptResponse{}
	}
	eng, err := s.store.ForScope(scope)
	if err != nil {
		logOpenFail(scope, err)
		return pluginapi.ResponseInterceptResponse{}
	}
	// Flush only THIS request's stream residual; concurrent streams in the
	// same scope keep buffering. A failed residual ingest means the full body
	// is the recovery path (streamed stays false).
	if hasStreamBuf(skey) {
		ingested, failed := s.flushStreamBuf(skey, scope, eng, cfg)
		if ingested && !failed {
			streamed = true
		}
		clearStreamBuf(skey)
	}

	if streamed || !cfg.IngestAssistant || strings.TrimSpace(text) == "" {
		dropStreamIngested(skey)
		return pluginapi.ResponseInterceptResponse{}
	}
	// Stale/expired mark (or a full ingest about to happen): clear it.
	dropStreamIngested(skey)
	h := dedupeHash(text)
	if !s.store.ClaimSeen(scope, h) {
		return pluginapi.ResponseInterceptResponse{}
	}
	pkt, err := eng.ProcessText(text, sourceID(scope, "assistant", "response"), RetentionDurable)
	if err != nil {
		s.store.UnmarkSeen(scope, h)
		logFail(&metrics.ProcessFail, "process_fail", fmt.Sprintf("response scope=%s err=%v", scope, err))
		return pluginapi.ResponseInterceptResponse{}
	}
	s.maybeConsolidate(eng, scope, pkt.ConsolidationState)
	return pluginapi.ResponseInterceptResponse{}
}

// streamBuf holds partial SSE text for interrupt-gate segmentation.
type streamBuf struct {
	assistant strings.Builder
	reasoning strings.Builder
	// toolcalls accumulates raw tool-call argument JSON (Claude partial_json,
	// Responses function_call_arguments): buffered whole, ingested once at
	// stream end — never fragmented by the prose segmenter.
	toolcalls strings.Builder
	// whole accumulates ALL assistant text deltas so a later history
	// resubmit of the assembled message dedupes against it (whitespace-
	// normalized hash) instead of storing a second whole-message copy.
	whole strings.Builder
	// storedAny records that some segment of this stream was durably stored
	// (the whole-message claim is only safe then).
	storedAny bool
	last      time.Time
	// conflicted marks a buffer shared by two in-flight byte-identical
	// streams (see resetStreamBuf): their deltas cannot be told apart, so
	// nothing from this key is durably ingested.
	conflicted bool
}

// maxStreamBufs bounds the process-global stream map; streams that never
// terminate (client disconnect, upstream error, no [DONE]) would otherwise
// leak one entry per request.
const maxStreamBufs = 256

// streamKey separates concurrent streams that share an isolation scope:
// distinct outbound request bodies get distinct buffers, so parallel streams
// in one session cannot interleave into each other's ingested text. The key
// is over the OUTBOUND body bytes the host passes back to us — if a host
// hands the plugin the memory-injected rewrite, retries can land on a
// different key (the aborted attempt's buffer then orphans and is evicted;
// its content is recoverable via next-turn history resubmit).
func streamKey(scope string, originalRequest []byte) string {
	return scope + "|" + hashText(string(originalRequest))
}

// streamStates is process-local; keyed by streamKey for gate segments.
var streamStates struct {
	mu sync.Mutex
	m  map[string]*streamBuf
	// scopes caches bodyHash → scope+owner so per-chunk handling skips the
	// gjson body scans after the first chunk of a stream.
	scopes map[string]streamScope
}

type streamScope struct {
	scope string
	owner string
}

func init() {
	streamStates.m = make(map[string]*streamBuf)
	streamStates.scopes = make(map[string]streamScope)
	streamIngested.m = make(map[string]streamMark)
}

// streamScopeFor resolves the scope (and chain-owner hash) for a stream
// chunk, caching per (identity, body) pair so chunks after the first skip
// the body scans. The identity hash covers every header/metadata field
// ResolveScopeIDs consults — same body under a different session is a
// different stream.
func (s *Service) streamScopeFor(cfg PluginConfig, req pluginapi.StreamChunkInterceptRequest, bodyHash string) (scope, owner string) {
	key := bodyHash + "|" + streamIdentityHash(cfg, req)
	streamStates.mu.Lock()
	cached, ok := streamStates.scopes[key]
	streamStates.mu.Unlock()
	if ok {
		return cached.scope, cached.owner
	}
	ids := ResolveScopeIDs(cfg, req.RequestHeaders, req.OriginalRequest, req.Metadata)
	scope = s.store.ScopeKey(ids)
	owner = ownerHash(ids)
	if scope != "" {
		streamStates.mu.Lock()
		if len(streamStates.scopes) < maxStreamBufs {
			streamStates.scopes[key] = streamScope{scope: scope, owner: owner}
		}
		streamStates.mu.Unlock()
	}
	return scope, owner
}

// streamIdentityHash fingerprints the scope-relevant identity fields without
// scanning the request body.
func streamIdentityHash(cfg PluginConfig, req pluginapi.StreamChunkInterceptRequest) string {
	var parts []string
	if h := req.RequestHeaders; h != nil {
		parts = append(parts,
			h.Get(cfg.SessionHeader), h.Get("X-Session-Id"), h.Get("X-Conversation-Id"),
			h.Get(cfg.AgentHeader), h.Get("X-Agent-Id"),
			h.Get("Authorization"), h.Get("x-api-key"), h.Get("x-goog-api-key"))
	}
	if v, ok := req.Metadata["session_id"].(string); ok {
		parts = append(parts, v)
	}
	if v, ok := req.Metadata["agent_id"].(string); ok {
		parts = append(parts, v)
	}
	return hashText(strings.Join(parts, "\x00"))
}

// resetStreamBuf starts a fresh buffer at stream init. clearStreamBuf removes
// the entry at stream end, so ANY existing entry means another stream with a
// byte-identical body is still in flight (or an aborted attempt left one):
// the two cannot be told apart — not even when the buffer is momentarily
// empty (two inits can arrive before either stream's first delta) — so the
// buffer is poisoned instead of reset. Conflicted buffers are never durably
// ingested; interleaved text never reaches memory. On hosts that fire a
// response intercept for streams, the assembled full body is the recovery;
// on the pinned host, next-turn history resubmit carries the content.
// streamConflictTTL bounds how long a poisoned buffer blocks new streams on
// the same key. The sibling stream's late deltas arrive within this window;
// after it, an alone-in-flight identical stream is a fresh attempt.
const streamConflictTTL = 60 * time.Second

func resetStreamBuf(skey string) {
	streamStates.mu.Lock()
	defer streamStates.mu.Unlock()
	if old := streamStates.m[skey]; old != nil {
		if old.conflicted && time.Since(old.last) > streamConflictTTL {
			// Decayed: the conflicting streams are long gone.
			delete(streamStates.m, skey)
		} else {
			old.conflicted = true
			old.last = time.Now()
			metrics.StreamConflict.Add(1)
			return
		}
	}
	streamStates.m[skey] = &streamBuf{last: time.Now()}
	evictStaleStreamBufsLocked()
}

// streamConflicted reports whether the stream's buffer was poisoned by an
// identical-body concurrent stream (see resetStreamBuf).
func streamConflicted(skey string) bool {
	streamStates.mu.Lock()
	defer streamStates.mu.Unlock()
	buf := streamStates.m[skey]
	return buf != nil && buf.conflicted
}

// hasStreamBuf reports whether the stream has buffered (unflushed) text.
func hasStreamBuf(skey string) bool {
	streamStates.mu.Lock()
	defer streamStates.mu.Unlock()
	buf := streamStates.m[skey]
	return buf != nil && (buf.assistant.Len() > 0 || buf.reasoning.Len() > 0 || buf.toolcalls.Len() > 0)
}

// evictStaleStreamBufsLocked drops the oldest buffers once the map is full.
// Caller must hold streamStates.mu.
func evictStaleStreamBufsLocked() {
	for len(streamStates.m) > maxStreamBufs {
		var oldestKey string
		var oldest time.Time
		for k, b := range streamStates.m {
			if oldestKey == "" || b.last.Before(oldest) {
				oldestKey, oldest = k, b.last
			}
		}
		delete(streamStates.m, oldestKey)
		metrics.StreamEvict.Add(1)
	}
}

// appendStreamDelta appends under the streamStates lock so concurrent chunks
// for the same stream do not race on strings.Builder. Tool-argument JSON
// never segment-flushes; it is ingested whole at stream end.
func appendStreamDelta(skey string, kind deltaKind, delta string) (segment string, role string, flush bool) {
	streamStates.mu.Lock()
	defer streamStates.mu.Unlock()
	buf := streamStates.m[skey]
	if buf == nil {
		// Stamp last BEFORE the cap eviction: a zero-time buffer is always the
		// oldest and would self-evict under stream-map pressure.
		buf = &streamBuf{last: time.Now()}
		streamStates.m[skey] = buf
		evictStaleStreamBufsLocked()
	}
	buf.last = time.Now()
	if kind == deltaToolArgs {
		buf.toolcalls.WriteString(delta)
		return "", "assistant", false
	}
	target := &buf.assistant
	role = "assistant"
	if kind == deltaReasoning {
		target = &buf.reasoning
		role = "reasoning"
	} else {
		buf.whole.WriteString(delta)
	}
	target.WriteString(delta)
	segment = target.String()
	if len(segment) < 120 && !strings.ContainsAny(delta, ".!?\n") {
		return "", role, false
	}
	target.Reset()
	return segment, role, true
}

// admitDelta reports whether any consumer (durable ingest or the interrupt
// gate) wants a delta of this kind under cfg.
func admitDelta(cfg PluginConfig, kind deltaKind) bool {
	switch kind {
	case deltaReasoning:
		return cfg.IngestReasoning || cfg.InterruptGate
	case deltaToolArgs:
		return cfg.IngestAssistant
	default:
		return cfg.IngestAssistant || cfg.InterruptGate
	}
}

// HandleStreamChunk durable-ingests assistant deltas and optionally runs the
// interrupt gate (stages recall for the *next* request only).
func (s *Service) HandleStreamChunk(req pluginapi.StreamChunkInterceptRequest) pluginapi.StreamChunkInterceptResponse {
	cfg := s.store.Config()
	if !cfg.Enabled {
		return pluginapi.StreamChunkInterceptResponse{}
	}

	bodyHash := hashText(string(req.OriginalRequest))
	scope, owner := s.streamScopeFor(cfg, req, bodyHash)
	if scope == "" {
		metrics.SkipNoIdentity.Add(1)
		return pluginapi.StreamChunkInterceptResponse{}
	}
	skey := scope + "|" + bodyHash

	// Stream start: fresh buffer (see resetStreamBuf). No engine needed.
	if req.ChunkIndex == pluginapi.StreamChunkHeaderInitIndex {
		resetStreamBuf(skey)
		return pluginapi.StreamChunkInterceptResponse{}
	}

	// Explicit stream end ([DONE] / message_stop / response.completed, possibly
	// coalesced with a final data line): capture any trailing delta and the
	// response id, then flush and clear only this stream's buffer; concurrent
	// streams in the scope keep theirs.
	if IsStreamDone(req.Body) {
		if id := ExtractResponseID(req.Body); id != "" {
			s.store.RememberResponseScope(id, scope, owner)
		}
		if streamConflicted(skey) {
			// Poisoned buffer (identical-body concurrent stream): ingest
			// nothing, create no engine. Tombstone a FAILED mark so a
			// full-body response intercept (hosts that fire one for streams;
			// CPA v7.2.96 does not) stays a recovery path — on the pinned
			// host, next-turn history resubmit is the real recovery. Keep
			// the poisoned buffer so the sibling stream's late deltas keep
			// being dropped instead of starting a fresh (partial) ingest.
			noteStreamResult(skey, false, true)
			return pluginapi.StreamChunkInterceptResponse{}
		}
		var eng Engine
		// Only open an engine when some consumer (durable ingest or the
		// interrupt gate) will use stream text.
		consumers := cfg.IngestAssistant || cfg.IngestReasoning || cfg.InterruptGate
		if consumers {
			for _, d := range ExtractStreamTextDeltas(req.SourceFormat, req.Body) {
				if d.Text == "" || !admitDelta(cfg, d.Kind) {
					continue
				}
				if eng == nil {
					var err error
					if eng, err = s.store.ForScope(scope); err != nil {
						logOpenFail(scope, err)
						break
					}
				}
				if segment, role, flush := appendStreamDelta(skey, d.Kind, d.Text); flush && !streamConflicted(skey) {
					ingested, failed := s.ingestStreamSegment(scope, eng, cfg, segment, role, d.Kind == deltaReasoning)
					noteStreamResult(skey, ingested, failed)
				}
			}
			if hasStreamBuf(skey) {
				if eng == nil {
					var err error
					if eng, err = s.store.ForScope(scope); err != nil {
						logOpenFail(scope, err)
					}
				}
				if eng != nil {
					ingested, failed := s.flushStreamBuf(skey, scope, eng, cfg)
					noteStreamResult(skey, ingested, failed)
				}
			}
			// The stream stored the text as segments; claim the
			// assembled-message hash so the next history resubmit dedupes
			// away instead of storing a second whole-message copy.
			if whole, storedAny := takeStreamWhole(skey); storedAny && whole != "" && peekStreamIngested(skey) {
				s.store.ClaimSeen(scope, dedupeHash(whole))
			}
		}
		clearStreamBuf(skey)
		return pluginapi.StreamChunkInterceptResponse{}
	}
	if len(req.Body) == 0 {
		return pluginapi.StreamChunkInterceptResponse{}
	}

	// Chain the response id (first chunk usually carries it) so later
	// previous_response_id requests find this scope.
	if id := ExtractResponseID(req.Body); id != "" {
		s.store.RememberResponseScope(id, scope, owner)
	}

	deltas := ExtractStreamTextDeltas(req.SourceFormat, req.Body)
	if len(deltas) == 0 {
		return pluginapi.StreamChunkInterceptResponse{}
	}
	if streamConflicted(skey) {
		// Poisoned buffer (identical-body concurrent stream): drop the delta
		// instead of interleaving two responses into durable memory.
		return pluginapi.StreamChunkInterceptResponse{}
	}
	if !cfg.IngestAssistant && !cfg.IngestReasoning && !cfg.InterruptGate {
		// No consumer for stream text: do not open an engine or create a
		// store file.
		return pluginapi.StreamChunkInterceptResponse{}
	}

	// Engine opens lazily: tool-call-only streams never create a SQLite file.
	var eng Engine
	for _, d := range deltas {
		if d.Text == "" || !admitDelta(cfg, d.Kind) {
			continue
		}
		if eng == nil {
			var err error
			if eng, err = s.store.ForScope(scope); err != nil {
				logOpenFail(scope, err)
				return pluginapi.StreamChunkInterceptResponse{}
			}
		}
		segment, role, flush := appendStreamDelta(skey, d.Kind, d.Text)
		if !flush || streamConflicted(skey) {
			continue
		}
		ingested, failed := s.ingestStreamSegment(scope, eng, cfg, segment, role, d.Kind == deltaReasoning)
		noteStreamResult(skey, ingested, failed)
	}
	return pluginapi.StreamChunkInterceptResponse{}
}

// ingestStreamSegment reports whether the segment was durably ingested, and
// whether a durable ingest was attempted and FAILED (transient process error)
// — callers mark the stream key so a later full-body response intercept can
// skip re-storing the assembled text, but only when nothing was lost.
func (s *Service) ingestStreamSegment(scope string, eng Engine, cfg PluginConfig, segment, role string, isReasoning bool) (ingested bool, failed bool) {
	segment = strings.TrimSpace(segment)
	if segment == "" {
		return false, false
	}
	// Hash the FULL segment, truncate for storage (see durableIngest).
	if (!isReasoning && cfg.IngestAssistant) || (isReasoning && cfg.IngestReasoning) {
		h := dedupeHash(segment)
		segment = headBytes(segment, maxIngestBytes)
		if s.store.ClaimSeen(scope, h) {
			pkt, err := eng.ProcessText(segment, sourceID(scope, role, "stream"), RetentionDurable)
			if err != nil {
				s.store.UnmarkSeen(scope, h)
				logFail(&metrics.ProcessFail, "process_fail", fmt.Sprintf("stream scope=%s err=%v", scope, err))
				failed = true
			} else {
				s.maybeConsolidate(eng, scope, pkt.ConsolidationState)
				ingested = true
			}
		}
	}
	if cfg.InterruptGate {
		ctx, err := eng.ProcessText(segment, sourceID(scope, "agent", "stream", role), RetentionEphemeral)
		if err == nil && (ctx.ShouldInterrupt || ctx.AtBoundary) {
			if block := formatMemories(ctx.RetrievedMemory, cfg.RecallLimit); block != "" {
				s.store.Bus().Stage(scope, block)
				log.Printf("cortext: gate %s on %s (scope %s)", gateKind(ctx), role, scope)
			}
		}
	}
	return ingested, failed
}

// flushStreamBuf durable-ingests one stream's buffered text that never
// crossed the sentence/length boundary (end-of-stream residual), including
// the assembled tool-call argument buffer. Only the given stream key is
// drained; concurrent streams in the scope keep buffering.
// Conflicted buffers (identical-body concurrent streams) are drained without
// ingest — interleaved text must never reach memory.
// Reports whether any residual text was durably ingested, and whether any
// durable ingest attempt failed.
func (s *Service) flushStreamBuf(skey, scope string, eng Engine, cfg PluginConfig) (ingested bool, failed bool) {
	streamStates.mu.Lock()
	buf := streamStates.m[skey]
	var asst, reason, tools, whole string
	var storedAny, conflicted bool
	if buf != nil {
		conflicted = buf.conflicted
		asst = strings.TrimSpace(buf.assistant.String())
		reason = strings.TrimSpace(buf.reasoning.String())
		tools = strings.TrimSpace(buf.toolcalls.String())
		whole = flattenWS(buf.whole.String())
		storedAny = buf.storedAny
		buf.assistant.Reset()
		buf.reasoning.Reset()
		buf.toolcalls.Reset()
		buf.whole.Reset()
	}
	streamStates.mu.Unlock()
	if conflicted {
		return false, false
	}
	if asst != "" {
		i, f := s.ingestStreamSegment(scope, eng, cfg, asst, "assistant", false)
		ingested, failed = ingested || i, failed || f
	}
	if reason != "" {
		i, f := s.ingestStreamSegment(scope, eng, cfg, reason, "reasoning", true)
		ingested, failed = ingested || i, failed || f
	}
	if tools != "" {
		i, f := s.ingestStreamSegment(scope, eng, cfg, "[tool call] "+tools, "assistant", false)
		ingested, failed = ingested || i, failed || f
	}
	// The stream stored the text as segments; claim the assembled-message
	// hash so the next history resubmit dedupes away instead of storing a
	// second whole-message copy. Only when this stream actually stored
	// something and nothing failed (otherwise the claim would LOSE text).
	if (storedAny || ingested) && !failed && whole != "" {
		s.store.ClaimSeen(scope, dedupeHash(whole))
	}
	return ingested, failed
}

// streamIngested records stream keys whose assistant text was already durably
// ingested from stream deltas. HandleResponse takes the mark so the assembled
// full response body is not stored a second time as a near-duplicate of the
// segments — unless any segment ingest FAILED, in which case the full body
// is the recovery path. Bounded like streamStates: oldest marks evict past
// maxStreamBufs.
var streamIngested struct {
	mu sync.Mutex
	m  map[string]streamMark
}

type streamMark struct {
	at     time.Time
	failed bool // a durable segment ingest failed on this stream
}

// noteStreamResult records the outcome of a stream's durable ingests.
func noteStreamResult(skey string, ingested, failed bool) {
	if !ingested && !failed {
		return
	}
	if ingested {
		streamStates.mu.Lock()
		if buf := streamStates.m[skey]; buf != nil {
			buf.storedAny = true
		}
		streamStates.mu.Unlock()
	}
	streamIngested.mu.Lock()
	defer streamIngested.mu.Unlock()
	m := streamIngested.m[skey]
	m.at = time.Now()
	m.failed = m.failed || failed
	streamIngested.m[skey] = m
	for len(streamIngested.m) > maxStreamBufs {
		var oldestKey string
		var oldest time.Time
		for k, t := range streamIngested.m {
			if oldestKey == "" || t.at.Before(oldest) {
				oldestKey, oldest = k, t.at
			}
		}
		delete(streamIngested.m, oldestKey)
	}
}

// streamIngestedTTL bounds how long a stream-ingest mark can suppress a
// full-body response ingest. A byte-identical non-streamed retry long after
// the stream is a genuinely new response and must be stored.
const streamIngestedTTL = 5 * time.Minute

// peekStreamIngested reports whether a live success mark exists WITHOUT
// consuming it; dropStreamIngested removes the mark at the actual skip
// decision (a failure between peek and skip must not lose the dedupe).
func peekStreamIngested(skey string) bool {
	streamIngested.mu.Lock()
	defer streamIngested.mu.Unlock()
	m, ok := streamIngested.m[skey]
	if !ok || m.failed {
		// Something was lost on the stream path: the assembled body is the
		// recovery, not a duplicate.
		return false
	}
	return time.Since(m.at) < streamIngestedTTL
}

func dropStreamIngested(skey string) {
	streamIngested.mu.Lock()
	defer streamIngested.mu.Unlock()
	delete(streamIngested.m, skey)
}

// takeStreamWhole drains the accumulated whole assistant text for the
// whole-message dedupe claim (separate from residual buffers, which may
// already be empty when every segment flushed mid-stream).
func takeStreamWhole(skey string) (whole string, storedAny bool) {
	streamStates.mu.Lock()
	defer streamStates.mu.Unlock()
	buf := streamStates.m[skey]
	if buf == nil {
		return "", false
	}
	whole = flattenWS(buf.whole.String())
	storedAny = buf.storedAny
	buf.whole.Reset()
	return whole, storedAny
}

func clearStreamBuf(skey string) {
	streamStates.mu.Lock()
	defer streamStates.mu.Unlock()
	delete(streamStates.m, skey)
}

func gateKind(ctx ContextPacket) string {
	if ctx.ShouldInterrupt {
		return "interrupt"
	}
	return "boundary"
}

func (s *Service) durableIngest(eng Engine, scope, role, text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	// Hash the FULL text (two texts sharing a 16KB prefix must not collide),
	// then truncate for storage.
	h := dedupeHash(text)
	text = headBytes(text, maxIngestBytes)
	if !s.store.ClaimSeen(scope, h) {
		return
	}
	pkt, err := eng.ProcessText(text, sourceID(scope, role, "ingest"), RetentionDurable)
	if err != nil {
		s.store.UnmarkSeen(scope, h)
		logFail(&metrics.ProcessFail, "process_fail", fmt.Sprintf("ingest scope=%s err=%v", scope, err))
		return
	}
	s.maybeConsolidate(eng, scope, pkt.ConsolidationState)
}

// maybeConsolidate schedules a background consolidate when the engine asks
// for it (consolidation_state hint, cortext ≥1.2.2) or, as a fallback for
// engines that never emit the hint, every consolidate_every durable writes.
// Attempts are throttled to at most one per consolidate_every/5 writes per
// scope so a persistent hint (e.g. after a failed consolidate) cannot storm
// the hot path, and the consolidate itself runs off-path (single-flight per
// scope) so its model/DB latency never blocks a request.
func (s *Service) maybeConsolidate(eng Engine, scope, state string) {
	cfg := s.store.Config()
	if !cfg.AutoConsolidate {
		return
	}
	if s.store.NoteDurableIngest(scope, cfg.ConsolidateEvery, consolidationRequested(state)) &&
		s.store.AllowConsolidateAttempt(scope, cfg.ConsolidateEvery/5) {
		s.store.scheduleConsolidate(scope, eng)
	}
}

// dropQueryEchoes removes memories that are the same as the current query so
// inject does not restate the user's latest turn as "memory".
func dropQueryEchoes(items []MemoryItem, query string) []MemoryItem {
	q := strings.TrimSpace(strings.ToLower(query))
	if q == "" || len(items) == 0 {
		return items
	}
	out := make([]MemoryItem, 0, len(items))
	for _, it := range items {
		t := strings.TrimSpace(strings.ToLower(it.Text))
		if t == "" || t == q {
			continue
		}
		// Near-echo: query is a prefix/suffix of the memory or vice versa with high overlap.
		if strings.Contains(t, q) && len(q) > 40 && float64(len(q))/float64(len(t)) > 0.8 {
			continue
		}
		// Symmetric: memory is a long near-verbatim substring of the query.
		if strings.Contains(q, t) && len(t) > 40 && float64(len(t))/float64(len(q)) > 0.8 {
			continue
		}
		out = append(out, it)
	}
	return out
}
