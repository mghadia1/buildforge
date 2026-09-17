package runner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mghadia1/buildforge/internal/graph"
	"github.com/mghadia1/buildforge/internal/manifest"
)

// newGraph builds a graph from actions, failing the test if the manifest is
// invalid. Tests declare shell commands via sh -c, which keeps them readable
// and portable across macOS and Linux.
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

// writeFile creates a file inside the workspace.
func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	full := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}

func readFile(t *testing.T, dir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("ReadFile %s: %v", name, err)
	}
	return string(b)
}

func TestBuildRunsInDependencyOrder(t *testing.T) {
	t.Parallel()

	// t.TempDir is removed automatically when the test finishes.
	ws := t.TempDir()
	writeFile(t, ws, "src/a.txt", "alpha")

	g := newGraph(t,
		manifest.Action{
			Name:    "stage",
			Inputs:  []string{"src/a.txt"},
			Outputs: []string{"out/a.staged"},
			Command: sh("cat src/a.txt > out/a.staged"),
		},
		manifest.Action{
			// Depends on stage, so out/a.staged must already exist when this
			// runs. If the order were wrong, the input check would fail.
			Name:    "finish",
			Deps:    []string{"stage"},
			Inputs:  []string{"out/a.staged"},
			Outputs: []string{"out/a.final"},
			Command: sh("cat out/a.staged > out/a.final && echo '!' >> out/a.final"),
		},
	)

	sum, err := Build(context.Background(), g, Options{Workspace: ws})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got, want := len(sum.Results), 2; got != want {
		t.Fatalf("ran %d actions, want %d", got, want)
	}
	if got, want := readFile(t, ws, "out/a.final"), "alpha!\n"; got != want {
		t.Fatalf("out/a.final = %q, want %q", got, want)
	}
}

func TestBuildCreatesOutputDirectories(t *testing.T) {
	t.Parallel()

	ws := t.TempDir()
	g := newGraph(t, manifest.Action{
		// deeply/nested does not exist; the runner must create it, because
		// requiring every manifest to mkdir its own output tree would be a
		// papercut in every single action.
		Name:    "deep",
		Outputs: []string{"deeply/nested/out.txt"},
		Command: sh("echo hi > deeply/nested/out.txt"),
	})

	if _, err := Build(context.Background(), g, Options{Workspace: ws}); err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got, want := readFile(t, ws, "deeply/nested/out.txt"), "hi\n"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

func TestFailureNamesTheActionAndKeepsStderr(t *testing.T) {
	t.Parallel()

	ws := t.TempDir()
	g := newGraph(t, manifest.Action{
		Name:    "broken",
		Outputs: []string{"out/never"},
		Command: sh("echo 'something went wrong' >&2; exit 3"),
	})

	_, err := Build(context.Background(), g, Options{Workspace: ws})
	if err == nil {
		t.Fatal("Build succeeded, want failure")
	}

	var ae *ActionError
	if !errors.As(err, &ae) {
		t.Fatalf("error = %T (%v), want *ActionError", err, err)
	}
	if ae.Action != "broken" {
		t.Errorf("Action = %q, want %q", ae.Action, "broken")
	}
	if ae.ExitCode != 3 {
		t.Errorf("ExitCode = %d, want 3", ae.ExitCode)
	}
	if !strings.Contains(string(ae.Stderr), "something went wrong") {
		t.Errorf("Stderr = %q, want the command's message", ae.Stderr)
	}
}

func TestFailureStopsDependentsButNotSiblings(t *testing.T) {
	t.Parallel()

	ws := t.TempDir()
	g := newGraph(t,
		manifest.Action{
			Name:    "broken",
			Outputs: []string{"out/broken"},
			Command: sh("exit 1"),
		},
		manifest.Action{
			// Downstream of the failure: its input is never produced, so it
			// must be skipped rather than run and fail confusingly.
			Name:    "downstream",
			Deps:    []string{"broken"},
			Inputs:  []string{"out/broken"},
			Outputs: []string{"out/downstream"},
			Command: sh("cp out/broken out/downstream"),
		},
		manifest.Action{
			// Unrelated to the failure. With -k it should still run.
			Name:    "sibling",
			Outputs: []string{"out/sibling"},
			Command: sh("echo ok > out/sibling"),
		},
	)

	sum, err := Build(context.Background(), g, Options{Workspace: ws, KeepGoing: true})
	if err == nil {
		t.Fatal("Build succeeded, want failure")
	}
	if got, want := strings.Join(sum.Failed, ","), "broken"; got != want {
		t.Errorf("Failed = %q, want %q", got, want)
	}
	if got, want := strings.Join(sum.Skipped, ","), "downstream"; got != want {
		t.Errorf("Skipped = %q, want %q", got, want)
	}
	if _, err := os.Stat(filepath.Join(ws, "out/sibling")); err != nil {
		t.Errorf("sibling did not run under KeepGoing: %v", err)
	}
}

func TestFailFastStopsAtTheFirstFailure(t *testing.T) {
	t.Parallel()

	ws := t.TempDir()
	g := newGraph(t,
		manifest.Action{
			// Sorts first, so the ready set drains it before "later".
			Name:    "aaa_broken",
			Outputs: []string{"out/broken"},
			Command: sh("exit 1"),
		},
		manifest.Action{
			Name:    "zzz_later",
			Outputs: []string{"out/later"},
			Command: sh("echo ran > out/later"),
		},
	)

	if _, err := Build(context.Background(), g, Options{Workspace: ws}); err == nil {
		t.Fatal("Build succeeded, want failure")
	}
	if _, err := os.Stat(filepath.Join(ws, "out/later")); !os.IsNotExist(err) {
		t.Error("zzz_later ran, but the build should have stopped at the first failure")
	}
}

func TestMissingDeclaredInputIsReportedBeforeRunning(t *testing.T) {
	t.Parallel()

	ws := t.TempDir()
	g := newGraph(t, manifest.Action{
		Name:    "needs_input",
		Inputs:  []string{"src/absent.txt"},
		Outputs: []string{"out/x"},
		Command: sh("echo should-not-run > out/x"),
	})

	_, err := Build(context.Background(), g, Options{Workspace: ws})
	if err == nil {
		t.Fatal("Build succeeded, want failure")
	}
	if !strings.Contains(err.Error(), "needs_input") {
		t.Errorf("error = %q, want it to name the action", err)
	}
	if _, statErr := os.Stat(filepath.Join(ws, "out/x")); !os.IsNotExist(statErr) {
		t.Error("the command ran despite a missing declared input")
	}
}

func TestUnproducedOutputIsAFailure(t *testing.T) {
	t.Parallel()

	ws := t.TempDir()
	g := newGraph(t, manifest.Action{
		// Exits zero and produces nothing. Milestone 4 stores outputs by
		// digest, so an action that lies about what it writes would poison the
		// cache. Catch it at execution time instead.
		Name:    "liar",
		Outputs: []string{"out/promised"},
		Command: sh("true"),
	})

	_, err := Build(context.Background(), g, Options{Workspace: ws})
	if err == nil {
		t.Fatal("Build succeeded, want failure")
	}
	if !strings.Contains(err.Error(), "liar") {
		t.Errorf("error = %q, want it to name the action", err)
	}
}

func TestEnvironmentIsNotInherited(t *testing.T) {
	// Not parallel: it sets a process-wide environment variable.
	ws := t.TempDir()

	// t.Setenv restores the previous value when the test finishes.
	t.Setenv("BUILDFORGE_LEAK_CHECK", "leaked")

	g := newGraph(t, manifest.Action{
		Name:    "probe",
		Outputs: []string{"out/env.txt"},
		Env:     map[string]string{"DECLARED": "visible"},
		Command: sh(`echo "declared=${DECLARED-unset} leaked=${BUILDFORGE_LEAK_CHECK-unset}" > out/env.txt`),
	})

	if _, err := Build(context.Background(), g, Options{Workspace: ws}); err != nil {
		t.Fatalf("Build: %v", err)
	}

	// An inherited variable can change an action's output while being invisible
	// to the cache key. That is the exact shape of a false cache hit, so the
	// parent environment must not reach the command.
	got := strings.TrimSpace(readFile(t, ws, "out/env.txt"))
	want := "declared=visible leaked=unset"
	if got != want {
		t.Fatalf("env = %q, want %q", got, want)
	}
}

func TestDeclaredEnvOverridesInheritedPath(t *testing.T) {
	t.Parallel()

	ws := t.TempDir()
	g := newGraph(t, manifest.Action{
		// PATH is passed through so commands are findable, but an action that
		// declares its own PATH must win: os/exec keeps the last value for a
		// duplicated key, and declared variables are appended last.
		Name:    "override",
		Outputs: []string{"out/path.txt"},
		Env:     map[string]string{"PATH": "/custom/bin"},
		Command: sh(`echo "$PATH" > out/path.txt`),
	})

	if _, err := Build(context.Background(), g, Options{Workspace: ws}); err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got, want := strings.TrimSpace(readFile(t, ws, "out/path.txt")), "/custom/bin"; got != want {
		t.Fatalf("PATH = %q, want %q", got, want)
	}
}

func TestCancellationStopsTheBuild(t *testing.T) {
	t.Parallel()

	ws := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())

	g := newGraph(t,
		manifest.Action{
			Name:    "aaa_slow",
			Outputs: []string{"out/slow"},
			Command: sh("sleep 30; echo done > out/slow"),
		},
		manifest.Action{
			Name:    "zzz_after",
			Outputs: []string{"out/after"},
			Command: sh("echo ran > out/after"),
		},
	)

	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, err := Build(ctx, g, Options{Workspace: ws})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Build succeeded, want cancellation")
	}
	// Unwrap has to reach context.Canceled through the ActionError, or a caller
	// cannot tell "we stopped the build" from "this command is broken".
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want it to unwrap to context.Canceled", err)
	}
	if elapsed > 10*time.Second {
		t.Fatalf("Build took %s; the sleeping command was not killed", elapsed)
	}
}

func TestResultRecordsTiming(t *testing.T) {
	t.Parallel()

	ws := t.TempDir()
	g := newGraph(t, manifest.Action{
		Name:    "timed",
		Outputs: []string{"out/t"},
		Command: sh("sleep 0.05; echo done > out/t"),
	})

	sum, err := Build(context.Background(), g, Options{Workspace: ws})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	// Milestone 3's critical-path measurement depends on these timestamps, so
	// they are checked from the moment they are collected.
	d := sum.Results[0].Duration()
	if d < 40*time.Millisecond {
		t.Fatalf("Duration = %s, want at least the 50ms the action slept", d)
	}
}

func TestSummaryAccountsForEveryAction(t *testing.T) {
	t.Parallel()

	// Succeeded + Failed + Skipped must equal the number of actions in the
	// graph, in both fail-fast and keep-going mode. Under fail-fast the actions
	// after the failure are never reached, and they still have to be counted.
	for _, keepGoing := range []bool{false, true} {
		keepGoing := keepGoing
		name := "fail-fast"
		if keepGoing {
			name = "keep-going"
		}

		t.Run(name, func(t *testing.T) {
			t.Parallel()

			ws := t.TempDir()
			g := newGraph(t,
				manifest.Action{
					Name:    "a_ok",
					Outputs: []string{"out/a"},
					Command: sh("echo a > out/a"),
				},
				manifest.Action{
					Name:    "b_broken",
					Deps:    []string{"a_ok"},
					Outputs: []string{"out/b"},
					Command: sh("exit 1"),
				},
				manifest.Action{
					Name:    "c_downstream",
					Deps:    []string{"b_broken"},
					Inputs:  []string{"out/b"},
					Outputs: []string{"out/c"},
					Command: sh("cp out/b out/c"),
				},
				manifest.Action{
					Name:    "d_unrelated",
					Outputs: []string{"out/d"},
					Command: sh("echo d > out/d"),
				},
			)

			sum, err := Build(context.Background(), g, Options{Workspace: ws, KeepGoing: keepGoing})
			if err == nil {
				t.Fatal("Build succeeded, want failure")
			}

			total := sum.Succeeded() + len(sum.Failed) + len(sum.Skipped)
			if total != 4 {
				t.Fatalf("ok=%d failed=%d skipped=%d sums to %d, want 4",
					sum.Succeeded(), len(sum.Failed), len(sum.Skipped), total)
			}
		})
	}
}
