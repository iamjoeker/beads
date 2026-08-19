package workapi

import (
	"context"
	"errors"
	"testing"

	"github.com/steveyegge/beads/internal/types"
)

type probeSearcherStub struct {
	issues []*types.Issue
	err    error
	got    types.IssueFilter
}

func (s *probeSearcherStub) SearchIssues(_ context.Context, _ string, filter types.IssueFilter) ([]*types.Issue, error) {
	s.got = filter
	if s.err != nil {
		return nil, s.err
	}
	return s.issues, nil
}

func TestWispLabelProbeFilterTargetsWispsWithNoStatusScope(t *testing.T) {
	filter := WispLabelProbeFilter([]string{"gt:merge-request"}, []string{"gt:message"}, "gt:*", "^gt:", 5000)

	if filter.Ephemeral == nil || !*filter.Ephemeral {
		t.Fatal("the probe must be routed to the wisps table")
	}
	// A merged merge-request wisp is closed, so any status scope here would
	// reproduce the filter that manufactures the false zero this probe exists
	// to explain.
	if filter.Status != nil || len(filter.Statuses) != 0 || len(filter.ExcludeStatus) != 0 {
		t.Errorf("the probe must carry no status scope, got %+v", filter)
	}
	if len(filter.Labels) != 1 || len(filter.LabelsAny) != 1 || filter.LabelPattern != "gt:*" || filter.LabelRegex != "^gt:" {
		t.Errorf("the probe must carry every label predicate, got %+v", filter)
	}
	if filter.Limit != 5000 {
		t.Errorf("the probe must honor the caller's row cap, got %d", filter.Limit)
	}
}

func TestHasLabelPredicate(t *testing.T) {
	if HasLabelPredicate(nil, nil, "", "") {
		t.Error("no predicate is no predicate")
	}
	for name, ok := range map[string]bool{
		"labels":    HasLabelPredicate([]string{"a"}, nil, "", ""),
		"labelsAny": HasLabelPredicate(nil, []string{"a"}, "", ""),
		"pattern":   HasLabelPredicate(nil, nil, "a*", ""),
		"regex":     HasLabelPredicate(nil, nil, "", "^a"),
	} {
		if !ok {
			t.Errorf("%s selects on labels", name)
		}
	}
}

func TestLabelPredicatesNamesEveryTerm(t *testing.T) {
	got := LabelPredicates([]string{"a"}, []string{"b"}, "c*", "^d")
	if len(got) != 4 {
		t.Fatalf("expected every term to be nameable in a message, got %v", got)
	}
}

// "the probe found none" and "the probe could not run" must not collapse into
// one answer: reporting the second as the first is the substitution the whole
// probe exists to prevent.
func TestCountLabelledWispsKeepsFailureApartFromZero(t *testing.T) {
	ctx := context.Background()

	if _, err := CountLabelledWisps(ctx, nil, []string{"a"}, nil, "", "", 10); !errors.Is(err, ErrNoWispSearcher) {
		t.Errorf("no store is an error, not a zero; got %v", err)
	}

	boom := errors.New("boom")
	if _, err := CountLabelledWisps(ctx, &probeSearcherStub{err: boom}, []string{"a"}, nil, "", "", 10); !errors.Is(err, boom) {
		t.Errorf("a failed read must surface its error, got %v", err)
	}

	empty := &probeSearcherStub{}
	count, err := CountLabelledWisps(ctx, empty, []string{"a"}, nil, "", "", 10)
	if err != nil || count != 0 {
		t.Errorf("a probe that ran and found nothing is a measured zero, got %d (%v)", count, err)
	}
	if empty.got.Ephemeral == nil || !*empty.got.Ephemeral {
		t.Error("the filter that reached storage must be the wisps-table one")
	}

	found := &probeSearcherStub{issues: []*types.Issue{{ID: "w-1"}, {ID: "w-2"}}}
	if count, err := CountLabelledWisps(ctx, found, []string{"a"}, nil, "", "", 10); err != nil || count != 2 {
		t.Errorf("expected 2, got %d (%v)", count, err)
	}
}
