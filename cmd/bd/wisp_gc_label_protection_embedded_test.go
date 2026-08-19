//go:build cgo

package main

import (
	"os"
	"testing"
)

// These run the SHIPPED BINARY against a real database, which is the only
// place three things can be checked at once: that the label survives the
// create path, that it is hydrated onto the rows the sweep reads, and that the
// sweep the operator actually types holds the bead back. A guard can be
// correct in the filter and inert in the command if the labels never reach it
// — the shape of the defect these cases exist for (bd-czf, bd-6jp).
//
// EVERY CASE PAIRS A PROTECTED BEAD WITH AN UNPROTECTED ONE created the same
// way in the same run. The control being GONE is what proves the sweep ran;
// without it a command that silently did nothing would pass.

// TestWispGCClosedForceKeepsMergeRequestRecords is bd-czf itself:
// `bd mol wisp gc --closed --force` against a merge-request wisp that was
// CLOSED WITHOUT MERGING. The closed purge has no age window, so before this
// protection the record died on the next patrol cycle of any agent in the rig.
func TestWispGCClosedForceKeepsMergeRequestRecords(t *testing.T) {
	if os.Getenv("BEADS_TEST_EMBEDDED_DOLT") != "1" {
		t.Skip("set BEADS_TEST_EMBEDDED_DOLT=1 to run embedded dolt integration tests")
	}
	t.Parallel()

	bd := buildEmbeddedBD(t)
	dir, _, _ := bdInit(t, bd, "--prefix", "gcl")

	control := bdCreate(t, bd, dir, "finished patrol step", "--ephemeral").ID
	mergeRequest := bdCreate(t, bd, dir, "Merge: gcl-1", "--ephemeral", "--labels", "gt:merge-request").ID
	mail := bdCreate(t, bd, dir, "notification", "--ephemeral", "--labels", "gt:message,read").ID

	for _, id := range []string{control, mergeRequest, mail} {
		bdClose(t, bd, dir, id)
	}

	bdCommand(t, bd, dir, "mol", "wisp", "gc", "--closed", "--force")

	// The control is gone: the purge really ran.
	bdShowFail(t, bd, dir, control)

	for _, id := range []string{mergeRequest, mail} {
		if bdShowSucceeds(t, bd, dir, id) {
			continue
		}
		t.Errorf("label-protected wisp %s was purged by `wisp gc --closed --force`; "+
			"closed does not mean done for a merge-request or message record", id)
	}
}

// TestWispGCAgeKeepsMergeRequestRecords covers the other sweep. An OPEN merge
// request older than the age threshold sat in the abandoned set with the stale
// patrol steps beside it.
func TestWispGCAgeKeepsMergeRequestRecords(t *testing.T) {
	if os.Getenv("BEADS_TEST_EMBEDDED_DOLT") != "1" {
		t.Skip("set BEADS_TEST_EMBEDDED_DOLT=1 to run embedded dolt integration tests")
	}
	t.Parallel()

	bd := buildEmbeddedBD(t)
	dir, _, _ := bdInit(t, bd, "--prefix", "gca")

	control := bdCreate(t, bd, dir, "abandoned step", "--ephemeral").ID
	mergeRequest := bdCreate(t, bd, dir, "Merge: gca-1", "--ephemeral", "--labels", "gt:merge-request").ID

	// --age 0 makes every wisp old enough, so the sweep's own selection is not
	// what this case depends on.
	bdCommand(t, bd, dir, "mol", "wisp", "gc", "--age", "0")

	bdShowFail(t, bd, dir, control)
	if !bdShowSucceeds(t, bd, dir, mergeRequest) {
		t.Errorf("label-protected wisp %s was reclaimed by the age sweep", mergeRequest)
	}
}

// TestPurgeKeepsMergeRequestRecords covers the OTHER front door onto the same
// rows. `bd purge` sweeps closed ephemeral beads through issueops.Sweeper —
// a different code path from `bd mol wisp gc` — so a fix in one of them would
// leave the record destructible through the other while reading as fixed.
func TestPurgeKeepsMergeRequestRecords(t *testing.T) {
	if os.Getenv("BEADS_TEST_EMBEDDED_DOLT") != "1" {
		t.Skip("set BEADS_TEST_EMBEDDED_DOLT=1 to run embedded dolt integration tests")
	}
	t.Parallel()

	bd := buildEmbeddedBD(t)
	dir, _, _ := bdInit(t, bd, "--prefix", "gcp")

	control := bdCreate(t, bd, dir, "finished step", "--ephemeral").ID
	mergeRequest := bdCreate(t, bd, dir, "Merge: gcp-1", "--ephemeral", "--labels", "gt:merge-request").ID
	for _, id := range []string{control, mergeRequest} {
		bdClose(t, bd, dir, id)
	}

	bdCommand(t, bd, dir, "purge", "--force")

	bdShowFail(t, bd, dir, control)
	if !bdShowSucceeds(t, bd, dir, mergeRequest) {
		t.Errorf("label-protected wisp %s was deleted by `bd purge --force`", mergeRequest)
	}
}

// TestWispGCClosedForceHonorsConfiguredLabels proves the mechanism belongs to
// the WORKSPACE rather than to beads' own vocabulary: a configured list
// replaces the defaults, on the shipped binary.
func TestWispGCClosedForceHonorsConfiguredLabels(t *testing.T) {
	if os.Getenv("BEADS_TEST_EMBEDDED_DOLT") != "1" {
		t.Skip("set BEADS_TEST_EMBEDDED_DOLT=1 to run embedded dolt integration tests")
	}
	t.Parallel()

	bd := buildEmbeddedBD(t)
	dir, _, _ := bdInit(t, bd, "--prefix", "gcc")

	bdCommand(t, bd, dir, "config", "set", "gc.protected_labels", "ops:receipt")

	defaulted := bdCreate(t, bd, dir, "Merge: gcc-1", "--ephemeral", "--labels", "gt:merge-request").ID
	configured := bdCreate(t, bd, dir, "backup receipt", "--ephemeral", "--labels", "ops:receipt").ID
	for _, id := range []string{defaulted, configured} {
		bdClose(t, bd, dir, id)
	}

	bdCommand(t, bd, dir, "mol", "wisp", "gc", "--closed", "--force")

	if !bdShowSucceeds(t, bd, dir, configured) {
		t.Errorf("wisp %s carrying the CONFIGURED label was purged", configured)
	}
	// The built-in default is not additive: naming a list replaces it, which
	// is the same layering types.infra uses.
	bdShowFail(t, bd, dir, defaulted)
}
