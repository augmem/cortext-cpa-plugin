# Blackbox e2e — memory input + update (live CPA)

Targets a **running** CLIProxyAPI with `plugins/cortext.*` loaded (native).

## Why earlier suites were insufficient

A green “seed → probe” can pass for the wrong reasons:

1. **User confounds assistant ingest** — if the user message already contains the
   needle, `user/ingest` alone can make next-turn recall succeed without proving
   `HandleResponse` / stream durable paths.
2. **LLM-only scoring** — no check that the session SQLite actually contains the
   token bytes.
3. **Weak correction** — requiring only that the *new* token appears does not
   prove update preference (old may still be treated as current).

This suite addresses those:

| Guard | Mechanism |
|---|---|
| No-history probes | Latest user message only on recall turns |
| Exact needles | Case-insensitive full-token match |
| Store-layer proof | Token bytes scanned in session `.sqlite` + WAL |
| Free invent | User specifies only prefix+length; model invents random 8-hex suffix (not in user text) |
| Path markers | Store must show token bytes **and** `assistant/response`, `assistant/stream`, or `reasoning/stream` |
| Exclusive correction | CURRENT answer must contain new and not old |
| History resubmit | Multi-turn body durable-ingests prior user+assistant; later probe has no history |

## `run.py` cases

| Case | What it proves |
|---|---|
| `store_created_after_ingest` | Session SQLite created |
| `user_input_token_in_store` | User needle bytes in durable store |
| `multi_fact_recall_*` / `multi_fact_both_in_store` | Two independent facts input + selective recall |
| `correction_prefers_new` / `correction_exclusive_current` | Update path prefers current value |
| `correction_both_tokens_in_store` | Update is not silent erase; both writes durable |
| `correction_chain_final_current` | A→B→C chain; final is current |
| `history_resubmit_*` | Prior turns in multi-message body are ingested + recallable without history |
| `assistant_invent_*` | Assistant-origin needle never typed by user; store + next-turn recall |
| `recall_after_distractors` / `secret_token_in_store` | Survival under noise |
| `isolation_*` | Other `X-Cortext-Session` does not leak |
| `late_turn_still_recalls_deploy` | Still recalls after many updates |

## `gaps.py` cases

Assistant invent, SSE stream invent, user input + exclusive update, ranking ≥24
(late-window bar; early residual recorded), brew restart durability + dylib map,
all with store-layer checks where applicable.

## Load plugin into brew CPA

```bash
make build-native
mkdir -p ~/.cli-proxy-api/plugins ~/.cli-proxy-api/cortext
cp bin/cortext.dylib ~/.cli-proxy-api/plugins/
# plugins.enabled + configs.cortext in /opt/homebrew/etc/cliproxyapi.conf
brew services restart cliproxyapi
```

## Run

```bash
python3 bench/e2e_blackbox/run.py \
  --base http://127.0.0.1:8317 \
  --model kimi-k2.7-code \
  --out ./bench/e2e_blackbox/out

python3 bench/e2e_blackbox/gaps.py \
  --base http://127.0.0.1:8317 \
  --model kimi-k2.7-code \
  --out ./bench/e2e_blackbox/gaps_out

# or
make e2e-blackbox
```
