package git

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func runOK(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, out)
	}
	return string(out)
}

// setupOriginAndClone creates a bare "origin" repo and a clone of it with
// user.name/email configured, returning the clone's working directory.
func setupOriginAndClone(t *testing.T) (clonePath string) {
	t.Helper()
	base := t.TempDir()
	origin := filepath.Join(base, "origin.git")
	if err := os.MkdirAll(origin, 0750); err != nil {
		t.Fatal(err)
	}
	runOK(t, origin, "init", "--bare", "--initial-branch=main")

	seed := filepath.Join(base, "seed")
	if err := os.MkdirAll(seed, 0750); err != nil {
		t.Fatal(err)
	}
	runOK(t, seed, "init", "--initial-branch=main")
	runOK(t, seed, "config", "user.email", "test@example.com")
	runOK(t, seed, "config", "user.name", "Test User")
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("seed\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runOK(t, seed, "add", "README.md")
	runOK(t, seed, "commit", "-m", "seed commit")
	runOK(t, seed, "remote", "add", "origin", origin)
	runOK(t, seed, "push", "origin", "main")

	clonePath = filepath.Join(base, "clone")
	runOK(t, base, "clone", origin, clonePath)
	runOK(t, clonePath, "config", "user.email", "test@example.com")
	runOK(t, clonePath, "config", "user.name", "Test User")
	return clonePath
}

func TestVerifyHeadLandedOnOrigin_HeadIsOnOrigin(t *testing.T) {
	clone := setupOriginAndClone(t)

	check := VerifyHeadLandedOnOrigin(clone)
	if !check.Landed {
		t.Errorf("expected Landed=true for a HEAD that is origin/main's tip, got %+v", check)
	}
	if check.Target != "main" {
		t.Errorf("expected target=main, got %q", check.Target)
	}
}

func TestVerifyHeadLandedOnOrigin_UnpushedCommitNotLanded(t *testing.T) {
	clone := setupOriginAndClone(t)

	if err := os.WriteFile(filepath.Join(clone, "unpushed.txt"), []byte("local only\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runOK(t, clone, "add", "unpushed.txt")
	runOK(t, clone, "commit", "-m", "fixed: local-only change never pushed")

	check := VerifyHeadLandedOnOrigin(clone)
	if check.Landed {
		t.Errorf("expected Landed=false for a commit that only exists locally, got %+v", check)
	}
	if check.Target != "main" {
		t.Errorf("expected target=main, got %q", check.Target)
	}
	if check.HeadSHA == "" {
		t.Errorf("expected a non-empty HeadSHA")
	}
}

func TestVerifyHeadLandedOnOrigin_NotAGitRepo(t *testing.T) {
	dir := t.TempDir()
	check := VerifyHeadLandedOnOrigin(dir)
	if !check.Landed {
		t.Errorf("expected fail-open (Landed=true) for a non-git directory, got %+v", check)
	}
}

func TestVerifyHeadLandedOnOrigin_NoRemote(t *testing.T) {
	dir := t.TempDir()
	runOK(t, dir, "init", "--initial-branch=main")
	runOK(t, dir, "config", "user.email", "test@example.com")
	runOK(t, dir, "config", "user.name", "Test User")
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("x\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runOK(t, dir, "add", "f.txt")
	runOK(t, dir, "commit", "-m", "no remote configured")

	check := VerifyHeadLandedOnOrigin(dir)
	if !check.Landed {
		t.Errorf("expected fail-open (Landed=true) when there is no remote, got %+v", check)
	}
	if check.Target != "" {
		t.Errorf("expected empty target when there is no remote, got %q", check.Target)
	}
}
