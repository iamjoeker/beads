# Testing Guide

`TESTING.md` is the single authority for test commands, test selection, and
test design in this repository. Use the commands here for local work. For the
exact command behind a current CI check, inspect its workflow and corresponding
`Makefile` target; the CI audit and cleanup plan are dated maintainer context.

## Choose the Smallest Useful Test

Test at the lowest seam that can fail for the user-visible reason. Add a
higher tier only when it covers a distinct risk that the lower tier cannot
show: integration wiring, a real persistence property, a process boundary, or
an external contract.

This keeps feedback fast and failures readable. It does not mean every test is
a unit test: use the real boundary when the defect could live there.

| Need | Run | When |
|---|---|---|
| Docs-only validation | `git diff --check`, `go test -tags=gms_pure_go ./test/docsync`, and `./scripts/check-doc-freshness.sh` | For prose-only changes; add any generated-doc or surface-specific link check the changed paths require. Do not run the full Go suite merely because a Markdown file changed. |
| Focused red/green loop | `./scripts/test.sh -run '^TestExactName$' ./path/to/package/...` | While writing or fixing one behavior. |
| Affected-package confidence | `./scripts/test.sh ./path/to/package/...` | After the focused test passes; include directly affected neighbors when their contract changed. |
| Final Go baseline | `make test` | Once after focused work on Go code is green. It applies the normal local build flags, coverage, and local skip handling. |
| Named CI wrapper | `make ci-pr-core`, `make ci-pr-policy`, or `make ci-pr-lint` | Run the wrapper whose risk or surface is affected, or use it to reproduce that CI check. Do not run all three routinely for every edit. |

Do not replace the focused loop with repeated full-suite runs. Run the final
`make test` once the affected Go tests are green. For docs-only changes, use
the docs, link, and diff checks instead.

## Commands and Local Environment

`./scripts/test.sh` is the normal runner. It sources `.buildflags`, creates an
isolated test environment, applies `.test-skip`, and defaults to a **25m
per-package** Go-test timeout. The timeout is a hang backstop, not a target
runtime. Override it only when diagnosing a legitimate slow path:

```bash
TEST_TIMEOUT=30m ./scripts/test.sh ./cmd/bd/...
TEST_VERBOSE=1 ./scripts/test.sh ./cmd/bd/...
TEST_RUN='^TestExactName$' ./scripts/test.sh ./cmd/bd/...

# Equivalent command-line options.
./scripts/test.sh -v -run '^TestExactName$' ./cmd/bd/...
./scripts/test.sh -timeout 30m ./cmd/bd/...
```

Use the opt-in ICU regex path only when the change requires it:

```bash
make test-icu-path
```

It is maintainer-only and not part of normal validation. `make test-full-cgo`
and `./scripts/test-cgo.sh` remain deprecated compatibility aliases.

Use a named specialized target only when its risk is in scope:

```bash
make test-regression
make test-upgrade
make test-cross-version
make test-migration
```

For a failing GitHub Actions check, follow the current workflow and its
`Makefile` target when exact reproduction matters. The local runner and CI
intentionally have different contracts in some cases.

### Test Environment and Readiness

The runner isolates `HOME`, Git configuration, and Dolt state. By default its
test environment adds `dolt` to `BEADS_TEST_SKIP`; set
`BEADS_TEST_ENV_RUN_DOLT=1` only when deliberately exercising the Dolt path
and its prerequisites are available. Do not make ordinary tests depend on a
developer's database, daemon, global Git configuration, or filesystem state.

To skip an optional service explicitly, use the existing skip mechanism:

```bash
BEADS_TEST_SKIP=dolt ./scripts/test.sh ./...
```

### The Dolt Coverage Tier

That default is right for a contributor without Docker and wrong for the run a
merge decision rests on. An MR touching only `backend/conformance/` was gated
on

```
TestImporterContract       SKIP (0.00s)
TestRelationsContract      SKIP (0.00s)
TestCycleDetectorContract  SKIP (0.00s)
```

reported as `96 packages ok, 0 FAIL`. The only tell was a runtime —
`internal/storage/uow` finished in 0.348s, and nothing that drives importer and
relations contracts against a storage backend runs in a third of a second
(bd-dln).

So `scripts/test.sh` no longer leaves that to whoever is reading the timings.
When the tree differs from its merge base with `origin/main` under
`backend/conformance/`, `internal/storage/uow/`, `internal/storage/dolt/` or
`internal/storage/embeddeddolt/`, the wrapper owes those contracts a real run
and does one of three things — never a green over code it did not execute:

- **runs them**, as a narrow second pass after the main suite, over just the
  packages the change put on the hook and just their `*Contract` and
  `TestConformance` tests. It is the last thing on screen, where the eye lands
  on a gate. Three backends measured 2m44s together;
- **refuses to start**, naming the missing dependency, when this machine cannot
  run them (`dolt` off `PATH`, no Docker daemon, no cached image). It checks
  before the main suite, so a gate that cannot finish costs seconds;
- **prints a waiver banner** naming the paths the result is not evidence for,
  under `BEADS_TEST_DOLT_COVERAGE=off`.

The tier stands down for a run narrowed by `-run`/`-skip` or `TEST_RUN` — a
targeted debugging pass is not the gate — and for a package set that does not
include the affected packages. `BEADS_TEST_ENV_RUN_DOLT=1` and
`BEADS_TEST_EMBEDDED_DOLT=1` already cover their halves, so a run that sets
them does not repeat the work.

`scripts/ci/lib/dolt-coverage.sh` holds the path table and the preconditions;
`scripts/dolt_coverage_lib_test.go` grades it by behaviour, including that an
unrelated change selects nothing and that a git probe which cannot answer says
so rather than reporting a clean tree.

### The `internal/storage/dolt` Suite

Because the runner and every CI job that walks `./...` skip Dolt, no routine
gate runs this package end to end. When you need to, run it through its target
rather than by hand:

```bash
make test-dolt
```

The target carries the two things a hand-written invocation keeps missing. It
passes `-timeout 60m`, because the suite measured 3414s (56.9 min) on an
unloaded box and `go test`'s 10m default therefore cannot reach the end of it —
a run that dies at its ceiling reports a deadline panic naming no failing test,
which reads as a slow suite rather than as a result you never got. And it
clears the injected `BEADS_DOLT_*` variables, which otherwise point the suite
at an agent's own server instead of the container `TestMain` starts.

`TestMain` refuses to start a whole-package run below that ceiling, so a wrong
invocation costs seconds instead of the ceiling. The refusal does not apply
to `-run`, `-bench`, or `-list`: a single test in this package finishes in
under two seconds, and that is the right way to work one of them.

### The `cmd/bd` Dolt Surface

The same default hides roughly 150 tests in `cmd/bd`: they need the Dolt
server that `TestMain` starts in a container, and `dolt` in `BEADS_TEST_SKIP`
makes every one of them skip. Run them through their wrapper:

```bash
scripts/ci/test-cmd-bd-dolt.sh              # the package, minus known-red
scripts/ci/test-cmd-bd-dolt.sh -run TestInit
```

The wrapper sets `BEADS_TEST_ENV_RUN_DOLT=1` so the tests run at all, and
`BEADS_CMD_BD_REQUIRE_DOLT=1` so a missing container fails the run instead of
skipping into a green one — without that, a Docker daemon that is not
reachable and a suite with nothing to report look identical. It needs `dolt`
on `PATH`, a reachable Docker daemon, and the Dolt image cached locally
(`scripts/ci/pull-dolt-image.sh`).

`scripts/ci/cmd-bd-dolt-known-red.txt` lists tests the wrapper skips because
they are known to fail here. **It is empty** — bd-2k4 drained the last twelve,
so the two jobs below now gate the whole package with nothing excused. Adding
an entry is a regression: it needs a bead and a reason on the line.
`scripts/ci/cmd-bd-dolt-known-red.sh check` fails when an entry stops naming a
real test, so the file cannot rot into a list of tests that no longer exist.

Both CI jobs shard `./cmd/bd` by test name, so neither ever runs the package
as one process. `scripts/ci/test-cmd-bd-dolt.sh` with no `-run` does, and is
the stricter check for that reason: it is what catches order-dependence
between tests the shards happen to separate. Two of bd-2k4's twelve — a pair
asserting on human-readable stderr while an earlier test left the `jsonOutput`
global set — were only ever red that way.

CI runs this surface in two places, both against the container: `Test (cmd/bd
init under real Dolt)` on every PR covers the `TestInit` family, and `Test
(cmd/bd under real Dolt N/4)` on every push to main covers the whole package.
Before those existed no job ran any of it, and the surface accumulated 25
failures across several releases (bd-kbx, bd-9jl).

Tests that need a temporary repository or store should use `t.TempDir()` and
`t.Cleanup()`. Temporary repositories must set a repository-local hooks path;
do not inherit the developer's global hooks configuration.

For manual CLI experiments, run both initialization and subsequent commands
from a disposable working directory:

```bash
beads_manual_dir="$(mktemp -d)"
(
  set -e
  cd "$beads_manual_dir"
  bd init --quiet --prefix test --skip-hooks --skip-agents
  bd create "Test issue" -p 1
)
rm -rf -- "$beads_manual_dir"
```

`BEADS_DB` selects a database for database-opening commands, but it does not by
itself redirect `bd init` workspace setup. Never run a manual `bd init` from a
production workspace merely because `BEADS_DB` points elsewhere.

`testing.Short()` is for genuine runtime, stress, or large-fixture skips. It
is not a substitute for declaring an integration, end-to-end, API, Docker, or
external-dependency boundary. Keep new uses within the repository policy:

```bash
make check-testing-short
```

## Test Design

### Seams, Scenarios, and Doubles

Write one scenario at the smallest seam that demonstrates the behavior. Cover
the boundary or failure mode that changes the user result; use table-driven
subtests when examples share setup. Do not repeat the same scenario through a
helper, every caller, and the CLI merely because all are available.

A recording double or fake should be narrow: model only the calls, inputs,
outputs, and failures the test needs. It should not recreate a storage engine,
process manager, or another subsystem just to make a unit test look realistic.

A behavioral fake is different. If it stands in for a contract shared by
multiple production implementations, give it the same semantic-conformance
suite as those implementations. That shared suite defines observable behavior;
it prevents the fake from teaching callers a contract production code does not
honor.

Semantic conformance asks whether an implementation produces the promised
results, errors, and state transitions for the same operation. Persistence
conformance asks whether the real persistence boundary preserves its required
durability, transaction, migration, and recovery properties. They answer
different questions. Do not claim backend parity unless a stated contract and
its conformance suite establish it.

### Tier Admission

An integration test belongs above the unit seam only when it covers a distinct
boundary that a narrow double cannot prove, such as configuration wiring, a
real filesystem or Git interaction, a subprocess protocol, or persistence
behavior.

An end-to-end test is admitted only when all of these are true:

1. The failure would be user-visible.
2. A real process, setup, or wiring boundary owns a distinct failure risk that
   no lower seam can prove.
3. Lower-tier tests cover the underlying behavior where practical, leaving the
   end-to-end test focused on that boundary.
4. No existing end-to-end test already covers the same boundary risk.

Lower-tier coverage of the same user journey does not disqualify the
end-to-end test; duplicate coverage of the same boundary risk does. State that
risk in the test name or nearby documentation.

### Avoid Incidental Complexity

Avoid these patterns unless they are the behavior under test:

- Duplicate scenarios at several layers.
- Global-state reset choreography. Prefer per-test state and cleanup.
- Subprocesses, listeners, sleeps, or real-store setup for a unit-level claim.
- Assertions on private implementation shape when observable behavior is the
  contract.
- Performance claims without a repeatable measurement and stated workload.

Sleeps, listeners, and real stores are appropriate when the test is specifically
about timing, lifecycle, protocol, or persistence. Keep the setup scoped and
make that reason apparent.

## Failures, Skips, and Review

`.test-skip` is a local, temporary exception list. If an unrelated failure is
already listed, report it rather than silently broadening the skip. Before
adding a new skip, record the issue it tracks and remove the skip when the
underlying failure is fixed.

Before opening a PR:

1. For docs-only changes, run the applicable docs, link, freshness, and diff
   checks; do not run the full Go suite by default.
2. For Go code, keep the focused and affected-package tests green, then run one
   final `make test`.
3. Run only the named CI wrapper, specialized target, or risk gate required by
   the changed surface, or the one needed to reproduce a CI result.

For historical CI inventory and maintainer planning context, consult
[CI_TEST_SURFACE_AUDIT.md](CI_TEST_SURFACE_AUDIT.md) and
[CI_CLEANUP_PLAN.md](CI_CLEANUP_PLAN.md). For current commands, use the
workflow files and `Makefile`; these context documents are not a second testing
guide or a live CI inventory.
