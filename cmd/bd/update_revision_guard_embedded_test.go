//go:build cgo

package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

// bdShowRevision runs "bd show <id> --json" and returns the "revision" field.
// types.Issue.RowVersion is json:"-" (see internal/types/types.go), so bdShow's
// generic *types.Issue unmarshal silently drops it; this reads it directly the
// way a caller building an --if-revision guard would. "bd show --json" wraps
// the result in an array even for a single id, so this unwraps element 0.
func bdShowRevision(t *testing.T, bd, dir, id string) int64 {
	t.Helper()
	cmd := exec.Command(bd, "show", id, "--json")
	cmd.Dir = dir
	cmd.Env = bdEnv(dir)
	stdout, stderr, err := runCommandBuffers(t, cmd)
	if err != nil {
		t.Fatalf("bd show %s --json failed: %v\nstdout:\n%s\nstderr:\n%s", id, err, stdout.String(), stderr.String())
	}
	var body []struct {
		Revision int64 `json:"revision"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &body); err != nil {
		t.Fatalf("parse bd show --json for %s: %v\n%s", id, err, stdout.String())
	}
	if len(body) != 1 {
		t.Fatalf("bd show %s --json returned %d entries, want 1:\n%s", id, len(body), stdout.String())
	}
	return body[0].Revision
}

// TestUpdateIfRevisionCLI drives the bd-0fn revision guard (`bd update
// --if-revision`) end-to-end against the embedded Dolt backend. It is the
// general compare-and-swap for a safe read-modify-write of ANY field —
// notes in particular, whose only prior safety net (bd-2mx's
// refuse-to-overwrite-unless---replace-notes guard) does not protect an
// intentional --replace-notes rewrite from a concurrent editor's write in the
// same window. A stale --if-revision must refuse atomically, write nothing,
// and exit 13; a matching one must apply and mint a fresh revision so the
// guard cannot be replayed.
func TestUpdateIfRevisionCLI(t *testing.T) {
	if os.Getenv("BEADS_TEST_EMBEDDED_DOLT") != "1" {
		t.Skip("set BEADS_TEST_EMBEDDED_DOLT=1 to run embedded dolt integration tests")
	}
	t.Parallel()

	bd := buildEmbeddedBD(t)
	dir, _, _ := bdInit(t, bd, "--prefix", "ur")

	t.Run("stale_revision_refuses_and_writes_nothing", func(t *testing.T) {
		issue := bdCreate(t, bd, dir, "Notes race", "--type", "task", "--notes", "original")
		rev := bdShowRevision(t, bd, dir, issue.ID)

		// A concurrent writer lands first and bumps the revision.
		bdUpdate(t, bd, dir, issue.ID, "--priority", "1")

		out, code := bdUpdateFailCode(t, bd, dir, issue.ID,
			"--if-revision", strconv.FormatInt(rev, 10),
			"--replace-notes", "--notes", "stomped by a stale read")
		if code != ExitGuardMismatch {
			t.Errorf("stale --if-revision exit code = %d, want %d\n%s", code, ExitGuardMismatch, out)
		}
		if !strings.Contains(out, "version mismatch") && !strings.Contains(out, "revision") {
			t.Errorf("expected a version/revision mismatch message, got:\n%s", out)
		}
		got := bdShow(t, bd, dir, issue.ID)
		if got.Notes != "original" {
			t.Errorf("stale --if-revision clobbered notes: got %q, want unchanged %q", got.Notes, "original")
		}
	})

	t.Run("matching_revision_applies_and_is_exactly_once", func(t *testing.T) {
		issue := bdCreate(t, bd, dir, "Matching revision", "--type", "task", "--notes", "v1")
		rev := bdShowRevision(t, bd, dir, issue.ID)

		bdUpdate(t, bd, dir, issue.ID,
			"--if-revision", strconv.FormatInt(rev, 10),
			"--replace-notes", "--notes", "v2")
		got := bdShow(t, bd, dir, issue.ID)
		if got.Notes != "v2" {
			t.Errorf("after matching --if-revision: notes = %q, want v2", got.Notes)
		}

		// Replaying the same (now-stale) revision must fail, not silently
		// no-op or re-apply — the guard is exactly-once.
		out, code := bdUpdateFailCode(t, bd, dir, issue.ID,
			"--if-revision", strconv.FormatInt(rev, 10),
			"--replace-notes", "--notes", "v3-should-not-land")
		if code != ExitGuardMismatch {
			t.Errorf("replayed --if-revision exit code = %d, want %d\n%s", code, ExitGuardMismatch, out)
		}
		if got := bdShow(t, bd, dir, issue.ID); got.Notes != "v2" {
			t.Errorf("replayed stale guard clobbered notes: got %q, want v2", got.Notes)
		}
	})

	t.Run("composable_with_claim", func(t *testing.T) {
		issue := bdCreate(t, bd, dir, "Revision with claim", "--type", "task")
		rev := bdShowRevision(t, bd, dir, issue.ID)

		// Unlike --if-assignee/--if-status, --if-revision may ride with
		// --claim (issueops.UpdateRequest documents it as an independent
		// precondition).
		bdUpdate(t, bd, dir, issue.ID, "--if-revision", strconv.FormatInt(rev, 10), "--claim")
		got := bdShow(t, bd, dir, issue.ID)
		if got.Assignee == "" {
			t.Errorf("--if-revision + --claim did not claim the issue")
		}
	})

	t.Run("requires_a_field_update_or_claim", func(t *testing.T) {
		issue := bdCreate(t, bd, dir, "Bare guard", "--type", "task")
		rev := bdShowRevision(t, bd, dir, issue.ID)

		out := bdUpdateFail(t, bd, dir, issue.ID, "--if-revision", strconv.FormatInt(rev, 10), "--add-label", "x")
		if !strings.Contains(out, "field update") {
			t.Errorf("expected the field-update/--claim requirement in the error, got:\n%s", out)
		}
	})
}
