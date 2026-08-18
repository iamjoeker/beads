package testutil

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// Branch-per-test cleanup used to discard the errors from its own teardown
// statements. That is defensible per test — failing a green test in its
// cleanup is worse than leaking a branch — but it made a whole class of
// suite-level slowdown unobservable: a branch that is never deleted is still
// there for every later test, and per-test setup runs the full migration chain
// against a growing branch set.
//
// So the errors are still swallowed, and now they are also counted. The count
// costs nothing while cleanup works, and when it does not it is the difference
// between "the suite got slow" and a name for why.

// branchCleanupCounter tallies failed teardown statements by statement kind.
// The zero value is ready to use.
type branchCleanupCounter struct {
	mu       sync.Mutex
	total    int
	failures map[string]int
	firstErr map[string]error
}

// record tallies one teardown statement. A nil error is the common case.
func (c *branchCleanupCounter) record(statement string, err error) {
	if err == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.failures == nil {
		c.failures = map[string]int{}
		c.firstErr = map[string]error{}
	}
	c.failures[statement]++
	if c.firstErr[statement] == nil {
		c.firstErr[statement] = err
	}
	c.total++
}

// count reports how many teardown statements have failed.
func (c *branchCleanupCounter) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.total
}

// report renders one line per statement kind, or "" when nothing failed.
func (c *branchCleanupCounter) report() string {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.total == 0 {
		return ""
	}

	statements := make([]string, 0, len(c.failures))
	for statement := range c.failures {
		statements = append(statements, statement)
	}
	sort.Strings(statements)

	var b strings.Builder
	fmt.Fprintf(&b, "test-branch cleanup failed %d time(s); a leaked branch slows down every later test:\n", c.total)
	for _, statement := range statements {
		fmt.Fprintf(&b, "  %-24s %d failure(s), first: %v\n", statement, c.failures[statement], c.firstErr[statement])
	}
	return b.String()
}

// branchCleanup is the process-wide tally the test-branch helpers write to.
var branchCleanup branchCleanupCounter

// execDiscardingResult runs a teardown statement and keeps only its error.
func execDiscardingResult(db *sql.DB, query string, args ...any) error {
	_, err := db.Exec(query, args...)
	return err
}

// recordBranchCleanupResult tallies one teardown statement.
func recordBranchCleanupResult(statement string, err error) {
	branchCleanup.record(statement, err)
}

// BranchCleanupFailures reports how many test-branch teardown statements have
// failed so far in this process.
func BranchCleanupFailures() int {
	return branchCleanup.count()
}

// BranchCleanupReport returns a one-line-per-statement summary of those
// failures, or "" when every teardown statement succeeded. Call it from
// TestMain after m.Run; printing nothing on a clean run is the point.
func BranchCleanupReport() string {
	return branchCleanup.report()
}
