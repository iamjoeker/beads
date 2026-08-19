//go:build cgo

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/beads/internal/types"
)

// The listing's own scope is what bd-nc4 is about: the store it read and the
// closed rows it hid are both invisible in the rows, so they are asserted here
// as output, not as internal state.

func wispIssue(id string, status types.Status) *types.Issue {
	return &types.Issue{
		ID:        id,
		Title:     id,
		Status:    status,
		IssueType: types.IssueType("task"),
		UpdatedAt: time.Now(),
	}
}

func TestBuildWispListResultReportsHiddenClosed(t *testing.T) {
	issues := []*types.Issue{
		wispIssue("w-open", types.StatusOpen),
		wispIssue("w-closed-1", types.StatusClosed),
		wispIssue("w-closed-2", types.StatusClosed),
	}
	ref := wispStoreRef{Database: "hq", BeadsDir: "/town/.beads", Current: true}

	result := buildWispListResultFromStores([]wispStoreResult{{Ref: ref, Issues: issues}}, false, false, "")
	if result.Count != 1 {
		t.Fatalf("expected 1 open wisp listed, got %d", result.Count)
	}
	if result.HiddenClosed != 2 {
		t.Errorf("expected 2 closed wisps reported as hidden, got %d", result.HiddenClosed)
	}
	if result.IncludedClosed {
		t.Error("IncludedClosed must be false without --all")
	}
	if len(result.Stores) != 1 {
		t.Fatalf("expected the store to be named, got %d stores", len(result.Stores))
	}
	if got := result.Stores[0]; got.Total != 3 || got.Closed != 2 || got.Shown != 1 {
		t.Errorf("store summary should be the differential count (3 total, 2 closed, 1 shown), got %+v", got)
	}

	all := buildWispListResultFromStores([]wispStoreResult{{Ref: ref, Issues: issues}}, true, false, "")
	if all.Count != 3 || all.HiddenClosed != 0 || !all.IncludedClosed {
		t.Errorf("--all should list all 3 and hide nothing, got count=%d hidden=%d included=%v",
			all.Count, all.HiddenClosed, all.IncludedClosed)
	}
}

// A store that failed to answer must survive into the output. Folding it into
// the zero is the defect: an unsearched store and an empty one then look the
// same.
func TestBuildWispListResultKeepsUnreachableStore(t *testing.T) {
	results := []wispStoreResult{
		{Ref: wispStoreRef{Database: "hq", BeadsDir: "/town/.beads", Current: true}, Issues: []*types.Issue{wispIssue("w-1", types.StatusOpen)}},
		{Ref: wispStoreRef{Database: "gastown", BeadsDir: "/town/gastown/.beads", Rig: "gastown"}, Err: os.ErrPermission},
	}
	result := buildWispListResultFromStores(results, true, true, "/town/.beads/routes.jsonl")

	if len(result.Stores) != 2 {
		t.Fatalf("expected both stores reported, got %d", len(result.Stores))
	}
	failed := result.Stores[1]
	if failed.Error == "" {
		t.Fatal("the unreachable store must carry its error")
	}
	if failed.Shown != 0 || failed.Total != 0 {
		t.Errorf("an unreachable store has no counts to report, got %+v", failed)
	}

	lines := strings.Join(wispListScopeLines(result), "\n")
	if !strings.Contains(lines, "NOT searched") {
		t.Errorf("scope lines must say the store was not searched:\n%s", lines)
	}
	if !strings.Contains(lines, "routes.jsonl") {
		t.Errorf("an --all-stores sweep must name the routes file that bounded it:\n%s", lines)
	}
}

func TestBuildWispListResultAttributesRowsOnlyAcrossStores(t *testing.T) {
	single := buildWispListResultFromStores([]wispStoreResult{
		{Ref: wispStoreRef{Database: "hq", BeadsDir: "/town/.beads", Current: true}, Issues: []*types.Issue{wispIssue("w-1", types.StatusOpen)}},
	}, true, false, "")
	if single.Wisps[0].Store != "" {
		t.Errorf("a single-store listing names its store in the header, not per row; got %q", single.Wisps[0].Store)
	}

	multi := buildWispListResultFromStores([]wispStoreResult{
		{Ref: wispStoreRef{Database: "hq", BeadsDir: "/town/.beads", Current: true}, Issues: []*types.Issue{wispIssue("w-1", types.StatusOpen)}},
		{Ref: wispStoreRef{Database: "gastown", BeadsDir: "/town/gastown/.beads", Rig: "gastown"}, Issues: []*types.Issue{wispIssue("w-2", types.StatusOpen)}},
	}, true, true, "")
	for _, item := range multi.Wisps {
		if item.Store == "" {
			t.Errorf("a multi-store listing must attribute row %s to a store", item.ID)
		}
	}
}

func TestWispListScopeLinesAlwaysStateStoreAndScope(t *testing.T) {
	result := buildWispListResultFromStores([]wispStoreResult{
		{Ref: wispStoreRef{Database: "hq", BeadsDir: "/town/.beads", Current: true}},
	}, false, false, "")
	if result.Count != 0 {
		t.Fatalf("expected an empty listing, got %d", result.Count)
	}

	lines := strings.Join(wispListScopeLines(result), "\n")
	for _, want := range []string{`database "hq"`, "/town/.beads", "open and in_progress only", "--all-stores"} {
		if !strings.Contains(lines, want) {
			t.Errorf("an EMPTY listing must still disclose %q:\n%s", want, lines)
		}
	}
}

func TestBuildWispListResultFlagsTruncation(t *testing.T) {
	issues := make([]*types.Issue, wispListRowCap)
	for i := range issues {
		issues[i] = wispIssue("w", types.StatusOpen)
	}
	result := buildWispListResultFromStores([]wispStoreResult{
		{Ref: wispStoreRef{Database: "hq", BeadsDir: "/town/.beads", Current: true}, Issues: issues},
	}, true, false, "")
	if !result.Stores[0].Truncated {
		t.Fatal("a page at the row cap must be reported as truncated, not as a complete answer")
	}
	if !strings.Contains(strings.Join(wispListScopeLines(result), "\n"), "TRUNCATED") {
		t.Error("truncation must be visible on screen, not only in JSON")
	}
}

func TestSelectWispStores(t *testing.T) {
	stores := []wispStoreRef{
		{Database: "hq", BeadsDir: "/town/.beads", Rig: ".", Current: true},
		{Database: "gastown", BeadsDir: "/town/gastown/mayor/rig/.beads", Rig: "gastown/mayor/rig"},
	}

	for _, selector := range []string{"gastown", "gastown/mayor/rig", "rig", "GASTOWN"} {
		matched, err := selectWispStores(stores, selector)
		if err != nil {
			t.Fatalf("selector %q: %v", selector, err)
		}
		if len(matched) != 1 || matched[0].Database != "gastown" {
			t.Errorf("selector %q matched %+v", selector, matched)
		}
	}

	if _, err := selectWispStores(stores, "nope"); err == nil {
		t.Fatal("an unmatched --rig must be an error, never an empty listing")
	} else if !strings.Contains(err.Error(), "gastown") {
		t.Errorf("the error must name what was available, got %v", err)
	}

	all, err := selectWispStores(stores, "")
	if err != nil || len(all) != 2 {
		t.Errorf("an empty selector selects everything, got %d (%v)", len(all), err)
	}
}

func TestDiscoverWispStoresDedupesAndOrders(t *testing.T) {
	town := t.TempDir()
	townBeads := filepath.Join(town, ".beads")
	rigBeads := filepath.Join(town, "rig", ".beads")
	for dir, database := range map[string]string{townBeads: "hq", rigBeads: "rigdb"} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "metadata.json"),
			[]byte(`{"dolt_database":"`+database+`"}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Two prefixes routing to the same rig, plus the town route, which is also
	// the current store here.
	routes := `{"prefix":"a-","path":"rig"}
{"prefix":"b-","path":"rig"}
{"prefix":"hq-","path":"."}
`
	if err := os.WriteFile(filepath.Join(townBeads, "routes.jsonl"), []byte(routes), 0o600); err != nil {
		t.Fatal(err)
	}

	stores, routesFile := discoverWispStores(townBeads)
	if routesFile == "" {
		t.Fatal("the routes file that bounded the sweep must be reported")
	}
	if len(stores) != 2 {
		t.Fatalf("two prefixes to one rig plus the current store is 2 stores, got %d: %+v", len(stores), stores)
	}
	if !stores[0].Current || stores[0].Database != "hq" {
		t.Errorf("the current store must come first so a default listing keeps reading it, got %+v", stores[0])
	}
	if stores[1].Database != "rigdb" || stores[1].Rig != "rig" {
		t.Errorf("expected the routed rig second, got %+v", stores[1])
	}
}
