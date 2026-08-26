package configfile

import (
	"testing"
)

// setPortEnv sets both port vars for the duration of t, treating "" as unset.
// t.Setenv("X", "") leaves X *set* to the empty string, which the resolver
// skips, so the two are equivalent here — but naming the intent keeps the
// tables below readable.
func setPortEnv(t *testing.T, primary, legacy string) {
	t.Helper()
	t.Setenv(DoltPortEnvVars[0], primary)
	t.Setenv(DoltPortEnvVars[1], legacy)
}

// TestResolveDoltPortEnv_PrecedenceUnchangedOutsideTestMode pins the
// documented precedence for ordinary bd invocations. The bd-4xn guard must not
// touch these: it is scoped to BEADS_TEST_MODE=1 precisely so a production
// `bd` keeps resolving exactly as its docs say.
func TestResolveDoltPortEnv_PrecedenceUnchangedOutsideTestMode(t *testing.T) {
	tests := []struct {
		name     string
		primary  string
		legacy   string
		wantPort int
		wantVar  string
	}{
		{"primary wins", "43211", "43212", 43211, DoltPortEnvVars[0]},
		{"legacy fills in when primary unset", "", "43212", 43212, DoltPortEnvVars[1]},
		{"production primary still wins", "3307", "1", 3307, DoltPortEnvVars[0]},
		{"unparseable primary falls through", "not-a-number", "43212", 43212, DoltPortEnvVars[1]},
		{"non-positive primary falls through", "0", "43212", 43212, DoltPortEnvVars[1]},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(EnvTestMode, "")
			setPortEnv(t, tt.primary, tt.legacy)

			got := ResolveDoltPortEnv()
			if !got.Found {
				t.Fatalf("ResolveDoltPortEnv() found nothing, want port %d", tt.wantPort)
			}
			if got.Port != tt.wantPort || got.Var != tt.wantVar {
				t.Errorf("ResolveDoltPortEnv() = port %d from %s, want port %d from %s",
					got.Port, got.Var, tt.wantPort, tt.wantVar)
			}
			if got.GuardApplied {
				t.Error("GuardApplied = true outside BEADS_TEST_MODE=1")
			}
		})
	}
}

// TestResolveDoltPortEnv_NothingSet covers the shapes with no usable port, so
// callers keep falling through to config and defaults rather than treating a
// garbled variable as an answer.
func TestResolveDoltPortEnv_NothingSet(t *testing.T) {
	for _, tt := range []struct{ name, primary, legacy string }{
		{"both unset", "", ""},
		{"both unparseable", "x", "y"},
		{"both non-positive", "0", "-1"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(EnvTestMode, "")
			setPortEnv(t, tt.primary, tt.legacy)
			if got := ResolveDoltPortEnv(); got.Found {
				t.Errorf("ResolveDoltPortEnv() = %+v, want not found", got)
			}
		})
	}
}

// TestResolveDoltPortEnv_TestIsolationGuard is bd-4xn's acceptance criterion.
//
// The guard fires only for the one unambiguous shape — test mode, both vars
// set, disagreeing, winner production and loser not — and resolves AWAY from
// production. Every other shape is a control: the same guard, the same
// environment but for one field, and the documented precedence intact. A guard
// that blocked everything would satisfy the first case alone.
func TestResolveDoltPortEnv_TestIsolationGuard(t *testing.T) {
	tests := []struct {
		name      string
		testMode  string
		primary   string
		legacy    string
		wantPort  int
		wantGuard bool
	}{
		{
			// The reported defect: the documented guard is poisoned onto the
			// LOSING variable while an agent shell holds production on the
			// winning one. Before bd-4xn this resolved to 3307.
			name:     "poison on the losing var beats an ambient production primary",
			testMode: "1", primary: "3307", legacy: "1",
			wantPort: 1, wantGuard: true,
		},
		{
			// Same shape with a real (reachable) test port rather than the
			// unreachable sentinel: the guard is about provenance, not about
			// the literal value 1.
			name:     "a container port on the losing var beats production",
			testMode: "1", primary: "3307", legacy: "54321",
			wantPort: 54321, wantGuard: true,
		},
		{
			// Control: the winner is not production, so precedence stands
			// even though the two disagree and the loser looks like a poison.
			name:     "non-production primary keeps precedence over a poisoned legacy",
			testMode: "1", primary: "43211", legacy: "1",
			wantPort: 43211, wantGuard: false,
		},
		{
			// Control: agreement is not a conflict. This is the shape a
			// correct harness produces (publish both), and it must run
			// normally.
			name:     "both hermetic and agreeing resolves normally",
			testMode: "1", primary: "43211", legacy: "43211",
			wantPort: 43211, wantGuard: false,
		},
		{
			// Control: both production. There is no non-production value to
			// prefer, so the guard has nothing to do — the storage layer's
			// own production check is what refuses this one.
			name:     "both production leaves the guard out of it",
			testMode: "1", primary: "3307", legacy: "3307",
			wantPort: 3307, wantGuard: false,
		},
		{
			// Control: outside test mode the same conflicting environment
			// resolves by the documented precedence.
			name:     "outside test mode the identical conflict resolves to production",
			testMode: "", primary: "3307", legacy: "1",
			wantPort: 3307, wantGuard: false,
		},
		{
			// Control: only one var set. A guard cannot be inferred from a
			// single value, and nothing here should invent one.
			name:     "production primary alone is left alone",
			testMode: "1", primary: "3307", legacy: "",
			wantPort: 3307, wantGuard: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(EnvTestMode, tt.testMode)
			t.Setenv(EnvTestServer, "")
			setPortEnv(t, tt.primary, tt.legacy)

			got := ResolveDoltPortEnv()
			if !got.Found {
				t.Fatalf("ResolveDoltPortEnv() found nothing, want port %d", tt.wantPort)
			}
			if got.Port != tt.wantPort {
				t.Errorf("ResolveDoltPortEnv() port = %d, want %d", got.Port, tt.wantPort)
			}
			if got.GuardApplied != tt.wantGuard {
				t.Errorf("GuardApplied = %v, want %v", got.GuardApplied, tt.wantGuard)
			}

			// The same resolution seen through the exported accessor every
			// caller actually uses, so the guard cannot be correct in the
			// helper and absent one layer up.
			cfg := &Config{DoltServerPort: 59999}
			if port := cfg.GetDoltServerPort(); port != tt.wantPort {
				t.Errorf("GetDoltServerPort() = %d, want %d", port, tt.wantPort)
			}
		})
	}
}

// TestResolveDoltPortEnv_GuardNeverResolvesToProduction is the property the
// guard must hold whatever the inputs: it may move a resolution off a
// production port, never onto one.
func TestResolveDoltPortEnv_GuardNeverResolvesToProduction(t *testing.T) {
	ports := []string{"", "0", "1", "3307", "43211", "not-a-number"}
	for _, primary := range ports {
		for _, legacy := range ports {
			t.Setenv(EnvTestMode, "1")
			t.Setenv(EnvTestServer, "")
			setPortEnv(t, primary, legacy)

			got := ResolveDoltPortEnv()
			if got.GuardApplied && IsProductionDoltPort(got.Port) {
				t.Errorf("primary=%q legacy=%q: guard resolved to production port %d",
					primary, legacy, got.Port)
			}
		}
	}
}

func TestProductionDoltPortReasons(t *testing.T) {
	t.Run("default port is unconditional", func(t *testing.T) {
		t.Setenv(EnvTestServer, "1")
		t.Setenv(EnvProductionPort, "")
		if !IsProductionDoltPort(DefaultDoltServerPort) {
			t.Errorf("port %d not detected as production even with %s=1",
				DefaultDoltServerPort, EnvTestServer)
		}
	})

	t.Run("BEADS_PRODUCTION_PORT is honored", func(t *testing.T) {
		t.Setenv(EnvTestServer, "")
		t.Setenv(EnvProductionPort, "28231")
		if !IsProductionDoltPort(28231) {
			t.Error("port named by BEADS_PRODUCTION_PORT not detected as production")
		}
	})

	t.Run("BEADS_PRODUCTION_PORT is suppressed by the test-server opt-in", func(t *testing.T) {
		t.Setenv(EnvTestServer, "1")
		t.Setenv(EnvProductionPort, "28231")
		if IsProductionDoltPort(28231) {
			t.Errorf("%s=1 should suppress the %s heuristic", EnvTestServer, EnvProductionPort)
		}
	})

	t.Run("ordinary ports are not production", func(t *testing.T) {
		t.Setenv(EnvTestServer, "")
		t.Setenv(EnvProductionPort, "")
		for _, port := range []int{0, -1, 1, 43211, 54321} {
			if IsProductionDoltPort(port) {
				t.Errorf("port %d detected as production", port)
			}
		}
	})
}
