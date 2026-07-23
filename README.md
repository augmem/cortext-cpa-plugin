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
| `gemini` / `antigravity` / `interactions` | `systemInstruction` + `contents` | `systemInstruction.parts` |

Upstream provider choice is irrelevant: memory is applied on the client body
before translation.

## Session isolation

One SQLite file per scope key:

| `memory_scope` | Key |
|---|---|
| `session` (default) | `X-Cortext-Session` / `X-Session-Id` / body `conversation_id` / `previous_response_id`, else hashed API key |
| `agent` | `X-Cortext-Agent` or API key |
| `global` | single shared store |

Send a stable session header from your client when you can:

```http
X-Cortext-Session: my-conversation-id
```

## Build

Requirements: Go 1.24+, CGO (for `-buildmode=c-shared`).

```bash
# Protocol + stub engine (no libcortext) — fine for unit tests and wiring CPA
make test
make build
# → bin/cortext.dylib  (macOS) or bin/cortext.so (Linux)

# Production engine (needs a local cortext tree with the Go binding)
export CORTEXT_ROOT=../cortext
# build libcortext first, e.g.:
#   (cd "$CORTEXT_ROOT" && cmake --preset ffi-release && cmake --build --preset ffi-release --target cortext)
make build-native
```

Default `go.mod` `replace` points at a sibling `../cortext-proxy` checkout for
the CPA `pluginapi` / `pluginabi` packages. Point it at a module version if you
prefer not to keep a local clone.

## Install into CLIProxyAPI

```bash
make install PLUGINS_DIR=/path/to/CLIProxyAPI/plugins
# or: make install-native PLUGINS_DIR=...
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
```

Plugin id is the library basename (`cortext`), matching `plugins.configs.cortext`.

## Layout

```text
cortext-cpa-plugin/
  plugin/           # Go c-shared plugin (package main)
    main.go         # C ABI + method dispatch
    intercept.go    # request / response / stream handlers
    formats.go      # OpenAI / Claude / Gemini / Responses adapters
    store.go        # per-scope engines + ingest dedupe
    engine_*.go     # stub (default) or cortext_native
    memory.go       # fence, neutralize, format
    config.go
  vendor/           # optional dropped-in libcortext (see vendor/README.md)
  config.example.yaml
  Makefile
```

## Engines

| Build tag | Engine | When |
|---|---|---|
| *(default)* | In-process stub (keyword overlap) | Unit tests, ABI wiring, CI without models |
| `cortext_native` | [`bindings/go`](https://github.com/augmem/cortext) over `libcortext` | Production |

The stub is **not** a substitute for Cortext retrieval quality. Ship
`build-native` artifacts for real use.

## Keeping CPA in sync with upstream

This repository does **not** patch CLIProxyAPI. Track upstream with a clean
clone; only your `config.yaml` and `plugins/cortext.*` are local. No provider
or translator forks.

## Limits (honest)

- No mid-turn answer revise (proxy has no OpenClaw finalize hook).
- Session identity is only as good as headers / body fields the client sends.
- Compaction is optional outbound windowing, not host-transcript ownership.
- Native builds need a built `libcortext` and encoder assets from Cortext.

## Related

- [augmem/cortext](https://github.com/augmem/cortext) — native memory engine
- [augmem/cortext-openclaw-plugin](https://github.com/augmem/cortext-openclaw-plugin)
- [augmem/cortext-hermes-plugin](https://github.com/augmem/cortext-hermes-plugin)
- [router-for-me/CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI)

## License

Apache-2.0. See [LICENSE](./LICENSE) and [NOTICE](./NOTICE).
