package doltserver

import (
	"testing"

	"github.com/steveyegge/beads/internal/configfile"
)

// TestDefaultConfig_NonzeroPortAlwaysHasSource pins the invariant that makes
// applyConfigDefaults' "nonzero ServerPort + PortSourceUnset ⇒ caller-explicit"
// inference sound: DefaultConfig must never hand back a resolved port with no
// source attached. If it does, a port DefaultConfig chose on the user's behalf
// is indistinguishable from one the caller explicitly asserted, and the storage
// layer stamps it PortSourceCallerExplicit (authoritative) — which silently
// disables the BEADS_DOLT_SERVER_PORT override and turns a benign auto-start
// port change into a hard failure (GH#4052).
func TestDefaultConfig_NonzeroPortAlwaysHasSource(t *testing.T) {
	// Neutralize ambient port env: a leaked BEADS_DOLT_SERVER_PORT from the
	// surrounding shell would resolve via PortSourceEnv and mask the gap.
	t.Setenv("BEADS_DOLT_SERVER_PORT", "")
	t.Setenv("BEADS_DOLT_PORT", "")
	t.Setenv("HOME", t.TempDir())

	for _, tc := range []struct {
		name   string
		shared string
	}{
		{name: "per-project mode", shared: "0"},
		{name: "shared-server mode", shared: "1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("BEADS_DOLT_SHARED_SERVER", tc.shared)
			cfg := DefaultConfig(t.TempDir())
			if cfg.Port != 0 && cfg.PortSource == PortSourceUnset {
				t.Fatalf("DefaultConfig returned Port=%d with PortSourceUnset "+
					"(PortSharedServer=%v): a port resolved on the user's behalf "+
					"is indistinguishable from a caller-explicit assertion",
					cfg.Port, cfg.PortSharedServer)
			}
		})
	}
}

// TestDefaultConfig_HonorsTestIsolationPortGuard pins that the server-lifecycle
// port chain and the connection port cannot disagree about which server this
// run is talking to.
//
// The two resolve independently: DefaultConfig walks portSources, while the
// storage layer resolves the env directly. Under bd-4xn's shape — a poisoned
// legacy BEADS_DOLT_PORT and an ambient production BEADS_DOLT_SERVER_PORT —
// the connection honours the poison. If this chain kept reading the raw
// primary, `bd dolt start`, the readiness probe and the credentials lookup
// would all aim at production while the queries went to the test server: a
// split no single-sided assertion would catch.
//
// Asserted through DefaultConfig rather than through portSources[0] directly,
// so a future rewrite of the chain that drops the guard still fails here.
func TestDefaultConfig_HonorsTestIsolationPortGuard(t *testing.T) {
	const poisonedPort = 54321

	t.Run("guard fires: the lifecycle follows the connection off production", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		t.Setenv("BEADS_TEST_MODE", "1")
		t.Setenv("BEADS_TEST_SERVER", "")
		t.Setenv("BEADS_DOLT_SERVER_PORT", "3307") // ambient production
		t.Setenv("BEADS_DOLT_PORT", "54321")       // the isolation guard

		cfg := DefaultConfig(t.TempDir())
		if cfg.Port != poisonedPort {
			t.Errorf("DefaultConfig().Port = %d, want %d (the guarded port the connection uses)", cfg.Port, poisonedPort)
		}
		if cfg.PortSource != PortSourceEnv {
			t.Errorf("DefaultConfig().PortSource = %q, want %q", cfg.PortSource, PortSourceEnv)
		}
		// Same environment, same answer, through the resolver the storage
		// layer calls — this is the agreement the test exists to pin.
		if env := configfile.ResolveDoltPortEnv(); env.Port != cfg.Port {
			t.Errorf("lifecycle resolved %d but the connection resolves %d", cfg.Port, env.Port)
		}
	})

	t.Run("guard silent: the documented primary still wins", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		t.Setenv("BEADS_TEST_MODE", "1")
		t.Setenv("BEADS_TEST_SERVER", "")
		t.Setenv("BEADS_DOLT_SERVER_PORT", "43211") // not production
		t.Setenv("BEADS_DOLT_PORT", "54321")

		cfg := DefaultConfig(t.TempDir())
		if cfg.Port != 43211 {
			t.Errorf("DefaultConfig().Port = %d, want 43211 (BEADS_DOLT_SERVER_PORT keeps precedence)", cfg.Port)
		}
	})

	t.Run("outside test mode nothing changes", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		t.Setenv("BEADS_TEST_MODE", "")
		t.Setenv("BEADS_TEST_SERVER", "")
		t.Setenv("BEADS_DOLT_SERVER_PORT", "3307")
		t.Setenv("BEADS_DOLT_PORT", "54321")

		cfg := DefaultConfig(t.TempDir())
		if cfg.Port != 3307 {
			t.Errorf("DefaultConfig().Port = %d, want 3307 (precedence is unchanged for a production bd)", cfg.Port)
		}
	})
}
