package testenv_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/steveyegge/beads/internal/configfile"
	"github.com/steveyegge/beads/internal/storage/dolt"
	"github.com/steveyegge/beads/internal/testenv"
)

// setPortEnv applies one grid row to the environment. A nil pointer means the
// variable is unset for this case, which is a distinct input from the empty
// string as far as os.LookupEnv is concerned even though the guard treats them
// alike.
func setPortEnv(t *testing.T, name string, value *string) {
	t.Helper()
	if value == nil {
		// t.Setenv registers the restore; unsetting afterwards keeps that
		// restore while making the variable absent for the test body.
		t.Setenv(name, "placeholder")
		if err := os.Unsetenv(name); err != nil {
			t.Fatalf("unsetenv %s: %v", name, err)
		}
		return
	}
	t.Setenv(name, *value)
}

func ptr(s string) *string { return &s }

// TestGuardProductionDoltNeverResolvesTowardProduction is the property this
// guard exists for, checked over every value shape a port variable can hold:
// after the guard runs, no variable names the production port, and no variable
// holds a value the resolvers would skip on their way to the production
// default.
//
// It is a grid rather than a list of cases because the failure this bead
// records was a guard that was correct for the input its author had in mind
// and inert for the one the process actually had.
func TestGuardProductionDoltNeverResolvesTowardProduction(t *testing.T) {
	inputs := []*string{
		nil,             // unset: the shape that reaches the 3307 default
		ptr(""),         // set but empty: same fallback
		ptr("   "),      // whitespace: Atoi fails, same fallback
		ptr("notaport"), // unparseable: same fallback
		ptr("0"),        // rejected by every `p > 0` check: same fallback
		ptr("-5"),       // likewise
		ptr("3307"),     // production, named outright
		ptr("  3307  "), // production behind whitespace the resolvers trim
		ptr("43211"),    // a deliberate test server: must survive
		ptr("63307"),    // another deliberate port: must survive
		ptr(strconv.Itoa(testenv.GuardedDoltPort)), // already guarded
	}

	vars := testenv.DoltPortEnvVars()
	for _, name := range vars {
		for _, in := range inputs {
			label := "unset"
			if in != nil {
				label = strconv.Quote(*in)
			}
			t.Run(name+"/"+label, func(t *testing.T) {
				// Every variable starts unset so this case's only signal is
				// the one under test.
				for _, other := range vars {
					setPortEnv(t, other, nil)
				}
				setPortEnv(t, name, in)
				t.Setenv(testenv.AllowProductionDoltEnv, "")
				if err := os.Unsetenv(testenv.AllowProductionDoltEnv); err != nil {
					t.Fatalf("unsetenv opt-in: %v", err)
				}

				testenv.GuardProductionDolt()

				for _, v := range vars {
					got, ok := os.LookupEnv(v)
					if !ok {
						t.Errorf("%s left unset after guard: an unset port variable is exactly how resolution reaches the %d default", v, testenv.ProductionDoltPort)
						continue
					}
					port, err := strconv.Atoi(strings.TrimSpace(got))
					if err != nil {
						t.Errorf("%s = %q after guard: unparseable values are skipped by every resolver, which lands on the %d default", v, got, testenv.ProductionDoltPort)
						continue
					}
					if port <= 0 {
						t.Errorf("%s = %q after guard: non-positive ports fail every `p > 0` check, which lands on the %d default", v, got, testenv.ProductionDoltPort)
					}
					if port == testenv.ProductionDoltPort {
						t.Errorf("%s = %q after guard: names the production port", v, got)
					}
				}
			})
		}
	}
}

// TestGuardProductionDoltPreservesDeliberatePorts is the discriminating half
// of the acceptance criterion: a guard that pointed everything at the dead
// port would pass the property above and be useless, because a test that
// brought its own server could no longer reach it.
func TestGuardProductionDoltPreservesDeliberatePorts(t *testing.T) {
	const container = "43211"
	for _, name := range testenv.DoltPortEnvVars() {
		t.Run(name, func(t *testing.T) {
			for _, other := range testenv.DoltPortEnvVars() {
				setPortEnv(t, other, nil)
			}
			t.Setenv(name, container)

			testenv.GuardProductionDolt()

			if got := os.Getenv(name); got != container {
				t.Errorf("%s = %q, want %q: a deliberate non-production port must survive the guard", name, got, container)
			}
		})
	}
}

// TestGuardProductionDoltGuardsEveryVariableIndependently covers the shape
// this bead's original report was about: two variables with a precedence
// between them, one poisoned and one pointed at production. Whichever wins,
// the winner must not be production — so the guard has to act per variable
// rather than stopping once it finds one that looks safe.
func TestGuardProductionDoltGuardsEveryVariableIndependently(t *testing.T) {
	guarded := strconv.Itoa(testenv.GuardedDoltPort)
	prod := strconv.Itoa(testenv.ProductionDoltPort)

	cases := []struct {
		name   string
		server string
		legacy string
		want   map[string]string
	}{
		{
			// The exact configuration this bead was filed over: the poison on
			// the losing variable, production on the winning one.
			name:   "poison_on_loser_production_on_winner",
			server: prod,
			legacy: guarded,
			want:   map[string]string{"BEADS_DOLT_SERVER_PORT": guarded, "BEADS_DOLT_PORT": guarded},
		},
		{
			// The mirror image. The container port wins and must survive; the
			// production value on the loser is still replaced, because a
			// subprocess reading only the legacy variable would otherwise find
			// the live server.
			name:   "container_on_winner_production_on_loser",
			server: "43211",
			legacy: prod,
			want:   map[string]string{"BEADS_DOLT_SERVER_PORT": "43211", "BEADS_DOLT_PORT": guarded},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setPortEnv(t, "GT_DOLT_PORT", nil)
			t.Setenv("BEADS_DOLT_SERVER_PORT", tc.server)
			t.Setenv("BEADS_DOLT_PORT", tc.legacy)

			testenv.GuardProductionDolt()

			for name, want := range tc.want {
				if got := os.Getenv(name); got != want {
					t.Errorf("%s = %q, want %q", name, got, want)
				}
			}
		})
	}
}

// TestProductionDoltAllowedRequiresTheBoundaryByName holds the rule that
// separates an operator who knows they are aimed at the live server from a
// process that inherited a flag.
func TestProductionDoltAllowedRequiresTheBoundaryByName(t *testing.T) {
	cases := []struct {
		value *string
		want  bool
	}{
		{nil, false},
		{ptr(""), false},
		{ptr("1"), false},    // the bare boolean that authorizes nothing
		{ptr("true"), false}, // likewise
		{ptr("yes"), false},
		{ptr("3306"), false}, // names a boundary, but not this one
		{ptr(strconv.Itoa(testenv.ProductionDoltPort)), true},
		{ptr(" " + strconv.Itoa(testenv.ProductionDoltPort) + " "), true}, // shells add whitespace
	}
	for _, tc := range cases {
		label := "unset"
		if tc.value != nil {
			label = strconv.Quote(*tc.value)
		}
		t.Run(label, func(t *testing.T) {
			setPortEnv(t, testenv.AllowProductionDoltEnv, tc.value)
			if got := testenv.ProductionDoltAllowed(); got != tc.want {
				t.Errorf("ProductionDoltAllowed() = %v, want %v for %s=%s", got, tc.want, testenv.AllowProductionDoltEnv, label)
			}
		})
	}
}

// TestGuardProductionDoltHonorsOptIn checks that the opt-in leaves the
// environment exactly as the operator arranged it — including leaving the
// variables unset, which is what a smoke check against the live server needs.
func TestGuardProductionDoltHonorsOptIn(t *testing.T) {
	for _, name := range testenv.DoltPortEnvVars() {
		setPortEnv(t, name, nil)
	}
	t.Setenv(testenv.AllowProductionDoltEnv, strconv.Itoa(testenv.ProductionDoltPort))

	testenv.GuardProductionDolt()

	for _, name := range testenv.DoltPortEnvVars() {
		if got, ok := os.LookupEnv(name); ok {
			t.Errorf("%s = %q after guard: an explicit opt-in must not rewrite the environment", name, got)
		}
	}
}

// TestWithoutDoltPortGuardRestoresBothShapes covers the restore path for a
// variable that was set and for one that was not. Getting the second wrong
// leaves the variable set to the empty string, which reads as "configured" to
// os.LookupEnv and as "fall through to the default" to the resolvers — a
// silently unguarded process.
func TestWithoutDoltPortGuardRestoresBothShapes(t *testing.T) {
	const set = "43211"
	names := testenv.DoltPortEnvVars()
	if len(names) < 2 {
		t.Fatalf("expected at least two guarded variables, got %v", names)
	}
	setName, unsetName := names[0], names[1]

	t.Setenv(setName, set)
	setPortEnv(t, unsetName, nil)

	t.Run("cleared", func(t *testing.T) {
		testenv.WithoutDoltPortGuard(t)
		for _, name := range names {
			if got, ok := os.LookupEnv(name); ok {
				t.Errorf("%s = %q inside WithoutDoltPortGuard, want unset", name, got)
			}
		}
	})

	if got, ok := os.LookupEnv(setName); !ok || got != set {
		t.Errorf("%s = %q (present=%v) after cleanup, want %q", setName, got, ok, set)
	}
	if got, ok := os.LookupEnv(unsetName); ok {
		t.Errorf("%s = %q after cleanup, want it to stay unset", unsetName, got)
	}
}

// TestDoltPortEnvVarsReturnsACopy keeps a caller from editing the guard's own
// list through the slice it hands out.
func TestDoltPortEnvVarsReturnsACopy(t *testing.T) {
	first := testenv.DoltPortEnvVars()
	if len(first) == 0 {
		t.Fatal("DoltPortEnvVars() is empty")
	}
	first[0] = "MUTATED"
	if second := testenv.DoltPortEnvVars(); second[0] == "MUTATED" {
		t.Error("DoltPortEnvVars() shares its backing array with the package-level list")
	}
}

// TestProductionDoltPortMatchesProductionConstants pins the duplicated-by-
// value constant to the two production constants it mirrors. The package
// comment explains why it is a copy; this is what keeps the copy honest.
func TestProductionDoltPortMatchesProductionConstants(t *testing.T) {
	if testenv.ProductionDoltPort != configfile.DefaultDoltServerPort {
		t.Errorf("ProductionDoltPort = %d, configfile.DefaultDoltServerPort = %d: the guard is aimed at a port that is no longer the default",
			testenv.ProductionDoltPort, configfile.DefaultDoltServerPort)
	}
	if testenv.ProductionDoltPort != dolt.DefaultSQLPort {
		t.Errorf("ProductionDoltPort = %d, dolt.DefaultSQLPort = %d: the guard and the store's production-port firewall disagree about which port is production",
			testenv.ProductionDoltPort, dolt.DefaultSQLPort)
	}
}

// TestGuardedDoltPortIsNotProduction is the one-line invariant that makes
// every other test in this file meaningful.
func TestGuardedDoltPortIsNotProduction(t *testing.T) {
	if testenv.GuardedDoltPort == testenv.ProductionDoltPort {
		t.Fatalf("GuardedDoltPort == ProductionDoltPort == %d: the guard points at the server it is guarding against", testenv.GuardedDoltPort)
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed")
	}
	return filepath.Join(filepath.Dir(filename), "..", "..")
}

// TestNoBeadsCodeReadsGTDoltPort backs the claim in doltPortEnvVars' comment
// that GT_DOLT_PORT is guarded for subprocesses only. If beads production code
// ever starts reading it, that comment becomes wrong and the variable has to
// be reasoned about as part of in-process resolution instead.
//
// The walk is explicit rather than a recursive grep: recursive search in this
// workspace honours .gitignore, and the trees that matter are gitignored.
func TestNoBeadsCodeReadsGTDoltPort(t *testing.T) {
	root := repoRoot(t)
	var hits []string
	scanned := 0

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "testdata", "vendor":
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		// The guard's own list is the one deliberate mention.
		if rel, relErr := filepath.Rel(root, path); relErr == nil && rel == filepath.Join("internal", "testenv", "doltguard.go") {
			return nil
		}
		scanned++
		content, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if strings.Contains(string(content), "GT_DOLT_PORT") {
			rel, _ := filepath.Rel(root, path)
			hits = append(hits, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	if scanned == 0 {
		t.Fatalf("scanned 0 non-test .go files under %s: the walk found nothing, which is not the same as finding no matches", root)
	}
	sort.Strings(hits)
	if len(hits) > 0 {
		t.Errorf("GT_DOLT_PORT is read by beads production code in %v (scanned %d files); doltPortEnvVars' comment says it is guarded for subprocesses only", hits, scanned)
	}
	t.Logf("scanned %d non-test .go files under %s", scanned, root)
}
