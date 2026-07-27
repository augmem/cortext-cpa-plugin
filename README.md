# Cortext for CLIProxyAPI

[![license](https://img.shields.io/badge/license-Apache--2.0-blue)](./LICENSE)

Living memory for [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) —
the same Cortext loop as
[`@augmem/cortext-openclaw-plugin`](https://github.com/augmem/cortext-openclaw-plugin)
and [`cortext-hermes-plugin`](https://github.com/augmem/cortext-hermes-plugin),
wired through CPA’s **plugin interceptors** so every client protocol and every
upstream provider gets memory without forking the proxy.

```text
client (OpenAI / Claude / Gemini / Responses / …)
        │
        ▼
  CLIProxyAPI  ── request interceptor ──► durable ingest + live recall inject
        │
        ▼
  upstream provider (Kimi / Claude / Codex / Gemini / xAI / …)
        │
        ▼
  response / stream interceptors ──► durable assistant (+ optional gate)
```

## Why a plugin (not a fork)

CLIProxyAPI already exposes:

| Capability | Method | Cortext use |
|---|---|---|
| `request_interceptor` | `request.intercept_before` | Ingest new messages, recall, inject `<cortext_memory>` |
| `response_interceptor` | `response.intercept_after` | Durable-ingest assistant text |
| `response_stream_interceptor` | `response.intercept_stream_chunk` | Stream ingest + interrupt staging |

Those hooks run on the shared `ExecuteWithAuthManager` path for **all** entry
protocols. Provider executors and translators stay untouched, so you can keep
`CLIProxyAPI` on stock upstream and only drop this `.so` / `.dylib` into
`plugins/`.

## The loop

1. **Request (before auth)** — extract messages from the *client* format,
   durable-ingest unseen turns (deduped; clients resend full history), run
   ephemeral recall on the latest user text, inject a fenced memory block into
   system / instructions, optionally window the outbound transcript.
2. **Response / stream** — durable-ingest assistant (and optional reasoning)
   text.
3. **Interrupt gate** — on stream segments, ephemeral recall may stage memory
   for the **next** request. A pure HTTP proxy cannot revise mid-generation the
   way OpenClaw’s `before_agent_finalize` can.

## Supported client formats

| `SourceFormat` | Extract | Inject |
|---|---|---|
| `openai` | `messages[]` | system message |
| `openai-response` / `codex` | `instructions` + `input` | `instructions` |
| `claude` | `system` + `messages[]` | `system` |
| `gemini` / `antigravity` | `systemInstruction` + `contents` | `systemInstruction.parts` |

(`antigravity` is defensive: no client route emits it in the pinned CPA
v7.2.96; the Gemini-family handling covers it if a future host does.
`interactions` is deliberately NOT handled: CPA's interactions wire shapes —
`system_instruction`, `steps[]` responses, `event_type` stream events — do
not match any adapter, so interactions traffic passes through untouched
rather than being half-rewritten.)

Upstream provider choice is irrelevant: memory is applied on the client body
before translation.

## Session isolation (multi-tenant)

One SQLite file per scope key:

| `memory_scope` | Key |
|---|---|
| `session` (default) | Explicit session (`X-Cortext-Session` / `X-Session-Id` / body `conversation_id`) is **namespaced by API-key hash** when an API key is present: `s-<keyHash>-<session>`. Same `conversation_id` under two keys ⇒ two stores. Unkeyed hosts use `s-anon-<session>`. No session → agent/API-key bucket. **Identity-less** (no session, agent, or API key) does **not** open a durable store. `previous_response_id` chains resolve to the scope that produced that response (bound to the same API key). |
| `agent` | `X-Cortext-Agent` namespaced by API key when present; key-only bucket if no agent header. |
| `global` | single shared store (**single-user only** — leaks across tenants on a multi-tenant proxy) |

Send a stable session header from your client when you can:

```http
X-Cortext-Session: my-conversation-id
```

## Build

Requirements: Go 1.26+ (see `plugin/go.mod`), CGO (for `-buildmode=c-shared` only).

The committed `plugin/go.mod` resolves **published** modules only (no sibling
`replace` directives). `plugin/go.sum` carries checksums for
`github.com/augmem/cortext.go` and `github.com/router-for-me/CLIProxyAPI/v7`.

```bash
# Protocol + stub engine (no native assets) — unit tests / CPA wiring only
make test
make build-stub
# → bin/cortext-stub.dylib  (macOS) or bin/cortext-stub.so (Linux)
# engine=stub — NOT production Cortext quality

# Production engine: github.com/augmem/cortext.go (pure-Go; downloads
# release natives + AIST on first open unless CORTEXT_ASSETS_DIR is set)
make build-native
make test-native
# → bin/cortext.dylib / bin/cortext.so  (production basename)
# registration Version: 0.1.0+native
```

### Local development (optional sibling checkouts)

```bash
cp go.work.example go.work   # gitignored; points at ../cortext-proxy and ../cortext.go
```

## Install into CLIProxyAPI

```bash
# Production (documented default)
make build-native
# optional offline assets (skip download on first open):
#   export CORTEXT_ASSETS_DIR=/path/to/unpacked/cortext-assets-1.2.4
make install-native PLUGINS_DIR=/path/to/CLIProxyAPI/plugins
# → plugins/cortext.dylib  (or .so) with engine=native

# Stub for protocol experiments only (different basename)
make build-stub
make install-stub PLUGINS_DIR=/path/to/CLIProxyAPI/plugins
# → plugins/cortext-stub.* — does not replace production cortext.*
```

`make install` without a suffix **exits with an error** so the stub cannot be
accidentally shipped under the production plugin id.

Provenance notes for native artifacts: [`PROVENANCE.md`](./PROVENANCE.md).

### Live ABI smoke (no upstream provider required)

Loads the real `cliproxy_plugin_init` entrypoint the same way CPA does:

```bash
# Protocol-only ABI against the stub artifact
make build-stub
cd bench/abi_live && go run . -plugin ../../bin/cortext-stub.dylib -out /tmp/cpa-live-stub

# Release-grade ABI against the native production artifact
make build-native
cd bench/abi_live && go run . -plugin ../../bin/cortext.dylib -out /tmp/cpa-live
# summary.json: multi-format inject, isolation, durability
```

In `config.yaml` (see [`config.example.yaml`](./config.example.yaml)):

```yaml
plugins:
  enabled: true
  dir: "plugins"
  configs:
    cortext:
      enabled: true
      memory_scope: session
      data_dir: "~/.cli-proxy-api/cortext"
      focus: 0.45
      stability: 0.5
      recall_limit: 12
      # CoT is not persisted unless opted in (default: false).
      # ingest_reasoning: true
      # Consolidate when the engine asks (consolidation_state hint), with an
      # every-N-writes fallback; attempts throttled to ≤1 per N/5 writes.
      auto_consolidate: true
      consolidate_every: 25
```

Plugin id is the library basename (`cortext`), matching `plugins.configs.cortext`.
Registration reports `Version: 0.1.0+native` (or `+stub`) so the linked engine
is visible at the host surface.

### Operator contracts

- **Config changes** to `data_dir`, `memory_scope`, engine knobs
  (`focus` / `sensitivity` / `stability`), or the identity headers dispose
  open engines so the next request reopens with the new settings. The
  in-memory `previous_response_id`
  map is process-local and is cleared on material reconfigure and process
  restart — clients should send an explicit session header after restart.
- **Fail-open**: interceptor errors pass the request/response through without
  memory so the proxy stays available. Counters
  (`open_fail`, `process_fail`, `inject_fail`, `skip_no_identity`,
  `scope_evict`, `stream_evict`, `stream_conflict`, `chain_fallback`,
  `consolidate_fail`) are logged with running
  totals; watch logs for growth.
- **data_dir** is created mode `0700`. Place it on a private volume.

### LLM-as-judge eval

```bash
# Requires JUDGE_API_KEY or OPENAI_API_KEY (or XAI_API_KEY)
cd bench/judge_eval && go run . -plugin ../../bin/cortext.dylib -out ./out
# writes summary.json; exits 2 with judges_blocked.txt if credentials missing
```

### Blackbox e2e (live CPA + real providers)

Load the **native** plugin into a **running** CPA, then:

```bash
make build-native
mkdir -p ~/.cli-proxy-api/plugins ~/.cli-proxy-api/cortext
cp bin/cortext.dylib ~/.cli-proxy-api/plugins/
# enable plugins.configs.cortext in /opt/homebrew/etc/cliproxyapi.conf (or your config)
brew services restart cliproxyapi

make e2e-blackbox CPA_BASE=http://127.0.0.1:8317 CPA_MODEL=kimi-k2.7-code
# → bench/e2e_blackbox/out/summary.json  (plugin store + recall + isolation)
```

Optional drivers (once the CLI is pointed at CPA): `--driver agy` / `--driver kimi`
(see [`bench/e2e_blackbox/README.md`](./bench/e2e_blackbox/README.md)).

## Layout

```text
cortext-cpa-plugin/
  plugin/           # Go c-shared plugin (package main)
    main.go         # C ABI + method dispatch
    intercept.go    # request / response / stream handlers
    formats.go      # OpenAI / Claude / Gemini / Responses adapters
    store.go        # per-scope engines + ingest dedupe + tenant isolation
    engine_*.go     # stub (default) or cortext_native
    memory.go       # fence, neutralize, format
    metrics.go      # fail-open counters
    config.go
  vendor/           # optional dropped-in libcortext (see vendor/README.md)
  config.example.yaml
  Makefile
```

## Engines

| Build tag | Artifact | Engine | When |
|---|---|---|---|
| *(default)* | `bin/cortext-stub.*` | In-process stub (keyword overlap) | Unit tests, ABI wiring, CI without models |
| `cortext_native` | `bin/cortext.*` | [`github.com/augmem/cortext.go`](https://github.com/augmem/cortext.go) (purego + release natives) | **Production** |

The stub is **not** a substitute for Cortext retrieval quality. Ship
`build-native` / `install-native` artifacts for real use. CI builds the stub
as `cortext-stub.*` and the native job as `cortext.*` so basenames cannot be
confused.

## Keeping CPA in sync with upstream

This repository does **not** patch CLIProxyAPI. Track upstream with a clean
clone; only your `config.yaml` and `plugins/cortext.*` are local. No provider
or translator forks.

## Evaluation

Release-grade evidence (live A/B, blackbox e2e, ABI smoke, review record):
[`bench/VERDICT.md`](./bench/VERDICT.md). Live run outputs under `bench/**/out/`
are gitignored (they may embed operator paths); regenerate with the commands
in VERDICT.md.

## Limits (honest)

- No mid-turn answer revise (proxy has no OpenClaw finalize hook).
- Session identity is only as good as headers / body fields the client sends.
  Unkeyed hosts that reuse guessable session ids can still collide under
  `s-anon-*`; multi-tenant proxies must present distinct API keys. CPA must
  forward client `Authorization` / `x-api-key` (and session headers) on
  request, response, and stream intercept metadata — if auth is stripped
  before the plugin, tenants collapse into unkeyed `s-anon-*` buckets.
- Compaction is optional outbound windowing, not host-transcript ownership.
- Native builds use `github.com/augmem/cortext.go`, which downloads release
  natives + AIST on first open unless `CORTEXT_ASSETS_DIR` or
  `CORTEXT_LIBRARY_PATH` is set (offline / air-gapped installs need those env
  vars — see PROVENANCE.md).
- `memory_scope: global` is single-user only.
- Stream ingest relies on two host contracts: CPA delivers a header-init
  interceptor call at stream start (used to detect conflicting byte-identical
  concurrent streams) and delivers SSE chunks containing whole lines (a host
  that splits a line across chunks would drop that delta). Both hold for
  CLIProxyAPI v7.2.96; verify before pinning a different host/version.

## Related

- [augmem/cortext](https://github.com/augmem/cortext) — native memory engine + release assets
- [augmem/cortext.go](https://github.com/augmem/cortext.go) — pure-Go binding used by `build-native`
- [augmem/cortext-openclaw-plugin](https://github.com/augmem/cortext-openclaw-plugin)
- [augmem/cortext-hermes-plugin](https://github.com/augmem/cortext-hermes-plugin)
- [router-for-me/CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI)

## License

Apache-2.0. See [LICENSE](./LICENSE) and [NOTICE](./NOTICE).
