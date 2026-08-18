package testutil

import (
	"errors"
	"strings"
	"sync"
	"testing"
)

func TestBranchCleanupCounterStaysSilentWhenTeardownWorks(t *testing.T) {
	t.Parallel()

	var c branchCleanupCounter
	c.record("DOLT_BRANCH('-D')", nil)
	c.record("DOLT_CHECKOUT('main')", nil)

	if got := c.count(); got != 0 {
		t.Errorf("count() = %d after only successful teardown; want 0", got)
	}
	if got := c.report(); got != "" {
		t.Errorf("report() = %q on a clean run; want empty so a passing suite prints nothing", got)
	}
}

func TestBranchCleanupCounterNamesTheFailingStatement(t *testing.T) {
	t.Parallel()

	var c branchCleanupCounter
	c.record("DOLT_CHECKOUT('main')", errors.New("cannot checkout with uncommitted changes"))
	c.record("DOLT_BRANCH('-D')", errors.New("attempted to delete checked out branch"))
	c.record("DOLT_BRANCH('-D')", errors.New("some later, less interesting error"))

	if got := c.count(); got != 3 {
		t.Errorf("count() = %d; want 3", got)
	}

	report := c.report()
	for _, want := range []string{
		"failed 3 time(s)",
		"DOLT_CHECKOUT('main')",
		"cannot checkout with uncommitted changes",
		"DOLT_BRANCH('-D')        2 failure(s)",
		"attempted to delete checked out branch", // the FIRST error, not the last
	} {
		if !strings.Contains(report, want) {
			t.Errorf("report is missing %q:\n%s", want, report)
		}
	}
	if strings.Contains(report, "less interesting") {
		t.Errorf("report kept a later error instead of the first one:\n%s", report)
	}
}

// TestBranchCleanupCounterIsConcurrencySafe matters because test cleanups run
// from parallel tests: the counter is written from many goroutines at once.
func TestBranchCleanupCounterIsConcurrencySafe(t *testing.T) {
	t.Parallel()

	var c branchCleanupCounter
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.record("DOLT_BRANCH('-D')", errors.New("boom"))
			_ = c.report()
		}()
	}
	wg.Wait()

	if got := c.count(); got != 50 {
		t.Errorf("count() = %d after 50 concurrent failures; want 50", got)
	}
}
