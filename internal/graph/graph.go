// Package graph turns a validated manifest into an action DAG and orders it for
// execution.
//
// Two jobs, kept separate on purpose:
//
//   - produce a topological order, so the executor never runs an action before
//     its dependencies;
//   - when no such order exists, report *which* actions form the cycle. "Cycle
//     detected" is not a usable error message in a build file with hundreds of
//     targets.
package graph

import (
	"fmt"
	"sort"
	"strings"

	"github.com/mghadia1/buildforge/internal/manifest"
)

// Graph is an immutable action DAG.
type Graph struct {
	actions map[string]manifest.Action
	deps    map[string][]string // action -> actions it waits on
	rdeps   map[string][]string // action -> actions waiting on it
	names   []string            // sorted
}

// CycleError reports a dependency cycle, naming every action in it.
//
// Cycle is closed: the first element is repeated as the last, so printing it
// with " -> " reads as a loop.
type CycleError struct {
	Cycle []string
}

func (e *CycleError) Error() string {
	return fmt.Sprintf("dependency cycle: %s", strings.Join(e.Cycle, " -> "))
}

// New builds a graph from a manifest. The manifest must already be valid;
// Load and Parse guarantee that.
func New(m *manifest.Manifest) (*Graph, error) {
	g := &Graph{
		actions: make(map[string]manifest.Action, len(m.Actions)),
		deps:    make(map[string][]string, len(m.Actions)),
		rdeps:   make(map[string][]string, len(m.Actions)),
		names:   m.Names(),
	}

	for _, a := range m.Actions {
		g.actions[a.Name] = a
	}

	for _, a := range m.Actions {
		// Copy and sort so iteration order never depends on how the manifest
		// happened to be written.
		ds := append([]string(nil), a.Deps...)
		sort.Strings(ds)

		for _, d := range ds {
			if _, ok := g.actions[d]; !ok {
				return nil, fmt.Errorf("action %q: depends on unknown action %q", a.Name, d)
			}
			g.deps[a.Name] = append(g.deps[a.Name], d)
			g.rdeps[d] = append(g.rdeps[d], a.Name)
		}
	}

	for name := range g.rdeps {
		sort.Strings(g.rdeps[name])
	}
	return g, nil
}

// Names returns every action name, sorted.
func (g *Graph) Names() []string { return append([]string(nil), g.names...) }

// Action returns the action with the given name.
func (g *Graph) Action(name string) (manifest.Action, bool) {
	a, ok := g.actions[name]
	return a, ok
}

// Dependencies returns the actions that name waits on, sorted.
func (g *Graph) Dependencies(name string) []string {
	return append([]string(nil), g.deps[name]...)
}

// Dependents returns the actions waiting on name, sorted. The executor needs
// this to decide what becomes runnable when an action finishes.
func (g *Graph) Dependents(name string) []string {
	return append([]string(nil), g.rdeps[name]...)
}

// TopologicalOrder returns an order in which every action appears after all of
// its dependencies. It returns a *CycleError if no such order exists.
//
// Kahn's algorithm: repeatedly emit an action whose dependencies are all
// already emitted. If it stalls with actions left over, those actions are
// reachable from a cycle — but Kahn does not say which cycle, so findCycle does
// a second pass to recover the actual loop for the error message.
//
// The ready set is drained in sorted order, so the same manifest always yields
// the same sequence. That matters more than it looks: a reproducible order is
// what lets two runs be compared at all.
func (g *Graph) TopologicalOrder() ([]string, error) {
	remaining := make(map[string]int, len(g.names))
	var ready []string

	for _, n := range g.names {
		remaining[n] = len(g.deps[n])
		if remaining[n] == 0 {
			ready = append(ready, n)
		}
	}
	sort.Strings(ready)

	order := make([]string, 0, len(g.names))
	for len(ready) > 0 {
		// Pop the lexicographically smallest ready action.
		n := ready[0]
		ready = ready[1:]
		order = append(order, n)

		var unblocked []string
		for _, dep := range g.rdeps[n] {
			remaining[dep]--
			if remaining[dep] == 0 {
				unblocked = append(unblocked, dep)
			}
		}
		if len(unblocked) > 0 {
			ready = append(ready, unblocked...)
			sort.Strings(ready)
		}
	}

	if len(order) != len(g.names) {
		return nil, &CycleError{Cycle: g.findCycle(remaining)}
	}
	return order, nil
}

// findCycle locates one concrete cycle among the actions Kahn could not emit.
//
// Depth-first search with three colors: white unvisited, gray on the current
// stack, black fully explored. Reaching a gray action means the stack has
// closed a loop, and the stack from that action to the top *is* the loop.
func (g *Graph) findCycle(remaining map[string]int) []string {
	const (
		white = 0
		gray  = 1
		black = 2
	)

	color := make(map[string]int, len(g.names))
	var stack []string
	var cycle []string

	var visit func(string) bool
	visit = func(n string) bool {
		color[n] = gray
		stack = append(stack, n)

		for _, d := range g.deps[n] {
			// Kahn emitted d, so d reached zero remaining dependencies and
			// cannot be on a cycle. Skip that whole subtree.
			if remaining[d] == 0 {
				continue
			}
			switch color[d] {
			case gray:
				// Found it. Cut the stack at d and close the loop.
				for i, s := range stack {
					if s == d {
						cycle = append(append([]string(nil), stack[i:]...), d)
						return true
					}
				}
			case white:
				if visit(d) {
					return true
				}
			}
		}

		stack = stack[:len(stack)-1]
		color[n] = black
		return false
	}

	for _, n := range g.names {
		if remaining[n] > 0 && color[n] == white {
			if visit(n) {
				return cycle
			}
		}
	}

	// Unreachable while TopologicalOrder only calls this after a stall, but a
	// nil cycle would print as an empty message, so say something true instead.
	return []string{"<unknown>"}
}
