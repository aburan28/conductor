#!/usr/bin/env python3
"""delphi on-result hook: staged goal pipelines.

Stages, all keyed by meta.root:

  frame      (frontier) restate the goal, success criteria, attack directions
  expand     (free)     one concrete plan per direction, fanned out
  critique   (frontier) rank the plans, name the fatal flaws
  synthesize (frontier) final brief -> pipeline/<root>/GOAL.md

The legacy "ideas" stage still fans out to free-tier expansions.
Everything a stage produces is also written to
~/.delphi/pipeline/<root>/<stage>-<id>.md.
"""
import shutil
import glob, json, os, re, subprocess, sys

DELPHI = (os.environ.get("DELPHI_BIN") or shutil.which("delphi")
          or os.path.expanduser("~/.local/bin/delphi"))
PIPE = os.path.expanduser("~/.delphi/pipeline")
FRONTIER = os.environ.get("PIPELINE_DEEP_PROFILE", "frontier")
FREE = os.environ.get("PIPELINE_FREE_PROFILE", "free")
MAX_DIRS = int(os.environ.get("PIPELINE_MAX_IDEAS", "6"))
DEEP = os.environ.get("DELPHI_GOAL_DEEP", "1") != "0"
CLIP = 1200


def enqueue(prompt, meta, profile=None):
    cmd = [DELPHI, "ask", "-profile", profile or FREE, "-prompt", prompt]
    if (profile or FREE) in (FRONTIER, "implementer"):
        cmd += ["-max-tokens", "64000", "-effort", "medium"]
    for k, v in meta.items():
        cmd += ["-meta", "%s=%s" % (k, v)]
    subprocess.run(cmd, check=False, capture_output=True, timeout=120)


def parse_list(text):
    out = []
    for line in text.splitlines():
        m = re.match(r"\s*(?:\d+[.)]|[-*])\s+(.*\S)", line)
        if m:
            out.append(m.group(1).strip())
    if not out:
        out = [p.strip() for p in re.split(r"\n\s*\n", text) if len(p.strip()) > 40]
    return out[:MAX_DIRS]


def state_path(root):
    return os.path.join(PIPE, root, "state.json")


def load_state(root):
    try:
        return json.load(open(state_path(root)))
    except Exception:
        return {}


def save_state(root, st):
    os.makedirs(os.path.join(PIPE, root), exist_ok=True)
    tmp = state_path(root) + ".tmp"
    json.dump(st, open(tmp, "w"), indent=1)
    os.replace(tmp, state_path(root))


def read_stage(root, stage):
    rows = []
    for f in sorted(glob.glob(os.path.join(PIPE, root, "%s-*.md" % stage))):
        rows.append(open(f).read().strip()[:CLIP])
    return rows


def main():
    rid = os.environ.get("DELPHI_ID", "")
    status = os.environ.get("DELPHI_STATUS", "")
    path = os.environ.get("DELPHI_RESULT", "")
    if not path or not os.path.exists(path):
        return 0
    try:
        rec = json.load(open(path))
    except Exception:
        return 0
    if rec.get("status") != "succeeded":
        return 0
    meta = rec.get("meta") or {}
    stage = meta.get("stage")
    root = meta.get("root") or rid
    goal = meta.get("goal", meta.get("topic", "the goal"))
    outdir = os.path.join(PIPE, root)
    os.makedirs(outdir, exist_ok=True)
    text = rec.get("text") or ""
    open(os.path.join(outdir, "%s-%s.md" % (stage or "result", rid)), "w").write(text)
    if not os.path.exists(os.path.join(outdir, "goal.txt")):
        open(os.path.join(outdir, "goal.txt"), "w").write(goal + "\n")

    base = {"root": root, "goal": goal}

    if stage == "frame":
        dirs = parse_list(text)
        if not dirs and rec.get("truncated") and not meta.get("retried"):
            enqueue("Goal: %s\n\nList %d concrete attack directions as a numbered list, "
                    "one line each, no preamble and no long reasoning. Keep it under 300 "
                    "words." % (goal, MAX_DIRS),
                    dict(base, stage="frame", retried="1", parent=rid),
                    os.environ.get("PIPELINE_RETRY_PROFILE", "implementer"))
            return 0
        if not dirs:
            open(os.path.join(outdir, "STALLED.txt"), "w").write(
                "frame returned no parseable directions (stop_reason=%s, truncated=%s)\n"
                % (rec.get("stop_reason"), rec.get("truncated")))
            return 0
        st = {"goal": goal, "expected": len(dirs), "expand_done": 0,
              "critiqued": False, "synthesized": False}
        save_state(root, st)
        for i, d in enumerate(dirs, 1):
            enqueue(
                "Goal: %s\n\nDirection %d: %s\n\nWrite a concrete research plan for this "
                "direction: the exact construction or algorithm, the cost model, what "
                "would falsify it, and the first experiment to run. Under 400 words, no "
                "preamble." % (goal, i, d),
                dict(base, stage="expand", idea=str(i), parent=rid))

    elif stage == "expand":
        st = load_state(root)
        st["expand_done"] = int(st.get("expand_done", 0)) + 1
        save_state(root, st)
        if st["expand_done"] >= st.get("expected", 0) and not st.get("critiqued"):
            plans = read_stage(root, "expand")
            body = "\n\n".join("PLAN %d:\n%s" % (i + 1, p) for i, p in enumerate(plans))
            enqueue(
                "Goal: %s\n\n%d candidate plans follow.\n\n%s\n\nCritique them: rank by "
                "expected value, name the fatal flaw in each, say which single direction "
                "is worth real compute, and what would kill it fastest. Under 600 words."
                % (goal, len(plans), body),
                dict(base, stage="critique", parent=rid),
                FRONTIER if DEEP else FREE)
            st["critiqued"] = True
            save_state(root, st)

    elif stage == "critique":
        enqueue(
            "Goal: %s\n\nA critique of the candidate directions follows. Turn it into a "
            "research brief: the recommended direction, the cost model to beat, the first "
            "three experiments in order, and the kill criteria. Under 800 words.\n\n%s"
            % (goal, text[:4000]),
            dict(base, stage="synthesize", parent=rid),
            FRONTIER if DEEP else FREE)

    elif stage == "synthesize":
        st = load_state(root)
        st["synthesized"] = True
        save_state(root, st)
        open(os.path.join(outdir, "GOAL.md"), "w").write(
            "# %s\n\n## Brief\n\n%s\n" % (goal, text))
        # emit the repo-shaped problem statement (ledger skeleton + evidence gate)
        subprocess.run([os.path.expanduser("~/.local/bin/delphi-goal-emit"), root],
                       check=False, capture_output=True, timeout=60)

    elif stage == "ideas":
        # legacy single-stage fan-out
        for i, idea in enumerate(parse_list(text), 1):
            enqueue("Idea %d on %s:\n\n%s\n\nTurn this into a concrete, step-by-step "
                    "execution plan. Be specific. Under 400 words." % (i, goal, idea),
                    dict(base, stage="expand", idea=str(i), parent=rid))
    return 0


if __name__ == "__main__":
    sys.exit(main())
