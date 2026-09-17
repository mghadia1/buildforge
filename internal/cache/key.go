// Package cache computes action keys and stores action results by content.
package cache

import (
	"bytes"
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"sync"

	"github.com/mghadia1/buildforge/internal/digest"
	"github.com/mghadia1/buildforge/internal/manifest"
)

// keyVersion is mixed into every key. Bumping it invalidates every entry ever
// written, which is the correct response to changing what a key covers: an old
// entry computed under different rules is not merely stale, it is wrong.
const keyVersion = "buildforge-key-v1"

// Key computes the cache key for an action.
//
// Everything that can change the action's output has to be in here, because
// anything left out is a false cache hit waiting to happen. Five components:
//
//   - the contents of every declared input, by digest;
//   - the command line;
//   - the declared environment;
//   - the tool being run, by digest of the binary itself;
//   - the platform.
//
// Note what is absent. Modification times are not included, so touching a file
// without editing it still hits. Absolute paths are not included — inputs are
// workspace-relative — so the same source tree checked out at two different
// locations produces the same key and can share a cache.
func Key(a manifest.Action, workspace string, tools *ToolCache) (string, error) {
	var buf bytes.Buffer

	// Every component is written length-prefixed. Plain concatenation is
	// ambiguous: the commands ["ab", "c"] and ["a", "bc"] would otherwise
	// produce identical bytes and therefore identical keys, and one would
	// silently return the other's outputs.
	write := func(tag, value string) {
		fmt.Fprintf(&buf, "%s %d\n%s\n", tag, len(value), value)
	}

	write("version", keyVersion)
	write("platform", runtime.GOOS+"/"+runtime.GOARCH)

	for _, arg := range a.Command {
		write("arg", arg)
	}

	// Go randomizes map iteration, so the environment must be sorted or the
	// same action would key differently on every run.
	envKeys := make([]string, 0, len(a.Env))
	for k := range a.Env {
		envKeys = append(envKeys, k)
	}
	sort.Strings(envKeys)
	for _, k := range envKeys {
		write("env", k+"="+a.Env[k])
	}

	toolDigest, err := tools.Digest(a.Command[0])
	if err != nil {
		return "", fmt.Errorf("digesting tool %q: %w", a.Command[0], err)
	}
	write("tool", string(toolDigest))

	// Inputs are sorted by path so that reordering them in the manifest, which
	// cannot change what the action reads, does not change the key.
	inputs := append([]string(nil), a.Inputs...)
	sort.Strings(inputs)

	abs := make([]string, len(inputs))
	for i, in := range inputs {
		abs[i] = filepath.Join(workspace, in)
	}
	digests, err := digest.Files(abs)
	if err != nil {
		return "", err
	}
	for i, in := range inputs {
		write("input", in+" "+string(digests[abs[i]]))
	}

	// Declared outputs are part of the key because they determine what a hit
	// restores. An action that declares a second output is a different action.
	outputs := append([]string(nil), a.Outputs...)
	sort.Strings(outputs)
	for _, out := range outputs {
		write("output", out)
	}

	return string(digest.Bytes(buf.Bytes())), nil
}

// ToolCache digests tool binaries, remembering each one for the life of the
// process.
//
// The tool belongs in the key: a compiler upgrade changes the output of every
// compile, and a cache that does not notice would serve the old objects
// forever. Hashing a hundred-megabyte compiler once per action would dominate
// the build, so each resolved path is hashed once and reused.
//
// The gap, stated plainly: this covers the tool's own bytes and nothing else.
// A compiler that reads a configuration file, or links a shared library that
// changed underneath it, is not detected here. Closing that needs the sandbox
// in Milestone 5.
type ToolCache struct {
	mu sync.Mutex
	m  map[string]digest.Digest
}

// NewToolCache returns an empty tool cache.
func NewToolCache() *ToolCache { return &ToolCache{m: make(map[string]digest.Digest)} }

// Digest returns the digest of the binary that name resolves to.
func (t *ToolCache) Digest(name string) (digest.Digest, error) {
	path, err := exec.LookPath(name)
	if err != nil {
		return "", err
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	if d, ok := t.m[path]; ok {
		return d, nil
	}
	d, err := digest.File(path)
	if err != nil {
		return "", err
	}
	t.m[path] = d
	return d, nil
}
