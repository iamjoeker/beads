package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// Contract tests for scripts/ci/lib/dolt-coverage.sh (bd-dln).
//
// The library decides whether a local run is allowed to report green while the
// Dolt-backed conformance contracts self-skip. It got written because a merge
// decision was made on
//
//	TestImporterContract       SKIP 0.00s
//	TestRelationsContract      SKIP 0.00s
//	TestCycleDetectorContract  SKIP 0.00s
//
// printed as "96 packages ok, 0 FAIL", with a 0.348s package runtime as the
// only tell. So the cases below grade BEHAVIOUR, not presence: which packages a
// changed path actually selects, that the selection is EMPTY for an unrelated
// path, that an unanswerable git probe is distinguishable from a clean tree,
// and that the run pattern really matches the three test names that skipped.

const doltCoverageLibRelPath = "ci/lib/dolt-coverage.sh"

func requireDoltCoverageShell(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("dolt-coverage.sh is a bash library for the POSIX wrappers")
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skipf("bash is required to exercise %s: %v", doltCoverageLibRelPath, err)
	}
	return bash
}

// runDoltCoverageShell sources the library and runs body, returning stdout.
// stderr is folded in so a failure explains itself.
func runDoltCoverageShell(t *testing.T, dir string, extraEnv []string, body string) (string, error) {
	t.Helper()
	bash := requireDoltCoverageShell(t)
	scriptsDir := filepath.Join(sourceRepoRoot(t), "scripts")

	script := "set -euo pipefail\n" +
		"source " + shSingleQuote(filepath.Join(scriptsDir, doltCoverageLibRelPath)) + "\n" +
		body

	env := make([]string, 0, len(os.Environ()))
	for _, entry := range os.Environ() {
		// The surrounding wrapper exports BEADS_TEST_SKIP=dolt and a live
		// BEADS_TEST_ENV_ROOT; inheriting either would make these cases read
		// the outer run's state instead of what they set themselves.
		if strings.HasPrefix(entry, "BEADS_TEST_") {
			continue
		}
		env = append(env, entry)
	}

	cmd := exec.Command(bash, "--noprofile", "--norc", "-c", script)
	if dir != "" {
		cmd.Dir = dir
	} else {
		cmd.Dir = scriptsDir
	}
	cmd.Env = append(env, extraEnv...)

	output, err := cmd.CombinedOutput()
	return string(output), err
}

func mustRunDoltCoverageShell(t *testing.T, dir string, extraEnv []string, body string) string {
	t.Helper()
	output, err := runDoltCoverageShell(t, dir, extraEnv, body)
	if err != nil {
		t.Fatalf("dolt-coverage shell failed: %v\n%s", err, output)
	}
	return output
}

func doltCoverageLines(output string) []string {
	var lines []string
	for _, line := range strings.Split(output, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

// TestDoltCoveragePackageSelection pins the changed-path -> package mapping.
//
// The "unrelated path selects nothing" case is the one that matters most: an
// empty selection is what this library produces on the overwhelmingly common
// change, and it is also what a broken selector produces on every change. The
// positives beside it are what make the empty result readable as a measurement
// rather than as silence.
func TestDoltCoveragePackageSelection(t *testing.T) {
	const (
		uow      = "./internal/storage/uow/"
		doltPkg  = "./internal/storage/dolt/"
		embedded = "./internal/storage/embeddeddolt/"
	)

	tests := []struct {
		name    string
		changed []string
		want    []string
	}{
		{
			// bd-a5s's actual diff: three files, all in backend/conformance/.
			name:    "a contract body puts every wiring on the hook",
			changed: []string{"backend/conformance/importer_contract.go"},
			want:    []string{uow, doltPkg, embedded},
		},
		{
			name:    "a wiring change selects only its own backend",
			changed: []string{"internal/storage/uow/importer_contract_test.go"},
			want:    []string{uow},
		},
		{
			name:    "the embedded backend has its own row",
			changed: []string{"internal/storage/embeddeddolt/conformance_test.go"},
			want:    []string{embedded},
		},
		{
			name:    "overlapping rows are reported once",
			changed: []string{"backend/conformance/audit.go", "internal/storage/dolt/store.go"},
			want:    []string{uow, doltPkg, embedded},
		},
		{
			name:    "an unrelated change selects nothing",
			changed: []string{"README.md", "cmd/bd/main.go", "internal/tracker/tracker.go"},
			want:    nil,
		},
		{
			name:    "a path that merely mentions a prefix does not match",
			changed: []string{"docs/backend/conformance/notes.md"},
			want:    nil,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			body := "printf '%s\\n' " + shSingleQuote(strings.Join(test.changed, "\n")) +
				" | beads_dolt_coverage_packages\n"
			got := doltCoverageLines(mustRunDoltCoverageShell(t, "", nil, body))
			if strings.Join(got, ",") != strings.Join(test.want, ",") {
				t.Fatalf("packages = %q, want %q", got, test.want)
			}
		})
	}
}

// TestDoltCoverageRunPatternMatchesTheTestsThatSkipped grades the pattern by
// the names it must select, not by reading it back. These three are the exact
// tests bd-dln measured at SKIP 0.00s.
func TestDoltCoverageRunPatternMatchesTheTestsThatSkipped(t *testing.T) {
	out := mustRunDoltCoverageShell(t, "", nil, "printf '%s' \"$BEADS_DOLT_COVERAGE_RUN\"\n")
	pattern, err := regexp.Compile(out)
	if err != nil {
		t.Fatalf("BEADS_DOLT_COVERAGE_RUN %q does not compile: %v", out, err)
	}

	for _, name := range []string{
		"TestImporterContract",
		"TestRelationsContract",
		"TestCycleDetectorContract",
		"TestConformance",
	} {
		if !pattern.MatchString(name) {
			t.Errorf("%s does not select %s", out, name)
		}
	}
	// Negative controls: the tier is the contracts, not each package's whole
	// real-Dolt surface, which a local gate never carried and which has its
	// own opt-in wrapper (scripts/ci/test-cmd-bd-dolt.sh).
	for _, name := range []string{
		"TestImporterUOW",
		"TestContractHelper",
		"TestConformanceReportFormatting",
	} {
		if pattern.MatchString(name) {
			t.Errorf("%s selects %s, which is outside the contract tier", out, name)
		}
	}
}

// TestDoltCoverageChangedFilesSeesEveryKindOfChange covers the probe itself.
// Committed-on-branch, unstaged and untracked are three different git
// questions, and a probe that answers only one of them reports a clean tree for
// the two it cannot see.
func TestDoltCoverageChangedFilesSeesEveryKindOfChange(t *testing.T) {
	repo := newDoltCoverageFixtureRepo(t)

	// The shape the gate actually runs in: a work branch off main, which is
	// what both a polecat's pre-verification and the refinery's merge gate
	// have in front of them.
	gitInRepo(t, repo, "checkout", "-b", "work")
	writeDoltCoverageFile(t, repo, "backend/conformance/committed.go", "package conformance\n")
	gitInRepo(t, repo, "add", "-A")
	gitInRepo(t, repo, "commit", "-m", "committed on the branch")

	writeDoltCoverageFile(t, repo, "base.txt", "modified after the base commit\n")
	writeDoltCoverageFile(t, repo, "internal/storage/uow/untracked.go", "package uow\n")

	got := doltCoverageLines(mustRunDoltCoverageShell(t, repo, nil,
		"beads_dolt_coverage_changed_files "+shSingleQuote(repo)+"\n"))

	want := []string{
		"backend/conformance/committed.go",
		"base.txt",
		"internal/storage/uow/untracked.go",
	}
	for _, path := range want {
		found := false
		for _, line := range got {
			if line == path {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("changed files %q missing %s", got, path)
		}
	}
}

// TestDoltCoverageChangedFilesReportsWhyItCannotAnswer is the control that
// keeps a broken probe from reading as a clean tree. Both failures below
// produce no file list; each must produce a reason and a non-zero status.
func TestDoltCoverageChangedFilesReportsWhyItCannotAnswer(t *testing.T) {
	t.Run("outside a git repository", func(t *testing.T) {
		dir := t.TempDir()
		output, err := runDoltCoverageShell(t, dir, nil,
			"beads_dolt_coverage_changed_files "+shSingleQuote(dir)+"\n")
		if err == nil {
			t.Fatalf("probe succeeded outside a repository: %q", output)
		}
		if !strings.Contains(output, "not a git repository") {
			t.Fatalf("reason = %q, want it to name the missing repository", output)
		}
	})

	t.Run("with no integration branch to compare against", func(t *testing.T) {
		repo := newDoltCoverageFixtureRepo(t)
		// The fixture commits on `main`; rename it away so neither
		// origin/main nor main resolves and no merge base exists.
		gitInRepo(t, repo, "branch", "-m", "main", "detached-work")

		output, err := runDoltCoverageShell(t, repo, nil,
			"beads_dolt_coverage_changed_files "+shSingleQuote(repo)+"\n")
		if err == nil {
			t.Fatalf("probe succeeded with no merge base: %q", output)
		}
		if !strings.Contains(output, "no merge base") {
			t.Fatalf("reason = %q, want it to name the missing merge base", output)
		}
	})
}

// TestDoltCoveragePreconditionNamesTheMissingDependency: when the tier cannot
// run, the wrapper has to put a reason in front of a human. A bare non-zero
// would be indistinguishable from the silence this whole bead is about.
func TestDoltCoveragePreconditionNamesTheMissingDependency(t *testing.T) {
	emptyPath := t.TempDir()

	output, err := runDoltCoverageShell(t, "", []string{"PATH=" + emptyPath},
		"beads_dolt_coverage_precondition ./internal/storage/uow/\n")
	if err == nil {
		t.Fatalf("precondition passed with no dolt on PATH: %q", output)
	}
	if !strings.Contains(output, "dolt") {
		t.Fatalf("reason = %q, want it to name the dolt binary", output)
	}

	fakeBin := t.TempDir()
	if err := os.WriteFile(filepath.Join(fakeBin, "dolt"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write fake dolt: %v", err)
	}
	output = mustRunDoltCoverageShell(t, "", []string{"PATH=" + fakeBin},
		"beads_dolt_coverage_precondition ./internal/storage/uow/\n")
	if strings.TrimSpace(output) != "" {
		t.Fatalf("precondition with dolt present printed %q, want nothing", output)
	}
}

// TestDoltCoverageEnableEnvStripsTheInheritedSkip pins the trap that would make
// the tier itself hollow. beads_test_env_enter has already exported
// BEADS_TEST_SKIP=dolt by the time the tier runs, and it returns early on a
// live inherited root — so BEADS_TEST_ENV_RUN_DOLT=1 alone leaves the skip in
// place and the tier self-skips exactly like the run it exists to catch.
func TestDoltCoverageEnableEnvStripsTheInheritedSkip(t *testing.T) {
	got := doltCoverageLines(mustRunDoltCoverageShell(t, "",
		[]string{"BEADS_TEST_SKIP=dolt,slow"},
		"beads_dolt_coverage_enable_env ./internal/storage/uow/\n"))

	want := []string{"BEADS_TEST_ENV_RUN_DOLT=1", "BEADS_TEST_SKIP=slow"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("enable env = %q, want %q", got, want)
	}

	// The embedded backend skips behind a different switch. That the two are
	// separate is a large part of why they went dark separately.
	got = doltCoverageLines(mustRunDoltCoverageShell(t, "",
		[]string{"BEADS_TEST_SKIP=dolt"},
		"beads_dolt_coverage_enable_env ./internal/storage/embeddeddolt/\n"))
	if strings.Join(got, ",") != "BEADS_TEST_EMBEDDED_DOLT=1" {
		t.Fatalf("embedded enable env = %q, want BEADS_TEST_EMBEDDED_DOLT=1", got)
	}
}

// TestDoltCoverageRequestedScopesTheTierToTheRun keeps a targeted run from
// growing a storage tier it never asked for, and keeps `./...` from losing one.
func TestDoltCoverageRequestedScopesTheTierToTheRun(t *testing.T) {
	tests := []struct {
		requested string
		want      bool
	}{
		{"./...", true},
		{"./internal/...", true},
		{"./internal/storage/...", true},
		{"./internal/storage/uow/", true},
		{"./internal/storage/uow", true},
		{"./internal/tracker/", false},
		{"./cmd/bd", false},
	}

	for _, test := range tests {
		t.Run(test.requested, func(t *testing.T) {
			body := "if beads_dolt_coverage_requested ./internal/storage/uow/ " +
				shSingleQuote(test.requested) + "; then echo yes; else echo no; fi\n"
			got := strings.TrimSpace(mustRunDoltCoverageShell(t, "", nil, body))
			want := "no"
			if test.want {
				want = "yes"
			}
			if got != want {
				t.Fatalf("requested(%s) = %s, want %s", test.requested, got, want)
			}
		})
	}
}

// TestDoltCoverageImageTagMatchesTheGoConstant: the precondition checks for a
// cached image by tag. A tag that has drifted from the one the tests actually
// pull would report "not cached" for an image that is right there, or pass for
// one that is wrong.
func TestDoltCoverageImageTagMatchesTheGoConstant(t *testing.T) {
	tag := strings.TrimSpace(mustRunDoltCoverageShell(t, "", nil,
		"printf '%s' \"$BEADS_DOLT_COVERAGE_IMAGE\"\n"))
	if tag != doltSQLServerImage {
		t.Fatalf("BEADS_DOLT_COVERAGE_IMAGE = %q, want %q (internal/testutil/testdoltcommon.go)", tag, doltSQLServerImage)
	}
}

// TestTestScriptSourcesTheDoltCoverageLibrary: the library only closes bd-dln
// if the wrapper that made the hollow merge decision actually consults it.
func TestTestScriptSourcesTheDoltCoverageLibrary(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(sourceRepoRoot(t), "scripts", "test.sh"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, needle := range []string{
		"scripts/ci/lib/dolt-coverage.sh",
		"beads_dolt_coverage_changed_files",
		"beads_dolt_coverage_packages",
		"beads_dolt_coverage_precondition",
		"beads_dolt_coverage_enable_env",
	} {
		if !strings.Contains(text, needle) {
			t.Errorf("scripts/test.sh does not reference %s", needle)
		}
	}
}

func newDoltCoverageFixtureRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git is required: %v", err)
	}
	repo := t.TempDir()
	gitInRepo(t, repo, "init", "-b", "main")
	gitInRepo(t, repo, "config", "user.email", "test@beads.local")
	gitInRepo(t, repo, "config", "user.name", "beads-test")
	writeDoltCoverageFile(t, repo, "base.txt", "base\n")
	gitInRepo(t, repo, "add", "-A")
	gitInRepo(t, repo, "commit", "-m", "base")
	return repo
}

func gitInRepo(t *testing.T, repo string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = repo
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL="+filepath.Join(repo, ".gitconfig-none"),
	)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
}

func writeDoltCoverageFile(t *testing.T, repo string, rel string, content string) {
	t.Helper()
	path := filepath.Join(repo, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
