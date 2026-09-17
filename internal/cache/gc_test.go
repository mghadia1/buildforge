package cache

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mghadia1/buildforge/internal/digest"
)

// gcFixture is a store plus a workspace to stage outputs in.
type gcFixture struct {
	store *Store
	ws    string
}

func newGCFixture(t *testing.T) *gcFixture {
	t.Helper()

	store, err := New(filepath.Join(t.TempDir(), "cache"))
	if err != nil {
		t.Fatal(err)
	}
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, "out"), 0o755); err != nil {
		t.Fatal(err)
	}
	return &gcFixture{store: store, ws: ws}
}

// put stores one entry whose single output has the given content.
func (f *gcFixture) put(t *testing.T, key, content string) {
	t.Helper()

	name := "out/" + key[:8]
	if err := os.WriteFile(filepath.Join(f.ws, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.Put(key, f.ws, []string{name}); err != nil {
		t.Fatalf("Put: %v", err)
	}
}

func (f *gcFixture) blobCount(t *testing.T) int {
	t.Helper()

	blobs, err := f.store.scanBlobs()
	if err != nil {
		t.Fatal(err)
	}
	return len(blobs)
}

func (f *gcFixture) entryCount(t *testing.T) int {
	t.Helper()

	entries, err := f.store.scanEntries()
	if err != nil {
		t.Fatal(err)
	}
	return len(entries)
}

func TestGCRemovesUnreferencedBlobs(t *testing.T) {
	t.Parallel()

	// A blob nothing points at is garbage. These accumulate from interrupted
	// builds, which write blobs before the entry that would reference them.
	f := newGCFixture(t)
	f.put(t, testKey("kept"), "referenced content")

	orphan := digest.Bytes([]byte("nobody refers to this"))
	if err := f.store.WriteBlob(orphan, []byte("nobody refers to this")); err != nil {
		t.Fatal(err)
	}
	if got := f.blobCount(t); got != 2 {
		t.Fatalf("blobs = %d, want 2 before collection", got)
	}

	res, err := f.store.GC(GCOptions{})
	if err != nil {
		t.Fatalf("GC: %v", err)
	}
	if res.Unreferenced != 1 {
		t.Errorf("Unreferenced = %d, want 1", res.Unreferenced)
	}
	if got := f.blobCount(t); got != 1 {
		t.Errorf("blobs = %d after collection, want 1", got)
	}
	if f.store.HasBlob(orphan) {
		t.Error("the unreferenced blob survived")
	}
}

func TestGCKeepsReferencedBlobsWithoutABudget(t *testing.T) {
	t.Parallel()

	// With no size budget the collector must not touch anything reachable,
	// however old it is.
	f := newGCFixture(t)
	for _, k := range []string{"a", "b", "c"} {
		f.put(t, testKey(k), "content "+k)
	}

	res, err := f.store.GC(GCOptions{})
	if err != nil {
		t.Fatalf("GC: %v", err)
	}
	if res.Evicted != 0 || res.EvictedEntry != 0 {
		t.Errorf("evicted %d blobs and %d entries with no budget set", res.Evicted, res.EvictedEntry)
	}
	if got := f.blobCount(t); got != 3 {
		t.Errorf("blobs = %d, want 3", got)
	}
}

func TestGCEvictsLeastRecentlyUsedFirst(t *testing.T) {
	t.Parallel()

	// Three entries, each with a distinct 100-byte output. A 250-byte budget
	// must drop exactly one, and it must be the one used longest ago.
	f := newGCFixture(t)
	keys := []string{testKey("old"), testKey("middle"), testKey("fresh")}
	for _, k := range keys {
		// Exactly 100 bytes, distinct per key.
		f.put(t, k, strings.Repeat("x", 90)+k[:10])
	}

	// Stamp deliberate usage times rather than relying on how fast the test runs.
	base := time.Now().Add(-24 * time.Hour)
	for i, k := range keys {
		path, err := f.store.entryPath(k)
		if err != nil {
			t.Fatal(err)
		}
		when := base.Add(time.Duration(i) * time.Hour)
		if err := os.Chtimes(path, when, when); err != nil {
			t.Fatal(err)
		}
	}

	res, err := f.store.GC(GCOptions{MaxBytes: 250})
	if err != nil {
		t.Fatalf("GC: %v", err)
	}
	if res.EvictedEntry != 1 {
		t.Fatalf("EvictedEntry = %d, want 1", res.EvictedEntry)
	}
	if res.BytesAfter > 250 {
		t.Errorf("BytesAfter = %d, want at most 250", res.BytesAfter)
	}

	// The oldest entry is gone; the two more recent ones survive.
	if e, _ := f.store.Lookup(keys[0]); e != nil {
		t.Error("the least recently used entry survived")
	}
	for _, k := range keys[1:] {
		if e, _ := f.store.Lookup(k); e == nil {
			t.Errorf("entry %s... was evicted before an older one", k[:8])
		}
	}
}

func TestGCKeepsBlobsSharedWithASurvivingEntry(t *testing.T) {
	t.Parallel()

	// Two entries produce identical bytes, so they share one blob. Evicting one
	// entry must not delete the blob the other still needs — deleting it would
	// break a surviving entry and reclaim nothing, because the space is still
	// referenced.
	f := newGCFixture(t)

	shared := strings.Repeat("s", 200)
	if err := os.WriteFile(filepath.Join(f.ws, "out/shared"), []byte(shared), 0o644); err != nil {
		t.Fatal(err)
	}
	keyOld, keyNew := testKey("shared-old"), testKey("shared-new")
	for _, k := range []string{keyOld, keyNew} {
		if _, err := f.store.Put(k, f.ws, []string{"out/shared"}); err != nil {
			t.Fatal(err)
		}
	}
	// An unrelated 500-byte entry, so the collector has something it can
	// actually free once it finds that dropping keyOld frees nothing.
	f.put(t, testKey("bulk"), strings.Repeat("b", 500))

	// Oldest first: keyOld, then bulk, then keyNew. The CAS holds 700 bytes.
	// A 550-byte budget makes the collector drop keyOld (which frees nothing,
	// because keyNew still references the shared blob) and then bulk (which
	// frees 500 and satisfies the budget), leaving keyNew intact.
	now := time.Now()
	for k, age := range map[string]time.Duration{
		keyOld:          48 * time.Hour,
		testKey("bulk"): 24 * time.Hour,
		keyNew:          0,
	} {
		path, err := f.store.entryPath(k)
		if err != nil {
			t.Fatal(err)
		}
		when := now.Add(-age)
		if err := os.Chtimes(path, when, when); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := f.store.GC(GCOptions{MaxBytes: 550}); err != nil {
		t.Fatalf("GC: %v", err)
	}

	if e, _ := f.store.Lookup(keyOld); e != nil {
		t.Error("the old entry survived eviction")
	}
	// The surviving entry must still be fully restorable: Lookup returns nil
	// when any of an entry's blobs is missing, so this catches the bug.
	if e, _ := f.store.Lookup(keyNew); e == nil {
		t.Fatal("evicting one entry broke another that shared its blob")
	}
	if !f.store.HasBlob(digest.Bytes([]byte(shared))) {
		t.Error("the shared blob was deleted while still referenced")
	}
}

func TestGCLeavesNoDanglingEntries(t *testing.T) {
	t.Parallel()

	// Every entry left behind must still be usable. An entry whose blobs were
	// collected is dead weight: Lookup treats it as a miss, so it is safe, but
	// it should not be left lying there.
	f := newGCFixture(t)
	for i, k := range []string{"e1", "e2", "e3", "e4"} {
		f.put(t, testKey(k), strings.Repeat("y", 100+i))
	}

	if _, err := f.store.GC(GCOptions{MaxBytes: 150}); err != nil {
		t.Fatalf("GC: %v", err)
	}

	entries, err := f.store.scanEntries()
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		got, err := f.store.Lookup(e.key)
		if err != nil {
			t.Fatal(err)
		}
		if got == nil {
			t.Errorf("entry %s... survived but its blobs did not", e.key[:8])
		}
	}
}

func TestGCRespectsTheBudget(t *testing.T) {
	t.Parallel()

	f := newGCFixture(t)
	for i := 0; i < 10; i++ {
		f.put(t, testKey(string(rune('a'+i))), strings.Repeat("z", 100)+string(rune('a'+i)))
	}

	res, err := f.store.GC(GCOptions{MaxBytes: 400})
	if err != nil {
		t.Fatalf("GC: %v", err)
	}
	if res.BytesAfter > 400 {
		t.Fatalf("BytesAfter = %d, want at most 400", res.BytesAfter)
	}
	if res.BytesFreed == 0 {
		t.Error("BytesFreed = 0 although the budget was exceeded")
	}

	// And the reported figure must match what is actually on disk.
	var onDisk int64
	blobs, err := f.store.scanBlobs()
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range blobs {
		onDisk += b.size
	}
	if onDisk != res.BytesAfter {
		t.Errorf("BytesAfter = %d but %d bytes are on disk", res.BytesAfter, onDisk)
	}
}

func TestGCDryRunChangesNothing(t *testing.T) {
	t.Parallel()

	f := newGCFixture(t)
	for i := 0; i < 5; i++ {
		f.put(t, testKey(string(rune('p'+i))), strings.Repeat("d", 100))
	}
	orphan := digest.Bytes([]byte("orphan"))
	if err := f.store.WriteBlob(orphan, []byte("orphan")); err != nil {
		t.Fatal(err)
	}

	beforeBlobs, beforeEntries := f.blobCount(t), f.entryCount(t)

	res, err := f.store.GC(GCOptions{MaxBytes: 100, DryRun: true})
	if err != nil {
		t.Fatalf("GC: %v", err)
	}
	if res.Unreferenced == 0 && res.Evicted == 0 {
		t.Error("the dry run reported nothing to collect")
	}
	if got := f.blobCount(t); got != beforeBlobs {
		t.Errorf("blobs = %d after a dry run, want %d", got, beforeBlobs)
	}
	if got := f.entryCount(t); got != beforeEntries {
		t.Errorf("entries = %d after a dry run, want %d", got, beforeEntries)
	}
}

func TestGCMinAgeProtectsFreshFiles(t *testing.T) {
	t.Parallel()

	// A build in progress writes blobs before the entry that references them,
	// so for a moment they look exactly like garbage. MinAge narrows that
	// window. It does not close it, and the documentation says so.
	f := newGCFixture(t)

	fresh := digest.Bytes([]byte("just written by a running build"))
	if err := f.store.WriteBlob(fresh, []byte("just written by a running build")); err != nil {
		t.Fatal(err)
	}

	res, err := f.store.GC(GCOptions{MinAge: time.Hour})
	if err != nil {
		t.Fatalf("GC: %v", err)
	}
	if res.Unreferenced != 0 {
		t.Errorf("Unreferenced = %d, want 0: a fresh blob was collected", res.Unreferenced)
	}
	if res.Skipped != 1 {
		t.Errorf("Skipped = %d, want 1", res.Skipped)
	}
	if !f.store.HasBlob(fresh) {
		t.Error("a blob younger than MinAge was deleted")
	}
}

func TestGCOnEmptyStore(t *testing.T) {
	t.Parallel()

	f := newGCFixture(t)
	res, err := f.store.GC(GCOptions{MaxBytes: 1})
	if err != nil {
		t.Fatalf("GC: %v", err)
	}
	if res.Entries != 0 || res.Blobs != 0 || res.BytesFreed != 0 {
		t.Fatalf("GC on an empty store reported %+v", res)
	}
}

func TestLookupRecordsUse(t *testing.T) {
	t.Parallel()

	// The LRU ordering depends on this. Access times are the natural signal and
	// are unreliable — many filesystems mount with relatime or noatime — so a
	// hit deliberately writes to the entry file.
	f := newGCFixture(t)
	key := testKey("touched")
	f.put(t, key, "content")

	path, err := f.store.entryPath(key)
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}

	if e, _ := f.store.Lookup(key); e == nil {
		t.Fatal("Lookup missed")
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().After(old) {
		t.Fatalf("modification time is still %s; the hit was not recorded", info.ModTime())
	}
}
