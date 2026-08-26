#!/usr/bin/env bash
# Required PR formatting and Go lint contract.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"

# shellcheck source=../.buildflags
source "$REPO_ROOT/.buildflags"
# shellcheck source=lib/timing.sh
source "$REPO_ROOT/scripts/ci/lib/timing.sh"
# shellcheck source=lib/golangci-lint.sh
source "$REPO_ROOT/scripts/ci/lib/golangci-lint.sh"

cd "$REPO_ROOT"

# Run the version CI runs, never whatever is first on PATH. The two are
# different instruments that disagree about the finding SET, not just the count,
# and the newer one's gosec taint rules are not even deterministic here — see
# the header of scripts/ci/lib/golangci-lint.sh and bd-824.
GOLANGCI_LINT="$(golangci_lint_binary "$REPO_ROOT")"

# Print it. Every report of this gate needs to say which binary produced it, and
# a wrapper that states it itself does not depend on the reporter remembering to.
printf 'golangci-lint %s (%s)\n' \
    "$(golangci_lint_pinned_version "$REPO_ROOT")" "$GOLANGCI_LINT"

# The PR lane sets this to the branch it merges into so only the issues the PR
# introduces are reported; main's push lane leaves it unset and sweeps the
# whole tree. See the lint comment in .github/workflows/pr.yml.
lint_scope=()
if [[ -n "${BD_LINT_NEW_FROM_MERGE_BASE:-}" ]]; then
    lint_scope=(--new-from-merge-base="$BD_LINT_NEW_FROM_MERGE_BASE")
fi

ci_time "gofmt check" -- ./scripts/ci/fmt-check.sh
ci_time "golangci-lint" -- \
    "$GOLANGCI_LINT" run --config=.golangci.yml --modules-download-mode=readonly \
        --timeout=5m --build-tags=gms_pure_go \
        ${lint_scope[@]+"${lint_scope[@]}"} ./...

# Other target tuples may not load files guarded by //go:build windows && !cgo.
# Cross-lint that non-CGO Windows build too, unless the native target above
# already matches it exactly.
native_goos="$(go env GOOS)"
native_cgo_enabled="$(go env CGO_ENABLED)"
if [[ "$native_goos" != "windows" || "$native_cgo_enabled" != "0" ]]; then
    ci_time "golangci-lint (windows)" -- \
        env GOOS=windows GOARCH=amd64 CGO_ENABLED=0 GOWORK=off \
            "$GOLANGCI_LINT" run --config=.golangci.yml --modules-download-mode=readonly \
                --timeout=5m --build-tags=gms_pure_go \
                ${lint_scope[@]+"${lint_scope[@]}"} ./...
fi
