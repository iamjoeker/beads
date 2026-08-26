//go:build darwin

package doltserver

import (
	"os/exec"
	"strconv"
	"strings"
)

// SweepOrphanedTestServers reaps `dolt sql-server` processes that are
// provably leaked test debris: their working directory has been deleted, or
// sits under one of suiteTempRoots. On Darwin, process candidates come from
// ps and their working directories from lsof because /proc is unavailable.
//
// suiteTempRoots MUST be directories owned by the calling suite alone, never
// a shared/global temp directory. This is best-effort: process-listing errors
// and candidates whose cwd cannot be resolved are ignored.
//
// Returns the PIDs it sent a kill signal to.
func SweepOrphanedTestServers(suiteTempRoots ...string) []int {
	candidates := gatherDoltServerCandidates()
	return terminateDoltServerPIDs(selectOrphanTestServerPIDs(candidates, canonicalRoots(suiteTempRoots)))
}

func gatherDoltServerCandidates() []serverCandidate {
	out, err := exec.Command("ps", "-axo", "pid=,command=").Output()
	if err != nil {
		return nil
	}
	return gatherPSCandidates(out, readDarwinCwd)
}

func isDoltServerProcess(pid int) bool {
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "command=").Output()
	if err != nil {
		return false
	}
	return isDoltServerCmdline(strings.TrimSpace(string(out)))
}

// readDarwinCwd resolves pid's cwd from lsof's machine-readable field output.
// lsof emits the name as an `n` field after selecting descriptor `cwd`.
func readDarwinCwd(pid int) (cwd string, deleted bool, ok bool) {
	out, err := exec.Command(
		"lsof", "-a", "-p", strconv.Itoa(pid), "-d", "cwd", "-Fn",
	).Output()
	if err != nil {
		return "", false, false
	}
	for _, line := range strings.Split(string(out), "\n") {
		if !strings.HasPrefix(line, "n") {
			continue
		}
		cwd = strings.TrimPrefix(line, "n")
		const deletedSuffix = " (deleted)"
		if strings.HasSuffix(cwd, deletedSuffix) {
			return strings.TrimSuffix(cwd, deletedSuffix), true, true
		}
		if cwd != "" {
			return cwd, false, true
		}
	}
	return "", false, false
}
