package cache

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/mghadia1/buildforge/internal/digest"
)

// GCOptions configures a collection.
type GCOptions struct {
	// MaxBytes is the size budget for the CAS. Zero collects only unreachable
	// blobs and evicts nothing that is still referenced.
	MaxBytes int64

	// MinAge protects recently written files from collection.
	//
	// This is a mitigation, not a solution. A build in progress writes blobs
	// before it writes the entry that references them, so for a moment those
	// blobs look like garbage — and a collector running at that moment would
	// delete the outputs of an action that is still finishing. MinAge makes
	// that window unlikely rather than impossible. A correct fix needs a lock
	// between the collector and running builds, or a two-phase mark that
	// tolerates concurrent writers. Neither is implemented, and GC is
	// documented as unsafe to run during a build.
	MinAge time.Duration

	// DryRun reports what would be collected without deleting anything.
	DryRun bool

	// now overrides the clock in tests.
	now time.Time
}

// GCResult reports what a collection did.
type GCResult struct {
	Entries      int // action cache entries scanned
	Blobs        int // blobs scanned
	BytesBefore  int64
	Unreferenced int // blobs no entry pointed at
	Evicted      int // referenced blobs removed to meet the budget
	EvictedEntry int // entries removed so their blobs could be evicted
	Skipped      int // files left alone because of MinAge
	BytesFreed   int64
	BytesAfter   int64
}

// GC removes blobs the action cache no longer references, then evicts whole
// entries, least recently used first, until the CAS fits its budget.
//
// Two phases, because they answer different questions.
//
// The first is reachability: a blob nothing points at is garbage, and deleting
// it costs nothing. These accumulate from interrupted builds, which write blobs
// before the entry that references them, and from entries overwritten by a
// nondeterministic action.
//
// The second is capacity, and it is a policy choice rather than a fact. Every
// blob left is still referenced, so evicting one means a future build will miss
// and re-run an action. Eviction is done **at entry granularity**: an entry is
// dropped and only then are the blobs whose reference count fell to zero
// removed. Evicting an individual blob instead would leave every entry that
// shared it half-restorable — which Lookup does treat as a miss, so it would be
// safe, but the space would not actually be reclaimed and the entries would
// linger as dead weight.
func (s *Store) GC(opts GCOptions) (*GCResult, error) {
	now := opts.now
	if now.IsZero() {
		now = time.Now()
	}

	entries, err := s.scanEntries()
	if err != nil {
		return nil, err
	}
	blobs, err := s.scanBlobs()
	if err != nil {
		return nil, err
	}

	res := &GCResult{Entries: len(entries), Blobs: len(blobs)}
	for _, b := range blobs {
		res.BytesBefore += b.size
	}
	res.BytesAfter = res.BytesBefore

	// Reference counts from the action cache into the CAS.
	refs := make(map[digest.Digest]int, len(blobs))
	for _, e := range entries {
		for _, d := range e.digests {
			refs[d]++
		}
	}

	remove := func(d digest.Digest, b blobInfo, counter *int) {
		if opts.MinAge > 0 && now.Sub(b.modTime) < opts.MinAge {
			res.Skipped++
			return
		}
		if !opts.DryRun {
			if path, err := s.blobPath(d); err == nil {
				os.Remove(path)
			}
		}
		*counter++
		res.BytesFreed += b.size
		res.BytesAfter -= b.size
		delete(blobs, d)
	}

	// Phase one: unreachable blobs.
	for d, b := range blobs {
		if refs[d] == 0 {
			remove(d, b, &res.Unreferenced)
		}
	}

	// Phase two: capacity.
	if opts.MaxBytes <= 0 || res.BytesAfter <= opts.MaxBytes {
		return res, nil
	}

	// Least recently used first. "Used" is the entry file's modification time,
	// which Lookup updates on every hit — see touchEntry. Access times would be
	// the natural signal and are unreliable: many filesystems mount with
	// relatime or noatime, so a hit may not move them at all.
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].modTime.Equal(entries[j].modTime) {
			return entries[i].key < entries[j].key
		}
		return entries[i].modTime.Before(entries[j].modTime)
	})

	for _, e := range entries {
		if res.BytesAfter <= opts.MaxBytes {
			break
		}
		if opts.MinAge > 0 && now.Sub(e.modTime) < opts.MinAge {
			res.Skipped++
			continue
		}

		if !opts.DryRun {
			if path, err := s.entryPath(e.key); err == nil {
				os.Remove(path)
			}
		}
		res.EvictedEntry++

		for _, d := range e.digests {
			refs[d]--
			if refs[d] > 0 {
				// Another entry still points at this blob. Dropping it here
				// would break that entry for no space saved.
				continue
			}
			if b, ok := blobs[d]; ok {
				remove(d, b, &res.Evicted)
			}
		}
	}
	return res, nil
}

type entryInfo struct {
	key     string
	digests []digest.Digest
	modTime time.Time
}

type blobInfo struct {
	size    int64
	modTime time.Time
}

func (s *Store) scanEntries() ([]entryInfo, error) {
	root := filepath.Join(s.root, "ac")
	var out []entryInfo

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".json") {
			return nil // an unreadable file is skipped, not fatal
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		var e Entry
		if err := json.Unmarshal(b, &e); err != nil {
			return nil
		}

		digests := make([]digest.Digest, 0, len(e.Outputs))
		for _, o := range e.Outputs {
			digests = append(digests, o.Digest)
		}
		out = append(out, entryInfo{key: e.Key, digests: digests, modTime: info.ModTime()})
		return nil
	})
	if os.IsNotExist(err) {
		return nil, nil
	}
	return out, err
}

func (s *Store) scanBlobs() (map[digest.Digest]blobInfo, error) {
	root := filepath.Join(s.root, "cas")
	out := make(map[digest.Digest]blobInfo)

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		// A blob's name is its digest, split across two directory levels.
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		parts := strings.Split(rel, string(filepath.Separator))
		if len(parts) != 2 {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		out[digest.Digest(parts[0]+parts[1])] = blobInfo{size: info.Size(), modTime: info.ModTime()}
		return nil
	})
	if os.IsNotExist(err) {
		return out, nil
	}
	return out, err
}

// touchGranularity is how stale a usage record is allowed to get.
//
// The LRU only has to order entries well enough to decide which to throw away,
// and at that job an hour and a millisecond are worth the same. So the common
// case — an entry already touched recently — costs a stat instead of a
// metadata write.
//
// An honest note about why this exists. The first version wrote on every hit,
// and a no-op build appeared to go from 9.8 ms to 12.0 ms, which looked like
// the obvious culprit. A controlled A/B of the two binaries, interleaved inside
// a single session, then found the difference **inconclusive**: the touch costs
// nothing this workload can measure, and the 9.8-to-12.0 change was drift
// between two benchmark sessions run minutes apart.
//
// The coarse granularity is kept because avoiding a metadata write on every
// cache hit is right on its own terms, not because any measurement here
// demonstrates it helps. See bench/RESULTS.md, Run 5.
const touchGranularity = time.Hour

// touchEntry records that an entry was used, for the collector's LRU ordering.
//
// A read that writes is unusual enough to justify: the alternative is the
// file's access time, which many filesystems do not update reliably, and
// without a usage signal eviction would fall back to insertion order and throw
// away exactly the entries a build depends on most.
func (s *Store) touchEntry(key string) {
	path, err := s.entryPath(key)
	if err != nil {
		return
	}

	now := time.Now()
	if info, err := os.Stat(path); err == nil && now.Sub(info.ModTime()) < touchGranularity {
		return
	}
	os.Chtimes(path, now, now)
}
