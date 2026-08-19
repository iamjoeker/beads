package main

import (
	"os"
	"path/filepath"
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
