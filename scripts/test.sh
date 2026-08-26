#!/usr/bin/env bash
# Test runner that automatically skips known broken tests

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
SKIP_FILE="$REPO_ROOT/.test-skip"

# Canonical build flags (GOFLAGS=-tags=gms_pure_go, CGO_ENABLED=1).
# Opt-in ICU-path coverage remains available via scripts/test-icu-path.sh.
# shellcheck source=../.buildflags
source "$REPO_ROOT/.buildflags"
# shellcheck source=ci/lib/test-env.sh
source "$REPO_ROOT/scripts/ci/lib/test-env.sh"
# shellcheck source=ci/lib/dolt-coverage.sh
source "$REPO_ROOT/scripts/ci/lib/dolt-coverage.sh"

beads_test_env_enter

# Everything this script creates outside the hermetic root, removed on exit.
TEST_SH_OWNED_TMP=()

# Single EXIT handler for the whole script. beads_test_env_enter installs its
# own; replacing it here (rather than at each new cleanup obligation) keeps the
# ordering explicit — reap and remove what this run started, then hand off to
# the library, which removes the root only if this shell owns it.
test_sh_cleanup() {
    if declare -F cleanup_shared_server >/dev/null 2>&1; then
        cleanup_shared_server
    fi
    if [[ "${BEADS_TEST_ENV_KEEP:-0}" != "1" && ${#TEST_SH_OWNED_TMP[@]} -gt 0 ]]; then
        rm -rf "${TEST_SH_OWNED_TMP[@]}"
    fi
    beads_test_env_cleanup
}
trap test_sh_cleanup EXIT

# Build skip pattern from .test-skip file
build_skip_pattern() {
    if [[ ! -f "$SKIP_FILE" ]]; then
        echo ""
        return
    fi

    # Read non-comment, non-empty lines and join with |
    local pattern=$(grep -v '^#' "$SKIP_FILE" | grep -v '^[[:space:]]*$' | paste -sd '|' -)
    echo "$pattern"
}

# Default values.
#
# TIMEOUT is go test's PER-PACKAGE deadline — a hang backstop, not a
# performance budget: it costs nothing while packages pass. The floor is set
# by cmd/bd, the slowest package: measured 2026-07-26 on a busy darwin fleet
# box (wy-4mtr0), its default suite is ~1090s of test time across ~1490 tests
# (mostly serial — subprocess tests that spawn bd + embedded Dolt per
# invocation, largely t.Setenv-bound so they cannot t.Parallel), giving
# ~18min wall WITH the prebuilt-binary fast path below already removing the
# in-test `go build` steps. 3m could never fit that, which made every
# full-suite run FAIL with a package deadline panic naming no failing test.
# 25m holds the measurement plus fleet-load headroom (the passing full-suite
# acceptance run clocked cmd/bd at 870s under -p 4 package concurrency).
# Raise via TEST_TIMEOUT; don't lower it below cmd/bd's measured runtime.
TIMEOUT="${TEST_TIMEOUT:-25m}"
GO_TEST_PKG_PARALLEL="${GO_TEST_PKG_PARALLEL:-4}"
GO_TEST_PARALLEL="${GO_TEST_PARALLEL:-4}"
SKIP_PATTERN=$(build_skip_pattern)
VERBOSE="${TEST_VERBOSE:-}"
RUN_PATTERN="${TEST_RUN:-}"
COVERAGE="${TEST_COVER:-}"
COVERPROFILE="${TEST_COVERPROFILE:-/tmp/beads.coverage.out}"
COVERPKG="${TEST_COVERPKG:-}"

# Parse arguments
PACKAGES=()
# Set by a caller-supplied -run/-skip. A narrowed run is a targeted debugging
# pass, not the gate a merge decision is made on, so it does not owe the Dolt
# coverage tier below.
NARROWED_BY_CALLER=0
while [[ $# -gt 0 ]]; do
    case $1 in
        -v|--verbose)
            VERBOSE="-v"
            shift
            ;;
        -timeout)
            TIMEOUT="$2"
            shift 2
            ;;
        -run)
            RUN_PATTERN="$2"
            shift 2
            ;;
        -skip)
            # Allow additional skip patterns
            if [[ -n "$SKIP_PATTERN" ]]; then
                SKIP_PATTERN="$SKIP_PATTERN|$2"
            else
                SKIP_PATTERN="$2"
            fi
            NARROWED_BY_CALLER=1
            shift 2
            ;;
        *)
            PACKAGES+=("$1")
            shift
            ;;
    esac
done

# TEST_RUN narrows the run exactly as -run does.
if [[ -n "$RUN_PATTERN" ]]; then
    NARROWED_BY_CALLER=1
fi

# Default to all packages if none specified
if [[ ${#PACKAGES[@]} -eq 0 ]]; then
    PACKAGES=("./...")
fi

# Both run-mode strings are validated here, before any work: a typo should cost
# nothing, and the census one would otherwise sit behind a ~200 MB bd prebuild.
# A misspelt value is an error rather than a silent fall-through to the default
# — whoever typed it believed they had chosen something, and would read whatever
# came next as the result of that choice.
CENSUS_MODE="${BEADS_TEST_CENSUS:-on}"
case "$CENSUS_MODE" in
    on | strict | off) ;;
    *)
        echo "FATAL: BEADS_TEST_CENSUS=$CENSUS_MODE; valid values are 'on' (default), 'strict' and 'off'" >&2
        exit 1
        ;;
esac

# ---------------------------------------------------------------------------
# Dolt coverage tier (bd-dln)
#
# beads_test_env_enter has just added `dolt` to BEADS_TEST_SKIP, and
# internal/storage/embeddeddolt self-skips without BEADS_TEST_EMBEDDED_DOLT=1.
# Both are the right default for a contributor without Docker; both are wrong
# for the run a merge decision rests on. An MR touching backend/conformance/
# was gated on TestImporterContract / TestRelationsContract /
# TestCycleDetectorContract at SKIP 0.00s and reported "96 packages ok".
#
# So: when the tree under test differs from its merge base in a Dolt-backed
# path, this wrapper owes those contracts a real run. It either runs them (as a
# second, narrow pass after the main suite, where the eye lands) or it refuses
# to start — never a green over code it did not execute. Set
# BEADS_TEST_DOLT_COVERAGE=off to waive it, which prints a banner naming what
# the result is not evidence for. That is a decision a human makes and the log
# records, rather than one a 0.348s runtime hides.
# ---------------------------------------------------------------------------
DOLT_COVERAGE_MODE="${BEADS_TEST_DOLT_COVERAGE:-auto}"
DOLT_COVERAGE_PKGS=()
DOLT_COVERAGE_ON_HOOK=""

# A misspelt waiver is worth an error rather than a silent fall-through to
# auto: the user who typed it believed they had waived the tier, and would
# read whatever came next as the result of that decision.
if [[ "$DOLT_COVERAGE_MODE" != "auto" && "$DOLT_COVERAGE_MODE" != "off" ]]; then
    echo "FATAL: BEADS_TEST_DOLT_COVERAGE=$DOLT_COVERAGE_MODE; valid values are 'auto' (default) and 'off'" >&2
    exit 1
fi

if [[ "$NARROWED_BY_CALLER" != "1" ]]; then
    if _changed="$(beads_dolt_coverage_changed_files "$REPO_ROOT")"; then
        DOLT_COVERAGE_ON_HOOK="$(printf '%s\n' "$_changed" |
            grep -E '^(backend/conformance/|internal/storage/(uow|dolt|embeddeddolt)/)' || true)"
        while IFS= read -r _pkg; do
            [[ -n "$_pkg" ]] || continue
            beads_dolt_coverage_requested "$_pkg" "${PACKAGES[@]}" || continue
            # Already covered by the main run's own environment.
            case "$_pkg" in
                ./internal/storage/embeddeddolt/)
                    [[ "${BEADS_TEST_EMBEDDED_DOLT:-0}" == "1" ]] && continue
                    ;;
                *)
                    [[ "${BEADS_TEST_ENV_RUN_DOLT:-0}" == "1" ]] && continue
                    ;;
            esac
            DOLT_COVERAGE_PKGS+=("$_pkg")
        done < <(printf '%s\n' "$_changed" | beads_dolt_coverage_packages)
    else
        # An unanswerable probe and a clean tree both produce no packages. Say
        # which one this was, so a missing tier is never mistaken for the
        # absence of a Dolt-backed change ($_changed holds the reason here).
        echo "WARN: Dolt coverage tier could not run: $_changed" >&2
    fi
fi

if ((${#DOLT_COVERAGE_PKGS[@]} > 0)) && [[ "$DOLT_COVERAGE_MODE" == "off" ]]; then
    {
        echo ""
        echo "=============================================================="
        echo "  DOLT COVERAGE WAIVED (BEADS_TEST_DOLT_COVERAGE=off)"
        echo ""
        echo "  This tree changes:"
        printf '%s\n' "$DOLT_COVERAGE_ON_HOOK" | sed 's/^/    /'
        echo ""
        echo "  The contracts covering it self-skip at 0.00s in this run."
        echo "  Whatever it reports is NOT evidence for those paths."
        echo "=============================================================="
        echo ""
    } >&2
    DOLT_COVERAGE_PKGS=()
fi

# Refuse BEFORE the main suite rather than after it: a missing dependency is
# knowable now, and finding out 20 minutes later that the gate cannot finish is
# the same wasted run with a worse ending.
if ((${#DOLT_COVERAGE_PKGS[@]} > 0)); then
    _blocked=""
    for _pkg in "${DOLT_COVERAGE_PKGS[@]}"; do
        if ! _reason="$(beads_dolt_coverage_precondition "$_pkg")"; then
            _blocked+="  $_pkg: $_reason"$'\n'
        fi
    done
    if [[ -n "$_blocked" ]]; then
        {
            echo ""
            echo "FATAL: this tree changes Dolt-backed code whose contracts cannot run here."
            echo ""
            echo "Changed paths on the hook:"
            printf '%s\n' "$DOLT_COVERAGE_ON_HOOK" | sed 's/^/  /'
            echo ""
            echo "Blocked:"
            printf '%s' "$_blocked"
            echo "Install the missing dependency, or waive the tier with:"
            echo "  BEADS_TEST_DOLT_COVERAGE=off ./scripts/test.sh ..."
            echo ""
            echo "Waiving is a decision, not a default: the contracts self-skip at 0.00s"
            echo "and the suite still prints 'ok', which is how bd-dln happened."
            echo ""
        } >&2
        exit 1
    fi
fi

# Prebuild bd once for subprocess-style tests (wy-4mtr0). cmd/bd has a dozen
# test helpers that otherwise each `go build` the full bd binary inside the
# test run — on a busy machine those link steps alone can blow the package
# deadline, and one helper used to silently fall back to a stale repo-root
# ./bd. CI already exports BEADS_TEST_BD_BINARY from a prebuilt artifact
# (.github/workflows/main.yml); this gives the local runner the same fast
# path. A caller-supplied BEADS_TEST_BD_BINARY always wins; skipped when the
# requested packages cannot include cmd/bd.
if [[ -z "${BEADS_TEST_BD_BINARY:-}" ]]; then
    case " ${PACKAGES[*]} " in
        # Any recursive pattern (./..., ./cmd/...) can expand to cmd/bd.
        *"..."* | *"cmd/bd"*)
            # Only build into a root that is still live. An inherited
            # BEADS_TEST_ENV_ROOT whose owner already cleaned up is a grave,
            # not a workspace: `mkdir -p` would write a ~200 MB bd binary back
            # into a directory nothing will ever remove again, which is how
            # bd-iik's prebuilt-bd-only leftovers got there.
            if beads_test_env_root_is_live; then
                PREBUILT_BD_DIR="$BEADS_TEST_ENV_ROOT/prebuilt-bd"
            else
                PREBUILT_BD_DIR=$(mktemp -d "${TMPDIR:-/tmp}/beads-prebuilt-bd-XXXXXX")
                TEST_SH_OWNED_TMP+=("$PREBUILT_BD_DIR")
            fi
            mkdir -p "$PREBUILT_BD_DIR"
            echo "Prebuilding bd for subprocess tests..." >&2
            PREBUILT_BD_BIN="$PREBUILT_BD_DIR/bd$(go env GOEXE)"
            if go build -o "$PREBUILT_BD_BIN" "$REPO_ROOT/cmd/bd"; then
                export BEADS_TEST_BD_BINARY="$PREBUILT_BD_BIN"
                echo "Prebuilt bd: $BEADS_TEST_BD_BINARY" >&2
            else
                echo "WARN: bd prebuild failed; tests will build their own binaries" >&2
            fi
            ;;
    esac
fi

# ---------------------------------------------------------------------------
# Skip census (bd-5er)
#
# bd-dln taught this wrapper to refuse a green over Dolt-backed contracts it
# did not execute. That covers ~2% of the tree's self-skips. The other 98% are
# 283 t.Skip statements across 126 files gating on BEADS_TEST_EMBEDDED_DOLT,
# and they leave NO tell at all: a package whose every test skips prints
#
#   ok  	github.com/steveyegge/beads/internal/storage/embeddeddolt	0.001s
#
# byte-for-byte what a package whose every test passed prints. bd-dln's own
# defect was caught off a 0.348s runtime; there is no counterpart here.
#
# This cannot be fixed by printing harder from inside the tests. Measured on
# go1.26.6: a TestMain writing to BOTH os.Stdout and os.Stderr after m.Run()
# produced exactly "ok  \tprobe/m\t0.002s" and nothing else — the go tool
# buffers a test binary's entire output and discards it when the package
# passes. The only channels that survive a passing package are the ok line,
# -v, and -json. So the run goes through -json and tools/testcensus turns it
# back into the output you expect, followed by the count.
#
#   BEADS_TEST_CENSUS=on      (default) print the census; never changes status
#   BEADS_TEST_CENSUS=strict  also FAIL when a package reports ok having run
#                             no tests at all — the merge-gate posture
#   BEADS_TEST_CENSUS=off     plain `go test` output, no census
# ---------------------------------------------------------------------------
CENSUS_BIN=""
CENSUS_ARGS=()
CENSUS_JSON=()
if [[ "$CENSUS_MODE" != "off" ]]; then
    # Same live-root rule as the bd prebuild above: never write into an
    # inherited root whose owner has already cleaned up.
    if beads_test_env_root_is_live; then
        CENSUS_DIR="$BEADS_TEST_ENV_ROOT/testcensus"
    else
        CENSUS_DIR=$(mktemp -d "${TMPDIR:-/tmp}/beads-testcensus-XXXXXX")
        TEST_SH_OWNED_TMP+=("$CENSUS_DIR")
    fi
    mkdir -p "$CENSUS_DIR"
    CENSUS_CANDIDATE="$CENSUS_DIR/testcensus$(go env GOEXE)"
    if go build -o "$CENSUS_CANDIDATE" "$REPO_ROOT/tools/testcensus"; then
        CENSUS_BIN="$CENSUS_CANDIDATE"
        CENSUS_JSON=(-json)
        CENSUS_ARGS=(-trim "$(awk '/^module /{print $2; exit}' "$REPO_ROOT/go.mod")")
        if [[ "$CENSUS_MODE" == "strict" ]]; then
            CENSUS_ARGS+=(-strict)
        fi
        # -json puts the test binary in verbose mode either way; the filter
        # reconstructs the non-verbose view from it. Under -v there is nothing
        # to reconstruct and everything to pass through — without this, `-v`
        # would silently produce the same output as a run without it.
        if [[ -n "$VERBOSE" ]]; then
            CENSUS_ARGS+=(-v)
        fi
    else
        # Loud, because the fallback is the silence this exists to remove.
        echo "WARN: tools/testcensus failed to build; this run cannot tell you what it skipped" >&2
    fi
fi

# census_run LABEL COMMAND...
#
# Runs COMMAND — a `go test` invocation already carrying -json when the census
# is on — and returns GO TEST's status, not the pipeline's. A pipe replaces the
# exit code with its last stage's, which would make the census the thing being
# graded; PIPESTATUS is captured inside the group so no later command can
# clobber it. Census exit 3 is the strict-mode verdict and is the only case
# where this returns a status go test did not produce.
census_run() {
    local label="$1"
    shift

    if [[ -z "$CENSUS_BIN" ]]; then
        "$@"
        return $?
    fi

    local -a statuses=()
    { "$@" | "$CENSUS_BIN" "${CENSUS_ARGS[@]}" -label "$label"; statuses=("${PIPESTATUS[@]}"); } || true

    local go_status="${statuses[0]:-1}"
    local census_status="${statuses[1]:-0}"

    if ((go_status != 0)); then
        return "$go_status"
    fi
    if ((census_status == 3)); then
        echo "FAIL: BEADS_TEST_CENSUS=strict — a package reported ok having run no tests at all" >&2
        return 3
    fi
    if ((census_status != 0)); then
        echo "WARN: testcensus exited $census_status; the census above may be incomplete" >&2
    fi
    return 0
}

# Optional: start a single shared Dolt test server for all packages.
# When BEADS_TEST_SHARED_SERVER=1, we start one dolt sql-server and export the
# port so every test package reuses it instead of spawning its own. This
# reduces 8-16+ concurrent dolt processes down to 1.
#
# Both port vars are read here and written below. bd resolves
# BEADS_DOLT_SERVER_PORT before the legacy BEADS_DOLT_PORT, so naming only the
# legacy one leaves whichever value the other holds in charge — the shape that
# made a poisoned test-isolation guard inert (bd-4xn) and, before that, hid a
# test container behind an ambient production port (bd-799). Either var being
# set already means someone else has chosen the server for this run.
if [[ "${BEADS_TEST_SHARED_SERVER:-}" == "1" && -z "${BEADS_DOLT_PORT:-}" && -z "${BEADS_DOLT_SERVER_PORT:-}" ]]; then
    if command -v dolt &>/dev/null; then
        SHARED_DOLT_DIR=$(mktemp -d /tmp/beads-shared-test-dolt-XXXXXX)
        DOLT_ROOT_PATH="$SHARED_DOLT_DIR"
        export DOLT_ROOT_PATH

        dolt config --global --add user.name "beads-test" 2>/dev/null
        dolt config --global --add user.email "test@beads.local" 2>/dev/null

        SHARED_DB_DIR="$SHARED_DOLT_DIR/data"
        mkdir -p "$SHARED_DB_DIR"
        (cd "$SHARED_DB_DIR" && dolt init) >/dev/null 2>&1

        # Find a free port
        SHARED_PORT=$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()')

        dolt sql-server -H 127.0.0.1 -P "$SHARED_PORT" --no-auto-commit \
            --data-dir "$SHARED_DB_DIR" &>/dev/null &
        SHARED_DOLT_PID=$!

        # Wait for server to accept connections (up to 30s)
        for i in $(seq 1 60); do
            if nc -z 127.0.0.1 "$SHARED_PORT" 2>/dev/null; then
                break
            fi
            sleep 0.5
        done

        if nc -z 127.0.0.1 "$SHARED_PORT" 2>/dev/null; then
            # Every var bd consults for the port, not just the legacy one.
            export BEADS_DOLT_PORT="$SHARED_PORT"
            export BEADS_DOLT_SERVER_PORT="$SHARED_PORT"
            export BEADS_TEST_MODE=1
            echo "Shared test Dolt server started on port $SHARED_PORT (PID $SHARED_DOLT_PID)" >&2
            # Picked up by test_sh_cleanup, which is already trapped on EXIT.
            # Defining it rather than re-trapping keeps a single handler: the
            # old `trap ... EXIT` here silently replaced whatever the library
            # had installed.
            cleanup_shared_server() {
                kill "$SHARED_DOLT_PID" 2>/dev/null || true
                wait "$SHARED_DOLT_PID" 2>/dev/null || true
                rm -rf "$SHARED_DOLT_DIR"
            }
        else
            echo "WARN: shared Dolt server failed to start, falling back to per-package servers" >&2
            kill "$SHARED_DOLT_PID" 2>/dev/null || true
            rm -rf "$SHARED_DOLT_DIR"
        fi
    fi
fi

# Build go test command. -json goes in only when the census is on; it is the
# only way a passing package's skip count reaches the log at all, and
# tools/testcensus turns the stream back into the output you would otherwise
# have seen.
CMD=(go test ${CENSUS_JSON[@]+"${CENSUS_JSON[@]}"} -p "$GO_TEST_PKG_PARALLEL" -parallel "$GO_TEST_PARALLEL" -timeout "$TIMEOUT")

if [[ -n "$VERBOSE" ]]; then
    CMD+=(-v)
fi

if [[ -n "$SKIP_PATTERN" ]]; then
    CMD+=(-skip "$SKIP_PATTERN")
fi

if [[ -n "$RUN_PATTERN" ]]; then
    CMD+=(-run "$RUN_PATTERN")
fi

if [[ -n "$COVERAGE" ]]; then
    CMD+=(-covermode=atomic -coverprofile "$COVERPROFILE")
    if [[ -n "$COVERPKG" ]]; then
        CMD+=(-coverpkg "$COVERPKG")
    fi
fi

CMD+=("${PACKAGES[@]}")

echo "Running: ${CMD[*]}" >&2
echo "Skipping: $SKIP_PATTERN" >&2
echo "" >&2

status=0
census_run "main suite" "${CMD[@]}" || status=$?

if [[ -n "$COVERAGE" ]]; then
    total=$(go tool cover -func="$COVERPROFILE" | awk '/^total:/ {print $NF}')
    echo "Total coverage: ${total} (profile: ${COVERPROFILE})" >&2
fi

# The Dolt coverage tier runs LAST, so its result is the last thing on screen —
# the place the eye lands on a gate. It runs only over the packages the change
# put on the hook, and only over the contracts (BEADS_DOLT_COVERAGE_RUN), so it
# does not drag each package's whole real-Dolt surface into a local run that
# never carried it. A red main suite is already a red gate; nothing to add.
if ((status == 0)) && ((${#DOLT_COVERAGE_PKGS[@]} > 0)); then
    DOLT_COVERAGE_ENV=()
    for _pkg in "${DOLT_COVERAGE_PKGS[@]}"; do
        while IFS= read -r _assign; do
            [[ -n "$_assign" ]] || continue
            _seen=0
            for _have in ${DOLT_COVERAGE_ENV[@]+"${DOLT_COVERAGE_ENV[@]}"}; do
                [[ "$_have" == "$_assign" ]] && _seen=1 && break
            done
            ((_seen)) || DOLT_COVERAGE_ENV+=("$_assign")
        done < <(beads_dolt_coverage_enable_env "$_pkg")
    done

    DOLT_CMD=(env "${DOLT_COVERAGE_ENV[@]}"
        go test ${CENSUS_JSON[@]+"${CENSUS_JSON[@]}"}
        -p "$GO_TEST_PKG_PARALLEL" -parallel "$GO_TEST_PARALLEL"
        -timeout "$TIMEOUT" -run "$BEADS_DOLT_COVERAGE_RUN")
    if [[ -n "$VERBOSE" ]]; then
        DOLT_CMD+=(-v)
    fi
    DOLT_CMD+=("${DOLT_COVERAGE_PKGS[@]}")

    {
        echo ""
        echo "==> Dolt coverage tier: this tree changes"
        printf '%s\n' "$DOLT_COVERAGE_ON_HOOK" | sed 's/^/      /'
        echo "    so the contracts covering it must run rather than skip (bd-dln)."
        echo "Running: ${DOLT_CMD[*]}"
        echo ""
    } >&2

    # Censused too: a coverage tier that reports ok having run nothing is
    # precisely the failure this tier exists to prevent, one level up.
    census_run "Dolt coverage tier" "${DOLT_CMD[@]}" || status=$?
    if ((status != 0)); then
        echo "FAIL: Dolt coverage tier" >&2
    else
        echo "==> Dolt coverage tier OK" >&2
    fi
fi

exit $status
