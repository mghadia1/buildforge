package graph

import (
	"sort"
	"time"
)

// CriticalPath returns the longest chain of dependent actions by measured
// duration, and its total length.
//
// This is the number that bounds a parallel build. No amount of parallelism can
// finish faster than the longest dependency chain, because every action on it
// must wait for the one before it. When the speedup curve from adding workers
// flattens, this is what it flattens against — so a build tool that reports
// wall time without reporting the critical path cannot tell you whether its
// scheduler is inefficient or its graph is simply narrow.
//
// Actions missing from dur (skipped, or never reached) count as zero.
//
// Ties are broken toward the longer chain, then toward the lexicographically
// smaller name. Zero-duration actions make ties common, and stopping early
// would report a path that ends in the middle of the graph; preferring depth
// follows the chain to its end. The name rule then makes the result
// reproducible, which is what lets two runs be compared at all.
func CriticalPath(g *Graph, dur map[string]time.Duration) ([]string, time.Duration) {
	order, err := g.TopologicalOrder()
	if err != nil {
		return nil, 0 // a cyclic graph has no finish times
	}

	// finish[n] is the earliest wall-clock time n could complete given infinite
	// workers: its own duration, after the latest of its dependencies.
	finish := make(map[string]time.Duration, len(order))
	// depth[n] counts the actions on n's own critical chain, including n.
	depth := make(map[string]int, len(order))
	// pred[n] is the dependency that held n up, for reconstructing the path.
	pred := make(map[string]string, len(order))

	for _, n := range order {
		var latest time.Duration
		var chosen string

		// Dependencies() is sorted, so equal candidates resolve deterministically.
		for _, d := range g.Dependencies(n) {
			if better(finish[d], depth[d], d, latest, depth[chosen], chosen) {
				latest, chosen = finish[d], d
			}
		}

		finish[n] = latest + dur[n]
		depth[n] = depth[chosen] + 1
		if chosen != "" {
			pred[n] = chosen
		}
	}

	// The critical path ends at whichever action finishes last.
	names := append([]string(nil), order...)
	sort.Strings(names)

	var end string
	var length time.Duration
	for _, n := range names {
		if better(finish[n], depth[n], n, length, depth[end], end) {
			length, end = finish[n], n
		}
	}
	if end == "" {
		return nil, 0
	}

	// Walk back along the predecessors, then reverse.
	var path []string
	for n := end; n != ""; n = pred[n] {
		path = append(path, n)
	}
	for i, j := 0, len(path)-1; i < j; i, j = i+1, j-1 {
		path[i], path[j] = path[j], path[i]
	}
	return path, length
}

// better reports whether candidate (f, d, name) beats the incumbent
// (bestF, bestD, bestName): later finish first, then deeper chain, then
// smaller name. An empty incumbent name loses to anything.
func better(f time.Duration, d int, name string, bestF time.Duration, bestD int, bestName string) bool {
	if bestName == "" {
		return true
	}
	if f != bestF {
		return f > bestF
	}
	if d != bestD {
		return d > bestD
	}
	return name < bestName
}
