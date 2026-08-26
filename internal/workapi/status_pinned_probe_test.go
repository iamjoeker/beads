package workapi

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"testing"

	"github.com/steveyegge/beads/internal/types"
	"github.com/steveyegge/beads/issueops"
)

// The seam between the two disclosures, and the row that fell into it.
//
// `bd list` can hide a live row for two independent reasons, and each notice
// probes for exactly one of them: the pinned probe inherits the caller's STATUS
// term, and the status probe inherits the PINNED default. That kept their
// counts from overlapping — and left a row carrying BOTH properties outside
// both, reported by neither disclosure (bd-6xa). It is the same shape the two
// notices exist to end: a row that matched the table that was read, dropped by
// two defaults at once, and mentioned nowhere.
//
// The tests below pin the partition rather than the two probes separately,
// because "disjoint" was never the property worth having and asserting it row
// by row is what let the gap through. Every row a listing hides must be counted
// by exactly one probe.

// probeRow is a row as far as the three probes are concerned. Only the status
// column and the pinned flag are modelled, and that is not a simplification:
// the three probe filters are built from the SAME listing filter and differ in
// those two terms alone, so no other predicate can move a row between them.
type probeRow struct {
	status types.Status
	pinned bool
}

func (r probeRow) String() string {
	if r.pinned {
		return "pinned " + string(r.status)
	}
	return string(r.status)
}

// matches evaluates the status and pinned terms of a filter against a row, in
// the tri-state form BuildListFilter leaves them in: the singular Status, the
// Statuses OR set, the ExcludeStatus complement, and a *bool Pinned whose nil
// admits both kinds.
func (r probeRow) matches(f types.IssueFilter) bool {
	switch {
	case f.Status != nil:
		if r.status != *f.Status {
			return false
		}
	case len(f.Statuses) > 0:
		if !slices.Contains(f.Statuses, r.status) {
			return false
		}
	}
	if slices.Contains(f.ExcludeStatus, r.status) {
		return false
	}
	if f.Pinned != nil && *f.Pinned != r.pinned {
		return false
	}
	return true
}

// TestStatusAndPinnedProbesPartitionTheHiddenRows is the regression test for
// bd-6xa. Under `bd list --status open` every live row is either listed or
// counted by exactly one probe; before the intersection probe existed, the
// pinned hooked row was counted by none.
func TestStatusAndPinnedProbesPartitionTheHiddenRows(t *testing.T) {
	in := issueops.ListRequest{Status: "open"}
	var cfg ListConfig
	listing, err := BuildListFilter(in, cfg)
	if err != nil {
		t.Fatalf("BuildListFilter(%+v): %v", in, err)
	}
	notice := StatusNoticeFor(in, listing, cfg)
	dropped := slices.Clone(notice.dropped)

	probes := []struct {
		name   string
		filter types.IssueFilter
	}{
		{"listed", listing},
		{"pinned notice", PinnedProbeFilter(listing, 5000)},
		{"status notice", StatusProbeFilter(listing, dropped, 5000)},
		{"status+pinned notice", StatusPinnedProbeFilter(listing, dropped, 5000)},
	}

	// Every live row, in both pinned states, plus the closed rows that must
	// stay outside every count.
	var rows []probeRow
	for _, s := range LiveStatuses(cfg) {
		rows = append(rows, probeRow{status: s}, probeRow{status: s, pinned: true})
	}
	rows = append(rows, probeRow{status: types.StatusClosed}, probeRow{status: types.StatusClosed, pinned: true})

	for _, row := range rows {
		var by []string
		for _, p := range probes {
			if row.matches(p.filter) {
				by = append(by, p.name)
			}
		}
		want := 1
		if row.status == types.StatusClosed {
			// A closed row is not live work; no disclosure claims it, and one
			// that did would be reporting finished beads as hidden work.
			want = 0
		}
		if len(by) != want {
			t.Errorf("%s is accounted for by %d of the four queries (%v), want %d", row, len(by), by, want)
		}
	}
}

// The one row the gap was about, named on its own so a regression reads as the
// defect rather than as an arithmetic mismatch.
func TestPinnedHookedRowIsCountedByTheIntersectionProbe(t *testing.T) {
	in := issueops.ListRequest{Status: "open"}
	var cfg ListConfig
	listing, err := BuildListFilter(in, cfg)
	if err != nil {
		t.Fatalf("BuildListFilter: %v", err)
	}
	dropped := slices.Clone(StatusNoticeFor(in, listing, cfg).dropped)
	row := probeRow{status: types.StatusHooked, pinned: true}

	if row.matches(listing) {
		t.Fatal("the listing itself would have shown the row; there is no gap to close")
	}
	if row.matches(PinnedProbeFilter(listing, 5000)) {
		t.Error("the pinned probe inherits the caller's status term and cannot reach a hooked row")
	}
	if row.matches(StatusProbeFilter(listing, dropped, 5000)) {
		t.Error("the status probe inherits the pinned default and cannot reach a pinned row")
	}
	if !row.matches(StatusPinnedProbeFilter(listing, dropped, 5000)) {
		t.Error("a pinned hooked row is hidden twice over and must be counted by the intersection probe")
	}
}

// The intersection probe is the status probe's own filter with the pinned term
// turned the other way up, so the two counts are taken over the same scope and
// add up to the whole rather than to something narrower.
func TestStatusPinnedProbeFilterDiffersOnlyInThePinnedTerm(t *testing.T) {
	no := false
	assignee := "beads/witness"
	listing := types.IssueFilter{
		Assignee: &assignee,
		Labels:   []string{"gt:escalation"},
		Pinned:   &no,
		Offset:   40,
	}
	dropped := []types.Status{types.StatusInProgress, types.StatusHooked}

	base := StatusProbeFilter(listing, dropped, 5000)
	pinned := StatusPinnedProbeFilter(listing, dropped, 5000)

	if pinned.Pinned == nil || !*pinned.Pinned {
		t.Fatalf("Pinned = %v, want true", pinned.Pinned)
	}
	// Compared with the pinned term equalized, so any second divergence fails
	// here instead of being discovered as a count that does not add up.
	pinned.Pinned = base.Pinned
	if !reflect.DeepEqual(base, pinned) {
		t.Errorf("the intersection probe differs from the status probe by more than the pinned term:\n base   = %+v\n pinned = %+v", base, pinned)
	}

	// The `pinned` STATUS exclusion is left in place, unlike in
	// PinnedProbeFilter: the status term here is a positive selection of LIVE
	// statuses, and `pinned` is not one of them, so a row carrying that status
	// cannot enter this count from either direction.
	if !reflect.DeepEqual(pinned.ExcludeStatus, listing.ExcludeStatus) {
		t.Errorf("ExcludeStatus = %v, want the listing's own %v", pinned.ExcludeStatus, listing.ExcludeStatus)
	}
	if listing.Offset != 40 || listing.Pinned == nil || *listing.Pinned {
		t.Error("the probe edited the caller's filter in place")
	}
}

// Which listings owe the doubly-hidden disclosure. It is the status notice's
// rule AND the pinned notice's rule, read from the same request so the two
// cannot come to disagree about who owes what.
func TestAppliesToPinned(t *testing.T) {
	cases := []struct {
		name string
		in   issueops.ListRequest
		want bool
	}{
		{"open under the pinned default", issueops.ListRequest{Status: "open"}, true},
		{"a partial OR set narrows both ways", issueops.ListRequest{Status: "open,blocked"}, true},

		// --status hooked and --status pinned lift the pinned default
		// themselves (statusSelectsPinned), so nothing is hidden twice.
		{"hooked lifts the pinned default", issueops.ListRequest{Status: "hooked"}, false},
		{"--pinned asked for those rows", issueops.ListRequest{Status: "open", PinnedFlag: true}, false},
		{"--all admits both kinds", issueops.ListRequest{Status: "open", AllFlag: true}, false},

		// A caller who typed --no-pinned excluded those rows on purpose, and is
		// not being kept in the dark by a default — the same line
		// PinnedNoticeContext draws.
		{"--no-pinned is the caller's own choice", issueops.ListRequest{Status: "open", NoPinnedFlag: true}, false},

		// Everything the status notice itself stays silent about.
		{"a bare listing narrows nothing", issueops.ListRequest{}, false},
		{"live is the bare listing", issueops.ListRequest{Status: "live"}, false},
		{"closed asks about finished work", issueops.ListRequest{Status: "closed"}, false},
		{"--ready is answered by another query", issueops.ListRequest{Status: "open", ReadyFlag: true}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := noticeFor(t, tc.in).AppliesToPinned(); got != tc.want {
				t.Fatalf("AppliesToPinned() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestStatusNoticeCountHiddenPinned(t *testing.T) {
	listing := noticeFor(t, issueops.ListRequest{Status: "open"})

	t.Run("counts the rows the probe returned", func(t *testing.T) {
		s := &statusSearcherStub{issues: []*types.Issue{{ID: "bd-1"}, {ID: "bd-2"}}}
		got, err := listing.CountHiddenPinned(context.Background(), s, 5000)
		if err != nil {
			t.Fatalf("CountHiddenPinned: %v", err)
		}
		if got != 2 {
			t.Fatalf("CountHiddenPinned = %d, want 2", got)
		}
		if s.got.Pinned == nil || !*s.got.Pinned {
			t.Errorf("the probe asked with Pinned = %v, want true", s.got.Pinned)
		}
	})

	// "the probe ran and found none" and "the probe could not run" are
	// different answers here for the reason they are everywhere else: a zero
	// substituted for the second hides exactly the rows this count exists to
	// disclose.
	t.Run("no store is an error, not a zero", func(t *testing.T) {
		if _, err := listing.CountHiddenPinned(context.Background(), nil, 5000); !errors.Is(err, ErrNoStatusSearcher) {
			t.Fatalf("CountHiddenPinned(nil) error = %v, want ErrNoStatusSearcher", err)
		}
	})

	t.Run("a store error is returned", func(t *testing.T) {
		boom := errors.New("boom")
		if _, err := listing.CountHiddenPinned(context.Background(), &statusSearcherStub{err: boom}, 5000); !errors.Is(err, boom) {
			t.Fatalf("CountHiddenPinned error = %v, want %v", err, boom)
		}
	})

	// A listing with no gap under it must not pay for a query.
	t.Run("a listing with no gap does not query", func(t *testing.T) {
		s := &statusSearcherStub{}
		got, err := noticeFor(t, issueops.ListRequest{Status: "open", PinnedFlag: true}).
			CountHiddenPinned(context.Background(), s, 5000)
		if err != nil || got != 0 {
			t.Fatalf("CountHiddenPinned = (%d, %v), want (0, nil)", got, err)
		}
		if s.calls != 0 {
			t.Fatalf("probe ran %d times on a listing with nothing hidden twice", s.calls)
		}
	})
}

// The zero value is a listing nothing is known about — the state the proxied
// route is in when it could not open a unit of work. It owes no disclosure
// rather than an invented one.
func TestStatusNoticeZeroValueOwesNoPinnedDisclosure(t *testing.T) {
	var c StatusNoticeContext
	if c.AppliesToPinned() {
		t.Error("a listing nothing is known about owes no disclosure")
	}
	got, err := c.CountHiddenPinned(context.Background(), nil, 5000)
	if got != 0 || err != nil {
		t.Errorf("CountHiddenPinned = (%d, %v), want (0, nil)", got, err)
	}
}
