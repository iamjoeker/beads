package workapi

import (
	"context"

	"github.com/steveyegge/beads/internal/types"
	"github.com/steveyegge/beads/issueops"
)

// The pinned-plane probe behind a listing that dropped rows it never mentioned.
//
// `bd list` excludes PINNED issues unless the caller asked for them (--pinned,
// --all, --status pinned/hooked). That default is deliberate — pinned beads are
// permanent reference rows and would otherwise clutter every listing — but it
// is applied silently, so a listing whose matches are all pinned prints the
// ordinary empty screen. A reader takes that for "the records do not exist",
// which is what happened to an escalation surface reporting "No escalations
// found" over three open, pinned escalations (bd-f76).
//
// The probe answers with a COUNT of the rows the caller's own query dropped:
// the listing's own filter with the pinned exclusion inverted. It is built from
// that filter rather than rebuilt from the request, so the count cannot come to
// describe a query the caller did not run.

// PinnedSearcher is the slice of storage this probe needs: one filtered read of
// the plane the listing itself selected.
//
// It is the same shape as WispSearcher and satisfied by the same stores, and it
// is kept separate for the reason the two probes are separate: that one asks
// about the wisps table by fixing Ephemeral, this one asks about whatever plane
// the listing's filter already chose. The two are free to diverge.
type PinnedSearcher interface {
	SearchIssues(ctx context.Context, query string, filter types.IssueFilter) ([]*types.Issue, error)
}

// PinnedExclusionArmed reports whether a listing's filter carries the pinned
// exclusion — the default that drops permanent reference rows from listings
// that did not ask for them.
//
// A nil Pinned means the listing admits both kinds and hid nothing; a true one
// means the caller asked for pinned rows specifically. Neither has anything to
// disclose, and only the false case does.
func PinnedExclusionArmed(filter types.IssueFilter) bool {
	return filter.Pinned != nil && !*filter.Pinned
}

// PinnedProbeFilter is a listing's OWN filter with the pinned exclusion
// inverted, so the rows it selects are the rows the listing dropped for being
// pinned.
//
// EVERY OTHER PREDICATE IS INHERITED, and that is the opposite choice from
// WispLabelProbeFilter. That probe drops the status scope because it asks a
// question about a different table, where the listing's scope would reproduce
// the false zero. This one asks about the SAME table under the SAME query, and
// the only honest count of "what did this listing hide" is one taken under the
// listing's own predicates — a probe that widened the status scope would report
// closed rows to a caller who asked about open work.
//
// THE ONE STATUS TERM IT DOES DROP is the default exclusion of the `pinned`
// STATUS. A bead can be pinned twice over — the boolean column and the status —
// and BuildListFilter's default excludes both independently, so a probe that
// inherited that term would report a measured zero over rows carrying the very
// property it is reporting on. That is the failure mode this notice exists to
// end, reproduced inside its own instrument. The exclusion is dropped rather
// than the whole default (which also hides closed rows), so the probe still
// answers within the caller's scope. Note that `--pinned` lifts the default
// wholesale, so it can reveal MORE than this count — a floor, never a bound.
//
// WHAT THIS PROBE CANNOT REACH is a row whose STATUS the caller did not select:
// the status term is inherited, so under `--status open` a pinned HOOKED row
// fails it. That row is hidden twice over, and until StatusPinnedProbeFilter
// existed no probe on the listing counted it at all (bd-6xa). The intersection
// is taken there rather than by widening this one, because a probe that lifted
// the caller's status term here would tell a reader to type `--pinned` when
// `--pinned` alone still would not show the row.
//
// The pagination and cap knobs are dropped too, because they belong to the
// caller's page rather than to their question: an offset would skip part of the
// count, and MaxRows would fail the probe on a store where the caller's own
// listing succeeded. limit bounds the scan instead, and a count that reaches it
// is reported as a floor by the caller.
func PinnedProbeFilter(filter types.IssueFilter, limit int) types.IssueFilter {
	probe := filter
	pinned := true
	probe.Pinned = &pinned
	probe.ExcludeStatus = withoutStatus(filter.ExcludeStatus, types.StatusPinned)
	probe.Limit = limit
	probe.Offset = 0
	probe.MaxRows = 0
	probe.MaxRowsSource = ""
	probe.AfterCreatedAt = nil
	probe.AfterID = ""

	// The probe counts rows; it renders none. Labels, cardinalities and the
	// heavy TEXT columns are all projection, and none of them changes which
	// rows match.
	probe.SkipLabels = true
	probe.SkipCounts = true
	probe.IncludeDependencies = false
	probe.Lite = true
	return probe
}

// withoutStatus returns statuses minus drop, as a copy. The listing's own slice
// is left alone: the notice runs alongside a query, not instead of it, and a
// probe that edited the caller's filter in place would change what a --watch
// loop re-runs on its next tick.
func withoutStatus(statuses []types.Status, drop types.Status) []types.Status {
	if len(statuses) == 0 {
		return nil
	}
	out := make([]types.Status, 0, len(statuses))
	for _, s := range statuses {
		if s != drop {
			out = append(out, s)
		}
	}
	return out
}

// PinnedNoticeContext is a listing, as far as the pinned disclosure is
// concerned: the filter it ran, plus the two things that filter cannot express.
// It carries the filter rather than handing it out because the frontend has no
// business writing one — the same boundary .golangci.yml enforces on cmd/bd,
// and the reason the whole probe lives beside BuildListFilter instead of beside
// the printer.
//
// Its zero value is a listing nothing is known about, which owes no
// disclosure — the state the proxied route is in when it could not open a unit
// of work.
type PinnedNoticeContext struct {
	filter types.IssueFilter

	// ready marks a --ready listing. Those are answered by GetReadyWork, whose
	// readiness logic no SearchIssues probe reproduces, so a count taken here
	// would describe a query the caller did not run — the substitution this
	// notice exists to stop. A ready listing gets no disclosure rather than an
	// approximate one.
	ready bool

	// exclusionRequested marks a caller who typed --no-pinned. The filter is
	// identical either way, and the subject of the notice is a default the
	// caller never chose: someone who excluded pinned rows on purpose is not
	// being kept in the dark by one.
	exclusionRequested bool
}

// PinnedNoticeFor describes a listing that just ran, from the request it came
// from and the filter that request became.
//
// Both routes build it here rather than reading the flags at their own call
// sites, so they cannot come to differ on what counts as a caller-chosen
// exclusion.
func PinnedNoticeFor(in issueops.ListRequest, filter types.IssueFilter) PinnedNoticeContext {
	return PinnedNoticeContext{
		filter:             filter,
		ready:              in.ReadyFlag,
		exclusionRequested: in.NoPinnedFlag,
	}
}

// Applies reports whether this listing owes a pinned disclosure at all: the
// default was armed, the caller did not ask for it, and the query is one this
// probe can speak about.
func (c PinnedNoticeContext) Applies() bool {
	return !c.ready && !c.exclusionRequested && PinnedExclusionArmed(c.filter)
}

// CountHidden counts the issues the listing dropped to the pinned default.
//
// The error is returned rather than folded into a zero for the reason the wisp
// probe returns one: "the probe ran and found none" and "the probe could not
// run" are different answers, and a silent substitution of the second for the
// first is the failure this probe exists to stop.
func (c PinnedNoticeContext) CountHidden(ctx context.Context, s PinnedSearcher, limit int) (int, error) {
	if !c.Applies() {
		return 0, nil
	}
	if s == nil {
		return 0, ErrNoPinnedSearcher
	}
	issues, err := s.SearchIssues(ctx, "", PinnedProbeFilter(c.filter, limit))
	if err != nil {
		return 0, err
	}
	return len(issues), nil
}

// ErrNoPinnedSearcher marks a probe that had no store to ask. It is an error,
// not a zero, for the reason above.
var ErrNoPinnedSearcher = errNoPinnedSearcher{}

type errNoPinnedSearcher struct{}

func (errNoPinnedSearcher) Error() string { return "no store available to probe for pinned rows" }
