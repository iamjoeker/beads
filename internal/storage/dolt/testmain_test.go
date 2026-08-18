package dolt

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/steveyegge/beads/internal/doltserver"
	"github.com/steveyegge/beads/internal/testutil"
)

// testServerPort is the port of the shared test Dolt server (0 = not running).
// Set by TestMain before tests run, used implicitly via BEADS_DOLT_PORT env var
// which applyConfigDefaults reads when ServerPort is 0.
var testServerPort int

// testSharedDB is the name of the shared database for branch-per-test isolation.
var testSharedDB string

// testSharedConn is a raw *sql.DB for branch operations in the shared database.
var testSharedConn *sql.DB

func TestMain(m *testing.M) {
	os.Exit(testMainInner(m))
}

func testMainInner(m *testing.M) int {
	os.Setenv("BEADS_TEST_MODE", "1")
	// BEADS_TEST_PDEATHSIG=1 protects TestMultiProcessSchemaInit_DoltVerify
	// (initschema_multiprocess_test.go), which calls doltserver.Start
	// directly (in-process, no exec boundary) and this TestMain's own
	// process stays alive for the server's whole lifetime. See
	// internal/doltserver/procattr_linux.go for why this is a narrower,
	// separate flag from BEADS_TEST_MODE.
	os.Setenv("BEADS_TEST_PDEATHSIG", "1")

	// Suite-owned root for the orphan-server sweep below. Must never be a
	// shared/global temp dir (see SweepOrphanedTestServers) — this one is
	// unique to this test run and removed when it exits.
	suiteTempRoot, tempRootErr := os.MkdirTemp("", "beads-storage-dolt-tests-*")
	if tempRootErr != nil {
		fmt.Fprintf(os.Stderr, "FATAL: failed to create suite temp root: %v\n", tempRootErr)
		return 1
	} else {
		defer os.RemoveAll(suiteTempRoot)
		if err := os.Setenv(testCircuitBreakerDirEnv, filepath.Join(suiteTempRoot, "circuit")); err != nil {
			fmt.Fprintf(os.Stderr, "FATAL: failed to isolate circuit state: %v\n", err)
			return 1
		}
		defer os.Unsetenv(testCircuitBreakerDirEnv)
	}

	// AD-01 (be-c5p): the test/bench harness opens a process-local dolt
	// sql-server (testcontainer or external port). The new database-name
	// firewall in dolt.New refuses test-named DBs unless this opt-in is set.
	os.Setenv("BEADS_TEST_SERVER", "1")
	if err := testutil.EnsureDoltContainerForTestMain(); err != nil {
		fmt.Fprintf(os.Stderr, "WARN: %v, skipping Dolt tests\n", err)
	} else {
		defer testutil.TerminateDoltContainer()

		// The timeout guard lives here, inside the branch where Dolt is
		// actually available, because that is the only branch that does
		// real work. With Docker absent or BEADS_TEST_SKIP=dolt set —
		// which is the default for scripts/test.sh and for every CI job
		// that runs ./... — every test self-skips in seconds and any
		// ceiling is fine. See requiredSuiteTimeout in suite_timeout_test.go.
		flag.Parse() // testing.Init registered the flags; m.Run has not parsed them yet
		if msg := suiteTimeoutRefusal(readSuiteRunFlags()); msg != "" {
			fmt.Fprint(os.Stderr, msg)
			return 1
		}

		testServerPort = testutil.DoltContainerPortInt()

		// Set up shared database for branch-per-test isolation
		testSharedDB = "dolt_pkg_shared"
		db, err := testutil.SetupSharedTestDB(testServerPort, testSharedDB)
		if err != nil {
			fmt.Fprintf(os.Stderr, "FATAL: shared DB setup failed: %v\n", err)
			return 1
		}
		testSharedConn = db
		defer db.Close()

		// Create the schema by opening a store against the shared DB,
		// configuring it, and committing.
		if err := initSharedSchema(testServerPort); err != nil {
			fmt.Fprintf(os.Stderr, "FATAL: shared schema init failed: %v\n", err)
			return 1
		}
	}

	code := m.Run()

	// Branch-per-test residue. Nothing here fails the run — a suite that
	// passed did pass — but the two numbers are what a whole-package run
	// costs an hour to produce, so print them rather than throw them away.
	// The standing question they answer (bd-033) is whether test branches
	// accumulate across the run, which would make per-test setup slower as
	// the suite goes on and would explain deadline expiries that cluster
	// toward the end.
	if report := testutil.BranchCleanupReport(); report != "" {
		fmt.Fprint(os.Stderr, report)
	}
	if testSharedConn != nil {
		if leaked := testutil.CountTestBranches(testSharedConn, testSharedDB); leaked > 0 {
			fmt.Fprintf(os.Stderr, "%d test branch(es) survived the run in %s\n", leaked, testSharedDB)
		}
	}

	// Best-effort reap of any dolt sql-server left running under this
	// suite's own temp root (e.g. a SIGKILLed run of the multiprocess
	// schema init tests, which call doltserver.Start directly) — see
	// gastownhall/beads mybd-q6cz.
	doltserver.SweepOrphanedTestServers(suiteTempRoot)

	testServerPort = 0
	os.Unsetenv("BEADS_DOLT_PORT")
	os.Unsetenv("BEADS_TEST_MODE")
	os.Unsetenv("BEADS_TEST_PDEATHSIG")
	return code
}

// initSharedSchema creates a store on the shared DB, sets config, and commits
// so that branches inherit the full schema.
func initSharedSchema(port int) error {
	ctx := context.Background()
	cfg := &Config{
		Path:            "/tmp/dolt-shared-init", // not used, just needs to be non-empty
		ServerHost:      "127.0.0.1",
		ServerPort:      port,
		Database:        testSharedDB,
		MaxOpenConns:    1,
		CreateIfMissing: true, // TestMain creates the shared database
	}
	store, err := New(ctx, cfg)
	if err != nil {
		return fmt.Errorf("New: %w", err)
	}
	defer store.Close()

	if err := store.SetConfig(ctx, "issue_prefix", "test"); err != nil {
		return fmt.Errorf("SetConfig(issue_prefix): %w", err)
	}

	// Commit schema to main so branches get a clean snapshot
	if _, err := store.db.ExecContext(ctx, "CALL DOLT_ADD('-A')"); err != nil {
		return fmt.Errorf("DOLT_ADD: %w", err)
	}
	if _, err := store.db.ExecContext(ctx, "CALL DOLT_COMMIT('--allow-empty', '-m', 'test: init shared schema')"); err != nil {
		return fmt.Errorf("DOLT_COMMIT: %w", err)
	}
	if err := testutil.MaterializeLocalTableSchemasForBranchTests(ctx, store.db); err != nil {
		return fmt.Errorf("materialize local table schemas: %w", err)
	}

	return nil
}
