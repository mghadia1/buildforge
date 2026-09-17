package runner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mghadia1/buildforge/internal/cache"
	"github.com/mghadia1/buildforge/internal/manifest"
)

// underDeclared is the manifest at the centre of this milestone. The action
// reads two files and declares one.
//
// Nothing about it is exotic: forgetting to declare a header is the single most
// common mistake in a hand-written build file, and it is invisible until
// somebody edits the file nobody declared.
func underDeclared() []manifest.Action {
	return []manifest.Action{{
		Name:    "compile",
		Inputs:  []string{"src/main.c"}, // src/app.h is read but NOT declared
		Outputs: []string{"out/main.txt"},
		Command: sh("cat src/main.c src/app.h > out/main.txt"),
	}}
}

func newLeakyWorkspace(t *testing.T) string {
	t.Helper()

	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "src/main.c"), []byte("MAIN-V1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "src/app.h"), []byte("HEADER-V1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return ws
}

// TestFalseCacheHitWithoutSandbox demonstrates the bug the sandbox exists to
// prevent. It asserts the WRONG behaviour on purpose: this is what BuildForge
// does when actions are not sandboxed, and the next test is the fix.
func TestFalseCacheHitWithoutSandbox(t *testing.T) {
	t.Parallel()

	ws := newLeakyWorkspace(t)
	store, err := cache.New(filepath.Join(t.TempDir(), "cache"))
	if err != nil {
		t.Fatal(err)
	}

	build := func() *Summary {
		t.Helper()
		g := newGraph(t, underDeclared()...)
		sum, err := Build(context.Background(), g, Options{Workspace: ws, Cache: store})
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		return sum
	}

	build()
	if got, want := readFile(t, ws, "out/main.txt"), "MAIN-V1\nHEADER-V1\n"; got != want {
		t.Fatalf("first build produced %q, want %q", got, want)
	}

	// Edit the undeclared header. Its content is not in the cache key, because
	// the key only covers declared inputs, so the key does not change.
	if err := os.WriteFile(filepath.Join(ws, "src/app.h"), []byte("HEADER-V2\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	sum := build()

	if sum.Cached() != 1 {
		t.Fatalf("Cached = %d, want 1: the point of this test is that it hits", sum.Cached())
	}
	// The wrong answer, asserted so that a change in behaviour here is loud.
	// The build is green, the cache is "working", and the output is stale.
	if got, want := readFile(t, ws, "out/main.txt"), "MAIN-V1\nHEADER-V1\n"; got != want {
		t.Fatalf("out/main.txt = %q, want the STALE %q", got, want)
	}
	if strings.Contains(readFile(t, ws, "out/main.txt"), "HEADER-V2") {
		t.Fatal("the output is fresh; this test no longer demonstrates the bug")
	}
}

// TestSandboxCatchesUnderDeclaration is the fix: the same manifest now fails at
// the moment it becomes wrong, instead of lying later.
func TestSandboxCatchesUnderDeclaration(t *testing.T) {
	t.Parallel()

	ws := newLeakyWorkspace(t)
	g := newGraph(t, underDeclared()...)

	_, err := Build(context.Background(), g, Options{
		Workspace:   ws,
		SandboxRoot: filepath.Join(t.TempDir(), "sandbox"),
	})
	if err == nil {
		t.Fatal("Build succeeded; the sandbox did not catch the undeclared input")
	}
	if !strings.Contains(err.Error(), "compile") {
		t.Errorf("error = %q, want it to name the action", err)
	}

	// And the failure is the useful kind: the command's own message about the
	// file it could not find.
	var ae *ActionError
	if !errors.As(err, &ae) {
		t.Fatalf("error = %T, want *ActionError", err)
	}
	if !strings.Contains(string(ae.Stderr), "app.h") {
		t.Errorf("stderr = %q, want it to name the missing file", ae.Stderr)
	}
}

func TestSandboxAllowsDeclaredInputs(t *testing.T) {
	t.Parallel()

	// The same action with the header declared must succeed and produce the
	// same bytes as an unsandboxed run. A sandbox that breaks correct manifests
	// is worse than no sandbox.
	ws := newLeakyWorkspace(t)
	actions := underDeclared()
	actions[0].Inputs = append(actions[0].Inputs, "src/app.h")

	g := newGraph(t, actions...)
	if _, err := Build(context.Background(), g, Options{
		Workspace:   ws,
		SandboxRoot: filepath.Join(t.TempDir(), "sandbox"),
	}); err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got, want := readFile(t, ws, "out/main.txt"), "MAIN-V1\nHEADER-V1\n"; got != want {
		t.Fatalf("out/main.txt = %q, want %q", got, want)
	}
}

func TestSandboxedAndUnsandboxedAgreeOnCorrectManifests(t *testing.T) {
	t.Parallel()

	// The differential test, again: a correct manifest must produce identical
	// bytes either way.
	actions := []manifest.Action{
		{
			Name:    "stage",
			Inputs:  []string{"src/main.c"},
			Outputs: []string{"out/staged"},
			Command: sh("cat src/main.c > out/staged"),
		},
		{
			Name:    "finish",
			Deps:    []string{"stage"},
			Inputs:  []string{"out/staged", "src/app.h"},
			Outputs: []string{"out/final"},
			Command: sh("cat out/staged src/app.h > out/final"),
		},
	}

	plainWS := newLeakyWorkspace(t)
	g := newGraph(t, actions...)
	if _, err := Build(context.Background(), g, Options{Workspace: plainWS}); err != nil {
		t.Fatalf("unsandboxed Build: %v", err)
	}

	boxedWS := newLeakyWorkspace(t)
	g = newGraph(t, actions...)
	if _, err := Build(context.Background(), g, Options{
		Workspace:   boxedWS,
		SandboxRoot: filepath.Join(t.TempDir(), "sandbox"),
	}); err != nil {
		t.Fatalf("sandboxed Build: %v", err)
	}

	for _, f := range []string{"out/staged", "out/final"} {
		if got, want := readFile(t, boxedWS, f), readFile(t, plainWS, f); got != want {
			t.Errorf("%s: sandboxed = %q, unsandboxed = %q", f, got, want)
		}
	}
}

func TestUndeclaredOutputDoesNotEscapeTheSandbox(t *testing.T) {
	t.Parallel()

	// An action that writes a file it did not declare leaves it in the sandbox,
	// where it is deleted. Otherwise the next action could come to depend on
	// something that was never part of anyone's contract, and the cache would
	// have no idea it existed.
	ws := newLeakyWorkspace(t)
	g := newGraph(t, manifest.Action{
		Name:    "sloppy",
		Inputs:  []string{"src/main.c"},
		Outputs: []string{"out/declared"},
		Command: sh("cat src/main.c > out/declared; echo surprise > out/undeclared"),
	})

	if _, err := Build(context.Background(), g, Options{
		Workspace:   ws,
		SandboxRoot: filepath.Join(t.TempDir(), "sandbox"),
	}); err != nil {
		t.Fatalf("Build: %v", err)
	}

	if _, err := os.Stat(filepath.Join(ws, "out/declared")); err != nil {
		t.Errorf("declared output missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(ws, "out/undeclared")); !os.IsNotExist(err) {
		t.Error("an undeclared output reached the workspace")
	}
}

func TestSandboxIsRemoved(t *testing.T) {
	t.Parallel()

	root := filepath.Join(t.TempDir(), "sandbox")
	ws := newLeakyWorkspace(t)

	actions := underDeclared()
	actions[0].Inputs = append(actions[0].Inputs, "src/app.h")

	g := newGraph(t, actions...)
	if _, err := Build(context.Background(), g, Options{Workspace: ws, SandboxRoot: root}); err != nil {
		t.Fatalf("Build: %v", err)
	}
	assertEmptyDir(t, root, "after a successful build")

	// And after a failure, so a broken build does not fill the disk.
	g = newGraph(t, underDeclared()...)
	if _, err := Build(context.Background(), g, Options{Workspace: ws, SandboxRoot: root}); err == nil {
		t.Fatal("Build succeeded, want the under-declared failure")
	}
	assertEmptyDir(t, root, "after a failed build")
}

func TestSandboxWithCacheStillHits(t *testing.T) {
	t.Parallel()

	// Sandboxing and caching have to compose: outputs are harvested out of the
	// sandbox before the cache stores them, and a hit skips the sandbox
	// entirely.
	ws := newLeakyWorkspace(t)
	store, err := cache.New(filepath.Join(t.TempDir(), "cache"))
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "sandbox")

	actions := underDeclared()
	actions[0].Inputs = append(actions[0].Inputs, "src/app.h")

	build := func() *Summary {
		t.Helper()
		g := newGraph(t, actions...)
		sum, err := Build(context.Background(), g, Options{
			Workspace: ws, Cache: store, SandboxRoot: root,
		})
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		return sum
	}

	if got := build().Cached(); got != 0 {
		t.Fatalf("first build cached %d, want 0", got)
	}
	if err := os.RemoveAll(filepath.Join(ws, "out")); err != nil {
		t.Fatal(err)
	}
	if got := build().Cached(); got != 1 {
		t.Fatalf("second build cached %d, want 1", got)
	}
	if got, want := readFile(t, ws, "out/main.txt"), "MAIN-V1\nHEADER-V1\n"; got != want {
		t.Fatalf("restored out/main.txt = %q, want %q", got, want)
	}

	// Now the header really is in the key, so editing it must miss.
	if err := os.WriteFile(filepath.Join(ws, "src/app.h"), []byte("HEADER-V2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := build().Cached(); got != 0 {
		t.Fatalf("after editing a declared input, cached %d, want 0", got)
	}
	if got, want := readFile(t, ws, "out/main.txt"), "MAIN-V1\nHEADER-V2\n"; got != want {
		t.Fatalf("out/main.txt = %q, want the fresh %q", got, want)
	}
}

func assertEmptyDir(t *testing.T, dir, when string) {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("%d sandbox directories left behind %s", len(entries), when)
	}
}
