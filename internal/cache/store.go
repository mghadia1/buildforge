package cache

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/mghadia1/buildforge/internal/digest"
)

// ErrCorrupt reports a blob whose bytes do not match the digest it is filed
// under. Callers treat it as a miss and re-run the action: a damaged cache
// should cost time, never correctness.
var ErrCorrupt = errors.New("cache blob does not match its digest")

// Store is a content-addressed store plus an action cache.
//
// Two maps, deliberately separate:
//
//	action cache   key    -> the outputs an action produced (paths, digests, modes)
//	CAS            digest -> the bytes
//
// Splitting them is what makes deduplication work. Two unrelated actions that
// happen to produce identical bytes — the same header compiled by two targets,
// an empty file, a copied asset — store one blob between them.
type Store struct {
	root string
}

// OutputFile records one file an action produced.
type OutputFile struct {
	Path   string        `json:"path"` // workspace-relative
	Digest digest.Digest `json:"digest"`

	// Mode carries the permission bits. Restoring a compiled binary without its
	// executable bit produces a file that is byte-identical and useless, which
	// is a cache hit that breaks the build in a way nothing else would catch.
	Mode uint32 `json:"mode"`
}

// Entry is one action cache record.
type Entry struct {
	Key     string       `json:"key"`
	Outputs []OutputFile `json:"outputs"`
}

// PutResult is the outcome of storing an action's outputs.
type PutResult struct {
	Entry *Entry

	// Nondeterministic names outputs whose digest differs from a previous entry
	// under the same key. Same key means the same inputs, command, tool, and
	// environment, so differing output can only come from the action itself —
	// an embedded timestamp, a hash-ordered map, an absolute path in a header.
	// It is not an error here, but it does mean this action can never be cached
	// reliably, and silently overwriting the old entry would hide that.
	Nondeterministic []string
}

// New opens (and creates) a store rooted at dir.
func New(dir string) (*Store, error) {
	for _, sub := range []string{"cas", "ac"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			return nil, fmt.Errorf("creating cache directory: %w", err)
		}
	}
	return &Store{root: dir}, nil
}

// Root returns the store's directory.
func (s *Store) Root() string { return s.root }

// ErrBadName reports a key or digest that is not lowercase hex.
var ErrBadName = errors.New("cache name must be at least three lowercase hex characters")

// shard validates a key or digest and splits it across two directory levels.
//
// The validation is not defensive padding. These strings become path
// components, so a name containing "/" or ".." would write outside the cache
// directory entirely; and a name shorter than the shard prefix would panic the
// slice below. Every name this package produces is a 64-character hex SHA-256,
// so anything else is a caller's bug and is refused rather than interpreted.
//
// The two-level split exists because a flat directory holding a hundred
// thousand entries is slow to open on most filesystems.
func shard(base, name, suffix string) (string, error) {
	if len(name) < 3 {
		return "", fmt.Errorf("%w: %q", ErrBadName, name)
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return "", fmt.Errorf("%w: %q", ErrBadName, name)
		}
	}
	return filepath.Join(base, name[:2], name[2:]+suffix), nil
}

func (s *Store) blobPath(d digest.Digest) (string, error) {
	return shard(filepath.Join(s.root, "cas"), string(d), "")
}

func (s *Store) entryPath(key string) (string, error) {
	return shard(filepath.Join(s.root, "ac"), key, ".json")
}

// Lookup returns the entry for key, or nil if there is none.
func (s *Store) Lookup(key string) (*Entry, error) {
	path, err := s.entryPath(key)
	if err != nil {
		return nil, err
	}

	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	var e Entry
	if err := json.Unmarshal(b, &e); err != nil {
		// A damaged entry is a miss, not a build failure.
		return nil, nil
	}

	// An entry is only usable if every blob it references is still present.
	// Garbage collection in Milestone 7 can remove a blob out from under an
	// entry, and a half-restorable hit is worse than a miss.
	for _, o := range e.Outputs {
		blob, err := s.blobPath(o.Digest)
		if err != nil {
			return nil, nil
		}
		if _, err := os.Stat(blob); err != nil {
			return nil, nil
		}
	}

	// Record the hit so the collector can evict by least-recently-used rather
	// than by insertion order.
	s.touchEntry(key)
	return &e, nil
}

// Put stores an action's declared outputs and records the entry.
func (s *Store) Put(key, workspace string, outputs []string) (*PutResult, error) {
	e := &Entry{Key: key}

	for _, rel := range outputs {
		full := filepath.Join(workspace, rel)

		info, err := os.Stat(full)
		if err != nil {
			return nil, fmt.Errorf("output %q: %w", rel, err)
		}

		d, err := digest.File(full)
		if err != nil {
			return nil, err
		}
		if err := s.putBlob(d, full); err != nil {
			return nil, err
		}

		e.Outputs = append(e.Outputs, OutputFile{
			Path:   rel,
			Digest: d,
			Mode:   uint32(info.Mode().Perm()),
		})
	}

	res := &PutResult{Entry: e}

	// Compare against any previous entry before overwriting it.
	if prev, err := s.Lookup(key); err == nil && prev != nil {
		was := make(map[string]digest.Digest, len(prev.Outputs))
		for _, o := range prev.Outputs {
			was[o.Path] = o.Digest
		}
		for _, o := range e.Outputs {
			if old, ok := was[o.Path]; ok && old != o.Digest {
				res.Nondeterministic = append(res.Nondeterministic, o.Path)
			}
		}
	}

	b, err := json.Marshal(e)
	if err != nil {
		return nil, err
	}
	path, err := s.entryPath(key)
	if err != nil {
		return nil, err
	}
	if err := writeAtomic(path, b, 0o644); err != nil {
		return nil, err
	}
	return res, nil
}

// putBlob copies a file into the CAS. A blob already present is left alone:
// identical content is identical content, and rewriting it would only widen the
// window in which a concurrent reader sees a partial file.
func (s *Store) putBlob(d digest.Digest, src string) error {
	dst, err := s.blobPath(d)
	if err != nil {
		return err
	}
	if _, err := os.Stat(dst); err == nil {
		return nil
	}

	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return writeAtomic(dst, b, 0o444)
}

// Restore materializes an entry's outputs into the workspace, verifying each
// blob against the digest it is filed under.
func (s *Store) Restore(e *Entry, workspace string) error {
	for _, o := range e.Outputs {
		blob, err := s.blobPath(o.Digest)
		if err != nil {
			return err
		}

		b, err := os.ReadFile(blob)
		if err != nil {
			return err
		}

		// Verify on the way out. A blob whose bytes were damaged on disk would
		// otherwise be restored as though it were the real output, which is the
		// one failure mode a content-addressed store exists to make impossible.
		if digest.Bytes(b) != o.Digest {
			return fmt.Errorf("%w: %s", ErrCorrupt, o.Path)
		}

		dst := filepath.Join(workspace, o.Path)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		if err := writeAtomic(dst, b, os.FileMode(o.Mode)); err != nil {
			return err
		}
	}
	return nil
}

// writeAtomic writes to a temporary file in the destination directory and
// renames it into place.
//
// rename(2) is atomic within a filesystem, so a reader sees either the old file
// or the complete new one, never a half-written file. An interrupted build
// therefore cannot leave a truncated blob that would later be served as a
// legitimate cache hit.
func writeAtomic(path string, b []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()

	// If anything below fails, do not leave the temporary file behind.
	defer func() {
		tmp.Close()
		os.Remove(tmpName)
	}()

	if _, err := tmp.Write(b); err != nil {
		return err
	}
	// Chmod before the rename so the file is never briefly visible at the wrong
	// mode under its final name.
	if err := tmp.Chmod(mode); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	// Remove any existing file first: a CAS blob is written 0444, and renaming
	// over a read-only file fails on some filesystems.
	os.Remove(path)
	return os.Rename(tmpName, path)
}

// --- blob and entry access, used by the remote cache in Milestone 6 ---------

// HasBlob reports whether the CAS already holds these bytes.
func (s *Store) HasBlob(d digest.Digest) bool {
	path, err := s.blobPath(d)
	if err != nil {
		return false
	}
	_, err = os.Stat(path)
	return err == nil
}

// ReadBlob returns a blob's bytes, verifying them against the digest they are
// filed under.
func (s *Store) ReadBlob(d digest.Digest) ([]byte, error) {
	path, err := s.blobPath(d)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if digest.Bytes(b) != d {
		return nil, fmt.Errorf("%w: %s", ErrCorrupt, d)
	}
	return b, nil
}

// WriteBlob stores bytes under their own digest.
//
// The digest is recomputed rather than trusted. These bytes may have arrived
// over a network from another machine, and a content-addressed store whose
// contents do not match their addresses is no longer content-addressed.
func (s *Store) WriteBlob(d digest.Digest, b []byte) error {
	if got := digest.Bytes(b); got != d {
		return fmt.Errorf("%w: content hashes to %s, not %s", ErrCorrupt, got, d)
	}
	path, err := s.blobPath(d)
	if err != nil {
		return err
	}
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	return writeAtomic(path, b, 0o444)
}

// PutEntry records an action cache entry directly, without touching a
// workspace. The remote cache uses this to install an entry it downloaded.
func (s *Store) PutEntry(e *Entry) error {
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	path, err := s.entryPath(e.Key)
	if err != nil {
		return err
	}
	return writeAtomic(path, b, 0o644)
}
