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

// readOOMScoreAdj is a seam for tests. Raising a process's own oom_score_adj is
// permitted but lowering it again is not without CAP_SYS_RESOURCE, so a test
// that set the real value would leave the whole `go test` process sacrificial
// for the rest of the run. Swapping the reader is the only way to exercise the
// sacrificial branch without doing that.
var readOOMScoreAdj = procpressure.OOMScoreAdj

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
func reportProcPressure() {
	if debug.IsQuiet() {
		return
	}
	if msg := procPressureReport.Warning(); msg != "" {
		fmt.Fprintln(os.Stderr, msg)
		// Only alongside the pile-up warning, and only then. A standing OOM
		// bias is not news on its own — it is news when a pile-up is already
		// under way, because that is the pairing that killed the host on
		// 2026-08-16: many processes, each one picked first. Printing it on
		// every invocation would also cost every invocation a /proc read for a
		// line nobody is reading yet. `bd doctor` is where to ask on purpose.
		if msg := oomSacrificeNote(); msg != "" {
			fmt.Fprintln(os.Stderr, msg)
		}
	}
	if msg := procPressureAdmission.Notice(); msg != "" {
		fmt.Fprintln(os.Stderr, msg)
	}
}

// oomSacrificeNote is the second line of a pile-up warning: what the kernel
// will do with this pile. It is empty unless bd is actually biased toward being
// killed, so the common case adds nothing.
func oomSacrificeNote() string {
	adj, ok := readOOMScoreAdj()
	if !ok || !procpressure.SacrificialOOMScore(adj) {
		return ""
	}
	return fmt.Sprintf(
		"Warning: bd is running at oom_score_adj %d, so the kernel kills these processes before an "+
			"average one. An OOM kill is SIGKILL: bd cannot report it, and the calls simply stop "+
			"answering. If bd invocations are vanishing without an error, look here first.",
		adj,
	)
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

// checkOOMScore is the `bd doctor` readout of where bd sits in the kernel's
// kill order (bd-kih, from bd-x33).
//
// This check exists because the failure it describes cannot report itself. An
// OOM kill is SIGKILL: the process does not log, does not run its deferred
// release, and returns no exit code to anyone watching. On 2026-08-16 seven bd
// processes died that way over ninety minutes and nothing surfaced anywhere, so
// a queueing collapse looked like nothing at all. The bias that selected them
// is readable from inside a healthy bd, which makes it the one part of that
// story a diagnostic can show BEFORE the kills start.
//
// Warning, never error: a sacrificial bias is a property of whatever spawned
// bd, not of the beads installation, and failing doctor over it would send
// people to fix the wrong thing.
func checkOOMScore() doctorCheck {
	check := doctorCheck{Name: "OOM Priority", Category: doctor.CategoryRuntime}

	adj, ok := readOOMScoreAdj()
	if !ok {
		// Deliberately not "oom_score_adj 0". An absent reading is not the
		// kernel default, and rendering it as one would assert bd is unbiased
		// on a host where that is simply unknown.
		check.Status = statusOK
		check.Message = "not reported on this host"
		check.Detail = "the per-process OOM bias is a Linux interface; bd cannot tell where it sits " +
			"in this kernel's kill order"
		return check
	}

	if !procpressure.SacrificialOOMScore(adj) {
		check.Status = statusOK
		if adj == 0 {
			check.Message = "oom_score_adj 0 (kernel default)"
			check.Detail = "bd is no more likely to be killed under memory pressure than any other process"
		} else {
			check.Message = fmt.Sprintf("oom_score_adj %d (protected)", adj)
			check.Detail = "bd is biased away from the OOM killer"
		}
		return check
	}

	check.Status = statusWarning
	check.Message = fmt.Sprintf("oom_score_adj %d (killed before an average process)", adj)
	check.Detail = "an OOM kill is SIGKILL, so bd cannot log it, run its cleanup, or return an exit " +
		"code; under memory pressure these calls stop answering with no error anywhere"
	// Deliberately points at the ancestry rather than at bd. Measured on the
	// town host 2026-08-25: bd under tmux read 0 while bd launched from a
	// desktop terminal read 200, inherited unchanged down
	// systemd(100) -> konsole(200) -> shell -> gt -> bd. bd cannot lower this
	// for itself — raising oom_score_adj is unprivileged, lowering it needs
	// CAP_SYS_RESOURCE — so the only place to act is the ancestor that set it.
	check.Fix = "bd inherits this from whatever spawned it, and cannot lower it without " +
		"CAP_SYS_RESOURCE; trace the ancestry (cat /proc/PID/oom_score_adj up the parents) and " +
		"clear it at the ancestor that sets it — commonly a desktop terminal's systemd app scope, " +
		"a supervisor's OOMScoreAdjust, or a shell profile"
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
