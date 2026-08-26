package procpressure

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fastPolicy is the cap under test: small enough that a park costs
// milliseconds, explicit enough that no test depends on the production
// constants staying where they are.
func fastPolicy(capacity int, wait time.Duration) Policy {
	return Policy{Cap: capacity, Wait: wait, Poll: 2 * time.Millisecond}
}

// plantRunning fills dir with n live peers holding slots, and returns their
// entry paths. Each is a real process, so the liveness check in scan has
// something genuine to verify and the peers survive for the whole test.
func plantRunning(t *testing.T, dir string, n int) []string {
	t.Helper()
	paths := make([]string, 0, n)
	for i := range n {
		pid := spawnSleeper(t)
		paths = append(paths, writePeer(t, dir, livePeer(t, pid, "list", time.Now().Add(-time.Duration(i)*time.Second))))
	}
	return paths
}

// selfEntry reads back this process's own registry entry.
func selfEntry(t *testing.T, dir string) Peer {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, strconv.Itoa(os.Getpid())+".json"))
	if err != nil {
		t.Fatalf("read own entry: %v", err)
	}
	var p Peer
	if err := json.Unmarshal(data, &p); err != nil {
		t.Fatalf("unmarshal own entry: %v", err)
	}
	return p
}

func TestAcquireUnderCapDoesNotPark(t *testing.T) {
	dir := t.TempDir()
	plantRunning(t, dir, 2)
	r := &Registry{Dir: dir, Threshold: DefaultThreshold}

	adm, release, err := r.Acquire("list", fastPolicy(4, time.Minute))
	defer release()

	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if adm.Waited != 0 {
		t.Errorf("Waited = %v, want 0: three of four slots in use is not full", adm.Waited)
	}
	if adm.Expired {
		t.Error("Expired is true without a wait")
	}
	if adm.Report.Running() != 3 {
		t.Errorf("Running() = %d, want 3 (two peers plus self)", adm.Report.Running())
	}
	if got := selfEntry(t, dir).State; got != StateRunning {
		t.Errorf("own entry State = %q, want running", got)
	}
}

func TestAcquireParksThenProceedsWhenCapStaysFull(t *testing.T) {
	dir := t.TempDir()
	plantRunning(t, dir, 2)
	r := &Registry{Dir: dir, Threshold: DefaultThreshold}

	start := time.Now()
	adm, release, err := r.Acquire("list", fastPolicy(2, 60*time.Millisecond))
	defer release()

	if err != nil {
		t.Fatalf("Acquire returned an error under the default fail-open policy: %v", err)
	}
	if !adm.Expired {
		t.Error("Expired is false; the cap was full for the whole wait")
	}
	if adm.Waited < 60*time.Millisecond {
		t.Errorf("Waited = %v, want at least the 60ms deadline", adm.Waited)
	}
	if elapsed := time.Since(start); elapsed < 60*time.Millisecond {
		t.Errorf("Acquire returned after %v, want it to have actually blocked for the deadline", elapsed)
	}
	// Fail-open means the process runs. If this entry were still marked
	// waiting, peers would undercount it and the cap would drift upward.
	if got := selfEntry(t, dir).State; got != StateRunning {
		t.Errorf("own entry State = %q after a fail-open expiry, want running", got)
	}
	// The report a parked process gets must count itself the same way an
	// uncontended one does, or a caller cannot compare the two.
	if adm.Report.Running() != 3 {
		t.Errorf("Running() = %d, want 3 (two holders plus this process, now running)", adm.Report.Running())
	}
	if adm.Notice() == "" {
		t.Error("Notice() is empty after the cap was exceeded; the one case that must always speak")
	}
}

func TestAcquireAdmittedAsSoonAsASlotFrees(t *testing.T) {
	dir := t.TempDir()
	paths := plantRunning(t, dir, 2)
	r := &Registry{Dir: dir, Threshold: DefaultThreshold}

	freed := make(chan struct{})
	go func() {
		time.Sleep(40 * time.Millisecond)
		_ = os.Remove(paths[0])
		close(freed)
	}()

	adm, release, err := r.Acquire("list", fastPolicy(2, 10*time.Second))
	defer release()
	<-freed

	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if adm.Expired {
		t.Error("Expired is true, but a slot freed well inside the deadline")
	}
	if adm.Waited == 0 {
		t.Error("Waited = 0; the cap was full when Acquire started")
	}
	if adm.Waited > 5*time.Second {
		t.Errorf("Waited = %v, want release shortly after the slot freed at 40ms", adm.Waited)
	}
	if got := selfEntry(t, dir).State; got != StateRunning {
		t.Errorf("own entry State = %q after admission, want running", got)
	}
}

func TestAcquireFailClosedRefusesAndReleasesItsEntry(t *testing.T) {
	dir := t.TempDir()
	plantRunning(t, dir, 2)
	r := &Registry{Dir: dir, Threshold: DefaultThreshold}

	policy := fastPolicy(2, 40*time.Millisecond)
	policy.FailClosed = true

	adm, release, err := r.Acquire("list", policy)
	defer release() // must be safe to call after a refusal

	if err == nil {
		t.Fatal("Acquire succeeded under FailClosed with the cap full for the whole wait")
	}
	var capped *ErrCapped
	if !errors.As(err, &capped) {
		t.Fatalf("error is %T, want *ErrCapped: callers distinguish a full cap from a broken bd", err)
	}
	if capped.Cap != 2 {
		t.Errorf("ErrCapped.Cap = %d, want 2", capped.Cap)
	}
	if strings.Contains(capped.Error(), "1 bd processes") {
		t.Errorf("ErrCapped message %q says %q; a refusal that cannot count reads as a bug in the cap",
			capped.Error(), "1 bd processes")
	}
	if !strings.Contains(capped.Error(), ModeEnv) {
		t.Errorf("ErrCapped message %q does not name %s; a refusal must say how to override it", capped.Error(), ModeEnv)
	}
	if adm.Report.Count() != 0 {
		t.Errorf("Report is populated on a refusal (count %d); nothing ran", adm.Report.Count())
	}
	// A configured zero wait must not read as a slot that freed instantly.
	// Sub-millisecond, not exactly zero: that is what a Wait of 0 actually
	// measures, and an exact-zero test would never exercise this branch.
	zero := (&ErrCapped{Cap: 1, Waited: 80 * time.Microsecond}).Error()
	if !strings.Contains(zero, WaitEnv+" is 0") {
		t.Errorf("ErrCapped with no wait = %q, want it to name the setting that caused it", zero)
	}
	if !strings.Contains(zero, "1 bd process ") {
		t.Errorf("ErrCapped at cap 1 = %q, want the singular noun", zero)
	}

	// A refused process leaves nothing behind. Otherwise every refusal would
	// consume a slot it never used and the pool would ratchet closed.
	if _, err := os.Stat(filepath.Join(dir, strconv.Itoa(os.Getpid())+".json")); !os.IsNotExist(err) {
		t.Errorf("own entry survived the refusal (stat err %v)", err)
	}
}

func TestAcquireReclaimsSlotFromDeadHolder(t *testing.T) {
	dir := t.TempDir()

	// The case the whole package exists for: a holder SIGKILLed mid-run. Its
	// entry is still on disk and nothing ran to clean it up.
	pid := spawnSleeper(t)
	writePeer(t, dir, livePeer(t, pid, "list", time.Now()))
	proc, err := os.FindProcess(pid)
	if err != nil {
		t.Fatalf("FindProcess: %v", err)
	}
	_ = proc.Kill()
	_, _ = proc.Wait()

	r := &Registry{Dir: dir, Threshold: DefaultThreshold}
	adm, release, err := r.Acquire("list", fastPolicy(1, time.Minute))
	defer release()

	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if adm.Waited != 0 {
		t.Errorf("Waited = %v, want 0: the dead holder's slot is reclaimed by the scan itself", adm.Waited)
	}
	if adm.Report.Running() != 1 {
		t.Errorf("Running() = %d, want 1 (only this process)", adm.Report.Running())
	}
}

func TestAcquireWithCapDisabledMatchesRegister(t *testing.T) {
	dir := t.TempDir()
	plantRunning(t, dir, 3)
	r := &Registry{Dir: dir, Threshold: DefaultThreshold}

	adm, release, err := r.Acquire("list", Policy{})
	defer release()

	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if adm.Cap != 0 || adm.Waited != 0 || adm.Expired {
		t.Errorf("Admission = %+v, want an uncapped start", adm)
	}
	if adm.Report.Count() != 4 {
		t.Errorf("Count() = %d, want 4 (three peers plus self)", adm.Report.Count())
	}
	if adm.Notice() != "" {
		t.Errorf("Notice() = %q, want empty when no cap is in force", adm.Notice())
	}
}

func TestAcquireFailsOpenOnUnusableDir(t *testing.T) {
	base := t.TempDir()
	blocker := filepath.Join(base, "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("write blocker: %v", err)
	}

	// A cap that cannot read its registry cannot count, and must not be the
	// thing that stops bd from running — even under FailClosed.
	policy := fastPolicy(1, time.Minute)
	policy.FailClosed = true
	r := &Registry{Dir: filepath.Join(blocker, "procs"), Threshold: 1}

	adm, release, err := r.Acquire("list", policy)
	release() // must not panic

	if err != nil {
		t.Fatalf("Acquire returned %v on an unusable registry; a broken cap must degrade to no cap", err)
	}
	if adm.Report.Count() != 0 || adm.Waited != 0 {
		t.Errorf("Admission = %+v, want an inert uncapped start", adm)
	}
}

func TestWaitersCountAsPressureButNotAsSlots(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	writePeer(t, dir, Peer{PID: 810001, Command: "list", Started: now.Add(-3 * time.Second)})
	writePeer(t, dir, Peer{PID: 810002, Command: "ready", Started: now.Add(-2 * time.Second), State: StateWaiting})
	writePeer(t, dir, Peer{PID: 810003, Command: "show", Started: now.Add(-time.Second), State: StateWaiting})

	rep := (&Registry{Dir: dir, Threshold: 3, Now: fixedClock(now)}).Scan()

	// A parked bd is a started bd: its ~93MB was paid before it parked, so it
	// is pressure. It holds no slot, so it is not capacity.
	if rep.Count() != 3 {
		t.Errorf("Count() = %d, want 3: waiters still cost memory", rep.Count())
	}
	if rep.Running() != 1 {
		t.Errorf("Running() = %d, want 1: waiters hold no slot", rep.Running())
	}
	if got := len(rep.WaitingPeers()); got != 2 {
		t.Errorf("len(WaitingPeers()) = %d, want 2", got)
	}
	if !rep.Over() {
		t.Error("Over() is false at 3 peers with threshold 3; waiters must reach the alarm")
	}
}

func TestWarningNamesWhatTheOldestPeerIsDoing(t *testing.T) {
	now := time.Now()
	peers := []Peer{
		{PID: 820001, Command: "list", Started: now.Add(-5 * time.Second), State: StateWaiting},
		{PID: 820002, Command: "show", Started: now.Add(-time.Second)},
	}

	msg := Report{Peers: peers, Threshold: 2, Now: now}.Warning()
	if !strings.Contains(msg, `"list", waiting`) {
		t.Errorf("Warning() = %q, want it to say the oldest peer is waiting, not running", msg)
	}

	peers[0].State = StateRunning
	msg = Report{Peers: peers, Threshold: 2, Now: now}.Warning()
	if !strings.Contains(msg, `"list", running`) {
		t.Errorf("Warning() = %q, want it to say the oldest peer is running", msg)
	}
}

func TestAdmissionRankIsArrivalOrder(t *testing.T) {
	now := time.Now()
	rep := Report{Now: now, Peers: []Peer{
		{PID: 830001, Started: now.Add(-4 * time.Second)},                      // running, not in the queue
		{PID: 830002, Started: now.Add(-3 * time.Second), State: StateWaiting}, // first in
		{PID: 830003, Started: now.Add(-2 * time.Second), State: StateWaiting}, // second
		{PID: 830004, Started: now.Add(-1 * time.Second), State: StateWaiting}, // third
	}}

	for pid, want := range map[int]int{830002: 0, 830003: 1, 830004: 2} {
		if got := admissionRank(rep, pid); got != want {
			t.Errorf("admissionRank(%d) = %d, want %d", pid, got, want)
		}
	}
	// A process whose entry has vanished cannot be ordered against peers it can
	// no longer see, so it stops waiting rather than waiting forever.
	if got := admissionRank(rep, 999999); got != 0 {
		t.Errorf("admissionRank of an absent pid = %d, want 0 (fail-open)", got)
	}
}

func TestDefaultPolicyHonoursEnv(t *testing.T) {
	clear := func(t *testing.T) {
		t.Helper()
		t.Setenv(DisableEnv, "")
		t.Setenv(CapEnv, "")
		t.Setenv(WaitEnv, "")
		t.Setenv(ModeEnv, "")
	}

	t.Run("defaults", func(t *testing.T) {
		clear(t)
		p := DefaultPolicy()
		if p.Cap != DefaultCap || p.Wait != DefaultWait || p.FailClosed {
			t.Errorf("DefaultPolicy() = %+v, want cap %d wait %v fail-open", p, DefaultCap, DefaultWait)
		}
		if !p.Enabled() {
			t.Error("Enabled() is false by default; the cap ships on")
		}
	})

	t.Run("overrides", func(t *testing.T) {
		clear(t)
		t.Setenv(CapEnv, "4")
		t.Setenv(WaitEnv, "2s")
		t.Setenv(ModeEnv, "CLOSED")
		p := DefaultPolicy()
		if p.Cap != 4 || p.Wait != 2*time.Second || !p.FailClosed {
			t.Errorf("DefaultPolicy() = %+v, want cap 4 wait 2s fail-closed", p)
		}
	})

	t.Run("explicit zero disables", func(t *testing.T) {
		clear(t)
		t.Setenv(CapEnv, "0")
		if DefaultPolicy().Enabled() {
			t.Errorf("%s=0 left the cap enabled; that is the documented way to turn it off", CapEnv)
		}
	})

	t.Run("malformed values keep the bound", func(t *testing.T) {
		clear(t)
		// A typo must not silently remove the cap. Disabling is something an
		// operator does on purpose, by writing 0.
		t.Setenv(CapEnv, "lots")
		t.Setenv(WaitEnv, "soon")
		p := DefaultPolicy()
		if p.Cap != DefaultCap {
			t.Errorf("Cap = %d for a malformed %s, want DefaultCap %d", p.Cap, CapEnv, DefaultCap)
		}
		if p.Wait != DefaultWait {
			t.Errorf("Wait = %v for a malformed %s, want DefaultWait %v", p.Wait, WaitEnv, DefaultWait)
		}
	})

	t.Run("instrumentation off means cap off", func(t *testing.T) {
		clear(t)
		t.Setenv(DisableEnv, "1")
		if DefaultPolicy().Enabled() {
			t.Errorf("%s left the cap enabled; a cap that cannot read the registry cannot count", DisableEnv)
		}
	})
}

func TestAdmissionNotice(t *testing.T) {
	if got := (Admission{Cap: 8, Waited: 5 * time.Millisecond}).Notice(); got != "" {
		t.Errorf("Notice() = %q for a 5ms park, want silence: a burst that drained is the cap working", got)
	}
	if got := (Admission{Cap: 8, Waited: 3 * time.Second}).Notice(); !strings.Contains(got, "3s") {
		t.Errorf("Notice() = %q for a 3s park, want it to state the wait", got)
	}
	// A deadline expiry always speaks, however short the wait, because it means
	// the cap has stopped bounding anything.
	expired := Admission{Cap: 8, Waited: time.Millisecond, Expired: true}.Notice()
	if !strings.Contains(expired, "proceeded anyway") {
		t.Errorf("Notice() = %q on expiry, want it to say the cap was exceeded", expired)
	}
	if got := (Admission{Waited: time.Minute}).Notice(); got != "" {
		t.Errorf("Notice() = %q with no cap in force, want empty", got)
	}
}

func TestAcquireReleaseRemovesEntry(t *testing.T) {
	dir := t.TempDir()
	r := &Registry{Dir: dir}

	_, release, err := r.Acquire("show", fastPolicy(4, time.Minute))
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	release()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("after release: %d entries, want 0 — a leaked entry consumes a slot forever", len(entries))
	}
}

// capChildEnv carries the registry directory to a child process. Its presence
// is also what tells TestCapChildHelper it is a child rather than a stray unit
// test, so the helper is inert in a normal run.
const capChildEnv = "BD_PROC_CAP_TEST_CHILD_DIR"

const (
	capTestCap  = 4
	capTestHold = 120 * time.Millisecond
	capTestKids = 12
)

// TestCapChildHelper is one contender in TestCapBoundsRealProcesses. It is a
// test only because re-executing the test binary is the one way to get real,
// distinct PIDs contending on a real registry — the registry keys entries by
// PID, so goroutines cannot stand in for processes here.
func TestCapChildHelper(t *testing.T) {
	dir := os.Getenv(capChildEnv)
	if dir == "" {
		t.Skip("helper process only; run via TestCapBoundsRealProcesses")
	}

	r := &Registry{Dir: dir, Threshold: DefaultThreshold}
	adm, release, err := r.Acquire("list", Policy{
		Cap: capTestCap, Wait: 30 * time.Second, Poll: 3 * time.Millisecond,
	})
	defer release()
	if err != nil {
		t.Fatalf("child Acquire: %v", err)
	}
	// Reported before the hold, so the parent reads the count at the instant
	// this child was admitted rather than after the crowd has moved on.
	//
	//nolint:forbidigo // the child's stdout is this test's transport
	fmt.Printf("ADMITTED running=%d waited=%s expired=%v\n", adm.Report.Running(), adm.Waited, adm.Expired)
	time.Sleep(capTestHold)
}

// TestCapBoundsRealProcesses is the control the unit tests above cannot be:
// every one of them drives Acquire from a single process against planted
// entries, so all of them would still pass if the cap held only against
// fabricated peers. This one starts twelve real processes against a cap of
// four and reads back what each of them actually saw.
//
// It asserts two things, and the second matters as much as the first. That the
// observed peak stayed near the cap is the claim. That somebody had to wait is
// the proof the claim was tested at all — without it, a run in which the
// children never overlapped would report a peak of one and pass while measuring
// nothing.
func TestCapBoundsRealProcesses(t *testing.T) {
	dir := t.TempDir()

	type result struct {
		running int
		waited  time.Duration
		expired bool
	}
	results := make(chan result, capTestKids)
	errs := make(chan error, capTestKids)

	var wg sync.WaitGroup
	for range capTestKids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cmd := exec.Command(os.Args[0], "-test.run=^TestCapChildHelper$", "-test.v")
			cmd.Env = append(os.Environ(), capChildEnv+"="+dir)
			out, err := cmd.CombinedOutput()
			if err != nil {
				errs <- fmt.Errorf("child failed: %v\n%s", err, out)
				return
			}
			r, ok := parseAdmitted(string(out))
			if !ok {
				errs <- fmt.Errorf("child printed no ADMITTED line:\n%s", out)
				return
			}
			results <- result{running: r.running, waited: r.waited, expired: r.expired}
		}()
	}
	wg.Wait()
	close(results)
	close(errs)

	for err := range errs {
		t.Fatalf("%v", err)
	}

	peak, contended, seen := 0, 0, 0
	for r := range results {
		seen++
		peak = max(peak, r.running)
		if r.waited > 0 {
			contended++
		}
		if r.expired {
			t.Errorf("a child's 30s wait expired; the cap should have drained a %v hold long before that", capTestHold)
		}
	}
	if seen != capTestKids {
		t.Fatalf("collected %d results, want %d", seen, capTestKids)
	}
	if contended == 0 {
		t.Fatalf("no child ever waited: %d processes against a cap of %d never overlapped, so this run "+
			"measured nothing — raise capTestHold rather than trusting the peak below", capTestKids, capTestCap)
	}
	// Two slots of slack. Admission is advisory, not a mutex: two waiters
	// scanning in the same instant can both claim the last free slot, and the
	// point is that the overshoot is the width of that race rather than the
	// size of the crowd.
	if peak > capTestCap+2 {
		t.Errorf("peak concurrency %d with cap %d across %d processes (%d of them waited); "+
			"the cap is not bounding anything", peak, capTestCap, capTestKids, contended)
	}
	t.Logf("peak %d, cap %d, %d of %d children waited", peak, capTestCap, contended, capTestKids)
}

type admittedLine struct {
	running int
	waited  time.Duration
	expired bool
}

func parseAdmitted(out string) (admittedLine, bool) {
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) == 0 || fields[0] != "ADMITTED" {
			continue
		}
		var a admittedLine
		for _, f := range fields[1:] {
			k, v, ok := strings.Cut(f, "=")
			if !ok {
				continue
			}
			switch k {
			case "running":
				a.running, _ = strconv.Atoi(v)
			case "waited":
				a.waited, _ = time.ParseDuration(v)
			case "expired":
				a.expired = v == "true"
			}
		}
		return a, true
	}
	return admittedLine{}, false
}
