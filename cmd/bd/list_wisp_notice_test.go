//go:build cgo

package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/steveyegge/beads/internal/types"
	"github.com/steveyegge/beads/internal/workapi"
)

type wispSearcherStub struct {
	issues []*types.Issue
	err    error
	got    types.IssueFilter
}

func (s *wispSearcherStub) SearchIssues(_ context.Context, _ string, filter types.IssueFilter) ([]*types.Issue, error) {
	s.got = filter
	if s.err != nil {
		return nil, s.err
	}
	return s.issues, nil
}

func labelPredicates(labels ...string) listLabelPredicates {
	return listLabelPredicates{Labels: labels}
}

func TestWispOnlyLabelsAmong(t *testing.T) {
	got := wispOnlyLabelsAmong([]string{"  GT:Merge-Request ", "tech-debt"}, []string{"gt:merge-request", "gt:message"})
	want := []string{"gt:merge-request", "gt:message"}
	if len(got) != len(want) {
		t.Fatalf("expected %v, got %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("expected %v (normalized, deduped, sorted), got %v", want, got)
		}
	}
	if len(wispOnlyLabelsAmong([]string{"tech-debt"})) != 0 {
		t.Error("an ordinary label is not wisp-only")
	}
}

func TestListLabelPredicateTermsCoverPatternAndRegex(t *testing.T) {
	if _, ok := (listLabelPredicates{}).terms(); ok {
		t.Error("no label predicate means no notice")
	}
	for name, p := range map[string]listLabelPredicates{
		"labels":    {Labels: []string{"a"}},
		"labelsAny": {LabelsAny: []string{"a"}},
		"pattern":   {Pattern: "gt:*"},
		"regex":     {Regex: "^gt:"},
	} {
		terms, ok := p.terms()
		if !ok {
			t.Errorf("%s selects on labels and must be treated as a label filter", name)
		}
		if len(terms) != 1 {
			t.Errorf("%s must contribute its term to the message, got %v", name, terms)
		}
	}
}

// The probe must NOT inherit the list query's status scope: a merged
// merge-request wisp is closed, so an open-only probe would reproduce the very
// filter that manufactures the false zero.
func TestWispProbeFilterDropsStatusAndTargetsWisps(t *testing.T) {
	probe := workapi.WispLabelProbeFilter([]string{"gt:merge-request"}, nil, "gt:*", "", wispListRowCap)

	if probe.Ephemeral == nil || !*probe.Ephemeral {
		t.Fatal("the probe must be routed to the wisps table")
	}
	if probe.Status != nil || len(probe.Statuses) != 0 {
		t.Error("the probe must carry no status scope")
	}
	if len(probe.Labels) != 1 || probe.LabelPattern != "gt:*" {
		t.Errorf("the probe must carry the label predicates, got %+v", probe)
	}
	if probe.Assignee != nil {
		t.Error("the probe asks one question — which labels wisps carry — and inherits nothing else")
	}
}

func TestCountMatchingWispsDistinguishesZeroFromUnknown(t *testing.T) {
	if got := countMatchingWisps(context.Background(), nil, labelPredicates("x")); got != unknownIssueCount {
		t.Errorf("no store means the probe never ran, want %d, got %d", unknownIssueCount, got)
	}
	failing := &wispSearcherStub{err: errors.New("boom")}
	if got := countMatchingWisps(context.Background(), failing, labelPredicates("x")); got != unknownIssueCount {
		t.Errorf("a failed probe is not a measured zero, want %d, got %d", unknownIssueCount, got)
	}
	empty := &wispSearcherStub{}
	if got := countMatchingWisps(context.Background(), empty, labelPredicates("x")); got != 0 {
		t.Errorf("a probe that ran and found nothing is a real zero, got %d", got)
	}
	if empty.got.Ephemeral == nil || !*empty.got.Ephemeral {
		t.Error("the probe that reached the store must be the wisps-table one")
	}
	found := &wispSearcherStub{issues: []*types.Issue{{ID: "w-1"}, {ID: "w-2"}}}
	if got := countMatchingWisps(context.Background(), found, labelPredicates("x")); got != 2 {
		t.Errorf("expected 2, got %d", got)
	}
}

func TestEmptyLabelledListNoticeLines(t *testing.T) {
	store := `database "hq" at /town/.beads`

	t.Run("wisps carry the label", func(t *testing.T) {
		lines := strings.Join(emptyLabelledListNoticeLines(
			[]string{"gt:merge-request"}, []string{"gt:merge-request"}, 14, store), "\n")
		for _, want := range []string{"14 WISP(s)", store, "wisps table", "bd mol wisp list --all --all-stores"} {
			if !strings.Contains(lines, want) {
				t.Errorf("expected %q in:\n%s", want, lines)
			}
		}
	})

	t.Run("structural zero with no wisps either", func(t *testing.T) {
		lines := strings.Join(emptyLabelledListNoticeLines(
			[]string{"gt:message"}, []string{"gt:message"}, 0, store), "\n")
		if !strings.Contains(lines, "structural") {
			t.Errorf("a zero that cannot be nonzero must say so:\n%s", lines)
		}
		if !strings.Contains(lines, "0 matched") {
			t.Errorf("the measured zero must be reported as measured:\n%s", lines)
		}
	})

	t.Run("probe did not run", func(t *testing.T) {
		lines := strings.Join(emptyLabelledListNoticeLines(
			[]string{"gt:message"}, []string{"gt:message"}, unknownIssueCount, store), "\n")
		if strings.Contains(lines, "0 matched") {
			t.Errorf("an unrun probe must never be reported as a zero:\n%s", lines)
		}
		if !strings.Contains(lines, "not consulted") {
			t.Errorf("expected the notice to say the wisps table was not consulted:\n%s", lines)
		}
	})

	t.Run("ordinary empty result stays ordinary", func(t *testing.T) {
		if lines := emptyLabelledListNoticeLines([]string{"tech-debt"}, nil, 0, store); lines != nil {
			t.Errorf("a label nothing carries is an ordinary zero, got %v", lines)
		}
		if lines := emptyLabelledListNoticeLines(nil, nil, 0, store); lines != nil {
			t.Errorf("no label filter means no notice, got %v", lines)
		}
	})

	t.Run("unnamed store still reads as a place", func(t *testing.T) {
		lines := strings.Join(emptyLabelledListNoticeLines(
			[]string{"gt:merge-request"}, []string{"gt:merge-request"}, 3, ""), "\n")
		if !strings.Contains(lines, "in the same store") {
			t.Errorf("expected a store-agnostic phrasing, got:\n%s", lines)
		}
	})
}
