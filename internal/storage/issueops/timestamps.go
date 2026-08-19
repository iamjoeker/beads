package issueops

import (
	"time"

	"github.com/steveyegge/beads/internal/types"
)

// timestampUpdateFields names the update columns that carry an instant. Both
// write planes normalize these to UTC before binding them, so the set lives
// here rather than beside either one.
var timestampUpdateFields = map[string]struct{}{
	"started_at": {}, "closed_at": {}, "due_at": {}, "defer_until": {},
}

// IsTimestampUpdateField reports whether an update field names a DATETIME
// column whose value must be normalized to UTC before it is bound.
func IsTimestampUpdateField(key string) bool {
	_, ok := timestampUpdateFields[key]
	return ok
}

// NormalizeTimestampUpdateValueUTC converts an update value for a timestamp
// column to UTC, preserving the instant. A nil pointer clears the column and is
// returned as an untyped nil; a value that is not a time is returned unchanged
// so the caller's own type handling still sees it.
func NormalizeTimestampUpdateValueUTC(value any) any {
	switch v := value.(type) {
	case time.Time:
		return v.UTC()
	case *time.Time:
		if v == nil {
			return nil
		}
		return v.UTC()
	}
	return value
}

// UTCTimePtr converts an optional instant to UTC for binding, leaving the
// caller's value untouched. Every DATETIME column is read back as UTC, so the
// bind is the last place a zone-aware value can still be made to mean what the
// writer meant.
func UTCTimePtr(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	utc := value.UTC()
	return &utc
}

// NormalizeIssueOptionalTimestampsUTC rewrites an issue's optional instants to
// UTC ahead of an insert. Each field is reallocated rather than converted in
// place: the caller still holds these pointers — public_create clones them from
// a caller-owned issue — and a shared time.Time must not change zone underneath
// whoever supplied it.
func NormalizeIssueOptionalTimestampsUTC(issue *types.Issue) {
	if issue == nil {
		return
	}
	issue.StartedAt = UTCTimePtr(issue.StartedAt)
	issue.ClosedAt = UTCTimePtr(issue.ClosedAt)
	issue.DueAt = UTCTimePtr(issue.DueAt)
	issue.DeferUntil = UTCTimePtr(issue.DeferUntil)
	issue.CompactedAt = UTCTimePtr(issue.CompactedAt)
}
