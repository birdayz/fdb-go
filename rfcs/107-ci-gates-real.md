# RFC-107: Make the CI gates real — stress in a workflow, fuzz the client, race-gate the client

**Status:** IMPLEMENTED — Torvalds ACK + codex clean on HEAD `b1779f49`. RFC accepted r2
(scoped PR race; latency-as-trend not a gate; query-generated stress labels; Bazel-native fuzzing;
Docker/cgo gating; no-op guards; +8 diff-oracle reply types). Implementation review (driven by the
new `codexreview` tool — codex no longer hangs) caught and fixed two real silent-pass footguns:
(1) every FDB-driving gate now runs a `docker info` preflight — the stress/race/coverage tests
otherwise `t.Skip` with `FDB not available (no Docker)` and exit 0, silently passing with ZERO
coverage; (2) report/upload steps are gated on `steps.<id>.outcome != 'skipped'` so a preflight
abort can't truncate + publish an empty report that masks the failure. CI/infra item — no
query-engine or client/wire behaviour change (no Graefe / FDB-C++ gate); every bazel command
validated locally (scoped client `-race` passes; Bazel-native fuzz runs on pure + container targets;
the stress query resolves). diff-fuzz needs no preflight (serialization-only vs the oracle binary).
**Item:** Client launch-readiness #4 (TODO.md) — TODO-production P1.6.

## Problem — three load-bearing test suites exist but never gate anything

The repo HAS the hard tests; CI just doesn't run them. Three concrete holes (verified against
`.github/workflows/{ci,nightly-fuzz}.yml`, `.bazelrc`, and the target list):

1. **The 1M stress test runs in NO workflow.** `//pkg/relational/sqldriver/stress:stress_test`
   carries the `stress` tag, and EVERY workflow filters it out (`--test_tag_filters=-stress` in
   `ci.yml:41`; the nightly jobs never name it). So the planner/cost-model/executor regression net
   that the CLAUDE.md "stress comparison workflow" depends on (point lookups <5ms, full scans
   ~3s/1M, index equality <10ms) is never exercised in CI. A cost-model regression ships green.

2. **The 23 `pkg/fdbgo` fuzz targets get ZERO fuzz time.** The from-scratch Go client has 23
   `Fuzz*` targets — `pkg/fdbgo/client` (10, incl. RYW + parse), `pkg/fdbgo/wire/types` (7,
   marshal), `pkg/fdbgo/wire` (2, reader), `pkg/fdbgo/bench` (3, incl. `FuzzDifferential*` — the
   byte-level Go-vs-`libfdb_c` oracle), `pkg/fdbgo/fdb/tuple` (1). `nightly-fuzz.yml` only fuzzes
   the **9** `cmd/fdb-diff-oracle` request types — NOT one of the 23. The client's wire-encode /
   RYW / parse paths — the exact code where a one-byte divergence corrupts a shared FDB cluster —
   get only their seed corpus in the normal `go test`, never real fuzzing.

3. **`-race` is not a PR gate for the client.** `ci.yml:110` runs `-race` on `//pkg/relational/...`
   only. The client (`//pkg/fdbgo/...`) gets `-race` ONLY in the nightly job, and only on **5**
   hand-named targets (`client_test`, `fdb_test`, `recordlayer_test`, `chaos_test`,
   `conformance_test`) — the pipelined-read / commit-path / transport goroutines are the project's
   highest-concurrency code (the `hadRead` data race that shipped was exactly this class), yet a
   race introduced in a PR isn't caught until the next night, if the nightly even covers that target.

CLAUDE.md is explicit: "A flaky or intermittently-failing test is a REAL BUG"; "Prove with FDB …
integration tests are the gold standard"; "Fuzz is non-negotiable … 200k+ execs should produce 0
panics." The tests embody that. CI doesn't enforce it.

## Proposed change (CI wiring only — no product code change) — r2, addressing Torvalds + codex

1. **Scheduled stress workflow (`nightly-stress.yml`).** A new scheduled workflow (off-peak cron,
   `workflow_dispatch` for manual). The stress target is tagged BOTH `stress` AND `manual`
   (`stress/BUILD.bazel:11`), so a wildcard `//pkg/... --test_tag_filters=stress` can drop it (manual
   targets are excluded from wildcard expansion before the tag filter runs — codex). Resolve the
   targets explicitly via query and FAIL on an empty set (no-op guard):
   ```sh
   STRESS=$(bazelisk query 'attr(tags, stress, tests(//pkg/...))')
   [ -n "$STRESS" ] || { echo "no stress targets resolved — CI gate is a no-op"; exit 1; }
   bazelisk test $STRESS --test_output=streamed --test_arg=--test.v --test_timeout=3600
   ```
   This auto-picks up any future stress target (no rotting label) while still being explicit. It does
   NOT gate PRs (too heavy) but a red scheduled run is a release blocker, surfaced like nightly-fuzz.
   Nightly to start; drop to weekly if it crowds the box. **Gate only on row-count/correctness + hard
   error/crash — NOT on wall-clock latency** (Torvalds: latency on a shared self-hosted runner is
   noisy; a latency-failure gate manufactures false reds, and CLAUDE.md treats every red as a
   must-investigate bug). The per-op latencies ARE parsed and emitted to the GitHub step summary as a
   reported trend (and archived to the Hetzner bucket like the test report) so a real regression is
   visible without flaking the build; an out-of-band alarm on the trend is future work.

2. **Fuzz the 23 client targets nightly (extend `nightly-fuzz.yml`).** Add a `client-fuzz` job that
   fuzzes each `Fuzz*` under `//pkg/fdbgo/...` via the repo's **Bazel-native** invocation (CLAUDE.md),
   NOT plain `go test -fuzz`:
   ```sh
   bazelisk test <owning_target> --test_arg=-test.fuzz='^FuzzXxx$' --test_arg=-test.run='^$' \
     --test_arg=-test.fuzztime=Nm --test_arg=-test.fuzzcachedir=/tmp/fuzz-cache \
     --sandbox_writable_path=/tmp/fuzz-cache --test_timeout=… --nocache_test_results
   ```
   Bazel-native is REQUIRED for faithfulness (codex): plain `go test` ignores Bazel `data`/`env` and
   the `go_deps.module_override` that patches the Apple FDB Go binding (`MODULE.bazel:15`) — the
   `pkg/fdbgo/bench` `FuzzDifferential*` targets compare Go vs the cgo `libfdb_c`, so they MUST run
   against the Bazel-built binding, not `go.mod`'s raw module. `-test.run='^$'` runs the fuzz target
   only, not the package's unit tests first (codex). Per-target owning package (Bazel test targets
   are per-package, so this is natural). The targets are DISCOVERED, not hard-listed, and the job
   fails if discovery yields zero (no-op guard): `grep -rl '^func Fuzz' pkg/fdbgo/ | …` → bazel labels.
   **Docker/cgo gating is mandatory, not optional** (codex): `pkg/fdbgo/client` `TestMain`
   (`testmain_test.go:94`) and `pkg/fdbgo/bench` (`bench_test.go:29,96`) always start
   FDB-testcontainers (+ cgo for bench), so the job documents and checks the Docker + `libfdb_c`
   requirement up front. Budget: start 5m each; the pure wire/type/tuple targets (10) are fast, the
   container-backed client/bench (13) dominate — shard across jobs if the total overruns the timeout.
   On a crash / new corpus entry: fail and upload `pkg/fdbgo/**/testdata/fuzz/**` (the minimized seed)
   as an artifact so it reproduces (`-test.run=FuzzXxx/<hash>`).
   **Also close the diff-oracle gap (Torvalds):** the existing differential job loops only 9 of the
   17 `cmd/fdb-diff-oracle` `Fuzz*` funcs — extend it to the 8 unfuzzed REPLY-parse types.

3. **Add the CLIENT to the PR `-race` job (`ci.yml`), scoped (Torvalds + codex).** Extend the `race`
   job to also run the **concurrency-bearing, non-Docker-cgo** client packages on every PR:
   ```sh
   bazelisk test //pkg/fdbgo/client/... //pkg/fdbgo/transport/... //pkg/fdbgo/fdb/... \
     --@rules_go//go/config:race --test_tag_filters=-stress --test_output=errors
   ```
   This is the pipelined-read / commit-path / transport goroutine code (the `hadRead` race class) —
   the SAME gate the SQL layer already is. The FULL `//pkg/fdbgo/...` is deliberately NOT the default:
   it pulls in `bench` (cgo/`libfdb_c`) and `conformance` (Docker) tests that, race-instrumented
   (2-10×) under `--local_test_jobs=4` on the single Hetzner runner, would dominate PR latency and
   serialize behind the existing relational race job (codex/Torvalds). Expand the scope only with
   measured numbers. A nonzero-target guard (`bazelisk query 'tests(...)'` non-empty) prevents a typo
   silently shrinking the gate to nothing.

## Test plan (CI can't be unit-tested — validate the underlying commands + YAML)

- **The bazel commands are load-bearing, the YAML is glue.** Each command is run LOCALLY and shown
  green before committing: the `attr(tags, stress, …)` query resolves the stress target and the
  explicit-label `bazelisk test` runs it; the scoped race set
  `//pkg/fdbgo/client/... //pkg/fdbgo/transport/... //pkg/fdbgo/fdb/... --@rules_go//go/config:race`
  builds + passes under the race detector; a representative **Bazel-native** fuzz run
  (`bazelisk test //pkg/fdbgo/wire/types:types_test --test_arg=-test.fuzz='^FuzzXxx$'
  --test_arg=-test.fuzztime=30s --test_arg=-test.fuzzcachedir=/tmp/fuzz-cache
  --sandbox_writable_path=/tmp/fuzz-cache`) executes real iterations on a pure target; and a
  container-backed one (e.g. a `pkg/fdbgo/client` fuzz target) runs given Docker. Local green proves
  the CI step does real work.
- **No-op guards (the no-fake-checkbox rule applied to CI itself).** Each job FAILS if its target
  discovery resolves zero: stress (`[ -n "$STRESS" ]`), fuzz (`grep -rl '^func Fuzz' pkg/fdbgo/`
  non-empty → labels), race (`bazelisk query 'tests(<scope>)'` non-empty). Lint the YAML
  (actionlint). The classic "green because it ran nothing" failure (an inverted tag filter, a
  manual-tag drop, a typo in a label) is caught by the guard, not by a passing-but-empty run.
- **Race job actually exercises the client + has teeth.** Confirm the scoped race set resolves >1
  test target (not zero), and revert-prove the gate: back out a known-safe `atomic` in the client to
  a plain field, confirm `--@rules_go//go/config:race` reports a data race locally, then restore.
- **Fuzz faithfulness.** Confirm a `pkg/fdbgo/bench` differential fuzz target run via the Bazel-native
  invocation actually exercises the cgo `libfdb_c` path (the Bazel-patched binding), since plain
  `go test` would not — a short `-test.fuzztime` smoke run that loads the oracle proves it.

## What this does NOT do

- It does not move CI off the single Hetzner runner (reproducibility / bus-factor) — that is the
  separate **#5 / RFC-108**. This RFC assumes the existing self-hosted runner and only adds jobs.
- It does not add NEW fuzz targets or stress scenarios — it runs the ones that already exist. New
  coverage is a product-code concern, out of scope for "make the existing gates real."
- It does not change `.bazelrc` defaults beyond what a job needs (the race config stays opt-in via
  the `--@rules_go//go/config:race` flag, matching the existing relational race job).
