package digest

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestBytesIsStableAndDistinct(t *testing.T) {
	t.Parallel()

	if Bytes([]byte("hello")) != Bytes([]byte("hello")) {
		t.Fatal("the same bytes digested differently")
	}
	if Bytes([]byte("hello")) == Bytes([]byte("hellp")) {
		t.Fatal("different bytes digested identically")
	}
	// The well-known SHA-256 of the empty input, as a check that this is
	// actually SHA-256 and not something that merely looks like it.
	const emptySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	if got := string(Bytes(nil)); got != emptySHA256 {
		t.Fatalf("Bytes(nil) = %s, want %s", got, emptySHA256)
	}
}

func TestFileIgnoresModificationTime(t *testing.T) {
	t.Parallel()

	// The central property. A file that was touched but not edited must digest
	// identically, or every `git checkout` would invalidate the whole cache.
	dir := t.TempDir()
	p := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(p, []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}

	before, err := File(p)
	if err != nil {
		t.Fatal(err)
	}

	future := time.Now().Add(2 * time.Hour)
	if err := os.Chtimes(p, future, future); err != nil {
		t.Fatal(err)
	}

	after, err := File(p)
	if err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatalf("digest changed after touch: %s -> %s", before, after)
	}
}

func TestFilesConcurrent(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	var paths []string
	for i := 0; i < 50; i++ {
		p := filepath.Join(dir, strings.Repeat("a", i+1)+".txt")
		if err := os.WriteFile(p, []byte(strings.Repeat("x", i)), 0o644); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, p)
	}

	got, err := Files(paths)
	if err != nil {
		t.Fatalf("Files: %v", err)
	}
	if len(got) != len(paths) {
		t.Fatalf("got %d digests, want %d", len(got), len(paths))
	}

	// Every digest must match the sequential result exactly.
	for _, p := range paths {
		want, err := File(p)
		if err != nil {
			t.Fatal(err)
		}
		if got[p] != want {
			t.Errorf("%s: concurrent = %s, sequential = %s", filepath.Base(p), got[p], want)
		}
	}
}

func TestFilesReportsMissingFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	ok := filepath.Join(dir, "present.txt")
	if err := os.WriteFile(ok, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	paths := []string{ok, filepath.Join(dir, "absent.txt")}
	sort.Strings(paths)

	if _, err := Files(paths); err == nil {
		t.Fatal("Files succeeded, want an error for the missing file")
	}
}
