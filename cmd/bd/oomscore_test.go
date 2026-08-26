package main

import (
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/beads/cmd/bd/doctor"
	"github.com/steveyegge/beads/internal/debug"
	"github.com/steveyegge/beads/internal/procpressure"
)

// stubOOMScoreAdj pins what bd believes its OOM bias to be for the duration of
// one test. Tests must not set the real value: raising oom_score_adj is
// permitted but lowering it back is not without CAP_SYS_RESOURCE, so a test
// that did would leave the whole `go test` process first in the kill order.
func stubOOMScoreAdj(t *testing.T, adj int, ok bool) {
	t.Helper()
	saved := readOOMScoreAdj
	t.Cleanup(func() { readOOMScoreAdj = saved })
	readOOMScoreAdj = func() (int, bool) { return adj, ok }
}

func TestCheckOOMScoreUnmeasuredIsNotZero(t *testing.T) {
	// The distinction this guards: "we did not measure" and "the kernel default
	// is in force" are different facts, and only the second one is reassuring.
	// Rendering an absent reading as 0 would tell an operator bd is unbiased on
	// a host where that is simply unknown.
	stubOOMScoreAdj(t, 0, false)

	check := checkOOMScore()
	if check.Status != statusOK {
		t.Errorf("Status = %q for an unmeasured host, want %q", check.Status, statusOK)
	}
	if check.Category != doctor.CategoryRuntime {
		t.Errorf("Category = %q, want %q", check.Category, doctor.CategoryRuntime)
	}
	if strings.Contains(check.Message, "oom_score_adj") {
		t.Errorf("Message = %q; an unmeasured host must not be reported as a numeric bias", check.Message)
	}
	if check.Fix != "" {
		t.Errorf("Fix = %q; there is nothing to fix on a host that reports no bias", check.Fix)
	}
}

func TestCheckOOMScoreDefaultAndProtectedPass(t *testing.T) {
	for _, tt := range []struct {
		name string
		adj  int
		want string
	}{
		{name: "kernel default", adj: 0, want: "kernel default"},
		{name: "protected", adj: -1000, want: "protected"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			stubOOMScoreAdj(t, tt.adj, true)

			check := checkOOMScore()
			if check.Status != statusOK {
				t.Errorf("Status = %q at oom_score_adj %d, want %q", check.Status, tt.adj, statusOK)
			}
			if !strings.Contains(check.Message, tt.want) {
				t.Errorf("Message = %q, want it to contain %q", check.Message, tt.want)
			}
			if check.Fix != "" {
				t.Errorf("Fix = %q at oom_score_adj %d; nothing here needs changing", check.Fix, tt.adj)
			}
		})
	}
}

func TestCheckOOMScoreWarnsWhenSacrificial(t *testing.T) {
	// 200 is the value bd actually runs at in the town, and the one that made
	// seven kills on 2026-08-16 invisible.
	stubOOMScoreAdj(t, 200, true)

	check := checkOOMScore()
	if check.Status != statusWarning {
		t.Errorf("Status = %q at oom_score_adj 200, want %q — an error would fail bd doctor over a "+
			"setting that belongs to the spawner, not to beads", check.Status, statusWarning)
	}
	if !strings.Contains(check.Message, "200") {
		t.Errorf("Message = %q, want the actual bias named", check.Message)
	}
	if !strings.Contains(check.Detail, "SIGKILL") {
		t.Errorf("Detail = %q, want it to say why the failure is silent", check.Detail)
	}
	if check.Fix == "" {
		t.Error("Fix is empty; a warning an operator cannot act on is noise")
	}
}

func TestCheckOOMScoreBoundaryIsAboveZero(t *testing.T) {
	// Every process shares 0, so only a positive bias is worth a warning. One
	// point either side of the boundary decides whether bd doctor is quiet on
	// ordinary hosts.
	stubOOMScoreAdj(t, -1, true)
	if got := checkOOMScore().Status; got != statusOK {
		t.Errorf("Status = %q at oom_score_adj -1, want %q", got, statusOK)
	}
	stubOOMScoreAdj(t, 1, true)
	if got := checkOOMScore().Status; got != statusWarning {
		t.Errorf("Status = %q at oom_score_adj 1, want %q", got, statusWarning)
	}
}

func TestReportProcPressureNotesSacrificialOOMScore(t *testing.T) {
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
	debug.SetQuiet(false)
	stubOOMScoreAdj(t, 200, true)

	procPressureReport = over
	got := captureStderr(t, reportProcPressure)
	if !strings.Contains(got, over.Warning()) {
		t.Errorf("stderr = %q, want it to still carry the pile-up warning", got)
	}
	if !strings.Contains(got, "oom_score_adj 200") {
		t.Errorf("stderr = %q, want the OOM bias reported alongside the pile-up", got)
	}

	// Under the threshold there is no pile-up, so there is nothing for the OOM
	// bias to be news about — and reporting it on every healthy invocation is
	// how a warning gets filtered out permanently.
	procPressureReport = procpressure.Report{
		Peers:     []procpressure.Peer{{PID: 1, Command: "list", Started: now}},
		Threshold: procpressure.DefaultThreshold,
		Now:       now,
	}
	if got := captureStderr(t, reportProcPressure); got != "" {
		t.Errorf("stderr = %q for a healthy count on a sacrificial host, want nothing", got)
	}
}

func TestOOMSacrificeNoteSilentUnlessSacrificial(t *testing.T) {
	stubOOMScoreAdj(t, 0, false)
	if got := oomSacrificeNote(); got != "" {
		t.Errorf("oomSacrificeNote() = %q for an unmeasured host, want empty", got)
	}
	stubOOMScoreAdj(t, 0, true)
	if got := oomSacrificeNote(); got != "" {
		t.Errorf("oomSacrificeNote() = %q at the kernel default, want empty", got)
	}
	stubOOMScoreAdj(t, 200, true)
	if got := oomSacrificeNote(); !strings.Contains(got, "SIGKILL") {
		t.Errorf("oomSacrificeNote() = %q, want it to explain why the kill is silent", got)
	}
}
