package dolt

import (
	"flag"
	"fmt"
	"time"
)

// requiredSuiteTimeout is the `go test -timeout` floor for a whole-package run
// of this suite. It is a REFUSAL threshold, not a budget: the suite is allowed
// to finish sooner, but a run that starts under this ceiling is guaranteed to
// die at it rather than report a result.
//
// The number comes from the only whole-package run known to have finished:
// 3414.958s (56.9 min) on an unloaded 32-core box at upstream 7505e173f
// (bd-033). go test's default ceiling is 10m, so the documented command
// `go test ./internal/storage/dolt/` CANNOT pass — it dies at the ceiling and
// reports a deadline panic that names no failing test, which reads as "slow
// suite" and closes the investigation. Three separate runs (612s at the 10m
// default, 2411s and 2414s at a 40m ceiling) each burned their whole ceiling
// and learned nothing.
//
// Recording the right number in prose has already failed those three times;
// this constant is the same number in the one place the operator cannot skip.
// If the suite gets faster, lower this — the ceiling should track the suite.
const requiredSuiteTimeout = 60 * time.Minute

// suiteRunFlags is the part of the test binary's flag state the timeout guard
// reads. It is a struct so the decision can be tested without a real flag set.
type suiteRunFlags struct {
	// timeout is -test.timeout; 0 means no deadline at all.
	timeout time.Duration
	// run is -test.run. Non-empty means the operator asked for a subset, and
	// a subset has no business inheriting a whole-suite ceiling.
	run string
	// bench is -test.bench. A benchmark run is not this suite.
	bench string
	// list is -test.list, which prints names and runs nothing.
	list string
}

// readSuiteRunFlags reads the flags the guard cares about. Flags are registered
// by testing.Init before TestMain runs but are not parsed until m.Run, so the
// caller must flag.Parse first. Missing flags yield zero values, which the
// guard treats as "no reason to refuse".
func readSuiteRunFlags() suiteRunFlags {
	var f suiteRunFlags
	if d, ok := lookupFlag[time.Duration]("test.timeout"); ok {
		f.timeout = d
	}
	f.run, _ = lookupFlag[string]("test.run")
	f.bench, _ = lookupFlag[string]("test.bench")
	f.list, _ = lookupFlag[string]("test.list")
	return f
}

// lookupFlag returns the value of a registered flag, or the zero value and
// false when the flag is absent or holds a different type.
func lookupFlag[T any](name string) (T, bool) {
	var zero T
	f := flag.Lookup(name)
	if f == nil {
		return zero, false
	}
	getter, ok := f.Value.(flag.Getter)
	if !ok {
		return zero, false
	}
	v, ok := getter.Get().(T)
	if !ok {
		return zero, false
	}
	return v, true
}

// suiteTimeoutRefusal returns the message to print before refusing to start, or
// "" when the run may proceed. It refuses only a whole-package run under a
// ceiling the package is known not to fit in; a narrowed run (-run, -bench,
// -list) and an unlimited one (-timeout 0) both proceed.
func suiteTimeoutRefusal(f suiteRunFlags) string {
	switch {
	case f.run != "", f.bench != "", f.list != "":
		return ""
	case f.timeout <= 0: // no deadline
		return ""
	case f.timeout >= requiredSuiteTimeout:
		return ""
	}

	return fmt.Sprintf(`FATAL: internal/storage/dolt refuses to start under -timeout %s.

This package needs at least %s. A whole-package run measured 3414s (56.9 min)
on an unloaded box, and go test's default ceiling is 10m, so a run started
under that floor dies at the ceiling and reports a deadline panic naming no
failing test. Refusing now costs you seconds; finding out at the ceiling costs
you the whole ceiling.

Run the whole suite with:

    make test-dolt

or, if you need the raw command (a gt-managed shell injects BEADS_DOLT_* vars
that point this suite at an unreachable server, hence the env -u):

    env -u BEADS_DOLT_AUTO_START -u BEADS_DOLT_PORT -u BEADS_DOLT_SERVER_PORT \
        -u BEADS_DOLT_HOST go test -timeout %s ./internal/storage/dolt/

To work one test instead — each is under 2s in isolation — pass -run, which
this guard does not apply to:

    go test -run TestConcurrentInitSchema ./internal/storage/dolt/
`, f.timeout, requiredSuiteTimeout, requiredSuiteTimeout)
}
