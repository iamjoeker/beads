#!/usr/bin/env bash
# Shared hermetic environment setup for broad local/CI test wrappers.

if [[ -n "${BEADS_CI_TEST_ENV_SH_LOADED:-}" ]]; then
    return 0
fi
BEADS_CI_TEST_ENV_SH_LOADED=1

# Ownership stamp written into every root this library creates; its contents
# are the creating shell's PID. Two rules hang off it (bd-iik):
#
#   * cleanup removes a root only when the stamp still names the running
#     shell, so a shell that merely INHERITED BEADS_TEST_ENV_ROOT can never
#     delete a live root out from under the shell that owns it;
#   * a root is only trusted while its stamp exists, so a root that was
#     already rm -rf'd cannot be silently resurrected by `mkdir -p`.
#
# The second rule is the one that matters most. HOME, DOLT_ROOT_PATH,
# XDG_CONFIG_HOME and GIT_CONFIG_GLOBAL are all exported paths INSIDE the
# root, so any process holding that environment writes the root back into
# existence the moment it touches one of them. A resurrected root is
# distinguishable after the fact: mktemp -d creates 0700, mkdir -p re-creates
# 0755 under the usual umask. On the host that produced bd-iik, 49 of 63
# stranded roots were 0755 — deleted, then written back by something that
# outlived the delete.
BEADS_TEST_ENV_STAMP=".beads-test-env-owner"

beads_test_env_enter() {
    if [[ "${BEADS_TEST_ENV_DISABLE:-0}" == "1" ]]; then
        return 0
    fi
    if [[ "${BEADS_TEST_ENV_ACTIVE:-0}" == "1" ]]; then
        if beads_test_env_root_is_live; then
            return 0
        fi
        # We inherited BEADS_TEST_ENV_ROOT from a shell that has already
        # cleaned up. Every exported path still points into the deleted
        # directory, so this run is not hermetic in the way the wrapper
        # promises: its $HOME and Dolt root would be whatever the first
        # consumer happened to mkdir back, not the environment that was set
        # up for it. Build a fresh root this shell owns instead.
        unset BEADS_TEST_ENV_ROOT
        BEADS_TEST_ENV_ACTIVE=0
    fi

    local root
    root="$(mktemp -d "${TMPDIR:-/tmp}/beads-test-env-XXXXXX")"
    export BEADS_TEST_ENV_ROOT="$root"
    export BEADS_TEST_ENV_ACTIVE=1

    if [[ -z "${GOCACHE:-}" ]]; then
        local go_cache
        go_cache="$(go env GOCACHE 2>/dev/null || true)"
        if [[ -n "$go_cache" ]]; then
            export GOCACHE="$go_cache"
        fi
    fi
    if [[ -z "${GOMODCACHE:-}" ]]; then
        local go_mod_cache
        go_mod_cache="$(go env GOMODCACHE 2>/dev/null || true)"
        if [[ -n "$go_mod_cache" ]]; then
            export GOMODCACHE="$go_mod_cache"
        fi
    fi

    mkdir -p "$root/home" "$root/xdg-config" "$root/dolt-root"
    : >"$root/gitconfig"
    printf '%s\n' "$$" >"$root/$BEADS_TEST_ENV_STAMP"

    export HOME="$root/home"
    export USERPROFILE="$root/home"
    export XDG_CONFIG_HOME="$root/xdg-config"
    export DOLT_ROOT_PATH="$root/dolt-root"
    export GIT_CONFIG_NOSYSTEM=1
    export GIT_CONFIG_GLOBAL="$root/gitconfig"
    export BEADS_TEST_IGNORE_REPO_CONFIG=1
    if [[ "${BEADS_TEST_ENV_RUN_DOLT:-0}" != "1" ]]; then
        beads_test_env_add_skip "dolt"
    fi

    unset BEADS_DIR
    unset BEADS_DB
    unset BD_DB
    unset BD_JSON
    unset BD_NO_DB
    unset BD_NO_DAEMON
    unset BD_ACTOR
    unset BD_DOLT_AUTO_COMMIT
    unset BEADS_ACTOR
    unset GT_ROOT
    unset BEADS_DOLT_SHARED_SERVER
    unset BEADS_DOLT_SERVER_MODE
    unset BEADS_DOLT_AUTO_START
    unset BEADS_DOLT_SERVER_HOST
    unset BEADS_DOLT_SERVER_PORT
    unset BEADS_DOLT_PORT
    unset BEADS_DOLT_SERVER_DATABASE
    unset BEADS_DOLT_SERVER_SOCKET
    unset BEADS_DOLT_PASSWORD

    if command -v dolt >/dev/null 2>&1; then
        dolt config --global --add user.name "beads-test" >/dev/null 2>&1 || true
        dolt config --global --add user.email "test@beads.local" >/dev/null 2>&1 || true
    fi

    trap beads_test_env_cleanup EXIT
}

beads_test_env_add_skip() {
    local service="$1"
    local current=",${BEADS_TEST_SKIP:-},"
    if [[ "$current" != *",$service,"* ]]; then
        if [[ -n "${BEADS_TEST_SKIP:-}" ]]; then
            export BEADS_TEST_SKIP="${BEADS_TEST_SKIP},${service}"
        else
            export BEADS_TEST_SKIP="$service"
        fi
    fi
}

# beads_test_env_root_is_live: true when BEADS_TEST_ENV_ROOT still names a
# root this library created and nobody has since removed. Consumers MUST call
# this before writing into a root they inherited rather than created; writing
# unconditionally is what resurrects a deleted root (bd-iik).
beads_test_env_root_is_live() {
    local root="${BEADS_TEST_ENV_ROOT:-}"
    [[ -n "$root" && -d "$root" && -f "$root/$BEADS_TEST_ENV_STAMP" ]]
}

# beads_test_env_owner: PID recorded in the stamp of the current root, or the
# empty string when there is no readable stamp.
beads_test_env_owner() {
    local root="${BEADS_TEST_ENV_ROOT:-}"
    local owner=""
    if [[ -n "$root" && -r "$root/$BEADS_TEST_ENV_STAMP" ]]; then
        read -r owner <"$root/$BEADS_TEST_ENV_STAMP" || owner=""
    fi
    printf '%s' "$owner"
}

# beads_test_env_holders: PIDs of every live process other than this shell
# whose environment still names $1 as BEADS_TEST_ENV_ROOT.
#
# Deliberately NOT a process-tree walk. By the time cleanup runs, `go test` has
# exited and everything it spawned — dolt servers above all — has been
# reparented to init, so a walk from $$ finds nothing. The environment is the
# durable link: beads_test_env_enter exports BEADS_TEST_ENV_ROOT before any
# test process starts, so every process that could write inside the root
# carries the root's path, reparented or not.
#
# Empty (and silent) wherever neither probe is available; reaping is a
# best-effort narrowing of the window, never a correctness requirement.
beads_test_env_holders() {
    local root="$1" entry pid
    local needle="BEADS_TEST_ENV_ROOT=$root"

    if [[ -r /proc/self/environ ]]; then
        # environ is NUL-separated, which is exactly grep -z's record
        # separator; -x keeps a root from matching a longer sibling path.
        for entry in $(grep -l -a -z -F -x "$needle" /proc/[0-9]*/environ 2>/dev/null); do
            pid="${entry#/proc/}"
            pid="${pid%/environ}"
            printf '%s\n' "$pid"
        done
        return 0
    fi

    # BSD/macOS: ps -E appends the environment to the command column.
    ps -A -E -o pid=,command= 2>/dev/null |
        awk -v needle="$needle" 'index($0, needle) { print $1 }'
}

# beads_test_env_reap: terminate anything still holding the root before the
# root goes away. A survivor holds the exported DOLT_ROOT_PATH, HOME and
# GIT_CONFIG_GLOBAL, so removing the root without reaping just hands it a fresh
# empty directory to re-create — the stranded dolt-root-only leftovers in
# bd-iik are exactly that. Set BEADS_TEST_ENV_NO_REAP=1 to leave survivors
# alone.
beads_test_env_reap() {
    local root="$1"

    if [[ "${BEADS_TEST_ENV_NO_REAP:-0}" == "1" || -z "$root" ]]; then
        return 0
    fi

    local -a kids=()
    local pid
    for pid in $(beads_test_env_holders "$root"); do
        # This shell holds the root too; never signal it. The scan's own
        # subshell and grep inherit the environment as well, but they have
        # already exited by the time the loop body runs, so the liveness
        # re-check drops them — and stops a recycled PID being signalled in
        # place of something that died mid-scan.
        if [[ "$pid" == "$$" || "$pid" == "${BASHPID:-0}" ]]; then
            continue
        fi
        if kill -0 "$pid" 2>/dev/null; then
            kids+=("$pid")
        fi
    done
    ((${#kids[@]} > 0)) || return 0

    kill -TERM "${kids[@]}" 2>/dev/null || true

    # Up to two seconds for a graceful exit, then insist.
    local waited=0 alive
    while ((waited < 20)); do
        alive=0
        for pid in "${kids[@]}"; do
            if kill -0 "$pid" 2>/dev/null; then
                alive=1
                break
            fi
        done
        ((alive)) || return 0
        sleep 0.1
        ((waited += 1))
    done

    kill -KILL "${kids[@]}" 2>/dev/null || true
}

beads_test_env_cleanup() {
    if [[ "${BEADS_TEST_ENV_KEEP:-0}" == "1" ]]; then
        return 0
    fi

    local root="${BEADS_TEST_ENV_ROOT:-}"
    [[ -n "$root" ]] || return 0

    # Only the creating shell removes the root. Without this check a nested
    # wrapper that inherited BEADS_TEST_ENV_ROOT deletes the outer run's live
    # environment on its own EXIT, and the outer run then mkdir's it back.
    if [[ "$(beads_test_env_owner)" != "$$" ]]; then
        return 0
    fi

    beads_test_env_reap "$root"

    # Stop pointing at the grave BEFORE digging it, so nothing this shell runs
    # afterwards can write the root back into existence.
    unset BEADS_TEST_ENV_ROOT
    unset BEADS_TEST_ENV_ACTIVE
    unset DOLT_ROOT_PATH
    unset XDG_CONFIG_HOME
    unset GIT_CONFIG_GLOBAL

    rm -rf "$root"
}
