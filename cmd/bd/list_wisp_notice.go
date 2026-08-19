package main

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/steveyegge/beads/internal/debug"
	"github.com/steveyegge/beads/internal/workapi"
)

// A zero from `bd list` can be STRUCTURAL rather than empirical.
//
// Wisps live in the wisps table; `bd list` queries the issues table. They are
// different tables, so NO filter on `bd list` can ever return a wisp — and the
// records agents most often go looking for by label (merge requests, mail) are
// wisps. `bd list --label gt:merge-request --status all` therefore prints "No
// issues found." for records that demonstrably exist, in the same words it uses
// for a label nothing carries. Read as an ordinary empty result it says the
// records are gone, and acting on that — restoring, force-cleaning, rebuilding
// a store — is what costs real data (bd-nc4).
//
// The answer here is a MEASURED one rather than a lecture: when a labeled
// listing comes back empty, the same label filter is re-run against the wisps
// table of the same store, and the notice reports what it found there. A count
// is falsifiable; "maybe try wisps" is not.

// wispOnlyLabels are labels this workspace only ever puts on ephemeral records,
// so a `bd list` zero for one of them is structural: the issues table cannot
// hold such a row at all, and the empty result is not evidence about whether
// the records exist.
//
// It is deliberately NOT workapi.DefaultGCProtectedLabels(), whose current
// membership happens to coincide: that set answers "what may a bulk delete
// never take", this one answers "which labels never appear on a permanent
// issue". Sharing a variable would couple two questions that are free to
// diverge.
var wispOnlyLabels = []string{"gt:merge-request", "gt:message"}

// wispOnlyLabelsAmong returns the requested labels that are wisp-only, sorted
// and deduplicated, so the notice can name exactly which part of the filter
// could never have matched an issue.
func wispOnlyLabelsAmong(requested ...[]string) []string {
	known := make(map[string]bool, len(wispOnlyLabels))
	for _, label := range wispOnlyLabels {
		known[normalizeListLabel(label)] = true
	}
	seen := make(map[string]bool)
	var out []string
	for _, group := range requested {
		for _, label := range group {
			normalized := normalizeListLabel(label)
			if known[normalized] && !seen[normalized] {
				seen[normalized] = true
				out = append(out, normalized)
			}
		}
	}
	sort.Strings(out)
	return out
}

func normalizeListLabel(label string) string {
	return strings.ToLower(strings.TrimSpace(label))
}

// listLabelPredicates is the label part of a listing, in the shape both the
// probe and the message need. The filter that carried them is not named here:
// the terms come from workapi, which owns what a listing's label predicates
// are, and the probe filter itself is built there too.
type listLabelPredicates struct {
	Labels    []string
	LabelsAny []string
	Pattern   string
	Regex     string
}

// terms lists what the query looked for, and whether it looked for anything at
// all.
func (p listLabelPredicates) terms() ([]string, bool) {
	described := workapi.LabelPredicates(p.Labels, p.LabelsAny, p.Pattern, p.Regex)
	return described, workapi.HasLabelPredicate(p.Labels, p.LabelsAny, p.Pattern, p.Regex)
}

// countMatchingWisps counts the wisps in s that carry the same labels, or
// unknownIssueCount when the store could not be asked. The distinction matters:
// a probe that never ran must not be reported as a probe that found nothing.
func countMatchingWisps(ctx context.Context, s workapi.WispSearcher, p listLabelPredicates) int {
	count, err := workapi.CountLabelledWisps(ctx, s, p.Labels, p.LabelsAny, p.Pattern, p.Regex, wispListRowCap)
	if err != nil {
		debug.Logf("[list] could not probe the wisps table for a labeled zero: %v\n", err)
		return unknownIssueCount
	}
	return count
}

// emptyLabelledListNoticeLines renders the notice for a labeled `bd list` that
// returned nothing. It returns nil when there is nothing worth saying: no label
// filter, a non-empty result, or a label filter that is neither wisp-only nor
// matched by any wisp — an ordinary empty result, which should stay ordinary.
//
// store names the database that was searched, so the notice is about a place
// and not about the tracker in general.
func emptyLabelledListNoticeLines(labels []string, wispOnly []string, wispCount int, store string) []string {
	if len(labels) == 0 {
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

	switch {
	case wispCount > 0:
		return []string{
			fmt.Sprintf("note: no ISSUE carries %s, but %d WISP(s)%s do.", joined, wispCount, where),
			"  bd list queries the issues table; wisps live in the wisps table, so no bd list filter can ever return one.",
			"  Fix: bd mol wisp list --all --all-stores",
		}
	case len(wispOnly) > 0 && wispCount == 0:
		return []string{
			fmt.Sprintf("note: %s is carried only by wisps, never by issues — this zero is structural: bd list queries the issues table and can never return such a record.",
				strings.Join(quotedLabels(wispOnly), ", ")),
			fmt.Sprintf("  The wisps%s carry none either (0 matched), and wisps are per-store, so this says nothing about the other stores.", where),
			"  Fix: bd mol wisp list --all --all-stores",
		}
	case len(wispOnly) > 0:
		// The probe did not run. Say the structural half, which is true
		// without it, and do not imply a count was taken.
		return []string{
			fmt.Sprintf("note: %s is carried only by wisps, never by issues, so this zero is structural — bd list queries the issues table and can never return such a record.",
				strings.Join(quotedLabels(wispOnly), ", ")),
			"  The wisps table was not consulted here.",
			"  Fix: bd mol wisp list --all --all-stores",
		}
	default:
		return nil
	}
}

func quotedLabels(labels []string) []string {
	out := make([]string, 0, len(labels))
	for _, label := range labels {
		out = append(out, fmt.Sprintf("%q", label))
	}
	return out
}

// printEmptyLabelledListNotice emits the notice for an empty labeled listing.
//
// storeDesc names the database that actually answered — the ROUTED store when
// auto-routing swapped it, not the local one. Naming a store the command did
// not read would be one more instrument lying about its scope.
//
// stderr, not stdout: --json output must stay parseable, and every other
// non-error advisory in this package (the routing notice, tips) goes to stderr
// and respects --quiet the same way.
func printEmptyLabelledListNotice(ctx context.Context, s workapi.WispSearcher, p listLabelPredicates, resultCount int, storeDesc string) {
	if resultCount > 0 || isQuiet() {
		return
	}
	labels, ok := p.terms()
	if !ok {
		return
	}
	wispOnly := wispOnlyLabelsAmong(p.Labels, p.LabelsAny)
	count := countMatchingWisps(ctx, s, p)
	lines := emptyLabelledListNoticeLines(labels, wispOnly, count, storeDesc)
	for _, line := range lines {
		fmt.Fprintln(os.Stderr, line)
	}
}

// describeLocalSearchedStore names the database this command opened locally, in
// the same form the routing and not-found messages use.
func describeLocalSearchedStore() string {
	beadsDir := resolveCommandBeadsDir(dbPath)
	if beadsDir == "" {
		return ""
	}
	return describeDatabaseAt(beadsDir)
}
