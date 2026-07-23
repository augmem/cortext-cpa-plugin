//go:build cortext_native

package main

import (
	"fmt"
	"strings"

	cortext "github.com/gabrielwillen/cortext/bindings/go"
)

func openDefaultEngine(dbPath string, cfg PluginConfig) (Engine, error) {
	nativeCfg := &cortext.Config{
		Focus:       cortext.Ptr(cfg.Focus),
		Sensitivity: cortext.Ptr(cfg.Sensitivity),
		Stability:   cortext.Ptr(cfg.Stability),
	}
	h, err := cortext.New(dbPath, nativeCfg)
	if err != nil {
		return nil, fmt.Errorf("cortext open %s: %w", dbPath, err)
	}
	return &nativeEngine{h: h}, nil
}

type nativeEngine struct {
	h *cortext.Handle
}

func (e *nativeEngine) ProcessText(text, source string, retention Retention) (ContextPacket, error) {
	text = strings.TrimSpace(text)
	if text == "" || e == nil || e.h == nil {
		return ContextPacket{}, nil
	}
	opts := &cortext.ProcessOptions{
		OmitEmbedding: true,
		Retention:     cortext.Retention(retention),
	}
	raw, err := e.h.ProcessTextWithOptions(text, source, opts)
	if err != nil {
		return ContextPacket{}, err
	}
	return packetFromMap(raw), nil
}

func (e *nativeEngine) Consolidate() error {
	if e == nil || e.h == nil {
		return nil
	}
	_, err := e.h.Consolidate()
	return err
}

func (e *nativeEngine) Flush() error {
	if e == nil || e.h == nil {
		return nil
	}
	return e.h.Flush()
}

func (e *nativeEngine) Close() error {
	if e == nil || e.h == nil {
		return nil
	}
	e.h.Close()
	e.h = nil
	return nil
}

func packetFromMap(raw map[string]any) ContextPacket {
	var out ContextPacket
	if raw == nil {
		return out
	}
	out.RetrievedMemory = memoryItemsFromAny(raw["retrieved_memory"])
	out.WorkingMemory = memoryItemsFromAny(raw["working_memory"])
	if v, ok := raw["should_interrupt"].(bool); ok {
		out.ShouldInterrupt = v
	}
	if v, ok := raw["at_boundary"].(bool); ok {
		out.AtBoundary = v
	}
	return out
}

func memoryItemsFromAny(v any) []MemoryItem {
	list, ok := v.([]any)
	if !ok || len(list) == 0 {
		return nil
	}
	out := make([]MemoryItem, 0, len(list))
	for _, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		text := memoryTextFromMap(m)
		if text == "" {
			continue
		}
		mod, _ := m["modality"].(string)
		out = append(out, MemoryItem{Text: text, Modality: mod})
	}
	return out
}

func memoryTextFromMap(m map[string]any) string {
	if t, ok := m["text"].(string); ok && strings.TrimSpace(t) != "" {
		return strings.TrimSpace(t)
	}
	switch c := m["content"].(type) {
	case string:
		return strings.TrimSpace(c)
	case []any:
		var parts []string
		for _, p := range c {
			switch part := p.(type) {
			case string:
				parts = append(parts, part)
			case map[string]any:
				if t, ok := part["text"].(string); ok {
					parts = append(parts, t)
				}
			}
		}
		return strings.TrimSpace(strings.Join(parts, " "))
	}
	return ""
}
