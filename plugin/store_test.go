package main

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// Responses-API clients chain turns via previous_response_id, which changes
// every turn. The scope must follow the chain back to one continuous store
// instead of fragmenting into one-turn silos.
func TestScopeKeyPreviousResponseChain(t *testing.T) {
	st := NewStore(DefaultConfig())
	defer st.DisposeAll()

	// Turn 1: no session header, no previous_response_id → API-key bucket.
	turn1 := st.ScopeKey(ScopeIDs{APIKey: "key-xyz"})
	if !strings.HasPrefix(turn1, "s-") {
		t.Fatalf("turn1 scope = %q", turn1)
	}

	// The upstream response id produced under turn1's scope is recorded.
	st.RememberResponseScope("resp_aaa", turn1, ownerHash(ScopeIDs{APIKey: "key-xyz"}))

	// Turn 2 chains via previous_response_id → same scope as turn 1.
	turn2 := st.ScopeKey(ScopeIDs{APIKey: "key-xyz", PrevResponseID: "resp_aaa"})
	if turn2 != turn1 {
		t.Fatalf("chain broke: turn1=%q turn2=%q", turn1, turn2)
	}

	// Turn 3 chains from turn 2's response → still the same scope.
	st.RememberResponseScope("resp_bbb", turn2, ownerHash(ScopeIDs{APIKey: "key-xyz"}))
	turn3 := st.ScopeKey(ScopeIDs{APIKey: "key-xyz", PrevResponseID: "resp_bbb"})
	if turn3 != turn1 {
		t.Fatalf("chain broke at turn 3: got %q want %q", turn3, turn1)
	}
}

// A response id presented under a different API key must not unlock the
// producing tenant's scope (bearer-token leak vector).
func TestScopeForResponseOwnerBinding(t *testing.T) {
	st := NewStore(DefaultConfig())
	defer st.DisposeAll()

	st.RememberResponseScope("resp_secret", "s-victim", ownerHash(ScopeIDs{APIKey: "key-a"}))
	if _, ok := st.ScopeForResponse("resp_secret", ownerHash(ScopeIDs{APIKey: "key-b"})); ok {
		t.Fatal("cross-key chain resolution must be denied")
	}
	if got, ok := st.ScopeForResponse("resp_secret", ownerHash(ScopeIDs{APIKey: "key-a"})); !ok || got != "s-victim" {
		t.Fatalf("same-key resolution failed: %q %v", got, ok)
	}
	// Unkeyed entries are a distinct identity, not a wildcard: keyed presenters
	// are denied, unkeyed presenters admitted.
	st.RememberResponseScope("resp_open", "s-shared", "")
	if _, ok := st.ScopeForResponse("resp_open", ownerHash(ScopeIDs{APIKey: "key-b"})); ok {
		t.Fatal("unkeyed entry must not admit a keyed presenter")
	}
	if got, ok := st.ScopeForResponse("resp_open", ""); !ok || got != "s-shared" {
		t.Fatalf("unkeyed presenter resolution failed: %q %v", got, ok)
	}
}

// The 4096 bound must evict the oldest entry only, not reset the whole map
// (a full reset breaks every in-flight chain simultaneously).
func TestRememberResponseScopeFIFOEviction(t *testing.T) {
	st := NewStore(DefaultConfig())
	defer st.DisposeAll()

	st.RememberResponseScope("resp_oldest", "s-old", "")
	for i := 0; i < 4096; i++ {
		st.RememberResponseScope(fmt.Sprintf("resp_%d", i), "s-new", "")
	}
	if _, ok := st.ScopeForResponse("resp_oldest", ""); ok {
		t.Fatal("oldest entry should have been FIFO-evicted")
	}
	if len(st.respScope) != 4096 {
		t.Fatalf("map size = %d, want 4096", len(st.respScope))
	}
}

// An unmapped previous_response_id (restart, foreign id) must fall back to
// the stable agent/API-key bucket — never a silo keyed by the id itself.
func TestScopeKeyUnmappedPrevResponseFallsBack(t *testing.T) {
	st := NewStore(DefaultConfig())
	defer st.DisposeAll()

	got := st.ScopeKey(ScopeIDs{APIKey: "key-xyz", PrevResponseID: "resp_unknown"})
	want := st.ScopeKey(ScopeIDs{APIKey: "key-xyz"})
	if got != want {
		t.Fatalf("unmapped prev id: got %q want bucket %q", got, want)
	}
	if strings.Contains(got, "resp_unknown") {
		t.Fatalf("response id leaked into scope key: %q", got)
	}
}

func TestResolveScopeIDsSplitsPrevResponseID(t *testing.T) {
	cfg := DefaultConfig()
	body := []byte(`{"previous_response_id":"resp_123","input":"hi"}`)
	ids := ResolveScopeIDs(cfg, nil, body, nil)
	if ids.PrevResponseID != "resp_123" {
		t.Fatalf("PrevResponseID = %q", ids.PrevResponseID)
	}
	if ids.Session != "" {
		t.Fatalf("previous_response_id must not become Session, got %q", ids.Session)
	}
	// conversation_id still wins as a real session key.
	body2 := []byte(`{"conversation_id":"conv-9","previous_response_id":"resp_123"}`)
	ids2 := ResolveScopeIDs(cfg, nil, body2, nil)
	if ids2.Session != "conv-9" || ids2.PrevResponseID != "resp_123" {
		t.Fatalf("ids2 = %+v", ids2)
	}
}

func TestExtractResponseID(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"plain", `{"id":"resp_aaa","object":"response"}`, "resp_aaa"},
		{"sse", `data: {"id":"chatcmpl-1","choices":[]}`, "chatcmpl-1"},
		{"responses event", `data: {"type":"response.completed","response":{"id":"resp_done"}}`, "resp_done"},
		{"event-prefixed framing", "event: response.created\ndata: {\"response\":{\"id\":\"resp_ev\"}}", "resp_ev"},
		{"multi-line chunk, id on later line", "data: {\"choices\":[]}\n\ndata: {\"id\":\"resp_late\"}", "resp_late"},
		{"id in first line wins", "data: {\"id\":\"resp_first\"}\n\ndata: [DONE]", "resp_first"},
		{"comment line skipped", ": keep-alive\ndata: {\"id\":\"resp_c\"}", "resp_c"},
		{"done marker", `data: [DONE]`, ""},
		{"none", `{"choices":[]}`, ""},
		{"empty", ``, ""},
	}
	for _, tc := range cases {
		if got := ExtractResponseID([]byte(tc.body)); got != tc.want {
			t.Errorf("%s: got %q want %q", tc.name, got, tc.want)
		}
	}
}

// CPA chunks can carry several SSE lines; [DONE] must be detected anywhere.
func TestIsStreamDoneMultiline(t *testing.T) {
	cases := []struct {
		body string
		want bool
	}{
		{"data: [DONE]", true},
		{"[DONE]", true},
		{"data: {\"choices\":[]}\n\ndata: [DONE]", true},
		{"event: done\ndata: [DONE]", true},
		{"data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := IsStreamDone([]byte(tc.body)); got != tc.want {
			t.Errorf("IsStreamDone(%q) = %v, want %v", tc.body, got, tc.want)
		}
	}
}

// Two concurrent streams in the same scope must not share a buffer, and a
// retry of an aborted identical request must not inherit stale partial text.
func TestStreamKeysIsolateConcurrentStreams(t *testing.T) {
	k1 := streamKey("s-x", []byte(`{"messages":[{"role":"user","content":"q1"}]}`))
	k2 := streamKey("s-x", []byte(`{"messages":[{"role":"user","content":"q2"}]}`))
	if k1 == k2 {
		t.Fatal("distinct requests must get distinct stream keys")
	}
	if _, _, flush := appendStreamDelta(k1, deltaText, "hello from stream one."); !flush {
		t.Fatal("expected sentence-boundary flush")
	}
	seg, _, flush := appendStreamDelta(k2, deltaText, "two.")
	if !flush || !strings.Contains(seg, "two") || strings.Contains(seg, "one") {
		t.Fatalf("stream two buffer contaminated: %q", seg)
	}
	clearStreamBuf(k1)
	clearStreamBuf(k2)
}

func TestStreamResetOnRetry(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"same question"}]}`)
	key := streamKey("s-retry", body)
	// Aborted attempt leaves a partial buffer (no boundary, no flush).
	if _, _, flush := appendStreamDelta(key, deltaText, "partial aborted text"); flush {
		t.Fatal("short delta should not flush")
	}
	// Re-init over unflushed text cannot distinguish a retry from a concurrent
	// identical stream: the buffer is poisoned, never reset-then-interleaved.
	resetStreamBuf(key)
	if !streamConflicted(key) {
		t.Fatal("re-init over unflushed text must mark conflict")
	}
	clearStreamBuf(key)

	// Init with no in-flight buffer starts clean; the fresh attempt's text is
	// not contaminated by anything stale.
	resetStreamBuf(key)
	seg, _, flush := appendStreamDelta(key, deltaText, "fresh attempt.")
	if !flush || seg != "fresh attempt." {
		t.Fatalf("retry inherited stale text: %q", seg)
	}
	if streamConflicted(key) {
		t.Fatal("init with no in-flight buffer must not conflict")
	}
	clearStreamBuf(key)
}

// Flushing one stream's residual must leave a concurrent stream's buffer
// intact (no premature mid-stream flush).
func TestFlushStreamBufIsolatesConcurrentStreams(t *testing.T) {
	dir := t.TempDir()
	cfg := DefaultConfig()
	cfg.DataDir = dir
	cfg.AutoConsolidate = false
	cfg.InterruptGate = false
	svc := NewService(cfg)

	scope := "s-flush"
	kA := streamKey(scope, []byte(`{"req":"a"}`))
	kB := streamKey(scope, []byte(`{"req":"b"}`))
	appendStreamDelta(kA, deltaText, "residual from A")
	appendStreamDelta(kB, deltaText, "still streaming from B")

	eng, err := svc.store.ForScope(scope)
	if err != nil {
		t.Fatal(err)
	}
	svc.flushStreamBuf(kA, scope, eng, cfg)
	clearStreamBuf(kA)

	if hasStreamBuf(kA) {
		t.Fatal("A should be drained")
	}
	if !hasStreamBuf(kB) {
		t.Fatal("B must keep its buffer while still streaming")
	}
	seg, _, _ := appendStreamDelta(kB, deltaText, " more of B.")
	if !strings.Contains(seg, "still streaming from B more of B.") {
		t.Fatalf("B buffer lost or corrupted: %q", seg)
	}
	clearStreamBuf(kB)
}

func TestDropQueryEchoes(t *testing.T) {
	long := strings.Repeat("the quick brown fox jumps over the lazy dog ", 3)
	items := []MemoryItem{
		{Text: long},            // exact echo
		{Text: long + " extra"}, // memory contains query
		{Text: strings.TrimSpace(long[:len(long)-8])}, // memory is near-verbatim substring of query
		{Text: "Bailey likes tennis"},                 // unrelated, kept
	}
	out := dropQueryEchoes(items, long)
	if len(out) != 1 || out[0].Text != "Bailey likes tennis" {
		t.Fatalf("out = %+v", out)
	}
}

// Same explicit session id under two API keys must never share a durable store
// (multi-tenant isolation on the shipped ScopeKey path).
func TestSessionScopeNamespacedByAPIKey(t *testing.T) {
	st := NewStore(DefaultConfig())
	defer st.DisposeAll()

	const sess = "shared-conversation-id"
	a := st.ScopeKey(ScopeIDs{Session: sess, APIKey: "Bearer key-tenant-a"})
	b := st.ScopeKey(ScopeIDs{Session: sess, APIKey: "Bearer key-tenant-b"})
	if a == "" || b == "" {
		t.Fatalf("expected durable scopes, got a=%q b=%q", a, b)
	}
	if a == b {
		t.Fatalf("same session under different API keys collided: %q", a)
	}
	if !strings.HasPrefix(a, "s-") || !strings.HasPrefix(b, "s-") {
		t.Fatalf("unexpected scope shape a=%q b=%q", a, b)
	}
	// Tenant hash must appear in the key; bare session alone is insufficient.
	if a == "s-"+shortHash(sess) || b == "s-"+shortHash(sess) {
		t.Fatalf("session not namespaced by tenant: a=%q b=%q", a, b)
	}
	// Pathological session strings that safeKey would collapse must still isolate.
	p1 := st.ScopeKey(ScopeIDs{Session: "...", APIKey: "k1"})
	p2 := st.ScopeKey(ScopeIDs{Session: "///", APIKey: "k1"})
	if p1 == p2 {
		t.Fatalf("pathological sessions collided under same tenant: %q", p1)
	}
}

// Identity-less traffic must not open a shared s-default soup.
func TestIdentityLessScopeIsEmpty(t *testing.T) {
	st := NewStore(DefaultConfig())
	defer st.DisposeAll()
	if got := st.ScopeKey(ScopeIDs{}); got != "" {
		t.Fatalf("identity-less ScopeKey = %q, want empty (no durable store)", got)
	}
	if _, err := st.ForScope(""); err == nil {
		t.Fatal("ForScope(\"\") must error")
	}
}

// Cross-tenant: seed under key A, probe under key B with same session header —
// B must not recall A's durable fact (shipped HandleRequestBeforeAuth path).
func TestCrossTenantSessionIsolationEndToEnd(t *testing.T) {
	dir := t.TempDir()
	cfg := DefaultConfig()
	cfg.DataDir = dir
	cfg.AutoConsolidate = false
	cfg.InterruptGate = false
	svc := NewService(cfg)

	hA := makeHeader("X-Cortext-Session", "conv-shared", "Authorization", "Bearer secret-a")
	hB := makeHeader("X-Cortext-Session", "conv-shared", "Authorization", "Bearer secret-b")

	// Resolve scopes through the same production path interceptors use.
	idsA := ResolveScopeIDs(cfg, hA, nil, nil)
	idsB := ResolveScopeIDs(cfg, hB, nil, nil)
	scopeA := svc.store.ScopeKey(idsA)
	scopeB := svc.store.ScopeKey(idsB)
	if scopeA == "" || scopeB == "" || scopeA == scopeB {
		t.Fatalf("scopes not isolated: A=%q B=%q", scopeA, scopeB)
	}

	svc.HandleRequestBeforeAuth(pluginapi.RequestInterceptRequest{
		SourceFormat: fmtOpenAI,
		Headers:      hA,
		Body:         []byte(`{"messages":[{"role":"user","content":"Tenant-A-only secret: purple-narwhal-99"}]}`),
	})

	// Confirm A can recall its own fact.
	respA := svc.HandleRequestBeforeAuth(pluginapi.RequestInterceptRequest{
		SourceFormat: fmtOpenAI,
		Headers:      hA,
		Body:         []byte(`{"messages":[{"role":"user","content":"purple-narwhal"}]}`),
	})
	if len(respA.Body) == 0 || !strings.Contains(string(respA.Body), "purple-narwhal-99") {
		t.Fatalf("tenant A should recall own secret, got %s", respA.Body)
	}

	// B with same session id must not see A's secret.
	respB := svc.HandleRequestBeforeAuth(pluginapi.RequestInterceptRequest{
		SourceFormat: fmtOpenAI,
		Headers:      hB,
		Body:         []byte(`{"messages":[{"role":"user","content":"purple-narwhal"}]}`),
	})
	if len(respB.Body) > 0 && strings.Contains(string(respB.Body), "purple-narwhal-99") {
		t.Fatalf("tenant B leaked tenant A memory: %s", respB.Body)
	}

	// Identity-less probe must not durable-ingest or create a default store.
	resetMetricsForTest()
	before, _ := listScopeFiles(dir)
	svc.HandleRequestBeforeAuth(pluginapi.RequestInterceptRequest{
		SourceFormat: fmtOpenAI,
		Body:         []byte(`{"messages":[{"role":"user","content":"orphan fact should not land in soup"}]}`),
	})
	after, _ := listScopeFiles(dir)
	if len(after) != len(before) {
		t.Fatalf("identity-less request created store files: before=%v after=%v", before, after)
	}
	if SnapshotMetrics().SkipNoIdentity == 0 {
		t.Fatal("expected skip_no_identity metric for identity-less request")
	}
}

func makeHeader(kv ...string) http.Header {
	h := http.Header{}
	for i := 0; i+1 < len(kv); i += 2 {
		h.Set(kv[i], kv[i+1])
	}
	return h
}

func listScopeFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names, nil
}

// Reconfigure with material engine knobs must dispose open handles so the next
// open picks up new settings (and clear the process-local response-id map).
func TestReconfigureMaterialDisposesEngines(t *testing.T) {
	dir := t.TempDir()
	cfg := DefaultConfig()
	cfg.DataDir = dir
	cfg.AutoConsolidate = false
	st := NewStore(cfg)
	defer st.DisposeAll()

	scope := st.ScopeKey(ScopeIDs{Session: "r1", APIKey: "k1"})
	eng1, err := st.ForScope(scope)
	if err != nil {
		t.Fatal(err)
	}
	st.RememberResponseScope("resp_keep", scope, ownerHash(ScopeIDs{APIKey: "k1"}))

	cfg2 := cfg
	cfg2.Focus = 0.9
	st.Reconfigure(cfg2)

	if _, ok := st.ScopeForResponse("resp_keep", ownerHash(ScopeIDs{APIKey: "k1"})); ok {
		t.Fatal("material reconfigure must clear previous_response_id map")
	}
	eng2, err := st.ForScope(scope)
	if err != nil {
		t.Fatal(err)
	}
	if eng1 == eng2 {
		t.Fatal("expected new engine instance after material reconfigure")
	}
}

// Registration metadata embeds engine flavor so operators can tell stub vs native.
func TestEngineFlavorInRegistration(t *testing.T) {
	// engineFlavor is set by the build-tagged engine file (stub under default tests).
	if engineFlavor != "stub" && engineFlavor != "native" {
		t.Fatalf("engineFlavor = %q", engineFlavor)
	}
	reg := pluginRegistration()
	wantSuffix := "+" + engineFlavor
	if !strings.HasSuffix(reg.Metadata.Version, wantSuffix) {
		t.Fatalf("registration Version = %q, want suffix %q", reg.Metadata.Version, wantSuffix)
	}
	if !strings.HasPrefix(reg.Metadata.Version, pluginVersion) {
		t.Fatalf("registration Version = %q, want prefix %q", reg.Metadata.Version, pluginVersion)
	}
}

// data_dir must be created private (0700).
func TestDataDirPrivateMode(t *testing.T) {
	dir := t.TempDir()
	// Use a subdirectory the store will create.
	data := filepath.Join(dir, "cortext-data")
	cfg := DefaultConfig()
	cfg.DataDir = data
	cfg.AutoConsolidate = false
	st := NewStore(cfg)
	defer st.DisposeAll()
	if _, err := st.ForScope(st.ScopeKey(ScopeIDs{Session: "m", APIKey: "k"})); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(data)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Fatalf("data_dir mode = %o, want 0700", perm)
	}
}

// The seen-map bound evicts oldest-first (FIFO); it must not reset the whole
// set — a full reset let the next request re-ingest thousands of old lines.
func TestSeenMapFIFOEviction(t *testing.T) {
	cfg := DefaultConfig()
	cfg.AutoConsolidate = false
	st := NewStore(cfg)
	defer st.DisposeAll()
	hash := func(i int) string { return fmt.Sprintf("h%05d", i) }
	total := maxSeenHashes + 200
	for i := 0; i < total; i++ {
		if !st.ClaimSeen("scope", hash(i)) {
			t.Fatalf("claim %d should win (unique hash)", i)
		}
	}
	if st.HasSeen("scope", hash(0)) {
		t.Error("oldest hash should have been FIFO-evicted")
	}
	if !st.HasSeen("scope", hash(maxSeenHashes/2)) {
		t.Error("mid-range hash lost: map was reset, not FIFO-evicted")
	}
	if !st.HasSeen("scope", hash(total-1)) {
		t.Error("newest hash missing")
	}
	// Re-claiming an existing hash must fail and not duplicate its order entry.
	if st.ClaimSeen("scope", hash(total-1)) {
		t.Error("duplicate claim should lose")
	}
	if st.ClaimSeen("scope", hash(total-1)) {
		t.Error("duplicate claim should lose")
	}
	if got := len(st.seenOrder["scope"]); got != total-1025 {
		t.Errorf("seenOrder len = %d, want %d", got, total-1025)
	}
	// UnmarkSeen releases a claim so a failed ingest can retry.
	st.UnmarkSeen("scope", hash(total-1))
	if !st.ClaimSeen("scope", hash(total-1)) {
		t.Error("claim after UnmarkSeen should win")
	}
}

// After DisposeAll, a late intercept holding the old Service must not reopen
// engines into the drained store.
func TestForScopeAfterDisposeAll(t *testing.T) {
	cfg := DefaultConfig()
	cfg.DataDir = t.TempDir()
	cfg.AutoConsolidate = false
	st := NewStore(cfg)
	scope := st.ScopeKey(ScopeIDs{Session: "late"})
	if _, err := st.ForScope(scope); err != nil {
		t.Fatal(err)
	}
	st.DisposeAll()
	if _, err := st.ForScope(scope); !errors.Is(err, errStoreClosed) {
		t.Fatalf("ForScope after DisposeAll = %v, want errStoreClosed", err)
	}
}

// enabled=false must turn every interceptor into a pass-through and create no
// store, regardless of traffic shape.
func TestDisabledIsPassThrough(t *testing.T) {
	dir := t.TempDir()
	cfg := DefaultConfig()
	cfg.DataDir = dir
	cfg.Enabled = false
	svc := NewService(cfg)
	h := http.Header{}
	h.Set("X-Cortext-Session", "off-1")

	if r := svc.HandleRequestBeforeAuth(pluginapi.RequestInterceptRequest{
		SourceFormat: fmtOpenAI,
		Headers:      h,
		Body:         []byte(`{"messages":[{"role":"user","content":"store me not"}]}`),
	}); len(r.Body) != 0 {
		t.Fatal("disabled request interceptor modified the body")
	}
	if r := svc.HandleResponse(pluginapi.ResponseInterceptRequest{
		SourceFormat:   fmtOpenAI,
		RequestHeaders: h,
		Body:           []byte(`{"choices":[{"message":{"content":"assistant text"}}]}`),
	}); len(r.Body) != 0 {
		t.Fatal("disabled response interceptor modified the body")
	}
	if r := svc.HandleStreamChunk(pluginapi.StreamChunkInterceptRequest{
		SourceFormat:   fmtOpenAI,
		RequestHeaders: h,
		ChunkIndex:     0,
		Body:           []byte(`{"choices":[{"delta":{"content":"chunk"}}]}`),
	}); len(r.Body) != 0 {
		t.Fatal("disabled stream interceptor modified the chunk")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("disabled service created store files: %v", entries)
	}
}

// A conflicted (poisoned) buffer decays after streamConflictTTL: an
// alone-in-flight identical stream later starts clean instead of being
// poisoned forever.
func TestConflictedBufferDecaysAfterTTL(t *testing.T) {
	key := streamKey("s-decay", []byte("body"))
	appendStreamDelta(key, deltaText, "stale residual")
	resetStreamBuf(key) // conflict
	if !streamConflicted(key) {
		t.Fatal("expected conflict")
	}
	// Age the entry past the TTL.
	streamStates.mu.Lock()
	streamStates.m[key].last = time.Now().Add(-2 * streamConflictTTL)
	streamStates.mu.Unlock()
	resetStreamBuf(key)
	if streamConflicted(key) {
		t.Fatal("decayed conflict must not poison a fresh stream")
	}
	clearStreamBuf(key)
}
