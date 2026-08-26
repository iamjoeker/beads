//go:build linux

package procpressure

import "os"

// oomScoreAdjPath is a variable so tests can point the read at a fixture
// instead of at the running process.
var oomScoreAdjPath = "/proc/self/oom_score_adj"

func oomScoreAdj() (int, bool) {
	data, err := os.ReadFile(oomScoreAdjPath) // #nosec G304 -- constant procfs path; the variable exists only so tests can point at a fixture
	if err != nil {
		// No procfs, or a kernel that does not expose the knob. Unreadable is
		// unmeasured, not zero.
		return 0, false
	}
	return parseOOMScoreAdj(string(data))
}
