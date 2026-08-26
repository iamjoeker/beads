//go:build cgo

package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/steveyegge/beads/internal/types"
	"github.com/steveyegge/beads/internal/workapi"
	"github.com/steveyegge/beads/issueops"
)

// pinnedExcludingListing is the shape `bd list --label <labels>` reaches the
// notice in: the pinned exclusion armed by the default, and nothing the caller
// typed to arm it. It is built through the same BuildListFilter the command
// runs, so a change to when the default applies moves this fixture with it
// instead of leaving a hand-written filter asserting the old rule.
func pinnedExcludingListing(t *testing.T, labels ...string) workapi.PinnedNoticeContext {
	t.Helper()
	return listingFor(t, issueops.ListRequest{Labels: labels})
}

func listingFor(t *testing.T, req issueops.ListRequest) workapi.PinnedNoticeContext {
	t.Helper()
	filter, err := workapi.BuildListFilter(req, workapi.ListConfig{})
	if err != nil {
		t.Fatalf("build the listing filter: %v", err)
	}
	return workapi.PinnedNoticeFor(req, filter)
}

func TestCountHiddenPinnedDistinguishesZeroFromUnknown(t *testing.T) {
	ctx := context.Background()
	listing := pinnedExcludingListing(t, "gt:escalation")

	if got := countHiddenPinned(ctx, nil, listing); got != unknownIssueCount {
		t.Errorf("no store means the probe never ran, want %d, got %d", unknownIssueCount, got)
	}
	failing := &wispSearcherStub{err: errors.New("boom")}
	if got := countHiddenPinned(ctx, failing, listing); got != unknownIssueCount {
		t.Errorf("a failed probe is not a measured zero, want %d, got %d", unknownIssueCount, got)
	}
	empty := &wispSearcherStub{}
	if got := countHiddenPinned(ctx, empty, listing); got != 0 {
		t.Errorf("a probe that ran and found nothing is a real zero, got %d", got)
	}
	found := &wispSearcherStub{issues: []*types.Issue{{ID: "bd-1"}, {ID: "bd-2"}, {ID: "bd-3"}}}
	if got := countHiddenPinned(ctx, found, listing); got != 3 {
		t.Errorf("expected 3, got %d", got)
	}
}

func TestHiddenPinnedNoticeLines(t *testing.T) {
	store := `database "hq" at /town/.beads`

	// The incident this bug was filed for: a labeled listing prints the ordinary
	// empty screen while the store holds pinned matches.
	t.Run("empty listing over pinned matches", func(t *testing.T) {
		lines := strings.Join(hiddenPinnedNoticeLines([]string{"gt:escalation"}, 3, 0, store), "\n")
		for _, want := range []string{`"gt:escalation"`, "3 PINNED", store, "--pinned", "--all"} {
			if !strings.Contains(lines, want) {
				t.Errorf("expected %q in:\n%s", want, lines)
			}
		}
	})

	// 1-of-4 is the same silence as 0-of-3: a short listing gives its reader no
	// more way to know rows were withheld than an empty one does.
	t.Run("short listing over pinned matches", func(t *testing.T) {
		lines := strings.Join(hiddenPinnedNoticeLines([]string{"gt:escalation"}, 3, 1, store), "\n")
		if !strings.Contains(lines, "3 further PINNED") {
			t.Errorf("a non-empty listing must report the rows it hid as further ones:\n%s", lines)
		}
		if strings.Contains(lines, "no listed issue") {
			t.Errorf("a listing that returned rows must not be described as empty:\n%s", lines)
		}
	})

	t.Run("a probe that could not run says nothing", func(t *testing.T) {
		if lines := hiddenPinnedNoticeLines([]string{"gt:escalation"}, unknownIssueCount, 0, store); lines != nil {
			t.Errorf("an unrun probe must never be rendered as a count, got %v", lines)
		}
	})

	t.Run("an ordinary listing stays ordinary", func(t *testing.T) {
		if lines := hiddenPinnedNoticeLines([]string{"tech-debt"}, 0, 0, store); lines != nil {
			t.Errorf("a listing that hid nothing has nothing to disclose, got %v", lines)
		}
		if lines := hiddenPinnedNoticeLines(nil, 3, 0, store); lines != nil {
			t.Errorf("no label filter means no notice, got %v", lines)
		}
	})

	// At the cap the scan stopped counting, so the number is a floor and saying
	// it flat would overstate what was measured.
	t.Run("a capped count is reported as a floor", func(t *testing.T) {
		lines := strings.Join(hiddenPinnedNoticeLines([]string{"gt:escalation"}, pinnedProbeRowCap, 0, store), "\n")
		if !strings.Contains(lines, "at least") {
			t.Errorf("a count that reached the cap is a floor:\n%s", lines)
		}
	})

	t.Run("unnamed store still reads as a place", func(t *testing.T) {
		lines := strings.Join(hiddenPinnedNoticeLines([]string{"gt:escalation"}, 3, 0, ""), "\n")
		if !strings.Contains(lines, "in the same store") {
			t.Errorf("expected a store-agnostic phrasing, got:\n%s", lines)
		}
	})
}

func TestPrintHiddenPinnedNoticeFiresOnlyWhenTheDefaultHidSomething(t *testing.T) {
	ctx := context.Background()
	hidden := func() *wispSearcherStub {
		return &wispSearcherStub{issues: []*types.Issue{{ID: "bd-1"}}}
	}
	labelled := []string{"gt:escalation"}

	// Every case is the flags a caller actually typed, put through the same
	// BuildListFilter the command runs. A hand-set filter would assert this
	// file's reading of the pinned default rather than the default itself.
	cases := map[string]struct {
		predicates listLabelPredicates
		request    issueops.ListRequest
		want       bool
		why        string
	}{
		"armed exclusion with hidden rows": {
			predicates: labelPredicates("gt:escalation"),
			request:    issueops.ListRequest{Labels: labelled},
			want:       true,
			why:        "the default hid rows the caller asked for",
		},
		"caller already sees pinned rows": {
			predicates: labelPredicates("gt:escalation"),
			request:    issueops.ListRequest{Labels: labelled, PinnedFlag: true},
			why:        "--pinned hides nothing, so there is nothing to disclose",
		},
		"caller asked for the exclusion": {
			predicates: labelPredicates("gt:escalation"),
			request:    issueops.ListRequest{Labels: labelled, NoPinnedFlag: true},
			why:        "--no-pinned is the caller's own choice, not a default keeping them in the dark",
		},
		"ready listing": {
			predicates: labelPredicates("gt:escalation"),
			request:    issueops.ListRequest{Labels: labelled, ReadyFlag: true},
			why:        "GetReadyWork answered, and this probe cannot reproduce that query",
		},
		"no label filter": {
			predicates: listLabelPredicates{},
			request:    issueops.ListRequest{},
			why:        "an unlabeled listing gets no notice",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var printed bool
			out := captureStderr(t, func() {
				printed = printHiddenPinnedNotice(ctx, hidden(), tc.predicates, listingFor(t, tc.request), 0, "")
			})
			if printed != tc.want {
				t.Errorf("%s: printed=%v, want %v (stderr: %q)", tc.why, printed, tc.want, out)
			}
			if tc.want && !strings.Contains(out, "PINNED") {
				t.Errorf("expected the disclosure on stderr, got %q", out)
			}
			if !tc.want && out != "" {
				t.Errorf("%s: expected silence, got %q", tc.why, out)
			}
		})
	}
}

// The wisp notice's headline is "no ISSUE carries <label>", which a hidden
// pinned match makes false: the issues exist, in the table that was read.
// Naming the wisp plane would send a reader looking in the wrong place.
func TestPinnedNoticeSuppressesTheWispNotice(t *testing.T) {
	ctx := context.Background()
	// One row satisfies both probes — the wisp probe counts it as a wisp, the
	// pinned probe as a hidden issue — so the two notices are in direct
	// competition and the ordering is what the test observes.
	both := &wispSearcherStub{issues: []*types.Issue{{ID: "x-1"}}}

	out := captureStderr(t, func() {
		printLabelledListNotices(ctx, both, labelPredicates("gt:merge-request"),
			pinnedExcludingListing(t, "gt:merge-request"), 0, "")
	})
	if !strings.Contains(out, "PINNED") {
		t.Errorf("the nearer explanation must be the one printed:\n%s", out)
	}
	if strings.Contains(out, "no ISSUE carries") {
		t.Errorf("the wisp notice's headline is false when issues were hidden:\n%s", out)
	}

	// With nothing hidden, the wisp notice is still the right answer and must
	// not have been displaced.
	noneHidden := &countingSearcherStub{whenPinned: nil, otherwise: []*types.Issue{{ID: "w-1"}}}
	out = captureStderr(t, func() {
		printLabelledListNotices(ctx, noneHidden, labelPredicates("gt:merge-request"),
			pinnedExcludingListing(t, "gt:merge-request"), 0, "")
	})
	if !strings.Contains(out, "WISP(s)") {
		t.Errorf("a listing that hid no pinned row still owes the wisp disclosure:\n%s", out)
	}
}

// countingSearcherStub answers the two probes differently, so a test can put a
// store in a state only one of them reports on.
type countingSearcherStub struct {
	whenPinned []*types.Issue
	otherwise  []*types.Issue
}

func (s *countingSearcherStub) SearchIssues(_ context.Context, _ string, filter types.IssueFilter) ([]*types.Issue, error) {
	if filter.Pinned != nil && *filter.Pinned {
		return s.whenPinned, nil
	}
	return s.otherwise, nil
}
