package main

import (
	"context"
	"time"

	"github.com/steveyegge/beads/internal/config"
	"github.com/steveyegge/beads/internal/types"
	"github.com/steveyegge/beads/internal/workapi"
	"github.com/steveyegge/beads/issueops"
)

// `bd gc` is the one caller left that is not behind issueops.Sweeper, so what
// is here is a thin call into the SAME pure function
// (workapi.FilterSweepCandidates) the role applies below both front doors,
// plus gc's own warning line. One definition, two callers: gc and the role
// cannot come to disagree about what "a closed bead safe to delete" means.

type closedDeletionCandidateStats = issueops.SweepSkips

// filterClosedDeletionCandidates keeps the closed, unpinned, unprotected,
// old-enough candidates and reports what it held back. `bd gc` matches no
// glob, so it passes the empty pattern, which admits everything.
func filterClosedDeletionCandidates(issues []*types.Issue, cutoff *time.Time, protected workapi.GCProtectedLabels) ([]*types.Issue, closedDeletionCandidateStats) {
	return workapi.FilterSweepCandidates(issues, "", cutoff, protected)
}

// resolveGCProtectedLabels reads the workspace's GC-protected label set for
// the front doors that are not behind issueops.Sweeper: `bd gc` and
// `bd mol wisp gc`. The layering — stored setting, config.yaml, built-in
// defaults — is workapi's, so all four bulk-delete paths protect the same
// beads.
//
// A NIL READER OR A FAILED READ STILL RETURNS A PROTECTING SET. There is no
// branch here that answers "protect nothing": a settings read that failed says
// nothing about whether a merge-request record is safe to delete, and this
// mechanism exists because deleting on that silence is unrecoverable.
func resolveGCProtectedLabels(ctx context.Context, r configReader) workapi.GCProtectedLabels {
	var stored string
	if r != nil {
		if value, err := r.GetConfig(ctx, workapi.ConfigKeyGCProtectedLabels); err == nil {
			stored = value
		}
	}
	return workapi.ResolveGCProtectedLabels(stored, config.GetGCProtectedLabelsFromYAML())
}

// reportGCLabelProtectedSkips says when the protection FIRED. It is not
// decoration: a sweep that silently kept back the record an operator came to
// clean up, and a sweep that found nothing to keep back, print the same thing
// otherwise — so the one number that distinguishes "protected" from "not
// present" would live only in the code. It goes to stderr so it survives a
// caller reading stdout as JSON.
// It takes the set the filter actually used rather than re-reading it, so the
// protections it names cannot disagree with the ones that held the beads back.
func reportGCLabelProtectedSkips(stats closedDeletionCandidateStats, protected workapi.GCProtectedLabels) {
	if stats.LabelProtected == 0 {
		return
	}
	WarnError("kept %d protected bead(s) (%s); delete one deliberately with `bd delete <id>`",
		stats.LabelProtected, protected.Describe())
}

func warnClosedDeletionSafetySkips(stats closedDeletionCandidateStats) {
	if workapi.SweepDefenseSkips(stats) == 0 {
		return
	}
	WarnError("skipped %d deletion candidate(s) after closed_at safety recheck (nil=%d, non_closed=%d, missing_closed_at=%d, too_recent=%d)",
		workapi.SweepDefenseSkips(stats),
		stats.Unreadable,
		stats.NotClosed,
		stats.UnknownClosedAt,
		stats.ClosedAtOrAfterCutoff,
	)
}
