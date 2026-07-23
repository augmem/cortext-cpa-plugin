package main

import (
	"sync"
)

// Retention mirrors the Cortext C API.
type Retention int

const (
	RetentionNatural Retention = iota
	RetentionDurable
	RetentionBoundary
	RetentionEphemeral
)

// Engine is the per-scope Cortext handle surface used by interceptors.
type Engine interface {
	ProcessText(text, source string, retention Retention) (ContextPacket, error)
	Consolidate() error
	Flush() error
	Close() error
}

// EngineFactory opens a durable engine for a SQLite path.
type EngineFactory func(dbPath string, cfg PluginConfig) (Engine, error)

var (
	engineFactoryMu sync.RWMutex
	engineFactory   EngineFactory = openDefaultEngine
)

// SetEngineFactory overrides the factory (tests / alternate backends).
func SetEngineFactory(f EngineFactory) {
	engineFactoryMu.Lock()
	defer engineFactoryMu.Unlock()
	if f == nil {
		engineFactory = openDefaultEngine
		return
	}
	engineFactory = f
}

func openEngine(dbPath string, cfg PluginConfig) (Engine, error) {
	engineFactoryMu.RLock()
	f := engineFactory
	engineFactoryMu.RUnlock()
	return f(dbPath, cfg)
}

// openDefaultEngine is set by engine_native.go or engine_stub.go.
