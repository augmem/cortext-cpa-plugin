# Release evaluation — does cortext-cpa-plugin help?

**Verdict: yes, with named limits.** Evaluated 2026-07-25 against a live
CLIProxyAPI (homebrew service, macOS arm64) loading the `cortext_native`
build of this plugin (`github.com/augmem/cortext.go` + release natives).

This document is the scrutiny entrypoint: every claim below names the
artifact that proves it and the command that regenerates it.

## Proof layers (strongest first)

### 1. Live A/B — memory arm vs control arm, same proxy, same model

`bench/judge_eval/live_ab.py` seeds facts in session A, then probes with
history-less bodies (only plugin-injected memory can answer), and asks the
identical probe in a fresh control session B (plugin active, nothing seeded).

- **memory arm 8/8, control arm 0/8** (4 semantic facts graded by an LLM
  judge; 4 random-hex-token facts exact-matched, where a hallucinated
  control pass is ~2^-32). 5 of 8 control answers were empty/refusals —
  expected, since the control model had no access to the facts.
- Derailment guard: after seeding, "What is the capital of France?" still
  answered "Paris" in all 8 memory sessions — injection does not hijack
  unrelated answers.
- Judge calls each ran in their own fresh session so judge prompts could not
  contaminate subjects or each other.

Artifacts (local, gitignored under `bench/**/out/` so operator home paths
are not committed): regenerate with the command below; expect
`memory_gt_control: true`, `guards_ok: true` in `summary.json`.
Reproduce: `python3 bench/judge_eval/live_ab.py --base http://127.0.0.1:8317/v1 --key <cpa-key> --out ./bench/judge_eval/out`

### 2. Blackbox e2e — 25/25 against live CPA

`bench/e2e_blackbox/run.py`: store creation, user-input recall, multi-fact
selectivity, correction chains (A→B→C; only the final value is current, both
versions remain in the store), history resubmit, assistant-invented tokens
(never typed by the user) recalled next turn, distractor survival, session
isolation (other session neither recalls nor stores the secret), late-turn
recall. Every token claim is verified at the store layer (bytes present in
the session SQLite/WAL), not just in model output.

Artifact (gitignored): `bench/e2e_blackbox/out/summary.json` should show
`ok: true`, `failed: []`, 25 passed cases with token identities and store paths.
Reproduce: `make e2e-blackbox CPA_BASE=http://127.0.0.1:8317 CPA_MODEL=kimi-k2.7-code`

### 3. Gap paths — 18/18 against live CPA

`bench/e2e_blackbox/gaps.py`: SSE stream ingest + next-turn recall,
stream path markers in store, exclusive update, ranking stress (≥24-turn
window), **brew-service-restart durability** (CPA restarted mid-suite;
plugin remapped; pre-restart facts still recalled after).

Artifact (gitignored): `bench/e2e_blackbox/gaps_out/summary.json` should show
`failed: []`, 18 passed.
Reproduce: `python3 bench/e2e_blackbox/gaps.py --base http://127.0.0.1:8317 --model kimi-k2.7-code --out ./bench/e2e_blackbox/gaps_out`

### 4. ABI live smoke

`bench/abi_live` loads `bin/cortext.dylib` through the real
`cliproxy_plugin_init` entrypoint: registration, memory inject into
OpenAI/Claude/Gemini bodies, session isolation (no leak), durability across
engine re-open. Passed against the exact native dylib shipped to
`~/.cli-proxy-api/plugins/`.
Reproduce: `make build-native && cd bench/abi_live && go run . -plugin ../../bin/cortext.dylib -out /tmp/cpa-live`
(Stub ABI only: `make build-stub` and point `-plugin` at `bin/cortext-stub.*`.)

### 5. Unit + race + native smoke

`cd plugin && go test ./... -count=1 && go test -race ./... -count=1`
(stub engine) and `go test -tags cortext_native ./...` (real libcortext
open/process/flush) all pass. Coverage includes: previous_response_id
chain continuity + owner binding + FIFO eviction, concurrent-stream buffer
isolation, retry buffer reset, multi-line/`event:`-framed SSE parsing,
coalesced final-delta capture, non-OpenAI stream terminators, memory-line
dedupe, query-echo dropping, noise-stream lazy engine creation.

### 6. Blind review — recursive, fresh reviewer per round

Blind reviewers (no shared context, fresh instance per round per the
recursive-review rule) reviewed the uncommitted diff. The first pass (in the
evaluation session) ran 3 rounds: round 1 found 9 defects (cross-stream
premature flush; stream-buffer leak and retry contamination; multi-line
`[DONE]` missed; `event:`-framed response-id extraction; full-map reset cliff
in the response-id map; engine created for noise chunks; untracked build
binary; missing test coverage; reasoning ingest predicate), round 2 found 2
(final delta dropped on coalesced terminator chunks; OpenAI-only
terminators), round 3 was P1-clean. A later consolidation-cadence change went
through the same loop (3 findings, all fixed).

The second pass (2026-07-26, this diff) fixed the four residuals named below,
then ran three more blind rounds with every finding fixed and covered by a
regression test:

- Round 1 (8 findings): closed engines silently swallowed durable ingests
  while marking them seen (`errEngineClosed` sentinel + claim release);
  `window_messages` could orphan tool turns (provider 400s — window now
  extends left); inject not idempotent on Claude/Gemini array paths;
  unkeyed response-id chains admitted any presenter (strict identity match);
  service resurrected with default config after shutdown (now pass-through);
  slow engine closes ran under the global store lock (now outside both
  locks); Claude streamed tool-call args never ingested.
- Round 2 (6 findings): native first-open (asset download) ran under the
  store lock on the hot path (opens now serialized outside the lock +
  prewarm at registration); byte-identical concurrent streams interleaved
  into durable memory (buffer poisoning + response-body recovery path);
  HasSeen/MarkSeen check-then-act race (atomic ClaimSeen); OpenAI chat
  `tool_calls` never extracted; bus truncation could split a UTF-8 rune;
  stale docs.
- Round 3 (6 findings): `Consolidate()` ran synchronously on the hot path
  (background single-flight per scope); engines created for traffic with no
  memory consumer (lazy-open gates); Responses `function_call` items dropped;
  multipart OpenAI system messages flattened on inject; register blocked on
  prewarm (now detached); `go vet` unwired (Makefile + CI).
- Sign-off pass (3 fresh reviewers in parallel, P1-blocking): reviewer B
  found a P1 — `ingest_assistant: false` was bypassed on the history-resubmit
  path (fixed, negative test added) — plus a fence-neutralize whitespace
  bypass and a prewarm/openMu race (both fixed); reviewer A found a
  close-under-openMu stall and a stale consolidate-loop flag race
  (reproduced, fixed, regression test); reviewer C found the `interactions`
  format family silently no-op'd, a stream-buffer self-eviction at map cap,
  dead window-dedupe code (now wired post-windowing), an unclosed-fence
  truncation, and smaller nits.
- Sign-off pass 2 (3 fresh reviewers): proved the pending-close reopen race
  was incompletely guarded (re-validated under openMu), a fence-regex bypass
  via malformed tags (broadened), and found the eviction close still running
  synchronously on the request path (detached), the seen-order unbounded
  growth under ingest failure, JSON round-trip corruption of 64-bit ints
  (UseNumber decode), stream partial-failure suppression (full-body recovery
  now keyed on failure marks), tool-arg JSON fragmented by the prose
  segmenter (dedicated whole-buffer path), plus smaller fixes.
- Sign-off pass 3 (3 fresh reviewers): proved a neutralize reassembly attack
  (role token inside the fence name; passes now run to a fixpoint), Gemini
  `thought:true` parts bypassing `ingest_reasoning` (excluded/classified as
  reasoning everywhere), and the `interactions` family still broken at the
  CPA translation layer (support dropped honestly — interactions traffic
  passes through untouched instead of being half-rewritten; the earlier
  "routed to input shape" fix was wrong about CPA's interactions wire
  shapes). Also: full-width/entity fence lookalikes, errStoreClosed log
  noise, eviction-close detach (actually landed this time, with WaitGroup
  ordering), non-stream multi-choice extraction, window system-hoisting.
- Sign-off pass 4 (3 fresh reviewers): proved two P1s — init-init-before-
  delta concurrent identical streams still interleaved into durable memory
  (poisoning now triggers on ANY existing buffer entry) and Responses-API
  `reasoning` items leaking CoT as user text (excluded) — plus the mixed-
  kind coalesced-chunk drop, OpenAI streamed tool_calls capture, conflict
  stickiness past [DONE], and a stale `streamIngested` doc.
- Sign-off pass 5 (3 fresh reviewers, one timed out): proved two P1s —
  non-stream Responses reasoning items leaking CoT via `ExtractAssistantText`
  and unrecognized-format bodies being rewritten with an invented `messages`
  array (injection now no-ops on bodies with neither `messages` nor a string
  `system`) — plus antigravity-envelope delta extraction, per-line mixed
  kinds, conflict TOCTOU, and a prewarm data race I introduced mid-round
  (fixed with `s.Config()`).
- Sign-off pass 6 (3 fresh reviewers): proved a P1 — stream delta
  `content`/`reasoning_content` read with gjson `.String()` on non-string
  values, ingesting raw JSON incl. CoT (all scalar delta reads now require
  `Type == gjson.String`, array content is walked part-by-part with the
  reasoning filter) — and a second P1, `stripMemoryBlock` deleting client
  text on re-inject (now strips only blocks carrying the plugin's fixed
  header line). Also: permanent conflict poisoning (60s TTL decay),
  whitespace-colliding dedupe hash (now run-collapsing, full-text hashed
  before truncation), per-chunk scope-cache (identity+body keyed), and a
  scope-cache key bug my own fix introduced (caught by test pollution).
- **Sign-off pass 8 (final): three fresh blind adversarial reviewers
  (concurrency, security, release-surface lenses) each returned
  "Decision: Proven for release-readiness" with zero P1 findings on the
  final diff.** Their P2/P3 findings are preserved as the residuals below;
  reports are in the review-baselines directory named above.

(One pass-5 reviewer timed out and one pass-6 reviewer rejected on a race
introduced by that round's own fixes; both were replaced by fresh instances
per the no-reviewer-reuse rule. Pass numbering skips the timed-out round's
replacement.)

Review artifacts live under `~/.agents/projects/cortext-cpa-plugin/artifacts/review-baselines/`
(agent-session files, not in this repo); every fix ships with a named
regression test verifiable via section 5.

## Defects found and fixed during this evaluation

The pre-evaluation code had real defects that would have undermined the
release; all were fixed and covered by tests before the live runs above:

- `previous_response_id` was used as a session scope key — it changes every
  turn, fragmenting Responses-API clients into one-turn silos with no
  cross-turn memory. Now resolved through a response-id→scope map, bound to
  the producing API key (cross-key presentation denied), FIFO-bounded.
- Stream buffers were keyed only by scope: concurrent streams in one session
  interleaved; retries of aborted identical requests inherited stale text;
  streams without `[DONE]` leaked. Now keyed per request body, reset on
  stream init, flushed per stream, oldest-first evicted at 256 entries.
- `[DONE]`/terminator detection missed multi-line chunks and non-OpenAI
  terminators (Claude `message_stop`, Responses `response.completed|failed|
  incomplete`); a final text delta coalesced with the terminator was dropped.
- Injected memory could contain the same snippet multiple times; recalled
  text identical to (or a near-verbatim substring of) the current query was
  injected as "memory". Both now filtered.
- Reasoning text was durable-ingested when `ingest_reasoning=false` if
  `ingest_assistant=true`.
- `auto_consolidate` ran `Consolidate()` on **every request**. It now
  consolidates when the engine asks for it — the `consolidation_state` hint
  (`"none" | "recommended" | "required"`, cortext ≥1.2.2, parsed with the
  same legacy-boolean fallback as the reference cortext.ts binding) — with an
  every-N-durable-writes fallback (`consolidate_every: 25`) for engines that
  never emit the hint, plus once at shutdown and engine eviction. Attempts
  are throttled to ≤1 per `consolidate_every/5` writes per scope, so a
  persistent hint (e.g. a failing consolidate that never clears the engine
  backlog) cannot storm the request path — under default knobs the cadence
  usually fires first and the hint is the escalation path. The native
  engine wrapper also gained a mutex + closed guard: an in-flight
  `ProcessText`/`Consolidate` can no longer race `Close` during LRU eviction
  or shutdown.

## Limits (honest)

- **Retrieval quality is Cortext's, not this plugin's.** These suites prove
  the plumbing (ingest, scope, recall, inject, isolation, durability) and
  that injected memory changes answers for the better on seeded facts. They
  do not measure ranking quality on long, messy real conversations.
- Session identity is only as good as client headers. Clients that send no
  session header and no chain pointer share the per-API-key bucket.
  Unmapped/expired `previous_response_id` chains fall back to the tenant
  bucket (conversations blend); this is counted (`chain_fallback`) but not
  prevented without host-side session ids.
- No mid-turn revise: the interrupt gate stages memory for the *next*
  request only (proxy limitation, by design).
- Live-run artifacts (sections 1-4) are operator-run results: outputs are
  gitignored under `bench/**/out*` so no reviewer can falsify the numbers
  from the checkout alone — only the reproduce commands above regenerate
  them against a live CPA. Sections 5-6 are verifiable in-repo.
- Review residuals (non-blocking, documented in code): memories already
  visible verbatim in the outbound window are not re-injected, but
  near-verbatim (not exact-substring) overlap can still double-state; on
  the pinned host (CPA v7.2.96) the response intercept fires only for
  NON-streamed requests, so the stream→response dedupe machinery is for
  hosts that do fire it — actual recovery of a failed/conflict-poisoned
  stream turn is next-turn history resubmit, and store=true Responses-API
  chaining clients (which resend no transcript) can permanently lose such a
  turn (bounded, `stream_conflict`/`process_fail` counted); the
  stream-scope cache is not invalidated on reconfigure/shutdown (stream
  deltas route to the pre-reconfigure scope until process restart);
  identical-stream poisoning engages only when the host sends stream-init
  calls, and a poisoned key decays after 60s; stream ingest assumes SSE
  chunks contain whole lines (verified: `bufio.Scanner` in v7.2.96);
  `stripMemoryBlock` removes any block carrying the plugin's fixed header
  line, including one a client pasted themselves; `contentToText` fails
  closed only on reasoning/thinking type NAMES (a provider emitting CoT
  under a novel part-type name would still be ingested); Claude/Gemini
  non-stream tool calls are not rendered (history resubmit recovers them);
  neutralize over-strips prose that literally discusses `<system>`-style
  tags or `<user guide>`; `interactions`-format traffic passes through
  untouched (no adapter); pre-auth traffic can create scope files for
  self-asserted identities (open handles LRU-bounded at 64, files not —
  size data_dir accordingly); `data_dir` is always chmod 0700 on open;
  stream durable ingest is synchronous per flushed segment; double-encoded
  HTML entities and full-width letters in fence names are not normalized
  (no literal ASCII fence can result); `ingest_reasoning` defaults to false
  (CoT is not persisted unless opted in); `recall_limit: 0` disables
  injection; `stdJSONMarshal` is unused dead code kept for symmetry.
- All live runs used `kimi-k2.7-code` as subject and `gpt-5.4-mini` as
  judge via this CPA. Other providers/models route through the same
  interceptors, but were not separately scored.

## Reproduce the whole evaluation

```bash
cd plugin && go test ./... -count=1 && go test -race ./... -count=1
make build-native && make test-native
make install-native PLUGINS_DIR=~/.cli-proxy-api/plugins
brew services restart cliproxyapi   # or your CPA supervisor
cd bench/abi_live && go run . -plugin ../../bin/cortext.dylib -out /tmp/cpa-live
make e2e-blackbox CPA_BASE=http://127.0.0.1:8317 CPA_MODEL=kimi-k2.7-code
python3 bench/e2e_blackbox/gaps.py --base http://127.0.0.1:8317 --model kimi-k2.7-code --out bench/e2e_blackbox/gaps_out
python3 bench/judge_eval/live_ab.py --base http://127.0.0.1:8317/v1 --key <cpa-key> --out bench/judge_eval/out
```
