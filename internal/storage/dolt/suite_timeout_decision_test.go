package dolt

import (
	"strings"
	"testing"
	"time"
)

// TestSuiteTimeoutRefusal covers the guard's decision table. These cases are
// the ones that have actually occurred: the go test default (10m), the two 40m
// ceilings that burned a polecat session each, the compiled-binary conformance
// run in pr-risk.yml (-test.timeout=15m -test.run '^TestConformance$'), and the
// benchmark targets in the Makefile (-run=^$ -bench=. -timeout=30m).
func TestSuiteTimeoutRefusal(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		flags   suiteRunFlags
		refuses bool
	}{
		{
			name:    "go test default ceiling",
			flags:   suiteRunFlags{timeout: 10 * time.Minute},
			refuses: true,
		},
		{
			name:    "the 40m ceiling that failed three times",
			flags:   suiteRunFlags{timeout: 40 * time.Minute},
			refuses: true,
		},
		{
			name:    "one second under the floor",
			flags:   suiteRunFlags{timeout: requiredSuiteTimeout - time.Second},
			refuses: true,
		},
		{
			name:  "exactly the floor",
			flags: suiteRunFlags{timeout: requiredSuiteTimeout},
		},
		{
			name:  "above the floor",
			flags: suiteRunFlags{timeout: 90 * time.Minute},
		},
		{
			name:  "no deadline at all",
			flags: suiteRunFlags{timeout: 0},
		},
		{
			name:  "compiled binary with no -test.timeout",
			flags: suiteRunFlags{},
		},
		{
			name:  "narrowed by -run, as pr-risk.yml runs the conformance test",
			flags: suiteRunFlags{timeout: 15 * time.Minute, run: "^TestConformance$"},
		},
		{
			name:  "isolated repro of one test under a short ceiling",
			flags: suiteRunFlags{timeout: time.Minute, run: "TestMergeRecomputesIsBlocked"},
		},
		{
			name:  "benchmark run, as make bench invokes it",
			flags: suiteRunFlags{timeout: 30 * time.Minute, run: "^$", bench: "."},
		},
		{
			name:  "listing names runs nothing",
			flags: suiteRunFlags{timeout: time.Minute, list: "."},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			msg := suiteTimeoutRefusal(tc.flags)
			if tc.refuses && msg == "" {
				t.Fatalf("suiteTimeoutRefusal(%+v) allowed the run; want a refusal", tc.flags)
			}
			if !tc.refuses && msg != "" {
				t.Fatalf("suiteTimeoutRefusal(%+v) refused the run:\n%s", tc.flags, msg)
			}
		})
	}
}

// TestSuiteTimeoutRefusalIsActionable guards the part that matters more than the
// threshold: the message has to hand the operator the working command, because
// the failure this replaces is one where the operator did not know the number.
func TestSuiteTimeoutRefusalIsActionable(t *testing.T) {
	t.Parallel()

	msg := suiteTimeoutRefusal(suiteRunFlags{timeout: 10 * time.Minute})
	for _, want := range []string{
		"make test-dolt",        // the invocation you copy instead of remember
		"-timeout 1h0m0s",       // the raw command carries the floor
		"BEADS_DOLT_AUTO_START", // a gt shell needs the env cleared too
		"-run",                  // the fast path for working a single test
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal message is missing %q:\n%s", want, msg)
		}
	}
}

// TestReadSuiteRunFlags checks that the flags this binary was started with are
// readable at all — a rename in the testing package would otherwise silently
// disable the guard by leaving every field at its zero value.
func TestReadSuiteRunFlags(t *testing.T) {
	t.Parallel()

	if _, ok := lookupFlag[time.Duration]("test.timeout"); !ok {
		t.Error("test.timeout is not a readable duration flag; the guard cannot see the ceiling")
	}
	if _, ok := lookupFlag[string]("test.run"); !ok {
		t.Error("test.run is not a readable string flag; the guard cannot see a narrowed run")
	}
	if _, ok := lookupFlag[string]("nonexistent.flag"); ok {
		t.Error("lookupFlag reported a flag that does not exist")
	}
}
