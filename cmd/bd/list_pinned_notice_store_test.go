//go:build cgo

package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/steveyegge/beads/internal/types"
	"github.com/steveyegge/beads/internal/workapi"
	"github.com/steveyegge/beads/issueops"
)

// The incident, reproduced against a real store and the real filter builder.
//
// A stub can show that the renderer says the right words about a number. It
// cannot show that the number is the one the caller's own listing dropped —
// that depends on BuildListFilter arming the pinned exclusion and on the SQL
// predicate selecting on pinned=1 through the same label join. This test grades
// the behaviour: the listing must under-report, and the probe must recover
// exactly what it under-reported by.
func TestHiddenPinnedProbeCountsWhatTheListingDropped(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tmpDir := t.TempDir()
	testDB := filepath.Join(tmpDir, ".beads", "beads.db")
	s := newTestStore(t, testDB)

	const label = "gt:escalation"
	visible := &types.Issue{
		Title:     "an ordinary open escalation",
		Status:    types.StatusOpen,
		Priority:  1,
		IssueType: types.TypeTask,
		Labels:    []string{label},
		CreatedAt: time.Now(),
	}
	if err := s.CreateIssue(ctx, visible, "test"); err != nil {
		t.Fatalf("create the visible escalation: %v", err)
	}
	for _, title := range []string{"pinned escalation A", "pinned escalation B", "pinned escalation C"} {
		iss := &types.Issue{
			Title:     title,
			Status:    types.StatusOpen,
			Priority:  1,
			IssueType: types.TypeTask,
			Labels:    []string{label},
			Pinned:    true,
			CreatedAt: time.Now(),
		}
		if err := s.CreateIssue(ctx, iss, "test"); err != nil {
			t.Fatalf("create %s: %v", title, err)
		}
	}

	// The filter `bd list --label gt:escalation` builds, from the same config
	// loader and the same builder the command runs.
	cfg, err := workapi.LoadStoreListConfig(ctx, s)
	if err != nil {
		t.Fatalf("load list config: %v", err)
	}
	req := issueops.ListRequest{Labels: []string{label}}
	filter, err := workapi.BuildListFilter(req, cfg)
	if err != nil {
		t.Fatalf("build list filter: %v", err)
	}
	listing := workapi.PinnedNoticeFor(req, filter)
	if !listing.Applies() {
		t.Fatal("a plain labeled listing must be the case that arms the pinned exclusion; the probe has nothing to explain otherwise")
	}

	// The silence: four open issues carry the label, the listing shows one, and
	// the screen says nothing about the other three.
	listed, err := s.SearchIssues(ctx, "", filter)
	if err != nil {
		t.Fatalf("run the listing: %v", err)
	}
	if len(listed) != 1 || listed[0].ID != visible.ID {
		t.Fatalf("expected the listing to return only the unpinned escalation, got %d rows", len(listed))
	}

	// The disclosure: the same query, with the exclusion inverted, recovers the
	// three the caller was never told about.
	hidden := countHiddenPinned(ctx, s, listing)
	if hidden != 3 {
		t.Fatalf("expected the probe to count the 3 rows the listing dropped, got %d", hidden)
	}

	// A bead pinned twice over — the boolean AND the status — is hidden by two
	// defaults at once, and the default status exclusion is one the probe must
	// drop. Inheriting it would return a measured zero over exactly the rows
	// this notice reports on.
	doublyPinned := &types.Issue{
		Title:     "a reference bead parked in the pinned status",
		Status:    types.StatusPinned,
		Priority:  1,
		IssueType: types.TypeTask,
		Labels:    []string{label},
		Pinned:    true,
		CreatedAt: time.Now(),
	}
	if err := s.CreateIssue(ctx, doublyPinned, "test"); err != nil {
		t.Fatalf("create the doubly-pinned escalation: %v", err)
	}
	if got := countHiddenPinned(ctx, s, listing); got != 4 {
		t.Fatalf("a row in the pinned STATUS is hidden too and must be counted, got %d", got)
	}

	// The count is the caller's question, not a wider one: an issue that is
	// pinned but carries a different label is not part of it.
	other := &types.Issue{
		Title:     "a pinned reference bead about something else",
		Status:    types.StatusOpen,
		Priority:  1,
		IssueType: types.TypeTask,
		Labels:    []string{"tech-debt"},
		Pinned:    true,
		CreatedAt: time.Now(),
	}
	if err := s.CreateIssue(ctx, other, "test"); err != nil {
		t.Fatalf("create the unrelated pinned bead: %v", err)
	}
	if got := countHiddenPinned(ctx, s, listing); got != 4 {
		t.Fatalf("the probe must inherit the label predicate, got %d", got)
	}
}
