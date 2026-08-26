package scripts_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestTestScriptSharedServerPublishesBothPortVars pins the harness half of the
// production-Dolt guard (bd-4xn).
//
// The shared-server block starts one dolt sql-server for the whole run and
// exports its port. It used to export BEADS_DOLT_PORT alone, which is the
// losing variable: bd resolves BEADS_DOLT_SERVER_PORT first, and the guard
// pins that one to its dead port at TestMain time precisely because leaving it
// unset is a path back to production. A half publish therefore points every
// package at port 1 while a healthy shared server sits unused — and no default
// `./scripts/test.sh` run exercises this lane, so no suite result would say so.
//
// The precondition has the same shape for the same reason: a caller who
// exported only BEADS_DOLT_SERVER_PORT has already chosen a server, and the
// old half check would have started a second one and pointed nothing at it.
//
// This reads the source rather than running the block, which needs `dolt`,
// `nc`, a free port and about a second of server startup. What can go wrong
// here is a variable being dropped from the pair, and that is visible in the
// text.
func TestTestScriptSharedServerPublishesBothPortVars(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(sourceRepoRoot(t), "scripts", "test.sh"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)

	const blockStart = `if [[ "${BEADS_TEST_SHARED_SERVER:-}" == "1"`
	start := strings.Index(text, blockStart)
	if start < 0 {
		t.Fatalf("shared-server block not found in scripts/test.sh: this check cannot have inspected anything")
	}
	// The block ends at the next top-level `fi` at column zero.
	end := strings.Index(text[start:], "\nfi\n")
	if end < 0 {
		t.Fatalf("shared-server block has no closing fi: cannot delimit what to check")
	}
	block := text[start : start+end]

	guardCondition := text[start : start+strings.Index(text[start:], "\n")]
	for _, name := range []string{"BEADS_DOLT_PORT", "BEADS_DOLT_SERVER_PORT"} {
		if !strings.Contains(guardCondition, name) {
			t.Errorf("shared-server precondition does not check %s: %s\n"+
				"A caller who set only the other variable had already chosen a server.", name, guardCondition)
		}
		if !strings.Contains(block, "export "+name+"=") {
			t.Errorf("shared-server block does not export %s: bd reads BEADS_DOLT_SERVER_PORT ahead of BEADS_DOLT_PORT, "+
				"so publishing the shared port under only one of them leaves the guard's dead port shadowing it", name)
		}
	}
}
