package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func runGitOK(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, out)
	}
}

// setupMergeLandingRepo creates a bare "origin" and a clone whose HEAD is
// origin's tip, returning the clone's working directory.
func setupMergeLandingRepo(t *testing.T) string {
	t.Helper()
	base := t.TempDir()
	origin := filepath.Join(base, "origin.git")
	runGitOK(t, base, "init", "--bare", "--initial-branch=main", origin)

	seed := filepath.Join(base, "seed")
	if err := os.MkdirAll(seed, 0750); err != nil {
		t.Fatal(err)
	}
	runGitOK(t, seed, "init", "--initial-branch=main")
	runGitOK(t, seed, "config", "user.email", "test@example.com")
	runGitOK(t, seed, "config", "user.name", "Test User")
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("seed\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runGitOK(t, seed, "add", "README.md")
	runGitOK(t, seed, "commit", "-m", "seed commit")
	runGitOK(t, seed, "remote", "add", "origin", origin)
	runGitOK(t, seed, "push", "origin", "main")

	clone := filepath.Join(base, "clone")
	runGitOK(t, base, "clone", origin, clone)
	runGitOK(t, clone, "config", "user.email", "test@example.com")
	runGitOK(t, clone, "config", "user.name", "Test User")
	return clone
}

func TestCheckMergeLandingClaims_UnpushedFixIsRefused(t *testing.T) {
	clone := setupMergeLandingRepo(t)
	if err := os.WriteFile(filepath.Join(clone, "local.txt"), []byte("local only\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runGitOK(t, clone, "add", "local.txt")
	runGitOK(t, clone, "commit", "-m", "local change never pushed")

	t.Chdir(clone)

	err := checkMergeLandingClaims([]string{"Fixed: something that never left this branch"})
	if err == nil {
		t.Fatal("expected checkMergeLandingClaims to refuse an unlanded 'Fixed:' claim")
	}
	if !strings.Contains(err.Error(), "origin/main") {
		t.Errorf("expected error to name origin/main, got: %v", err)
	}
}

func TestCheckMergeLandingClaims_LandedHeadIsAllowed(t *testing.T) {
	clone := setupMergeLandingRepo(t)
	t.Chdir(clone)

	if err := checkMergeLandingClaims([]string{"Fixed: already on origin/main"}); err != nil {
		t.Errorf("expected no error when HEAD is origin's tip, got: %v", err)
	}
}

func TestCheckMergeLandingClaims_NonLandingReasonIsAllowed(t *testing.T) {
	clone := setupMergeLandingRepo(t)
	if err := os.WriteFile(filepath.Join(clone, "local.txt"), []byte("local only\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runGitOK(t, clone, "add", "local.txt")
	runGitOK(t, clone, "commit", "-m", "local change never pushed")
	t.Chdir(clone)

	if err := checkMergeLandingClaims([]string{"no-changes: nothing to implement here"}); err != nil {
		t.Errorf("expected a non-landing-claim reason to pass unchecked, got: %v", err)
	}
}

func TestCheckMergeLandingClaims_NotAGitRepoFailsOpen(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	if err := checkMergeLandingClaims([]string{"Fixed: something"}); err != nil {
		t.Errorf("expected fail-open outside a git repo, got: %v", err)
	}
}
