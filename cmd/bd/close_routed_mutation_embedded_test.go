//go:build cgo

package main

import (
	"os"
	"strings"
	"testing"

	"github.com/steveyegge/beads/internal/types"
)

// TestEmbeddedRoutedMutationPersists is the regression test for bd-gq7.
//
// Contributor auto-routing opened the routed target read-only even for
// mutation commands, so an issue that lives only in the routed store was
// append-only: `bd create` landed there (routing sends creates to the planning
// store) while every later `bd close`/`bd update` on that same issue died with
// "embeddeddolt: store is read-only" and left the row untouched. Merge-request
// wisps are the case that made it a P0 — they are created in the rig store from
// a sandbox cwd and must later be retired there, so no MR could ever be closed
// or reaped.
//
// The wisp subtest is the acceptance case ("a wisp can be closed from a polecat
// sandbox cwd and the change persists"); the plain-issue subtest pins that the
// defect was never wisp-specific, and the update subtest covers the other half
// of the mutation surface. Each asserts persistence via a fresh `bd show`
// rather than the closing command's own output, because the old code printed
// its error to stderr while the read path kept reporting the stale open row —
// that split is exactly what made the bug survive so long in the field.
func TestEmbeddedRoutedMutationPersists(t *testing.T) {
	if os.Getenv("BEADS_TEST_EMBEDDED_DOLT") != "1" {
		t.Skip("set BEADS_TEST_EMBEDDED_DOLT=1 to run embedded dolt integration tests")
	}
	t.Parallel()

	bd := buildEmbeddedBD(t)

	cases := []struct {
		name       string
		createArgs []string
		// mutate runs the command under test from the sandbox cwd.
		mutate     func(t *testing.T, bd, sandboxDir, id string)
		wantStatus types.Status
	}{
		{
			name:       "wisp_close_persists",
			createArgs: []string{"Merge: routed branch", "--ephemeral", "-p", "1"},
			mutate: func(t *testing.T, bd, sandboxDir, id string) {
				bdClose(t, bd, sandboxDir, id, "--reason", "merged")
			},
			wantStatus: types.StatusClosed,
		},
		{
			name:       "plain_issue_close_persists",
			createArgs: []string{"routed task", "-p", "2"},
			mutate: func(t *testing.T, bd, sandboxDir, id string) {
				bdClose(t, bd, sandboxDir, id, "--reason", "done")
			},
			wantStatus: types.StatusClosed,
		},
		{
			name:       "plain_issue_update_persists",
			createArgs: []string{"routed task", "-p", "2"},
			mutate: func(t *testing.T, bd, sandboxDir, id string) {
				bdRunOK(t, bd, sandboxDir, "update", id, "--status=in_progress")
			},
			wantStatus: types.StatusInProgress,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			planningDir, _, _ := bdInit(t, bd, "--prefix", "pl")
			sandboxDir, _, _ := bdInit(t, bd, "--prefix", "sb")

			// routing.default is the auto-routing rule with no git-role setup:
			// it sends resolution from the sandbox to the planning store, the
			// same leg a Gas Town sandbox reaches its rig's store through.
			bdRunOK(t, bd, sandboxDir, "config", "set", "routing.default", planningDir)

			// Created from the sandbox, on purpose: routing puts it in the
			// planning store, which is what makes the read-only mutation open a
			// contradiction rather than a safety property.
			issue := bdCreate(t, bd, sandboxDir, tc.createArgs...)
			if issue.ID == "" {
				t.Fatal("bd create returned no ID")
			}

			tc.mutate(t, bd, sandboxDir, issue.ID)

			if got := bdShow(t, bd, sandboxDir, issue.ID); got.Status != tc.wantStatus {
				t.Errorf("after mutation from the sandbox cwd, %s has status %q, want %q "+
					"(the routed store swallowed the write)", issue.ID, got.Status, tc.wantStatus)
			}
		})
	}
}

// TestEmbeddedClosePartialBatchExitsNonZero pins the second half of bd-gq7:
// `bd close` reported success for a batch in which some IDs never closed.
//
// The exit gate only fired when NOTHING settled (closedCount == 0 &&
// alreadyClosed == 0), so a single success masked every sibling refusal and the
// command exited 0 with issues still open. Any automation checking exit status —
// the refinery retiring merged MRs, the wisp reaper — read that as "the batch
// closed". `bd update` had already been fixed for the identical shape
// (reportUpdateFailures, "multi-ID update used to exit 0 after mid-batch
// failures"); close had not.
//
// The refusal is produced with the open-children guard rather than a read-only
// store so the case is topology-independent: this is about the exit code, not
// about routing.
func TestEmbeddedClosePartialBatchExitsNonZero(t *testing.T) {
	if os.Getenv("BEADS_TEST_EMBEDDED_DOLT") != "1" {
		t.Skip("set BEADS_TEST_EMBEDDED_DOLT=1 to run embedded dolt integration tests")
	}
	t.Parallel()

	bd := buildEmbeddedBD(t)
	dir, _, _ := bdInit(t, bd, "--prefix", "pb")

	parent := bdCreate(t, bd, dir, "parent with open child", "-p", "2")
	child := bdCreate(t, bd, dir, "open child", "-p", "2")
	closable := bdCreate(t, bd, dir, "closable", "-p", "2")
	bdDepAdd(t, bd, dir, child.ID, parent.ID, "--type", "parent-child")

	// parent is refused (open child), closable succeeds — the partial batch.
	out, code := bdRunFailCode(t, bd, dir, "close", parent.ID, closable.ID, "--reason", "batch")
	if code != 1 {
		t.Errorf("partial-failure close exit code = %d, want 1\n%s", code, out)
	}
	if !strings.Contains(out, "1 of 2 issues failed to close") {
		t.Errorf("partial-failure close should summarize which IDs failed, got:\n%s", out)
	}
	if !strings.Contains(out, parent.ID) {
		t.Errorf("failure summary should name the refused ID %s, got:\n%s", parent.ID, out)
	}

	// The successful half must stay closed and committed: the batch is per-ID,
	// not atomic, so a nonzero exit must not read as "nothing happened".
	if got := bdShow(t, bd, dir, closable.ID); got.Status != types.StatusClosed {
		t.Errorf("%s status = %q, want closed: a sibling's refusal must not roll back a real close",
			closable.ID, got.Status)
	}
	if got := bdShow(t, bd, dir, parent.ID); got.Status == types.StatusClosed {
		t.Errorf("%s was refused and must still be open, got closed", parent.ID)
	}

	// Guard the other direction: an all-success batch still exits 0, so the new
	// gate cannot turn ordinary closes into spurious failures.
	a := bdCreate(t, bd, dir, "batch ok a", "-p", "2")
	b := bdCreate(t, bd, dir, "batch ok b", "-p", "2")
	bdClose(t, bd, dir, a.ID, b.ID, "--reason", "batch")
}

// TestEmbeddedMutateUnknownIDExitsNonZero pins the not-found half of bd-gq7's
// exit-code acceptance: `bd close <unknown>` printed "no issue found" and
// exited 0, so automation that closes by ID — the refinery retiring an MR whose
// wisp it cannot see, the reaper — read a missed target as a completed close.
// The routing rewrite made both commands surface the resolution failure as a
// real error, and this pins it so a future routing change cannot quietly send
// the miss back down a nil-returning path.
//
// Unknown-ID resolution is the one failure mode reachable without constructing
// a store that refuses writes, and it exercises the same reporting seam the
// read-only error travels through, so it guards both.
func TestEmbeddedMutateUnknownIDExitsNonZero(t *testing.T) {
	if os.Getenv("BEADS_TEST_EMBEDDED_DOLT") != "1" {
		t.Skip("set BEADS_TEST_EMBEDDED_DOLT=1 to run embedded dolt integration tests")
	}
	t.Parallel()

	bd := buildEmbeddedBD(t)
	dir, _, _ := bdInit(t, bd, "--prefix", "nf")

	// Well-formed for the workspace prefix and simply absent, so the miss comes
	// from resolution rather than from argument validation.
	const missing = "nf-nosuchissue"

	for _, tc := range []struct {
		name string
		args []string
	}{
		{name: "close", args: []string{"close", missing, "--reason", "gone"}},
		{name: "update", args: []string{"update", missing, "--status=in_progress"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, code := bdRunFailCode(t, bd, dir, tc.args...)
			if code != 1 {
				t.Errorf("bd %s on an unknown ID exit code = %d, want 1\n%s", tc.name, code, out)
			}
			if !strings.Contains(out, missing) {
				t.Errorf("bd %s on an unknown ID should name %s, got:\n%s", tc.name, missing, out)
			}
		})
	}
}
