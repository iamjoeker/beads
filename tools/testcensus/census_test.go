package main

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// packageElapsed matches the "\t0.009s" in a package result line. Under
// coverage the line continues past it ("\tcoverage: 66.7% of statements"), so
// this cannot be anchored to the end — an anchored version normalized nothing
// under -coverprofile and failed the control on two runs' timings alone.
var packageElapsed = regexp.MustCompile(`\t\d+\.\d+s(\t|$)`)

func normalizeElapsed(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = packageElapsed.ReplaceAllString(l, "\tELAPSED$1")
	}
	return strings.Join(lines, "\n")
}

// events joins test2json lines into a stream. The Time and Elapsed fields go
// tool emits are omitted: this reader ignores them, and their absence keeps
// the fixtures readable.
func events(lines ...string) string {
	return strings.Join(lines, "\n") + "\n"
}

func runCensus(t *testing.T, stream string, opts options) (stdout, stderr string, code int) {
	t.Helper()
	var out, errOut bytes.Buffer
	code, err := run(strings.NewReader(stream), &out, &errOut, opts)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	return out.String(), errOut.String(), code
}

// TestPassingPackagePrintsOnlyItsOkLine is the base case: -json forces the
// test binary verbose, and none of that scaffolding may reach the reader.
func TestPassingPackagePrintsOnlyItsOkLine(t *testing.T) {
	stream := events(
		`{"Action":"run","Package":"probe/p","Test":"TestPasses"}`,
		`{"Action":"output","Package":"probe/p","Test":"TestPasses","Output":"=== RUN   TestPasses\n"}`,
		`{"Action":"output","Package":"probe/p","Test":"TestPasses","Output":"    p_test.go:8: logline\n"}`,
		`{"Action":"output","Package":"probe/p","Test":"TestPasses","Output":"--- PASS: TestPasses (0.00s)\n"}`,
		`{"Action":"pass","Package":"probe/p","Test":"TestPasses"}`,
		`{"Action":"output","Package":"probe/p","Output":"PASS\n"}`,
		`{"Action":"output","Package":"probe/p","Output":"ok  \tprobe/p\t0.001s\n"}`,
		`{"Action":"pass","Package":"probe/p"}`,
	)

	stdout, stderr, code := runCensus(t, stream, options{})

	if want := "ok  \tprobe/p\t0.001s\n"; stdout != want {
		t.Errorf("stdout = %q, want %q", stdout, want)
	}
	if code != exitOK {
		t.Errorf("exit = %d, want %d", code, exitOK)
	}
	if !strings.Contains(stderr, "all of them ran") {
		t.Errorf("census should report a clean run, got:\n%s", stderr)
	}
}

// TestVacuousOkIsReported is the defect bd-5er names: every test in the
// package skipped, and the package still says ok.
func TestVacuousOkIsReported(t *testing.T) {
	stdout, stderr, code := runCensus(t, allSkippedStream(), options{trim: "probe"})

	if want := "ok  \tprobe/q\t0.001s\n"; stdout != want {
		t.Errorf("stdout = %q, want %q", stdout, want)
	}
	if code != exitOK {
		t.Errorf("exit = %d, want %d (warn mode never fails the run)", code, exitOK)
	}
	for _, want := range []string{
		"2 top-level tests: 0 ran, 2 SKIPPED",
		"1 package reported \"ok\" having run NO tests at all",
		"BEADS_TEST_EMBEDDED_DOLT",
		"0 ran / 2 skipped",
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("census missing %q, got:\n%s", want, stderr)
		}
	}
}

func TestStrictFailsOnVacuousOk(t *testing.T) {
	_, stderr, code := runCensus(t, allSkippedStream(), options{strict: true})

	if code != exitVacuous {
		t.Fatalf("exit = %d, want %d", code, exitVacuous)
	}
	if !strings.Contains(stderr, "STRICT:") {
		t.Errorf("strict mode should say so, got:\n%s", stderr)
	}
}

// A package where SOME tests ran is not vacuous: its ok still means something,
// and failing it would make the strict mode unusable.
func TestPartiallySkippedPackageIsNotVacuous(t *testing.T) {
	stream := events(
		`{"Action":"run","Package":"probe/p","Test":"TestRuns"}`,
		`{"Action":"output","Package":"probe/p","Test":"TestRuns","Output":"--- PASS: TestRuns (0.00s)\n"}`,
		`{"Action":"pass","Package":"probe/p","Test":"TestRuns"}`,
		`{"Action":"run","Package":"probe/p","Test":"TestSkips"}`,
		`{"Action":"output","Package":"probe/p","Test":"TestSkips","Output":"    p_test.go:9: set BEADS_TEST_EMBEDDED_DOLT=1 to run embedded dolt integration tests\n"}`,
		`{"Action":"output","Package":"probe/p","Test":"TestSkips","Output":"--- SKIP: TestSkips (0.00s)\n"}`,
		`{"Action":"skip","Package":"probe/p","Test":"TestSkips"}`,
		`{"Action":"output","Package":"probe/p","Output":"ok  \tprobe/p\t0.001s\n"}`,
		`{"Action":"pass","Package":"probe/p"}`,
	)

	_, stderr, code := runCensus(t, stream, options{strict: true})

	if code != exitOK {
		t.Errorf("exit = %d, want %d", code, exitOK)
	}
	if strings.Contains(stderr, "having run NO tests") {
		t.Errorf("a partially skipped package is not vacuous, got:\n%s", stderr)
	}
	if !strings.Contains(stderr, "2 top-level tests: 1 ran, 1 SKIPPED") {
		t.Errorf("census should still count the skip, got:\n%s", stderr)
	}
}

// A package with no test files at all is not hiding anything.
func TestNoTestFilesIsNotVacuous(t *testing.T) {
	stream := events(
		`{"Action":"start","Package":"probe/r"}`,
		`{"Action":"output","Package":"probe/r","Output":"?   \tprobe/r\t[no test files]\n"}`,
		`{"Action":"skip","Package":"probe/r"}`,
	)

	stdout, stderr, code := runCensus(t, stream, options{strict: true})

	if want := "?   \tprobe/r\t[no test files]\n"; stdout != want {
		t.Errorf("stdout = %q, want %q", stdout, want)
	}
	if code != exitOK {
		t.Errorf("exit = %d, want %d", code, exitOK)
	}
	if strings.Contains(stderr, "having run NO tests") {
		t.Errorf("[no test files] is not a vacuous ok, got:\n%s", stderr)
	}
}

// A failing package must keep the failure detail and drop the passing and
// skipped tests' noise, and must report the run as red.
func TestFailingPackageKeepsFailureDetail(t *testing.T) {
	stream := events(
		`{"Action":"run","Package":"probe/f","Test":"TestOK"}`,
		`{"Action":"output","Package":"probe/f","Test":"TestOK","Output":"=== RUN   TestOK\n"}`,
		`{"Action":"output","Package":"probe/f","Test":"TestOK","Output":"--- PASS: TestOK (0.00s)\n"}`,
		`{"Action":"pass","Package":"probe/f","Test":"TestOK"}`,
		`{"Action":"run","Package":"probe/f","Test":"TestBad"}`,
		`{"Action":"output","Package":"probe/f","Test":"TestBad","Output":"=== RUN   TestBad\n"}`,
		`{"Action":"output","Package":"probe/f","Test":"TestBad","Output":"    f_test.go:9: boom\n"}`,
		`{"Action":"output","Package":"probe/f","Test":"TestBad","Output":"--- FAIL: TestBad (0.00s)\n"}`,
		`{"Action":"fail","Package":"probe/f","Test":"TestBad"}`,
		`{"Action":"output","Package":"probe/f","Output":"FAIL\n"}`,
		`{"Action":"output","Package":"probe/f","Output":"FAIL\tprobe/f\t0.002s\n"}`,
		`{"Action":"fail","Package":"probe/f"}`,
	)

	stdout, _, code := runCensus(t, stream, options{})

	if code != exitTestsRed {
		t.Errorf("exit = %d, want %d", code, exitTestsRed)
	}
	// Header above its detail, which is the order `go test` uses without -v.
	// The event stream has them the other way round because -json runs the
	// binary verbose.
	want := "--- FAIL: TestBad (0.00s)\n    f_test.go:9: boom\nFAIL\nFAIL\tprobe/f\t0.002s\n"
	if stdout != want {
		t.Errorf("stdout = %q, want %q", stdout, want)
	}
}

// The single place this filter does not reproduce `go test`: a passing test's
// raw stdout inside a FAILING package. Pinned rather than left to be
// rediscovered — if go's behaviour or this filter's changes, one of the two
// halves of this test breaks and says which.
func TestFailingPackageDropsAPassingTestsStdout(t *testing.T) {
	stream := events(
		`{"Action":"run","Package":"probe/f","Test":"TestOK"}`,
		`{"Action":"output","Package":"probe/f","Test":"TestOK","Output":"=== RUN   TestOK\n"}`,
		`{"Action":"output","Package":"probe/f","Test":"TestOK","Output":"stray-from-passing-test\n"}`,
		`{"Action":"output","Package":"probe/f","Test":"TestOK","Output":"--- PASS: TestOK (0.00s)\n"}`,
		`{"Action":"pass","Package":"probe/f","Test":"TestOK"}`,
		`{"Action":"run","Package":"probe/f","Test":"TestBad"}`,
		`{"Action":"output","Package":"probe/f","Test":"TestBad","Output":"    f_test.go:9: boom\n"}`,
		`{"Action":"output","Package":"probe/f","Test":"TestBad","Output":"--- FAIL: TestBad (0.00s)\n"}`,
		`{"Action":"fail","Package":"probe/f","Test":"TestBad"}`,
		`{"Action":"output","Package":"probe/f","Output":"FAIL\tprobe/f\t0.002s\n"}`,
		`{"Action":"fail","Package":"probe/f"}`,
	)

	stdout, _, _ := runCensus(t, stream, options{})

	if strings.Contains(stdout, "stray-from-passing-test") {
		t.Errorf("the known divergence has closed; update the comment on flush() and this test:\n%s", stdout)
	}
	if !strings.Contains(stdout, "f_test.go:9: boom") {
		t.Errorf("the failing test's own detail must survive:\n%s", stdout)
	}
}

// A failing subtest nests under its parent, and both are reordered.
func TestFailingSubtestNestsUnderItsParent(t *testing.T) {
	stream := events(
		`{"Action":"run","Package":"probe/n","Test":"TestParent"}`,
		`{"Action":"output","Package":"probe/n","Test":"TestParent","Output":"=== RUN   TestParent\n"}`,
		`{"Action":"run","Package":"probe/n","Test":"TestParent/sub"}`,
		`{"Action":"output","Package":"probe/n","Test":"TestParent/sub","Output":"=== RUN   TestParent/sub\n"}`,
		// Dedented, as test2json emits it: the subtest's frame indentation is
		// stripped when the line is attributed, so only testing's own
		// four-space log prefix survives and the marker arrives at column 0.
		`{"Action":"output","Package":"probe/n","Test":"TestParent/sub","Output":"    n_test.go:9: boom\n"}`,
		`{"Action":"output","Package":"probe/n","Test":"TestParent/sub","Output":"--- FAIL: TestParent/sub (0.00s)\n"}`,
		`{"Action":"fail","Package":"probe/n","Test":"TestParent/sub"}`,
		`{"Action":"output","Package":"probe/n","Test":"TestParent","Output":"--- FAIL: TestParent (0.00s)\n"}`,
		`{"Action":"fail","Package":"probe/n","Test":"TestParent"}`,
		`{"Action":"output","Package":"probe/n","Output":"FAIL\tprobe/n\t0.002s\n"}`,
		`{"Action":"fail","Package":"probe/n"}`,
	)

	stdout, _, _ := runCensus(t, stream, options{})

	want := "--- FAIL: TestParent (0.00s)\n    --- FAIL: TestParent/sub (0.00s)\n        n_test.go:9: boom\nFAIL\tprobe/n\t0.002s\n"
	if stdout != want {
		t.Errorf("stdout = %q, want %q", stdout, want)
	}
}

// A build failure arrives keyed by ImportPath, before any package event, and
// must reach the reader unchanged.
func TestBuildFailurePassesThrough(t *testing.T) {
	stream := events(
		`{"ImportPath":"probe/b [probe/b.test]","Action":"build-output","Output":"# probe/b [probe/b.test]\n"}`,
		`{"ImportPath":"probe/b [probe/b.test]","Action":"build-output","Output":"b/b_test.go:5:33: undefined: undefinedSymbol\n"}`,
		`{"ImportPath":"probe/b [probe/b.test]","Action":"build-fail"}`,
		`{"Action":"start","Package":"probe/b"}`,
		`{"Action":"output","Package":"probe/b","Output":"FAIL\tprobe/b [build failed]\n"}`,
		`{"Action":"fail","Package":"probe/b","FailedBuild":"probe/b [probe/b.test]"}`,
	)

	stdout, _, code := runCensus(t, stream, options{})

	if code != exitTestsRed {
		t.Errorf("exit = %d, want %d", code, exitTestsRed)
	}
	for _, want := range []string{"undefined: undefinedSymbol", "FAIL\tprobe/b [build failed]"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout missing %q, got:\n%s", want, stdout)
		}
	}
}

// The dangerous case: go test is killed mid-package. Nothing suppressed the
// buffer may be the only record of what the suite was doing.
func TestStrandedPackageIsFlushedAndRed(t *testing.T) {
	stream := events(
		`{"Action":"run","Package":"probe/hang","Test":"TestHangs"}`,
		`{"Action":"output","Package":"probe/hang","Test":"TestHangs","Output":"=== RUN   TestHangs\n"}`,
		`{"Action":"output","Package":"probe/hang","Test":"TestHangs","Output":"panic: test timed out after 25m0s\n"}`,
	)

	stdout, _, code := runCensus(t, stream, options{})

	if code != exitTestsRed {
		t.Errorf("exit = %d, want %d", code, exitTestsRed)
	}
	for _, want := range []string{"=== RUN   TestHangs", "panic: test timed out"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stranded output must survive; missing %q, got:\n%s", want, stdout)
		}
	}
}

// Anything that is not a test2json line is passed through rather than
// swallowed: a dropped line is the same class of silence this tool exists for.
func TestNonJSONLineIsPassedThrough(t *testing.T) {
	stream := events(
		`warning: GOFLAGS is set to something surprising`,
		`{"Action":"output","Package":"probe/p","Output":"ok  \tprobe/p\t0.001s\n"}`,
		`{"Action":"pass","Package":"probe/p"}`,
	)

	stdout, _, _ := runCensus(t, stream, options{})

	if !strings.Contains(stdout, "warning: GOFLAGS is set to something surprising") {
		t.Errorf("stdout dropped a non-JSON line, got:\n%s", stdout)
	}
}

// Skips are grouped by the environment variable their message names, which is
// what turns 283 individual skips into the handful of decisions behind them.
func TestSkipsAreGroupedByGate(t *testing.T) {
	stream := events(
		`{"Action":"run","Package":"probe/g","Test":"TestA"}`,
		`{"Action":"output","Package":"probe/g","Test":"TestA","Output":"    a_test.go:1: set BEADS_TEST_EMBEDDED_DOLT=1 to run embedded dolt integration tests\n"}`,
		`{"Action":"skip","Package":"probe/g","Test":"TestA"}`,
		`{"Action":"run","Package":"probe/g","Test":"TestB"}`,
		`{"Action":"output","Package":"probe/g","Test":"TestB","Output":"    b_test.go:1: set BEADS_TEST_EMBEDDED_DOLT=1 to run embedded dolt tests\n"}`,
		`{"Action":"skip","Package":"probe/g","Test":"TestB"}`,
		`{"Action":"run","Package":"probe/g","Test":"TestC"}`,
		`{"Action":"output","Package":"probe/g","Test":"TestC","Output":"    c_test.go:1: Dolt test server not available\n"}`,
		`{"Action":"skip","Package":"probe/g","Test":"TestC"}`,
		`{"Action":"output","Package":"probe/g","Output":"ok  \tprobe/g\t0.001s\n"}`,
		`{"Action":"pass","Package":"probe/g"}`,
	)

	_, stderr, _ := runCensus(t, stream, options{})

	if !strings.Contains(stderr, "     2  BEADS_TEST_EMBEDDED_DOLT") {
		t.Errorf("two skips should be attributed to the gate, got:\n%s", stderr)
	}
	if !strings.Contains(stderr, "(no environment gate named)") {
		t.Errorf("an unattributed skip should be reported as such, got:\n%s", stderr)
	}
	if !strings.Contains(stderr, "Dolt test server not available") {
		t.Errorf("an unattributed skip should carry an example message, got:\n%s", stderr)
	}
}

// Subtests are not counted: the census must be comparable against the count of
// t.Skip statements in the tree, which are test functions.
func TestSubtestsAreNotCounted(t *testing.T) {
	stream := events(
		`{"Action":"run","Package":"probe/s","Test":"TestParent"}`,
		`{"Action":"run","Package":"probe/s","Test":"TestParent/sub-skip"}`,
		`{"Action":"skip","Package":"probe/s","Test":"TestParent/sub-skip"}`,
		`{"Action":"run","Package":"probe/s","Test":"TestParent/sub-pass"}`,
		`{"Action":"pass","Package":"probe/s","Test":"TestParent/sub-pass"}`,
		`{"Action":"pass","Package":"probe/s","Test":"TestParent"}`,
		`{"Action":"output","Package":"probe/s","Output":"ok  \tprobe/s\t0.001s\n"}`,
		`{"Action":"pass","Package":"probe/s"}`,
	)

	_, stderr, _ := runCensus(t, stream, options{})

	if !strings.Contains(stderr, "1 top-level tests, all of them ran") {
		t.Errorf("only the parent should be counted, got:\n%s", stderr)
	}
}

// The filter reconstructs the NON-verbose view. When the caller asked for -v,
// reconstructing it would delete the output they invoked the flag for.
func TestVerbosePassesEverythingThrough(t *testing.T) {
	stdout, _, _ := runCensus(t, allSkippedStream(), options{verbose: true})

	for _, want := range []string{
		"=== RUN   TestAllSkipA",
		"q_test.go:5: set BEADS_TEST_EMBEDDED_DOLT=1",
		"--- SKIP: TestAllSkipA (0.00s)",
		"PASS\n",
		"ok  \tprobe/q\t0.001s",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("-v output missing %q, got:\n%s", want, stdout)
		}
	}
}

func TestParseArgs(t *testing.T) {
	opts, err := parseArgs([]string{"-strict", "-label", "main suite", "-trim", "example.com/m"})
	if err != nil {
		t.Fatalf("parseArgs: %v", err)
	}
	if !opts.strict || opts.label != "main suite" || opts.trim != "example.com/m" {
		t.Errorf("parseArgs = %+v", opts)
	}

	// A misspelt flag is an error rather than a silent no-op: the caller who
	// typed it believed they had turned something on.
	if _, err := parseArgs([]string{"-strictt"}); err == nil {
		t.Error("parseArgs should reject an unknown flag")
	}
	if _, err := parseArgs([]string{"-label"}); err == nil {
		t.Error("parseArgs should reject a flag with no value")
	}
}

func allSkippedStream() string {
	return events(
		`{"Action":"run","Package":"probe/q","Test":"TestAllSkipA"}`,
		`{"Action":"output","Package":"probe/q","Test":"TestAllSkipA","Output":"=== RUN   TestAllSkipA\n"}`,
		`{"Action":"output","Package":"probe/q","Test":"TestAllSkipA","Output":"    q_test.go:5: set BEADS_TEST_EMBEDDED_DOLT=1 to run embedded dolt integration tests\n"}`,
		`{"Action":"output","Package":"probe/q","Test":"TestAllSkipA","Output":"--- SKIP: TestAllSkipA (0.00s)\n"}`,
		`{"Action":"skip","Package":"probe/q","Test":"TestAllSkipA"}`,
		`{"Action":"run","Package":"probe/q","Test":"TestAllSkipB"}`,
		`{"Action":"output","Package":"probe/q","Test":"TestAllSkipB","Output":"=== RUN   TestAllSkipB\n"}`,
		`{"Action":"output","Package":"probe/q","Test":"TestAllSkipB","Output":"    q_test.go:6: set BEADS_TEST_EMBEDDED_DOLT=1 to run embedded dolt integration tests\n"}`,
		`{"Action":"output","Package":"probe/q","Test":"TestAllSkipB","Output":"--- SKIP: TestAllSkipB (0.00s)\n"}`,
		`{"Action":"skip","Package":"probe/q","Test":"TestAllSkipB"}`,
		`{"Action":"output","Package":"probe/q","Output":"PASS\n"}`,
		`{"Action":"output","Package":"probe/q","Output":"ok  \tprobe/q\t0.001s\n"}`,
		`{"Action":"pass","Package":"probe/q"}`,
	)
}

// TestDifferentialAgainstRealGoTest is the control the fixtures above cannot
// be: it builds a throwaway module, runs `go test` over it BOTH ways, and
// requires this filter's reconstruction to equal what the go tool itself
// prints without -json. A fixture only proves the filter is consistent with
// what its author believed go emits; this proves it against go.
//
// It also plants the positive the whole tool exists for — a package whose
// every test skips — INSIDE the compared tree, so a reconstruction that
// happened to be right about everything else would still fail here.
func TestDifferentialAgainstRealGoTest(t *testing.T) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("go must be on PATH to compare against it: %v", err)
	}

	dir := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		path := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	write("go.mod", "module census.example/probe\n\ngo 1.26\n")
	// Real statements, so the coverage run below reports a percentage rather
	// than "[no statements]" — the two take different paths in `go test`.
	write("mixed/mixed.go", `package mixed

func Double(n int) int {
	if n < 0 {
		return -Double(-n)
	}
	return n * 2
}
`)
	write("mixed/mixed_test.go", `package mixed

import "testing"

func TestPasses(t *testing.T) {
	if Double(2) != 4 {
		t.Fatal("arithmetic")
	}
}
func TestSkips(t *testing.T)   { t.Skip("set BEADS_TEST_EMBEDDED_DOLT=1 to run embedded dolt integration tests") }
func TestSubtests(t *testing.T) {
	t.Run("a", func(t *testing.T) {})
	t.Run("b", func(t *testing.T) { t.Skip("nested") })
}
`)
	write("allskipped/allskipped_test.go", `package allskipped

import "testing"

func TestOne(t *testing.T) { t.Skip("set BEADS_TEST_EMBEDDED_DOLT=1 to run embedded dolt integration tests") }
func TestTwo(t *testing.T) { t.Skip("set BEADS_TEST_EMBEDDED_DOLT=1 to run embedded dolt integration tests") }
`)
	write("notests/notests.go", "package notests\n")

	// -p 1 -count=1 so the two runs order packages identically and neither
	// is served from the test cache.
	goTest := func(extra ...string) string {
		t.Helper()
		args := append([]string{"test", "-p", "1", "-count=1"}, extra...)
		args = append(args, "./...")
		cmd := exec.Command(goBin, args...)
		cmd.Dir = dir
		// A parent test run may have narrowed or redirected the child;
		// GOFLAGS is the one that would silently apply -run here.
		cmd.Env = append(os.Environ(), "GOFLAGS=")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("go test %v: %v\n%s", args, err, out)
		}
		return string(out)
	}

	plain := goTest()
	stream := goTest("-json")

	var got, censusOut bytes.Buffer
	code, err := run(strings.NewReader(stream), &got, &censusOut, options{trim: "census.example/probe"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if code != exitOK {
		t.Fatalf("exit = %d, want %d", code, exitOK)
	}

	// And the same control under coverage, which is how `make test` runs.
	// `go test` folds the percentage into its result line and DISCARDS the
	// standalone "coverage: N% of statements" line the binary emits; a filter
	// that passed package-scope output through wholesale would print both.
	coverPlain := goTest("-covermode=atomic", "-coverprofile="+filepath.Join(dir, "plain.cover"))
	coverStream := goTest("-covermode=atomic", "-coverprofile="+filepath.Join(dir, "json.cover"), "-json")
	var gotCover bytes.Buffer
	if _, err := run(strings.NewReader(coverStream), &gotCover, io.Discard, options{}); err != nil {
		t.Fatalf("run -cover: %v", err)
	}
	if !strings.Contains(coverPlain, "% of statements") {
		t.Fatalf("the coverage control needs a real percentage, got:\n%s", coverPlain)
	}
	if normalizeElapsed(gotCover.String()) != normalizeElapsed(coverPlain) {
		t.Errorf("coverage reconstruction differs from `go test`\n--- go test ---\n%s\n--- testcensus ---\n%s",
			coverPlain, gotCover.String())
	}

	// The same control for -v: what the filter emits from a -json stream must
	// equal what `go test -v` prints, or the flag has been quietly disarmed.
	verbosePlain := goTest("-v")
	verboseStream := goTest("-json", "-v")
	var gotVerbose bytes.Buffer
	if _, err := run(strings.NewReader(verboseStream), &gotVerbose, io.Discard, options{verbose: true}); err != nil {
		t.Fatalf("run -v: %v", err)
	}
	if normalizeElapsed(gotVerbose.String()) != normalizeElapsed(verbosePlain) {
		t.Errorf("-v reconstruction differs from `go test -v`\n--- go test -v ---\n%s\n--- testcensus -v ---\n%s",
			verbosePlain, gotVerbose.String())
	}

	// Two runs of the same package do not take the same number of
	// milliseconds; nothing else in these lines is allowed to differ.
	if normalizeElapsed(got.String()) != normalizeElapsed(plain) {
		t.Errorf("reconstruction differs from `go test`\n--- go test ---\n%s\n--- testcensus ---\n%s", plain, got.String())
	}

	// The planted positive: `go test` called this green and said nothing.
	if !strings.Contains(plain, "ok") || strings.Contains(plain, "SKIP") {
		t.Fatalf("the control is only meaningful if go test stayed silent about the skips, got:\n%s", plain)
	}
	for _, want := range []string{
		"1 package reported \"ok\" having run NO tests at all",
		"allskipped",
		"BEADS_TEST_EMBEDDED_DOLT",
	} {
		if !strings.Contains(censusOut.String(), want) {
			t.Errorf("census missing %q, got:\n%s", want, censusOut.String())
		}
	}
}

// The same control over a RED tree, which is the half that matters most: a
// filter that mangles a failure report costs more than one that miscounts
// skips. Reordering a failing test's block is the specific risk — -json runs
// the binary verbose, where the detail precedes the "--- FAIL:" header and
// without -v it follows it.
func TestDifferentialAgainstRealGoTestWhenRed(t *testing.T) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("go must be on PATH to compare against it: %v", err)
	}

	dir := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		path := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	write("go.mod", "module census.example/red\n\ngo 1.26\n")
	write("red/red_test.go", `package red

import "testing"

func TestPasses(t *testing.T) { t.Log("a passing test's log is not the story") }
func TestSkips(t *testing.T)  { t.Skip("set BEADS_TEST_EMBEDDED_DOLT=1 to run embedded dolt integration tests") }
func TestFailsFlat(t *testing.T) {
	t.Log("context before the failure")
	t.Errorf("flat boom")
}
func TestFailsNested(t *testing.T) {
	t.Run("inner", func(t *testing.T) { t.Fatalf("nested boom") })
}
func TestFailsTwoDeep(t *testing.T) {
	t.Run("inner", func(t *testing.T) {
		t.Run("deeper", func(t *testing.T) { t.Fatalf("two levels down") })
	})
}
`)
	write("green/green_test.go", `package green

import "testing"

func TestFine(t *testing.T) {}
`)

	goTest := func(extra ...string) string {
		t.Helper()
		args := append([]string{"test", "-p", "1", "-count=1"}, extra...)
		args = append(args, "./...")
		cmd := exec.Command(goBin, args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GOFLAGS=")
		out, err := cmd.CombinedOutput()
		if err == nil {
			t.Fatalf("the fixture must fail, or this control proves nothing:\n%s", out)
		}
		return string(out)
	}

	plain := goTest()
	stream := goTest("-json")

	var got bytes.Buffer
	code, err := run(strings.NewReader(stream), &got, io.Discard, options{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if code != exitTestsRed {
		t.Fatalf("exit = %d, want %d", code, exitTestsRed)
	}

	// `go test` writes a final bare "FAIL" itself, after the last package.
	// It is the go command's own line and never appears in the event stream.
	wantOut := strings.TrimSuffix(plain, "FAIL\n")

	if normalizeElapsed(got.String()) != normalizeElapsed(wantOut) {
		t.Errorf("red reconstruction differs from `go test`\n--- go test ---\n%s\n--- testcensus ---\n%s", wantOut, got.String())
	}

	// -v over a RED tree is where passthrough stops matching `go test -v`, and
	// the divergence is pinned here rather than left to be rediscovered.
	// -test.v=test2json is its own verbosity mode: it emits a nested failure
	// flat and innermost-first, where `go test -v` prints the tree outermost-
	// first and indented. Everything is present either way, which is what -v
	// is for; reproducing testing's tree printer is not worth owning.
	verboseStream := goTest("-json", "-v")
	var gotVerbose bytes.Buffer
	if _, err := run(strings.NewReader(verboseStream), &gotVerbose, io.Discard, options{verbose: true}); err != nil {
		t.Fatalf("run -v: %v", err)
	}
	flat := "--- FAIL: TestFailsTwoDeep/inner/deeper (0.00s)\n"
	if !strings.Contains(gotVerbose.String(), flat) {
		t.Errorf("expected test2json's flat verbose form to pass through unchanged; got:\n%s", gotVerbose.String())
	}
	if !strings.Contains(gotVerbose.String(), "two levels down") {
		t.Errorf("-v must still carry the failure detail:\n%s", gotVerbose.String())
	}
}
