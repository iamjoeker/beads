//go:build cgo

package main

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/steveyegge/beads/internal/types"
	"github.com/steveyegge/beads/internal/workapi"
	"github.com/steveyegge/beads/issueops"
)

// The gap between the two disclosures, measured against a REAL store rather
// than against a model of one.
//
// internal/workapi pins the same partition at the filter level, which is where
// the fix lives — but that test evaluates the probe filters with a matcher this
// repo wrote, so it can only show that the three filters SAY different things.
// It cannot show that the backend reads them the way they were meant, and the
// whole defect (bd-6xa) was two filters that each read correctly and left a row
// between them. This one puts four rows in Dolt and counts what comes back.
//
// The four rows are the two properties crossed: a status the caller selected or
// one they did not, pinned or not. Under `bd list --status open` exactly one of
// them is listed and each of the other three must be counted by exactly one
// probe. Before the intersection probe existed, the pinned hooked row was
// counted by none — it matched the table that was read, was dropped by two
// independent defaults, and was reported by no disclosure.
func TestStatusAndPinnedProbesPartitionRealRows(t *testing.T) {
	t.Parallel()
	s := newTestStore(t, filepath.Join(t.TempDir(), ".beads", "beads.db"))
	ctx := context.Background()

	rows := []struct {
		name   string
		status types.Status
		pinned bool
		// wantBy is the query that must account for the row: the listing
		// itself, or exactly one of the three probes.
		wantBy string
	}{
		{"open and unpinned", types.StatusOpen, false, "listed"},
		{"open but pinned", types.StatusOpen, true, "pinned notice"},
		{"hooked but unpinned", types.StatusHooked, false, "status notice"},
		{"hooked AND pinned", types.StatusHooked, true, "status+pinned notice"},
	}
	ids := make(map[string]string, len(rows))
	for _, r := range rows {
		issue := &types.Issue{
			Title:     r.name,
			Priority:  2,
			IssueType: types.TypeTask,
			Status:    r.status,
			Pinned:    r.pinned,
		}
		if err := s.CreateIssue(ctx, issue, "test"); err != nil {
			t.Fatalf("create %q: %v", r.name, err)
		}
		ids[issue.ID] = r.wantBy
	}

	in := issueops.ListRequest{Status: "open"}
	var cfg workapi.ListConfig
	listing, err := workapi.BuildListFilter(in, cfg)
	if err != nil {
		t.Fatalf("BuildListFilter: %v", err)
	}
	notice := workapi.StatusNoticeFor(in, listing, cfg)
	if !notice.AppliesToPinned() {
		t.Fatal("`--status open` under the pinned default is the listing the gap was about")
	}
	dropped := make([]types.Status, 0, len(notice.Dropped()))
	for _, d := range notice.Dropped() {
		dropped = append(dropped, types.Status(d))
	}

	queries := []struct {
		name   string
		filter types.IssueFilter
	}{
		{"listed", listing},
		{"pinned notice", workapi.PinnedProbeFilter(listing, statusProbeRowCap)},
		{"status notice", workapi.StatusProbeFilter(listing, dropped, statusProbeRowCap)},
		{"status+pinned notice", workapi.StatusPinnedProbeFilter(listing, dropped, statusProbeRowCap)},
	}

	// accountedBy[id] is every query that returned the row. The assertion is on
	// the whole map rather than per query, because "counted by exactly one" is
	// the property that was missing; each probe on its own was already correct.
	accountedBy := make(map[string][]string, len(ids))
	for _, q := range queries {
		issues, err := s.SearchIssues(ctx, "", q.filter)
		if err != nil {
			t.Fatalf("%s query: %v", q.name, err)
		}
		for _, issue := range issues {
			if _, ours := ids[issue.ID]; !ours {
				continue
			}
			accountedBy[issue.ID] = append(accountedBy[issue.ID], q.name)
		}
	}

	for id, want := range ids {
		got := accountedBy[id]
		if len(got) != 1 {
			t.Errorf("%s (%s) is accounted for by %d queries (%v), want exactly 1 (%s)", id, want, len(got), got, want)
			continue
		}
		if got[0] != want {
			t.Errorf("%s is accounted for by %q, want %q", id, got[0], want)
		}
	}
}

// The counts the reader actually sees, from the same real store: the status
// notice reports the singly hidden row and the doubly hidden one separately, so
// the remedy it prints matches the rows it is talking about.
func TestStatusNoticeCountsRealHiddenRowsSeparately(t *testing.T) {
	t.Parallel()
	s := newTestStore(t, filepath.Join(t.TempDir(), ".beads", "beads.db"))
	ctx := context.Background()

	for _, r := range []struct {
		title  string
		status types.Status
		pinned bool
	}{
		{"listed", types.StatusOpen, false},
		{"hidden by status alone", types.StatusHooked, false},
		{"hidden by status and the pinned default", types.StatusHooked, true},
		{"hidden by status and the pinned default, again", types.StatusInProgress, true},
	} {
		issue := &types.Issue{Title: r.title, Priority: 2, IssueType: types.TypeTask, Status: r.status, Pinned: r.pinned}
		if err := s.CreateIssue(ctx, issue, "test"); err != nil {
			t.Fatalf("create %q: %v", r.title, err)
		}
	}

	in := issueops.ListRequest{Status: "open"}
	var cfg workapi.ListConfig
	listing, err := workapi.BuildListFilter(in, cfg)
	if err != nil {
		t.Fatalf("BuildListFilter: %v", err)
	}
	notice := workapi.StatusNoticeFor(in, listing, cfg)

	hidden, err := notice.CountHidden(ctx, s, statusProbeRowCap)
	if err != nil {
		t.Fatalf("CountHidden: %v", err)
	}
	if hidden != 1 {
		t.Errorf("CountHidden = %d, want 1 (the unpinned hooked row)", hidden)
	}
	pinnedHidden, err := notice.CountHiddenPinned(ctx, s, statusProbeRowCap)
	if err != nil {
		t.Fatalf("CountHiddenPinned: %v", err)
	}
	if pinnedHidden != 2 {
		t.Errorf("CountHiddenPinned = %d, want 2 (the pinned hooked and pinned in_progress rows)", pinnedHidden)
	}
}
