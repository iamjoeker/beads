//go:build !linux

package procpressure

// oomScoreAdj reports no reading. The OOM killer's per-process bias is a Linux
// interface; the outage this instruments (bd-x33) happened on a Linux host, and
// inventing a "0" for macOS or Windows would tell an operator the kernel
// default is in force on a kernel that has no such setting.
func oomScoreAdj() (int, bool) { return 0, false }
