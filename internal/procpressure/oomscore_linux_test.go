//go:build linux

package procpressure

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOOMScoreAdjReadsTheRunningProcess(t *testing.T) {
	// The real path, on a real Linux host: the reading must be present. A false
	// here would mean bd reports "unmeasured" on exactly the platform the
	// outage happened on.
	adj, ok := OOMScoreAdj()
	if !ok {
		t.Fatalf("OOMScoreAdj() reported no reading on linux; %s unreadable?", oomScoreAdjPath)
	}
	if adj < -1000 || adj > 1000 {
		t.Errorf("OOMScoreAdj() = %d, outside the kernel's [-1000, 1000]", adj)
	}
}

func TestOOMScoreAdjMissingFileIsUnmeasured(t *testing.T) {
	// A host without the knob must read as unmeasured rather than as the
	// kernel default, because "0" is a claim that bd is not sacrificial.
	orig := oomScoreAdjPath
	t.Cleanup(func() { oomScoreAdjPath = orig })
	oomScoreAdjPath = filepath.Join(t.TempDir(), "absent")

	if adj, ok := OOMScoreAdj(); ok {
		t.Errorf("OOMScoreAdj() = (%d, true) for a missing file, want unmeasured", adj)
	}
}

func TestOOMScoreAdjReadsTheTownValue(t *testing.T) {
	orig := oomScoreAdjPath
	t.Cleanup(func() { oomScoreAdjPath = orig })
	path := filepath.Join(t.TempDir(), "oom_score_adj")
	if err := os.WriteFile(path, []byte("200\n"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	oomScoreAdjPath = path

	adj, ok := OOMScoreAdj()
	if !ok {
		t.Fatal("OOMScoreAdj() reported no reading for a readable file")
	}
	if adj != 200 {
		t.Errorf("OOMScoreAdj() = %d, want 200", adj)
	}
	if !SacrificialOOMScore(adj) {
		t.Error("SacrificialOOMScore(200) = false; the town value must read as sacrificial")
	}
}
