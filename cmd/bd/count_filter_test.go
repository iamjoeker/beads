//go:build cgo

package main

import (
	"reflect"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/beads/issueops"
)

// The filter semantics of `bd count` live in the Counter role now, built once
// in workapi.BuildCountFilter, with the cardinality-parity assertions in
// internal/workapi/count_test.go. What is left here is turning flags into the
// role's request and refusing a combination the request cannot express.

// TestCountIncludeInfraFlagShape pins the flag's existence and default so
// scripted callers keep byte-identical behavior (GH#4387). The no-flag path
// carries IncludeInfra=false into the role, where the durable-only default is
// now decided.
func TestCountIncludeInfraFlagShape(t *testing.T) {
	flag := countCmd.Flags().Lookup("include-infra")
	if flag == nil {
		t.Fatal("bd count must expose an --include-infra flag (GH#4387)")
	}
	if flag.DefValue != "false" {
		t.Fatalf("--include-infra must default to false, got %q", flag.DefValue)
	}

	initConfigWithExcludeLabels(t, "", "")
	request, _, err := parseCountRequest(newCountFlagSet(t))
	if err != nil {
		t.Fatalf("parseCountRequest with no flags set: %v", err)
	}
	if request.IncludeInfra {
		t.Error("IncludeInfra = true with no flags set, want the durable-only default")
	}
}

// TestParseCountRequestCarriesEveryFilterFlag is the tripwire for a flag that
// is registered, documented and silently dropped on the way into the request.
// Every filter flag is set to a value distinguishable from its zero and read
// back off the request.
func TestParseCountRequestCarriesEveryFilterFlag(t *testing.T) {
	// parseCountRequest resolves --exclude-label against the workspace's
	// list.exclude-labels, so the key is pinned UNSET here: an ambient value or
	// this repository's own .beads/config.yaml would otherwise be unioned into
	// the expectation below and the case would be measuring the machine.
	initConfigWithExcludeLabels(t, "", "")
	flags := newCountFlagSet(t)
	for flag, value := range map[string]string{
		"status":            "closed",
		"assignee":          "alice",
		"type":              "bug",
		"label":             "alpha,beta",
		"label-any":         "gamma",
		"exclude-label":     "delta",
		"title":             "needle",
		"id":                "bd-1,bd-2",
		"title-contains":    "tc",
		"desc-contains":     "dc",
		"notes-contains":    "nc",
		"created-after":     "2026-01-01",
		"created-before":    "2026-01-02",
		"updated-after":     "2026-01-03",
		"updated-before":    "2026-01-04",
		"closed-after":      "2026-01-05",
		"closed-before":     "2026-01-06",
		"empty-description": "true",
		"no-assignee":       "true",
		"no-labels":         "true",
		"priority":          "1",
		"priority-min":      "0",
		"priority-max":      "4",
		"include-infra":     "true",
	} {
		if err := flags.Flags().Set(flag, value); err != nil {
			t.Fatalf("set --%s=%s: %v", flag, value, err)
		}
	}

	request, group, err := parseCountRequest(flags)
	if err != nil {
		t.Fatalf("parseCountRequest: %v", err)
	}
	if group != "" {
		t.Errorf("group = %q with no --by-* flag, want the scalar count", group)
	}

	// parseTimeFlag resolves a bare date in the LOCAL zone, which is what a
	// user typing --created-after 2026-01-01 means, then normalizes the
	// representation to UTC so the storage layer binds the same instant on
	// every backend. The expectation constructs local midnight and converts,
	// so a change to either half of that contract shows up here instead of
	// shifting every bound by the test machine's offset.
	day := func(d int) *time.Time {
		stamp := time.Date(2026, 1, d, 0, 0, 0, 0, time.Local).UTC()
		return &stamp
	}
	priority, min, max := 1, 0, 4
	want := issueops.CountRequest{
		Status:        "closed",
		IssueType:     "bug",
		Assignee:      "alice",
		Priority:      &priority,
		PriorityMin:   &min,
		PriorityMax:   &max,
		Labels:        []string{"alpha", "beta"},
		LabelsAny:     []string{"gamma"},
		ExcludeLabels: []string{"delta"},
		TitleSearch:   "needle",
		IDFilter:      "bd-1,bd-2",
		TitleContains: "tc",
		DescContains:  "dc",
		NotesContains: "nc",
		CreatedAfter:  day(1),
		CreatedBefore: day(2),
		UpdatedAfter:  day(3),
		UpdatedBefore: day(4),
		ClosedAfter:   day(5),
		ClosedBefore:  day(6),
		EmptyDesc:     true,
		NoAssignee:    true,
		NoLabels:      true,
		IncludeInfra:  true,
	}
	if !reflect.DeepEqual(request, want) {
		t.Errorf("parseCountRequest built\n %#v\nwant\n %#v", request, want)
	}
}

// TestParseCountRequestResolvesTheGroupingFlags pins each --by-* flag to its
// dimension and the refusal for two at once. The exclusivity check cannot live
// behind the role — by then only one dimension is left — so it is checked here.
func TestParseCountRequestResolvesTheGroupingFlags(t *testing.T) {
	for flag, want := range map[string]issueops.CountGroup{
		"by-status":   issueops.CountGroupStatus,
		"by-priority": issueops.CountGroupPriority,
		"by-type":     issueops.CountGroupType,
		"by-assignee": issueops.CountGroupAssignee,
		"by-label":    issueops.CountGroupLabel,
	} {
		flags := newCountFlagSet(t)
		if err := flags.Flags().Set(flag, "true"); err != nil {
			t.Fatalf("set --%s: %v", flag, err)
		}
		_, group, err := parseCountRequest(flags)
		if err != nil {
			t.Fatalf("parseCountRequest(--%s): %v", flag, err)
		}
		if group != want {
			t.Errorf("--%s resolved to %q, want %q", flag, group, want)
		}
	}

	flags := newCountFlagSet(t)
	for _, flag := range []string{"by-status", "by-label"} {
		if err := flags.Flags().Set(flag, "true"); err != nil {
			t.Fatalf("set --%s: %v", flag, err)
		}
	}
	if _, _, err := parseCountRequest(flags); err == nil {
		t.Fatal("two --by-* flags were accepted, want a refusal")
	}
}

// TestParseCountRequestRejectsAnUnparseableDate pins that a bad date bound is
// refused at the flag seam rather than reaching the role as a zero time, which
// would silently widen the count to everything.
func TestParseCountRequestRejectsAnUnparseableDate(t *testing.T) {
	flags := newCountFlagSet(t)
	if err := flags.Flags().Set("created-after", "not-a-date"); err != nil {
		t.Fatalf("set --created-after: %v", err)
	}
	if _, _, err := parseCountRequest(flags); err == nil {
		t.Fatal("an unparseable --created-after was accepted, want a refusal")
	}
}

// TestParseCountRequestAppliesTheConfiguredExclusions is bd-1v3's symptom
// asserted directly: on a store that sets list.exclude-labels, the number
// `bd count` returns must describe the same set `bd list` shows rows for.
//
// The three sub-cases are the three states the pairing has to get right, and
// the FIRST is the one that protects everyone else — a workspace that does not
// set the key must see no change at all.
func TestParseCountRequestAppliesTheConfiguredExclusions(t *testing.T) {
	// Not parallel: config is process-global viper state, as the sibling cases
	// in list_exclude_labels_test.go note.
	t.Run("unset key changes nothing", func(t *testing.T) {
		initConfigWithExcludeLabels(t, "", "")
		var request issueops.CountRequest
		captureStderrLines(t, func() {
			var err error
			request, _, err = parseCountRequest(newCountFlagSet(t))
			if err != nil {
				t.Errorf("parseCountRequest: %v", err)
			}
		})
		// EMPTY, not nil: resolveExcludeLabels returns NormalizeLabels' own
		// slice, which is allocated and empty for an unset key. What matters is
		// that it contributes no exclusion — BuildCountFilter tests len — and
		// an empty slice is what `bd list` has always carried here too.
		if len(request.ExcludeLabels) != 0 {
			t.Errorf("ExcludeLabels = %v, want none on an unconfigured store", request.ExcludeLabels)
		}
	})

	t.Run("configured key reaches the request", func(t *testing.T) {
		initConfigWithExcludeLabels(t, "BD_LIST_EXCLUDE_LABELS", "gt:message")
		var request issueops.CountRequest
		captureStderrLines(t, func() {
			var err error
			request, _, err = parseCountRequest(newCountFlagSet(t))
			if err != nil {
				t.Errorf("parseCountRequest: %v", err)
			}
		})
		if want := []string{"gt:message"}; !reflect.DeepEqual(request.ExcludeLabels, want) {
			t.Errorf("ExcludeLabels = %v, want %v — the count still answers about the unfiltered set",
				request.ExcludeLabels, want)
		}
	})

	t.Run("include-hidden drops it", func(t *testing.T) {
		initConfigWithExcludeLabels(t, "BD_LIST_EXCLUDE_LABELS", "gt:message")
		flags := newCountFlagSet(t)
		if err := flags.Flags().Set(includeHiddenFlag, "true"); err != nil {
			t.Fatalf("set --%s: %v", includeHiddenFlag, err)
		}
		var request issueops.CountRequest
		captureStderrLines(t, func() {
			var err error
			request, _, err = parseCountRequest(flags)
			if err != nil {
				t.Errorf("parseCountRequest: %v", err)
			}
		})
		if len(request.ExcludeLabels) != 0 {
			t.Errorf("ExcludeLabels = %v, want none: --%s is the opt-out that makes the default safe",
				request.ExcludeLabels, includeHiddenFlag)
		}
	})
}

// newCountFlagSet returns a command carrying `bd count`'s flags at their
// defaults. It REGISTERS them rather than copying countCmd's set: cobra's
// AddFlagSet shares the underlying *Flag values, so a case that set a flag on
// the copy would leak it into the real command and into every later case.
func newCountFlagSet(t *testing.T) *cobra.Command {
	t.Helper()
	cmd := &cobra.Command{Use: "count"}
	registerCountFlags(cmd)
	return cmd
}
