package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/tidwall/gjson"
)

const maxEngines = 64

// dataDirPerm is private to the process owner: session stores hold conversation
// memory and must not be world-traversable on multi-user hosts.
const dataDirPerm = 0o700

// errStoreClosed is returned by ForScope after DisposeAll: a late intercept
// holding a pre-shutdown Service must not reopen engines into drained maps.
var errStoreClosed = errors.New("cortext: store closed")

// errConfigChanged marks a material reconfigure racing an in-flight open —
// normal self-healing behavior, kept out of the open_fail alert counter.
var errConfigChanged = errors.New("cortext: config changed during engine open; retry")

// ScopeIDs carries isolation inputs for a single request.
type ScopeIDs struct {
	Session string
	Agent   string
	APIKey  string
	// PrevResponseID is the OpenAI Responses-API chain pointer. It is NOT a
	// session key on its own (it changes every turn); it is resolved through
	// the response-id→scope map recorded when earlier responses were seen.
	PrevResponseID string
}

// Store manages one Engine (one SQLite file) per isolation scope.
type Store struct {
	cfg     PluginConfig
	mu      sync.Mutex
	openMu  sync.Mutex        // serializes engine opens (miss path + prewarm); never nested under mu
	closed  atomic.Bool       // set by DisposeAll: late intercepts must not reopen engines
	closeWg sync.WaitGroup    // detached closeEngines runs; DisposeAll joins them
	engines map[string]Engine // insertion order via companion slice
	order   []string
	// seen maps scopeKey → last durable text hashes (dedupe resubmitted history);
	// seenOrder keeps insertion order so the bound evicts oldest-first instead
	// of resetting the whole set (a full reset re-ingested thousands of old
	// lines on the next request).
	seen      map[string]map[string]struct{}
	seenOrder map[string][]string
	// writes counts durable ingests per scope for the consolidate cadence.
	writes map[string]int
	// writesTotal counts all durable writes per scope (monotonic) and
	// lastConsolidate marks the writesTotal value at the last consolidate
	// attempt — together they throttle consolidate attempts (failure/storm
	// backoff for a persistent consolidation hint).
	writesTotal     map[string]int
	lastConsolidate map[string]int
	// consol tracks background consolidate single-flight per scope.
	consol map[string]*consolState
	// closings holds a done-channel per scope whose engine close was detached
	// (reconfigure / LRU eviction). ForScope waits for the pending close
	// before reopening the same SQLite file, so a slow close cannot clobber
	// or race fresh writes.
	closings map[string]chan struct{}
	// respScope maps upstream response id → scope key so Responses-API clients
	// chaining via previous_response_id stay in one continuous store.
	respScope map[string]respScopeEntry
	respOrder []string // FIFO eviction order for respScope
	// bus stages interrupt-gate recall for the next assemble on the same scope.
	bus *InterruptBus
}

func NewStore(cfg PluginConfig) *Store {
	return &Store{
		cfg:             cfg,
		engines:         make(map[string]Engine),
		seen:            make(map[string]map[string]struct{}),
		seenOrder:       make(map[string][]string),
		writes:          make(map[string]int),
		writesTotal:     make(map[string]int),
		lastConsolidate: make(map[string]int),
		consol:          make(map[string]*consolState),
		closings:        make(map[string]chan struct{}),
		respScope:       make(map[string]respScopeEntry),
		bus:             NewInterruptBus(),
	}
}

// materialConfigChange reports whether engine settings changed enough that
// open engines must be disposed and reopened.
func materialConfigChange(old, cfg PluginConfig) bool {
	return old.DataDir != cfg.DataDir ||
		old.MemoryScope != cfg.MemoryScope ||
		old.Focus != cfg.Focus ||
		old.Sensitivity != cfg.Sensitivity ||
		old.Stability != cfg.Stability ||
		old.SessionHeader != cfg.SessionHeader ||
		old.AgentHeader != cfg.AgentHeader
}

// Reconfigure applies new plugin config. Material changes (data_dir, scope mode,
// engine knobs) dispose open engines so the next request reopens with the new
// settings. The in-memory previous_response_id map is process-local and is
// cleared on material reconfigure and on full DisposeAll / process restart —
// clients must re-establish chains or send an explicit session header after
// restart (documented operator contract).
func (s *Store) Reconfigure(cfg PluginConfig) {
	s.mu.Lock()
	material := materialConfigChange(s.cfg, cfg)
	s.cfg = cfg
	var doomed []doomedEngine
	if material {
		doomed = s.drainEnginesLocked()
		// Scope identity / paths may have changed; drop chain map too.
		s.respScope = make(map[string]respScopeEntry)
		s.respOrder = nil
	}
	// Add under s.mu so it is ordered before DisposeAll's closeWg.Wait().
	detach := len(doomed) > 0
	if detach {
		s.closeWg.Add(1)
	}
	s.mu.Unlock()
	// Close drained engines detached: consolidate/flush/close on up to
	// maxEngines native handles can take seconds and must not block the
	// configure RPC (and through it, every interceptor on serviceMu). The
	// engines are already out of the maps and carry their own close guards.
	if detach {
		go func() {
			defer s.closeWg.Done()
			s.closeEngines(cfg, doomed)
		}()
	}
}

func (s *Store) Config() PluginConfig {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg
}

func (s *Store) Bus() *InterruptBus { return s.bus }

// tenantHash returns a short stable tenant id from the API key, or "" when the
// request is unkeyed. Session and agent scopes always include this when present
// so two tenants cannot share a store by colliding on conversation_id / headers.
func tenantHash(ids ScopeIDs) string {
	if ids.APIKey == "" {
		return ""
	}
	return shortHash(ids.APIKey)
}

// ScopeKey is the only isolation boundary. Distinct keys ⇒ distinct DBs.
// An empty return means "no durable store": identity-less traffic under
// session/agent scope must not open a shared soup (s-default). Global scope
// is an explicit single shared store and is documented as multi-tenant-unsafe.
func (s *Store) ScopeKey(ids ScopeIDs) string {
	cfg := s.Config()
	switch cfg.MemoryScope {
	case ScopeGlobal:
		return "global"
	case ScopeAgent:
		th := tenantHash(ids)
		agent := ids.Agent
		if agent == "" {
			if th != "" {
				// No agent header: one store per API key.
				return "a-" + th
			}
			// No agent, no key — refuse durable store.
			return ""
		}
		// Hash agent id so sanitization cannot collide distinct values.
		ah := shortHash(agent)
		if th != "" {
			return "a-" + th + "-" + ah
		}
		// Unkeyed deployment with explicit agent header.
		return "a-anon-" + ah
	default: // session
		sess := ids.Session
		if sess == "" && ids.PrevResponseID != "" {
			// Responses-API chain: continue the scope that produced the
			// referenced response. Unmapped (restart, foreign id) falls
			// through to the stable agent/API-key bucket below — never a
			// one-turn silo keyed by the response id itself.
			if scope, ok := s.ScopeForResponse(ids.PrevResponseID, ownerHash(ids)); ok {
				return scope
			}
			// Chain fallthrough blends conversations into the tenant bucket;
			// make the silent merge observable.
			metrics.ChainFallback.Add(1)
		}
		if sess != "" {
			// Hash session id: filesystem-safe and collision-resistant under
			// safeKey-style sanitization of pathological inputs.
			sh := shortHash(sess)
			th := tenantHash(ids)
			if th != "" {
				// Multi-tenant safe: same conversation_id under two keys
				// must not share memory.
				return "s-" + th + "-" + sh
			}
			// Unkeyed but explicit session. Safe only when session IDs are
			// unguessable or the host is single-tenant.
			return "s-anon-" + sh
		}
		// No session: fall back to stable per-tenant / per-agent buckets.
		if ids.Agent != "" {
			ah := shortHash(ids.Agent)
			th := tenantHash(ids)
			if th != "" {
				return "s-" + th + "-agent-" + ah
			}
			return "s-anon-agent-" + ah
		}
		if th := tenantHash(ids); th != "" {
			return "s-" + th
		}
		// Identity-less: no durable store (closes s-default soup).
		return ""
	}
}

// respScopeEntry binds a scope to the API-key hash that produced it, so a
// response id cannot be used as a cross-key bearer token into another
// tenant's memory scope.
type respScopeEntry struct {
	scope     string
	ownerHash string // shortHash of the producing API key; "" = unkeyed deployment
}

// RememberResponseScope records which scope produced an upstream response id.
func (s *Store) RememberResponseScope(respID, scope, ownerHash string) {
	respID = trim(respID)
	if respID == "" || scope == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.respScope[respID]; !exists {
		s.respOrder = append(s.respOrder, respID)
	}
	s.respScope[respID] = respScopeEntry{scope: scope, ownerHash: ownerHash}
	// FIFO-evict oldest: a full-map reset would break every in-flight chain.
	for len(s.respOrder) > 4096 {
		oldest := s.respOrder[0]
		s.respOrder = s.respOrder[1:]
		delete(s.respScope, oldest)
	}
}

// ScopeForResponse resolves a previous_response_id to its originating scope.
// The presenter must carry the same API-key identity as the producer: a
// different key is denied, and an unkeyed ("", distinct identity — not a
// wildcard) producer's chain is only resolvable by unkeyed presenters, so a
// keyed tenant in a mixed deployment cannot ride an unkeyed chain into a
// foreign scope.
func (s *Store) ScopeForResponse(respID, presenterHash string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.respScope[respID]
	if !ok {
		return "", false
	}
	if e.ownerHash != presenterHash {
		return "", false
	}
	return e.scope, true
}

// waitPendingClose blocks until a detached close for key (reconfigure / LRU
// eviction) completes. A pending close can only exist when an engine for key
// was drained, so checking before the open-miss path is sufficient.
func (s *Store) waitPendingClose(key string) {
	for {
		s.mu.Lock()
		ch := s.closings[key]
		s.mu.Unlock()
		if ch == nil {
			return
		}
		<-ch
	}
}

func (s *Store) ForScope(key string) (Engine, error) {
	if key == "" {
		return nil, fmt.Errorf("cortext: no durable scope (missing session/agent/API identity)")
	}
	if s.closed.Load() {
		return nil, errStoreClosed
	}
	s.mu.Lock()
	if eng, ok := s.engines[key]; ok {
		// bump LRU
		for i, k := range s.order {
			if k == key {
				s.order = append(s.order[:i], s.order[i+1:]...)
				break
			}
		}
		s.order = append(s.order, key)
		s.mu.Unlock()
		return eng, nil
	}
	cfg := s.cfg
	s.mu.Unlock()

	// Miss path: open OUTSIDE the global lock. A native first open can
	// download/assemble release assets; that must never stall every scope's
	// request path (all interceptors read s.cfg under s.mu). openMu serializes
	// opens so concurrent misses for the same scope open exactly one handle.
	s.openMu.Lock()
	// A detached close for this scope's previous engine must finish before we
	// reopen the same file (slow native closes; stub persist overwrites).
	// Checked UNDER openMu: an eviction between acquiring openMu and opening
	// must not race this open.
	s.waitPendingClose(key)
	s.mu.Lock()
	if eng, ok := s.engines[key]; ok {
		s.mu.Unlock()
		s.openMu.Unlock()
		return eng, nil
	}
	s.mu.Unlock()

	if err := os.MkdirAll(cfg.DataDir, dataDirPerm); err != nil {
		// Caller (intercept) logs open_fail; avoid double-count here.
		s.openMu.Unlock()
		return nil, err
	}
	// Tighten permissions if the directory already existed world-readable.
	if err := os.Chmod(cfg.DataDir, dataDirPerm); err != nil {
		s.openMu.Unlock()
		return nil, fmt.Errorf("cortext: chmod data_dir: %w", err)
	}
	dbPath := filepath.Join(cfg.DataDir, key+".sqlite")
	eng, err := openEngine(dbPath, cfg)
	if err != nil {
		// Caller logs open_fail via logFail.
		s.openMu.Unlock()
		return nil, err
	}

	s.mu.Lock()
	if s.closed.Load() {
		// DisposeAll landed mid-open: do not insert into drained maps.
		s.mu.Unlock()
		s.openMu.Unlock()
		// Detached: a slow native close must not stall other scope opens.
		go func() { _ = eng.Close() }()
		return nil, errStoreClosed
	}
	if materialConfigChange(cfg, s.cfg) {
		// Config changed while opening; this handle points at the old
		// settings. Discard it and fail open — the next request reopens
		// against the new config.
		s.mu.Unlock()
		s.openMu.Unlock()
		// Detached: a slow native close must not stall other scope opens.
		go func() { _ = eng.Close() }()
		return nil, errConfigChanged
	}
	s.engines[key] = eng
	s.order = append(s.order, key)
	var doomed []doomedEngine
	for len(s.order) > maxEngines {
		oldest := s.order[0]
		s.order = s.order[1:]
		if e := s.engines[oldest]; e != nil {
			doomed = append(doomed, doomedEngine{key: oldest, eng: e})
			s.closings[oldest] = make(chan struct{})
		}
		delete(s.engines, oldest)
		delete(s.seen, oldest)
		delete(s.seenOrder, oldest)
		delete(s.writes, oldest)
		delete(s.writesTotal, oldest)
		delete(s.lastConsolidate, oldest)
		delete(s.consol, oldest)
		if s.bus != nil {
			s.bus.Drop(oldest)
		}
		metrics.ScopeEvict.Add(1)
	}
	closeCfg := s.cfg
	// Add under s.mu so it is ordered before DisposeAll's closeWg.Wait()
	// (a post-closeAdd Wait would race the goroutine spawn).
	detach := len(doomed) > 0
	if detach {
		s.closeWg.Add(1)
	}
	s.mu.Unlock()
	s.openMu.Unlock()
	// Consolidate/flush/close DETACHED: native close paths can be slow and
	// must not stall this request. The closings channels above already order
	// any same-file reopen; closeWg lets DisposeAll join in-flight closes.
	if detach {
		go func() {
			defer s.closeWg.Done()
			s.closeEngines(closeCfg, doomed)
		}()
	}
	return eng, nil
}

// Prewarm opens and closes a throwaway engine at registration so the native
// library's first-open cost (release-asset download/assembly, model init)
// lands at configure time, not on the first request. Best-effort: failures
// surface again on first real use. Takes openMu so a concurrent first request
// cannot double the download/assembly (the native library's concurrency
// guarantees are not assumed). Tracked in closeWg so DisposeAll joins it.
func (s *Store) Prewarm() {
	cfg := s.Config()
	if strings.TrimSpace(cfg.DataDir) == "" {
		return
	}
	s.mu.Lock()
	if s.closed.Load() {
		s.mu.Unlock()
		return
	}
	s.closeWg.Add(1)
	s.mu.Unlock()
	defer s.closeWg.Done()
	s.openMu.Lock()
	defer s.openMu.Unlock()
	if s.closed.Load() {
		return
	}
	// Re-read under openMu: a material reconfigure may have landed since the
	// snapshot; prewarm against current settings (Config takes s.mu).
	cfg = s.Config()
	if err := os.MkdirAll(cfg.DataDir, dataDirPerm); err != nil {
		return
	}
	if err := os.Chmod(cfg.DataDir, dataDirPerm); err != nil {
		return
	}
	p := filepath.Join(cfg.DataDir, ".prewarm.sqlite")
	eng, err := openEngine(p, cfg)
	if err != nil {
		return
	}
	_ = eng.Close()
	// Best-effort cleanup of the throwaway store and known sidecars.
	for _, f := range []string{p, p + "-wal", p + "-shm", p + ".stub.json"} {
		_ = os.Remove(f)
	}
}

// HasSeen reports whether a durable text hash was already claimed (dedupe for
// resubmitted transcripts). Ingest paths use ClaimSeen directly; HasSeen
// remains for assertions and diagnostics.
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

// maxSeenHashes bounds the per-scope durable-ingest dedupe set.
const maxSeenHashes = 4096

// ClaimSeen atomically records a durable-ingest hash and reports whether the
// caller won the claim. Concurrent identical ingests (retries, duplicate
// responses) previously passed a HasSeen check together and double-wrote;
// the claim closes that race. On ingest failure the winner must UnmarkSeen
// so a later attempt can retry.
func (s *Store) ClaimSeen(scope, hash string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.seen[scope]
	if m == nil {
		m = make(map[string]struct{})
		s.seen[scope] = m
	}
	if _, dup := m[hash]; dup {
		return false
	}
	m[hash] = struct{}{}
	s.seenOrder[scope] = append(s.seenOrder[scope], hash)
	// Bound growth per scope on the ORDER length (UnmarkSeen shrinks only the
	// map): FIFO-evict the oldest quarter so recent hashes survive; only
	// genuinely old lines can re-ingest.
	if order := s.seenOrder[scope]; len(order) > maxSeenHashes {
		drop := len(order) - maxSeenHashes + maxSeenHashes/4
		for _, h := range order[:drop] {
			delete(m, h)
		}
		s.seenOrder[scope] = append([]string(nil), order[drop:]...)
	}
	return true
}

// UnmarkSeen releases a claim after a failed ingest so retries re-ingest.
func (s *Store) UnmarkSeen(scope, hash string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.seen[scope], hash)
}

// NoteDurableIngest counts a durable write for the scope and reports whether
// a consolidate() is due. An explicit engine hint (hintDue) reports due
// immediately and resets the cadence counter; otherwise the counter advances
// and reports due every `every` writes. every <= 0 never reports due via the
// cadence, but configs normalize <= 0 to the default (25) — the user-facing
// disable knob is auto_consolidate: false.
func (s *Store) NoteDurableIngest(scope string, every int, hintDue bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writesTotal[scope]++
	if hintDue {
		s.writes[scope] = 0
		return true
	}
	if every <= 0 {
		return false
	}
	n := s.writes[scope] + 1
	due := n >= every
	if due {
		n = 0
	}
	s.writes[scope] = n
	return due
}

// AllowConsolidateAttempt throttles consolidate attempts to at most one per
// `gap` durable writes per scope. Without it, a persistent consolidation hint
// (e.g. a failing Consolidate that never clears the engine's backlog) would
// retry synchronously on every durable write on the request hot path.
func (s *Store) AllowConsolidateAttempt(scope string, gap int) bool {
	if gap < 1 {
		gap = 1
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	total := s.writesTotal[scope]
	if last, ok := s.lastConsolidate[scope]; ok && total-last < gap {
		return false
	}
	s.lastConsolidate[scope] = total
	return true
}

// consolState is the background-consolidate single-flight for one scope.
type consolState struct {
	running bool
	pending bool // a due arrived mid-run: coalesce into one follow-up run
}

// scheduleConsolidate runs eng.Consolidate in the background, at most one
// in-flight per scope. Consolidation is a model/DB operation whose latency
// must never land on the request/stream hot path; a due arriving mid-run
// coalesces into a single follow-up run instead of queueing unboundedly.
func (s *Store) scheduleConsolidate(scope string, eng Engine) {
	s.mu.Lock()
	st := s.consol[scope]
	if st == nil {
		st = &consolState{}
		s.consol[scope] = st
	}
	if st.running {
		st.pending = true
		s.mu.Unlock()
		return
	}
	st.running = true
	s.mu.Unlock()
	go s.consolidateLoop(scope, eng, st)
}

func (s *Store) consolidateLoop(scope string, eng Engine, st *consolState) {
	for {
		if err := eng.Consolidate(); err != nil {
			logFail(&metrics.ConsolidateFail, "consolidate_fail", fmt.Sprintf("scope=%s err=%v", scope, err))
		}
		s.mu.Lock()
		if s.consol[scope] != st {
			// State was dropped or replaced (engine eviction/dispose): stop
			// without touching the replacement state's flags.
			s.mu.Unlock()
			return
		}
		if st.pending {
			st.pending = false
			s.mu.Unlock()
			continue
		}
		st.running = false
		s.mu.Unlock()
		return
	}
}

func (s *Store) DisposeAll() {
	s.closed.Store(true)
	s.mu.Lock()
	doomed := s.drainEnginesLocked()
	s.respScope = make(map[string]respScopeEntry)
	s.respOrder = nil
	if s.bus != nil {
		s.bus.Clear()
	}
	cfg := s.cfg
	s.mu.Unlock()
	// Synchronous on shutdown: durability flushes must complete before exit —
	// both our own drained engines and any detached closes still in flight.
	s.closeEngines(cfg, doomed)
	s.closeWg.Wait()
}

// drainEnginesLocked clears all engine bookkeeping and returns the open
// engines. Caller must hold s.mu; the returned engines are closed after the
// lock is released via closeEngines. A pending-close marker per scope makes
// a same-file reopen wait for the close to finish.
func (s *Store) drainEnginesLocked() []doomedEngine {
	doomed := make([]doomedEngine, 0, len(s.engines))
	for k, e := range s.engines {
		doomed = append(doomed, doomedEngine{key: k, eng: e})
		s.closings[k] = make(chan struct{})
		if s.bus != nil {
			s.bus.Drop(k)
		}
	}
	s.engines = make(map[string]Engine)
	s.order = nil
	s.seen = make(map[string]map[string]struct{})
	s.seenOrder = make(map[string][]string)
	s.writes = make(map[string]int)
	s.writesTotal = make(map[string]int)
	s.lastConsolidate = make(map[string]int)
	s.consol = make(map[string]*consolState)
	return doomed
}

// doomedEngine is an engine drained from the live maps, pending close.
type doomedEngine struct {
	key string
	eng Engine
}

// closeEngines consolidates/flushes/closes drained engines. Runs outside the
// store lock so slow native close paths never stall other scopes, then
// signals any ForScope waiting to reopen the same scope file.
func (s *Store) closeEngines(cfg PluginConfig, doomed []doomedEngine) {
	for _, d := range doomed {
		if cfg.AutoConsolidate {
			_ = d.eng.Consolidate()
		}
		_ = d.eng.Flush()
		_ = d.eng.Close()
	}
	if len(doomed) > 0 {
		s.mu.Lock()
		for _, d := range doomed {
			if ch := s.closings[d.key]; ch != nil {
				delete(s.closings, d.key)
				close(ch)
			}
		}
		s.mu.Unlock()
	}
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
			ids.APIKey = normalizeAuthKey(auth)
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
		)
		// Chain pointer, not a session key — resolved via the response-id map.
		ids.PrevResponseID = gjson.GetBytes(body, "previous_response_id").String()
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

// normalizeAuthKey strips the bearer scheme (any case) so the same
// credential hashes to one tenant regardless of header formatting.
func normalizeAuthKey(v string) string {
	v = trim(v)
	if len(v) >= 7 && strings.EqualFold(v[:7], "bearer ") {
		v = trim(v[7:])
	}
	return v
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

// ownerHash binds response-id chains to the API key that produced them.
func ownerHash(ids ScopeIDs) string {
	if ids.APIKey == "" {
		return ""
	}
	return shortHash(ids.APIKey)
}
