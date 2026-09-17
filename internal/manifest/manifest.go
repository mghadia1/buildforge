// Package manifest parses and validates a BuildForge build manifest.
//
// A manifest is the complete, declarative description of a build: every action,
// what it reads, what it writes, and what must run before it. BuildForge never
// discovers dependencies by watching the filesystem, because a dependency it
// cannot see is a dependency it cannot put in a cache key — and a cache key with
// a missing input produces a false cache hit, which is a silently wrong build.
package manifest

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strings"
)

// Action is one unit of work: run Command, consuming Inputs and producing
// Outputs. Deps names other actions that must complete first.
//
// Every field here is an input to the action's cache key, which is why the
// struct is deliberately small: anything that can change an action's output and
// is not represented here is a correctness bug waiting to happen.
type Action struct {
	Name    string            `json:"name"`
	Deps    []string          `json:"deps,omitempty"`
	Inputs  []string          `json:"inputs,omitempty"`
	Outputs []string          `json:"outputs"`
	Command []string          `json:"command"`
	Env     map[string]string `json:"env,omitempty"`
}

// Manifest is a parsed build file.
type Manifest struct {
	Actions []Action `json:"actions"`
}

// Load reads and validates a manifest from a file.
func Load(filename string) (*Manifest, error) {
	f, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	// defer runs when the function returns, however it returns. It is Go's
	// answer to try-with-resources.
	defer f.Close()

	m, err := Parse(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", filename, err)
	}
	return m, nil
}

// Parse decodes and validates a manifest.
func Parse(r io.Reader) (*Manifest, error) {
	dec := json.NewDecoder(r)

	// Reject fields the struct does not declare. A typo like "output" instead of
	// "outputs" would otherwise parse cleanly and produce an action that claims
	// to write nothing, which then caches wrongly. Fail at parse time instead.
	dec.DisallowUnknownFields()

	var m Manifest
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

// Validate checks every structural rule and reports *all* violations at once
// rather than stopping at the first, so one run surfaces every problem.
func (m *Manifest) Validate() error {
	if len(m.Actions) == 0 {
		return errors.New("manifest declares no actions")
	}

	var problems []error

	seen := make(map[string]bool, len(m.Actions))
	for i, a := range m.Actions {
		if a.Name == "" {
			problems = append(problems, fmt.Errorf("actions[%d]: name must not be empty", i))
			continue
		}
		if seen[a.Name] {
			problems = append(problems, fmt.Errorf("action %q: declared more than once", a.Name))
		}
		seen[a.Name] = true
	}

	for _, a := range m.Actions {
		if a.Name == "" {
			continue // already reported
		}

		if len(a.Command) == 0 {
			problems = append(problems, fmt.Errorf("action %q: command must not be empty", a.Name))
		}

		// An action with no declared outputs cannot be cached: the action cache
		// maps a cache key to a list of output digests, so an action with no
		// outputs has nothing to restore on a hit.
		if len(a.Outputs) == 0 {
			problems = append(problems, fmt.Errorf("action %q: must declare at least one output", a.Name))
		}

		for _, d := range a.Deps {
			switch {
			case d == a.Name:
				problems = append(problems, fmt.Errorf("action %q: depends on itself", a.Name))
			case !seen[d]:
				problems = append(problems, fmt.Errorf("action %q: depends on unknown action %q", a.Name, d))
			}
		}

		problems = append(problems, checkPaths(a.Name, "input", a.Inputs)...)
		problems = append(problems, checkPaths(a.Name, "output", a.Outputs)...)

		for k := range a.Env {
			if strings.TrimSpace(k) == "" {
				problems = append(problems, fmt.Errorf("action %q: env contains an empty variable name", a.Name))
			}
		}
	}

	// errors.Join returns nil when every element is nil, so this is also the
	// success path.
	return errors.Join(problems...)
}

// checkPaths enforces that every declared path is workspace-relative and stays
// inside the workspace.
//
// Relative paths are not a style preference. The cache key hashes these strings,
// so an absolute path would bake the checkout location into the key and no two
// machines would ever share a cache entry.
func checkPaths(action, kind string, paths []string) []error {
	var problems []error
	for _, p := range paths {
		switch {
		case p == "":
			problems = append(problems, fmt.Errorf("action %q: empty %s path", action, kind))
		case strings.HasPrefix(p, "/"):
			problems = append(problems, fmt.Errorf("action %q: %s %q must be workspace-relative, not absolute", action, kind, p))
		case p != path.Clean(p):
			problems = append(problems, fmt.Errorf("action %q: %s %q is not in canonical form (want %q)", action, kind, p, path.Clean(p)))
		case p == ".." || strings.HasPrefix(p, "../"):
			problems = append(problems, fmt.Errorf("action %q: %s %q escapes the workspace", action, kind, p))
		}
	}
	return problems
}

// Names returns every action name in sorted order.
//
// Sorted, not manifest order: several later stages iterate this, and a stable
// order is what makes builds reproducible and test output comparable.
func (m *Manifest) Names() []string {
	names := make([]string, 0, len(m.Actions))
	for _, a := range m.Actions {
		names = append(names, a.Name)
	}
	sort.Strings(names)
	return names
}
