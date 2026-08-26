// Package schema's tests can resolve a Dolt endpoint, so the process needs a
// TestMain whose only job is to point the port variables at a dead port before
// any of them runs.
//
// Without one, resolution here ends at the production default and `go test`
// from a developer or agent sandbox creates databases on the live server
// (bd-4xn). test/doltguardpolicy enforces that this file exists.
package schema

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
