//go:build cgo

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/steveyegge/beads/internal/testutil"
	"golang.org/x/sync/errgroup"
)

// ---------------------------------------------------------------------------
// Test configuration
// ---------------------------------------------------------------------------

func ssEnvInt(key string, def int) int {
	if s := os.Getenv(key); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			return n
		}
	}
	return def
}

// ---------------------------------------------------------------------------
// TestSharedServerConcurrent
// ---------------------------------------------------------------------------

// TestSharedServerConcurrent builds the bd binary, starts a single Dolt
// container via testcontainers, initializes numDirs project directories,
// then fans out numClients concurrent workloads across those directories.
// Multiple clients may share a directory (and therefore a database),
// exercising concurrent multi-writer access to the same Dolt database.
//
// Requires BEADS_TEST_SHARED_SERVER=1 to run (skipped by default).
//
// Configuration via environment variables:
//
//	BEADS_TEST_SS_DIRS     — number of project directories  (default: 10)
//	BEADS_TEST_SS_CLIENTS  — number of concurrent clients   (default: 50)
//	BEADS_TEST_SS_MAXPROCS — max concurrent subprocesses    (default: GOMAXPROCS*4)
//
// The defaults are sized to finish inside scripts/test.sh's 25m timeout. Each
// client is ssOpsPerClient bd SUBPROCESSES run back to back, so cost is
// clients×ssOpsPerClient process spawns, not clients goroutines: the historical
// 50×500 default is ~50,500 spawns and measured ~60-85 minutes, which cannot fit and
// produced a timeout that was read as a deadlock (bd-9p6). Run that load
// deliberately, with a timeout to match:
//
//	BEADS_TEST_SS_DIRS=50 BEADS_TEST_SS_CLIENTS=500 TEST_TIMEOUT=150m \
//	  ./scripts/test.sh -run TestSharedServerConcurrent ./cmd/bd
//
// Any configuration that cannot finish in the remaining deadline fails early
// with the measured rate rather than running out the clock — see watch.
//
// This test runs in NO automated lane today, and sizing was only half the
// reason. Nothing sets BEADS_TEST_SHARED_SERVER, and setting it in
// .github/workflows/nightly.yml would still not run it: that job sets
// BEADS_TEST_SKIP=dolt because Docker-based Dolt testcontainers hang in GitHub
// Actions (scripts/repro-dolt-hang/), and NewContainerProvider honors that
// switch, so the test would t.Skipf at the container step. Enabling it for real
// is blocked on that hang, not on this file.
//
// Recommended: set BEADS_TEST_EMBEDDED_DOLT=1 to skip the unrelated
// singleton Dolt container that TestMain starts for other tests in this package.
func TestSharedServerConcurrent(t *testing.T) {
	if os.Getenv("BEADS_TEST_SHARED_SERVER") == "" {
		t.Skip("skipping: set BEADS_TEST_SHARED_SERVER=1 to run")
	}
	if runtime.GOOS == "windows" {
		t.Skip("not supported on Windows")
	}

	numDirs := ssEnvInt("BEADS_TEST_SS_DIRS", 10)
	numClients := ssEnvInt("BEADS_TEST_SS_CLIENTS", 50)
	maxProcs := ssEnvInt("BEADS_TEST_SS_MAXPROCS", runtime.GOMAXPROCS(0)*4)
	t.Logf("config: dirs=%d clients=%d maxprocs=%d", numDirs, numClients, maxProcs)

	testStart := time.Now()

	// Build or reuse bd binary.
	phase := time.Now()
	bdBinary := buildSharedServerTestBinary(t)
	t.Logf("build bd binary: %s", time.Since(phase))

	// Start Dolt container.
	phase = time.Now()
	cp, err := testutil.NewContainerProvider()
	if err != nil {
		t.Skipf("cannot start Dolt container: %v", err)
	}
	containerPort := cp.Port()
	t.Cleanup(func() { _ = cp.Stop() })
	t.Logf("start container (port %d): %s", containerPort, time.Since(phase))

	// Shared server directory + port file.
	sharedDir := t.TempDir()
	if err := cp.WritePortFile(sharedDir); err != nil {
		t.Fatalf("write port file: %v", err)
	}

	// Base environment for every bd subprocess.
	baseEnv := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + t.TempDir(),
		"GOPATH=" + os.Getenv("GOPATH"),
		"GOROOT=" + os.Getenv("GOROOT"),
		"BEADS_SHARED_SERVER_DIR=" + sharedDir,
		"BEADS_DOLT_SHARED_SERVER=1",
		"BEADS_DOLT_SERVER_PORT=" + strconv.Itoa(containerPort),
		"BEADS_DOLT_AUTO_START=0",
		"BEADS_TEST_MODE=1",
		"BD_DISABLE_METRICS=1",
		"BD_DISABLE_EVENT_FLUSH=1",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=",
		"SSH_ASKPASS=",
		"GT_ROOT=",
	}

	// Context inherits from -timeout.
	ctx := context.Background()
	if dl, ok := t.Deadline(); ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, dl)
		defer cancel()
	}

	// ── Init project directories ────────────────────────────────────────
	phase = time.Now()
	type project struct {
		dir, prefix string
	}
	projects := make([]project, numDirs)

	eg, egCtx := errgroup.WithContext(ctx)
	eg.SetLimit(maxProcs)
	for i := range numDirs {
		i := i
		eg.Go(func() error {
			prefix := fmt.Sprintf("proj%d", i)
			dir := filepath.Join(t.TempDir(), prefix)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return fmt.Errorf("project %d mkdir: %w", i, err)
			}
			if err := gitInit(egCtx, dir); err != nil {
				return fmt.Errorf("project %d git init: %w", i, err)
			}
			out, err := ssExec(egCtx, bdBinary, dir, baseEnv,
				"init", "--shared-server", "--external",
				"--prefix", prefix, "--quiet", "--non-interactive")
			if err != nil {
				return fmt.Errorf("project %d init: %s: %w", i, out, err)
			}
			projects[i] = project{dir: dir, prefix: prefix}
			return nil
		})
	}
	if err := eg.Wait(); err != nil {
		t.Fatalf("init: %v", err)
	}
	t.Logf("init %d dirs: %s", numDirs, time.Since(phase))

	// ── Fan out client workloads ────────────────────────────────────────
	phase = time.Now()
	wlCtx, abort := context.WithCancel(ctx)
	defer abort()
	eg, egCtx = errgroup.WithContext(wlCtx)
	eg.SetLimit(maxProcs)

	prog := &ssProgress{}
	stopWatch := prog.watch(t, numClients, maxProcs, abort)

	for c := range numClients {
		c := c
		eg.Go(func() error {
			p := projects[c%numDirs]
			cl := &bdClient{
				tag:    fmt.Sprintf("c%d", c),
				binary: bdBinary,
				dir:    p.dir,
				env:    baseEnv,
				ctx:    egCtx,
				t:      t,
				prog:   prog,
			}
			err := cl.runWorkload()
			if err == nil {
				prog.clients.Add(1)
			}
			return err
		})
	}
	err = eg.Wait()
	stopWatch()
	if err != nil {
		// A projected overrun cancels wlCtx, so the errgroup surfaces a
		// context error rather than a real defect. Say which one happened.
		if reason := prog.abortReason(); reason != "" {
			t.Fatalf("workload: %s", reason)
		}
		t.Fatalf("workload: %v", err)
	}
	t.Logf("workloads (%d clients x %d dirs): %s — %d ops",
		numClients, numDirs, time.Since(phase), prog.ops.Load())
	t.Logf("total: %s", time.Since(testStart))
}

// ---------------------------------------------------------------------------
// Progress accounting
// ---------------------------------------------------------------------------

// ssOpsPerClient is how many bd subprocesses one runWorkload spawns:
// 30 create + 14 dep + 25 update + 15 verify + 7 list + 10 delete.
// Used only to project a completion time, so it costs nothing if it drifts
// slightly — the projection is a guard rail, not an assertion.
const ssOpsPerClient = 101

// ssProgress counts finished subprocess invocations so that a run which is
// merely SLOW is distinguishable from one that is WEDGED.
//
// Go prints the same "panic: test timed out after 25m0s" for both, and the
// goroutine dump does not separate them either. At saturation this test's dump
// is ~N [select] + ~N [syscall] + ~N [IO wait] for N live subprocesses — the
// per-subprocess triple os/exec creates — plus exactly ONE [chan send]: the
// errgroup admission semaphore in the fan-out loop above, which stays blocked
// for the whole run because that is what backpressure looks like when
// numClients exceeds maxProcs. That shape was read as "126 blocked producers
// against 4 blocked receivers, a producer/consumer stall" and cost a merge
// (bd-9p6); the run was in fact spawning ~850 subprocesses a minute at the
// moment it was dumped. No channel in this test has more than one sender.
//
// The heartbeat is the discriminator that was missing: "+0 ops" across an
// interval is a stall, a nonzero delta is not.
type ssProgress struct {
	ops     atomic.Int64
	clients atomic.Int64

	mu     sync.Mutex
	reason string
}

// op records one completed bd subprocess invocation. Nil-safe so that a
// bdClient built without a progress counter degrades to "no heartbeat" rather
// than panicking in a goroutine, which would surface as an unrelated crash.
func (p *ssProgress) op() {
	if p == nil {
		return
	}
	p.ops.Add(1)
}

func (p *ssProgress) setReason(s string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.reason == "" {
		p.reason = s
	}
}

func (p *ssProgress) abortReason() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.reason
}

// watch logs a heartbeat every 30s and, once it has a rate to extrapolate
// from, aborts the run if the configured load cannot finish before the test
// deadline. Returns a stop function.
//
// Failing at 90s with "this load needs ~62m, deadline allows ~21m" is worth
// far more than the same failure at 25m with a goroutine dump attached: the
// dump invites a deadlock diagnosis, and the numbers do not.
func (p *ssProgress) watch(t *testing.T, numClients, maxProcs int, abort context.CancelFunc) func() {
	t.Helper()

	const interval = 30 * time.Second
	totalOps := int64(numClients) * ssOpsPerClient
	deadline, hasDeadline := t.Deadline()

	done := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		tick := time.NewTicker(interval)
		defer tick.Stop()
		start := time.Now()
		var last int64
		for {
			select {
			case <-done:
				return
			case <-tick.C:
			}

			n := p.ops.Load()
			delta := n - last
			last = n
			elapsed := time.Since(start)
			t.Logf("progress: %d/%d clients, %d/%d ops (+%d in %s, %.1f ops/s)",
				p.clients.Load(), numClients, n, totalOps, delta, interval,
				float64(n)/elapsed.Seconds())

			if !hasDeadline || n == 0 {
				continue
			}
			// Project from the rate observed so far. Only act once a full
			// interval of evidence exists and the overrun is not marginal —
			// a slow start under load must not fail an otherwise fine run.
			eta := time.Duration(float64(elapsed) * float64(totalOps) / float64(n))
			remaining := time.Until(deadline)
			if eta-elapsed > remaining*2 {
				p.setReason(fmt.Sprintf(
					"projected %s to run %d clients x %d ops at the observed %.1f ops/s "+
						"(maxprocs=%d), but only %s of the test deadline remains. "+
						"This is a sizing failure, not a hang — %d ops completed in %s. "+
						"Lower BEADS_TEST_SS_CLIENTS/BEADS_TEST_SS_DIRS, or raise the "+
						"timeout (TEST_TIMEOUT for scripts/test.sh).",
					eta.Round(time.Second), numClients, ssOpsPerClient,
					float64(n)/elapsed.Seconds(), maxProcs,
					remaining.Round(time.Second), n, elapsed.Round(time.Second)))
				abort()
				return
			}
		}
	}()

	return func() {
		close(done)
		<-stopped
	}
}

// ---------------------------------------------------------------------------
// bdClient — wraps a bd subprocess invocation for one client
// ---------------------------------------------------------------------------

type bdClient struct {
	tag    string // unique client identifier (e.g. "c42")
	binary string
	dir    string
	env    []string
	ctx    context.Context
	t      *testing.T
	prog   *ssProgress
	op     int // running operation counter
}

// bd runs an arbitrary bd command and returns combined output.
func (c *bdClient) bd(args ...string) (string, error) {
	c.op++
	start := time.Now()
	out, err := ssExec(c.ctx, c.binary, c.dir, c.env, args...)
	c.prog.op()
	c.t.Logf("%s [op %d] %s — %s", c.tag, c.op, strings.Join(args, " "), time.Since(start))
	return out, err
}

// create runs bd create --json and returns the new issue ID.
func (c *bdClient) create(title string, extra ...string) (string, error) {
	c.op++
	start := time.Now()
	args := append([]string{"create", title, "--json"}, extra...)
	out, err := ssExec(c.ctx, c.binary, c.dir, c.env, args...)
	c.prog.op()
	c.t.Logf("%s [op %d] create %q — %s", c.tag, c.op, title, time.Since(start))
	if err != nil {
		return "", fmt.Errorf("%s: %w", out, err)
	}
	return ssJSONField(out, "id")
}

// show runs bd show <id> --json and returns the parsed issue.
func (c *bdClient) show(id string) (map[string]any, error) {
	c.op++
	start := time.Now()
	out, err := ssExec(c.ctx, c.binary, c.dir, c.env, "show", id, "--json")
	c.prog.op()
	c.t.Logf("%s [op %d] show %s — %s", c.tag, c.op, id, time.Since(start))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", out, err)
	}
	return ssParseShowJSON(out)
}

// list runs bd list --json --flat with extra args and returns the result array.
func (c *bdClient) list(extra ...string) ([]any, error) {
	c.op++
	start := time.Now()
	args := append([]string{"list", "--json", "--flat"}, extra...)
	out, err := ssExec(c.ctx, c.binary, c.dir, c.env, args...)
	c.prog.op()
	c.t.Logf("%s [op %d] list %s — %s", c.tag, c.op, strings.Join(extra, " "), time.Since(start))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", out, err)
	}
	var result []any
	if err := json.Unmarshal([]byte(ssFirstJSON(out)), &result); err != nil {
		return nil, fmt.Errorf("parse list JSON: %w\noutput: %s", err, out)
	}
	return result, nil
}

// errf formats an error with the client tag and current op number.
func (c *bdClient) errf(format string, args ...any) error {
	prefix := fmt.Sprintf("%s [op %d] ", c.tag, c.op)
	return fmt.Errorf(prefix+format, args...)
}

// ---------------------------------------------------------------------------
// Workload — the actual issue management workflow
// ---------------------------------------------------------------------------

func (c *bdClient) runWorkload() error {
	ids, err := c.phaseCreate()
	if err != nil {
		return err
	}
	if err := c.phaseDeps(ids); err != nil {
		return err
	}
	if err := c.phaseUpdate(ids); err != nil {
		return err
	}
	if err := c.phaseVerify(ids); err != nil {
		return err
	}
	if err := c.phaseList(); err != nil {
		return err
	}
	return c.phaseDelete(ids)
}

// phaseCreate creates 30 issues across multiple types and returns their IDs.
//
//	0-9   tasks
//	10-14 bugs (with descriptions)
//	15-19 features
//	20-24 epics
//	25-29 chores (children of epics 20-24)
func (c *bdClient) phaseCreate() ([]string, error) {
	types := []string{
		"task", "task", "task", "task", "task",
		"task", "task", "task", "task", "task",
		"bug", "bug", "bug", "bug", "bug",
		"feature", "feature", "feature", "feature", "feature",
		"epic", "epic", "epic", "epic", "epic",
		"chore", "chore", "chore", "chore", "chore",
	}
	ids := make([]string, len(types))

	for i, typ := range types {
		var id string
		var err error
		switch {
		case i >= 10 && i <= 14:
			id, err = c.create(
				fmt.Sprintf("%s bug %d", c.tag, i),
				"--type", typ,
				"-d", fmt.Sprintf("Bug description for issue %d in %s", i, c.tag),
			)
		case i >= 25:
			id, err = c.create(
				fmt.Sprintf("%s chore %d", c.tag, i),
				"--type", typ,
				"--parent", ids[20+(i-25)],
			)
		default:
			id, err = c.create(
				fmt.Sprintf("%s %s %d", c.tag, typ, i),
				"--type", typ,
			)
		}
		if err != nil {
			return nil, c.errf("create issue %d (%s): %w", i, typ, err)
		}
		ids[i] = id
	}
	return ids, nil
}

// phaseDeps wires dependencies between issues.
func (c *bdClient) phaseDeps(ids []string) error {
	pairs := [][2]int{
		{1, 0}, {2, 0}, {3, 0}, {4, 0}, {5, 0},
		{6, 0}, {7, 0}, {8, 0}, {9, 0}, // tasks 1-9 → task 0
		{16, 15}, {17, 15}, {18, 15}, {19, 15}, // features 16-19 → feature 15
		{11, 10}, // bug 11 → bug 10
	}
	for _, p := range pairs {
		from, to := ids[p[0]], ids[p[1]]
		if out, err := c.bd("dep", "add", from, to, "--json"); err != nil {
			return c.errf("dep add %s->%s: %s: %w", from, to, out, err)
		}
	}
	return nil
}

// phaseUpdate modifies titles, statuses, labels, priorities, and descriptions.
func (c *bdClient) phaseUpdate(ids []string) error {
	// Rename tasks 0-4.
	for i := range 5 {
		if out, err := c.bd("update", ids[i], "--title", fmt.Sprintf("%s task %d UPDATED", c.tag, i)); err != nil {
			return c.errf("update title %s: %s: %w", ids[i], out, err)
		}
	}
	// Tasks 0-2 → in_progress.
	for i := range 3 {
		if out, err := c.bd("update", ids[i], "--status", "in_progress"); err != nil {
			return c.errf("update status %s: %s: %w", ids[i], out, err)
		}
	}
	// Close bugs 10-14.
	for i := 10; i <= 14; i++ {
		if out, err := c.bd("update", ids[i], "--status", "closed"); err != nil {
			return c.errf("close %s: %s: %w", ids[i], out, err)
		}
	}
	// Label tasks 3-6.
	for j, i := range []int{3, 4, 5, 6} {
		label := []string{"urgent", "backend", "frontend", "infra"}[j]
		if out, err := c.bd("update", ids[i], "--add-label", label, "--add-label", c.tag); err != nil {
			return c.errf("add-label %s: %s: %w", ids[i], out, err)
		}
	}
	// Prioritize features 15-19.
	for j, i := range []int{15, 16, 17, 18, 19} {
		pri := []string{"P0", "P1", "P2", "P3", "P4"}[j]
		if out, err := c.bd("update", ids[i], "--priority", pri); err != nil {
			return c.errf("set priority %s: %s: %w", ids[i], out, err)
		}
	}
	// Describe epics 20-22.
	for i := 20; i <= 22; i++ {
		if out, err := c.bd("update", ids[i], "-d", fmt.Sprintf("Epic %d plan for %s", i, c.tag)); err != nil {
			return c.errf("update desc %s: %s: %w", ids[i], out, err)
		}
	}
	return nil
}

// phaseVerify reads back issues and checks that mutations took effect.
func (c *bdClient) phaseVerify(ids []string) error {
	// Titles on tasks 0-4.
	for i := range 5 {
		if err := c.expectField(ids[i], "title", fmt.Sprintf("%s task %d UPDATED", c.tag, i)); err != nil {
			return err
		}
	}
	// Bugs 10-14 closed.
	for i := 10; i <= 14; i++ {
		if err := c.expectField(ids[i], "status", "closed"); err != nil {
			return err
		}
	}
	// Feature 15 priority = P0.
	if err := c.expectFieldFloat(ids[15], "priority", 0); err != nil {
		return err
	}
	// Feature 16 has dependencies.
	f16, err := c.show(ids[16])
	if err != nil {
		return c.errf("show %s: %w", ids[16], err)
	}
	if deps, _ := f16["dependencies"].([]any); len(deps) == 0 {
		return c.errf("show %s: expected dependencies, got none", ids[16])
	}
	// Chore 25 parent = epic 20.
	if err := c.expectField(ids[25], "parent", ids[20]); err != nil {
		return err
	}
	// Labels on task 3.
	t3, err := c.show(ids[3])
	if err != nil {
		return c.errf("show %s: %w", ids[3], err)
	}
	if err := checkLabels(t3, "urgent", c.tag); err != nil {
		return c.errf("show %s: %w", ids[3], err)
	}
	// Epic 20 description.
	e20, err := c.show(ids[20])
	if err != nil {
		return c.errf("show %s: %w", ids[20], err)
	}
	want := fmt.Sprintf("Epic 20 plan for %s", c.tag)
	if desc, _ := e20["description"].(string); !strings.Contains(desc, want) {
		return c.errf("show %s: description = %q, missing %q", ids[20], desc, want)
	}
	return nil
}

// phaseList runs filtered list queries as spot-checks.
// Counts use >= because multiple clients may share the same database.
func (c *bdClient) phaseList() error {
	checks := []struct {
		args []string
		min  int
	}{
		{[]string{"--label", c.tag}, 4},
		{[]string{"--all", "--type", "bug", "--limit", "5"}, 5},
		{[]string{"--status", "closed", "--limit", "5"}, 5},
		{[]string{"--type", "feature", "--priority-max", "1", "--limit", "2"}, 2},
		{[]string{"--type", "epic", "--limit", "5"}, 5},
		{[]string{"--status", "open,in_progress,blocked", "--limit", "10"}, 10},
		{[]string{"--all", "--limit", "50"}, 30},
	}
	for _, ch := range checks {
		result, err := c.list(ch.args...)
		if err != nil {
			return c.errf("list %s: %w", strings.Join(ch.args, " "), err)
		}
		if len(result) < ch.min {
			return c.errf("list %s: got %d, want >= %d", strings.Join(ch.args, " "), len(result), ch.min)
		}
	}
	return nil
}

// phaseDelete removes chores 25-29 and verifies they're gone.
func (c *bdClient) phaseDelete(ids []string) error {
	for i := 25; i <= 29; i++ {
		if out, err := c.bd("delete", ids[i], "--force", "--json"); err != nil {
			return c.errf("delete %s: %s: %w", ids[i], out, err)
		}
	}
	for i := 25; i <= 29; i++ {
		if out, err := c.bd("show", ids[i], "--json"); err == nil {
			return c.errf("show deleted %s should fail: %s", ids[i], out)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Verification helpers
// ---------------------------------------------------------------------------

// expectField shows an issue and asserts a string field matches exactly.
func (c *bdClient) expectField(id, field, want string) error {
	issue, err := c.show(id)
	if err != nil {
		return c.errf("show %s: %w", id, err)
	}
	if got, _ := issue[field].(string); got != want {
		return c.errf("show %s: %s = %q, want %q", id, field, got, want)
	}
	return nil
}

// expectFieldFloat shows an issue and asserts a numeric field matches.
func (c *bdClient) expectFieldFloat(id, field string, want float64) error {
	issue, err := c.show(id)
	if err != nil {
		return c.errf("show %s: %w", id, err)
	}
	if got, _ := issue[field].(float64); got != want {
		return c.errf("show %s: %s = %v, want %v", id, field, got, want)
	}
	return nil
}

// checkLabels verifies that an issue's labels contain all expected values.
func checkLabels(issue map[string]any, required ...string) error {
	labels, _ := issue["labels"].([]any)
	set := make(map[string]bool, len(labels))
	for _, l := range labels {
		if s, ok := l.(string); ok {
			set[s] = true
		}
	}
	for _, r := range required {
		if !set[r] {
			return fmt.Errorf("labels %v missing %q", labels, r)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Subprocess + JSON helpers
// ---------------------------------------------------------------------------

// ssExec returns bd's combined output. Use it for assertions about what bd
// SAID; for anything that parses a value out of the result, use ssExecStdout —
// see the note there.
func ssExec(ctx context.Context, binary, dir string, env []string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Dir = dir
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// ssExecStdout returns bd's stdout alone, with stderr folded into the error so
// a failure is still diagnosable.
//
// Anything that reads a machine-readable value out of bd's output must go
// through this rather than ssExec. bd writes diagnostics to stderr — the
// procpressure pile-up warning is the one that bites, since it appears only
// when enough bd processes are alive at once, which is to say only under a full
// parallel suite. A combined read splices it onto the front of the value; that
// is bd-pyu, where an exact ID comparison started matching against a warning
// with the ID stuck on the end.
func ssExecStdout(ctx context.Context, binary, dir string, env []string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Dir = dir
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("%w; stderr:\n%s", err, stderr.String())
	}
	return stdout.String(), nil
}

// ssFirstJSON returns the substring starting at the first '{' or '['.
func ssFirstJSON(output string) string {
	for i, ch := range output {
		if ch == '{' || ch == '[' {
			return output[i:]
		}
	}
	return output
}

// ssJSONField extracts a string field from the first JSON object in output.
func ssJSONField(output, field string) (string, error) {
	jsonStr := ssFirstJSON(output)
	var m map[string]any
	if err := json.Unmarshal([]byte(jsonStr), &m); err != nil {
		return "", fmt.Errorf("parse JSON: %w\nraw: %s", err, output)
	}
	v, ok := m[field].(string)
	if !ok || v == "" {
		return "", fmt.Errorf("field %q not found or empty in JSON", field)
	}
	return v, nil
}

// ssParseShowJSON parses bd show --json output (an array) into a single object.
func ssParseShowJSON(output string) (map[string]any, error) {
	jsonStr := ssFirstJSON(output)
	var arr []map[string]any
	if err := json.Unmarshal([]byte(jsonStr), &arr); err != nil {
		// Fall back to single object.
		var m map[string]any
		if err2 := json.Unmarshal([]byte(jsonStr), &m); err2 != nil {
			return nil, fmt.Errorf("parse show JSON: %w\nraw: %s", err, output)
		}
		return m, nil
	}
	if len(arr) == 0 {
		return nil, fmt.Errorf("bd show returned empty array")
	}
	return arr[0], nil
}

// ---------------------------------------------------------------------------
// Git + build helpers
// ---------------------------------------------------------------------------

func gitInit(ctx context.Context, dir string) error {
	for _, c := range [][]string{
		{"git", "init"},
		{"git", "config", "user.email", "test@test.com"},
		{"git", "config", "user.name", "Test"},
	} {
		cmd := exec.CommandContext(ctx, c[0], c[1:]...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("%s: %s: %w", strings.Join(c, " "), string(out), err)
		}
	}
	return nil
}

var (
	sharedServerBdBinary  string
	sharedServerBuildOnce sync.Once
	sharedServerBuildErr  error
)

// buildSharedServerTestBinary returns the path to a bd binary.
// If BEADS_TEST_BD_BINARY is set, uses that pre-built binary.
// Otherwise builds one from source (cached across tests via sync.Once).
func buildSharedServerTestBinary(t *testing.T) string {
	t.Helper()
	sharedServerBuildOnce.Do(func() {
		if prebuilt := os.Getenv("BEADS_TEST_BD_BINARY"); prebuilt != "" {
			if _, err := os.Stat(prebuilt); err != nil {
				sharedServerBuildErr = fmt.Errorf("BEADS_TEST_BD_BINARY=%q not found: %w", prebuilt, err)
				return
			}
			sharedServerBdBinary = prebuilt
			return
		}
		pkgDir, err := os.Getwd()
		if err != nil {
			sharedServerBuildErr = fmt.Errorf("getwd: %w", err)
			return
		}
		buildDir, err := testTempDir("beads-shared-server-bd-*")
		if err != nil {
			sharedServerBuildErr = fmt.Errorf("mkdirtemp: %w", err)
			return
		}
		bdBin := filepath.Join(buildDir, "bd")
		cmd := exec.Command("go", "build", "-tags", "gms_pure_go", "-o", bdBin, ".")
		cmd.Dir = pkgDir
		cmd.Env = append(os.Environ(), "CGO_ENABLED=1")
		out, err := cmd.CombinedOutput()
		if err != nil {
			_ = os.RemoveAll(buildDir)
			sharedServerBuildErr = fmt.Errorf("go build: %s: %w", string(out), err)
			return
		}
		sharedServerBdBinary = bdBin
	})
	if sharedServerBuildErr != nil {
		t.Fatalf("build bd: %v", sharedServerBuildErr)
	}
	return sharedServerBdBinary
}
