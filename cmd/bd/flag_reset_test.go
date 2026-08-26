// Cobra/pflag flag-state reset for in-process test runners.
//
// pflag never clears Flag.Changed or the parsed value between calls to
// rootCmd.Execute(). In the real CLI that is invisible — one command per
// process. In the ./cmd/bd test binary every in-process runner shares one
// rootCmd, so:
//
//	test A runs `list --format json` -> root persistent "format".Changed = true, value "json"
//	test B runs `status`             -> Changed("format") is STILL true, value STILL "json"
//
// Any code that asks "did this command set this flag?" therefore answers for a
// previous command. The root pre-run asks exactly that for --format, --json,
// --readonly, --actor, --db, --sandbox and --dolt-auto-commit (main.go), and
// resolveJSONOutput reads Changed("format")/Changed("json") on every command.
//
// Resetting the package globals is not the same thing and does not cover it.
// It happens to cover --json only because the root --json flag is bound to
// &jsonOutput, so clearing the global clears the flag's value too — an
// accident that stops holding the moment a flag is given its own backing var.
// --format has no backing global at all and was never covered.
//
// This file has NO build tag on purpose: the runners that need it live behind
// `cgo`, `cgo && integration` and no tag at all.
//
// See bd-hcl.
package main

import (
	"encoding/csv"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// pflagPkgPath is the import path of the package that owns the built-in
// pflag.Value implementations. Any flag value from outside it is a bd-local
// type whose Set/String semantics this file cannot reason about, so
// TestCommandTreeFlagValuesAreResettable requires such a type to implement
// flagValueResetter.
const pflagPkgPath = "github.com/spf13/pflag"

// flagValueResetter is implemented by pflag.Value types that cannot be
// returned to their registered default by Set(DefValue) alone.
//
// Two shapes need it, and bd has both:
//
//   - values whose Set accumulates (closeReasonFlagValue appends every
//     occurrence of --reason, so Set("") adds an empty reason rather than
//     clearing the list)
//   - values whose Set is once-only (singleWorktreeBoolFlag and
//     singleWorktreeStringFlag reject a second --force / --merged-into with
//     "may be specified only once", so the second in-process run of the same
//     command fails at parse time)
//
// Implementations must return the value to its REGISTERED DEFAULT, not merely
// to the zero value — those coincide for every current implementation, and a
// custom value with a non-zero default must say so itself.
type flagValueResetter interface {
	ResetForTesting()
}

// visitCommandTreeFlags calls fn once for every distinct *pflag.Flag reachable
// from root's subtree, with the command whose flag set it was found on.
//
// Deduplication is by flag pointer, which is what makes one pass over
// PersistentFlags() and Flags() sufficient: cobra's mergePersistentFlags
// copies the SAME *pflag.Flag from a parent's persistent set into an executed
// child's Flags(), so an inherited flag is visited once, at its owner.
//
// Callers must hold whatever mutex serializes cobra state for the package
// (stdioMutex, inProcessMutex, cliCoverageMutex): Flags() lazily allocates.
func visitCommandTreeFlags(root *cobra.Command, fn func(owner *cobra.Command, f *pflag.Flag)) {
	if root == nil {
		return
	}
	seen := map[*pflag.Flag]bool{}
	var walk func(*cobra.Command)
	walk = func(c *cobra.Command) {
		for _, set := range []*pflag.FlagSet{c.PersistentFlags(), c.Flags()} {
			set.VisitAll(func(f *pflag.Flag) {
				if seen[f] {
					return
				}
				seen[f] = true
				fn(c, f)
			})
		}
		for _, child := range c.Commands() {
			walk(child)
		}
	}
	walk(root)
}

// sliceDefaultValues parses the DefValue of a pflag.SliceValue back into the
// elements it was rendered from. pflag writes those defaults as a CSV record
// wrapped in brackets ("[]", "[a,b]", "[k=v]"), so this is the inverse of
// Value.String() for every slice and map value pflag ships.
func sliceDefaultValues(defValue string) []string {
	s := strings.TrimSpace(defValue)
	s = strings.TrimPrefix(s, "[")
	s = strings.TrimSuffix(s, "]")
	if s == "" {
		return nil
	}
	record, err := csv.NewReader(strings.NewReader(s)).Read()
	if err != nil {
		// Not a CSV record — fall back to the whole string as one element
		// rather than dropping a non-empty default on the floor.
		return []string{s}
	}
	return record
}

// setFlagValue writes vals into f's value, choosing the assignment that
// actually replaces rather than appends.
//
// Slice and map values must go through SliceValue.Replace. Their Set() appends
// once the value has been parsed for the first time in the process
// (stringSliceValue keeps its own `changed` bit, unreachable from here and
// never cleared), so Set() on a slice would grow it every reset. Replace has
// no such branch.
func setFlagValue(f *pflag.Flag, scalar string, vals []string, isSlice bool) error {
	if isSlice {
		sv, ok := f.Value.(pflag.SliceValue)
		if !ok {
			return fmt.Errorf("--%s: value %T is no longer a pflag.SliceValue", f.Name, f.Value)
		}
		return sv.Replace(vals)
	}
	return f.Value.Set(scalar)
}

// resetFlagToDefault restores f to its registered default and clears Changed.
func resetFlagToDefault(f *pflag.Flag) error {
	// Checked before the SliceValue branch: a bd-local value may be both, and
	// only it knows how to undo its own accumulation.
	if r, ok := f.Value.(flagValueResetter); ok {
		r.ResetForTesting()
		f.Changed = false
		return nil
	}
	if sv, ok := f.Value.(pflag.SliceValue); ok {
		if err := sv.Replace(sliceDefaultValues(f.DefValue)); err != nil {
			return fmt.Errorf("--%s: replace with default %q: %w", f.Name, f.DefValue, err)
		}
		f.Changed = false
		return nil
	}
	if err := f.Value.Set(f.DefValue); err != nil {
		return fmt.Errorf("--%s: set to default %q: %w", f.Name, f.DefValue, err)
	}
	f.Changed = false
	return nil
}

// resetCommandTreeFlagState returns every flag in root's subtree to its
// registered default and clears Changed on all of them.
//
// Call it from every in-process runner AFTER rootCmd.Execute(), next to the
// package-global resets those runners already do. Failing to reset is reported
// through t rather than swallowed: a flag this cannot restore is a flag that
// will leak into the next test, and a silent partial reset reads exactly like
// a complete one.
func resetCommandTreeFlagState(t *testing.T, root *cobra.Command) {
	t.Helper()
	visitCommandTreeFlags(root, func(owner *cobra.Command, f *pflag.Flag) {
		if err := resetFlagToDefault(f); err != nil {
			t.Errorf("reset flag state for %q: %v", owner.CommandPath(), err)
		}
	})
}

// flagSnapshot records one flag's value and Changed bit.
type flagSnapshot struct {
	flag    *pflag.Flag
	path    string
	scalar  string
	slice   []string
	isSlice bool
	changed bool
}

// commandTreeFlagState is a snapshot of a whole command tree's flag state.
type commandTreeFlagState []flagSnapshot

// snapshotCommandTreeFlagState captures the value and Changed bit of every
// flag in root's subtree.
//
// This is the generalization of the six-name snapshotRootFlagState this
// package used to carry: naming the flags to protect meant every flag nobody
// had been bitten by yet was unprotected, which is the defect itself.
func snapshotCommandTreeFlagState(root *cobra.Command) commandTreeFlagState {
	var state commandTreeFlagState
	visitCommandTreeFlags(root, func(owner *cobra.Command, f *pflag.Flag) {
		snap := flagSnapshot{flag: f, path: owner.CommandPath(), scalar: f.Value.String(), changed: f.Changed}
		if sv, ok := f.Value.(pflag.SliceValue); ok {
			snap.isSlice = true
			snap.slice = append([]string(nil), sv.GetSlice()...)
		}
		state = append(state, snap)
	})
	return state
}

// restoreCommandTreeFlagState puts back what snapshotCommandTreeFlagState
// captured.
//
// A pflag value already holding the snapshotted state is left untouched rather
// than re-Set, so restoring cannot itself accumulate on a value whose Set
// appends.
//
// One honest limitation: a bd-local accumulating value is restored from its
// String(), and closeReasonFlagValue's String() reports only the LAST reason.
// A snapshot taken while --reason held several therefore comes back holding
// one. Nothing in the suite leaves it in that state — the runners reset it —
// and the alternative is a snapshot method on flagValueResetter for a single
// flag, so this records the gap rather than closing it.
func restoreCommandTreeFlagState(t *testing.T, state commandTreeFlagState) {
	t.Helper()
	for _, snap := range state {
		f := snap.flag
		// Custom values are cleared first, and unconditionally: their Set is
		// the operation that cannot be undone by another Set, and their
		// internal "already set" bookkeeping is invisible to both String() and
		// Changed — so the fast path below cannot be trusted to see it.
		if r, ok := f.Value.(flagValueResetter); ok {
			r.ResetForTesting()
			if snap.scalar != f.DefValue {
				if err := f.Value.Set(snap.scalar); err != nil {
					t.Errorf("restore flag state for %q: --%s: %v", snap.path, f.Name, err)
					continue
				}
			}
			f.Changed = snap.changed
			continue
		}
		if f.Changed == snap.changed && f.Value.String() == snap.scalar {
			continue
		}
		if err := setFlagValue(f, snap.scalar, snap.slice, snap.isSlice); err != nil {
			t.Errorf("restore flag state for %q: %v", snap.path, err)
			continue
		}
		f.Changed = snap.changed
	}
}

// TestCommandTreeFlagValuesAreResettable fails when a bd-local pflag.Value
// reaches the command tree without a way to be returned to its default.
//
// The reset above can only reason about pflag's own value types. A bd-local
// type is opaque to it: closeReasonFlagValue appends on every Set and
// singleWorktree{Bool,String}Flag reject a second Set outright, so for both of
// them Set(DefValue) is not a reset — for the worktree pair it is an error,
// and for --reason it silently adds an empty reason to the list. Neither shows
// up as a value mismatch afterwards, which is why this guard checks the TYPE
// rather than round-tripping the value.
func TestCommandTreeFlagValuesAreResettable(t *testing.T) {
	stdioMutex.Lock()
	defer stdioMutex.Unlock()

	var offenders []string
	visitCommandTreeFlags(rootCmd, func(owner *cobra.Command, f *pflag.Flag) {
		typ := reflect.TypeOf(f.Value)
		for typ != nil && typ.Kind() == reflect.Ptr {
			typ = typ.Elem()
		}
		if typ == nil || typ.PkgPath() == pflagPkgPath {
			return
		}
		if _, ok := f.Value.(flagValueResetter); ok {
			return
		}
		offenders = append(offenders, fmt.Sprintf("%s --%s (%s)", owner.CommandPath(), f.Name, typ.String()))
	})

	if len(offenders) > 0 {
		sort.Strings(offenders)
		t.Fatalf("these flags use a bd-local pflag.Value that resetCommandTreeFlagState cannot return to its "+
			"default; give each value type a ResetForTesting() method (see flagValueResetter):\n  %s",
			strings.Join(offenders, "\n  "))
	}
}

// TestResetCommandTreeFlagStateCoversTheRealRootCmd runs the reset over bd's
// actual command tree and checks the result, flag by flag.
//
// The type guard above and the synthetic-tree tests below each cover one
// property on a corpus chosen to hold it. Neither answers the question the
// in-process runners actually depend on: does this reset work on THIS tree, all
// of it, today. A flag whose Set rejects its own DefValue would fail here and
// nowhere else — and inside a runner it would surface as a confusing t.Errorf
// attributed to whichever test happened to call the runner.
func TestResetCommandTreeFlagStateCoversTheRealRootCmd(t *testing.T) {
	stdioMutex.Lock()
	defer stdioMutex.Unlock()

	state := snapshotCommandTreeFlagState(rootCmd)
	t.Cleanup(func() { restoreCommandTreeFlagState(t, state) })

	// resetCommandTreeFlagState reports failures through t, so a reset this
	// cannot perform fails the test here.
	resetCommandTreeFlagState(t, rootCmd)

	var (
		scanned int
		bad     []string
	)
	visitCommandTreeFlags(rootCmd, func(owner *cobra.Command, f *pflag.Flag) {
		scanned++
		if f.Changed {
			bad = append(bad, fmt.Sprintf("%s --%s: Changed still true", owner.CommandPath(), f.Name))
		}
		if got := f.Value.String(); got != f.DefValue {
			bad = append(bad, fmt.Sprintf("%s --%s: value %q, want default %q", owner.CommandPath(), f.Name, got, f.DefValue))
		}
	})

	if len(bad) > 0 {
		sort.Strings(bad)
		t.Fatalf("reset left %d of %d flags dirty:\n  %s", len(bad), scanned, strings.Join(bad, "\n  "))
	}
	// A zero here is the result, so say what was measured: an empty tree walk
	// and a clean one are otherwise the same output.
	t.Logf("reset %d flags across bd's command tree, all at default", scanned)
	if scanned < 100 {
		t.Fatalf("only %d flags visited — the tree walk is not reaching bd's command tree", scanned)
	}
}

// TestResetCommandTreeFlagStateClearsAStaleFormatFlag is the end-to-end
// characterization of bd-hcl on a tree shaped like bd's, and of the fix.
//
// The first assertion is the known positive: it proves the leak is reachable
// here at all. Without it, the third assertion passing would be
// indistinguishable from a test that never put the tree into the leaking state.
func TestResetCommandTreeFlagStateClearsAStaleFormatFlag(t *testing.T) {
	root, _, _ := newOutputModeTestTree()

	// Command one asks for JSON via the --format alias.
	first := findAndParse(t, root, "plain", "--format", "json")
	if !resolveJSONOutput(first, false) {
		t.Fatal("--format json should select JSON output")
	}

	// Command two, same process, asks for nothing. pflag has kept both
	// Changed and the parsed value, so it inherits the previous command's
	// answer. This is the defect, asserted so the reset below means something.
	leaked := findAndParse(t, root, "plain")
	if !resolveJSONOutput(leaked, false) {
		t.Fatal("expected the documented pflag leak (a second command inheriting --format json); " +
			"if pflag now clears Changed between parses, this whole file is obsolete")
	}

	resetCommandTreeFlagState(t, root)

	after := findAndParse(t, root, "plain")
	if resolveJSONOutput(after, false) {
		t.Fatal("after resetCommandTreeFlagState, a command that asked for nothing still resolved to JSON")
	}
	if f := root.PersistentFlags().Lookup("format"); f.Changed || f.Value.String() != f.DefValue {
		t.Fatalf("--format not reset: Changed=%v value=%q want Changed=false value=%q",
			f.Changed, f.Value.String(), f.DefValue)
	}
}

// TestResetCommandTreeFlagStateReplacesSliceValues covers the branch that
// Set(DefValue) gets wrong. pflag's slice values append rather than replace
// once they have been parsed, so a reset written as Set(DefValue) leaves the
// previous command's elements in place and the next command's --label lands on
// top of them.
func TestResetCommandTreeFlagStateReplacesSliceValues(t *testing.T) {
	var labels []string
	root := &cobra.Command{Use: "bd", Run: func(*cobra.Command, []string) {}}
	root.PersistentFlags().StringSliceVar(&labels, "label", nil, "Filter by label")
	child := &cobra.Command{Use: "list", Run: func(*cobra.Command, []string) {}}
	root.AddCommand(child)

	findAndParse(t, root, "list", "--label", "alpha")
	if got := []string{"alpha"}; !equalStrings(labels, got) {
		t.Fatalf("first parse: labels = %v, want %v", labels, got)
	}

	resetCommandTreeFlagState(t, root)
	if len(labels) != 0 {
		t.Fatalf("after reset: labels = %v, want empty", labels)
	}

	findAndParse(t, root, "list", "--label", "beta")
	if got := []string{"beta"}; !equalStrings(labels, got) {
		t.Fatalf("second parse: labels = %v, want %v — the first command's value survived the reset", labels, got)
	}
}

// TestResetCommandTreeFlagStateResetsWorktreeRemoveFlags covers the once-only
// custom values on the real command tree. Their Set rejects a second call, so
// before this reset existed the second in-process `bd worktree remove --force`
// in a process failed at parse time with "may be specified only once".
func TestResetCommandTreeFlagStateResetsWorktreeRemoveFlags(t *testing.T) {
	stdioMutex.Lock()
	defer stdioMutex.Unlock()

	state := snapshotCommandTreeFlagState(worktreeRemoveCmd)
	t.Cleanup(func() { restoreCommandTreeFlagState(t, state) })

	force := worktreeRemoveCmd.Flags().Lookup("force")
	if force == nil {
		t.Fatal("worktree remove has no --force flag")
	}
	resetCommandTreeFlagState(t, worktreeRemoveCmd)

	if err := force.Value.Set("true"); err != nil {
		t.Fatalf("first --force: %v", err)
	}
	force.Changed = true

	resetCommandTreeFlagState(t, worktreeRemoveCmd)

	if force.Changed {
		t.Error("--force still Changed after reset")
	}
	if got := force.Value.String(); got != "false" {
		t.Errorf("--force value = %q, want %q", got, "false")
	}
	if err := force.Value.Set("true"); err != nil {
		t.Fatalf("second --force after reset: %v — the once-only guard was not cleared", err)
	}
}

// TestSnapshotCommandTreeFlagStateRoundTrips checks the snapshot/restore pair
// the context-binding tests rely on: a flag mutated between snapshot and
// restore comes back exactly as it was, including Changed.
func TestSnapshotCommandTreeFlagStateRoundTrips(t *testing.T) {
	var labels []string
	root := &cobra.Command{Use: "bd", Run: func(*cobra.Command, []string) {}}
	root.PersistentFlags().String("actor", "", "Actor name")
	root.PersistentFlags().StringSliceVar(&labels, "label", nil, "Filter by label")

	findAndParse(t, root, "--actor", "before", "--label", "kept")
	state := snapshotCommandTreeFlagState(root)

	actorFlag := root.PersistentFlags().Lookup("actor")
	if err := actorFlag.Value.Set("after"); err != nil {
		t.Fatalf("set --actor: %v", err)
	}
	resetCommandTreeFlagState(t, root)

	restoreCommandTreeFlagState(t, state)

	if got := actorFlag.Value.String(); got != "before" {
		t.Errorf("--actor = %q, want %q", got, "before")
	}
	if !actorFlag.Changed {
		t.Error("--actor Changed = false, want true")
	}
	if want := []string{"kept"}; !equalStrings(labels, want) {
		t.Errorf("--label = %v, want %v", labels, want)
	}
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
