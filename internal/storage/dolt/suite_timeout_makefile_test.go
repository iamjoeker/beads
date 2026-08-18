package dolt

import (
	"os"
	"regexp"
	"testing"
	"time"
)

// makefileDoltTimeout matches the DOLT_SUITE_TIMEOUT assignment in the repo
// Makefile, which is what `make test-dolt` passes to go test -timeout.
var makefileDoltTimeout = regexp.MustCompile(`(?m)^DOLT_SUITE_TIMEOUT\s*\?=\s*(\S+)`)

// TestMakefileDoltTargetCarriesTheFloor keeps the two halves of this fix from
// drifting apart. The guard in TestMain refuses to start below
// requiredSuiteTimeout; `make test-dolt` is the command the refusal tells you
// to run. If the Makefile's ceiling ever falls below the constant, the one
// recommended invocation becomes the one that gets refused.
//
// This test reads a file rather than a symbol because make cannot import Go
// constants. It needs no Dolt server, so it still runs under
// BEADS_TEST_SKIP=dolt — which is where CI exercises this package.
func TestMakefileDoltTargetCarriesTheFloor(t *testing.T) {
	t.Parallel()

	const makefilePath = "../../../Makefile"
	content, err := os.ReadFile(makefilePath)
	if err != nil {
		t.Fatalf("read %s: %v", makefilePath, err)
	}

	match := makefileDoltTimeout.FindSubmatch(content)
	if match == nil {
		t.Fatalf("%s no longer defines DOLT_SUITE_TIMEOUT; the make test-dolt target named by "+
			"the timeout refusal message must keep carrying the ceiling", makefilePath)
	}

	got, err := time.ParseDuration(string(match[1]))
	if err != nil {
		t.Fatalf("DOLT_SUITE_TIMEOUT=%q is not a duration go test will accept: %v", match[1], err)
	}
	if got < requiredSuiteTimeout {
		t.Fatalf("make test-dolt passes -timeout=%s but TestMain refuses to start below %s; "+
			"the recommended command would be refused", got, requiredSuiteTimeout)
	}
}
