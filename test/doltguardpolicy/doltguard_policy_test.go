// Package doltguardpolicy enforces, at the source level, that the
// production-Dolt guard is actually reachable from the test binaries that need
// it.
//
// bd-4xn is what this exists to prevent, twice over. The first time, a guard
// was set on a variable nothing read, and everyone who checked found the
// variable they remembered setting. The second time, the absence of a guard in
// this repo was measured as "61 test files reference the port variables", and
// referencing a variable was read as guarding against it. Both failures are
// invisible to a passing suite: an inert guard and a present one produce
// identical green.
//
// So the property is checked structurally rather than behaviourally. Two rules:
//
//	Rule 1  Every TestMain in the tree calls testenv.GuardProductionDolt() as
//	        the first statement of its body. A guard that runs after a helper
//	        has already resolved a port is a guard that did not run.
//
//	Rule 2  Every test package that can reach Dolt has a TestMain in the
//	        default build. "Can reach Dolt" is three functional signals, not a
//	        label: the package's test files name a Dolt port environment
//	        variable, import the Dolt store, or import the container helpers.
//
// Rule 2 is evaluated against the DEFAULT BUILD, using go/build's own
// constraint evaluation, because a TestMain behind `//go:build integration` is
// not a TestMain for the run anybody actually does. internal/doltserver had
// exactly that shape: a guarded, careful TestMain that the default `go test`
// never compiled.
package doltguardpolicy

import (
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// guardCall is the function every TestMain must open with. Matched on the
// selector alone so an import alias cannot hide a match — and cannot fake one
// either, since no other function in the tree has this name.
const guardCall = "GuardProductionDolt"

// doltReachSignals are the functional markers for "this package's tests can
// resolve a Dolt endpoint". They are deliberately about behaviour the package
// exhibits rather than anything it declares about itself.
var doltReachSignals = []string{
	"BEADS_DOLT_SERVER_PORT",
	"BEADS_DOLT_PORT",
	`"github.com/steveyegge/beads/internal/storage/dolt"`,
	`"github.com/steveyegge/beads/internal/testutil"`,
}

// exemptDirs are repo-relative directories where a guarded TestMain would be
// wrong rather than merely unnecessary.
var exemptDirs = map[string]string{
	// The guard's own tests drive the port variables through every shape a
	// process can present, including unset and production. A TestMain that
	// pinned them first would leave the tests asserting the guard's own
	// output instead of its behaviour.
	filepath.Join("internal", "testenv"): "the guard's own tests must control the port variables directly",
	// This package names the variables as data.
	filepath.Join("test", "doltguardpolicy"): "names the guarded variables as policy data, never resolves one",
}

func repoRoot(t *testing.T) string {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed")
	}
	return filepath.Join(filepath.Dir(filename), "..", "..")
}

// defaultBuildContext is go/build's default context with cgo forced on, which
// is what .buildflags pins for every run in this repo. Without it a machine
// with CGO_ENABLED=0 would report the cgo-tagged TestMains as missing.
func defaultBuildContext() build.Context {
	ctxt := build.Default
	ctxt.CgoEnabled = true
	return ctxt
}

// isTestMain reports whether decl is `func TestMain(m *testing.M)`.
func isTestMain(decl *ast.FuncDecl) bool {
	if decl.Name.Name != "TestMain" || decl.Recv != nil || decl.Body == nil {
		return false
	}
	params := decl.Type.Params
	if params == nil || len(params.List) != 1 {
		return false
	}
	star, ok := params.List[0].Type.(*ast.StarExpr)
	if !ok {
		return false
	}
	sel, ok := star.X.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkgIdent, ok := sel.X.(*ast.Ident)
	return ok && pkgIdent.Name == "testing" && sel.Sel.Name == "M"
}

// opensWithGuard reports whether body's first statement is a bare call to
// something.GuardProductionDolt().
func opensWithGuard(body *ast.BlockStmt) bool {
	if len(body.List) == 0 {
		return false
	}
	stmt, ok := body.List[0].(*ast.ExprStmt)
	if !ok {
		return false
	}
	call, ok := stmt.X.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	return ok && sel.Sel.Name == guardCall
}

// testFile is one *_test.go file with the facts both rules need.
type testFile struct {
	rel          string
	inDefault    bool
	testMains    []*ast.FuncDecl
	reachesDolt  bool
	fset         *token.FileSet
	unguardedPos []token.Position
}

// collect walks the repo and returns every *_test.go file, keyed by the
// repo-relative directory of the package it belongs to.
//
// The walk is explicit rather than a recursive grep: recursive search in this
// workspace honours .gitignore, and reports a confident zero for trees it
// never entered.
func collect(t *testing.T, root string) (map[string][]testFile, int) {
	t.Helper()
	ctxt := defaultBuildContext()
	byDir := map[string][]testFile{}
	scanned := 0

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "vendor", "testdata":
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		scanned++
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}

		src, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		fset := token.NewFileSet()
		file, parseErr := parser.ParseFile(fset, path, src, parser.ParseComments)
		if parseErr != nil {
			return nil // not our business to fail on unparseable fixtures
		}

		inDefault, matchErr := ctxt.MatchFile(filepath.Dir(path), filepath.Base(path))
		if matchErr != nil {
			inDefault = false
		}

		tf := testFile{rel: rel, inDefault: inDefault, fset: fset}
		for _, sig := range doltReachSignals {
			if strings.Contains(string(src), sig) {
				tf.reachesDolt = true
				break
			}
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || !isTestMain(fn) {
				continue
			}
			tf.testMains = append(tf.testMains, fn)
			if !opensWithGuard(fn.Body) {
				tf.unguardedPos = append(tf.unguardedPos, fset.Position(fn.Pos()))
			}
		}
		dir := filepath.Dir(rel)
		byDir[dir] = append(byDir[dir], tf)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	if scanned == 0 {
		t.Fatalf("scanned 0 _test.go files under %s: the walk found nothing, which is not the same as finding no violations", root)
	}
	return byDir, scanned
}

// TestEveryTestMainOpensWithTheDoltGuard is Rule 1.
func TestEveryTestMainOpensWithTheDoltGuard(t *testing.T) {
	root := repoRoot(t)
	byDir, scanned := collect(t, root)

	var violations []string
	testMains := 0
	for dir, files := range byDir {
		if _, exempt := exemptDirs[dir]; exempt {
			continue
		}
		for _, f := range files {
			testMains += len(f.testMains)
			for _, pos := range f.unguardedPos {
				violations = append(violations, f.rel+":"+strconv.Itoa(pos.Line))
			}
		}
	}
	sort.Strings(violations)

	if testMains == 0 {
		t.Fatalf("found 0 TestMain functions across %d _test.go files: the check cannot have seen anything", scanned)
	}
	if len(violations) > 0 {
		t.Errorf("TestMain must open with testenv.%s(); %d of %d do not:\n  %s\n\n"+
			"A guard that runs after a helper has already resolved a port is a guard that did not run.",
			guardCall, len(violations), testMains, strings.Join(violations, "\n  "))
	}
	t.Logf("checked %d TestMain functions across %d _test.go files in %d packages", testMains, scanned, len(byDir))
}

// TestPackagesThatCanReachDoltHaveAGuardedTestMain is Rule 2.
func TestPackagesThatCanReachDoltHaveAGuardedTestMain(t *testing.T) {
	root := repoRoot(t)
	byDir, scanned := collect(t, root)

	var missing []string
	needing := 0
	for dir, files := range byDir {
		if _, exempt := exemptDirs[dir]; exempt {
			continue
		}
		needs, hasDefaultTestMain := false, false
		for _, f := range files {
			if !f.inDefault {
				continue
			}
			if f.reachesDolt {
				needs = true
			}
			if len(f.testMains) > 0 {
				hasDefaultTestMain = true
			}
		}
		if !needs {
			continue
		}
		needing++
		if !hasDefaultTestMain {
			missing = append(missing, dir)
		}
	}
	sort.Strings(missing)

	if needing == 0 {
		t.Fatalf("found 0 packages that can reach Dolt across %d _test.go files: the signals cannot be matching anything", scanned)
	}
	if len(missing) > 0 {
		t.Errorf("%d of %d packages whose tests can reach Dolt have no TestMain in the default build:\n  %s\n\n"+
			"Add one that opens with testenv.%s(). A TestMain behind a build tag does not count: "+
			"the run that creates databases on the live server is the untagged one.",
			len(missing), needing, strings.Join(missing, "\n  "), guardCall)
	}
	t.Logf("checked %d Dolt-reaching packages across %d _test.go files", needing, scanned)
}
