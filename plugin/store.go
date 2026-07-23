package main

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"os"
	"path/filepath"
	"sync"

	"github.com/tidwall/gjson"
)

const maxEngines = 64

// ScopeIDs carries isolation inputs for a single request.
type ScopeIDs struct {
	Session string
	Agent   string
	APIKey  string
}

// Store manages one Engine (one SQLite file) per isolation scope.
type Store struct {
	cfg     PluginConfig
	mu      sync.Mutex
	engines map[string]Engine // insertion order via companion slice
	order   []string
	// seen maps scopeKey → last durable text hashes (dedupe resubmitted history).
	seen map[string]map[string]struct{}
	// bus stages interrupt-gate recall for the next assemble on the same scope.
	bus *InterruptBus
}

func NewStore(cfg PluginConfig) *Store {
	return &Store{
		cfg:     cfg,
		engines: make(map[string]Engine),
		seen:    make(map[string]map[string]struct{}),
		bus:     NewInterruptBus(),
	}
}

func (s *Store) Reconfigure(cfg PluginConfig) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cfg = cfg
}

func (s *Store) Config() PluginConfig {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg
}

func (s *Store) Bus() *InterruptBus { return s.bus }

// ScopeKey is the only isolation boundary. Distinct keys ⇒ distinct DBs.
func (s *Store) ScopeKey(ids ScopeIDs) string {
	cfg := s.Config()
	switch cfg.MemoryScope {
	case ScopeGlobal:
		return "global"
	case ScopeAgent:
		agent := ids.Agent
		if agent == "" {
			agent = ids.APIKey
		}
		if agent == "" {
			agent = "agent"
		}
		return "a-" + safeKey(agent)
	default: // session
		sess := ids.Session
		if sess == "" {
			// Fall back to a stable agent bucket rather than one global soup.
			if ids.Agent != "" {
				return "s-" + safeKey(ids.Agent)
			}
			if ids.APIKey != "" {
				return "s-" + safeKey(shortHash(ids.APIKey))
			}
			return "s-default"
		}
		return "s-" + safeKey(sess)
	}
}

func (s *Store) ForScope(key string) (Engine, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if eng, ok := s.engines[key]; ok {
		// bump LRU
		for i, k := range s.order {
			if k == key {
				s.order = append(s.order[:i], s.order[i+1:]...)
				break
			}
		}
		s.order = append(s.order, key)
		return eng, nil
	}
	if err := os.MkdirAll(s.cfg.DataDir, 0o755); err != nil {
		return nil, err
	}
	dbPath := filepath.Join(s.cfg.DataDir, key+".sqlite")
	eng, err := openEngine(dbPath, s.cfg)
	if err != nil {
		return nil, err
	}
	s.engines[key] = eng
	s.order = append(s.order, key)
	for len(s.order) > maxEngines {
		oldest := s.order[0]
		s.order = s.order[1:]
		if e := s.engines[oldest]; e != nil {
			_ = e.Flush()
			_ = e.Close()
		}
		delete(s.engines, oldest)
		delete(s.seen, oldest)
	}
	return eng, nil
}

// MarkSeen / HasSeen implement durable-ingest dedupe for resubmitted transcripts.
func (s *Store) HasSeen(scope, hash string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.seen[scope]
	if m == nil {
		return false
	}
	_, ok := m[hash]
	return ok
}

func (s *Store) MarkSeen(scope, hash string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.seen[scope]
	if m == nil {
		m = make(map[string]struct{})
		s.seen[scope] = m
	}
	m[hash] = struct{}{}
	// Bound growth per scope.
	if len(m) > 4096 {
		// Drop arbitrarily; next request may re-ingest a few old lines.
		s.seen[scope] = make(map[string]struct{})
	}
}

func (s *Store) DisposeAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.engines {
		_ = e.Flush()
		_ = e.Close()
	}
	s.engines = make(map[string]Engine)
	s.order = nil
	s.seen = make(map[string]map[string]struct{})
}

// ResolveScopeIDs pulls session/agent identity from headers, body, and metadata.
func ResolveScopeIDs(cfg PluginConfig, headers http.Header, body []byte, metadata map[string]any) ScopeIDs {
	ids := ScopeIDs{}
	if headers != nil {
		ids.Session = firstNonEmpty(
			headers.Get(cfg.SessionHeader),
			headers.Get("X-Session-Id"),
			headers.Get("X-Conversation-Id"),
		)
		ids.Agent = firstNonEmpty(
			headers.Get(cfg.AgentHeader),
			headers.Get("X-Agent-Id"),
		)
		// Authorization bearer is a last-resort agent bucket (hashed later).
		if auth := headers.Get("Authorization"); auth != "" {
			ids.APIKey = auth
		}
		if ids.APIKey == "" {
			ids.APIKey = headers.Get("x-api-key")
		}
		if ids.APIKey == "" {
			ids.APIKey = headers.Get("x-goog-api-key")
		}
	}
	if ids.Session == "" && len(body) > 0 {
		ids.Session = firstNonEmpty(
			gjson.GetBytes(body, "conversation_id").String(),
			gjson.GetBytes(body, "session_id").String(),
			gjson.GetBytes(body, "metadata.conversation_id").String(),
			gjson.GetBytes(body, "metadata.session_id").String(),
			gjson.GetBytes(body, "previous_response_id").String(),
		)
	}
	if metadata != nil {
		if ids.Session == "" {
			if v, ok := metadata["session_id"].(string); ok {
				ids.Session = v
			}
		}
		if ids.Agent == "" {
			if v, ok := metadata["agent_id"].(string); ok {
				ids.Agent = v
			}
		}
	}
	return ids
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if t := trim(v); t != "" {
			return t
		}
	}
	return ""
}

func trim(s string) string {
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\t') {
		s = s[1:]
	}
	for len(s) > 0 && (s[len(s)-1] == ' ' || s[len(s)-1] == '\t') {
		s = s[:len(s)-1]
	}
	return s
}

func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:8])
}
