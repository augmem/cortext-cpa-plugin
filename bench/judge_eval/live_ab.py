#!/usr/bin/env python3
"""live_ab.py — live A/B "does memory help" eval through a running CLIProxyAPI.

Arm design (all traffic passes through the cortext plugin):

  memory arm : seed a fact as a user turn in session A, then probe with a
               history-less body in the SAME session. Only plugin-injected
               memory can supply the answer.
  control arm: the identical probe in a fresh session B (plugin active, but
               no seeded memory). This is the no-memory baseline under the
               same proxy, model, and prompt.

Guards:
  - token facts use random hex suffixes the model cannot know (exact-match
    scored; hallucinated passes are ~impossible).
  - semantic facts are graded by an LLM judge, each judge call in its OWN
    fresh session so judge prompts never contaminate each other or subjects.
  - derailment guard: after seeding, a general-knowledge question in the
    memory session must still be answered correctly.

Exit 0 when memory arm strictly beats control on judge-scored items and the
derailment guard passes; exit 1 otherwise. Artifacts: summary.json, items.jsonl.
"""

import argparse
import json
import os
import re
import secrets
import sys
import time
import urllib.request

SEMANTIC_FACTS = [
    ("Deploy only from the release branch, never main.", "Which branch do we deploy from?", "release"),
    ("The on-call runbook lives at ops/RUNBOOK.md.", "Where is the on-call runbook?", "ops/RUNBOOK.md"),
    ("Pager rotation is owned by team-delta.", "Who owns pager rotation?", "team-delta"),
    ("Database migrations run at 03:15 UTC.", "When do database migrations run?", "03:15"),
]
TOKEN_FACT_TEMPLATES = [
    ("The canary token is {tok}.", "What is the canary token?"),
    ("The staging vault password is {tok}.", "What is the staging vault password?"),
    ("The cache service pet name is {tok}.", "What is the cache service pet name?"),
    ("The deploy window code is {tok}.", "What is the deploy window code?"),
]
GUARD_Q = ("What is the capital of France? Answer with just the city.", "Paris")


def chat(base, key, model, messages, session, max_tokens=128, timeout=120):
    body = json.dumps({
        "model": model,
        "messages": messages,
        "max_tokens": max_tokens,
    }).encode()
    req = urllib.request.Request(
        base.rstrip("/") + "/chat/completions",
        data=body,
        headers={
            "Authorization": "Bearer " + key,
            "Content-Type": "application/json",
            "X-Cortext-Session": session,
        },
    )
    with urllib.request.urlopen(req, timeout=timeout) as resp:
        data = json.loads(resp.read())
    return data["choices"][0]["message"]["content"]


def token_in(text, tok):
    return re.search(r"(?i)(?<![a-z0-9-])" + re.escape(tok) + r"(?![a-z0-9-])", text) is not None


def judge(base, key, model, session, probe, gold, answer):
    prompt = (
        "You are a strict binary grader.\n"
        f"Question: {probe}\n"
        f"Expected answer must include: {gold}\n"
        f"Model answer: {answer}\n\n"
        "Does the model answer correctly include the expected information? "
        'Reply JSON only: {"score":1 or 0,"rationale":"short reason"}'
    )
    text = chat(base, key, model, [{"role": "user", "content": prompt}], session, max_tokens=120)
    m = re.search(r"\{.*\}", text, re.S)
    if m:
        try:
            parsed = json.loads(m.group(0))
            return int(parsed.get("score", 0) == 1), parsed.get("rationale", "")
        except (ValueError, AttributeError):
            pass
    ok = gold.lower() in answer.lower()
    return ok, "fallback keyword match; judge JSON parse failed"


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--base", default="http://127.0.0.1:8317/v1")
    ap.add_argument("--key", default=os.environ.get("CPA_API_KEY", ""))
    ap.add_argument("--subject-model", default="kimi-k2.7-code")
    ap.add_argument("--judge-model", default="gpt-5.4-mini")
    ap.add_argument("--out", default="./out")
    args = ap.parse_args()
    if not args.key:
        sys.exit("CPA api key required (--key or CPA_API_KEY)")

    run = secrets.token_hex(3)
    os.makedirs(args.out, exist_ok=True)
    items, scores = [], {"memory": 0, "control": 0}

    facts = []
    for fact, probe, gold in SEMANTIC_FACTS:
        facts.append({"kind": "semantic", "fact": fact, "probe": probe, "gold": gold})
    for tmpl, probe in TOKEN_FACT_TEMPLATES:
        tok = secrets.token_hex(4)
        facts.append({"kind": "token", "fact": tmpl.format(tok=tok), "probe": probe, "gold": tok})

    for i, it in enumerate(facts):
        sess_a, sess_b = f"ab-mem-{run}-{i}", f"ab-ctl-{run}-{i}"
        # Seed (memory arm only). The plugin durable-ingests this user turn.
        chat(args.base, args.key, args.subject_model,
             [{"role": "user", "content": "Remember this fact for later: " + it["fact"]}],
             sess_a, max_tokens=32)
        # Probe both arms with history-less bodies.
        probe_msgs = [{"role": "user", "content": it["probe"]}]
        ans_mem = chat(args.base, args.key, args.subject_model, probe_msgs, sess_a)
        ans_ctl = chat(args.base, args.key, args.subject_model, probe_msgs, sess_b)

        if it["kind"] == "token":
            mem_ok, ctl_ok = token_in(ans_mem, it["gold"]), token_in(ans_ctl, it["gold"])
            why = "exact-token match"
        else:
            mem_ok, why_m = judge(args.base, args.key, args.judge_model,
                                  f"ab-judge-{run}-{i}-m", it["probe"], it["gold"], ans_mem)
            ctl_ok, why_c = judge(args.base, args.key, args.judge_model,
                                  f"ab-judge-{run}-{i}-c", it["probe"], it["gold"], ans_ctl)
            why = f"memory: {why_m} | control: {why_c}"
        scores["memory"] += mem_ok
        scores["control"] += ctl_ok
        items.append({**it, "answer_memory": ans_mem, "answer_control": ans_ctl,
                      "memory_ok": mem_ok, "control_ok": ctl_ok, "grader": why})

        # Derailment guard on the memory session.
        guard_ans = chat(args.base, args.key, args.subject_model,
                         [{"role": "user", "content": GUARD_Q[0]}], sess_a, max_tokens=128)
        guard_ok = GUARD_Q[1].lower() in guard_ans.lower()
        items[-1]["guard_answer"] = guard_ans
        items[-1]["guard_ok"] = guard_ok

    guards_ok = all(it["guard_ok"] for it in items)
    summary = {
        "run": run,
        "path": "live-cpa-http-ab",
        "base": args.base,
        "subject_model": args.subject_model,
        "judge_model": args.judge_model,
        "n_items": len(items),
        "scores": scores,
        "memory_gt_control": scores["memory"] > scores["control"],
        "guards_ok": guards_ok,
        "generated_at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
    }
    with open(os.path.join(args.out, "items.jsonl"), "w") as f:
        for it in items:
            f.write(json.dumps(it) + "\n")
    with open(os.path.join(args.out, "summary.json"), "w") as f:
        json.dump(summary, f, indent=2)
    print(json.dumps(summary, indent=2))
    if not (summary["memory_gt_control"] and guards_ok):
        sys.exit(1)


if __name__ == "__main__":
    main()
