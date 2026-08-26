//go:build linux || darwin

package doltserver

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// ClaimTestRoot marks root as in use by the calling process, for as long as
// that process lives. Call it immediately after creating a suite temp root,
// and hold the returned release func for the lifetime of the process —
// releasing early only makes the root look abandoned to a concurrent sweeper.
//
// The claim is an advisory flock(2) held through an open file descriptor on
// <root>/.beads-test-root.lock. The kernel drops a flock when the file
// description closes, which includes every way a process can die without
// running any cleanup code: SIGKILL, a panic, the OOM killer, a `go test`
// timeout, ^C on a CI runner. That is precisely why the claim is a flock and
// not a recorded PID. The situation this exists to detect is the one where no
// deferred cleanup ran at all, so any marker the dying process had to update
// itself is exactly the marker that will be wrong; and a PID, unlike a file
// description, can be recycled by the kernel to an unrelated live process.
func ClaimTestRoot(root string) (release func(), err error) {
	f, err := claimTestRootFile(root)
	if err != nil {
		return func() {}, err
	}
	return func() { _ = f.Close() }, nil
}

// claimTestRootFile is ClaimTestRoot's body, returning the file that holds
// the claim rather than a closure over it. The claim lives in the open file
// description, so a test can hand this file to a child process and prove the
// property the design rests on: the lock outlives the process that took it
// for exactly as long as some descriptor for it survives, and no longer.
func claimTestRootFile(root string) (*os.File, error) {
	// #nosec G304 -- root is the caller's own temp directory, and naming it is
	// the entire API; the file opened inside it is a fixed constant.
	f, err := os.OpenFile(filepath.Join(root, testRootLockName), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("claim test root %s: %w", root, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("claim test root %s: %w", root, err)
	}
	return f, nil
}

// SweepAbandonedTestRoots reaps the debris of previous, interrupted test runs
// whose temp roots match pattern: it SIGTERM/SIGKILLs any dolt sql-server
// still running out of one, then deletes the directory tree.
//
// Call it at suite STARTUP, before creating this run's own root. The exit-path
// backstop (SweepOrphanedTestServers) only ever runs on the normal return
// path, so a run that is SIGKILLed — the exact case that leaks — reaps
// nothing, and its server survives holding whatever port it bound. On a shared
// machine that server does not merely make later shared-server tests skip: it
// still carries a database, so it ANSWERS them, and they pass against a
// stranger's data. Nothing surfaces that, which is why the corpse has to be
// cleared before the next run starts rather than after it finishes (bd-sxh,
// found while auditing bd-zje; bd-l9c fixed the leak at its source but could
// not clear the process already running).
//
// The safety question is the whole design. scripts/test.sh runs packages in
// parallel and a developer may have several runs going at once, so pattern
// necessarily matches roots belonging to LIVE sibling runs as well as dead
// ones. Two independent gates separate them:
//
//   - A directory with no .beads-test-root.lock is never touched. Only
//     ClaimTestRoot creates that file, so anything else that happens to match
//     pattern — including a root left by a binary built before this existed —
//     is left alone. This fails closed: the cost is an unreaped corpse, not a
//     deleted live root.
//   - A directory whose lock is still flock-held is never touched. The holder
//     is alive by construction (see ClaimTestRoot), so the root is in use.
//
// Only when the lock exists AND is acquirable is a root treated as abandoned,
// and only then are servers under it reaped — via selectServersUnderRoots,
// which has no deleted-cwd branch, so nothing outside the proven-dead roots
// is ever signaled.
//
// Returns the PIDs signaled and the root directories removed. Best-effort
// throughout: a root that cannot be read or removed is skipped silently.
func SweepAbandonedTestRoots(pattern string) (killed []int, removed []string) {
	abandoned := abandonedTestRoots(pattern, testRootIsAbandoned)
	if len(abandoned) == 0 {
		return nil, nil
	}

	// Kill before deleting, so a server is never left running against a
	// directory that has been removed out from under it — the very state
	// this function exists to clean up.
	killed = terminateDoltServerPIDs(
		selectServersUnderRoots(gatherDoltServerCandidates(), canonicalRoots(abandoned)),
	)

	for _, root := range abandoned {
		if err := removeTestRootTree(root); err == nil {
			removed = append(removed, root)
		}
	}
	if len(removed) > 0 {
		fmt.Fprintf(os.Stderr,
			"Info: removed %d abandoned test temp root(s) left by previous runs: %v\n",
			len(removed), removed)
	}
	return killed, removed
}

// testRootIsAbandoned reports whether dir is a claimed test root whose owning
// process is gone: the claim marker exists and its flock is acquirable.
//
// Every failure path returns false. "I could not tell" must never read as
// "nothing is using it", because the caller's response to true is deletion.
func testRootIsAbandoned(dir string) bool {
	lock := filepath.Join(dir, testRootLockName)
	// #nosec G304 -- dir came from the caller's own glob over its temp roots,
	// and the file opened inside it is a fixed constant. Nothing is read from
	// it; it is opened only to take a lock.
	f, err := os.OpenFile(lock, os.O_RDWR, 0o600)
	if err != nil {
		// Missing marker (a root not created by ClaimTestRoot, or created
		// before it existed) or unreadable. Not provably abandoned.
		return false
	}
	defer func() { _ = f.Close() }()

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return false // a live process still holds the claim
	}
	// Drop the probe lock immediately. Holding it buys nothing: os.MkdirTemp
	// never reuses a name, so no new owner can appear for this root, and two
	// concurrent sweepers both deleting it is harmless (one gets ENOENT).
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return true
}
