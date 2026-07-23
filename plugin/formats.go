package main

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

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
	fmtInteractions   = "interactions"
	fmtAntigravity    = "antigravity"
)

// ExtractMessages pulls role/text pairs from a client request body.
func ExtractMessages(sourceFormat string, body []byte) []Message {
	switch strings.ToLower(strings.TrimSpace(sourceFormat)) {
	case fmtClaude:
		return extractClaude(body)
	case fmtGemini, fmtAntigravity, fmtInteractions:
		return extractGemini(body)
	case fmtOpenAIResponse, fmtCodex:
		return extractOpenAIResponse(body)
	default:
		return extractOpenAI(body)
	}
}

// LatestUserText returns the most recent user message text.
func LatestUserText(msgs []Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if strings.EqualFold(msgs[i].Role, "user") && strings.TrimSpace(msgs[i].Text) != "" {
			return strings.TrimSpace(msgs[i].Text)
		}
	}
	if len(msgs) > 0 {
		return strings.TrimSpace(msgs[len(msgs)-1].Text)
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
	case fmtGemini, fmtAntigravity, fmtInteractions:
		return injectGeminiSystem(body, block)
	case fmtOpenAIResponse, fmtCodex:
		return injectOpenAIResponseInstructions(body, block)
	default:
		return injectOpenAISystem(body, block)
	}
}

// WindowMessages keeps system messages + the last n non-system messages.
func WindowMessages(sourceFormat string, body []byte, n int) ([]byte, error) {
	if n <= 0 || len(body) == 0 {
		return body, nil
	}
	switch strings.ToLower(strings.TrimSpace(sourceFormat)) {
	case fmtClaude:
		return windowClaude(body, n)
	case fmtOpenAIResponse, fmtCodex:
		return windowOpenAIResponse(body, n)
	case fmtGemini, fmtAntigravity, fmtInteractions:
		// Gemini contents windowing is more invasive; skip for v0.
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
	case fmtGemini, fmtAntigravity, fmtInteractions:
		var parts []string
		gjson.GetBytes(body, "candidates").ForEach(func(_, cand gjson.Result) bool {
			cand.Get("content.parts").ForEach(func(_, p gjson.Result) bool {
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
			item.Get("content").ForEach(func(_, c gjson.Result) bool {
				if t := c.Get("text").String(); t != "" {
					parts = append(parts, t)
				}
				return true
			})
			return true
		})
		return strings.TrimSpace(strings.Join(parts, "\n"))
	default:
		// chat.completion
		if t := gjson.GetBytes(body, "choices.0.message.content").String(); t != "" {
			return strings.TrimSpace(t)
		}
		// content array
		var parts []string
		gjson.GetBytes(body, "choices.0.message.content").ForEach(func(_, v gjson.Result) bool {
			if v.Type == gjson.String {
				parts = append(parts, v.String())
				return true
			}
			if t := v.Get("text").String(); t != "" {
				parts = append(parts, t)
			}
			return true
		})
		return strings.TrimSpace(strings.Join(parts, "\n"))
	}
}

// ExtractStreamTextDelta best-effort pulls a text increment from a stream chunk.
func ExtractStreamTextDelta(sourceFormat string, chunk []byte) (text string, isReasoning bool) {
	if len(chunk) == 0 {
		return "", false
	}
	raw := strings.TrimSpace(string(chunk))
	// SSE "data: …" lines
	if strings.HasPrefix(raw, "data:") {
		raw = strings.TrimSpace(strings.TrimPrefix(raw, "data:"))
		if raw == "[DONE]" {
			return "", false
		}
		chunk = []byte(raw)
	}
	// OpenAI chat delta
	if d := gjson.GetBytes(chunk, "choices.0.delta"); d.Exists() {
		if t := d.Get("content").String(); t != "" {
			return t, false
		}
		if t := d.Get("reasoning_content").String(); t != "" {
			return t, true
		}
		if t := d.Get("reasoning").String(); t != "" {
			return t, true
		}
	}
	// Claude SSE-style content_block_delta
	if t := gjson.GetBytes(chunk, "delta.text").String(); t != "" {
		return t, false
	}
	if t := gjson.GetBytes(chunk, "delta.thinking").String(); t != "" {
		return t, true
	}
	// OpenAI responses stream
	if t := gjson.GetBytes(chunk, "delta").String(); t != "" && gjson.GetBytes(chunk, "type").String() != "" {
		typ := gjson.GetBytes(chunk, "type").String()
		if strings.Contains(typ, "reasoning") {
			return t, true
		}
		if strings.Contains(typ, "output_text") || strings.Contains(typ, "text") {
			return t, false
		}
	}
	if t := gjson.GetBytes(chunk, "candidates.0.content.parts.0.text").String(); t != "" {
		return t, false
	}
	_ = sourceFormat
	return "", false
}

// --- extractors ---

func extractOpenAI(body []byte) []Message {
	var out []Message
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
		role := v.Get("role").String()
		if role == "" {
			typ := v.Get("type").String()
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
			text = v.Get("output").String()
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
		case "tool_use", "toolCall", "function_call":
			name := p.Get("name").String()
			args := p.Get("input").Raw
			if args == "" {
				args = p.Get("arguments").Raw
			}
			if len(args) > 2000 {
				args = args[:2000] + "…"
			}
			parts = append(parts, "[tool call] "+name+" "+args)
		case "tool_result":
			if t := p.Get("content").String(); t != "" {
				parts = append(parts, t)
			} else if t := contentToText(p.Get("content")); t != "" {
				parts = append(parts, t)
			}
		default:
			if t := p.Get("text").String(); t != "" {
				parts = append(parts, t)
			}
		}
		return true
	})
	return strings.TrimSpace(strings.Join(parts, " "))
}

func geminiPartsText(parts gjson.Result) string {
	var out []string
	parts.ForEach(func(_, p gjson.Result) bool {
		if t := p.Get("text").String(); t != "" {
			out = append(out, t)
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
	for {
		start := strings.Index(s, "<cortext_memory>")
		if start < 0 {
			return s
		}
		end := strings.Index(s[start:], "</cortext_memory>")
		if end < 0 {
			return s[:start]
		}
		end = start + end + len("</cortext_memory>")
		s = s[:start] + s[end:]
	}
}

func injectOpenAISystem(body []byte, block string) ([]byte, error) {
	msgs := gjson.GetBytes(body, "messages")
	if !msgs.IsArray() {
		// No messages array — prepend via system field if present, else invent messages.
		if gjson.GetBytes(body, "system").Exists() {
			merged := mergeMemoryInto(gjson.GetBytes(body, "system").String(), block)
			return setJSON(body, "system", merged)
		}
		return setJSON(body, "messages", []map[string]string{{"role": "system", "content": block}})
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
		cur := contentToText(arr[firstSystem].Get("content"))
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
	// block array: append a text block
	arr := sys.Array()
	var rebuilt []any
	for _, m := range arr {
		var obj any
		if err := jsonUnmarshal([]byte(m.Raw), &obj); err == nil {
			rebuilt = append(rebuilt, obj)
		}
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
	parts.ForEach(func(_, p gjson.Result) bool {
		var obj any
		if err := jsonUnmarshal([]byte(p.Raw), &obj); err == nil {
			rebuilt = append(rebuilt, obj)
		}
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
	var system []any
	var rest []any
	for _, m := range msgs {
		var obj any
		if err := jsonUnmarshal([]byte(m.Raw), &obj); err != nil {
			continue
		}
		if m.Get("role").String() == "system" {
			system = append(system, obj)
		} else {
			rest = append(rest, obj)
		}
	}
	if len(rest) > n {
		rest = rest[len(rest)-n:]
	}
	out := append(system, rest...)
	return setJSON(body, "messages", out)
}

func windowClaude(body []byte, n int) ([]byte, error) {
	msgs := gjson.GetBytes(body, "messages").Array()
	if len(msgs) <= n {
		return body, nil
	}
	var rest []any
	for _, m := range msgs[len(msgs)-n:] {
		var obj any
		if err := jsonUnmarshal([]byte(m.Raw), &obj); err == nil {
			rest = append(rest, obj)
		}
	}
	return setJSON(body, "messages", rest)
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
	var rest []any
	for _, m := range arr[len(arr)-n:] {
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
