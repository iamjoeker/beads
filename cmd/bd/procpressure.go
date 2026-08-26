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

// procPressureAdmission records how this process got its slot, for the same
// reason and at the same moment as procPressureReport: the notice cannot be
// printed until --quiet is resolved.
var procPressureAdmission procpressure.Admission

// capExemptCommands are the subcommands the concurrency cap never gates.
//
// Two kinds, and both would turn the cap into the wedge it is designed to
// avoid. The first is anything an operator runs BECAUSE the town is piling up:
// `bd doctor` is the readout of the pile-up, and `bd dolt` starts and stops the
// database whose slowness caused it, so queueing either one behind the very
// backlog it is meant to clear would remove the way out. The second is anything
// long-lived: serve, db-proxy-child and events tail run for as long as they are
// wanted, and a gated one would hold its slot for that whole time and starve
// every short call behind it. send-metrics is a detached fire-and-forget flush
// that must never block its parent's exit.
//
// The empty string covers a bare `bd`, a flags-only invocation, and any argv
// whose first non-flag token is not a registered subcommand — see
// commandNameFromArgs. Those resolve to help, version and shell completion,
// which are cheap and must answer instantly.
var capExemptCommands = map[string]bool{
	"bd":             true,
	"version":        true,
	"help":           true,
	"completion":     true,
	"doctor":         true,
	"dolt":           true,
	"serve":          true,
	"db-proxy-child": true,
	"events":         true,
	"send-metrics":   true,
}

// capPolicyFor resolves the cap for one invocation, disabling it for the
// commands that must never queue.
func capPolicyFor(command string) procpressure.Policy {
	if capExemptCommands[command] {
		return procpressure.Policy{}
	}
	return procpressure.DefaultPolicy()
}

// acquireProcPressure records this process in the concurrency registry, waits
// for a slot if the cap is full, and returns its release function. See
// internal/procpressure for why bd counts itself at all (bd-x33) and
// internal/procpressure/cap.go for what the cap does when it is full (bd-91c).
//
// Under the default fail-open policy this never stops bd; it can only delay it.
// Under BD_PROC_CAP_MODE=closed a full cap exits before Cobra parses anything,
// which is why the refusal is written here rather than deferred to the --quiet
// gate: an error the caller must act on is not a warning to be suppressed.
func acquireProcPressure() func() {
	command := commandNameFromArgs(os.Args, isRootSubcommand)
	admission, release, err := procpressure.Acquire(command, capPolicyFor(command))
	procPressureAdmission = admission
	procPressureReport = admission.Report
	if err != nil {
		release()
		fmt.Fprintf(os.Stderr, "Error: %s\n", err)
		os.Exit(capExitCode)
	}
	return release
}

// capExitCode is what a fail-closed refusal exits with. It is sysexits.h's
// EX_TEMPFAIL: the request was well-formed and the caller should retry, which
// is exactly the contract a full cap offers and is distinguishable from the
// generic 1 that a real bd failure returns.
const capExitCode = 75

// reportProcPressure writes the pile-up warning and any cap notice to stderr,
// once, based on what this process saw at startup.
//
// stderr, not stdout: every bd command may be asked for --json, and a warning
// that corrupts a JSON document is a warning that gets suppressed permanently.
// The same holds for `create --silent`, whose stdout is a bare issue ID that
// scripts capture with id=$(bd create --silent ...).
//
// --silent does NOT suppress this warning, and deliberately so (bd-pyu asked).
// It shapes stdout for a caller parsing it; it says nothing about diagnostics,
// which is what stderr is for. Silencing the alarm for scripted callers would
// blind it precisely where it matters: automated fan-out is what produces a
// pile-up in the first place, so the callers most likely to pass --silent are
// the ones whose operator most needs to see the count climbing. --quiet, an
// explicit request for less output, is the flag that suppresses it.
func reportProcPressure() {
	if debug.IsQuiet() {
		return
	}
	if msg := procPressureReport.Warning(); msg != "" {
		fmt.Fprintln(os.Stderr, msg)
	}
	if msg := procPressureAdmission.Notice(); msg != "" {
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
		// The cap reads the same registry, so an unreadable one means bd is
		// running unbounded as well as uncounted. Saying so here is the only
		// place an operator learns it.
		check.Detail = "the process registry could not be read; concurrency is unmeasured, not unhealthy, " +
			"and the concurrency cap is not in force"
		return check
	}

	count := report.Count()
	if !report.Over() {
		check.Status = statusOK
		check.Message = fmt.Sprintf("%d bd %s running (threshold %d)", count, pluralProcess(count), report.Threshold)
		check.Detail = capDetail(report)
		return check
	}

	oldest := report.Peers[0]
	verb := "running"
	if oldest.Waiting() {
		verb = "waiting"
	}
	check.Status = statusWarning
	check.Message = fmt.Sprintf("%d bd %s running concurrently (threshold %d)", count, pluralProcess(count), report.Threshold)
	check.Detail = fmt.Sprintf("oldest is %q, %s %s", oldest.Command, verb, oldest.Age(report.Now).Round(time.Millisecond))
	if capMsg := capDetail(report); capMsg != "" {
		check.Detail += "; " + capMsg
	}
	check.Fix = "Check database health ('bd doctor --check-health'); calls are arriving faster than they finish"
	return check
}

// capDetail describes the concurrency cap alongside the count, so an operator
// reading a high number can tell a bound that is holding from no bound at all.
// The waiting count is the part that matters: peers parked behind the cap are
// the difference between throttling and a pile-up.
func capDetail(report procpressure.Report) string {
	policy := procpressure.DefaultPolicy()
	if !policy.Enabled() {
		return fmt.Sprintf("concurrency cap disabled (%s)", procpressure.CapEnv)
	}
	detail := fmt.Sprintf("cap %d, %d running", policy.Cap, report.Running())
	if waiting := len(report.WaitingPeers()); waiting > 0 {
		detail += fmt.Sprintf(", %d waiting for a slot", waiting)
	}
	return detail
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
