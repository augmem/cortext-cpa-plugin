//go:build !cortext_native

package main

import (
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// waitConsolidations polls until the stub reaches want consolidations.
// Consolidation runs single-flight in the background (hot path must not block
// on it), so exact-count assertions wait instead of racing the scheduler.
func waitConsolidations(t *testing.T, e *stubEngine, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if consolidations(e) >= want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("consolidations = %d, want %d", consolidations(e), want)
}

// settleConsolidations gives a (potentially erroneous) background consolidate
// time to fire before a negative assertion.
func settleConsolidations() {
	time.Sleep(100 * time.Millisecond)
}

func stubFor(t *testing.T, svc *Service, scope string) *stubEngine {
	t.Helper()
	eng, err := svc.store.ForScope(scope)
	if err != nil {
		t.Fatal(err)
	}
	stub, ok := eng.(*stubEngine)
	if !ok {
		t.Fatalf("expected stub engine, got %T", eng)
	}
	return stub
}

// scopeFromHeaders resolves the production ScopeKey path for a test header set.
func scopeFromHeaders(t *testing.T, svc *Service, h http.Header) string {
	t.Helper()
	ids := ResolveScopeIDs(svc.store.Config(), h, nil, nil)
	scope := svc.store.ScopeKey(ids)
	if scope == "" {
		t.Fatal("expected durable scope for test headers")
	}
	return scope
}

func consolidations(e *stubEngine) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.consolidations
}

// Consolidation must fire on the every-N-durable-writes cadence, not on
// every request (OpenClaw's measured finding: per-request consolidation is
// hot-path cost with no retrieval gain).
func TestConsolidateCadence(t *testing.T) {
	dir := t.TempDir()
	cfg := DefaultConfig()
	cfg.DataDir = dir
	cfg.InterruptGate = false
	cfg.ConsolidateEvery = 2
	svc := NewService(cfg)
	h := http.Header{}
	h.Set("X-Cortext-Session", "cons-1")

	ingest := func(text string) {
		svc.HandleRequestBeforeAuth(pluginapi.RequestInterceptRequest{
			SourceFormat: fmtOpenAI,
			Headers:      h,
			Body:         []byte(`{"messages":[{"role":"user","content":"` + text + `"}]}`),
		})
	}

	ingest("fact one about quokkas.")
	stub := stubFor(t, svc, scopeFromHeaders(t, svc, h))
	if got := consolidations(stub); got != 0 {
		t.Fatalf("consolidated before cadence: %d", got)
	}
	ingest("fact two about wombats.") // 2nd durable write → consolidate
	waitConsolidations(t, stub, 1)
	ingest("fact three about narwhals.")
	ingest("fact four about lemurs.") // 4th durable write → second consolidate
	waitConsolidations(t, stub, 2)

	// Shutdown consolidates once more (compact-time analog). The stub instance
	// is closed by DisposeAll but its counter survives on the object.
	svc.Shutdown()
	if got := consolidations(stub); got != 3 {
		t.Fatalf("expected shutdown consolidation, got %d", got)
	}
}

// auto_consolidate=false must fully disable the cadence and shutdown run.
func TestConsolidateDisabled(t *testing.T) {
	dir := t.TempDir()
	cfg := DefaultConfig()
	cfg.DataDir = dir
	cfg.InterruptGate = false
	cfg.AutoConsolidate = false
	cfg.ConsolidateEvery = 1
	svc := NewService(cfg)
	h := http.Header{}
	h.Set("X-Cortext-Session", "cons-off")

	svc.HandleRequestBeforeAuth(pluginapi.RequestInterceptRequest{
		SourceFormat: fmtOpenAI,
		Headers:      h,
		Body:         []byte(`{"messages":[{"role":"user","content":"anything."}]}`),
	})
	stub := stubFor(t, svc, scopeFromHeaders(t, svc, h))
	svc.Shutdown()
	if got := consolidations(stub); got != 0 {
		t.Fatalf("consolidated despite auto_consolidate=false: %d", got)
	}
}

// Exactly one due per cadence window under concurrency: N goroutines x M
// same-scope durable ingests with every=K must yield floor(N*M/K)
// consolidations — no double-due, no lost due.
func TestConsolidateCadenceConcurrent(t *testing.T) {
	dir := t.TempDir()
	cfg := DefaultConfig()
	cfg.DataDir = dir
	cfg.InterruptGate = false
	cfg.ConsolidateEvery = 10
	svc := NewService(cfg)
	h := http.Header{}
	h.Set("X-Cortext-Session", "cons-conc")

	const goroutines, per = 8, 25 // 200 durable writes → 20 consolidations
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < per; i++ {
				svc.HandleRequestBeforeAuth(pluginapi.RequestInterceptRequest{
					SourceFormat: fmtOpenAI,
					Headers:      h,
					Body: []byte(`{"messages":[{"role":"user","content":"concurrent fact ` +
						itoa(g) + `-` + itoa(i) + `."}]}`),
				})
			}
		}(g)
	}
	wg.Wait()

	stub := stubFor(t, svc, scopeFromHeaders(t, svc, h))
	waitConsolidations(t, stub, goroutines*per/cfg.ConsolidateEvery)
}

// The engine's consolidation_state hint triggers consolidation immediately
// and resets the cadence counter; cadence remains the fallback when no hint
// is emitted. every=3 makes a missing reset observable: without it the
// cadence would fire at write 4 instead of write 5.
func TestConsolidateHintDriven(t *testing.T) {
	dir := t.TempDir()
	cfg := DefaultConfig()
	cfg.DataDir = dir
	cfg.InterruptGate = false
	cfg.ConsolidateEvery = 3
	svc := NewService(cfg)
	h := http.Header{}
	h.Set("X-Cortext-Session", "cons-hint")

	ingest := func(text string) {
		svc.HandleRequestBeforeAuth(pluginapi.RequestInterceptRequest{
			SourceFormat: fmtOpenAI,
			Headers:      h,
			Body:         []byte(`{"messages":[{"role":"user","content":"` + text + `"}]}`),
		})
	}

	ingest("seed one.")
	stub := stubFor(t, svc, scopeFromHeaders(t, svc, h))

	// Engine asks for consolidation on the next durable write → fires now,
	// ahead of the 3-write cadence.
	stub.mu.Lock()
	stub.consolidationState = "required"
	stub.mu.Unlock()
	ingest("seed two.")
	waitConsolidations(t, stub, 1)

	// Hint cleared; the cadence counter was reset by the hint fire, so
	// writes 3 and 4 must NOT consolidate (counter at 1 and 2 of 3)…
	stub.mu.Lock()
	stub.consolidationState = ""
	stub.mu.Unlock()
	ingest("seed three.")
	ingest("seed four.")
	settleConsolidations()
	if got := consolidations(stub); got != 1 {
		t.Fatalf("cadence counter not reset by hint (early fire): %d", got)
	}
	// …and the cadence fires exactly at write 5 (3 writes after the reset).
	ingest("seed five.")
	waitConsolidations(t, stub, 2)

	// "recommended" also counts as an explicit engine request.
	stub.mu.Lock()
	stub.consolidationState = "recommended"
	stub.mu.Unlock()
	ingest("seed six.")
	waitConsolidations(t, stub, 3)
}

// A persistent hint (e.g. Consolidate keeps failing and never clears the
// engine's backlog) must not consolidate on every durable write: attempts
// are throttled to one per consolidate_every/5 writes.
func TestConsolidateHintThrottled(t *testing.T) {
	dir := t.TempDir()
	cfg := DefaultConfig()
	cfg.DataDir = dir
	cfg.InterruptGate = false
	cfg.ConsolidateEvery = 25 // gap = 5
	svc := NewService(cfg)
	h := http.Header{}
	h.Set("X-Cortext-Session", "cons-storm")

	stub := stubFor(t, svc, scopeFromHeaders(t, svc, h))
	stub.mu.Lock()
	stub.consolidationState = "required" // persistent hint
	stub.mu.Unlock()

	for i := 0; i < 12; i++ {
		svc.HandleRequestBeforeAuth(pluginapi.RequestInterceptRequest{
			SourceFormat: fmtOpenAI,
			Headers:      h,
			Body:         []byte(`{"messages":[{"role":"user","content":"storm ` + itoa(i) + `."}]}`),
		})
	}
	// Attempts allowed at writes 1, 6, 11 → 3, not 12.
	waitConsolidations(t, stub, 3)
	settleConsolidations()
	if got := consolidations(stub); got != 3 {
		t.Fatalf("persistent hint not throttled: %d consolidations in 12 writes", got)
	}
}

// auto_consolidate=false must suppress hint-driven consolidation too.
func TestConsolidateHintSuppressedWhenDisabled(t *testing.T) {
	dir := t.TempDir()
	cfg := DefaultConfig()
	cfg.DataDir = dir
	cfg.InterruptGate = false
	cfg.AutoConsolidate = false
	svc := NewService(cfg)
	h := http.Header{}
	h.Set("X-Cortext-Session", "cons-hint-off")

	svc.HandleRequestBeforeAuth(pluginapi.RequestInterceptRequest{
		SourceFormat: fmtOpenAI,
		Headers:      h,
		Body:         []byte(`{"messages":[{"role":"user","content":"seed."}]}`),
	})
	stub := stubFor(t, svc, scopeFromHeaders(t, svc, h))
	stub.mu.Lock()
	stub.consolidationState = "required"
	stub.mu.Unlock()
	svc.HandleRequestBeforeAuth(pluginapi.RequestInterceptRequest{
		SourceFormat: fmtOpenAI,
		Headers:      h,
		Body:         []byte(`{"messages":[{"role":"user","content":"another."}]}`),
	})
	if got := consolidations(stub); got != 0 {
		t.Fatalf("hint fired despite auto_consolidate=false: %d", got)
	}
}

// gateEngine blocks inside Consolidate until released and tracks concurrent
// entries, so tests can reproduce in-flight consolidate races deterministically.
type gateEngine struct {
	*stubEngine
	started  chan struct{}
	release  chan struct{}
	inFlight atomic.Int32
	maxSeen  atomic.Int32
}

func (e *gateEngine) Consolidate() error {
	n := e.inFlight.Add(1)
	for {
		m := e.maxSeen.Load()
		if n <= m || e.maxSeen.CompareAndSwap(m, n) {
			break
		}
	}
	e.started <- struct{}{}
	<-e.release
	e.inFlight.Add(-1)
	return e.stubEngine.Consolidate()
}

// A stale background consolidate loop (its scope's state dropped mid-run by
// eviction/reconfigure) must exit without touching the replacement state's
// flags — clearing running spawned a second concurrent consolidate goroutine.
func TestConsolidateLoopStaleStateDoesNotDoubleRun(t *testing.T) {
	cfg := DefaultConfig()
	cfg.AutoConsolidate = false
	st := NewStore(cfg)
	defer st.DisposeAll()

	eng1 := &gateEngine{stubEngine: &stubEngine{}, started: make(chan struct{}, 4), release: make(chan struct{})}
	eng2 := &gateEngine{stubEngine: &stubEngine{}, started: make(chan struct{}, 4), release: make(chan struct{})}

	// loop1 runs against the pre-eviction engine and blocks mid-consolidate.
	st.scheduleConsolidate("scope", eng1)
	<-eng1.started

	// Eviction drops the scope's consolidate state; the reopened scope
	// schedules a fresh single-flight run against the new engine.
	st.mu.Lock()
	st.drainEnginesLocked()
	st.mu.Unlock()
	st.scheduleConsolidate("scope", eng2)
	<-eng2.started

	// A further due during loop2 coalesces into pending, not a third run.
	st.scheduleConsolidate("scope", eng2)

	// loop1 wakes: it must exit without touching the replacement state.
	close(eng1.release)
	time.Sleep(50 * time.Millisecond) // let loop1's exit path run
	st.mu.Lock()
	cur := st.consol["scope"]
	st.mu.Unlock()
	if cur == nil || !cur.running {
		t.Fatal("stale loop cleared the replacement state's running flag")
	}
	if got := consolidations(eng1.stubEngine); got != 1 {
		t.Fatalf("stale loop re-ran consolidate on the old engine: %d", got)
	}

	// loop2 finishes, runs the coalesced pending follow-up, then stops.
	close(eng2.release)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		st.mu.Lock()
		running := st.consol["scope"] != nil && st.consol["scope"].running
		st.mu.Unlock()
		if !running {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	st.mu.Lock()
	stillRunning := st.consol["scope"].running
	st.mu.Unlock()
	if stillRunning {
		t.Fatal("replacement loop never released the single-flight flag")
	}
	if got := consolidations(eng2.stubEngine); got != 2 {
		t.Fatalf("replacement consolidations = %d, want 2 (run + coalesced pending)", got)
	}
	if got := eng2.maxSeen.Load(); got != 1 {
		t.Fatalf("concurrent consolidations on one scope: max %d", got)
	}
}
