#!/usr/bin/env bash
# Run ./cmd/bd against BOTH of its Dolt backends (bd-9jl, bd-nn6).
#
#   scripts/ci/test-cmd-bd-dolt.sh              # both passes, minus known-red
#   scripts/ci/test-cmd-bd-dolt.sh -run TestInit
#
# cmd/bd's Dolt-backed tests are two populations, and they cannot share a
# process:
#
#   server-backed    ~150 tests, dark unless BEADS_TEST_ENV_RUN_DOLT=1
#   embedded-backed  266 top-level tests across 112 files, dark unless
#                    BEADS_TEST_EMBEDDED_DOLT=1
#
# Mutually exclusive by construction: startTestDoltServerInner() returns early
# when BEADS_TEST_EMBEDDED_DOLT=1, so no container starts, testDoltServerPort
# stays 0, and BEADS_CMD_BD_REQUIRE_DOLT then fails the run outright. Setting
# both in one invocation does not widen the run; it aborts it. Hence two
# passes over the same package rather than one.
#
# Pass 1 is the original gate (bd-9jl): it turns the server surface on,
# refuses to run if the container is missing rather than skipping into a green
# run, and applies the documented known-red allowlist.
#
# Pass 2 is bd-nn6. Until it existed this script set the server pair only,
# while its header read as though that covered the package. It did not, and
# the shortfall was not merely local to this wrapper: 79 of the 266
# embedded-gated tests, across 24 files, were run by NO job anywhere.
# .github/scripts/embedded-test-shard.sh (job `test-embedded-cmd`) discovers
# tests by `^func TestEmbedded` in cmd/bd/*_embedded_test.go and finds 191 of
# them; the other 79 are gate-bearing under names that regex cannot see --
# TestDoltLocalOnly_*, TestDoltRemoteAdd_*, TestMigratePersonal_*,
# TestDoctor_*, TestExportExcludeOwner_* and friends, several of them in files
# whose own name ends _embedded_test.go. Selecting by package instead of by
# name is what closes that hole, and this is the only job that runs ./cmd/bd
# as a whole package.
#
# Requires: the `dolt` binary on PATH, a reachable Docker daemon, and the Dolt
# sql-server image cached locally (scripts/ci/pull-dolt-image.sh). Without
# them the suite exits non-zero with the reason -- silence is never success
# here, which is the whole point of the job. Pass 2 needs no container (the
# engine is in-process) but does need the same cgo-capable toolchain.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"

# A misspelt waiver is an error rather than a silent fall-through to the
# default: whoever typed it believed they had chosen something, and would read
# whatever came next as the result of that choice. Same rule as
# BEADS_TEST_DOLT_COVERAGE and BEADS_TEST_CENSUS in scripts/test.sh.
EMBEDDED_MODE="${BEADS_CMD_BD_EMBEDDED:-on}"
case "$EMBEDDED_MODE" in
    on | off) ;;
    *)
        echo "FATAL: BEADS_CMD_BD_EMBEDDED=$EMBEDDED_MODE; valid values are 'on' (default) and 'off'" >&2
        exit 1
        ;;
esac

# Run the tests that need a Dolt server instead of skipping them...
export BEADS_TEST_ENV_RUN_DOLT=1
# ...and fail loudly if the container never came up, so a broken runner can
# never look like a clean pass (cmd/bd's startTestDoltServer honours this).
export BEADS_CMD_BD_REQUIRE_DOLT=1

declare -a args=()
skip_regex="$("$SCRIPT_DIR/cmd-bd-dolt-known-red.sh" regex)"
if [[ -n "$skip_regex" ]]; then
    echo "Known-red (skipped, see scripts/ci/cmd-bd-dolt-known-red.txt):" >&2
    "$SCRIPT_DIR/cmd-bd-dolt-known-red.sh" names | sed 's/^/  /' >&2
    args+=(-skip "$skip_regex")
fi

args+=("$@")

# Default to the package the gate exists for. cmd/bd's subpackages have their
# own container-backed jobs (cmd/bd/doctor/fix via BEADS_FIX_REQUIRE_DOLT).
if [[ "$*" != *"./cmd/bd"* ]]; then
    args+=("./cmd/bd")
fi

# Statuses are captured per pass, never piped. A pipe replaces the exit code
# with its last stage's, and `set -e` would abandon pass 2 on a pass 1 failure
# -- both turn a two-pass gate back into a one-pass one, the second silently.
server_status=0
{
    echo ""
    echo "=============================================================="
    echo "  PASS 1/2 — cmd/bd against a real Dolt server (container)"
    echo "=============================================================="
} >&2
"$REPO_ROOT/scripts/test.sh" "${args[@]}" || server_status=$?

embedded_status=0
if [[ "$EMBEDDED_MODE" == "off" ]]; then
    {
        echo ""
        echo "=============================================================="
        echo "  PASS 2/2 WAIVED (BEADS_CMD_BD_EMBEDDED=off)"
        echo ""
        echo "  Whatever this run selected, it ran none of it against embedded"
        echo "  Dolt, so the result is not evidence about that backend."
        echo ""
        echo "  For an unnarrowed run that is 266 top-level tests across 112 of"
        echo "  cmd/bd's files -- and 79 of those, in 24 files, are run by no"
        echo "  other job either (test-embedded-cmd discovers by name and"
        echo "  cannot see them)."
        echo "=============================================================="
        echo ""
    } >&2
else
    {
        echo ""
        echo "=============================================================="
        echo "  PASS 2/2 — cmd/bd against embedded Dolt (in-process)"
        echo "=============================================================="
    } >&2
    # A subshell, so nothing here leaks back into pass 1's environment or into
    # a caller that sourced this script.
    (
        # REQUIRE_DOLT would abort this pass: with the embedded flag set, no
        # container starts, and requiring one is precisely how that gets
        # reported. RUN_DOLT is dropped with it so test-env.sh restores
        # BEADS_TEST_SKIP=dolt -- the server-backed tests then self-skip
        # instead of dialling a port nothing is listening on.
        unset BEADS_CMD_BD_REQUIRE_DOLT
        unset BEADS_TEST_ENV_RUN_DOLT
        # cmd/bd's embedded tests shell out to $BEADS_TEST_BD_BINARY
        # (init_embedded_test.go, test_helpers_pure_test.go and friends), so
        # this pass is only meaningful against a bd built from the tree under
        # test. scripts/test.sh builds exactly that -- but only when the value
        # is empty, because it deliberately yields to a caller-supplied one
        # (right for a CI job that just built and sha-verified the artifact,
        # wrong after a rehearsal merge, where an inherited binary is a
        # different tree's and every subprocess test fails as "unknown flag"
        # while reading as this branch's regression). Clearing it here buys
        # the prebuild from the tree under test, with .buildflags applied and
        # the temp dir cleaned up, rather than reimplementing all three.
        unset BEADS_TEST_BD_BINARY
        export BEADS_TEST_EMBEDDED_DOLT=1
        exec "$REPO_ROOT/scripts/test.sh" "${args[@]}"
    ) || embedded_status=$?
fi

{
    echo ""
    echo "=============================================================="
    echo "  cmd/bd Dolt gate"
    echo "    pass 1/2  real Dolt server   exit $server_status"
    if [[ "$EMBEDDED_MODE" == "off" ]]; then
        echo "    pass 2/2  embedded Dolt      WAIVED"
    else
        echo "    pass 2/2  embedded Dolt      exit $embedded_status"
    fi
    echo "=============================================================="
    echo ""
} >&2

if ((server_status != 0)); then
    exit "$server_status"
fi
exit "$embedded_status"
