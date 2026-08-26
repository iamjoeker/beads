package doltserver

import (
	"os"
	"path/filepath"
)

// testRootLockName is the claim marker a test suite's temp root carries for
// as long as the process that created it is alive. See ClaimTestRoot.
const testRootLockName = ".beads-test-root.lock"

// abandonedTestRoots globs pattern and returns the directories isAbandoned
// accepts.
//
// Only real directories are considered: a glob match is Lstat'ed, so a
// symlink is skipped rather than followed. SweepAbandonedTestRoots deletes
// what this returns, and following a symlink would let anything that can
// write next to the temp roots redirect that deletion elsewhere.
//
// A glob error yields no roots at all. Failing to enumerate is not evidence
// that anything is abandoned.
func abandonedTestRoots(pattern string, isAbandoned func(string) bool) []string {
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return nil
	}
	var roots []string
	for _, match := range matches {
		info, err := os.Lstat(match)
		if err != nil || !info.IsDir() {
			continue
		}
		if !isAbandoned(match) {
			continue
		}
		roots = append(roots, match)
	}
	return roots
}

// removeTestRootTree deletes an abandoned temp root, making read-only
// directories writable first. Test roots double as isolated HOMEs and can
// contain read-only Go module cache entries, which os.RemoveAll alone refuses
// to descend into.
func removeTestRootTree(root string) error {
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() && info.Mode()&0o200 == 0 {
			_ = os.Chmod(path, info.Mode()|0o200)
		}
		return nil
	})
	return os.RemoveAll(root)
}
