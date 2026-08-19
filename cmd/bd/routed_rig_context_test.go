//go:build cgo

package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/beads/internal/types"
)

// TestPrefixRoutingFromRigContext is the end-to-end regression test for bd-4sw.
//
// Reproduces the reported failure: from a rig worktree — where the command
// resolves to the rig's database, not the town's — every foreign-prefix bead
// reported "no issue found". Two independent defects produced that:
//
//  1. routes.jsonl was loaded from the resolved beads directory, which in a rig
//     context has no routes at all, and the town root was derived as that
//     directory's parent.
//  2. the town route (path ".") was skipped unconditionally as "already
//     checked" — true only at the town root. From a rig context the town
//     database is a different database and must be followed.
//
// Both must be fixed for an hq- lookup from a rig to succeed, so this test
// covers them together.
//
// NOTE: uses os.Chdir and mutates the dbPath global; cannot run in parallel.
func TestPrefixRoutingFromRigContext(t *testing.T) {
	ctx := context.Background()

	// tmpDir/                     <- town root
	//   .beads/dolt               (town database, prefix "hq")
	//   .beads/routes.jsonl
	//   rig/.beads/dolt           (rig database, prefix "gt")
	//   rig/worktree/             <- cwd, a polecat-style working directory
	tmpDir := t.TempDir()
	townBeadsDir := filepath.Join(tmpDir, ".beads")
	rigBeadsDir := filepath.Join(tmpDir, "rig", ".beads")
	worktree := filepath.Join(tmpDir, "rig", "worktree")
	for _, dir := range []string{townBeadsDir, rigBeadsDir, worktree} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("create %s: %v", dir, err)
		}
	}

	townStore := newTestStoreIsolatedDB(t, filepath.Join(townBeadsDir, "dolt"), "hq")
	if err := townStore.CreateIssue(ctx, &types.Issue{
		ID:        "hq-town1",
		Title:     "Town mail bead",
		Status:    types.StatusOpen,
		Priority:  1,
		IssueType: types.TypeTask,
	}, "test"); err != nil {
		t.Fatalf("create town issue: %v", err)
	}
	// Release the town store so routing can reopen it by path.
	townStore.Close()

	rigStore := newTestStoreIsolatedDB(t, filepath.Join(rigBeadsDir, "dolt"), "gt")
	if err := rigStore.CreateIssue(ctx, &types.Issue{
		ID:        "gt-rig1",
		Title:     "Rig-local bead",
		Status:    types.StatusOpen,
		Priority:  2,
		IssueType: types.TypeTask,
	}, "test"); err != nil {
		t.Fatalf("create rig issue: %v", err)
	}

	routesContent := `{"prefix":"hq-","path":"."}` + "\n" + `{"prefix":"gt-","path":"rig"}` + "\n"
	if err := os.WriteFile(filepath.Join(townBeadsDir, "routes.jsonl"), []byte(routesContent), 0o600); err != nil {
		t.Fatalf("write routes.jsonl: %v", err)
	}

	// The command is running against the RIG database, from inside the rig.
	oldDBPath := dbPath
	dbPath = filepath.Join(rigBeadsDir, "dolt")
	t.Cleanup(func() { dbPath = oldDBPath })

	oldWd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(worktree); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWd) })

	t.Run("foreign_prefix_resolves_from_rig", func(t *testing.T) {
		result, err := resolveAndGetIssueWithRouting(ctx, rigStore, "hq-town1")
		if err != nil {
			t.Fatalf("hq- bead unresolvable from a rig context (bd-4sw): %v", err)
		}
		defer result.Close()
		if !result.Routed {
			t.Error("expected Routed=true for a cross-database lookup")
		}
		if result.Issue == nil || result.Issue.ID != "hq-town1" {
			t.Fatalf("resolved issue = %+v, want hq-town1", result.Issue)
		}
	})

	t.Run("local_prefix_still_resolves_locally", func(t *testing.T) {
		result, err := resolveAndGetIssueWithRouting(ctx, rigStore, "gt-rig1")
		if err != nil {
			t.Fatalf("rig-local lookup broke: %v", err)
		}
		defer result.Close()
		if result.Routed {
			t.Error("rig-local bead should resolve in the local store, not via routing")
		}
	})

	t.Run("absent_foreign_bead_names_both_databases", func(t *testing.T) {
		_, err := resolveAndGetIssueWithRouting(ctx, rigStore, "hq-nosuch")
		if err == nil {
			t.Fatal("expected an error for an absent bead")
		}
		msg := err.Error()
		// An unroutable prefix and an absent bead are different answers; the
		// message must say which databases actually answered (bd-4sw).
		for _, want := range []string{"hq-nosuch", "searched database", "the town root", "also searched"} {
			if !strings.Contains(msg, want) {
				t.Errorf("not-found message missing %q:\n%s", want, msg)
			}
		}
		if !isNotFoundErr(err) {
			t.Errorf("annotated error no longer reads as not-found:\n%s", msg)
		}
	})
}
