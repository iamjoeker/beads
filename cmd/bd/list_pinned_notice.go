package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/steveyegge/beads/internal/debug"
	"github.com/steveyegge/beads/internal/workapi"
)

// The second structural zero a `bd list` can print.
//
// The first is the wisp plane (list_wisp_notice.go): the record is in a table
// this query cannot reach. This one is nearer — the record is in the table the
// query DID read, matched every predicate the caller typed, and was dropped by
// a default the caller never typed and the output never mentions. `bd list`
// excludes pinned rows unless asked (internal/workapi/list.go), so a label
// whose matches are all pinned prints the same empty screen as a label nothing
// carries.
//
// Measured on the hq store while bd-f76 was filed: `bd list --label
// gt:escalation` returned 1 row where the store held 4 open matches, the other
// 3 being pinned. An escalation surface reported "No escalations found" over
// three live escalations, and a monitoring loop read that zero as evidence for
// continuing.
//
// The disclosure is a COUNT of the dropped rows, taken under the caller's own
// filter, rather than advice to go looking. It also fires on a NON-empty
// listing: 1-of-4 is the same silence as 0-of-3, and the reader of a short
// listing has no more way to know rows were withheld than the reader of an
// empty one.

// pinnedProbeRowCap bounds the probe's scan. A store with more hidden pinned
// rows than this reports the cap as a floor rather than as a count, which is
// why the renderer is told the cap too.
const pinnedProbeRowCap = 5000

// countHiddenPinned counts the rows the listing dropped to the pinned default,
// or unknownIssueCount when the store could not be asked. The distinction is
// the same one countMatchingWisps draws: a probe that never ran must not be
// reported as a probe that found nothing.
func countHiddenPinned(ctx context.Context, s workapi.PinnedSearcher, listing workapi.PinnedNoticeContext) int {
	count, err := listing.CountHidden(ctx, s, pinnedProbeRowCap)
	if err != nil {
		debug.Logf("[list] could not probe for pinned rows hidden from a labeled listing: %v\n", err)
		return unknownIssueCount
	}
	return count
}

// hiddenPinnedNoticeLines renders the notice for a labeled listing that hid
// pinned matches. It returns nil when there is nothing measured to say: no
// label filter, a probe that could not run, or a probe that found the listing
// hid nothing — an ordinary listing, which should stay ordinary.
//
// store names the database that was searched, so the count is about a place and
// not about the tracker in general.
func hiddenPinnedNoticeLines(labels []string, hidden, resultCount int, store string) []string {
	if len(labels) == 0 || hidden <= 0 {
		return nil
	}
	joined := strings.Join(quotedLabels(labels), ", ")

	// The store is named where one is known and called "the same store"
	// otherwise, so the sentence never implies a place the caller could not
	// have been told.
	where := " in the same store"
	if store != "" {
		where = " in " + store
	}

	// At the cap the scan stopped counting, so the number is a floor. Saying it
	// flat would be the probe overstating what it measured.
	count := fmt.Sprintf("%d", hidden)
	if hidden >= pinnedProbeRowCap {
		count = fmt.Sprintf("at least %d", hidden)
	}

	var headline string
	if resultCount == 0 {
		headline = fmt.Sprintf("note: no listed issue carries %s, but %s PINNED issue(s)%s match this same query.", joined, count, where)
	} else {
		headline = fmt.Sprintf("note: %d issue(s) listed, and %s further PINNED issue(s) carrying %s%s were hidden.", resultCount, count, joined, where)
	}
	return []string{
		headline,
		"  bd list hides pinned rows by default; they are permanent reference beads.",
		"  Fix: re-run with --pinned for those rows alone, or --all for both.",
	}
}

// printHiddenPinnedNotice emits the pinned disclosure and reports whether it
// said anything.
//
// Whether a listing owes one is workapi.PinnedNoticeContext's question, not
// this file's: the exclusion is a rule of the listing query, and a frontend
// that re-derived it from the flags would be a second reading of it.
//
// stderr and --quiet for the reasons printEmptyLabelledListNotice uses them:
// --json output must stay parseable, and every other non-error advisory in this
// package goes to stderr and respects --quiet the same way.
func printHiddenPinnedNotice(ctx context.Context, s workapi.PinnedSearcher, p listLabelPredicates, listing workapi.PinnedNoticeContext, resultCount int, storeDesc string) bool {
	if isQuiet() || !listing.Applies() {
		return false
	}
	labels, ok := p.terms()
	if !ok {
		return false
	}
	lines := hiddenPinnedNoticeLines(labels, countHiddenPinned(ctx, s, listing), resultCount, storeDesc)
	for _, line := range lines {
		fmt.Fprintln(os.Stderr, line)
	}
	return len(lines) > 0
}

// printLabelledListNotices emits the disclosures a labeled listing owes its
// reader, in the order of nearness: the rows this very query matched and hid
// first, the other table second.
//
// The pinned notice SUPPRESSES the wisp one when it fires, and not merely to
// keep the output short. The wisp notice's own headline is "no ISSUE carries
// <label>", which a hidden pinned match makes false — the issues exist, in the
// table that was read, and naming the wisp plane would send a reader looking
// for them in the wrong place.
func printLabelledListNotices(ctx context.Context, s workapi.WispSearcher, p listLabelPredicates, listing workapi.PinnedNoticeContext, resultCount int, storeDesc string) {
	if printHiddenPinnedNotice(ctx, s, p, listing, resultCount, storeDesc) {
		return
	}
	printEmptyLabelledListNotice(ctx, s, p, resultCount, storeDesc)
}
