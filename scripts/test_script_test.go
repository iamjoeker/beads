package scripts_test

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

const (
	testScriptFakeGoLogEnv      = "BEADS_TEST_SCRIPT_FAKE_GO_LOG"
	testScriptExpectedBinaryEnv = "BEADS_TEST_SCRIPT_EXPECTED_BINARY"
	testScriptExpectedBaseEnv   = "BEADS_TEST_SCRIPT_EXPECTED_BASENAME"
	testScriptDriverEnv         = "BEADS_TEST_SCRIPT_DRIVER"
	testScriptNativeSuffixEnv   = "BEADS_TEST_SCRIPT_NATIVE_SUFFIX"
	testScriptLaunchProbeEnv    = "BEADS_TEST_SCRIPT_LAUNCH_PROBE"
	testScriptBuildOutputEnv    = "BEADS_TEST_SCRIPT_BUILD_OUTPUT"
)

// beadsTestEnvStamp mirrors BEADS_TEST_ENV_STAMP in scripts/ci/lib/test-env.sh:
// the per-root ownership marker whose presence means "this root is live".
const beadsTestEnvStamp = ".beads-test-env-owner"

const testScriptFakeGo = `#!/usr/bin/env bash
set -euo pipefail

record() {
    printf '%s\n' "$1" >>"$BEADS_TEST_SCRIPT_FAKE_GO_LOG"
}

case "${1:-}" in
    env)
        record env
        if [[ $# -ne 2 || "$2" != "GOEXE" ]]; then
            printf 'fake go: unsupported env arguments: %s\n' "$*" >&2
            exit 90
        fi
        printf '%s\n' "$BEADS_TEST_SCRIPT_NATIVE_SUFFIX"
        ;;
    build)
        record build
        shift
        output=""
        while [[ $# -gt 0 ]]; do
            if [[ "$1" == "-o" ]]; then
                if [[ $# -lt 2 ]]; then
                    printf 'fake go: -o is missing its output\n' >&2
                    exit 90
                fi
                output="$2"
                shift 2
            else
                shift
            fi
        done
        if [[ -z "$output" ]]; then
            printf 'fake go: build is missing its output\n' >&2
            exit 90
        fi
        printf '%s\n' "$output" >>"$BEADS_TEST_SCRIPT_BUILD_OUTPUT"
        # An empty expectation means the caller asserts on the recorded output
        # instead: the test.sh fallback path builds into a mktemp directory
        # whose name it cannot know in advance.
        if [[ -n "$BEADS_TEST_SCRIPT_EXPECTED_BINARY" && "$output" != "$BEADS_TEST_SCRIPT_EXPECTED_BINARY" ]]; then
            printf 'fake go: build output %q, want %q\n' "$output" "$BEADS_TEST_SCRIPT_EXPECTED_BINARY" >&2
            exit 90
        fi
        cp -f -- "$BEADS_TEST_SCRIPT_DRIVER" "$output"
        chmod +x "$output"
        ;;
    test)
        record test
        "$BEADS_TEST_SCRIPT_DRIVER" \
            -test.run '^TestTestScriptPrebuiltBinaryLaunchProbe$' \
            -test.count=1
        ;;
    *)
        printf 'fake go: unsupported command: %s\n' "$*" >&2
        exit 90
        ;;
esac
`

func TestTestScriptPrebuiltBinaryContract(t *testing.T) {
	t.Run("generated path uses the native executable suffix and launches", func(t *testing.T) {
		commands := runTestScriptWithFakeGo(t, "")
		assertFakeGoCommands(t, commands, "env", "build", "test")
	})

	t.Run("caller supplied binary wins without a build", func(t *testing.T) {
		fixtureRoot := filepath.Join(t.TempDir(), "caller override with spaces")
		if err := os.MkdirAll(fixtureRoot, 0o755); err != nil {
			t.Fatalf("create caller fixture root: %v", err)
		}
		callerBinary := filepath.Join(fixtureRoot, "caller supplied bd"+nativeExecutableSuffix())
		copyCurrentTestExecutable(t, callerBinary)

		commands := runTestScriptWithFakeGo(t, callerBinary)
		assertFakeGoCommands(t, commands, "test")
	})
}

// TestTestScriptDoesNotResurrectACleanedRoot pins the consumer half of bd-iik.
//
// test.sh used to `mkdir -p "$BEADS_TEST_ENV_ROOT/prebuilt-bd"` on whatever
// root it found in the environment. When that root belonged to a shell that
// had already cleaned up, the mkdir wrote it back into existence and a ~200 MB
// bd binary landed in a directory nothing would ever remove again. On the host
// that produced bd-iik, 49 of 63 stranded roots had been removed and then
// re-created that way.
func TestTestScriptDoesNotResurrectACleanedRoot(t *testing.T) {
	result := runTestScript(t, testScriptRun{cleanedRoot: true, disableTestEnv: true})

	assertFakeGoCommands(t, result.commands, "env", "build", "test")

	if _, err := os.Stat(result.testEnvRoot); !os.IsNotExist(err) {
		t.Fatalf("cleaned root %s was re-created (stat error %v)", result.testEnvRoot, err)
	}
	if len(result.buildOutputs) != 1 {
		t.Fatalf("fake-go build outputs = %q, want exactly one", result.buildOutputs)
	}
	built := filepath.FromSlash(result.buildOutputs[0])
	if strings.HasPrefix(built, filepath.Clean(result.testEnvRoot)+string(filepath.Separator)) {
		t.Fatalf("prebuild wrote into the cleaned root: %s", built)
	}
	// The fallback directory is test.sh's own, so test.sh must remove it too:
	// trading one stranded root for another would not be a fix.
	entries, err := os.ReadDir(result.tempRoot)
	if err != nil {
		t.Fatalf("read fallback temp root %s: %v", result.tempRoot, err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Fatalf("test.sh left %v behind in %s", names, result.tempRoot)
	}
}

// TestTestScriptPrebuiltBinaryLaunchProbe is selected only by the fake go test
// process above. Keeping the os/exec probe in a normal test avoids claiming the
// package-wide TestMain authority needed by other script-selection contracts.
func TestTestScriptPrebuiltBinaryLaunchProbe(t *testing.T) {
	if os.Getenv(testScriptLaunchProbeEnv) != "1" {
		t.Skip("re-exec probe runs only under the test.sh fake-go driver")
	}

	prebuilt := os.Getenv("BEADS_TEST_BD_BINARY")
	expected := os.Getenv(testScriptExpectedBinaryEnv)
	if prebuilt == "" || expected == "" || !sameTestScriptFile(prebuilt, expected) {
		t.Fatalf("exported prebuilt binary %q is not expected file %q", prebuilt, expected)
	}
	if want := os.Getenv(testScriptExpectedBaseEnv); filepath.Base(prebuilt) != want {
		t.Fatalf("exported prebuilt basename = %q, want %q", filepath.Base(prebuilt), want)
	}

	command := exec.Command(prebuilt, "-test.run=^$")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("launch exported prebuilt binary through os/exec: %v\n%s", err, output)
	}
}

// testScriptRun configures one scripts/test.sh invocation against the fake go.
type testScriptRun struct {
	// callerBinary is exported as BEADS_TEST_BD_BINARY when non-empty.
	callerBinary string
	// cleanedRoot leaves the inherited BEADS_TEST_ENV_ROOT nonexistent instead
	// of creating and stamping it: a root whose owning shell already cleaned
	// up. test.sh must not write it back into existence (bd-iik).
	cleanedRoot bool
	// disableTestEnv exports BEADS_TEST_ENV_DISABLE=1, which makes
	// beads_test_env_enter a no-op so test.sh alone decides what to do with
	// the root it inherited.
	disableTestEnv bool
}

// testScriptResult reports what the fake go saw during one invocation.
type testScriptResult struct {
	commands     []string
	buildOutputs []string
	testEnvRoot  string
	tempRoot     string
}

func runTestScriptWithFakeGo(t *testing.T, callerBinary string) []string {
	t.Helper()
	return runTestScript(t, testScriptRun{callerBinary: callerBinary}).commands
}

func runTestScript(t *testing.T, run testScriptRun) testScriptResult {
	t.Helper()

	root := filepath.Join(t.TempDir(), "test script root with spaces")
	fakeBin := filepath.Join(root, "fake go bin")
	testEnvRoot := filepath.Join(root, "isolated test environment")
	tempRoot := filepath.Join(root, "temporary files")
	create := []string{fakeBin, tempRoot}
	if !run.cleanedRoot {
		create = append(create, testEnvRoot)
	}
	for _, path := range create {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatalf("create fixture directory %s: %v", path, err)
		}
	}
	if !run.cleanedRoot {
		// The fixture stands in for a root an outer wrapper created and still
		// owns, so it needs that wrapper's ownership stamp: test.sh builds
		// into an inherited BEADS_TEST_ENV_ROOT only while the root is stamped
		// (bd-iik).
		writeTestEnvStamp(t, testEnvRoot, 1)
	}

	fakeGo := filepath.Join(fakeBin, "go")
	if err := os.WriteFile(fakeGo, []byte(testScriptFakeGo), 0o755); err != nil {
		t.Fatalf("write fake go: %v", err)
	}
	callLog := filepath.Join(root, "fake go calls")
	buildLog := filepath.Join(root, "fake go build outputs")
	for _, path := range []string{callLog, buildLog} {
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatalf("initialize fake-go log %s: %v", path, err)
		}
	}

	// A cleaned root sends the prebuild down test.sh's mktemp fallback, whose
	// path no fixture can predict; the caller asserts on buildOutputs instead.
	expected := run.callerBinary
	if expected == "" && !run.cleanedRoot {
		expected = filepath.Join(testEnvRoot, "prebuilt-bd", "bd"+nativeExecutableSuffix())
	}

	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Fatalf("bash is required to exercise scripts/test.sh: %v", err)
	}
	repoRoot := sourceRepoRoot(t)
	env := testScriptEnvironment(testEnvRoot, tempRoot, expected, run)
	fakeBinShellPath := shellPathUnderEnv(t, bash, fakeBin, env)
	fakeGoShellPath := shellPathUnderEnv(t, bash, fakeGo, env)
	driverShellPath := shellPathUnderEnv(t, bash, currentTestExecutable(t), env)
	callLogShellPath := shellPathUnderEnv(t, bash, callLog, env)
	buildLogShellPath := shellPathUnderEnv(t, bash, buildLog, env)
	env = append(env,
		"BEADS_TEST_COMMAND_PATH="+fakeBinShellPath+":/usr/bin:/bin",
		testScriptDriverEnv+"="+driverShellPath,
		testScriptFakeGoLogEnv+"="+callLogShellPath,
		testScriptBuildOutputEnv+"="+buildLogShellPath,
	)

	cmd := exec.Command(
		bash,
		"--noprofile",
		"--norc",
		"-c",
		`PATH="$BEADS_TEST_COMMAND_PATH"; export PATH; exec "$BASH" --noprofile --norc "$1" "$2"`,
		"test-script",
		shellPathUnderEnv(t, bash, filepath.Join(repoRoot, "scripts", "test.sh"), env),
		"./cmd/bd",
	)
	cmd.Dir = repoRoot
	cmd.Env = env
	requireShellCommandPath(t, bash, repoRoot, env, "go", fakeGoShellPath)
	output, runErr := cmd.CombinedOutput()
	if runErr != nil {
		t.Fatalf("scripts/test.sh failed: %v\n%s", runErr, output)
	}

	return testScriptResult{
		commands:     readTestScriptLog(t, callLog),
		buildOutputs: readTestScriptLog(t, buildLog),
		testEnvRoot:  testEnvRoot,
		tempRoot:     tempRoot,
	}
}

func readTestScriptLog(t *testing.T, path string) []string {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fake-go log %s: %v", path, err)
	}
	var lines []string
	for _, line := range strings.Split(string(content), "\n") {
		if line = strings.TrimRight(line, "\r"); line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

func testScriptEnvironment(testEnvRoot string, tempRoot string, expected string, run testScriptRun) []string {
	home := filepath.Join(testEnvRoot, "home")
	expectedBinary := ""
	expectedBase := ""
	launchProbe := "0"
	if expected != "" {
		expectedBinary = portableTestScriptPath(expected)
		expectedBase = filepath.Base(expected)
		launchProbe = "1"
	}
	env := []string{
		"PATH=/usr/bin:/bin",
		"HOME=" + portableTestScriptPath(home),
		"USERPROFILE=" + portableTestScriptPath(home),
		"TMPDIR=" + portableTestScriptPath(tempRoot),
		"TEMP=" + portableTestScriptPath(tempRoot),
		"TMP=" + portableTestScriptPath(tempRoot),
		"LC_ALL=C",
		"LANG=C",
		"BASH_ENV=",
		"ENV=",
		"CGO_ENABLED=1",
		"GOFLAGS=",
		"BEADS_TEST_ENV_ACTIVE=1",
		"BEADS_TEST_ENV_ROOT=" + portableTestScriptPath(testEnvRoot),
		testScriptExpectedBinaryEnv + "=" + expectedBinary,
		testScriptExpectedBaseEnv + "=" + expectedBase,
		testScriptNativeSuffixEnv + "=" + nativeExecutableSuffix(),
		testScriptLaunchProbeEnv + "=" + launchProbe,
	}
	if run.callerBinary != "" {
		env = append(env, "BEADS_TEST_BD_BINARY="+portableTestScriptPath(run.callerBinary))
	}
	if run.disableTestEnv {
		env = append(env, "BEADS_TEST_ENV_DISABLE=1")
	}
	for _, key := range []string{"SYSTEMROOT", "WINDIR", "COMSPEC", "PATHEXT"} {
		if value, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+value)
		}
	}
	return env
}

// writeTestEnvStamp marks root live for scripts/ci/lib/test-env.sh by writing
// the ownership stamp that beads_test_env_enter writes at create time.
func writeTestEnvStamp(t *testing.T, root string, ownerPID int) {
	t.Helper()
	stamp := filepath.Join(root, beadsTestEnvStamp)
	if err := os.WriteFile(stamp, []byte(strconv.Itoa(ownerPID)+"\n"), 0o600); err != nil {
		t.Fatalf("write test-env ownership stamp %s: %v", stamp, err)
	}
}

func assertFakeGoCommands(t *testing.T, commands []string, want ...string) {
	t.Helper()
	if strings.Join(commands, " ") != strings.Join(want, " ") {
		t.Fatalf("fake-go commands = %q, want %q", commands, want)
	}
}

func copyCurrentTestExecutable(t *testing.T, destination string) {
	t.Helper()
	input, err := os.Open(currentTestExecutable(t))
	if err != nil {
		t.Fatalf("open current test executable: %v", err)
	}
	defer input.Close()

	output, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o755)
	if err != nil {
		t.Fatalf("create native test executable: %v", err)
	}
	if _, err := io.Copy(output, input); err != nil {
		_ = output.Close()
		t.Fatalf("copy native test executable: %v", err)
	}
	if err := output.Close(); err != nil {
		t.Fatalf("close native test executable: %v", err)
	}
}

func currentTestExecutable(t *testing.T) string {
	t.Helper()
	path, err := os.Executable()
	if err != nil {
		t.Fatalf("resolve current test executable: %v", err)
	}
	return path
}

func sameTestScriptFile(first string, second string) bool {
	firstInfo, firstErr := os.Stat(first)
	secondInfo, secondErr := os.Stat(second)
	return firstErr == nil && secondErr == nil && os.SameFile(firstInfo, secondInfo)
}

func nativeExecutableSuffix() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}

func portableTestScriptPath(path string) string {
	return filepath.ToSlash(filepath.Clean(path))
}
