package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/beads/cmd/bd/doctor"
	"github.com/steveyegge/beads/internal/debug"
	"github.com/steveyegge/beads/internal/procpressure"
)

func TestCommandNameFromArgs(t *testing.T) {
	known := map[string]bool{"list": true, "show": true, "ready": true}
	isCommand := func(s string) bool { return known[s] }

	tests := []struct {
		name string
		argv []string
		want string
	}{
		{"plain subcommand", []string{"bd", "list"}, "list"},
		{"subcommand with args", []string{"bd", "show", "bd-x33"}, "show"},
		{"flag before subcommand", []string{"bd", "--json", "list"}, "list"},
		{"value flag before subcommand", []string{"bd", "--db", "beads", "list"}, "list"},
		{"inline value flag", []string{"bd", "--db=beads", "ready"}, "ready"},
		{"short flag cluster", []string{"bd", "-q", "list"}, "list"},
		{"bare bd", []string{"bd"}, "bd"},
		{"flags only", []string{"bd", "--version"}, "bd"},
		{"unknown subcommand", []string{"bd", "frobnicate"}, "bd"},
		// The whole reason for the isCommand lookup: a flag value that happens
		// to spell a real subcommand must not win over the actual subcommand.
		// It does here, and that is the accepted cost of not parsing flags —
		// the label is diagnostic only.
		{"flag value shadowing a command", []string{"bd", "--db", "list", "show"}, "list"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := commandNameFromArgs(tt.argv, isCommand); got != tt.want {
				t.Errorf("commandNameFromArgs(%q) = %q, want %q", tt.argv, got, tt.want)
			}
		})
	}
}

func TestIsRootSubcommandSeesRealCommands(t *testing.T) {
	// Guards the ordering contract in main(): registerProcPressure runs after
	// init() has attached every subcommand, so the lookup is not empty.
	if !isRootSubcommand("list") {
		t.Error(`isRootSubcommand("list") = false; the registry label would be "bd" for every invocation`)
	}
	if isRootSubcommand("definitely-not-a-bd-command") {
		t.Error("isRootSubcommand accepted a name that is not a subcommand")
	}
}

func TestReportProcPressureRespectsQuiet(t *testing.T) {
	now := time.Now()
	over := procpressure.Report{
		Peers: []procpressure.Peer{
			{PID: 1, Command: "list", Started: now.Add(-2 * time.Second)},
			{PID: 2, Command: "list", Started: now.Add(-time.Second)},
		},
		Threshold: 2,
		Now:       now,
	}

	saved := procPressureReport
	t.Cleanup(func() {
		procPressureReport = saved
		debug.SetQuiet(false)
	})
	procPressureReport = over

	debug.SetQuiet(true)
	quiet := captureStderr(t, reportProcPressure)
	if quiet != "" {
		t.Errorf("stderr = %q under --quiet, want nothing", quiet)
	}

	debug.SetQuiet(false)
	loud := captureStderr(t, reportProcPressure)
	if loud == "" {
		t.Fatal("stderr is empty for a report over the threshold; the alarm never fires")
	}
	if want := over.Warning(); loud != want+"\n" {
		t.Errorf("stderr = %q, want %q", loud, want+"\n")
	}
}

func TestReportProcPressureSilentUnderThreshold(t *testing.T) {
	saved := procPressureReport
	t.Cleanup(func() {
		procPressureReport = saved
		debug.SetQuiet(false)
	})

	debug.SetQuiet(false)
	// The zero report is what every failed registration produces. It must be
	// silent, or a host with no /run and no writable temp dir warns on every
	// single bd invocation.
	procPressureReport = procpressure.Report{}
	if got := captureStderr(t, reportProcPressure); got != "" {
		t.Errorf("stderr = %q for the zero report, want nothing", got)
	}

	now := time.Now()
	procPressureReport = procpressure.Report{
		Peers:     []procpressure.Peer{{PID: 1, Command: "list", Started: now}},
		Threshold: procpressure.DefaultThreshold,
		Now:       now,
	}
	if got := captureStderr(t, reportProcPressure); got != "" {
		t.Errorf("stderr = %q for a single healthy invocation, want nothing", got)
	}
}

func TestCheckProcPressureReportsWarningNotError(t *testing.T) {
	// A pile-up is a symptom of a slow database, not a broken installation.
	// Doctor must say so without failing, or operators go fix the wrong thing.
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	t.Setenv(procpressure.DisableEnv, "")
	t.Setenv(procpressure.ThresholdEnv, "1")

	// Nothing is registered in this fresh dir, so the scan finds zero peers and
	// the threshold of 1 is not met.
	quiet := checkProcPressure()
	if quiet.Status != statusOK {
		t.Errorf("Status = %q on an empty registry, want %q", quiet.Status, statusOK)
	}

	// Register one process so the threshold of 1 is met.
	_, release := procpressure.Register("list")
	defer release()

	loud := checkProcPressure()
	if loud.Status != statusWarning {
		t.Errorf("Status = %q over threshold, want %q — an error here would fail bd doctor over a busy host", loud.Status, statusWarning)
	}
	if loud.Category != doctor.CategoryRuntime {
		t.Errorf("Category = %q, want %q", loud.Category, doctor.CategoryRuntime)
	}
	if !strings.Contains(loud.Message, "1 bd process running") {
		t.Errorf("Message = %q, want the singular count", loud.Message)
	}
	if !strings.Contains(loud.Detail, `"list"`) {
		t.Errorf("Detail = %q, want the oldest command named", loud.Detail)
	}
	if loud.Fix == "" {
		t.Error("Fix is empty; a warning with no next step is noise")
	}
}

func TestCheckProcPressureUnmeasuredIsNotAFailure(t *testing.T) {
	// A host where the registry cannot be read must report "unmeasured", not a
	// clean bill of health and not a failure.
	base := t.TempDir()
	blocker := filepath.Join(base, "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("write blocker: %v", err)
	}
	t.Setenv("XDG_RUNTIME_DIR", blocker)
	t.Setenv(procpressure.DisableEnv, "")
	t.Setenv(procpressure.ThresholdEnv, "1")

	check := checkProcPressure()
	if check.Status != statusOK {
		t.Errorf("Status = %q, want %q — an unreadable registry is not a beads fault", check.Status, statusOK)
	}
	if !strings.Contains(check.Message, "not instrumented") {
		t.Errorf("Message = %q, want it to say concurrency is unmeasured rather than healthy", check.Message)
	}
}
