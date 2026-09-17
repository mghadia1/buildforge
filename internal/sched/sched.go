// Package sched runs a build in parallel.
//
// The sequential driver in package runner stays as the reference
// implementation. It is not dead code: the differential test runs both over the
// same graph and requires byte-identical outputs, which is the only cheap way
// to catch a scheduler that is fast and subtly wrong.
package sched

import (
	"context"
	"fmt"
	"runtime"
	"sort"
	"sync"
	"time"

	"github.com/mghadia1/buildforge/internal/cache"
	"github.com/mghadia1/buildforge/internal/graph"
	"github.com/mghadia1/buildforge/internal/runner"
)

// Options configures a parallel build.
type Options struct {
	runner.Options

	// Workers bounds how many actions run at once. Zero means one per CPU.
	Workers int
}

// status is what became of one action.
type status int

const (
	statusOK status = iota
	statusFailed
	statusSkipped
)

// Build executes the graph in parallel and returns the same Summary shape the
// sequential driver returns.
//
// Design: one goroutine per action, each blocking on its dependencies' done
// channels, with a buffered channel as a counting semaphore to bound how many
// run at once.
//
// The alternative is a fixed worker pool pulling from a ready queue. Both are
// correct; this one is chosen because the dispatch rule falls out of the
// structure instead of being maintained by hand. An action starts the instant
// its last dependency closes its channel — not at the start of the next
// topological wave, which is the classic way to accidentally serialize a wide
// graph. The cost is a goroutine per action (a few KB each, parked on a channel
// receive), so a graph of millions of actions would want the worker pool
// instead. Builds of that size are not the target.
func Build(ctx context.Context, g *graph.Graph, opts Options) (*runner.Summary, error) {
	// A cycle has to be caught before anything starts; the topological order is
	// otherwise unused here, since dependencies are enforced by the channels.
	if _, err := g.TopologicalOrder(); err != nil {
		return nil, err
	}

	workers := opts.Workers
	if workers <= 0 {
		workers = runtime.NumCPU()
	}

	// Cancelling this context kills in-flight commands. Under fail-fast, the
	// first failure cancels it; the process groups set up in package runner are
	// what make that actually stop the work rather than orphan it.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	names := g.Names()

	// Every done channel must exist before any goroutine starts, or a goroutine
	// could read the map while another writes it.
	done := make(map[string]chan struct{}, len(names))
	for _, n := range names {
		done[n] = make(chan struct{})
	}

	sem := make(chan struct{}, workers)
	// One Runner shared by every goroutine. It holds no per-action state, and
	// the tool cache inside it is mutex-guarded, so each compiler is digested
	// once for the whole build rather than once per action.
	r := &runner.Runner{
		Workspace:   opts.Workspace,
		Cache:       opts.Cache,
		Tools:       cache.NewToolCache(),
		SandboxRoot: opts.SandboxRoot,
	}

	var (
		mu       sync.Mutex // guards everything below, including opts.Progress
		state    = make(map[string]status, len(names))
		results  []*runner.Result
		failed   []string
		skipped  []string
		firstErr error
	)

	// skip records an action that never ran and releases anything waiting on it.
	skip := func(n string) {
		mu.Lock()
		state[n] = statusSkipped
		skipped = append(skipped, n)
		mu.Unlock()
	}

	var wg sync.WaitGroup
	start := time.Now()

	for _, name := range names {
		wg.Add(1)

		go func(name string) {
			defer wg.Done()
			// Closing this releases every action waiting on it, however this
			// goroutine exits. Without the defer, one early return deadlocks
			// the entire build.
			defer close(done[name])

			// Wait for dependencies. A cancelled build stops waiting.
			for _, d := range g.Dependencies(name) {
				select {
				case <-done[d]:
				case <-ctx.Done():
					skip(name)
					return
				}
			}

			// A dependency that failed or was skipped never produced this
			// action's inputs, so running it would fail confusingly.
			mu.Lock()
			for _, d := range g.Dependencies(name) {
				if state[d] != statusOK {
					mu.Unlock()
					skip(name)
					return
				}
			}
			mu.Unlock()

			if ctx.Err() != nil {
				skip(name)
				return
			}

			// Acquire a slot. This is the only thing bounding parallelism.
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				skip(name)
				return
			}

			a, ok := g.Action(name)
			if !ok { // unreachable: names came from the graph
				skip(name)
				return
			}

			if opts.Progress != nil {
				mu.Lock()
				fmt.Fprintf(opts.Progress, "run  %s\n", name)
				mu.Unlock()
			}

			res, runErr := r.Run(ctx, a)

			mu.Lock()
			if res != nil {
				results = append(results, res)
			}
			if runErr == nil {
				state[name] = statusOK
				mu.Unlock()
				return
			}
			state[name] = statusFailed
			failed = append(failed, name)
			if firstErr == nil {
				firstErr = runErr
			}
			mu.Unlock()

			if !opts.KeepGoing {
				// Stop in-flight work. Actions already waiting see ctx.Done and
				// skip themselves.
				cancel()
			}
		}(name)
	}

	wg.Wait()
	elapsed := time.Since(start)

	// Goroutines finish in whatever order the scheduler chooses, so sort before
	// reporting. Results go in chronological order because that is how a build
	// log reads; the name lists sort by name so two runs can be diffed.
	sort.Slice(results, func(i, j int) bool {
		if results[i].Start.Equal(results[j].Start) {
			return results[i].Action < results[j].Action
		}
		return results[i].Start.Before(results[j].Start)
	})
	sort.Strings(failed)
	sort.Strings(skipped)

	return &runner.Summary{
		Results: results,
		Failed:  failed,
		Skipped: skipped,
		Elapsed: elapsed,
	}, firstErr
}

// Durations extracts per-action wall times for graph.CriticalPath.
func Durations(sum *runner.Summary) map[string]time.Duration {
	dur := make(map[string]time.Duration, len(sum.Results))
	for _, r := range sum.Results {
		dur[r.Action] = r.Duration()
	}
	return dur
}
