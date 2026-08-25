package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestResolveRepoTargetBeadsDir covers the two silent-write-loss traps on the
// explicit `bd create --repo <path>` route (bd-1yi): a target path that does
// not exist, and a target whose .beads is a redirect stub.
func TestResolveRepoTargetBeadsDir(t *testing.T) {
	t.Parallel()

	t.Run("missing_target_errors_and_creates_nothing", func(t *testing.T) {
		t.Parallel()
		// A repository NAME rather than a path is the shape that lost writes:
		// nothing on disk matches it, so the old code invented a workspace for
		// it and reported success.
		root := t.TempDir()
		target := filepath.Join(root, "gastown")

		got, err := resolveRepoTargetBeadsDir(target)
		if err == nil {
			t.Fatalf("expected an error for a nonexistent --repo target, got %q", got)
		}
		if !strings.Contains(err.Error(), "does not exist") {
			t.Errorf("error should name the missing target, got: %v", err)
		}
		if _, statErr := os.Stat(target); !os.IsNotExist(statErr) {
			t.Errorf("resolving must not create the target: %v", statErr)
		}
	})

	t.Run("empty_target_errors", func(t *testing.T) {
		t.Parallel()
		if got, err := resolveRepoTargetBeadsDir(""); err == nil {
			t.Fatalf("expected an error for an empty --repo target, got %q", got)
		}
	})

	t.Run("file_target_errors", func(t *testing.T) {
		t.Parallel()
		target := filepath.Join(t.TempDir(), "notadir")
		if err := os.WriteFile(target, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}

		_, err := resolveRepoTargetBeadsDir(target)
		if err == nil {
			t.Fatal("expected an error for a --repo target that is a file")
		}
		if !strings.Contains(err.Error(), "not a directory") {
			t.Errorf("error should say the target is not a directory, got: %v", err)
		}
	})

	t.Run("uninitialized_target_resolves_to_its_own_beads_dir", func(t *testing.T) {
		t.Parallel()
		// An existing directory with no .beads is still a legal target: create
		// initializes it in place (TestEmbeddedCreateCrossRepoUninit).
		target := t.TempDir()

		got, err := resolveRepoTargetBeadsDir(target)
		if err != nil {
			t.Fatalf("resolveRepoTargetBeadsDir: %v", err)
		}
		if want := filepath.Join(target, ".beads"); got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("redirect_stub_resolves_to_redirect_target", func(t *testing.T) {
		t.Parallel()
		// The stub carries no metadata.json of its own, so without following
		// the redirect the caller reads it as uninitialized and initializes a
		// second, unrelated database beside it.
		root := t.TempDir()

		realBeadsDir := filepath.Join(root, "real", ".beads")
		if err := os.MkdirAll(realBeadsDir, 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(realBeadsDir, "metadata.json"),
			[]byte(`{"dolt_database":"real_db"}`), 0o600); err != nil {
			t.Fatal(err)
		}

		stubDir := filepath.Join(root, "stub")
		stubBeadsDir := filepath.Join(stubDir, ".beads")
		if err := os.MkdirAll(stubBeadsDir, 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(stubBeadsDir, "redirect"),
			[]byte(realBeadsDir+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}

		got, err := resolveRepoTargetBeadsDir(stubDir)
		if err != nil {
			t.Fatalf("resolveRepoTargetBeadsDir: %v", err)
		}
		if got == stubBeadsDir {
			t.Fatalf("redirect was not followed: got the stub %q", got)
		}
		// Compare by content rather than path: the resolver canonicalizes the
		// redirect target, so the string need not match what was written.
		data, err := os.ReadFile(filepath.Join(got, "metadata.json"))
		if err != nil {
			t.Fatalf("resolved dir %q has no metadata.json: %v", got, err)
		}
		if !strings.Contains(string(data), "real_db") {
			t.Errorf("resolved to the wrong workspace %q: %s", got, data)
		}
	})
}

// TestCheckRepoTargetInitializable pins the two refusals ensureBeadsDirForPath
// used to hold inline (bd-cqv). They live in their own stat-only function so
// `--dry-run` can apply them without creating the workspace it is previewing a
// create into (bd-e7v) — which means this function must stay free of writes.
func TestCheckRepoTargetInitializable(t *testing.T) {
	t.Parallel()

	t.Run("initialized_target_reports_initialized", func(t *testing.T) {
		t.Parallel()
		beadsDir := filepath.Join(t.TempDir(), ".beads")
		if err := os.MkdirAll(beadsDir, 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"),
			[]byte(`{"dolt_database":"real_db"}`), 0o600); err != nil {
			t.Fatal(err)
		}

		initialized, err := checkRepoTargetInitializable(beadsDir)
		if err != nil {
			t.Fatalf("checkRepoTargetInitializable: %v", err)
		}
		if !initialized {
			t.Error("a target with metadata.json must report initialized")
		}
	})

	t.Run("unresolved_redirect_is_refused", func(t *testing.T) {
		t.Parallel()
		// resolveRepoTargetBeadsDir already followed every redirect it could,
		// so one surviving to here is one FollowRedirect fell back from.
		beadsDir := filepath.Join(t.TempDir(), ".beads")
		if err := os.MkdirAll(beadsDir, 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(beadsDir, "redirect"),
			[]byte("/nonexistent/target/.beads\n"), 0o600); err != nil {
			t.Fatal(err)
		}

		initialized, err := checkRepoTargetInitializable(beadsDir)
		if err == nil {
			t.Fatalf("expected a refusal for an unresolved redirect (initialized=%v)", initialized)
		}
		if !strings.Contains(err.Error(), "redirect") {
			t.Errorf("error should name the redirect, got: %v", err)
		}
		// Stat-only: the refusal must leave the stub exactly as it found it.
		if _, statErr := os.Stat(filepath.Join(beadsDir, "metadata.json")); !os.IsNotExist(statErr) {
			t.Errorf("checking wrote an identity file beside the redirect: %v", statErr)
		}
	})

	t.Run("uninitialized_target_reports_not_initialized_or_refuses", func(t *testing.T) {
		t.Parallel()
		// An existing directory with no .beads and no redirect: embedded-mode
		// invocations may initialize it in place, server-backed ones must
		// refuse. Which applies depends on this build's mode, so assert the
		// pairing rather than one branch.
		beadsDir := filepath.Join(t.TempDir(), ".beads")

		initialized, err := checkRepoTargetInitializable(beadsDir)
		if initialized {
			t.Fatal("a target with no metadata.json must not report initialized")
		}
		if isEmbeddedMode() {
			if err != nil {
				t.Errorf("embedded mode initializes an empty target in place, got: %v", err)
			}
		} else if err == nil {
			t.Error("a server-backed invocation must refuse an uninitialized --repo target")
		}
	})
}

// TestCreateTargetPrefixOverlay covers which workspace's config.yaml decides
// what an explicit --id must look like (bd-5ut). A routed create writes to
// another project's store, so reading the LOCAL overlay there judges the id by
// one workspace's prefix while writing to another's database — the same
// store-vs-naming split that reported an hq-prefixed id from a create aimed at
// a gastown store.
func TestCreateTargetPrefixOverlay(t *testing.T) {
	t.Parallel()

	writeOverlay := func(t *testing.T, prefix string) string {
		t.Helper()
		beadsDir := filepath.Join(t.TempDir(), ".beads")
		if err := os.MkdirAll(beadsDir, 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(beadsDir, "config.yaml"),
			[]byte("issue-prefix: \""+prefix+"\"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return beadsDir
	}

	t.Run("unrouted_create_reads_the_local_overlay", func(t *testing.T) {
		t.Parallel()
		// Compared against overlayYAMLPrefix rather than a literal: the point
		// is that an unrouted create is left exactly as it was, whatever the
		// ambient config says.
		if got, want := createTargetPrefixOverlay(".", ""), overlayYAMLPrefix(""); got != want {
			t.Errorf("unrouted overlay = %q, want the local overlay %q", got, want)
		}
	})

	t.Run("routed_create_reads_the_target_overlay", func(t *testing.T) {
		t.Parallel()
		beadsDir := writeOverlay(t, "gt")
		if got := createTargetPrefixOverlay("../gastown", beadsDir); got != "gt" {
			t.Errorf("routed overlay = %q, want the target's own %q", got, "gt")
		}
	})

	t.Run("routed_create_with_no_target_overlay_defers_to_the_target_store", func(t *testing.T) {
		t.Parallel()
		// Empty is not "fall back to the local prefix" — it is "the target
		// substrate's own prefix is authoritative", which selectCreateIDPrefix
		// then reads off the store the create was routed to.
		beadsDir := filepath.Join(t.TempDir(), ".beads")
		if err := os.MkdirAll(beadsDir, 0o750); err != nil {
			t.Fatal(err)
		}
		if got := createTargetPrefixOverlay("../gastown", beadsDir); got != "" {
			t.Errorf("routed overlay = %q, want empty for a target with no overlay", got)
		}
	})

	t.Run("remote_repo_url_has_no_local_overlay_to_read", func(t *testing.T) {
		t.Parallel()
		if got := createTargetPrefixOverlay("https://github.com/example/repo.git", ""); got != "" {
			t.Errorf("remote overlay = %q, want empty", got)
		}
	})
}

// TestEnsureBeadsDirForPathRefusesToStrandAWrite covers the two shapes from
// bd-cqv that reach ensureBeadsDirForPath with no metadata.json and must not be
// answered by materializing a fresh embedded database. Both end the same way if
// they are: a success line quoting an ID that lives in a store nothing reads,
// and — for the redirect case — an identity file that repoints the target's
// whole tree for every other caller in it.
func TestEnsureBeadsDirForPathRefusesToStrandAWrite(t *testing.T) {
	// Not parallel: the server-mode subtest drives the storage-mode globals.

	t.Run("redirect_present_but_unfollowed", func(t *testing.T) {
		// resolveRepoTargetBeadsDir already follows every redirect it can, so
		// a redirect still here is one FollowRedirect refused and fell back
		// from — a broken target, or a chain.
		beadsDir := filepath.Join(t.TempDir(), ".beads")
		if err := os.MkdirAll(beadsDir, 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(beadsDir, "redirect"),
			[]byte("/nonexistent/elsewhere/.beads\n"), 0o600); err != nil {
			t.Fatal(err)
		}

		err := ensureBeadsDirForPath(rootCtx, beadsDir, nil, true)
		if err == nil {
			t.Fatal("expected a refusal to initialize a database beside a redirect")
		}
		if !strings.Contains(err.Error(), "redirect") {
			t.Errorf("error should name the redirect, got: %v", err)
		}
		if _, statErr := os.Stat(filepath.Join(beadsDir, "metadata.json")); statErr == nil {
			t.Error("an identity file beside a redirect overrides it for every caller in that tree")
		}
		if _, statErr := os.Stat(filepath.Join(beadsDir, "embeddeddolt")); statErr == nil {
			t.Error("no embedded database may be created beside a redirect")
		}
	})

	t.Run("server_backed_workspace_does_not_invent_an_embedded_store", func(t *testing.T) {
		oldUseGlobals, oldServerMode := testModeUseGlobals, serverMode
		t.Cleanup(func() {
			testModeUseGlobals, serverMode = oldUseGlobals, oldServerMode
		})
		testModeUseGlobals = true
		serverMode = true

		target := t.TempDir()
		beadsDir := filepath.Join(target, ".beads")

		err := ensureBeadsDirForPath(rootCtx, beadsDir, nil, true)
		if err == nil {
			t.Fatal("expected a refusal to initialize an embedded database from a server-backed workspace")
		}
		if !strings.Contains(err.Error(), "server-backed") {
			t.Errorf("error should explain the server-backed refusal, got: %v", err)
		}
		if _, statErr := os.Stat(beadsDir); statErr == nil {
			t.Error("the refusal must leave no half-built workspace behind")
		}
	})
}
