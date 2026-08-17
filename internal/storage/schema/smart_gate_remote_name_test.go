package schema

import (
	"context"
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// expectRemoteNames mocks resolveSmartGateRemote's remote-name listing, which
// routeSmartGate issues between the local content-hash read and active_branch().
func expectRemoteNames(mock sqlmock.Sqlmock, names ...string) {
	rows := sqlmock.NewRows([]string{"name"})
	for _, n := range names {
		rows.AddRow(n)
	}
	mock.ExpectQuery(`SELECT name FROM dolt_remotes`).WillReturnRows(rows)
}

// expectSmartReadResolving mocks the full smart-router read sequence for a
// database whose configured sync remote is `configured` but whose actual Dolt
// remotes are `names` — asserting the remote-tracking ref is built from
// `wantRemote`, the RESOLVED name.
func expectSmartReadResolving(mock sqlmock.Sqlmock, names []string, wantRemote string, local, remote map[int]string) {
	mock.ExpectQuery(`SELECT version, content_hash FROM schema_migrations$`).
		WillReturnRows(hashRows(local))
	expectRemoteNames(mock, names...)
	mock.ExpectQuery(`SELECT active_branch\(\)`).
		WillReturnRows(sqlmock.NewRows([]string{"active_branch()"}).AddRow("main"))
	ref := "remotes/" + wantRemote + "/main"
	mock.ExpectQuery(`SHOW TABLES AS OF '` + ref + `' LIKE 'schema_migrations'`).
		WillReturnRows(sqlmock.NewRows([]string{"Tables_in_beads"}).AddRow("schema_migrations"))
	mock.ExpectQuery(`SHOW COLUMNS FROM schema_migrations AS OF '` + ref + `' LIKE 'content_hash'`).
		WillReturnRows(sqlmock.NewRows([]string{"Field"}).AddRow("content_hash"))
	mock.ExpectQuery(`SELECT version, content_hash FROM schema_migrations AS OF '` + ref + `'`).
		WillReturnRows(hashRows(remote))
}

// TestResolveSmartGateRemote pins the resolution order directly.
func TestResolveSmartGateRemote(t *testing.T) {
	tests := []struct {
		name       string
		configured string
		remotes    []string
		want       string
	}{
		{
			name:       "sync.remote names a real remote is used unchanged",
			configured: "origin",
			remotes:    []string{"origin", "upstream"},
			want:       "origin",
		},
		{
			// The regression: sync.remote defaults to the literal "upstream"
			// while the only configured Dolt remote is "origin".
			name:       "default upstream with only origin resolves to origin",
			configured: "upstream",
			remotes:    []string{"origin"},
			want:       "origin",
		},
		{
			// sync.remote frequently holds a URL, never a remote name.
			name:       "URL-valued sync.remote resolves to origin",
			configured: "git+ssh://git@github.com/owner/repo.git",
			remotes:    []string{"origin"},
			want:       "origin",
		},
		{
			name:       "no origin but exactly one remote resolves to that remote",
			configured: "upstream",
			remotes:    []string{"seed"},
			want:       "seed",
		},
		{
			// Ambiguous and unmatched: keep the caller's value so the existing
			// unreadable-remote-state block still fires. Never guess.
			name:       "ambiguous with no origin and no match is left unchanged",
			configured: "upstream",
			remotes:    []string{"alpha", "beta"},
			want:       "upstream",
		},
		{
			// Zero configured remotes: nothing to resolve onto, so the caller's
			// value passes through untouched. Raised by beads/refinery on review
			// of the parent fix. Note this is the resolver's LOCAL behaviour
			// only — see TestZeroRemoteStoreNeverReachesSmartRouter for why a
			// database in this state never reaches the resolver at all.
			name:       "zero remotes is left unchanged",
			configured: "upstream",
			remotes:    nil,
			want:       "upstream",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			db, mock, _ := sqlmock.New()
			defer db.Close()
			expectRemoteNames(mock, tc.remotes...)

			if got := resolveSmartGateRemote(context.Background(), db, tc.configured); got != tc.want {
				t.Errorf("resolveSmartGateRemote(%q) with remotes %v = %q, want %q",
					tc.configured, tc.remotes, got, tc.want)
			}
		})
	}
}

// TestResolveSmartGateRemoteFailsSafe: if the remote list cannot be read we must
// return the caller's value untouched, so the gate keeps its existing block
// rather than resolving toward a migrate on unknown state.
func TestResolveSmartGateRemoteFailsSafe(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer db.Close()
	mock.ExpectQuery(`SELECT name FROM dolt_remotes`).
		WillReturnError(errors.New("no such table: dolt_remotes"))

	if got := resolveSmartGateRemote(context.Background(), db, "upstream"); got != "upstream" {
		t.Errorf("on read failure got %q, want the caller's value %q", got, "upstream")
	}
}

// TestZeroRemoteStoreNeverReachesSmartRouter pins the claim that resolves a
// review dispute on the parent fix (bd-2x7).
//
// beads/refinery read resolveSmartGateRemote's zero-remote early return —
// "names empty -> return remoteName unchanged -> unresolvable ref -> block" —
// and concluded that a database with NO remotes still blocks. The trace is
// accurate but the function is unreachable in that state:
// checkRemoteMigrateGate returns nil as soon as hasRemote is false, well before
// the smart router, the resolver, or any ref construction.
//
// Both sides of that exchange were code-path analysis, and the one field
// observation suggesting otherwise was confounded (the gate error and the
// repo_state capture may have come from different stores). So pin it with a
// test instead of an argument.
//
// The assertion is structural, not just the returned error: the mock is primed
// ONLY for the blunt-gate probes. If control ever reached routeSmartGate it
// would issue "SELECT version, content_hash FROM schema_migrations", which is
// unmocked — sqlmock fails, and this test fails with it. That makes the test
// sensitive to the exact regression it guards, rather than merely asserting a
// nil error that a dozen unrelated changes could also produce.
func TestZeroRemoteStoreNeverReachesSmartRouter(t *testing.T) {
	t.Setenv(SmartGateEnv, "1")
	t.Setenv(AllowRemoteMigrateEnv, "0")
	floor := LastNonDeterministicMigration

	for _, tc := range []struct {
		name           string
		extraHasRemote func() bool
	}{
		{"no extra probe wired (embedded path)", nil},
		{"extra probe reports no persisted remote", func() bool { return false }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, mock, _ := sqlmock.New()
			defer db.Close()

			// Pending migrations exist — so the gate is NOT short-circuiting on
			// "nothing to migrate". Without this the test would pass vacuously.
			expectGateCurrentVersion(mock, floor) // CurrentVersion
			expectGateCurrentVersion(mock, floor) // PendingVersions -> pending
			// The database has NO Dolt remotes.
			mock.ExpectQuery(`SELECT COUNT\(\*\) FROM dolt_remotes`).
				WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))

			err := CheckRemoteMigrateGateForRemoteWithRemoteCheck(
				context.Background(), db, "upstream", tc.extraHasRemote)
			if err != nil {
				t.Fatalf("a database with no remotes has no cross-clone fork risk "+
					"and must not be gated, got %v", err)
			}
			// Proves the smart router never ran: any further query would be
			// unexpected and is reported here.
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatalf("gate issued queries beyond the blunt probes — it reached "+
					"the smart router despite having no remote: %v", err)
			}
		})
	}
}

// TestSmartGateDoesNotBlockOnMismatchedRemoteName is the regression test for the
// production incident: three rigs had every write refused for 12 days with
// fallback_reason "unreadable-remote-state" because the gate built
// remotes/upstream/main (from sync.remote's default) while the real remote was
// named origin. Measured against the live rig, remotes/upstream/main gave
// "branch not found" while remotes/origin/main returned the full migration set —
// the gate blocked on state it could have read.
//
// With the remote name resolved, the same database is a clean first-mover and
// must be allowed to migrate.
func TestSmartGateDoesNotBlockOnMismatchedRemoteName(t *testing.T) {
	t.Setenv(SmartGateEnv, "1")
	t.Setenv(AllowRemoteMigrateEnv, "0")
	floor := LastNonDeterministicMigration

	db, mock, _ := sqlmock.New()
	defer db.Close()
	expectSmartFiringGate(mock, floor)
	hashes := map[int]string{floor - 1: "h1", floor: "h2"}
	// Configured sync remote is "upstream"; the only real remote is "origin".
	expectSmartReadResolving(mock, []string{"origin"}, "origin", hashes, hashes)

	err := CheckRemoteMigrateGateForRemoteWithRemoteCheck(
		context.Background(), db, "upstream", func() bool { return true })
	if err != nil {
		t.Fatalf("gate must resolve the remote name and allow the first-mover migrate, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestSmartGateStillBlocksOnGenuineForkSkew guards the fix from loosening the
// gate: resolving the remote NAME must not weaken the skew check. Same
// name mismatch, but the remote's content hashes genuinely diverge — the gate
// must still stop with a fork-skew decision.
func TestSmartGateStillBlocksOnGenuineForkSkew(t *testing.T) {
	t.Setenv(SmartGateEnv, "1")
	t.Setenv(AllowRemoteMigrateEnv, "0")
	floor := LastNonDeterministicMigration

	db, mock, _ := sqlmock.New()
	defer db.Close()
	expectSmartFiringGate(mock, floor)
	local := map[int]string{floor - 1: "h1", floor: "h2"}
	remote := map[int]string{floor - 1: "h1", floor: "DIVERGED"}
	expectSmartReadResolving(mock, []string{"origin"}, "origin", local, remote)

	err := CheckRemoteMigrateGateForRemoteWithRemoteCheck(
		context.Background(), db, "upstream", func() bool { return true })
	var gateErr *RemoteMigrateGateError
	if !errors.As(err, &gateErr) {
		t.Fatalf("a genuine fork must still be blocked, got %v", err)
	}
	if gateErr.Decision != gateDecisionForkSkew {
		t.Errorf("Decision = %q, want %q", gateErr.Decision, gateDecisionForkSkew)
	}
}
