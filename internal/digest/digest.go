// Package digest computes content digests.
//
// Everything BuildForge caches is addressed by the SHA-256 of its bytes. Not by
// path, not by modification time: a file that was touched but not edited is the
// same file, and a file that was edited back to its original bytes is the
// original file. Content addressing is what makes both of those true without
// any special handling.
package digest

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"runtime"
	"sync"
)

// Digest is a lowercase hex SHA-256.
type Digest string

// Bytes digests a byte slice.
func Bytes(b []byte) Digest {
	sum := sha256.Sum256(b)
	return Digest(hex.EncodeToString(sum[:]))
}

// File digests a file's contents, streaming so that a large file does not have
// to fit in memory.
func File(path string) (Digest, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("digesting %s: %w", path, err)
	}
	return Digest(hex.EncodeToString(h.Sum(nil))), nil
}

// Files digests many files concurrently and returns a path-to-digest map.
//
// Hashing is IO-bound then CPU-bound, and a build's inputs are usually many
// small files, so a bounded pool beats both a sequential loop and one goroutine
// per file. The first error encountered is returned; the others are discarded,
// because a manifest with ten missing inputs is one problem, not ten.
func Files(paths []string) (map[string]Digest, error) {
	out := make(map[string]Digest, len(paths))
	if len(paths) == 0 {
		return out, nil
	}

	workers := runtime.NumCPU()
	if workers > len(paths) {
		workers = len(paths)
	}

	var (
		mu       sync.Mutex
		firstErr error
		wg       sync.WaitGroup
	)
	jobs := make(chan string)

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for p := range jobs {
				d, err := File(p)

				mu.Lock()
				if err != nil {
					if firstErr == nil {
						firstErr = err
					}
				} else {
					out[p] = d
				}
				mu.Unlock()
			}
		}()
	}

	for _, p := range paths {
		jobs <- p
	}
	close(jobs)
	wg.Wait()

	if firstErr != nil {
		return nil, firstErr
	}
	return out, nil
}
