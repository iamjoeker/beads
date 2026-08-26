package workspacegate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// mixedTestGates builds n distinct gates rooted in one temp dir, so a test
// can reason about a set without caring which sorted order they land in.
func mixedTestGates(t *testing.T, names ...string) []Gate {
	t.Helper()
	dir := t.TempDir()
	gates := make([]Gate, 0, len(names))
	for _, n := range names {
		g, err := ForPhysicalRoot(filepath.Join(dir, n))
		if err != nil {
			t.Fatalf("ForPhysicalRoot(%s): %v", n, err)
		}
		gates = append(gates, g)
	}
	return gates
}

// The behavior bd init depends on: within ONE acquisition, some gates are
// held exclusively and others shared. A second acquirer must then be blocked
// on the exclusive ones and admitted on the shared ones.
func TestAcquireMixedAppliesPerGateModes(t *testing.T) {
	gates := mixedTestGates(t, "ws", "sharedroot")
	wsGate, sharedRoot := gates[0], gates[1]

	m, err := AcquireMixed(context.Background(), Options{},
		GateMode{Gate: wsGate, Mode: Exclusive},
		GateMode{Gate: sharedRoot, Mode: Shared},
	)
	if err != nil {
		t.Fatalf("AcquireMixed: %v", err)
	}
	defer func() { _ = m.Release() }()

	// The shared one admits another shared holder...
	h, err := sharedRoot.Acquire(context.Background(), Shared, Options{})
	if err != nil {
		t.Fatalf("second SHARED holder on the shared-mode gate must be admitted: %v", err)
	}
	_ = h.Release()

	// ...but still excludes an exclusive one, which is the whole reason a
	// demoted gate is held at all rather than dropped from the set.
	if _, err := sharedRoot.Acquire(context.Background(), Exclusive, Options{}); !errors.Is(err, ErrBusy) {
		t.Fatalf("EXCLUSIVE on a shared-mode gate must be busy, got %v", err)
	}

	// The exclusive one excludes everything.
	if _, err := wsGate.Acquire(context.Background(), Shared, Options{}); !errors.Is(err, ErrBusy) {
		t.Fatalf("SHARED on an exclusive-mode gate must be busy, got %v", err)
	}
}

// Dedupe must keep the STRONGER mode. A caller that names one gate both ways
// (init's workspace gate and its resolved physical root can be the same file)
// asked for exclusivity somewhere in the set and must get it, whichever order
// the duplicates arrive in.
func TestAcquireMixedDedupeKeepsExclusive(t *testing.T) {
	for _, tc := range []struct {
		name  string
		modes [2]Mode
	}{
		{"shared then exclusive", [2]Mode{Shared, Exclusive}},
		{"exclusive then shared", [2]Mode{Exclusive, Shared}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := mixedTestGates(t, "root")[0]
			m, err := AcquireMixed(context.Background(), Options{},
				GateMode{Gate: g, Mode: tc.modes[0]},
				GateMode{Gate: g, Mode: tc.modes[1]},
			)
			if err != nil {
				t.Fatalf("AcquireMixed: %v", err)
			}
			defer func() { _ = m.Release() }()

			if len(m.handles) != 1 {
				t.Fatalf("duplicate gate must collapse to one acquisition, got %d", len(m.handles))
			}
			if got := m.handles[0].Mode(); got != Exclusive {
				t.Fatalf("collapsed mode = %s, want exclusive (dedupe must not downgrade)", got)
			}
			if _, err := g.Acquire(context.Background(), Shared, Options{}); !errors.Is(err, ErrBusy) {
				t.Fatalf("collapsed hold must exclude a SHARED acquirer, got %v", err)
			}
		})
	}
}

// A failure partway through a mixed set releases what it already took,
// regardless of the modes involved.
func TestAcquireMixedReleasesOnPartialFailure(t *testing.T) {
	gates := mixedTestGates(t, "a-first", "z-second")
	first, second := gates[0], gates[1]
	if first.Path() > second.Path() {
		first, second = second, first
	}

	blocker := mustAcquire(t, second, Exclusive, Options{})
	defer func() { _ = blocker.Release() }()

	if _, err := AcquireMixed(context.Background(), Options{},
		GateMode{Gate: first, Mode: Shared},
		GateMode{Gate: second, Mode: Exclusive},
	); !errors.Is(err, ErrBusy) {
		t.Fatalf("AcquireMixed must fail busy on the blocked gate, got %v", err)
	}

	// The first gate must not still be held by the abandoned attempt.
	h, err := first.Acquire(context.Background(), Exclusive, Options{})
	if err != nil {
		t.Fatalf("gate acquired before the failure was not released: %v", err)
	}
	_ = h.Release()
}

// Options.Wait stays a TOTAL budget across a mixed set, exactly as for
// AcquireAll — the mixed path must not turn it into a per-gate budget.
func TestAcquireMixedTotalWaitBudget(t *testing.T) {
	gates := mixedTestGates(t, "a-first", "z-second")
	first, second := gates[0], gates[1]
	if first.Path() > second.Path() {
		first, second = second, first
	}

	// Block BOTH so the budget is spent on the first and none is left for
	// the second; the whole call must still return within about Wait.
	b1 := mustAcquire(t, first, Exclusive, Options{})
	defer func() { _ = b1.Release() }()
	b2 := mustAcquire(t, second, Exclusive, Options{})
	defer func() { _ = b2.Release() }()

	const budget = 200 * time.Millisecond
	start := time.Now()
	_, err := AcquireMixed(context.Background(), Options{Wait: budget, PollInterval: 10 * time.Millisecond},
		GateMode{Gate: first, Mode: Shared},
		GateMode{Gate: second, Mode: Exclusive},
	)
	elapsed := time.Since(start)
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("want ErrBusy, got %v", err)
	}
	if elapsed > 2*budget {
		t.Fatalf("elapsed %s exceeds twice the TOTAL budget %s — Wait became per-gate", elapsed, budget)
	}
}

func TestAcquireMixedRejectsZeroGate(t *testing.T) {
	if _, err := AcquireMixed(context.Background(), Options{}, GateMode{Mode: Shared}); err == nil {
		t.Fatal("a zero Gate must be rejected, not silently skipped")
	}
}

// SameAs is what cmd/bd uses to recognize one particular gate inside a
// planned set, so it must agree with the equality AcquireMixed dedupes by —
// including across path spellings that canonicalize to the same file.
func TestGateSameAs(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	direct, err := ForPhysicalRoot(filepath.Join(dir, "dolt"))
	if err != nil {
		t.Fatal(err)
	}
	viaLink, err := ForPhysicalRoot(filepath.Join(link, "dolt"))
	if err != nil {
		t.Fatal(err)
	}
	if !direct.SameAs(viaLink) {
		t.Fatalf("gates for the same physical root must compare equal: %s vs %s", direct.Path(), viaLink.Path())
	}

	other, err := ForPhysicalRoot(filepath.Join(dir, "embeddeddolt"))
	if err != nil {
		t.Fatal(err)
	}
	if direct.SameAs(other) {
		t.Fatal("sibling roots must not compare equal")
	}
	if (Gate{}).SameAs(direct) || direct.SameAs(Gate{}) {
		t.Fatal("a zero Gate must never compare equal to a real one")
	}
}
