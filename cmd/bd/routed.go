package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/steveyegge/beads/internal/beads"
	"github.com/steveyegge/beads/internal/configfile"
	"github.com/steveyegge/beads/internal/debug"
	"github.com/steveyegge/beads/internal/remedy"
	"github.com/steveyegge/beads/internal/routing"
	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/storage/embeddeddolt"
	"github.com/steveyegge/beads/internal/types"
	"github.com/steveyegge/beads/internal/utils"
)

var errRoutingUnavailable = errors.New("routing unavailable")

// isNotFoundErr returns true if the error indicates the issue was not found.
// This covers both storage.ErrNotFound (from GetIssue) and the plain error
// from ResolvePartialID which doesn't wrap the sentinel.
func isNotFoundErr(err error) bool {
	if errors.Is(err, storage.ErrNotFound) {
		return true
	}
	if err != nil && strings.Contains(err.Error(), "no issue found matching") {
		return true
	}
	return false
}

// isRoutingFailure reports whether a routed tier's error must reach the caller
// instead of falling through to the next tier: anything but "this tier has no
// route" (errRoutingUnavailable) or a plain not-found. An unreadable
// routes.jsonl or a routed store that fails to open is why the lookup missed,
// and returning the local not-found in its place would misstate the cause.
func isRoutingFailure(err error) bool {
	return err != nil && !errors.Is(err, errRoutingUnavailable) && !isNotFoundErr(err)
}

// RoutedResult contains the result of a routed issue lookup
type RoutedResult struct {
	Issue      *types.Issue
	Store      storage.DoltStorage // The store that contains this issue (may be routed)
	Routed     bool                // true if the issue was found via routing
	ResolvedID string              // The resolved (full) issue ID
	closeFn    func()              // Function to close routed storage (if any)
}

// Close closes any routed storage. Safe to call if Routed is false.
func (r *RoutedResult) Close() {
	if r.closeFn != nil {
		r.closeFn()
		r.closeFn = nil
	}
}

// commitRoutedWrite commits what command wrote through result when result is
// routed. The post-run auto-commit covers only the local store, so a command
// that writes to a routed target commits that write itself, as bd update and
// bd close do. An unrouted result is left to the post-run auto-commit.
func commitRoutedWrite(ctx context.Context, result *RoutedResult, command string) error {
	if !result.Routed {
		return nil
	}
	return commitPendingIfEmbedded(ctx, result.Store, currentActor(), doltAutoCommitParams{
		Command:  command,
		IssueIDs: []string{result.ResolvedID},
	})
}

// resolveAndGetIssueWithRouting resolves a partial ID and gets the issue.
// Tries the local store first, then prefix-based routing via routes.jsonl,
// then falls back to auto-routing (routing.mode=auto). A routing failure, such
// as an unreadable routes.jsonl or a routed store that fails to open, is
// returned as itself rather than as the local not-found.
//
// Returns a RoutedResult containing the issue, resolved ID, and the store to use.
// The caller MUST call result.Close() when done to release any routed storage.
//
// Routed target stores, prefix and auto alike, are opened read-only; mutating
// commands must use resolveAndGetIssueForMutation instead so a routed read can
// never write migrations or other open-time mutations into a foreign project
// (GH#3231, #4141).
func resolveAndGetIssueWithRouting(ctx context.Context, localStore storage.DoltStorage, id string) (*RoutedResult, error) {
	return resolveAndGetIssueWithRoutingAccess(ctx, localStore, id, false)
}

// resolveAndGetIssueForMutation resolves an issue like
// resolveAndGetIssueWithRouting, but opens the routed target store writable so
// mutation commands commit to the repository that holds the issue: a
// prefix-routed rig, or the target of any auto-route, a contributor's planning
// repository included (openAutoRoutedStore).
func resolveAndGetIssueForMutation(ctx context.Context, localStore storage.DoltStorage, id string) (*RoutedResult, error) {
	return resolveAndGetIssueWithRoutingAccess(ctx, localStore, id, true)
}

func resolveAndGetIssueWithRoutingAccess(ctx context.Context, localStore storage.DoltStorage, id string, writable bool) (*RoutedResult, error) {
	// Try local store first.
	result, err := resolveAndGetFromStore(ctx, localStore, id, false)
	if err == nil {
		return result, nil
	}

	// The first routed tier that refuses the id answers instead of the local
	// not-found (isRoutedResolutionRefusal). The refusal is checked before
	// isRoutingFailure, which it would also satisfy, so the auto tier still
	// gets its chance.
	var refusal error

	// If not found locally, try prefix-based routing via routes.jsonl.
	// This handles cross-rig lookups where the ID's prefix maps to a different
	// database (e.g., hr-8wn.1 routes to the herald rig's database).
	var prefixErr error
	if isNotFoundErr(err) {
		prefixResult, routeErr := resolveViaPrefixRoutingWithAccess(ctx, id, writable, false)
		if routeErr == nil {
			return prefixResult, nil
		} else if isRoutedResolutionRefusal(routeErr) {
			refusal = routeErr
		} else if isRoutingFailure(routeErr) {
			return nil, routeErr
		}
		prefixErr = routeErr
	}

	// If not found via prefix routing, try auto-routing as fallback (GH#2345).
	// The target reopens writable when the caller has write intent; a read-intent
	// caller keeps the read-only open that guarantees hydrating a foreign project
	// cannot mutate it (GH#3231, bd-6dnrw.32).
	var autoErr error
	if isNotFoundErr(err) {
		autoResult, routeErr := resolveViaAutoRoutingWithAccess(ctx, localStore, id, writable, false)
		if routeErr == nil {
			return autoResult, nil
		} else if refusal != nil {
			return nil, refusal
		} else if isRoutingFailure(routeErr) {
			return nil, routeErr
		}
		autoErr = routeErr
	}

	return nil, annotateLookupFailure(ctx, localStore, err, prefixErr, autoErr)
}

// isRoutedResolutionRefusal reports whether a routed tier's resolution error
// answers in place of the local store's not-found. Both sentinels mean the
// routed store holds what the id names but will not pick it: the id names
// several issues there (utils.ErrAmbiguousID), or it only abbreviates an
// issue there and exact resolution was required
// (utils.ErrAbbreviatedIDNotAllowed). Reporting the local "no issue found"
// instead would be false.
func isRoutedResolutionRefusal(err error) bool {
	return errors.Is(err, utils.ErrAmbiguousID) || errors.Is(err, utils.ErrAbbreviatedIDNotAllowed)
}

// idResolverFor is the store id resolution should ask. A backend that can only
// look ids up exactly (utils.ExactIDLookupStore, the http backend) sits under
// the decorator chain, which does not forward that capability, so the
// resolver is handed the backend itself: resolution is a pure read, and the
// decorators (hooks, the external-dependency policy, telemetry) decide
// nothing about it. Every other store is resolved through s unchanged.
func idResolverFor(s storage.DoltStorage) utils.PartialIDResolverStore {
	if s == nil {
		return s
	}
	if raw := storage.UnwrapStore(s); raw != nil {
		if exact, ok := raw.(utils.ExactIDLookupStore); ok && exact.ExactIDLookupOnly() {
			return raw
		}
	}
	return s
}

// resolveAndGetFromStore resolves a partial ID and gets the issue from a specific store.
func resolveAndGetFromStore(ctx context.Context, s storage.DoltStorage, id string, routed bool) (*RoutedResult, error) {
	// First, resolve the partial ID
	resolvedID, err := utils.ResolvePartialID(ctx, idResolverFor(s), id)
	if err != nil {
		return nil, err
	}

	// Then get the issue
	issue, err := s.GetIssue(ctx, resolvedID)
	if err != nil {
		return nil, err
	}

	return &RoutedResult{
		Issue:      issue,
		Store:      s,
		Routed:     routed,
		ResolvedID: resolvedID,
	}, nil
}

// resolveAndGetFromStoreExact is resolveAndGetFromStore's exact-match sibling:
// it requires utils.ResolvePartialIDExact instead of the default abbreviation-
// tolerant resolver, so a non-exact candidate (e.g. a reserved word that
// happens to be a leading-prefix abbreviation of some issue's hash) is
// reported as "not found" rather than silently resolved.
func resolveAndGetFromStoreExact(ctx context.Context, s storage.DoltStorage, id string, routed bool) (*RoutedResult, error) {
	resolvedID, err := utils.ResolvePartialIDExact(ctx, idResolverFor(s), id)
	if err != nil {
		return nil, err
	}

	issue, err := s.GetIssue(ctx, resolvedID)
	if err != nil {
		return nil, err
	}

	return &RoutedResult{
		Issue:      issue,
		Store:      s,
		Routed:     routed,
		ResolvedID: resolvedID,
	}, nil
}

// resolveAndGetIssueForMutationExact resolves an issue like
// resolveAndGetIssueForMutation, but requires an EXACT id match against each
// store it tries (no leading-prefix abbreviation matching) — see
// utils.ResolvePartialIDExact. Cross-rig prefix routing and auto-routing are
// still attempted as fallbacks exactly as before if the local store doesn't
// have the issue at all; only the per-store resolution within each tier is
// tightened. Used by write paths where an id argument
// that doesn't exactly name a real issue must error out instead of silently
// mutating whatever it fuzzy-matches (`bd comment list <id>`, a typo for
// `bd comments list`, was silently resolving "list" to a wisp whose hash
// happened to start with "list"). A routing failure is returned as itself, as
// resolveAndGetIssueForMutation returns it.
func resolveAndGetIssueForMutationExact(ctx context.Context, localStore storage.DoltStorage, id string) (*RoutedResult, error) {
	result, err := resolveAndGetFromStoreExact(ctx, localStore, id, false)
	if err == nil {
		return result, nil
	}
	if !isNotFoundErr(err) {
		return nil, err
	}

	// The routed tiers resolve with utils.ResolvePartialIDExact too, so each
	// can refuse the id on its own — it abbreviates a real issue in the routed
	// rig, or is a hash several issues share there — and both wrappers hand
	// that error back unwrapped. Keep the first such refusal: the local error
	// is only a plain not-found, and returning it would tell the caller no
	// issue matches an id a routed store demonstrably holds, which is exactly
	// the falsehood the sentinels exist to prevent (isRoutedResolutionRefusal).
	// The refusal is checked before isRoutingFailure, which it would also
	// satisfy, so the auto tier still gets its chance at an exact match.
	var refusal error
	prefixResult, prefixErr := resolveViaPrefixRoutingWithAccess(ctx, id, true, true)
	switch {
	case prefixErr == nil:
		return prefixResult, nil
	case isRoutedResolutionRefusal(prefixErr):
		refusal = prefixErr
	case isRoutingFailure(prefixErr):
		return nil, prefixErr
	}

	// Write intent, as in resolveAndGetIssueForMutation: the auto-route opens
	// writable so the write commits where the issue lives. The auto tier's own
	// refusal is a routing failure here, and is returned as one.
	autoResult, autoErr := resolveViaAutoRoutingWithAccess(ctx, localStore, id, true, true)
	switch {
	case autoErr == nil:
		return autoResult, nil
	case refusal != nil:
		return nil, refusal
	case isRoutingFailure(autoErr):
		return nil, autoErr
	}

	return nil, err
}

// resolveViaAutoRoutingWithAccess attempts to find an issue using auto-routing.
// This is the fallback when the local store doesn't have the issue (GH#2345).
// Returns a RoutedResult if the issue is found in the auto-routed store.
//
// writable is the caller's write intent: the target opens writable for it,
// whichever rule selected the target (openAutoRoutedStore).
//
// exact selects utils.ResolvePartialIDExact over the default abbreviation-
// tolerant resolver for the auto-routed store lookup, mirroring the local-store
// distinction in resolveAndGetFromStore vs resolveAndGetFromStoreExact.
//
// Not-found and unavailable outcomes come back as *autoRouteFailure so the
// caller can disclose the routed store in the not-found message. bd list
// already names it unprompted; the lookup path knew it too and used to throw it
// away (bd-1uu). The failure wraps the underlying error, so isRoutingFailure and
// errors.Is see through it.
func resolveViaAutoRoutingWithAccess(ctx context.Context, localStore storage.DoltStorage, id string, writable bool, exact bool) (*RoutedResult, error) {
	routedStore, target, err := openRoutedStoreTarget(ctx, localStore, writable)
	if target == nil {
		if err != nil {
			return nil, err
		}
		return nil, newAutoRouteFailure(ctx, nil, nil, fmt.Errorf("%w: no auto-routed store available", errRoutingUnavailable))
	}
	if err != nil {
		return nil, newAutoRouteFailure(ctx, target, nil, err)
	}

	var result *RoutedResult
	var resolveErr error
	if exact {
		result, resolveErr = resolveAndGetFromStoreExact(ctx, routedStore, id, true)
	} else {
		result, resolveErr = resolveAndGetFromStore(ctx, routedStore, id, true)
	}
	if resolveErr != nil {
		// Build the failure before closing: the differential count can only be
		// read from a store that is still open.
		failure := newAutoRouteFailure(ctx, target, routedStore, resolveErr)
		_ = routedStore.Close()
		return nil, failure
	}
	result.closeFn = func() { _ = routedStore.Close() }
	return result, nil
}

// newAutoRouteFailure builds the auto-routing disclosure for a lookup that did
// not find the issue. target is nil when no routing rule applies at all; store
// is nil when the routed target was never opened, and must still be open when
// it is passed, since the issue count is read from it.
func newAutoRouteFailure(ctx context.Context, target *routedTarget, store issueCounter, err error) *autoRouteFailure {
	failure := &autoRouteFailure{Count: unknownIssueCount, Err: err}
	if target == nil {
		return failure
	}
	failure.Rule = target.Rule
	failure.BeadsDir = target.BeadsDir
	failure.Searched = store != nil && isNotFoundErr(err)
	if failure.Searched {
		failure.Count = countIssues(ctx, store)
	}
	return failure
}

// autoRouteFailure records that contributor auto-routing was consulted and did
// not produce the issue, together with the store it addressed.
//
// The routed store is the whole content of bd list's unprompted notice, so a
// lookup that silently drops it makes a misroute indistinguishable from a
// deleted bead — the defect bd-1uu was filed for.
type autoRouteFailure struct {
	Rule     routing.RoutingRule // the rule that selected the target
	BeadsDir string              // .beads directory of the routed target ("" when no rule applies)
	Searched bool                // the routed store was opened and queried
	Count    int                 // total issues in the routed store; unknownIssueCount when not counted
	Err      error
}

func (f *autoRouteFailure) Error() string {
	if f.Err == nil {
		return "auto-routing failed"
	}
	return f.Err.Error()
}

func (f *autoRouteFailure) Unwrap() error { return f.Err }

// explain renders the auto-routing half of a lookup failure. It returns "" when
// no routing rule applies at all — the ordinary case, where there is no second
// store and therefore nothing the reader could act on.
func (f *autoRouteFailure) explain() string {
	if f.BeadsDir == "" {
		return ""
	}
	mechanism, fix := routingRuleMechanism(f.Rule)
	if f.Searched {
		return fmt.Sprintf("%s also searched %s%s (fix: %s)",
			mechanism, describeDatabaseAt(f.BeadsDir), describeIssueCount(f.Count), fix)
	}
	return fmt.Sprintf("%s routes to %s, which could not be searched: %v (fix: %s)",
		mechanism, describeDatabaseAt(f.BeadsDir), f.Err, fix)
}

// prefixRouteFailure records why prefix routing did not produce an issue, so a
// failed lookup can name the database it actually addressed.
//
// A bare "no issue found" cannot distinguish an absent bead from a bead that
// lives in a database the command never opened. Agents have read the first as
// the second and concluded that live P1 beads had been destroyed by a purge
// (bd-4sw). An unroutable prefix and an absent bead are different answers and
// must not print the same string.
type prefixRouteFailure struct {
	Prefix      string // bead ID prefix, e.g. "hq-"
	RoutesFile  string // routes.jsonl consulted ("" when none was found)
	RoutePath   string // matched route's path, relative to the town root
	TargetDB    string // dolt database the route resolves to
	Searched    bool   // the routed database was opened and queried
	SameAsLocal bool   // the route resolves back to the local database
	Count       int    // total issues in the routed database; unknownIssueCount when not counted
	Err         error
}

func (f *prefixRouteFailure) Error() string {
	if f.Err == nil {
		return fmt.Sprintf("prefix routing failed for %q", f.Prefix)
	}
	return f.Err.Error()
}

func (f *prefixRouteFailure) Unwrap() error { return f.Err }

// explain renders the routing half of a lookup failure for the user. It
// returns "" when there is nothing worth saying: when no routes.jsonl exists at
// all (the ordinary single-repo case, not a routing problem), and when the
// route resolves back to the database the caller already named as searched.
func (f *prefixRouteFailure) explain() string {
	switch {
	case f.RoutesFile == "" || f.SameAsLocal:
		return ""
	case f.Searched:
		return fmt.Sprintf("prefix %q routes to %s (database %q%s), which was also searched",
			f.Prefix, f.describeTarget(), f.TargetDB, describeIssueCount(f.Count))
	case f.RoutePath != "":
		return fmt.Sprintf("prefix %q routes to %s, but that database could not be searched: %v",
			f.Prefix, f.describeTarget(), f.Err)
	default:
		return fmt.Sprintf("prefix %q has no route in %s", f.Prefix, f.RoutesFile)
	}
}

// describeTarget names the routed target the way a reader can act on. The town
// route is recorded as "." in routes.jsonl, which is meaningless on its own.
func (f *prefixRouteFailure) describeTarget() string {
	if f.RoutePath == "." {
		return "the town root"
	}
	return f.RoutePath
}

// annotateLookupFailure names the database a not-found lookup actually searched
// and, when the ID's prefix routes somewhere else, says where (bd-4sw). It also
// discloses contributor auto-routing and the total issue count of every store
// that answered (bd-1uu). Other errors pass through untouched.
//
// The counts are the positive control, and they are the reason the sibling
// bd list notice actually works: a store holding zero issues is a wrong store,
// a store holding thousands really is missing the bead. Without them "not
// found" cannot be told apart from "you are addressing the wrong database",
// and agents have read the first as proof that live P1 beads were destroyed.
//
// The original error is wrapped, so errors.Is(storage.ErrNotFound) and the
// "no issue found matching" text that isNotFoundErr and the protocol contract
// match on both still hold.
func annotateLookupFailure(ctx context.Context, localStore issueCounter, err error, prefixErr, autoErr error) error {
	if !isNotFoundErr(err) {
		return err
	}
	var parts []string
	if where := describeLocalDatabase(ctx, localStore); where != "" {
		parts = append(parts, "searched "+where)
	}
	var routeFailure *prefixRouteFailure
	if errors.As(prefixErr, &routeFailure) {
		if explanation := routeFailure.explain(); explanation != "" {
			parts = append(parts, explanation)
		}
	}
	var autoFailure *autoRouteFailure
	if errors.As(autoErr, &autoFailure) {
		if explanation := autoFailure.explain(); explanation != "" {
			parts = append(parts, explanation)
		}
	}
	if len(parts) == 0 {
		return err
	}
	return fmt.Errorf("%w (%s)", err, strings.Join(parts, "; "))
}

// describeLocalDatabase names the database the command opened and how many
// issues it holds, so a not-found answer says which database it is an answer
// about and whether that database holds anything at all.
func describeLocalDatabase(ctx context.Context, localStore issueCounter) string {
	beadsDir := resolveCommandBeadsDir(dbPath)
	if beadsDir == "" {
		return ""
	}
	return describeDatabaseAt(beadsDir) + describeIssueCount(countIssues(ctx, localStore))
}

// describeDatabaseAt names a store by its declared Dolt database and the beads
// directory it was opened from, falling back to the directory alone when the
// database cannot be read. Every store a lookup consulted is named this way, so
// the reader compares like with like.
func describeDatabaseAt(beadsDir string) string {
	if database := readDoltDatabase(beadsDir); database != "" {
		return fmt.Sprintf("database %q at %s", database, beadsDir)
	}
	return beadsDir
}

// unknownIssueCount marks a store that was never counted, so a real zero — the
// single most diagnostic count there is — is never confused with "no answer".
const unknownIssueCount = -1

// issueCounter is the slice of storage a lookup failure needs: the total issue
// count that turns a bare "not found" into a differential answer.
type issueCounter interface {
	GetStatisticsNoBlocked(ctx context.Context) (*types.Statistics, error)
}

// countIssues returns the total number of issues in s, or unknownIssueCount if
// it cannot be read. The no-blocked variant is deliberate: only the total is
// wanted, and this runs on an error path where the blocked-set traversal would
// be pure cost.
func countIssues(ctx context.Context, s issueCounter) int {
	if s == nil {
		return unknownIssueCount
	}
	stats, err := s.GetStatisticsNoBlocked(ctx)
	if err != nil || stats == nil {
		debug.Logf("[routing] could not count issues for lookup failure: %v\n", err)
		return unknownIssueCount
	}
	return stats.TotalIssues
}

// describeIssueCount renders a store's issue count as a clause to append to the
// store's name, or "" when the store was not counted.
func describeIssueCount(count int) string {
	if count < 0 {
		return ""
	}
	return fmt.Sprintf(", holding %d issue(s)", count)
}

// prefixRoute represents a prefix-to-path routing rule from routes.jsonl.
type prefixRoute struct {
	Prefix string `json:"prefix"` // Issue ID prefix (e.g., "hr-")
	Path   string `json:"path"`   // Relative path to rig directory from town root
}

// resolveViaPrefixRouting attempts to find an issue by looking up its prefix
// in routes.jsonl and opening the target rig's database read-only.
//
// This enables cross-rig lookups: when running from a redirected .beads directory
// (e.g., crew/beercan → town/.beads with database "hq"), a bead ID like "hr-8wn.1"
// can be resolved by following the "hr-" route to the herald rig's .beads directory,
// which declares dolt_database="herald".
//
// The read-only open guarantees a routed read cannot mutate the target; mutation
// commands must route through resolveViaPrefixRoutingWithAccess with writable=true.
func resolveViaPrefixRouting(ctx context.Context, id string) (*RoutedResult, error) {
	return resolveViaPrefixRoutingWithAccess(ctx, id, false, false)
}

// resolveViaPrefixRoutingWithAccess is the shared implementation that selects the
// store-open mode. writable opens the routed target writable, behaving like running
// the command inside that rig; false keeps the read-only open that guarantees a
// routed read cannot mutate the target (bd-6dnrw.32). exact selects
// utils.ResolvePartialIDExact over the default abbreviation-tolerant resolver for
// the routed store lookup, mirroring resolveAndGetFromStore vs
// resolveAndGetFromStoreExact.
//
// Every failure that is not a bare "no route to follow" comes back as a
// *prefixRouteFailure wrapping the underlying error, so a not-found can name the
// database it addressed (bd-4sw, bd-1uu) while isRoutingFailure and errors.Is
// still see the underlying error.
func resolveViaPrefixRoutingWithAccess(ctx context.Context, id string, writable bool, exact bool) (*RoutedResult, error) {
	matchedRoute, targetBeadsDir, src, err := lookupPrefixRoute(id)
	if err != nil {
		return nil, err
	}
	prefix := matchedRoute.Prefix

	// Refuse what admission refuses from the target's own contents first, as
	// an auto-route does (openAutoRoutedStore): the dolt_database lookup
	// below would otherwise report a historical SQLite or legacy Dolt rig as
	// a missing dolt_database field, or a missing metadata.json.
	if _, err := admitForeignWorkspace(targetBeadsDir); err != nil {
		return nil, fmt.Errorf("prefix route %q to %s: %w", matchedRoute.Prefix, matchedRoute.Path, err)
	}

	// Check that the target has a different dolt_database
	targetDB, err := loadDoltDatabase(targetBeadsDir)
	if err == nil && targetDB == "" {
		err = fmt.Errorf("%s has no dolt_database field", filepath.Join(targetBeadsDir, "metadata.json"))
	}
	if err != nil {
		return nil, &prefixRouteFailure{
			Prefix:     prefix,
			RoutesFile: src.File,
			RoutePath:  matchedRoute.Path,
			Count:      unknownIssueCount,
			Err:        fmt.Errorf("prefix route %q to %s: target rig has no dolt_database configured: %w", matchedRoute.Prefix, matchedRoute.Path, err),
		}
	}

	debug.Logf("[routing] Prefix %q matched route to %s (database: %s)\n", matchedRoute.Prefix, matchedRoute.Path, targetDB)

	targetStore, err := openRoutedTargetStore(ctx, targetBeadsDir, targetDB, writable)
	if err != nil {
		return nil, &prefixRouteFailure{
			Prefix:     prefix,
			RoutesFile: src.File,
			RoutePath:  matchedRoute.Path,
			TargetDB:   targetDB,
			Count:      unknownIssueCount,
			Err:        fmt.Errorf("opening routed store for %s: %w", matchedRoute.Path, err),
		}
	}

	var result *RoutedResult
	if exact {
		result, err = resolveAndGetFromStoreExact(ctx, targetStore, id, true)
	} else {
		result, err = resolveAndGetFromStore(ctx, targetStore, id, true)
	}
	if err != nil {
		failure := &prefixRouteFailure{
			Prefix:     prefix,
			RoutesFile: src.File,
			RoutePath:  matchedRoute.Path,
			TargetDB:   targetDB,
			Searched:   isNotFoundErr(err),
			Count:      unknownIssueCount,
			Err:        err,
		}
		// Count while the routed store is still open. A routed database that
		// holds zero issues is the reader's evidence that the route landed
		// somewhere unrelated rather than that the bead is gone (bd-1uu).
		if failure.Searched {
			failure.Count = countIssues(ctx, targetStore)
		}
		_ = targetStore.Close()
		return nil, failure
	}
	result.closeFn = func() { _ = targetStore.Close() }

	if os.Getenv("BD_DEBUG_ROUTING") != "" {
		fmt.Fprintf(os.Stderr, "[routing] Resolved %s via prefix route to %s (database: %s)\n", id, remedy.PrintedValue(matchedRoute.Path), remedy.PrintedValue(targetDB))
	}

	return result, nil
}

// lookupPrefixRoute finds the routes.jsonl route for id and returns it with
// the target rig's .beads directory and the routes file it came from. Having no
// route to follow (no prefix in the ID, no routes, no matching route, or a route
// back to the database already searched) wraps errRoutingUnavailable; an
// unreadable routes.jsonl does not, so callers report it instead of falling
// through.
//
// routes.jsonl is located by search rather than assumed to sit in the resolved
// beads directory (bd-4sw), and the most specific matching prefix wins
// (GH#5048).
func lookupPrefixRoute(id string) (*prefixRoute, string, *prefixRoutesSource, error) {
	prefix := extractBeadPrefix(id)
	if strings.Index(id, "-") <= 0 || prefix == "" {
		return nil, "", nil, fmt.Errorf("%w: no prefix in ID %q", errRoutingUnavailable, id)
	}

	// The beads directory this command resolved to. It is the town's own
	// .beads only when cwd happens to sit at the town root; everywhere else it
	// is a rig's (often redirected) .beads.
	currentBeadsDir := resolveCommandBeadsDir(dbPath)

	src, err := findPrefixRoutesSourceErr(currentBeadsDir)
	if err != nil {
		return nil, "", nil, fmt.Errorf("loading routes: %w", err)
	}
	if src == nil {
		return nil, "", nil, &prefixRouteFailure{
			Prefix: prefix,
			Count:  unknownIssueCount,
			Err:    fmt.Errorf("%w: no routes available", errRoutingUnavailable),
		}
	}

	// Find the most specific route for this ID
	matchedRoute := matchPrefixRoute(src.Routes, id)
	if matchedRoute == nil {
		return nil, "", nil, &prefixRouteFailure{
			Prefix:     prefix,
			RoutesFile: src.File,
			Count:      unknownIssueCount,
			Err:        fmt.Errorf("%w: no route for ID %q", errRoutingUnavailable, id),
		}
	}

	// Resolve the target rig's .beads directory. Path "." is the town itself,
	// which filepath.Join resolves to the town root.
	rigDir := filepath.Join(src.TownRoot, matchedRoute.Path)
	targetBeadsDir := beads.FollowRedirect(filepath.Join(rigDir, ".beads"))

	// A route resolving back to the database this command already searched has
	// nothing to add. Compare the resolved directories instead of skipping the
	// town route ("." ) outright: from a rig context the town database is a
	// genuinely different database and must be followed (bd-4sw).
	if sameResolvedDir(targetBeadsDir, currentBeadsDir) {
		return nil, "", nil, &prefixRouteFailure{
			Prefix:      matchedRoute.Prefix,
			RoutesFile:  src.File,
			RoutePath:   matchedRoute.Path,
			SameAsLocal: true,
			Count:       unknownIssueCount,
			Err:         fmt.Errorf("%w: route points to the database already searched", errRoutingUnavailable),
		}
	}
	return matchedRoute, targetBeadsDir, src, nil
}

// openRoutedTargetStore opens the store of a routed target, prefix or auto,
// writable or read-only. BEADS_DOLT_SERVER_DATABASE outranks the target's
// metadata.json when a store resolves its database name, and it names the
// launching workspace's database (set by the user, or by
// preserveRedirectSourceDatabase after a redirect), so while the target opens
// it is pointed at database, the target's own dolt_database. Otherwise a
// server-mode open would connect to the launcher's database on the shared
// server, and an embedded writable open would create or migrate a wrongly
// named database under the target. An empty database leaves the environment
// as it is: the target names no database of its own to prefer.
//
// It opens through openForeignStore, so a target admitForeignWorkspace
// refuses (a legacy Dolt workspace among them) is refused before the open can
// start a server there or run migrations into its database. That is not every
// target bd run inside it refuses: see guardForeignLegacyDoltWorkspace and
// targetSelectsServerMode for the legacy Dolt layouts it admits.
//
// A writable open never creates the target's database
// (newExistingDoltStoreFromConfig): the open runs before the lookup it serves,
// so an id that misses must leave a repository that has not been bootstrapped
// yet as it found it. A target that has been bootstrapped does run its
// open-time schema migrations on such a miss, as any bd command run in that
// repository would; they bring it to the schema this bd writes, not to
// anything the missed id asked for.
func openRoutedTargetStore(ctx context.Context, beadsDir, database string, writable bool) (storage.DoltStorage, error) {
	if database != "" {
		origDB := os.Getenv("BEADS_DOLT_SERVER_DATABASE")
		_ = os.Setenv("BEADS_DOLT_SERVER_DATABASE", database)
		defer func() {
			// Restore the original env var
			if origDB != "" {
				_ = os.Setenv("BEADS_DOLT_SERVER_DATABASE", origDB)
			} else {
				_ = os.Unsetenv("BEADS_DOLT_SERVER_DATABASE")
			}
		}()
	}
	if writable {
		return openForeignStore(ctx, beadsDir, foreignWrite)
	}
	return openForeignStore(ctx, beadsDir, foreignRead)
}

// matchPrefixRoute returns the route whose prefix is the longest leading match
// for beadID, or nil if none matches. Route prefixes end in "-", so a
// multi-hyphen prefix such as "claude-os-" matches "claude-os-76l", and with
// both "hq-" and "hq-cv-" configured, "hq-cv-abc" takes the "hq-cv-" route
// (GH#5048).
func matchPrefixRoute(routes []prefixRoute, beadID string) *prefixRoute {
	var best *prefixRoute
	for i, r := range routes {
		if !strings.HasSuffix(r.Prefix, "-") || !strings.HasPrefix(beadID, r.Prefix) {
			continue
		}
		if best == nil || len(r.Prefix) > len(best.Prefix) {
			best = &routes[i]
		}
	}
	return best
}

// prefixRoutesSource is a routes.jsonl located for prefix routing, together
// with the town root that the routes' relative paths are anchored to.
type prefixRoutesSource struct {
	File     string // absolute path to the routes.jsonl that was loaded
	TownRoot string // directory owning the .beads dir that holds File
	Routes   []prefixRoute
}

// findPrefixRoutesSource locates the routes.jsonl that governs prefix routing.
//
// routes.jsonl lives only in the town's own .beads directory. The beads
// directory a command resolved to is that directory only when cwd happens to
// sit at the town root; from any rig worktree it is the rig's (often
// redirected) .beads, which carries no routes at all. Loading routes from the
// resolved directory — and deriving the town root as that directory's parent —
// therefore made prefix routing silently unavailable everywhere except the
// town root, so every foreign-prefix bead reported "no issue found" (bd-4sw).
//
// Search the resolved beads directory first (that is the town's own .beads at
// the town root, and preserves the historical behavior), then walk up from cwd
// and from the resolved beads directory. The town root is derived from where
// routes.jsonl actually was, never assumed.
func findPrefixRoutesSource(currentBeadsDir string) *prefixRoutesSource {
	src, _ := findPrefixRoutesSourceErr(currentBeadsDir)
	return src
}

// findPrefixRoutesSourceErr is findPrefixRoutesSource that also reports a
// routes.jsonl that exists but could not be read, when no readable one was
// found. A missing file is not an error: it is the ordinary single-repo case.
func findPrefixRoutesSourceErr(currentBeadsDir string) (*prefixRoutesSource, error) {
	var candidates []string
	if currentBeadsDir != "" {
		candidates = append(candidates, filepath.Join(currentBeadsDir, "routes.jsonl"))
	}
	if cwd, err := os.Getwd(); err == nil {
		candidates = append(candidates, routesFilesAbove(cwd)...)
	}
	if currentBeadsDir != "" {
		candidates = append(candidates, routesFilesAbove(filepath.Dir(currentBeadsDir))...)
	}

	var readErr error
	seen := make(map[string]bool, len(candidates))
	for _, path := range candidates {
		if seen[path] {
			continue
		}
		seen[path] = true
		routes, err := loadPrefixRoutesFile(path)
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) && readErr == nil {
				readErr = err
			}
			continue
		}
		if len(routes) == 0 {
			continue
		}
		debug.Logf("[routing] Loaded %d prefix route(s) from %s\n", len(routes), path)
		return &prefixRoutesSource{
			File: path,
			// <town>/.beads/routes.jsonl -> <town>
			TownRoot: filepath.Dir(filepath.Dir(path)),
			Routes:   routes,
		}, nil
	}
	return nil, readErr
}

// routesFilesAbove returns candidate routes.jsonl paths for dir and each of its
// ancestors, nearest first.
func routesFilesAbove(dir string) []string {
	if dir == "" {
		return nil
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		abs = dir
	}
	var out []string
	for d := filepath.Clean(abs); ; d = filepath.Dir(d) {
		out = append(out, filepath.Join(d, ".beads", "routes.jsonl"))
		if parent := filepath.Dir(d); parent == d {
			return out
		}
	}
}

// sameResolvedDir reports whether two paths denote the same directory,
// resolving symlinks when both sides can be resolved.
func sameResolvedDir(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	resolvedA, errA := filepath.EvalSymlinks(a)
	resolvedB, errB := filepath.EvalSymlinks(b)
	if errA != nil || errB != nil {
		return false
	}
	return resolvedA == resolvedB
}

// extractBeadPrefix extracts the prefix from a bead ID.
// For example, "hr-8wn.1" returns "hr-", "hq-cv-abc" returns "hq-".
func extractBeadPrefix(beadID string) string {
	if beadID == "" {
		return ""
	}
	idx := strings.Index(beadID, "-")
	if idx <= 0 {
		return ""
	}
	return beadID[:idx+1]
}

// readDoltDatabase reads the dolt_database field from a .beads/metadata.json
// file, or "" when it cannot be read.
func readDoltDatabase(beadsDir string) string {
	database, err := loadDoltDatabase(beadsDir)
	if err != nil {
		return ""
	}
	return database
}

// loadPrefixRoutes loads prefix-to-path routes from routes.jsonl in the beads directory.
func loadPrefixRoutes(beadsDir string) ([]prefixRoute, error) {
	return loadPrefixRoutesFile(filepath.Join(beadsDir, "routes.jsonl"))
}

// loadPrefixRoutesFile loads prefix-to-path routes from a routes.jsonl file.
func loadPrefixRoutesFile(routesPath string) ([]prefixRoute, error) {
	file, err := os.Open(routesPath) //nolint:gosec // G304: path is constructed from trusted beads directory
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var routes []prefixRoute
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		var route prefixRoute
		if err := json.Unmarshal([]byte(line), &route); err != nil {
			continue
		}
		if route.Prefix != "" && route.Path != "" {
			routes = append(routes, route)
		}
	}
	return routes, scanner.Err()
}

// loadDoltDatabase reads the dolt_database field from a .beads/metadata.json
// file, and reports a metadata.json it cannot read or parse; a file without the
// field returns "" and no error.
func loadDoltDatabase(beadsDir string) (string, error) {
	metadataPath := filepath.Join(beadsDir, "metadata.json")
	data, err := os.ReadFile(metadataPath) //nolint:gosec // G304: path is constructed from trusted beads directory
	if err != nil {
		return "", err
	}
	var meta struct {
		DoltDatabase string `json:"dolt_database"`
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		return "", fmt.Errorf("parsing %s: %w", remedy.PrintedValue(metadataPath), remedy.PrintedError(err))
	}
	return meta.DoltDatabase, nil
}

// openStoreAtBeadsDir opens the store that beadsDir declares, and reports which
// Dolt database that was. A "" database with a non-nil error means beadsDir
// declares no database at all — nothing was opened and nothing was searched,
// which is a different answer from "the store was opened and held nothing".
//
// The declared database has to be published through BEADS_DOLT_SERVER_DATABASE
// for the duration of the open: in server mode a store connects to whichever
// database that variable names, so opening a second store without setting it
// hands back a handle on the FIRST store's database. That failure is silent —
// it answers, and it answers about the wrong data — which is exactly the class
// of wrong-store zero this command tree keeps producing (bd-nc4). The previous
// value is restored before returning.
//
// writable opens the target for mutation; false keeps the read-only open that
// guarantees reading a foreign project cannot modify it (GH#3231).
func openStoreAtBeadsDir(ctx context.Context, beadsDir string, writable bool) (storage.DoltStorage, string, error) {
	database := readDoltDatabase(beadsDir)
	if database == "" {
		return nil, "", fmt.Errorf("%s has no dolt_database configured", beadsDir)
	}

	restore := overrideServerDatabase(database)
	defer restore()

	var s storage.DoltStorage
	var err error
	if writable {
		s, err = newDoltStoreFromConfig(ctx, beadsDir)
	} else {
		s, err = newReadOnlyStoreFromConfig(ctx, beadsDir)
	}
	if err != nil {
		return nil, database, err
	}
	return s, database, nil
}

// overrideServerDatabase points server-mode opens at database and returns the
// function that puts the environment back the way it was found — including
// unsetting the variable when it was previously unset, so a restore never
// invents a value the process did not start with.
func overrideServerDatabase(database string) func() {
	const key = "BEADS_DOLT_SERVER_DATABASE"
	original, had := os.LookupEnv(key)
	_ = os.Setenv(key, database)
	return func() {
		if had {
			_ = os.Setenv(key, original)
			return
		}
		_ = os.Unsetenv(key)
	}
}

// getIssueWithRouting gets an issue by exact ID.
// Tries the local store first, then prefix-based routing, then contributor auto-routing.
//
// Returns a RoutedResult containing the issue and the store to use for related queries.
// The caller MUST call result.Close() when done to release any routed storage.
func getIssueWithRouting(ctx context.Context, localStore storage.DoltStorage, id string) (*RoutedResult, error) {
	// Try local store first.
	issue, err := localStore.GetIssue(ctx, id)
	if err == nil {
		return &RoutedResult{
			Issue:      issue,
			Store:      localStore,
			Routed:     false,
			ResolvedID: id,
		}, nil
	}

	// If not found locally, try prefix-based routing via routes.jsonl.
	var prefixErr error
	if isNotFoundErr(err) {
		prefixResult, routeErr := resolveViaPrefixRouting(ctx, id)
		if routeErr == nil {
			return prefixResult, nil
		} else if isRoutingFailure(routeErr) {
			return nil, routeErr
		}
		prefixErr = routeErr
	}

	// If not found via prefix routing, try contributor auto-routing as fallback (GH#2345).
	// getIssueWithRouting is the read-intent entry point, so the routed store stays
	// read-only here; mutation commands go through resolveAndGetIssueForMutation.
	var autoErr error
	if isNotFoundErr(err) {
		autoResult, routeErr := resolveViaAutoRoutingWithAccess(ctx, localStore, id, false, false)
		if routeErr == nil {
			return autoResult, nil
		} else if isRoutingFailure(routeErr) {
			return nil, routeErr
		}
		autoErr = routeErr
	}

	return nil, annotateLookupFailure(ctx, localStore, err, prefixErr, autoErr)
}

// autoRouteTargetDatabase is the dolt_database an auto-routed open points its
// target at (openRoutedTargetStore). A read stays tolerant: a metadata.json
// that is unreadable or names no database yields "", and the target's own open
// decides. A write refuses both, as a prefix route does,
// because the launcher's BEADS_DOLT_SERVER_DATABASE would then pick the
// database, and an embedded writable open creates the database it is given
// when the target holds any other (newExistingDoltStoreFromConfig refuses only
// a target that holds none).
//
// A target with no metadata.json at all is still opened, read or written. That
// is the planning repository contributor setup creates (runContributorWizard,
// autoConfigureForkContributor) until the first routed bd create writes one
// (ensureBeadsDirForPath). Setup names its database as any workspace without
// metadata.json does, after the variable when it is set and the default
// database otherwise, so the open is pointed at the only embedded database the
// target holds, or at the default database when it holds none or several.
// Resolving the name again at use would follow the launcher's variable, which
// need not be the one setup ran with: a read would then miss the database
// setup created, and a write would create another beside it.
func autoRouteTargetDatabase(beadsDir string, writable bool) (string, error) {
	database, err := loadDoltDatabase(beadsDir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if databases := embeddeddolt.Databases(beadsDir); len(databases) == 1 {
			return databases[0], nil
		}
		return configfile.DefaultDoltDatabase, nil
	case !writable:
		return database, nil
	case err != nil:
		// A read or parse error repeats the routed path as the OS spelled it.
		return "", remedy.PrintedError(err)
	case database == "":
		return "", fmt.Errorf("%s has no dolt_database field", remedy.PrintedValue(filepath.Join(beadsDir, "metadata.json")))
	}
	return database, nil
}
