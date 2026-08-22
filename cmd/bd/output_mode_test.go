package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// newOutputModeTestTree builds a command tree with the same shape as bd's: a
// root that owns the persistent --json/--format pair, a plain child that
// inherits them, and a child that declares its own --json and so shadows the
// inherited one (compact, migrate, repo and restore all do this).
func newOutputModeTestTree() (root, plain, shadow *cobra.Command) {
	var rootJSON bool
	var shadowJSON bool

	root = &cobra.Command{Use: "bd", Run: func(*cobra.Command, []string) {}}
	root.PersistentFlags().BoolVar(&rootJSON, "json", false, "Output in JSON format")
	root.PersistentFlags().String("format", "", "Output format (json). Alias for --json")

	plain = &cobra.Command{Use: "plain", Run: func(*cobra.Command, []string) {}}
	shadow = &cobra.Command{Use: "shadow", Run: func(*cobra.Command, []string) {}}
	shadow.Flags().BoolVar(&shadowJSON, "json", false, "Output JSON")

	root.AddCommand(plain, shadow)
	return root, plain, shadow
}

// findAndParse resolves argv against root the way cobra's Execute does, then
// parses the remaining flags onto the command it found, and returns it. This
// is the state resolveJSONOutput sees when the root pre-run calls it.
func findAndParse(t *testing.T, root *cobra.Command, argv ...string) *cobra.Command {
	t.Helper()
	cmd, rest, err := root.Find(argv)
	if err != nil {
		t.Fatalf("Find(%v): %v", argv, err)
	}
	if err := cmd.ParseFlags(rest); err != nil {
		t.Fatalf("ParseFlags(%v): %v", rest, err)
	}
	return cmd
}

func TestResolveJSONOutput(t *testing.T) {
	tests := []struct {
		name       string
		argv       []string
		configJSON bool
		want       bool
	}{
		{name: "no flags, config off", argv: []string{"plain"}, want: false},
		{name: "no flags, config on", argv: []string{"plain"}, configJSON: true, want: true},
		{name: "--json", argv: []string{"plain", "--json"}, want: true},
		{name: "--json before subcommand", argv: []string{"--json", "plain"}, want: true},
		{
			// An explicit refusal has to beat `json: true` in config, or a
			// configured default becomes impossible to turn off per-command.
			name: "--json=false beats config on", argv: []string{"plain", "--json=false"}, configJSON: true, want: false,
		},
		{name: "--format json is the --json alias", argv: []string{"plain", "--format", "json"}, want: true},
		{name: "--format JSON is case-insensitive", argv: []string{"plain", "--format", "JSON"}, want: true},
		{
			// A non-json rendering was asked for by name; config must not
			// override it back to JSON.
			name: "--format other beats config on", argv: []string{"plain", "--format", "dot"}, configJSON: true, want: false,
		},
		{name: "--format other, config off", argv: []string{"plain", "--format", "dot"}, want: false},
		{
			// The pre-existing precedence: --format json wins outright. It is
			// the alias spelling, so it is read as the later, louder request.
			name: "--format json beats --json=false", argv: []string{"plain", "--json=false", "--format", "json"}, want: true,
		},

		// The regression this function exists for: a command that declares its
		// own --json never sees the root's persistent one, so resolving from
		// cmd.Root().PersistentFlags() reports "not set" and falls through to
		// config. `bd repo list --json` printed human output because of it.
		{name: "shadowing local --json", argv: []string{"shadow", "--json"}, want: true},
		{name: "shadowing local --json before subcommand", argv: []string{"--json", "shadow"}, want: true},
		{name: "shadowing command, no flag, config off", argv: []string{"shadow"}, want: false},
		{name: "shadowing command, no flag, config on", argv: []string{"shadow"}, configJSON: true, want: true},
		{name: "shadowing command, --format json", argv: []string{"shadow", "--format", "json"}, want: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// A fresh tree per case: pflag never clears Changed, so a shared
			// one would carry each case's flags into the next.
			root, _, _ := newOutputModeTestTree()
			cmd := findAndParse(t, root, tc.argv...)
			if got := resolveJSONOutput(cmd, tc.configJSON); got != tc.want {
				t.Errorf("resolveJSONOutput(%v, config=%v) = %v, want %v", tc.argv, tc.configJSON, got, tc.want)
			}
		})
	}
}

func TestResolveJSONOutputNilCommand(t *testing.T) {
	if resolveJSONOutput(nil, true) != true {
		t.Error("resolveJSONOutput(nil, true) = false, want true: with no command to read, config is all there is")
	}
	if resolveJSONOutput(nil, false) != false {
		t.Error("resolveJSONOutput(nil, false) = true, want false")
	}
}

// TestRealCommandsDeclareShadowingJSONFlags keeps the synthetic tree above
// honest. The shadowing shape it exercises is not hypothetical: several real
// commands declare their own --json, pflag keeps the first flag registered
// under a name, and so the root's persistent --json never reaches their flag
// set. That is why the resolver reads cmd.Flags() and not
// cmd.Root().PersistentFlags() — the latter reports "not set" for a --json the
// user typed, and the value falls through to config.
//
// Read-only on purpose: parsing onto the real tree would write cobra state
// that parallel tests share, and these flags are bound to the jsonOutput
// global itself.
func TestRealCommandsDeclareShadowingJSONFlags(t *testing.T) {
	rootJSON := rootCmd.PersistentFlags().Lookup("json")
	if rootJSON == nil {
		t.Fatal("root has no persistent --json flag")
	}

	var shadowing []string
	var walk func(cmd *cobra.Command)
	walk = func(cmd *cobra.Command) {
		if local := cmd.LocalNonPersistentFlags().Lookup("json"); local != nil && local != rootJSON {
			shadowing = append(shadowing, cmd.CommandPath())
		}
		for _, child := range cmd.Commands() {
			walk(child)
		}
	}
	walk(rootCmd)

	if len(shadowing) == 0 {
		t.Fatal("no command declares a local --json; the shadowing cases in TestResolveJSONOutput no longer describe this CLI, " +
			"so either they are dead weight or the walk stopped finding commands")
	}
	t.Logf("commands with a local --json that shadows the root's: %s", strings.Join(shadowing, ", "))
}

// TestJSONOutputHasOneProductionWriter is the audit the bd-vm6 sweep could not
// run. That sweep worked because every leak site was a visible `jsonOutput =
// true` in a _test.go file; a production command that sets the global from its
// own flag is invisible to it, and to any linter that only reads test sources.
//
// The invariant this pins: in production code jsonOutput is assigned only from
// resolveJSONOutput (which is called once per command, before Run) or by the
// CommandContext setter that mirrors it. No command may set it from inside its
// own Run, because nothing there puts it back and the next command in the same
// process inherits it.
func TestJSONOutputHasOneProductionWriter(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed; cannot locate the package source")
	}
	pkgDir := filepath.Dir(thisFile)

	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, pkgDir, func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", pkgDir, err)
	}
	if len(pkgs) == 0 {
		t.Fatalf("no non-test packages parsed from %s", pkgDir)
	}

	var offenders []string
	files := 0
	for _, pkg := range pkgs {
		for path, file := range pkg.Files {
			files++
			base := filepath.Base(path)
			ast.Inspect(file, func(n ast.Node) bool {
				assign, isAssign := n.(*ast.AssignStmt)
				if !isAssign {
					return true
				}
				for i, lhs := range assign.Lhs {
					ident, isIdent := lhs.(*ast.Ident)
					if !isIdent || ident.Name != "jsonOutput" {
						continue
					}
					if base == "context.go" {
						// setJSONOutput mirrors the resolved value onto the
						// CommandContext and the legacy global together.
						continue
					}
					if i < len(assign.Rhs) && isCallTo(assign.Rhs[i], "resolveJSONOutput") {
						continue
					}
					offenders = append(offenders, fset.Position(assign.Pos()).String())
				}
				return true
			})
		}
	}

	// A zero here must mean "scanned and found nothing", not "scanned nothing".
	if files < 100 {
		t.Fatalf("parsed only %d non-test files from %s; the scan is not covering the package", files, pkgDir)
	}
	if len(offenders) > 0 {
		t.Errorf("jsonOutput assigned outside resolveJSONOutput at:\n  %s\n\n"+
			"Resolve the output mode once per command instead: read the flag and let the root\n"+
			"pre-run's resolveJSONOutput call install it. Setting the global from inside a Run\n"+
			"leaves it set for whatever runs next in the same process.",
			strings.Join(offenders, "\n  "))
	}
}

func isCallTo(expr ast.Expr, name string) bool {
	call, ok := expr.(*ast.CallExpr)
	if !ok {
		return false
	}
	ident, ok := call.Fun.(*ast.Ident)
	return ok && ident.Name == name
}
