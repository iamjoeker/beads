package utils

import (
	"context"
	"strings"
	"testing"

	"github.com/steveyegge/beads/internal/types"
)

// fakeResolverStore is an in-memory PartialIDResolverStore holding the two
// planes separately. SearchIssues never merges wisps, which models the
// transaction-level store the wisp fallback in ResolvePartialID exists for.
type fakeResolverStore struct {
	issues []string
	wisps  []string
	config map[string]string
}

func (f *fakeResolverStore) SearchIssues(ctx context.Context, query string, filter types.IssueFilter) ([]*types.Issue, error) {
	ids, err := f.SearchIssueIDs(ctx, query, filter)
	if err != nil {
		return nil, err
	}
	issues := make([]*types.Issue, 0, len(ids))
	for _, id := range ids {
		issues = append(issues, &types.Issue{ID: id})
	}
	return issues, nil
}

func (f *fakeResolverStore) SearchIssueIDs(ctx context.Context, query string, filter types.IssueFilter) ([]string, error) {
	pool := f.issues
	if filter.Ephemeral != nil && *filter.Ephemeral {
		pool = f.wisps
	}

	var out []string
	for _, id := range pool {
		if len(filter.IDs) > 0 {
			for _, want := range filter.IDs {
				if id == want {
					out = append(out, id)
					break
				}
			}
			continue
		}
		// Mirrors the storage layer's `id LIKE %query%` filtering.
		if query == "" || strings.Contains(id, query) {
			out = append(out, id)
		}
	}
	return out, nil
}

func (f *fakeResolverStore) GetConfig(ctx context.Context, key string) (string, error) {
	return f.config[key], nil
}

// TestResolvePartialID_WispPlaneMarker pins which lookups may cross planes.
// A prefixed id names a plane, so it must carry the "wisp-" marker to reach a
// wisp; a bare hash names no plane, so it may reach either (bd-qsw).
func TestResolvePartialID_WispPlaneMarker(t *testing.T) {
	ctx := context.Background()
	store := &fakeResolverStore{
		issues: []string{"hq-a3f8e9"},
		wisps:  []string{"hq-wisp-gyn1c8", "hq-custom1"},
		config: map[string]string{"issue_prefix": "hq"},
	}

	tests := []struct {
		name     string
		input    string
		expected string
		errorMsg string
	}{
		{
			// The bd-qsw regression: "hq-gyn" is an issue-plane lookup, and
			// "hq-wisp-gyn1c8" is not an abbreviation of it.
			name:     "plane-qualified issue lookup does not resolve to a wisp",
			input:    "hq-gyn",
			errorMsg: "no issue found",
		},
		{
			name:     "cross-plane near miss names the wisp it declined to return",
			input:    "hq-gyn",
			errorMsg: "hq-wisp-gyn1c8",
		},
		{
			name:     "plane-qualified wisp abbreviation resolves",
			input:    "hq-wisp-gyn",
			expected: "hq-wisp-gyn1c8",
		},
		{
			name:     "full wisp ID resolves",
			input:    "hq-wisp-gyn1c8",
			expected: "hq-wisp-gyn1c8",
		},
		{
			name:     "wisp marker without issue prefix resolves",
			input:    "wisp-gyn1c8",
			expected: "hq-wisp-gyn1c8",
		},
		{
			name:     "bare hash still crosses into the wisp plane",
			input:    "gyn1c8",
			expected: "hq-wisp-gyn1c8",
		},
		{
			name:     "bare hash abbreviation still crosses into the wisp plane",
			input:    "gyn1",
			expected: "hq-wisp-gyn1c8",
		},
		{
			// Ephemerals created with --id=<custom> carry no "wisp-" infix, so
			// nothing is stripped and a prefixed lookup reaches them normally.
			name:     "custom-ID ephemeral resolves from a plane-qualified input",
			input:    "hq-custom",
			expected: "hq-custom1",
		},
		{
			name:     "issue-plane abbreviation is untouched",
			input:    "hq-a3f8",
			expected: "hq-a3f8e9",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := ResolvePartialID(ctx, store, tt.input)

			if tt.errorMsg != "" {
				if err == nil {
					t.Fatalf("ResolvePartialID(%q) = %q, nil; want error containing %q", tt.input, result, tt.errorMsg)
				}
				if !strings.Contains(err.Error(), tt.errorMsg) {
					t.Fatalf("ResolvePartialID(%q) error = %q; want it to contain %q", tt.input, err.Error(), tt.errorMsg)
				}
				return
			}

			if err != nil {
				t.Fatalf("ResolvePartialID(%q) unexpected error: %v", tt.input, err)
			}
			if result != tt.expected {
				t.Fatalf("ResolvePartialID(%q) = %q; want %q", tt.input, result, tt.expected)
			}
		})
	}
}

// TestResolvePartialID_CrossPlaneReportsEveryCandidate verifies the cross-plane
// message lists all near misses deterministically instead of picking one.
func TestResolvePartialID_CrossPlaneReportsEveryCandidate(t *testing.T) {
	ctx := context.Background()
	store := &fakeResolverStore{
		wisps:  []string{"hq-wisp-gyn9zz", "hq-wisp-gyn1c8"},
		config: map[string]string{"issue_prefix": "hq"},
	}

	_, err := ResolvePartialID(ctx, store, "hq-gyn")
	if err == nil {
		t.Fatal("ResolvePartialID(\"hq-gyn\") succeeded; want a cross-plane error")
	}
	want := "[hq-wisp-gyn1c8 hq-wisp-gyn9zz]"
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %q; want it to contain %q", err.Error(), want)
	}
}

// TestResolvePartialID_BareHashWispAmbiguityStillReported guards the bare-hash
// path: dropping the marker is still allowed there, and two wisps sharing a
// hash prefix must remain ambiguous rather than silently picking one.
func TestResolvePartialID_BareHashWispAmbiguityStillReported(t *testing.T) {
	ctx := context.Background()
	store := &fakeResolverStore{
		wisps:  []string{"hq-wisp-gyn9zz", "hq-wisp-gyn1c8"},
		config: map[string]string{"issue_prefix": "hq"},
	}

	_, err := ResolvePartialID(ctx, store, "gyn")
	if err == nil {
		t.Fatal("ResolvePartialID(\"gyn\") succeeded; want an ambiguity error")
	}
	if !strings.Contains(err.Error(), "matches 2 issues") {
		t.Fatalf("error = %q; want it to report 2 matches", err.Error())
	}
}
