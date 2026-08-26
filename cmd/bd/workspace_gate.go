package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/beads/internal/doltserver"
	"github.com/steveyegge/beads/internal/workspacegate"
)

// Workspace operation gate wiring (see internal/workspacegate).
//
// Every store-opening command holds the workspace gate (and the physical
// database-root gate(s)) SHARED for its store/provider lifetime, so
// maintenance operations — mode migration, backup restore, bd init — can
// hold them EXCLUSIVELY and refuse to run over live bd activity instead of
// running blind. Acquisition happens once at the PersistentPreRunE
// chokepoint with the final mode preselected (there is deliberately no
// SH→EX upgrade in workspacegate), and release happens in
// PersistentPostRunE after the store closes.
//
// Posture (deliberate, from the adversarial design review):
//
//   - SHARED failures that are NOT contention (resolver error, gate file
//     unbuildable, unsupported filesystem) warn once to stderr and continue
//     UNGATED. The gate is cooperative; a normal `bd list` must never brick
//     an existing deployment because its network mount cannot flock. Note
//     the honest reading of fail-open: it means "not DETECTABLY contended",
//     not "not contended" — e.g. EACCES on another OS user's 0600 gate file
//     beside a cross-user shared root lands here and proceeds ungated even
//     though that user may be mid-maintenance (workspacegate documents
//     cross-user shared roots as unsupported).
//   - ErrBusy on SHARED means an exclusive maintenance operation is live on
//     this workspace: abort with an actionable error naming the holder
//     (the gate's busy detail carries pid/reason/host from the advisory
//     sidecar). Proceeding would race a migration/restore mid-replace.
//   - EXCLUSIVE failures of any kind are hard errors: maintenance refuses
//     rather than pretends.
//
// Known residual (PR-B2 scope, documented honestly): the pre-chokepoint
// DISCOVERY code paths (configfile.Load at main.go's early config probe and
// internal/beads.findDatabaseInBeadsDir) can perform the legacy
// config.json→metadata.json migration write BEFORE this gate is acquired —
// the chokepoint necessarily runs after workspace selection. Deferring that
// legacy write behind the gate is out of scope here.

// workspaceGateHandle is the gate set held for the current command, stored
// beside `store` because their lifetimes are paired: acquired just before
// the store-opening phase of PersistentPreRunE, released after store close
// in PersistentPostRunE. nil when the command runs ungated (skipsStoreInit
// path, fail-open posture, or no workspace on disk).
var workspaceGateHandle *workspacegate.MultiHandle

// exclusiveGateWait is how long EXCLUSIVE acquisitions poll before giving
// up: long enough to ride out a short-lived `bd list` finishing, short
// enough that a genuinely busy workspace fails with a clear message rather
// than hanging. SHARED acquisitions stay non-blocking (a live exclusive
// holder is a migration/restore — waiting seconds will not outlast it, and
// normal commands should fail fast with the holder's name). A var, not a
// const, so tests can shorten it.
var exclusiveGateWait = 5 * time.Second

// exclusiveGateOptions builds the acquisition options for an EXCLUSIVE
// hold: bounded wait, holder-info reason, and a single stderr note when the
// first attempt comes back busy so the wait does not look like a hang.
func exclusiveGateOptions(reason string) workspacegate.Options {
	return workspacegate.Options{
		Wait:   exclusiveGateWait,
		Reason: reason,
		OnWait: func(holder string) {
			if !quietFlag {
				// %q: the holder string comes from another process's gate
				// sidecar; quoting neutralizes terminal escape sequences a
				// hostile or corrupt sidecar could smuggle into stderr.
				fmt.Fprintf(os.Stderr, "waiting for other bd commands to finish (%q)...\n", holder) //nolint:gosec // G705: stderr, not a browser context; %q additionally neutralizes terminal escapes
			}
		},
	}
}

// closeStoreBeforeGateRelease enforces "gates outlive the store" on the
// error exits: PersistentPostRunE's early returns (auto-commit/auto-export
// failures) and PersistentPreRunE failures after the store opened would
// otherwise release the gates while the store is still open, letting an
// exclusive maintenance operation start against storage that has not
// quiesced. Close whatever is still open, then release. The success paths
// nil out store/uowProvider after their own close, so this is a no-op
// there.
func closeStoreBeforeGateRelease() {
	ctx := rootCtx
	if ctx == nil {
		ctx = context.Background()
	}
	if uowProvider != nil {
		_ = uowProvider.Close(ctx) // Best effort: we are on an error exit already
		uowProvider = nil
	}
	if store != nil {
		storeMutex.Lock()
		storeActive = false
		storeMutex.Unlock()
		_ = store.Close() // Best effort: we are on an error exit already
		store = nil
	}
}

// releaseWorkspaceGates drops the command's gate set. Idempotent and
// nil-safe (MultiHandle.Release is once-guarded per handle), so it is safe
// to call from every exit path — PersistentPostRunE's deferred cleanup and
// PersistentPreRunE's error paths both call it, and double release is a
// no-op.
func releaseWorkspaceGates() {
	if workspaceGateHandle != nil {
		_ = workspaceGateHandle.Release() // Best effort: the flock dies with the process anyway
		workspaceGateHandle = nil
	}
}

// commandNeedsExclusiveGate classifies the store-opening commands that
// REPLACE storage rather than use it. Currently only `bd backup restore`
// flows through the chokepoint and needs exclusivity; `bd init` and the
// `bd migrate from-*-to-*` family are in the skip-store lists and acquire
// their exclusive gates inside their own Run functions instead.
func commandNeedsExclusiveGate(cmd *cobra.Command) bool {
	return cmd.Name() == "restore" && cmd.Parent() != nil && cmd.Parent().Name() == "backup"
}

// buildWorkspaceGateSet resolves the workspace gate plus the physical-root
// gates for whatever the open path will actually open, appending any
// extraRoots (used by migrate to cover the DESTINATION mode's root as well
// as the source's). Roots whose parent directory does not exist are skipped:
// workspacegate needs the gate file's parent, and a root whose parent is
// absent cannot be holding data anyone could clobber.
//
// The resolver's PhysicalRoots provenance is deliberately NOT returned: both
// callers discard it today, and the code comments at each call site already
// explain that provenance is intentionally not surfaced in user-facing
// errors (it names which directory got gated, not who holds it — the gate's
// own busy detail already names the holder).
func buildWorkspaceGateSet(beadsDir string, extraRoots ...string) ([]workspacegate.Gate, error) {
	pr, err := doltserver.ResolvePhysicalRoots(beadsDir)
	if err != nil {
		return nil, err
	}
	wsGate, err := workspacegate.ForWorkspace(pr.BeadsDir)
	if err != nil {
		return nil, err
	}
	gates := []workspacegate.Gate{wsGate}
	roots := append(append([]string{}, pr.Roots...), extraRoots...)
	for _, root := range roots {
		if _, serr := os.Stat(filepath.Dir(root)); serr != nil {
			continue
		}
		g, gerr := workspacegate.ForPhysicalRoot(root)
		if gerr != nil {
			return nil, gerr
		}
		gates = append(gates, g)
	}
	return gates, nil
}

// acquireCommandWorkspaceGates is the chokepoint acquisition for
// store-opening commands, called from PersistentPreRunE right after the
// workspace is selected. It stores the handle in workspaceGateHandle on
// success; on the fail-open paths it returns nil with no handle.
func acquireCommandWorkspaceGates(ctx context.Context, cmd *cobra.Command, beadsDir string) error {
	exclusive := commandNeedsExclusiveGate(cmd)

	// No workspace on disk: nothing to guard, and the store-open path will
	// produce its own (better) "no database found" error. Gating here would
	// scatter .gate.lock files into arbitrary directories users run bd in.
	if _, err := os.Stat(beadsDir); err != nil {
		return nil
	}

	gates, err := buildWorkspaceGateSet(beadsDir)
	if err != nil {
		if exclusive {
			return HandleErrorRespectJSON("workspace gate: %v", err)
		}
		if !quietFlag {
			fmt.Fprintf(os.Stderr, "warning: workspace gate unavailable, continuing ungated: %v\n", err)
		}
		return nil
	}

	mode := workspacegate.Shared
	opts := workspacegate.Options{}
	if exclusive {
		mode = workspacegate.Exclusive
		opts = exclusiveGateOptions("bd backup restore")
	}
	h, err := workspacegate.AcquireAll(ctx, mode, opts, gates...)
	if err != nil {
		if errors.Is(err, workspacegate.ErrBusy) {
			// Contention is never fail-open, in either mode — but the two
			// modes are blocked by OPPOSITE kinds of holders, so the
			// message must not claim "a maintenance operation" for both:
			// a SHARED attempt is blocked only by an exclusive maintenance
			// holder, while an EXCLUSIVE attempt (backup restore) is
			// usually blocked by ordinary shared commands.
			if exclusive {
				return HandleErrorRespectJSON("other bd commands are using this workspace; wait for them to finish and retry: %v", err)
			}
			return HandleErrorRespectJSON("a maintenance operation is running on this workspace; retry when it completes: %v", err)
		}
		if exclusive {
			return HandleErrorRespectJSON("workspace gate: %v", err)
		}
		if !quietFlag {
			fmt.Fprintf(os.Stderr, "warning: workspace gate acquisition failed, continuing ungated: %v\n", err)
		}
		return nil
	}
	workspaceGateHandle = h
	return nil
}

// acquireExclusiveWorkspaceGates is the maintenance-side acquisition used by
// bd bootstrap and bd migrate, which live on the skip-store path and therefore
// never reach the chokepoint. It takes the workspace gate plus the resolved
// physical roots (when .beads exists — a fresh directory has nothing to
// resolve yet) plus any extraRoots, all EXCLUSIVE in ONE AcquireAll (never
// nested — that is a workspacegate invariant). Failures are returned, not
// softened: maintenance refuses rather than pretends.
//
// bd init goes through acquireInitWorkspaceGates instead: it is the one
// maintenance command that USES the shared-server root rather than replacing
// it, and so must not hold that particular gate exclusively.
//
// Lock ordering (normative for the callers): workspace gate(s) →
// physical-root gate(s) → migrate.lock → embedded .lock → proxy locks →
// dolt-server.lock.
//
// Re-entrancy hazard for future callers: an EXCLUSIVE holder that shells
// out to git can re-enter bd through git hooks — the bd hook wrappers spawn
// `bd export`/similar, which flows through the chokepoint, attempts a
// SHARED acquisition against our own exclusive hold, and dies with ErrBusy.
// bd init is safe today because its git plumbing runs with
// `-c core.hooksPath=` / --no-verify; any future maintenance command that
// acquires these gates and then runs git must do the same, or the hook's
// child bd will fail (fail-closed, but confusing).
func acquireExclusiveWorkspaceGates(ctx context.Context, beadsDir, reason string, extraRoots ...string) (*workspacegate.MultiHandle, error) {
	return acquireMaintenanceWorkspaceGates(ctx, beadsDir, reason, false, extraRoots...)
}

// sharedServerRootGate returns the gate for the shared-server database root
// (<BEADS_SHARED_SERVER_DIR|~/.beads/shared-server>/dolt), and whether it
// could be resolved at all. Failure is not an error for the callers: a
// shared root whose parent does not exist cannot be in any gate set either
// (buildWorkspaceGateSet skips exactly those), so an unresolvable answer and
// "not present in this set" lead to the same decision.
func sharedServerRootGate() (workspacegate.Gate, bool) {
	root, err := doltserver.SharedDoltPath()
	if err != nil {
		return workspacegate.Gate{}, false
	}
	g, gerr := workspacegate.ForPhysicalRoot(root)
	if gerr != nil {
		return workspacegate.Gate{}, false
	}
	return g, true
}

// planMaintenanceGateModes assigns each gate the mode a maintenance
// acquisition should take it in: EXCLUSIVE for all of them, except that when
// shareSharedServerRoot is set, the shared-server database root is taken
// SHARED.
//
// Only bd init sets that flag, and the asymmetry is the point. The
// shared-server root is not this workspace's root — it is the one root every
// workspace on the machine resolves to, so an exclusive claim on it is a
// claim over other people's workspaces. bd init does not earn that claim: it
// creates a database ON that server (with --external it never writes the
// directory at all), it does not replace the root the way `bd migrate
// from-*-to-*` and `bd backup restore` do. Those keep EXCLUSIVE.
//
// The demotion loses nothing bd init needs. SHARED still conflicts with
// EXCLUSIVE, so an init still cannot run underneath a migration replacing
// that root — which is the only holder it had to be excluded by. What it
// stops doing is excluding holders it never had a reason to exclude: other
// inits, and every ordinary command in every unrelated workspace, whose
// SHARED acquisition is non-blocking and whose contention path is a hard
// error (see acquireCommandWorkspaceGates). Before this, a single bd init
// made concurrent `bd list` in an unrelated shared-server workspace fail
// with "a maintenance operation is running on this workspace" (bd-436).
//
// gates[0] is the workspace gate — both builders (buildWorkspaceGateSet and
// metadataFreeGateSet) put it there — and it is never demoted, whatever it
// resolves to. That matters for one degenerate configuration: a workspace
// whose .beads directory IS the shared root's directory resolves both gates
// to the same file, and the workspace gate is precisely the hold that keeps
// two inits out of each other's workspace. Exclusivity there is never the
// machine-wide claim this function exists to give up.
func planMaintenanceGateModes(gates []workspacegate.Gate, shareSharedServerRoot bool) []workspacegate.GateMode {
	sharedGate, haveShared := workspacegate.Gate{}, false
	if shareSharedServerRoot {
		sharedGate, haveShared = sharedServerRootGate()
	}
	modes := make([]workspacegate.GateMode, 0, len(gates))
	for i, g := range gates {
		mode := workspacegate.Exclusive
		if i > 0 && haveShared && g.SameAs(sharedGate) {
			mode = workspacegate.Shared
		}
		modes = append(modes, workspacegate.GateMode{Gate: g, Mode: mode})
	}
	return modes
}

// acquireMaintenanceWorkspaceGates is the body of
// acquireExclusiveWorkspaceGates, with the shared-server-root demotion
// described on planMaintenanceGateModes as an explicit parameter so that
// only bd init opts into it.
func acquireMaintenanceWorkspaceGates(ctx context.Context, beadsDir, reason string, shareSharedServerRoot bool, extraRoots ...string) (*workspacegate.MultiHandle, error) {
	// Defense against callers that computed no workspace (bootstrap plans
	// are the untrusted case): gating "" would resolve against the CWD and
	// fence an arbitrary directory.
	if strings.TrimSpace(beadsDir) == "" {
		return nil, errors.New("workspace gate: empty beads directory")
	}
	// Normalize a nil context (tests and helpers call maintenance paths
	// directly, before the process-level signal context exists); the
	// workspacegate package also defends, but do not rely on callees.
	if ctx == nil {
		ctx = context.Background()
	}
	var gates []workspacegate.Gate
	if _, err := os.Stat(beadsDir); err == nil {
		var gerr error
		gates, gerr = buildWorkspaceGateSet(beadsDir, extraRoots...)
		if gerr != nil {
			return nil, gerr
		}
	} else {
		// .beads does not exist yet (bd init creating it): the workspace
		// gate still works because its file lives BESIDE .beads in the
		// project directory, which does exist. Physical-root gates for
		// in-.beads roots are impossible (no parent) and unnecessary —
		// exclusivity on the workspace gate already excludes every gated
		// opener. Out-of-.beads extraRoots are still gated when their
		// parent exists.
		var werr error
		gates, werr = metadataFreeGateSet(beadsDir, extraRoots...)
		if werr != nil {
			return nil, werr
		}
	}
	return workspacegate.AcquireMixed(ctx, exclusiveGateOptions(reason),
		planMaintenanceGateModes(gates, shareSharedServerRoot)...)
}

// metadataFreeGateSet builds the gates that can be derived without reading
// workspace metadata: the workspace gate, whose file lives BESIDE .beads, plus
// each caller-supplied root whose parent directory exists.
func metadataFreeGateSet(beadsDir string, extraRoots ...string) ([]workspacegate.Gate, error) {
	wsGate, err := workspacegate.ForWorkspace(beadsDir)
	if err != nil {
		return nil, err
	}
	gates := []workspacegate.Gate{wsGate}
	for _, root := range extraRoots {
		if _, serr := os.Stat(filepath.Dir(root)); serr != nil {
			continue
		}
		g, gerr := workspacegate.ForPhysicalRoot(root)
		if gerr != nil {
			return nil, gerr
		}
		gates = append(gates, g)
	}
	return gates, nil
}

// acquireInitWorkspaceGates is bd init's acquisition. It differs from
// acquireExclusiveWorkspaceGates in two ways, both specific to init.
//
// First, init is the one caller explicitly authorized to REWRITE this
// workspace's metadata. Gate planning refuses to guess when metadata.json
// cannot be parsed, which is right for every command that goes on to open a
// store — but the roots it would have derived are exactly what the repair is
// about to replace, and refusing there leaves a truncated metadata.json with no
// in-tool recovery at all: the reinitialize path that the fail-closed checks
// name as the escape hatch cannot itself run. Fall back to the gates that do
// not depend on that file (the workspace gate plus the caller's own target
// root), which still excludes every gated opener of this workspace. Contention,
// an unbuildable gate file, and every other resolution failure stay hard errors.
//
// Second, it takes the shared-server database root SHARED rather than
// EXCLUSIVE — see planMaintenanceGateModes for why that is init's scope and not
// migrate's. The fallback path below applies the same demotion, so a repair
// init cannot re-acquire machine-wide exclusivity by the back door.
func acquireInitWorkspaceGates(ctx context.Context, beadsDir, reason string, extraRoots ...string) (*workspacegate.MultiHandle, error) {
	handle, err := acquireMaintenanceWorkspaceGates(ctx, beadsDir, reason, true, extraRoots...)
	if err == nil || !errors.Is(err, doltserver.ErrUnresolvableWorkspaceMetadata) {
		return handle, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if !quietFlag {
		fmt.Fprintf(os.Stderr, "warning: %v; gating this workspace and the target database root only\n", err)
	}
	gates, gerr := metadataFreeGateSet(beadsDir, extraRoots...)
	if gerr != nil {
		return nil, gerr
	}
	return workspacegate.AcquireMixed(ctx, exclusiveGateOptions(reason),
		planMaintenanceGateModes(gates, true)...)
}
