package dolt

import (
	"testing"

	"github.com/steveyegge/beads/internal/testutil"
)

// TestCountTestBranchesSeesALiveTestBranch checks the measurement TestMain
// prints after m.Run — the number of test branches that outlived the run.
// A count that silently reported 0, or -1 because the query failed, would look
// exactly like "nothing leaked", which is the answer the number exists to
// distinguish from. So take it while a branch is known to exist.
func TestCountTestBranchesSeesALiveTestBranch(t *testing.T) {
	// setupTestStore creates this test's own branch and parallelises it.
	_, cleanup := setupTestStore(t)
	defer cleanup()

	if got := testutil.CountTestBranches(testSharedConn, testSharedDB); got < 1 {
		t.Fatalf("CountTestBranches = %d while this test holds a branch; want at least 1 "+
			"(-1 means the count could not be taken at all)", got)
	}
}
