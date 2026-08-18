package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/steveyegge/beads/internal/routing"
)

// newChangeDirProject creates a minimal beads project and returns its root and
// .beads directory, both symlink-resolved so they compare equal to what
// os.Getwd reports after a chdir into them (macOS /var -> /private/var).
func newChangeDirProject(t *testing.T) (projectDir, beadsDir string) {
	t.Helper()
	projectDir = t.TempDir()
	if resolved, err := filepath.EvalSymlinks(projectDir); err == nil {
		projectDir = resolved
	}
	beadsDir = filepath.Join(projectDir, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), []byte(`{"backend":"dolt"}`), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return projectDir, beadsDir
}

// withChangeDirFlag sets the -C global for one test and guarantees both the
// flag and anything applyChangeDirSelection moved are restored afterwards.
func withChangeDirFlag(t *testing.T, value string) {
	t.Helper()
	prev := changeDir
	changeDir = value
	t.Cleanup(func() {
		restoreChangeDirSelection()
		changeDir = prev
	})
}

// Resolution is a pure validation step: it reports the target and the store
// without moving the process. applyChangeDirSelection owns the move, because
// only it holds the snapshot that can undo it.
func TestResolveChangeDirTargetDoesNotChangeCWD(t *testing.T) {
	startDir := t.TempDir()
	t.Chdir(startDir)
	if resolved, err := filepath.EvalSymlinks(startDir); err == nil {
		startDir = resolved
	}

	projectDir, beadsDir := newChangeDirProject(t)

	gotTarget, gotBeadsDir, err := resolveChangeDirTarget(projectDir)
	if err != nil {
		t.Fatalf("resolveChangeDirTarget: %v", err)
	}
	if gotTarget != projectDir {
		t.Fatalf("resolveChangeDirTarget() target = %q, want %q", gotTarget, projectDir)
	}
	if gotBeadsDir != beadsDir {
		t.Fatalf("resolveChangeDirTarget() beadsDir = %q, want %q", gotBeadsDir, beadsDir)
	}

	afterWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd after resolve: %v", err)
	}
	if afterWD != startDir {
		t.Fatalf("working directory changed to %q, want %q", afterWD, startDir)
	}
}

func TestResolveChangeDirTargetRejectsFile(t *testing.T) {
	filePath := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(filePath, []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if _, _, err := resolveChangeDirTarget(filePath); err == nil {
		t.Fatal("expected non-directory -C target to fail")
	}
}

func TestResolveChangeDirTargetRejectsDirectoryWithoutProject(t *testing.T) {
	if _, _, err := resolveChangeDirTarget(t.TempDir()); err == nil {
		t.Fatal("expected -C target without a beads project to fail")
	}
}

// The flag's whole contract (bd-det): -C moves the process, so every
// cwd-sensitive decision downstream — role detection above all — sees the
// target, not the caller's directory.
func TestApplyChangeDirSelectionChangesCWD(t *testing.T) {
	startDir := t.TempDir()
	t.Chdir(startDir)

	projectDir, beadsDir := newChangeDirProject(t)
	withChangeDirFlag(t, projectDir)

	if err := applyChangeDirSelection(); err != nil {
		t.Fatalf("applyChangeDirSelection: %v", err)
	}

	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	if resolved, err := filepath.EvalSymlinks(wd); err == nil {
		wd = resolved
	}
	if wd != projectDir {
		t.Fatalf("working directory = %q, want the -C target %q", wd, projectDir)
	}
	if got := os.Getenv("BEADS_DIR"); got != beadsDir {
		t.Fatalf("BEADS_DIR = %q, want %q", got, beadsDir)
	}
}

// A subdirectory target follows git -C: bd runs from the literal path given,
// while the store is still the project discovered by walking up from it.
func TestApplyChangeDirSelectionChangesToSubdirectoryTarget(t *testing.T) {
	t.Chdir(t.TempDir())

	projectDir, beadsDir := newChangeDirProject(t)
	subDir := filepath.Join(projectDir, "nested", "deeper")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	withChangeDirFlag(t, subDir)

	if err := applyChangeDirSelection(); err != nil {
		t.Fatalf("applyChangeDirSelection: %v", err)
	}

	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	if resolved, err := filepath.EvalSymlinks(wd); err == nil {
		wd = resolved
	}
	if wd != subDir {
		t.Fatalf("working directory = %q, want the -C target %q", wd, subDir)
	}
	if got := os.Getenv("BEADS_DIR"); got != beadsDir {
		t.Fatalf("BEADS_DIR = %q, want the project store %q", got, beadsDir)
	}
}

func TestRestoreChangeDirSelectionRestoresCWD(t *testing.T) {
	startDir := t.TempDir()
	t.Chdir(startDir)
	if resolved, err := filepath.EvalSymlinks(startDir); err == nil {
		startDir = resolved
	}

	projectDir, _ := newChangeDirProject(t)
	withChangeDirFlag(t, projectDir)

	if err := applyChangeDirSelection(); err != nil {
		t.Fatalf("applyChangeDirSelection: %v", err)
	}
	restoreChangeDirSelection()

	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	if resolved, err := filepath.EvalSymlinks(wd); err == nil {
		wd = resolved
	}
	if wd != startDir {
		t.Fatalf("working directory = %q, want the original %q", wd, startDir)
	}

	// Restore is idempotent: PersistentPostRunE's defer can run after a
	// t.Cleanup or an error path already called it.
	restoreChangeDirSelection()
	if wd, err := os.Getwd(); err != nil {
		t.Fatalf("Getwd after second restore: %v", err)
	} else if resolved, rerr := filepath.EvalSymlinks(wd); rerr == nil && resolved != startDir {
		t.Fatalf("second restore moved cwd to %q, want %q", resolved, startDir)
	}
}

// setGitRole makes dir a git repository carrying an explicit beads.role, the
// preferred source DetectUserRole reads before any URL heuristic.
func setGitRole(t *testing.T, dir, role string) {
	t.Helper()
	for _, args := range [][]string{
		{"init"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "Test User"},
		{"config", "beads.role", role},
	} {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}

// The defect bd-det was filed for: role came from the process cwd while the
// store came from -C, so the two halves of every routing decision could
// disagree and no single-variable rule about the flag held. With -C moving the
// process, both halves name the -C target.
func TestApplyChangeDirSelectionMovesRoleDetection(t *testing.T) {
	callerDir := t.TempDir()
	setGitRole(t, callerDir, "maintainer")
	t.Chdir(callerDir)

	targetDir, _ := newChangeDirProject(t)
	setGitRole(t, targetDir, "contributor")

	if role, err := routing.DetectUserRole("."); err != nil {
		t.Fatalf("DetectUserRole before -C: %v", err)
	} else if role != routing.Maintainer {
		t.Fatalf("role before -C = %q, want %q", role, routing.Maintainer)
	}

	withChangeDirFlag(t, targetDir)
	if err := applyChangeDirSelection(); err != nil {
		t.Fatalf("applyChangeDirSelection: %v", err)
	}

	role, err := routing.DetectUserRole(".")
	if err != nil {
		t.Fatalf("DetectUserRole after -C: %v", err)
	}
	if role != routing.Contributor {
		t.Fatalf("role after -C = %q, want %q (role must follow the -C target)", role, routing.Contributor)
	}
}

// A rejected -C target must leave the process where it was: half-applying the
// flag is the failure mode this whole seam exists to prevent.
func TestApplyChangeDirSelectionLeavesCWDOnError(t *testing.T) {
	startDir := t.TempDir()
	t.Chdir(startDir)
	if resolved, err := filepath.EvalSymlinks(startDir); err == nil {
		startDir = resolved
	}

	withChangeDirFlag(t, t.TempDir()) // a directory with no beads project

	if err := applyChangeDirSelection(); err == nil {
		t.Fatal("expected -C target without a beads project to fail")
	}

	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	if resolved, err := filepath.EvalSymlinks(wd); err == nil {
		wd = resolved
	}
	if wd != startDir {
		t.Fatalf("working directory = %q, want the original %q", wd, startDir)
	}
}

func TestIsPreviewCommand(t *testing.T) {
	tests := []struct {
		name string
		flag string
		set  string
		want bool
	}{
		{name: "dry run", flag: "dry-run", set: "true", want: true},
		{name: "inspect", flag: "inspect", set: "true", want: true},
		{name: "false preview flag", flag: "dry-run", set: "false", want: false},
		{name: "no preview flag", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := &cobra.Command{Use: "test"}
			if tt.flag != "" {
				cmd.Flags().Bool(tt.flag, false, "")
				if err := cmd.Flags().Set(tt.flag, tt.set); err != nil {
					t.Fatalf("set %s: %v", tt.flag, err)
				}
			}
			if got := isPreviewCommand(cmd); got != tt.want {
				t.Fatalf("isPreviewCommand() = %v, want %v", got, tt.want)
			}
		})
	}
}
