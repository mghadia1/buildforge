#!/usr/bin/env python3
"""Measure a shared cache, following PROJECT_SPEC.md section 8.

Three configurations, every one run once per round in a fixed order, first
round discarded:

  cold    empty local cache, no remote. The baseline.
  remote  empty local cache, warm remote. What a new machine, or a fresh CI
          runner, experiences.
  local   warm local cache. The second build on the same machine, which must
          not touch the network at all.

The server runs on localhost, so these numbers exclude everything a real
network would add: latency, bandwidth limits, TLS, and contention. They show
that the mechanism works and roughly what the local-side cost is. They are not
a prediction of what a shared cache does across a building.

Usage:
    python3 testdata/gen_cproject.py --count 32 --iters 400 --out /tmp/buildforge-bench
    go build -o /tmp/buildforge ./cmd/buildforge
    python3 bench/remote.py --binary /tmp/buildforge --workspace /tmp/buildforge-bench
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
import tempfile
import time

CONFIGS = ["cold", "remote", "local"]


def quartiles(xs):
    s = sorted(xs)
    med = statistics.median(s)
    mid = len(s) // 2
    lower, upper = s[:mid], (s[mid + 1:] if len(s) % 2 else s[mid:])
    return statistics.median(lower), med, statistics.median(upper)


def build(binary, workspace, cache_dir, remote=None):
    cmd = [binary, "build", "-f", str(workspace / "build.json"), "-q", "-cache", str(cache_dir)]
    if remote:
        cmd += ["-remote", remote]
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
    ap.add_argument("--port", type=int, default=8178)
    ap.add_argument("--runs", type=int, default=7)
    args = ap.parse_args()

    ws = pathlib.Path(args.workspace).resolve()
    n_actions = len(json.loads((ws / "build.json").read_text())["actions"])
    url = f"http://127.0.0.1:{args.port}"

    server_dir = pathlib.Path(tempfile.mkdtemp(prefix="bf-server-"))
    local_dir = ws / ".bench-local"

    server = subprocess.Popen(
        [args.binary, "serve", "-addr", f"127.0.0.1:{args.port}", "-cache", str(server_dir / "cache")],
        stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    time.sleep(1.0)

    samples = {c: [] for c in CONFIGS}
    cached = {c: [] for c in CONFIGS}

    try:
        if server.poll() is not None:
            sys.exit("cache server exited immediately; is the port in use?")

        print(f"workload : {ws}/build.json ({n_actions} actions)")
        print(f"host     : {platform.platform()} (server on localhost)")
        print(f"protocol : {args.runs} kept rounds, first discarded\n")

        # Populate the remote once, so every 'remote' measurement is a hit.
        shutil.rmtree(local_dir, ignore_errors=True)
        shutil.rmtree(ws / "out", ignore_errors=True)
        build(args.binary, ws, local_dir, url)

        for rnd in range(args.runs + 1):
            shutil.rmtree(local_dir, ignore_errors=True)
            shutil.rmtree(ws / "out", ignore_errors=True)
            secs, n = build(args.binary, ws, local_dir)
            samples["cold"].append(secs); cached["cold"].append(n)

            shutil.rmtree(local_dir, ignore_errors=True)
            shutil.rmtree(ws / "out", ignore_errors=True)
            secs, n = build(args.binary, ws, local_dir, url)
            samples["remote"].append(secs); cached["remote"].append(n)

            shutil.rmtree(ws / "out", ignore_errors=True)
            secs, n = build(args.binary, ws, local_dir, url)
            samples["local"].append(secs); cached["local"].append(n)

            tag = "  (discarded)" if rnd == 0 else ""
            print(f"  round {rnd}  " + "  ".join(
                f"{c}={samples[c][-1]:.3f}s/{cached[c][-1]}c" for c in CONFIGS) + tag)
    finally:
        server.terminate()
        server.wait(timeout=10)
        shutil.rmtree(server_dir, ignore_errors=True)
        shutil.rmtree(local_dir, ignore_errors=True)

    rows = {c: quartiles(samples[c][1:]) for c in CONFIGS}
    cold = rows["cold"][1]

    print(f"\n{'config':>8}  {'median':>9}  {'IQR':>18}  {'vs cold':>8}  {'cached':>7}")
    print("-" * 60)
    for c in CONFIGS:
        q1, med, q3 = rows[c]
        hits = statistics.median(cached[c][1:])
        print(f"{c:>8}  {med:8.4f}s  [{q1:7.4f}, {q3:7.4f}]  {cold/med:7.1f}x  {hits:5.0f}/{n_actions}")

    print()
    for a, b in zip(CONFIGS, CONFIGS[1:]):
        a1, _, a3 = rows[a]
        b1, _, b3 = rows[b]
        if a1 <= b3 and b1 <= a3:
            print(f"INCONCLUSIVE: {a} and {b} have overlapping IQRs; this data cannot separate them")


if __name__ == "__main__":
    main()
