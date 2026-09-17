// Command buildforge is the BuildForge command-line interface.
//
// Milestone 2 ships three subcommands:
//
//	buildforge validate [-f build.json]            check the manifest, report every problem
//	buildforge graph    [-f build.json]            print the execution order, or name the cycle
//	buildforge build    [-f build.json] [-C dir]   execute the actions in dependency order
//	buildforge serve    [-addr :8080] [-cache dir] serve a cache to other machines
//	buildforge gc       [-cache dir] [-max 2GB]    collect the cache
//
// Builds run in parallel by default; -j bounds how many actions run at once.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/mghadia1/buildforge/internal/cache"
	"github.com/mghadia1/buildforge/internal/graph"
	"github.com/mghadia1/buildforge/internal/manifest"
	"github.com/mghadia1/buildforge/internal/remote"
	"github.com/mghadia1/buildforge/internal/runner"
	"github.com/mghadia1/buildforge/internal/sched"
)

const usage = `buildforge - an incremental build system

usage:
  buildforge validate [-f build.json]                validate a manifest
  buildforge graph    [-f build.json]                print the topological execution order
  buildforge build    [-f build.json] [-C dir] [-j N]  execute the build
  buildforge serve    [-addr :8080] [-cache dir]       serve a cache to other machines
  buildforge gc       [-cache dir] [-max 2GB] [-n]     collect the cache

flags:
  -f          path to the build manifest (default build.json)
  -C          workspace root (default: the manifest's directory)
  -j          actions to run at once (default: one per CPU)
  -k          keep going after a failed action
  -q          suppress per-action progress
  -seq        use the reference sequential driver instead of the scheduler
  -cache dir    cache directory (default <workspace>/.buildforge)
  -no-cache     run every action, reading and writing nothing
  -no-sandbox   let actions read the whole workspace, not just declared inputs
  -remote URL   share a cache with other machines via a buildforge serve instance
  -addr         address for serve (default :8080)
  -max          size budget for gc, e.g. 500MB or 2GB (default: collect garbage only)
  -min-age      protect files younger than this from gc (default 1h)
  -n            dry run for gc
`

func main() {
	// main does nothing but choose an exit code. Everything else returns an
	// error, so every failure path stays visible in a signature rather than
	// hidden in an os.Exit deep in the tree.
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "buildforge:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}

	cmd, rest := args[0], args[1:]

	switch cmd {
	case "-h", "--help", "help":
		fmt.Print(usage)
		return nil
	case "serve":
		return serve(rest)
	case "gc":
		return collect(rest)
	case "validate", "graph", "build":
	default:
		fmt.Fprint(os.Stderr, usage)
		return fmt.Errorf("unknown subcommand %q", cmd)
	}

	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	file := fs.String("f", "build.json", "path to the build manifest")
	workspace := fs.String("C", "", "workspace root (default: the manifest's directory)")
	jobs := fs.Int("j", 0, "actions to run at once (default: one per CPU)")
	keepGoing := fs.Bool("k", false, "keep going after a failed action")
	quiet := fs.Bool("q", false, "suppress per-action progress")
	sequential := fs.Bool("seq", false, "use the reference sequential driver")
	cacheDir := fs.String("cache", "", "cache directory (default <workspace>/.buildforge)")
	noCache := fs.Bool("no-cache", false, "run every action, reading and writing nothing")
	noSandbox := fs.Bool("no-sandbox", false, "let actions read the whole workspace")
	remoteURL := fs.String("remote", "", "share a cache with other machines")
	if err := fs.Parse(rest); err != nil {
		return err
	}

	switch cmd {
	case "validate":
		return validate(*file)
	case "graph":
		return printGraph(*file)
	default:
		return build(buildConfig{
			file:       *file,
			workspace:  *workspace,
			jobs:       *jobs,
			keepGoing:  *keepGoing,
			quiet:      *quiet,
			sequential: *sequential,
			cacheDir:   *cacheDir,
			noCache:    *noCache,
			noSandbox:  *noSandbox,
			remoteURL:  *remoteURL,
		})
	}
}

// load parses the manifest and builds the graph, the first step of every
// subcommand.
func load(file string) (*graph.Graph, int, error) {
	m, err := manifest.Load(file)
	if err != nil {
		return nil, 0, err
	}
	g, err := graph.New(m)
	if err != nil {
		return nil, 0, err
	}
	return g, len(m.Actions), nil
}

func validate(file string) error {
	_, n, err := load(file)
	if err != nil {
		return err
	}
	fmt.Printf("%s: ok, %d actions\n", file, n)
	return nil
}

func printGraph(file string) error {
	g, _, err := load(file)
	if err != nil {
		return err
	}

	order, err := g.TopologicalOrder()
	if err != nil {
		// A cycle is a mistake in the build file, not a crash. Print it the way
		// a compiler would.
		return err
	}

	for i, name := range order {
		deps := g.Dependencies(name)
		if len(deps) == 0 {
			fmt.Printf("%3d. %s\n", i+1, name)
			continue
		}
		fmt.Printf("%3d. %s  (after %s)\n", i+1, name, strings.Join(deps, ", "))
	}
	return nil
}

type buildConfig struct {
	file       string
	workspace  string
	jobs       int
	keepGoing  bool
	quiet      bool
	sequential bool
	cacheDir   string
	noCache    bool
	noSandbox  bool
	remoteURL  string
}

func build(cfg buildConfig) error {
	g, _, err := load(cfg.file)
	if err != nil {
		return err
	}
	workspace := cfg.workspace

	// Declared paths are workspace-relative, so the workspace root has to come
	// from somewhere. The manifest's own directory is the least surprising
	// default: the build file sits at the root of what it builds.
	if workspace == "" {
		workspace = filepath.Dir(cfg.file)
	}

	// Ctrl-C cancels the context, which kills the running command rather than
	// leaving an orphan behind. Milestone 3 reuses the same context to stop
	// in-flight work when a sibling action fails.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var tiered *remote.Tiered

	opts := runner.Options{Workspace: workspace, KeepGoing: cfg.keepGoing}
	if !cfg.quiet {
		opts.Progress = os.Stderr
	}

	// Sandboxed by default. An action that can read an undeclared file produces
	// a cache key that does not cover it, and editing that file then yields a
	// hit that restores stale outputs — a green build with the wrong bytes in
	// it. Opting out is a debugging aid, not a supported way to build.
	if !cfg.noSandbox {
		opts.SandboxRoot = filepath.Join(workspace, ".buildforge", "sandbox")
	}

	if !cfg.noCache {
		dir := cfg.cacheDir
		if dir == "" {
			dir = filepath.Join(workspace, ".buildforge")
		}
		store, err := cache.New(dir)
		if err != nil {
			return err
		}

		// Assigned only on success. A nil *cache.Store placed in the interface
		// would not compare equal to nil, and the cache would be "enabled"
		// right up to the first nil dereference.
		if cfg.remoteURL == "" {
			opts.Cache = store
		} else {
			client, err := remote.NewClient(cfg.remoteURL)
			if err != nil {
				return err
			}
			tiered = remote.NewTiered(store, client)
			opts.Cache = tiered
		}
	}

	var sum *runner.Summary
	var buildErr error

	if cfg.sequential {
		sum, buildErr = runner.Build(ctx, g, opts)
	} else {
		sum, buildErr = sched.Build(ctx, g, sched.Options{Options: opts, Workers: cfg.jobs})
	}

	if sum != nil {
		cached := sum.Cached()
		fmt.Printf("%d ok (%d cached), %d failed, %d skipped in %s\n",
			sum.Succeeded(), cached, len(sum.Failed), len(sum.Skipped),
			sum.Elapsed.Round(time.Millisecond))

		// An action that produces different bytes from the same inputs cannot
		// be cached reliably, and it poisons everything downstream of it. Say
		// so loudly rather than letting the cache quietly underperform.
		if nd := sum.Nondeterministic(); len(nd) > 0 {
			fmt.Fprintf(os.Stderr,
				"warning: nondeterministic output from an identical cache key: %s\n",
				strings.Join(nd, ", "))
		}

		// The critical path is what the wall time is really competing against.
		// Reporting one without the other cannot distinguish a slow scheduler
		// from a graph that is simply too narrow to parallelize.
		if path, length := graph.CriticalPath(g, sched.Durations(sum)); len(path) > 0 {
			fmt.Printf("critical path %s: %s\n",
				length.Round(time.Millisecond), strings.Join(path, " -> "))
		}

		if tiered != nil {
			st := tiered.Stats()
			fmt.Printf("remote: %d hits, %d local hits, %d misses, %d uploads, %d errors\n",
				st.RemoteHits, st.LocalHits, st.Misses, st.Uploads, st.Errors)
		}
	}

	if buildErr != nil {
		// Surface the failing action's stderr. It is the part a person needs,
		// and burying it behind a one-line error is the most common way build
		// tools waste their users' time.
		var ae *runner.ActionError
		if errors.As(buildErr, &ae) && len(ae.Stderr) > 0 {
			fmt.Fprintf(os.Stderr, "\n--- %s stderr ---\n%s", ae.Action, ae.Stderr)
			if !strings.HasSuffix(string(ae.Stderr), "\n") {
				fmt.Fprintln(os.Stderr)
			}
		}
		return buildErr
	}
	return nil
}

// serve runs a cache server for other machines.
func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", ":8080", "address to listen on")
	dir := fs.String("cache", ".buildforge-server", "cache directory to serve")
	if err := fs.Parse(args); err != nil {
		return err
	}

	store, err := cache.New(*dir)
	if err != nil {
		return err
	}

	srv := remote.NewServer(store)
	srv.Logger = log.New(os.Stderr, "buildforge serve: ", log.LstdFlags)

	fmt.Printf("serving %s on %s\n", store.Root(), *addr)
	fmt.Println("no authentication and no TLS: this is a cache for a trusted network")

	// Timeouts, because a server with none can be held open indefinitely by a
	// client that opens a connection and then says nothing.
	httpServer := &http.Server{
		Addr:              *addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	return httpServer.ListenAndServe()
}

// collect runs garbage collection over a cache directory.
func collect(args []string) error {
	fs := flag.NewFlagSet("gc", flag.ExitOnError)
	dir := fs.String("cache", ".buildforge", "cache directory to collect")
	max := fs.String("max", "", "size budget, e.g. 500MB or 2GB (default: collect garbage only)")
	minAge := fs.Duration("min-age", time.Hour, "protect files younger than this")
	dryRun := fs.Bool("n", false, "report what would be collected without deleting")
	if err := fs.Parse(args); err != nil {
		return err
	}

	budget, err := parseSize(*max)
	if err != nil {
		return err
	}

	store, err := cache.New(*dir)
	if err != nil {
		return err
	}

	res, err := store.GC(cache.GCOptions{
		MaxBytes: budget,
		MinAge:   *minAge,
		DryRun:   *dryRun,
	})
	if err != nil {
		return err
	}

	if *dryRun {
		fmt.Println("dry run: nothing was deleted")
	}
	fmt.Printf("scanned   %d entries, %d blobs, %s\n", res.Entries, res.Blobs, humanSize(res.BytesBefore))
	fmt.Printf("garbage   %d unreferenced blobs\n", res.Unreferenced)
	if budget > 0 {
		fmt.Printf("evicted   %d entries, %d blobs (budget %s)\n",
			res.EvictedEntry, res.Evicted, humanSize(budget))
	}
	if res.Skipped > 0 {
		fmt.Printf("skipped   %d files younger than %s\n", res.Skipped, *minAge)
	}
	fmt.Printf("freed     %s, now %s\n", humanSize(res.BytesFreed), humanSize(res.BytesAfter))
	return nil
}

// parseSize accepts a plain byte count or a suffixed size such as 500MB.
func parseSize(s string) (int64, error) {
	s = strings.TrimSpace(strings.ToUpper(s))
	if s == "" {
		return 0, nil
	}

	mult := int64(1)
	for _, suffix := range []struct {
		text string
		n    int64
	}{
		{"GB", 1 << 30}, {"MB", 1 << 20}, {"KB", 1 << 10},
		{"G", 1 << 30}, {"M", 1 << 20}, {"K", 1 << 10}, {"B", 1},
	} {
		if strings.HasSuffix(s, suffix.text) {
			mult = suffix.n
			s = strings.TrimSpace(strings.TrimSuffix(s, suffix.text))
			break
		}
	}

	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid size %q: want a number, optionally suffixed with KB, MB or GB", s)
	}
	if n < 0 {
		return 0, fmt.Errorf("invalid size %q: must not be negative", s)
	}
	return n * mult, nil
}

func humanSize(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.2f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.2f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.2f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
