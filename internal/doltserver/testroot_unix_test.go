//go:build linux || darwin

package doltserver

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestTestRootIsAbandoned covers the gate that decides whether
// SweepAbandonedTestRoots may delete a directory. Everything here is about
// the safe direction of each failure: only a claimed root whose claim has
// been released reads as abandoned.
func TestTestRootIsAbandoned(t *testing.T) {
	t.Run("unclaimed root is never abandoned", func(t *testing.T) {
		// A root with no marker is either not ours or predates ClaimTestRoot.
		// Either way nothing proves it idle, so it must be left alone.
		root := t.TempDir()
		if testRootIsAbandoned(root) {
			t.Fatal("root with no claim marker reported abandoned; it must fail closed")
		}
	})

	t.Run("claimed root is not abandoned while the claim is held", func(t *testing.T) {
		root := t.TempDir()
		release, err := ClaimTestRoot(root)
		if err != nil {
			t.Fatalf("ClaimTestRoot: %v", err)
		}
		defer release()

		// testRootIsAbandoned opens the lock file fresh, so this probes the
		// claim through a separate open file description — the same way a
		// sweeper in another process would.
		if testRootIsAbandoned(root) {
			t.Fatal("held claim reported abandoned; a concurrent run's root would be deleted")
		}
	})

	t.Run("released claim reads as abandoned", func(t *testing.T) {
		root := t.TempDir()
		release, err := ClaimTestRoot(root)
		if err != nil {
			t.Fatalf("ClaimTestRoot: %v", err)
		}
		release()

		if !testRootIsAbandoned(root) {
			t.Fatal("released claim not reported abandoned; corpses would never be reaped")
		}
	})

	t.Run("a second claim on a held root is refused", func(t *testing.T) {
		root := t.TempDir()
		release, err := ClaimTestRoot(root)
		if err != nil {
			t.Fatalf("ClaimTestRoot: %v", err)
		}
		defer release()

		if _, err := ClaimTestRoot(root); err == nil {
			t.Fatal("second ClaimTestRoot on a held root succeeded")
		}
	})
}

// TestTestRootClaimReleasedOnSIGKILL is the load-bearing test for this whole
// mechanism. The claim exists to be correct when NO cleanup code runs, so the
// only meaningful proof is a holder that is killed outright.
//
// A child process inherits the claim's file descriptor, so the claim is held
// by the child's open file description after the parent closes its own copy —
// the same shape as the leaked dolt sql-server this fixes, which outlived the
// test run that started it. SIGKILL gives the child no chance to release
// anything; the kernel does it, which is the entire reason the marker is a
// flock and not a recorded PID or a timestamp.
func TestTestRootClaimReleasedOnSIGKILL(t *testing.T) {
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Skipf("sleep not available: %v", err)
	}

	root := t.TempDir()
	lockFile, err := claimTestRootFile(root)
	if err != nil {
		t.Fatalf("claimTestRootFile: %v", err)
	}

	cmd := exec.Command(sleep, "300")
	cmd.ExtraFiles = []*os.File{lockFile} // child inherits the claim
	if err := cmd.Start(); err != nil {
		_ = lockFile.Close()
		t.Fatalf("start holder process: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()

	// Drop this process's copy. The claim must now rest solely on the child.
	if err := lockFile.Close(); err != nil {
		t.Fatalf("close local copy: %v", err)
	}

	if testRootIsAbandoned(root) {
		t.Fatal("root reported abandoned while a live child still holds the claim")
	}

	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("kill holder: %v", err)
	}
	if _, err := cmd.Process.Wait(); err != nil {
		t.Fatalf("reap holder: %v", err)
	}

	if !testRootIsAbandoned(root) {
		t.Fatal("root still reported in use after its SIGKILLed holder died; " +
			"an interrupted run's corpse would never be reaped")
	}
}

// TestSweepAbandonedTestRootsSkipsLiveRoots exercises the whole entry point
// against a live root and a dead one side by side — the situation on any
// machine running more than one suite at a time. Neither directory holds a
// dolt server, so this checks the deletion half only; which processes may be
// signaled is pinned by TestSelectServersUnderRoots.
func TestSweepAbandonedTestRootsSkipsLiveRoots(t *testing.T) {
	base := t.TempDir()

	mkRoot := func(name string) string {
		dir := filepath.Join(base, name)
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		return dir
	}

	dead := mkRoot("root-dead")
	live := mkRoot("root-live")
	unclaimed := mkRoot("root-unclaimed")

	deadRelease, err := ClaimTestRoot(dead)
	if err != nil {
		t.Fatalf("claim dead root: %v", err)
	}
	deadRelease() // simulates the owning process having exited

	liveRelease, err := ClaimTestRoot(live)
	if err != nil {
		t.Fatalf("claim live root: %v", err)
	}
	defer liveRelease()

	_, removed := SweepAbandonedTestRoots(filepath.Join(base, "root-*"))

	if len(removed) != 1 || removed[0] != dead {
		t.Errorf("removed = %v, want exactly [%s]", removed, dead)
	}
	if _, err := os.Stat(dead); !os.IsNotExist(err) {
		t.Errorf("abandoned root still present: stat err = %v", err)
	}
	if _, err := os.Stat(live); err != nil {
		t.Errorf("live root was removed out from under its owner: %v", err)
	}
	if _, err := os.Stat(unclaimed); err != nil {
		t.Errorf("unclaimed root was removed; the sweep must fail closed on unmarked dirs: %v", err)
	}
}

// TestRemoveTestRootTreeHandlesReadOnlyDirs: test roots double as isolated
// HOMEs and can contain read-only Go module cache entries, which os.RemoveAll
// alone will not descend into.
func TestRemoveTestRootTreeHandlesReadOnlyDirs(t *testing.T) {
	root := filepath.Join(t.TempDir(), "tree")
	nested := filepath.Join(root, "pkg", "mod", "readonly")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "file"), []byte("x"), 0o400); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(nested, 0o500); err != nil {
		t.Fatal(err)
	}

	if err := removeTestRootTree(root); err != nil {
		t.Fatalf("removeTestRootTree: %v", err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Errorf("tree still present: stat err = %v", err)
	}
}

// TestCanonicalRoots: unresolvable roots pass through unchanged rather than
// being dropped, so a resolution failure cannot silently change the set of
// directories the caller vouched for.
func TestCanonicalRoots(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	missing := filepath.Join(t.TempDir(), "does-not-exist")
	got := canonicalRoots([]string{link, "", missing})

	resolved, err := filepath.EvalSymlinks(real)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{resolved, missing}
	if len(got) != len(want) {
		t.Fatalf("canonicalRoots() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("canonicalRoots() = %v, want %v", got, want)
		}
	}
}

// TestClaimTestRootOnMissingDirFails: a claim that cannot be recorded must
// report an error rather than pretend success, or the caller would believe a
// future sweeper can see its root when no marker exists.
func TestClaimTestRootOnMissingDirFails(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope")
	release, err := ClaimTestRoot(missing)
	if err == nil {
		release()
		t.Fatal("ClaimTestRoot on a nonexistent directory reported success")
	}
	// The returned release must still be safe to call.
	release()
}
