package procpressure

// The cap (bd-91c). The rest of this package makes a pile-up of concurrent bd
// processes visible; this file bounds it.
//
// # Why a cap needs a policy and not just a mechanism
//
// The mechanism is small — a wait loop over Registry.Scan. The policy is not,
// because a town-wide gate on a CLI has a wedge failure mode: if one holder
// blocks forever, every later arrival blocks behind it, and a tool that was
// merely slow becomes a tool that has stopped. Three decisions keep that from
// happening, and each is a constant below so an operator can overrule it.
//
// WAIT, THEN PROCEED. A bd that finds the cap full parks, polls for a free
// slot, and gives up at a deadline. Giving up means running anyway
// (fail-open): the cap is advisory by default. That choice makes shipping this
// strictly non-regressive — today nothing bounds concurrency at all, so a
// deadline expiry reproduces exactly today's behavior. The worst case becomes
// "bd was slower under load", never "bd stopped working". FailClosed is
// available for an operator who wants a hard ceiling on a particular host, and
// it is off by default because it introduces a new error class that every
// script, hook and agent in a town would meet at once.
//
// THE CAP EQUALS THE WARNING THRESHOLD. Both are 16, deliberately: they
// describe one event. Routine mail-check fan-out across a town's agents puts
// about ten bd processes in flight, so 16 never throttles healthy operation;
// thirty is the count that took a host down on 2026-08-16. With the two numbers
// equal, the pile-up warning fires exactly when the cap starts doing work,
// which makes the alarm a report on the cap rather than an unrelated signal.
//
// ONE POOL FOR READS AND WRITES. The cost being bounded is the ~93MB
// per-process floor, which bd-x33 measured as the same for `bd version` as for
// `bd list` — it is paid by starting the process, not by what the process does.
// Separate read and write pools would double the ceiling and bound nothing.
//
// # What the cap does not do
//
// It does not prevent the per-process floor. By the time bd reaches main() the
// Go runtime and its mappings are already resident, so a parked bd still costs
// its ~93MB — mostly file-backed pages shared with its peers. What a parked bd
// does not do is open a database connection or build a query working set, and
// that is where the variance lives: the processes the OOM killer took in
// bd-x33 carried 412MB-3.1GB of anon-rss against 15-205MB for healthy ones. The
// cap bounds the heavy, variable part of the footprint. Sizing one against the
// ~15.6GB total-vm in the OOM records would size it wrong; that number is
// PROT_NONE address-space reservation and costs nothing.
//
// # Slot leaks
//
// Nothing needs to reclaim a slot from a process that was SIGKILLed, which is
// the common case here. A dead holder's entry fails the birth-token liveness
// check the first time any waiter scans, is unlinked there and then, and its
// slot is free on that same pass. The recovery is the scan itself.

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// DefaultCap is the number of bd processes allowed to run at once. It matches
// DefaultThreshold; see the file comment for why the two are one number.
const DefaultCap = 16

// CapEnv overrides DefaultCap. Zero or negative disables the cap entirely,
// leaving registration and the warning in place.
const CapEnv = "BD_PROC_CAP"

// DefaultWait is how long a process parks for a slot before the cap yields.
//
// A bd against a wedged database already exits on its own in about ten seconds
// (measured in bd-x33), so thirty clears the known-bad case threefold while
// still bounding what one stuck holder can cost a caller. The unbounded case
// this guards is not a wedge but a database that answers every packet slowly,
// where nothing else imposes a deadline.
const DefaultWait = 30 * time.Second

// WaitEnv overrides DefaultWait with a Go duration string ("5s", "1m"). Zero
// means do not park: over the cap, act immediately per the mode.
const WaitEnv = "BD_PROC_CAP_WAIT"

// ModeEnv selects what happens when the wait deadline passes: "open" (default)
// runs anyway, "closed" exits with ErrCapped.
const ModeEnv = "BD_PROC_CAP_MODE"

// ModeClosed is the ModeEnv value that makes the cap a hard ceiling.
const ModeClosed = "closed"

// pollMin and pollMax bound the retry cadence while parked. It starts tight so
// a slot freed immediately is taken immediately, and backs off so a long park
// against a slow database is not itself a source of load: at pollMax a
// thirty-second wait costs about 150 scans, each one directory read.
const (
	pollMin = 20 * time.Millisecond
	pollMax = 200 * time.Millisecond
)

// noticeFloor is the wait below which Admission.Notice stays silent. Brief
// parking is the cap working as intended on a burst that drained, and saying so
// on every invocation would train people to ignore the line that matters.
const noticeFloor = time.Second

// Policy is the cap configuration in force for one invocation. A zero Policy
// has no cap, which is how every disabled path behaves.
type Policy struct {
	// Cap is the number of processes allowed to run at once. Zero or negative
	// disables the cap.
	Cap int
	// Wait is how long to park for a slot before yielding. Zero does not park.
	Wait time.Duration
	// FailClosed makes a wait that reaches its deadline return ErrCapped
	// instead of running anyway.
	FailClosed bool
	// Poll overrides the retry cadence while parked. Tests set it; production
	// leaves it zero and gets the pollMin/pollMax backoff.
	Poll time.Duration
}

// DefaultPolicy resolves the cap from the environment. It mirrors
// defaultRegistry: BD_PROC_PRESSURE_DISABLE turns the whole package off, and
// with it the cap, because a cap that cannot read the registry cannot count.
func DefaultPolicy() Policy {
	if truthy(os.Getenv(DisableEnv)) {
		return Policy{}
	}
	return Policy{
		Cap:        capFromEnv(),
		Wait:       waitFromEnv(),
		FailClosed: strings.EqualFold(strings.TrimSpace(os.Getenv(ModeEnv)), ModeClosed),
	}
}

func capFromEnv() int {
	raw := strings.TrimSpace(os.Getenv(CapEnv))
	if raw == "" {
		return DefaultCap
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		// A typo must not silently remove the bound. Disabling the cap is
		// something an operator does on purpose, by writing 0.
		return DefaultCap
	}
	return n
}

func waitFromEnv() time.Duration {
	raw := strings.TrimSpace(os.Getenv(WaitEnv))
	if raw == "" {
		return DefaultWait
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d < 0 {
		return DefaultWait
	}
	return d
}

// Enabled reports whether this policy bounds anything.
func (p Policy) Enabled() bool { return p.Cap > 0 }

// ErrCapped reports that the cap was full for the whole wait and the policy is
// fail-closed. It is the only error Acquire returns, and only ever under
// Policy.FailClosed.
type ErrCapped struct {
	Cap    int
	Waited time.Duration
}

func (e *ErrCapped) Error() string {
	// A zero wait is a configured choice, not a slot that freed instantly, and
	// "waited 0s and gave up" reads as a bug in the cap rather than as the
	// setting it is.
	//
	// The test is the rounded wait, not the raw one: a Wait of 0 still measures
	// the microseconds Acquire spent scanning, so an exact comparison would
	// never take this branch and the message would always read "0s".
	waited := e.Waited.Round(time.Millisecond)
	how := fmt.Sprintf("no slot freed in %s", waited)
	if waited == 0 {
		how = fmt.Sprintf("%s is 0, so bd did not wait", WaitEnv)
	}
	return fmt.Sprintf(
		"concurrency cap reached: %d bd %s already running (%s=%d) and %s. "+
			"Set %s=open to run anyway, or raise %s.",
		e.Cap, plural(e.Cap), CapEnv, e.Cap, how, ModeEnv, CapEnv,
	)
}

// plural is the noun for a count of bd processes. An operator-facing line that
// says "1 bd processes" reads as a bug in the tool that printed it.
func plural(n int) string {
	if n == 1 {
		return "process"
	}
	return "processes"
}

// Admission describes how this process got its slot. The zero Admission is
// valid and describes an uncapped start, which is what callers get whenever the
// cap could not run.
type Admission struct {
	// Report is the concurrency seen at the moment this process began work.
	Report Report
	// Cap is the cap that was in force, or zero when uncapped.
	Cap int
	// Waited is how long this process was parked before it began work.
	Waited time.Duration
	// Expired is true when the wait reached its deadline and the cap yielded.
	// It is the signal that the bound is being exceeded, not merely approached.
	Expired bool
}

// Notice returns the operator-facing line for this admission, or the empty
// string when there is nothing worth saying. A deadline expiry always speaks,
// because it means the cap stopped bounding; a park that ended in time speaks
// only once it was long enough for someone to have felt it.
func (a Admission) Notice() string {
	if a.Waited <= 0 || a.Cap <= 0 {
		return ""
	}
	if a.Expired {
		return fmt.Sprintf(
			"Warning: waited %s for a bd slot (%s=%d) and proceeded anyway — the concurrency cap is "+
				"being exceeded, not enforced. Check database health ('bd doctor --check-health'); "+
				"set %s=%s to make the cap refuse instead.",
			a.Waited.Round(time.Millisecond), CapEnv, a.Cap, ModeEnv, ModeClosed,
		)
	}
	if a.Waited < noticeFloor {
		return ""
	}
	return fmt.Sprintf(
		"Note: waited %s for a bd slot (%s=%d). Calls are arriving faster than they finish.",
		a.Waited.Round(time.Millisecond), CapEnv, a.Cap,
	)
}

// Acquire registers this process and, if the cap is full, parks until a slot
// frees or the wait deadline passes. The returned release function removes this
// process's entry and must be deferred; it is safe to call on every path,
// including the error one.
//
// An error is returned only under Policy.FailClosed, and only as *ErrCapped.
// Every other failure — an unreadable registry, an unwritable entry, a
// malformed peer — degrades to an uncapped start, because a cap that has lost
// track of its peers must not be the thing that stops bd from running.
func Acquire(command string, p Policy) (Admission, func(), error) {
	return defaultRegistry().Acquire(command, p)
}

// Acquire is the Registry-scoped form of the package-level Acquire.
func (r *Registry) Acquire(command string, p Policy) (Admission, func(), error) {
	noop := func() {}
	if r == nil || r.Dir == "" || !p.Enabled() {
		rep, release := r.Register(command)
		return Admission{Report: rep}, release, nil
	}
	if err := os.MkdirAll(r.Dir, 0o700); err != nil {
		return Admission{}, noop, nil
	}

	// Enroll optimistically as running. The uncontended path — which is nearly
	// every invocation — then costs exactly what it cost before the cap
	// existed: one write and one scan. Only a process that finds itself over
	// the line pays for the demotion and the park.
	self, ok := r.enroll(command, StateRunning)
	if !ok {
		return Admission{}, noop, nil
	}
	release := r.releaser(self.PID)

	rep := r.scan()
	if rep.Running() <= p.Cap {
		return Admission{Report: rep, Cap: p.Cap}, release, nil
	}

	return r.park(self, p, release)
}

// park demotes this process to waiting and polls for a slot. It returns when
// one is claimed or the deadline decides.
//
// Demoting first is what keeps a crowd from deadlocking: if every process over
// the line parks, the running count falls to zero, and the admission order
// below refills the slots in arrival order rather than letting all of them
// stampede back in at once.
func (r *Registry) park(self Peer, p Policy, release func()) (Admission, func(), error) {
	self.State = StateWaiting
	if !r.writeEntry(self) {
		// The entry cannot be updated, so peers would keep counting this
		// process as running while it waits — parking would deadlock the pool
		// against a slot nobody holds. Run instead.
		return Admission{Report: r.scan(), Cap: p.Cap}, release, nil
	}

	start := r.now()
	deadline := start.Add(p.Wait)
	backoff := pollMin
	if p.Poll > 0 {
		backoff = p.Poll
	}

	for {
		rep := r.scan()
		if slots := p.Cap - rep.Running(); slots > 0 && admissionRank(rep, self.PID) < slots {
			return r.promote(self, p, r.now().Sub(start), false), release, nil
		}

		now := r.now()
		if !now.Before(deadline) {
			break
		}
		nap := backoff
		if left := deadline.Sub(now); nap > left {
			nap = left
		}
		time.Sleep(nap)
		if p.Poll <= 0 {
			backoff = min(backoff*2, pollMax)
		}
	}

	waited := r.now().Sub(start)
	if p.FailClosed {
		release()
		return Admission{Cap: p.Cap, Waited: waited}, func() {}, &ErrCapped{Cap: p.Cap, Waited: waited}
	}
	return r.promote(self, p, waited, true), release, nil
}

// promote marks this process running again and reports the concurrency it is
// joining. The report is taken after the write so it counts this process as
// running, matching what the uncontended path in Acquire returns.
//
// A failed write is not fatal: the entry stays marked waiting, which
// undercounts this process against the cap but never blocks it, and it is
// unlinked by release as normal.
func (r *Registry) promote(self Peer, p Policy, waited time.Duration, expired bool) Admission {
	self.State = StateRunning
	_ = r.writeEntry(self)
	return Admission{Report: r.scan(), Cap: p.Cap, Waited: waited, Expired: expired}
}

// admissionRank is how many parked peers are ahead of pid in arrival order.
// Report.WaitingPeers is already oldest-first, so this is first-come-first-
// served, and a waiter only claims a slot when its rank is inside the number of
// free ones. Two waiters scanning at the same instant can still both claim the
// last slot — this is an advisory cap, not a mutex — but the ordering keeps the
// overshoot to the width of that race instead of the size of the crowd.
//
// A pid absent from the list ranks first. That happens when the registry became
// unreadable or another process unlinked the entry, and in both cases the right
// answer is to stop waiting for peers that can no longer be counted.
func admissionRank(rep Report, pid int) int {
	for i, w := range rep.WaitingPeers() {
		if w.PID == pid {
			return i
		}
	}
	return 0
}
