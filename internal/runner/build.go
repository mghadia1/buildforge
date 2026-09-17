package runner

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/mghadia1/buildforge/internal/cache"
	"github.com/mghadia1/buildforge/internal/graph"
)

// Options configures a build.
type Options struct {
	// Workspace is the root every declared path resolves against.
	Workspace string

	// Progress receives one line per action as it starts. Nil is silent.
	Progress io.Writer

	// KeepGoing continues after a failed action instead of stopping at the
	// first one. Actions downstream of a failure are skipped either way,
	// because their inputs do not exist.
	KeepGoing bool

	// Cache, when set, is consulted before each action and updated after it
	// succeeds. Leave it unset to disable caching — see the trap noted on the
	// Cache interface about typed nils.
	Cache Cache

	// SandboxRoot, when set, runs each action against only its declared
	// inputs. Empty disables the sandbox, which is what the demonstration of
	// the false cache hit relies on.
	SandboxRoot string
}

// Summary is the outcome of a whole build.
//
// Results holds every action that executed, including the ones that failed;
// Failed names that subset. Succeeded, Failed, and Skipped together account for
// every action in the graph.
type Summary struct {
	Results []*Result
	Failed  []string
	Skipped []string
	Elapsed time.Duration
}

// Succeeded is the number of actions that ran and exited cleanly.
func (s *Summary) Succeeded() int { return len(s.Results) - len(s.Failed) }

// Cached is the number of actions restored from the cache without running.
func (s *Summary) Cached() int {
	n := 0
	for _, r := range s.Results {
		if r.Cached {
			n++
		}
	}
	return n
}

// Nondeterministic names every output that differed from a previous run under
// an identical cache key.
func (s *Summary) Nondeterministic() []string {
	var out []string
	for _, r := range s.Results {
		out = append(out, r.Nondeterministic...)
	}
	return out
}

// Build executes every action in dependency order, one at a time.
//
// Sequential on purpose: Milestone 2 establishes that the build is *correct*,
// so that Milestone 3's parallel scheduler has a known-good result to be
// checked against. A parallel executor written first has nothing to be wrong
// relative to.
func Build(ctx context.Context, g *graph.Graph, opts Options) (*Summary, error) {
	order, err := g.TopologicalOrder()
	if err != nil {
		return nil, err
	}

	r := &Runner{
		Workspace:   opts.Workspace,
		Cache:       opts.Cache,
		Tools:       cache.NewToolCache(),
		SandboxRoot: opts.SandboxRoot,
	}
	sum := &Summary{}
	start := time.Now()

	// An action whose dependency failed cannot run: its inputs were never
	// produced. Tracking this separately from failure keeps the summary honest
	// about what broke versus what never got a chance.
	poisoned := make(map[string]bool)

	var firstErr error

	for i, name := range order {
		a, ok := g.Action(name)
		if !ok {
			return nil, fmt.Errorf("internal: action %q in order but not in graph", name)
		}

		if poisoned[name] {
			sum.Skipped = append(sum.Skipped, name)
			markDependents(g, name, poisoned)
			continue
		}

		// Check cancellation between actions as well as during them, so a
		// cancelled build stops even if every action is fast.
		if err := ctx.Err(); err != nil {
			sum.Skipped = append(sum.Skipped, name)
			markDependents(g, name, poisoned)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}

		if opts.Progress != nil {
			fmt.Fprintf(opts.Progress, "run  %s\n", name)
		}

		res, runErr := r.Run(ctx, a)
		if res != nil {
			sum.Results = append(sum.Results, res)
		}
		if runErr == nil {
			continue
		}

		sum.Failed = append(sum.Failed, name)
		markDependents(g, name, poisoned)
		if firstErr == nil {
			firstErr = runErr
		}
		if !opts.KeepGoing {
			// Everything after this point never gets a chance to run. Record it
			// so the summary accounts for every action in the graph: a build
			// tool whose own numbers do not add up to the total teaches its
			// users to distrust them.
			sum.Skipped = append(sum.Skipped, order[i+1:]...)
			sum.Elapsed = time.Since(start)
			return sum, firstErr
		}
	}

	sum.Elapsed = time.Since(start)
	return sum, firstErr
}

// markDependents poisons everything transitively downstream of name.
func markDependents(g *graph.Graph, name string, poisoned map[string]bool) {
	for _, d := range g.Dependents(name) {
		if poisoned[d] {
			continue // already marked; also stops infinite recursion
		}
		poisoned[d] = true
		markDependents(g, d, poisoned)
	}
}
