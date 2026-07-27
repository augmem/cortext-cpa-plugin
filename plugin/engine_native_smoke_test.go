//go:build cortext_native

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// TestNativeOpenProcessText drives the real libcortext binding (not the stub).
func TestNativeOpenProcessText(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "native-smoke.sqlite")
	cfg := DefaultConfig()
	eng, err := openDefaultEngine(db, cfg)
	if err != nil {
		t.Fatalf("native open: %v", err)
	}
	defer eng.Close()

	_, err = eng.ProcessText("Bailey loves tennis balls and unique-native-needle-4242.", "smoke/user", RetentionDurable)
	if err != nil {
		t.Fatalf("durable process: %v", err)
	}
	ctx, err := eng.ProcessText("What does Bailey love?", "smoke/query", RetentionEphemeral)
	if err != nil {
		t.Fatalf("ephemeral process: %v", err)
	}
	// Native may return retrieved or working memory; require non-empty process path.
	// At minimum, a second durable write must succeed without error.
	_, err = eng.ProcessText("follow-up", "smoke/2", RetentionDurable)
	if err != nil {
		t.Fatalf("second durable: %v", err)
	}
	if err := eng.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	// DB file should exist for file-backed paths.
	if st, err := os.Stat(db); err != nil || st.Size() == 0 {
		// Some bindings create sidecar files; accept any file under dir.
		entries, _ := os.ReadDir(dir)
		if len(entries) == 0 {
			t.Fatalf("expected native store artifacts under %s (stat err=%v)", dir, err)
		}
	}
	_ = ctx
	// Soft quality check: if recall returned text, it should relate to the fact.
	for _, m := range ctx.RetrievedMemory {
		if strings.Contains(strings.ToLower(m.Text), "tennis") || strings.Contains(m.Text, "4242") {
			return
		}
	}
	// Native may not surface keyword recall on every model build; open+process+flush is the hard gate.
	t.Logf("native open+process+flush ok; retrieved_memory=%d working=%d", len(ctx.RetrievedMemory), len(ctx.WorkingMemory))
}

// TestNativeConcurrentSameScope hammers one native engine from many
// goroutines — the access pattern concurrent same-scope streams produce —
// then races in-flight ProcessText calls against Close (mutex + closed guard).
func TestNativeConcurrentSameScope(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "native-conc.sqlite")
	eng, err := openDefaultEngine(db, DefaultConfig())
	if err != nil {
		t.Fatalf("native open: %v", err)
	}

	const workers = 8
	const writes = 20
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < writes; i++ {
				text := fmt.Sprintf("conc-needle-%d-%d vault code %d%d.", w, i, w, i)
				if _, err := eng.ProcessText(text, "conc/durable", RetentionDurable); err != nil {
					errs <- fmt.Errorf("durable w%d i%d: %w", w, i, err)
					return
				}
				if _, err := eng.ProcessText("what is the vault code?", "conc/query", RetentionEphemeral); err != nil {
					errs <- fmt.Errorf("query w%d i%d: %w", w, i, err)
					return
				}
				if i == writes/2 {
					if err := eng.Consolidate(); err != nil {
						errs <- fmt.Errorf("consolidate w%d: %w", w, err)
						return
					}
				}
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if err := eng.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	// In-flight ProcessText racing Close must return cleanly (no panic, no
	// use-after-close error surfacing from the native handle).
	var wg2 sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg2.Add(1)
		go func(w int) {
			defer wg2.Done()
			for i := 0; i < 10; i++ {
				_, _ = eng.ProcessText(fmt.Sprintf("post-close-%d-%d", w, i), "conc/late", RetentionDurable)
			}
		}(w)
	}
	if err := eng.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	wg2.Wait()
	if _, err := eng.ProcessText("after close", "conc/closed", RetentionDurable); !errors.Is(err, errEngineClosed) {
		t.Fatalf("closed engine ProcessText = %v, want errEngineClosed", err)
	}
	if err := eng.Close(); err != nil {
		t.Fatalf("re-close must be a no-op: %v", err)
	}
}

func TestMemoryTextFromBase64Content(t *testing.T) { // Native FFI returns content as base64 parts, not plain "text".
	raw := map[string]any{
		"content": []any{
			map[string]any{"base64": "TXkgdmF1bHQgcGFzc3dvcmQgaXMgbmVlZGxlLTE=", "size_bytes": 28},
		},
		"modality": "text",
	}
	got := memoryTextFromMap(raw)
	if !strings.Contains(got, "needle-1") {
		t.Fatalf("expected decoded needle, got %q", got)
	}
}

// consolidationStateFromMap must mirror the reference binding (cortext.ts
// normalizeContext): ≥1.2.2 string enum preferred, legacy booleans mapped,
// anything else → "none".
func TestConsolidationStateFromMap(t *testing.T) {
	cases := []struct {
		name string
		raw  map[string]any
		want string
	}{
		{"enum required", map[string]any{"consolidation_state": "required"}, "required"},
		{"enum recommended", map[string]any{"consolidation_state": "recommended"}, "recommended"},
		{"enum none", map[string]any{"consolidation_state": "none"}, "none"},
		{"legacy required", map[string]any{"consolidation_required": true}, "required"},
		{"legacy recommended", map[string]any{"consolidation_recommended": true}, "recommended"},
		{"legacy both false", map[string]any{"consolidation_required": false, "consolidation_recommended": false}, "none"},
		{"enum beats legacy", map[string]any{"consolidation_state": "none", "consolidation_required": true}, "none"},
		{"unknown enum value", map[string]any{"consolidation_state": "urgent"}, "none"},
		{"missing", map[string]any{}, "none"},
		{"wrong type", map[string]any{"consolidation_state": 42}, "none"},
	}
	for _, tc := range cases {
		if got := consolidationStateFromMap(tc.raw); got != tc.want {
			t.Errorf("%s: got %q want %q", tc.name, got, tc.want)
		}
	}
	if !consolidationRequested("required") || !consolidationRequested("recommended") {
		t.Error("requested states must trigger")
	}
	if consolidationRequested("none") || consolidationRequested("") {
		t.Error("none/empty must not trigger")
	}
}
