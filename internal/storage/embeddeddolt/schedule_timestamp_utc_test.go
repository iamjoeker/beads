//go:build cgo

package embeddeddolt_test

import (
	"testing"
	"time"

	"github.com/steveyegge/beads/internal/types"
)

// TestScheduleTimestampsStoredAsUTC pins that a zone-aware due_at/defer_until
// is persisted as the instant its writer meant, on both the create and the
// update path.
//
// The column is a bare DATETIME that every reader parses as UTC, and the
// embedded Dolt driver — unlike go-sql-driver, which converts a time.Time to
// the connection zone before binding it — writes the wall clock it is handed.
// So an unnormalized UTC-7 value used to land seven hours early: the row said
// 10:00Z when the writer meant 17:00Z. created_at and updated_at were never
// affected, because they are normalized before the bind; due_at and defer_until
// were not, which is the asymmetry this test forbids from coming back.
//
// The zone is constructed explicitly rather than taken from the runner's TZ:
// the skew is exactly zero on a UTC machine, which is how this survived CI.
func TestScheduleTimestampsStoredAsUTC(t *testing.T) {
	skipUnlessEmbeddedDolt(t)

	te := newTestEnv(t, "tzn")
	ctx := t.Context()

	tz, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}
	// 2030-06-15 10:00 PDT is 2030-06-15 17:00 UTC; a wall-clock write would
	// store 10:00 and read back seven hours early.
	due := time.Date(2030, 6, 15, 10, 0, 0, 0, tz)
	deferUntil := time.Date(2030, 6, 16, 11, 0, 0, 0, tz)

	issue := &types.Issue{
		ID: "tzn-1", Title: "zone-aware schedule", Status: types.StatusOpen,
		Priority: 2, IssueType: types.TypeTask,
		DueAt: &due, DeferUntil: &deferUntil,
	}
	if err := te.store.CreateIssue(ctx, issue, "tester"); err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}

	var storedDue, storedDefer time.Time
	te.queryScalar(t, ctx, "SELECT due_at, defer_until FROM issues WHERE id = ?", []any{"tzn-1"}, &storedDue, &storedDefer)
	if !storedDue.Equal(due) {
		t.Errorf("stored due_at = %s, want the instant %s", storedDue.Format(time.RFC3339), due.UTC().Format(time.RFC3339))
	}
	if !storedDefer.Equal(deferUntil) {
		t.Errorf("stored defer_until = %s, want the instant %s", storedDefer.Format(time.RFC3339), deferUntil.UTC().Format(time.RFC3339))
	}

	// The caller still holds the pointers it passed in; normalization must not
	// reach back through them and change the zone underneath it.
	if due.Location() != tz || deferUntil.Location() != tz {
		t.Errorf("create mutated the caller's values: due=%v defer=%v", due.Location(), deferUntil.Location())
	}

	read, err := te.store.GetIssue(ctx, "tzn-1")
	if err != nil {
		t.Fatalf("GetIssue: %v", err)
	}
	if read.DueAt == nil || !read.DueAt.Equal(due) {
		t.Errorf("GetIssue due_at = %v, want the instant %s", read.DueAt, due.UTC().Format(time.RFC3339))
	}
	if read.DeferUntil == nil || !read.DeferUntil.Equal(deferUntil) {
		t.Errorf("GetIssue defer_until = %v, want the instant %s", read.DeferUntil, deferUntil.UTC().Format(time.RFC3339))
	}

	// The update path binds its own values and so needs its own normalization.
	newDue := time.Date(2031, 6, 15, 10, 0, 0, 0, tz)
	if err := te.store.UpdateIssue(ctx, "tzn-1", map[string]any{"due_at": newDue}, "tester"); err != nil {
		t.Fatalf("UpdateIssue: %v", err)
	}
	var updatedDue time.Time
	te.queryScalar(t, ctx, "SELECT due_at FROM issues WHERE id = ?", []any{"tzn-1"}, &updatedDue)
	if !updatedDue.Equal(newDue) {
		t.Errorf("updated due_at = %s, want the instant %s", updatedDue.Format(time.RFC3339), newDue.UTC().Format(time.RFC3339))
	}
}
