//go:build cgo

package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/steveyegge/beads/internal/types"
	"github.com/steveyegge/beads/internal/workapi"
)

func TestCountMatchingStatusWispsDistinguishesZeroFromUnknown(t *testing.T) {
	listing := statusNarrowedListing(t, "in_progress")

	if got := countMatchingStatusWisps(context.Background(), nil, listing); got != unknownIssueCount {
		t.Errorf("no store means the probe never ran, want %d, got %d", unknownIssueCount, got)
	}
	failing := &wispSearcherStub{err: errors.New("boom")}
	if got := countMatchingStatusWisps(context.Background(), failing, listing); got != unknownIssueCount {
		t.Errorf("a failed probe is not a measured zero, want %d, got %d", unknownIssueCount, got)
	}
	found := &wispSearcherStub{issues: []*types.Issue{{ID: "bd-wisp-1"}}}
	if got := countMatchingStatusWisps(context.Background(), found, listing); got != 1 {
		t.Errorf("expected 1, got %d", got)
	}
}

func TestEmptyStatusListNoticeLines(t *testing.T) {
	store := `database "beads" at /rig/.beads`

	t.Run("wisps carry the status", func(t *testing.T) {
		lines := strings.Join(emptyStatusListNoticeLines([]string{"in_progress"}, 3, store), "\n")
		for _, want := range []string{"3 WISP(s)", store, "wisps table", "bd mol wisp list --all --all-stores"} {
			if !strings.Contains(lines, want) {
				t.Errorf("expected %q in:\n%s", want, lines)
			}
		}
	})

	t.Run("no status selected", func(t *testing.T) {
		if lines := emptyStatusListNoticeLines(nil, 5, store); lines != nil {
			t.Errorf("no selector, no notice, got %v", lines)
		}
	})

	t.Run("no matching wisps either", func(t *testing.T) {
		if lines := emptyStatusListNoticeLines([]string{"in_progress"}, 0, store); lines != nil {
			t.Errorf("an ordinary empty result must stay silent, got %v", lines)
		}
	})
}

// This is the bd-7ti reproduction: `bd list --status=in_progress` with no
// label predicate at all, over a store whose only in_progress record is a
// wisp. Before this notice existed, printListNotices had nothing to say —
// the label-based wisp notice never runs without a label, and the status
// notice only ever asks the issues table.
func TestPrintEmptyStatusListNoticeFiresOnBareStatusFilter(t *testing.T) {
	ctx := context.Background()
	listing := statusNarrowedListing(t, "in_progress")

	out := captureStderr(t, func() {
		s := &wispSearcherStub{issues: []*types.Issue{{ID: "bd-wisp-5aw"}}}
		if !printEmptyStatusListNotice(ctx, s, listing, 0, "beads") {
			t.Error("expected the notice to fire")
		}
	})
	for _, want := range []string{"no ISSUE has status", "in_progress", "1 WISP(s)", "wisps table"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in:\n%s", want, out)
		}
	}

	t.Run("silent when nothing matches", func(t *testing.T) {
		out := captureStderr(t, func() {
			s := &wispSearcherStub{}
			if printEmptyStatusListNotice(ctx, s, listing, 0, "beads") {
				t.Error("expected no notice")
			}
		})
		if out != "" {
			t.Errorf("expected silence, got %q", out)
		}
	})

	t.Run("silent on a non-empty listing", func(t *testing.T) {
		out := captureStderr(t, func() {
			s := &wispSearcherStub{issues: []*types.Issue{{ID: "bd-wisp-5aw"}}}
			if printEmptyStatusListNotice(ctx, s, listing, 1, "beads") {
				t.Error("expected no notice")
			}
		})
		if out != "" {
			t.Errorf("expected silence, got %q", out)
		}
	})

	t.Run("silent when the listing selected no status", func(t *testing.T) {
		unfiltered := statusNarrowedListing(t, "")
		out := captureStderr(t, func() {
			s := &wispSearcherStub{issues: []*types.Issue{{ID: "bd-wisp-5aw"}}}
			if printEmptyStatusListNotice(ctx, s, unfiltered, 0, "beads") {
				t.Error("expected no notice")
			}
		})
		if out != "" {
			t.Errorf("expected silence, got %q", out)
		}
	})
}

// ephemeralAwareSearcherStub tells the issues-table probes and the wisp-plane
// probes apart by the Ephemeral bit each sets on its filter, so a test can put
// a store in the exact bd-7ti state: nothing in the issues table, one matching
// row in the wisps table.
type ephemeralAwareSearcherStub struct {
	wisps []*types.Issue
}

func (s *ephemeralAwareSearcherStub) SearchIssues(_ context.Context, _ string, filter types.IssueFilter) ([]*types.Issue, error) {
	if filter.Ephemeral != nil && *filter.Ephemeral {
		return s.wisps, nil
	}
	return nil, nil
}

// printListNotices reaches the new notice only as a last resort, after the
// issues-table notices (which name rows that actually exist in the table this
// query read) have had their say.
func TestPrintListNoticesFallsBackToWispStatusNotice(t *testing.T) {
	ctx := context.Background()
	listing := statusNarrowedListing(t, "in_progress")

	out := captureStderr(t, func() {
		s := &ephemeralAwareSearcherStub{wisps: []*types.Issue{{ID: "bd-wisp-5aw"}}}
		printListNotices(ctx, s, labelPredicates(), listing, workapi.PinnedNoticeContext{}, 0, "beads")
	})
	if !strings.Contains(out, "no ISSUE has status") {
		t.Errorf("expected the wisp-status fallback to fire:\n%s", out)
	}
}
