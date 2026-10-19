# Testing Guide

`TESTING.md` is the single authority for test commands, test selection, and
test design in this repository. Use the commands here for local work.

## Building and Testing: Bazel Is the Gate

Bazel is beads' build and test system. CI gates every pull request on the
`bazel test` lanes in `.github/workflows/bazel.yml`, and several checks exist
only as Bazel targets: nogo (go test's vet checks plus the golangci-lint
linters, `tools/nogo`), gofmt (`//scripts/repochecks:fmt_test`) and the
repository guards under `scripts/repochecks`. A plain `go test` runs none of
them, so it is an inner-loop convenience CI does not enforce; a change is
ready when its Bazel lanes pass.

Install [Bazelisk](https://github.com/bazelbuild/bazelisk) as `bazel`; it
reads `.bazelversion`. The pre-commit hook (nogo on staged packages) and the
pre-push hook (the test lane) both need it.

Where actions run is per machine, never committed:

- **Contributors** read rbe-west's anonymous, read-only cache with
  `--config=fork-cache`: everything CI already executed is a cache hit, misses
  run on your machine, and nothing is uploaded. Make it the default with
  `build --config=fork-cache` in your gitignored `.bazelrc.local`, or pass it
  per command (`make test BAZEL_FLAGS=--config=fork-cache`).
- **Maintainers** with an rbe-west client certificate execute remotely with
  `--config=remote-exec`; the executor endpoint and TLS lines go in the
  gitignored `user.bazelrc` or `.bazelrc.local` (see the `.bazelrc` comments).
- **Agent hosts** whose `~/.bazelrc` names the executor need neither flag.

Each lane, as bazel.yml runs it (add your `--config=fork-cache` or
`--config=remote-exec` when it is not your default):

| Lane (bazel.yml job) | Command | Notes |
|---|---|---|
| Test (`bazel-test`) | `make test`, i.e. `bazel test //... --config=ci` | PR Core's selection: race, `-short`, skips. Includes nogo, gofmt and the repository guards. The default gate for every Go change. |
| Lint, all platforms | `make ci-pr-lint` | nogo natively plus the windows/amd64 and darwin/arm64 passes. `make lint-changed` covers only your changed packages. |
| Pure-Go (`bazel-pure`) | `bazel build --config=pure //cmd/bd:bd //cmd/bd:bd_test` | cgo off. The job's cmd/bd test subset (`PURE_CMD_BD_TESTS`) and js/wasm step are in bazel.yml. |
| Release cross-compile (`bazel-release-cross`) | `./scripts/ci/bazel-release-cross-compile.sh` | Every `go_library`/`go_binary` for each row of `scripts/ci/release-targets.txt`, cgo off, with nogo. |
| Integration (`bazel-integration`) | `bazel test //... --config=integration` | The `integration`-tagged build. Runs with the read-only cache too. |
| Dolt server (`bazel-doltserver`) | `bazel test //... --config=doltserver` | Starts its own `dolt sql-server` from the pinned binary; no docker. |
| cmd/bd Dolt server (`bazel-cmd-dolt`) | `bazel test //cmd/bd:bd_dolt_server_test --config=doltserver-cmd` | 32 shards of the integration-tagged cmd/bd suite (the slowest tests pinned to 16 of them). |
| Embedded Dolt (`bazel-embedded`) | `bazel test //... --config=embedded` | Remote execution only in CI; locally it is slow. Also runs the served http tiers with `BEADS_HTTP_TEST_REQUIRED=1`: `//backend/http:http_served_test`, `//internal/httpclient:httpclient_served_test` and the command corpus `//backend/http/corpus:corpus_served_test`, each sharded through `tools/bazel/go_test_pinned_shard.sh`. |
| Proxied server (`bazel-proxied`) | `bazel test //... --config=doltserver-proxied` | Remote execution only: 34 shards, each with a Dolt server. |
| Dolt server + proxied (`bazel-dolt-race`) | `bazel test //... --config=dolt-race` | Both rows above in one invocation; CI runs this instead of them in mode `remote`. |
| Server-Dolt storage (`bazel-server-storage`) | `bazel test //... --config=doltserver-integration` | Remote execution only. |
| Server storage + cmd/bd Dolt server (`bazel-dolt-int`) | `bazel test //... --config=dolt-int` | The two rows above in one invocation; CI runs this instead of them in mode `remote`. No job-level `continue-on-error`: it reports a `server_storage_result` output (plain-gated, like `bazel-server-storage`) and a `cmd_dolt_result` output (advisory unless `BAZEL_CMD_DOLT_REQUIRED` is `true`, like `bazel-cmd-dolt`) separately, via a per-tier classify step over the BEP, since its members have different gating categories. |
| Docs | `make check-docs` | `//test/docsync:docsync_test` and `//scripts/repochecks:doc_freshness_test` in the test lane's configuration, then `scripts/check-doc-flags.sh` against the pinned release. |

`make check` runs the testing.Short policy, `make ci-pr-lint` and
`make test`. After adding, removing or renaming Go files or changing imports
or `go.mod`, run `make bazel-sync` (gazelle, the `go_srcs` filegroups and
`MODULE.bazel`).

The Makefile keeps plain-Go twins under explicit `-go` names: `make test-go`
(`./scripts/test.sh`), `make check-go` and `make check-docs-go`. They work
offline and without Bazel, but they are not what CI enforces. GitHub Actions
jobs that still run Go-native suites call the `-go` names, never `make test`
(`TestWorkflowsNameTheirTestEngine`).

### Native macOS C/C++ toolchain

Bazel uses pinned LLVM 22.1.8 Clang on macOS, with the installed Apple SDK
and its native linker (`xcrun --find ld`). `MODULE.bazel` selects
`linker = "auto"` only for Darwin arm64 and x86_64. The macOS 27 SDK contains
`arm64e.x1` TAPI targets that the bundled LLVM 22.1.8 `ld64.lld` cannot parse;
pairing the SDK with Apple's linker avoids that incompatibility without
changing the shared SDK, Xcode selection, or installed compiler. Darwin's
SDK/linker are host dependencies, so this is not a hermetic macOS build.

Linux x86_64 still uses the SHA-pinned LLVM compiler and bundled LLD with
the pinned Ubuntu 24.04 sysroot. Host C/C++ toolchain detection stays disabled
on every platform; the Darwin override does not select `local_config_cc`.
`scripts/bazel_hermetic_cc_test.go` guards both contracts. A focused local
race build exercises the native cgo link through Go's race standard library:

```bash
bazel test //scripts:scripts_test --config=ci --test_filter='^TestBazelHermetic'
```

This link check is separate from full macOS gate portability: the Linux-only
timeout test backends remain tracked in upstream issue #7362. Run the full
`make test` gate on its supported Linux platform as well.

## Choose the Smallest Useful Test

Test at the lowest seam that can fail for the user-visible reason. Add a
higher tier only when it covers a distinct risk that the lower tier cannot
show: integration wiring, a real persistence property, a process boundary, or
an external contract.

This keeps feedback fast and failures readable. It does not mean every test is
a unit test: use the real boundary when the defect could live there.

| Need | Run | When |
|---|---|---|
| Docs-only validation | `git diff --check` and `make check-docs` | For prose-only changes; add any generated-doc or surface-specific link check the changed paths require. Do not run the full suite merely because a Markdown file changed. |
| Focused red/green loop | `bazel test //path/to/package:package_test --config=ci --test_filter='^TestExactName$'`, or the plain-Go `./scripts/test.sh -run '^TestExactName$' ./path/to/package/...` | While writing or fixing one behavior. The `go test` loop is fine here; the Bazel targets below are the gate. |
| Affected-package confidence | `bazel test //path/to/package/... --config=ci` | After the focused test passes; include directly affected neighbors when their contract changed. |
| Final gate | `make test` | Once after focused work on Go code is green: the whole test lane, mostly cache hits. |
| Another lane's risk | that lane's command from the table above | When the change touches what that lane covers (integration-tagged files, the Dolt server path, embedded Dolt, pure-Go builds). |
| Named CI wrapper | `make ci-pr-core` or `make ci-pr-lint` | Run the wrapper whose risk or surface is affected, or use it to reproduce that CI check. Do not run all three routinely for every edit. |
| Hook shims against real timeout implementations | `bazel test //tests/hook_timeout_backends:hook_timeout_backends_test` | After changing the hook generator in `cmd/bd/hooks.go` (then `make githooks-regen`) or anything under `.githooks/`. Runs the tracked managed sections against GNU coreutils, uutils, busybox and toybox `timeout` — the multicalls also installed as `gtimeout` alone — with and without Perl, under dash, bash and busybox ash (240 cases). About one deadline of wall time; needs no Go build. The PR-core lane runs it on every PR. |

Do not replace the focused loop with repeated full-suite runs. Run the final
`make test` once the affected tests are green. For docs-only changes, use
the docs, link, and diff checks instead.

### Git Hooks

`make install` points `core.hooksPath` at `.githooks`. The pre-commit hook
formats staged Go files and runs nogo on their packages
(`make lint-changed LINT_CHANGED_SCOPE=staged`). The pre-push hook runs
`scripts/pre-push-suite.sh` for branch pushes whose commits change Go or
Bazel inputs: `bazel test //... --config=ci`, with `--config=remote-exec`
when some rc file names an executor and `--config=fork-cache` otherwise.
`BD_PREPUSH_SUITE=rbe|cache|go` picks the mode; `go` (and the fallback when
bazel is not installed) runs `make test-go` under a banner saying it is not
the suite CI gates on.

## Plain `go test` (Inner Loop)

`./scripts/test.sh` is the plain-Go runner behind `make test-go`. It sources
`.buildflags`, creates an isolated test environment, applies `.test-skip`, and
defaults to a **25m per-package** Go-test timeout. The timeout is a hang
backstop, not a target runtime. Override it only when diagnosing a legitimate
slow path:

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

For a failing GitHub Actions check, run the command of its bazel.yml lane
from the table above; for the jobs that are not Bazel lanes, follow the
workflow and its `Makefile` target.

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

### The Production-Dolt Guard

Every Dolt port resolver in the tree ends in the same fallback: port 3307, the
live server. A test that builds a fixture `.beads` dir under `t.TempDir()` and
then calls production code inherits that fallback, so a test run from a
developer or agent sandbox can create real databases on the live server.

`internal/testenv.GuardProductionDolt` removes the fallback. It points
`BEADS_DOLT_SERVER_PORT`, `BEADS_DOLT_PORT` and `GT_DOLT_PORT` at port 1 —
dead, unbindable, and already this repo's sentinel for "resolution landed
somewhere deliberately dead" — so resolution stops there instead of reaching
the default. It acts on each variable independently and can only ever resolve
*away* from production: a variable already naming a deliberate non-production
port is left alone.

Call it as the first statement of `TestMain`:

```go
func TestMain(m *testing.M) {
    testenv.GuardProductionDolt()
    os.Exit(testMainInner(m))
}
```

First matters. A helper that starts a server for the whole run —
`testutil.EnsureDoltContainerForTestMain` — publishes its own port over these
variables, so it has to run after the guard, not before.

`test/doltguardpolicy` enforces both halves in CI: every `TestMain` opens with
the guard, and every test package that can reach Dolt has a `TestMain` in the
**default build**. A guard behind `//go:build integration` is not a guard for
the run anyone actually does.

Three things follow for test authors:

- **Never gate a test on whether a port variable is set.** It is always set
  now. `os.Getenv("BEADS_DOLT_PORT") != ""` was a stand-in for "a test
  container is running" and it never meant that — an agent shell satisfied it
  by exporting the production port. Use
  `testenv.SkipUnlessDoltServer(t, port)`, which dials.
- **A test whose subject is the unconfigured fallback** — "with no env, no port
  file and no config.yaml, which port does this resolve to?" — calls
  `testenv.WithoutDoltPortGuard(t)`, or it asserts the guard's output instead
  of the real default. An env port is authoritative in beads, so it also
  shadows the port-file and metadata chain.
- **Talking to the live server** needs `BEADS_ALLOW_TEST_DOLT=3307`. The value
  has to name the port; a bare `1` authorizes nothing. It is never set in CI.

This is separate from `BEADS_TEST_MODE=1`, which switches a good deal of
unrelated test behaviour and which nothing sets for a bare `go test`. The
guard does not set it and does not depend on it.

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

### The Skip Census

The tier above covers four paths and roughly 2% of this tree's self-skips. The
other 98% are 283 `t.Skip` statements across 126 files gating on
`BEADS_TEST_EMBEDDED_DOLT`, and they leave no tell at all — not even a runtime
(bd-5er):

```
$ ./scripts/test.sh ./internal/storage/embeddeddolt/
ok  	github.com/steveyegge/beads/internal/storage/embeddeddolt	34.241s
```

329 top-level tests in that package. **62 ran; 267 skipped.** Thirty-four
seconds of real work, so the 0.348s tell that exposed bd-dln has no counterpart
here, and the package is not empty either, so no "did anything run at all"
check reaches it.

This cannot be fixed inside the test files. Measured on go1.26.6, a `TestMain`
that writes a census to **both** `os.Stdout` and `os.Stderr` after `m.Run()`
produces, from `go test`, exactly `ok  probe/m  0.002s` and nothing else: the
go tool buffers a test binary's entire output and discards it when the package
passes. The only channels that survive a passing package are the `ok` line
itself, `-v`, and `-json`.

So `scripts/test.sh` runs the suite under `-json` and pipes it through
`tools/testcensus`, which reconstructs the plain non-verbose output you would
otherwise have seen and then adds what `go test` cannot say:

```
==============================================================
  SKIP CENSUS — main suite — what this run's "ok" is not evidence for (bd-5er)

  329 top-level tests: 62 ran, 267 SKIPPED

  Skipped behind:
       267  BEADS_TEST_EMBEDDED_DOLT
==============================================================
```

Skips are grouped by the environment variable their message names, which turns
283 individual skips back into the handful of decisions that produced them. A
skip whose message names no variable is reported separately, with an example,
rather than being folded into a total.

| `BEADS_TEST_CENSUS` | behaviour |
| --- | --- |
| `on` (default) | print the census; never changes the run's status |
| `strict` | also **fail** when a package reports `ok` having run no tests at all |
| `off` | plain `go test` output, no census |

`strict` is the merge-gate posture. It fails only on a package that ran
*nothing*, not on a skip count: a partially skipped package's `ok` still means
something, and a threshold on 283 would be a number nobody could maintain. A
package with no test files at all is not vacuous and never fails it.

**Measured over `./...`, no package is vacuous** — every one runs at least one
test — so `strict` is adoptable today and costs nothing until a package goes
fully dark. That is also the reason the count, not the vacuity check, is the
part that had to exist: over the whole tree one run reported

```
8518 top-level tests: 6400 ran, 2118 SKIPPED
    1050  BEADS_TEST_SKIP
     528  BEADS_TEST_EMBEDDED_DOLT
     376  (no environment gate named)     e.g. Dolt test server not available
     153  BEADS_TEST_PROXIED_SERVER
       8  BEADS_TEST_PROXIED_LOCAL
       1  BEADS_DB / BEADS_RUN_DOLT_UPSTREAM_REPRO / BEADS_TEST_SHARED_SERVER
```

A quarter of the suite, behind eight distinct gates rather than the one this
started from, and a strict run would have passed every bit of it.

Two properties the implementation is graded on, in
`tools/testcensus/census_test.go`, both differentially against the go tool
itself rather than against a fixture:

- what the filter prints must equal what `go test` prints without `-json`;
- and the same under `-v`. `-json` puts the test binary in verbose mode, so a
  filter that reconstructs the non-verbose view unconditionally would silently
  delete the output `-v` was invoked for.

The census reports `go test`'s exit status, never the pipeline's — a pipe
otherwise replaces the exit code with its last stage's, which would make the
census the thing being graded.

#### Two things `-json` changes that will surprise you

**`os.Stderr` is not `os.Stderr` under `-json`.** The go command runs the
binary with `-test.v=test2json`, and `testing.M.Run` then points `os.Stderr`
**at `os.Stdout`** so stderr writes join the single JSON stream. It never
restores it. Measured on go1.26.6 in a module containing nothing but the probe:

```
TESTMAIN-BEFORE  stderr fd=2 name="/dev/stderr"     (before m.Run)
inside a test    stderr fd=1 name="/dev/stdout"
TESTMAIN-AFTER   stderr fd=1 name="/dev/stdout"     (after m.Run returns)
```

`cmd/bd/zz_stdio_leak_guard_test.go` captured its baseline at package-variable
initialization — the only place it can be captured, since `m.Run` performs the
swap — and therefore reported a stdio leak on *every* run under `-json`,
whatever the tests did. It now expects whichever stream the runner installed,
and a second test grades that by behaviour so it cannot quietly revert. Any
future test that asserts stdout and stderr are distinct files will fail under
the default runner for the same reason, and it will not be a leak.

**test2json dedents subtests, and orders them innermost-first.** A failing
subtest's lines arrive with their frame indentation stripped and before their
parent's marker. The filter puts the nesting back (four spaces per `/` in the
test name) and hoists each failure's header above its detail, so the default
output matches `go test` exactly. Under `-v` it does not: `-test.v=test2json`
is its own verbosity mode, and passthrough keeps its flat innermost-first
shape rather than `go test -v`'s indented tree. Everything is present either
way; reproducing `testing`'s tree printer is not worth owning.

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
Tests that need a Dolt SQL server get one from `internal/testutil`
(`EnsureDoltContainerForTestMain`, `RequireDoltContainer`,
`StartIsolatedDoltContainer[Handle]`, `NewContainerProvider`). Two backends
sit behind that API, selected by `BEADS_TEST_DOLT_SERVER`:

- `container`: the `dolthub/dolt-sql-server` image through testcontainers
  (needs docker and the pulled image). The default under plain `go test`.
- `local`: a `dolt sql-server` started by the test process from the pinned
  dolt CLI (`BEADS_TEST_DOLT_BINARY`, else `dolt` on `PATH`; it must be the
  image's version). No docker. Used only when explicitly selected, under
  `go test` and `bazel test` alike (a Bazel target's `env`, or
  `--test_env=BEADS_TEST_DOLT_SERVER=local`); unset means `container`, which
  in a Bazel action without docker keeps the usual skip.

`BEADS_TEST_REQUIRE_DOLT_CONTAINER=1` turns an unavailable backend into a
failure (per test and in every `TestMain`) instead of a skip; lanes that
exist to run the Dolt suites set it. `BEADS_TEST_REQUIRE_SOCAT=1` does the
same for the proxied subtests that bridge an external endpoint with `socat`
(external-unix, the outage/reconnect matrix); `//cmd/bd:bd_proxied_test`
sets it; the legacy GitHub proxied jobs retired in #7353 had no `socat`
and left it unset.

Under Bazel, `bazel test //... --config=doltserver` runs the Dolt-backed
domain, uow, tracker, doctor/fix, protocol and testutil suites on the `local`
backend (the `dolt-server` targets); they need no docker and execute
remotely with `--config=remote-exec`. No Bazel target uses the `container`
backend; run `go test` with docker and the pulled image to exercise it.
PR Risk's heavier server tiers have configs of their own, run by bazel.yml
only with remote execution, each in a job of its own (`bazel-proxied`,
which in mode `remote` is folded with `bazel-doltserver` into
`bazel-dolt-race`, and `bazel-server-storage`, which in mode `remote` is
folded with `bazel-cmd-dolt` into `bazel-dolt-int`):
`--config=doltserver-proxied` is the
proxied-server cmd/bd tier ("Test (Proxied Dolt Cmd N/15)",
`//cmd/bd:bd_proxied_test`), and `--config=doltserver-integration` the
server-Dolt storage tier ("Test (Server Dolt Conformance)", "Test (Server
Dolt Full Suite N/16)", `//internal/storage/dolt:dolt_server_*_test`), which
builds with the integration tag like `--config=integration`. Each shard runs
its CI job's shard script, so for `--config=doltserver-integration` Bazel
shard k runs the tests of job k+1 (both split the manifest's 16-shard block
the same way). `--config=doltserver-proxied`'s `bd_proxied_test` instead
runs the manifest's own 34-shard block, bin-packed by measured duration (the
manifest's legacy 15-shard, bd-init-cost-proxy block, once read by PR Risk's
retired fork/push jobs, has been deleted; those jobs no longer exist).

`--config=doltserver-cmd` (`//cmd/bd:bd_dolt_server_test`, bazel.yml's
advisory `bazel-cmd-dolt` job) runs the whole integration-tagged cmd/bd
suite on the `local` backend: the Dolt-gated cmd/bd tests (`TestCLI_*`, the
init and store-backed suites) that every other lane skips with
`BEADS_TEST_SKIP=dolt` or leaves out of its manifest. It shares
`--config=integration`'s build and runs it in 32 shards: 16 run the slowest
tests `cmd/bd/dolt_server_pinned_shards.txt` pins to them, and 16 are the Go
binary's own round-robin shards over every other top-level test
(`tools/bazel/go_test_pinned_shard.sh`; `//tests/regression:regression_test`
splits the same way). After a change that moves test durations, regenerate the
manifest from a run of the lane with `tools/bazel/pin_shards.py` (see its
usage); a stale manifest only costs balance, never coverage. It runs where
the integration lane runs (remote, or with the read-only cache). pr.yml's
gate requires it once `BAZEL_CMD_DOLT_REQUIRED` is `"true"`; pr.yml then
also passes bazel.yml `cmd-dolt-required: true`, and the PR's
`bazel-integration` lane runs `//... -//cmd/bd:bd_test`, so each cmd/bd
integration-build test runs once (push, nightly and bazel-farm runs keep
`bd_test` in the integration lane). Locally:
`bazel test //cmd/bd:bd_dolt_server_test --config=doltserver-cmd`.

An ambient `BEADS_DOLT_SERVER_PORT` or `BEADS_DOLT_PORT` is never honored by
the suites that call `testutil.EnsureDoltContainerForTestMain`. When a test
container is started, that container's port overwrites both variables; when one
cannot be started -- for any reason, including `BEADS_TEST_SKIP=dolt` -- both
are cleared, so no environment-named server can be resolved. Point a test run
at a specific Dolt server by starting a container for it, not by exporting a
port.

The one sanctioned way to hand these suites a server you started yourself is
`./scripts/test.sh` with `BEADS_TEST_SHARED_SERVER=1`: it starts one
`dolt sql-server`, exports its port as `BEADS_DOLT_PORT`, and marks it by
exporting `BEADS_TEST_SHARED_DOLT_SERVER` set to that same port number. It
starts nothing if either port variable is still set when it gets there. The
runner's test environment clears both first, so that only happens when that
isolation is skipped (for example `BEADS_TEST_ENV_DISABLE=1`), and the script
then says so on stderr. The marker -- set only by that script, only for a
port it allocated -- is what the helper treats as container-equivalent, and
only for a variable holding exactly the port it names: that variable survives
the clearing above, and any other port variable is still cleared. A container
still wins where one can be started; the shared server is the Docker-less path.

Clearing the environment variables closes the channel the ambient port
travelled on; it does not make a port unresolvable in general. Resolution
continues into the file chain (`.beads/dolt-server.port`, `config.yaml`,
`metadata.json`), and `internal/storage/dolt`'s production-port detection is
narrower than that resolution -- see `be-rl6tm`, which tracks the remaining
gap.

Tests that need a temporary repository or store should use `t.TempDir()` and
`t.Cleanup()`. Temporary repositories must set a repository-local hooks path;
do not inherit the developer's global hooks configuration.

In `cmd/bd`, fresh-workspace command fixtures should call
`isolateBeadsDirForTest(t)` before setup or dispatch. It clears inherited
`BEADS_DIR` and restores that variable exactly at cleanup, even after raw
command-dispatch mutations. The `TestMain` reset only isolates startup.
These fixtures must not use `t.Parallel()`. Tests intentionally selecting a
workspace should use `t.Setenv("BEADS_DIR", ...)`; `initConfigForTest` and
`ensureCleanGlobalState` preserve that selection.

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

**Tmpfs hosts:** the `cmd/bd` test suite creates an isolated `$HOME` and several
test binaries under `$TMPDIR`. They are normally cleaned by the test process,
but a SIGKILLed or OOMed run can leave orphans behind. On hosts where `/tmp`
is tmpfs (e.g. Fedora Atomic / Bluefin), run `make clean-test-tmp` between
test runs if `du -sh /tmp/beads-* /tmp/bd-*` shows accumulation. See bd-3q2u.

`testing.Short()` is for genuine runtime, stress, or large-fixture skips. It
is not a substitute for declaring an integration, end-to-end, API, Docker, or
external-dependency boundary. Keep new uses within the repository policy:

```bash
make check-testing-short
```

### Dolt Container Tests (podman-rootless)

Anything that does not carry `BEADS_TEST_SKIP=dolt` — including
`BEADS_TEST_ENV_RUN_DOLT=1` and a bare `go test` — reaches a real
`dolt sql-server`: by default through testcontainers-go where a container
runtime and the pinned image are present (otherwise those suites self-skip),
and from the local `dolt` CLI in the suites that use
`testutil.RequireDoltBinary`. Lanes that set
`BEADS_TEST_REQUIRE_DOLT_CONTAINER=1` fail instead of skipping. Two limitations
of the containerized path are worth recognizing before reading a failure as a
product bug. Neither failure mode manifests in production or on GitHub Actions:
CI exercises this same containerized path green on every risk-tier PR, and
`.github/workflows/pr-risk.yml` pulls the pinned image precisely to defeat the
self-skip. A hang there is a real bug, not this section's subject.

**Migration 0032 hangs over the wire protocol.** Migration `0032`
(`drop_schema_migrations_applied_at`) hangs indefinitely when applied through
the containerized sql-server. How it surfaces depends on the caller's context.
A `go test ./internal/storage/uow/... -count=1` with no `BEADS_TEST_SKIP`
passes `context.Background()`, so it sits in `initSchema` until the package
timeout rather than failing — that is the shape you will normally see, with no
deadline to cut it short. A caller that does supply one gets the hang reported
by the server-mode store open (`newServerMode` in
`internal/storage/dolt/store.go`) instead:

```
failed to initialize schema: context deadline exceeded
```

The cause is the podman-rootless container port-forwarding path, not migration
0032's SQL: the identical statement completes normally through the embedded/CLI
engine, and against a bare-host-process `dolt sql-server` matching the deployed
shape. Evidence chain in `be-j3szz`. Use `BEADS_TEST_SKIP=dolt` unless the
container path is what you are testing.

**Disabling Ryuk removes the container safety net.** This repository sets no
`TESTCONTAINERS_RYUK_DISABLED`, so CI runs with Ryuk — testcontainers-go's
orphan-reaper sidecar — enabled. The podman-rootless flow generally requires
turning it off by hand (`TESTCONTAINERS_RYUK_DISABLED=true`), because under
rootless podman Ryuk frequently cannot start: it wants the runtime socket. See
`be-w3n2m`. With Ryuk off, nothing reaps a container whose test process exited
without running its cleanup — `os.Exit` reached before a deferred
`TerminateDoltContainer`, for instance. `be-5kkk6` records the cost: 101 leaked
containers, and an exhausted swap. When running with Ryuk disabled, keep
teardown on the normal return path using the `testMainInner` pattern
(`beads_test.go`), and check for strays afterwards. Carry the rootless socket
this flow runs on — a bare `docker ps` talks to the CLI's default endpoint and
reports a false all-clear — and read the tag from the pin rather than copying
it, so the command cannot drift when the pin moves. An empty read would be the
same false all-clear (`ancestor=` matches nothing), so the command refuses to
run without a tag:

```bash
dolt_image=$(sed -n 's/.*DoltDockerImage = "\(.*\)".*/\1/p' \
  "$(git rev-parse --show-toplevel)/internal/testutil/testdoltcommon.go")
DOCKER_HOST=unix:///run/user/$(id -u)/podman/podman.sock \
  docker ps -a --filter "ancestor=${dolt_image:?could not read the Dolt image pin}"
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
   checks; do not run the full suite by default.
2. For Go code, keep the focused and affected-package tests green, then run one
   final `make test` (the Bazel test lane), plus the lane of any other tier
   the change touches.
3. Run only the named CI wrapper, specialized target, or risk gate required by
   the changed surface, or the one needed to reproduce a CI result.

For historical CI inventory and maintainer planning context, consult
[CI_TEST_SURFACE_AUDIT.md](CI_TEST_SURFACE_AUDIT.md) and
[CI_CLEANUP_PLAN.md](CI_CLEANUP_PLAN.md). For current commands, use the
workflow files and `Makefile`; these context documents are not a second testing
guide or a live CI inventory.
