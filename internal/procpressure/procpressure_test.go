package procpressure

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/beads/internal/procid"
)

func fixedClock(t time.Time) func() time.Time { return func() time.Time { return t } }

// writePeer plants an entry directly, standing in for another bd process.
func writePeer(t *testing.T, dir string, p Peer) string {
	t.Helper()
	data, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal peer: %v", err)
	}
	path := filepath.Join(dir, strconv.Itoa(p.PID)+".json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write peer: %v", err)
	}
	return path
}

// livePeer describes a process that is genuinely running, so scan's liveness
// check has something real to verify against.
func livePeer(t *testing.T, pid int, command string, started time.Time) Peer {
	t.Helper()
	tok, err := procid.Capture(pid)
	if err != nil {
		t.Skipf("procid.Capture unsupported on this platform: %v", err)
	}
	return Peer{PID: pid, Token: tok, Command: command, Started: started}
}

// spawnSleeper starts a real child process and returns its pid. It is killed
// and reaped when the test ends.
func spawnSleeper(t *testing.T) int {
	t.Helper()
	// The test binary itself is a process we know exists and can control;
	// re-running it with a flag that matches nothing makes it a cheap sleeper
	// only if it blocks, so use the platform's own sleep instead.
	bin, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("no sleep binary available")
	}
	cmd := exec.Command(bin, "60")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleeper: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return cmd.Process.Pid
}

func TestRegisterCountsSelf(t *testing.T) {
	dir := t.TempDir()
	r := &Registry{Dir: dir, Threshold: DefaultThreshold}

	rep, release := r.Register("list")
	defer release()

	if rep.Count() != 1 {
		t.Fatalf("Count() = %d, want 1 (this process)", rep.Count())
	}
	if rep.Peers[0].PID != os.Getpid() {
		t.Errorf("peer PID = %d, want this process %d", rep.Peers[0].PID, os.Getpid())
	}
	if rep.Peers[0].Command != "list" {
		t.Errorf("peer Command = %q, want %q", rep.Peers[0].Command, "list")
	}
	if rep.Over() {
		t.Error("Over() is true for a single process; the alarm would fire on every healthy invocation")
	}
}

func TestReleaseRemovesEntry(t *testing.T) {
	dir := t.TempDir()
	r := &Registry{Dir: dir}

	_, release := r.Register("show")
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("after Register: %d entries (err %v), want 1", len(entries), err)
	}

	release()

	entries, err = os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("after release: %d entries, want 0 — a leaked entry inflates every later count", len(entries))
	}
}

func TestScanUnlinksDeadPeers(t *testing.T) {
	dir := t.TempDir()

	// A process that really existed and really is gone. This is the case the
	// package exists for: bd killed by the OOM killer never runs its release.
	pid := spawnSleeper(t)
	dead := livePeer(t, pid, "list", time.Now())
	deadPath := writePeer(t, dir, dead)
	proc, err := os.FindProcess(pid)
	if err != nil {
		t.Fatalf("FindProcess: %v", err)
	}
	_ = proc.Kill()
	_, _ = proc.Wait()

	r := &Registry{Dir: dir}
	rep := r.Scan()

	for _, p := range rep.Peers {
		if p.PID == pid {
			t.Fatalf("dead peer %d was counted as live", pid)
		}
	}
	if _, err := os.Stat(deadPath); !os.IsNotExist(err) {
		t.Errorf("dead peer's entry still on disk (stat err %v); the registry would grow without bound", err)
	}
}

func TestScanRejectsRecycledPID(t *testing.T) {
	dir := t.TempDir()

	// Same PID as a live process, but a token from a different birth. Without
	// the token check, a recycled PID would keep a dead entry alive forever.
	self := livePeer(t, os.Getpid(), "list", time.Now())
	self.Token = self.Token + "-not-this-birth"
	path := writePeer(t, dir, self)

	rep := (&Registry{Dir: dir}).Scan()

	if rep.Count() != 0 {
		t.Errorf("Count() = %d, want 0: the token does not match this process's birth", rep.Count())
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("mismatched entry still on disk (stat err %v)", err)
	}
}

func TestScanKeepsTokenlessEntryUntilStale(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()

	// No token: a platform with no birth identity. Age is the only signal.
	writePeer(t, dir, Peer{PID: 424242, Command: "fresh", Started: now.Add(-time.Minute)})
	stalePath := writePeer(t, dir, Peer{PID: 424243, Command: "stale", Started: now.Add(-staleMaxAge - time.Minute)})

	rep := (&Registry{Dir: dir, Now: fixedClock(now)}).Scan()

	if rep.Count() != 1 {
		t.Fatalf("Count() = %d, want 1 (the fresh tokenless entry)", rep.Count())
	}
	if rep.Peers[0].Command != "fresh" {
		t.Errorf("kept %q, want %q", rep.Peers[0].Command, "fresh")
	}
	if _, err := os.Stat(stalePath); !os.IsNotExist(err) {
		t.Errorf("stale tokenless entry survived (stat err %v); tokenless platforms would leak entries forever", err)
	}
}

func TestScanIgnoresTornWrites(t *testing.T) {
	dir := t.TempDir()
	// A peer caught mid-registration. Counting it is impossible (no PID to
	// verify) and unlinking it would delete a live peer's entry.
	if err := os.WriteFile(filepath.Join(dir, "999999.json"), []byte(`{"pid":`), 0o600); err != nil {
		t.Fatalf("write torn entry: %v", err)
	}

	rep := (&Registry{Dir: dir}).Scan()

	if rep.Count() != 0 {
		t.Errorf("Count() = %d, want 0", rep.Count())
	}
	if _, err := os.Stat(filepath.Join(dir, "999999.json")); err != nil {
		t.Errorf("torn entry was unlinked (stat err %v); that would delete a live peer's registration", err)
	}
}

func TestPeersSortedOldestFirst(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()

	writePeer(t, dir, Peer{PID: 500001, Command: "newest", Started: now.Add(-time.Second)})
	writePeer(t, dir, Peer{PID: 500002, Command: "oldest", Started: now.Add(-time.Hour / 2 / 2)})
	writePeer(t, dir, Peer{PID: 500003, Command: "middle", Started: now.Add(-time.Minute)})

	rep := (&Registry{Dir: dir, Now: fixedClock(now)}).Scan()

	var got []string
	for _, p := range rep.Peers {
		got = append(got, p.Command)
	}
	want := []string{"oldest", "middle", "newest"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("order = %v, want %v", got, want)
	}
	if rep.Oldest() < 7*time.Minute {
		t.Errorf("Oldest() = %v, want the ~15m age of the oldest peer", rep.Oldest())
	}
}

func TestOverAndWarning(t *testing.T) {
	now := time.Now()
	peers := make([]Peer, 0, 3)
	for i := range 3 {
		peers = append(peers, Peer{PID: 600000 + i, Command: "list", Started: now.Add(-time.Duration(3-i) * time.Second)})
	}

	under := Report{Peers: peers, Threshold: 4, Now: now}
	if under.Over() {
		t.Error("Over() is true at 3 peers with threshold 4")
	}
	if under.Warning() != "" {
		t.Errorf("Warning() = %q, want empty below threshold", under.Warning())
	}

	at := Report{Peers: peers, Threshold: 3, Now: now}
	if !at.Over() {
		t.Error("Over() is false at 3 peers with threshold 3; the threshold is inclusive")
	}
	msg := at.Warning()
	if !strings.Contains(msg, "3 bd processes") {
		t.Errorf("Warning() = %q, want it to state the count", msg)
	}
	if !strings.Contains(msg, "3s") {
		t.Errorf("Warning() = %q, want it to state the oldest peer's age", msg)
	}

	off := Report{Peers: peers, Threshold: 0, Now: now}
	if off.Over() {
		t.Error("Over() is true with threshold 0; a non-positive threshold disables the alarm")
	}

	// An operator-facing alarm that says "1 bd processes" reads as a bug in
	// the alarm, which is the last thing it can afford.
	one := Report{Peers: peers[:1], Threshold: 1, Now: now}
	if msg := one.Warning(); !strings.Contains(msg, "1 bd process running") {
		t.Errorf("Warning() = %q, want the singular %q", msg, "1 bd process running")
	}
}

func TestZeroReportIsSafe(t *testing.T) {
	var rep Report
	if rep.Count() != 0 || rep.Over() || rep.Warning() != "" || rep.Oldest() != 0 {
		t.Errorf("zero Report is not inert: count=%d over=%v oldest=%v warning=%q",
			rep.Count(), rep.Over(), rep.Oldest(), rep.Warning())
	}
}

func TestDisabledRegistryIsInertAndSafe(t *testing.T) {
	// Empty Dir is how every failure path degrades: no instrumentation, but
	// never a broken bd.
	r := &Registry{}
	rep, release := r.Register("list")
	if rep.Count() != 0 {
		t.Errorf("Count() = %d, want 0 when instrumentation is off", rep.Count())
	}
	release() // must not panic
	if got := r.Scan().Count(); got != 0 {
		t.Errorf("Scan().Count() = %d, want 0", got)
	}

	var nilReg *Registry
	nilRep, nilRelease := nilReg.Register("list")
	if nilRep.Count() != 0 {
		t.Errorf("nil Registry Count() = %d, want 0", nilRep.Count())
	}
	nilRelease()
}

func TestRegisterFailsOpenOnUnusableDir(t *testing.T) {
	// A path whose parent is a file cannot be created. bd must still run.
	base := t.TempDir()
	blocker := filepath.Join(base, "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("write blocker: %v", err)
	}

	r := &Registry{Dir: filepath.Join(blocker, "procs"), Threshold: 1}
	rep, release := r.Register("list")
	release() // must not panic

	if rep.Count() != 0 {
		t.Errorf("Count() = %d, want 0 when the registry dir cannot be created", rep.Count())
	}
	if rep.Over() {
		t.Error("Over() is true on a failed registration; a broken registry must not raise a false alarm")
	}
}

func TestDefaultRegistryHonoursEnv(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())

	t.Setenv(DisableEnv, "")
	t.Setenv(ThresholdEnv, "")
	if got := defaultRegistry().Threshold; got != DefaultThreshold {
		t.Errorf("Threshold = %d, want DefaultThreshold %d", got, DefaultThreshold)
	}

	t.Setenv(ThresholdEnv, "4")
	if got := defaultRegistry().Threshold; got != 4 {
		t.Errorf("Threshold = %d, want 4 from %s", got, ThresholdEnv)
	}

	// A typo must not silently disable the alarm this package exists to raise.
	t.Setenv(ThresholdEnv, "lots")
	if got := defaultRegistry().Threshold; got != DefaultThreshold {
		t.Errorf("Threshold = %d for a malformed override, want DefaultThreshold %d", got, DefaultThreshold)
	}

	t.Setenv(ThresholdEnv, "8")
	t.Setenv(DisableEnv, "1")
	if got := defaultRegistry().Dir; got != "" {
		t.Errorf("Dir = %q with %s set, want empty (fully off)", got, DisableEnv)
	}
}

func TestRegistryDirPrefersRuntimeDir(t *testing.T) {
	run := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", run)
	if got, want := registryDir(), filepath.Join(run, dirName); got != want {
		t.Errorf("registryDir() = %q, want %q", got, want)
	}

	t.Setenv("XDG_RUNTIME_DIR", "")
	got := registryDir()
	if !strings.HasPrefix(got, os.TempDir()) {
		t.Errorf("registryDir() = %q, want a path under %q", got, os.TempDir())
	}
	if !strings.HasSuffix(got, strconv.Itoa(os.Getuid())) {
		t.Errorf("registryDir() = %q, want a UID suffix so users cannot collide in the temp dir", got)
	}
}

func TestConcurrentRegistrationsAllCount(t *testing.T) {
	dir := t.TempDir()

	// Three real, live processes registered by hand, plus this one.
	for i := range 3 {
		pid := spawnSleeper(t)
		writePeer(t, dir, livePeer(t, pid, "list", time.Now().Add(-time.Duration(i)*time.Second)))
	}

	r := &Registry{Dir: dir, Threshold: 4}
	rep, release := r.Register("ready")
	defer release()

	if rep.Count() != 4 {
		t.Fatalf("Count() = %d, want 4 (three peers plus self)", rep.Count())
	}
	if !rep.Over() {
		t.Error("Over() is false at 4 peers with threshold 4")
	}
	if rep.Warning() == "" {
		t.Error("Warning() is empty for a report that is Over")
	}
}

// BenchmarkRegister measures what every bd invocation pays for this
// instrumentation. The whole design is justified only if this stays far below
// the ~40ms a healthy bd call takes; if it ever approaches that, the
// instrumentation has become part of the problem it measures.
func BenchmarkRegister(b *testing.B) {
	dir := b.TempDir()
	r := &Registry{Dir: dir, Threshold: DefaultThreshold}
	b.ResetTimer()
	for b.Loop() {
		_, release := r.Register("list")
		release()
	}
}

// BenchmarkScanWithPeers measures the scan against a registry the size of the
// pile-up that took the host down. Every entry carries this process's PID and
// a real birth token, so the per-peer procid.Verify actually runs and the
// entries survive across iterations — a scan of entries that all verify is the
// expensive case and the one that matters.
func BenchmarkScanWithPeers(b *testing.B) {
	dir := b.TempDir()
	now := time.Now()
	tok, err := procid.Capture(os.Getpid())
	if err != nil {
		b.Skipf("procid.Capture unsupported on this platform: %v", err)
	}
	for i := range 30 {
		p := Peer{PID: os.Getpid(), Token: tok, Command: "list", Started: now.Add(-time.Duration(i) * time.Second)}
		data, err := json.Marshal(p)
		if err != nil {
			b.Fatalf("marshal: %v", err)
		}
		// Distinct filenames, same PID: scan keys off file contents, so this
		// gives 30 verifiable entries without needing 30 live processes.
		path := filepath.Join(dir, strconv.Itoa(700000+i)+".json")
		if err := os.WriteFile(path, data, 0o600); err != nil {
			b.Fatalf("write: %v", err)
		}
	}
	r := &Registry{Dir: dir, Threshold: DefaultThreshold, Now: fixedClock(now)}
	b.ResetTimer()
	for b.Loop() {
		r.Scan()
	}
}
