package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/steveyegge/beads/internal/debug"
	"github.com/steveyegge/beads/internal/workapi"
)

// The fourth structural zero a `bd list` can print, and the one bd-7ti filed:
// a bare `--status` selector with no label predicate at all.
//
// list_wisp_notice.go already disclosed the wisp plane for a LABELED zero —
// `bd list --label gt:merge-request` — but that probe deliberately drops
// status, and printListNotices only reaches it when no label predicate was
// typed to begin with. A plain `bd list --status=in_progress` therefore had no
// probe watching the one table where every merge-request wisp actually lives:
// the issues-table zero looked ordinary, `bd list` printed "No issues found.",
// and the Idle Town Protocol probe read a live merge as a quiet town (bd-7ti).
//
// The fix mirrors the labeled notice exactly, with status in place of label:
// when a status-selecting listing returns nothing and the other status
// disclosures (list_status_notice.go, list_pinned_notice.go) had nothing to
// add, the same status filter is re-run against the wisps table, and this
// notice reports what it found there.

// countMatchingStatusWisps counts the wisps carrying one of a listing's
// selected statuses, or unknownIssueCount when the store could not be asked.
func countMatchingStatusWisps(ctx context.Context, s workapi.WispSearcher, status workapi.StatusNoticeContext) int {
	count, err := status.CountMatchingWisps(ctx, s, wispListRowCap)
	if err != nil {
		debug.Logf("[list] could not probe the wisps table for a status-filtered zero: %v\n", err)
		return unknownIssueCount
	}
	return count
}

// emptyStatusListNoticeLines renders the notice for a status-filtered `bd
// list` that returned nothing. It returns nil when there is nothing worth
// saying: no status selector, or a status selector no wisp matches — an
// ordinary empty result, which should stay ordinary.
func emptyStatusListNoticeLines(selected []string, wispCount int, store string) []string {
	if len(selected) == 0 || wispCount <= 0 {
		return nil
	}
	joined := strings.Join(quotedLabels(selected), ", ")

	where := " in the same store"
	if store != "" {
		where = " in " + store
	}

	return []string{
		fmt.Sprintf("note: no ISSUE has status %s, but %d WISP(s)%s do.", joined, wispCount, where),
		"  bd list queries the issues table; wisps live in the wisps table, so no bd list filter can ever return one.",
		"  Fix: bd mol wisp list --all --all-stores",
	}
}

// printEmptyStatusListNotice emits the notice for an empty status-filtered
// listing and reports whether it said anything.
//
// It is checked only after the two notices that already ask about a
// status-filtered zero in the issues table (list_status_notice.go's own
// disclosure, and the pinned one it can trigger): both name rows that
// actually exist in the table this query read, which is a nearer and more
// actionable answer than "try the other table" whenever it applies.
func printEmptyStatusListNotice(ctx context.Context, s workapi.WispSearcher, status workapi.StatusNoticeContext, resultCount int, storeDesc string) bool {
	if resultCount > 0 || isQuiet() {
		return false
	}
	selected := status.Selected()
	if len(selected) == 0 {
		return false
	}
	count := countMatchingStatusWisps(ctx, s, status)
	lines := emptyStatusListNoticeLines(selected, count, storeDesc)
	for _, line := range lines {
		fmt.Fprintln(os.Stderr, line)
	}
	return len(lines) > 0
}
