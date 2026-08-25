//go:build cgo

// Cgo-only test helpers for cmd/bd. Helpers in this file pull in
// internal/storage/dolt, database/sql, and the embedded Dolt server, which
// require cgo to link. Pure-Go-compatible helpers (captureStdout,
// stdioMutex, runCommandInDir, etc.) live in test_helpers_pure_test.go and
// are intentionally untagged so non-cgo tests in this package compile under
// CGO_ENABLED=0 with the gms_pure_go build tag.

package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/beads/internal/configfile"
	"github.com/steveyegge/beads/internal/doltserver"
	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/storage/dolt"
	"github.com/steveyegge/beads/internal/storage/doltutil"
	"github.com/steveyegge/beads/internal/testutil"
)

// testDoltServerPort is the port of the shared test Dolt server (0 = not running).
var testDoltServerPort int

// testStoreOpenTimeout bounds dolt.New/dolt.NewFromConfig calls in these
// helpers. The testcontainers wait strategy only confirms the TCP listener
// is up, not that Dolt's SQL engine can serve queries yet (see
// waitForDoltReady in internal/testutil/testdoltserver.go for the same class
// of race on shared-container startup). A query issued with an unbounded
// context in that narrow window can block indefinitely instead of erroring,
// because nothing ever cancels it to unstick the read — confirmed via
// goroutine dump during be-fgd round-2 triage: a per-test dolt.New() sat in
// mysqlConn.readWithTimeout / net.Read for 2+ minutes after the container
// had already logged ready.
//
// The fallback branch (per-test DB, exercised only when shared-schema init
// in test_dolt_server_cgo_test.go fails) runs a full v0->v65 migration
// replay from scratch, which is genuinely slow, not hung: a single isolated
// measurement (BEADS_TEST_FORCE_FALLBACK_DIAG diagnostic, reverted after
// use) clocked one such replay at 219.62s on this hardware. 60s (an earlier
// calibration attempt, matching contributor_routing_e2e_test.go's precedent)
// measurably undershot this and produced a clean "context deadline exceeded"
// at exactly 60.01s instead of the real result. 300s gives that observed
// value ~37% margin for run-to-run variance while still failing fast
// relative to an actually-stuck connection. In normal operation the shared
// fast path (newTestStoreSharedBranch) avoids this branch entirely and
// resolves in under a second; this bound only matters for the rare/backstop
// fallback case.
const testStoreOpenTimeout = 300 * time.Second

// writeTestMetadata writes metadata.json in the .beads directory (parent of dbPath)
// so that NewFromConfig can find the correct database name and server settings when
// routing reopens a store by path.
func writeTestMetadata(t *testing.T, dbPath string, database string) {
	t.Helper()
	beadsDir := filepath.Dir(dbPath)
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatalf("Failed to create beads dir: %v", err)
	}
	cfg := &configfile.Config{
		Database:       "dolt",
		Backend:        configfile.BackendDolt,
		DoltMode:       configfile.DoltModeServer,
		DoltDatabase:   database,
		DoltServerHost: "127.0.0.1",
		DoltServerPort: testDoltServerPort,
	}
	if err := cfg.Save(beadsDir); err != nil {
		t.Fatalf("Failed to write test metadata.json: %v", err)
	}
}

// newTestStore creates a dolt store with issue_prefix configured (bd-166).
// Uses shared database with branch-per-test isolation (bd-xmf) to avoid
// the overhead of CREATE/DROP DATABASE per test.
// Falls back to per-test databases if the shared DB is not available.
func newTestStore(t *testing.T, dbPath string) *dolt.DoltStore {
	t.Helper()
	return newTestStoreWithPrefix(t, dbPath, "test")
}

// newTestStoreIsolatedDB creates a dolt store with its own dedicated database.
// Use this instead of newTestStoreWithPrefix when the test needs a truly separate
// database (e.g., routing tests that create multiple stores with different paths
// and expect routing to reopen them by path via metadata.json).
func newTestStoreIsolatedDB(t *testing.T, dbPath string, prefix string) *dolt.DoltStore {
	t.Helper()

	ensureTestMode(t)

	if testDoltServerPort == 0 {
		t.Skip("Dolt test server not available, skipping")
	}
	if testutil.DoltContainerCrashed() {
		t.Skipf("Dolt test server crashed: %v", testutil.DoltContainerCrashError())
	}

	ctx, cancel := context.WithTimeout(context.Background(), testStoreOpenTimeout)
	defer cancel()

	cfg := &dolt.Config{
		Path:            dbPath,
		ServerHost:      "127.0.0.1",
		ServerPort:      testDoltServerPort,
		Database:        uniqueTestDBName(t),
		CreateIfMissing: true,
	}
	writeTestMetadata(t, dbPath, cfg.Database)

	doltNewMutex.Lock()
	s, err := dolt.New(ctx, cfg)
	doltNewMutex.Unlock()
	if err != nil {
		t.Fatalf("Failed to create dolt store: %v", err)
	}

	if err := s.SetConfig(ctx, "issue_prefix", prefix); err != nil {
		s.Close()
		t.Fatalf("Failed to set issue_prefix: %v", err)
	}
	if err := s.SetConfig(ctx, "types.custom", "molecule,gate,convoy,merge-request,slot,agent,role,rig,event,message"); err != nil {
		s.Close()
		t.Fatalf("Failed to set types.custom: %v", err)
	}

	t.Cleanup(func() {
		s.Close()
		if cfg.Database != "" {
			dropTestDatabase(cfg.Database, testDoltServerPort)
		}
	})
	return s
}

// newTestStoreWithPrefix creates a dolt store with custom issue_prefix configured.
// Uses shared database with branch-per-test isolation (bd-xmf) when available,
// falling back to per-test databases otherwise.
func newTestStoreWithPrefix(t *testing.T, dbPath string, prefix string) *dolt.DoltStore {
	t.Helper()

	ensureTestMode(t)

	if testDoltServerPort == 0 {
		t.Skip("Dolt test server not available, skipping")
	}
	if testutil.DoltContainerCrashed() {
		t.Skipf("Dolt test server crashed: %v", testutil.DoltContainerCrashError())
	}

	// Fast path: use shared DB with branch-per-test isolation (bd-xmf)
	if testSharedDB != "" {
		return newTestStoreSharedBranch(t, dbPath, prefix)
	}

	// Fallback: per-test database (original slow path)
	ctx, cancel := context.WithTimeout(context.Background(), testStoreOpenTimeout)
	defer cancel()

	cfg := &dolt.Config{
		Path:            dbPath,
		ServerHost:      "127.0.0.1",
		ServerPort:      testDoltServerPort,
		Database:        uniqueTestDBName(t),
		CreateIfMissing: true,
	}
	writeTestMetadata(t, dbPath, cfg.Database)

	doltNewMutex.Lock()
	s, err := dolt.New(ctx, cfg)
	doltNewMutex.Unlock()
	if err != nil {
		t.Fatalf("Failed to create dolt store: %v", err)
	}

	if err := s.SetConfig(ctx, "issue_prefix", prefix); err != nil {
		s.Close()
		t.Fatalf("Failed to set issue_prefix: %v", err)
	}
	if err := s.SetConfig(ctx, "types.custom", "molecule,gate,convoy,merge-request,slot,agent,role,rig,event,message"); err != nil {
		s.Close()
		t.Fatalf("Failed to set types.custom: %v", err)
	}

	t.Cleanup(func() {
		s.Close()
		if cfg.Database != "" {
			dropTestDatabase(cfg.Database, testDoltServerPort)
		}
	})
	return s
}

// newTestStoreSharedBranch creates a store using the shared database with
// branch-per-test isolation. Each test gets its own Dolt branch, avoiding
// the expensive CREATE DATABASE + schema init + DROP DATABASE + PURGE cycle.
func newTestStoreSharedBranch(t *testing.T, dbPath string, prefix string) *dolt.DoltStore {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testStoreOpenTimeout)
	defer cancel()

	// Write metadata.json pointing to the shared database
	writeTestMetadata(t, dbPath, testSharedDB)

	// Open store against the shared database with MaxOpenConns=1
	// (required for DOLT_CHECKOUT session affinity)
	doltNewMutex.Lock()
	s, err := dolt.New(ctx, &dolt.Config{
		Path:         dbPath,
		ServerHost:   "127.0.0.1",
		ServerPort:   testDoltServerPort,
		Database:     testSharedDB,
		MaxOpenConns: 1,
	})
	doltNewMutex.Unlock()
	if err != nil {
		t.Fatalf("Failed to create dolt store (shared): %v", err)
	}

	// Create isolated branch for this test
	_, branchCleanup := testutil.StartTestBranch(t, s.DB(), testSharedDB)

	// Set prefix for this test (overrides the shared schema's default)
	if err := s.SetConfig(ctx, "issue_prefix", prefix); err != nil {
		branchCleanup()
		s.Close()
		t.Fatalf("Failed to set issue_prefix: %v", err)
	}

	t.Cleanup(func() {
		branchCleanup()
		s.Close()
	})
	return s
}

// newTestStoreSharedBranchWithReadTimeout is newTestStoreSharedBranch with a
// caller-specified Config.PoolReadTimeout. Existing shared-branch callers
// stay on the 10s default (defaultPoolReadTimeout in internal/storage/dolt);
// this is for the rare test whose own write volume — not server health —
// needs more slack, e.g. a bulk seed of thousands of rows over the single
// held connection that MaxOpenConns=1 forces on this path (be-uoat round 2).
func newTestStoreSharedBranchWithReadTimeout(t *testing.T, dbPath string, prefix string, readTimeout time.Duration) *dolt.DoltStore {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testStoreOpenTimeout)
	defer cancel()

	// Write metadata.json pointing to the shared database
	writeTestMetadata(t, dbPath, testSharedDB)

	// Open store against the shared database with MaxOpenConns=1
	// (required for DOLT_CHECKOUT session affinity)
	doltNewMutex.Lock()
	s, err := dolt.New(ctx, &dolt.Config{
		Path:            dbPath,
		ServerHost:      "127.0.0.1",
		ServerPort:      testDoltServerPort,
		Database:        testSharedDB,
		MaxOpenConns:    1,
		PoolReadTimeout: readTimeout,
	})
	doltNewMutex.Unlock()
	if err != nil {
		t.Fatalf("Failed to create dolt store (shared): %v", err)
	}

	// Create isolated branch for this test
	_, branchCleanup := testutil.StartTestBranch(t, s.DB(), testSharedDB)

	// Set prefix for this test (overrides the shared schema's default)
	if err := s.SetConfig(ctx, "issue_prefix", prefix); err != nil {
		branchCleanup()
		s.Close()
		t.Fatalf("Failed to set issue_prefix: %v", err)
	}

	t.Cleanup(func() {
		branchCleanup()
		s.Close()
	})
	return s
}

// newTestStoreWithPrefixAndReadTimeout is newTestStoreWithPrefix with a
// caller-specified Config.PoolReadTimeout, threaded through whichever branch
// (shared-DB fast path or per-test-DB fallback) actually runs. See
// newTestStoreSharedBranchWithReadTimeout for why this knob exists.
func newTestStoreWithPrefixAndReadTimeout(t *testing.T, dbPath string, prefix string, readTimeout time.Duration) *dolt.DoltStore {
	t.Helper()

	ensureTestMode(t)

	if testDoltServerPort == 0 {
		t.Skip("Dolt test server not available, skipping")
	}
	if testutil.DoltContainerCrashed() {
		t.Skipf("Dolt test server crashed: %v", testutil.DoltContainerCrashError())
	}

	// Fast path: use shared DB with branch-per-test isolation (bd-xmf)
	if testSharedDB != "" {
		return newTestStoreSharedBranchWithReadTimeout(t, dbPath, prefix, readTimeout)
	}

	// Fallback: per-test database (original slow path)
	ctx, cancel := context.WithTimeout(context.Background(), testStoreOpenTimeout)
	defer cancel()

	cfg := &dolt.Config{
		Path:            dbPath,
		ServerHost:      "127.0.0.1",
		ServerPort:      testDoltServerPort,
		Database:        uniqueTestDBName(t),
		CreateIfMissing: true,
		PoolReadTimeout: readTimeout,
	}
	writeTestMetadata(t, dbPath, cfg.Database)

	doltNewMutex.Lock()
	s, err := dolt.New(ctx, cfg)
	doltNewMutex.Unlock()
	if err != nil {
		t.Fatalf("Failed to create dolt store: %v", err)
	}

	if err := s.SetConfig(ctx, "issue_prefix", prefix); err != nil {
		s.Close()
		t.Fatalf("Failed to set issue_prefix: %v", err)
	}
	if err := s.SetConfig(ctx, "types.custom", "molecule,gate,convoy,merge-request,slot,agent,role,rig,event,message"); err != nil {
		s.Close()
		t.Fatalf("Failed to set types.custom: %v", err)
	}

	t.Cleanup(func() {
		s.Close()
		if cfg.Database != "" {
			dropTestDatabase(cfg.Database, testDoltServerPort)
		}
	})
	return s
}

// tryNewTestStoreWithReadTimeout is the dolt.New portion of
// newTestStoreWithPrefixAndReadTimeout (same branching, same Config shape),
// returning the error directly instead of calling t.Fatal on it. Use this
// when the caller expects the open itself to fail — e.g. proving a short
// PoolReadTimeout is actually honored — since a t.Run subtest's failure
// always propagates FAIL to the parent test and the process exit code in
// Go's testing package regardless of what the parent does with t.Run's bool
// return; there is no way to observe an "expected" subtest failure without
// also failing the overall run.
func tryNewTestStoreWithReadTimeout(t *testing.T, dbPath string, readTimeout time.Duration) (*dolt.DoltStore, error) {
	t.Helper()

	ensureTestMode(t)

	if testDoltServerPort == 0 {
		t.Skip("Dolt test server not available, skipping")
	}
	if testutil.DoltContainerCrashed() {
		t.Skipf("Dolt test server crashed: %v", testutil.DoltContainerCrashError())
	}

	ctx, cancel := context.WithTimeout(context.Background(), testStoreOpenTimeout)
	defer cancel()

	// Fast path: use shared DB with branch-per-test isolation (bd-xmf)
	if testSharedDB != "" {
		writeTestMetadata(t, dbPath, testSharedDB)
		doltNewMutex.Lock()
		s, err := dolt.New(ctx, &dolt.Config{
			Path:            dbPath,
			ServerHost:      "127.0.0.1",
			ServerPort:      testDoltServerPort,
			Database:        testSharedDB,
			MaxOpenConns:    1,
			PoolReadTimeout: readTimeout,
		})
		doltNewMutex.Unlock()
		return s, err
	}

	// Fallback: per-test database (original slow path)
	cfg := &dolt.Config{
		Path:            dbPath,
		ServerHost:      "127.0.0.1",
		ServerPort:      testDoltServerPort,
		Database:        uniqueTestDBName(t),
		CreateIfMissing: true,
		PoolReadTimeout: readTimeout,
	}
	writeTestMetadata(t, dbPath, cfg.Database)

	doltNewMutex.Lock()
	s, err := dolt.New(ctx, cfg)
	doltNewMutex.Unlock()
	return s, err
}

// dropTestDatabase drops a test database from the shared server (best-effort cleanup).
func dropTestDatabase(dbName string, port int) {
	dsn := doltutil.ServerDSN{Host: "127.0.0.1", Port: port, User: "root"}.String()
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	//nolint:gosec // G201: dbName is generated by uniqueTestDBName (testdb_ + random hex)
	_, _ = db.ExecContext(ctx, fmt.Sprintf("DROP DATABASE IF EXISTS `%s`", dbName))
	// Purge dropped databases from Dolt's trash directory to reclaim disk space
	_, _ = db.ExecContext(ctx, "CALL dolt_purge_dropped_databases()")
}

// sharedServerTestEnv returns the process environment plus shared-server
// settings for a subprocess `bd`, isolated to this test on two axes.
//
// Port: shared-server mode with no configured port always resolves to the
// fixed DefaultSharedServerPort (3308) and `bd init` starts that server
// itself, so a test relying on the default passes only while nothing else on
// the box holds 3308. When something does -- another rig's Dolt server, a
// second suite running in parallel -- init fails with "cannot start dolt
// server on port 3308: port 3308 is in use", naming a process that has
// nothing to do with the test (bd-uh0).
//
// Server directory: the shared server's state otherwise lives under HOME,
// which every subprocess in this package shares (TestMain pins one temp HOME
// for the whole run). Two shared-server tests in one process then inherit
// each other's port file and the second one dies on "server configured at
// port N is unreachable; auto-start started a new server on port M". The
// directory is allocated under testTempRoot so the suite's orphan-server
// sweep still reaps anything left listening.
func sharedServerTestEnv(t *testing.T) []string {
	t.Helper()
	return append(sharedServerPortOnlyTestEnv(t), "BEADS_DOLT_SHARED_SERVER=1")
}

// sharedServerPortOnlyTestEnv is sharedServerTestEnv without the mode switch:
// the isolated port and server directory, but no BEADS_DOLT_SHARED_SERVER.
//
// Use it for a test whose subject IS how shared-server mode gets selected --
// `bd init --shared-server`. Exporting the env flag there would satisfy the
// assertion no matter what the flag did, so the test needs the isolation
// without the answer.
func sharedServerPortOnlyTestEnv(t *testing.T) []string {
	t.Helper()
	port, err := testutil.FindFreePort()
	if err != nil {
		t.Fatalf("find free port for shared server: %v", err)
	}
	serverDir, err := testTempDir("shared-server-*")
	if err != nil {
		t.Fatalf("create shared server dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(serverDir) })
	return append(os.Environ(),
		"BEADS_SHARED_SERVER_DIR="+serverDir,
		"BEADS_DOLT_SERVER_PORT="+strconv.Itoa(port),
	)
}

// openExistingTestDB reopens an existing Dolt store for verification in tests.
// It tries NewFromConfig first (reads metadata.json for correct database name),
// then falls back to direct open for BEADS_DB or other non-standard paths.
//
// Server-mode workspaces only: dolt.New always builds a client for a dolt
// sql-server, so this cannot read back the DEFAULT `bd init`, which is
// embedded. Use openWorkspaceStoreForTest for a workspace whose mode is
// whatever init chose.
func openExistingTestDB(t *testing.T, dbPath string) (*dolt.DoltStore, error) {
	t.Helper()
	// Serialize dolt.New() to avoid race in Dolt's InitStatusVariables (bd-cqjoi)
	doltNewMutex.Lock()
	defer doltNewMutex.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), testStoreOpenTimeout)
	defer cancel()
	// Try NewFromConfig which reads metadata.json for correct database name
	beadsDir := filepath.Dir(dbPath)
	if store, err := dolt.NewFromConfig(ctx, beadsDir); err == nil {
		return store, nil
	}
	// Fallback: open directly with test server config
	cfg := &dolt.Config{Path: dbPath}
	if testDoltServerPort != 0 {
		cfg.ServerHost = "127.0.0.1"
		cfg.ServerPort = testDoltServerPort
	}
	return dolt.New(ctx, cfg)
}

// openWorkspaceStoreForTest reopens the workspace at beadsDir through the same
// mode-dispatching factory the CLI uses, so the store it returns matches the
// mode `bd init` actually wrote to metadata.json.
//
// Tests here used to reopen with dolt.NewFromConfig, which is server-only:
// dolt.New ends in newServerMode unconditionally, and the embedded engine
// lives in a different package. Since plain `bd init` became embedded, that
// route could not read back its own workspace, and this suite made the
// mismatch loud rather than absent -- cmd/bd's TestMain exports BEADS_DOLT_PORT
// process-wide for the test container, so the doomed open resolved to the
// container and failed with `database "..." not found on Dolt server at
// 127.0.0.1:<port>` instead of an error naming the mode confusion (bd-kbx).
func openWorkspaceStoreForTest(t *testing.T, beadsDir string) (storage.DoltStorage, error) {
	t.Helper()
	// Serialize store construction to avoid a race in Dolt's
	// InitStatusVariables (bd-cqjoi); the embedded engine shares it.
	doltNewMutex.Lock()
	defer doltNewMutex.Unlock()
	return newDoltStoreFromConfig(context.Background(), beadsDir)
}

// initDataRootForTest returns the directory the workspace's own metadata says
// its database lives in, so a test can assert `bd init` created a database
// without hardcoding one mode's layout.
//
// The layout is not a constant: embedded init writes .beads/embeddeddolt,
// server-layout workspaces use .beads/dolt (or dolt_data_dir /
// BEADS_DOLT_DATA_DIR), and shared-server mode puts it under the shared
// server directory entirely. Hardcoded `.beads/dolt` assertions rotted into
// permanent failures when plain init became embedded, and only surfaced when
// Dolt actually ran (bd-kbx). ResolvePhysicalRoots is the resolver the gate
// wiring already trusts for the same question.
func initDataRootForTest(t *testing.T, beadsDir string) string {
	t.Helper()
	pr, err := doltserver.ResolvePhysicalRoots(beadsDir)
	if err != nil {
		t.Fatalf("resolve physical roots for %s: %v", beadsDir, err)
	}
	if len(pr.Roots) != 1 {
		t.Fatalf("expected exactly one physical root for %s, got %v (mode %q, %s)",
			beadsDir, pr.Roots, pr.Mode, pr.Provenance)
	}
	return pr.Roots[0]
}

// requireInitDataRoot asserts that `bd init` created the workspace's database
// directory, wherever the workspace's mode puts it, and returns that path.
func requireInitDataRoot(t *testing.T, beadsDir string) string {
	t.Helper()
	root := initDataRootForTest(t, beadsDir)
	info, err := os.Stat(root)
	if err != nil {
		t.Errorf("Dolt database directory was not created at %s: %v", root, err)
		return root
	}
	if !info.IsDir() {
		t.Errorf("expected %s to be a directory", root)
	}
	return root
}

// seedCurrentEraServerWorkspace makes beadsDir look like a server workspace
// that THIS bd version created: metadata.json naming server mode and the
// database, plus the local version witness.
//
// Tests that need an existing `.beads/dolt/` data directory have to say that
// much. A bare `.beads/dolt/` with no metadata and no version witness is
// precisely the pre-0.56 shape guardLegacyUpgradeWorkspace refuses, and it
// cannot tell a hand-built fixture from a genuine cross-era workspace --
// refusing is the reviewed behaviour there, so the fixture is what has to be
// specific. These tests predate the guard and had been failing with "legacy
// Dolt workspace detected" ever since (bd-kbx).
func seedCurrentEraServerWorkspace(t *testing.T, beadsDir, database string) {
	t.Helper()
	if err := os.MkdirAll(beadsDir, 0o700); err != nil {
		t.Fatalf("creating beads dir: %v", err)
	}
	cfg := configfile.DefaultConfig()
	cfg.Backend = configfile.BackendDolt
	cfg.DoltMode = configfile.DoltModeServer
	cfg.DoltDatabase = database
	cfg.DoltServerHost = "127.0.0.1"
	cfg.DoltServerPort = testDoltServerPort
	if err := cfg.Save(beadsDir); err != nil {
		t.Fatalf("writing metadata.json: %v", err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, localVersionFile), []byte(Version), 0o600); err != nil {
		t.Fatalf("writing version witness: %v", err)
	}
}

// requireDefaultInitMode asserts the workspace was written in init's default
// storage mode, embedded.
//
// It exists to pin down what `--database` does NOT do: it names the database,
// it does not select a mode. Mode comes from --server / --shared-server /
// --proxied-server, and init's own hyphen check on --database is conditioned
// on the resolved mode being embedded, so a `--database`-only init is
// embedded by construction. The tests here asserted DoltMode "server" from
// the era when --database meant "an existing server database", and had been
// failing ever since the default flipped (bd-kbx).
func requireDefaultInitMode(t *testing.T, cfg *configfile.Config) {
	t.Helper()
	if got := cfg.GetDoltMode(); got != configfile.DoltModeEmbedded {
		t.Errorf("Expected DoltMode %q (--database names the database, it does not select a mode), got %q",
			configfile.DoltModeEmbedded, got)
	}
}

// requireNoInitDataRoot asserts that `bd init` did NOT put a database under
// beadsDir. Unlike the positive assertion this cannot ask the resolver, which
// answers with the path a database WOULD occupy; it checks every layout an
// init could have produced there, so the negative does not go quietly stale
// the way the hardcoded `.beads/dolt` positives did (bd-kbx).
func requireNoInitDataRoot(t *testing.T, beadsDir string) {
	t.Helper()
	for _, layout := range []string{"embeddeddolt", "dolt"} {
		path := filepath.Join(beadsDir, layout)
		if _, err := os.Stat(path); err == nil {
			t.Errorf("database should NOT have been created at %s", path)
		}
	}
}

// initExportedEnvVars are the environment variables an in-process `bd`
// command writes back into the process it runs in: the selected workspace
// (main.go prepareSelectedCommandContext and the -C handler), the database
// carried across a redirect (preserveRedirectSourceDatabase), and the two
// switches `bd init` turns on for its own children (init.go).
//
// A real CLI run gets to leak these -- the process exits immediately after.
// Running the same code in-process does not, and nothing else resets them.
var initExportedEnvVars = []string{
	"BEADS_DIR",
	"BEADS_DOLT_SERVER_DATABASE",
	"BEADS_DOLT_SHARED_SERVER",
	"BEADS_DOLT_DEBUG",
}

// isolateInitEnvForTest confines the process-global environment that an
// in-process `bd init` mutates to the calling test, in both directions.
//
// Inbound, it drops any workspace selection an earlier test exported: a
// leaked BEADS_DIR points at that test's temp directory, which t.TempDir has
// already deleted, so the next init aborts before doing anything with
// "bd init refuses to run over live bd activity on this workspace:
// workspacegate: gate parent <the other test's dir> must exist". One leaking
// test took out every init test after it -- including the ones that shell out
// to a real `bd`, which inherit os.Environ() (bd-kbx).
//
// Outbound, it restores the whole BEADS_* namespace on cleanup, which is
// wider than the inbound list on purpose: the inbound clear must name only
// variables that are safe to drop, while the restore only has to put things
// back, so it stays correct as init grows new exports.
//
// Call this BEFORE any t.Setenv in the test. Cleanups run
// last-registered-first, so the snapshot must be taken first to be restored
// last.
func isolateInitEnvForTest(t *testing.T) {
	t.Helper()
	const prefix = "BEADS_"
	saved := map[string]string{}
	for _, kv := range os.Environ() {
		k, v, ok := strings.Cut(kv, "=")
		if ok && strings.HasPrefix(k, prefix) {
			saved[k] = v
		}
	}
	t.Cleanup(func() {
		for _, kv := range os.Environ() {
			k, _, ok := strings.Cut(kv, "=")
			if !ok || !strings.HasPrefix(k, prefix) {
				continue
			}
			if _, kept := saved[k]; !kept {
				_ = os.Unsetenv(k)
			}
		}
		for k, v := range saved {
			_ = os.Setenv(k, v)
		}
	})
	for _, k := range initExportedEnvVars {
		_ = os.Unsetenv(k)
	}
}
