package workapi

import (
	"testing"
	"time"

	"github.com/steveyegge/beads/internal/types"
)

// Every case here asserts an EFFECT — which beads a filter kept and which it
// dropped — rather than the presence of the guard. The defect this protection
// replaces shipped with a comment describing it and a test naming it, and the
// predicate underneath matched zero rows: both artifacts were satisfied by a
// guard that did nothing. So each case carries a NEGATIVE CONTROL of the same
// shape as its positive one (same status, same timestamps, no protected
// label), which is what makes "the protected bead survived" mean the
// protection fired rather than the sweep having done nothing at all.

func TestResolveGCProtectedLabelsPrefersStoredThenYAMLThenDefaults(t *testing.T) {
	stored := ResolveGCProtectedLabels("team:keep, ops:keep", []string{"yaml:keep"})
	if !stored["team:keep"] || !stored["ops:keep"] {
		t.Errorf("stored setting = %v, want both configured labels", stored.Labels())
	}
	if stored["yaml:keep"] || stored["gt:merge-request"] {
		t.Errorf("stored setting = %v, want it to REPLACE the lower layers", stored.Labels())
	}

	fromYAML := ResolveGCProtectedLabels("", []string{"yaml:keep"})
	if !fromYAML["yaml:keep"] || fromYAML["gt:merge-request"] {
		t.Errorf("yaml layer = %v, want only the yaml label", fromYAML.Labels())
	}

	// An unset setting must not mean "nothing is protected": an unconfigured
	// workspace runs the same sweeps against the same classes of record.
	defaults := ResolveGCProtectedLabels("", nil)
	for _, want := range DefaultGCProtectedLabels() {
		if !defaults[want] {
			t.Errorf("defaults = %v, want %q", defaults.Labels(), want)
		}
	}

	// A whitespace-only value is unset, not "protect the empty label".
	if blank := ResolveGCProtectedLabels("  ", nil); !blank["gt:merge-request"] {
		t.Errorf("blank setting = %v, want the defaults", blank.Labels())
	}
}

func TestGCProtectedLabelsMatchesLooselyAndNeverOnEmpty(t *testing.T) {
	set := NewGCProtectedLabels([]string{" GT:Merge-Request ", "", "   "})
	if len(set) != 1 {
		t.Fatalf("set = %v, want the one non-empty label", set.Labels())
	}

	if !set.Protects(&types.Issue{ID: "mr", Labels: []string{"gt:merge-request"}}) {
		t.Error("a configured label spelled with different case and padding must still protect")
	}
	// An empty configured label would otherwise match a bead carrying an empty
	// label string, which is every bead in some import paths.
	if set.Protects(&types.Issue{ID: "blank", Labels: []string{""}}) {
		t.Error("an empty label must never match")
	}
	if set.Protects(&types.Issue{ID: "other", Labels: []string{"gt:merge-request-v2"}}) {
		t.Error("matching is by whole label, not prefix")
	}
	if set.Protects(nil) {
		t.Error("a nil issue is not protected here; the sweep counts it as unreadable")
	}
	if (GCProtectedLabels(nil)).Protects(&types.Issue{Labels: []string{"gt:merge-request"}}) {
		t.Error("an empty set protects nothing — resolvers are what supply the defaults")
	}
}

// TestFilterSweepCandidatesHoldsBackProtectedLabels is the row-count assertion
// the protection is for: a CLOSED merge-request record and a closed bead of
// the same age and status go in, and only the unprotected one comes out.
func TestFilterSweepCandidatesHoldsBackProtectedLabels(t *testing.T) {
	closedAt := time.Date(2026, 8, 4, 6, 46, 0, 0, time.UTC)
	cutoff := closedAt.Add(time.Hour)

	// Same status, same closed_at, same tier — the ONLY difference is the
	// label. Without the negative control, a filter that returned nothing at
	// all would pass this test.
	control := &types.Issue{ID: "sw-plain", Status: types.StatusClosed, ClosedAt: &closedAt}
	mergeRequest := &types.Issue{
		ID: "sw-mr", Status: types.StatusClosed, ClosedAt: &closedAt,
		Labels: []string{"gt:merge-request"},
	}
	message := &types.Issue{
		ID: "sw-msg", Status: types.StatusClosed, ClosedAt: &closedAt,
		Labels: []string{"read", "gt:message", "delivery:acked"},
	}

	kept, skips := FilterSweepCandidates(
		[]*types.Issue{control, mergeRequest, message}, "", &cutoff,
		ResolveGCProtectedLabels("", nil))

	if len(kept) != 1 || kept[0].ID != control.ID {
		t.Fatalf("kept = %v, want only %s", sweepCandidateIDs(kept), control.ID)
	}
	if skips.LabelProtected != 2 {
		t.Errorf("skips.LabelProtected = %d, want 2", skips.LabelProtected)
	}
	// A protected bead is a PROTECTION, not a defense firing: counting it in
	// the closed_at buckets would make an ordinary sweep look self-inconsistent.
	if defense := SweepDefenseSkips(skips); defense != 0 {
		t.Errorf("SweepDefenseSkips = %d, want 0", defense)
	}
	if skips.Pinned != 0 {
		t.Errorf("skips.Pinned = %d, want 0", skips.Pinned)
	}
}

// TestFilterSweepCandidatesProtectsRegardlessOfCutoff pins what makes this
// protection different from an age window: the record is held back at any age,
// because the class it names is destroyed precisely by being finished with.
func TestFilterSweepCandidatesProtectsRegardlessOfCutoff(t *testing.T) {
	ancient := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	cutoff := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)

	kept, skips := FilterSweepCandidates([]*types.Issue{
		{ID: "old-plain", Status: types.StatusClosed, ClosedAt: &ancient},
		{ID: "old-mr", Status: types.StatusClosed, ClosedAt: &ancient, Labels: []string{"gt:merge-request"}},
	}, "", &cutoff, ResolveGCProtectedLabels("", nil))

	if len(kept) != 1 || kept[0].ID != "old-plain" {
		t.Fatalf("kept = %v, want only old-plain — no age makes a merge-request record sweepable", sweepCandidateIDs(kept))
	}
	if skips.LabelProtected != 1 {
		t.Errorf("skips.LabelProtected = %d, want 1", skips.LabelProtected)
	}
}

// TestFilterSweepCandidatesPatternStillNarrowsFirst extends the documented
// ordering to the new bucket: a protected bead the pattern excluded was never
// a candidate, so it is not counted as protected.
func TestFilterSweepCandidatesPatternStillNarrowsFirst(t *testing.T) {
	closedAt := time.Date(2026, 8, 4, 6, 46, 0, 0, time.UTC)

	kept, skips := FilterSweepCandidates([]*types.Issue{
		{ID: "keep-plain", Status: types.StatusClosed, ClosedAt: &closedAt},
		{ID: "keep-mr", Status: types.StatusClosed, ClosedAt: &closedAt, Labels: []string{"gt:merge-request"}},
		{ID: "other-mr", Status: types.StatusClosed, ClosedAt: &closedAt, Labels: []string{"gt:merge-request"}},
	}, "keep-*", nil, ResolveGCProtectedLabels("", nil))

	if len(kept) != 1 || kept[0].ID != "keep-plain" {
		t.Fatalf("kept = %v, want only keep-plain", sweepCandidateIDs(kept))
	}
	if skips.LabelProtected != 1 {
		t.Errorf("skips.LabelProtected = %d, want 1 — only the protected bead the pattern ADMITTED", skips.LabelProtected)
	}
}
