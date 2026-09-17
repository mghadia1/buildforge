#!/usr/bin/env python3
"""Measure clean, no-op, and incremental builds, following PROJECT_SPEC.md section 8.

Each round runs every configuration once, in the same order, so a machine that
warms up or throttles part way through cannot favour one configuration over
another. The first round is discarded. Medians and IQRs are reported, and
adjacent configurations whose IQRs overlap are called inconclusive.

Four configurations:

  clean     empty cache, no outputs. The baseline.
  noop      warm cache, nothing changed. Pure cache-lookup overhead.
  cosmetic  a comment added to one source file. The compile must re-run because
            the source changed -- but the object file comes out byte-identical,
            so the link is correctly skipped. This is early cutoff, and it is
            the property that separates content addressing from timestamps.
  semantic  a constant changed in one source file, so the object really differs
            and the link must re-run too.

Usage:
    python3 testdata/gen_cproject.py --count 32 --iters 400 --out /tmp/buildforge-bench
    go build -o /tmp/buildforge ./cmd/buildforge
    python3 bench/cache.py --binary /tmp/buildforge --workspace /tmp/buildforge-bench
"""

import argparse
import json
import pathlib
import platform
import re
import shutil
import statistics
import subprocess
import sys
import time

CONFIGS = ["clean", "noop", "cosmetic", "semantic"]


def quartiles(xs):
    s = sorted(xs)
    med = statistics.median(s)
    mid = len(s) // 2
    lower, upper = s[:mid], (s[mid + 1:] if len(s) % 2 else s[mid:])
    return statistics.median(lower), med, statistics.median(upper)


def build(binary, workspace, cache_dir, sandbox=True):
    """Run one build. Returns (wall seconds, actions cached)."""
    cmd = [binary, "build", "-f", str(workspace / "build.json"), "-q", "-cache", str(cache_dir)]
    if not sandbox:
        cmd.append("-no-sandbox")
    start = time.perf_counter()
    proc = subprocess.run(cmd, capture_output=True, text=True)
    elapsed = time.perf_counter() - start
    if proc.returncode != 0:
        sys.exit(f"build failed:\n{proc.stdout}{proc.stderr}")
    m = re.search(r"\((\d+) cached\)", proc.stdout)
    return elapsed, int(m.group(1)) if m else -1


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("--binary", default="/tmp/buildforge")
    ap.add_argument("--workspace", default="/tmp/buildforge-bench")
    ap.add_argument("--runs", type=int, default=7)
    ap.add_argument("--json")
    ap.add_argument("--no-sandbox", action="store_true",
                    help="measure the cost of hermeticity by turning it off")
    args = ap.parse_args()

    ws = pathlib.Path(args.workspace).resolve()
    cache_dir = ws / ".bench-cache"
    n_actions = len(json.loads((ws / "build.json").read_text())["actions"])

    cosmetic = ws / "src" / "unit_3.c"
    semantic = ws / "src" / "unit_11.c"
    sandbox = not args.no_sandbox
    cosmetic_base = cosmetic.read_text()
    semantic_base = semantic.read_text()

    samples = {c: [] for c in CONFIGS}
    cached = {c: [] for c in CONFIGS}

    print(f"workload : {ws}/build.json ({n_actions} actions)")
    print(f"host     : {platform.platform()}")
    print(f"sandbox  : {'on' if sandbox else 'OFF'}")
    print(f"protocol : {args.runs} kept rounds, first discarded\n")

    try:
        for rnd in range(args.runs + 1):
            # Restore the sources so every round starts from the same tree.
            cosmetic.write_text(cosmetic_base)
            semantic.write_text(semantic_base)

            # clean: empty cache, no outputs.
            shutil.rmtree(cache_dir, ignore_errors=True)
            shutil.rmtree(ws / "out", ignore_errors=True)
            secs, n = build(args.binary, ws, cache_dir, sandbox)
            samples["clean"].append(secs); cached["clean"].append(n)

            # noop: warm cache, nothing changed.
            secs, n = build(args.binary, ws, cache_dir, sandbox)
            samples["noop"].append(secs); cached["noop"].append(n)

            # cosmetic: a comment only. The object should not change.
            cosmetic.write_text(cosmetic_base + f"\n/* round {rnd} */\n")
            secs, n = build(args.binary, ws, cache_dir, sandbox)
            samples["cosmetic"].append(secs); cached["cosmetic"].append(n)

            # semantic: a different constant, so the object really differs.
            semantic.write_text(re.sub(r"return acc \+ 11;",
                                       f"return acc + 11 + {rnd}.0;", semantic_base))
            secs, n = build(args.binary, ws, cache_dir, sandbox)
            samples["semantic"].append(secs); cached["semantic"].append(n)

            tag = "  (discarded)" if rnd == 0 else ""
            print("  round %d  " % rnd + "  ".join(
                f"{c}={samples[c][-1]:.3f}s/{cached[c][-1]}c" for c in CONFIGS) + tag)
    finally:
        cosmetic.write_text(cosmetic_base)
        semantic.write_text(semantic_base)

    kept = {c: samples[c][1:] for c in CONFIGS}
    rows = {c: quartiles(kept[c]) for c in CONFIGS}
    clean_med = rows["clean"][1]

    print(f"\n{'config':>9}  {'median':>9}  {'IQR':>18}  {'vs clean':>9}  {'cached':>7}")
    print("-" * 62)
    for c in CONFIGS:
        q1, med, q3 = rows[c]
        hits = statistics.median(cached[c][1:])
        print(f"{c:>9}  {med:8.4f}s  [{q1:7.4f}, {q3:7.4f}]  {clean_med/med:8.1f}x  "
              f"{hits:5.0f}/{n_actions}")

    print()
    for a, b in zip(CONFIGS, CONFIGS[1:]):
        a1, _, a3 = rows[a]
        b1, _, b3 = rows[b]
        if a1 <= b3 and b1 <= a3:
            print(f"INCONCLUSIVE: {a} and {b} have overlapping IQRs; this data cannot separate them")

    if args.json:
        pathlib.Path(args.json).write_text(json.dumps(
            {"workload": str(ws), "actions": n_actions, "host": platform.platform(),
             "runs_kept": args.runs, "seconds": samples, "cached": cached}, indent=2) + "\n")
        print(f"\nraw timings: {args.json}")


if __name__ == "__main__":
    main()
