//go:build !cortext_native

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// engineFlavor identifies the linked engine for registration/logs.
// Stub builds are CI/protocol only — not production Cortext retrieval.
const engineFlavor = "stub"

// openDefaultEngine provides an in-process fallback so the plugin builds and
// unit-tests without linking libcortext. Production builds should use:
//
//	go build -tags cortext_native -buildmode=c-shared
//
// See engine_native.go and the root Makefile.
//
// The stub persists durable rows next to dbPath so reopen/restart isolation
// tests can pass offline. It is NOT Cortext retrieval quality.
func openDefaultEngine(dbPath string, cfg PluginConfig) (Engine, error) {
	_ = cfg
	return openStubEngine(dbPath)
}

type stubEngine struct {
	mu             sync.Mutex
	path           string
	rows           []string
	closed         bool
	consolidations int
	// consolidationState is a test hook: the value reported on process results.
	consolidationState string
}

func openStubEngine(dbPath string) (*stubEngine, error) {
	e := &stubEngine{
		path: stubPersistPath(dbPath),
		rows: make([]string, 0, 64),
	}
	if e.path != "" {
		if b, err := os.ReadFile(e.path); err == nil && len(b) > 0 {
			var rows []string
			if json.Unmarshal(b, &rows) == nil {
				e.rows = rows
			}
		}
	}
	return e, nil
}

func stubPersistPath(dbPath string) string {
	if strings.TrimSpace(dbPath) == "" || dbPath == ":memory:" {
		return ""
	}
	return dbPath + ".stub.json"
}

func (e *stubEngine) ProcessText(text, source string, retention Retention) (ContextPacket, error) {
	_ = source
	text = strings.TrimSpace(text)
	if text == "" {
		return ContextPacket{}, nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return ContextPacket{}, errEngineClosed
	}
	if retention == RetentionDurable || retention == RetentionNatural || retention == RetentionBoundary {
		e.rows = append(e.rows, text)
		if len(e.rows) > 256 {
			e.rows = e.rows[len(e.rows)-256:]
		}
		_ = e.persistLocked()
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
	return ContextPacket{RetrievedMemory: retrieved, ConsolidationState: e.consolidationState}, nil
}

func (e *stubEngine) persistLocked() error {
	if e.path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(e.path), dataDirPerm); err != nil {
		return err
	}
	b, err := json.Marshal(e.rows)
	if err != nil {
		return err
	}
	// Atomic write (temp + rename): a torn write would silently reset the
	// scope's memory on next open.
	// 0o600: session memory must not be world-readable.
	tmp := e.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, e.path)
}

func (e *stubEngine) Consolidate() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return nil
	}
	e.consolidations++
	return nil
}

func (e *stubEngine) Flush() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.persistLocked()
}

func (e *stubEngine) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	err := e.persistLocked()
	e.closed = true
	return err
}

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
