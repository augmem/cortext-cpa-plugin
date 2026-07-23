package main

import "testing"

func TestLoadConfigYAMLDefaults(t *testing.T) {
	cfg, err := LoadConfigYAML(nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MemoryScope != ScopeSession {
		t.Fatalf("scope = %q", cfg.MemoryScope)
	}
	if cfg.Focus != 0.45 {
		t.Fatalf("focus = %v", cfg.Focus)
	}
	if cfg.SessionHeader != "X-Cortext-Session" {
		t.Fatalf("session header = %q", cfg.SessionHeader)
	}
}

func TestLoadConfigYAMLOverrides(t *testing.T) {
	raw := []byte(`
enabled: true
memory_scope: agent
focus: 0.6
recall_limit: 5
window_messages: 8
data_dir: /tmp/cortext-test
`)
	cfg, err := LoadConfigYAML(raw)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MemoryScope != ScopeAgent {
		t.Fatalf("scope = %q", cfg.MemoryScope)
	}
	if cfg.Focus != 0.6 {
		t.Fatalf("focus = %v", cfg.Focus)
	}
	if cfg.RecallLimit != 5 {
		t.Fatalf("recall_limit = %d", cfg.RecallLimit)
	}
	if cfg.WindowMessages != 8 {
		t.Fatalf("window = %d", cfg.WindowMessages)
	}
	if cfg.DataDir != "/tmp/cortext-test" {
		t.Fatalf("data_dir = %q", cfg.DataDir)
	}
}

func TestLoadConfigYAMLNestedCortext(t *testing.T) {
	raw := []byte(`cortext:
  memory_scope: global
  enabled: false
`)
	cfg, err := LoadConfigYAML(raw)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MemoryScope != ScopeGlobal {
		t.Fatalf("scope = %q", cfg.MemoryScope)
	}
	if cfg.Enabled {
		t.Fatal("expected enabled=false")
	}
}
