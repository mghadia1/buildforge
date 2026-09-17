// Package runner executes actions.
//
// The directory is named runner rather than exec so that the standard library's
// os/exec can be imported under its own name throughout.
//
// One action at a time here; Milestone 3 adds the scheduler that runs many at
// once. Everything in this file is written so that adding concurrency later
// changes the scheduling, not the execution: a Runner holds no per-action state,
// every call takes a context, and results are returned rather than accumulated.
package runner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"time"

	"github.com/mghadia1/buildforge/internal/cache"
	"github.com/mghadia1/buildforge/internal/manifest"
)

// Cache is what a Runner needs from a cache. Declaring it here rather than in
// package cache is the Go convention: the consumer states what it requires, and
// both the local store and the tiered local-plus-remote cache satisfy it
// without either knowing this interface exists.
//
// A trap worth naming, because it bites everyone once: assigning a nil
// *cache.Store to this interface produces a value that is NOT equal to nil, and
// the nil checks below would sail past it straight into a nil dereference.
// Callers must leave the field unset rather than setting it to a typed nil.
type Cache interface {
	Lookup(key string) (*cache.Entry, error)
	Restore(e *cache.Entry, workspace string) error
	Put(key, workspace string, outputs []string) (*cache.PutResult, error)
}

// Runner executes single actions inside a workspace.
type Runner struct {
	// Workspace is the root every declared path resolves against.
	Workspace string

	// Cache, when set, is consulted before an action runs and updated after it
	// succeeds. A nil Cache disables caching entirely, which is what the
	// benchmark's clean-build configuration and several tests rely on.
	Cache Cache

	// Tools digests tool binaries for the cache key. Shared across a build so
	// each compiler is hashed once rather than once per action.
	Tools *cache.ToolCache

	// SandboxRoot, when set, makes each action run in a directory holding
	// symlinks to exactly its declared inputs. Empty means no sandbox, and an
	// action can then read anything in the workspace — including files it did
	// not declare, which is how a false cache hit is born.
	SandboxRoot string
}

// Result records one execution. The timestamps are unused in Milestone 2 and
// are collected anyway: Milestone 3's critical-path measurement needs them, and
// recording them after the fact would mean re-running every benchmark.
type Result struct {
	Action   string
	ExitCode int
	Stdout   []byte
	Stderr   []byte
	Start    time.Time
	End      time.Time

	// Cached is true when the outputs were restored from the cache and the
	// command never ran.
	Cached bool

	// Nondeterministic names outputs that differed from a previous run under an
	// identical cache key. Such an action cannot be cached reliably.
	Nondeterministic []string
}

// Duration is the wall time the action took.
func (r *Result) Duration() time.Duration { return r.End.Sub(r.Start) }

// ActionError reports an action that did not succeed, naming it.
//
// "the build failed" is not a usable message. Every failure path in this
// package carries the action name, and a command failure also carries the
// captured stderr, because that is the part a person actually needs.
type ActionError struct {
	Action   string
	ExitCode int
	Stderr   []byte
	Err      error
}

func (e *ActionError) Error() string {
	if e.ExitCode != 0 {
		return fmt.Sprintf("action %q failed with exit code %d", e.Action, e.ExitCode)
	}
	return fmt.Sprintf("action %q: %v", e.Action, e.Err)
}

// Unwrap lets errors.Is and errors.As see the underlying cause, so a caller can
// still detect context.Canceled through the wrapping.
func (e *ActionError) Unwrap() error { return e.Err }

// Run executes one action and returns its result.
//
// The sequence is deliberate: check inputs, create output directories, run,
// then verify outputs. Checking inputs first turns a manifest mistake into a
// clear error instead of an obscure failure from the compiler. Verifying
// outputs afterwards catches an action that exits zero without producing what
// it promised — which Milestone 4 cannot tolerate, because the cache stores an
// action's outputs by digest and has nothing to store if they do not exist.
func (r *Runner) Run(ctx context.Context, a manifest.Action) (*Result, error) {
	if err := r.checkInputs(a); err != nil {
		return nil, &ActionError{Action: a.Name, Err: err}
	}

	// The key is computed after the input check, so it is only ever taken over
	// inputs that exist.
	key, err := r.cacheKey(a)
	if err != nil {
		return nil, &ActionError{Action: a.Name, Err: err}
	}
	if res := r.tryRestore(a, key); res != nil {
		return res, nil
	}

	// execDir is where the command actually runs: a sandbox holding only the
	// declared inputs when one is configured, the workspace itself otherwise.
	execDir := r.Workspace
	var box *sandbox

	if r.SandboxRoot != "" {
		box, err = newSandbox(r.SandboxRoot, r.Workspace, a)
		if err != nil {
			return nil, &ActionError{Action: a.Name, Err: err}
		}
		defer box.remove()
		execDir = box.dir
	} else if err := r.prepareOutputDirs(a); err != nil {
		return nil, &ActionError{Action: a.Name, Err: err}
	}

	var stdout, stderr bytes.Buffer

	// CommandContext kills the process if ctx is cancelled. Milestone 3 relies
	// on this to stop in-flight work when a sibling action fails.
	cmd := exec.CommandContext(ctx, a.Command[0], a.Command[1:]...)
	setupProcessGroup(cmd)
	cmd.Dir = execDir
	cmd.Env = r.environ(a)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	res := &Result{Action: a.Name, Start: time.Now()}
	err = cmd.Run()
	res.End = time.Now()
	res.Stdout = stdout.Bytes()
	res.Stderr = stderr.Bytes()

	if err != nil {
		// A cancelled context is not the action's fault; report it as itself so
		// a caller can distinguish "we stopped the build" from "this command is
		// broken".
		if ctxErr := ctx.Err(); ctxErr != nil {
			return res, &ActionError{Action: a.Name, Err: ctxErr}
		}

		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			res.ExitCode = exitErr.ExitCode()
			return res, &ActionError{
				Action:   a.Name,
				ExitCode: exitErr.ExitCode(),
				Stderr:   res.Stderr,
				Err:      err,
			}
		}
		// Not an exit status: the binary was missing, not executable, and so on.
		return res, &ActionError{Action: a.Name, Stderr: res.Stderr, Err: err}
	}

	// Move the declared outputs out of the sandbox before anything looks for
	// them in the workspace. Undeclared files the action wrote are left behind
	// and deleted with the sandbox.
	if box != nil {
		if err := box.harvest(r.Workspace, a.Outputs); err != nil {
			return res, &ActionError{Action: a.Name, Stderr: res.Stderr, Err: err}
		}
	}

	if err := r.checkOutputs(a); err != nil {
		return res, &ActionError{Action: a.Name, Stderr: res.Stderr, Err: err}
	}

	if r.Cache != nil && key != "" {
		put, err := r.Cache.Put(key, r.Workspace, a.Outputs)
		if err != nil {
			// A cache that cannot be written is a performance problem, not a
			// correctness one: the action ran and its outputs are in place.
			// Failing the build here would turn a full disk into a broken build.
			return res, nil
		}
		res.Nondeterministic = put.Nondeterministic
	}
	return res, nil
}

// cacheKey computes the action's cache key, or "" when caching is disabled.
func (r *Runner) cacheKey(a manifest.Action) (string, error) {
	if r.Cache == nil {
		return "", nil
	}
	tools := r.Tools
	if tools == nil {
		tools = cache.NewToolCache()
	}
	return cache.Key(a, r.Workspace, tools)
}

// tryRestore returns a cached result, or nil to run the action.
//
// Every failure below falls through to running the action. A cache that is
// missing, damaged, or unreadable must cost time and never correctness, so
// there is deliberately no path here that fails a build.
func (r *Runner) tryRestore(a manifest.Action, key string) *Result {
	if r.Cache == nil || key == "" {
		return nil
	}

	entry, err := r.Cache.Lookup(key)
	if err != nil || entry == nil {
		return nil
	}

	now := time.Now()
	if err := r.Cache.Restore(entry, r.Workspace); err != nil {
		return nil
	}
	return &Result{Action: a.Name, Cached: true, Start: now, End: time.Now()}
}

// environ builds the action's environment.
//
// The parent environment is *not* inherited. An inherited variable can change
// an action's output while being invisible to the cache key, which is exactly
// the shape of a false cache hit. Only declared variables are passed.
//
// PATH is the one exception, and it is a known hole rather than a design: the
// command has to be findable. PATH is not part of the cache key, so two
// machines with different toolchains on PATH can currently share a cache entry
// they should not. Milestone 4 narrows this by hashing the toolchain version
// into the key; Milestone 5 closes it with a sandbox. Recorded here so the gap
// is documented where the decision lives.
//
// Entries are sorted because Go randomizes map iteration order, and an
// environment that differs run to run would make builds irreproducible for no
// reason. Declared variables come last: os/exec keeps the final value when a
// key appears twice, so an action that declares its own PATH wins.
func (r *Runner) environ(a manifest.Action) []string {
	env := make([]string, 0, len(a.Env)+1)
	if p := os.Getenv("PATH"); p != "" {
		env = append(env, "PATH="+p)
	}

	declared := make([]string, 0, len(a.Env))
	for k, v := range a.Env {
		declared = append(declared, k+"="+v)
	}
	sort.Strings(declared)

	return append(env, declared...)
}

func (r *Runner) checkInputs(a manifest.Action) error {
	for _, in := range a.Inputs {
		if _, err := os.Stat(filepath.Join(r.Workspace, in)); err != nil {
			return fmt.Errorf("declared input %q is missing: %w", in, err)
		}
	}
	return nil
}

func (r *Runner) prepareOutputDirs(a manifest.Action) error {
	for _, out := range a.Outputs {
		dir := filepath.Dir(filepath.Join(r.Workspace, out))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("creating output directory for %q: %w", out, err)
		}
	}
	return nil
}

func (r *Runner) checkOutputs(a manifest.Action) error {
	for _, out := range a.Outputs {
		if _, err := os.Stat(filepath.Join(r.Workspace, out)); err != nil {
			return fmt.Errorf("declared output %q was not produced: %w", out, err)
		}
	}
	return nil
}
