package main

import (
	"bytes"
	"encoding/json"
	"log"
	"strings"
	"sync"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// warnWindowSkipOnce rate-limits the unsupported-format windowing warning.
var warnWindowSkipOnce sync.Once

// Message is a normalized chat turn extracted from any client format.
type Message struct {
	Role string
	Text string
}

// Supported SourceFormat values from CLIProxyAPI.
const (
	fmtOpenAI         = "openai"
	fmtOpenAIResponse = "openai-response"
	fmtClaude         = "claude"
	fmtGemini         = "gemini"
	fmtCodex          = "codex"
	fmtAntigravity    = "antigravity"
)

// ExtractMessages pulls role/text pairs from a client request body.
func ExtractMessages(sourceFormat string, body []byte) []Message {
	switch strings.ToLower(strings.TrimSpace(sourceFormat)) {
	case fmtClaude:
		return extractClaude(body)
	case fmtGemini, fmtAntigravity:
		return extractGemini(body)
	case fmtOpenAIResponse, fmtCodex:
		return extractOpenAIResponse(body)
	default:
		return extractOpenAI(body)
	}
}

// LatestUserText returns the most recent user message text, or "" when the
// body has no user turn. No fallback to the last message: a trailing
// assistant prefill must never be ingested or probed as a "user" turn.
func LatestUserText(msgs []Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if strings.EqualFold(msgs[i].Role, "user") && strings.TrimSpace(msgs[i].Text) != "" {
			return strings.TrimSpace(msgs[i].Text)
		}
	}
	return ""
}

// InjectMemoryBlock adds the cortext memory block into the system/instructions
// field appropriate for the client format. Idempotent: replaces a previous
// <cortext_memory>…</cortext_memory> block if present.
func InjectMemoryBlock(sourceFormat string, body []byte, block string) ([]byte, error) {
	block = strings.TrimSpace(block)
	if block == "" || len(body) == 0 {
		return body, nil
	}
	switch strings.ToLower(strings.TrimSpace(sourceFormat)) {
	case fmtClaude:
		return injectClaudeSystem(body, block)
	case fmtGemini, fmtAntigravity:
		return injectGeminiSystem(body, block)
	case fmtOpenAIResponse, fmtCodex:
		return injectOpenAIResponseInstructions(body, block)
	default:
		return injectOpenAISystem(body, block)
	}
}

// WindowMessages keeps system messages + the last n non-system messages.
// The kept window is extended left past orphaned tool-turn items (a tool /
// tool_result / function_call_output whose parent was cut) so providers do
// not reject the transcript with a 400.
func WindowMessages(sourceFormat string, body []byte, n int) ([]byte, error) {
	if n <= 0 || len(body) == 0 {
		return body, nil
	}
	switch strings.ToLower(strings.TrimSpace(sourceFormat)) {
	case fmtClaude:
		return windowClaude(body, n)
	case fmtOpenAIResponse, fmtCodex:
		return windowOpenAIResponse(body, n)
	case fmtGemini, fmtAntigravity:
		// Gemini contents windowing is more invasive; skip for v0. Log once
		// per process so the silent no-op is observable.
		warnWindowSkipOnce.Do(func() {
			log.Printf("cortext: window_messages=%d ignored for %s-format traffic (unsupported)", n, sourceFormat)
		})
		return body, nil
	default:
		return windowOpenAI(body, n)
	}
}

// ExtractAssistantText pulls assistant-visible text from a non-stream response body.
func ExtractAssistantText(sourceFormat string, body []byte) string {
	if len(body) == 0 {
		return ""
	}
	switch strings.ToLower(strings.TrimSpace(sourceFormat)) {
	case fmtClaude:
		var parts []string
		gjson.GetBytes(body, "content").ForEach(func(_, v gjson.Result) bool {
			if v.Get("type").String() == "text" {
				if t := v.Get("text").String(); t != "" {
					parts = append(parts, t)
				}
			}
			return true
		})
		return strings.TrimSpace(strings.Join(parts, "\n"))
	case fmtGemini, fmtAntigravity:
		var parts []string
		gjson.GetBytes(body, "candidates").ForEach(func(_, cand gjson.Result) bool {
			cand.Get("content.parts").ForEach(func(_, p gjson.Result) bool {
				// thought:true parts are chain-of-thought, never assistant text.
				if p.Get("thought").Bool() {
					return true
				}
				if t := p.Get("text").String(); t != "" {
					parts = append(parts, t)
				}
				return true
			})
			return true
		})
		return strings.TrimSpace(strings.Join(parts, "\n"))
	case fmtOpenAIResponse, fmtCodex:
		if t := gjson.GetBytes(body, "output_text").String(); t != "" {
			return strings.TrimSpace(t)
		}
		var parts []string
		gjson.GetBytes(body, "output").ForEach(func(_, item gjson.Result) bool {
			switch item.Get("type").String() {
			case "reasoning":
				// Hidden chain-of-thought: never extracted as assistant text.
				return true
			case "function_call":
				name := item.Get("name").String()
				args := item.Get("arguments").String()
				if len(args) > 2000 {
					args = headBytes(args, 2000) + "…"
				}
				if name != "" {
					parts = append(parts, "[tool call] "+name+" "+args)
				}
				return true
			}
			item.Get("content").ForEach(func(_, c gjson.Result) bool {
				if isReasoningPartType(c.Get("type").String()) {
					return true
				}
				if t := c.Get("text").String(); t != "" {
					parts = append(parts, t)
				}
				return true
			})
			return true
		})
		return strings.TrimSpace(strings.Join(parts, "\n"))
	default:
		// chat.completion: iterate ALL choices (n>1 responses lose siblings
		// when only choices.0 is read).
		var parts []string
		gjson.GetBytes(body, "choices").ForEach(func(_, c gjson.Result) bool {
			text := contentToText(c.Get("message.content"))
			if text == "" {
				text = toolCallsText(c.Get("message.tool_calls"))
			}
			if text != "" {
				parts = append(parts, text)
			}
			return true
		})
		return strings.TrimSpace(strings.Join(parts, "\n"))
	}
}

// IsStreamDone reports an explicit stream-termination chunk. CPA chunk
// payloads can carry several SSE lines, so scan every line. Recognized
// terminators: OpenAI [DONE]; Claude message_stop; Responses-API
// response.completed / response.failed / response.incomplete; and final
// chunks carrying a candidate finishReason/finish_reason — Gemini has no
// sentinel line, so its finishReason chunk is the only end signal (bare
// Gemini shape and code-assist/antigravity "response" envelope both match).
func IsStreamDone(chunk []byte) bool {
	if len(chunk) == 0 {
		return false
	}
	for _, line := range strings.Split(string(chunk), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "event:") {
			if streamTerminalEvent(strings.TrimSpace(strings.TrimPrefix(line, "event:"))) {
				return true
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			line = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		}
		if line == "[DONE]" {
			return true
		}
		if streamTerminalEvent(gjson.Get(line, "type").String()) {
			return true
		}
		if hasFinishReason(line) {
			return true
		}
	}
	return false
}

// hasFinishReason reports a terminal per-candidate finish marker. Gemini SSE
// ends without [DONE]; the last chunk carries finishReason on a candidate.
// The OpenAI finish_reason final chunk is likewise terminal ([DONE] still
// follows and is handled on its own). Every candidate/choice present in the
// chunk must be finished, so a chunk showing partial progress is not an end.
// Caveat: n>1 providers that emit one choice per chunk still terminate on
// the first finisher — benign (the buffer flushes early; siblings flush at
// their own finish chunk).
func hasFinishReason(line string) bool {
	for _, base := range []string{"candidates", "response.candidates"} {
		arr := gjson.Get(line, base).Array()
		if len(arr) == 0 {
			continue
		}
		finished := 0
		for _, c := range arr {
			if c.Get("finishReason").String() != "" {
				finished++
			}
		}
		if finished > 0 && finished == len(arr) {
			return true
		}
	}
	choices := gjson.Get(line, "choices").Array()
	finished := 0
	for _, c := range choices {
		if c.Get("finish_reason").String() != "" {
			finished++
		}
	}
	return finished > 0 && finished == len(choices)
}

func streamTerminalEvent(typ string) bool {
	switch typ {
	case "message_stop", "response.completed", "response.failed", "response.incomplete":
		return true
	}
	return false
}

// ExtractResponseID best-effort pulls the upstream response/chunk id from a
// response body or SSE chunk. Handles bare JSON, multi-line SSE payloads, and
// "event:"-prefixed framing (Responses API emits event+data line pairs).
func ExtractResponseID(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	raw := strings.TrimSpace(string(body))
	if !strings.Contains(raw, "\n") && !strings.HasPrefix(raw, "data:") && !strings.HasPrefix(raw, "event:") {
		return responseIDFromJSON(raw)
	}
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "event:") || strings.HasPrefix(line, ":") {
			continue
		}
		if strings.HasPrefix(line, "data:") {
			line = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if line == "[DONE]" {
				continue
			}
		}
		if id := responseIDFromJSON(line); id != "" {
			return id
		}
	}
	return ""
}

func responseIDFromJSON(raw string) string {
	if id := gjson.Get(raw, "id").String(); id != "" {
		return id
	}
	// Responses-API stream events carry the object under "response".
	return gjson.Get(raw, "response.id").String()
}

// deltaKind classifies a stream text increment.
type deltaKind int

const (
	deltaText deltaKind = iota
	deltaReasoning
	deltaToolArgs
)

// typedDelta is one kind-classified text increment from a stream chunk.
type typedDelta struct {
	Text string
	Kind deltaKind
}

// ExtractStreamTextDeltas best-effort pulls text increments from a stream
// chunk. Multi-line SSE payloads are scanned line by line; same-kind deltas
// (text vs reasoning vs tool arguments) are concatenated so coalesced chunks
// lose nothing, and ALL kinds present in the chunk are returned (a coalesced
// chunk mixing text with reasoning or tool-arg lines drops nothing). [DONE]
// and terminal-event lines yield no delta, so this is safe to call on a
// coalesced final chunk.
func ExtractStreamTextDeltas(sourceFormat string, chunk []byte) []typedDelta {
	if len(chunk) == 0 {
		return nil
	}
	var textDeltas, reasoningDeltas, toolDeltas []string
	for _, line := range strings.Split(string(chunk), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "event:") || strings.HasPrefix(line, ":") {
			continue
		}
		if strings.HasPrefix(line, "data:") {
			line = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if line == "[DONE]" {
				continue
			}
		}
		for _, d := range streamTextDeltaFromJSON([]byte(line)) {
			switch d.Kind {
			case deltaReasoning:
				reasoningDeltas = append(reasoningDeltas, d.Text)
			case deltaToolArgs:
				toolDeltas = append(toolDeltas, d.Text)
			default:
				textDeltas = append(textDeltas, d.Text)
			}
		}
	}
	_ = sourceFormat
	var out []typedDelta
	if len(textDeltas) > 0 {
		out = append(out, typedDelta{strings.Join(textDeltas, ""), deltaText})
	}
	if len(reasoningDeltas) > 0 {
		out = append(out, typedDelta{strings.Join(reasoningDeltas, ""), deltaReasoning})
	}
	if len(toolDeltas) > 0 {
		out = append(out, typedDelta{strings.Join(toolDeltas, ""), deltaToolArgs})
	}
	return out
}

// ExtractStreamTextDelta returns the highest-priority single delta of the
// chunk (text > reasoning > tool args). Kept for simple callers and tests;
// interceptors use ExtractStreamTextDeltas so mixed-kind chunks lose nothing.
func ExtractStreamTextDelta(sourceFormat string, chunk []byte) (text string, kind deltaKind) {
	deltas := ExtractStreamTextDeltas(sourceFormat, chunk)
	if len(deltas) == 0 {
		return "", deltaText
	}
	return deltas[0].Text, deltas[0].Kind
}

// streamTextDeltaFromJSON returns ALL kind-classified deltas on one SSE
// line (a single JSON object can carry content in one choice and tool-call
// arguments in another).
func streamTextDeltaFromJSON(chunk []byte) []typedDelta {
	var out []typedDelta
	emit := func(text string, kind deltaKind) {
		if text != "" {
			out = append(out, typedDelta{text, kind})
		}
	}
	// OpenAI chat deltas: iterate ALL choices (multi-choice streams lose
	// siblings when only choices.0 is read).
	var texts, reasonings, toolArgs []string
	gjson.GetBytes(chunk, "choices").ForEach(func(_, c gjson.Result) bool {
		d := c.Get("delta")
		if !d.Exists() {
			return true
		}
		content := d.Get("content")
		switch {
		case content.Type == gjson.String:
			if t := content.String(); t != "" {
				texts = append(texts, t)
			}
		case content.IsArray():
			// Content-part arrays (translated providers): walk parts with the
			// same reasoning filter as the non-stream path — raw JSON must
			// never be ingested as assistant text.
			content.ForEach(func(_, p gjson.Result) bool {
				if p.Type == gjson.String {
					texts = append(texts, p.String())
					return true
				}
				if isReasoningPartType(p.Get("type").String()) {
					if t := p.Get("text").String(); t != "" {
						reasonings = append(reasonings, t)
					}
					return true
				}
				if t := p.Get("text").String(); t != "" {
					texts = append(texts, t)
				}
				return true
			})
		}
		if rc := d.Get("reasoning_content"); rc.Type == gjson.String {
			if t := rc.String(); t != "" {
				reasonings = append(reasonings, t)
			}
		}
		if rc := d.Get("reasoning"); rc.Type == gjson.String {
			if t := rc.String(); t != "" {
				reasonings = append(reasonings, t)
			}
		}
		// OpenAI chat streamed tool calls: argument fragments buffer whole
		// like Claude partial_json.
		d.Get("tool_calls").ForEach(func(_, tc gjson.Result) bool {
			if a := tc.Get("function.arguments").String(); a != "" {
				toolArgs = append(toolArgs, a)
			}
			return true
		})
		return true
	})
	emit(strings.Join(texts, ""), deltaText)
	emit(strings.Join(reasonings, ""), deltaReasoning)
	emit(strings.Join(toolArgs, ""), deltaToolArgs)
	if len(out) > 0 {
		return out
	}
	// Claude SSE-style content_block_delta
	if dt := gjson.GetBytes(chunk, "delta.text"); dt.Type == gjson.String && dt.String() != "" {
		return []typedDelta{{dt.String(), deltaText}}
	}
	if dt := gjson.GetBytes(chunk, "delta.thinking"); dt.Type == gjson.String && dt.String() != "" {
		return []typedDelta{{dt.String(), deltaReasoning}}
	}
	// Claude tool-use argument deltas (input_json_delta): buffered separately
	// and ingested whole at stream end (never fragmented by the prose
	// segmenter), so streamed tool calls reach memory like the non-stream
	// path's "[tool call] ..." capture.
	if pj := gjson.GetBytes(chunk, "delta.partial_json"); pj.Type == gjson.String && pj.String() != "" {
		return []typedDelta{{pj.String(), deltaToolArgs}}
	}
	// OpenAI responses stream
	if dv := gjson.GetBytes(chunk, "delta"); dv.Type == gjson.String && dv.String() != "" && gjson.GetBytes(chunk, "type").String() != "" {
		t := dv.String()
		typ := gjson.GetBytes(chunk, "type").String()
		if strings.Contains(typ, "reasoning") {
			return []typedDelta{{t, deltaReasoning}}
		}
		if strings.Contains(typ, "function_call_arguments") {
			return []typedDelta{{t, deltaToolArgs}}
		}
		if strings.Contains(typ, "output_text") || strings.Contains(typ, "text") {
			return []typedDelta{{t, deltaText}}
		}
	}
	// Gemini candidates (bare, or under the code-assist/antigravity
	// "response" envelope): iterate ALL candidates and parts; thought:true
	// parts are reasoning, not assistant text.
	var parts, thoughts []string
	candidates := gjson.GetBytes(chunk, "candidates")
	if !candidates.Exists() {
		candidates = gjson.GetBytes(chunk, "response.candidates")
	}
	candidates.ForEach(func(_, c gjson.Result) bool {
		c.Get("content.parts").ForEach(func(_, p gjson.Result) bool {
			if t := p.Get("text").String(); t != "" {
				if p.Get("thought").Bool() {
					thoughts = append(thoughts, t)
				} else {
					parts = append(parts, t)
				}
			}
			return true
		})
		return true
	})
	emit(strings.Join(parts, ""), deltaText)
	emit(strings.Join(thoughts, ""), deltaReasoning)
	return out
}

// --- extractors ---

func extractOpenAI(body []byte) []Message {
	var out []Message
	gjson.GetBytes(body, "messages").ForEach(func(_, v gjson.Result) bool {
		role := v.Get("role").String()
		text := contentToText(v.Get("content"))
		if text == "" {
			// OpenAI chat tool_calls are a sibling of content, not a content
			// part; render them so tool use reaches memory like Claude's.
			text = toolCallsText(v.Get("tool_calls"))
		}
		if text != "" || role != "" {
			out = append(out, Message{Role: role, Text: text})
		}
		return true
	})
	return out
}

// toolCallsText renders OpenAI chat tool_calls as "[tool call] name args"
// lines, matching the content-part capture in contentToText.
func toolCallsText(calls gjson.Result) string {
	var parts []string
	calls.ForEach(func(_, c gjson.Result) bool {
		name := c.Get("function.name").String()
		args := c.Get("function.arguments").String()
		if len(args) > 2000 {
			args = headBytes(args, 2000) + "…"
		}
		if name != "" {
			parts = append(parts, "[tool call] "+name+" "+args)
		}
		return true
	})
	return strings.Join(parts, " ")
}

func extractClaude(body []byte) []Message {
	var out []Message
	// system can be string or block array
	if sys := gjson.GetBytes(body, "system"); sys.Exists() {
		if t := contentToText(sys); t != "" {
			out = append(out, Message{Role: "system", Text: t})
		}
	}
	gjson.GetBytes(body, "messages").ForEach(func(_, v gjson.Result) bool {
		role := v.Get("role").String()
		text := contentToText(v.Get("content"))
		if text != "" || role != "" {
			out = append(out, Message{Role: role, Text: text})
		}
		return true
	})
	return out
}

func extractGemini(body []byte) []Message {
	var out []Message
	if sys := gjson.GetBytes(body, "systemInstruction"); sys.Exists() {
		if t := geminiPartsText(sys.Get("parts")); t != "" {
			out = append(out, Message{Role: "system", Text: t})
		}
	}
	if sys := gjson.GetBytes(body, "system_instruction"); sys.Exists() {
		if t := geminiPartsText(sys.Get("parts")); t != "" {
			out = append(out, Message{Role: "system", Text: t})
		}
	}
	gjson.GetBytes(body, "contents").ForEach(func(_, v gjson.Result) bool {
		role := v.Get("role").String()
		if role == "model" {
			role = "assistant"
		}
		if role == "" {
			// Gemini contents default to the user role.
			role = "user"
		}
		text := geminiPartsText(v.Get("parts"))
		if text != "" || role != "" {
			out = append(out, Message{Role: role, Text: text})
		}
		return true
	})
	return out
}

func extractOpenAIResponse(body []byte) []Message {
	var out []Message
	if inst := gjson.GetBytes(body, "instructions").String(); inst != "" {
		out = append(out, Message{Role: "system", Text: inst})
	}
	// input may be string or array of items
	input := gjson.GetBytes(body, "input")
	if input.Type == gjson.String {
		out = append(out, Message{Role: "user", Text: input.String()})
		return out
	}
	input.ForEach(func(_, v gjson.Result) bool {
		typ := v.Get("type").String()
		if typ == "reasoning" {
			// Hidden chain-of-thought (resent every turn by store=false
			// clients): never extract it — it would be durable-ingested as a
			// user turn past ingest_reasoning=false, like Claude thinking
			// blocks and Gemini thought parts.
			return true
		}
		if typ == "function_call" {
			// Responses/Codex tool calls: render like chat tool_calls so they
			// reach memory.
			name := v.Get("name").String()
			args := v.Get("arguments").String()
			if len(args) > 2000 {
				args = headBytes(args, 2000) + "…"
			}
			if name != "" {
				out = append(out, Message{Role: "assistant", Text: "[tool call] " + name + " " + args})
			}
			return true
		}
		role := v.Get("role").String()
		if role == "" {
			switch typ {
			case "message":
				role = v.Get("role").String()
				if role == "" {
					role = "user"
				}
			case "function_call_output", "item_reference":
				role = "tool"
			default:
				role = "user"
			}
		}
		text := contentToText(v.Get("content"))
		if text == "" {
			text = v.Get("text").String()
		}
		if text == "" {
			// function_call_output output may be a string or content parts.
			text = v.Get("output").String()
			if text == "" {
				text = contentToText(v.Get("output"))
			}
		}
		if text != "" || role != "" {
			out = append(out, Message{Role: role, Text: text})
		}
		return true
	})
	return out
}

func contentToText(v gjson.Result) string {
	if !v.Exists() {
		return ""
	}
	if v.Type == gjson.String {
		return v.String()
	}
	var parts []string
	v.ForEach(func(_, p gjson.Result) bool {
		if p.Type == gjson.String {
			parts = append(parts, p.String())
			return true
		}
		typ := p.Get("type").String()
		switch typ {
		case "text", "input_text", "output_text":
			if t := p.Get("text").String(); t != "" {
				parts = append(parts, t)
			}
		case "reasoning_text":
			// Chain-of-thought: never extracted (see extractOpenAIResponse).
		case "tool_use", "toolCall", "function_call":
			name := p.Get("name").String()
			args := p.Get("input").Raw
			if args == "" {
				args = p.Get("arguments").Raw
			}
			if len(args) > 2000 {
				args = headBytes(args, 2000) + "…"
			}
			parts = append(parts, "[tool call] "+name+" "+args)
		case "tool_result":
			if t := p.Get("content").String(); t != "" {
				parts = append(parts, t)
			} else if t := contentToText(p.Get("content")); t != "" {
				parts = append(parts, t)
			}
		default:
			// Unknown part types fail CLOSED for reasoning-shaped parts:
			// chain-of-thought must never leak past ingest_reasoning.
			if isReasoningPartType(typ) {
				return true
			}
			if t := p.Get("text").String(); t != "" {
				parts = append(parts, t)
			}
		}
		return true
	})
	return strings.TrimSpace(strings.Join(parts, " "))
}

// isReasoningPartType reports content-part types that carry chain-of-thought.
func isReasoningPartType(typ string) bool {
	return strings.Contains(typ, "reasoning") || strings.Contains(typ, "thinking")
}

func geminiPartsText(parts gjson.Result) string {
	var out []string
	parts.ForEach(func(_, p gjson.Result) bool {
		// thought:true parts are chain-of-thought: skip like Claude thinking
		// blocks, so ingest_reasoning=false holds for Gemini too.
		if p.Get("thought").Bool() {
			return true
		}
		if t := p.Get("text").String(); t != "" {
			out = append(out, t)
			return true
		}
		// Gemini tool calls/results are parts, not a content sibling; render
		// them so tool use reaches memory like OpenAI/Claude tool calls.
		if fc := p.Get("functionCall"); fc.Exists() {
			name := fc.Get("name").String()
			args := fc.Get("args").Raw
			if len(args) > 2000 {
				args = headBytes(args, 2000) + "…"
			}
			if name != "" {
				out = append(out, "[tool call] "+name+" "+args)
			}
			return true
		}
		if fr := p.Get("functionResponse"); fr.Exists() {
			name := fr.Get("name").String()
			resp := fr.Get("response").Raw
			if len(resp) > 2000 {
				resp = headBytes(resp, 2000) + "…"
			}
			if name != "" || resp != "" {
				out = append(out, strings.TrimSpace("[tool result] "+name+" "+resp))
			}
		}
		return true
	})
	return strings.TrimSpace(strings.Join(out, " "))
}

// --- injectors ---

func mergeMemoryInto(existing, block string) string {
	existing = stripMemoryBlock(existing)
	existing = strings.TrimSpace(existing)
	if existing == "" {
		return block
	}
	return existing + "\n\n" + block
}

func stripMemoryBlock(s string) string {
	const open = "<cortext_memory>"
	const close = "</cortext_memory>"
	// Only a block this plugin injected — identified by its fixed header
	// line — may be stripped. A user's own literal <cortext_memory> text
	// (closed or not) is left untouched.
	const header = "\nThe following are stored memory snippets"
	search := 0
	for {
		idx := strings.Index(s[search:], open)
		if idx < 0 {
			return s
		}
		start := search + idx
		if !strings.HasPrefix(s[start+len(open):], header) {
			search = start + len(open)
			continue
		}
		rel := strings.Index(s[start:], close)
		if rel < 0 {
			return s
		}
		end := start + rel + len(close)
		s = s[:start] + s[end:]
		search = start
	}
}

func injectOpenAISystem(body []byte, block string) ([]byte, error) {
	msgs := gjson.GetBytes(body, "messages")
	if !msgs.IsArray() {
		// No messages array — merge into a system field if present. Bodies
		// with neither (unrecognized formats, non-chat endpoints) pass
		// through untouched: never invent a foreign messages array.
		if gjson.GetBytes(body, "system").Exists() {
			merged := mergeMemoryInto(gjson.GetBytes(body, "system").String(), block)
			return setJSON(body, "system", merged)
		}
		return body, nil
	}
	// Prefer first system message.
	var firstSystem = -1
	arr := msgs.Array()
	for i, m := range arr {
		if m.Get("role").String() == "system" {
			firstSystem = i
			break
		}
	}
	if firstSystem >= 0 {
		path := "messages." + itoa(firstSystem) + ".content"
		content := arr[firstSystem].Get("content")
		if content.IsArray() {
			// Multipart system message: keep non-text parts (image_url,
			// cache_control, …) intact; strip any previous memory block from
			// text parts and append the fresh block as a new text part.
			var rebuilt []any
			for _, p := range content.Array() {
				var obj any
				if err := jsonUnmarshal([]byte(p.Raw), &obj); err != nil {
					continue
				}
				if mp, ok := obj.(map[string]any); ok {
					if t, ok := mp["text"].(string); ok {
						stripped := strings.TrimSpace(stripMemoryBlock(t))
						if stripped == "" {
							continue
						}
						mp["text"] = stripped
					}
				}
				rebuilt = append(rebuilt, obj)
			}
			rebuilt = append(rebuilt, map[string]string{"type": "text", "text": block})
			return setJSON(body, path, rebuilt)
		}
		cur := contentToText(content)
		return setJSON(body, path, mergeMemoryInto(cur, block))
	}
	// Prepend a system message.
	newMsg := map[string]string{"role": "system", "content": block}
	// Rebuild array: new system + existing
	var rebuilt []any
	rebuilt = append(rebuilt, newMsg)
	for _, m := range arr {
		var obj any
		if err := jsonUnmarshal([]byte(m.Raw), &obj); err == nil {
			rebuilt = append(rebuilt, obj)
		}
	}
	return setJSON(body, "messages", rebuilt)
}

func injectClaudeSystem(body []byte, block string) ([]byte, error) {
	sys := gjson.GetBytes(body, "system")
	if !sys.Exists() {
		return setJSON(body, "system", block)
	}
	if sys.Type == gjson.String {
		return setJSON(body, "system", mergeMemoryInto(sys.String(), block))
	}
	// block array: append a text block, first stripping any previous memory
	// block from text parts so injection stays idempotent.
	arr := sys.Array()
	var rebuilt []any
	for _, m := range arr {
		var obj any
		if err := jsonUnmarshal([]byte(m.Raw), &obj); err != nil {
			continue
		}
		if mp, ok := obj.(map[string]any); ok {
			if t, ok := mp["text"].(string); ok {
				stripped := strings.TrimSpace(stripMemoryBlock(t))
				if stripped == "" {
					continue
				}
				mp["text"] = stripped
			}
		}
		rebuilt = append(rebuilt, obj)
	}
	rebuilt = append(rebuilt, map[string]string{"type": "text", "text": block})
	return setJSON(body, "system", rebuilt)
}

func injectGeminiSystem(body []byte, block string) ([]byte, error) {
	key := "systemInstruction"
	sys := gjson.GetBytes(body, key)
	if !sys.Exists() {
		key = "system_instruction"
		sys = gjson.GetBytes(body, key)
	}
	if !sys.Exists() {
		return setJSON(body, "systemInstruction", map[string]any{
			"parts": []map[string]string{{"text": block}},
		})
	}
	parts := sys.Get("parts")
	var rebuilt []any
	// Strip any previous memory block from text parts so injection stays
	// idempotent, then append the fresh block.
	parts.ForEach(func(_, p gjson.Result) bool {
		var obj any
		if err := jsonUnmarshal([]byte(p.Raw), &obj); err != nil {
			return true
		}
		if mp, ok := obj.(map[string]any); ok {
			if t, ok := mp["text"].(string); ok {
				stripped := strings.TrimSpace(stripMemoryBlock(t))
				if stripped == "" {
					return true
				}
				mp["text"] = stripped
			}
		}
		rebuilt = append(rebuilt, obj)
		return true
	})
	rebuilt = append(rebuilt, map[string]string{"text": block})
	return setJSON(body, key+".parts", rebuilt)
}

func injectOpenAIResponseInstructions(body []byte, block string) ([]byte, error) {
	cur := gjson.GetBytes(body, "instructions").String()
	return setJSON(body, "instructions", mergeMemoryInto(cur, block))
}

// setJSON writes JSON without HTML-escaping angle brackets (memory fences).
// sjson's default Marshal escapes <>&; we encode with EscapeHTML=false and SetRaw.
func setJSON(body []byte, path string, value any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(value); err != nil {
		return nil, err
	}
	raw := bytes.TrimSpace(buf.Bytes())
	return sjson.SetRawBytes(body, path, raw)
}

// --- windowing ---

func windowOpenAI(body []byte, n int) ([]byte, error) {
	msgs := gjson.GetBytes(body, "messages").Array()
	if len(msgs) == 0 {
		return body, nil
	}
	var rest []gjson.Result
	for _, m := range msgs {
		if r := m.Get("role").String(); r != "system" && r != "developer" {
			rest = append(rest, m)
		}
	}
	if len(rest) <= n {
		// Nothing to trim: leave the client's payload byte-identical.
		return body, nil
	}
	start := len(rest) - n
	// Orphan guard: a leading tool-result message ("tool", legacy
	// "function") lost its parent assistant tool_calls message; OpenAI
	// rejects that transcript. Extend left.
	for start > 0 {
		r := rest[start].Get("role").String()
		if r != "tool" && r != "function" {
			break
		}
		start--
	}
	keep := make(map[int]bool, len(rest)-start)
	for i := start; i < len(rest); i++ {
		keep[i] = true
	}
	// Rebuild in ORIGINAL order: system/developer messages keep their
	// positions (no hoisting to the front).
	var out []any
	ri := 0
	for _, m := range msgs {
		if r := m.Get("role").String(); r != "system" && r != "developer" {
			if !keep[ri] {
				ri++
				continue
			}
			ri++
		}
		var obj any
		if err := jsonUnmarshal([]byte(m.Raw), &obj); err == nil {
			out = append(out, obj)
		}
	}
	return setJSON(body, "messages", out)
}

func windowClaude(body []byte, n int) ([]byte, error) {
	msgs := gjson.GetBytes(body, "messages").Array()
	if len(msgs) <= n {
		return body, nil
	}
	start := len(msgs) - n
	// Orphan guard: Anthropic rejects a window whose head is not a user turn,
	// and a user turn whose tool_result lost its parent tool_use. Extend left.
	for start > 0 && claudeOrphanHead(msgs[start]) {
		start--
	}
	var rest []any
	for _, m := range msgs[start:] {
		var obj any
		if err := jsonUnmarshal([]byte(m.Raw), &obj); err == nil {
			rest = append(rest, obj)
		}
	}
	return setJSON(body, "messages", rest)
}

// claudeOrphanHead reports a kept-window head Anthropic would reject.
func claudeOrphanHead(m gjson.Result) bool {
	if m.Get("role").String() != "user" {
		return true
	}
	orphan := false
	m.Get("content").ForEach(func(_, b gjson.Result) bool {
		if b.Get("type").String() == "tool_result" {
			orphan = true
			return false
		}
		return true
	})
	return orphan
}

func windowOpenAIResponse(body []byte, n int) ([]byte, error) {
	input := gjson.GetBytes(body, "input")
	if !input.IsArray() {
		return body, nil
	}
	arr := input.Array()
	if len(arr) <= n {
		return body, nil
	}
	start := len(arr) - n
	// Orphan guard: a leading tool-output item lost its parent call item.
	for start > 0 {
		t := arr[start].Get("type").String()
		if t != "function_call_output" && t != "computer_call_output" && t != "custom_tool_call_output" {
			break
		}
		start--
	}
	var rest []any
	for _, m := range arr[start:] {
		var obj any
		if err := jsonUnmarshal([]byte(m.Raw), &obj); err == nil {
			rest = append(rest, obj)
		}
	}
	return setJSON(body, "input", rest)
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [12]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(b[pos:])
}

// jsonUnmarshal avoids importing encoding/json name clash noise in this file
// by wrapping the standard library.
func jsonUnmarshal(data []byte, v any) error {
	return stdJSONUnmarshal(data, v)
}
