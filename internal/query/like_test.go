package query

import (
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/beads/internal/types"
)

// The LIKE operator exists because "does a bead about X already exist?" is the
// duplicate check that precedes filing, and an operator that answers "no"
// regardless of the data manufactures duplicates instead of preventing them
// (bd-791). These tests pin the two properties that failure violated: a
// wildcard-free LIKE agrees with =, and an unsupported LIKE is a loud error
// rather than an empty result set.

func TestLexerLike(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  []TokenType
	}{
		{
			name:  "LIKE uppercase",
			input: `title LIKE "%auth%"`,
			want:  []TokenType{TokenIdent, TokenLike, TokenString, TokenEOF},
		},
		{
			name:  "like lowercase",
			input: `title like "%auth%"`,
			want:  []TokenType{TokenIdent, TokenLike, TokenString, TokenEOF},
		},
		{
			name:  "NOT LIKE",
			input: `title NOT LIKE "%auth%"`,
			want:  []TokenType{TokenIdent, TokenNot, TokenLike, TokenString, TokenEOF},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tokens, err := NewLexer(tt.input).Tokenize()
			if err != nil {
				t.Fatalf("Tokenize() error = %v", err)
			}
			if len(tokens) != len(tt.want) {
				t.Fatalf("got %d tokens, want %d: %v", len(tokens), len(tt.want), tokens)
			}
			for i, want := range tt.want {
				if tokens[i].Type != want {
					t.Errorf("token %d: got %s, want %s", i, tokens[i].Type, want)
				}
			}
		})
	}
}

func TestParserLike(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "LIKE comparison",
			input:    `title LIKE "%auth%"`,
			expected: "title LIKE %auth%",
		},
		{
			name:     "NOT LIKE desugars to NOT (field LIKE value)",
			input:    `title NOT LIKE "%auth%"`,
			expected: "NOT title LIKE %auth%",
		},
		{
			name:     "LIKE composes with AND",
			input:    `title LIKE "mol-deacon%" AND status=open`,
			expected: "(title LIKE mol-deacon% AND status=open)",
		},
		{
			name:     "like is still usable as a value",
			input:    "assignee=like",
			expected: "assignee=like",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			node, err := Parse(tt.input)
			if err != nil {
				t.Fatalf("Parse() error = %v", err)
			}
			if got := node.String(); got != tt.expected {
				t.Errorf("got %q, want %q", got, tt.expected)
			}
		})
	}
}

// TestParserMissingOperatorErrorNamesTheOperators is the regression guard for
// the half of bd-791 that made the failure look like an empty result: the
// error has to name the operator that was missing and the ones on offer.
func TestParserMissingOperatorErrorNamesTheOperators(t *testing.T) {
	_, err := Parse(`title SIMILAR "%auth%"`)
	if err == nil {
		t.Fatal("expected error for unknown operator, got nil")
	}
	for _, want := range []string{"title", "LIKE", "SIMILAR"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err.Error(), want)
		}
	}
}

func TestEvaluatorLikeBuildsSQLFilter(t *testing.T) {
	now := time.Date(2025, 2, 4, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name   string
		query  string
		verify func(*testing.T, *types.IssueFilter)
	}{
		{
			name:  "title LIKE binds the pattern verbatim",
			query: `title LIKE "mol-deacon%"`,
			verify: func(t *testing.T, f *types.IssueFilter) {
				if f.TitleLike != "mol-deacon%" {
					t.Errorf("TitleLike = %q, want %q", f.TitleLike, "mol-deacon%")
				}
				if f.TitleContains != "" {
					t.Errorf("TitleContains = %q, want empty (LIKE must not add its own wildcards)", f.TitleContains)
				}
			},
		},
		{
			name:  "description LIKE",
			query: `description LIKE "%imposters%"`,
			verify: func(t *testing.T, f *types.IssueFilter) {
				if f.DescriptionLike != "%imposters%" {
					t.Errorf("DescriptionLike = %q", f.DescriptionLike)
				}
			},
		},
		{
			name:  "notes LIKE",
			query: `notes LIKE "%handoff%"`,
			verify: func(t *testing.T, f *types.IssueFilter) {
				if f.NotesLike != "%handoff%" {
					t.Errorf("NotesLike = %q", f.NotesLike)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := EvaluateAt(tt.query, now)
			if err != nil {
				t.Fatalf("EvaluateAt() error = %v", err)
			}
			// A plain LIKE must stay in SQL: predicate mode caps the rows it
			// pre-fetches, which is how a real match becomes a false zero.
			if result.RequiresPredicate {
				t.Error("LIKE fell back to in-memory predicate filtering; it must be pushed down to SQL")
			}
			tt.verify(t, &result.Filter)
		})
	}
}

func TestEvaluatorLikeRejectsUnsupportedFields(t *testing.T) {
	now := time.Date(2025, 2, 4, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name        string
		query       string
		wantMessage string
	}{
		{"status", "status LIKE open", "does not support LIKE"},
		{"assignee", `assignee LIKE "%deacon%"`, "does not support LIKE"},
		{"id", `id LIKE "gt-%"`, "prefix*"},
		{"priority", "priority LIKE 1", "does not support LIKE"},
		{"metadata", `metadata.sprint LIKE "%q3%"`, "does not support LIKE"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := EvaluateAt(tt.query, now)
			if err == nil {
				t.Fatal("expected an error, got nil (an unsupported LIKE must never look like an empty result)")
			}
			if !strings.Contains(err.Error(), tt.wantMessage) {
				t.Errorf("error %q does not contain %q", err.Error(), tt.wantMessage)
			}
		})
	}
}

func TestEvaluatorLikeRejectsUnusablePatterns(t *testing.T) {
	now := time.Date(2025, 2, 4, 12, 0, 0, 0, time.UTC)

	for _, tt := range []struct {
		name  string
		query string
	}{
		{"empty pattern", `title LIKE ""`},
		{"backslash escape", `title LIKE "50\\% done"`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := EvaluateAt(tt.query, now); err == nil {
				t.Fatal("expected an error, got nil")
			}
		})
	}
}

func TestLikePredicateEvaluation(t *testing.T) {
	now := time.Date(2025, 2, 4, 12, 0, 0, 0, time.UTC)

	issues := []*types.Issue{
		{ID: "hq-1", Title: "mol-deacon-patrol", Description: "runs the patrol"},
		{ID: "hq-2", Title: "mol-deacon-sweep", Description: "sweeps"},
		{ID: "hq-3", Title: "gt health reports zombie dolt servers", Description: "kill-imposters cannot see them"},
	}

	tests := []struct {
		name    string
		query   string
		wantIDs []string
	}{
		{
			// The core defect: with no wildcard, LIKE must agree with =.
			name:    "no wildcard matches like equality does",
			query:   `title LIKE "mol-deacon-patrol" OR id=nothing`,
			wantIDs: []string{"hq-1"},
		},
		{
			name:    "trailing wildcard is anchored at the front",
			query:   `title LIKE "mol-deacon%" OR id=nothing`,
			wantIDs: []string{"hq-1", "hq-2"},
		},
		{
			name:    "leading wildcard is not anchored",
			query:   `title LIKE "%patrol" OR id=nothing`,
			wantIDs: []string{"hq-1"},
		},
		{
			name:    "substring pattern",
			query:   `description LIKE "%imposters%" OR id=nothing`,
			wantIDs: []string{"hq-3"},
		},
		{
			name:    "underscore matches exactly one character",
			query:   `title LIKE "mol-deacon-swee_" OR id=nothing`,
			wantIDs: []string{"hq-2"},
		},
		{
			name:    "match is case-insensitive",
			query:   `title LIKE "%DEACON%" OR id=nothing`,
			wantIDs: []string{"hq-1", "hq-2"},
		},
		{
			name:    "NOT LIKE negates",
			query:   `title NOT LIKE "mol-%"`,
			wantIDs: []string{"hq-3"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := EvaluateAt(tt.query, now)
			if err != nil {
				t.Fatalf("EvaluateAt() error = %v", err)
			}
			if result.Predicate == nil {
				t.Fatal("expected a predicate for this query")
			}
			var got []string
			for _, issue := range issues {
				if result.Predicate(issue) {
					got = append(got, issue.ID)
				}
			}
			if len(got) != len(tt.wantIDs) {
				t.Fatalf("matched %v, want %v", got, tt.wantIDs)
			}
			for i, want := range tt.wantIDs {
				if got[i] != want {
					t.Errorf("match %d: got %s, want %s", i, got[i], want)
				}
			}
		})
	}
}

func TestLikeMatch(t *testing.T) {
	tests := []struct {
		pattern string
		subject string
		want    bool
	}{
		{"abc", "abc", true},
		{"abc", "abcd", false},
		{"abc", "xabc", false},
		{"%", "", true},
		{"%", "anything", true},
		{"%%", "anything", true},
		{"a%", "abc", true},
		{"a%", "bac", false},
		{"%c", "abc", true},
		{"%b%", "abc", true},
		{"%z%", "abc", false},
		{"_bc", "abc", true},
		{"_bc", "bc", false},
		{"a_c", "abc", true},
		{"a_c", "ac", false},
		{"%a%b%c%", "xxaxxbxxcxx", true},
		{"%a%b%c%", "xxaxxcxxbxx", false},
		{"ABC", "abc", true},
		{"%AUTH%", "refresh auth token", true},
		// Backtracking must not blow up on a pathological pattern.
		{strings.Repeat("%a", 20), strings.Repeat("a", 200) + "b", false},
	}

	for _, tt := range tests {
		t.Run(tt.pattern+"~"+tt.subject, func(t *testing.T) {
			if got := likeMatch(tt.pattern, tt.subject); got != tt.want {
				t.Errorf("likeMatch(%q, %q) = %v, want %v", tt.pattern, tt.subject, got, tt.want)
			}
		})
	}
}
