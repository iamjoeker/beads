package dolt

import (
	"context"
	"strings"
	"testing"
)

// TestPullWithAutoResolve_BranchTrackingFallback verifies that when DOLT_PULL
// returns the GH#3144 branch-tracking error (repo_state.json 'branches' map
// is empty because the remote was added via `bd dolt remote add` rather than
// `dolt clone`), pullWithAutoResolve enters the DOLT_FETCH + DOLT_MERGE
// fallback path.
//
// This test covers the fallback error leg (DOLT_FETCH fails because the test
// store has no configured remote). The success path — where DOLT_FETCH and
// DOLT_MERGE both succeed — is covered by
// TestPullWithAutoResolve_BranchTrackingFallbackSuccess below, and end to end
// against a remotesapi server by TestPullWithAutoResolve_BranchTrackingSuccess
// in the integration test file (//go:build integration).
func TestPullWithAutoResolve_BranchTrackingFallback(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx, cancel := testContext(t)
	defer cancel()

	// Create a stored procedure that injects the Dolt GH#3144 error text.
	// This reproduces the message DOLT_PULL emits when repo_state.json lacks
	// branch-tracking info for the remote, without requiring a real remote.
	//
	// The text is TRUNCATED relative to Dolt's real message, and must stay
	// under 128 characters: MySQL caps SIGNAL's MESSAGE_TEXT at 128, and
	// exceeding it raises "signal condition information item MESSAGE_TEXT has
	// max length of 128" INSTEAD of the message we are trying to inject — so
	// the fixture silently stops testing what it claims to. Only the substring
	// matched by isBranchTrackingError ("did not specify a branch") is load-
	// bearing; the rest of Dolt's real sentence is decoration.
	const createSP = `
		CREATE PROCEDURE inject_tracking_error()
		BEGIN
			SIGNAL SQLSTATE 'HY000'
			SET MESSAGE_TEXT = 'Error 1105: You asked to pull from the remote origin, but did not specify a branch.';
		END`
	if _, err := store.execContext(ctx, createSP); err != nil {
		t.Skipf("stored procedures with SIGNAL not supported by this Dolt version: %v", err)
	}
	defer func() {
		_, _ = store.execContext(context.Background(), "DROP PROCEDURE IF EXISTS inject_tracking_error")
	}()

	// pullWithAutoResolve executes the query inside a transaction, checks the
	// error with isBranchTrackingError, and — on match — falls back to
	// DOLT_FETCH(remote, s.branch). The test store's s.remote is "" (no
	// remote configured), so DOLT_FETCH immediately fails, producing the
	// "fetch from /" error that confirms the fallback was entered.
	err := store.pullWithAutoResolve(ctx, store.remote, "CALL inject_tracking_error()")

	// The error must come from the DOLT_FETCH attempt, not from the original
	// DOLT_PULL proxy. If the fallback was not triggered, the error would
	// surface a different message (e.g. the raw SIGNAL text).
	if err == nil {
		t.Fatal("expected an error from DOLT_FETCH (no remote configured), got nil")
	}
	// This was a t.Skipf until bd-cn4. It must NOT be: "the procedure is not
	// visible to the pull long-timeout connection" is not a Dolt version
	// difference, it is exactly the bug bd-cn4 fixed — the long-timeout
	// connection opened on the default branch instead of the store's active
	// branch, where the procedure had been created. Stored procedures live in
	// dolt_procedures, which is branch-scoped, so a mismatched branch makes
	// them invisible.
	//
	// While it skipped, this test never executed its own assertion ONCE, on any
	// commit, and reported ok the whole time. Restoring the skip would re-hide a
	// regression of bd-cn4 in precisely the same way.
	if strings.Contains(err.Error(), "inject_tracking_error") && strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("stored procedure invisible to the pull long-timeout connection — "+
			"this is the bd-cn4 branch-matching regression, not a Dolt version issue: %v", err)
	}
	if !strings.Contains(err.Error(), "fetch from") {
		t.Errorf("expected 'fetch from' error confirming fallback was triggered; got: %v", err)
	}
}

// TestPullWithAutoResolve_BranchTrackingFallbackSuccess covers the SUCCESS leg
// of the GH#3144 fallback: DOLT_PULL fails for lack of upstream tracking,
// DOLT_FETCH + DOLT_MERGE take over, and the remote's rows land locally.
//
// Two fixture bugs kept this test skipping — and therefore never asserting — on
// every commit before bd-6n5. Both are load-bearing; don't "simplify" them back:
//
//  1. The remote lived on the TEST PROCESS's filesystem (a t.TempDir() seeded
//     with the dolt CLI). The suite's Dolt server runs in a container, so that
//     path does not exist for the server, and a file:// URL pointing at it looks
//     exactly like an empty remote: `branch "main" not found on remote`. The
//     remote must be built on the SERVER's filesystem, which DOLT_PUSH does —
//     it creates the remote's chunk-store layout, which an in-place `dolt init`
//     working repo does not produce anyway.
//
//  2. The precondition pulled with an explicit branch, CALL DOLT_PULL(remote,
//     branch). That form never needs tracking config, so against a remote the
//     server can actually read it SUCCEEDS — fixing only bug 1 would have moved
//     the skip to the "succeeded without tracking config" branch instead of
//     reaching the assertion. The tracking error requires the single-argument
//     form, CALL DOLT_PULL(remote), which is what federation.go's PullFromPeer
//     issues — the production caller this fallback exists for.
//
// The preconditions below are guaranteed by construction, so they are hard
// failures. A skip here would re-hide a regression exactly as before.
func TestPullWithAutoResolve_BranchTrackingFallbackSuccess(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx, cancel := testContext(t)
	defer cancel()

	var branch string
	if err := store.db.QueryRowContext(ctx, "SELECT active_branch()").Scan(&branch); err != nil {
		t.Fatalf("read active branch: %v", err)
	}

	// Server-side remote path, unique per test branch so parallel tests in this
	// package cannot collide in the server's filesystem. Nothing here can remove
	// it afterwards (no SQL reaches the server's filesystem); it dies with the
	// suite's container.
	remoteURL := "file:///tmp/beads-branch-tracking-remote-" + branch
	if _, err := store.db.ExecContext(ctx, "CALL DOLT_REMOTE('add', 'origin', ?)", remoteURL); err != nil {
		t.Fatalf("add remote via DOLT_REMOTE: %v", err)
	}
	refspec := branch + ":main"
	if _, err := store.db.ExecContext(ctx, "CALL DOLT_PUSH(?, ?)", "origin", refspec); err != nil {
		t.Fatalf("seed remote via DOLT_PUSH: %v", err)
	}

	// Put a marker on the remote that the local branch does not have: commit it,
	// push it, then reset the local branch back one commit. The fallback's merge
	// is what has to bring it back.
	if _, err := store.db.ExecContext(ctx, "CREATE TABLE branch_tracking_marker (id INT PRIMARY KEY, value TEXT)"); err != nil {
		t.Fatalf("create marker table: %v", err)
	}
	if _, err := store.db.ExecContext(ctx, "INSERT INTO branch_tracking_marker VALUES (1, 'from remote')"); err != nil {
		t.Fatalf("insert marker row: %v", err)
	}
	if _, err := store.db.ExecContext(ctx, "CALL DOLT_COMMIT('-Am', 'test: branch tracking marker')"); err != nil {
		t.Fatalf("commit marker: %v", err)
	}
	if _, err := store.db.ExecContext(ctx, "CALL DOLT_PUSH(?, ?)", "origin", refspec); err != nil {
		t.Fatalf("push marker to remote: %v", err)
	}
	if _, err := store.db.ExecContext(ctx, "CALL DOLT_RESET('--hard', 'HEAD~1')"); err != nil {
		t.Fatalf("reset local branch behind remote: %v", err)
	}
	if err := store.db.QueryRowContext(ctx, "SELECT value FROM branch_tracking_marker WHERE id = 1").Scan(new(string)); err == nil {
		t.Fatal("marker still present locally after reset — the pull below would prove nothing")
	}

	store.remote = "origin"
	store.branch = "main"

	tx, txErr := store.db.BeginTx(ctx, nil)
	if txErr != nil {
		t.Fatalf("begin tx for raw pull check: %v", txErr)
	}
	_, rawPullErr := tx.ExecContext(ctx, "CALL DOLT_PULL(?)", "origin")
	_ = tx.Rollback()
	if rawPullErr == nil {
		t.Fatal("DOLT_PULL succeeded without tracking config: the fallback under test was never entered")
	}
	if !isBranchTrackingError(rawPullErr) {
		t.Fatalf("DOLT_PULL failed with a non-tracking error, so the fallback under test was never entered: %v", rawPullErr)
	}

	if err := store.pullWithAutoResolve(ctx, "origin", "CALL DOLT_PULL(?)", "origin"); err != nil {
		t.Fatalf("pullWithAutoResolve fallback failed: %v", err)
	}

	var got string
	if err := store.db.QueryRowContext(ctx, "SELECT value FROM branch_tracking_marker WHERE id = 1").Scan(&got); err != nil {
		t.Fatalf("query pulled marker: %v", err)
	}
	if got != "from remote" {
		t.Fatalf("pulled marker value = %q, want %q", got, "from remote")
	}
}
