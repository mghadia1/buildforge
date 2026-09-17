package runner

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/mghadia1/buildforge/internal/manifest"
)

// sandbox is a directory containing symlinks to exactly an action's declared
// inputs, and nothing else.
//
// This is what makes a declared dependency mean something. Without it, an
// action can read any file in the workspace, including ones it never declared —
// and an undeclared input is not in the cache key, so editing it produces a
// cache hit that restores stale outputs. A silently wrong build.
//
// Inside the sandbox an undeclared read simply fails: the file is not there.
// The build breaks loudly at the moment the manifest becomes wrong, instead of
// lying at some later point when someone edits the file nobody declared.
//
// Symlinks rather than copies, because copying every input of every action
// would dominate the build. They are hermetic for the case that matters:
// a compiler resolves #include "x.h" against the path it was given, which is
// the path inside the sandbox, so an undeclared header is not found even
// though the declared source file it sits beside is a link into the real tree.
type sandbox struct {
	dir string
}

// newSandbox builds the symlink farm for one action.
func newSandbox(root, workspace string, a manifest.Action) (*sandbox, error) {
	absWorkspace, err := filepath.Abs(workspace)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}

	dir, err := os.MkdirTemp(root, "act-")
	if err != nil {
		return nil, err
	}
	s := &sandbox{dir: dir}

	for _, in := range a.Inputs {
		link := filepath.Join(dir, in)
		if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
			s.remove()
			return nil, err
		}
		// An absolute target, because the link's own directory depth inside the
		// sandbox has nothing to do with the workspace's layout.
		if err := os.Symlink(filepath.Join(absWorkspace, in), link); err != nil {
			s.remove()
			return nil, fmt.Errorf("linking input %q: %w", in, err)
		}
	}

	// Output directories are created but left empty: the action writes here,
	// and anything it writes that it did not declare stays behind when the
	// sandbox is removed.
	for _, out := range a.Outputs {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, out)), 0o755); err != nil {
			s.remove()
			return nil, err
		}
	}
	return s, nil
}

// harvest moves the action's declared outputs from the sandbox into the
// workspace.
//
// Only declared outputs are moved. A file the action wrote without declaring it
// is left in the sandbox and deleted with it, so the next action cannot
// accidentally depend on something that was never part of the contract.
func (s *sandbox) harvest(workspace string, outputs []string) error {
	for _, out := range outputs {
		src := filepath.Join(s.dir, out)
		if _, err := os.Stat(src); err != nil {
			return fmt.Errorf("declared output %q was not produced: %w", out, err)
		}

		dst := filepath.Join(workspace, out)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		os.Remove(dst)

		// Same filesystem in the normal case, so this is a rename. The copy is
		// the fallback for a sandbox root on a different device.
		if err := os.Rename(src, dst); err == nil {
			continue
		}
		if err := copyFile(dst, src); err != nil {
			return fmt.Errorf("harvesting output %q: %w", out, err)
		}
	}
	return nil
}

func (s *sandbox) remove() {
	if s != nil && s.dir != "" {
		os.RemoveAll(s.dir)
	}
}

func copyFile(dst, src string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	info, err := in.Stat()
	if err != nil {
		return err
	}

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Close()
}
