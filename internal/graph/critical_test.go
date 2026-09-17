package graph

import (
	"strings"
	"testing"
	"time"
)

func ms(n int) time.Duration { return time.Duration(n) * time.Millisecond }

func TestCriticalPath(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		deps     map[string][]string
		dur      map[string]time.Duration
		wantPath string
		wantLen  time.Duration
	}{
		{
			// A chain has only one possible path.
			name:     "linear chain",
			deps:     map[string][]string{"a": nil, "b": {"a"}, "c": {"b"}},
			dur:      map[string]time.Duration{"a": ms(10), "b": ms(20), "c": ms(5)},
			wantPath: "a,b,c",
			wantLen:  ms(35),
		},
		{
			// Two independent branches: the slow one is critical, and the fast
			// one is free — it runs in the shadow of the slow one.
			name: "diamond picks the slow branch",
			deps: map[string][]string{
				"start": nil,
				"slow":  {"start"},
				"fast":  {"start"},
				"end":   {"slow", "fast"},
			},
			dur: map[string]time.Duration{
				"start": ms(10), "slow": ms(100), "fast": ms(5), "end": ms(10),
			},
			wantPath: "start,slow,end",
			wantLen:  ms(120),
		},
		{
			// Fully parallel: the path is whichever single action is longest.
			name:     "no dependencies",
			deps:     map[string][]string{"a": nil, "b": nil, "c": nil},
			dur:      map[string]time.Duration{"a": ms(10), "b": ms(50), "c": ms(20)},
			wantPath: "b",
			wantLen:  ms(50),
		},
		{
			// An action with no recorded duration (skipped, or never reached)
			// contributes nothing rather than breaking the computation.
			name:     "missing durations count as zero",
			deps:     map[string][]string{"a": nil, "b": {"a"}},
			dur:      map[string]time.Duration{"a": ms(30)},
			wantPath: "a,b",
			wantLen:  ms(30),
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			g := build(t, tc.deps)
			path, length := CriticalPath(g, tc.dur)

			if got := strings.Join(path, ","); got != tc.wantPath {
				t.Errorf("path = %q, want %q", got, tc.wantPath)
			}
			if length != tc.wantLen {
				t.Errorf("length = %s, want %s", length, tc.wantLen)
			}
		})
	}
}

func TestCriticalPathOnCyclicGraphReturnsNothing(t *testing.T) {
	t.Parallel()

	g := build(t, map[string][]string{"a": {"b"}, "b": {"a"}})
	path, length := CriticalPath(g, map[string]time.Duration{"a": ms(1), "b": ms(1)})
	if path != nil || length != 0 {
		t.Fatalf("CriticalPath = (%v, %s), want (nil, 0) for a cyclic graph", path, length)
	}
}
