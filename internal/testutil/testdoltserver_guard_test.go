//go:build !windows

package testutil

import (
	"testing"
)

// TestDoltPortEnvVarsAreTheGuardedSet names the port variables literally, so
// that dropping one — from this helper's publish set or from the guard's
// pinned set, which are now the same slice — fails a test instead of silently
// shrinking both at once.
//
// The membership matters in both directions. A variable the guard pins and
// this helper skips keeps the dead guarded port and shadows the container this
// package just started, which is bd-799 one layer up: every store-backed test
// in the package fails with "unreachable at 127.0.0.1:1" and nothing in the
// message names the environment. A variable this helper publishes and the
// guard does not pin is a route to the live server that the guard leaves open.
func TestDoltPortEnvVarsAreTheGuardedSet(t *testing.T) {
	want := []string{"BEADS_DOLT_SERVER_PORT", "BEADS_DOLT_PORT", "GT_DOLT_PORT"}

	if len(doltPortEnvVars) != len(want) {
		t.Fatalf("doltPortEnvVars = %v, want %v", doltPortEnvVars, want)
	}
	for i, name := range want {
		if doltPortEnvVars[i] != name {
			t.Errorf("doltPortEnvVars[%d] = %q, want %q (order is the resolution order)", i, doltPortEnvVars[i], name)
		}
	}
}
