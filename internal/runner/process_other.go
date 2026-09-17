//go:build !unix

package runner

import (
	"os/exec"
	"time"
)

// setupProcessGroup falls back to os/exec's default cancellation on platforms
// without POSIX process groups. WaitDelay still prevents an indefinite hang,
// but an action that spawns children can leave orphans behind. BuildForge is
// developed and tested on macOS and Linux; this exists so the package builds
// elsewhere, not because that path is supported.
func setupProcessGroup(cmd *exec.Cmd) {
	cmd.WaitDelay = 2 * time.Second
}
