package workapi

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/steveyegge/beads/internal/types"
	"github.com/steveyegge/beads/issueops"
)

type statusSearcherStub struct {
	issues []*types.Issue
	err    error
	got    types.IssueFilter
	calls  int
}

func (s *statusSearcherStub) SearchIssues(_ context.Context, _ string, filter types.IssueFilter) ([]*types.Issue, error) {
	s.calls++
	s.got = filter
	if s.err != nil {
		return nil, s.err
	}
	return s.issues, nil
}

func noticeCfg() ListConfig {
	return ListConfig{CustomStatuses: []types.CustomStatus{
		{Name: "in_review", Category: types.CategoryWIP},
		{Name: "shipped", Category: types.CategoryDone},
	}}
}

func noticeFor(t *testing.T, in issueops.ListRequest) StatusNoticeContext {
	t.Helper()
	cfg := noticeCfg()
	filter, err := BuildListFilter(in, cfg)
	if err != nil {
		t.Fatalf("BuildListFilter(%+v): %v", in, err)
	}
	return StatusNoticeFor(in, filter, cfg)
}

// Which listings owe the disclosure. The rule is narrow on purpose: it fires
// for a caller who selected SOME live statuses and not others — the `--status
// open` reading of "not closed" that bd-j3z is about — and stays silent for
// every listing that was not narrowed that way.
func TestStatusNoticeApplies(t *testing.T) {
	cases := []struct {
		name string
		in   issueops.ListRequest
		want bool
	}{
		{"open hides the rest of live work", issueops.ListRequest{Status: "open"}, true},
		{"a partial OR set still hides some", issueops.ListRequest{Status: "open,in_progress"}, true},
		{"a custom live status narrows too", issueops.ListRequest{Status: "in_review"}, true},
		{"open,closed still lost the live rest", issueops.ListRequest{Status: "open,closed"}, true},

		{"no selector hides nothing for its status", issueops.ListRequest{}, false},
		{"live is the bare listing", issueops.ListRequest{Status: "live"}, false},
		{"all promises every status", issueops.ListRequest{Status: "all"}, false},
		{"closed asks about finished work", issueops.ListRequest{Status: "closed"}, false},
		{"pinned is the other notice's subject", issueops.ListRequest{Status: "pinned"}, false},
		{"a complete live set drops nothing", issueops.ListRequest{
			Status: "open,in_progress,blocked,deferred,hooked,in_review",
		}, false},
		{"--ready is answered by a query this probe cannot reproduce", issueops.ListRequest{
			Status: "open", ReadyFlag: true,
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := noticeFor(t, tc.in).Applies(); got != tc.want {
				t.Fatalf("Applies() = %v, want %v", got, tc.want)
			}
		})
	}
}

// The notice names what was left out, not only how much. The count alone does
// not tell a reader what to type next.
func TestStatusNoticeNamesTheDroppedStatuses(t *testing.T) {
	got := noticeFor(t, issueops.ListRequest{Status: "open"}).Dropped()
	want := []string{"in_progress", "blocked", "deferred", "hooked", "in_review"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Dropped() = %v, want %v", got, want)
	}
}

// The probe asks about the SAME table under the SAME query: every predicate the
// caller typed is inherited, and only the status term is replaced. A probe that
// widened the scope would report beads the caller never asked about.
func TestStatusProbeFilterInheritsTheListing(t *testing.T) {
	no := false
	assignee := "beads/witness"
	then := time.Unix(1, 0).UTC()
	listing := types.IssueFilter{
		Assignee:       &assignee,
		Labels:         []string{"gt:escalation"},
		Pinned:         &no,
		CreatedAfter:   &then,
		Limit:          20,
		Offset:         40,
		MaxRows:        7,
		MaxRowsSource:  "config",
		AfterID:        "bd-abc",
		AfterCreatedAt: &then,
	}
	dropped := []types.Status{types.StatusInProgress, types.StatusHooked}
	probe := StatusProbeFilter(listing, dropped, 5000)

	if probe.Assignee != listing.Assignee || !reflect.DeepEqual(probe.Labels, listing.Labels) {
		t.Error("the caller's own predicates must be inherited")
	}
	if probe.CreatedAfter != listing.CreatedAfter {
		t.Error("the caller's date scope must be inherited")
	}
	// The pinned default is inherited, which is what keeps this notice and the
	// pinned one disjoint: a row hidden for being pinned is the other notice's
	// to report, and counting it here would blame --status for a default.
	if probe.Pinned == nil || *probe.Pinned {
		t.Error("the pinned exclusion must be inherited, not lifted")
	}
	if !reflect.DeepEqual(probe.Statuses, dropped) || probe.Status != nil {
		t.Errorf("Statuses = %v / Status = %v, want the dropped set", probe.Statuses, probe.Status)
	}

	// Pagination belongs to the caller's page, not to their question.
	if probe.Limit != 5000 || probe.Offset != 0 || probe.MaxRows != 0 ||
		probe.MaxRowsSource != "" || probe.AfterID != "" || probe.AfterCreatedAt != nil {
		t.Errorf("pagination and caps must be dropped, got %+v", probe)
	}
	if !probe.SkipLabels || !probe.SkipCounts || !probe.Lite || probe.IncludeDependencies {
		t.Error("the probe counts rows and renders none; projection must be off")
	}

	// The listing's own slice is left alone: the notice runs alongside a query,
	// not instead of it.
	if listing.Status != nil || listing.Statuses != nil || listing.Offset != 40 {
		t.Error("the probe edited the caller's filter in place")
	}
}

// A single dropped status uses the singular Status, the form every backend has
// always implemented, rather than a one-element OR set.
func TestStatusProbeFilterUsesSingularForOne(t *testing.T) {
	probe := StatusProbeFilter(types.IssueFilter{}, []types.Status{types.StatusHooked}, 10)
	if probe.Status == nil || *probe.Status != types.StatusHooked {
		t.Fatalf("Status = %v, want %q", probe.Status, types.StatusHooked)
	}
	if probe.Statuses != nil {
		t.Fatalf("Statuses = %v, want nil", probe.Statuses)
	}
}

func TestStatusNoticeCountHidden(t *testing.T) {
	listing := noticeFor(t, issueops.ListRequest{Status: "open"})

	t.Run("counts the rows the probe returned", func(t *testing.T) {
		s := &statusSearcherStub{issues: []*types.Issue{{ID: "bd-1"}, {ID: "bd-2"}}}
		got, err := listing.CountHidden(context.Background(), s, 5000)
		if err != nil {
			t.Fatalf("CountHidden: %v", err)
		}
		if got != 2 {
			t.Fatalf("CountHidden = %d, want 2", got)
		}
	})

	// "the probe ran and found none" and "the probe could not run" are
	// different answers; folding the second into a zero is the substitution
	// this whole probe exists to stop.
	t.Run("no store is an error, not a zero", func(t *testing.T) {
		if _, err := listing.CountHidden(context.Background(), nil, 5000); !errors.Is(err, ErrNoStatusSearcher) {
			t.Fatalf("CountHidden(nil) error = %v, want ErrNoStatusSearcher", err)
		}
	})

	t.Run("a store error is returned", func(t *testing.T) {
		boom := errors.New("boom")
		if _, err := listing.CountHidden(context.Background(), &statusSearcherStub{err: boom}, 5000); !errors.Is(err, boom) {
			t.Fatalf("CountHidden error = %v, want %v", err, boom)
		}
	})

	// A listing that owes no disclosure must not pay for a query.
	t.Run("a listing that owes nothing does not query", func(t *testing.T) {
		s := &statusSearcherStub{}
		got, err := noticeFor(t, issueops.ListRequest{Status: "live"}).CountHidden(context.Background(), s, 5000)
		if err != nil || got != 0 {
			t.Fatalf("CountHidden = (%d, %v), want (0, nil)", got, err)
		}
		if s.calls != 0 {
			t.Fatalf("probe ran %d times on a listing that owes no notice", s.calls)
		}
	})
}

// The zero value is a listing nothing is known about — the state the proxied
// route is in when it could not open a unit of work. It owes no disclosure
// rather than an invented one.
func TestStatusNoticeZeroValueOwesNothing(t *testing.T) {
	var c StatusNoticeContext
	if c.Applies() {
		t.Error("a listing nothing is known about owes no disclosure")
	}
	if len(c.Dropped()) != 0 {
		t.Errorf("Dropped() = %v, want empty", c.Dropped())
	}
}
