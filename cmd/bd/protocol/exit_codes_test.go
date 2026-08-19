package protocol

import (
	"strings"
	"testing"
)

// TestProtocol_CloseBlockedExitsNonZero verifies that closing an issue blocked
// by open dependencies returns exit code 1.
func TestProtocol_CloseBlockedExitsNonZero(t *testing.T) {
	t.Parallel()
	w := newWorkspace(t)

	blocker := w.create("Blocker issue")
	blocked := w.create("Blocked issue")
	w.run("dep", "add", blocked, blocker, "--type=blocks")

	_, code := w.runExpectError("close", blocked)
	if code != 1 {
		t.Errorf("expected exit code 1, got %d", code)
	}
}

// TestProtocol_CloseUnblockedExitsZero verifies that closing an unblocked
// issue returns exit code 0 (no regression).
func TestProtocol_CloseUnblockedExitsZero(t *testing.T) {
	t.Parallel()
	w := newWorkspace(t)
	id := w.create("Simple issue")
	w.run("close", id)
}

// TestProtocol_UpdateNonexistentExitsNonZero verifies that updating a
// nonexistent issue returns exit code 1.
func TestProtocol_UpdateNonexistentExitsNonZero(t *testing.T) {
	t.Parallel()
	w := newWorkspace(t)
	_, code := w.runExpectError("update", "nonexistent-xyz", "--status", "in_progress")
	if code != 1 {
		t.Errorf("expected exit code 1, got %d", code)
	}
}

// TestProtocol_ClosePartialFailureExitsNonZero verifies that when closing
// multiple issues where some succeed and some fail (e.g., blocked), the
// command exits 1 — a partial batch is a failure, not a success — while still
// closing (and committing) the closeable ones.
//
// This test used to pin the opposite contract, under the name
// ...ExitsZero: partial success exited 0 (GH#2014, Feb 2026). bd-gq7 reversed
// it deliberately, because every caller that checks exit status — the refinery
// retiring merged MRs, the wisp reaper — read a 0 as "the whole batch closed"
// while siblings were still open. The closes themselves are per-ID and are NOT
// rolled back, so the assertions below still pin that half.
func TestProtocol_ClosePartialFailureExitsNonZero(t *testing.T) {
	t.Parallel()
	w := newWorkspace(t)

	closeable := w.create("Closeable issue")
	blocker := w.create("Blocker issue")
	blocked := w.create("Blocked issue")
	w.run("dep", "add", blocked, blocker, "--type=blocks")

	// Close both: closeable should succeed, blocked should fail.
	// A partial batch (some closed, some refused) exits 1.
	out0, code := w.runExpectError("close", closeable, blocked)
	if code != 1 {
		t.Errorf("expected exit code 1, got %d", code)
	}
	if !strings.Contains(out0, "1 of 2 issues failed to close") {
		t.Errorf("expected the per-ID failure summary naming the refused ID, got:\n%s", out0)
	}

	// Verify the closeable one was actually closed despite partial failure
	out := w.run("show", closeable, "--json")
	issues := parseJSONOutput(t, out)
	if len(issues) == 0 {
		t.Fatal("show returned no issues")
	}
	status, _ := issues[0]["status"].(string)
	if status != "closed" {
		t.Errorf("closeable issue should be closed despite partial failure, got status=%q", status)
	}

	// Verify the blocked one is still open
	out2 := w.run("show", blocked, "--json")
	issues2 := parseJSONOutput(t, out2)
	if len(issues2) > 0 {
		status2, _ := issues2[0]["status"].(string)
		if status2 == "closed" {
			t.Error("blocked issue should NOT be closed")
		}
	}
}

// TestProtocol_CloseNonexistentExitsNonZero verifies that closing a
// nonexistent issue returns a non-zero exit code.
func TestProtocol_CloseNonexistentExitsNonZero(t *testing.T) {
	t.Parallel()
	w := newWorkspace(t)
	out, _ := w.runExpectError("close", "nonexistent-xyz")
	if !strings.Contains(strings.ToLower(out), "not found") {
		t.Logf("output: %s", out)
	}
}
