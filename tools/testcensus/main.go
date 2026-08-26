// Command testcensus reads `go test -json` on stdin, writes back the plain
// non-verbose output a reader expects, and then reports the one thing the go
// tool structurally cannot: how many tests SKIPPED, and which packages printed
// "ok" having run nothing at all.
//
// WHY THIS EXISTS (bd-5er). 283 t.Skip statements across 126 files gate
// themselves on BEADS_TEST_EMBEDDED_DOLT, and a package whose every test skips
// still prints
//
//	ok  	github.com/steveyegge/beads/internal/storage/embeddeddolt	0.001s
//
// which is byte-for-byte what a package whose every test PASSED prints. bd-dln
// caught its sibling defect off a runtime — internal/storage/uow finishing in
// 0.348s — and there is no counterpart tell here: skipping 61 tests and
// passing 61 trivially fast tests look the same from outside.
//
// This is not a beads convention that could be tightened by printing harder
// from inside the tests. Measured on go1.26.6: a TestMain that writes a census
// to BOTH os.Stdout and os.Stderr after m.Run() produces, from `go test`,
// exactly
//
//	ok  	probe/m	0.002s
//
// and nothing else. The go tool buffers a test binary's whole output and
// discards it when the package passes. The only channels that survive a
// passing package are the ok line itself, -v, and -json. Hence a -json filter:
// it is the only place the information exists.
//
// Usage:
//
//	go test -json ./... | testcensus [-strict] [-label NAME] [-trim PREFIX]
//
// Stdout carries the reconstructed non-verbose output; the census goes to
// stderr, so a caller that pipes stdout somewhere still sees it.
package main

import (
	"fmt"
	"os"
)

func main() {
	opts, err := parseArgs(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "testcensus:", err)
		fmt.Fprintln(os.Stderr, "usage: go test -json ... | testcensus [-strict] [-label NAME] [-trim PREFIX]")
		os.Exit(2)
	}

	code, err := run(os.Stdin, os.Stdout, os.Stderr, opts)
	if err != nil {
		fmt.Fprintln(os.Stderr, "testcensus:", err)
		os.Exit(2)
	}
	os.Exit(code)
}
