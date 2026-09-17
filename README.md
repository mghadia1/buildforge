# BuildForge

An incremental build system with a content-addressed cache, written in Go.

> **What this does not claim.** BuildForge is not competitive with Bazel or Buck2 and is not
> trying to be. It has no persistent workers, no dynamic local/remote scheduling, no
> cross-platform toolchain resolution, and no language-specific rule sets — which is most of what
> makes a production build system fast at organizational scale. Every number in this repository
> comes from one laptop running one workload, and characterizes that configuration only.

> **Status: Milestone 7 of 7, partially.** Manifest parsing, the action DAG, cycle detection, a
> deterministic topological order, a sequential executor, a parallel scheduler with critical-path
> reporting, a sandboxed content-addressed cache, a cache shared between machines over HTTP, and a
> garbage collector with reference counting and LRU eviction. **The Remote Execution API subset is
> not built** — see the roadmap.
> `learning_status: learning`. Not on a resume until the explanation gate in
> [`PROJECT_SPEC.md`](PROJECT_SPEC.md) passes.

## Why

A build system looks like tooling and is really a concurrency, caching, and distributed-state
project: a DAG scheduler with a bounded worker pool, a content-addressed blob store with
deduplication and eviction, and a shared cache with concurrent writers and partial failure. The
central bug of the domain — an action reads a file it did not declare, so the cache key misses it,
so a later build gets a **false cache hit and a silently wrong output** — is reproducible on
demand here, and the fix is demonstrable.

Full design, milestone gates, correctness suite, and benchmark protocol: [`PROJECT_SPEC.md`](PROJECT_SPEC.md).

## Quickstart

```bash
go test ./... -race          # 76 tests, 109 cases with subtests
go run ./cmd/buildforge graph    -f testdata/simple.json
go run ./cmd/buildforge graph    -f testdata/cycle.json          # names the cycle
go run ./cmd/buildforge validate -f testdata/bad.json            # reports every problem at once
go run ./cmd/buildforge build    -f testdata/cproject/build.json # compiles a real C program
```

`build` on the bundled C project runs the actions in dependency order, in parallel by default
(`-j` bounds how many run at once, `-seq` uses the reference sequential driver):

```
run  compile_main
run  compile_util
run  link
3 ok, 0 failed, 0 skipped in 432ms
critical path 432ms: compile_main -> link
```

The resulting `out/main.o`, `out/util.o`, and `out/app` are **byte-identical** to the same three
commands run by hand from a shell script. That equivalence is the Milestone 2 gate: the parallel
scheduler in Milestone 3 has to produce the same bytes, and it needs a known-good result to be
compared against.

When an action fails, the failing action is named and its stderr is surfaced rather than buried:

```
1 ok, 1 failed, 1 skipped in 288ms

--- compile_util stderr ---
src/util.c:1:36: error: expected expression
    1 | int add(int a, int b) { return a + ; }
      |                                    ^
buildforge: action "compile_util" failed with exit code 1
```

`graph` on `testdata/simple.json` prints the execution order:

```
  1. compile_main
  2. compile_util
  3. link  (after compile_main, compile_util)
```

`graph` on `testdata/cycle.json` fails with the actions responsible — not just "cycle detected":

```
buildforge: dependency cycle: compile -> generate_header -> compile
```

Note that `docs`, which is not on the cycle, is not blamed. Pointing at the wrong target is what
makes cycle errors useless in a build file with hundreds of them.

## The manifest

Dependencies are declared, never discovered. A dependency BuildForge cannot see is one it cannot
put in a cache key, and a cache key with a missing input is a wrong build waiting to happen.

```json
{
  "actions": [
    {
      "name": "link",
      "deps": ["compile_main", "compile_util"],
      "inputs": ["out/main.o", "out/util.o"],
      "outputs": ["out/app"],
      "command": ["cc", "-o", "out/app", "out/main.o", "out/util.o"],
      "env": { "SOURCE_DATE_EPOCH": "0" }
    }
  ]
}
```

Three validation rules that exist for cache reasons, not style reasons:

| Rule | Why |
|---|---|
| Paths must be workspace-relative | An absolute path bakes the checkout location into the cache key, so no two machines ever share an entry |
| Every action must declare an output | The action cache maps a key to output digests; an action with no outputs has nothing to restore on a hit |
| Unknown JSON fields are rejected | `"output"` instead of `"outputs"` would parse cleanly into an action that claims to write nothing, and then cache wrongly |

Validation reports **every** problem in one pass rather than stopping at the first.

## Layout

```
cmd/buildforge/      CLI: validate, graph, build
internal/manifest/   parse and validate build.json
internal/graph/      DAG, cycle detection, topological order
internal/runner/     action execution, process groups, the sequential reference driver
internal/sched/      the parallel scheduler
internal/digest/     SHA-256 of files, concurrently
internal/cache/      action keys, the content-addressed store, the action cache
internal/remote/     the cache server, its client, and the local+remote tier
bench/               protocol, harness, and results
testdata/            fixtures: a cycle, an invalid manifest, two real C projects
```

## Execution rules, and the cache reasons behind them

| Rule | Why |
|---|---|
| The parent environment is **not** inherited | An inherited variable can change an action's output while being invisible to the cache key. That is the exact shape of a false cache hit. Only declared variables are passed. |
| Declared inputs are checked before the command runs | Turns a manifest mistake into a clear error instead of an obscure one from the compiler. |
| Declared outputs are checked after it exits | An action that exits zero without producing what it promised would poison the cache in Milestone 4, which stores outputs by digest. |
| Each action runs in its own process group | So cancellation kills its children too. See below. |
| The cache key covers inputs, command, env, **tool binary**, and platform | Anything omitted is a false hit waiting to happen. The tool is in there because a compiler upgrade changes every object it produces. |
| Modification times are **not** in the key | `git checkout` and `touch` change every mtime and not one byte of content. Hashing mtime would invalidate the cache for nothing. |
| Absolute paths are **not** in the key | Two checkouts at different paths — and two machines — must be able to share a cache. This is what Milestone 6 is built on. |
| Key components are length-prefixed | Plain concatenation would make `["ab","c"]` and `["a","bc"]` hash identically, and one command would return the other'"'"'s outputs. |
| A damaged cache falls back to running the action | A cache must cost time, never correctness. Blobs are verified against their digest on the way out. |
| Actions are sandboxed by default | An undeclared input is not in the cache key, so reading one is how a false hit is born. `-no-sandbox` is a debugging aid, not a way to build. |

**The holes that remain.** The sandbox is a symlink farm, not a container. It controls which files
*inside the workspace* an action can see; it does not stop an action reading `/usr/include`, its own
configuration files, or anything else by absolute path, and it does not stop an action writing
through a symlink to modify a declared input in place. Closing those needs a real filesystem
sandbox. `PATH` is still passed through and still absent from the cache key — the tool binary's own
digest is in the key, so a compiler *upgrade* is caught, but a compiler that reads a changed config
file or links a changed shared library is not.

## Garbage collection

A cache with no eviction is a disk filling up. `buildforge gc` runs two phases, and they answer
different questions.

```
$ buildforge gc -cache .buildforge -max 40KB
scanned   38 entries, 38 blobs, 128.71 KB
garbage   0 unreferenced blobs
evicted   29 entries, 29 blobs (budget 40.00 KB)
freed     88.97 KB, now 39.74 KB

$ buildforge build -f build.json -q          # same build, collected cache
34 ok (9 cached), 0 failed, 0 skipped in 229ms
```

**Phase one is reachability.** Reference counts are computed from the action cache into the CAS; a
blob nothing points at is garbage and deleting it costs nothing. These accumulate from interrupted
builds, which write blobs before the entry that would reference them.

**Phase two is capacity, and it is a policy choice rather than a fact.** Everything left is still
referenced, so evicting means some future build will miss. Eviction works **at entry granularity**:
an entry is dropped, and only then are blobs whose reference count fell to zero removed. Evicting
an individual blob instead would leave every entry that shared it half-restorable — safe, because
`Lookup` treats a missing blob as a miss, but the space would not actually be reclaimed and the
dead entries would linger.

The last line above is the property that matters: after evicting 29 of 38 entries, the build is
still correct. Nine entries survived and still hit; the rest re-ran. **Eviction costs time, never
correctness.**

`-n` is a dry run. `-min-age` (default 1h) protects recently written files, because a build in
progress writes blobs before the entry that references them and for a moment they look exactly like
garbage. **That is a mitigation, not a solution** — GC is not safe to run during a build, and
closing the window properly needs a lock or a two-phase mark. Neither is implemented, and the code
says so where the decision lives.

## The shared cache

One machine builds; every other machine gets it for free. Start a server, point builds at it:

```bash
buildforge serve -addr :8080 -cache /var/cache/buildforge
```

```
# machine A, cold
18 ok (0 cached), 0 failed, 0 skipped in 640ms
remote: 0 hits, 0 local hits, 18 misses, 18 uploads, 0 errors

# machine B, different path, empty local cache
18 ok (18 cached), 0 failed, 0 skipped in 12ms
remote: 18 hits, 0 local hits, 0 misses, 0 uploads, 0 errors

# machine B again, local cache now warm
18 ok (18 cached), 0 failed, 0 skipped in 3ms
remote: 0 hits, 18 local hits, 0 misses, 0 uploads, 0 errors
```

Machine B reproduced a build it never ran, from a different directory, with nothing in its own
cache. **That works only because a cache key contains no absolute paths and no modification
times** — the Milestone 4 decision that had no visible payoff until now.

Four design decisions worth asking about:

| Decision | Reasoning |
|---|---|
| Local first, then remote | A local hit is a file read; a remote hit is a round trip. A remote hit is written into the local store on the way past, so the next build does not pay twice. |
| **The remote may never fail a build** | Server down, slow, or serving damaged bytes → the action runs locally, exactly as if the cache were empty. Every failure is counted rather than swallowed. |
| Blobs are re-verified on arrival | These bytes came from another machine. A server that could install arbitrary content as an action'"'"'s output would be worse than no cache. |
| The entry is uploaded **last** | Until the entry exists, nothing can find those blobs. Publishing it first would advertise an entry whose outputs are not all uploaded yet, and another machine would take that as a hit. |

Concurrent writers need no locking: two clients uploading the same blob are by definition uploading
the same bytes, and the store renames into place atomically. That is not luck — it is what content
addressing buys. A cache whose value was not determined by its key would need coordination here.

Measured: **16.7× against a cold build, 34/34 actions restored across machines**
([`bench/RESULTS.md`](bench/RESULTS.md)). The server in that measurement is on localhost, which
excludes latency, bandwidth, TLS, and contention — so it shows the mechanism works and is not a
prediction of what a shared cache is worth across a building.

No authentication and no TLS. This is a cache for a trusted network, and `serve` prints that on
startup rather than implying otherwise.

## The false cache hit, and the sandbox that stops it

This is the bug the whole project is built around, reproduced on a real C program. The manifest in
[`testdata/underdeclared/`](testdata/underdeclared) compiles `src/main.c`, which includes
`src/config.h` — and declares only the `.c` file. Forgetting a header is the most ordinary mistake
there is in a hand-written build file.

**Without the sandbox**, edit the undeclared header and rebuild:

```
$ buildforge build -f build.json -q -no-sandbox && ./out/app
2 ok (0 cached), 0 failed, 0 skipped in 342ms
answer = 42

$ sed -i '' 's/ANSWER 42/ANSWER 99/' src/config.h     # the header now says 99

$ buildforge build -f build.json -q -no-sandbox && ./out/app
2 ok (2 cached), 0 failed, 0 skipped in 1ms
answer = 42
```

The build is green. The cache reports a perfect hit. The binary is **wrong** — and nothing anywhere
says so. `config.h` was never declared, so it was never in the cache key, so changing it changed
nothing the cache could see.

**With the sandbox** (the default), the same manifest fails at the moment it became wrong:

```
$ buildforge build -f build.json -q
0 ok (0 cached), 1 failed, 1 skipped in 32ms

--- compile stderr ---
src/main.c:2:10: fatal error: '"'"'config.h'"'"' file not found
    2 | #include "config.h"
      |          ^~~~~~~~~~
buildforge: action "compile" failed with exit code 1
```

Each action runs in a directory containing symlinks to exactly its declared inputs and nothing
else, so an undeclared read is simply a missing file. The manifest breaks loudly when it is
written, instead of lying months later when somebody edits the file nobody declared.

Symlinks rather than copies, because copying every input of every action would dominate the build.
They are hermetic for the case that matters — a compiler resolves `#include "x.h"` against the path
it was handed, which is the path inside the sandbox — and that was verified before the design was
committed to, not assumed.

Undeclared *outputs* are handled by the same mechanism: only declared outputs are harvested out of
the sandbox, so a file an action wrote without declaring it is deleted with the sandbox rather than
becoming an invisible dependency for whatever runs next.

**What hermeticity costs:** a small single-digit percentage on this workload. The measurement is
the weakest one in [`bench/RESULTS.md`](bench/RESULTS.md) and says so — the no-op row, which the
sandbox cannot affect at all, differs by 9 % between sessions, which bounds how much of the rest is
signal.

## Measured: the cache

Same 34-action workload. Median of 7 interleaved rounds, first discarded. Full conditions in
[`bench/RESULTS.md`](bench/RESULTS.md).

| Config | Median | IQR | vs clean | Cached |
|---|---|---|---|---|
| clean (empty cache) | 0.2823 s | [0.2612, 0.3290] | 1.0× | 0/34 |
| no-op rebuild | 0.0098 s | [0.0096, 0.0100] | **28.7×** | 34/34 |
| cosmetic edit (a comment) | 0.0374 s | [0.0370, 0.0404] | 7.5× | 33/34 |
| semantic edit (a constant) | 0.0753 s | [0.0727, 0.0768] | 3.8× | 32/34 |

### Early cutoff

The two edit rows differ by one character of intent and by a factor of two in wall time.

Adding a **comment** to a source file changes its digest, so the compile re-runs. But the object
file it produces is byte-identical — a comment does not survive into `.o` — so the link's inputs
are unchanged, its key is unchanged, and it hits. Changing a **constant** re-runs the same compile,
but now the object really differs, so the link re-runs too.

Invalidation stops where content stops changing instead of cascading downstream. Nothing implements
this; it falls out of keying on content rather than on timestamps. A timestamp-based system
re-links in both cases, because it cannot tell that the new `.o` is the old `.o`.

Deduplication, honestly: this workload's 34 outputs are all distinct, so the CAS holds 34 blobs and
saves nothing. `TestBlobsAreDeduplicated` proves the mechanism; the benchmark shows it buying
nothing *here*.

## Measured: the speedup curve

34 actions — 33 independent `cc -O2` compiles feeding one link — on an Apple M2 with 8 logical
cores. Median of 7 interleaved runs per configuration, first discarded, IQR in brackets. Full
conditions and caveats in [`bench/RESULTS.md`](bench/RESULTS.md).

| Config | Median | IQR | Speedup |
|---|---|---|---|
| `seq` | 0.913 s | [0.887, 0.988] | 0.98× |
| `j=1` | 0.896 s | [0.879, 0.990] | 1.00× |
| `j=2` | 0.525 s | [0.511, 0.543] | 1.71× |
| `j=4` | 0.310 s | [0.309, 0.316] | 2.89× |
| `j=8` | 0.250 s | [0.246, 0.262] | 3.58× |

Two things worth reading off it. **`seq` and `j=1` are inconclusive** — their IQRs overlap, so the
scheduler adds no overhead this experiment can detect, which is what you want from it. And the
speedup **flattens well short of 8×**, which is not the graph's fault: this DAG is 33 wide and two
deep, so nothing structural stops it. The M2's 8 logical cores are 4 performance plus 4 efficiency,
and 4 → 8 workers buying 1.24× is the shape heterogeneity predicts. That explanation is plausible
and **not proven here**.

A caveat found while checking rather than while planning: the per-run critical path is a maximum
over 33 noisy durations, so it swings (551, 136, 145, 98 ms across four runs of the same workload)
and inflates under contention. It is a diagnostic for one build, not a measurement of the graph.

## What the tests caught

The cancellation test failed on its first run, taking the full 30 seconds it was meant to prove
could not happen.

`exec.CommandContext` kills the command's own process — but `sh -c "sleep 30; ..."` forks a child
for `sleep`, so killing the shell left `sleep` running. The orphan had inherited the stdout pipe,
so `cmd.Wait` blocked until it finished on its own. Cancelling the build did nothing for thirty
seconds.

The fix is what real build systems do: `Setpgid` puts each action in its own process group, and
cancellation signals the whole group via the negative PID, so children die with their parent.
`WaitDelay` is the backstop — if anything still holds the pipe open after the kill, os/exec closes
the pipes and returns rather than hanging. A build tool that ignores Ctrl-C is worse than one that
fails.

## Roadmap

| # | Milestone | State |
|---|---|---|
| 0 | Go ramp | gate in `PROJECT_SPEC.md` §5 |
| 1 | Graph, parser, cycle detection | **Done** |
| 2 | Sequential executor | **Done** |
| 3 | Parallel executor, speedup curve, critical path | **Done** |
| 4 | Local content-addressed cache | **Done** |
| 5 | Sandboxing and hermeticity | **Done** |
| 6 | Remote cache | **Done** |
| 7a | CAS garbage collection, LRU eviction | **Done** |
| 7b | Remote Execution API subset | **Not built** — see below |

## What is deliberately not built

**The Bazel Remote Execution API subset (Milestone 7b).** Running actions on a remote worker fleet
needs a gRPC and protobuf dependency, a worker protocol, and an input-tree upload path — a
milestone's worth of work in its own right, and a shallow version would be worse than none, because
the interesting parts of that API are exactly the parts a sketch would skip. The remote *cache*
here is a plain HTTP protocol of BuildForge's own, not an implementation of anyone's spec.

Also absent, in rough order of how much they would matter: persistent workers, dynamic
local/remote scheduling, cross-platform toolchain resolution, and language-specific rule sets —
which together are most of what makes a production build system fast at organizational scale.
