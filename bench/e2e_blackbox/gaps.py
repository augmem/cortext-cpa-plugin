#!/usr/bin/env python3
"""
Gap suite: mid-stream ingest, assistant-invent→next-turn, process restart,
and adversarial multi-fact ranking — against a *running* CPA with cortext loaded.

Probes that claim inject send ONLY the latest user message (no needle in history).
Success requires exact needle tokens (no fluff scoring).

Input/update honesty:
  - Assistant path invents the needle; user text never contains the full token.
  - Stream path invents under stream:true; store scanned for token + assistant markers.
  - Ranking late-window bar under n_facts competitors; early residual recorded.
"""

from __future__ import annotations

import argparse
import json
import os
import re
import subprocess
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


def chat(base: str, model: str, session: str, content: str, *, stream: bool = False, max_tokens: int = 256) -> str:
    headers = {"Content-Type": "application/json", "X-Cortext-Session": session}
    key = os.environ.get("CPA_API_KEY") or os.environ.get("OPENAI_API_KEY")
    if key:
        headers["Authorization"] = f"Bearer {key}"
    body = {
        "model": model,
        "messages": [{"role": "user", "content": content}],
        "max_tokens": max_tokens,
        "stream": stream,
    }
    if stream:
        return chat_stream(base, body, headers)
    out = http_json(base.rstrip("/") + "/v1/chat/completions", body=body, headers=headers)
    msg = ((out.get("choices") or [{}])[0].get("message") or {})
    text = (msg.get("content") or "").strip()
    if not text:
        text = (msg.get("reasoning_content") or "").strip()
    return text


def chat_stream(base: str, body: dict, headers: dict) -> str:
    """Drive real SSE stream so HandleStreamChunk runs on CPA."""
    data = json.dumps(body).encode()
    req = urllib.request.Request(
        base.rstrip("/") + "/v1/chat/completions",
        data=data,
        headers=headers,
        method="POST",
    )
    parts: list[str] = []
    with urllib.request.urlopen(req, timeout=180) as resp:
        for raw_line in resp:
            line = raw_line.decode(errors="replace").strip()
            if not line.startswith("data:"):
                continue
            payload = line[5:].strip()
            if payload == "[DONE]":
                break
            try:
                obj = json.loads(payload)
            except json.JSONDecodeError:
                continue
            delta = ((obj.get("choices") or [{}])[0].get("delta") or {})
            if delta.get("content"):
                parts.append(delta["content"])
            if delta.get("reasoning_content"):
                parts.append(delta["reasoning_content"])
    return "".join(parts).strip()


def has_token(text: str, token: str) -> bool:
    """Exact needle required (case-insensitive). No partial-prefix credit."""
    t = (text or "").lower()
    tok = token.lower()
    if tok in t:
        return True
    return re.search(rf"(?<![a-z0-9]){re.escape(tok)}(?![a-z0-9])", t) is not None


def primary_answer(text: str) -> str:
    lines = [ln.strip() for ln in (text or "").splitlines() if ln.strip()]
    if not lines:
        return (text or "").strip()
    for ln in reversed(lines):
        if re.fullmatch(r"[A-Za-z0-9_-]{8,48}", ln):
            return ln
    return lines[-1] if len(lines[-1]) <= 120 else (text or "")[-120:]


def last_matching_token(text: str, tokens: list[str]) -> str | None:
    low = (text or "").lower()
    best, pos = None, -1
    for t in tokens:
        p = low.rfind(t.lower())
        if p > pos:
            best, pos = t, p
    return best


def strip_negated_mentions(text: str, tokens: list[str]) -> str:
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
    cands = [current, *superseded]
    pa = primary_answer(text)
    if has_token(pa, current) and not any(has_token(pa, s) for s in superseded):
        return True
    cleaned = strip_negated_mentions(text, cands)
    if last_matching_token(cleaned, cands) == current:
        return True
    tail = strip_negated_mentions((text or "")[-240:], cands)
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
    pat = re.compile(rf"\b({re.escape(prefix)}[a-z0-9]{{8,16}})\b", re.I)
    m = pat.search(text or "")
    return m.group(1) if m else None


def free_invent_prompt(prefix: str, n: int = 8) -> str:
    """User specifies only prefix+length; model invents the random suffix."""
    return (
        f"Invent a one-time laboratory passphrase. "
        f"Format: the literal prefix {prefix} followed by exactly {n} lowercase "
        f"hexadecimal digits (0-9, a-f) that you invent at random. "
        f"Do not use all zeros, sequential digits, or the word deadbeef. "
        f"Reply with ONLY the finished contiguous passphrase on one line and nothing else."
    )


def assistant_path_markers(data_dir: Path, session: str) -> dict:
    """Markers actually used by native ingest (incl. reasoning/stream for kimi)."""
    return {
        "assistant/response": store_has_marker(data_dir, session, "assistant/response"),
        "assistant/stream": store_has_marker(data_dir, session, "assistant/stream"),
        "reasoning/stream": store_has_marker(data_dir, session, "reasoning/stream"),
    }


def session_db(data_dir: Path, session: str) -> Path | None:
    safe = re.sub(r"[^a-zA-Z0-9._-]+", "_", session)[:80]
    if not data_dir.is_dir():
        return None
    for p in data_dir.iterdir():
        if safe in p.name and p.suffix == ".sqlite":
            return p
    return None


def store_blob(data_dir: Path, session: str) -> bytes:
    db = session_db(data_dir, session)
    if not db or not db.exists():
        return b""
    blob = db.read_bytes()
    for suffix in ("-wal", "-shm"):
        side = Path(str(db) + suffix)
        if side.exists():
            blob += side.read_bytes()
    return blob


def store_has_token(data_dir: Path, session: str, token: str) -> bool:
    if not token:
        return False
    blob = store_blob(data_dir, session)
    if not blob:
        return False
    raw = token.encode("utf-8", errors="ignore")
    return raw in blob or raw.lower() in blob.lower()


def store_has_marker(data_dir: Path, session: str, marker: str) -> bool:
    blob = store_blob(data_dir, session)
    return marker.encode() in blob if blob else False


def case(name: str, ok: bool, **kw) -> dict:
    return {"name": name, "ok": bool(ok), **kw}


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--base", default=os.environ.get("CPA_BASE", "http://127.0.0.1:8317"))
    ap.add_argument("--model", default=os.environ.get("CPA_MODEL", "kimi-k2.7-code"))
    ap.add_argument(
        "--data-dir",
        default=os.environ.get("CORTEXT_DATA_DIR", str(Path.home() / ".cli-proxy-api" / "cortext")),
    )
    ap.add_argument("--out", default="")
    ap.add_argument("--ranking-n", type=int, default=32, help="competing facts (≥24)")
    ap.add_argument("--ranking-probes", type=int, default=8, help="how many selective probes")
    ap.add_argument("--ranking-min-pass", type=float, default=0.5, help="min pass rate for ranking probes")
    ap.add_argument("--skip-restart", action="store_true")
    ap.add_argument("--pause", type=float, default=0.35)
    args = ap.parse_args()

    if args.ranking_n < 24:
        print("ranking-n must be >= 24", file=sys.stderr)
        return 2

    out = Path(args.out) if args.out else Path.cwd() / "out"
    out.mkdir(parents=True, exist_ok=True)
    ranking_dir = out / "ranking"
    restart_dir = out / "restart"
    ranking_dir.mkdir(exist_ok=True)
    restart_dir.mkdir(exist_ok=True)
    data_dir = Path(args.data_dir).expanduser()
    data_dir.mkdir(parents=True, exist_ok=True)

    uid = uuid.uuid4().hex[:8]
    report: dict = {
        "base": args.base,
        "model": args.model,
        "data_dir": str(data_dir),
        "cases": [],
        "ok": False,
        "design": {
            "no_history_probes": True,
            "exact_needle_tokens": True,
            "stream_is_sse": True,
            "interrupt_gate_is_next_turn_only": True,
            "assistant_invent_unconfounded": True,
            "store_layer_token_scan": True,
        },
    }

    def turn(session: str, content: str, *, stream: bool = False, max_tokens: int = 256) -> str:
        time.sleep(args.pause)
        return chat(args.base, args.model, session, content, stream=stream, max_tokens=max_tokens)

    # Health
    try:
        models = http_json(args.base.rstrip("/") + "/v1/models")
        report["models_ok"] = bool(models.get("data"))
    except Exception as e:
        report["error"] = f"CPA unreachable: {e}"
        (out / "summary.json").write_text(json.dumps(report, indent=2))
        print(json.dumps(report, indent=2))
        return 2

    try:
        # ========== 1) Free assistant invent → store → next-turn (unconfounded) ==========
        sess_a = f"gap-asst-{uid}"
        invent_prompt = free_invent_prompt("asstpass-", n=8)
        seed = turn(sess_a, invent_prompt, max_tokens=256)
        asst_needle = extract_invented(seed, "asstpass-")
        if asst_needle is None:
            seed = turn(
                sess_a,
                invent_prompt + " Output exactly one asstpass-######## token (8 hex digits).",
                max_tokens=256,
            )
            asst_needle = extract_invented(seed, "asstpass-")
        banned = {"asstpass-00000000", "asstpass-deadbeef", "asstpass-12345678", "asstpass-ffffffff"}
        confounded = bool(asst_needle) and asst_needle.lower() in invent_prompt.lower()
        trivial = bool(asst_needle) and asst_needle.lower() in banned
        emitted = bool(asst_needle) and not confounded and not trivial
        report["cases"].append(
            case(
                "assistant_invented_needle_emitted",
                emitted,
                seed=seed[:300],
                needle=asst_needle,
                prompt_contains_full_needle=confounded,
                trivial=trivial,
            )
        )
        time.sleep(0.5)
        markers_a = assistant_path_markers(data_dir, sess_a) if emitted else {}
        report["cases"].append(
            case(
                "assistant_invent_token_in_store",
                bool(emitted and asst_needle and store_has_token(data_dir, sess_a, asst_needle)),
                markers=markers_a,
                has_assistant_or_reasoning_marker=any(markers_a.values()) if markers_a else False,
                has_user_ingest_marker=store_has_marker(data_dir, sess_a, "user/ingest"),
                note="random suffix never in user text; durable write must be assistant/reasoning path",
            )
        )
        # Gate: free invent must leave an assistant-side source marker (response or stream).
        report["cases"].append(
            case(
                "assistant_invent_has_asst_path_marker",
                bool(emitted and any(markers_a.values())),
                markers=markers_a,
            )
        )
        probe = turn(
            sess_a,
            "What laboratory passphrase did you invent earlier in this session? "
            "Reply with only the exact asstpass-… token you produced and nothing else. "
            "Do not invent a new one.",
            max_tokens=128,
        )
        if emitted and asst_needle and not has_token(probe, asst_needle):
            probe = turn(
                sess_a,
                "Recall the exact laboratory passphrase token you previously invented. "
                "Output that same asstpass- + 8 hex digits token only. Do not create a new passphrase.",
                max_tokens=128,
            )
        report["cases"].append(
            case(
                "assistant_ingest_next_turn_recall",
                bool(emitted and asst_needle and has_token(probe, asst_needle)),
                probe=probe[:400],
                note="no chat history; random suffix never in any user message",
            )
        )

        # ========== 2) Free stream invent path (SSE) ==========
        sess_s = f"gap-stream-{uid}"
        stream_prompt = free_invent_prompt("streampass-", n=8).replace(
            "laboratory passphrase", "stream vault code"
        )
        stream_out = turn(sess_s, stream_prompt, stream=True, max_tokens=256)
        stream_needle = extract_invented(stream_out, "streampass-")
        if stream_needle is None:
            stream_out = turn(
                sess_s,
                stream_prompt + " Output exactly one streampass-######## token (8 hex digits).",
                stream=True,
                max_tokens=256,
            )
            stream_needle = extract_invented(stream_out, "streampass-")
        sbanned = {
            "streampass-00000000",
            "streampass-deadbeef",
            "streampass-12345678",
            "streampass-ffffffff",
        }
        stream_confounded = bool(stream_needle) and stream_needle.lower() in stream_prompt.lower()
        stream_trivial = bool(stream_needle) and stream_needle.lower() in sbanned
        stream_emitted = bool(stream_needle) and not stream_confounded and not stream_trivial
        report["cases"].append(
            case(
                "stream_path_ran",
                len(stream_out) > 0 and stream_emitted,
                stream_bytes=len(stream_out),
                stream_out=stream_out[:300],
                needle=stream_needle,
                note="stream:true OpenAI SSE; free invent (suffix not in user text)",
            )
        )
        time.sleep(0.5)
        markers_s = assistant_path_markers(data_dir, sess_s) if stream_emitted else {}
        # HARD gate: token in store AND at least one assistant/reasoning path marker.
        report["cases"].append(
            case(
                "stream_invent_token_in_store",
                bool(
                    stream_emitted
                    and stream_needle
                    and store_has_token(data_dir, sess_s, stream_needle)
                    and any(markers_s.values())
                ),
                markers=markers_s,
                token_in_store=bool(stream_needle and store_has_token(data_dir, sess_s, stream_needle or "")),
                note="requires token bytes + assistant/stream|response or reasoning/stream marker",
            )
        )
        sprobe = turn(
            sess_s,
            "What stream vault code did you invent earlier in this session? "
            "Reply with only the exact streampass-… token you produced and nothing else. "
            "Do not invent a new one.",
            max_tokens=128,
        )
        if stream_emitted and stream_needle and not has_token(sprobe, stream_needle):
            sprobe = turn(
                sess_s,
                "Recall the exact stream vault code you previously invented. "
                "Output that same streampass- + 8 hex digits token only. Do not create a new one.",
                max_tokens=128,
            )
        report["cases"].append(
            case(
                "stream_path_next_turn_recall",
                bool(stream_emitted and stream_needle and has_token(sprobe, stream_needle)),
                probe=sprobe[:400],
            )
        )
        sdb = session_db(data_dir, sess_s)
        report["cases"].append(
            case("stream_session_store_exists", sdb is not None and sdb.exists(), path=str(sdb))
        )

        # ========== 3) User input + correction update (store + exclusive current) ==========
        sess_u = f"gap-upd-{uid}"
        t_old = f"oldv-{uuid.uuid4().hex[:10]}"
        t_new = f"newv-{uuid.uuid4().hex[:10]}"
        turn(sess_u, f"Service version for project {uid} is {t_old}.")
        report["cases"].append(
            case(
                "user_input_token_in_store",
                store_has_token(data_dir, sess_u, t_old),
                has_user_ingest=store_has_marker(data_dir, sess_u, "user/ingest"),
            )
        )
        turn(
            sess_u,
            f"Correction for project {uid}: service version is now {t_new}, not {t_old}.",
        )
        report["cases"].append(
            case(
                "update_both_versions_in_store",
                store_has_token(data_dir, sess_u, t_old) and store_has_token(data_dir, sess_u, t_new),
            )
        )
        uprobe = turn(
            sess_u,
            f"What is the CURRENT service version for project {uid}? "
            f"Reply with only the newv-… or oldv-… token that is CURRENT. "
            f"Do not mention superseded versions.",
            max_tokens=128,
        )
        ans_u = primary_answer(uprobe)
        has_new = has_token(uprobe, t_new)
        has_old = has_token(uprobe, t_old)
        exclusive = exclusive_current_ok(uprobe, t_new, t_old)
        report["cases"].append(
            case(
                "update_exclusive_current",
                exclusive,
                reply=uprobe[:400],
                primary=ans_u[:200],
                has_new=has_new,
                has_old=has_old,
                note="exclusive: new is current; ignores 'not OLD' restatements",
            )
        )

        # ========== 4) Ranking stress ≥24 competing facts ==========
        sess_r = f"gap-rank-{uid}"
        facts = []
        for i in range(args.ranking_n):
            tok = f"rank-{i:02d}-{uuid.uuid4().hex[:8]}"
            facts.append({"i": i, "token": tok, "text": f"Fact slot {i} for project {uid} holds token {tok}."})
        (ranking_dir / "facts.json").write_text(json.dumps(facts, indent=2))
        for f in facts:
            turn(sess_r, f["text"], max_tokens=48)
        # Spot-check first and last tokens in store (input proof under load)
        report["cases"].append(
            case(
                "ranking_input_first_last_in_store",
                store_has_token(data_dir, sess_r, facts[0]["token"])
                and store_has_token(data_dir, sess_r, facts[-1]["token"]),
                n_facts=args.ranking_n,
            )
        )
        half = args.ranking_n // 2
        late_idxs = list(range(half, args.ranking_n))
        early_idxs = list(range(0, half))

        def sample_idxs(idxs: list[int], k: int) -> list[int]:
            if k >= len(idxs):
                return list(idxs)
            step = max(1, len(idxs) // k)
            return [idxs[i] for i in range(0, len(idxs), step)][:k]

        late_probe_idxs = sample_idxs(late_idxs, args.ranking_probes)
        early_probe_idxs = sample_idxs(early_idxs, min(4, args.ranking_probes))

        def probe_slot(idx: int) -> dict:
            f = facts[idx]
            ans = turn(
                sess_r,
                f"What token is in fact slot {idx} for project {uid}? "
                f"Reply with only the exact rank-{idx:02d}-… token characters and nothing else. "
                f"Do not explain.",
                max_tokens=128,
            )
            return {"idx": idx, "token": f["token"], "ok": has_token(ans, f["token"]), "reply": ans[:300]}

        late_outcomes = [probe_slot(i) for i in late_probe_idxs]
        early_outcomes = [probe_slot(i) for i in early_probe_idxs]
        (ranking_dir / "outcomes.json").write_text(
            json.dumps({"late": late_outcomes, "early": early_outcomes, "n_facts": args.ranking_n}, indent=2)
        )
        late_hits = sum(1 for o in late_outcomes if o["ok"])
        late_rate = late_hits / max(1, len(late_outcomes))
        early_hits = sum(1 for o in early_outcomes if o["ok"])
        early_rate = early_hits / max(1, len(early_outcomes))
        report["cases"].append(
            case(
                "ranking_stress",
                late_rate >= args.ranking_min_pass and args.ranking_n >= 24,
                n_facts=args.ranking_n,
                late_n_probes=len(late_outcomes),
                late_hits=late_hits,
                late_rate=late_rate,
                early_n_probes=len(early_outcomes),
                early_hits=early_hits,
                early_rate=early_rate,
                min_pass=args.ranking_min_pass,
                bar="late_window_hits/late_window_probes under n_facts competitors",
                late_outcomes=late_outcomes,
                early_outcomes=early_outcomes,
            )
        )

        # ========== 5) Process restart durability ==========
        if not args.skip_restart:
            sess_d = f"gap-dur-{uid}"
            dur_needle = f"dur-needle-{uuid.uuid4().hex[:12]}"
            turn(sess_d, f"Persistence check for project {uid}: durable token is {dur_needle}.")
            report["cases"].append(
                case(
                    "restart_pre_token_in_store",
                    store_has_token(data_dir, sess_d, dur_needle),
                )
            )
            pre = turn(
                sess_d,
                f"What is the durable token for project {uid}? "
                f"Reply with only the exact dur-needle-… token and nothing else.",
                max_tokens=128,
            )
            report["cases"].append(
                case("restart_pre_recall", has_token(pre, dur_needle), reply=pre[:300])
            )
            db_pre = session_db(data_dir, sess_d)
            (restart_dir / "pre.json").write_text(
                json.dumps(
                    {
                        "session": sess_d,
                        "needle": dur_needle,
                        "pre_reply": pre,
                        "db": str(db_pre) if db_pre else None,
                        "db_size": db_pre.stat().st_size if db_pre and db_pre.exists() else 0,
                        "store_has": store_has_token(data_dir, sess_d, dur_needle),
                    },
                    indent=2,
                )
            )
            rlog = restart_dir / "brew_restart.log"
            proc = subprocess.run(
                ["brew", "services", "restart", "cliproxyapi"],
                capture_output=True,
                text=True,
                timeout=120,
            )
            rlog.write_text(proc.stdout + "\n" + proc.stderr)
            up = False
            for _ in range(40):
                try:
                    http_json(args.base.rstrip("/") + "/v1/models")
                    up = True
                    break
                except Exception:
                    time.sleep(0.5)
            report["cases"].append(case("restart_cpa_up", up, returncode=proc.returncode))
            mapped = False
            try:
                ps = subprocess.run(["pgrep", "-f", "cliproxyapi"], capture_output=True, text=True)
                pid = (ps.stdout or "").strip().split("\n")[0]
                if pid:
                    lo = subprocess.run(["lsof", "-p", pid], capture_output=True, text=True)
                    mapped = "cortext.dylib" in (lo.stdout or "")
                    (restart_dir / "lsof_cortext.txt").write_text(
                        "\n".join(l for l in (lo.stdout or "").splitlines() if "cortext" in l)
                    )
            except Exception as e:
                (restart_dir / "lsof_error.txt").write_text(str(e))
            report["cases"].append(case("restart_plugin_mapped", mapped))

            post = turn(
                sess_d,
                f"After restart: durable token for project {uid}? "
                f"Reply with only the exact dur-needle-… token and nothing else.",
                max_tokens=128,
            )
            (restart_dir / "post.json").write_text(
                json.dumps(
                    {
                        "needle": dur_needle,
                        "post_reply": post,
                        "ok": has_token(post, dur_needle),
                        "store_has_after": store_has_token(data_dir, sess_d, dur_needle),
                    },
                    indent=2,
                )
            )
            report["cases"].append(
                case(
                    "restart_post_recall",
                    has_token(post, dur_needle),
                    reply=post[:400],
                    session=sess_d,
                )
            )
        else:
            report["cases"].append(case("restart_skipped", False, note="--skip-restart set"))

    except Exception as e:
        report["error"] = str(e)
        report["ok"] = False
        (out / "summary.json").write_text(json.dumps(report, indent=2))
        print(json.dumps(report, indent=2))
        return 1

    failed = [c["name"] for c in report["cases"] if not c.get("ok")]
    report["failed"] = failed
    report["passed"] = [c["name"] for c in report["cases"] if c.get("ok")]
    report["ok"] = len(failed) == 0
    (out / "summary.json").write_text(json.dumps(report, indent=2))
    print(json.dumps(report, indent=2))
    return 0 if report["ok"] else 1


if __name__ == "__main__":
    sys.exit(main())
