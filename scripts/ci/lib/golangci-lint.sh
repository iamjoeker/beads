# shellcheck shell=bash
#
# Resolve a golangci-lint binary at the version this repository pins.
#
# WHY THIS EXISTS
#
# `make ci-pr-lint` used to run whatever `golangci-lint` was first on PATH. CI
# installs a pinned version (.github/workflows/{pr,main}.yml); a contributor or
# agent who runs `go install .../golangci-lint@latest` gets a newer one. Those
# are not the same instrument, and they do not merely disagree about the NUMBER
# of findings — they disagree about the finding SET:
#
#   * The four G602 findings in backend/conformance that a v2.11.x run reports
#     do not exist at the pin, on this fork's main or on upstream's.
#   * gosec's v2.11.x taint rules (G702/G703/G704/G705) are additionally NOT
#     deterministic here: internal/doltserver/probe.go G704 flips run to run on
#     the GOOS=windows cross-lint pass below, cache-cleared and caps-off, on
#     unmodified main included.
#
# So the gate kept producing a red that CI could not reproduce — and, worse, a
# green that was not evidence, because the same tree came back clean on one run
# and dirty on the next. Reading those reports more carefully cannot reconcile
# two instruments. This makes there be one instrument.
#
# The pin lives in .golangci-version at the repo root, which is also what
# scripts/check-golangci-version.sh holds the workflow literals to. Bumping the
# version means editing that file and the workflows together; the check fails
# the PR if they drift. See bd-824, bd-8ob, bd-byk.
#
# A local install that already matches the pin is used as-is (this is the CI
# path — the workflow installs it onto PATH before calling the wrapper, so
# nothing is downloaded here). Anything else is ignored in favour of a pinned
# copy cached outside the repo, installed on first use.

# Print the version string recorded in .golangci-version, e.g. "v2.10.1".
golangci_lint_pinned_version() {
    local repo_root=$1 pinned
    pinned="$(tr -d '[:space:]' < "$repo_root/.golangci-version")"
    if [[ -z "$pinned" ]]; then
        printf 'error: %s/.golangci-version is empty\n' "$repo_root" >&2
        return 1
    fi
    printf '%s\n' "$pinned"
}

# Print the semantic version a golangci-lint binary reports, without a leading
# "v". Both spellings have shipped ("has version 2.10.1", "has version v2.11.4"),
# so match the number rather than the sentence around it.
_golangci_lint_version_of() {
    local bin=$1 out
    out="$("$bin" version 2>/dev/null)" || return 1
    [[ "$out" =~ ([0-9]+\.[0-9]+\.[0-9]+) ]] || return 1
    printf '%s\n' "${BASH_REMATCH[1]}"
}

_golangci_lint_install() {
    local pinned=$1 bindir=$2
    mkdir -p "$bindir"
    printf 'golangci-lint %s not found locally; installing it into %s\n' \
        "$pinned" "$bindir" >&2
    # GOFLAGS carries this repo's -tags=gms_pure_go (see .buildflags) and GOWORK
    # may point at a workspace; neither belongs in a third-party tool build.
    # `go install pkg@version` ignores the current module, so this does not
    # touch go.mod.
    env -u GOFLAGS GOWORK=off GOBIN="$bindir" \
        go install "github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$pinned" >&2
}

# Print the path to a golangci-lint binary at exactly the pinned version,
# installing one if no candidate matches. Fails rather than falling back to a
# different version — a wrapper that quietly linted with the wrong instrument is
# the whole defect this file closes.
golangci_lint_binary() {
    local repo_root=$1
    local pinned want bindir candidate found

    pinned="$(golangci_lint_pinned_version "$repo_root")" || return 1
    want="${pinned#v}"

    # An explicit override is honoured, but is still held to the pin: an
    # override that silently disagreed would reintroduce two instruments under a
    # name that looks deliberate.
    if [[ -n "${BD_GOLANGCI_LINT:-}" ]]; then
        found="$(_golangci_lint_version_of "$BD_GOLANGCI_LINT")" || found=""
        if [[ "$found" != "$want" ]]; then
            printf 'error: BD_GOLANGCI_LINT=%s reports version %s, but this repo pins %s (.golangci-version).\n' \
                "$BD_GOLANGCI_LINT" "${found:-unknown}" "$pinned" >&2
            return 1
        fi
        printf '%s\n' "$BD_GOLANGCI_LINT"
        return 0
    fi

    bindir="${BD_TOOLS_CACHE:-${XDG_CACHE_HOME:-$HOME/.cache}/beads/tools}/golangci-lint/$pinned"

    # PATH first (the CI path), then GOPATH/bin — which is where `go install`
    # puts it and which is frequently NOT on PATH, so a repo-local run would
    # otherwise die with exit 127 having linted nothing — then the pinned cache.
    for candidate in \
        "$(command -v golangci-lint 2>/dev/null || true)" \
        "$(go env GOPATH)/bin/golangci-lint" \
        "$bindir/golangci-lint"; do
        [[ -x "$candidate" ]] || continue
        found="$(_golangci_lint_version_of "$candidate")" || continue
        if [[ "$found" == "$want" ]]; then
            printf '%s\n' "$candidate"
            return 0
        fi
    done

    _golangci_lint_install "$pinned" "$bindir" || return 1

    found="$(_golangci_lint_version_of "$bindir/golangci-lint")" || found=""
    if [[ "$found" != "$want" ]]; then
        printf 'error: installed %s but it reports version %s\n' \
            "$pinned" "${found:-unknown}" >&2
        return 1
    fi
    printf '%s\n' "$bindir/golangci-lint"
}
