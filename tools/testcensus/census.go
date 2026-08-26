package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
)

// options is the command line, already parsed.
type options struct {
	// strict turns a vacuous "ok" — a package that reported ok having run no
	// tests at all — into a nonzero exit rather than a paragraph on stderr.
	strict bool
	// label names the run in the census header, so a wrapper that censuses
	// two passes ("main suite", "Dolt coverage tier") produces two blocks a
	// reader can tell apart.
	label string
	// trim is stripped from the front of package names in the report. The
	// module path, normally: 126 lines of
	// github.com/steveyegge/beads/... hide the part that differs.
	trim string
	// verbose says the caller passed -v to go test and wants every line.
	// Without this the filter would reconstruct the NON-verbose view over a
	// run the user explicitly asked to be verbose — quietly deleting the
	// output they invoked the flag for.
	verbose bool
}

func parseArgs(argv []string) (options, error) {
	var opts options
	for i := 0; i < len(argv); i++ {
		switch arg := argv[i]; arg {
		case "-strict", "--strict":
			opts.strict = true
		case "-v", "-verbose", "--verbose":
			opts.verbose = true
		case "-label", "--label":
			if i+1 >= len(argv) {
				return opts, fmt.Errorf("%s needs a value", arg)
			}
			i++
			opts.label = argv[i]
		case "-trim", "--trim":
			if i+1 >= len(argv) {
				return opts, fmt.Errorf("%s needs a value", arg)
			}
			i++
			opts.trim = argv[i]
		default:
			return opts, fmt.Errorf("unknown argument %q", arg)
		}
	}
	return opts, nil
}

// Exit codes. Kept distinct so a caller can tell "the tests failed" from "the
// tests did not run and said ok anyway" — the second is this tool's whole
// reason for existing and would otherwise be invisible behind a generic 1.
const (
	exitOK       = 0
	exitTestsRed = 1
	exitVacuous  = 3
)

// event is the subset of cmd/internal/test2json's schema this reads. Field
// names match go's exactly; unknown fields (Time, Elapsed, FailedBuild) are
// ignored by encoding/json.
type event struct {
	Action     string `json:"Action"`
	Package    string `json:"Package"`
	ImportPath string `json:"ImportPath"`
	Test       string `json:"Test"`
	Output     string `json:"Output"`
}

// line is one buffered output line together with the test it belongs to
// (empty for package-scope output such as "ok  \tpkg\t1.2s").
type line struct {
	test string
	text string
}

type pkgState struct {
	name  string
	lines []line
	// result maps a test name to its terminal action (pass, fail, skip). A
	// test with no entry never finished — a panic or a package deadline —
	// and its output must survive, which is why absence is meaningful here.
	result map[string]string
	// skipReason maps a skipped test to the t.Skip message, recovered from
	// the last log line the test emitted before its --- SKIP marker.
	skipReason map[string]string
	// order is top-level test names in first-seen order, for stable reports.
	order []string
	seen  map[string]bool
}

func newPkgState(name string) *pkgState {
	return &pkgState{
		name:       name,
		result:     map[string]string{},
		skipReason: map[string]string{},
		seen:       map[string]bool{},
	}
}

// tally is what the census reports about one package.
type tally struct {
	pkg     string
	ran     int // top-level tests that passed or failed
	skipped int // top-level tests that skipped
	// vacuous: the package reported ok and not one of its tests ran.
	vacuous bool
}

type census struct {
	tallies []tally
	// gates counts skipped top-level tests by the environment variable their
	// skip message names, e.g. BEADS_TEST_EMBEDDED_DOLT -> 271.
	gates map[string]int
	// gateExample keeps one skip message per gate, so an unattributed group
	// can still be identified by a reader.
	gateExample map[string]string
	ran         int
	skipped     int
	failed      bool
}

// framing matches the per-test scaffolding lines that only exist because
// -json forces the test binary into verbose mode. Plain `go test` never
// prints them and neither does this.
var framing = regexp.MustCompile(`^\s*=== (RUN|PAUSE|CONT|NAME)\b`)

// resultMarker matches the per-test result lines, likewise verbose-only.
var resultMarker = regexp.MustCompile(`^\s*--- (PASS|SKIP|FAIL|BENCH):`)

// logPrefix matches the "    file_test.go:42: " that testing prepends to a
// t.Log/t.Skip message. Stripping it leaves the message the author wrote.
var logPrefix = regexp.MustCompile(`^\s*[^\s:]+\.go:\d+: `)

// goResultLine matches the go COMMAND's own per-package verdict — the only
// package-scope line it prints for a package that passed.
var goResultLine = regexp.MustCompile(`^(ok|\?)\s`)

// envGate matches the environment variable a skip message names. Nearly every
// opt-in gate in this repo says "set BEADS_TEST_X=1 to run ..."; grouping by
// the variable turns 283 individual skips into the three or four decisions
// that actually produced them.
var envGate = regexp.MustCompile(`\bBEADS_[A-Z0-9_]+\b`)

// run consumes a `go test -json` stream, reproduces the non-verbose output on
// out, and writes the census to errOut.
func run(in io.Reader, out, errOut io.Writer, opts options) (int, error) {
	scanner := bufio.NewScanner(in)
	// Test output lines can be long (a diff dump, a panic trace). The
	// default 64KiB limit would abort the scan mid-run and lose the rest of
	// the suite, which is a worse failure than any it exists to report.
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)

	// Packages are buffered until they report a result and flushed in
	// completion order, which is what plain `go test` does: under -p 4 the
	// event stream interleaves, and printing as it arrives would shuffle four
	// packages' output together.
	pkgs := map[string]*pkgState{}
	c := &census{gates: map[string]int{}, gateExample: map[string]string{}}

	for scanner.Scan() {
		raw := scanner.Text()
		if raw == "" {
			continue
		}

		var ev event
		if err := json.Unmarshal([]byte(raw), &ev); err != nil {
			// Not a test2json line. Something wrote to the same stream;
			// pass it through rather than swallowing it. A dropped line
			// here is exactly the class of silence this tool is about.
			fmt.Fprintln(out, raw)
			continue
		}

		// Build failures arrive before any package events, keyed by
		// ImportPath rather than Package. They are terminal and there is
		// nothing to buffer them against, so they go straight out.
		if ev.Package == "" {
			switch ev.Action {
			case "build-output":
				fmt.Fprint(out, ev.Output)
			case "build-fail":
				c.failed = true
			}
			continue
		}

		p, ok := pkgs[ev.Package]
		if !ok {
			p = newPkgState(ev.Package)
			pkgs[ev.Package] = p
		}

		switch ev.Action {
		case "output":
			p.lines = append(p.lines, line{test: ev.Test, text: ev.Output})
			if ev.Test != "" && !resultMarker.MatchString(ev.Output) && !framing.MatchString(ev.Output) {
				// Candidate skip reason: the last thing the test logged
				// before its marker. Overwritten until the marker lands.
				p.skipReason[ev.Test] = strings.TrimRight(logPrefix.ReplaceAllString(ev.Output, ""), "\n")
			}
		case "run":
			p.note(ev.Test)
		case "pass", "fail", "skip":
			if ev.Test != "" {
				p.note(ev.Test)
				p.result[ev.Test] = ev.Action
				continue
			}
			// Package-scope terminal event: flush and tally.
			flush(out, p, ev.Action, opts.verbose)
			c.absorb(p, ev.Action)
			delete(pkgs, ev.Package)
		}
	}
	if err := scanner.Err(); err != nil {
		return exitTestsRed, fmt.Errorf("reading go test -json: %w", err)
	}

	// Anything still buffered belongs to a package that never reported a
	// result — go test was killed, or a deadline fired. Treat it as not
	// passing and print everything: this is the case where suppressing
	// output would hide the only evidence of what happened.
	var stranded []string
	for name := range pkgs {
		stranded = append(stranded, name)
	}
	sort.Strings(stranded)
	for _, name := range stranded {
		flush(out, pkgs[name], "", opts.verbose)
		c.absorb(pkgs[name], "")
		c.failed = true
	}

	report(errOut, c, opts)

	switch {
	case c.failed:
		return exitTestsRed, nil
	case opts.strict && c.vacuousCount() > 0:
		return exitVacuous, nil
	}
	return exitOK, nil
}

// note records a top-level test name the first time it is seen. Subtests
// (names containing "/") are deliberately not counted: the unit a reader
// reasons about is the test function, and counting both tiers in one number
// makes it impossible to compare against the 283 t.Skip statements in the
// tree.
func (p *pkgState) note(test string) {
	if test == "" || strings.Contains(test, "/") || p.seen[test] {
		return
	}
	p.seen[test] = true
	p.order = append(p.order, test)
}

// flush writes the package's buffered output in the shape plain `go test`
// would have produced, given the package's terminal action ("" when it never
// reported one). Under verbose the stream is reproduced untouched: -json puts
// the test binary in verbose mode, which is exactly what -v asked for.
//
// ONE KNOWN DIVERGENCE, pinned by TestFailingPackageDropsAPassingTestsStdout.
// In a FAILING package, a passing test that wrote straight to os.Stdout has
// that write dropped here; `go test` shows it. The two are indistinguishable
// in the event stream — testing's buffered t.Log output and a raw fmt.Println
// both arrive as output events attributed to the test — and keeping them would
// mean dumping every passing test's t.Log on any single failure. Nothing else
// differs, under -v or without it.
func flush(out io.Writer, p *pkgState, pkgAction string, verbose bool) {
	if verbose {
		for _, l := range p.lines {
			fmt.Fprint(out, l.text)
		}
		return
	}

	passed := pkgAction == "pass"

	// Lines regrouped by test, because a failing test's block has to be
	// REORDERED: -json runs the binary verbose, where testing prints a
	// failure's log lines and then its "--- FAIL:" header; without -v it
	// prints the header first and flushes the log under it.
	byTest := map[string][]string{}
	for _, l := range p.lines {
		if l.test != "" {
			byTest[l.test] = append(byTest[l.test], l.text)
		}
	}
	emitted := map[string]bool{}

	for _, l := range p.lines {
		if l.test == "" {
			// For a package that PASSED, the only thing `go test` prints is
			// its own result line. Everything else arriving at package scope
			// is the test binary's output, which it discards — the bare
			// "PASS", a TestMain's prints, and the standalone
			// "coverage: 84.1% of statements" line whose number it folds
			// into the result line instead. A failing package keeps all of
			// it, "FAIL" included.
			if passed && !goResultLine.MatchString(l.text) {
				continue
			}
			fmt.Fprint(out, l.text)
			continue
		}
		if passed {
			// A passing package prints none of its tests' output. This is
			// go's behavior, not a choice: verified on go1.26.6, a passing
			// package's stdout AND stderr are both discarded wholesale.
			continue
		}
		if emitted[l.test] {
			continue
		}

		switch p.result[l.test] {
		case "pass", "skip":
			// The package failed elsewhere; this test is not the story.
			emitted[l.test] = true
		case "fail":
			emitted[l.test] = true
			// test2json DEDENTS a subtest's lines — it strips the frame
			// indentation as part of attributing them — so the nesting has
			// to be put back, four spaces per level, or a failing subtest
			// reads as a sibling of its parent.
			indent := strings.Repeat("    ", strings.Count(l.test, "/"))
			var header string
			var body []string
			for _, text := range byTest[l.test] {
				switch {
				case framing.MatchString(text):
				case header == "" && resultMarker.MatchString(text):
					header = text
				default:
					body = append(body, text)
				}
			}
			if header != "" {
				fmt.Fprint(out, reindent(header, indent))
			}
			for _, text := range body {
				fmt.Fprint(out, reindent(text, indent))
			}
		default:
			// No terminal action: the test was still running when the
			// process died. Keep everything, framing and order included —
			// the "=== RUN" line may be the only thing naming the culprit,
			// and there is no header to hoist.
			emitted[l.test] = true
			for _, text := range byTest[l.test] {
				fmt.Fprint(out, text)
			}
		}
	}
}

// reindent prefixes every non-empty line of text with indent. An empty line
// is left alone: padding it would add trailing whitespace `go test` does not.
func reindent(text, indent string) string {
	if indent == "" {
		return text
	}
	lines := strings.Split(text, "\n")
	for i, l := range lines {
		if l != "" {
			lines[i] = indent + l
		}
	}
	return strings.Join(lines, "\n")
}

func (c *census) absorb(p *pkgState, pkgAction string) {
	t := tally{pkg: p.name}
	for _, name := range p.order {
		switch p.result[name] {
		case "skip":
			t.skipped++
			gate := "(no environment gate named)"
			reason := p.skipReason[name]
			if m := envGate.FindString(reason); m != "" {
				gate = m
			}
			c.gates[gate]++
			if _, ok := c.gateExample[gate]; !ok && reason != "" {
				c.gateExample[gate] = reason
			}
		case "pass", "fail":
			t.ran++
		default:
			// Unfinished; counts as neither. The package cannot be vacuous
			// in that case, which the check below relies on.
		}
	}
	if pkgAction == "fail" || pkgAction == "" {
		c.failed = true
	}
	// A package with no tests at all ("[no test files]") is not vacuous —
	// there was never anything to run and nothing is being hidden.
	t.vacuous = pkgAction == "pass" && t.ran == 0 && t.skipped > 0
	c.ran += t.ran
	c.skipped += t.skipped
	if t.ran > 0 || t.skipped > 0 {
		c.tallies = append(c.tallies, t)
	}
}

func (c *census) vacuousCount() int {
	n := 0
	for _, t := range c.tallies {
		if t.vacuous {
			n++
		}
	}
	return n
}

const rule = "=============================================================="

func report(w io.Writer, c *census, opts options) {
	label := opts.label
	if label != "" {
		label = " — " + label
	}

	total := c.ran + c.skipped
	if c.skipped == 0 {
		fmt.Fprintf(w, "\n==> Skip census%s: %d top-level tests, all of them ran. (bd-5er)\n", label, total)
		return
	}

	vacuous := c.vacuousCount()

	fmt.Fprintf(w, "\n%s\n", rule)
	fmt.Fprintf(w, "  SKIP CENSUS%s — what this run's \"ok\" is not evidence for (bd-5er)\n", label)
	fmt.Fprintf(w, "\n  %d top-level tests: %d ran, %d SKIPPED\n", total, c.ran, c.skipped)

	fmt.Fprintf(w, "\n  Skipped behind:\n")
	for _, g := range sortedGates(c.gates) {
		fmt.Fprintf(w, "    %6d  %s\n", c.gates[g], g)
		if strings.HasPrefix(g, "(") {
			if ex := c.gateExample[g]; ex != "" {
				fmt.Fprintf(w, "            e.g. %s\n", truncate(ex, 100))
			}
		}
	}

	if vacuous > 0 {
		noun := "package"
		if vacuous != 1 {
			noun = "packages"
		}
		fmt.Fprintf(w, "\n  %d %s reported \"ok\" having run NO tests at all:\n", vacuous, noun)
		width := 0
		for _, t := range c.tallies {
			if t.vacuous {
				if n := len(display(t.pkg, opts.trim)); n > width {
					width = n
				}
			}
		}
		for _, t := range sortedTallies(c.tallies) {
			if !t.vacuous {
				continue
			}
			fmt.Fprintf(w, "    %-*s   0 ran / %d skipped\n", width, display(t.pkg, opts.trim), t.skipped)
		}
	}

	fmt.Fprintf(w, "\n  `go test` discards a passing package's own output, so nothing above\n")
	fmt.Fprintf(w, "  this line could have told you any of it. Turn a gate on by exporting\n")
	fmt.Fprintf(w, "  the variable it names, e.g.\n")
	fmt.Fprintf(w, "      BEADS_TEST_EMBEDDED_DOLT=1 ./scripts/test.sh ./internal/storage/embeddeddolt/\n")
	if opts.strict && vacuous > 0 {
		fmt.Fprintf(w, "\n  STRICT: a package that reports ok having run nothing fails this run.\n")
	}
	fmt.Fprintf(w, "%s\n", rule)
}

func sortedGates(gates map[string]int) []string {
	out := make([]string, 0, len(gates))
	for g := range gates {
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool {
		if gates[out[i]] != gates[out[j]] {
			return gates[out[i]] > gates[out[j]]
		}
		return out[i] < out[j]
	})
	return out
}

func sortedTallies(in []tally) []tally {
	out := make([]tally, len(in))
	copy(out, in)
	sort.Slice(out, func(i, j int) bool {
		if out[i].skipped != out[j].skipped {
			return out[i].skipped > out[j].skipped
		}
		return out[i].pkg < out[j].pkg
	})
	return out
}

func display(pkg, trim string) string {
	if trim == "" {
		return pkg
	}
	trimmed := strings.TrimPrefix(pkg, trim)
	trimmed = strings.TrimPrefix(trimmed, "/")
	if trimmed == "" {
		// The package IS the module root; naming it "" helps nobody.
		return pkg
	}
	return trimmed
}

// truncate shortens s to at most n runes, counting runes rather than bytes so
// a multi-byte character in a skip message cannot be cut in half.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
