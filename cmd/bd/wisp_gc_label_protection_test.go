package main

import (
	"context"
	"testing"
	"time"

	"github.com/steveyegge/beads/internal/types"
	"github.com/steveyegge/beads/internal/workapi"
)

// The wisp GC's two sweeps — the age sweep (findAbandonedWisps) and the closed
// purge (filterClosedWispPurgeCandidates) — decide which rows die, so these
// cases assert WHICH ROWS SURVIVE rather than that a guard is present.
//
// EVERY CASE CARRIES A NEGATIVE CONTROL of the same age, status and plane as
// its protected bead. The guard this replaces was inert (its predicate matched
// no rows anywhere) and passed a suite that asserted only its presence; a
// same-shape control is what makes "the merge request survived" evidence of
// the protection rather than of a sweep that selected nothing.

// stubMolReader answers the handful of reads the two wisp sweeps perform. It
// embeds the molReader INTERFACE so any read these cases do not stub panics
// loudly instead of quietly answering zero.
type stubMolReader struct {
	molReader
	wisps      []*types.Issue
	blocked    []*types.BlockedIssue
	config     map[string]string
	configErr  error
	infraTypes map[types.IssueType]bool
	dependents map[string]bool
}

func (s *stubMolReader) SearchIssues(context.Context, string, types.IssueFilter) ([]*types.Issue, error) {
	return s.wisps, nil
}

func (s *stubMolReader) GetBlockedIssues(context.Context, types.WorkFilter) ([]*types.BlockedIssue, error) {
	return s.blocked, nil
}

func (s *stubMolReader) GetCustomStatusesDetailed(context.Context) ([]types.CustomStatus, error) {
	return nil, nil
}

func (s *stubMolReader) IsInfraTypeCtx(_ context.Context, t types.IssueType) bool {
	return s.infraTypes[t]
}

func (s *stubMolReader) GetConfig(_ context.Context, key string) (string, error) {
	if s.configErr != nil {
		return "", s.configErr
	}
	return s.config[key], nil
}

func (s *stubMolReader) FindWispDependentsRecursive(context.Context, []string) (map[string]bool, error) {
	return s.dependents, nil
}

func (s *stubMolReader) GetIssuesByIDs(_ context.Context, ids []string) ([]*types.Issue, error) {
	byID := make(map[string]*types.Issue, len(s.wisps))
	for _, issue := range s.wisps {
		byID[issue.ID] = issue
	}
	out := make([]*types.Issue, 0, len(ids))
	for _, id := range ids {
		if issue, ok := byID[id]; ok {
			out = append(out, issue)
		}
	}
	return out, nil
}

func wispIDs(issues []*types.Issue) []string {
	ids := make([]string, len(issues))
	for i, issue := range issues {
		ids[i] = issue.ID
	}
	return ids
}

// TestFindAbandonedWispsKeepsLabelProtectedRecords covers the `--age` sweep,
// which reaches OPEN wisps: an open merge-request record older than the
// threshold was previously deleted with the abandoned patrol steps beside it.
func TestFindAbandonedWispsKeepsLabelProtectedRecords(t *testing.T) {
	stale := time.Now().Add(-24 * time.Hour)
	r := &stubMolReader{wisps: []*types.Issue{
		{ID: "w-step", Status: types.StatusOpen, UpdatedAt: stale},
		{ID: "w-mr", Status: types.StatusOpen, UpdatedAt: stale, Labels: []string{"gt:merge-request"}},
		{ID: "w-mail", Status: types.StatusOpen, UpdatedAt: stale, Labels: []string{"gt:message", "read"}},
	}}

	abandoned, labelProtected, err := findAbandonedWisps(context.Background(), r, false, time.Hour, nil)
	if err != nil {
		t.Fatalf("findAbandonedWisps: %v", err)
	}
	if len(abandoned) != 1 || abandoned[0].ID != "w-step" {
		t.Fatalf("abandoned = %v, want only w-step", wispIDs(abandoned))
	}
	if labelProtected != 2 {
		t.Errorf("labelProtected = %d, want 2", labelProtected)
	}
}

// TestFindAbandonedWispsCountsOnlyAgeEligibleProtection keeps the reported
// number honest: a protected wisp INSIDE the age window was never a candidate,
// so reporting it would make the protection look like it fires every run.
func TestFindAbandonedWispsCountsOnlyAgeEligibleProtection(t *testing.T) {
	r := &stubMolReader{wisps: []*types.Issue{
		{ID: "w-step", Status: types.StatusOpen, UpdatedAt: time.Now().Add(-24 * time.Hour)},
		{ID: "w-fresh-mr", Status: types.StatusOpen, UpdatedAt: time.Now(), Labels: []string{"gt:merge-request"}},
	}}

	abandoned, labelProtected, err := findAbandonedWisps(context.Background(), r, false, time.Hour, nil)
	if err != nil {
		t.Fatalf("findAbandonedWisps: %v", err)
	}
	if len(abandoned) != 1 || abandoned[0].ID != "w-step" {
		t.Fatalf("abandoned = %v, want only w-step", wispIDs(abandoned))
	}
	if labelProtected != 0 {
		t.Errorf("labelProtected = %d, want 0 — the fresh record was not a candidate", labelProtected)
	}
}

// TestFindAbandonedWispsKeepsProtectedCascadeChildren closes the second door
// into the same delete batch: the cascade expansion adds transitive dependents
// of the abandoned set, and it ran its own protection check.
func TestFindAbandonedWispsKeepsProtectedCascadeChildren(t *testing.T) {
	stale := time.Now().Add(-24 * time.Hour)
	r := &stubMolReader{
		wisps: []*types.Issue{
			{ID: "w-root", Status: types.StatusOpen, UpdatedAt: stale},
			{ID: "w-child", Status: types.StatusOpen, UpdatedAt: stale},
			{ID: "w-child-mr", Status: types.StatusOpen, UpdatedAt: stale, Labels: []string{"gt:merge-request"}},
		},
		dependents: map[string]bool{"w-child": true, "w-child-mr": true},
	}

	abandoned, labelProtected, err := findAbandonedWisps(context.Background(), r, false, time.Hour, nil)
	if err != nil {
		t.Fatalf("findAbandonedWisps: %v", err)
	}
	for _, issue := range abandoned {
		if issue.ID == "w-child-mr" {
			t.Fatalf("abandoned = %v, want the protected dependent held back", wispIDs(abandoned))
		}
	}
	if len(abandoned) != 2 {
		t.Fatalf("abandoned = %v, want w-root and w-child", wispIDs(abandoned))
	}
	if labelProtected != 1 {
		t.Errorf("labelProtected = %d, want 1", labelProtected)
	}
}

// TestFilterClosedWispPurgeCandidatesKeepsLabelProtected is the case bd-czf
// was filed on: `bd mol wisp gc --closed --force` against a merge-request
// record that was CLOSED WITHOUT MERGING. The purge has no age window at all,
// so before this protection the record died on the next patrol cycle.
func TestFilterClosedWispPurgeCandidatesKeepsLabelProtected(t *testing.T) {
	closed := []*types.Issue{
		{ID: "w-done-step", Status: types.StatusClosed},
		{ID: "w-mr", Status: types.StatusClosed, Labels: []string{"gt:merge-request"}},
		{ID: "w-acked-mail", Status: types.StatusClosed, Labels: []string{"gt:message", "delivery:acked"}},
		{ID: "w-pinned", Status: types.StatusClosed, Pinned: true},
		{ID: "w-infra", Status: types.StatusClosed, IssueType: types.IssueType("agent")},
	}
	r := &stubMolReader{infraTypes: map[types.IssueType]bool{types.IssueType("agent"): true}}

	kept, skips := filterClosedWispPurgeCandidates(context.Background(), r, closed)

	if len(kept) != 1 || kept[0].ID != "w-done-step" {
		t.Fatalf("kept = %v, want only w-done-step", wispIDs(kept))
	}
	if skips.LabelProtected != 2 {
		t.Errorf("skips.LabelProtected = %d, want 2", skips.LabelProtected)
	}
	if skips.Pinned != 1 || skips.Infra != 1 {
		t.Errorf("skips.Pinned = %d, skips.Infra = %d, want 1 and 1 — the pre-existing protections must survive the refactor",
			skips.Pinned, skips.Infra)
	}
}

// TestFilterClosedWispPurgeCandidatesHonorsConfiguredLabels proves the
// mechanism is the workspace's to name — beads does not own the orchestration
// layer's vocabulary — and that a configured list REPLACES the defaults.
func TestFilterClosedWispPurgeCandidatesHonorsConfiguredLabels(t *testing.T) {
	closed := []*types.Issue{
		{ID: "w-mr", Status: types.StatusClosed, Labels: []string{"gt:merge-request"}},
		{ID: "w-custom", Status: types.StatusClosed, Labels: []string{"ops:receipt"}},
	}
	r := &stubMolReader{config: map[string]string{workapi.ConfigKeyGCProtectedLabels: "ops:receipt"}}

	kept, skips := filterClosedWispPurgeCandidates(context.Background(), r, closed)

	if len(kept) != 1 || kept[0].ID != "w-mr" {
		t.Fatalf("kept = %v, want only w-mr — a configured list replaces the defaults", wispIDs(kept))
	}
	if skips.LabelProtected != 1 {
		t.Errorf("skips.LabelProtected = %d, want 1", skips.LabelProtected)
	}
}

// TestFilterClosedWispPurgeCandidatesProtectsWhenTheSettingCannotBeRead is the
// fail-safe direction. A settings read that failed says nothing about whether
// a merge-request record is safe to delete, and the deletion is unrecoverable
// (wisp tables are dolt_ignored: no history, no backup, no undo).
func TestFilterClosedWispPurgeCandidatesProtectsWhenTheSettingCannotBeRead(t *testing.T) {
	closed := []*types.Issue{
		{ID: "w-done-step", Status: types.StatusClosed},
		{ID: "w-mr", Status: types.StatusClosed, Labels: []string{"gt:merge-request"}},
	}
	r := &stubMolReader{configErr: context.DeadlineExceeded}

	kept, skips := filterClosedWispPurgeCandidates(context.Background(), r, closed)

	if len(kept) != 1 || kept[0].ID != "w-done-step" {
		t.Fatalf("kept = %v, want the merge request held back on an unreadable setting", wispIDs(kept))
	}
	if skips.LabelProtected != 1 {
		t.Errorf("skips.LabelProtected = %d, want 1", skips.LabelProtected)
	}
}

// TestFindAbandonedWispsKeepsOpenEscalations is bd-724's headline case, and it
// is the age sweep rather than the closed purge because that is the one that
// reaches OPEN rows.
//
// The row shapes are copied from the town that produced the bug: no labels,
// not pinned, status open, last touched hours ago. Seven wisps in exactly this
// state were deletable by `bd mol wisp gc --age 1h`, and an open escalation is
// by definition an incident nobody has resolved. The control is a patrol step
// with the same status and the same age, so a sweep that had simply stopped
// selecting anything fails this test.
func TestFindAbandonedWispsKeepsOpenEscalations(t *testing.T) {
	stale := time.Now().Add(-24 * time.Hour)
	r := &stubMolReader{wisps: []*types.Issue{
		{ID: "hq-wisp-step", Status: types.StatusOpen, UpdatedAt: stale, WispType: types.WispTypePatrol},
		{ID: "hq-wisp-esc", Status: types.StatusOpen, UpdatedAt: stale, WispType: types.WispTypeEscalation},
	}}

	abandoned, recordProtected, err := findAbandonedWisps(context.Background(), r, false, time.Hour, nil)
	if err != nil {
		t.Fatalf("findAbandonedWisps: %v", err)
	}
	if len(abandoned) != 1 || abandoned[0].ID != "hq-wisp-step" {
		t.Fatalf("abandoned = %v, want only hq-wisp-step — an open escalation is unresolved, not abandoned", wispIDs(abandoned))
	}
	if recordProtected != 1 {
		t.Errorf("protected count = %d, want 1", recordProtected)
	}
}

// TestFindAbandonedWispsKeepsEscalationsWithLabelsConfiguredAway is the part
// that makes the guard worth having. gc.protected_labels REPLACES the defaults
// when it is set, and on the town in question it was unset against wisps that
// carry no labels at all — either way the label axis protected nothing. The
// escalation has to survive a configuration that names something else.
func TestFindAbandonedWispsKeepsEscalationsWithLabelsConfiguredAway(t *testing.T) {
	stale := time.Now().Add(-24 * time.Hour)
	r := &stubMolReader{
		wisps: []*types.Issue{
			{ID: "hq-wisp-step", Status: types.StatusOpen, UpdatedAt: stale},
			{ID: "hq-wisp-esc", Status: types.StatusOpen, UpdatedAt: stale, WispType: types.WispTypeEscalation},
		},
		config: map[string]string{workapi.ConfigKeyGCProtectedLabels: "ops:receipt"},
	}

	abandoned, _, err := findAbandonedWisps(context.Background(), r, false, time.Hour, nil)
	if err != nil {
		t.Fatalf("findAbandonedWisps: %v", err)
	}
	if len(abandoned) != 1 || abandoned[0].ID != "hq-wisp-step" {
		t.Fatalf("abandoned = %v, want the escalation held back by the kind guard, not the label setting", wispIDs(abandoned))
	}
}

// TestFilterClosedWispPurgeCandidatesKeepsEscalations covers the other sweep.
// `gt escalate` closes an escalation when it is acked or resolved, and 62 of
// the 69 escalation wisps measured on the town were already closed — so
// without this the record of every resolved incident goes on the next
// `bd mol wisp gc --closed --force`.
func TestFilterClosedWispPurgeCandidatesKeepsEscalations(t *testing.T) {
	closed := []*types.Issue{
		{ID: "hq-wisp-hb", Status: types.StatusClosed, WispType: types.WispTypeHeartbeat},
		{ID: "hq-wisp-esc", Status: types.StatusClosed, WispType: types.WispTypeEscalation},
	}
	r := &stubMolReader{}

	kept, skips := filterClosedWispPurgeCandidates(context.Background(), r, closed)

	if len(kept) != 1 || kept[0].ID != "hq-wisp-hb" {
		t.Fatalf("kept = %v, want only the heartbeat", wispIDs(kept))
	}
	if skips.LabelProtected != 1 {
		t.Errorf("skips.LabelProtected = %d, want 1", skips.LabelProtected)
	}
}
