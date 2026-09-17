# BuildForge — Incremental Build System with a Content-Addressed Remote Cache (Go)

> **Build spec, September 16, 2026.** A pure software-engineering project: no model, no dataset,
> no GPU. It exists to cover the one gap in this portfolio — concurrency, cache correctness, and
> distributed state under contention — with a problem every large engineering organization has
> solved internally (Bazel at Google, Buck2 at Meta, CloudBuild at Microsoft).

**Honesty gate.** Nothing here reaches a resume, the portfolio site, or an application until
Mayank can run it and explain, unaided: how a cache key is computed, why a false cache hit
happens, and one failure mode he found himself. Default `learning_status: learning`.
Building with Claude Code is fine — but the scheduler and the cache-key logic are exactly where
an interviewer will probe, so those must be understood line by line.

**Second gate, specific to this project.** Go is a new language. "The code compiles" is not the
bar. The bar is being able to read your own scheduler aloud and explain why a channel send
blocks. See Milestone 0.

---

## 1. Why a build system

It looks like tooling and is actually a distributed systems project wearing a disguise:

- **Concurrency** — a DAG scheduler with a bounded worker pool, cancellation, and cycle detection.
- **Storage** — content-addressed blob store, deduplication, reference counting, eviction.
- **Cache correctness** — the hardest problem in the domain, with a failure mode (the silent
  wrong build) that is genuinely instructive and demonstrable.
- **Distributed state** — a shared remote cache, concurrent writers, partial failure.
- **A scale ladder that is real**, not hypothetical: local cache → remote cache → remote execution.

And the demo is visceral. A clean build takes ~45 seconds; you change one line; the rebuild takes
under a second and prints exactly which actions it skipped and why.

## 2. What it does (one sentence)

BuildForge reads a declarative manifest of build actions, executes them in dependency order
across a worker pool, and skips any action whose inputs, command, and environment hash to a key
already present in a local or shared content-addressed cache.

## 3. Architecture

```
  build.json  ──►  Parser  ──►  Action DAG  ──►  Cycle check + topological order
  (actions:                                            │
   inputs, outputs,                                    ▼
   command, env)                               Scheduler (worker pool)
                                                       │
                              ┌────────────────────────┼────────────────────────┐
                              ▼                        ▼                        ▼
                        Cache lookup            Sandbox exec              Cache store
                      key = H(inputs,        (declared inputs only,     outputs → CAS
                        cmd, env,             via symlink farm)         key → digests
                        toolchain,                                       in action cache
                        platform)
                              │
                              ├── local:  .buildforge/cas/<sha256>
                              └── remote: HTTP/gRPC shared cache
```

Two caches, deliberately separate, because they answer different questions:

- **Action cache** — `action key → list of output digests`. "Have I run this exact action before?"
- **CAS (content-addressed store)** — `content digest → bytes`. "Do I already have these bytes?"

Splitting them is what makes deduplication work: two different actions that happen to produce
identical output store one copy.

## 4. Tech stack, and why Go

- **Language:** Go 1.23+. Standard library only for the core; `google.golang.org/grpc` and
  `protobuf` only if Milestone 7 is attempted.
- **Test workload:** a real multi-module project (a C project with 30–60 translation units is
  ideal — it gives a deep DAG and slow enough actions that caching visibly matters). Do **not**
  benchmark against synthetic `sleep` actions.
- **Manifest format:** JSON to start. A nicer DSL is a distraction from the actual problems.

Why Go rather than Java, C++, or Rust:

| Reason | Detail |
|---|---|
| Domain fit | Docker, Kubernetes, Turborepo, Earthly, Dagger are all Go. Infra tooling converged here. |
| Concurrency | Worker-per-action with `errgroup` + `context` cancellation is ~150 readable lines. |
| Distribution | Cross-compiles to one static binary with no runtime. A build tool must be one file. |
| gRPC | Bazel's Remote Execution API is a published gRPC spec; Go makes implementing a subset the easy path. |
| Ramp | Small language. Roughly two weeks from Java to productive — and Summer 2027 applications are already open. |

This is a defensible answer to "why Go?" in an interview. Have it ready.

---

## 5. Milestone 0 — The Go ramp (one week, gated)

Do not start Milestone 1 until this gate passes. The capstone on day 7 is a real BuildForge
component, so the week is not throwaway work.

| Day | Focus | Concretely |
|---|---|---|
| 1 | Syntax and the type system | structs, methods, interfaces, **errors as values** (no exceptions — this is the biggest shift from Java) |
| 2 | Memory model basics | slices vs arrays, map semantics, pointers vs values, zero values, `defer` |
| 3 | Concurrency I | goroutines, channels, `select`, `sync.WaitGroup`, `sync.Mutex`; run everything under `-race` |
| 4 | Concurrency II | `context` cancellation, `errgroup`, bounded worker pools, the fan-out/fan-in pattern |
| 5 | Standard library | `os/exec`, `io/fs`, `filepath.WalkDir`, `crypto/sha256`, `encoding/json` |
| 6 | Testing | table-driven tests, `t.Parallel()`, `go test -race`, `testing.B` benchmarks |
| 7 | **Capstone** | A parallel directory hasher: walk a tree, SHA-256 every file with a bounded worker pool, print a stable manifest |

**Gate — all four, in his own words:**

1. Why does a send on an unbuffered channel block, and what does a buffered channel change?
2. What does `go test -race` actually detect, and what class of bug does it *not* catch?
3. What happens to the other goroutines when one returns an error inside an `errgroup`?
4. The day-7 hasher passes `go test -race` and produces byte-identical output across runs.

The hasher becomes the input-digesting layer in Milestone 4. Keep it.

---

## 6. Build order

Each milestone ships something demoable and has an explicit gate.

### M1 — Graph and parser
Parse `build.json` into an action DAG. Each action declares `inputs`, `outputs`, `command`,
`env`, and `deps`. Detect cycles and report **which** actions form the cycle, not just that one
exists. Produce a topological order.

*Gate:* a cyclic manifest produces a readable error naming the participating actions.

### M2 — Sequential executor
Execute actions in dependency order via `os/exec`. Capture stdout/stderr per action. Fail the
build on the first non-zero exit and report which action failed.

*Gate:* builds the real test project correctly, output byte-identical to a plain `make`/script run.

### M3 — Parallel executor
Bounded worker pool. Dispatch an action the moment all its dependencies complete (not in strict
topological batches — that detail is the whole point). `context` cancellation so a failure stops
in-flight work. Record per-action start/end timestamps.

*Gate:* correct builds under `-race` at 1, 2, 4, 8 workers, plus a **speedup curve** and the
computed **critical path length**. The curve should flatten near the critical path — that flattening
is Amdahl's law in your own data, and it is the strongest measured result in the project.

### M4 — Local content-addressed cache
Cache key = SHA-256 over: input file **content** digests, the command line, the declared
environment variables, the toolchain version, and the platform string. Outputs stored in the CAS
by content digest; the action cache maps key → output digests.

Write blobs to a temp file and `rename()` into place so an interrupted write never leaves a
partial blob visible.

*Gate:* clean build vs. no-op rebuild vs. one-file-changed rebuild, all three measured; cache hit
rate reported; the full M4 test list in section 7 passes.

### M5 — Sandboxing and hermeticity
Run each action in a directory containing **only** its declared inputs, assembled as a symlink
farm. An undeclared read now fails loudly instead of silently poisoning the cache.

*Gate:* the false-cache-hit suite (section 7) passes, including the demonstration that removing a
declaration causes a **build failure** rather than a wrong output. Record the before/after — this
is the project's best interview story.

### M6 — Remote cache
A shared cache over HTTP (gRPC optional). `GET /cas/<digest>`, `PUT /cas/<digest>`,
`GET|PUT /ac/<key>`. Decide and document: does a remote miss block, or fall back to local
execution immediately? Handle the remote being unreachable without failing the build.

*Gate:* machine A (or cache dir A) builds cold; machine B builds with a measured cross-machine hit
rate. Remote-down path tested.

### M7 — Stretch, in priority order
1. **CAS garbage collection** — reference counting from the action cache, LRU eviction under a
   size budget. This is a real storage problem and most toy build systems skip it.
2. **Remote Execution API subset** — implement enough of Bazel's published gRPC spec to run
   actions on a remote worker. Interoperating with an industry protocol beats inventing one.

---

## 7. Correctness tests (the heart of the project)

Cache correctness bugs are silent, so the tests are the deliverable, not an afterthought.

**Must-miss (a stale hit here is a wrong build):**

- Input file **content** changes → miss.
- Command line changes → miss.
- A declared environment variable changes → miss.
- Toolchain version changes → miss.
- Platform/OS changes → miss.
- An action's **dependency's output** changes → miss (transitive correctness).

**Must-hit (a spurious miss here means the cache is worthless):**

- Input **mtime** changes but content is identical → **hit**. Content-addressed, not
  timestamp-addressed. This is a deliberate design choice and a good interview question.
- Files reordered in the manifest, same set → hit (canonicalize before hashing).
- Absolute path of the workspace changes → hit (paths must be workspace-relative in the key).

**Hermeticity (M5):**

- An action reads an undeclared file → **build fails** in the sandbox. Without the sandbox, the
  same manifest produces a false hit and a wrong build. Test and record **both** behaviours.
- An action writes an undeclared output → detected and reported.

**Robustness:**

- A CAS blob whose bytes do not match its digest → detected on read, treated as a miss.
- An interrupted cache write leaves no visible partial blob.
- Two workers execute the identical action concurrently → no corruption (document whether you
  deduplicate the work or allow the duplicate and let the atomic rename settle it).
- Nondeterministic output (an action embedding a timestamp) → detect, and document why this
  breaks caching for everyone downstream.

---

## 8. Benchmark protocol — fix this before the first measurement

Written in direct response to LinkForge's inconclusive Redis benchmark, where run-to-run variance
exceeded the effect being measured and the sign was not stable.

1. **One fixed workload**, committed to the repo, named in every result.
2. **Quiet host.** No compiles, no browser, no other benchmarks. Record what else was running.
3. **N = 7 runs minimum** per configuration, interleaved A/B/A/B rather than all-A-then-all-B.
4. Report **median and interquartile range**, never a single run, never a best-of.
5. Discard the first run of each configuration (page cache warming) and say that you did.
6. **If the IQRs overlap, the result is inconclusive** and is written up as inconclusive. That
   outcome is publishable and is not a failure.
7. Wall-clock timings come from BuildForge's own per-action timestamps *and* an external
   `/usr/bin/time` check, so a bug in the instrumentation cannot manufacture a result.

## 9. Metrics to report

| Metric | What it demonstrates |
|---|---|
| Clean build time | Baseline |
| No-op rebuild time | Cache lookup overhead in isolation |
| One-file-changed rebuild | The actual value proposition |
| Cache hit rate (local, then cross-machine) | The cache works, and works for someone else |
| Speedup vs. worker count (1/2/4/8) | Understanding of parallelism limits |
| Wall time vs. critical path length | Scheduler efficiency, honestly bounded |
| CAS size with and without deduplication | Storage design paying off |

## 10. The scale ladder (the interview answer)

Walk the first rungs; know the rest and why each one comes next.

1. **Local cache** — helps one developer, one machine. *(built)*
2. **Shared remote cache** — CI's build warms every developer's. Now: coherence, concurrent
   writers, partial failure. *(built)*
3. **CAS GC and eviction** — the store grows without bound; reference counting plus a size budget.
   *(stretch)*
4. **Remote execution** — actions run on a worker fleet. The build becomes a distributed job
   scheduler, and dependency ordering becomes a distributed scheduling problem.
5. **Cache sharding by digest prefix, graph sharding by target** — when one cache node stops
   being enough.

Each step should be justified by a number you measured at the step before it.

## 11. What this does not claim

Written first, in the README, before any results:

- BuildForge is **not** competitive with Bazel or Buck2 and is not intended to be.
- It does **not** implement persistent workers, dynamic local/remote scheduling, cross-platform
  toolchain resolution, or language-specific rule sets — all of which is most of what makes a
  production build system fast at organizational scale.
- Hermeticity is enforced by a symlink sandbox, which is **weaker** than a container or a
  filesystem-level sandbox. Name the gap.
- All numbers are single-machine on one laptop with one workload. They characterize this
  configuration, not build systems in general.
- Any comparison against Bazel is reported with Bazel winning where it wins, and why — the same
  pattern KernelForge uses to document where cuBLAS still beats the hand-written kernel.

## 12. Repository layout

```
buildforge/
  cmd/buildforge/         main.go — CLI
  internal/
    manifest/             parse build.json, validate
    graph/                DAG, cycle detection, topological order, critical path
    runner/               action execution, process groups, sandbox (M5)
    sched/                worker pool, context cancellation
    cache/
      key/                action key computation
      cas/                content-addressed store, atomic writes, GC
      remote/             HTTP client + server
    digest/               parallel file hasher (from the Milestone 0 capstone)
  testdata/               fixture projects, including the hermeticity fixtures
  bench/
    PROTOCOL.md           section 8, verbatim
    RESULTS.md            every run, including inconclusive ones
  docs/
    what-i-learned.md     the explanation gate, in Mayank's own words
  README.md               "what this does not claim" above the fold
```

## 13. Questions this project answers in an interview

Prepare a two-minute answer to each before calling it resume-ready:

1. How do you compute a cache key, and what happens if you forget to include the toolchain version?
2. Why is mtime the wrong thing to hash, and what does content-addressing cost you?
3. Walk me through a false cache hit. How did you find one, and how does the sandbox prevent it?
4. Your build takes 12s wall with 8 workers and the critical path is 11s. What do you do next?
5. Two machines write the same blob to the remote cache simultaneously. Is that safe? Why?
6. Why Go?

## 14. Schedule

| Week | Milestone |
|---|---|
| 0 | Go ramp + gate |
| 1 | M1, M2 |
| 2 | M3 + speedup curve |
| 3 | M4 + must-hit/must-miss suite |
| 4 | M5 + hermeticity story |
| 5 | M6 + cross-machine numbers |
| 6+ | M7 stretch, `what-i-learned.md`, README |

A defensible stopping point is the end of week 3: a parallel build system with a correct
content-addressed cache and a real test suite is already a complete, uncommon project. Everything
after that is additive, and each milestone is independently shippable.
