package doltserver

import (
	"fmt"
	"net"
	"os"
	"strings"
	"testing"
)

func TestParseSystemdUnit(t *testing.T) {
	tests := []struct {
		name     string
		cgroup   string
		wantUnit string
		wantUser bool
	}{
		{
			// The live gt-dolt.service on a cgroup v2 host.
			name:     "cgroup v2 user unit",
			cgroup:   "0::/user.slice/user-1000.slice/user@1000.service/app.slice/gt-dolt.service\n",
			wantUnit: "gt-dolt.service",
			wantUser: true,
		},
		{
			name:     "cgroup v2 system unit",
			cgroup:   "0::/system.slice/gt-dolt.service\n",
			wantUnit: "gt-dolt.service",
			wantUser: false,
		},
		{
			// A server started by hand from a shell: nothing restarts it.
			name:     "tmux scope is not a supervisor",
			cgroup:   "0::/user.slice/user-1000.slice/user@1000.service/tmux-spawn-219312f4.scope\n",
			wantUnit: "",
		},
		{
			// Directly in the per-user service manager's own cgroup — that is
			// the manager, not a unit supervising this process.
			name:     "user manager service is not a unit",
			cgroup:   "0::/user.slice/user-1000.slice/user@1000.service\n",
			wantUnit: "",
		},
		{
			name: "cgroup v1 picks the systemd hierarchy",
			cgroup: "11:devices:/system.slice\n" +
				"3:cpu,cpuacct:/system.slice\n" +
				"1:name=systemd:/system.slice/gt-dolt.service\n",
			wantUnit: "gt-dolt.service",
			wantUser: false,
		},
		{
			name:     "empty file",
			cgroup:   "",
			wantUnit: "",
		},
		{
			name:     "malformed lines are skipped",
			cgroup:   "garbage\n0::\n",
			wantUnit: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			unit, userUnit := parseSystemdUnit(tt.cgroup)
			if unit != tt.wantUnit {
				t.Errorf("unit = %q, want %q", unit, tt.wantUnit)
			}
			if unit != "" && userUnit != tt.wantUser {
				t.Errorf("userUnit = %v, want %v", userUnit, tt.wantUser)
			}
		})
	}
}

// gracefulStop sends SIGTERM, which systemd scores as a CLEAN exit (SIGTERM is
// one of the four signals excluded from "terminated by a signal" in
// systemd.service(5), and Dolt handles it and exits 0 besides). Only policies
// that restart after SUCCESS revive the server.
//
// The failure-triggered policies are the ones that matter to get right: claiming
// Restart=on-failure revives the server would tell an operator their stop failed
// when it worked.
func TestSupervisorAutoRestarts(t *testing.T) {
	tests := []struct {
		name string
		sup  *Supervisor
		want bool
	}{
		{"nil supervisor", nil, false},
		{"Restart=always", &Supervisor{Unit: "gt-dolt.service", Restart: "always"}, true},
		{"Restart=on-success", &Supervisor{Unit: "gt-dolt.service", Restart: "on-success"}, true},
		{"Restart=on-failure does not revive a clean SIGTERM exit", &Supervisor{Unit: "gt-dolt.service", Restart: "on-failure"}, false},
		{"Restart=on-abnormal does not revive a clean SIGTERM exit", &Supervisor{Unit: "gt-dolt.service", Restart: "on-abnormal"}, false},
		{"Restart=on-abort does not revive a handled signal", &Supervisor{Unit: "gt-dolt.service", Restart: "on-abort"}, false},
		{"Restart=on-watchdog does not revive a clean exit", &Supervisor{Unit: "gt-dolt.service", Restart: "on-watchdog"}, false},
		{"Restart=no", &Supervisor{Unit: "gt-dolt.service", Restart: "no"}, false},
		{"unknown policy", &Supervisor{Unit: "gt-dolt.service"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.sup.AutoRestarts(); got != tt.want {
				t.Errorf("AutoRestarts() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSupervisorCommands(t *testing.T) {
	tests := []struct {
		name      string
		sup       *Supervisor
		wantStop  string
		wantStart string
		wantDesc  string
	}{
		{
			name:      "no supervisor falls back to bd",
			sup:       nil,
			wantStop:  "bd dolt stop",
			wantStart: "bd dolt start",
			wantDesc:  "",
		},
		{
			name:      "user unit",
			sup:       &Supervisor{Kind: "systemd", Unit: "gt-dolt.service", UserUnit: true, Restart: "always"},
			wantStop:  "systemctl --user stop gt-dolt.service",
			wantStart: "systemctl --user start gt-dolt.service",
			wantDesc:  "systemd unit gt-dolt.service (Restart=always)",
		},
		{
			name:      "system unit",
			sup:       &Supervisor{Kind: "systemd", Unit: "dolt.service"},
			wantStop:  "systemctl stop dolt.service",
			wantStart: "systemctl start dolt.service",
			wantDesc:  "systemd unit dolt.service",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.sup.StopCommand(); got != tt.wantStop {
				t.Errorf("StopCommand() = %q, want %q", got, tt.wantStop)
			}
			if got := tt.sup.StartCommand(); got != tt.wantStart {
				t.Errorf("StartCommand() = %q, want %q", got, tt.wantStart)
			}
			if got := tt.sup.Describe(); got != tt.wantDesc {
				t.Errorf("Describe() = %q, want %q", got, tt.wantDesc)
			}
		})
	}
}

func TestDetectSupervisorRejectsBadPID(t *testing.T) {
	if sup := DetectSupervisor(0); sup != nil {
		t.Errorf("DetectSupervisor(0) = %+v, want nil", sup)
	}
	if sup := DetectSupervisor(-1); sup != nil {
		t.Errorf("DetectSupervisor(-1) = %+v, want nil", sup)
	}
}

// unitOwns is what separates "this unit supervises the server" from "the server
// merely inherited a unit's cgroup from whoever started it". A Dolt process
// spawned by bd while bd ran under, say, a timer-driven unit sits in that unit's
// cgroup without being its main process; reporting it as supervised sends the
// operator to stop an unrelated unit.
func TestOwnsPID(t *testing.T) {
	tests := []struct {
		name     string
		property string
		pid      int
		want     bool
	}{
		{"unit's main process is the server", "3580495", 3580495, true},
		{"unit names a different main process", "1416", 3580495, false},
		{"no systemctl answer leaves cgroup evidence standing", "", 3580495, true},
		{"unit not running reports MainPID 0", "0", 3580495, true},
		{"unparseable property is not a contradiction", "n/a", 3580495, true},
		{"trailing newline from systemctl --value", "3580495\n", 3580495, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ownsPID(tt.property, tt.pid); got != tt.want {
				t.Errorf("ownsPID(%q, %d) = %v, want %v", tt.property, tt.pid, got, tt.want)
			}
		})
	}
}

// The live half of the same rule. Skips on machines where the test process is
// not inside a service cgroup, so it can only ever add confidence, never remove
// it — TestOwnsPID above is the load-bearing check.
func TestUnitOwnsAgainstLiveSystemd(t *testing.T) {
	// A unit that does not exist yields no MainPID. That is a missing answer,
	// not a contradicting one, so detection must be kept rather than discarded.
	if !unitOwns("bd-does-not-exist-9f3a.service", true, 1234) {
		t.Error("unitOwns should keep detection when systemd gives no MainPID")
	}

	// Self-check against the live system: this test process is never the main
	// process of any unit, so any unit systemd does name must be rejected. This
	// is the case the check exists for, and it fails if the comparison is
	// dropped.
	self := os.Getpid()
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", self))
	if err != nil {
		t.Skip("no /proc cgroup for this process")
	}
	unit, userUnit := parseSystemdUnit(string(data))
	if unit == "" {
		t.Skip("test process is not inside a systemd service cgroup")
	}
	if mainPID := systemctlProperty(unit, userUnit, "MainPID"); mainPID == "" || mainPID == "0" {
		t.Skipf("systemd reports no MainPID for %s", unit)
	}
	if unitOwns(unit, userUnit, self) {
		t.Errorf("unitOwns(%s, pid=%d) = true, but the test process is not that unit's main process", unit, self)
	}
	if sup := DetectSupervisor(self); sup != nil {
		t.Errorf("DetectSupervisor(self) = %+v, want nil — cgroup membership alone is not supervision", sup)
	}
}

func TestPortOwnerDescribe(t *testing.T) {
	tests := []struct {
		name  string
		owner PortOwner
		want  string
	}{
		{
			name:  "nothing listening has no story of its own",
			owner: PortOwner{},
			want:  "",
		},
		{
			name:  "listener with no identifiable pid",
			owner: PortOwner{Listening: true},
			want:  "Something is already listening on port 3307.",
		},
		{
			name:  "listener with pid but no supervisor",
			owner: PortOwner{Listening: true, PID: 1234},
			want:  "Something is already listening on port 3307 (PID 1234).",
		},
		{
			name: "supervised listener names the unit",
			owner: PortOwner{
				Listening:  true,
				PID:        1234,
				Supervisor: &Supervisor{Kind: "systemd", Unit: "gt-dolt.service", Restart: "always"},
			},
			want: "Something is already listening on port 3307 (PID 1234), owned by systemd unit gt-dolt.service (Restart=always).",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.owner.Describe(3307); got != tt.want {
				t.Errorf("Describe(3307) = %q, want %q", got, tt.want)
			}
		})
	}
}

// The bead this file exists for (bd-8ef): bd used to name dolt.auto-start as the
// reason the server was down. On a host where a systemd unit owns the server
// that claim is true and irrelevant at once, and it sent two agents through gt's
// config layers hunting a knob that could neither explain nor fix the outage.
//
// The rule this locks in: bd may state why IT did nothing, but must not promote
// that to the cause, and must offer an external supervisor as a live candidate.
func TestExplainAutoStartDisabledDoesNotAssertACause(t *testing.T) {
	msg := ExplainAutoStartDisabled()

	if !strings.Contains(msg, "external supervisor") {
		t.Errorf("message must offer an external supervisor as a candidate cause; got:\n%s", msg)
	}
	if !strings.Contains(msg, "explains bd's inaction, not the outage") {
		t.Errorf("message must disclaim auto-start as the cause; got:\n%s", msg)
	}
	// The knob must still be named — an operator who did set it deserves to
	// know bd saw it. It just may not stand alone as the explanation.
	if !strings.Contains(msg, "dolt.auto-start: false") {
		t.Errorf("message should still name the setting bd acted on; got:\n%s", msg)
	}
}

func TestExplainLocalServerNotStartedIdlePort(t *testing.T) {
	port := closedLocalPort(t)

	msg := ExplainLocalServerNotStarted(port, ReasonAutoStartOff)

	if !strings.Contains(msg, "did not answer") {
		t.Errorf("an idle port should be reported as observed, not asserted; got:\n%s", msg)
	}
	if !strings.Contains(msg, "external supervisor") {
		t.Errorf("message must offer an external supervisor as a candidate cause; got:\n%s", msg)
	}
	if strings.Contains(msg, "Something is already listening") {
		t.Errorf("nothing was listening; got:\n%s", msg)
	}
}

// The false claim the bead reported: a systemd-owned server is alive on the
// port, but bd has no PID file for it, so bd announced the server was not
// running. Probing the port is what makes the message true.
func TestExplainLocalServerNotStartedBusyPortDoesNotClaimTheServerIsDown(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("cannot bind a local listener: %v", err)
	}
	defer func() { _ = ln.Close() }()
	port := ln.Addr().(*net.TCPAddr).Port

	for _, reason := range []string{ReasonAutoStartOff, ReasonExternalMode} {
		msg := ExplainLocalServerNotStarted(port, reason)

		if !strings.Contains(msg, fmt.Sprintf("Something is already listening on port %d", port)) {
			t.Errorf("reason %q: a live listener must be reported; got:\n%s", reason, msg)
		}
		if strings.Contains(msg, "did not answer") {
			t.Errorf("reason %q: must not claim the port is idle while something is listening; got:\n%s", reason, msg)
		}
		if !strings.Contains(msg, "a statement about bd, not about the server") {
			t.Errorf("reason %q: must separate bd's config from the server's state; got:\n%s", reason, msg)
		}
		if !strings.Contains(msg, reason) {
			t.Errorf("reason %q: must say why bd declined to start a server; got:\n%s", reason, msg)
		}
	}
}

func TestExplainRemoteServerNotStartedBlamesNeitherKnob(t *testing.T) {
	port := closedLocalPort(t)

	msg := ExplainRemoteServerNotStarted("127.0.0.1", port)

	if !strings.Contains(msg, "did not answer a connection probe") {
		t.Errorf("reachability must be reported as probed, not assumed; got:\n%s", msg)
	}
	if !strings.Contains(msg, "does not govern that one") {
		t.Errorf("message must deny that dolt.auto-start explains a remote server's state; got:\n%s", msg)
	}
	if !strings.Contains(msg, fmt.Sprintf("nc -zv 127.0.0.1 %d", port)) {
		t.Errorf("message must hand over a way to verify the remote server; got:\n%s", msg)
	}
}

// closedLocalPort returns a local TCP port with nothing listening on it: bound
// to claim it, then released. A later bind by an unrelated process is possible
// but vanishingly unlikely within a test, and would only ever make an assertion
// about "no listener" fail loudly rather than pass wrongly.
func closedLocalPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("cannot bind a local listener: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil {
		t.Fatalf("closing probe listener: %v", err)
	}
	return port
}
