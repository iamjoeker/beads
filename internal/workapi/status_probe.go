package workapi

import (
	"context"
	"slices"

	"github.com/steveyegge/beads/internal/types"
	"github.com/steveyegge/beads/issueops"
)

// The status-selector probe behind a listing that answered a narrower question
// than the caller asked.
//
// `--status open` matches the status column EXACTLY. It is not "not closed",
// and on this tracker the difference is most of the interesting work: hooked,
// in_progress, blocked and deferred beads all fail it. Two agents measured the
// same structure independently on the same repo — CLI row counts and direct SQL
// agreeing on both numbers — and found roughly a third of live beads outside
// `--status open` (bd-j3z). The exclusion is in the filter's semantics, not in
// the rendering.
//
// What makes it worth a disclosure rather than only better flag help is WHEN it
// bites. Which beads are hidden depends on how much work is in flight at that
// instant, so the filter is LEAST complete exactly when the most is happening —
// which is precisely when someone asks "is anyone already on this?". Three
// duplicate dispatches followed from a pre-sling check written with it: the
// instant the first bead is hooked it leaves `--status open`, so the check
// returned a clean zero at the only moment it mattered.
//
// The probe answers with a COUNT of the live rows the caller's own query
// dropped for their status alone: the listing's filter with the status term
// replaced by "live, and not one of the statuses you named". It is built from
// that filter rather than rebuilt from the request, so the count cannot come to
// describe a query the caller did not run.

// StatusSearcher is the slice of storage this probe needs: one filtered read of
// the plane the listing itself selected. It is the same shape as PinnedSearcher
// and WispSearcher, and kept separate for the same reason they are kept
// separate from each other — the three ask about different populations and are
// free to diverge.
type StatusSearcher interface {
	SearchIssues(ctx context.Context, query string, filter types.IssueFilter) ([]*types.Issue, error)
}

// LiveStatuses returns every status a bare `bd list` shows: the built-ins minus
// closed and pinned, plus the custom statuses that are not categorized done or
// frozen.
//
// It is the complement of LiveStatusExclusions over the known status names, and
// derived from it rather than listed again, so the two cannot drift into
// disagreeing about what "live" is.
func LiveStatuses(cfg ListConfig) []types.Status {
	excluded := LiveStatusExclusions(cfg)
	live := make([]types.Status, 0, len(types.AllStatuses)+len(cfg.CustomStatuses))
	for _, s := range types.AllStatuses {
		if !slices.Contains(excluded, s) {
			live = append(live, s)
		}
	}
	for _, cs := range cfg.CustomStatuses {
		s := types.Status(cs.Name)
		if !slices.Contains(excluded, s) && !slices.Contains(live, s) {
			live = append(live, s)
		}
	}
	return live
}

// selectedStatuses returns the statuses a filter's status term names, in either
// of the two forms BuildListFilter can leave it in: the singular Status for a
// one-status selector, the Statuses set for a comma-separated one.
func selectedStatuses(filter types.IssueFilter) []types.Status {
	if filter.Status != nil {
		return []types.Status{*filter.Status}
	}
	return slices.Clone(filter.Statuses)
}

// StatusNoticeContext is a listing, as far as the status disclosure is
// concerned: the filter it ran, plus the live statuses that filter left out.
// It carries the filter rather than handing it out for the reason
// PinnedNoticeContext does — the frontend has no business writing one, which is
// the boundary .golangci.yml enforces on cmd/bd.
//
// Its zero value is a listing nothing is known about, which owes no
// disclosure — the state the proxied route is in when it could not open a unit
// of work.
type StatusNoticeContext struct {
	filter types.IssueFilter

	// selected and dropped are the caller's live selection and its complement
	// within the live set. Both are computed once, where the config that
	// defines "live" is in hand, so the renderer can name the missing statuses
	// without a second reading of the categories.
	selected []types.Status
	dropped  []types.Status

	// ready marks a --ready listing, for the reason PinnedNoticeContext refuses
	// one: those are answered by GetReadyWork, whose readiness logic no
	// SearchIssues probe reproduces, so a count taken here would describe a
	// query the caller did not run. --ready also forces status open itself
	// rather than honoring the selector, so the narrowing is not the caller's
	// to be warned about.
	ready bool
}

// StatusNoticeFor describes a listing that just ran, from the request it came
// from, the filter that request became, and the config that says which statuses
// are live.
//
// Both routes build it here rather than reading the flags at their own call
// sites, so they cannot come to differ on what counts as a narrowed listing.
func StatusNoticeFor(in issueops.ListRequest, filter types.IssueFilter, cfg ListConfig) StatusNoticeContext {
	c := StatusNoticeContext{filter: filter, ready: in.ReadyFlag}
	if c.ready {
		return c
	}

	live := LiveStatuses(cfg)
	for _, s := range selectedStatuses(filter) {
		if slices.Contains(live, s) {
			c.selected = append(c.selected, s)
		}
	}
	// A caller who named no LIVE status is not being narrowed in the sense this
	// notice is about: `--status closed` asks a question about finished work,
	// and answering it with a count of live beads would be an interruption
	// rather than a disclosure.
	if len(c.selected) == 0 {
		return StatusNoticeContext{filter: filter}
	}
	for _, s := range live {
		if !slices.Contains(c.selected, s) {
			c.dropped = append(c.dropped, s)
		}
	}
	return c
}

// Applies reports whether this listing owes a status disclosure at all: the
// caller selected some live statuses and not others, and the query is one this
// probe can speak about.
func (c StatusNoticeContext) Applies() bool {
	return !c.ready && len(c.selected) > 0 && len(c.dropped) > 0
}

// Dropped returns the live statuses this listing's selector left out, for the
// renderer to name. The slice is a copy: the notice runs alongside a query, not
// instead of it, and a renderer that sorted the context's own slice in place
// would be editing state a --watch loop re-reads on its next tick.
func (c StatusNoticeContext) Dropped() []string {
	out := make([]string, 0, len(c.dropped))
	for _, s := range c.dropped {
		out = append(out, string(s))
	}
	return out
}

// CountHidden counts the live issues the listing dropped for their status.
//
// The error is returned rather than folded into a zero for the reason the other
// two probes return one: "the probe ran and found none" and "the probe could
// not run" are different answers, and a silent substitution of the second for
// the first is the failure this probe exists to stop.
func (c StatusNoticeContext) CountHidden(ctx context.Context, s StatusSearcher, limit int) (int, error) {
	if !c.Applies() {
		return 0, nil
	}
	if s == nil {
		return 0, ErrNoStatusSearcher
	}
	issues, err := s.SearchIssues(ctx, "", StatusProbeFilter(c.filter, c.dropped, limit))
	if err != nil {
		return 0, err
	}
	return len(issues), nil
}

// ErrNoStatusSearcher marks a probe that had no store to ask. It is an error,
// not a zero, for the reason above.
var ErrNoStatusSearcher = errNoStatusSearcher{}

type errNoStatusSearcher struct{}

func (errNoStatusSearcher) Error() string {
	return "no store available to probe for statuses hidden from a listing"
}

// StatusProbeFilter is a listing's OWN filter with the status term replaced by
// the dropped set, so the rows it selects are the live rows the listing dropped
// for their status and nothing else.
//
// EVERY OTHER PREDICATE IS INHERITED, and for the reason PinnedProbeFilter
// inherits them: this probe asks about the SAME table under the SAME query, and
// the only honest count of "what did this listing hide" is one taken under the
// caller's own predicates. A probe that widened the assignee or the label scope
// would report beads the caller never asked about.
//
// THE PINNED DEFAULT IS INHERITED TOO, which keeps this notice and the pinned
// one disjoint: a row hidden for being pinned is the pinned notice's to report,
// and counting it here as well would tell the reader to fix their --status when
// their --status was not the reason. The dropped set is used as a positive
// selection rather than as an inversion of the exclusion for the same reason —
// it names only the live statuses, so closed rows can never enter the count.
//
// The pagination and cap knobs are dropped because they belong to the caller's
// page rather than to their question: an offset would skip part of the count,
// and MaxRows would fail the probe on a store where the caller's own listing
// succeeded. limit bounds the scan instead, and a count that reaches it is
// reported as a floor by the caller.
func StatusProbeFilter(filter types.IssueFilter, dropped []types.Status, limit int) types.IssueFilter {
	probe := filter
	probe.Status = nil
	probe.Statuses = slices.Clone(dropped)

	// A one-element Statuses is a legal OR set, but the singular Status is the
	// form every backend has always implemented, so a single dropped status is
	// expressed that way rather than through the wider path.
	if len(probe.Statuses) == 1 {
		s := probe.Statuses[0]
		probe.Status = &s
		probe.Statuses = nil
	}

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
