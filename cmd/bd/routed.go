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
	if isNotFoundErr(err) {
		if autoResult, autoErr := resolveViaAutoRouting(ctx, localStore, id, writablePrefixRoute); autoErr == nil {
			return autoResult, nil
		}
	}

	return nil, annotateLookupFailure(err, prefixErr)
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
func resolveViaAutoRouting(ctx context.Context, localStore storage.DoltStorage, id string, writable bool) (*RoutedResult, error) {
	routedStore, routed, _, err := openRoutedStore(ctx, localStore, writable)
	if err != nil || !routed {
		return nil, fmt.Errorf("no auto-routed store available")
	}

	result, err := resolveAndGetFromStore(ctx, routedStore, id, true)
	if err != nil {
		_ = routedStore.Close()
		return nil, err
	}
	result.closeFn = func() { _ = routedStore.Close() }
	return result, nil
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
		return fmt.Sprintf("prefix %q routes to %s (database %q), which was also searched",
			f.Prefix, f.describeTarget(), f.TargetDB)
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
// and, when the ID's prefix routes somewhere else, says where (bd-4sw). Other
// errors pass through untouched.
//
// The original error is wrapped, so errors.Is(storage.ErrNotFound) and the
// "no issue found matching" text that isNotFoundErr and the protocol contract
// match on both still hold.
func annotateLookupFailure(err error, prefixErr error) error {
	if !isNotFoundErr(err) {
		return err
	}
	var parts []string
	if where := describeLocalDatabase(); where != "" {
		parts = append(parts, "searched "+where)
	}
	var routeFailure *prefixRouteFailure
	if errors.As(prefixErr, &routeFailure) {
		if explanation := routeFailure.explain(); explanation != "" {
			parts = append(parts, explanation)
		}
	}
	if len(parts) == 0 {
		return err
	}
	return fmt.Errorf("%w (%s)", err, strings.Join(parts, "; "))
}

// describeLocalDatabase names the database the command opened, so a not-found
// answer says which database it is an answer about.
func describeLocalDatabase() string {
	beadsDir := resolveCommandBeadsDir(dbPath)
	if beadsDir == "" {
		return ""
	}
	if database := readDoltDatabase(beadsDir); database != "" {
		return fmt.Sprintf("database %q at %s", database, beadsDir)
	}
	return beadsDir
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
			Err:        fmt.Errorf("opening routed store for %s: %w", matchedRoute.Path, openErr),
		}
	}

	result, err := resolveAndGetFromStore(ctx, targetStore, id, true)
	if err != nil {
		_ = targetStore.Close()
		return nil, &prefixRouteFailure{
			Prefix:     prefix,
			RoutesFile: src.File,
			RoutePath:  matchedRoute.Path,
			TargetDB:   targetDB,
			Searched:   isNotFoundErr(err),
			Err:        err,
		}
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
	if isNotFoundErr(err) {
		if autoResult, autoErr := resolveViaAutoRouting(ctx, localStore, id, false); autoErr == nil {
			return autoResult, nil
		}
	}

	return nil, annotateLookupFailure(err, prefixErr)
}
