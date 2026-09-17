//go:build unix

package runner

import (
	"os/exec"
	"syscall"
	"time"
)

// setupProcessGroup makes an action killable as a unit.
//
// This exists because of a bug the cancellation test caught. Killing the
// command's own process is not enough: `sh -c "sleep 30; ..."` forks a child
// for sleep, so killing the shell leaves sleep running — and the orphan
// inherited the stdout pipe, so cmd.Wait blocked until it finished on its own.
// A cancelled build took the full thirty seconds to stop.
//
// Two mechanisms, deliberately both:
//
//   - Setpgid puts the action in its own process group, and Cancel signals the
//     whole group (the negative PID), so children die with their parent.
//   - WaitDelay is the backstop. If something still holds the output pipe open
//     after the kill, os/exec closes the pipes and returns instead of blocking
//     forever. A build tool that hangs on Ctrl-C is worse than one that fails.
func setupProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		// A negative PID addresses the process group.
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}

	cmd.WaitDelay = 2 * time.Second
}
