package main

import "sync"

// InterruptBus stages recalled memory from the stream gate until the next
// request assemble for the same scope. Proxy hosts cannot revise mid-turn.
type InterruptBus struct {
	mu    sync.Mutex
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
		b.staged[scope] = prev + "\n" + block
		return
	}
	b.staged[scope] = block
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
