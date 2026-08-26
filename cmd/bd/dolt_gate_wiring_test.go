package main

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The cmd/bd tests that need a Dolt server were dark in CI for several
// releases and rotted to 25 failures on pristine main (bd-kbx). The fix was a
// chain: scripts/ci/lib/test-env.sh only skips Dolt when
// BEADS_TEST_ENV_RUN_DOLT is unset -> scripts/ci/test-cmd-bd-dolt.sh sets that
// plus the require flag -> startTestDoltServer turns a missing container into
// a non-zero exit -> two workflow jobs run the wrapper (bd-9jl).
//
// Every link is a string in a different language, so nothing but this test
// connects them. Break any one and the surface goes dark again, silently and
// greenly — which is exactly how it went dark the first time.
//
// These read files rather than exercising behaviour: shell and YAML cannot
// import Go constants, and the failure being guarded against is a rename or a
// deletion, not a logic error. They need no Dolt server, so they run in the
// dolt-skipped lanes where the rest of CI lives.

const gateWrapperPath = "../../scripts/ci/test-cmd-bd-dolt.sh"

// The gate for the OTHER half of cmd/bd's Dolt surface. requireDoltEnvVar
// (test_repo_beads_guard_test.go) names the server one; this names the
// in-process one, and the two cannot be set in the same run -- see
// TestEmbeddedDoltShortCircuitsTheContainer.
const embeddedDoltEnvVar = "BEADS_TEST_EMBEDDED_DOLT"

func readRepoFile(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(content)
}

// TestDoltGateWrapperSetsTheRequireFlag ties the wrapper to the env var the
// cgo hook reads. Rename one side only and the gate keeps passing while
// running nothing.
func TestDoltGateWrapperSetsTheRequireFlag(t *testing.T) {
	t.Parallel()

	wrapper := readRepoFile(t, gateWrapperPath)

	if !strings.Contains(wrapper, "export "+requireDoltEnvVar+"=1") {
		t.Errorf("%s no longer exports %s=1; without it a missing Dolt container "+
			"makes every Dolt-backed cmd/bd test self-skip and the job passes green",
			gateWrapperPath, requireDoltEnvVar)
	}
	if !strings.Contains(wrapper, "export BEADS_TEST_ENV_RUN_DOLT=1") {
		t.Errorf("%s no longer exports BEADS_TEST_ENV_RUN_DOLT=1; scripts/ci/lib/test-env.sh "+
			"would then add dolt to BEADS_TEST_SKIP and the wrapper would run the same "+
			"skipped suite as every other lane", gateWrapperPath)
	}
}

// TestDoltGateWrapperRunsTheEmbeddedPass ties the wrapper to the OTHER env
// var cmd/bd's Dolt tests gate on. The server pair above un-skips ~150 tests;
// 266 more gate on BEADS_TEST_EMBEDDED_DOLT and are untouched by it, which is
// the state bd-nn6 fixed by adding a second pass.
func TestDoltGateWrapperRunsTheEmbeddedPass(t *testing.T) {
	t.Parallel()

	wrapper := readRepoFile(t, gateWrapperPath)

	if !strings.Contains(wrapper, "export "+embeddedDoltEnvVar+"=1") {
		t.Errorf("%s no longer exports %s=1; cmd/bd's embedded-Dolt tests would "+
			"self-skip again, and this is the only job that runs the package as a "+
			"whole (bd-nn6)", gateWrapperPath, embeddedDoltEnvVar)
	}
	// The two flags abort each other, so the second pass is only real if it
	// drops the first pass's require flag. Setting both in one invocation
	// does not widen a run; startTestDoltServerInner returns early on the
	// embedded flag, testDoltServerPort stays 0, and the require flag turns
	// that into a FATAL before a single test executes.
	if !strings.Contains(wrapper, "unset "+requireDoltEnvVar) {
		t.Errorf("%s exports %s=1 without unsetting %s for that pass; the two are "+
			"mutually exclusive and the embedded pass would abort rather than run",
			gateWrapperPath, embeddedDoltEnvVar, requireDoltEnvVar)
	}
}

// TestEmbeddedDoltShortCircuitsTheContainer pins the premise the two-pass
// shape rests on: the embedded flag makes startTestDoltServerInner return
// before any container starts. If that ever stops being true the passes could
// merge back into one -- but until someone checks, splitting them is what
// keeps BEADS_CMD_BD_REQUIRE_DOLT from failing the embedded pass outright.
func TestEmbeddedDoltShortCircuitsTheContainer(t *testing.T) {
	t.Parallel()

	const helperPath = "test_dolt_server_cgo_test.go"
	content := readRepoFile(t, helperPath)

	if !strings.Contains(content, `os.Getenv("`+embeddedDoltEnvVar+`") == "1"`) {
		t.Fatalf("%s no longer short-circuits on %s; %s splits its run into two passes "+
			"precisely because it does, and would now be paying for a second pass that "+
			"needs no splitting", helperPath, embeddedDoltEnvVar, gateWrapperPath)
	}
}

// TestEmbeddedGatedTestsEscapeNameBasedDiscovery is the reason the embedded
// pass selects by PACKAGE and not by name.
//
// .github/scripts/embedded-test-shard.sh discovers work with
// `grep '^func TestEmbedded' cmd/bd/*_embedded_test.go`. When bd-nn6 was
// filed that found 191 of the 266 embedded-gated top-level tests here; the
// other 79, across 24 files, ran in no job at all -- TestDoltLocalOnly_*,
// TestDoltRemoteAdd_*, TestMigratePersonal_*, TestDoctor_* and friends, some
// of them in files whose own name does end _embedded_test.go. A name regex
// cannot see them, and a count is not what should hold the line, so this
// recomputes both sets from source.
func TestEmbeddedGatedTestsEscapeNameBasedDiscovery(t *testing.T) {
	t.Parallel()

	gated := embeddedGatedTests(t)
	// Positive control. A parser that matches nothing produces a clean
	// "everything is covered" verdict indistinguishable from real coverage,
	// which is the exact failure mode this whole file exists to prevent.
	if len(gated) == 0 {
		t.Fatalf("found no %s-gated tests in cmd/bd; the parser below is broken, "+
			"not the coverage -- there were 266 when this test was written", embeddedDoltEnvVar)
	}

	discovered := nameDiscoverableEmbeddedTests(t)
	if len(discovered) == 0 {
		t.Fatalf("found no `^func TestEmbedded` tests in cmd/bd/*_embedded_test.go; " +
			"the parser is broken, not the coverage -- there were 191 when this test was written")
	}

	var missed []string
	for name := range gated {
		if !discovered[name] {
			missed = append(missed, name)
		}
	}
	sort.Strings(missed)
	t.Logf("%s-gated: %d; reachable by embedded-test-shard.sh's name regex: %d; "+
		"reachable only by running the package: %d",
		embeddedDoltEnvVar, len(gated), len(discovered), len(missed))

	if len(missed) == 0 {
		// Not a failure: it would mean every gated test had been renamed into
		// the TestEmbedded* convention. Say so, because the wrapper's second
		// pass would then be belt-and-braces rather than sole coverage.
		t.Log("every gated test is now name-discoverable; the wrapper's embedded pass " +
			"is no longer the only thing running any of them")
		return
	}

	wrapper := readRepoFile(t, gateWrapperPath)
	if !strings.Contains(wrapper, "export "+embeddedDoltEnvVar+"=1") {
		shown := missed
		if len(shown) > 8 {
			shown = shown[:8]
		}
		t.Errorf("%d cmd/bd tests gate on %s but are invisible to "+
			".github/scripts/embedded-test-shard.sh's `^func TestEmbedded` discovery, "+
			"and %s does not run an embedded pass -- nothing executes them. e.g. %s",
			len(missed), embeddedDoltEnvVar, gateWrapperPath, strings.Join(shown, ", "))
	}
}

var (
	topLevelTestRe = regexp.MustCompile(`(?m)^func (Test[A-Za-z0-9_]*)\(`)
	embeddedNameRe = regexp.MustCompile(`(?m)^func (TestEmbedded[A-Za-z0-9_]*)\(`)
)

// embeddedGatedTests returns the top-level tests in this package whose body
// mentions the embedded gate. Source text, not reflection: a self-skipping
// test is indistinguishable from a passing one at runtime, which is why the
// gap went unnoticed in the first place.
func embeddedGatedTests(t *testing.T) map[string]bool {
	t.Helper()

	gated := map[string]bool{}
	paths, err := filepath.Glob("*_test.go")
	if err != nil {
		t.Fatalf("glob cmd/bd test files: %v", err)
	}
	for _, path := range paths {
		src := readRepoFile(t, path)
		locs := topLevelTestRe.FindAllStringSubmatchIndex(src, -1)
		for i, loc := range locs {
			end := len(src)
			if i+1 < len(locs) {
				end = locs[i+1][0]
			}
			if strings.Contains(src[loc[0]:end], embeddedDoltEnvVar) {
				gated[src[loc[2]:loc[3]]] = true
			}
		}
	}
	return gated
}

// nameDiscoverableEmbeddedTests mirrors embedded-test-shard.sh's discovery
// exactly: `^func TestEmbedded` in cmd/bd/*_embedded_test.go, nothing else.
func nameDiscoverableEmbeddedTests(t *testing.T) map[string]bool {
	t.Helper()

	found := map[string]bool{}
	paths, err := filepath.Glob("*_embedded_test.go")
	if err != nil {
		t.Fatalf("glob cmd/bd embedded test files: %v", err)
	}
	for _, path := range paths {
		for _, m := range embeddedNameRe.FindAllStringSubmatch(readRepoFile(t, path), -1) {
			found[m[1]] = true
		}
	}
	return found
}

// TestDoltSkipStillGatedOnRunDoltOptIn pins the premise the wrapper depends
// on: test-env.sh skips Dolt unless BEADS_TEST_ENV_RUN_DOLT=1.
func TestDoltSkipStillGatedOnRunDoltOptIn(t *testing.T) {
	t.Parallel()

	const testEnvPath = "../../scripts/ci/lib/test-env.sh"
	content := readRepoFile(t, testEnvPath)

	if !strings.Contains(content, "BEADS_TEST_ENV_RUN_DOLT") {
		t.Fatalf("%s no longer mentions BEADS_TEST_ENV_RUN_DOLT; %s exports it to turn the "+
			"Dolt surface on, and would now be a no-op", testEnvPath, gateWrapperPath)
	}
	if !strings.Contains(content, `beads_test_env_add_skip "dolt"`) {
		t.Fatalf("%s no longer adds the dolt skip; if the default changed, the wrapper's "+
			"opt-in and this whole gate need revisiting rather than silently drifting",
			testEnvPath)
	}
}

// TestDoltGateJobsStillRunTheWrapper keeps the CI jobs pointed at the wrapper.
// A deleted job is the failure mode this whole bead exists to prevent, and a
// job that stops calling the wrapper loses the require flag with it.
func TestDoltGateJobsStillRunTheWrapper(t *testing.T) {
	t.Parallel()

	for _, workflow := range []string{"pr.yml", "main.yml"} {
		t.Run(workflow, func(t *testing.T) {
			t.Parallel()

			path := filepath.Join("../../.github/workflows", workflow)
			content := readRepoFile(t, path)

			if !strings.Contains(content, "scripts/ci/test-cmd-bd-dolt.sh") {
				t.Errorf("%s no longer runs scripts/ci/test-cmd-bd-dolt.sh; without it no CI "+
					"job exercises the ~150 cmd/bd tests that need a Dolt server, which is "+
					"the state that let them rot to 25 failures (bd-kbx)", path)
			}
			if !strings.Contains(content, "cmd-bd-dolt-known-red.sh check") {
				t.Errorf("%s no longer validates the known-red allowlist; entries could then "+
					"outlive the tests they name and keep real coverage skipped", path)
			}
		})
	}
}
