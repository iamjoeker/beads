package main

import (
	"fmt"
	"strings"
)

// errEmptyIssueIDArg refuses a positional issue id that is empty or
// whitespace-only, before any write path runs.
//
// This is the quoted sibling of the no-ID last-touched fallback guard
// (see AllowLastTouchedFallback). An unquoted substitution that yields
// nothing drops the argument entirely — `bd close $(...)` becomes
// `bd close` — and that case is refused in each command's Args validator.
// A *quoted* one keeps the argument and hands the command an empty string:
// `bd close "$ID"` with $ID unset is `bd close ""`.
//
// No issue id is empty, so an empty positional is never something the caller
// meant; it is a failed expansion wearing the shape of an argument. Naming it
// as such beats the resolver's generic `no issue found matching ""`, which
// reads as "that bead is gone" rather than "your variable was empty" (bd-lrk).
func errEmptyIssueIDArg(args []string) error {
	for i, arg := range args {
		if strings.TrimSpace(arg) == "" {
			return fmt.Errorf("positional argument %d is an empty issue ID — a quoted shell expansion that produced nothing looks like this (e.g. `\"$ID\"` with $ID unset); pass an explicit issue ID", i+1)
		}
	}
	return nil
}
