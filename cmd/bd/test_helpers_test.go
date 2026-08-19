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
	"testing"
	"time"

	"github.com/steveyegge/beads/internal/configfile"
	"github.com/steveyegge/beads/internal/storage/dolt"
	"github.com/steveyegge/beads/internal/storage/doltutil"
	"github.com/steveyegge/beads/internal/testutil"
)

// testDoltServerPort is the port of the shared test Dolt server (0 = not running).
var testDoltServerPort int

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

	ctx := context.Background()

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

	ctx := context.Background()

	// Fast path: use shared DB with branch-per-test isolation (bd-xmf)
	if testSharedDB != "" {
		return newTestStoreSharedBranch(t, dbPath, prefix)
	}

	// Fallback: per-test database (original slow path)
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
	ctx := context.Background()

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
		"BEADS_DOLT_SHARED_SERVER=1",
		"BEADS_SHARED_SERVER_DIR="+serverDir,
		"BEADS_DOLT_SERVER_PORT="+strconv.Itoa(port),
	)
}

// openExistingTestDB reopens an existing Dolt store for verification in tests.
// It tries NewFromConfig first (reads metadata.json for correct database name),
// then falls back to direct open for BEADS_DB or other non-standard paths.
func openExistingTestDB(t *testing.T, dbPath string) (*dolt.DoltStore, error) {
	t.Helper()
	// Serialize dolt.New() to avoid race in Dolt's InitStatusVariables (bd-cqjoi)
	doltNewMutex.Lock()
	defer doltNewMutex.Unlock()
	ctx := context.Background()
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
