package procpressure

import (
	"strconv"
	"strings"
)

// OOMScoreAdj reports this process's OOM-killer bias and whether the host
// exposes one at all.
//
// Why this belongs here (bd-kih, from bd-x33): a bd killed by the OOM killer
// receives SIGKILL, so it cannot log its own death, run its deferred release,
// or set an exit code anyone reads. That is the whole reason the 2026-08-16
// collapse was invisible — seven bd processes died across ninety minutes and no
// error surfaced anywhere. The bias that selected them is the one part of that
// story bd can still see, because it is readable from inside the process before
// anything goes wrong.
//
// So this does not detect a kill. It reports the standing that makes a kill
// likely, which is the only OOM fact bd is alive to report.
//
// Reported values run from -1000 (never killed) to 1000 (killed first), with 0
// the kernel default. A positive value means the kernel picks this process
// ahead of an average one under global memory pressure; bd runs at 200 in the
// town, inherited from whatever spawns it.
//
// The second result is false when the host does not report a bias — every
// non-Linux platform, and a Linux whose procfs is not mounted. False means
// unmeasured, never zero: a caller must not render a missing reading as "the
// kernel default", because those are different situations and only one of them
// is known to be safe.
func OOMScoreAdj() (int, bool) { return oomScoreAdj() }

// SacrificialOOMScore reports whether adj puts this process ahead of an average
// one in the kernel's kill order. Callers should pass a reading they know to be
// present; an absent one is not a zero.
func SacrificialOOMScore(adj int) bool { return adj > 0 }

// parseOOMScoreAdj reads the contents of an oom_score_adj file. It is separate
// from the file read so the parse is testable on every platform, including the
// ones where the file does not exist.
//
// It rejects out-of-range values rather than clamping them. A number outside
// [-1000, 1000] did not come from this kernel interface, and reporting it as a
// bias would put a fabricated number in front of an operator; unmeasured is the
// honest answer.
func parseOOMScoreAdj(contents string) (int, bool) {
	n, err := strconv.Atoi(strings.TrimSpace(contents))
	if err != nil {
		return 0, false
	}
	if n < -1000 || n > 1000 {
		return 0, false
	}
	return n, true
}
