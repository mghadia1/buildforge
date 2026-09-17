package manifest

import (
	"strings"
	"testing"
)

// Table-driven tests are the Go idiom: one slice of cases, one loop, one
// subtest each, so a failure names the case that broke.
func TestParseRejects(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		json string
		want string // substring the error must contain
	}{
		{
			name: "no actions",
			json: `{"actions": []}`,
			want: "no actions",
		},
		{
			name: "duplicate name",
			json: `{"actions":[
				{"name":"a","outputs":["o1"],"command":["true"]},
				{"name":"a","outputs":["o2"],"command":["true"]}]}`,
			want: "declared more than once",
		},
		{
			name: "empty command",
			json: `{"actions":[{"name":"a","outputs":["o"],"command":[]}]}`,
			want: "command must not be empty",
		},
		{
			name: "no outputs",
			json: `{"actions":[{"name":"a","outputs":[],"command":["true"]}]}`,
			want: "at least one output",
		},
		{
			name: "unknown dep",
			json: `{"actions":[{"name":"a","deps":["ghost"],"outputs":["o"],"command":["true"]}]}`,
			want: `unknown action "ghost"`,
		},
		{
			name: "self dep",
			json: `{"actions":[{"name":"a","deps":["a"],"outputs":["o"],"command":["true"]}]}`,
			want: "depends on itself",
		},
		{
			name: "absolute input path",
			json: `{"actions":[{"name":"a","inputs":["/etc/passwd"],"outputs":["o"],"command":["true"]}]}`,
			want: "must be workspace-relative",
		},
		{
			name: "escaping input path",
			json: `{"actions":[{"name":"a","inputs":["../secret"],"outputs":["o"],"command":["true"]}]}`,
			want: "escapes the workspace",
		},
		{
			name: "non-canonical path",
			json: `{"actions":[{"name":"a","inputs":["src/./x.c"],"outputs":["o"],"command":["true"]}]}`,
			want: "canonical form",
		},
		{
			// "output" instead of "outputs" would otherwise parse into an
			// action that declares no outputs at all.
			name: "unknown field",
			json: `{"actions":[{"name":"a","output":["o"],"command":["true"]}]}`,
			want: "unknown field",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := Parse(strings.NewReader(tc.json))
			if err == nil {
				t.Fatalf("Parse succeeded, want error containing %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestParseReportsEveryProblemAtOnce(t *testing.T) {
	t.Parallel()

	// Two independent problems in one action. Stopping at the first would make
	// fixing a broken manifest an exercise in repeated guessing.
	const src = `{"actions":[{"name":"a","outputs":[],"command":[]}]}`

	_, err := Parse(strings.NewReader(src))
	if err == nil {
		t.Fatal("Parse succeeded, want error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "command must not be empty") || !strings.Contains(msg, "at least one output") {
		t.Fatalf("error = %q, want both the command and the output problem", msg)
	}
}

func TestParseAcceptsValidManifest(t *testing.T) {
	t.Parallel()

	const src = `{"actions":[
		{"name":"compile","inputs":["src/main.c"],"outputs":["out/main.o"],"command":["cc","-c","src/main.c"]},
		{"name":"link","deps":["compile"],"inputs":["out/main.o"],"outputs":["out/app"],"command":["cc","-o","out/app","out/main.o"],"env":{"SOURCE_DATE_EPOCH":"0"}}]}`

	m, err := Parse(strings.NewReader(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got, want := len(m.Actions), 2; got != want {
		t.Fatalf("len(Actions) = %d, want %d", got, want)
	}
	if got, want := strings.Join(m.Names(), ","), "compile,link"; got != want {
		t.Fatalf("Names() = %q, want %q (sorted)", got, want)
	}
}
