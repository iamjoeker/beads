package issueops

import (
	"testing"
	"time"

	"github.com/steveyegge/beads/internal/types"
)

// TestPrepareIssueForInsertNormalizesOptionalTimestamps pins that
// PrepareIssueForInsert's promise — "normalizes timestamps to UTC" — covers the
// optional instants and not just created_at/updated_at. The gap was silent on a
// UTC machine and one zone offset wide everywhere else.
func TestPrepareIssueForInsertNormalizesOptionalTimestamps(t *testing.T) {
	tz, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}
	// 2030-06-15 10:00 PDT is 2030-06-15 17:00 UTC.
	local := time.Date(2030, 6, 15, 10, 0, 0, 0, tz)
	started := local.Add(-time.Hour)
	deferUntil := local.Add(time.Hour)

	issue := &types.Issue{
		ID: "bd-tz", Title: "zone-aware", Status: types.StatusOpen,
		Priority: 2, IssueType: types.TypeTask,
		CreatedAt: local, UpdatedAt: local,
		DueAt: &local, DeferUntil: &deferUntil, StartedAt: &started,
	}
	if err := PrepareIssueForInsert(issue, nil, nil); err != nil {
		t.Fatalf("PrepareIssueForInsert: %v", err)
	}

	for _, field := range []struct {
		name string
		got  *time.Time
		want time.Time
	}{
		{"due_at", issue.DueAt, local},
		{"defer_until", issue.DeferUntil, deferUntil},
		{"started_at", issue.StartedAt, started},
	} {
		if field.got == nil {
			t.Errorf("%s was dropped", field.name)
			continue
		}
		if field.got.Location() != time.UTC {
			t.Errorf("%s location = %v, want UTC", field.name, field.got.Location())
		}
		if !field.got.Equal(field.want) {
			t.Errorf("%s = %s, want the instant %s", field.name, field.got.Format(time.RFC3339), field.want.UTC().Format(time.RFC3339))
		}
	}

	// The caller supplied &local and still holds it; normalizing through the
	// shared pointer would silently rewrite its zone.
	if local.Location() != tz {
		t.Errorf("caller's time was mutated: location = %v, want %v", local.Location(), tz)
	}
}

// TestNormalizeTimestampUpdateValueUTC pins the shapes the update planes hand
// in: a value, a pointer, a nil pointer that clears the column, and a
// non-timestamp that must pass through for the caller's own type handling.
func TestNormalizeTimestampUpdateValueUTC(t *testing.T) {
	tz, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}
	local := time.Date(2030, 6, 15, 10, 0, 0, 0, tz)

	if got, ok := NormalizeTimestampUpdateValueUTC(local).(time.Time); !ok || got.Location() != time.UTC || !got.Equal(local) {
		t.Errorf("time.Time value = %v (ok=%v), want the same instant in UTC", got, ok)
	}
	if got, ok := NormalizeTimestampUpdateValueUTC(&local).(time.Time); !ok || got.Location() != time.UTC || !got.Equal(local) {
		t.Errorf("*time.Time value = %v (ok=%v), want the same instant in UTC", got, ok)
	}
	if got := NormalizeTimestampUpdateValueUTC((*time.Time)(nil)); got != nil {
		t.Errorf("nil pointer = %v, want nil so the column clears", got)
	}
	if got := NormalizeTimestampUpdateValueUTC(nil); got != nil {
		t.Errorf("nil = %v, want nil", got)
	}
	if got := NormalizeTimestampUpdateValueUTC("not a time"); got != "not a time" {
		t.Errorf("non-timestamp = %v, want it returned unchanged", got)
	}

	if !IsTimestampUpdateField("due_at") || !IsTimestampUpdateField("defer_until") ||
		!IsTimestampUpdateField("started_at") || !IsTimestampUpdateField("closed_at") {
		t.Error("IsTimestampUpdateField missed a DATETIME column")
	}
	if IsTimestampUpdateField("title") {
		t.Error("IsTimestampUpdateField claimed title is a timestamp")
	}
}
