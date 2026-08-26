// Package procpressure instruments how many bd processes are alive at once
// and how long the oldest has been running, so a pile-up of concurrent
// invocations becomes a signal instead of a silent host degradation.
//
// Why this exists (bd-x33): every bd invocation carries a fixed startup cost —
// on Linux roughly 93MB RSS and ~2.4GB of virtual address space — whether it
// runs a query or just prints its version. Query size is nearly free by
// comparison. So the cost that matters scales with the NUMBER of concurrent
// processes, not with what any of them is doing.
//
// Nothing bounds that number. Agents poll on a fixed cadence, so when the
// database slows down, each call's lifetime stretches while the arrival rate
// stays put and the pile grows without bound. bd also runs at oom_score_adj
// 200 in the town, which makes the kernel pick it first: the processes are
// killed quietly, no error reaches anyone, and the collapse is invisible until
// a human reboots the host. That is exactly what happened on 2026-08-16 —
// ~30 concurrent bd processes and seven OOM kills.
//
// This package does not bound concurrency. It makes concurrency observable:
// each invocation registers itself, counts its live peers, and warns once when
// the count crosses a threshold. Converting a silent degradation into a line on
// stderr is the whole point.
//
// # Design constraints
//
// Every bd invocation pays for this, so it must be cheap and it must never
// fail the command. Registration is one small write, one directory read, and a
// liveness check per peer, against a runtime directory that is tmpfs on most
// systems. Every error path returns an empty report and a no-op release: a
// broken registry degrades to no instrumentation, never to a broken bd.
//
// # Stale entries
//
// The failure this instruments is bd being SIGKILLed, which means entries are
// routinely orphaned with no chance to clean up. Peers are therefore verified
// by process-birth token (see internal/procid), not by PID alone — a recycled
// PID must not resurrect a dead entry — and entries that fail verification are
// unlinked by whichever invocation notices. The registry self-heals; there is
// no reaper to run and nothing to garbage-collect on a schedule.
package procpressure

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/steveyegge/beads/internal/procid"
)

// DefaultThreshold is the live-process count at or above which Report.Over
// reports pressure.
//
// Calibrated from observed town behavior rather than from the per-process
// cost: a normal mail-check fan-out across every agent puts about ten bd
// processes in flight at once for a few tens of milliseconds, and warning on
// that would be noise. Thirty is the count that took the host down. Sixteen
// sits clear of routine fan-out and well under the failure, so crossing it
// means arrivals have started outrunning departures.
const DefaultThreshold = 16

// ThresholdEnv overrides DefaultThreshold. A value of 0 or less disables the
// warning while leaving registration in place, so the count stays available to
// callers that want to report it themselves.
const ThresholdEnv = "BD_PROC_PRESSURE_THRESHOLD"

// DisableEnv, when set to a truthy value, turns the package off entirely:
// Register does nothing and returns an empty report.
const DisableEnv = "BD_PROC_PRESSURE_DISABLE"

// dirName is the registry directory, created under the user runtime dir when
// one exists and under the temp dir otherwise. The UID suffix keeps two users
// on a shared host from colliding in /tmp.
const dirName = "bd-procs"

// Peer is one live bd process seen in the registry.
type Peer struct {
	PID     int          `json:"pid"`
	Token   procid.Token `json:"token"`
	Command string       `json:"command"`
	Started time.Time    `json:"started"`
	// State distinguishes a process doing work from one parked in Acquire
	// waiting for a slot. Empty means running: entries written by a bd that
	// predates the cap carry no state, and counting an unknown entry against
	// the cap is the conservative reading. See cap.go.
	State State `json:"state,omitempty"`
}

// State is what a registered process is doing. Only Waiting is written
// explicitly; a running process leaves the field empty so its entry stays
// byte-identical to one written before the cap existed.
type State string

const (
	// StateRunning is a process holding a slot and doing work.
	StateRunning State = ""
	// StateWaiting is a process parked in Acquire, holding no slot.
	StateWaiting State = "waiting"
)

// Waiting reports whether this peer is parked waiting for a slot rather than
// holding one.
func (p Peer) Waiting() bool { return p.State == StateWaiting }

// Age returns how long the peer has been registered, as of now.
func (p Peer) Age(now time.Time) time.Duration {
	if p.Started.IsZero() {
		return 0
	}
	d := now.Sub(p.Started)
	if d < 0 {
		return 0
	}
	return d
}

// Report describes the concurrency this invocation found, including itself.
// The zero Report is valid and describes no pressure, which is what callers
// get whenever instrumentation could not run.
type Report struct {
	// Peers are the live bd processes, oldest first. Includes this process.
	Peers []Peer
	// Threshold is the count at or above which Over reports pressure.
	Threshold int
	// Now is the instant the report was taken.
	Now time.Time
}

// Count returns the number of live bd processes, including this one. It counts
// processes parked in Acquire too: a waiting bd is a started bd, and the
// per-process floor this package exists to measure was paid before it parked.
func (r Report) Count() int { return len(r.Peers) }

// Running returns the number of live bd processes holding a slot — the count
// the cap in cap.go bounds. It excludes peers parked waiting for a slot.
func (r Report) Running() int {
	n := 0
	for _, p := range r.Peers {
		if !p.Waiting() {
			n++
		}
	}
	return n
}

// WaitingPeers returns the peers parked waiting for a slot, oldest first. The
// order is the admission order Acquire uses, so the first entry is the next
// process due to run.
func (r Report) WaitingPeers() []Peer {
	var out []Peer
	for _, p := range r.Peers {
		if p.Waiting() {
			out = append(out, p)
		}
	}
	return out
}

// Oldest returns the age of the longest-running live bd process. It is the
// half of the signal that says whether a high count is a healthy burst
// draining normally or a queue that has stopped draining.
func (r Report) Oldest() time.Duration {
	if len(r.Peers) == 0 {
		return 0
	}
	return r.Peers[0].Age(r.Now)
}

// Over reports whether the live count has reached the threshold. A threshold
// of 0 or less never reports pressure.
func (r Report) Over() bool {
	return r.Threshold > 0 && len(r.Peers) >= r.Threshold
}

// Warning returns the operator-facing message for a report that is Over, or
// the empty string otherwise. It names the oldest command because that is the
// one worth looking at first: in a queue that has stopped draining, the oldest
// entry is the call that is holding everything up.
func (r Report) Warning() string {
	if !r.Over() {
		return ""
	}
	oldest := r.Peers[0]
	noun := "processes"
	if len(r.Peers) == 1 {
		noun = "process"
	}
	// "running" would be a lie about a process parked in Acquire, and the
	// distinction is the first thing an operator wants: peers stacked up
	// waiting means the cap is doing its job, peers stacked up running means
	// nothing is bounding them.
	verb := "running"
	if oldest.Waiting() {
		verb = "waiting"
	}
	return fmt.Sprintf(
		"Warning: %d bd %s running concurrently (threshold %d); oldest is %q, %s %s. "+
			"Each bd process costs ~93MB regardless of what it does, so a count that keeps climbing "+
			"means calls are arriving faster than they finish — check database health.",
		len(r.Peers), noun, r.Threshold, oldest.Command, verb, oldest.Age(r.Now).Round(time.Millisecond),
	)
}

// Register records this process in the registry and reports the live
// concurrency it found. The returned release function removes this process's
// entry; callers must defer it. Both the report and the release function are
// safe to use when instrumentation could not run — the report is empty and the
// release is a no-op.
//
// command names this invocation for peers that read the registry; pass the
// subcommand name, not the full argv, so nothing sensitive lands in a
// world-readable temp directory.
func Register(command string) (Report, func()) {
	return defaultRegistry().Register(command)
}

// Scan reports the live bd processes without registering the caller. Use it
// for read-only inspection — a diagnostic readout, say — where adding an entry
// would be noise. The returned Report has a zero Now when instrumentation
// could not run, which is how a caller tells "nothing running" (impossible,
// since the caller is running) from "not measured".
func Scan() Report {
	return defaultRegistry().Scan()
}

// Registry is the on-disk set of live bd processes. Tests construct one
// directly to control the directory, the clock, and the threshold; production
// code uses Register.
type Registry struct {
	// Dir holds one file per live process. Empty means instrumentation is off.
	Dir string
	// Threshold is copied into each Report.
	Threshold int
	// Now defaults to time.Now.
	Now func() time.Time
}

func defaultRegistry() *Registry {
	if truthy(os.Getenv(DisableEnv)) {
		return &Registry{}
	}
	return &Registry{Dir: registryDir(), Threshold: threshold()}
}

// registryDir prefers the user runtime directory, which is tmpfs on systemd
// hosts and is cleared on logout. It falls back to the temp dir, where the UID
// suffix keeps users from colliding.
func registryDir() string {
	if run := os.Getenv("XDG_RUNTIME_DIR"); run != "" {
		return filepath.Join(run, dirName)
	}
	return filepath.Join(os.TempDir(), fmt.Sprintf("%s-%d", dirName, os.Getuid()))
}

func threshold() int {
	raw := strings.TrimSpace(os.Getenv(ThresholdEnv))
	if raw == "" {
		return DefaultThreshold
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		// A malformed override must not silently disable the alarm that this
		// package exists to raise.
		return DefaultThreshold
	}
	return n
}

func truthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func (r *Registry) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// Register writes this process's entry, then scans for live peers. See the
// package-level Register for the contract.
func (r *Registry) Register(command string) (Report, func()) {
	noop := func() {}
	if r == nil || r.Dir == "" {
		return Report{}, noop
	}
	if err := os.MkdirAll(r.Dir, 0o700); err != nil {
		return Report{}, noop
	}

	self, ok := r.enroll(command, StateRunning)
	if !ok {
		return Report{}, noop
	}

	return r.scan(), r.releaser(self.PID)
}

// enroll writes this process's entry in the given state. It reports false when
// the entry could not be written, which is the caller's signal to degrade to no
// instrumentation rather than to fail.
func (r *Registry) enroll(command string, state State) (Peer, bool) {
	pid := os.Getpid()
	// A missing token is not fatal: the entry is still written, and peers fall
	// back to a PID-only liveness check for it. Losing reuse-safety for one
	// entry is better than losing the count entirely.
	token, _ := procid.Capture(pid)

	self := Peer{PID: pid, Token: token, Command: command, Started: r.now(), State: state}
	if !r.writeEntry(self) {
		return Peer{}, false
	}
	return self, true
}

// writeEntry persists one peer, replacing any entry this process already has.
// Acquire rewrites its own entry to change state, and rewriting in place keeps
// Started — and so the process's place in the admission order — unchanged.
func (r *Registry) writeEntry(p Peer) bool {
	data, err := json.Marshal(p)
	if err != nil {
		return false
	}
	return os.WriteFile(r.entryPath(p.PID), data, 0o600) == nil
}

func (r *Registry) releaser(pid int) func() {
	path := r.entryPath(pid)
	return func() { _ = os.Remove(path) }
}

// Scan reports the live peers without registering this process. Useful for
// read-only inspection.
func (r *Registry) Scan() Report {
	if r == nil || r.Dir == "" {
		return Report{}
	}
	return r.scan()
}

func (r *Registry) entryPath(pid int) string {
	return filepath.Join(r.Dir, strconv.Itoa(pid)+".json")
}

// scan reads every entry, drops the ones whose process is gone, and returns
// what is left oldest-first. Unlinking dead entries here is what keeps the
// registry bounded without a reaper: the common case for this package is bd
// being SIGKILLed, which leaves its entry behind by definition.
func (r *Registry) scan() Report {
	now := r.now()
	rep := Report{Threshold: r.Threshold, Now: now}

	entries, err := os.ReadDir(r.Dir)
	if err != nil {
		// Return the zero Report, not an empty-but-timestamped one. A caller
		// reading Now can then tell "the registry is unreadable, concurrency is
		// unmeasured" from "the registry is readable and holds no entries".
		return Report{}
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		full := filepath.Join(r.Dir, e.Name())
		data, err := os.ReadFile(full) // #nosec G304 -- full is r.Dir joined with a name from ReadDir of that same dir
		if err != nil {
			// Racing with a peer's own release. Not ours to interpret.
			continue
		}
		var p Peer
		if err := json.Unmarshal(data, &p); err != nil || p.PID <= 0 {
			// A torn write from a peer mid-registration. Counting it would be
			// wrong (we have no PID to verify) and unlinking it would delete a
			// live peer's entry, so leave it alone; its owner cleans it up.
			continue
		}
		if !alive(p, now) {
			_ = os.Remove(full)
			continue
		}
		rep.Peers = append(rep.Peers, p)
	}

	sort.Slice(rep.Peers, func(i, j int) bool {
		if rep.Peers[i].Started.Equal(rep.Peers[j].Started) {
			return rep.Peers[i].PID < rep.Peers[j].PID
		}
		return rep.Peers[i].Started.Before(rep.Peers[j].Started)
	})
	return rep
}

// staleMaxAge bounds how long an entry survives when liveness cannot be
// decided. It is not a timeout on bd: it is the backstop that stops entries
// accumulating on a platform with no birth token, or when verification itself
// keeps failing. Half an hour is far longer than any bd invocation and far
// shorter than a host's uptime, so it never drops a live peer and never lets a
// leak persist.
const staleMaxAge = 30 * time.Minute

// alive reports whether the peer's process still exists AND is the same
// process that wrote the entry. Without the token check a recycled PID would
// keep a dead entry alive forever, and on a busy host PIDs recycle fast.
//
// When liveness cannot be decided, age decides. An unverifiable entry is not
// evidence of death — wrongly unlinking a live peer undercounts the very
// pressure this package measures — so a recent one is counted; only one past
// staleMaxAge is dropped.
func alive(p Peer, now time.Time) bool {
	if p.Token == "" {
		// No birth token on this platform, so there is nothing to verify
		// against and age is the only signal available.
		return p.Age(now) < staleMaxAge
	}
	ok, err := procid.Verify(p.PID, p.Token)
	if err != nil {
		if procid.IsProcessGone(err) {
			return false
		}
		return p.Age(now) < staleMaxAge
	}
	return ok
}
