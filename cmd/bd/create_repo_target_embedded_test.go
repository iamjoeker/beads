//go:build cgo

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestEmbeddedCreateRepoTargetResolution is the end-to-end half of bd-1yi:
// `bd create --repo <name-that-is-not-a-path>` used to print a success line
// with a freshly minted ID and persist the issue into a workspace it invented
// on the spot, and `--repo <dir whose .beads is a redirect stub>` used to
// initialize a second database beside the stub instead of following it. Both
// end as silent write loss — the operator quotes an ID that no read resolves.
func TestEmbeddedCreateRepoTargetResolution(t *testing.T) {
	if os.Getenv("BEADS_TEST_EMBEDDED_DOLT") != "1" {
		t.Skip("set BEADS_TEST_EMBEDDED_DOLT=1 to run embedded dolt create tests")
	}
	t.Parallel()

	bd := buildEmbeddedBD(t)

	t.Run("unresolvable_repo_name_fails_without_creating_a_workspace", func(t *testing.T) {
		dir, _, _ := bdInit(t, bd, "--prefix", "sr")

		// "gastown" names a rig, not a path — the reported reproduction.
		out := bdCreateFail(t, bd, dir, "--repo", "gastown", "Should not be created")
		if !strings.Contains(out, "does not exist") {
			t.Errorf("expected a --repo target error, got:\n%s", out)
		}
		if strings.Contains(out, "Created issue") {
			t.Errorf("no ID may be minted for an unresolvable --repo target:\n%s", out)
		}
		if _, err := os.Stat(filepath.Join(dir, "gastown")); !os.IsNotExist(err) {
			t.Errorf("create must not invent a workspace at %s: %v", filepath.Join(dir, "gastown"), err)
		}
	})

	t.Run("redirect_stub_target_writes_to_the_redirect_target", func(t *testing.T) {
		dir, _, _ := bdInit(t, bd, "--prefix", "sr")
		_, realBeadsDir, _ := bdInit(t, bd, "--prefix", "rt")

		// A rig whose .beads only points at the real workspace.
		stubDir := t.TempDir()
		stubBeadsDir := filepath.Join(stubDir, ".beads")
		if err := os.MkdirAll(stubBeadsDir, 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(stubBeadsDir, "redirect"),
			[]byte(realBeadsDir+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}

		issue := bdCreate(t, bd, dir, "Routed through a redirect stub", "--repo", stubDir)
		if !strings.HasPrefix(issue.ID, "rt-") {
			t.Errorf("ID should carry the redirect target's prefix rt-, got %q", issue.ID)
		}
		assertIssueInStore(t, realBeadsDir, "rt", issue.ID)

		// The stub must stay a stub: a database created beside it is exactly
		// the store no later read would consult.
		if entries, err := os.ReadDir(filepath.Join(stubBeadsDir, "embeddeddolt")); err == nil && len(entries) > 0 {
			t.Errorf("create initialized a database beside the redirect stub: %v", entries)
		}
		if _, err := os.Stat(filepath.Join(stubBeadsDir, "metadata.json")); err == nil {
			t.Errorf("create wrote metadata.json into the redirect stub at %s", stubBeadsDir)
		}
	})
}
