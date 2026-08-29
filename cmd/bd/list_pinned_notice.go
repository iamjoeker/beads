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

// hiddenPinnedNoticeLines renders the notice for a listing that hid pinned
// matches. It fires whether or not the listing carried a label — the pinned
// default excludes those rows from every `bd list`, labeled or not, so a
// notice gated on a label predicate would stay silent on the commonest
// invocation on this rig (bd-qk2). It returns nil when there is nothing
// measured to say: a probe that could not run, or a probe that found the
// listing hid nothing — an ordinary listing, which should stay ordinary.
//
// store names the database that was searched, so the count is about a place and
// not about the tracker in general.
func hiddenPinnedNoticeLines(labels []string, hidden, resultCount int, store string) []string {
	if hidden <= 0 {
		return nil
	}

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

	// carrying is the label clause used when the listing hid rows it still
	// found something to say about, present only when the listing actually
	// filtered by label — an unlabeled listing has no label to name, and
	// saying "carrying" nothing would be a clause about a filter that was
	// never typed.
	var carrying string
	if len(labels) > 0 {
		carrying = " carrying " + strings.Join(quotedLabels(labels), ", ")
	}

	var headline string
	switch {
	case resultCount == 0 && len(labels) > 0:
		joined := strings.Join(quotedLabels(labels), ", ")
		headline = fmt.Sprintf("note: no listed issue carries %s, but %s PINNED issue(s)%s match this same query.", joined, count, where)
	case resultCount == 0:
		headline = fmt.Sprintf("note: nothing was listed, but %s PINNED issue(s)%s match this same query.", count, where)
	default:
		headline = fmt.Sprintf("note: %d issue(s) listed, and %s further PINNED issue(s)%s%s were hidden.", resultCount, count, carrying, where)
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
// that re-derived it from the flags would be a second reading of it. It is NOT
// gated on a label predicate — the pinned default hides rows from every
// `bd list`, labeled or not, and gating the disclosure on a label left the
// commonest invocation on this rig, a bare `bd list --status open`, disclosing
// nothing about the pinned rows it dropped (bd-qk2). labels is used only for
// wording: present, it names what the hidden rows carry; empty, the notice
// still fires, worded without a label clause.
//
// stderr and --quiet for the reasons printEmptyLabelledListNotice uses them:
// --json output must stay parseable, and every other non-error advisory in this
// package goes to stderr and respects --quiet the same way.
func printHiddenPinnedNotice(ctx context.Context, s workapi.PinnedSearcher, p listLabelPredicates, listing workapi.PinnedNoticeContext, resultCount int, storeDesc string) bool {
	if isQuiet() || !listing.Applies() {
		return false
	}
	labels, _ := p.terms()
	lines := hiddenPinnedNoticeLines(labels, countHiddenPinned(ctx, s, listing), resultCount, storeDesc)
	for _, line := range lines {
		fmt.Fprintln(os.Stderr, line)
	}
	return len(lines) > 0
}

// printLabelledListNotices emits the disclosures a listing owes its reader
// beyond the status one, in the order of nearness: the rows this very query
// matched and hid first, the other table second. Its name predates the pinned
// half firing on an unlabeled listing too (bd-qk2); only the wisp fallback it
// calls is actually gated on a label predicate.
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
