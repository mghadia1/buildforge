package runner

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mghadia1/buildforge/internal/cache"
	"github.com/mghadia1/buildforge/internal/manifest"
)

// cachedFixture is a two-action build behind a shared cache: compile reads a
// source file, link reads compile's output.
type cachedFixture struct {
	ws    string
	store *cache.Store
}

func newCachedFixture(t *testing.T) *cachedFixture {
	t.Helper()

	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "src/a.txt"), []byte("alpha"), 0o644); err != nil {
		t.Fatal(err)
	}

	store, err := cache.New(filepath.Join(t.TempDir(), "cache"))
	if err != nil {
		t.Fatal(err)
	}
	return &cachedFixture{ws: ws, store: store}
}

func (f *cachedFixture) graphOf(t *testing.T) *manifest.Manifest {
	t.Helper()
	return &manifest.Manifest{Actions: []manifest.Action{
		{
			Name:    "stage",
			Inputs:  []string{"src/a.txt"},
			Outputs: []string{"out/a.staged"},
			Command: sh("cat src/a.txt > out/a.staged"),
		},
		{
			Name:    "finish",
			Deps:    []string{"stage"},
			Inputs:  []string{"out/a.staged"},
			Outputs: []string{"out/a.final"},
			Command: sh("cat out/a.staged > out/a.final"),
		},
	}}
}

func (f *cachedFixture) build(t *testing.T) *Summary {
	t.Helper()

	m := f.graphOf(t)
	g := newGraph(t, m.Actions...)
	sum, err := Build(context.Background(), g, Options{Workspace: f.ws, Cache: f.store})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return sum
}

func TestSecondBuildIsFullyCached(t *testing.T) {
	t.Parallel()

	f := newCachedFixture(t)

	if got := f.build(t).Cached(); got != 0 {
		t.Fatalf("first build had %d cached actions, want 0", got)
	}

	// Remove the outputs so the second build has to restore them rather than
	// merely find them already present.
	if err := os.RemoveAll(filepath.Join(f.ws, "out")); err != nil {
		t.Fatal(err)
	}

	sum := f.build(t)
	if got, want := sum.Cached(), 2; got != want {
		t.Fatalf("second build cached %d actions, want %d", got, want)
	}
	if got, want := readFile(t, f.ws, "out/a.final"), "alpha"; got != want {
		t.Fatalf("restored out/a.final = %q, want %q", got, want)
	}
}

func TestTouchingAnInputStillHits(t *testing.T) {
	t.Parallel()

	// The design choice, end to end: `git checkout` and `touch` change every
	// modification time and not one byte of content. Hashing mtime would
	// invalidate the whole cache for nothing.
	f := newCachedFixture(t)
	f.build(t)

	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(filepath.Join(f.ws, "src/a.txt"), future, future); err != nil {
		t.Fatal(err)
	}

	if got, want := f.build(t).Cached(), 2; got != want {
		t.Fatalf("after touch, cached %d actions, want %d", got, want)
	}
}

func TestEditingAnInputInvalidatesTransitively(t *testing.T) {
	t.Parallel()

	// stage reads the edited file, so it must re-run. finish does not read it
	// at all — but it reads stage's output, whose content changed, so its own
	// key changes too. Transitive invalidation falls out of content addressing
	// rather than needing to be propagated by hand.
	f := newCachedFixture(t)
	f.build(t)

	if err := os.WriteFile(filepath.Join(f.ws, "src/a.txt"), []byte("beta"), 0o644); err != nil {
		t.Fatal(err)
	}

	sum := f.build(t)
	if got := sum.Cached(); got != 0 {
		t.Fatalf("after editing the source, cached %d actions, want 0", got)
	}
	if got, want := readFile(t, f.ws, "out/a.final"), "beta"; got != want {
		t.Fatalf("out/a.final = %q, want %q", got, want)
	}
}

func TestEditingAndRevertingHitsAgain(t *testing.T) {
	t.Parallel()

	// Content addressing means a file edited back to its original bytes is the
	// original file. No special handling; it simply digests the same.
	f := newCachedFixture(t)
	f.build(t)

	src := filepath.Join(f.ws, "src/a.txt")
	if err := os.WriteFile(src, []byte("beta"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.build(t)

	if err := os.WriteFile(src, []byte("alpha"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, want := f.build(t).Cached(), 2; got != want {
		t.Fatalf("after reverting, cached %d actions, want %d", got, want)
	}
}

func TestCacheIsSharedAcrossWorkspaces(t *testing.T) {
	t.Parallel()

	// Two checkouts of the same sources at different paths share one cache.
	// This is the property the remote cache in Milestone 6 is built on: if
	// absolute paths were in the key, no two machines would ever hit.
	first := newCachedFixture(t)
	first.build(t)

	second := newCachedFixture(t)
	second.store = first.store

	if got, want := second.build(t).Cached(), 2; got != want {
		t.Fatalf("second workspace cached %d actions, want %d", got, want)
	}
	if got, want := readFile(t, second.ws, "out/a.final"), "alpha"; got != want {
		t.Fatalf("out/a.final = %q, want %q", got, want)
	}
}

func TestCachedActionDoesNotRun(t *testing.T) {
	t.Parallel()

	// The strongest form of the test: the cached action's command appends to a
	// side file, so if it ever runs again the evidence is unambiguous.
	ws := t.TempDir()
	store, err := cache.New(filepath.Join(t.TempDir(), "cache"))
	if err != nil {
		t.Fatal(err)
	}

	actions := []manifest.Action{{
		Name:    "counted",
		Outputs: []string{"out/result"},
		Command: sh("echo tick >> " + filepath.Join(ws, "runs.log") + "; echo done > out/result"),
	}}

	for i := 0; i < 3; i++ {
		g := newGraph(t, actions...)
		if _, err := Build(context.Background(), g, Options{Workspace: ws, Cache: store}); err != nil {
			t.Fatalf("build %d: %v", i, err)
		}
	}

	b, err := os.ReadFile(filepath.Join(ws, "runs.log"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(b), "tick\n"; got != want {
		t.Fatalf("runs.log = %q, want %q: the command ran more than once", got, want)
	}
}

func TestCorruptCacheFallsBackToRunning(t *testing.T) {
	t.Parallel()

	// A damaged cache must cost time, never correctness. The build has to
	// succeed with the right bytes even when a blob has been tampered with.
	f := newCachedFixture(t)
	f.build(t)

	err := filepath.Walk(filepath.Join(f.store.Root(), "cas"), func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		if err := os.Chmod(path, 0o644); err != nil {
			return err
		}
		return os.WriteFile(path, []byte("corrupted"), 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(f.ws, "out")); err != nil {
		t.Fatal(err)
	}

	sum := f.build(t)
	if got, want := readFile(t, f.ws, "out/a.final"), "alpha"; got != want {
		t.Fatalf("out/a.final = %q, want %q", got, want)
	}
	if sum.Cached() != 0 {
		t.Errorf("Cached = %d, want 0: the corrupted blobs should have missed", sum.Cached())
	}
}
