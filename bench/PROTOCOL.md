# Benchmark protocol

The rules are in [`../PROJECT_SPEC.md`](../PROJECT_SPEC.md) section 8 and are implemented by
[`speedup.py`](speedup.py). They are restated here because a benchmark whose protocol lives
somewhere else tends to acquire exceptions.

1. One fixed workload, named in every result.
2. A quiet host. Record what else was running — including "unknown".
3. At least 7 kept runs per configuration, **interleaved** (`seq, j=1, j=2, j=4, j=8, seq, ...`)
   rather than all of one configuration then all of the next, so a machine that warms up or
   thermally throttles part way through does not silently favour whichever configuration ran later.
4. Discard the first run of each configuration (page cache warming) and say so.
5. Report **median and interquartile range**. Never a single run, never a best-of.
6. **If the IQRs of two configurations overlap, they are reported as inconclusive.** `speedup.py`
   prints this automatically for adjacent pairs; it is not a judgement call.
7. Timings come from the harness's own wall clock *and* BuildForge's internal per-action
   timestamps, so a bug in one cannot manufacture a result in the other.

## Reproducing

```bash
python3 testdata/gen_cproject.py --count 32 --iters 400 --out /tmp/buildforge-bench
go build -o /tmp/buildforge ./cmd/buildforge
python3 bench/speedup.py --binary /tmp/buildforge --manifest /tmp/buildforge-bench/build.json --runs 7
```

The workload is generated deterministically, so the same `--count` and `--iters` always compile
byte-identical sources. These are real `cc -O2` compiles, not `sleep` calls: a sleep does not
contend for CPU, memory bandwidth, or the filesystem, and that contention is exactly what bounds a
real build.
