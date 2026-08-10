//go:build cgo

package main

import (
	"os"
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
