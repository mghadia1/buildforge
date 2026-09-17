package sched

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/mghadia1/buildforge/internal/graph"
	"github.com/mghadia1/buildforge/internal/manifest"
	"github.com/mghadia1/buildforge/internal/runner"
)

func newGraph(t *testing.T, actions ...manifest.Action) *graph.Graph {
	t.Helper()

	m := &manifest.Manifest{Actions: actions}
	if err := m.Validate(); err != nil {
		t.Fatalf("invalid test manifest: %v", err)
	}
	g, err := graph.New(m)
	if err != nil {
		t.Fatalf("graph.New: %v", err)
	}
	return g
}

func sh(script string) []string { return []string{"/bin/sh", "-c", script} }

func readFile(t *testing.T, dir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("ReadFile %s: %v", name, err)
	}
	return string(b)
}

// chain builds n actions in a straight line, each sleeping for d.
func chain(t *testing.T, n int, d string) *graph.Graph {
	t.Helper()

	var actions []manifest.Action
	for i := 0; i < n; i++ {
		a := manifest.Action{
			Name:    fmt.Sprintf("step%02d", i),
			Outputs: []string{fmt.Sprintf("out/%02d", i)},
			Command: sh(fmt.Sprintf("sleep %s; echo %d > out/%02d", d, i, i)),
		}
		if i > 0 {
			a.Deps = []string{fmt.Sprintf("step%02d", i-1)}
			a.Inputs = []string{fmt.Sprintf("out/%02d", i-1)}
		}
		actions = append(actions, a)
	}
	return newGraph(t, actions...)
}

// fan builds n independent actions, each sleeping for d.
func fan(t *testing.T, n int, d string) *graph.Graph {
	t.Helper()

	var actions []manifest.Action
	for i := 0; i < n; i++ {
		actions = append(actions, manifest.Action{
			Name:    fmt.Sprintf("leaf%02d", i),
			Outputs: []string{fmt.Sprintf("out/%02d", i)},
			Command: sh(fmt.Sprintf("sleep %s; echo %d > out/%02d", d, i, i)),
		})
	}
	return newGraph(t, actions...)
}

func TestParallelRespectsDependencies(t *testing.T) {
	t.Parallel()

	// Each step reads the previous step's output, so a scheduler that starts an
	// action before its dependency finishes fails the input check.
	ws := t.TempDir()
	g := chain(t, 5, "0.01")

	sum, err := Build(context.Background(), g, Options{
		Options: runner.Options{Workspace: ws},
		Workers: 8,
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got, want := sum.Succeeded(), 5; got != want {
		t.Fatalf("Succeeded = %d, want %d", got, want)
	}
	if got, want := strings.TrimSpace(readFile(t, ws, "out/04")), "4"; got != want {
		t.Fatalf("out/04 = %q, want %q", got, want)
	}
}

func TestParallelActuallyOverlaps(t *testing.T) {
	t.Parallel()

	// Eight independent 100ms actions across 8 workers should take roughly
	// 100ms, not 800ms. The bound is deliberately loose: this asserts that work
	// overlaps at all, and is not a performance measurement. Real numbers come
	// from bench/speedup.py under the protocol in PROJECT_SPEC.md section 8.
	ws := t.TempDir()
	g := fan(t, 8, "0.1")

	start := time.Now()
	sum, err := Build(context.Background(), g, Options{
		Options: runner.Options{Workspace: ws},
		Workers: 8,
	})
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got, want := sum.Succeeded(), 8; got != want {
		t.Fatalf("Succeeded = %d, want %d", got, want)
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("8 independent 100ms actions took %s on 8 workers; they did not overlap", elapsed)
	}
}

func TestWorkerLimitIsRespected(t *testing.T) {
	t.Parallel()

	// With one worker, eight independent 50ms actions must serialize: no two
	// intervals may overlap. This is the test that would catch a semaphore that
	// is acquired but never released.
	ws := t.TempDir()
	g := fan(t, 8, "0.05")

	sum, err := Build(context.Background(), g, Options{
		Options: runner.Options{Workspace: ws},
		Workers: 1,
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	rs := append([]*runner.Result(nil), sum.Results...)
	sort.Slice(rs, func(i, j int) bool { return rs[i].Start.Before(rs[j].Start) })
	for i := 1; i < len(rs); i++ {
		if rs[i].Start.Before(rs[i-1].End) {
			t.Fatalf("%s started at %s, before %s ended at %s: more than one worker ran",
				rs[i].Action, rs[i].Start, rs[i-1].Action, rs[i-1].End)
		}
	}
}

func TestParallelMatchesSequentialOutput(t *testing.T) {
	t.Parallel()

	// The differential test. The sequential driver is the reference
	// implementation; a parallel scheduler that is fast and subtly wrong is
	// caught here and almost nowhere else.
	actions := []manifest.Action{
		{Name: "gen_a", Outputs: []string{"out/a"}, Command: sh("echo alpha > out/a")},
		{Name: "gen_b", Outputs: []string{"out/b"}, Command: sh("echo beta > out/b")},
		{
			Name:    "merge",
			Deps:    []string{"gen_a", "gen_b"},
			Inputs:  []string{"out/a", "out/b"},
			Outputs: []string{"out/merged"},
			Command: sh("cat out/a out/b > out/merged"),
		},
		{
			Name:    "count",
			Deps:    []string{"merge"},
			Inputs:  []string{"out/merged"},
			Outputs: []string{"out/count"},
			Command: sh("wc -l < out/merged | tr -d ' ' > out/count"),
		},
	}

	seqWS := t.TempDir()
	g := newGraph(t, actions...)
	if _, err := runner.Build(context.Background(), g, runner.Options{Workspace: seqWS}); err != nil {
		t.Fatalf("sequential Build: %v", err)
	}

	for _, workers := range []int{1, 2, 4, 8} {
		workers := workers
		t.Run(fmt.Sprintf("workers=%d", workers), func(t *testing.T) {
			t.Parallel()

			parWS := t.TempDir()
			g := newGraph(t, actions...)
			if _, err := Build(context.Background(), g, Options{
				Options: runner.Options{Workspace: parWS},
				Workers: workers,
			}); err != nil {
				t.Fatalf("parallel Build: %v", err)
			}

			for _, f := range []string{"out/a", "out/b", "out/merged", "out/count"} {
				if got, want := readFile(t, parWS, f), readFile(t, seqWS, f); got != want {
					t.Errorf("%s: parallel = %q, sequential = %q", f, got, want)
				}
			}
		})
	}
}

func TestFailureSkipsDownstreamAndCancelsSiblings(t *testing.T) {
	t.Parallel()

	ws := t.TempDir()
	g := newGraph(t,
		manifest.Action{
			Name:    "broken",
			Outputs: []string{"out/broken"},
			Command: sh("exit 7"),
		},
		manifest.Action{
			Name:    "downstream",
			Deps:    []string{"broken"},
			Inputs:  []string{"out/broken"},
			Outputs: []string{"out/downstream"},
			Command: sh("cp out/broken out/downstream"),
		},
		manifest.Action{
			// Long enough that fail-fast must cancel it mid-flight rather than
			// letting it finish.
			Name:    "slow_sibling",
			Outputs: []string{"out/slow"},
			Command: sh("sleep 30; echo done > out/slow"),
		},
	)

	start := time.Now()
	sum, err := Build(context.Background(), g, Options{
		Options: runner.Options{Workspace: ws},
		Workers: 4,
	})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Build succeeded, want failure")
	}
	var ae *runner.ActionError
	if !errors.As(err, &ae) {
		t.Fatalf("error = %T (%v), want *runner.ActionError", err, err)
	}
	if ae.Action != "broken" || ae.ExitCode != 7 {
		t.Errorf("error = %+v, want action broken with exit code 7", ae)
	}
	if elapsed > 10*time.Second {
		t.Fatalf("build took %s; the sleeping sibling was not cancelled", elapsed)
	}
	if got := strings.Join(sum.Skipped, ","); !strings.Contains(got, "downstream") {
		t.Errorf("Skipped = %q, want it to include downstream", got)
	}
	if total := sum.Succeeded() + len(sum.Failed) + len(sum.Skipped); total != 3 {
		t.Errorf("ok=%d failed=%d skipped=%d sums to %d, want 3",
			sum.Succeeded(), len(sum.Failed), len(sum.Skipped), total)
	}
}

func TestKeepGoingRunsIndependentWork(t *testing.T) {
	t.Parallel()

	ws := t.TempDir()
	g := newGraph(t,
		manifest.Action{Name: "broken", Outputs: []string{"out/broken"}, Command: sh("exit 1")},
		manifest.Action{
			Name: "downstream", Deps: []string{"broken"},
			Inputs: []string{"out/broken"}, Outputs: []string{"out/downstream"},
			Command: sh("cp out/broken out/downstream"),
		},
		manifest.Action{Name: "unrelated", Outputs: []string{"out/unrelated"}, Command: sh("echo ok > out/unrelated")},
	)

	sum, err := Build(context.Background(), g, Options{
		Options: runner.Options{Workspace: ws, KeepGoing: true},
		Workers: 4,
	})
	if err == nil {
		t.Fatal("Build succeeded, want failure")
	}
	if got, want := strings.Join(sum.Failed, ","), "broken"; got != want {
		t.Errorf("Failed = %q, want %q", got, want)
	}
	if got, want := strings.Join(sum.Skipped, ","), "downstream"; got != want {
		t.Errorf("Skipped = %q, want %q", got, want)
	}
	if got, want := strings.TrimSpace(readFile(t, ws, "out/unrelated")), "ok"; got != want {
		t.Errorf("unrelated output = %q, want %q", got, want)
	}
}

func TestCancellationStopsTheBuild(t *testing.T) {
	t.Parallel()

	ws := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	g := fan(t, 4, "30")

	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, err := Build(ctx, g, Options{Options: runner.Options{Workspace: ws}, Workers: 4})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Build succeeded, want cancellation")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want it to unwrap to context.Canceled", err)
	}
	if elapsed > 10*time.Second {
		t.Fatalf("Build took %s; the sleeping commands were not killed", elapsed)
	}
}

func TestCycleIsReportedBeforeAnythingRuns(t *testing.T) {
	t.Parallel()

	ws := t.TempDir()
	g := newGraph(t,
		manifest.Action{Name: "a", Deps: []string{"b"}, Outputs: []string{"out/a"}, Command: sh("echo a > out/a")},
		manifest.Action{Name: "b", Deps: []string{"a"}, Outputs: []string{"out/b"}, Command: sh("echo b > out/b")},
	)

	// Without the up-front check, every goroutine would block forever waiting
	// on a dependency that never completes, and the build would deadlock
	// instead of reporting the cycle.
	done := make(chan error, 1)
	go func() {
		_, err := Build(context.Background(), g, Options{Options: runner.Options{Workspace: ws}})
		done <- err
	}()

	select {
	case err := <-done:
		var ce *graph.CycleError
		if !errors.As(err, &ce) {
			t.Fatalf("error = %v, want *graph.CycleError", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Build deadlocked on a cyclic graph")
	}
}

func TestCriticalPathBoundsWallTime(t *testing.T) {
	t.Parallel()

	// A chain cannot be parallelized, so wall time can never beat the critical
	// path however many workers are available. This is the relationship the
	// speedup curve in Milestone 3's gate is measured against.
	ws := t.TempDir()
	g := chain(t, 4, "0.05")

	sum, err := Build(context.Background(), g, Options{
		Options: runner.Options{Workspace: ws},
		Workers: 8,
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	path, length := graph.CriticalPath(g, Durations(sum))
	if got, want := strings.Join(path, ","), "step00,step01,step02,step03"; got != want {
		t.Errorf("critical path = %q, want %q", got, want)
	}
	if sum.Elapsed < length {
		t.Errorf("elapsed %s is shorter than the critical path %s, which is impossible",
			sum.Elapsed, length)
	}
}
