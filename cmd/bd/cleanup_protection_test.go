package main

import (
	"testing"

	"github.com/steveyegge/beads/internal/types"
	"github.com/steveyegge/beads/internal/workapi"
)

// `bd admin cleanup` is the fourth front door onto the same rows as `bd gc`,
// `bd purge` and `bd mol wisp gc`, and until bd-724 it was the one with no GC
// protection at all — `--ephemeral --force` took every closed wisp the pinned
// flag did not cover.
//
// These cases run the filter rather than the command because the command
// refuses to run in embedded mode (requireServerMode), which is the harness
// the end-to-end sweep cases use. Each carries a same-shape control, so
// "the record survived" means the protection fired rather than the filter
// having returned nothing.

func cleanupIDs(issues []*types.Issue) []string {
	ids := make([]string, len(issues))
	for i, issue := range issues {
		ids[i] = issue.ID
	}
	return ids
}

// TestFilterCleanupCandidatesKeepsProtectedRecords covers both axes at once:
// the label the workspace configures and the wisp kind it cannot.
func TestFilterCleanupCandidatesKeepsProtectedRecords(t *testing.T) {
	closed := []*types.Issue{
		{ID: "c-step", Status: types.StatusClosed},
		{ID: "c-mr", Status: types.StatusClosed, Labels: []string{"gt:merge-request"}},
		{ID: "c-esc", Status: types.StatusClosed, WispType: types.WispTypeEscalation},
		{ID: "c-pinned", Status: types.StatusClosed, Pinned: true},
	}

	kept, skips := filterCleanupCandidates(closed, workapi.ResolveGCProtectedLabels("", nil))

	if len(kept) != 1 || kept[0].ID != "c-step" {
		t.Fatalf("kept = %v, want only c-step", cleanupIDs(kept))
	}
	if skips.Protected != 2 {
		t.Errorf("skips.Protected = %d, want 2 (the merge request and the escalation)", skips.Protected)
	}
	// The pre-existing guard has to survive the new one being layered above it.
	if skips.Pinned != 1 {
		t.Errorf("skips.Pinned = %d, want 1", skips.Pinned)
	}
}

// TestFilterCleanupCandidatesKeepsEscalationsUnderAnyLabelConfiguration is the
// bd-724 shape on this path: a label list that names nothing the wisps carry,
// which on the town in question was the state of every wisp in the database.
func TestFilterCleanupCandidatesKeepsEscalationsUnderAnyLabelConfiguration(t *testing.T) {
	closed := []*types.Issue{
		{ID: "c-hb", Status: types.StatusClosed, WispType: types.WispTypeHeartbeat},
		{ID: "c-esc", Status: types.StatusClosed, WispType: types.WispTypeEscalation},
	}

	kept, skips := filterCleanupCandidates(closed, workapi.ResolveGCProtectedLabels("ops:receipt", nil))

	if len(kept) != 1 || kept[0].ID != "c-hb" {
		t.Fatalf("kept = %v, want only c-hb — the escalation is held back by its kind, not by the setting", cleanupIDs(kept))
	}
	if skips.Protected != 1 {
		t.Errorf("skips.Protected = %d, want 1", skips.Protected)
	}
}
