package main

import (
	"log"
	"strings"
	"sync"

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
	eng, err := s.store.ForScope(scope)
	if err != nil {
		log.Printf("cortext: open engine %s: %v", scope, err)
		return pluginapi.RequestInterceptResponse{}
	}

	msgs := ExtractMessages(req.SourceFormat, req.Body)
	// Durable-ingest new messages (clients resend full history).
	for _, m := range msgs {
		text := strings.TrimSpace(m.Text)
		if text == "" {
			continue
		}
		role := strings.ToLower(m.Role)
		if role == "system" {
			// System prompts are host scaffolding; skip durable store by default.
			continue
		}
		h := hashText(text)
		if s.store.HasSeen(scope, h) {
			continue
		}
		src := sourceID(scope, role, "ingest")
		if _, err := eng.ProcessText(text, src, RetentionDurable); err != nil {
			log.Printf("cortext: ingest: %v", err)
			continue
		}
		s.store.MarkSeen(scope, h)
	}

	query := LatestUserText(msgs)
	var body strings.Builder
	if staged := s.store.Bus().Take(scope); staged != "" {
		body.WriteString(staged)
	}
	if query != "" {
		ctx, err := eng.ProcessText(query, sourceID(scope, "agent", "assemble"), RetentionEphemeral)
		if err != nil {
			log.Printf("cortext: recall: %v", err)
		} else {
			recalled := formatMemories(ctx.RetrievedMemory, cfg.RecallLimit)
			if recalled != "" {
				if body.Len() > 0 {
					body.WriteByte('\n')
				}
				body.WriteString(recalled)
			}
		}
	}

	out := req.Body
	if block := memoryBlock(body.String()); block != "" {
		updated, err := InjectMemoryBlock(req.SourceFormat, out, block)
		if err != nil {
			log.Printf("cortext: inject: %v", err)
		} else {
			out = updated
		}
	}

	if cfg.WindowMessages > 0 {
		updated, err := WindowMessages(req.SourceFormat, out, cfg.WindowMessages)
		if err != nil {
			log.Printf("cortext: window: %v", err)
		} else {
			out = updated
		}
	}

	if cfg.AutoConsolidate {
		// Best-effort; cadence can be refined later.
		_ = eng.Consolidate()
	}

	if string(out) == string(req.Body) {
		return pluginapi.RequestInterceptResponse{}
	}
	return pluginapi.RequestInterceptResponse{Body: out}
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
	if !cfg.Enabled || !cfg.IngestAssistant || len(req.Body) == 0 {
		return pluginapi.ResponseInterceptResponse{}
	}
	text := ExtractAssistantText(req.SourceFormat, req.Body)
	if strings.TrimSpace(text) == "" {
		return pluginapi.ResponseInterceptResponse{}
	}
	ids := ResolveScopeIDs(cfg, req.RequestHeaders, req.OriginalRequest, req.Metadata)
	scope := s.store.ScopeKey(ids)
	eng, err := s.store.ForScope(scope)
	if err != nil {
		return pluginapi.ResponseInterceptResponse{}
	}
	h := hashText(text)
	if s.store.HasSeen(scope, h) {
		return pluginapi.ResponseInterceptResponse{}
	}
	if _, err := eng.ProcessText(text, sourceID(scope, "assistant", "response"), RetentionDurable); err != nil {
		log.Printf("cortext: response ingest: %v", err)
		return pluginapi.ResponseInterceptResponse{}
	}
	s.store.MarkSeen(scope, h)
	return pluginapi.ResponseInterceptResponse{}
}

// streamBuf holds partial SSE text for interrupt-gate segmentation.
type streamBuf struct {
	assistant strings.Builder
	reasoning strings.Builder
}

// streamStates is process-local; keyed by scope for gate segments.
var streamStates struct {
	mu sync.Mutex
	m  map[string]*streamBuf
}

func init() {
	streamStates.m = make(map[string]*streamBuf)
}

func streamBufFor(scope string) *streamBuf {
	streamStates.mu.Lock()
	defer streamStates.mu.Unlock()
	buf := streamStates.m[scope]
	if buf == nil {
		buf = &streamBuf{}
		streamStates.m[scope] = buf
	}
	return buf
}

// HandleStreamChunk durable-ingests assistant deltas and optionally runs the
// interrupt gate (stages recall for the *next* request only).
func (s *Service) HandleStreamChunk(req pluginapi.StreamChunkInterceptRequest) pluginapi.StreamChunkInterceptResponse {
	cfg := s.store.Config()
	if !cfg.Enabled || len(req.Body) == 0 {
		return pluginapi.StreamChunkInterceptResponse{}
	}
	if req.ChunkIndex == pluginapi.StreamChunkHeaderInitIndex {
		return pluginapi.StreamChunkInterceptResponse{}
	}

	delta, isReasoning := ExtractStreamTextDelta(req.SourceFormat, req.Body)
	if delta == "" {
		return pluginapi.StreamChunkInterceptResponse{}
	}
	if isReasoning && !cfg.IngestReasoning {
		return pluginapi.StreamChunkInterceptResponse{}
	}

	ids := ResolveScopeIDs(cfg, req.RequestHeaders, req.OriginalRequest, req.Metadata)
	scope := s.store.ScopeKey(ids)
	eng, err := s.store.ForScope(scope)
	if err != nil {
		return pluginapi.StreamChunkInterceptResponse{}
	}

	buf := streamBufFor(scope)
	target := &buf.assistant
	role := "assistant"
	if isReasoning {
		target = &buf.reasoning
		role = "reasoning"
	}
	target.WriteString(delta)

	// Segment on sentence boundaries once we have enough text.
	segment := target.String()
	if len(segment) < 120 && !strings.ContainsAny(delta, ".!?\n") {
		return pluginapi.StreamChunkInterceptResponse{}
	}
	target.Reset()

	if cfg.IngestAssistant || (isReasoning && cfg.IngestReasoning) {
		h := hashText(segment)
		if !s.store.HasSeen(scope, h) {
			if _, err := eng.ProcessText(segment, sourceID(scope, role, "stream"), RetentionDurable); err != nil {
				log.Printf("cortext: stream ingest: %v", err)
			} else {
				s.store.MarkSeen(scope, h)
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

	return pluginapi.StreamChunkInterceptResponse{}
}

func gateKind(ctx ContextPacket) string {
	if ctx.ShouldInterrupt {
		return "interrupt"
	}
	return "boundary"
}
