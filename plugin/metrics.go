package main

import (
	"fmt"
	"log"
	"sync/atomic"
)

// Process-local counters for fail-open paths. Proxy availability requires
// silent pass-through on many errors; these counters make silent memory loss
// observable without changing intercept response contracts.
var metrics struct {
	OpenFail        atomic.Uint64
	ProcessFail     atomic.Uint64
	InjectFail      atomic.Uint64
	ScopeEvict      atomic.Uint64
	StreamEvict     atomic.Uint64
	StreamConflict  atomic.Uint64
	ChainFallback   atomic.Uint64
	SkipNoIdentity  atomic.Uint64
	ConsolidateFail atomic.Uint64
}

// MetricsSnapshot is a point-in-time copy for tests and diagnostics.
type MetricsSnapshot struct {
	OpenFail        uint64 `json:"open_fail"`
	ProcessFail     uint64 `json:"process_fail"`
	InjectFail      uint64 `json:"inject_fail"`
	ScopeEvict      uint64 `json:"scope_evict"`
	StreamEvict     uint64 `json:"stream_evict"`
	StreamConflict  uint64 `json:"stream_conflict"`
	ChainFallback   uint64 `json:"chain_fallback"`
	SkipNoIdentity  uint64 `json:"skip_no_identity"`
	ConsolidateFail uint64 `json:"consolidate_fail"`
}

// SnapshotMetrics returns current counter values.
func SnapshotMetrics() MetricsSnapshot {
	return MetricsSnapshot{
		OpenFail:        metrics.OpenFail.Load(),
		ProcessFail:     metrics.ProcessFail.Load(),
		InjectFail:      metrics.InjectFail.Load(),
		ScopeEvict:      metrics.ScopeEvict.Load(),
		StreamEvict:     metrics.StreamEvict.Load(),
		StreamConflict:  metrics.StreamConflict.Load(),
		ChainFallback:   metrics.ChainFallback.Load(),
		SkipNoIdentity:  metrics.SkipNoIdentity.Load(),
		ConsolidateFail: metrics.ConsolidateFail.Load(),
	}
}

// resetMetricsForTest zeros counters (tests only).
func resetMetricsForTest() {
	metrics.OpenFail.Store(0)
	metrics.ProcessFail.Store(0)
	metrics.InjectFail.Store(0)
	metrics.ScopeEvict.Store(0)
	metrics.StreamEvict.Store(0)
	metrics.StreamConflict.Store(0)
	metrics.ChainFallback.Store(0)
	metrics.SkipNoIdentity.Store(0)
	metrics.ConsolidateFail.Store(0)
}

// logFail records a failure counter and emits a high-signal log line that
// includes the running total so operators can alert on growth.
func logFail(counter *atomic.Uint64, kind, detail string) {
	n := counter.Add(1)
	log.Printf("cortext: %s (#%d) %s", kind, n, detail)
}

// metricsSummaryLine is attached to register/reconfigure logs.
func metricsSummaryLine() string {
	s := SnapshotMetrics()
	return fmt.Sprintf("metrics open_fail=%d process_fail=%d inject_fail=%d skip_no_identity=%d scope_evict=%d stream_evict=%d stream_conflict=%d chain_fallback=%d consolidate_fail=%d",
		s.OpenFail, s.ProcessFail, s.InjectFail, s.SkipNoIdentity, s.ScopeEvict, s.StreamEvict, s.StreamConflict, s.ChainFallback, s.ConsolidateFail)
}
