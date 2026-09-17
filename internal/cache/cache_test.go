package cache

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mghadia1/buildforge/internal/digest"
	"github.com/mghadia1/buildforge/internal/manifest"
)

// testKey returns a realistic cache key. Keys are always 64 lowercase hex
// characters in practice, and the store refuses anything else, because these
// strings become path components inside the cache directory.
func testKey(seed string) string { return string(digest.Bytes([]byte(seed))) }

// fixture is a workspace with one input file and a baseline action over it.
type fixture struct {
	ws    string
	tools *ToolCache
}

func newFixture(t *testing.T) *fixture {
	t.Helper()

	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "src/in.txt"), []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	return &fixture{ws: ws, tools: NewToolCache()}
}

func (f *fixture) action() manifest.Action {
	return manifest.Action{
		Name:    "copy",
		Inputs:  []string{"src/in.txt"},
		Outputs: []string{"out/copy.txt"},
		Command: []string{"cp", "src/in.txt", "out/copy.txt"},
		Env:     map[string]string{"MODE": "release"},
	}
}

func (f *fixture) key(t *testing.T, a manifest.Action) string {
	t.Helper()
	k, err := Key(a, f.ws, f.tools)
	if err != nil {
		t.Fatalf("Key: %v", err)
	}
	return k
}

// --- must-miss: a stale hit here would be a wrong build -----------------------

func TestKeyMustChange(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		mutate func(t *testing.T, f *fixture, a *manifest.Action)
	}{
		{
			name: "input content changes",
			mutate: func(t *testing.T, f *fixture, a *manifest.Action) {
				if err := os.WriteFile(filepath.Join(f.ws, "src/in.txt"), []byte("edited"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "command changes",
			mutate: func(t *testing.T, f *fixture, a *manifest.Action) {
				a.Command = []string{"cp", "-p", "src/in.txt", "out/copy.txt"}
			},
		},
		{
			name: "declared env changes",
			mutate: func(t *testing.T, f *fixture, a *manifest.Action) {
				a.Env = map[string]string{"MODE": "debug"}
			},
		},
		{
			name: "declared env gains a variable",
			mutate: func(t *testing.T, f *fixture, a *manifest.Action) {
				a.Env["EXTRA"] = "1"
			},
		},
		{
			name: "an input is added",
			mutate: func(t *testing.T, f *fixture, a *manifest.Action) {
				if err := os.WriteFile(filepath.Join(f.ws, "src/other.txt"), []byte("x"), 0o644); err != nil {
					t.Fatal(err)
				}
				a.Inputs = append(a.Inputs, "src/other.txt")
			},
		},
		{
			name: "an output is added",
			mutate: func(t *testing.T, f *fixture, a *manifest.Action) {
				a.Outputs = append(a.Outputs, "out/second.txt")
			},
		},
		{
			// The tool is part of the key: a compiler upgrade changes the
			// output of every compile, and a cache that did not notice would
			// serve the old objects forever.
			name: "the tool binary changes",
			mutate: func(t *testing.T, f *fixture, a *manifest.Action) {
				a.Command = []string{"cat", "src/in.txt"}
			},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f := newFixture(t)
			a := f.action()
			before := f.key(t, a)

			tc.mutate(t, f, &a)
			after := f.key(t, a)

			if before == after {
				t.Fatalf("key did not change: %s", before)
			}
		})
	}
}

func TestKeyIsUnambiguousAcrossArgumentBoundaries(t *testing.T) {
	t.Parallel()

	// Plain concatenation would make ["ab","c"] and ["a","bc"] hash identically,
	// and one command would silently return the other's outputs. Length
	// prefixing is what prevents it.
	f := newFixture(t)

	a := f.action()
	a.Inputs = nil
	a.Command = []string{"echo", "ab", "c"}
	first := f.key(t, a)

	a.Command = []string{"echo", "a", "bc"}
	second := f.key(t, a)

	if first == second {
		t.Fatal("two different commands produced the same key")
	}
}

// --- must-hit: a spurious miss here makes the cache worthless ----------------

func TestKeyMustNotChange(t *testing.T) {
	t.Parallel()

	t.Run("modification time changes but content does not", func(t *testing.T) {
		t.Parallel()

		// The deliberate design choice. Hashing mtime would be cheaper, and it
		// would also invalidate the entire cache on every `git checkout` and
		// every `touch`, because neither changes a single byte of content.
		f := newFixture(t)
		a := f.action()
		before := f.key(t, a)

		future := time.Now().Add(time.Hour)
		if err := os.Chtimes(filepath.Join(f.ws, "src/in.txt"), future, future); err != nil {
			t.Fatal(err)
		}

		if after := f.key(t, a); before != after {
			t.Fatalf("key changed after touch: %s -> %s", before, after)
		}
	})

	t.Run("inputs are reordered", func(t *testing.T) {
		t.Parallel()

		f := newFixture(t)
		if err := os.WriteFile(filepath.Join(f.ws, "src/b.txt"), []byte("b"), 0o644); err != nil {
			t.Fatal(err)
		}

		a := f.action()
		a.Inputs = []string{"src/in.txt", "src/b.txt"}
		first := f.key(t, a)

		// Reordering the manifest cannot change what the action reads.
		a.Inputs = []string{"src/b.txt", "src/in.txt"}
		if second := f.key(t, a); first != second {
			t.Fatalf("reordering inputs changed the key: %s -> %s", first, second)
		}
	})

	t.Run("the workspace moves", func(t *testing.T) {
		t.Parallel()

		// Two checkouts of the same tree at different paths must share a cache.
		// If absolute paths were in the key, no two machines — and no two
		// directories on one machine — would ever hit.
		f1 := newFixture(t)
		f2 := newFixture(t)

		if f1.ws == f2.ws {
			t.Fatal("fixtures share a directory; the test proves nothing")
		}
		if k1, k2 := f1.key(t, f1.action()), f2.key(t, f2.action()); k1 != k2 {
			t.Fatalf("same tree at different paths keyed differently:\n  %s\n  %s", k1, k2)
		}
	})
}

// --- the store ---------------------------------------------------------------

func TestPutAndRestoreRoundTrip(t *testing.T) {
	t.Parallel()

	src := t.TempDir()
	if err := os.MkdirAll(filepath.Join(src, "out"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "out/app"), []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	store, err := New(filepath.Join(t.TempDir(), "cache"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(testKey("k1"), src, []string{"out/app"}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	entry, err := store.Lookup(testKey("k1"))
	if err != nil || entry == nil {
		t.Fatalf("Lookup = (%v, %v), want an entry", entry, err)
	}

	dst := t.TempDir()
	if err := store.Restore(entry, dst); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	b, err := os.ReadFile(filepath.Join(dst, "out/app"))
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "binary" {
		t.Fatalf("restored content = %q, want %q", b, "binary")
	}

	// The executable bit is the one that matters: a restored binary without it
	// is byte-identical and useless.
	info, err := os.Stat(filepath.Join(dst, "out/app"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("restored mode = %v, want the executable bit preserved", info.Mode().Perm())
	}
}

func TestLookupMissesWhenBlobIsGone(t *testing.T) {
	t.Parallel()

	// Milestone 7's garbage collector can remove a blob out from under an
	// entry. A half-restorable hit is worse than a miss.
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, "out"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "out/f"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}

	store, err := New(filepath.Join(t.TempDir(), "cache"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(testKey("k"), ws, []string{"out/f"}); err != nil {
		t.Fatal(err)
	}

	if err := os.RemoveAll(filepath.Join(store.Root(), "cas")); err != nil {
		t.Fatal(err)
	}
	entry, err := store.Lookup(testKey("k"))
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if entry != nil {
		t.Fatal("Lookup returned an entry whose blobs are gone")
	}
}

func TestRestoreDetectsCorruption(t *testing.T) {
	t.Parallel()

	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, "out"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "out/f"), []byte("good"), 0o644); err != nil {
		t.Fatal(err)
	}

	store, err := New(filepath.Join(t.TempDir(), "cache"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(testKey("k"), ws, []string{"out/f"}); err != nil {
		t.Fatal(err)
	}

	// Damage the blob in place, keeping its filename. This is exactly the case
	// a content-addressed store exists to make detectable.
	d := digest.Bytes([]byte("good"))
	blob := filepath.Join(store.Root(), "cas", string(d[:2]), string(d[2:]))
	if err := os.Chmod(blob, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(blob, []byte("evil"), 0o644); err != nil {
		t.Fatal(err)
	}

	entry, err := store.Lookup(testKey("k"))
	if err != nil || entry == nil {
		t.Fatalf("Lookup = (%v, %v), want an entry", entry, err)
	}
	if err := store.Restore(entry, t.TempDir()); err == nil {
		t.Fatal("Restore succeeded on a damaged blob")
	} else if !strings.Contains(err.Error(), "does not match its digest") {
		t.Fatalf("error = %v, want a digest mismatch", err)
	}
}

func TestPutDetectsNondeterminism(t *testing.T) {
	t.Parallel()

	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, "out"), 0o755); err != nil {
		t.Fatal(err)
	}

	store, err := New(filepath.Join(t.TempDir(), "cache"))
	if err != nil {
		t.Fatal(err)
	}

	// Same key, different bytes. Only the action itself can explain that — an
	// embedded timestamp, a hash-ordered map, an absolute path baked into a
	// header — and it means the action can never be cached reliably.
	if err := os.WriteFile(filepath.Join(ws, "out/f"), []byte("run one"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(testKey("same-key"), ws, []string{"out/f"}); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(ws, "out/f"), []byte("run two"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := store.Put(testKey("same-key"), ws, []string{"out/f"})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(res.Nondeterministic, ","), "out/f"; got != want {
		t.Fatalf("Nondeterministic = %q, want %q", got, want)
	}
}

func TestBlobsAreDeduplicated(t *testing.T) {
	t.Parallel()

	// Two unrelated actions producing identical bytes must share one blob.
	// Splitting the action cache from the CAS is what buys this.
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, "out"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a", "b"} {
		if err := os.WriteFile(filepath.Join(ws, "out", name), []byte("identical"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	store, err := New(filepath.Join(t.TempDir(), "cache"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(testKey("key-a"), ws, []string{"out/a"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(testKey("key-b"), ws, []string{"out/b"}); err != nil {
		t.Fatal(err)
	}

	var blobs int
	err = filepath.Walk(filepath.Join(store.Root(), "cas"), func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			blobs++
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if blobs != 1 {
		t.Fatalf("CAS holds %d blobs, want 1 for two identical outputs", blobs)
	}
}

func TestStoreRefusesUnsafeNames(t *testing.T) {
	t.Parallel()

	// A key becomes a path component. Without validation, these would read and
	// write outside the cache directory.
	store, err := New(filepath.Join(t.TempDir(), "cache"))
	if err != nil {
		t.Fatal(err)
	}

	for _, key := range []string{
		"../../etc/passwd",
		"ab/cd",
		"..",
		"AB12", // uppercase is not produced by this package
		"zz",   // not hex
		"a",    // too short to shard
	} {
		if _, err := store.Lookup(key); err == nil {
			t.Errorf("Lookup(%q) succeeded, want a rejection", key)
		}
	}
}
