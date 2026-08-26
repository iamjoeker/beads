//go:build !linux && !darwin

package doltserver

// ClaimTestRoot is a no-op on platforms without the flock(2)-based claim
// used by SweepAbandonedTestRoots. The stub keeps callers (test TestMains)
// portable; it reports success so a suite still runs, it simply leaves no
// marker for a sweeper to read.
func ClaimTestRoot(_ string) (release func(), err error) {
	return func() {}, nil
}

// SweepAbandonedTestRoots is a no-op on platforms where this package cannot
// inspect process command lines and working directories (see sweep_other.go)
// and has no claim marker to distinguish an abandoned root from a live one.
// It reaps nothing rather than guessing.
func SweepAbandonedTestRoots(_ string) (killed []int, removed []string) {
	return nil, nil
}
