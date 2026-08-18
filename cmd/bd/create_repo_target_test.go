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

		err := ensureBeadsDirForPath(rootCtx, beadsDir, nil)
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

		err := ensureBeadsDirForPath(rootCtx, beadsDir, nil)
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
