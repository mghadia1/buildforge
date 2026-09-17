#!/usr/bin/env python3
"""Measure the speedup curve, following PROJECT_SPEC.md section 8.

The protocol is not optional decoration. It exists because an earlier project in
this portfolio produced a caching benchmark where run-to-run variance exceeded
the effect being measured and the sign was not even stable, and the numbers had
to be thrown away. The rules encoded here:

  * one fixed workload, named in the output;
  * runs INTERLEAVED across configurations (1,2,4,8,1,2,4,8,...) rather than all
    of one then all of the next, so a machine that warms up or throttles part way
    through does not silently favour whichever configuration ran later;
  * the first run of each configuration is discarded (page cache warming);
  * median and interquartile range reported, never a single run and never a
    best-of;
  * overlapping IQRs are reported as inconclusive rather than as a result.

Usage:
    python3 testdata/gen_cproject.py --count 24 --out /tmp/buildforge-bench
    go build -o /tmp/buildforge ./cmd/buildforge
    python3 bench/speedup.py --binary /tmp/buildforge --manifest /tmp/buildforge-bench/build.json
"""

import argparse
import json
import pathlib
import platform
import shutil
import statistics
import subprocess
import sys
import time


def quartiles(xs):
    """Return (q1, median, q3) using linear interpolation."""
    s = sorted(xs)
    med = statistics.median(s)
    mid = len(s) // 2
    lower = s[:mid]
    upper = s[mid + 1:] if len(s) % 2 else s[mid:]
    return statistics.median(lower), med, statistics.median(upper)


def run_once(binary, manifest, workspace, jobs, sequential):
    """Time one clean build. Returns wall seconds."""
    out = pathlib.Path(workspace) / "out"
    if out.exists():
        shutil.rmtree(out)  # no cache yet: every run is a clean build

    cmd = [binary, "build", "-f", manifest, "-q"]
    cmd += ["-seq"] if sequential else ["-j", str(jobs)]

    start = time.perf_counter()
    proc = subprocess.run(cmd, capture_output=True, text=True)
    elapsed = time.perf_counter() - start

    if proc.returncode != 0:
        sys.exit(f"build failed ({' '.join(cmd)}):\n{proc.stdout}{proc.stderr}")
    return elapsed


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("--binary", default="/tmp/buildforge")
    ap.add_argument("--manifest", default="/tmp/buildforge-bench/build.json")
    ap.add_argument("--workers", default="1,2,4,8")
    ap.add_argument("--runs", type=int, default=7, help="kept runs per configuration")
    ap.add_argument("--json", help="also write raw timings here")
    args = ap.parse_args()

    manifest = pathlib.Path(args.manifest).resolve()
    workspace = manifest.parent
    n_actions = len(json.loads(manifest.read_text())["actions"])

    configs = [("seq", True, 0)] + [(f"j={w}", False, int(w)) for w in args.workers.split(",")]

    # One extra run per configuration, discarded below.
    total = args.runs + 1
    samples = {label: [] for label, _, _ in configs}

    print(f"workload : {manifest} ({n_actions} actions)")
    print(f"host     : {platform.platform()}")
    print(f"protocol : {args.runs} kept runs per config, interleaved, first discarded\n")

    for i in range(total):
        for label, sequential, jobs in configs:
            secs = run_once(args.binary, str(manifest), workspace, jobs, sequential)
            samples[label].append(secs)
            marker = "  (discarded)" if i == 0 else ""
            print(f"  round {i}  {label:>5}  {secs:6.3f}s{marker}")
        print()

    kept = {label: vals[1:] for label, vals in samples.items()}

    print(f"{'config':>6}  {'median':>8}  {'IQR':>16}  {'speedup':>8}")
    print("-" * 46)

    baseline = None
    rows = {}
    for label, _, _ in configs:
        q1, med, q3 = quartiles(kept[label])
        rows[label] = (q1, med, q3)
        if label == "j=1":
            baseline = med

    for label, _, _ in configs:
        q1, med, q3 = rows[label]
        speedup = f"{baseline / med:.2f}x" if baseline else "-"
        print(f"{label:>6}  {med:7.3f}s  [{q1:6.3f}, {q3:6.3f}]  {speedup:>8}")

    # Inconclusive pairs: adjacent configurations whose IQRs overlap cannot be
    # distinguished by this data, and saying so is the point of the protocol.
    print()
    labels = [c[0] for c in configs]
    for a, b in zip(labels, labels[1:]):
        a1, _, a3 = rows[a]
        b1, _, b3 = rows[b]
        if a1 <= b3 and b1 <= a3:
            print(f"INCONCLUSIVE: {a} and {b} have overlapping IQRs; this data cannot separate them")

    if args.json:
        pathlib.Path(args.json).write_text(json.dumps({
            "workload": str(manifest),
            "actions": n_actions,
            "host": platform.platform(),
            "runs_kept": args.runs,
            "samples_including_discarded": samples,
        }, indent=2) + "\n")
        print(f"\nraw timings: {args.json}")


if __name__ == "__main__":
    main()
