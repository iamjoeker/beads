package workapi

import (
	"reflect"
	"strings"
	"testing"

	"github.com/steveyegge/beads/internal/types"
	"github.com/steveyegge/beads/issueops"
)

// The equality `--status live` exists to provide: it is a bare `bd list` under
// a name, so the two must produce the SAME filter, field for field.
//
// Asserting equality against the bare listing rather than against a written-out
// expected filter is the point. A test that spelled out the exclusion set would
// pass while the two drifted apart — the bare listing could gain an exclusion
// that `live` did not — and drift is exactly what makes a documented alias
// worth less than the flag help claims (bd-j3z).
func TestStatusLiveIsTheBareListing(t *testing.T) {
	cfg := ListConfig{CustomStatuses: []types.CustomStatus{
		{Name: "in_review", Category: types.CategoryWIP},
		{Name: "shipped", Category: types.CategoryDone},
		{Name: "on_ice", Category: types.CategoryFrozen},
	}}

	for _, in := range []issueops.ListRequest{
		{},
		{Assignee: "beads/witness", Priority: intPtr(1)},
		{Labels: []string{"gt:escalation"}},
	} {
		bare, err := BuildListFilter(in, cfg)
		if err != nil {
			t.Fatalf("BuildListFilter(bare, %+v): %v", in, err)
		}
		withLive := in
		withLive.Status = "live"
		live, err := BuildListFilter(withLive, cfg)
		if err != nil {
			t.Fatalf("BuildListFilter(--status live, %+v): %v", in, err)
		}
		if !reflect.DeepEqual(bare, live) {
			t.Errorf("--status live differs from a bare listing for %+v:\n bare = %+v\n live = %+v", in, bare, live)
		}
	}
}

// The exclusion set `live` and the bare listing share. Built-in deferred is
// absent from it on purpose: its category is frozen, but the default listing
// has always shown deferred beads, and they are the ones nobody revisits — the
// worst thing for a "what work exists" question to hide (bd-j3z).
func TestLiveStatusExclusionsKeepDeferredVisible(t *testing.T) {
	cfg := ListConfig{CustomStatuses: []types.CustomStatus{
		{Name: "in_review", Category: types.CategoryWIP},
		{Name: "shipped", Category: types.CategoryDone},
		{Name: "on_ice", Category: types.CategoryFrozen},
	}}
	got := LiveStatusExclusions(cfg)
	want := []types.Status{types.StatusClosed, types.StatusPinned, "shipped", "on_ice"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("LiveStatusExclusions = %v, want %v", got, want)
	}
	for _, s := range got {
		if s == types.StatusDeferred {
			t.Fatal("deferred is excluded from the live set; the default listing shows it")
		}
	}
}

// LiveStatuses is the complement, and the two must agree: nothing may be both
// live and excluded, and every known status must be one or the other.
func TestLiveStatusesComplementsTheExclusions(t *testing.T) {
	cfg := ListConfig{CustomStatuses: []types.CustomStatus{
		{Name: "in_review", Category: types.CategoryWIP},
		{Name: "shipped", Category: types.CategoryDone},
	}}
	live := LiveStatuses(cfg)
	excluded := LiveStatusExclusions(cfg)

	for _, s := range live {
		for _, e := range excluded {
			if s == e {
				t.Errorf("%q is both live and excluded", s)
			}
		}
	}

	want := []types.Status{
		types.StatusOpen, types.StatusInProgress, types.StatusBlocked,
		types.StatusDeferred, types.StatusHooked, "in_review",
	}
	if !reflect.DeepEqual(live, want) {
		t.Fatalf("LiveStatuses = %v, want %v", live, want)
	}
}

// `live` names a set, so combining it with a status is a caller error rather
// than a filter that quietly means one or the other. The message says which
// selector is the problem, because "invalid status" would contradict the flag
// help that told them to type it.
func TestStatusLiveCannotBeCombined(t *testing.T) {
	for _, selector := range []string{"live,open", "open,live", "all,live"} {
		_, err := BuildListFilter(issueops.ListRequest{Status: selector}, ListConfig{})
		if err == nil {
			t.Fatalf("--status %q unexpectedly succeeded", selector)
		}
		if !strings.Contains(err.Error(), "cannot be combined with other statuses") {
			t.Errorf("--status %q: %v, want a cannot-be-combined error", selector, err)
		}
	}
}

// A custom status literally named "live" is a status, not the selector, and it
// must keep working — the set names are checked only after validation fails.
func TestCustomStatusNamedLiveStillFilters(t *testing.T) {
	cfg := ListConfig{CustomStatuses: []types.CustomStatus{{Name: "live", Category: types.CategoryWIP}}}
	filter, err := BuildListFilter(issueops.ListRequest{Status: "live,open"}, cfg)
	if err != nil {
		t.Fatalf("BuildListFilter: %v", err)
	}
	want := []types.Status{"live", types.StatusOpen}
	if !reflect.DeepEqual(filter.Statuses, want) {
		t.Fatalf("Statuses = %v, want %v", filter.Statuses, want)
	}
}

// The single-part path is reachable from `bd search`, which implements neither
// set selector. It says so rather than calling them nonexistent statuses: a
// caller who typed one learned it from `bd list`, and "invalid status" would
// send them looking for a typo they did not make.
func TestSetSelectorsRejectedByName(t *testing.T) {
	for _, selector := range []string{"live", "all"} {
		var filter types.IssueFilter
		err := ApplyStatusFilter(&filter, selector, nil)
		if err == nil {
			t.Fatalf("ApplyStatusFilter(%q) unexpectedly succeeded", selector)
		}
		if !strings.Contains(err.Error(), "`bd list` selector") {
			t.Errorf("ApplyStatusFilter(%q): %v, want it named as a bd list selector", selector, err)
		}
	}
}

func intPtr(v int) *int { return &v }
