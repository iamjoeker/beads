// Regression tests for bd-4sw: prefix routing only worked from the town root.
//
// routes.jsonl lives only in the town's own .beads directory, but routing
// loaded it from whatever beads directory the command had already resolved to
// and derived the town root as that directory's parent. From any rig context
// the resolved directory is the rig's (often redirected) .beads, so routing was
// silently unavailable and every foreign-prefix bead reported "no issue found".
//
// These are pure-Go tests: they exercise route discovery and the failure
// diagnosis, neither of which opens a store. The end-to-end resolution path is
// covered by routed_rig_context_test.go, which needs a real Dolt store.
package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/beads/internal/routing"
	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/types"
)

// countingStore is a minimal issueCounter. The not-found annotation reads a
// store for exactly one thing — how many issues it holds — so the tests can
// supply that without a Dolt server.
type countingStore struct {
	total int
	err   error
}

func (s countingStore) GetStatisticsNoBlocked(context.Context) (*types.Statistics, error) {
	if s.err != nil {
		return nil, s.err
	}
	return &types.Statistics{TotalIssues: s.total}, nil
}

// writeRoutes creates dir/.beads/routes.jsonl with the given lines and returns
// the path to the routes file.
func writeRoutes(t *testing.T, dir string, lines ...string) string {
	t.Helper()
	beadsDir := filepath.Join(dir, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatalf("create %s: %v", beadsDir, err)
	}
	routesPath := filepath.Join(beadsDir, "routes.jsonl")
	if err := os.WriteFile(routesPath, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("write routes.jsonl: %v", err)
	}
	return routesPath
}

// writeBeadsMetadata creates dir/.beads/metadata.json declaring a dolt database
// and returns the .beads directory.
func writeBeadsMetadata(t *testing.T, dir, database string) string {
	t.Helper()
	beadsDir := filepath.Join(dir, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatalf("create %s: %v", beadsDir, err)
	}
	body := `{"dolt_database":"` + database + `"}`
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), []byte(body), 0o600); err != nil {
		t.Fatalf("write metadata.json: %v", err)
	}
	return beadsDir
}

// chdir switches to dir for the duration of the test.
//
// NOTE: callers cannot run in parallel — os.Chdir is process-wide.
func chdir(t *testing.T, dir string) {
	t.Helper()
	oldWd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir %s: %v", dir, err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWd) })
}

// TestFindPrefixRoutesSourceFromRigContext is the core bd-4sw guard: routes
// must be found, and the town root derived correctly, when the command's
// resolved beads directory is a rig's rather than the town's.
//
// NOTE: uses os.Chdir and cannot run in parallel.
func TestFindPrefixRoutesSourceFromRigContext(t *testing.T) {
	town := t.TempDir()
	routesPath := writeRoutes(t, town,
		`{"prefix":"hq-","path":"."}`,
		`{"prefix":"gt-","path":"gastown/mayor/rig"}`,
	)

	rig := filepath.Join(town, "gastown", "mayor", "rig")
	rigBeadsDir := filepath.Join(rig, ".beads")
	if err := os.MkdirAll(rigBeadsDir, 0o755); err != nil {
		t.Fatalf("create rig beads dir: %v", err)
	}
	worktree := filepath.Join(town, "gastown", "polecats", "fury", "gastown")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatalf("create worktree: %v", err)
	}

	cases := []struct {
		name            string
		cwd             string
		currentBeadsDir string
	}{
		{
			// The historical happy path: cwd resolves to the town database.
			name:            "town_root",
			cwd:             town,
			currentBeadsDir: filepath.Join(town, ".beads"),
		},
		{
			// The reported failure: a rig worktree whose .beads redirects to
			// the rig's database, which carries no routes.jsonl.
			name:            "rig_worktree_redirected_to_rig_db",
			cwd:             worktree,
			currentBeadsDir: rigBeadsDir,
		},
		{
			// An explicit --db against a rig database while cwd is elsewhere
			// entirely: discovery must still reach the town via the resolved
			// beads directory's ancestors.
			name:            "cwd_outside_town",
			cwd:             t.TempDir(),
			currentBeadsDir: rigBeadsDir,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			chdir(t, tc.cwd)

			src := findPrefixRoutesSource(tc.currentBeadsDir)
			if src == nil {
				t.Fatal("findPrefixRoutesSource returned nil; prefix routing is unavailable outside the town root (bd-4sw)")
			}
			if src.File != routesPath {
				t.Errorf("routes file = %q, want %q", src.File, routesPath)
			}
			// The town root must come from where routes.jsonl actually was,
			// not from the resolved beads directory's parent.
			if src.TownRoot != town {
				t.Errorf("town root = %q, want %q", src.TownRoot, town)
			}
			if len(src.Routes) != 2 {
				t.Fatalf("loaded %d routes, want 2", len(src.Routes))
			}
			if src.Routes[0].Prefix != "hq-" || src.Routes[0].Path != "." {
				t.Errorf("first route = %+v, want {hq- .}", src.Routes[0])
			}
		})
	}
}

// TestFindPrefixRoutesSourceNearestWins pins the search order: a nested town's
// own routes.jsonl must win over an outer one, so discovery cannot silently
// re-anchor relative route paths to the wrong root.
//
// NOTE: uses os.Chdir and cannot run in parallel.
func TestFindPrefixRoutesSourceNearestWins(t *testing.T) {
	outer := t.TempDir()
	writeRoutes(t, outer, `{"prefix":"hq-","path":"."}`)

	inner := filepath.Join(outer, "nested")
	innerRoutes := writeRoutes(t, inner, `{"prefix":"zz-","path":"rig"}`)

	chdir(t, inner)

	src := findPrefixRoutesSource("")
	if src == nil {
		t.Fatal("findPrefixRoutesSource returned nil")
	}
	if src.File != innerRoutes {
		t.Errorf("routes file = %q, want the nearest one %q", src.File, innerRoutes)
	}
	if src.TownRoot != inner {
		t.Errorf("town root = %q, want %q", src.TownRoot, inner)
	}
}

// TestFindPrefixRoutesSourceNoRoutes covers the ordinary single-repo case:
// no routes.jsonl anywhere means no routing, not an error.
//
// NOTE: uses os.Chdir and cannot run in parallel.
func TestFindPrefixRoutesSourceNoRoutes(t *testing.T) {
	repo := t.TempDir()
	beadsDir := filepath.Join(repo, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatalf("create beads dir: %v", err)
	}
	chdir(t, repo)

	if src := findPrefixRoutesSource(beadsDir); src != nil {
		t.Errorf("findPrefixRoutesSource = %+v, want nil when no routes.jsonl exists", src)
	}
}

func TestSameResolvedDir(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	real := filepath.Join(root, "real")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatalf("create dir: %v", err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	cases := []struct {
		name string
		a, b string
		want bool
	}{
		{"identical", real, real, true},
		{"unclean_but_equal", real, filepath.Join(real, "sub", ".."), true},
		{"through_symlink", link, real, true},
		{"different", real, root, false},
		{"empty_left", "", real, false},
		{"empty_right", real, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sameResolvedDir(tc.a, tc.b); got != tc.want {
				t.Errorf("sameResolvedDir(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

// TestAnnotateLookupFailure is the guard for the second half of bd-4sw: an
// unroutable prefix and an absent bead must not print the same string. A bare
// "no issue found" was read as proof that live beads had been destroyed.
//
// NOTE: mutates the dbPath global and cannot run in parallel.
func TestAnnotateLookupFailure(t *testing.T) {
	town := t.TempDir()
	rig := filepath.Join(town, "rig")
	rigBeadsDir := writeBeadsMetadata(t, rig, "rigdb")

	oldDBPath := dbPath
	dbPath = filepath.Join(rigBeadsDir, "dolt")
	t.Cleanup(func() { dbPath = oldDBPath })

	notFound := errors.New(`no issue found matching "hq-abc"`)
	// The contributor-routed target declares its own database, so the failure
	// can name it the same way it names the local one.
	planningDir := writeBeadsMetadata(t, filepath.Join(town, "planning"), "planningdb")

	cases := []struct {
		name       string
		err        error
		localStore issueCounter
		prefixErr  error
		autoErr    error
		wantAll    []string
		wantNone   []string
	}{
		{
			// The damaging case: the routed database was searched and really
			// is missing the bead. Say which databases answered.
			name:       "routed_and_searched",
			err:        notFound,
			localStore: countingStore{total: 1523},
			prefixErr: &prefixRouteFailure{
				Prefix: "hq-", RoutesFile: filepath.Join(town, ".beads", "routes.jsonl"),
				RoutePath: ".", TargetDB: "hq", Searched: true, Count: 412,
				Err: notFound,
			},
			wantAll: []string{
				`no issue found matching "hq-abc"`,
				`database "rigdb"`,
				rigBeadsDir,
				"holding 1523 issue(s)",
				`prefix "hq-"`,
				"the town root",
				`database "hq"`,
				"holding 412 issue(s)",
				"also searched",
			},
		},
		{
			name:       "prefix_has_no_route",
			err:        notFound,
			localStore: countingStore{total: 3},
			prefixErr: &prefixRouteFailure{
				Prefix: "hq-", RoutesFile: filepath.Join(town, ".beads", "routes.jsonl"),
				Count: unknownIssueCount,
				Err:   errors.New(`no route for prefix "hq-"`),
			},
			wantAll: []string{
				`database "rigdb"`,
				`prefix "hq-" has no route in`,
				filepath.Join(town, ".beads", "routes.jsonl"),
			},
		},
		{
			// The route resolves back to the database already named as
			// searched: naming it twice is noise, not information.
			name:       "route_is_the_local_database",
			err:        notFound,
			localStore: countingStore{total: 3},
			prefixErr: &prefixRouteFailure{
				Prefix: "rig-", RoutesFile: filepath.Join(town, ".beads", "routes.jsonl"),
				RoutePath: "rig", SameAsLocal: true, Count: unknownIssueCount,
				Err: errors.New("route points to the database already searched"),
			},
			wantAll:  []string{`database "rigdb"`},
			wantNone: []string{"routes to"},
		},
		{
			// Ordinary single-repo use: no routes.jsonl at all is not a
			// routing problem and must not be reported as one.
			name:       "no_routes_file",
			err:        notFound,
			localStore: countingStore{total: 3},
			prefixErr:  &prefixRouteFailure{Prefix: "hq-", Count: unknownIssueCount, Err: errors.New("no routes.jsonl found")},
			wantAll:    []string{`database "rigdb"`},
			wantNone:   []string{"routes.jsonl", "prefix"},
		},
		{
			// bd-1uu proper: contributor routing sent the lookup to a planning
			// store that does not carry these ids. The mechanism, the
			// destination, its (zero) count and the fix are all things bd
			// already knew and used to discard.
			name:       "contributor_route_also_searched",
			err:        notFound,
			localStore: countingStore{total: 15},
			autoErr: &autoRouteFailure{
				Rule: routing.RuleContributor, BeadsDir: planningDir,
				Searched: true, Count: 0, Err: notFound,
			},
			wantAll: []string{
				"holding 15 issue(s)",
				"contributor routing",
				"beads.role=contributor",
				`database "planningdb"`,
				planningDir,
				"holding 0 issue(s)",
				"fix: git config beads.role maintainer",
			},
		},
		{
			// A routed target that could not be opened is a different answer
			// from one that was searched and came up empty; saying "also
			// searched" would be a lie about what the store reported.
			name:       "contributor_route_unopenable",
			err:        notFound,
			localStore: countingStore{total: 15},
			autoErr: &autoRouteFailure{
				Rule: routing.RuleMaintainer, BeadsDir: planningDir,
				Count: unknownIssueCount,
				Err:   errors.New("failed to open routed store: connection refused"),
			},
			wantAll: []string{
				planningDir,
				"could not be searched",
				"connection refused",
				"fix: bd config unset routing.maintainer",
			},
			wantNone: []string{"also searched", "holding 0 issue(s)"},
		},
		{
			// No routing rule applies at all: there is no second store, so
			// there is nothing to disclose and the message stays short.
			name:       "no_auto_routing_configured",
			err:        notFound,
			localStore: countingStore{total: 15},
			autoErr:    &autoRouteFailure{Count: unknownIssueCount, Err: errors.New("no auto-routed store configured")},
			wantAll:    []string{`database "rigdb"`},
			wantNone:   []string{"routing", "fix:", "auto-routed"},
		},
		{
			// The count is an optional positive control, not a precondition:
			// a store that cannot be counted still gets named, and an
			// unknown count must never render as a zero.
			name:       "count_unavailable",
			err:        notFound,
			localStore: countingStore{err: errors.New("connection refused")},
			wantAll:    []string{`database "rigdb"`, rigBeadsDir},
			wantNone:   []string{"holding"},
		},
		{
			// No store handle at all (callers that annotate before opening
			// one). Same rule: name the database, omit the count.
			name:     "no_local_store",
			err:      notFound,
			wantAll:  []string{`database "rigdb"`},
			wantNone: []string{"holding"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := annotateLookupFailure(context.Background(), tc.localStore, tc.err, tc.prefixErr, tc.autoErr)
			msg := got.Error()
			for _, want := range tc.wantAll {
				if !strings.Contains(msg, want) {
					t.Errorf("message missing %q:\n%s", want, msg)
				}
			}
			for _, unwanted := range tc.wantNone {
				if strings.Contains(msg, unwanted) {
					t.Errorf("message should not mention %q:\n%s", unwanted, msg)
				}
			}
			// The annotation must not break not-found detection: isNotFoundErr
			// and the protocol §E4 error-class contract both match on the
			// original wording.
			if !isNotFoundErr(got) {
				t.Errorf("annotated error no longer reads as not-found:\n%s", msg)
			}
		})
	}
}

// TestAnnotateLookupFailurePassesThroughOtherErrors keeps the annotation
// confined to the not-found class; a connection failure must not be dressed up
// as a routing answer.
func TestAnnotateLookupFailurePassesThroughOtherErrors(t *testing.T) {
	t.Parallel()

	boom := errors.New("dial tcp 127.0.0.1:3307: connection refused")
	if got := annotateLookupFailure(context.Background(), countingStore{total: 5}, boom, &prefixRouteFailure{Prefix: "hq-"}, nil); got != boom {
		t.Errorf("annotateLookupFailure rewrote a non-not-found error: %v", got)
	}
}

// TestAnnotateLookupFailurePreservesErrNotFound pins the wrapping contract:
// callers branch on errors.Is(err, storage.ErrNotFound).
func TestAnnotateLookupFailurePreservesErrNotFound(t *testing.T) {
	t.Parallel()

	got := annotateLookupFailure(context.Background(), nil, storage.ErrNotFound, nil, nil)
	if !errors.Is(got, storage.ErrNotFound) {
		t.Errorf("annotated error lost storage.ErrNotFound: %v", got)
	}
}
