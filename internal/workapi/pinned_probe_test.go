package workapi

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/steveyegge/beads/internal/types"
	"github.com/steveyegge/beads/issueops"
)

type pinnedSearcherStub struct {
	issues []*types.Issue
	err    error
	got    types.IssueFilter
	calls  int
}

func (s *pinnedSearcherStub) SearchIssues(_ context.Context, _ string, filter types.IssueFilter) ([]*types.Issue, error) {
	s.calls++
	s.got = filter
	if s.err != nil {
		return nil, s.err
	}
	return s.issues, nil
}

func TestPinnedExclusionArmed(t *testing.T) {
	no, yes := false, true
	if PinnedExclusionArmed(types.IssueFilter{}) {
		t.Error("a filter that never mentions pinned hid nothing")
	}
	if PinnedExclusionArmed(types.IssueFilter{Pinned: &yes}) {
		t.Error("a caller who asked for pinned rows has nothing to be told")
	}
	if !PinnedExclusionArmed(types.IssueFilter{Pinned: &no}) {
		t.Error("Pinned=false is the exclusion this probe exists for")
	}
}

// The pinned probe is the OPPOSITE of the wisp probe on inheritance: it asks
// what THIS listing hid, under THIS listing's predicates, so a probe that
// widened the scope would report rows the caller never asked to see.
func TestPinnedProbeFilterInheritsTheListingAndInvertsOnlyPinned(t *testing.T) {
	no := false
	open := types.StatusOpen
	assignee := "beads/witness"
	then := time.Unix(1, 0).UTC()
	listing := types.IssueFilter{
		Status:         &open,
		Assignee:       &assignee,
		Labels:         []string{"gt:escalation"},
		LabelPattern:   "gt:*",
		ExcludeStatus:  []types.Status{types.StatusClosed},
		CreatedAfter:   &then,
		Pinned:         &no,
		Limit:          20,
		Offset:         40,
		MaxRows:        5,
		MaxRowsSource:  "--max-rows",
		AfterCreatedAt: &then,
		AfterID:        "bd-1",
	}

	probe := PinnedProbeFilter(listing, 5000)

	if probe.Pinned == nil || !*probe.Pinned {
		t.Fatal("the probe must select exactly the rows the exclusion dropped")
	}
	if probe.Status == nil || *probe.Status != open {
		t.Error("the status scope is the caller's question and must be inherited")
	}
	if probe.Assignee == nil || *probe.Assignee != assignee {
		t.Error("every non-pinned predicate must be inherited")
	}
	if len(probe.Labels) != 1 || probe.LabelPattern != "gt:*" || len(probe.ExcludeStatus) != 1 {
		t.Errorf("the label and exclusion predicates must survive, got %+v", probe)
	}
	if probe.CreatedAfter == nil || !probe.CreatedAfter.Equal(then) {
		t.Error("date predicates must be inherited")
	}

	// Pagination and the row cap belong to the caller's PAGE, not to their
	// question: an offset would skip part of the count and MaxRows would fail a
	// probe on a store where the listing itself succeeded.
	if probe.Offset != 0 || probe.AfterCreatedAt != nil || probe.AfterID != "" {
		t.Errorf("the probe must count from the first row, got offset=%d after=%v/%q", probe.Offset, probe.AfterCreatedAt, probe.AfterID)
	}
	if probe.MaxRows != 0 || probe.MaxRowsSource != "" {
		t.Error("the probe must not inherit a cap that can fail it")
	}
	if probe.Limit != 5000 {
		t.Errorf("the probe bounds its own scan, got limit %d", probe.Limit)
	}
	if !probe.SkipLabels || !probe.SkipCounts || !probe.Lite || probe.IncludeDependencies {
		t.Error("the probe counts rows and renders none; projection must be dropped")
	}

	// The caller's own filter must come back untouched — the notice runs
	// alongside a listing, not instead of it.
	if listing.Pinned == nil || *listing.Pinned || listing.Offset != 40 || listing.Limit != 20 {
		t.Errorf("the probe must not mutate the listing's filter, got %+v", listing)
	}
}

// A bead can be pinned twice over — the boolean column and the `pinned` STATUS
// — and BuildListFilter's default excludes both independently. A probe that
// inherited the status exclusion would report a measured ZERO over rows
// carrying the very property it reports on: this notice's own failure mode,
// reproduced inside its instrument.
func TestPinnedProbeFilterDropsTheDefaultPinnedStatusExclusion(t *testing.T) {
	no := false
	listing := types.IssueFilter{
		Pinned:        &no,
		Labels:        []string{"gt:escalation"},
		ExcludeStatus: []types.Status{types.StatusClosed, types.StatusPinned},
	}

	probe := PinnedProbeFilter(listing, 100)

	for _, s := range probe.ExcludeStatus {
		if s == types.StatusPinned {
			t.Fatal("the probe cannot exclude the status it is reporting on")
		}
	}
	var keptClosed bool
	for _, s := range probe.ExcludeStatus {
		if s == types.StatusClosed {
			keptClosed = true
		}
	}
	if !keptClosed {
		t.Error("only the pinned term is dropped: a caller asking about open work is not asking about closed rows")
	}

	if len(listing.ExcludeStatus) != 2 {
		t.Errorf("the listing's own slice must not be edited in place, got %v", listing.ExcludeStatus)
	}

	// A listing with no default exclusions has none to drop.
	if got := PinnedProbeFilter(types.IssueFilter{Pinned: &no}, 100); got.ExcludeStatus != nil {
		t.Errorf("expected no exclusions, got %v", got.ExcludeStatus)
	}
}

// listingFor is the notice's view of `bd list <flags>`, built through the same
// BuildListFilter the command runs — so these cases assert the pinned default
// itself rather than this file's reading of it.
func listingFor(t *testing.T, req issueops.ListRequest) PinnedNoticeContext {
	t.Helper()
	filter, err := BuildListFilter(req, ListConfig{})
	if err != nil {
		t.Fatalf("build the listing filter: %v", err)
	}
	return PinnedNoticeFor(req, filter)
}

func TestCountHiddenDistinguishesZeroFromUnknown(t *testing.T) {
	ctx := context.Background()
	armed := listingFor(t, issueops.ListRequest{Labels: []string{"gt:escalation"}})

	if _, err := armed.CountHidden(ctx, nil, 10); !errors.Is(err, ErrNoPinnedSearcher) {
		t.Errorf("no store means the probe never ran, want ErrNoPinnedSearcher, got %v", err)
	}

	boom := errors.New("boom")
	if _, err := armed.CountHidden(ctx, &pinnedSearcherStub{err: boom}, 10); !errors.Is(err, boom) {
		t.Errorf("a failed probe is not a measured zero, got %v", err)
	}

	empty := &pinnedSearcherStub{}
	got, err := armed.CountHidden(ctx, empty, 10)
	if err != nil || got != 0 {
		t.Errorf("a probe that ran and found nothing is a real zero, got %d, %v", got, err)
	}
	if empty.got.Pinned == nil || !*empty.got.Pinned {
		t.Error("the query that reached the store must be the pinned one")
	}

	found := &pinnedSearcherStub{issues: []*types.Issue{{ID: "bd-1"}, {ID: "bd-2"}, {ID: "bd-3"}}}
	if got, err := armed.CountHidden(ctx, found, 10); err != nil || got != 3 {
		t.Errorf("expected 3 hidden rows, got %d, %v", got, err)
	}
}

// A listing with nothing to disclose must not pay for a query, and must not be
// able to render a count either. The three shapes are: the caller can already
// see pinned rows, the caller asked for the exclusion, and the query is one
// this probe does not speak for.
func TestListingsThatOweNoPinnedDisclosure(t *testing.T) {
	ctx := context.Background()
	labels := []string{"gt:escalation"}
	for name, req := range map[string]issueops.ListRequest{
		"--all":           {Labels: labels, AllFlag: true},
		"--pinned":        {Labels: labels, PinnedFlag: true},
		"--no-pinned":     {Labels: labels, NoPinnedFlag: true},
		"--ready":         {Labels: labels, ReadyFlag: true},
		"--status pinned": {Labels: labels, Status: "pinned"},
	} {
		listing := listingFor(t, req)
		if listing.Applies() {
			t.Errorf("%s: this listing owes no pinned disclosure", name)
		}
		stub := &pinnedSearcherStub{issues: []*types.Issue{{ID: "bd-1"}}}
		got, err := listing.CountHidden(ctx, stub, 10)
		if err != nil || got != 0 {
			t.Errorf("%s: want a 0 with no error, got %d, %v", name, got, err)
		}
		if stub.calls != 0 {
			t.Errorf("%s: the store must not be asked, got %d calls", name, stub.calls)
		}
	}

	// And the case they are all measured against: the plain labeled listing,
	// where the default is armed and the caller never said so.
	plain := listingFor(t, issueops.ListRequest{Labels: labels})
	if !plain.Applies() {
		t.Fatal("a plain labeled listing is the case this notice exists for")
	}
	if _, err := plain.CountHidden(ctx, &pinnedSearcherStub{}, 10); err != nil {
		t.Errorf("the probe must run for it: %v", err)
	}
}
