package query

import (
	"fmt"
	"strings"
)

// LikeFields are the fields that accept a LIKE pattern. They are exactly the
// free-text columns the storage layer can pattern-match in SQL. Restricting
// LIKE to them is deliberate: a field that silently fell back to in-memory
// filtering would be capped by the pre-filter row limit and could report an
// empty result for a bead that exists, which is the failure this operator was
// added to remove (bd-791).
var LikeFields = map[string]bool{
	"title":       true,
	"description": true,
	"desc":        true,
	"notes":       true,
}

// likeFieldError explains that a field does not accept LIKE, and points at the
// operator that does the equivalent job for that field.
func likeFieldError(field string) error {
	switch field {
	case "id", "spec", "spec_id":
		return fmt.Errorf("%s does not support LIKE; use %s=prefix* for prefix matching", field, field)
	default:
		return fmt.Errorf("%s does not support LIKE; LIKE works on title, description, and notes, and %s=value already matches case-insensitively", field, field)
	}
}

// validateLikePattern checks a LIKE pattern before it reaches SQL.
//
// Backslash is rejected rather than passed through: MySQL/Dolt treat it as the
// default LIKE escape character while the in-memory matcher used for OR/NOT
// queries does not, so a pattern containing one would match different rows
// depending on which path evaluated it. An empty pattern is rejected because
// it would match only an empty column, which is never what a caller means.
func validateLikePattern(pattern string) (string, error) {
	if pattern == "" {
		return "", fmt.Errorf("LIKE pattern must not be empty")
	}
	if strings.Contains(pattern, "\\") {
		return "", fmt.Errorf("LIKE pattern %q contains a backslash; escape sequences are not supported", pattern)
	}
	return pattern, nil
}

// likeMatch reports whether s matches a SQL LIKE pattern, case-insensitively.
// '%' matches any run of characters (including none) and '_' matches exactly
// one. This mirrors what the storage layer evaluates as LOWER(col) LIKE ?, so
// filter-mode and predicate-mode queries agree on the same rows.
func likeMatch(pattern, s string) bool {
	p := []rune(strings.ToLower(pattern))
	t := []rune(strings.ToLower(s))

	// Greedy scan with backtracking to the most recent '%': linear in the
	// common case and never exponential, unlike naive recursion.
	var pi, ti int
	lastStar := -1
	lastStarTi := 0
	for ti < len(t) {
		switch {
		case pi < len(p) && (p[pi] == '_' || p[pi] == t[ti]):
			pi++
			ti++
		case pi < len(p) && p[pi] == '%':
			lastStar = pi
			lastStarTi = ti
			pi++
		case lastStar >= 0:
			// Mismatch: let the last '%' absorb one more character.
			lastStarTi++
			ti = lastStarTi
			pi = lastStar + 1
		default:
			return false
		}
	}
	for pi < len(p) && p[pi] == '%' {
		pi++
	}
	return pi == len(p)
}
