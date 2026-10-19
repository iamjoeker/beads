package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	storageissueops "github.com/steveyegge/beads/internal/storage/issueops"
	"github.com/steveyegge/beads/internal/types"
)

// The notes write path is where this repo's confirmed data loss came from, and
// every loss was reported as a success (bd-2mx): a substantive investigation
// note destroyed by a routine `bd update --notes`, and a scratch bead left
// holding only the fourth of four writes. Both callers saw "✓ Updated issue"
// and exit 0.
//
// The guard that existed for this was a warning printed to STDERR *after* the
// write had already committed, while the success line went to STDOUT and the
// process exited 0 — so both mechanisms a careful caller relies on (checking
// the exit status, discarding stderr) reported success over destroyed data. A
// warning that arrives after the data is gone is a log line, not a guardrail.
//
// The refusals below run BEFORE the mutation and surface as ordinary per-ID
// failures, so the destructive write does not happen at all unless the caller
// named it with --replace-notes. warnNotesReplacement survives for the write
// the caller did ask for.

// errEmptyAppendNotes refuses `--append-notes ""`.
//
// Appending nothing is never an intended edit — it cannot change the row — so
// an empty value here is the signature of an argument that did not survive the
// shell: `--append-notes "$(cat missing)"` and an unset `"$VAR"` both arrive as
// "". Before this refusal that combination printed "✓ Updated issue" and exited
// 0 having written nothing at all, which is the worse half of bd-2mx: the flag
// recommended as the SAFE alternative to --notes had its own silent failure
// mode, so the operator who took the advice believed they had recorded
// something and had not.
//
// Whitespace is trimmed for the test because a value that is only a newline
// carries no more than "" does, and `$(...)`-style expansions routinely leave
// one behind.
func errEmptyAppendNotes(text string) error {
	if strings.TrimSpace(text) != "" {
		return nil
	}
	return fmt.Errorf("--append-notes was given an empty value, which would append nothing and change nothing; " +
		"if this came from a shell expansion, the argument did not survive it (an unset variable or a failed " +
		"command substitution both arrive as \"\"). To clear the notes field instead, pass --notes \"\" --replace-notes")
}

// notesIntent is what the caller asked the notes field to become, carried from
// flag parsing to the post-write check so both routes of `bd update` verify the
// same post-condition.
type notesIntent struct {
	// replace is the value of a --notes write; replaceSet distinguishes
	// `--notes ""` (clear the field) from --notes never being passed.
	replace    string
	replaceSet bool
	// appended is the value of an --append-notes write. Empty means the flag
	// was not passed: errEmptyAppendNotes has already refused the only other
	// way to get here with an empty value.
	appended string
}

// notesIntentFromUpdates reads the caller's notes edit back out of the
// flag-derived update map, which is the one representation both routes build.
func notesIntentFromUpdates(updates map[string]any) notesIntent {
	var intent notesIntent
	if v, ok := updates["notes"].(string); ok {
		intent.replace, intent.replaceSet = v, true
	}
	if v, ok := updates[storageissueops.OpAppendNotes].(string); ok {
		intent.appended = v
	}
	return intent
}

// errNotesWriteNotLanded compares the mutation's own post-state row against
// what the caller asked to write, and reports a failure when the notes edit is
// not in it.
//
// This is the half of bd-2mx that the pre-write refusals cannot cover: the
// refusals stop a write the caller did not mean, but they say nothing about
// whether a write the caller DID mean actually landed. `bd update` printed its
// success line from the fact that the update call returned, never from the
// state it returned — so an edit dropped between the flag and the row (a key
// the field allowlist does not recognize, a no-op filter that elides it)
// reported "✓ Updated issue" and exit 0.
//
// The post-state here is a genuine re-read of the row inside the mutation's own
// transaction (ExecuteUpdate hydrates it after applying the patch), not the
// prepared values echoed back, so it can disagree with the request. It is still
// the command's own report of itself and so cannot catch a whole store being
// the wrong one; it catches the edit that silently did not apply, which is what
// was actually observed.
//
// A nil post-state means the caller had nothing to check against, not that the
// write is confirmed; it returns nil, and the caller's own "no issue returned"
// handling is what reports that case.
func errNotesWriteNotLanded(intent notesIntent, after *types.Issue) error {
	if after == nil {
		return nil
	}
	if intent.replaceSet && after.Notes != intent.replace {
		return fmt.Errorf("--notes did not land: the stored notes do not match the value written "+
			"(wrote %d characters, stored row has %d)", len(intent.replace), len(after.Notes))
	}
	if intent.appended != "" && !strings.Contains(after.Notes, intent.appended) {
		return fmt.Errorf("--append-notes did not land: the appended text is absent from the stored "+
			"notes (appended %d characters, stored row has %d)", len(intent.appended), len(after.Notes))
	}
	return nil
}

// validateNotesUpdate is the GH#6021 guard: `--notes ""` erases the whole
// notes field, and the empty string is exactly what a dead command
// substitution (`--notes "$(cat missing.txt)"`) collapses to — at the flag
// layer the accident and the deliberate clear are the same bytes, so the
// empty value is refused outright and the deliberate clear gets its own verb,
// --clear-notes. That differs from the description guard twice over:
// validateDescriptionUpdate guards only stdin/file input (where emptiness is
// an input-plumbing property and --allow-empty-description unblocks the same
// content flag), while notes has no file/stdin variant — inline is the only
// path, clearing already-empty notes is a no-op, and what needs authorizing
// is an intent, not a pipe. The notes-overwrite fence
// (issueops.NotesReplacement) exempts clears by design and --force does not
// reach here. Called only when --notes was explicitly passed.
func validateNotesUpdate(notes string) error {
	if notes != "" {
		return nil
	}
	return fmt.Errorf(`--notes "" would clear the notes field (an empty command substitution is the usual accident); use --clear-notes to clear it deliberately`)
}

// clearNotesRequested reports whether --clear-notes asked for the deliberate
// erase. Combining it with --notes or --append-notes is contradictory and
// refused by cobra's mutual-exclusion registration on the update command.
func clearNotesRequested(cmd *cobra.Command) bool {
	clear, _ := cmd.Flags().GetBool("clear-notes")
	return clear
}
