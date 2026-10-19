package creds

import (
	"fmt"
	"github.com/steveyegge/beads/internal/testenv"
	"os"
	"testing"

	"github.com/steveyegge/beads/internal/testutil/credentialcmd"
)

func TestMain(m *testing.M) {
	// First statement in TestMain: point every Dolt port variable at a dead
	// port before anything in this package can resolve one. Helpers that
	// start a server publish their own port and must run after this.
	testenv.GuardProductionDolt()
	if code, ok := credentialcmd.Dispatch(); ok {
		os.Exit(code)
	}
	code := m.Run()
	if err := credentialcmd.Cleanup(); err != nil {
		fmt.Fprintf(os.Stderr, "credential command fixture cleanup: %v\n", err)
		code = 1
	}
	os.Exit(code)
}
