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

// TestEmbeddedCreateRepoExplicitIDPrefix is the end-to-end half of bd-5ut: an
// explicit --id on a routed create was judged against the prefix of the
// workspace bd was RUN in, not the one it writes to. The target's own prefix
// came back "prefix mismatch: database uses 'hq-'" while the database being
// written was the gt one, and the local prefix passed a front door that the
// storage layer then had to catch — with --force, not even that.
func TestEmbeddedCreateRepoExplicitIDPrefix(t *testing.T) {
	if os.Getenv("BEADS_TEST_EMBEDDED_DOLT") != "1" {
		t.Skip("set BEADS_TEST_EMBEDDED_DOLT=1 to run embedded dolt create tests")
	}
	t.Parallel()

	bd := buildEmbeddedBD(t)

	// A local workspace with a config.yaml overlay of its own — the shape that
	// made the split visible, since the overlay outranks the store's prefix.
	localDir, localBeadsDir, _ := bdInit(t, bd, "--prefix", "hq")
	if err := os.WriteFile(filepath.Join(localBeadsDir, "config.yaml"),
		[]byte("issue-prefix: \"hq\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	targetDir, targetBeadsDir, _ := bdInit(t, bd, "--prefix", "gt")

	t.Run("target_prefixed_id_is_accepted", func(t *testing.T) {
		issue := bdCreate(t, bd, localDir, "Routed with the target's own prefix",
			"--repo", targetDir, "--id", "gt-tgt1")
		if issue.ID != "gt-tgt1" {
			t.Errorf("ID = %q, want gt-tgt1", issue.ID)
		}
		assertIssueInStore(t, targetBeadsDir, "gt", "gt-tgt1")
	})

	t.Run("local_prefixed_id_is_refused_naming_the_target_prefix", func(t *testing.T) {
		out := bdCreateFail(t, bd, localDir, "--repo", targetDir, "--id", "hq-tgt2",
			"Routed with the local workspace's prefix")
		if !strings.Contains(out, "gt-") {
			t.Errorf("refusal should name the TARGET's prefix gt-, got:\n%s", out)
		}
		assertIssueNotInStore(t, targetBeadsDir, "gt", "hq-tgt2")
	})

	t.Run("auto_minted_id_carries_the_target_prefix", func(t *testing.T) {
		issue := bdCreate(t, bd, localDir, "Routed with no explicit id", "--repo", targetDir)
		if !strings.HasPrefix(issue.ID, "gt-") {
			t.Errorf("ID %q should carry the target's prefix gt-", issue.ID)
		}
		assertIssueInStore(t, targetBeadsDir, "gt", issue.ID)
	})
}

// TestEmbeddedCreateRepoDryRunTargetResolution is the end-to-end half of
// bd-e7v. Every --repo check sat past the preview's early return, so
// `--dry-run --repo <name-that-is-not-a-path>` printed "Would create issue"
// and exited 0 for the exact target a real create refuses. A preview that
// disagrees with the command it previews is the same reported failure the
// --repo route has produced all along — a success line for a create that
// never lands — and the one an operator is most likely to trust, since
// checking with --dry-run first is what caution looks like here.
func TestEmbeddedCreateRepoDryRunTargetResolution(t *testing.T) {
	if os.Getenv("BEADS_TEST_EMBEDDED_DOLT") != "1" {
		t.Skip("set BEADS_TEST_EMBEDDED_DOLT=1 to run embedded dolt create tests")
	}
	t.Parallel()

	bd := buildEmbeddedBD(t)

	t.Run("unresolvable_repo_name_fails_the_preview_too", func(t *testing.T) {
		dir, _, _ := bdInit(t, bd, "--prefix", "sr")

		out := bdCreateFail(t, bd, dir, "--dry-run", "--repo", "gastown", "Should not be previewed")
		if !strings.Contains(out, "does not exist") {
			t.Errorf("expected a --repo target error, got:\n%s", out)
		}
		if strings.Contains(out, "Would create issue") {
			t.Errorf("no preview may be rendered for an unresolvable --repo target:\n%s", out)
		}
		if _, err := os.Stat(filepath.Join(dir, "gastown")); !os.IsNotExist(err) {
			t.Errorf("a dry run must not invent a workspace at %s: %v", filepath.Join(dir, "gastown"), err)
		}
	})

	t.Run("unresolved_redirect_stub_fails_the_preview", func(t *testing.T) {
		dir, _, _ := bdInit(t, bd, "--prefix", "sr")

		// A stub whose redirect names a directory that does not exist:
		// FollowRedirect falls back to the stub, so the preview reaches the
		// same refusal a real create reaches.
		stubDir := t.TempDir()
		stubBeadsDir := filepath.Join(stubDir, ".beads")
		if err := os.MkdirAll(stubBeadsDir, 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(stubBeadsDir, "redirect"),
			[]byte(filepath.Join(stubDir, "gone", ".beads")+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}

		out := bdCreateFail(t, bd, dir, "--dry-run", "--repo", stubDir, "Should not be previewed")
		if !strings.Contains(out, "redirect") {
			t.Errorf("refusal should name the redirect, got:\n%s", out)
		}
		// The preview must leave the stub a stub — writing an identity file
		// beside a redirect overrides it for every later command in that tree.
		if _, err := os.Stat(filepath.Join(stubBeadsDir, "metadata.json")); err == nil {
			t.Errorf("a dry run wrote metadata.json into the redirect stub at %s", stubBeadsDir)
		}
	})

	t.Run("valid_target_still_previews", func(t *testing.T) {
		dir, _, _ := bdInit(t, bd, "--prefix", "sr")
		targetDir, targetBeadsDir, _ := bdInit(t, bd, "--prefix", "dt")

		out := bdRunOK(t, bd, dir, "create", "--dry-run", "--repo", targetDir, "Previewed only")
		if !strings.Contains(out, "Would create issue") {
			t.Errorf("a resolvable --repo target must still preview, got:\n%s", out)
		}
		// A preview is still a preview: nothing may reach the target store.
		listed := bdRunOK(t, bd, targetDir, "list")
		if strings.Contains(listed, "Previewed only") {
			t.Errorf("dry run wrote into the target store at %s:\n%s", targetBeadsDir, listed)
		}
	})
}
