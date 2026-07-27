# LLM-as-judge evaluation for cortext-cpa-plugin

## Preferred: live CPA HTTP path

With a running CLIProxyAPI instance loading `plugins/cortext.*` and a mock/real upstream,
seed facts through the proxy, capture `<cortext_memory>` inject on the upstream request,
and grade memory vs no-memory answers with an LLM judge.

Evidence shape (goal scratch):

- `eval/summary.json` — `path: live-cpa-http`, scores, judge_model
- `eval/items.jsonl` — per-item scores + rationales
- `eval/transcripts/{memory,no_memory}/`

Requires: `OPENAI_API_KEY` or `XAI_API_KEY` or `JUDGE_API_KEY`.

## Offline helper (`go run .`)

`go run . -plugin ../../bin/cortext.dylib -out ./out` is a **protocol-level** helper
that grades synthetic memory-context vs empty answers. It does **not** replace the
live CPA path for release proof. Use the live HTTP harness for plan AC4.
