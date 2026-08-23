package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/steveyegge/beads/cmd/bd/doctor"
	"github.com/steveyegge/beads/internal/debug"
	"github.com/steveyegge/beads/internal/procpressure"
)

// procPressureReport is the concurrency snapshot taken in main(), before Cobra
// parses anything. It is reported later, from the root PersistentPreRunE, once
// --quiet is resolved.
//
// Split this way because the two halves want different moments. Registration
// belongs at the top of main() so the entry covers the process's whole
// lifetime — `bd version` carries the same ~93MB startup cost as `bd list`, so
// leaving cheap-looking commands out of the count would undercount exactly the
// fan-out that piles up. Reporting has to wait for flag parsing, because
// warning past a --quiet the user asked for is how a warning gets ignored.
var procPressureReport procpressure.Report

// registerProcPressure records this process in the concurrency registry and
// returns its release function. See internal/procpressure for why bd counts
// itself at all (bd-x33).
func registerProcPressure() func() {
	report, release := procpressure.Register(commandNameFromArgs(os.Args, isRootSubcommand))
	procPressureReport = report
	return release
}

// reportProcPressure writes the pile-up warning to stderr, once, if the count
// this process saw at startup was over the threshold.
//
// stderr, not stdout: every bd command may be asked for --json, and a warning
// that corrupts a JSON document is a warning that gets suppressed permanently.
func reportProcPressure() {
	if debug.IsQuiet() {
		return
	}
	if msg := procPressureReport.Warning(); msg != "" {
		fmt.Fprintln(os.Stderr, msg)
	}
}

// checkProcPressure is the `bd doctor` readout of concurrent bd processes. It
// takes a fresh scan rather than reusing the startup snapshot: doctor is the
// tool an operator reaches for while a host is struggling, and what matters
// then is the count right now.
//
// It is a warning, never an error. A high count is a symptom of a slow
// database, not a broken beads installation, and failing doctor over it would
// send people to fix the wrong thing.
func checkProcPressure() doctorCheck {
	check := doctorCheck{Name: "Process Concurrency", Category: doctor.CategoryRuntime}

	report := procpressure.Scan()
	if report.Now.IsZero() {
		check.Status = statusOK
		check.Message = "not instrumented on this host"
		check.Detail = "the process registry could not be read; concurrency is unmeasured, not unhealthy"
		return check
	}

	count := report.Count()
	if !report.Over() {
		check.Status = statusOK
		check.Message = fmt.Sprintf("%d bd %s running (threshold %d)", count, pluralProcess(count), report.Threshold)
		return check
	}

	oldest := report.Peers[0]
	check.Status = statusWarning
	check.Message = fmt.Sprintf("%d bd %s running concurrently (threshold %d)", count, pluralProcess(count), report.Threshold)
	check.Detail = fmt.Sprintf("oldest is %q, running %s", oldest.Command, oldest.Age(report.Now).Round(time.Millisecond))
	check.Fix = "Check database health ('bd doctor --check-health'); calls are arriving faster than they finish"
	return check
}

func pluralProcess(n int) string {
	if n == 1 {
		return "process"
	}
	return "processes"
}

// commandNameFromArgs picks the subcommand out of argv for use as a label in
// the registry, so a pile-up report can name the call that is holding things
// up.
//
// It runs before Cobra parses anything, so it cannot ask Cobra what the flags
// are — and rather than guess which flags take a value (guess wrong on
// `bd --db foo list` and the label becomes "foo"; guess wrong on
// `bd --json list` and it becomes "bd"), it takes the first token that
// isCommand recognizes as a real subcommand. A bare `bd`, an unrecognized
// token, or flags only all label as "bd".
func commandNameFromArgs(argv []string, isCommand func(string) bool) string {
	for _, arg := range argv[1:] {
		if strings.HasPrefix(arg, "-") {
			continue
		}
		if isCommand(arg) {
			return arg
		}
	}
	return "bd"
}

// isRootSubcommand reports whether name is a registered bd subcommand or alias.
// Safe to call from main() before Execute: init() has already attached every
// subcommand to rootCmd by then.
func isRootSubcommand(name string) bool {
	for _, c := range rootCmd.Commands() {
		if c.Name() == name || c.HasAlias(name) {
			return true
		}
	}
	return false
}
