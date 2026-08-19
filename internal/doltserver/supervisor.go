// Supervisor detection: who, other than bd, owns the Dolt server process.
//
// Background: bd's own knobs — dolt.auto-start, BEADS_DOLT_AUTO_START,
// dolt.port — describe only what BD does. They say nothing about a process
// supervisor (systemd, launchd, a container runtime) that may own the server
// on the other end of the port. Error text that names dolt.auto-start as the
// reason a server is down is therefore an assertion bd has not earned: on a
// host where a systemd unit with Restart=always owns the server, the claim is
// simultaneously true and irrelevant, and it sends operators hunting through
// config layers while the real actor is the unit (gastownhall/beads bd-8ef).
//
// This file answers the question bd can actually answer — "what owns THIS
// process" — and lets callers name the supervisor instead of guessing at a
// cause. When detection returns nothing, callers must degrade to listing
// candidates rather than to asserting one.

package doltserver

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// supervisorProbeTimeout bounds the reachability probe used to turn "bd has no
// server of its own" into an observation about the port. These run only on
// error paths that terminate the command, so a short wait is affordable; it is
// still short enough that a firewalled remote host does not hang the CLI.
const supervisorProbeTimeout = 2 * time.Second

// Supervisor describes a process supervisor that owns a running Dolt server.
//
// A nil *Supervisor means "no supervisor detected", which includes the case
// where detection is not implemented for the platform. Callers must treat that
// as "unknown", not as "definitely unsupervised" — never turn a nil into a
// claim that bd owns the server.
type Supervisor struct {
	Kind     string // "systemd"; empty when nothing was detected
	Unit     string // unit name, e.g. "gt-dolt.service"
	UserUnit bool   // true for a `systemctl --user` unit, false for a system unit
	Restart  string // the unit's Restart= policy ("always", "no", ...); "" if unknown
}

// AutoRestarts reports whether the supervisor brings the server back up on its
// own after bd stops it. When true, `bd dolt stop` does not keep the server
// down, and any advice of the form "stop the server, then touch the files" is
// wrong unless it stops the unit.
//
// The answer depends on the signal bd sends, not just on the policy. gracefulStop
// sends SIGTERM, and systemd counts termination by SIGTERM as a CLEAN exit — it
// is one of the four signals (SIGHUP, SIGINT, SIGTERM, SIGPIPE) excluded from
// the "terminated by a signal" clause in systemd.service(5). Dolt also handles
// the signal itself and exits 0. Both routes agree: the exit is a success. So
// only policies that restart after a SUCCESSFUL exit revive the server;
// "on-failure", "on-abnormal", "on-abort" and "on-watchdog" leave it down.
//
// Caveat: if the server ignores SIGTERM, gracefulStop escalates to SIGKILL,
// which IS an unclean exit and does revive a failure-triggered unit. That path
// is the exception, not the case this predicts.
func (s *Supervisor) AutoRestarts() bool {
	if s == nil {
		return false
	}
	return s.Restart == "always" || s.Restart == "on-success"
}

// StopCommand returns the command that actually stops the server: the
// supervisor's stop when one owns the process, bd's own stop otherwise.
func (s *Supervisor) StopCommand() string {
	if s == nil || s.Unit == "" {
		return "bd dolt stop"
	}
	return fmt.Sprintf("systemctl %sstop %s", s.systemctlScope(), s.Unit)
}

// StartCommand returns the command that brings the server back up after a
// StopCommand. Starting a server directly while the unit is stopped would
// leave the supervisor believing the service is down, and leaves bd owning a
// server the supervisor will later fight over.
func (s *Supervisor) StartCommand() string {
	if s == nil || s.Unit == "" {
		return "bd dolt start"
	}
	return fmt.Sprintf("systemctl %sstart %s", s.systemctlScope(), s.Unit)
}

// Describe renders the supervisor for operator-facing messages, e.g.
// "systemd unit gt-dolt.service (Restart=always)". Returns "" when nothing was
// detected, so callers can test the result rather than the pointer.
func (s *Supervisor) Describe() string {
	if s == nil || s.Unit == "" {
		return ""
	}
	desc := fmt.Sprintf("%s unit %s", s.Kind, s.Unit)
	if s.Restart != "" {
		desc += fmt.Sprintf(" (Restart=%s)", s.Restart)
	}
	return desc
}

func (s *Supervisor) systemctlScope() string {
	if s.UserUnit {
		return "--user "
	}
	return ""
}

// DetectSupervisor reports which supervisor, if any, owns the given process.
//
// Detection is by cgroup membership rather than by looking for a known unit
// name: it answers "what owns THIS process", which is the question that
// matters, and it stays correct when the unit is named something other than
// the one bd expects, or when the running server was started outside its unit.
// Returns nil when nothing is detected.
func DetectSupervisor(pid int) *Supervisor {
	if runtime.GOOS != "linux" || pid <= 0 {
		return nil
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", pid))
	if err != nil {
		return nil
	}
	unit, userUnit := parseSystemdUnit(string(data))
	if unit == "" {
		return nil
	}
	if !unitOwns(unit, userUnit, pid) {
		return nil
	}
	return &Supervisor{
		Kind:     "systemd",
		Unit:     unit,
		UserUnit: userUnit,
		Restart:  systemctlProperty(unit, userUnit, "Restart"),
	}
}

// PortOwner reports what bd could observe about a local TCP port at the moment
// an error was raised. It exists so error text can say what is true (something
// is listening; a unit owns it) instead of asserting a config knob as the cause.
//
// Listening=false does NOT mean "nothing owns this server" — it means nothing
// answered right now. Supervisor may be nil even when Listening is true: the
// listener may be unsupervised, or detection may not be available here.
type PortOwner struct {
	Listening  bool
	PID        int
	Supervisor *Supervisor
}

// InspectLocalPort probes a port on this host and, if something is listening,
// identifies the listener and any supervisor that owns it.
//
// Local only: the listener lookup reads this machine's process table, so it is
// meaningless for a server configured on another host. Use InspectRemoteAddr
// for those.
func InspectLocalPort(port int) PortOwner {
	if port <= 0 {
		return PortOwner{}
	}
	owner := PortOwner{Listening: probeReachable(net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))}
	if !owner.Listening {
		return owner
	}
	owner.PID = findPIDOnPort(port)
	owner.Supervisor = DetectSupervisor(owner.PID)
	return owner
}

// InspectRemoteAddr reports whether a server on another host answers right now.
// Only reachability is knowable from here — the listener's PID and supervisor
// live on that machine.
func InspectRemoteAddr(host string, port int) PortOwner {
	if host == "" || port <= 0 {
		return PortOwner{}
	}
	return PortOwner{Listening: probeReachable(net.JoinHostPort(host, strconv.Itoa(port)))}
}

// Describe renders what was observed about the port, for operator-facing error
// text. Returns "" when nothing is listening, since "no answer" is the caller's
// story to tell alongside its candidate causes.
func (o PortOwner) Describe(port int) string {
	if !o.Listening {
		return ""
	}
	desc := fmt.Sprintf("Something is already listening on port %d", port)
	if o.PID > 0 {
		desc += fmt.Sprintf(" (PID %d)", o.PID)
	}
	if s := o.Supervisor.Describe(); s != "" {
		desc += ", owned by " + s
	}
	return desc + "."
}

// The two reasons bd declines to start a local server. Both are facts about
// bd's configuration, and neither is evidence about the server on the port —
// keeping them as values makes that separation visible at the call sites and
// keeps the explanation text from hard-coding the wrong one.
const (
	ReasonAutoStartOff = "auto-start is off (dolt.auto-start: false in config.yaml, or BEADS_DOLT_AUTO_START=0)"
	ReasonExternalMode = "this project is configured for an externally managed server " +
		"(explicit dolt.port, shared-server mode, or dolt.auto-start: false)"
)

// ExplainLocalServerNotStarted renders the error for "bd has no server of its
// own on this local port, and will not start one". reason says why bd declined
// — one of ReasonAutoStartOff or ReasonExternalMode.
//
// bd reaching this point proves only that no PID file names a live server of
// bd's — it proves nothing about the port, which is why the port is probed
// before anything is claimed about it. When something answers, the message
// names that listener and its supervisor; when nothing answers, it lists
// candidate causes rather than promoting reason to the cause.
func ExplainLocalServerNotStarted(port int, reason string) string {
	owner := InspectLocalPort(port)
	if !owner.Listening {
		return fmt.Sprintf("Dolt server did not answer on port %d, and bd did not start one.\n\n%s",
			port, explainDeclined(reason))
	}

	msg := fmt.Sprintf("%s\n\n"+
		"bd has no Dolt server of its own for this project, and will not start one:\n"+
		"  %s.\n"+
		"That is a statement about bd, not about the server already on this port.\n",
		owner.Describe(port), reason)

	if desc := owner.Supervisor.Describe(); desc != "" {
		msg += fmt.Sprintf("\nThat server belongs to %s — control it there, not with bd:\n"+
			"  To stop it:  %s\n"+
			"  To start it: %s\n",
			desc, owner.Supervisor.StopCommand(), owner.Supervisor.StartCommand())
	} else {
		// Detection came back empty, which is "unknown", not "unsupervised".
		// Naming the possibilities is honest; claiming bd could take it over
		// would be the same overreach this whole file exists to remove.
		msg += "\nbd cannot tell what owns that listener from here. It may be an external\n" +
			"supervisor (systemd, launchd, a container runtime), another project's server,\n" +
			"or one started by hand — whatever started it owns its lifecycle, not bd.\n"
	}
	return msg + "\n  To check status: bd dolt status"
}

// ExplainRemoteServerNotStarted renders the error for a Dolt server configured
// on another host. bd never starts those, so dolt.auto-start cannot explain
// their state — only reachability is knowable from here, so only reachability
// is reported.
func ExplainRemoteServerNotStarted(host string, port int) string {
	if InspectRemoteAddr(host, port).Listening {
		return fmt.Sprintf("Configured Dolt server at %s:%d is reachable, but bd has no server of its\n"+
			"own for this project and will not start one for an external host.\n\n"+
			"  bd dolt status   # detailed external-server check", host, port)
	}
	return fmt.Sprintf("Configured Dolt server at %s:%d did not answer a connection probe just now.\n\n"+
		"bd will not start it: the server lives on another host. dolt.auto-start decides\n"+
		"only whether bd spawns a LOCAL server — it does not govern that one, and enabling\n"+
		"it would not bring that server up. Whatever runs it there owns its lifecycle.\n\n"+
		"Verify it is running and reachable from this host:\n"+
		"  nc -zv %s %d  # or curl %s:%d for an HTTP-style check\n"+
		"  bd dolt status   # detailed external-server check",
		host, port, host, port, host, port)
}

// ExplainAutoStartDisabled renders the hint for a local Dolt server that did
// not answer while bd's auto-start is off.
//
// The one thing bd knows is why IT did nothing. Everything past that is a guess,
// so this lists candidates instead of naming a cause — an operator told
// "auto-start is disabled" on a supervised host goes hunting through config
// layers for a knob that cannot explain the outage and cannot fix it (bd-8ef).
func ExplainAutoStartDisabled() string {
	return explainDeclined(ReasonAutoStartOff)
}

// explainDeclined states why bd did not start a server, then refuses to promote
// that reason to the cause of the outage, listing what bd cannot rule out.
func explainDeclined(reason string) string {
	return "bd did not try to start a server: " + reason + ".\n" +
		"That explains bd's inaction, not the outage. bd cannot tell which of these applies:\n" +
		"  - an external supervisor (systemd, launchd, a container runtime) owns this server\n" +
		"    and it is currently down — start it there; `bd dolt start` would start a separate\n" +
		"    server the supervisor does not know about\n" +
		"  - the server was stopped by hand, or it crashed\n" +
		"  - no server has been started for this project yet\n\n" +
		"  To check status: bd dolt status\n" +
		"  To start a bd-managed server: bd dolt start"
}

// probeReachable reports whether addr accepts a TCP connection right now. The
// handshake is drained before closing because a bare Close sends RST, which
// dolt sql-server can treat as an aborted MySQL handshake (see probe.go).
func probeReachable(addr string) bool {
	_, err := ProbeSQLServer("tcp", addr, supervisorProbeTimeout)
	return err == nil
}

// unitOwns reports whether pid is the unit's MAIN process, i.e. the process the
// unit's Restart= policy actually applies to.
//
// Cgroup membership alone is too weak a signal. Every child inherits its
// parent's cgroup, so a Dolt server that bd started itself sits in the cgroup of
// whatever unit bd was running under — a timer-driven unit, or a town daemon.
// Without this check, such a server is reported as supervised, and bd tells the
// operator to stop an unrelated unit to bring Dolt down. That advice does
// nothing to Dolt and stops something else.
//
// Only a positively different MainPID disproves ownership. If systemctl cannot
// be reached or gives no answer, we cannot disprove it and keep the detection —
// failing back to cgroup membership rather than to silence.
func unitOwns(unit string, userUnit bool, pid int) bool {
	return ownsPID(systemctlProperty(unit, userUnit, "MainPID"), pid)
}

// ownsPID decides ownership from a unit's raw MainPID property. Split out from
// unitOwns so the rule is testable without a live systemd.
//
// "" (no systemctl, no bus, unknown unit) and "0" (unit not running) are both
// absent answers, not contradicting ones — they leave the cgroup evidence
// standing. Only a parsed, non-zero MainPID naming a different process disproves
// ownership.
func ownsPID(mainPIDProperty string, pid int) bool {
	parsed, err := strconv.Atoi(strings.TrimSpace(mainPIDProperty))
	if err != nil || parsed == 0 {
		return true
	}
	return parsed == pid
}

// parseSystemdUnit extracts the owning systemd unit from the contents of
// /proc/<pid>/cgroup. Returns ("", false) when the process is not in a unit.
//
// cgroup v2 writes a single "0::<path>" line; v1 writes one line per
// controller, of which the "name=systemd" one carries the unit path. Both are
// handled by scanning every line for a path whose last segment is a unit.
func parseSystemdUnit(cgroupFile string) (unit string, userUnit bool) {
	for _, line := range strings.Split(cgroupFile, "\n") {
		parts := strings.SplitN(strings.TrimSpace(line), ":", 3)
		if len(parts) != 3 {
			continue
		}
		cgPath := parts[2]
		leaf := path.Base(cgPath)
		if !strings.HasSuffix(leaf, ".service") {
			// A .scope (tmux, a login session, a manually started process) is
			// not a supervisor: nothing restarts its members.
			continue
		}
		// user@<uid>.service is the per-user service manager itself. A process
		// sitting directly in it belongs to no unit of its own.
		if strings.HasPrefix(leaf, "user@") {
			continue
		}
		return leaf, strings.Contains(cgPath, "/user@")
	}
	return "", false
}

// systemctlProperty reads one property of a unit. Returns "" on any failure —
// callers degrade to less specific advice rather than to wrong advice.
func systemctlProperty(unit string, userUnit bool, property string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	args := []string{}
	if userUnit {
		args = append(args, "--user")
	}
	args = append(args, "show", "-p", property, "--value", unit)

	out, err := exec.CommandContext(ctx, "systemctl", args...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
