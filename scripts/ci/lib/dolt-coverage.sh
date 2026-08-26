#!/usr/bin/env bash
# Decide whether a test run is allowed to report green while the Dolt-backed
# conformance contracts self-skip (bd-dln).
#
# THE PROBLEM THIS EXISTS FOR. scripts/ci/lib/test-env.sh adds `dolt` to
# BEADS_TEST_SKIP unless BEADS_TEST_ENV_RUN_DOLT=1, and
# internal/storage/embeddeddolt self-skips unless BEADS_TEST_EMBEDDED_DOLT=1.
# Both defaults are right for a contributor without Docker. Both are wrong for
# the run that a merge decision is made on: an MR touching backend/conformance/
# was gated on
#
#   TestImporterContract       SKIP 0.00s
#   TestRelationsContract      SKIP 0.00s
#   TestCycleDetectorContract  SKIP 0.00s
#
# reported as "96 packages ok, 0 FAIL". The only tell was a runtime —
# internal/storage/uow finished in 0.348s — and nothing that drives importer and
# relations contracts against a storage backend runs in a third of a second. A
# convention that depends on a human noticing 0.348s is not a convention.
#
# WHAT THIS DOES INSTEAD. When the tree under test differs from its merge base
# in one of the paths below, the wrapper owes those contracts an actual run.
# This library answers three questions for it:
#
#   which packages does the change put on the hook   (_packages)
#   can this machine run them                        (_precondition)
#   what environment turns them on                   (_enable_env)
#
# It never decides silently. A change that puts nothing on the hook costs
# nothing; a change that does either runs the contracts or fails the wrapper.

if [[ -n "${BEADS_CI_DOLT_COVERAGE_SH_LOADED:-}" ]]; then
    return 0
fi
BEADS_CI_DOLT_COVERAGE_SH_LOADED=1

# Keep this tag in sync with internal/testutil/testdoltcommon.go:DoltDockerImage
# and scripts/ci/pull-dolt-image.sh. scripts/dolt_coverage_lib_test.go fails if
# the three drift apart.
BEADS_DOLT_COVERAGE_IMAGE="dolthub/dolt-sql-server:2.2.0"

# The contracts, and nothing else. `-run` is deliberately narrow: the point is
# to run the code the change touched, not to drag each package's whole
# real-Dolt surface into a local gate that never carried it before.
BEADS_DOLT_COVERAGE_RUN='^Test.*Contract$|^TestConformance$'

# path prefix under change -> packages whose contracts it puts on the hook.
#
# backend/conformance/ holds the shared contract BODIES; every wiring below
# runs them, so touching it puts all three backends on the hook. The per-backend
# rows cover a change to the wiring alone.
_beads_dolt_coverage_table() {
    cat <<'EOF'
backend/conformance/|./internal/storage/uow/ ./internal/storage/dolt/ ./internal/storage/embeddeddolt/
internal/storage/uow/|./internal/storage/uow/
internal/storage/dolt/|./internal/storage/dolt/
internal/storage/embeddeddolt/|./internal/storage/embeddeddolt/
EOF
}

# beads_dolt_coverage_changed_files [REPO_ROOT]
#
# Every path this working tree changes relative to its merge base with the
# integration branch: committed, staged, unstaged and untracked.
#
# Returns 1 with the reason on stdout when git cannot answer — the same
# reason-on-stdout convention beads_dolt_coverage_precondition uses. The caller
# MUST NOT read a failure as "nothing changed": an unanswerable probe and a
# clean tree produce the same empty file list, and only one of them is evidence.
beads_dolt_coverage_changed_files() {
    local root="${1:-.}"

    if ! git -C "$root" rev-parse --git-dir >/dev/null 2>&1; then
        echo "not a git repository: $root"
        return 1
    fi

    local base="" ref
    for ref in origin/main main; do
        if git -C "$root" rev-parse --verify --quiet "$ref^{commit}" >/dev/null 2>&1; then
            base="$(git -C "$root" merge-base "$ref" HEAD 2>/dev/null)" || base=""
            [[ -n "$base" ]] && break
        fi
    done
    if [[ -z "$base" ]]; then
        echo "no merge base against origin/main or main"
        return 1
    fi

    {
        # Committed on this branch, plus staged and unstaged, plus untracked.
        # `diff $base` (two dots, no HEAD) already spans commit-to-worktree.
        git -C "$root" diff --name-only "$base" 2>/dev/null
        git -C "$root" ls-files --others --exclude-standard 2>/dev/null
    } | sed '/^$/d' | sort -u
}

# beads_dolt_coverage_packages < FILE-LIST
#
# The packages the changed paths on stdin put on the hook, one per line, in
# table order and deduplicated. Empty output means the change touches nothing
# Dolt-backed, which is the common case and costs nothing.
beads_dolt_coverage_packages() {
    local -a files=()
    local line
    while IFS= read -r line; do
        [[ -n "$line" ]] && files+=("$line")
    done

    ((${#files[@]} > 0)) || return 0

    local -a selected=()
    local row prefix packages pkg file have seen
    while IFS= read -r row; do
        prefix="${row%%|*}"
        packages="${row#*|}"
        for file in "${files[@]}"; do
            if [[ "$file" == "$prefix"* ]]; then
                for pkg in $packages; do
                    seen=0
                    for have in ${selected[@]+"${selected[@]}"}; do
                        [[ "$have" == "$pkg" ]] && seen=1 && break
                    done
                    ((seen)) || selected+=("$pkg")
                done
                break
            fi
        done
    done < <(_beads_dolt_coverage_table)

    ((${#selected[@]} > 0)) || return 0
    printf '%s\n' "${selected[@]}"
}

# beads_dolt_coverage_precondition PACKAGE
#
# Empty output and 0 when this machine can actually run PACKAGE's contracts.
# Otherwise 1, with the missing dependency named on stdout so the caller can
# put it in front of a human rather than skipping into a green run.
beads_dolt_coverage_precondition() {
    local pkg="$1"

    case "$pkg" in
        ./internal/storage/uow/)
            # Boots a real `dolt sql-server` per fixture; no container involved.
            if ! command -v dolt >/dev/null 2>&1; then
                echo "the 'dolt' binary is not on PATH"
                return 1
            fi
            ;;
        ./internal/storage/dolt/)
            # TestMain starts the containerized server via testcontainers.
            if ! docker info >/dev/null 2>&1; then
                echo "the Docker daemon is not reachable"
                return 1
            fi
            if ! docker image inspect "$BEADS_DOLT_COVERAGE_IMAGE" >/dev/null 2>&1; then
                echo "the $BEADS_DOLT_COVERAGE_IMAGE image is not cached locally (run scripts/ci/pull-dolt-image.sh)"
                return 1
            fi
            ;;
        ./internal/storage/embeddeddolt/)
            # In-process engine: cgo is the whole requirement, and .buildflags
            # already pins CGO_ENABLED=1 for every wrapper that sources it.
            if [[ "${CGO_ENABLED:-1}" == "0" ]]; then
                echo "CGO_ENABLED=0 (the embedded engine cannot be built)"
                return 1
            fi
            ;;
    esac
    return 0
}

# beads_dolt_coverage_enable_env PACKAGE
#
# The NAME=VALUE assignments that turn PACKAGE's contracts on, one per line.
# Each package self-skips behind a different switch, which is a large part of
# why they went dark separately.
beads_dolt_coverage_enable_env() {
    local pkg="$1"

    case "$pkg" in
        ./internal/storage/embeddeddolt/)
            echo "BEADS_TEST_EMBEDDED_DOLT=1"
            ;;
        *)
            echo "BEADS_TEST_ENV_RUN_DOLT=1"
            echo "BEADS_TEST_SKIP=$(beads_dolt_coverage_strip_skip "${BEADS_TEST_SKIP:-}")"
            ;;
    esac
}

# beads_dolt_coverage_strip_skip LIST
#
# LIST without its `dolt` entry. Needed because beads_test_env_enter has
# already exported BEADS_TEST_SKIP=dolt by the time the tier runs, and it
# returns early on a live inherited root, so a nested wrapper never re-derives
# it — setting BEADS_TEST_ENV_RUN_DOLT=1 alone would leave the skip in place
# and the tier would self-skip exactly like the run it exists to catch.
beads_dolt_coverage_strip_skip() {
    local list="$1" out="" entry
    local IFS=','
    for entry in $list; do
        entry="${entry#"${entry%%[![:space:]]*}"}"
        entry="${entry%"${entry##*[![:space:]]}"}"
        [[ -z "$entry" || "$entry" == "dolt" ]] && continue
        if [[ -n "$out" ]]; then
            out="$out,$entry"
        else
            out="$entry"
        fi
    done
    printf '%s' "$out"
}

# beads_dolt_coverage_requested PACKAGE REQUESTED...
#
# True when PACKAGE falls inside the package set the caller asked for. A run
# narrowed to ./internal/tracker/ is not the merge gate and is not owed the
# storage contracts; `./...` and `./internal/...` are.
beads_dolt_coverage_requested() {
    local pkg="$1"
    shift

    local want="${pkg#./}"
    want="${want%/}"

    local arg norm
    for arg in "$@"; do
        norm="${arg#./}"
        norm="${norm%/}"
        if [[ "$norm" == "..." ]]; then
            return 0
        fi
        if [[ "$norm" == */... ]]; then
            norm="${norm%/...}"
            [[ "$want" == "$norm" || "$want" == "$norm"/* ]] && return 0
            continue
        fi
        [[ "$want" == "$norm" ]] && return 0
    done
    return 1
}
