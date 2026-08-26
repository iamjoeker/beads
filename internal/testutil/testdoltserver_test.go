//go:build !windows

package testutil

import (
	"os"
	"strconv"
	"testing"

	"github.com/steveyegge/beads/internal/testenv"
)

// TestPublishDoltPortEnv_OverridesAmbientProductionPort pins the fix for
// bd-799: the test container's port must reach bd through *every* env var bd's
// port resolver reads, not just the legacy one.
//
// bd resolves BEADS_DOLT_SERVER_PORT first and only falls back to
// BEADS_DOLT_PORT when it is unset. Agent shells export
// BEADS_DOLT_SERVER_PORT=3307 (the production Dolt server), so publishing only
// the legacy var leaves the ambient production port in effect; the
// BEADS_TEST_MODE guard then rewrites 3307 to the unreachable sentinel port 1
// and every store-backed test in the package fails to connect.
func TestPublishDoltPortEnv_OverridesAmbientProductionPort(t *testing.T) {
	const containerPort = "54321"

	// Ambient environment as an agent shell provides it: both vars pointing at
	// the production server.
	t.Setenv("BEADS_DOLT_SERVER_PORT", "3307")
	t.Setenv("BEADS_DOLT_PORT", "3307")

	if err := publishDoltPortEnv(containerPort); err != nil {
		t.Fatalf("publishDoltPortEnv: %v", err)
	}

	// Named literally rather than ranging over doltPortEnvVars: dropping a var
	// from that list must fail this test, not silently shrink it.
	for _, key := range []string{"BEADS_DOLT_SERVER_PORT", "BEADS_DOLT_PORT"} {
		if got := os.Getenv(key); got != containerPort {
			t.Errorf("%s = %q, want %q (ambient production port still shadows the test container)", key, got, containerPort)
		}
	}
}

// TestClearDoltPortEnv_RetractsEveryPortVar verifies the un-publish is as
// complete as the publish: a terminated container must not leave its now-dead
// port behind under any of the vars bd reads.
//
// It used to require the variables to end up UNSET, and that was the wrong
// half of the invariant. Unset is not neutral here — it is precisely how
// resolution reaches the 3307 production default, so retracting by unsetting
// handed the live server to everything that ran after teardown (bd-4xn). What
// the retraction owes is that no variable still names the terminated
// container, and that none of them leaves a path back to production. The
// guarded sentinel satisfies both; unset satisfies only the first.
func TestClearDoltPortEnv_RetractsEveryPortVar(t *testing.T) {
	const terminated = "54321"
	t.Setenv("BEADS_DOLT_SERVER_PORT", terminated)
	t.Setenv("BEADS_DOLT_PORT", terminated)
	t.Setenv("GT_DOLT_PORT", terminated)

	clearDoltPortEnv()

	guarded := strconv.Itoa(testenv.GuardedDoltPort)
	// Named literally rather than ranging over doltPortEnvVars: dropping a var
	// from that list must fail this test, not silently shrink it.
	for _, key := range []string{"BEADS_DOLT_SERVER_PORT", "BEADS_DOLT_PORT", "GT_DOLT_PORT"} {
		got, ok := os.LookupEnv(key)
		if !ok {
			t.Errorf("%s unset after clearDoltPortEnv: an unset port variable resolves to the production default", key)
			continue
		}
		if got == terminated {
			t.Errorf("%s = %q after clearDoltPortEnv: still names the terminated container", key, got)
		}
		if got != guarded {
			t.Errorf("%s = %q after clearDoltPortEnv, want the guarded port %q", key, got, guarded)
		}
	}
}
