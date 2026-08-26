// Package testenv holds the guards that keep a beads test binary off live
// state.
//
// It imports nothing outside the standard library. That is load-bearing rather
// than tidy: the guard is called from TestMain in packages at every level of
// the tree, including internal/configfile and internal/storage/dolt whose own
// code the guard is protecting against, so any import here would be an import
// cycle waiting for the next caller. Values that must agree with production
// constants are duplicated by value and pinned by a drift test instead
// (TestProductionDoltPortMatchesProductionConstants).
package testenv

import (
	"os"
	"strconv"
	"strings"
	"testing"
)

// ProductionDoltPort is the port the live Dolt server listens on — the value
// of configfile.DefaultDoltServerPort and dolt.DefaultSQLPort.
//
// It is duplicated by value rather than imported, for the reason in the
// package comment. TestProductionDoltPortMatchesProductionConstants fails if
// the copies ever drift apart.
const ProductionDoltPort = 3307

// GuardedDoltPort is the port a guarded test process is pointed at instead of
// the production server.
//
// It is 1 rather than some high unregistered port because 1 is already this
// repo's sentinel for "resolution landed somewhere deliberately dead":
// applyConfigDefaults rewrites a production port to 1 under BEADS_TEST_MODE=1,
// and "Dolt server unreachable at 127.0.0.1:1" is the documented shape of a
// test that reached for Dolt without arranging a server (bd-799). Reusing it
// means the guard produces the failure operators already recognize instead of
// a second dialect for the same condition.
//
// Being privileged is a second reason, not an obstacle: nothing can bind port
// 1, so "nothing listens there" holds by construction. A high port can be
// bound by a sibling test that starts a server on whatever resolution
// produced, which turns an expected connection-refused into a live server and
// a green-looking wrong answer.
const GuardedDoltPort = 1

// AllowProductionDoltEnv opts a test process back in to the production Dolt
// server. It exists for the handful of checks that genuinely have to talk to
// the live server — operational smoke tests run by hand — and is never set in
// CI or in an agent sandbox.
//
// Its value must name the boundary being crossed: the production port, as
// decimal digits. A bare "1" authorizes nothing. Naming the boundary rather
// than setting a boolean is what separates a process that knows it is aimed at
// the live server from one that merely inherited a flag: this bead exists
// because a guard everybody believed in was set on a variable nothing read,
// and a boolean opt-in fails the same way one layer up.
const AllowProductionDoltEnv = "BEADS_ALLOW_TEST_DOLT"

// doltPortEnvVars are the variables through which a Dolt endpoint reaches both
// the in-process resolvers and any bd or dolt subprocess a test spawns, which
// inherits them from os.Environ.
//
// The order is the resolution order the beads resolvers use, and all of them
// matter. There are four independent copies of the
// "BEADS_DOLT_SERVER_PORT, then BEADS_DOLT_PORT" precedence in production code
// (configfile.GetDoltServerPort, dolt.applyConfigDefaults, bootstrap's
// serverClonePort, doltserver's port sources); guarding only the one a given
// caller reads leaves the others pointed at the 3307 default.
//
// GT_DOLT_PORT is here for subprocesses only — no beads code reads it, and
// TestNoBeadsCodeReadsGTDoltPort holds that true. A beads test running inside
// a Gas Town workspace inherits it, and anything it shells out to that
// resolves through Gas Town's own resolver would find the live town port. It
// costs one Setenv to close that route and there is no reason to leave it
// open.
var doltPortEnvVars = []string{"BEADS_DOLT_SERVER_PORT", "BEADS_DOLT_PORT", "GT_DOLT_PORT"}

// DoltPortEnvVars returns the variables GuardProductionDolt points at the
// guarded port.
//
// A helper that starts a Dolt server for a test has to overwrite every one of
// them, not just the one its own caller reads: whatever it leaves untouched
// still holds the dead guarded port and shadows the helper's, per the
// precedence above. internal/testutil's container helpers are the callers this
// exists for; they keep their own copy of the list rather than importing this
// package, and TestDoltPortEnvVarsMatchGuard there fails if the two drift.
func DoltPortEnvVars() []string {
	out := make([]string, len(doltPortEnvVars))
	copy(out, doltPortEnvVars)
	return out
}

// ProductionDoltAllowed reports whether this process has explicitly opted in
// to using the production Dolt server.
//
// The opt-in has to name the port it is reaching for. An operator running a
// smoke check against the real server knows which one that is; a stray export
// of "1" does not, and authorizes nothing.
func ProductionDoltAllowed() bool {
	allowed, ok := os.LookupEnv(AllowProductionDoltEnv)
	return ok && strings.TrimSpace(allowed) == strconv.Itoa(ProductionDoltPort)
}

// GuardProductionDolt points the current process at GuardedDoltPort so its
// tests cannot reach the production Dolt server.
//
// Call it as the first statement of TestMain's body, before any helper that
// starts a server of its own:
//
//	func TestMain(m *testing.M) {
//	    testenv.GuardProductionDolt()
//	    os.Exit(testMainInner(m))
//	}
//
// Why this is needed at all: every Dolt port resolver in this repo ends in the
// same fallback — the 3307 default, which is the live server. A test that
// builds a fixture .beads dir under t.TempDir() and then calls production code
// inherits that fallback, so `go test` from a developer or agent sandbox
// creates real databases on the live server. The existing defenses do not
// close this: dolt.applyConfigDefaults rewrites a production port to 1 and
// dolt.New panics on one, but both are conditioned on BEADS_TEST_MODE=1, which
// nothing sets for a bare `go test` — and scripts/test.sh exports it only on
// the shared-server path. Setting the port variables to a dead port removes
// the fallback itself, so resolution finds the guarded port and never reaches
// the default, whether or not BEADS_TEST_MODE is set.
//
// The guard can only ever resolve AWAY from production. A variable already
// holding a deliberate non-production port is left alone, so a developer
// pointing BEADS_DOLT_SERVER_PORT at their own scratch server keeps it, and a
// helper that starts a container for the whole run —
// testutil.EnsureDoltContainerForTestMain — sets these variables itself and
// must therefore be called after this, not before.
//
// Individual tests that need a specific value still use t.Setenv, which
// restores the guarded value rather than the host's on cleanup.
func GuardProductionDolt() {
	if ProductionDoltAllowed() {
		return
	}
	guarded := strconv.Itoa(GuardedDoltPort)
	for _, name := range doltPortEnvVars {
		if needsGuarding(os.Getenv(name)) {
			_ = os.Setenv(name, guarded)
		}
	}
}

// WithoutDoltPortGuard clears the guarded port variables for the duration of
// one test and restores them when it finishes.
//
// It exists for the tests whose subject *is* the unconfigured fallback — "with
// no env, no port file and no config.yaml, which port does this resolve to?"
// GuardProductionDolt sets those variables precisely so that fallback is never
// reached, which would otherwise leave such tests asserting the guarded port
// and no longer checking the real default at all. It is needed for a second
// reason unique to beads: an env port is authoritative here, so it shadows the
// port-file and metadata resolution chain, and a test whose subject is that
// chain must clear the env to see it.
//
// This is safe only because such tests resolve a port or build a connection
// string without opening one. Do not use it to let a test talk to Dolt; that
// is what AllowProductionDoltEnv is for, and it is not set in CI.
//
// Like t.Setenv, it must not be combined with t.Parallel: the port variables
// are process-wide.
func WithoutDoltPortGuard(t testing.TB) {
	t.Helper()
	for _, name := range doltPortEnvVars {
		prev, had := os.LookupEnv(name)
		if err := os.Unsetenv(name); err != nil {
			t.Fatalf("unsetenv %s: %v", name, err)
		}
		t.Cleanup(func() {
			if had {
				_ = os.Setenv(name, prev)
				return
			}
			_ = os.Unsetenv(name)
		})
	}
}

// needsGuarding reports whether an environment value leaves a path back to the
// production server.
//
// Empty qualifies: an unset variable is exactly how the resolvers end up at
// the 3307 default. So does an unparseable or non-positive value, which every
// resolver in the tree skips for that same fallback — `strconv.Atoi` failing,
// or `p > 0` failing, sends each of them on to the next source and ultimately
// to the default. A deliberate value for some other port does not.
func needsGuarding(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return true
	}
	port, err := strconv.Atoi(value)
	if err != nil {
		return true
	}
	if port <= 0 {
		return true
	}
	return port == ProductionDoltPort
}
