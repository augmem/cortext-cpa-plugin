#!/usr/bin/env python3
"""
Blackbox memory suite against a *running* CLIProxyAPI with cortext loaded.

Hard requirements for "input" and "update" claims:
  - Probes send ONLY the latest user message (no transcript replay) so the model
    cannot answer from chat history — only via plugin inject.
  - Exact needle tokens required (case-insensitive).
  - Store-layer checks on the session SQLite (token bytes present) so pass is not
    only LLM theater.
  - Assistant-only invent: the full needle is NOT present in any user message.
  - Correction update: current value must be new; exclusive-current is preferred.

Default target: brew CPA http://127.0.0.1:8317 with real providers.
"""

from __future__ import annotations

import argparse
import json
import os
import re
import sys
import time
import urllib.error
import urllib.request
import uuid
from pathlib import Path


def http_json(url: str, body: dict | None = None, headers: dict | None = None, timeout: float = 180.0) -> dict:
    data = None if body is None else json.dumps(body).encode()
    req = urllib.request.Request(
        url,
        data=data,
        headers=headers or {},
        method="POST" if data is not None else "GET",
    )
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            raw = resp.read().decode()
            return json.loads(raw) if raw else {}
    except urllib.error.HTTPError as e:
        detail = e.read().decode(errors="replace")
        raise RuntimeError(f"HTTP {e.code}: {detail[:800]}") from e


def chat(
    base: str,
    model: str,
    session: str,
    content: str,
    *,
    history: list[dict] | None = None,
    max_tokens: int = 256,
) -> str:
    """Send a chat completion. Default: single user message (no history)."""
    headers = {
        "Content-Type": "application/json",
        "X-Cortext-Session": session,
    }
    key = os.environ.get("CPA_API_KEY") or os.environ.get("OPENAI_API_KEY")
    if key:
        headers["Authorization"] = f"Bearer {key}"
    messages = list(history or [])
    messages.append({"role": "user", "content": content})
    out = http_json(
        base.rstrip("/") + "/v1/chat/completions",
        body={"model": model, "messages": messages, "max_tokens": max_tokens},
        headers=headers,
    )
    choice = (out.get("choices") or [{}])[0]
    msg = choice.get("message") or {}
    text = (msg.get("content") or "").strip()
    if not text:
        text = (msg.get("reasoning_content") or "").strip()
    return text


def session_sqlite(data_dir: Path, session: str) -> Path | None:
    safe = re.sub(r"[^a-zA-Z0-9._-]+", "_", session)[:80]
    if not data_dir.is_dir():
        return None
    for p in data_dir.iterdir():
        if safe in p.name and p.suffix == ".sqlite":
            return p
    return None


def store_blob(data_dir: Path, session: str) -> bytes:
    """Raw session DB + WAL for structural needle scans."""
    db = session_sqlite(data_dir, session)
    if not db or not db.exists():
        return b""
    blob = db.read_bytes()
    for suffix in ("-wal", "-shm"):
        side = Path(str(db) + suffix)
        if side.exists():
            blob += side.read_bytes()
    return blob


def store_has_token(data_dir: Path, session: str, token: str) -> bool:
    """True if exact token bytes appear in the durable session store."""
    if not token:
        return False
    blob = store_blob(data_dir, session)
    if not blob:
        return False
    raw = token.encode("utf-8", errors="ignore")
    return raw in blob or raw.lower() in blob.lower()


def store_has_source_marker(data_dir: Path, session: str, marker: str) -> bool:
    """source_id fragments like user/ingest or assistant/response appear as plaintext."""
    blob = store_blob(data_dir, session)
    return marker.encode() in blob if blob else False


def contains_token(text: str, token: str) -> bool:
    return token.lower() in (text or "").lower()


def primary_answer(text: str) -> str:
    """
    Prefer a final single-token line over long reasoning dumps.
    Models often restate old values while deliberating; the last token-like
    line is the actual answer we grade for exclusive-current updates.
    """
    lines = [ln.strip() for ln in (text or "").splitlines() if ln.strip()]
    if not lines:
        return (text or "").strip()
    for ln in reversed(lines):
        if re.fullmatch(r"[A-Za-z0-9_-]{8,48}", ln):
            return ln
    # Fallback: last 120 chars (often where the model parks the answer)
    return lines[-1] if len(lines[-1]) <= 120 else (text or "")[-120:]


def last_matching_token(text: str, tokens: list[str]) -> str | None:
    """Which candidate appears last in the full text (final answer preference)."""
    low = (text or "").lower()
    best, pos = None, -1
    for t in tokens:
        p = low.rfind(t.lower())
        if p > pos:
            best, pos = t, p
    return best


def strip_negated_mentions(text: str, tokens: list[str]) -> str:
    """
    Drop 'not TOKEN' / 'superseded TOKEN' so exclusive-current scoring is not
    fooled by correction sentences like 'actually NEW, not OLD'.
    """
    out = text or ""
    for tok in tokens:
        out = re.sub(rf"\bnot\s+{re.escape(tok)}\b", " ", out, flags=re.I)
        out = re.sub(
            rf"\b(?:superseded|previous|former|old|was)\s+{re.escape(tok)}\b",
            " ",
            out,
            flags=re.I,
        )
    return out


def exclusive_current_ok(text: str, current: str, *superseded: str) -> bool:
    """True when the graded answer treats `current` as the live value."""
    cands = [current, *superseded]
    pa = primary_answer(text)
    if contains_token(pa, current) and not any(contains_token(pa, s) for s in superseded):
        return True
    cleaned = strip_negated_mentions(text, cands)
    if last_matching_token(cleaned, cands) == current:
        return True
    # Positive framing near the end of the reply.
    tail = (text or "")[-240:]
    tail = strip_negated_mentions(tail, cands)
    if last_matching_token(tail, cands) == current:
        return True
    if re.search(
        rf"(?:actually|now|current(?:ly)?|is)\s*[`'\"]?{re.escape(current)}",
        text or "",
        re.I,
    ):
        return True
    return False


def extract_invented(text: str, prefix: str) -> str | None:
    """Pull first prefix + 8–16 alnum token invented by the model."""
    pat = re.compile(rf"\b({re.escape(prefix)}[a-z0-9]{{8,16}})\b", re.I)
    m = pat.search(text or "")
    return m.group(1) if m else None


def free_invent_prompt(prefix: str, n: int = 8) -> str:
    """
    Unconfounded assistant-input prompt.

    User text specifies only the prefix + length pattern. The model invents the
    random suffix; the full contiguous needle is not determined by user text, so
    next-turn recall cannot reassemble from a durable user recipe.
    """
    return (
        f"Invent a one-time laboratory passphrase. "
        f"Format: the literal prefix {prefix} followed by exactly {n} lowercase "
        f"hexadecimal digits (0-9, a-f) that you invent at random. "
        f"Do not use all zeros, sequential digits, or the word deadbeef. "
        f"Reply with ONLY the finished contiguous passphrase on one line and nothing else."
    )


def case(name: str, ok: bool, **detail) -> dict:
    return {"name": name, "ok": ok, **detail}


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--base", default=os.environ.get("CPA_BASE", "http://127.0.0.1:8317"))
    ap.add_argument("--model", default=os.environ.get("CPA_MODEL", "kimi-k2.7-code"))
    ap.add_argument(
        "--data-dir",
        default=os.environ.get("CORTEXT_DATA_DIR", str(Path.home() / ".cli-proxy-api" / "cortext")),
    )
    ap.add_argument("--out", default="")
    ap.add_argument("--pause", type=float, default=0.4, help="seconds between turns")
    args = ap.parse_args()

    out_dir = Path(args.out) if args.out else Path.cwd() / "out"
    out_dir.mkdir(parents=True, exist_ok=True)
    data_dir = Path(args.data_dir).expanduser()
    data_dir.mkdir(parents=True, exist_ok=True)

    uid = uuid.uuid4().hex[:8]
    sess = f"suite-{uid}"
    sess_b = f"suite-b-{uid}"
    sess_hist = f"suite-hist-{uid}"
    sess_asst = f"suite-asst-{uid}"
    # Unique tokens unlikely to appear in model priors
    t_deploy = f"deploy-{uuid.uuid4().hex[:10]}"
    t_host = f"host-{uuid.uuid4().hex[:10]}"
    t_old = f"oldname-{uuid.uuid4().hex[:10]}"
    t_new = f"newname-{uuid.uuid4().hex[:10]}"
    t_final = f"finalname-{uuid.uuid4().hex[:10]}"
    t_secret = f"secret-{uuid.uuid4().hex[:10]}"
    t_hist_u = f"histu-{uuid.uuid4().hex[:10]}"
    t_hist_a = f"hista-{uuid.uuid4().hex[:10]}"

    report: dict = {
        "base": args.base,
        "model": args.model,
        "session": sess,
        "session_b": sess_b,
        "data_dir": str(data_dir),
        "tokens": {
            "deploy": t_deploy,
            "host": t_host,
            "old": t_old,
            "new": t_new,
            "final": t_final,
            "secret": t_secret,
            "hist_user": t_hist_u,
            "hist_asst": t_hist_a,
        },
        "cases": [],
        "ok": False,
        "design": {
            "no_history_probes": True,
            "exact_needle_tokens": True,
            "store_layer_token_scan": True,
            "assistant_invent_unconfounded": True,
            "correction_exclusive_current": True,
        },
    }

    def turn(session: str, content: str, max_tokens: int = 256, history: list[dict] | None = None) -> str:
        time.sleep(args.pause)
        return chat(args.base, args.model, session, content, history=history, max_tokens=max_tokens)

    # Health
    try:
        models = http_json(args.base.rstrip("/") + "/v1/models")
        report["models_ok"] = bool(models.get("data"))
    except Exception as e:
        report["error"] = f"CPA unreachable: {e}"
        (out_dir / "summary.json").write_text(json.dumps(report, indent=2))
        print(json.dumps(report, indent=2))
        return 2

    try:
        # --- I1: user durable input (store-layer, not LLM) ---
        turn(sess, f"Deployment rule for project {uid}: only ship from branch {t_deploy}, never main.")
        db1 = session_sqlite(data_dir, sess)
        size1 = db1.stat().st_size if db1 and db1.exists() else 0
        report["cases"].append(
            case(
                "store_created_after_ingest",
                db1 is not None and size1 > 0,
                path=str(db1) if db1 else None,
                size=size1,
            )
        )
        report["cases"].append(
            case(
                "user_input_token_in_store",
                store_has_token(data_dir, sess, t_deploy),
                note="session sqlite/WAL must contain deploy token bytes after user turn",
                has_user_ingest_marker=store_has_source_marker(data_dir, sess, "user/ingest"),
            )
        )

        # --- I2: multi-fact user input + selective recall (no history on probes) ---
        turn(sess, f"Staging endpoint for project {uid} is {t_host}.internal")
        r_deploy = turn(
            sess,
            f"For project {uid}, which branch do we deploy from? Reply with only the branch token "
            f"(it looks like deploy-…).",
            max_tokens=128,
        )
        r_host = turn(
            sess,
            f"For project {uid}, what is the FULL staging endpoint host token? "
            f"Reply with only the complete host-… token including every hex character after host-. "
            f"Do not abbreviate.",
            max_tokens=128,
        )
        if not contains_token(r_host, t_host):
            # One retry — models sometimes truncate long tokens on first ask.
            r_host = turn(
                sess,
                f"Repeat the complete host token for project {uid}. "
                f"It starts with host- and has 10 hex digits. Output the entire token only.",
                max_tokens=128,
            )
        report["cases"].append(
            case("multi_fact_recall_deploy", contains_token(r_deploy, t_deploy), reply=r_deploy[:400])
        )
        report["cases"].append(
            case("multi_fact_recall_host", contains_token(r_host, t_host), reply=r_host[:400])
        )
        report["cases"].append(
            case(
                "multi_fact_not_confused",
                contains_token(r_deploy, t_deploy) and contains_token(r_host, t_host),
                deploy_has_host=contains_token(r_deploy, t_host),
                host_has_deploy=contains_token(r_host, t_deploy),
            )
        )
        report["cases"].append(
            case(
                "multi_fact_both_in_store",
                store_has_token(data_dir, sess, t_deploy) and store_has_token(data_dir, sess, t_host),
            )
        )

        # --- U1: correction / update — exclusive current preferred ---
        turn(sess, f"The service pet name for project {uid} is {t_old}.")
        turn(sess, f"Correction for project {uid}: the service pet name is actually {t_new}, not {t_old}.")
        r_pet = turn(
            sess,
            f"What is the CURRENT service pet name for project {uid}? "
            f"Reply with only the single current token (newname-… or oldname-…). "
            f"Do not mention any superseded name.",
            max_tokens=128,
        )
        ans_pet = primary_answer(r_pet)
        has_new = contains_token(r_pet, t_new)
        has_old = contains_token(r_pet, t_old)
        # Prefer new anywhere in full text (recall worked).
        correction_ok = has_new
        exclusive = exclusive_current_ok(r_pet, t_new, t_old)
        report["cases"].append(
            case(
                "correction_prefers_new",
                correction_ok,
                reply=r_pet[:400],
                primary=ans_pet[:200],
                has_new=has_new,
                has_old=has_old,
                exclusive_current=exclusive,
            )
        )
        report["cases"].append(
            case(
                "correction_exclusive_current",
                exclusive,
                reply=r_pet[:400],
                primary=ans_pet[:200],
                note="exclusive: new is current; ignores 'not OLD' restatements",
                has_new=has_new,
                has_old=has_old,
            )
        )
        report["cases"].append(
            case(
                "correction_both_tokens_in_store",
                store_has_token(data_dir, sess, t_old) and store_has_token(data_dir, sess, t_new),
                note="update is not erase-only; both writes durable, ranking prefers new",
            )
        )

        # --- U2: chained corrections A→B→C; only C is current ---
        turn(sess, f"Codename for project {uid} is {t_old}.")
        turn(sess, f"Update codename for project {uid} to {t_new}.")
        turn(sess, f"Final codename update for project {uid}: now {t_final} (replaces prior).")
        r_code = turn(
            sess,
            f"What is the CURRENT codename for project {uid}? "
            f"Reply with only the finalname-… / newname-… / oldname-… token that is CURRENT. "
            f"Do not list superseded names.",
            max_tokens=128,
        )
        ans_code = primary_answer(r_code)
        chain_ok = exclusive_current_ok(r_code, t_final, t_old, t_new)
        report["cases"].append(
            case(
                "correction_chain_final_current",
                chain_ok,
                reply=r_code[:400],
                primary=ans_code[:200],
                has_final=contains_token(r_code, t_final),
                has_new=contains_token(r_code, t_new),
                has_old=contains_token(r_code, t_old),
            )
        )
        report["cases"].append(
            case(
                "correction_chain_final_in_store",
                store_has_token(data_dir, sess, t_final),
            )
        )

        # --- I3: history resubmit input (prior turns durable) then no-history probe ---
        # First request carries multi-turn body so plugin durable-ingests prior user+assistant.
        hist = [
            {"role": "user", "content": f"History note for project {uid}: alpha token is {t_hist_u}."},
            {
                "role": "assistant",
                "content": f"Acknowledged alpha. Also recording beta side-note {t_hist_a} for project {uid}.",
            },
        ]
        # Latest user is unrelated; prior turns still get durableIngest.
        turn(
            sess_hist,
            f"Continue setup for project {uid}; acknowledge history briefly.",
            history=hist,
            max_tokens=64,
        )
        report["cases"].append(
            case(
                "history_resubmit_user_token_in_store",
                store_has_token(data_dir, sess_hist, t_hist_u),
                note="prior user message in multi-turn body must durable-ingest",
            )
        )
        report["cases"].append(
            case(
                "history_resubmit_assistant_token_in_store",
                store_has_token(data_dir, sess_hist, t_hist_a),
                note="prior assistant message in multi-turn body must durable-ingest",
            )
        )
        # No-history probes — inject-only path
        r_hu = turn(
            sess_hist,
            f"What is the alpha token for project {uid}? Reply with only the complete histu-… token "
            f"(all hex digits). Do not abbreviate.",
            max_tokens=128,
        )
        if not contains_token(r_hu, t_hist_u):
            r_hu = turn(
                sess_hist,
                f"Repeat the full alpha histu- token for project {uid}. Complete token only.",
                max_tokens=128,
            )
        r_ha = turn(
            sess_hist,
            f"What is the beta side-note token for project {uid}? Reply with only the complete hista-… token "
            f"(all hex digits). Do not abbreviate.",
            max_tokens=128,
        )
        if not contains_token(r_ha, t_hist_a):
            r_ha = turn(
                sess_hist,
                f"Repeat the full beta hista- token for project {uid}. Complete token only.",
                max_tokens=128,
            )
        report["cases"].append(
            case("history_resubmit_recall_user_token", contains_token(r_hu, t_hist_u), reply=r_hu[:400])
        )
        report["cases"].append(
            case("history_resubmit_recall_asst_token", contains_token(r_ha, t_hist_a), reply=r_ha[:400])
        )

        # --- I4: free assistant invent (suffix NOT in user text at all) ---
        invent_prompt = free_invent_prompt("labpass-", n=8)
        invent = turn(sess_asst, invent_prompt, max_tokens=256)
        invented = extract_invented(invent, "labpass-")
        if invented is None:
            invent = turn(
                sess_asst,
                invent_prompt + " Output exactly one labpass-######## token (8 hex digits).",
                max_tokens=256,
            )
            invented = extract_invented(invent, "labpass-")
        # Reject trivial/placeholder inventions that could be guessed from the prompt text.
        banned = {"labpass-00000000", "labpass-deadbeef", "labpass-12345678", "labpass-ffffffff"}
        confounded = bool(invented) and invented.lower() in invent_prompt.lower()
        trivial = bool(invented) and invented.lower() in banned
        emitted = bool(invented) and not confounded and not trivial
        report["cases"].append(
            case(
                "assistant_invented_needle_emitted",
                emitted,
                invent_reply=invent[:300],
                invented=invented,
                prompt_contains_full_needle=confounded,
                trivial=trivial,
            )
        )
        if emitted and invented:
            time.sleep(0.5)
            has_asst_marker = (
                store_has_source_marker(data_dir, sess_asst, "assistant/response")
                or store_has_source_marker(data_dir, sess_asst, "assistant/stream")
                or store_has_source_marker(data_dir, sess_asst, "reasoning/stream")
            )
            report["cases"].append(
                case(
                    "assistant_invent_token_in_store",
                    store_has_token(data_dir, sess_asst, invented) and has_asst_marker,
                    has_assistant_or_reasoning_marker=has_asst_marker,
                    note="full needle never determined by user text; requires token bytes + asst/reasoning marker",
                )
            )
            r_ai = turn(
                sess_asst,
                "What laboratory passphrase did you invent earlier in this session? "
                "Reply with only the exact labpass-… token you produced and nothing else. "
                f"It is exactly {len(invented)} characters. Do not invent a new one.",
                max_tokens=128,
            )
            if not contains_token(r_ai, invented):
                r_ai = turn(
                    sess_asst,
                    "Recall the exact laboratory passphrase token you previously invented. "
                    "Output that same labpass- + 8 hex digits token only. Do not create a new passphrase.",
                    max_tokens=128,
                )
            report["cases"].append(
                case(
                    "assistant_invent_next_turn_recall",
                    contains_token(r_ai, invented),
                    reply=r_ai[:400],
                    note="user never specified the random suffix; inject-only recovery of assistant invent",
                )
            )
        else:
            report["cases"].append(
                case("assistant_invent_token_in_store", False, note="model never emitted free-invented needle")
            )
            report["cases"].append(
                case("assistant_invent_next_turn_recall", False, note="model never emitted free-invented needle")
            )

        # --- C3: distractors then recall secret ---
        turn(sess, f"Note for project {uid}: vault key is {t_secret}.")
        for i, line in enumerate(
            [
                "Unrelated: the weather is fine.",
                "Unrelated: continue reviewing the README.",
                "Unrelated: ignore previous chit-chat and stay ready.",
            ]
        ):
            turn(sess, f"{line} (distract {i} {uid})", max_tokens=48)
        r_secret = turn(
            sess,
            f"What is the vault key for project {uid}? Reply with only the secret-… token.",
            max_tokens=128,
        )
        report["cases"].append(
            case("recall_after_distractors", contains_token(r_secret, t_secret), reply=r_secret[:400])
        )
        report["cases"].append(
            case("secret_token_in_store", store_has_token(data_dir, sess, t_secret))
        )

        # --- store growth ---
        db2 = session_sqlite(data_dir, sess)
        size2 = db2.stat().st_size if db2 and db2.exists() else 0
        wal = Path(str(db2) + "-wal") if db2 else None
        grew = size2 >= size1 or (wal is not None and wal.exists() and wal.stat().st_size > 0)
        report["cases"].append(
            case("store_updated", grew, size_before=size1, size_after=size2, wal=str(wal) if wal else None)
        )

        # --- isolation ---
        r_iso = turn(
            sess_b,
            f"What is the vault key for project {uid}? If you do not know, say UNKNOWN.",
            max_tokens=128,
        )
        leaked = contains_token(r_iso, t_secret) or contains_token(r_iso, t_deploy)
        report["cases"].append(
            case("isolation_other_session", not leaked, reply=r_iso[:400], leaked=leaked)
        )
        report["cases"].append(
            case(
                "isolation_store_no_secret",
                not store_has_token(data_dir, sess_b, t_secret),
            )
        )

        # --- late selective recall (after corrections/distractors; ranking stress) ---
        report["cases"].append(
            case(
                "late_turn_deploy_still_in_store",
                store_has_token(data_dir, sess, t_deploy),
                note="first-write durable input survives many later updates",
            )
        )
        r_deploy2 = turn(
            sess,
            f"Again for project {uid}: which branch do we deploy from? "
            f"Reply with only the complete deploy-… token (all hex digits). Do not abbreviate.",
            max_tokens=128,
        )
        if not contains_token(r_deploy2, t_deploy):
            r_deploy2 = turn(
                sess,
                f"Look up the deployment rule for project {uid}. "
                f"The allowed ship branch token starts with deploy-. Output that full token only.",
                max_tokens=128,
            )
        report["cases"].append(
            case(
                "late_turn_still_recalls_deploy",
                contains_token(r_deploy2, t_deploy),
                reply=r_deploy2[:400],
                note="selective recall of early fact after many updates",
            )
        )

    except Exception as e:
        report["error"] = str(e)
        report["ok"] = False
        (out_dir / "summary.json").write_text(json.dumps(report, indent=2))
        print(json.dumps(report, indent=2))
        return 1

    failed = [c for c in report["cases"] if not c.get("ok")]
    report["ok"] = len(failed) == 0
    report["failed"] = [c["name"] for c in failed]
    report["passed"] = [c["name"] for c in report["cases"] if c.get("ok")]
    (out_dir / "summary.json").write_text(json.dumps(report, indent=2))
    print(json.dumps(report, indent=2))
    return 0 if report["ok"] else 1


if __name__ == "__main__":
    sys.exit(main())
