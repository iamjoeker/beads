package workapi

import (
	"context"

	"github.com/steveyegge/beads/internal/types"
)

// The wisp-plane probe behind a labeled zero.
//
// `bd list` queries the ISSUES table; wisps live in the WISPS table. They are
// different tables, so no filter on a listing can ever return a wisp — and the
// records agents most often go looking for by label (merge requests, mail) are
// wisps. A labeled listing that comes back empty is therefore ambiguous in a way
// its own result set cannot resolve: the records may be absent, or they may be
// in the table this query could not reach (bd-nc4).
//
// The probe answers that with a COUNT rather than a suggestion. It lives here,
// with BuildListFilter and the rest of the work-query contract, because it is a
// question about the same query rather than about one frontend: a listing that
// returns zero has the same disclosure to make whichever surface asked.

// WispLabelProbeFilter selects wisps by the LABEL predicates of a listing, and
// by nothing else.
//
// STATUS IS DELIBERATELY DROPPED. A merged merge-request wisp is closed by
// definition, so a probe that inherited a listing's default open-only scope
// would reproduce the very filter that manufactures the false zero it is meant
// to explain.
//
// Nothing else is inherited either: assignee, type and date predicates would
// narrow the probe without making its answer more relevant to the question
// "does this label exist in the wisp plane at all".
func WispLabelProbeFilter(labels, labelsAny []string, labelPattern, labelRegex string, limit int) types.IssueFilter {
	ephemeral := true
	return types.IssueFilter{
		Ephemeral:    &ephemeral,
		Labels:       labels,
		LabelsAny:    labelsAny,
		LabelPattern: labelPattern,
		LabelRegex:   labelRegex,
		Limit:        limit,
	}
}

// HasLabelPredicate reports whether a listing selected on labels at all. The
// pattern and regex forms count: they select on labels just as much as the
// exact forms do, and a zero from one of them is just as ambiguous.
func HasLabelPredicate(labels, labelsAny []string, labelPattern, labelRegex string) bool {
	return len(labels) > 0 || len(labelsAny) > 0 || labelPattern != "" || labelRegex != ""
}

// LabelPredicates lists the label terms a listing carried, for a message that
// has to name what it looked for.
func LabelPredicates(labels, labelsAny []string, labelPattern, labelRegex string) []string {
	out := make([]string, 0, len(labels)+len(labelsAny)+2)
	out = append(out, labels...)
	out = append(out, labelsAny...)
	if labelPattern != "" {
		out = append(out, labelPattern)
	}
	if labelRegex != "" {
		out = append(out, labelRegex)
	}
	return out
}

// WispSearcher is the slice of storage the probe needs: one label-filtered read
// of the wisp plane.
type WispSearcher interface {
	SearchIssues(ctx context.Context, query string, filter types.IssueFilter) ([]*types.Issue, error)
}

// CountLabelledWisps counts the wisps carrying a listing's labels.
//
// The error is returned rather than folded into a zero on purpose: "the probe
// ran and found none" and "the probe could not run" are different answers, and
// reporting the second as the first is the same substitution this whole probe
// exists to stop.
func CountLabelledWisps(ctx context.Context, s WispSearcher, labels, labelsAny []string, labelPattern, labelRegex string, limit int) (int, error) {
	if s == nil {
		return 0, ErrNoWispSearcher
	}
	wisps, err := s.SearchIssues(ctx, "", WispLabelProbeFilter(labels, labelsAny, labelPattern, labelRegex, limit))
	if err != nil {
		return 0, err
	}
	return len(wisps), nil
}

// ErrNoWispSearcher marks a probe that had no store to ask. It is an error, not
// a zero, for the reason above.
var ErrNoWispSearcher = errNoWispSearcher{}

type errNoWispSearcher struct{}

func (errNoWispSearcher) Error() string { return "no store available to probe the wisp plane" }
