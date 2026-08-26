package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/steveyegge/beads/internal/doltserver"
	"github.com/steveyegge/beads/internal/workspacegate"
)

// bd-436: the shared-server dolt root is the ONE physical root every
// workspace on the machine resolves to. bd init used to take it EXCLUSIVE,
// which serialized every init on the box and hard-failed ordinary commands
// in unrelated workspaces (their SHARED acquisition is non-blocking and
// contention is never fail-open). These tests pin the resulting asymmetry:
// init shares that root, root-REPLACING maintenance still owns it.

// sharedServerGateEnv puts the process in shared-server mode with a private
// shared dir, and shortens the exclusive wait so a refusal is a fast failure
// rather than a five-second one.
func sharedServerGateEnv(t *testing.T) string {
	t.Helper()
	resetGateTestEnv(t)
	sharedDir := t.TempDir()
	t.Setenv("BEADS_DOLT_SHARED_SERVER", "1")
	t.Setenv("BEADS_SHARED_SERVER_DIR", sharedDir)

	oldWait := exclusiveGateWait
	exclusiveGateWait = 20 * time.Millisecond
	t.Cleanup(func() { exclusiveGateWait = oldWait })
	return sharedDir
}

// sharedRootGate is the gate the tests assert about, resolved the same way
// production does.
func sharedRootGate(t *testing.T) workspacegate.Gate {
	t.Helper()
	root, err := doltserver.SharedDoltPath()
	if err != nil {
		t.Fatalf("SharedDoltPath: %v", err)
	}
	g, err := workspacegate.ForPhysicalRoot(root)
	if err != nil {
		t.Fatalf("ForPhysicalRoot(%s): %v", root, err)
	}
	return g
}

// sharedServerWorkspace creates a workspace whose metadata says shared-server,
// so ResolvePhysicalRoots lands on the shared root for it.
func sharedServerWorkspace(t *testing.T) string {
	t.Helper()
	beadsDir := filepath.Join(t.TempDir(), ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	meta := `{"backend":"dolt","database":"beads.db","dolt_mode":"shared-server"}`
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), []byte(meta), 0o600); err != nil {
		t.Fatal(err)
	}
	return beadsDir
}

// The bd-436 regression itself: two workspaces initializing against the same
// shared server must not exclude each other. Before the fix the second call
// failed with ErrBusy after burning the whole exclusive wait budget.
func TestInitGatesDoNotExcludeAnotherInitOnTheSharedRoot(t *testing.T) {
	sharedServerGateEnv(t)
	root, err := doltserver.SharedDoltPath()
	if err != nil {
		t.Fatal(err)
	}

	first, err := acquireInitWorkspaceGates(context.Background(), sharedServerWorkspace(t), "bd init", root)
	if err != nil {
		t.Fatalf("first init acquisition: %v", err)
	}
	defer func() { _ = first.Release() }()

	second, err := acquireInitWorkspaceGates(context.Background(), sharedServerWorkspace(t), "bd init", root)
	if err != nil {
		t.Fatalf("a concurrent init in a DIFFERENT workspace must not be blocked by the first: %v", err)
	}
	_ = second.Release()
}

// The demotion must not become a drop: init still HOLDS the shared root, just
// shared. Something that would replace the root is still excluded by it, and
// ordinary shared users are still admitted.
func TestInitGatesHoldSharedRootSharedNotDropped(t *testing.T) {
	sharedServerGateEnv(t)
	root, err := doltserver.SharedDoltPath()
	if err != nil {
		t.Fatal(err)
	}
	rootGate := sharedRootGate(t)

	h, err := acquireInitWorkspaceGates(context.Background(), sharedServerWorkspace(t), "bd init", root)
	if err != nil {
		t.Fatalf("init acquisition: %v", err)
	}
	defer func() { _ = h.Release() }()

	if _, err := rootGate.Acquire(context.Background(), workspacegate.Exclusive, workspacegate.Options{}); !errors.Is(err, workspacegate.ErrBusy) {
		t.Fatalf("init must still exclude an EXCLUSIVE holder of the shared root, got %v", err)
	}
	sh, err := rootGate.Acquire(context.Background(), workspacegate.Shared, workspacegate.Options{})
	if err != nil {
		t.Fatalf("an ordinary command's SHARED hold on the shared root must be admitted: %v", err)
	}
	_ = sh.Release()
}

// The protection init actually needs is intact: a maintenance operation that
// REPLACES the shared root (bd migrate to/from shared-server) holds it
// exclusively, and init refuses to run underneath it.
func TestInitGatesRefuseUnderExclusiveSharedRootHolder(t *testing.T) {
	sharedServerGateEnv(t)
	root, err := doltserver.SharedDoltPath()
	if err != nil {
		t.Fatal(err)
	}

	blocker, err := sharedRootGate(t).Acquire(context.Background(), workspacegate.Exclusive,
		workspacegate.Options{Reason: "test migrate replacing the shared root"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = blocker.Release() }()

	h, err := acquireInitWorkspaceGates(context.Background(), sharedServerWorkspace(t), "bd init", root)
	if err == nil {
		_ = h.Release()
		t.Fatal("bd init must refuse while a root-replacing maintenance holds the shared root exclusively")
	}
	if !errors.Is(err, workspacegate.ErrBusy) {
		t.Fatalf("want ErrBusy, got %v", err)
	}
}

// The asymmetry's other half: only bd init opts into the demotion. Migrate
// and bootstrap go through acquireExclusiveWorkspaceGates and must still own
// the shared root outright — they replace it.
func TestMaintenanceGatesKeepSharedRootExclusive(t *testing.T) {
	sharedServerGateEnv(t)
	root, err := doltserver.SharedDoltPath()
	if err != nil {
		t.Fatal(err)
	}

	h, err := acquireExclusiveWorkspaceGates(context.Background(), sharedServerWorkspace(t), "bd migrate", root)
	if err != nil {
		t.Fatalf("maintenance acquisition: %v", err)
	}
	defer func() { _ = h.Release() }()

	if _, err := sharedRootGate(t).Acquire(context.Background(), workspacegate.Shared, workspacegate.Options{}); !errors.Is(err, workspacegate.ErrBusy) {
		t.Fatalf("root-replacing maintenance must exclude even SHARED users of the shared root, got %v", err)
	}
}

// The degenerate configuration planMaintenanceGateModes guards: a workspace
// whose .beads directory resolves to the SAME gate file as the shared root.
// The demotion must not reach the workspace gate there — it is what keeps two
// inits out of each other's workspace, and it is never a machine-wide claim.
func TestInitGatesNeverDemoteTheWorkspaceGate(t *testing.T) {
	sharedDir := sharedServerGateEnv(t)

	// Make the workspace gate and the shared-root gate the same file by
	// pointing the workspace at <sharedDir>/dolt.
	beadsDir := filepath.Join(sharedDir, "dolt")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	wsGate, err := workspacegate.ForWorkspace(beadsDir)
	if err != nil {
		t.Fatal(err)
	}
	if !wsGate.SameAs(sharedRootGate(t)) {
		t.Fatalf("setup did not collide the gates: %s vs %s", wsGate.Path(), sharedRootGate(t).Path())
	}

	h, err := acquireInitWorkspaceGates(context.Background(), beadsDir, "bd init", beadsDir)
	if err != nil {
		t.Fatalf("init acquisition: %v", err)
	}
	defer func() { _ = h.Release() }()

	if _, err := wsGate.Acquire(context.Background(), workspacegate.Shared, workspacegate.Options{}); !errors.Is(err, workspacegate.ErrBusy) {
		t.Fatalf("the workspace gate must stay EXCLUSIVE even when it collides with the shared root, got %v", err)
	}
}

// A workspace-PRIVATE root is unaffected: nothing about bd-436 licenses
// weakening the gate on a root only this workspace uses. Two inits in the
// same workspace still exclude each other via the workspace gate, and the
// private root itself is held exclusively.
func TestInitGatesKeepPrivateRootExclusive(t *testing.T) {
	resetGateTestEnv(t)
	oldWait := exclusiveGateWait
	exclusiveGateWait = 20 * time.Millisecond
	t.Cleanup(func() { exclusiveGateWait = oldWait })

	beadsDir := newGateTestWorkspace(t)
	privateRoot := filepath.Join(beadsDir, "embeddeddolt")

	h, err := acquireInitWorkspaceGates(context.Background(), beadsDir, "bd init", privateRoot)
	if err != nil {
		t.Fatalf("init acquisition: %v", err)
	}
	defer func() { _ = h.Release() }()

	rootGate, err := workspacegate.ForPhysicalRoot(privateRoot)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rootGate.Acquire(context.Background(), workspacegate.Shared, workspacegate.Options{}); !errors.Is(err, workspacegate.ErrBusy) {
		t.Fatalf("a workspace-private root must stay EXCLUSIVE under bd init, got %v", err)
	}
	if _, err := acquireInitWorkspaceGates(context.Background(), beadsDir, "bd init", privateRoot); !errors.Is(err, workspacegate.ErrBusy) {
		t.Fatalf("a second init in the SAME workspace must still be excluded, got %v", err)
	}
}
