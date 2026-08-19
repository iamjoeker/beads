package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/steveyegge/beads/internal/beads"
	"github.com/steveyegge/beads/internal/debug"
	"github.com/steveyegge/beads/internal/routing"
	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/types"
	"github.com/steveyegge/beads/internal/utils"
)

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
	}
}

// resolveAndGetIssueWithRouting resolves a partial ID and gets the issue.
// Tries the local store first, then prefix-based routing via routes.jsonl,
// then falls back to contributor auto-routing.
//
// Returns a RoutedResult containing the issue, resolved ID, and the store to use.
// The caller MUST call result.Close() when done to release any routed storage.
//
// Prefix-routed target stores are opened read-only; mutating commands must use
// resolveAndGetIssueForMutation instead so a routed read can never write
// migrations or other open-time mutations into a foreign project (GH#3231, #4141).
func resolveAndGetIssueWithRouting(ctx context.Context, localStore storage.DoltStorage, id string) (*RoutedResult, error) {
	return resolveAndGetIssueWithRoutingAccess(ctx, localStore, id, false)
}

// resolveAndGetIssueForMutation resolves an issue like
// resolveAndGetIssueWithRouting, but opens prefix-routed target stores in
// writable mode so mutation commands can commit to the routed repository.
func resolveAndGetIssueForMutation(ctx context.Context, localStore storage.DoltStorage, id string) (*RoutedResult, error) {
	return resolveAndGetIssueWithRoutingAccess(ctx, localStore, id, true)
}

func resolveAndGetIssueWithRoutingAccess(ctx context.Context, localStore storage.DoltStorage, id string, writablePrefixRoute bool) (*RoutedResult, error) {
	// Try local store first.
	result, err := resolveAndGetFromStore(ctx, localStore, id, false)
	if err == nil {
		return result, nil
	}

	// If not found locally, try prefix-based routing via routes.jsonl.
	// This handles cross-rig lookups where the ID's prefix maps to a different
	// database (e.g., hr-8wn.1 routes to the herald rig's database).
	var prefixErr error
	if isNotFoundErr(err) {
		prefixResult, routeErr := resolveViaPrefixRoutingWithAccess(ctx, id, writablePrefixRoute)
		if routeErr == nil {
			return prefixResult, nil
		}
		prefixErr = routeErr
	}

	// If not found via prefix routing, try contributor auto-routing as fallback (GH#2345).
	// Write-intent callers open the auto-routed target writable: an issue that exists
	// only there is an issue the user can otherwise never close or update, and creates
	// already land there. A read-intent caller keeps the read-only open that guarantees
	// hydrating a foreign project cannot mutate it (GH#3231, bd-6dnrw.32).
	var autoErr error
	if isNotFoundErr(err) {
		autoResult, routeErr := resolveViaAutoRouting(ctx, localStore, id, writablePrefixRoute)
		if routeErr == nil {
			return autoResult, nil
		}
		autoErr = routeErr
	}

	return nil, annotateLookupFailure(ctx, localStore, err, prefixErr, autoErr)
}

// resolveAndGetFromStore resolves a partial ID and gets the issue from a specific store.
func resolveAndGetFromStore(ctx context.Context, s storage.DoltStorage, id string, routed bool) (*RoutedResult, error) {
	// First, resolve the partial ID
	resolvedID, err := utils.ResolvePartialID(ctx, s, id)
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

// resolveViaAutoRouting attempts to find an issue using contributor auto-routing.
// This is the fallback when the local store doesn't have the issue (GH#2345).
// Returns a RoutedResult if the issue is found in the auto-routed store.
//
// writable opens the routed target writable so a mutation command can commit
// there; false keeps the read-only open that guarantees a routed read cannot
// mutate the target.
//
// Failures come back as *autoRouteFailure so the caller can disclose the routed
// store in the not-found message. bd list already names it unprompted; the
// lookup path knew it too and used to throw it away (bd-1uu).
func resolveViaAutoRouting(ctx context.Context, localStore storage.DoltStorage, id string, writable bool) (*RoutedResult, error) {
	routedStore, target, err := openRoutedStoreTarget(ctx, localStore, writable)
	if target == nil {
		return nil, newAutoRouteFailure(ctx, nil, nil, errors.New("no auto-routed store configured"))
	}
	if err != nil {
		return nil, newAutoRouteFailure(ctx, target, nil, err)
	}

	result, resolveErr := resolveAndGetFromStore(ctx, routedStore, id, true)
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
	return resolveViaPrefixRoutingWithAccess(ctx, id, false)
}

// resolveViaPrefixRoutingWithAccess is the shared implementation that selects the
// store-open mode. writable opens the routed target writable, behaving like running
// the command inside that rig; false keeps the read-only open that guarantees a
// routed read cannot mutate the target (bd-6dnrw.32).
func resolveViaPrefixRoutingWithAccess(ctx context.Context, id string, writable bool) (*RoutedResult, error) {
	// Extract prefix from the bead ID (e.g., "hr-" from "hr-8wn.1")
	prefix := extractBeadPrefix(id)
	if prefix == "" {
		return nil, fmt.Errorf("no prefix in ID %q", id)
	}

	// The beads directory this command resolved to. It is the town's own
	// .beads only when cwd happens to sit at the town root; everywhere else it
	// is a rig's (often redirected) .beads.
	currentBeadsDir := resolveCommandBeadsDir(dbPath)

	// Locate routes.jsonl by search rather than assuming it sits in the
	// resolved beads directory (bd-4sw).
	src := findPrefixRoutesSource(currentBeadsDir)
	if src == nil {
		return nil, &prefixRouteFailure{
			Prefix: prefix,
			Count:  unknownIssueCount,
			Err:    errors.New("no routes.jsonl found"),
		}
	}

	// Find matching route for this prefix
	var matchedRoute *prefixRoute
	for i, r := range src.Routes {
		if r.Prefix == prefix {
			matchedRoute = &src.Routes[i]
			break
		}
	}
	if matchedRoute == nil {
		return nil, &prefixRouteFailure{
			Prefix:     prefix,
			RoutesFile: src.File,
			Count:      unknownIssueCount,
			Err:        fmt.Errorf("no route for prefix %q", prefix),
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
		return nil, &prefixRouteFailure{
			Prefix:      prefix,
			RoutesFile:  src.File,
			RoutePath:   matchedRoute.Path,
			SameAsLocal: true,
			Count:       unknownIssueCount,
			Err:         errors.New("route points to the database already searched"),
		}
	}

	// Check that the target declares a dolt_database
	targetDB := readDoltDatabase(targetBeadsDir)
	if targetDB == "" {
		return nil, &prefixRouteFailure{
			Prefix:     prefix,
			RoutesFile: src.File,
			RoutePath:  matchedRoute.Path,
			Count:      unknownIssueCount,
			Err:        fmt.Errorf("target rig %s has no dolt_database configured", targetBeadsDir),
		}
	}

	debug.Logf("[routing] Prefix %q matched route to %s (database: %s)\n", prefix, matchedRoute.Path, targetDB)

	// We need to temporarily override BEADS_DOLT_SERVER_DATABASE so server-mode
	// stores connect to the correct database on the shared Dolt server.
	origDB := os.Getenv("BEADS_DOLT_SERVER_DATABASE")
	_ = os.Setenv("BEADS_DOLT_SERVER_DATABASE", targetDB)
	var targetStore storage.DoltStorage
	var openErr error
	if writable {
		targetStore, openErr = newDoltStoreFromConfig(ctx, targetBeadsDir)
	} else {
		targetStore, openErr = newReadOnlyStoreFromConfig(ctx, targetBeadsDir)
	}
	// Restore the original env var
	if origDB != "" {
		_ = os.Setenv("BEADS_DOLT_SERVER_DATABASE", origDB)
	} else {
		_ = os.Unsetenv("BEADS_DOLT_SERVER_DATABASE")
	}
	if openErr != nil {
		return nil, &prefixRouteFailure{
			Prefix:     prefix,
			RoutesFile: src.File,
			RoutePath:  matchedRoute.Path,
			TargetDB:   targetDB,
			Count:      unknownIssueCount,
			Err:        fmt.Errorf("opening routed store for %s: %w", matchedRoute.Path, openErr),
		}
	}

	result, err := resolveAndGetFromStore(ctx, targetStore, id, true)
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
		fmt.Fprintf(os.Stderr, "[routing] Resolved %s via prefix route to %s (database: %s)\n", id, matchedRoute.Path, targetDB)
	}

	return result, nil
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

	seen := make(map[string]bool, len(candidates))
	for _, path := range candidates {
		if seen[path] {
			continue
		}
		seen[path] = true
		routes, err := loadPrefixRoutesFile(path)
		if err != nil || len(routes) == 0 {
			continue
		}
		debug.Logf("[routing] Loaded %d prefix route(s) from %s\n", len(routes), path)
		return &prefixRoutesSource{
			File: path,
			// <town>/.beads/routes.jsonl -> <town>
			TownRoot: filepath.Dir(filepath.Dir(path)),
			Routes:   routes,
		}
	}
	return nil
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

// readDoltDatabase reads the dolt_database field from a .beads/metadata.json file.
func readDoltDatabase(beadsDir string) string {
	metadataPath := filepath.Join(beadsDir, "metadata.json")
	data, err := os.ReadFile(metadataPath) //nolint:gosec // G304: path is constructed from trusted beads directory
	if err != nil {
		return ""
	}
	var meta struct {
		DoltDatabase string `json:"dolt_database"`
	}
	if json.Unmarshal(data, &meta) != nil {
		return ""
	}
	return meta.DoltDatabase
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
		}
		prefixErr = routeErr
	}

	// If not found via prefix routing, try contributor auto-routing as fallback (GH#2345).
	// getIssueWithRouting is the read-intent entry point, so the routed store stays
	// read-only here; mutation commands go through resolveAndGetIssueForMutation.
	var autoErr error
	if isNotFoundErr(err) {
		autoResult, routeErr := resolveViaAutoRouting(ctx, localStore, id, false)
		if routeErr == nil {
			return autoResult, nil
		}
		autoErr = routeErr
	}

	return nil, annotateLookupFailure(ctx, localStore, err, prefixErr, autoErr)
}
