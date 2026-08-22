package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Contract tests for scripts/ci/lib/test-env.sh's root ownership rules (bd-iik).
//
// The library hands every broad test wrapper a hermetic root under TMPDIR and
// exports HOME, DOLT_ROOT_PATH, XDG_CONFIG_HOME and GIT_CONFIG_GLOBAL as paths
// INSIDE it. That makes the root's lifetime load-bearing in two directions:
//
//   - whoever removes it must be its owner, or a nested wrapper deletes a live
//     run's environment out from under it;
//   - once it is removed, nothing may write it back, or the run that inherited
//     the environment silently continues against a fresh empty directory that
//     nothing will ever clean up.
//
// On the host that produced bd-iik, 49 of 63 stranded /tmp/beads-test-env-*
// roots were mode 0755 rather than mktemp's 0700 — removed, then re-created.
//
// These probe POSIX process and permission behaviour directly, so they run
// only where bash and a POSIX filesystem are the real thing.

const testEnvLibRelPath = "ci/lib/test-env.sh"

func requireTestEnvLibShell(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("test-env.sh ownership rules are probed through POSIX modes and process reaping")
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skipf("bash is required to exercise %s: %v", testEnvLibRelPath, err)
	}
	return bash
}

// runTestEnvShell runs body in a bash shell that has sourced the library, with
// tmpDir as TMPDIR so every root the library creates lands somewhere the test
// owns. It returns the shell's combined output.
func runTestEnvShell(t *testing.T, tmpDir string, extraEnv []string, body string) string {
	t.Helper()
	bash := requireTestEnvLibShell(t)
	scriptsDir := filepath.Join(sourceRepoRoot(t), "scripts")

	script := "set -euo pipefail\n" +
		"source " + shSingleQuote(filepath.Join(scriptsDir, testEnvLibRelPath)) + "\n" +
		body

	// These tests almost certainly run UNDER a wrapper that already called
	// beads_test_env_enter, so the Go process carries BEADS_TEST_ENV_ACTIVE=1
	// and a live BEADS_TEST_ENV_ROOT. Inheriting those would make every
	// fixture reuse — and try to clean up — the surrounding run's own root.
	// Drop the whole namespace and let each case declare what it needs.
	env := make([]string, 0, len(os.Environ()))
	for _, entry := range os.Environ() {
		if strings.HasPrefix(entry, "BEADS_TEST_ENV_") {
			continue
		}
		env = append(env, entry)
	}
	env = append(env, "TMPDIR="+tmpDir, "BEADS_TEST_ENV_KEEP=0")

	cmd := exec.Command(bash, "--noprofile", "--norc", "-c", script)
	cmd.Dir = scriptsDir
	cmd.Env = append(env, extraEnv...)

	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("test-env shell failed: %v\n%s", err, output)
	}
	return string(output)
}

// testEnvRootsIn lists the beads-test-env-* roots left under dir.
func testEnvRootsIn(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	var roots []string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "beads-test-env-") {
			roots = append(roots, filepath.Join(dir, entry.Name()))
		}
	}
	return roots
}

func TestTestEnvEnterCreatesAnOwnedPrivateRoot(t *testing.T) {
	tmpDir := t.TempDir()
	output := runTestEnvShell(t, tmpDir, nil, `
beads_test_env_enter
printf 'root=%s\n' "$BEADS_TEST_ENV_ROOT"
printf 'mode=%s\n' "$(stat -c %a "$BEADS_TEST_ENV_ROOT" 2>/dev/null || stat -f %Lp "$BEADS_TEST_ENV_ROOT")"
printf 'owner=%s\n' "$(beads_test_env_owner)"
printf 'self=%s\n' "$$"
printf 'live=%s\n' "$(beads_test_env_root_is_live && echo yes || echo no)"
`)

	fields := parseKeyValueOutput(output)
	if got := fields["mode"]; got != "700" {
		// mktemp -d creates 0700; a 0755 root is one mkdir -p wrote back.
		t.Errorf("root mode = %q, want %q", got, "700")
	}
	if fields["owner"] != fields["self"] || fields["owner"] == "" {
		t.Errorf("ownership stamp = %q, want the creating shell's PID %q", fields["owner"], fields["self"])
	}
	if fields["live"] != "yes" {
		t.Errorf("beads_test_env_root_is_live said %q for a root it just created", fields["live"])
	}
	if _, err := os.Stat(fields["root"]); !os.IsNotExist(err) {
		t.Errorf("root %s outlived its owning shell (stat error %v)", fields["root"], err)
	}
}

func TestTestEnvEnterReplacesAnInheritedRootThatWasAlreadyCleaned(t *testing.T) {
	tmpDir := t.TempDir()
	grave := filepath.Join(tmpDir, "beads-test-env-GRAVE0")

	// A shell that inherits BEADS_TEST_ENV_ACTIVE=1 from an owner that has
	// already exited holds an environment pointing entirely at deleted paths.
	// Reusing it is not hermetic and re-creating it strands a root, so enter
	// must set up a replacement it owns.
	output := runTestEnvShell(t, tmpDir, []string{
		"BEADS_TEST_ENV_ACTIVE=1",
		"BEADS_TEST_ENV_ROOT=" + grave,
	}, `
beads_test_env_enter
printf 'root=%s\n' "$BEADS_TEST_ENV_ROOT"
printf 'home=%s\n' "$HOME"
printf 'doltroot=%s\n' "$DOLT_ROOT_PATH"
printf 'owner=%s\n' "$(beads_test_env_owner)"
printf 'self=%s\n' "$$"
`)

	fields := parseKeyValueOutput(output)
	if fields["root"] == grave {
		t.Fatalf("enter kept the cleaned root %s", grave)
	}
	if fields["owner"] != fields["self"] {
		t.Errorf("replacement root stamp = %q, want the entering shell %q", fields["owner"], fields["self"])
	}
	for _, key := range []string{"home", "doltroot"} {
		if !strings.HasPrefix(fields[key], fields["root"]+string(filepath.Separator)) {
			t.Errorf("%s = %q, want a path inside the replacement root %q", key, fields[key], fields["root"])
		}
	}
	if _, err := os.Stat(grave); !os.IsNotExist(err) {
		t.Errorf("cleaned root %s was re-created (stat error %v)", grave, err)
	}
	if roots := testEnvRootsIn(t, tmpDir); len(roots) != 0 {
		t.Errorf("stranded roots after the shell exited: %v", roots)
	}
}

func TestTestEnvCleanupSparesARootItDoesNotOwn(t *testing.T) {
	tmpDir := t.TempDir()

	// The nested shell is a wrapper invoked from inside an outer wrapper: it
	// inherits BEADS_TEST_ENV_ROOT and runs its own cleanup on exit. Only the
	// creating shell may remove the root.
	output := runTestEnvShell(t, tmpDir, nil, `
beads_test_env_enter
outer="$BEADS_TEST_ENV_ROOT"
printf 'outer=%s\n' "$outer"
printf 'outerpid=%s\n' "$$"

bash --noprofile --norc -c '
set -euo pipefail
source "$1"
beads_test_env_enter
printf "nested=%s\n" "$BEADS_TEST_ENV_ROOT"
printf "nestedpid=%s\n" "$$"
beads_test_env_cleanup
' nested-wrapper "$PWD/ci/lib/test-env.sh"

printf 'survived=%s\n' "$(beads_test_env_root_is_live && echo yes || echo no)"
`)

	fields := parseKeyValueOutput(output)
	if fields["nested"] != fields["outer"] {
		t.Fatalf("nested wrapper used root %q, want the inherited %q", fields["nested"], fields["outer"])
	}
	if fields["nestedpid"] == fields["outerpid"] {
		t.Fatalf("nested wrapper shared the outer PID %q; the fixture is not exercising two shells", fields["outerpid"])
	}
	if fields["survived"] != "yes" {
		t.Errorf("nested wrapper's cleanup deleted the outer run's live root %s", fields["outer"])
	}
	if roots := testEnvRootsIn(t, tmpDir); len(roots) != 0 {
		t.Errorf("owner failed to remove its root on exit: %v", roots)
	}
}

// stragglerScript spawns a reparented grandchild that holds the wrapper's
// environment, waits for the test to release it, and then writes inside
// DOLT_ROOT_PATH — the dolt subprocess of bd-iik, which re-created a root that
// had already been removed.
const stragglerScript = `
beads_test_env_enter
printf 'root=%s\n' "$BEADS_TEST_ENV_ROOT"

# Double fork: the intermediate shell exits immediately, so the straggler is
# reparented to init and is no longer a descendant of this wrapper. A process
# tree walk cannot find it; only its environment still names the root.
bash --noprofile --norc -c '
(
    for _ in $(seq 1 300); do
        if [ -e "$BEADS_STRAGGLER_RELEASE" ]; then
            mkdir -p "$DOLT_ROOT_PATH/.dolt"
            : >"$DOLT_ROOT_PATH/.dolt/config_global.json"
            : >"$BEADS_STRAGGLER_DONE"
            exit 0
        fi
        sleep 0.1
    done
) &
' straggler >/dev/null 2>&1 &

# Give the straggler time to exist before this shell exits and cleans up.
sleep 0.5
`

func TestTestEnvCleanupReapsHoldersSoTheRootStaysRemoved(t *testing.T) {
	for _, tc := range []struct {
		name            string
		env             []string
		wantResurrected bool
	}{
		{
			// Control: proves the fixture really does reproduce the bug, so a
			// passing reaping case below is evidence and not a no-op.
			name:            "reaping disabled resurrects the root",
			env:             []string{"BEADS_TEST_ENV_NO_REAP=1"},
			wantResurrected: true,
		},
		{
			name:            "reaping kills the holder first",
			env:             nil,
			wantResurrected: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if runtime.GOOS != "linux" {
				t.Skip("reaping discovers holders through /proc/<pid>/environ")
			}
			tmpDir := t.TempDir()
			signals := t.TempDir()
			release := filepath.Join(signals, "release")
			done := filepath.Join(signals, "done")

			// Deliberately outside the BEADS_TEST_ENV_* namespace that
			// runTestEnvShell strips, so the straggler always sees them.
			env := append([]string{
				"BEADS_STRAGGLER_RELEASE=" + release,
				"BEADS_STRAGGLER_DONE=" + done,
			}, tc.env...)

			output := runTestEnvShell(t, tmpDir, env, stragglerScript)
			root := parseKeyValueOutput(output)["root"]
			if root == "" {
				t.Fatalf("wrapper did not report its root:\n%s", output)
			}
			if _, err := os.Stat(root); !os.IsNotExist(err) {
				t.Fatalf("owner did not remove %s on exit (stat error %v)", root, err)
			}

			// Release the straggler only now: if it survived cleanup it writes
			// the root back, and if it was reaped nothing ever happens.
			if err := os.WriteFile(release, nil, 0o600); err != nil {
				t.Fatalf("release straggler: %v", err)
			}

			resurrected := waitForPath(root, 5*time.Second)
			if resurrected != tc.wantResurrected {
				t.Fatalf("root re-created = %v, want %v (straggler finished = %v)",
					resurrected, tc.wantResurrected, pathExists(done))
			}
		})
	}
}

func waitForPath(path string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if pathExists(path) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// parseKeyValueOutput collects the `key=value` lines a fixture shell printed,
// ignoring anything else on the stream.
func parseKeyValueOutput(output string) map[string]string {
	fields := map[string]string{}
	for _, line := range strings.Split(output, "\n") {
		key, value, ok := strings.Cut(strings.TrimRight(line, "\r"), "=")
		if ok && key != "" && !strings.ContainsAny(key, " \t") {
			fields[key] = value
		}
	}
	return fields
}
