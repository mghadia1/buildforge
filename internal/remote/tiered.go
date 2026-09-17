package remote

import (
	"sync"

	"github.com/mghadia1/buildforge/internal/cache"
)

// Tiered is a local cache backed by a shared one.
//
// Lookups try local first, because a local hit costs a file read and a remote
// hit costs a network round trip. A remote hit is copied into the local store
// on the way past, so the second build on this machine does not pay for it
// again. Writes go to both.
//
// The rule that governs every error path here: **the remote cache may never
// fail a build.** A server that is down, slow, wrong, or serving damaged bytes
// causes the action to run locally, exactly as if the cache had been empty. A
// shared cache is an optimization, and an optimization that can break the build
// is not one. Every failure is counted so `Stats` can say what happened instead
// of the build merely feeling slow.
type Tiered struct {
	local  *cache.Store
	client *Client

	mu    sync.Mutex
	stats Stats
}

// Stats counts what the remote tier did.
type Stats struct {
	LocalHits  int
	RemoteHits int
	Misses     int
	Uploads    int
	Errors     int
}

// NewTiered pairs a local store with a cache server. A nil client makes this a
// plain local cache, which is what `-remote` being unset produces.
func NewTiered(local *cache.Store, client *Client) *Tiered {
	return &Tiered{local: local, client: client}
}

// Stats returns a snapshot of the counters.
func (t *Tiered) Stats() Stats {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.stats
}

func (t *Tiered) count(f func(*Stats)) {
	t.mu.Lock()
	defer t.mu.Unlock()
	f(&t.stats)
}

// Lookup returns a usable entry, fetching it from the server if necessary.
func (t *Tiered) Lookup(key string) (*cache.Entry, error) {
	if entry, err := t.local.Lookup(key); err == nil && entry != nil {
		t.count(func(s *Stats) { s.LocalHits++ })
		return entry, nil
	}
	if t.client == nil {
		t.count(func(s *Stats) { s.Misses++ })
		return nil, nil
	}

	entry, err := t.client.GetEntry(key)
	if err != nil {
		// Unreachable, slow, or misbehaving. Run the action.
		t.count(func(s *Stats) { s.Errors++ })
		return nil, nil
	}
	if entry == nil {
		t.count(func(s *Stats) { s.Misses++ })
		return nil, nil
	}

	// An entry is only useful with its blobs. Downloading them into the local
	// store first means Restore never has to know where the entry came from,
	// and means a partial download becomes a miss rather than a half-restored
	// output tree.
	for _, o := range entry.Outputs {
		if t.local.HasBlob(o.Digest) {
			continue
		}
		b, err := t.client.GetBlob(o.Digest)
		if err != nil || b == nil {
			t.count(func(s *Stats) { s.Errors++ })
			return nil, nil
		}
		// WriteBlob re-verifies; bytes off a network are not trusted.
		if err := t.local.WriteBlob(o.Digest, b); err != nil {
			t.count(func(s *Stats) { s.Errors++ })
			return nil, nil
		}
	}

	if err := t.local.PutEntry(entry); err != nil {
		t.count(func(s *Stats) { s.Errors++ })
		return nil, nil
	}

	t.count(func(s *Stats) { s.RemoteHits++ })
	return entry, nil
}

// Restore materializes an entry from the local store, which Lookup has already
// made sure is populated.
func (t *Tiered) Restore(e *cache.Entry, workspace string) error {
	return t.local.Restore(e, workspace)
}

// Put records the action locally and then uploads it.
//
// The upload is synchronous. It is the simpler choice and it makes the
// cross-machine tests deterministic; a production cache would hand the upload
// to a background queue so an action's completion never waits on a network. The
// cost is visible in the numbers and is not hidden.
func (t *Tiered) Put(key, workspace string, outputs []string) (*cache.PutResult, error) {
	res, err := t.local.Put(key, workspace, outputs)
	if err != nil || t.client == nil {
		return res, err
	}

	for _, o := range res.Entry.Outputs {
		b, err := t.local.ReadBlob(o.Digest)
		if err != nil {
			t.count(func(s *Stats) { s.Errors++ })
			return res, nil
		}
		if err := t.client.PutBlob(o.Digest, b); err != nil {
			t.count(func(s *Stats) { s.Errors++ })
			return res, nil
		}
	}

	// The entry goes last. Until it exists no client can find these blobs, so
	// uploading it first would publish an entry whose outputs are not all there
	// yet — and another machine could take that as a hit.
	if err := t.client.PutEntry(res.Entry); err != nil {
		t.count(func(s *Stats) { s.Errors++ })
		return res, nil
	}

	t.count(func(s *Stats) { s.Uploads++ })
	return res, nil
}
