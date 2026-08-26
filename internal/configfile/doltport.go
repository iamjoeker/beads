package configfile

import (
	"fmt"
	"os"
	"strconv"
)

// DoltPortEnvVars lists every environment variable bd consults for the Dolt
// server port, in resolution order: the primary spelling first, the legacy
// orchestrator spelling second.
//
// Anything that PUBLISHES a port must write every entry. Writing only one
// does not point bd at that port — the resolver takes the first var that is
// set, so a var left at its ambient value shadows the one the caller wrote
// (bd-799). Anything that CLEARS the port for isolation must likewise clear
// every entry, for the same reason from the other direction (bd-4xn).
var DoltPortEnvVars = []string{"BEADS_DOLT_SERVER_PORT", "BEADS_DOLT_PORT"}

// EnvTestMode names the process-wide flag that marks this process as a test
// run whose data must never reach a production Dolt server.
const EnvTestMode = "BEADS_TEST_MODE"

// EnvTestServer names the operator opt-in for a dedicated test server. It
// suppresses the suppressible production heuristics (see
// ProductionDoltPortReasons) but never the well-known default port.
const EnvTestServer = "BEADS_TEST_SERVER"

// EnvProductionPort lets a deployment on a non-default port declare it, so
// the production-port guards cover it too.
const EnvProductionPort = "BEADS_PRODUCTION_PORT"

// ProductionDoltPortReasons returns a human-readable label for each rule that
// flags port as belonging to a production Dolt server. An empty slice means
// the port is not detected as production.
//
// Two rules are knowable from the port alone:
//
//  1. port == DefaultDoltServerPort (3307). Unconditional — never suppressed
//     by BEADS_TEST_SERVER=1. The well-known Dolt default port is the single
//     highest-confidence production signal, and a dedicated test server
//     opting out of the other heuristics still must not bind to it.
//  2. BEADS_PRODUCTION_PORT, parsed to int, matches port. Suppressed by
//     BEADS_TEST_SERVER=1: it is a heuristic (an env var that can be stale or
//     misconfigured) rather than the fixed default, so an operator's explicit
//     opt-in into a dedicated test-server lane is honored for it.
//
// A third rule — a .beads/dolt-server.port file naming this port — needs a
// resolved beads directory and lives with the storage layer, which appends it
// to this slice.
func ProductionDoltPortReasons(port int) []string {
	if port <= 0 {
		return nil
	}
	var reasons []string
	if port == DefaultDoltServerPort {
		reasons = append(reasons, fmt.Sprintf("port %d == DefaultDoltServerPort", port))
	}
	if os.Getenv(EnvTestServer) == "1" {
		return reasons
	}
	if env := os.Getenv(EnvProductionPort); env != "" {
		if p, err := strconv.Atoi(env); err == nil && p > 0 && p == port {
			reasons = append(reasons, fmt.Sprintf("%s=%d matches", EnvProductionPort, p))
		}
	}
	return reasons
}

// IsProductionDoltPort reports whether port matches a production-server
// indicator that is knowable from the port alone. See
// ProductionDoltPortReasons for the rules and the BEADS_TEST_SERVER=1
// suppression.
func IsProductionDoltPort(port int) bool {
	return len(ProductionDoltPortReasons(port)) > 0
}

// DoltPortEnv is the outcome of resolving DoltPortEnvVars.
type DoltPortEnv struct {
	// Port is the resolved port. Only meaningful when Found is true.
	Port int
	// Var is the environment variable Port came from.
	Var string
	// Found reports whether any variable held a usable port.
	Found bool
	// GuardApplied reports that the test-isolation guard below overrode the
	// documented precedence. Callers that report provenance should say so.
	GuardApplied bool
}

// ResolveDoltPortEnv resolves the Dolt server port from the environment.
//
// The documented precedence is BEADS_DOLT_SERVER_PORT, then the legacy
// BEADS_DOLT_PORT. A variable that is unset, unparseable, or non-positive is
// skipped rather than treated as an answer, so a garbled primary falls
// through to the legacy spelling instead of silently discarding it.
//
// One exception exists because that precedence put the safety on the LOSING
// side (bd-4xn). Test harnesses and operators poison BEADS_DOLT_PORT to keep a
// run off the production server, but an agent shell exports
// BEADS_DOLT_SERVER_PORT=3307 — so the ambient production value won and the
// guard did nothing, while still reading as protection to whoever set it. When
//
//	BEADS_TEST_MODE=1, and
//	both variables hold a usable port, and
//	they disagree, and
//	the winner is a production port while the loser is not
//
// the loser wins. No configuration means "connect to production" while also
// naming a non-production port on the other variable; that combination is
// unambiguous evidence of a guard the precedence was about to discard.
//
// The exception is scoped to BEADS_TEST_MODE=1 so ordinary bd invocations keep
// the documented precedence exactly, and it can only ever resolve AWAY from a
// production port — never toward one.
func ResolveDoltPortEnv() DoltPortEnv {
	primary, primaryOK := doltPortFromEnv(DoltPortEnvVars[0])
	legacy, legacyOK := doltPortFromEnv(DoltPortEnvVars[1])

	if primaryOK && legacyOK && primary != legacy &&
		os.Getenv(EnvTestMode) == "1" &&
		IsProductionDoltPort(primary) && !IsProductionDoltPort(legacy) {
		return DoltPortEnv{Port: legacy, Var: DoltPortEnvVars[1], Found: true, GuardApplied: true}
	}
	if primaryOK {
		return DoltPortEnv{Port: primary, Var: DoltPortEnvVars[0], Found: true}
	}
	if legacyOK {
		return DoltPortEnv{Port: legacy, Var: DoltPortEnvVars[1], Found: true}
	}
	return DoltPortEnv{}
}

// doltPortFromEnv reads name and reports the port it holds. ok is false when
// the variable is unset, empty, unparseable, or non-positive.
func doltPortFromEnv(name string) (int, bool) {
	raw := os.Getenv(name)
	if raw == "" {
		return 0, false
	}
	port, err := strconv.Atoi(raw)
	if err != nil || port <= 0 {
		return 0, false
	}
	return port, true
}
