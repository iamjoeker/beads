package main

import (
	"flag"
	"os"
	"testing"
)

// realStdout/realStderr are captured at package-variable initialization, before
// any test runs, so they are the process's genuine streams.
var (
	realStdout = os.Stdout
	realStderr = os.Stderr
)

// expectedStderr is what os.Stderr should hold while tests run, which is not
// always what it held at package init (bd-5er).
//
// Under `go test -json` the go command runs the binary with
// -test.v=test2json, and testing.M.Run then points os.Stderr AT os.Stdout so
// that stderr writes join the single JSON stream. It never restores it.
// Measured on go1.26.6 in a two-file module with no beads code in it:
//
//	TESTMAIN-BEFORE stderr fd=2 name="/dev/stderr"    (before m.Run)
//	inside a test   stderr fd=1 name="/dev/stdout"
//	TESTMAIN-AFTER  stderr fd=1 name="/dev/stdout"    (after m.Run returns)
//
// So a baseline captured at package init — the only place it CAN be captured,
// since m.Run does the swap — disagrees with every test in the package, and
// this guard failed for every run under -json whatever the tests did. That is
// a property of the runner, not a leak, and grading it as one made the guard
// unusable under the mode scripts/test.sh now uses to count skips.
//
// The narrow cost: under -json a test that leaks exactly `os.Stderr =
// os.Stdout` is indistinguishable from the runner's own substitution and goes
// uncaught. A leak to anything else is still caught in both modes, and so is
// every os.Stdout leak.
func expectedStderr() *os.File {
	if v := flag.Lookup("test.v"); v != nil && v.Value.String() == "test2json" {
		return realStdout
	}
	return realStderr
}

// TestZZStdioNotLeaked fails if any test in this package reassigned os.Stdout or
// os.Stderr and did not restore it. A capture helper that restores in a defer
// cannot trip this; one that restores on the happy path only will trip it as soon
// as its callback calls t.Fatal. See be-gh02.
func TestZZStdioNotLeaked(t *testing.T) {
	if os.Stdout != realStdout {
		t.Errorf("os.Stdout was leaked by an earlier test (now fd=%d name=%q); "+
			"a capture helper restored it on the happy path only - move the restore into a defer",
			os.Stdout.Fd(), os.Stdout.Name())
		os.Stdout = realStdout
	}
	if want := expectedStderr(); os.Stderr != want {
		t.Errorf("os.Stderr was leaked by an earlier test (now fd=%d name=%q, want fd=%d name=%q); "+
			"a capture helper restored it on the happy path only - move the restore into a defer",
			os.Stderr.Fd(), os.Stderr.Name(), want.Fd(), want.Name())
		os.Stderr = want
	}
}

// TestZZStdioGuardTracksTheRunnersOwnSubstitution grades expectedStderr by the
// behaviour it exists for rather than by inspection: under -test.v=test2json it
// must expect the stream testing.M.Run actually installed, and outside that mode
// the real stderr. Without this, the guard silently reverts to a check that
// cannot pass under `go test -json`.
func TestZZStdioGuardTracksTheRunnersOwnSubstitution(t *testing.T) {
	v := flag.Lookup("test.v")
	if v == nil {
		t.Fatal("test.v is always registered by the testing package")
	}

	if v.Value.String() == "test2json" {
		if expectedStderr() != realStdout {
			t.Errorf("under test2json the guard must expect os.Stdout, got fd=%d name=%q",
				expectedStderr().Fd(), expectedStderr().Name())
		}
		// The premise itself: this is the substitution the guard is tracking.
		if os.Stderr != realStdout {
			t.Errorf("expected testing.M.Run to point os.Stderr at os.Stdout under test2json; "+
				"os.Stderr is fd=%d name=%q", os.Stderr.Fd(), os.Stderr.Name())
		}
		return
	}

	if expectedStderr() != realStderr {
		t.Errorf("outside test2json the guard must expect the real stderr, got fd=%d name=%q",
			expectedStderr().Fd(), expectedStderr().Name())
	}
}
