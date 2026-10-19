package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
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
//   - SHARED contention means an exclusive maintenance operation is live
//     (or queued, see workspacegate's writer fairness) on this workspace:
//     wait for it up to sharedGateWait() (BEADS_GATE_WAIT_TIMEOUT, default
//     30s; git hooks stay fail-fast), honoring Ctrl-C. ErrBusy past the
//     bound aborts with an actionable error naming the holder (the gate's
//     busy detail carries pid/reason/host from the advisory sidecar) and
//     the knob. Proceeding would race a migration/restore mid-replace, and
//     so would treating an interrupted wait as fail-open.
//   - EXCLUSIVE failures of any kind are hard errors: maintenance refuses
//     rather than pretends.
//   - Strict --readonly is mutation-free (effectiveRootStorePolicy), so its
//     SHARED hold opens the gate files read-only and never creates them (see
//     workspacegate.Options.ReadOnly). A gate file that does not exist means
//     nothing has ever been gated there, so the command proceeds holding
//     nothing; an existing one is held SHARED like any other command's.
//     Residual: on a workspace that has never been gated, a strict readonly
//     command is invisible to a maintenance operation that starts while it
//     runs. Every other command keeps creating the files, which is what makes
//     it visible. The gate follows the strict flag only: allowlisted reads
//     and --dry-run/--inspect previews also open the store read-only, but
//     they keep creating the files, so they stay visible too.
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

// inheritedHoldEnvSaved remembers workspacegate.InheritedHoldEnv as it was
// before this process advertised its own shared hold, so release restores
// it (an in-process test harness, or a child of a still-holding bd parent,
// must not lose or keep a stale marker). Valid when inheritedHoldEnvSet.
var (
	inheritedHoldEnvSaved string
	inheritedHoldEnvHad   bool
	inheritedHoldEnvSet   bool
)

// advertiseSharedHold exports this process's PID in
// workspacegate.InheritedHoldEnv while it holds its command gates SHARED, so
// bd processes it spawns (`bd orphans` -> `bd close`, git hooks fired by a
// git command bd runs) skip the writer-fairness queue: a queued exclusive
// acquirer cannot get in until this process releases, and this process may
// be waiting on the child, so queueing the child would deadlock until its
// wait bound expired.
func advertiseSharedHold() {
	if !advertiseSharedHoldEnabled {
		return
	}
	own := strconv.Itoa(os.Getpid())
	cur, had := os.LookupEnv(workspacegate.InheritedHoldEnv)
	// Save the pre-advertise value unless we are already advertising (a
	// re-acquire without release). Keying on the live value rather than on
	// inheritedHoldEnvSet alone keeps a missed withdraw from pinning a stale
	// saved value forever.
	if !inheritedHoldEnvSet || !had || cur != own {
		inheritedHoldEnvSaved, inheritedHoldEnvHad = cur, had
	}
	inheritedHoldEnvSet = true
	_ = os.Setenv(workspacegate.InheritedHoldEnv, own)
}

// envWithoutSharedHoldMarker is os.Environ() minus
// workspacegate.InheritedHoldEnv, for user-facing children that are not bd
// (the $EDITOR `bd edit` opens can outlive this process by hours, e.g. a new
// IDE window). A bd run from such a descendant then queues normally instead
// of trusting a PID that may since have been recycled. bd's own children
// keep the marker.
func envWithoutSharedHoldMarker() []string {
	return filterEnv(os.Environ(), workspacegate.InheritedHoldEnv)
}

// advertiseSharedHoldEnabled is off inside test binaries: there in-process
// command tests run beside tests that spawn bd subprocesses, and a marker
// naming the (live) test process would let those children skip the queue
// and weaken their fairness assertions. Tests of the marker itself turn it
// on.
var advertiseSharedHoldEnabled = !testing.Testing()

// withdrawSharedHold undoes advertiseSharedHold (no-op if never advertised).
func withdrawSharedHold() {
	if !inheritedHoldEnvSet {
		return
	}
	if inheritedHoldEnvHad {
		_ = os.Setenv(workspacegate.InheritedHoldEnv, inheritedHoldEnvSaved)
	} else {
		_ = os.Unsetenv(workspacegate.InheritedHoldEnv)
	}
	inheritedHoldEnvSet = false
}

// exclusiveGateWait is how long EXCLUSIVE acquisitions poll before giving
// up: long enough to ride out a short-lived `bd list` finishing, short
// enough that a genuinely busy workspace fails with a clear message rather
// than hanging. SHARED acquisitions use sharedGateWait instead. A var, not a
// const, so tests can shorten it.
var exclusiveGateWait = 5 * time.Second

// exclusiveGateOnWait reports the first contended exclusive-gate attempt.
// Kept as a variable so ordering tests can observe contention without sleeps.
var exclusiveGateOnWait = func(holder string) {
	if !quietFlag {
		// %q: the holder string comes from another process's gate sidecar;
		// quoting neutralizes terminal escape sequences a hostile or corrupt
		// sidecar could smuggle into stderr.
		fmt.Fprintf(os.Stderr, "waiting for other bd commands to finish (%q)...\n", holder) //nolint:gosec // G705: stderr, not a browser context; %q additionally neutralizes terminal escapes
	}
}

// exclusiveGateOptions builds the acquisition options for an EXCLUSIVE
// hold: bounded wait, holder-info reason, and a single stderr note when the
// first attempt comes back busy so the wait does not look like a hang.
func exclusiveGateOptions(reason string) workspacegate.Options {
	return workspacegate.Options{
		Wait:   exclusiveGateWait,
		Reason: reason,
		OnWait: exclusiveGateOnWait,
	}
}

// initGateTimeoutEnv overrides initGateWaitDefault, following the
// BEADS_*_TIMEOUT knob convention (BEADS_PRIME_TIMEOUT, BEADS_FSCK_TIMEOUT):
// a Go duration ("90s", "2m") or bare whole seconds ("90").
const initGateTimeoutEnv = "BEADS_INIT_GATE_TIMEOUT"

// initGateWaitDefault is how long bd init waits for its EXCLUSIVE gate set.
// It is deliberately longer than exclusiveGateWait and scoped to init only:
// in shared-server mode every project's physical root is the one shared
// dolt data dir, so init in project A contends with init (or any gated
// command) in project B. A single init holds the gate for ~8s, so the
// generic 5s budget made two concurrent `bd init --shared-server` runs in
// different projects refuse each other. Under load a single init holds the
// gate for 10-15s and each queued init waits for every one ahead of it, so
// 60s rides out a few back-to-back inits while still failing with a clear
// error on a genuinely stuck holder. (Waiting this long is only acceptable
// because a waiting init no longer holds ordinary commands back for its
// whole budget: workspacegate caps how long it queues them, maxIntentHold.)
// The other exclusive callers (backup restore, bootstrap, migrate) keep
// exclusiveGateWait: they are rare, deliberate maintenance operations rather
// than routine setup that tooling fans out across many projects at once.
const initGateWaitDefault = 60 * time.Second

// gateWaitNoticeDelay is how long a gate wait (bd init, or an ordinary
// command waiting out a maintenance operation) stays silent before telling
// the user it is blocked: a sub-2s wait is not worth a line of output. A var
// so tests can observe contention without sleeping.
var gateWaitNoticeDelay = 2 * time.Second

// gateWaitOnWait prints the single "still waiting" notice. A var so tests
// can observe it.
var gateWaitOnWait = func(holder string) {
	if quietFlag {
		return
	}
	where := "this workspace"
	if doltserver.IsSharedServerMode() {
		where = "the shared server"
	}
	// %q: holder text comes from another process's gate sidecar (see
	// exclusiveGateOnWait).
	fmt.Fprintf(os.Stderr, "waiting for another bd process on %s (%q)...\n", where, holder) //nolint:gosec // G705: stderr, not a browser context; %q additionally neutralizes terminal escapes
}

// delayedGateNotice returns an Options.OnWait that arms gateWaitOnWait to
// fire once after gateWaitNoticeDelay, plus a stop func the caller must run
// once acquisition returns. AcquireAll calls OnWait at most once,
// synchronously, before it returns, so the timer is set (if at all) by the
// time stop runs.
func delayedGateNotice() (onWait func(string), stop func()) {
	var notice *time.Timer
	onWait = func(holder string) {
		notice = time.AfterFunc(gateWaitNoticeDelay, func() { gateWaitOnWait(holder) })
	}
	stop = func() {
		if notice != nil {
			notice.Stop()
		}
	}
	return onWait, stop
}

// parseGateTimeoutEnv reads a BEADS_*_TIMEOUT-style knob: a Go duration
// ("90s", "2m") or bare whole seconds ("90"). ok is false when unset or
// malformed (malformed values warn once).
func parseGateTimeoutEnv(name string, fallback time.Duration) (d time.Duration, ok bool) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return 0, false
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		d, err = time.ParseDuration(raw + "s")
	}
	if err != nil {
		if !quietFlag {
			fmt.Fprintf(os.Stderr, "warning: %s=%q is not a duration; using default %s\n", name, raw, fallback)
		}
		return 0, false
	}
	return d, true
}

// sharedGateWaitEnv overrides sharedGateWaitDefault for ordinary (SHARED)
// commands, same syntax as BEADS_INIT_GATE_TIMEOUT; 0 restores the old
// fail-fast behavior.
const sharedGateWaitEnv = "BEADS_GATE_WAIT_TIMEOUT"

// sharedGateWaitDefault is how long an ordinary command waits for an
// exclusive maintenance holder (bd init ~8s, backup restore, migrate,
// bootstrap) — or for a queued one, see workspacegate's writer fairness —
// before failing. In shared-server mode every project shares one physical
// root gate, so another project's `bd init` briefly excludes every bd
// command on the machine; failing those instantly made routine tooling
// flaky. Queued maintenance operations run one after another (two inits
// fanned out across projects hold the gate back to back, 10-15s each under
// load), and a waiting command waits for all of them, so the default matches
// init's own patience rather than a single init: 30s. Humans see the notice
// after 2s and can Ctrl-C; agents and CI, the callers that suffered most
// from fail-fast, just get their command run. A genuinely long restore or
// migration still fails with a clear error naming the holder and the knob.
const sharedGateWaitDefault = 30 * time.Second

// sharedGateWait resolves an ordinary command's gate budget.
//
// Git hooks stay fail-fast (0): the hook paths (`bd hooks run`, and the
// `bd export` / `bd import` children the pre-commit and post-merge/checkout
// hooks spawn) all run with BD_GIT_HOOK=1, treat a failure as a warning,
// and must never stall the user's `git commit` / `git checkout` for 30s
// behind a maintenance operation. A fail-fast acquisition also skips the
// writer-fairness queue, so the hook behavior is exactly what it was.
func sharedGateWait() time.Duration {
	if os.Getenv("BD_GIT_HOOK") == "1" {
		return 0
	}
	d, ok := parseGateTimeoutEnv(sharedGateWaitEnv, sharedGateWaitDefault)
	if !ok {
		return sharedGateWaitDefault
	}
	if d < 0 {
		if !quietFlag {
			fmt.Fprintf(os.Stderr, "warning: %s=%s is negative; using default %s\n", sharedGateWaitEnv, d, sharedGateWaitDefault)
		}
		return sharedGateWaitDefault
	}
	return d
}

// initGateWait resolves init's gate budget from initGateTimeoutEnv, falling
// back to initGateWaitDefault (with a warning) on unset, malformed, or
// non-positive values.
func initGateWait() time.Duration {
	d, ok := parseGateTimeoutEnv(initGateTimeoutEnv, initGateWaitDefault)
	if !ok {
		return initGateWaitDefault
	}
	if d <= 0 {
		if !quietFlag {
			fmt.Fprintf(os.Stderr, "warning: %s=%s is not a positive duration; using default %s\n", initGateTimeoutEnv, d, initGateWaitDefault)
		}
		return initGateWaitDefault
	}
	return d
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
	withdrawSharedHold()
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
	// A process can execute more than one command in tests and embedded uses.
	// Never carry the preceding command's handle into an absent-workspace or
	// failed acquisition path, where no new gate is held. Gates outlive the
	// store, so close whatever store the preceding command left open before
	// its gates drop; this command opens its own only after this point.
	closeStoreBeforeGateRelease()
	releaseWorkspaceGates()
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
	var opts workspacegate.Options
	var sharedWait time.Duration
	stopNotice := func() {}
	if exclusive {
		mode = workspacegate.Exclusive
		opts = exclusiveGateOptions("bd backup restore")
	} else {
		// Ordinary commands wait (bounded, Ctrl-C aware) for a live or
		// queued exclusive maintenance holder instead of failing at once.
		sharedWait = sharedGateWait()
		opts = workspacegate.Options{
			Wait:                  sharedWait,
			IgnoreQueuedExclusive: workspacegate.InheritedSharedHold(),
			// readonlyMode is already resolved here. Strict readonly promises not to
			// mutate the workspace, so its SHARED hold must not create gate files.
			ReadOnly: readonlyMode,
		}
		if sharedWait > 0 {
			opts.OnWait, stopNotice = delayedGateNotice()
		}
	}
	h, err := workspacegate.AcquireAll(ctx, mode, opts, gates...)
	stopNotice()
	if err != nil {
		// Interrupted while waiting (Ctrl-C / SIGTERM cancels rootCtx):
		// abort. This must precede the SHARED fail-open branch below, or a
		// canceled wait would "continue ungated" over the maintenance
		// operation it was waiting out.
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return HandleErrorRespectJSON("interrupted while waiting for the workspace gate: %v", err)
		}
		if errors.Is(err, workspacegate.ErrBusy) {
			// Contention is never fail-open, in either mode — but the two
			// modes are blocked by OPPOSITE kinds of holders, so the
			// message must not claim "a maintenance operation" for both:
			// a SHARED attempt is blocked only by an exclusive maintenance
			// holder (or one queued for exclusive access), while an
			// EXCLUSIVE attempt (backup restore) is usually blocked by
			// ordinary shared commands.
			if exclusive {
				return HandleErrorRespectJSON("other bd commands are using this workspace; wait for them to finish and retry: %v", err)
			}
			// A shared acquisition fails only when the gate itself is held
			// exclusively (queued maintenance can delay it, never fail it —
			// workspacegate's final attempt ignores the queue), so "running"
			// is accurate. In shared-server mode the holder may be another
			// project's operation; the error detail names it when known.
			where := "this workspace"
			if doltserver.IsSharedServerMode() {
				where = "the shared server (possibly from another project)"
			}
			if sharedWait > 0 {
				return HandleErrorRespectJSON("a maintenance operation is running on %s; retry when it completes (waited %s; set %s to wait longer): %v", where, sharedWait, sharedGateWaitEnv, err)
			}
			return HandleErrorRespectJSON("a maintenance operation is running on %s; retry when it completes: %v", where, err)
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
	// A strict readonly hold on a never-gated workspace locks nothing, so it
	// blocks no exclusive acquirer and gives a child nothing to stand behind:
	// advertising it would let the child skip the queue for no reason.
	if !exclusive && !h.Empty() {
		advertiseSharedHold()
	}
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
	return acquireExclusiveWorkspaceGatesWithOptions(ctx, beadsDir, exclusiveGateOptions(reason), extraRoots...)
}

// acquireExclusiveWorkspaceGatesWithOptions is acquireExclusiveWorkspaceGates
// with caller-supplied acquisition options.
func acquireExclusiveWorkspaceGatesWithOptions(ctx context.Context, beadsDir string, opts workspacegate.Options, extraRoots ...string) (*workspacegate.MultiHandle, error) {
	return acquireMaintenanceWorkspaceGates(ctx, beadsDir, opts, false, extraRoots...)
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
// acquireExclusiveWorkspaceGatesWithOptions, with the shared-server-root demotion
// described on planMaintenanceGateModes as an explicit parameter so that
// only bd init opts into it.
func acquireMaintenanceWorkspaceGates(ctx context.Context, beadsDir string, opts workspacegate.Options, shareSharedServerRoot bool, extraRoots ...string) (*workspacegate.MultiHandle, error) {
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
	return workspacegate.AcquireMixed(ctx, opts,
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
	return acquireInitWorkspaceGatesWithOptions(ctx, beadsDir, exclusiveGateOptions(reason), extraRoots...)
}

// acquireInitWorkspaceGatesWithOptions is acquireInitWorkspaceGates with
// caller-supplied acquisition options (bd init uses a longer wait).
func acquireInitWorkspaceGatesWithOptions(ctx context.Context, beadsDir string, opts workspacegate.Options, extraRoots ...string) (*workspacegate.MultiHandle, error) {
	handle, err := acquireMaintenanceWorkspaceGates(ctx, beadsDir, opts, true, extraRoots...)
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
	return workspacegate.AcquireMixed(ctx, opts,
		planMaintenanceGateModes(gates, true)...)
}

// acquireInitMutationGate holds init's complete exclusive gate set while its
// destructive preflight runs. A refusal or preflight error releases the gates;
// a successful caller owns the returned handle through replacement.
//
// Contention waits up to initGateWait() (not the generic exclusiveGateWait),
// honoring ctx cancellation, and prints one notice if the wait outlasts
// gateWaitNoticeDelay. Exclusivity is unchanged; past the bound init fails
// with the same refusal, naming the budget and the knob that raises it.
//
// Gate acquisition goes through acquireInitWorkspaceGates, not the plain
// acquireExclusiveWorkspaceGates: init still needs the shared-server root
// taken SHARED rather than EXCLUSIVE (bd-436) and the unresolvable-metadata
// fallback for the repair variant, both of which are init-specific and would
// otherwise be lost by routing through the generic maintenance acquisition.
func acquireInitMutationGate(ctx context.Context, beadsDir, physicalRoot string, preflight func() error) (*workspacegate.MultiHandle, error) {
	wait := initGateWait()
	onWait, stopNotice := delayedGateNotice()
	opts := workspacegate.Options{
		Wait:   wait,
		Reason: "bd init",
		OnWait: onWait,
	}
	h, err := acquireInitWorkspaceGatesWithOptions(ctx, beadsDir, opts, physicalRoot)
	stopNotice()
	if err != nil {
		if errors.Is(err, workspacegate.ErrBusy) {
			return nil, fmt.Errorf("bd init refuses to run over live bd activity on this workspace (waited %s; set %s to wait longer): %w", wait, initGateTimeoutEnv, err)
		}
		return nil, fmt.Errorf("bd init refuses to run over live bd activity on this workspace: %w", err)
	}
	if preflight != nil {
		if err := preflight(); err != nil {
			return nil, errors.Join(err, h.Release())
		}
	}
	return h, nil
}
