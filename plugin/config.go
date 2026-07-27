package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// MemoryScope controls which SQLite file a request lands in.
type MemoryScope string

const (
	ScopeSession MemoryScope = "session"
	ScopeAgent   MemoryScope = "agent"
	ScopeGlobal  MemoryScope = "global"
)

// PluginConfig mirrors the OpenClaw / Hermes knobs that still make sense at
// the HTTP-proxy boundary.
type PluginConfig struct {
	// Enabled turns all interceptors into pass-through when false.
	Enabled bool `yaml:"enabled"`

	// DataDir is the root for per-scope SQLite files. Expands ~ and $HOME.
	// Default: ~/.cli-proxy-api/cortext
	DataDir string `yaml:"data_dir"`

	// MemoryScope: session (default), agent, or global.
	MemoryScope MemoryScope `yaml:"memory_scope"`

	// Engine knobs (mirrors @augmem/cortext + OpenClaw defaults).
	Focus       float64 `yaml:"focus"`
	Sensitivity float64 `yaml:"sensitivity"`
	Stability   float64 `yaml:"stability"`

	// RecallLimit caps injected memory lines (0 disables recall/injection;
	// absence keeps the default of 12).
	RecallLimit int `yaml:"recall_limit"`

	// IngestAssistant durable-ingests assistant text from responses/streams.
	IngestAssistant bool `yaml:"ingest_assistant"`

	// IngestReasoning durable-ingests thinking/reasoning stream segments when true.
	IngestReasoning bool `yaml:"ingest_reasoning"`

	// InterruptGate runs ephemeral recall on stream segments and stages hits
	// for the next request (proxy has no mid-turn revise).
	InterruptGate bool `yaml:"interrupt_gate"`

	// AutoConsolidate runs consolidate() when the engine asks for it
	// (consolidation_state hint, cortext ≥1.2.2 — the same stable contract the
	// reference cortext.ts binding types), falling back to every
	// ConsolidateEvery durable writes for engines that never emit the hint,
	// plus once at shutdown/eviction. Attempts are throttled
	// (≤1 per ConsolidateEvery/5 writes per scope) so a persistent hint
	// cannot storm the request path. Under default knobs the cadence usually
	// fires first; the hint is the engine's escalation path.
	AutoConsolidate bool `yaml:"auto_consolidate"`

	// ConsolidateEvery is the fallback cadence: durable ingests per scope
	// between consolidate() runs when the engine emits no hint. Default: 25.
	ConsolidateEvery int `yaml:"consolidate_every"`

	// WindowMessages, when > 0, keeps only the last N non-system messages on
	// the outbound request. Windowing runs BEFORE recall/inject so recall
	// dedupe compares against what the model will actually see; the kept
	// window extends left past orphaned tool turns. Gemini-family formats
	// pass through unwindowed (logged once per process).
	WindowMessages int `yaml:"window_messages"`

	// SessionHeader is the request header used as the session isolation key.
	// Default: X-Cortext-Session
	SessionHeader string `yaml:"session_header"`

	// AgentHeader optionally overrides agent-scope identity.
	// Default: X-Cortext-Agent
	AgentHeader string `yaml:"agent_header"`
}

// Defaults match cortext-openclaw-plugin DEFAULTS where applicable.
func DefaultConfig() PluginConfig {
	return PluginConfig{
		Enabled:         true,
		DataDir:         "~/.cli-proxy-api/cortext",
		MemoryScope:     ScopeSession,
		Focus:           0.45,
		Sensitivity:     0.5,
		Stability:       0.5,
		RecallLimit:     12,
		IngestAssistant: true,
		// CoT is toxic by default: reasoning is not persisted unless the
		// operator opts in.
		IngestReasoning:  false,
		InterruptGate:    true,
		AutoConsolidate:  true,
		ConsolidateEvery: 25,
		WindowMessages:   0,
		SessionHeader:    "X-Cortext-Session",
		AgentHeader:      "X-Cortext-Agent",
	}
}

// LoadConfigYAML merges plugin config YAML (plugins.configs.cortext body or a
// standalone file) onto defaults.
func LoadConfigYAML(raw []byte) (PluginConfig, error) {
	cfg := DefaultConfig()
	if len(raw) == 0 {
		return normalizeConfig(cfg), nil
	}
	// Accept either a bare object or nested under "cortext:".
	var top map[string]any
	if err := yaml.Unmarshal(raw, &top); err != nil {
		return cfg, fmt.Errorf("cortext config: %w", err)
	}
	if nested, ok := top["cortext"]; ok {
		b, err := yaml.Marshal(nested)
		if err != nil {
			return cfg, err
		}
		raw = b
	}
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return cfg, fmt.Errorf("cortext config: %w", err)
	}
	return normalizeConfig(cfg), nil
}

func normalizeConfig(cfg PluginConfig) PluginConfig {
	switch MemoryScope(strings.ToLower(string(cfg.MemoryScope))) {
	case ScopeAgent, ScopeGlobal, ScopeSession:
		cfg.MemoryScope = MemoryScope(strings.ToLower(string(cfg.MemoryScope)))
	default:
		cfg.MemoryScope = ScopeSession
	}
	if cfg.RecallLimit < 0 {
		// 0 disables recall/injection; absence keeps the default (YAML merge).
		cfg.RecallLimit = 0
	}
	if cfg.Focus <= 0 {
		cfg.Focus = 0.45
	}
	if cfg.Sensitivity <= 0 {
		cfg.Sensitivity = 0.5
	}
	if cfg.Stability <= 0 {
		cfg.Stability = 0.5
	}
	if strings.TrimSpace(cfg.DataDir) == "" {
		cfg.DataDir = "~/.cli-proxy-api/cortext"
	}
	if strings.TrimSpace(cfg.SessionHeader) == "" {
		cfg.SessionHeader = "X-Cortext-Session"
	}
	if strings.TrimSpace(cfg.AgentHeader) == "" {
		cfg.AgentHeader = "X-Cortext-Agent"
	}
	if cfg.WindowMessages < 0 {
		cfg.WindowMessages = 0
	}
	if cfg.ConsolidateEvery <= 0 {
		cfg.ConsolidateEvery = 25
	}
	cfg.DataDir = expandPath(cfg.DataDir)
	return cfg
}

func expandPath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return p
	}
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err == nil {
			if p == "~" {
				p = home
			} else {
				p = filepath.Join(home, p[2:])
			}
		}
	}
	return os.ExpandEnv(p)
}
