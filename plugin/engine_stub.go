//go:build !cortext_native

package main

import (
	"strings"
	"sync"
)

// openDefaultEngine provides an in-process fallback so the plugin builds and
// unit-tests without linking libcortext. Production builds should use:
//
//	go build -tags cortext_native -buildmode=c-shared
//
// See engine_native.go and the root Makefile.
func openDefaultEngine(dbPath string, cfg PluginConfig) (Engine, error) {
	_ = dbPath
	_ = cfg
	return newStubEngine(), nil
}

type stubEngine struct {
	mu   sync.Mutex
	rows []string
}

func newStubEngine() *stubEngine {
	return &stubEngine{rows: make([]string, 0, 64)}
}

func (e *stubEngine) ProcessText(text, source string, retention Retention) (ContextPacket, error) {
	_ = source
	text = strings.TrimSpace(text)
	if text == "" {
		return ContextPacket{}, nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if retention == RetentionDurable || retention == RetentionNatural || retention == RetentionBoundary {
		e.rows = append(e.rows, text)
		if len(e.rows) > 256 {
			e.rows = e.rows[len(e.rows)-256:]
		}
	}
	// Trivial keyword overlap recall for offline dev only.
	var retrieved []MemoryItem
	if retention == RetentionEphemeral || retention == RetentionNatural {
		q := strings.ToLower(text)
		for i := len(e.rows) - 1; i >= 0 && len(retrieved) < 8; i-- {
			row := e.rows[i]
			if row == text {
				continue
			}
			if strings.Contains(strings.ToLower(row), q) || sharesToken(q, strings.ToLower(row)) {
				retrieved = append(retrieved, MemoryItem{Text: row, Modality: "text"})
			}
		}
	}
	return ContextPacket{RetrievedMemory: retrieved}, nil
}

func (e *stubEngine) Consolidate() error { return nil }
func (e *stubEngine) Flush() error       { return nil }
func (e *stubEngine) Close() error       { return nil }

func sharesToken(query, row string) bool {
	for _, tok := range strings.Fields(query) {
		tok = strings.Trim(tok, ".,!?;:\"'()[]{}")
		if len(tok) < 4 {
			continue
		}
		if strings.Contains(row, tok) {
			return true
		}
	}
	return false
}
