package main

import (
	"strings"
	"sync"
	"unicode/utf8"
)

// maxBusScopes bounds staged interrupt-gate memory so abandoned scopes cannot
// grow the map without bound. Oldest keys (map iteration order is fine for a
// soft bound) are dropped when full.
const maxBusScopes = 256

// maxBusBlock caps concatenated staged text per scope (bytes).
const maxBusBlock = 16 * 1024

// tailBytes keeps the last max bytes of s, advanced to a rune boundary so the
// cut never splits a multi-byte UTF-8 rune into the injected prompt, then to
// the next line boundary so a staged block never starts mid-bullet.
func tailBytes(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := len(s) - max
	for cut < len(s) && !utf8.RuneStart(s[cut]) {
		cut++
	}
	if nl := strings.Index(s[cut:], "\n"); nl > 0 {
		cut += nl + 1
	}
	return s[cut:]
}

// InterruptBus stages recalled memory from the stream gate until the next
// request assemble for the same scope. Proxy hosts cannot revise mid-turn.
type InterruptBus struct {
	mu     sync.Mutex
	staged map[string]string
}

func NewInterruptBus() *InterruptBus {
	return &InterruptBus{staged: make(map[string]string)}
}

func (b *InterruptBus) Stage(scope, block string) {
	if b == nil || scope == "" || block == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if prev := b.staged[scope]; prev != "" {
		merged := prev + "\n" + block
		// Keep the newest tail so recent gate hits survive.
		merged = tailBytes(merged, maxBusBlock)
		b.staged[scope] = merged
	} else {
		b.staged[scope] = tailBytes(block, maxBusBlock)
	}
	for len(b.staged) > maxBusScopes {
		// Drop an arbitrary excess key (Go map iteration).
		for k := range b.staged {
			if k == scope {
				continue
			}
			delete(b.staged, k)
			break
		}
		// Only the current scope remains and still over limit — stop.
		if len(b.staged) <= maxBusScopes {
			break
		}
		// Single-scope overflow is impossible given maxBusScopes >= 1.
		if len(b.staged) == 1 {
			break
		}
	}
}

func (b *InterruptBus) Take(scope string) string {
	if b == nil || scope == "" {
		return ""
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	block := b.staged[scope]
	delete(b.staged, scope)
	return block
}

// Drop removes staged memory for one scope (engine eviction / dispose).
func (b *InterruptBus) Drop(scope string) {
	if b == nil || scope == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.staged, scope)
}

// Clear removes all staged memory (shutdown / material reconfigure).
func (b *InterruptBus) Clear() {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.staged = make(map[string]string)
}
