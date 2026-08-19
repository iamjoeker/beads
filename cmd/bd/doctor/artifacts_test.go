package doctor

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCheckClassicArtifacts_NoArtifacts(t *testing.T) {
	dir := t.TempDir()

	// Create a basic .beads directory with no artifacts
	beadsDir := filepath.Join(dir, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}

	check := CheckClassicArtifacts(dir)
	if check.Status != StatusOK {
		t.Errorf("expected StatusOK, got %s: %s", check.Status, check.Message)
	}
}

// TestScanForArtifacts_JSONLInDoltDir — JSONL artifact scanning removed (bd-9ni.2)
func TestScanForArtifacts_JSONLInDoltDir(t *testing.T) {
	t.Skip("JSONL artifact scanning removed as part of JSONL removal (bd-9ni.2)")
}

func TestScanForArtifacts_SQLiteFiles(t *testing.T) {
	dir := t.TempDir()

	beadsDir := filepath.Join(dir, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Create dolt/ directory so isDoltNative returns true (SQLite files
	// are only flagged as artifacts when Dolt is the active backend)
	if err := os.MkdirAll(filepath.Join(beadsDir, "dolt"), 0755); err != nil {
		t.Fatal(err)
	}

	// Create SQLite artifacts
	for _, name := range []string{"beads.db", "beads.db-shm", "beads.db-wal", "beads.backup-20260204.db"} {
		if err := os.WriteFile(filepath.Join(beadsDir, name), []byte("data"), 0644); err != nil {
			t.Fatal(err)
		}
	}

	report, err := ScanForArtifacts(dir)
	if err != nil {
		t.Fatal(err)
	}

	if len(report.SQLiteArtifacts) != 4 {
		t.Errorf("expected 4 SQLite artifacts, got %d", len(report.SQLiteArtifacts))
	}

	// beads.db should NOT be safe to delete
	for _, f := range report.SQLiteArtifacts {
		if filepath.Base(f.Path) == "beads.db" && f.SafeDelete {
			t.Error("beads.db should NOT be safe to delete")
		}
	}

	// WAL/SHM should be safe
	for _, f := range report.SQLiteArtifacts {
		name := filepath.Base(f.Path)
		if (name == "beads.db-shm" || name == "beads.db-wal") && !f.SafeDelete {
			t.Errorf("%s should be safe to delete", name)
		}
	}

	// Backup should be safe
	for _, f := range report.SQLiteArtifacts {
		name := filepath.Base(f.Path)
		if name == "beads.backup-20260204.db" && !f.SafeDelete {
			t.Error("backup should be safe to delete")
		}
	}
}

func TestScanForArtifacts_CruftBeadsDir(t *testing.T) {
	// Create a structure like: beads/polecats/testpolecat/.beads/ with extra files
	dir := t.TempDir()
	polecatsDir := filepath.Join(dir, "polecats", "testpolecat")
	beadsDir := filepath.Join(polecatsDir, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Add redirect file (expected)
	if err := os.WriteFile(filepath.Join(beadsDir, "redirect"), []byte("../../mayor/rig/.beads"), 0644); err != nil {
		t.Fatal(err)
	}

	// Add cruft files
	if err := os.WriteFile(filepath.Join(beadsDir, "beads.db"), []byte("data"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "issues.jsonl"), []byte("data"), 0644); err != nil {
		t.Fatal(err)
	}

	report, err := ScanForArtifacts(dir)
	if err != nil {
		t.Fatal(err)
	}

	if len(report.CruftBeadsDirs) != 1 {
		t.Errorf("expected 1 cruft beads dir, got %d", len(report.CruftBeadsDirs))
	}

	if len(report.CruftBeadsDirs) > 0 && !report.CruftBeadsDirs[0].SafeDelete {
		t.Error("cruft beads dir with redirect should be safe to delete")
	}
}

func TestScanForArtifacts_CruftBeadsDirNoRedirect(t *testing.T) {
	// Cruft dir without redirect should be detected AND safe to delete.
	// The location being redirect-expected is sufficient — stale cruft files
	// are what prevent the redirect from being created in the first place.
	dir := t.TempDir()
	polecatsDir := filepath.Join(dir, "polecats", "testpolecat")
	beadsDir := filepath.Join(polecatsDir, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}

	// No redirect file, just extra files
	if err := os.WriteFile(filepath.Join(beadsDir, "beads.db"), []byte("data"), 0644); err != nil {
		t.Fatal(err)
	}

	report, err := ScanForArtifacts(dir)
	if err != nil {
		t.Fatal(err)
	}

	// Should be detected as cruft (it's in a polecat location) and safe to delete
	if len(report.CruftBeadsDirs) != 1 {
		t.Errorf("expected 1 cruft beads dir, got %d", len(report.CruftBeadsDirs))
	}
	if len(report.CruftBeadsDirs) > 0 && !report.CruftBeadsDirs[0].SafeDelete {
		t.Error("cruft beads dir in redirect-expected location should be safe to delete")
	}
}

func TestScanForArtifacts_CrewDir(t *testing.T) {
	dir := t.TempDir()
	crewDir := filepath.Join(dir, "crew", "testcrew")
	beadsDir := filepath.Join(crewDir, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Add redirect and cruft
	if err := os.WriteFile(filepath.Join(beadsDir, "redirect"), []byte("../../mayor/rig/.beads"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "extra.txt"), []byte("cruft"), 0644); err != nil {
		t.Fatal(err)
	}

	report, err := ScanForArtifacts(dir)
	if err != nil {
		t.Fatal(err)
	}

	if len(report.CruftBeadsDirs) != 1 {
		t.Errorf("expected 1 cruft beads dir for crew, got %d", len(report.CruftBeadsDirs))
	}
}

func TestScanForArtifacts_RedirectValidation(t *testing.T) {
	dir := t.TempDir()
	beadsDir := filepath.Join(dir, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Create redirect pointing to nonexistent target
	if err := os.WriteFile(filepath.Join(beadsDir, "redirect"), []byte("/nonexistent/path"), 0644); err != nil {
		t.Fatal(err)
	}

	report, err := ScanForArtifacts(dir)
	if err != nil {
		t.Fatal(err)
	}

	if len(report.RedirectIssues) != 1 {
		t.Errorf("expected 1 redirect issue, got %d", len(report.RedirectIssues))
	}
}

func TestScanForArtifacts_EmptyRedirect(t *testing.T) {
	dir := t.TempDir()
	beadsDir := filepath.Join(dir, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Create empty redirect
	if err := os.WriteFile(filepath.Join(beadsDir, "redirect"), []byte(""), 0644); err != nil {
		t.Fatal(err)
	}

	report, err := ScanForArtifacts(dir)
	if err != nil {
		t.Fatal(err)
	}

	if len(report.RedirectIssues) != 1 {
		t.Errorf("expected 1 redirect issue (empty), got %d", len(report.RedirectIssues))
	}
}

func TestScanForArtifacts_ValidRedirect(t *testing.T) {
	dir := t.TempDir()
	beadsDir := filepath.Join(dir, ".beads")
	targetDir := filepath.Join(dir, "target-beads")

	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(targetDir, 0755); err != nil {
		t.Fatal(err)
	}
	// A valid redirect target must have a metadata.json or database
	// (gastownhall/beads#4692 guard).
	if err := os.WriteFile(filepath.Join(targetDir, "metadata.json"), []byte(`{"database":"beads.db"}`), 0644); err != nil {
		t.Fatal(err)
	}

	// Create redirect pointing to valid target
	if err := os.WriteFile(filepath.Join(beadsDir, "redirect"), []byte(targetDir), 0644); err != nil {
		t.Fatal(err)
	}

	report, err := ScanForArtifacts(dir)
	if err != nil {
		t.Fatal(err)
	}

	if len(report.RedirectIssues) != 0 {
		t.Errorf("expected 0 redirect issues for valid target, got %d", len(report.RedirectIssues))
	}
}

// TestScanForArtifacts_RedirectTargetHasNoDatabase is a regression test for
// gastownhall/beads#4692: a redirect target directory that exists but has no
// metadata.json and no recognizable database is flagged as an actionable
// warning (not SafeDelete cruft), matching FollowRedirect's own target-
// validity guard (internal/beads).
func TestScanForArtifacts_RedirectTargetHasNoDatabase(t *testing.T) {
	dir := t.TempDir()
	beadsDir := filepath.Join(dir, ".beads")
	targetDir := filepath.Join(dir, "target-beads")

	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(targetDir, 0755); err != nil {
		t.Fatal(err)
	}
	// targetDir exists but is empty: no metadata.json, no database.

	if err := os.WriteFile(filepath.Join(beadsDir, "redirect"), []byte(targetDir), 0644); err != nil {
		t.Fatal(err)
	}

	report, err := ScanForArtifacts(dir)
	if err != nil {
		t.Fatal(err)
	}

	if len(report.RedirectIssues) != 1 {
		t.Fatalf("expected 1 redirect issue (target has no database), got %d", len(report.RedirectIssues))
	}
	if report.RedirectIssues[0].SafeDelete {
		t.Errorf("expected SafeDelete=false: an invalid-target redirect is an actionable warning, not cruft")
	}
}

func TestIsRedirectExpectedDir(t *testing.T) {
	tests := []struct {
		name     string
		path     string
		expected bool
	}{
		{"polecat worktree", "/foo/polecats/obsidian/.beads", true},
		{"crew workspace", "/foo/crew/mel/.beads", true},
		{"refinery rig", "/foo/refinery/rig/.beads", true},
		{"beads-worktrees", "/foo/.git/beads-worktrees/abc/.beads", true},
		{"regular beads dir", "/foo/.beads", false},
		{"nested project", "/foo/bar/.beads", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isRedirectExpectedDir(tt.path)
			if got != tt.expected {
				t.Errorf("isRedirectExpectedDir(%q) = %v, want %v", tt.path, got, tt.expected)
			}
		})
	}
}

func TestScanForArtifacts_SkipsGitkeep(t *testing.T) {
	dir := t.TempDir()
	polecatsDir := filepath.Join(dir, "polecats", "test")
	beadsDir := filepath.Join(polecatsDir, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}

	// redirect + .gitkeep only = no cruft
	if err := os.WriteFile(filepath.Join(beadsDir, "redirect"), []byte("../../mayor/rig/.beads"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, ".gitkeep"), []byte{}, 0644); err != nil {
		t.Fatal(err)
	}

	report, err := ScanForArtifacts(dir)
	if err != nil {
		t.Fatal(err)
	}

	if len(report.CruftBeadsDirs) != 0 {
		t.Errorf("expected 0 cruft dirs (redirect + .gitkeep only), got %d", len(report.CruftBeadsDirs))
	}
}

func TestScanForArtifacts_SkipsGitInternalsButScansBeadsWorktrees(t *testing.T) {
	dir := t.TempDir()

	// A bogus .beads under .git internals should be ignored entirely.
	ignoredBeads := filepath.Join(dir, ".git", "objects", "pack", ".beads")
	if err := os.MkdirAll(ignoredBeads, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ignoredBeads, "beads.db"), []byte("stale"), 0644); err != nil {
		t.Fatal(err)
	}

	// A worktree .beads under .git/beads-worktrees must still be scanned.
	worktreeBeads := filepath.Join(dir, ".git", "beads-worktrees", "test", ".beads")
	if err := os.MkdirAll(worktreeBeads, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktreeBeads, "extra.txt"), []byte("cruft"), 0644); err != nil {
		t.Fatal(err)
	}

	report, err := ScanForArtifacts(dir)
	if err != nil {
		t.Fatal(err)
	}

	if len(report.SQLiteArtifacts) != 0 {
		t.Fatalf("expected .git internals to be skipped, got %d sqlite findings", len(report.SQLiteArtifacts))
	}
	if len(report.CruftBeadsDirs) != 1 {
		t.Fatalf("expected 1 worktree cruft finding, got %d", len(report.CruftBeadsDirs))
	}
	if got := report.CruftBeadsDirs[0].Path; got != worktreeBeads {
		t.Fatalf("cruft finding path = %q, want %q", got, worktreeBeads)
	}
}

// TestScanForArtifacts_NonEmptyInteractionsJSONL — JSONL artifact scanning removed (bd-9ni.2)
func TestScanForArtifacts_NonEmptyInteractionsJSONL(t *testing.T) {
	t.Skip("JSONL artifact scanning removed as part of JSONL removal (bd-9ni.2)")
}

func TestCheckClassicArtifacts_WithArtifacts(t *testing.T) {
	dir := t.TempDir()
	beadsDir := filepath.Join(dir, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Create dolt/ directory so isDoltNative returns true
	if err := os.MkdirAll(filepath.Join(beadsDir, "dolt"), 0755); err != nil {
		t.Fatal(err)
	}

	// Create a SQLite artifact
	if err := os.WriteFile(filepath.Join(beadsDir, "beads.db-wal"), []byte("data"), 0644); err != nil {
		t.Fatal(err)
	}

	check := CheckClassicArtifacts(dir)
	if check.Status != StatusWarning {
		t.Errorf("expected StatusWarning, got %s: %s", check.Status, check.Message)
	}
	if check.Name != "Classic Artifacts" {
		t.Errorf("expected name 'Classic Artifacts', got %q", check.Name)
	}
}

// TestScanForArtifacts_OrphanEmbeddedStoresBesideRedirect covers the sweep half
// of bd-cqv. A create that failed to follow a redirect materialized an embedded
// database inside the redirect stub's own .beads; nothing reads it, and the
// issue written into it was stranded there with exit 0 reported to the caller.
//
// The sweep must enumerate by directory presence: the reported incident left
// TWO stores side by side and the second one had zero issues in it, so a
// content-based probe would have called that directory clean.
func TestScanForArtifacts_OrphanEmbeddedStoresBesideRedirect(t *testing.T) {
	dir := t.TempDir()
	beadsDir := filepath.Join(dir, ".beads")
	targetDir := filepath.Join(dir, "target-beads")

	if err := os.MkdirAll(targetDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(targetDir, "metadata.json"), []byte(`{"database":"beads.db"}`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "redirect"), []byte(targetDir), 0644); err != nil {
		t.Fatal(err)
	}

	// "hq" is the store that received the stranded write; "beads" is the empty
	// sibling that a content-based check cannot see.
	if err := os.MkdirAll(filepath.Join(beadsDir, "embeddeddolt", "hq", ".dolt"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(beadsDir, "embeddeddolt", "beads"), 0755); err != nil {
		t.Fatal(err)
	}
	// A loose file in embeddeddolt/ is not a store and must not be reported.
	if err := os.WriteFile(filepath.Join(beadsDir, "embeddeddolt", "README"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}

	report, err := ScanForArtifacts(dir)
	if err != nil {
		t.Fatal(err)
	}

	found := map[string]ArtifactFinding{}
	for _, f := range report.RedirectIssues {
		if f.Type == "orphan-embedded" {
			found[filepath.Base(f.Path)] = f
		}
	}
	if len(found) != 2 {
		t.Fatalf("expected both embedded stores reported, got %d: %+v", len(found), report.RedirectIssues)
	}
	for _, name := range []string{"hq", "beads"} {
		f, ok := found[name]
		if !ok {
			t.Errorf("embedded store %q beside the redirect was not reported", name)
			continue
		}
		if f.SafeDelete {
			t.Errorf("%q: SafeDelete must stay false — an orphan store can hold the only copy of a stranded issue", name)
		}
	}
}

// TestScanForArtifacts_EmbeddedStoreWithoutRedirectIsNotAnOrphan guards the
// other side: .beads/embeddeddolt/ is where a normal embedded workspace keeps
// its database. It is only unreachable when a redirect answers for the
// directory instead.
func TestScanForArtifacts_EmbeddedStoreWithoutRedirectIsNotAnOrphan(t *testing.T) {
	dir := t.TempDir()
	beadsDir := filepath.Join(dir, ".beads")

	if err := os.MkdirAll(filepath.Join(beadsDir, "embeddeddolt", "beads", ".dolt"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), []byte(`{"database":"beads.db"}`), 0644); err != nil {
		t.Fatal(err)
	}

	report, err := ScanForArtifacts(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range report.RedirectIssues {
		if f.Type == "orphan-embedded" {
			t.Errorf("a live embedded workspace must not be reported as an orphan: %+v", f)
		}
	}
}
