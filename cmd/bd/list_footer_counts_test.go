package main

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/steveyegge/beads/internal/types"
)

// A count printed next to a noun is read as a fact about the database, not as a
// property of whatever subset happened to survive the filters. When the two
// diverge the message is worse than silence: "0 in progress" answers "is
// anything in progress?" with a confident no, and the reader has no way to tell
// that the rows which would have said otherwise were removed before counting.
//
// These tests are the general guard on that class. Every integer a summary line
// asserts must be justified by the fixture it was rendered from — so a future
// edit that introduces a new number has to declare what the number means, or
// fail here.

// footerFacts are the quantities a summary line is allowed to state, keyed by
// what they mean. assertEveryNumberIsJustified fails on any number in the line
// that is not one of these values, which is what forces a new number to be
// declared rather than smuggled in.
type footerFacts map[string]int

// assertEveryNumberIsJustified checks that each integer appearing in line is
// explained by facts. It deliberately does NOT check that every fact appears:
// omitting a count is fine (that is the fix in this very file), asserting an
// unexplained one is not.
func assertEveryNumberIsJustified(t *testing.T, line string, facts footerFacts) {
	t.Helper()
	allowed := map[int][]string{}
	for name, v := range facts {
		allowed[v] = append(allowed[v], name)
	}
	for _, tok := range regexp.MustCompile(`\d+`).FindAllString(line, -1) {
		n, err := strconv.Atoi(tok)
		if err != nil {
			t.Fatalf("unparseable integer %q in summary %q", tok, line)
		}
		if _, ok := allowed[n]; !ok {
			t.Errorf("summary asserts the number %d, which no fact about the data explains.\n  summary: %q\n  known facts: %v\n"+
				"If this number is legitimate, add it to footerFacts so it is checked; if it is a count of something the query filtered out, it should not be asserted at all.",
				n, line, facts)
		}
	}
}

// buckets is the test-side spelling of a status breakdown: the buckets the
// renderer would have derived from a page of rows, in the order it derives them.
func buckets(pairs ...any) []statusCount {
	out := make([]statusCount, 0, len(pairs)/2)
	for i := 0; i < len(pairs); i += 2 {
		out = append(out, statusCount{Status: pairs[i].(string), Count: pairs[i+1].(int)})
	}
	return out
}

// The scenarios below are the cross-product that matters: whether the page was
// cut by --limit, and whether --ready pinned the query to open issues.
func TestListFooterLineCountsAreJustified(t *testing.T) {
	tests := []struct {
		name                     string
		total                    int
		byStatus                 []statusCount
		truncated, readyFiltered bool
		facts                    footerFacts
	}{
		{
			name: "mixed statuses, whole result set",
			// A plain listing may state the breakdown: the query could have
			// returned any status, so each count is a real finding.
			total: 9, byStatus: buckets("open", 6, "in_progress", 3),
			facts: footerFacts{"total": 9, "open": 6, "in_progress": 3},
		},
		{
			name:  "mixed statuses, page cut by --limit",
			total: 2, byStatus: buckets("open", 1, "in_progress", 1), truncated: true,
			facts: footerFacts{"total": 2, "open": 1, "in_progress": 1, "limit-hint": 0},
		},
		{
			// The statuses the two hardcoded buckets used to drop on the floor.
			name:  "statuses beyond open and in_progress",
			total: 13, byStatus: buckets("open", 8, "hooked", 5),
			facts: footerFacts{"total": 13, "open": 8, "hooked": 5},
		},
		{
			// The regression this file exists for. --ready pins the filter to
			// open, so inProgress is 0 by construction for ANY database. The
			// summary must not assert it.
			name:  "ready-filtered, whole result set",
			total: 6, byStatus: buckets("open", 6), readyFiltered: true,
			facts: footerFacts{"total": 6},
		},
		{
			name:  "ready-filtered and truncated",
			total: 5, byStatus: buckets("open", 5), readyFiltered: true, truncated: true,
			facts: footerFacts{"total": 5, "limit-hint": 0},
		},
		{
			name:  "empty result set",
			total: 0, byStatus: nil,
			facts: footerFacts{"total": 0},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			line := listFooterLine(tt.total, tt.byStatus, tt.truncated, tt.readyFiltered)
			assertEveryNumberIsJustified(t, line, tt.facts)

			// The headline count always describes the rows actually rendered.
			if !strings.Contains(line, fmt.Sprintf("%d", tt.total)) {
				t.Errorf("summary %q omits the rendered row count %d", line, tt.total)
			}
		})
	}
}

// The specific claim: under --ready the summary must not report an in-progress
// count, because the filter guarantees it is zero regardless of the data. A
// reader cannot distinguish "none exist" from "none survived the filter".
func TestListFooterLineReadyOmitsVacuousInProgressCount(t *testing.T) {
	// 40 in-progress issues match the same query; --ready removed them all.
	line := listFooterLine(6, buckets("open", 6), false, true)

	if strings.Contains(line, "in progress)") {
		t.Errorf("--ready summary asserts an in-progress count that the filter forced to zero: %q", line)
	}
	if !strings.Contains(line, "excludes in_progress") {
		t.Errorf("--ready summary must disclose what it filtered, got: %q", line)
	}
	if strings.Contains(line, "0") {
		t.Errorf("--ready summary states a zero the data does not support: %q", line)
	}
}

// Without --ready the breakdown is a genuine finding and must survive: this is
// the half of the behaviour the fix must not regress.
func TestListFooterLineUnfilteredKeepsBreakdown(t *testing.T) {
	line := listFooterLine(9, buckets("open", 6, "in_progress", 3), false, false)
	for _, want := range []string{"Total: 9 issues", "6 open", "3 in progress"} {
		if !strings.Contains(line, want) {
			t.Errorf("unfiltered summary lost %q, got: %q", want, line)
		}
	}
}

// The footer is correct as a pure function only if every renderer actually
// tells it that --ready is in force. The display wrappers are where that can be
// lost: they take readyFiltered as a parameter, and a caller that omits it (or
// a wrapper that defaults it) silently restores the vacuous count with
// listFooterLine itself still passing every test above.
//
// These two tests pin the wrappers the --watch paths display through — the
// surface where a stale "0 in progress" is most likely to be read as a live
// fact, because it is re-rendered every two seconds under a heading that says
// the data is current.
//
// bd list --ready --watch (direct) → displayWatchedIssueList.
func TestDisplayWatchedIssueListReadyFooterDisclosesFilter(t *testing.T) {
	// Open by construction: --ready pinned the query, so these are all the
	// rows any database could return here.
	issues := []*types.Issue{
		{ID: "bd-1", Title: "A", Status: types.StatusOpen, Priority: 1, IssueType: types.TypeTask},
		{ID: "bd-2", Title: "B", Status: types.StatusOpen, Priority: 2, IssueType: types.TypeTask},
	}
	// A nil store is the no-dependency-data arm the function already guards
	// for; the footer is what is under test, and this keeps the file free of
	// the cgo-tagged stub so it still compiles under CGO_ENABLED=0.
	out := captureStdout(t, func() error {
		displayWatchedIssueList(context.Background(), nil, issues, false, true)
		return nil
	})

	if strings.Contains(out, "in progress)") {
		t.Errorf("--ready --watch summary asserts an in-progress count its own filter forced to zero: %q", out)
	}
	if !strings.Contains(out, "excludes in_progress") {
		t.Errorf("--ready --watch summary must disclose what it filtered, got: %q", out)
	}
}

// bd list --ready --watch (proxied) displays through displayPrettyListWithDeps
// directly, so the parameter has to survive that wrapper too.
func TestDisplayPrettyListWithDepsReadyFooterDisclosesFilter(t *testing.T) {
	issues := []*types.Issue{
		{ID: "bd-1", Title: "A", Status: types.StatusOpen, Priority: 1, IssueType: types.TypeTask},
	}

	out := captureStdout(t, func() error {
		displayPrettyListWithDeps(issues, false, nil, false, true)
		return nil
	})
	if strings.Contains(out, "in progress)") {
		t.Errorf("proxied --ready --watch summary asserts a filtered-out count: %q", out)
	}
	if !strings.Contains(out, "excludes in_progress") {
		t.Errorf("proxied --ready --watch summary must disclose its filter, got: %q", out)
	}

	// The other half: without --ready the same wrapper must keep the breakdown,
	// so the fix cannot be "never print counts".
	//
	// Note what this fixture can and cannot say. It is one open row, so the
	// breakdown it earns is "(1 open)" and nothing else — the "0 in progress"
	// this assertion used to look for was never a fact about the fixture, only
	// the hardcoded bucket asserting itself over a page that had no such row.
	plain := captureStdout(t, func() error {
		displayPrettyListWithDeps(issues, false, nil, false, false)
		return nil
	})
	if !strings.Contains(plain, "Total: 1 issues (1 open)") {
		t.Errorf("unfiltered listing lost its status breakdown: %q", plain)
	}
}

// The defect this file's second half exists for: the breakdown named exactly two
// buckets against five live statuses, so a blocked, deferred or hooked row
// landed in neither and the parenthetical disclaimed it without saying so.
//
// The guard is arithmetic rather than a list of status names — that is the point
// of driving the buckets off the rows. A status added to types.AllStatuses after
// this was written, or a custom one the code cannot know about, has to keep the
// parts summing to the total or fail here.
func TestListFooterBreakdownAccountsForEveryRow(t *testing.T) {
	tests := []struct {
		name   string
		issues []*types.Issue
	}{
		{
			// Verbatim from the bug report: one open row and one hooked row
			// printed "Total: 2 issues (1 open, 0 in progress)".
			name: "open and hooked",
			issues: []*types.Issue{
				{ID: "bd-1", Status: types.StatusOpen},
				{ID: "bd-2", Status: types.StatusHooked},
			},
		},
		{
			name: "every live status at once",
			issues: []*types.Issue{
				{ID: "bd-1", Status: types.StatusOpen},
				{ID: "bd-2", Status: types.StatusInProgress},
				{ID: "bd-3", Status: types.StatusBlocked},
				{ID: "bd-4", Status: types.StatusDeferred},
				{ID: "bd-5", Status: types.StatusHooked},
			},
		},
		{
			name: "pinned and closed rows a --status query can return",
			issues: []*types.Issue{
				{ID: "bd-1", Status: types.StatusPinned},
				{ID: "bd-2", Status: types.StatusClosed},
				{ID: "bd-3", Status: types.StatusClosed},
			},
		},
		{
			// bd config set status.custom "triage,waiting" — unknowable here,
			// and just as entitled to a bucket.
			name: "custom statuses",
			issues: []*types.Issue{
				{ID: "bd-1", Status: types.Status("triage")},
				{ID: "bd-2", Status: types.Status("triage")},
				{ID: "bd-3", Status: types.StatusOpen},
			},
		},
		{
			// A row with no status is still a row. It must not vanish from the
			// sum, and it must not render as a bare number either.
			name: "row with an unset status",
			issues: []*types.Issue{
				{ID: "bd-1", Status: types.StatusOpen},
				{ID: "bd-2"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			line := listFooterLine(len(tt.issues), countIssuesByStatus(tt.issues), false, false)
			assertBreakdownSumsToTotal(t, line, len(tt.issues))
		})
	}
}

// assertBreakdownSumsToTotal reads the rendered line back the way a user does —
// the headline count, then the parenthetical — and requires the parts to be the
// whole. Parsing the output rather than inspecting the buckets is deliberate:
// the defect was in what the line CLAIMED, and a bucket dropped between
// countIssuesByStatus and the format string would be invisible to a check on
// the buckets alone.
func assertBreakdownSumsToTotal(t *testing.T, line string, wantTotal int) {
	t.Helper()

	headline := regexp.MustCompile(`(?:Total|Showing): (\d+) issues`).FindStringSubmatch(line)
	if headline == nil {
		t.Fatalf("summary has no headline count: %q", line)
	}
	total, err := strconv.Atoi(headline[1])
	if err != nil {
		t.Fatalf("unparseable headline count in %q: %v", line, err)
	}
	if total != wantTotal {
		t.Errorf("summary says %d issues, fixture rendered %d: %q", total, wantTotal, line)
	}

	inner := regexp.MustCompile(`\(([^)]*)\)`).FindStringSubmatch(line)
	if inner == nil {
		if wantTotal == 0 {
			return // Nothing rendered, nothing to break down.
		}
		t.Fatalf("summary states a total but breaks it down into nothing: %q", line)
	}

	sum := 0
	for _, part := range strings.Split(inner[1], ", ") {
		bucket := regexp.MustCompile(`^(\d+) (\S.*)$`).FindStringSubmatch(part)
		if bucket == nil {
			t.Fatalf("breakdown part %q is not a labelled count: %q", part, line)
		}
		n, err := strconv.Atoi(bucket[1])
		if err != nil {
			t.Fatalf("unparseable count in part %q: %v", part, err)
		}
		sum += n
	}

	if sum != total {
		t.Errorf("the breakdown disclaims %d of %d rows without saying it did.\n  summary: %q\n"+
			"Every rendered row must land in some bucket; a status with no bucket is how "+
			"\"Total: 2 issues (1 open, 0 in progress)\" got printed over a hooked row.",
			total-sum, total, line)
	}
}

// The breakdown is built from a map, so without an explicit order the same page
// would render its buckets differently between two identical runs — a diff in
// the output of a command that read the same data twice.
func TestStatusBreakdownOrderIsStable(t *testing.T) {
	issues := []*types.Issue{
		{ID: "bd-1", Status: types.StatusHooked},
		{ID: "bd-2", Status: types.StatusOpen},
		{ID: "bd-3", Status: types.StatusBlocked},
		{ID: "bd-4", Status: types.Status("zeta")},
		{ID: "bd-5", Status: types.Status("alpha")},
		{ID: "bd-6", Status: types.StatusDeferred},
	}

	want := formatStatusBreakdown(countIssuesByStatus(issues))
	for i := 0; i < 50; i++ {
		if got := formatStatusBreakdown(countIssuesByStatus(issues)); got != want {
			t.Fatalf("breakdown order is not stable across runs:\n  %q\n  %q", want, got)
		}
	}

	// Built-in statuses lead in types.AllStatuses order; custom ones follow,
	// sorted, so an unfamiliar status still lands somewhere predictable.
	if want != "1 open, 1 blocked, 1 deferred, 1 hooked, 1 alpha, 1 zeta" {
		t.Errorf("unexpected bucket order: %q", want)
	}
}

// The footer is only as good as what the renderer hands it: the old call site
// counted two statuses itself and passed two integers, so a fix confined to
// listFooterLine would leave the real listing printing the same disclaimed rows.
func TestDisplayPrettyListFooterCountsNonOpenRows(t *testing.T) {
	issues := []*types.Issue{
		{ID: "bd-1", Title: "A", Status: types.StatusOpen, Priority: 1, IssueType: types.TypeTask},
		{ID: "bd-2", Title: "B", Status: types.StatusHooked, Priority: 2, IssueType: types.TypeTask},
		{ID: "bd-3", Title: "C", Status: types.StatusBlocked, Priority: 2, IssueType: types.TypeTask},
	}

	out := captureStdout(t, func() error {
		displayPrettyListWithDeps(issues, false, nil, false, false)
		return nil
	})

	footer := ""
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "Total: ") {
			footer = line
			break
		}
	}
	if footer == "" {
		t.Fatalf("listing printed no Total line: %q", out)
	}
	assertBreakdownSumsToTotal(t, footer, len(issues))

	for _, want := range []string{"1 open", "1 blocked", "1 hooked"} {
		if !strings.Contains(footer, want) {
			t.Errorf("footer omits %q, so that row is in no bucket: %q", want, footer)
		}
	}
	if strings.Contains(footer, "0 in progress") {
		t.Errorf("footer asserts an in-progress count no row supports: %q", footer)
	}
}

// Truncation and readiness are independent scopes and both must be disclosed
// when both apply — neither may silently mask the other.
func TestListFooterLineTruncationDisclosedAlongsideReady(t *testing.T) {
	line := listFooterLine(5, buckets("open", 5), true, true)
	if !strings.Contains(line, "truncated by --limit") {
		t.Errorf("truncated page must say so even under --ready, got: %q", line)
	}
	if !strings.Contains(line, "excludes in_progress") {
		t.Errorf("--ready must still disclose its filter when truncated, got: %q", line)
	}
	if strings.Contains(line, "Total:") {
		t.Errorf("a truncated page must never be labelled Total: %q", line)
	}
}
