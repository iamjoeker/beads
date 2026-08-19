#!/usr/bin/env bash
# Run ./cmd/bd against a REAL Dolt server (bd-9jl).
#
#   scripts/ci/test-cmd-bd-dolt.sh              # whole package, minus known-red
#   scripts/ci/test-cmd-bd-dolt.sh -run TestInit
#
# scripts/ci/lib/test-env.sh adds `dolt` to BEADS_TEST_SKIP unless
# BEADS_TEST_ENV_RUN_DOLT=1, so the ~150 cmd/bd tests that need a Dolt server
# are skipped by every default local and CI run. This wrapper is the opt-in:
# it turns the Dolt surface on, refuses to run if the server is missing
# (rather than skipping into a green run), and applies the documented
# known-red allowlist.
#
# Requires: the `dolt` binary on PATH, a reachable Docker daemon, and the Dolt
# sql-server image cached locally (scripts/ci/pull-dolt-image.sh). Without
# them the suite exits non-zero with the reason -- silence is never success
# here, which is the whole point of the job.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"

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

exec "$REPO_ROOT/scripts/test.sh" "${args[@]}"
