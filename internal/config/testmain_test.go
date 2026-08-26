package config

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/steveyegge/beads/internal/testenv"
)

// TestMain isolates tests from the repository's own `.beads/config.yaml`.
//
// Tests expect config defaults. If the test process
// runs from within this repo, Initialize() will walk up from CWD and load
// the repo's tracked `.beads/config.yaml`, which may override defaults.
func TestMain(m *testing.M) {
	// First statement in TestMain: point every Dolt port variable at a dead
	// port before anything in this package can resolve one. Helpers that
	// start a server publish their own port and must run after this.
	testenv.GuardProductionDolt()
	tmp, err := os.MkdirTemp("", "beads-config-tests-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create temp dir: %v\n", err)
		os.Exit(1)
	}

	oldWD, _ := os.Getwd()

	// Point config discovery away from the repo and user's machine.
	_ = os.Chdir(tmp)
	_ = os.Setenv("HOME", tmp)
	_ = os.Setenv("USERPROFILE", tmp) // Windows compatibility
	_ = os.Setenv("APPDATA", filepath.Join(tmp, "AppData", "Roaming"))
	_ = os.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, "xdg-config"))

	code := m.Run()

	_ = os.Chdir(oldWD)
	_ = os.RemoveAll(tmp)
	os.Exit(code)
}
