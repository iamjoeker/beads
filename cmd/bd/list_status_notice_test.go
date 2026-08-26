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

// statusNarrowedListing is the shape `bd list --status open` reaches the notice
// in. It is built through the same BuildListFilter the command runs, so a
// change to what counts as live moves this fixture with it instead of leaving a
// hand-written filter asserting the old rule.
func statusNarrowedListing(t *testing.T, selector string) workapi.StatusNoticeContext {
	t.Helper()
	req := issueops.ListRequest{Status: selector}
	filter, err := workapi.BuildListFilter(req, workapi.ListConfig{})
	if err != nil {
		t.Fatalf("build the listing filter: %v", err)
	}
	return workapi.StatusNoticeFor(req, filter, workapi.ListConfig{})
}

func TestCountHiddenByStatusDistinguishesZeroFromUnknown(t *testing.T) {
	ctx := context.Background()
	listing := statusNarrowedListing(t, "open")

	if got := countHiddenByStatus(ctx, nil, listing); got != unknownIssueCount {
		t.Errorf("no store means the probe never ran, want %d, got %d", unknownIssueCount, got)
	}
	failing := &wispSearcherStub{err: errors.New("boom")}
	if got := countHiddenByStatus(ctx, failing, listing); got != unknownIssueCount {
		t.Errorf("a failed probe is not a measured zero, want %d, got %d", unknownIssueCount, got)
	}
	empty := &wispSearcherStub{}
	if got := countHiddenByStatus(ctx, empty, listing); got != 0 {
		t.Errorf("a probe that ran and found none is a measured zero, want 0, got %d", got)
	}
}

func TestHiddenByStatusNoticeLines(t *testing.T) {
	dropped := []string{"in_progress", "blocked", "deferred", "hooked"}

	t.Run("says nothing when nothing was hidden", func(t *testing.T) {
		if lines := hiddenByStatusNoticeLines(dropped, 0, 0, 3, "beads"); lines != nil {
			t.Fatalf("an ordinary listing should stay ordinary, got %v", lines)
		}
	})

	// unknownIssueCount is negative, so it falls into the same "nothing
	// measured to say" arm: a probe that could not run must not be rendered as
	// a count.
	t.Run("says nothing when the probe could not run", func(t *testing.T) {
		if lines := hiddenByStatusNoticeLines(dropped, unknownIssueCount, unknownIssueCount, 3, "beads"); lines != nil {
			t.Fatalf("an unmeasured probe must claim no count, got %v", lines)
		}
	})

	t.Run("says nothing when the listing was not narrowed", func(t *testing.T) {
		if lines := hiddenByStatusNoticeLines(nil, 4, 0, 3, "beads"); lines != nil {
			t.Fatalf("no dropped statuses means no disclosure, got %v", lines)
		}
	})

	// The notice fires on a NON-empty listing too: 3-of-7 is the same silence
	// as 0-of-4, and the reader of a short listing has no more way to know rows
	// were withheld than the reader of an empty one.
	t.Run("a short listing is disclosed too", func(t *testing.T) {
		lines := hiddenByStatusNoticeLines(dropped, 4, 0, 3, "beads")
		if len(lines) == 0 {
			t.Fatal("a short listing that hid live rows owes a disclosure")
		}
		joined := strings.Join(lines, "\n")
		for _, want := range []string{"3 issue(s) listed", "4 further LIVE issue(s)", "in beads", "--status live"} {
			if !strings.Contains(joined, want) {
				t.Errorf("notice does not mention %q:\n%s", want, joined)
			}
		}
		for _, s := range dropped {
			if !strings.Contains(joined, s) {
				t.Errorf("notice does not name the dropped status %q:\n%s", s, joined)
			}
		}
	})

	t.Run("an empty listing gets the headline that fits it", func(t *testing.T) {
		joined := strings.Join(hiddenByStatusNoticeLines(dropped, 4, 0, 0, "beads"), "\n")
		if !strings.Contains(joined, "nothing matched your --status") {
			t.Errorf("an empty listing needs its own headline:\n%s", joined)
		}
	})

	// At the cap the scan stopped counting, so the number is a floor. Saying it
	// flat would be the probe overstating what it measured.
	t.Run("a capped scan reports a floor", func(t *testing.T) {
		joined := strings.Join(hiddenByStatusNoticeLines(dropped, statusProbeRowCap, 0, 1, "beads"), "\n")
		if !strings.Contains(joined, "at least") {
			t.Errorf("a count that reached the cap is a floor:\n%s", joined)
		}
	})

	// The store is named where one is known and called "the same store"
	// otherwise, so the sentence never implies a place the caller could not
	// have been told.
	t.Run("an unknown store is not named", func(t *testing.T) {
		joined := strings.Join(hiddenByStatusNoticeLines(dropped, 4, 0, 1, ""), "\n")
		if !strings.Contains(joined, "in the same store") {
			t.Errorf("an unnamed store must not be invented:\n%s", joined)
		}
	})
}

// The rows hidden TWICE — by the status selector and by the pinned default —
// get their own count and their own remedy (bd-6xa). Folding them into the
// number above would put them under a `--status live` fix that does not reveal
// them, which is the same silence the notice exists to break.
func TestHiddenByStatusNoticeSeparatesTheDoublyHiddenRows(t *testing.T) {
	dropped := []string{"in_progress", "blocked", "deferred", "hooked"}

	// The counts are disjoint, so the headline number must not absorb the
	// pinned one and the remedies must not be interchangeable.
	t.Run("both counts get their own line and remedy", func(t *testing.T) {
		joined := strings.Join(hiddenByStatusNoticeLines(dropped, 4, 2, 3, "beads"), "\n")
		for _, want := range []string{
			"4 further LIVE issue(s)",
			"2 further LIVE issue(s)",
			"--status live --pinned",
		} {
			if !strings.Contains(joined, want) {
				t.Errorf("notice does not mention %q:\n%s", want, joined)
			}
		}
		if strings.Contains(joined, "6 ") {
			t.Errorf("the two counts are disjoint and must not be summed:\n%s", joined)
		}
	})

	// The case the gap was really about: the status selector hid nothing on its
	// own, every dropped row was pinned as well, and the base remedy would have
	// been wrong. Before the second probe existed this printed nothing at all.
	t.Run("a listing that hid only doubly-hidden rows still speaks", func(t *testing.T) {
		lines := hiddenByStatusNoticeLines(dropped, 0, 2, 3, "beads")
		if len(lines) == 0 {
			t.Fatal("a row hidden by two defaults at once is still a hidden row")
		}
		joined := strings.Join(lines, "\n")
		for _, want := range []string{"2 further LIVE issue(s)", "the pinned default", "--status live --pinned"} {
			if !strings.Contains(joined, want) {
				t.Errorf("notice does not mention %q:\n%s", want, joined)
			}
		}
		// The bare `--status live` remedy would send the reader back to a
		// listing that still hides these rows.
		if strings.Contains(joined, "--status live for all work") {
			t.Errorf("the base remedy does not reveal pinned rows and must not be offered alone:\n%s", joined)
		}
	})

	t.Run("an empty listing of only doubly-hidden rows gets its own headline", func(t *testing.T) {
		joined := strings.Join(hiddenByStatusNoticeLines(dropped, 0, 2, 0, "beads"), "\n")
		if !strings.Contains(joined, "nothing matched your --status") {
			t.Errorf("an empty listing needs its own headline:\n%s", joined)
		}
		if !strings.Contains(joined, "PINNED") {
			t.Errorf("the headline must say the rows are pinned as well:\n%s", joined)
		}
	})

	// unknownIssueCount is negative, so an unmeasured pinned probe adds no line
	// rather than a count nobody took.
	t.Run("an unmeasured pinned probe claims no count", func(t *testing.T) {
		joined := strings.Join(hiddenByStatusNoticeLines(dropped, 4, unknownIssueCount, 3, "beads"), "\n")
		if strings.Contains(joined, "--pinned") {
			t.Errorf("a probe that could not run must not be rendered as a count:\n%s", joined)
		}
		if !strings.Contains(joined, "4 further LIVE issue(s)") {
			t.Errorf("the probe that DID run must still be reported:\n%s", joined)
		}
	})

	t.Run("a capped doubly-hidden scan reports a floor", func(t *testing.T) {
		joined := strings.Join(hiddenByStatusNoticeLines(dropped, 0, statusProbeRowCap, 1, "beads"), "\n")
		if !strings.Contains(joined, "at least") {
			t.Errorf("a count that reached the cap is a floor:\n%s", joined)
		}
	})
}

// countHiddenPinnedByStatus fails independently of countHiddenByStatus: one
// probe failing is not evidence about the other, and a zero substituted for
// "could not ask" would hide exactly the rows the count was added to disclose.
func TestCountHiddenPinnedByStatusDistinguishesZeroFromUnknown(t *testing.T) {
	ctx := context.Background()
	listing := statusNarrowedListing(t, "open")

	if got := countHiddenPinnedByStatus(ctx, nil, listing); got != unknownIssueCount {
		t.Errorf("no store means the probe never ran, want %d, got %d", unknownIssueCount, got)
	}
	failing := &wispSearcherStub{err: errors.New("boom")}
	if got := countHiddenPinnedByStatus(ctx, failing, listing); got != unknownIssueCount {
		t.Errorf("a failed probe is not a measured zero, want %d, got %d", unknownIssueCount, got)
	}
	empty := &wispSearcherStub{}
	if got := countHiddenPinnedByStatus(ctx, empty, listing); got != 0 {
		t.Errorf("a probe that ran and found none is a measured zero, want 0, got %d", got)
	}
}

// The notice's whole reason to exist: the reader typed --status open and got a
// plausible screen. It must name the statuses that failed the filter and the
// selector that would have shown them.
func TestHiddenByStatusNoticeExplainsTheExactSemantics(t *testing.T) {
	joined := strings.Join(hiddenByStatusNoticeLines([]string{"hooked"}, 1, 0, 6, "beads"), "\n")
	if !strings.Contains(joined, `not "not closed"`) {
		t.Errorf("the notice must correct the misreading it exists for:\n%s", joined)
	}
	if !strings.Contains(joined, "Not shown: hooked") {
		t.Errorf("the notice must name what was left out:\n%s", joined)
	}
}

func TestPrintHiddenByStatusNoticeReportsWhetherItSpoke(t *testing.T) {
	ctx := context.Background()

	// A listing that was never narrowed owes nothing and must not query.
	quiet := &wispSearcherStub{issues: []*types.Issue{{ID: "bd-1"}}}
	if printHiddenByStatusNotice(ctx, quiet, statusNarrowedListing(t, "live"), 1, "beads") {
		t.Error("a bare listing owes no status disclosure")
	}

	spoke := &wispSearcherStub{issues: []*types.Issue{{ID: "bd-1"}}}
	if !printHiddenByStatusNotice(ctx, spoke, statusNarrowedListing(t, "open"), 1, "beads") {
		t.Error("a narrowed listing that hid a live row owes a disclosure")
	}
}
