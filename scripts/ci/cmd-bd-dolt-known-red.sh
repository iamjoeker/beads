#!/usr/bin/env bash
# Read the known-red allowlist for the cmd/bd real-Dolt gate (bd-9jl).
#
# Usage:
#   cmd-bd-dolt-known-red.sh names   # one top-level test name per line
#   cmd-bd-dolt-known-red.sh regex   # anchored alternation for -skip / -run
#   cmd-bd-dolt-known-red.sh check   # fail if an entry no longer names a test
#
# The regex is anchored per alternative (^Name$). `go test -run/-skip` matches
# element-wise on the slash-separated test path, so ^Name$ selects that
# top-level test and everything under it, and nothing else.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
LIST_FILE="$SCRIPT_DIR/cmd-bd-dolt-known-red.txt"
PACKAGE="./cmd/bd"

read_names() {
    # Strip comments (whole-line and trailing) and blank lines.
    sed -e 's/#.*//' -e 's/[[:space:]]*$//' "$LIST_FILE" | grep -v '^$' || true
}

cmd_names() {
    read_names
}

cmd_regex() {
    local names
    mapfile -t names < <(read_names)
    if ((${#names[@]} == 0)); then
        return 0
    fi
    local joined
    joined="$(printf '^%s$|' "${names[@]}")"
    printf '%s\n' "${joined%|}"
}

cmd_check() {
    local names
    mapfile -t names < <(read_names)
    if ((${#names[@]} == 0)); then
        echo "Allowlist is empty -- nothing to check, and the gate now covers all of $PACKAGE."
        return 0
    fi

    # shellcheck source=../../.buildflags
    source "$REPO_ROOT/.buildflags"
    cd "$REPO_ROOT"

    local existing
    mapfile -t existing < <(
        GO_TEST_SHARD_TAGS="$BEADS_BUILD_TAGS" \
            go run -tags=ci_tools ./scripts/ci/go-list-test-names "$PACKAGE"
    )

    local -A have=()
    local name
    for name in "${existing[@]}"; do
        have["$name"]=1
    done

    local -a missing=()
    local -a duplicated=()
    local -A seen=()
    for name in "${names[@]}"; do
        if [[ -n "${seen[$name]:-}" ]]; then
            duplicated+=("$name")
        fi
        seen["$name"]=1
        if [[ -z "${have[$name]:-}" ]]; then
            missing+=("$name")
        fi
    done

    local status=0
    if ((${#missing[@]} > 0)); then
        echo "Known-red allowlist names tests that do not exist in $PACKAGE:" >&2
        printf '  %s\n' "${missing[@]}" >&2
        echo "Delete the line (the test is gone) or fix the name (it was renamed)." >&2
        status=1
    fi
    if ((${#duplicated[@]} > 0)); then
        echo "Known-red allowlist has duplicate entries:" >&2
        printf '  %s\n' "${duplicated[@]}" >&2
        status=1
    fi

    if ((status == 0)); then
        echo "Known-red allowlist OK: ${#names[@]} entries, all present in $PACKAGE."
    fi
    return "$status"
}

case "${1:-}" in
    names) cmd_names ;;
    regex) cmd_regex ;;
    check) cmd_check ;;
    *)
        echo "usage: $0 {names|regex|check}" >&2
        exit 2
        ;;
esac
