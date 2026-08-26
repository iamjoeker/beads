//go:build linux || darwin

package doltserver

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// terminateDoltServerPIDs sends SIGTERM to each pid, waits a short grace
// period, then SIGKILLs whatever is still alive. It is the shared tail of
// both sweep entry points (SweepOrphanedTestServers on each platform, and
// SweepAbandonedTestRoots); all of the safety reasoning about *which* PIDs
// may be signaled lives in the selection functions that produce the list.
//
// Every PID is revalidated with isDoltServerProcess immediately before each
// signal. Candidate selection did its own process read some time earlier, and
// in a PID-reuse window the kernel could have recycled that number to an
// unrelated process in between — including during the 300ms grace period,
// where the original server may well have exited cleanly in response to the
// SIGTERM we just sent it.
//
// Returns the PIDs it sent a kill signal to.
func terminateDoltServerPIDs(pids []int) []int {
	self := os.Getpid()
	var killed []int
	for _, pid := range pids {
		if pid == self {
			continue
		}
		if !isDoltServerProcess(pid) {
			continue
		}
		if err := syscall.Kill(pid, syscall.SIGTERM); err == nil {
			killed = append(killed, pid)
		}
	}

	if len(killed) == 0 {
		return killed
	}

	fmt.Fprintf(os.Stderr, "Info: swept %d orphaned test dolt sql-server process(es): %v\n", len(killed), killed)

	time.Sleep(300 * time.Millisecond)
	for _, pid := range killed {
		if !isDoltServerProcess(pid) {
			continue
		}
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}

	return killed
}

// canonicalRoots resolves symlinks in each root so the paths are comparable
// with the working directories reported by the OS. macOS commonly reports a
// cwd below /private/var where os.MkdirTemp returned the equivalent /var
// path; a Linux box whose /tmp is a symlink has the same mismatch, since
// /proc/<pid>/cwd is always fully resolved.
//
// A root that cannot be resolved is passed through unchanged rather than
// dropped: failing to resolve must not silently widen or narrow the set the
// caller vouched for.
func canonicalRoots(roots []string) []string {
	canonical := make([]string, 0, len(roots))
	for _, root := range roots {
		if root == "" {
			continue
		}
		resolved, err := filepath.EvalSymlinks(root)
		if err != nil {
			canonical = append(canonical, root)
			continue
		}
		canonical = append(canonical, resolved)
	}
	return canonical
}
