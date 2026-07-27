# cortext-cpa-plugin

A [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) plugin that adds
[Cortext](https://github.com/augmem/cortext) memory to proxied LLM traffic.
It implements CPA's three interceptor hooks:

- `request.intercept_before` — ingest new turns, recall relevant memory, inject
  it into the system/instructions field as a fenced `<cortext_memory>` block.
- `response.intercept_after` — ingest the assistant's reply.
- `response.intercept_stream_chunk` — same for streamed replies, plus an
  optional interrupt gate that stages recall for the next request (a plain
  HTTP proxy can't revise mid-generation).

Because these hooks run for every client protocol CPA serves, one `.so`/`.dylib`
covers OpenAI, Claude, Gemini, and Responses-API clients without touching CPA
itself. Sibling projects:
[cortext-openclaw-plugin](https://github.com/augmem/cortext-openclaw-plugin),
[cortext-hermes-plugin](https://github.com/augmem/cortext-hermes-plugin).

## Build

Requires Go 1.26+; CGO only for `-buildmode=c-shared`.

```bash
make test          # unit tests (in-process stub engine)
make build-stub    # bin/cortext-stub.* — CI/protocol use, not real retrieval

make build-native  # bin/cortext.* — production, real Cortext engine
make test-native   # native-tagged tests (downloads release assets on first open)
```

The production build links [`github.com/augmem/cortext.go`](https://github.com/augmem/cortext.go),
which downloads the release natives + model assets on first engine open. For
offline installs set `CORTEXT_ASSETS_DIR` (see [PROVENANCE.md](./PROVENANCE.md)).

`plugin/go.mod` has no `replace` directives; a clean clone builds from the
module proxy. For local development against sibling checkouts,
`cp go.work.example go.work` (gitignored).

## Install

```bash
make build-native
make install-native PLUGINS_DIR=/path/to/CLIProxyAPI/plugins
```

`make install-stub` exists for protocol experiments and installs under a
separate `cortext-stub` basename. Plain `make install` refuses to run so the
stub can't end up in production by accident. Then configure CPA (see
[config.example.yaml](./config.example.yaml)):

```yaml
plugins:
  enabled: true
  dir: "plugins"
  configs:
    cortext:
      enabled: true
      memory_scope: session
      data_dir: "~/.cli-proxy-api/cortext"
      recall_limit: 12
      # ingest_reasoning: true  # opt-in; chain-of-thought is not persisted by default
      auto_consolidate: true
      consolidate_every: 25
```

The plugin id is the library basename (`cortext`). Registration reports
`Version: 0.1.0+native` (or `+stub`) so you can tell which engine is linked.

## Isolation

One SQLite store per scope. With `memory_scope: session` (default), the scope
key is `s-<apiKeyHash>-<sessionHash>` when the client presents an API key, so
the same `conversation_id` from two tenants gets two stores. Unkeyed traffic
uses `s-anon-*` buckets; requests with no session, agent, or API-key identity
get no durable store at all. `previous_response_id` chains resolve to the
scope that produced the referenced response, and only for the same API key.
`memory_scope: agent` keys on the agent header instead. `memory_scope: global`
is one shared store — single-user deployments only.

Send a session header when you can:

```http
X-Cortext-Session: my-conversation-id
```

The host must forward client `Authorization` / `x-api-key` and session headers
in intercept metadata. If CPA strips auth before the plugin, tenants collapse
into the unkeyed `s-anon-*` buckets.

## Client formats

- `openai` — `messages[]`, inject into the first system message
- `openai-response` / `codex` — `instructions` + `input`, inject into `instructions`
- `claude` — `system` + `messages[]`, inject into `system`
- `gemini` / `antigravity` — `systemInstruction` + `contents`, inject into
  `systemInstruction.parts` (`antigravity` is defensive; no client route emits
  it in the pinned CPA v7.2.96)

`interactions` is deliberately not handled: CPA's interactions wire shapes
(`system_instruction`, `steps[]` responses, `event_type` stream events) match
no adapter here, so that traffic passes through untouched instead of being
half-rewritten. Upstream provider choice doesn't matter — memory is applied to
the client body before translation.

## Bench

`bench/` has the live harnesses and [VERDICT.md](./bench/VERDICT.md), the
scrutiny entrypoint: what was tested, what passed, what's still unproven.
Live outputs are gitignored (they embed operator paths); regenerate with the
commands in VERDICT.md.

```bash
# ABI smoke, no provider needed — loads the real cliproxy_plugin_init
cd bench/abi_live && go run . -plugin ../../bin/cortext.dylib -out /tmp/cpa-live

# Blackbox e2e against a running CPA with the native plugin installed
make e2e-blackbox CPA_BASE=http://127.0.0.1:8317 CPA_MODEL=kimi-k2.7-code

# LLM-judge A/B (needs JUDGE_API_KEY or OPENAI_API_KEY)
python3 bench/judge_eval/live_ab.py --base http://127.0.0.1:8317/v1 --key <cpa-key> --out bench/judge_eval/out
```

## Layout

```text
plugin/           Go c-shared plugin (package main)
  main.go         C ABI + method dispatch
  intercept.go    request / response / stream handlers
  formats.go      per-format extract/inject/window
  store.go        per-scope engines, dedupe, tenant isolation
  engine_*.go     stub (default) or cortext_native
  memory.go       fence, neutralize, formatting
  metrics.go      fail-open counters
bench/            live harnesses + VERDICT.md
```

## Operational notes

- Changing `data_dir`, `memory_scope`, engine knobs, or the identity headers
  disposes open engines; the `previous_response_id` map is process-local and
  is cleared on material reconfigure and restart. Clients should send an
  explicit session header after a restart.
- Every interceptor failure is fail-open: traffic passes through without
  memory and a counter increments (`open_fail`, `process_fail`, `inject_fail`,
  `skip_no_identity`, `scope_evict`, `stream_evict`, `stream_conflict`,
  `chain_fallback`, `consolidate_fail`). Watch the logs.
- `data_dir` is forced to mode `0700`.

## Limitations

- No mid-turn revise: the interrupt gate can only stage memory for the next
  request.
- Session identity is only as good as the headers the client sends. Unkeyed
  hosts with guessable session ids can collide under `s-anon-*`; multi-tenant
  proxies must present distinct API keys.
- `window_messages` is outbound windowing, not transcript compaction on the
  host. Gemini-family bodies are not windowed (logged once per process).
- Stream ingest assumes the host sends a stream-init call and whole-line SSE
  chunks. Both hold for CPA v7.2.96; check before pinning a different version.
- First native open can download release assets for minutes. It happens at
  plugin registration in a background goroutine, but the first request for a
  scope may still wait on a cold install. Pre-provision `CORTEXT_ASSETS_DIR`
  for cold-start-sensitive deploys.
- `memory_scope: global` is single-user only.
- Retrieval quality is the Cortext engine's, not this plugin's. The benches
  prove plumbing (ingest, recall, isolation, durability), not ranking quality
  on long real conversations.

## Related

- [augmem/cortext](https://github.com/augmem/cortext) — engine + release assets
- [augmem/cortext.go](https://github.com/augmem/cortext.go) — Go binding used by `build-native`
- [router-for-me/CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI)

## License

Apache-2.0. See [LICENSE](./LICENSE) and [NOTICE](./NOTICE).
