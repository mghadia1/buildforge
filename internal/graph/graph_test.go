package graph

import (
	"errors"
	"strings"
	"testing"

	"github.com/mghadia1/buildforge/internal/manifest"
)

// build constructs a graph from name -> dependencies, skipping the manifest's
// path and command rules so the tests stay about graph shape.
func build(t *testing.T, deps map[string][]string) *Graph {
	t.Helper()

	m := &manifest.Manifest{}
	for name, ds := range deps {
		m.Actions = append(m.Actions, manifest.Action{
			Name:    name,
			Deps:    ds,
			Outputs: []string{"out/" + name},
			Command: []string{"true"},
		})
	}

	g, err := New(m)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return g
}

func TestTopologicalOrderRespectsDependencies(t *testing.T) {
	t.Parallel()

	// A diamond: d waits on b and c, both of which wait on a.
	g := build(t, map[string][]string{
		"a": nil,
		"b": {"a"},
		"c": {"a"},
		"d": {"b", "c"},
	})

	order, err := g.TopologicalOrder()
	if err != nil {
		t.Fatalf("TopologicalOrder: %v", err)
	}
	if len(order) != 4 {
		t.Fatalf("order = %v, want 4 actions", order)
	}

	pos := make(map[string]int, len(order))
	for i, n := range order {
		pos[n] = i
	}
	for _, n := range order {
		for _, d := range g.Dependencies(n) {
			if pos[d] >= pos[n] {
				t.Errorf("%s at %d runs before its dependency %s at %d", n, pos[n], d, pos[d])
			}
		}
	}
}

func TestTopologicalOrderIsDeterministic(t *testing.T) {
	t.Parallel()

	// Two runs of the same graph must give the identical sequence. Without
	// this, no two builds are comparable and no benchmark means anything.
	g := build(t, map[string][]string{
		"a": nil,
		"b": nil,
		"c": nil,
		"d": {"a", "b", "c"},
	})

	first, err := g.TopologicalOrder()
	if err != nil {
		t.Fatalf("TopologicalOrder: %v", err)
	}
	for i := 0; i < 10; i++ {
		again, err := g.TopologicalOrder()
		if err != nil {
			t.Fatalf("TopologicalOrder: %v", err)
		}
		if strings.Join(again, ",") != strings.Join(first, ",") {
			t.Fatalf("run %d = %v, want %v", i, again, first)
		}
	}
	if got, want := strings.Join(first, ","), "a,b,c,d"; got != want {
		t.Fatalf("order = %q, want %q (ready set drained in sorted order)", got, want)
	}
}

func TestCycleErrorNamesTheActions(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		deps map[string][]string
		want []string // every action that must appear in the message
	}{
		{
			name: "two-cycle",
			deps: map[string][]string{"a": {"b"}, "b": {"a"}},
			want: []string{"a", "b"},
		},
		{
			name: "three-cycle",
			deps: map[string][]string{"a": {"c"}, "b": {"a"}, "c": {"b"}},
			want: []string{"a", "b", "c"},
		},
		{
			// The cycle is b->c->b; a and d hang off it and must not be blamed.
			name: "cycle with clean actions attached",
			deps: map[string][]string{
				"a": nil,
				"b": {"a", "c"},
				"c": {"b"},
				"d": {"a"},
			},
			want: []string{"b", "c"},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			g := build(t, tc.deps)
			_, err := g.TopologicalOrder()
			if err == nil {
				t.Fatal("TopologicalOrder succeeded, want a cycle error")
			}

			// errors.As is how Go asks "is this error of that type?" — it
			// unwraps through any wrapping on the way.
			var ce *CycleError
			if !errors.As(err, &ce) {
				t.Fatalf("error = %T (%v), want *CycleError", err, err)
			}
			for _, n := range tc.want {
				if !containsAction(ce.Cycle, n) {
					t.Errorf("cycle %v does not name %q", ce.Cycle, n)
				}
			}
			// A closed loop: first element repeated at the end.
			if len(ce.Cycle) < 3 || ce.Cycle[0] != ce.Cycle[len(ce.Cycle)-1] {
				t.Errorf("cycle %v is not closed", ce.Cycle)
			}
		})
	}
}

func TestCycleErrorDoesNotBlameCleanActions(t *testing.T) {
	t.Parallel()

	g := build(t, map[string][]string{
		"a": nil,
		"b": {"a", "c"},
		"c": {"b"},
		"d": {"a"},
	})

	_, err := g.TopologicalOrder()
	var ce *CycleError
	if !errors.As(err, &ce) {
		t.Fatalf("error = %v, want *CycleError", err)
	}
	for _, innocent := range []string{"a", "d"} {
		if containsAction(ce.Cycle, innocent) {
			t.Errorf("cycle %v blames %q, which is not on a cycle", ce.Cycle, innocent)
		}
	}
}

func TestDependentsIsTheReverseOfDependencies(t *testing.T) {
	t.Parallel()

	// The executor uses Dependents to decide what becomes runnable when an
	// action finishes, so the two views have to agree.
	g := build(t, map[string][]string{
		"a": nil,
		"b": {"a"},
		"c": {"a"},
	})

	if got, want := strings.Join(g.Dependents("a"), ","), "b,c"; got != want {
		t.Fatalf("Dependents(a) = %q, want %q", got, want)
	}
	if got := g.Dependents("b"); len(got) != 0 {
		t.Fatalf("Dependents(b) = %v, want none", got)
	}
}

func containsAction(cycle []string, name string) bool {
	for _, n := range cycle {
		if n == name {
			return true
		}
	}
	return false
}
