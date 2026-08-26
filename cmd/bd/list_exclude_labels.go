package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/steveyegge/beads/internal/config"
	"github.com/steveyegge/beads/internal/utils"
)

// A DEFAULT LISTING SHOULD BE A LISTING OF WORK.
//
// The three work-queue listings — `bd list`, `bd ready`, `bd blocked` — have
// always taken --exclude-label, and nothing ever set it. On a store that keeps
// records other than work as ordinary labeled beads (Gas Town writes agent
// mail that way, deliberately, so it survives the recipient's session death)
// the default listing is mostly not work and its size carries no signal:
// 608 open rows, 250 of them once "gt:message" was excluded.
//
// config.ListExcludeLabelsKey supplies the missing default. It changes nothing
// for a workspace that does not set it, and everything about it is stated at
// the point of use: the labels are named in a notice on every affected
// invocation, and --include-hidden turns it off for one command.
//
// WHY THESE THREE COMMANDS AND NO OTHERS. The default is applied exactly where
// the flag it defaults already exists, so it can always be overridden by the
// caller and never silently narrows a query that had no way to widen it back:
//
//   - `bd count` has no --exclude-label at all. Honoring the key there would
//     produce a hidden count with no opt-out; the flag has to come first
//     (bd-1v3, which also records what the count/list parity contract in
//     backend/conformance/counter_contract.go needs from that change).
//   - `bd stale` and `bd reclaim` take the flag, but reclaim MUTATES — a
//     configured default that silently changes which leases a sweep reverts is
//     a different and worse proposition than one that changes what a listing
//     shows.
//
// So `bd count` and `bd list` can disagree on a store that sets the key. That
// is a real asymmetry, stated here rather than discovered.

// includeHiddenFlag is the counterpart flag: one flag away from the unfiltered
// listing, on every command that applies the default.
const includeHiddenFlag = "include-hidden"

// registerIncludeHiddenFlag adds --include-hidden to a work-queue listing. It
// is registered on every command that calls resolveExcludeLabels and nowhere
// else: a flag that a command accepts and ignores is the silent no-op this
// package refuses everywhere else.
func registerIncludeHiddenFlag(cmd *cobra.Command) {
	cmd.Flags().Bool(includeHiddenFlag, false,
		"Include rows hidden by the "+config.ListExcludeLabelsKey+" config (normally excluded)")
}

// resolveExcludeLabels layers the configured exclusions under the caller's
// --exclude-label values and returns the set the query should use, normalized.
//
// THE TWO ARE UNIONED, NOT OVERRIDDEN. --exclude-label narrows a listing;
// reading it as "replace the configured exclusions" would mean
// `bd list --exclude-label wontfix` silently brought the mail back, so asking
// for less would return more. --include-hidden is the only way to widen, and it
// drops the configured set whole.
//
// It also emits the notice, once, from the one place both routes of every
// affected command pass through.
func resolveExcludeLabels(cmd *cobra.Command, requested []string) []string {
	requested = utils.NormalizeLabels(requested)

	includeHidden, _ := cmd.Flags().GetBool(includeHiddenFlag)
	if includeHidden {
		return requested
	}

	configured := utils.NormalizeLabels(config.GetListExcludeLabels())
	if len(configured) == 0 {
		return requested
	}

	merged, added := unionExcludeLabels(requested, configured)
	if len(added) > 0 {
		printExcludeLabelsNotice(added)
	}
	return merged
}

// unionExcludeLabels returns the caller's exclusions followed by the configured
// ones it did not already carry, plus that second group on its own. The caller's
// order is preserved so an error message or a --json echo still reads back the
// way it was typed, and the added set is returned separately so the notice names
// only what the configuration contributed.
func unionExcludeLabels(requested, configured []string) (merged, added []string) {
	seen := make(map[string]bool, len(requested)+len(configured))
	merged = make([]string, 0, len(requested)+len(configured))
	for _, label := range requested {
		if seen[label] {
			continue
		}
		seen[label] = true
		merged = append(merged, label)
	}
	for _, label := range configured {
		if seen[label] {
			continue
		}
		seen[label] = true
		merged = append(merged, label)
		added = append(added, label)
	}
	return merged, added
}

// excludeLabelsNoticeLine is what a caller is told when configuration narrowed
// the listing they asked for.
//
// IT DESCRIBES THE FILTER, NOT THE OUTCOME. No count is taken, so the sentence
// claims none: saying "N rows hidden" would require a second query on the most-
// run command in the tree, and saying "rows were hidden" when none carried the
// label would be an unmeasured claim. What it does say is falsifiable in one
// step — the labels, the key that named them, and the flag that turns it off.
func excludeLabelsNoticeLine(added []string) string {
	return fmt.Sprintf("note: excluding rows labeled %s (%s); --%s to include them",
		strings.Join(quotedLabels(added), ", "), config.ListExcludeLabelsKey, includeHiddenFlag)
}

// printExcludeLabelsNotice writes the notice to stderr.
//
// stderr, not stdout: --json output must stay parseable, which is where the
// notice matters most — an agent counting rows out of `bd list --json` is
// exactly the consumer this default exists for. Suppressed under --quiet, as
// every other advisory in this package is.
func printExcludeLabelsNotice(added []string) {
	if isQuiet() {
		return
	}
	fmt.Fprintln(os.Stderr, excludeLabelsNoticeLine(added))
}
