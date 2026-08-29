package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/steveyegge/beads/internal/debug"
	"github.com/steveyegge/beads/internal/workapi"
)

// The third structural zero a `bd list` can print.
//
// The first is the wisp plane (list_wisp_notice.go): the record is in a table
// this query cannot reach. The second is the pinned default
// (list_pinned_notice.go): the record matched every predicate the caller typed
// and was dropped by a default they never typed. This one is nearer still — the
// record was dropped by a predicate the caller DID type, and meant differently.
//
// `--status open` matches the status column exactly. Every reader on this repo
// took it for "not closed", including both agents who eventually found the
// difference, and on a busy tracker that is about a third of live work: hooked,
// in_progress, blocked and deferred beads all fail it (bd-j3z). The narrowing
// is worst exactly when it matters most, because which beads are hidden depends
// on how much work is in flight — so a pre-sling "is anyone already on this?"
// check written with `--status open` is blindest at the only moment it is
// asked. Three duplicate dispatches came of it.
//
// Flag help alone does not reach that reader: they have already typed the flag
// and are looking at a plausible screen. The disclosure is a COUNT of the live
// rows their own query dropped, taken under their own filter, and it fires on a
// NON-empty listing too — a short listing hides rows exactly as silently as an
// empty one, and the reader of it has no more way to know.

// statusProbeRowCap bounds the probe's scan. A store with more hidden live rows
// than this reports the cap as a floor rather than as a count, which is why the
// renderer is told the cap too.
const statusProbeRowCap = 5000

// countHiddenByStatus counts the live rows the listing dropped for their
// status, or unknownIssueCount when the store could not be asked. The
// distinction is the one countHiddenPinned and countMatchingWisps draw: a probe
// that never ran must not be reported as a probe that found nothing.
func countHiddenByStatus(ctx context.Context, s workapi.StatusSearcher, listing workapi.StatusNoticeContext) int {
	count, err := listing.CountHidden(ctx, s, statusProbeRowCap)
	if err != nil {
		debug.Logf("[list] could not probe for live rows hidden from a status-filtered listing: %v\n", err)
		return unknownIssueCount
	}
	return count
}

// countHiddenPinnedByStatus counts the live rows the listing dropped for their
// status AND for being pinned — the rows in the seam between this notice and
// the pinned one, which the two probes could not see while each inherited the
// other's term (bd-6xa).
//
// It fails to unknownIssueCount separately from countHiddenByStatus rather than
// sharing its verdict: one probe failing is not evidence about the other, and a
// zero substituted for "could not ask" here would hide exactly the rows this
// count was added to disclose.
func countHiddenPinnedByStatus(ctx context.Context, s workapi.StatusSearcher, listing workapi.StatusNoticeContext) int {
	count, err := listing.CountHiddenPinned(ctx, s, statusProbeRowCap)
	if err != nil {
		debug.Logf("[list] could not probe for pinned live rows hidden from a status-filtered listing: %v\n", err)
		return unknownIssueCount
	}
	return count
}

// hiddenByStatusNoticeLines renders the notice for a listing whose status
// selector hid live work. It returns nil when there is nothing measured to say:
// a listing that was not narrowed, a probe that could not run, or a probe that
// found the listing hid nothing — an ordinary listing, which should stay
// ordinary.
//
// dropped names the statuses that were left out rather than only counting the
// rows, because the count alone does not tell the reader what to type next; the
// remedy differs depending on whether they wanted one more status or all of
// them.
//
// pinnedHidden is counted and reported apart from hidden because the two have
// different remedies, not to make the message longer. The rows it counts were
// dropped TWICE — by the status selector and by the pinned default — so
// `--status live`, the fix the base notice offers, still does not show them
// (bd-6xa). Folding them into one number under one remedy would send the reader
// away believing they had now seen everything, which is the failure this whole
// notice exists to end.
//
// store names the database that was searched, so the count is about a place and
// not about the tracker in general.
func hiddenByStatusNoticeLines(dropped []string, hidden, pinnedHidden, resultCount int, store string) []string {
	if len(dropped) == 0 || (hidden <= 0 && pinnedHidden <= 0) {
		return nil
	}

	// The store is named where one is known and called "the same store"
	// otherwise, so the sentence never implies a place the caller could not
	// have been told.
	where := " in the same store"
	if store != "" {
		where = " in " + store
	}

	explain := fmt.Sprintf("  --status matches the status column exactly; it is not \"not closed\". Not shown: %s.", strings.Join(dropped, ", "))
	const liveFix = "  Fix: re-run with --status live for all work that is not closed, or name the statuses you want."

	// The pinned-only case is a listing whose status selector hid nothing on its
	// own: every row it dropped was pinned as well, so the base remedy would be
	// wrong on its own terms and the whole notice speaks about the doubly
	// hidden rows instead.
	if hidden <= 0 {
		count := statusProbeCount(pinnedHidden)
		headline := fmt.Sprintf("note: %d issue(s) listed, and %s further LIVE issue(s)%s were hidden by --status and the pinned default together.", resultCount, count, where)
		if resultCount == 0 {
			headline = fmt.Sprintf("note: nothing matched your --status, but %s PINNED LIVE issue(s)%s match this same query with another status.", count, where)
		}
		return []string{
			headline,
			explain,
			"  Those rows are pinned as well, and --status live alone will not reveal them.",
			"  Fix: re-run with --status live --pinned.",
		}
	}

	count := statusProbeCount(hidden)
	var headline string
	if resultCount == 0 {
		headline = fmt.Sprintf("note: nothing matched your --status, but %s LIVE issue(s)%s match this same query with another status.", count, where)
	} else {
		headline = fmt.Sprintf("note: %d issue(s) listed, and %s further LIVE issue(s)%s were hidden by --status alone.", resultCount, count, where)
	}
	if pinnedHidden <= 0 {
		return []string{headline, explain, liveFix}
	}
	// "further" because the counts are disjoint: these rows are not among the
	// number in the headline, and a reader who added the two would otherwise be
	// double-counting. The remedy line stays LAST, as it does in the other two
	// notices, and names the selector that reveals BOTH sets rather than
	// leaving the reader to combine two fixes.
	return []string{
		headline,
		explain,
		fmt.Sprintf("  %s further LIVE issue(s) were hidden by --status AND the pinned default, which --status live alone will not reveal.", statusProbeCount(pinnedHidden)),
		"  Fix: re-run with --status live --pinned for both sets, or --status live for the first alone.",
	}
}

// statusProbeCount renders a probe's count. At the cap the scan stopped
// counting, so the number is a floor; saying it flat would be the probe
// overstating what it measured.
func statusProbeCount(n int) string {
	if n >= statusProbeRowCap {
		return fmt.Sprintf("at least %d", n)
	}
	return fmt.Sprintf("%d", n)
}

// printHiddenByStatusNotice emits the status disclosure and reports whether it
// said anything.
//
// Whether a listing owes one is workapi.StatusNoticeContext's question, not
// this file's: which statuses count as live is a rule of the listing query, and
// a frontend that re-derived it from the flags would be a second reading of it.
//
// stderr and --quiet for the reasons the other two notices use them: --json
// output must stay parseable, and every other non-error advisory in this
// package goes to stderr and respects --quiet the same way.
//
// This notice is NOT gated on a label predicate. printHiddenPinnedNotice used
// to be, which left the pinned rows that DID match a caller's status silent on
// a plain `bd list --status open` even as this notice spoke about the doubly
// hidden ones — that asymmetry is what bd-qk2 filed and fixed: the pinned
// notice now fires unlabeled too, for the same reason this one always has.
func printHiddenByStatusNotice(ctx context.Context, s workapi.StatusSearcher, listing workapi.StatusNoticeContext, resultCount int, storeDesc string) bool {
	if isQuiet() || !listing.Applies() {
		return false
	}
	lines := hiddenByStatusNoticeLines(listing.Dropped(), countHiddenByStatus(ctx, s, listing), countHiddenPinnedByStatus(ctx, s, listing), resultCount, storeDesc)
	for _, line := range lines {
		fmt.Fprintln(os.Stderr, line)
	}
	return len(lines) > 0
}

// printListNotices emits every disclosure a listing owes its reader, in the
// order of nearness: the rows this query dropped for a predicate the caller
// typed first, the rows it dropped for a default they did not type second, the
// other table last.
//
// The status notice SUPPRESSES the wisp one when it fires, for the same reason
// the pinned notice does: the wisp notice's headline is "no ISSUE carries
// <label>", which a hidden live match makes false, and naming the wisp plane
// would send a reader looking in the wrong place for issues that are in the
// table already read.
//
// It does NOT suppress the pinned notice. The populations are disjoint by
// construction — the status probe inherits the pinned exclusion, and the pinned
// probe inherits the caller's status term — so both counts are true, both are
// about rows this listing hid, and they have different remedies.
//
// That construction is what left a row hidden by BOTH terms outside both counts
// (bd-6xa): each probe stops exactly where the other begins, and nothing stood
// in the seam. The intersection is now counted by the status notice's own
// second probe and reported on its own line, with its own remedy, so the three
// counts partition the hidden rows rather than merely failing to overlap.
// Disjoint was never the property worth having; exhaustive was.
func printListNotices(ctx context.Context, s workapi.WispSearcher, p listLabelPredicates, status workapi.StatusNoticeContext, pinned workapi.PinnedNoticeContext, resultCount int, storeDesc string) {
	if printHiddenByStatusNotice(ctx, s, status, resultCount, storeDesc) {
		printHiddenPinnedNotice(ctx, s, p, pinned, resultCount, storeDesc)
		return
	}
	printLabelledListNotices(ctx, s, p, pinned, resultCount, storeDesc)
}
