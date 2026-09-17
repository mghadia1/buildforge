package remote

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mghadia1/buildforge/internal/cache"
	"github.com/mghadia1/buildforge/internal/digest"
	"github.com/mghadia1/buildforge/internal/graph"
	"github.com/mghadia1/buildforge/internal/manifest"
	"github.com/mghadia1/buildforge/internal/runner"
)

// newServer starts a cache server backed by a fresh store.
func newServer(t *testing.T) (*httptest.Server, *cache.Store) {
	t.Helper()

	store, err := cache.New(filepath.Join(t.TempDir(), "server-cache"))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(NewServer(store).Handler())
	t.Cleanup(srv.Close)
	return srv, store
}

func newClient(t *testing.T, url string) *Client {
	t.Helper()
	c, err := NewClient(url)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// machine is one workspace with its own local cache, sharing a remote.
type machine struct {
	ws     string
	tiered *Tiered
}

func newMachine(t *testing.T, client *Client) *machine {
	t.Helper()

	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "src/a.txt"), []byte("alpha"), 0o644); err != nil {
		t.Fatal(err)
	}

	local, err := cache.New(filepath.Join(t.TempDir(), "local-cache"))
	if err != nil {
		t.Fatal(err)
	}
	return &machine{ws: ws, tiered: NewTiered(local, client)}
}

func actions() []manifest.Action {
	return []manifest.Action{
		{
			Name:    "stage",
			Inputs:  []string{"src/a.txt"},
			Outputs: []string{"out/a.staged"},
			Command: []string{"/bin/sh", "-c", "cat src/a.txt > out/a.staged"},
		},
		{
			Name:    "finish",
			Deps:    []string{"stage"},
			Inputs:  []string{"out/a.staged"},
			Outputs: []string{"out/a.final"},
			Command: []string{"/bin/sh", "-c", "cat out/a.staged > out/a.final"},
		},
	}
}

func (m *machine) build(t *testing.T) *runner.Summary {
	t.Helper()

	mf := &manifest.Manifest{Actions: actions()}
	if err := mf.Validate(); err != nil {
		t.Fatal(err)
	}
	g, err := graph.New(mf)
	if err != nil {
		t.Fatal(err)
	}

	sum, err := runner.Build(context.Background(), g, runner.Options{
		Workspace: m.ws,
		Cache:     m.tiered,
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return sum
}

// TestSecondMachineHitsTheFirstMachinesCache is the Milestone 6 gate.
func TestSecondMachineHitsTheFirstMachinesCache(t *testing.T) {
	t.Parallel()

	srv, _ := newServer(t)
	client := newClient(t, srv.URL)

	// Machine A builds cold and uploads.
	a := newMachine(t, client)
	if got := a.build(t).Cached(); got != 0 {
		t.Fatalf("machine A cached %d actions on a cold build, want 0", got)
	}
	if got := a.tiered.Stats().Uploads; got != 2 {
		t.Fatalf("machine A uploaded %d entries, want 2", got)
	}

	// Machine B has a different workspace path and an empty local cache. It can
	// only hit if the key contains no absolute paths — which is the design
	// decision from Milestone 4 that this whole milestone rests on.
	b := newMachine(t, client)
	if a.ws == b.ws {
		t.Fatal("both machines share a workspace; the test proves nothing")
	}

	sum := b.build(t)
	if got, want := sum.Cached(), 2; got != want {
		t.Fatalf("machine B cached %d actions, want %d", got, want)
	}
	if got := b.tiered.Stats().RemoteHits; got != 2 {
		t.Fatalf("machine B had %d remote hits, want 2", got)
	}
	if got, want := readFile(t, b.ws, "out/a.final"), "alpha"; got != want {
		t.Fatalf("machine B out/a.final = %q, want %q", got, want)
	}
}

func TestRemoteHitPopulatesTheLocalCache(t *testing.T) {
	t.Parallel()

	// The second build on a machine must not pay for the network again.
	srv, _ := newServer(t)
	client := newClient(t, srv.URL)

	newMachine(t, client).build(t)

	b := newMachine(t, client)
	b.build(t)
	if got := b.tiered.Stats().RemoteHits; got != 2 {
		t.Fatalf("first build had %d remote hits, want 2", got)
	}

	if err := os.RemoveAll(filepath.Join(b.ws, "out")); err != nil {
		t.Fatal(err)
	}
	before := b.tiered.Stats()
	b.build(t)
	after := b.tiered.Stats()

	if after.RemoteHits != before.RemoteHits {
		t.Errorf("the second build went to the network again: %d -> %d remote hits",
			before.RemoteHits, after.RemoteHits)
	}
	if after.LocalHits-before.LocalHits != 2 {
		t.Errorf("local hits went up by %d, want 2", after.LocalHits-before.LocalHits)
	}
}

func TestUnreachableServerDoesNotFailTheBuild(t *testing.T) {
	t.Parallel()

	// The rule: a shared cache is an optimization, and an optimization that can
	// break the build is not one.
	srv, _ := newServer(t)
	client := newClient(t, srv.URL)
	srv.Close() // every request from here on fails

	m := newMachine(t, client)
	sum := m.build(t)

	if got, want := readFile(t, m.ws, "out/a.final"), "alpha"; got != want {
		t.Fatalf("out/a.final = %q, want %q", got, want)
	}
	if sum.Cached() != 0 {
		t.Errorf("Cached = %d, want 0 with the server down", sum.Cached())
	}
	if m.tiered.Stats().Errors == 0 {
		t.Error("Errors = 0; the failures were swallowed without being counted")
	}
}

func TestServerRejectsBlobUnderTheWrongDigest(t *testing.T) {
	t.Parallel()

	// Everything downstream trusts that a blob's name describes its contents.
	srv, _ := newServer(t)
	client := newClient(t, srv.URL)

	wrong := digest.Bytes([]byte("something else"))
	if err := client.PutBlob(wrong, []byte("actual content")); err == nil {
		t.Fatal("server accepted bytes under a digest they do not hash to")
	}
}

func TestClientVerifiesWhatTheServerSends(t *testing.T) {
	t.Parallel()

	// A hostile or damaged server must not be able to install arbitrary bytes
	// as the output of a build action.
	want := digest.Bytes([]byte("honest"))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("tampered"))
	}))
	t.Cleanup(srv.Close)

	client := newClient(t, srv.URL)
	if _, err := client.GetBlob(want); err == nil {
		t.Fatal("client accepted a blob that does not match the digest it asked for")
	} else if !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("error = %v, want a digest mismatch", err)
	}
}

func TestServerRejectsUnsafeNames(t *testing.T) {
	t.Parallel()

	srv, _ := newServer(t)

	for _, path := range []string{
		"/cas/../../etc/passwd",
		"/ac/../../etc/passwd",
		"/cas/not-hex",
		"/ac/AB12",
	} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			t.Errorf("GET %s returned 200, want a rejection", path)
		}
	}
}

func TestEntryKeyMustMatchTheRequestPath(t *testing.T) {
	t.Parallel()

	// Otherwise a client could overwrite an unrelated action's record.
	srv, _ := newServer(t)

	entry := &cache.Entry{Key: string(digest.Bytes([]byte("real-key")))}
	body, err := json.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}

	req, err := http.NewRequest(http.MethodPut,
		srv.URL+"/ac/"+string(digest.Bytes([]byte("other-key"))), bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %s, want 400", resp.Status)
	}
}

func TestNewClientRejectsNonHTTPURLs(t *testing.T) {
	t.Parallel()

	for _, u := range []string{"file:///etc", "ftp://host/x", "://bad"} {
		if _, err := NewClient(u); err == nil {
			t.Errorf("NewClient(%q) succeeded, want an error", u)
		}
	}
}

func readFile(t *testing.T, dir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("ReadFile %s: %v", name, err)
	}
	return string(b)
}
