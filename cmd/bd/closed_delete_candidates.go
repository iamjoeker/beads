package main

import (
	"context"
	"fmt"
	"time"

	"github.com/steveyegge/beads/internal/config"
	"github.com/steveyegge/beads/internal/types"
	"github.com/steveyegge/beads/internal/workapi"
	"github.com/steveyegge/beads/issueops"
)

// `bd gc`'s decay selects closed beads outside issueops.Sweeper, so what is
// here is a thin call into the SAME pure function
// (workapi.FilterSweepCandidates) the role applies below both front doors,
// plus gc's own warning line. One definition, two callers: gc and the role
// cannot come to disagree about what "a closed bead safe to delete" means.
//
// The other retention doors outside the role — `bd admin cleanup` and the
// stale-closed-issues doctor fix — select by their own rules and delete with
// no tombstone (cleanup through the Deleter role, the doctor fix row by row),
// so each holds back the beads of a retained type: cleanup through
// holdBackRetainedTypes, the doctor fix, in its own package, through the
// workapi partition itself. Each reads types.retained with its selection, on
// one snapshot (storage.SearchIssuesWithConfig). `bd mol wisp gc` does not
// consult the setting; SweepRequest.RetainIDs says why.

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

// holdBackRetainedTypes keeps a retention door that cannot retire an id off
// the beads whose type the workspace retains (issueops.SettingKeyTypesRetained):
// `bd gc`'s decay and `bd admin cleanup`. It is the same
// partition the Sweeper role applies to a sweep without RetainIDs: those
// commands delete by id and write no tombstone, so a retained bead is never
// theirs to take. setting is the stored value of types.retained, read from the
// workspace database with the selection; a caller that cannot read it must
// fail rather than pass "".
func holdBackRetainedTypes(issues []*types.Issue, setting string) ([]*types.Issue, int) {
	return workapi.PartitionSweepRetainedTypes(issues, workapi.RetainedTypesFromSetting(setting))
}

// retainedTypesKeptLine is the text line for a bulk delete's kept count; noun
// names what was kept ("closed issue(s)").
func retainedTypesKeptLine(kept int, noun string) string {
	return fmt.Sprintf("  Kept %d %s of a retained type (types.retained) — only bd prune/purge --retain-ids deletes them", kept, noun)
}

// warnIgnoredYAMLRetainedTypes prints config.RetainedTypesYAMLWarning to
// stderr, if there is one. That layer is never read for the setting — every
// sweep and bulk delete reads the workspace database, so a shared workspace
// has one rule — so every bulk delete that honors types.retained calls it
// before it selects (the doctor fix through its own copy of this call).
func warnIgnoredYAMLRetainedTypes() {
	if msg := config.RetainedTypesYAMLWarning(); msg != "" {
		WarnError("%s", msg)
	}
}

// retentionReport is the --json member a bulk delete outside the Sweeper role
// adds when it held retained beads back: retention_required, the name the
// sweeps use. It is nil when nothing was kept, so a workspace without the
// setting sees its old shape.
func retentionReport(kept int) map[string]interface{} {
	if kept == 0 {
		return nil
	}
	return map[string]interface{}{"retention_required": kept}
}
