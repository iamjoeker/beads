// Package scripts_test's tests spawn the test runner and other scripts, which
// inherit this process's environment, so it needs a TestMain that points the
// Dolt port variables at a dead port before any of them runs.
//
// Without one, a spawned script's own `go test` or `bd` invocation resolves to
// the production default and can create databases on the live server
// (bd-4xn). test/doltguardpolicy enforces that this file exists.
package scripts_test

import (
	"os"
	"testing"

	"github.com/steveyegge/beads/internal/testenv"
)

func TestMain(m *testing.M) {
	// First statement in TestMain: point every Dolt port variable at a dead
	// port before anything in this package can resolve one. Helpers that
	// start a server publish their own port and must run after this.
	testenv.GuardProductionDolt()
	os.Exit(m.Run())
}
