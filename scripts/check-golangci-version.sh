#!/usr/bin/env bash
# Hold every golangci-lint version literal in the GitHub workflows to the pin in
# .golangci-version.
#
# .golangci-version is what scripts/ci/lib/golangci-lint.sh resolves a binary
# against, so `make ci-pr-lint` and CI are one instrument only for as long as
# these agree. They are separate literals on purpose — a workflow that read the
# version out of a file at run time would be invisible to a reviewer and to the
# bots that bump action pins — so this check is what keeps them from drifting
# apart silently, which is the failure bd-824 was filed for.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
cd "$REPO_ROOT"

PIN_FILE=".golangci-version"
WORKFLOW_DIR=".github/workflows"

if [[ ! -f "$PIN_FILE" ]]; then
    printf 'error: %s is missing; it is the single source of truth for the golangci-lint version.\n' \
        "$PIN_FILE" >&2
    exit 1
fi

pinned="$(tr -d '[:space:]' < "$PIN_FILE")"
if [[ -z "$pinned" ]]; then
    printf 'error: %s is empty.\n' "$PIN_FILE" >&2
    exit 1
fi

# Emit "file:line:version" for every place a workflow names a golangci-lint
# version. Two spellings exist and both are matched here:
#
#   go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@vX.Y.Z
#   uses: golangci/golangci-lint-action@<sha>
#     with:
#       version: vX.Y.Z
#
# The second is found by context rather than by the bare `version:` key, which
# also appears under unrelated actions (release.yml pins goreleaser that way).
scan_workflows() {
    local file
    for file in "$WORKFLOW_DIR"/*.yml "$WORKFLOW_DIR"/*.yaml; do
        [[ -f "$file" ]] || continue
        awk -v file="$file" '
            # A version glued to the module path.
            match($0, /golangci-lint@v[0-9]+\.[0-9]+\.[0-9]+/) {
                v = substr($0, RSTART, RLENGTH)
                sub(/^golangci-lint@/, "", v)
                printf "%s:%d:%s\n", file, NR, v
            }
            # The action step, whose version arrives a few lines later under
            # `with:`. Ten lines is comfortably past the `env:` blocks the
            # workflows put in between.
            /golangci-lint-action@/ { window = 10; next }
            window > 0 {
                window--
                if (match($0, /^[[:space:]]*version:[[:space:]]*v[0-9]+\.[0-9]+\.[0-9]+[[:space:]]*$/)) {
                    v = $2
                    printf "%s:%d:%s\n", file, NR, v
                    window = 0
                }
            }
        ' "$file"
    done
}

found="$(scan_workflows)"

# A check that matched nothing would pass forever. Renaming a workflow, or
# switching to an action spelling this scanner does not know, must read as a
# failure of the check rather than as a clean bill of health.
if [[ -z "$found" ]]; then
    printf 'error: no golangci-lint version literal found under %s/.\n' "$WORKFLOW_DIR" >&2
    printf 'Either the workflows stopped pinning it, or this scanner no longer\n' >&2
    printf 'recognises how they do. Both need a human; a zero here is not a pass.\n' >&2
    exit 1
fi

status=0
count=0
while IFS=: read -r file line version; do
    count=$((count + 1))
    if [[ "$version" != "$pinned" ]]; then
        printf 'error: %s:%s pins golangci-lint %s, but %s says %s\n' \
            "$file" "$line" "$version" "$PIN_FILE" "$pinned" >&2
        status=1
    fi
done <<< "$found"

if (( status != 0 )); then
    cat >&2 <<EOF

The workflows and $PIN_FILE must name the same golangci-lint version.
\`make ci-pr-lint\` resolves its binary from $PIN_FILE, so a mismatch means a
contributor's gate and CI's gate are different instruments — which report
different finding SETS, not just different counts. See bd-824.

To fix: update $PIN_FILE and every workflow literal above together.
EOF
    exit 1
fi

printf 'golangci-lint pin %s: %d workflow literal(s) agree.\n' "$pinned" "$count"
