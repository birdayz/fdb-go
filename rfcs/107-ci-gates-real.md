# RFC-107: Make the CI gates real — stress in a workflow, fuzz the client, race-gate the client

**Status:** Draft — for review by **Torvalds + codex** (CI/infra item; no query-engine or
client/wire *behavior* change, so no Graefe / FDB-C++ gate — but the underlying bazel commands
must be the same ones a developer runs locally).
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

## Proposed change (CI wiring only — no product code change)

1. **Scheduled stress workflow (`nightly-stress.yml`).** A new scheduled workflow (off-peak cron,
   `workflow_dispatch` for manual) that runs the stress-tagged target(s) WITH the tag included.
   `attr(tags, stress, //pkg/...)` resolves exactly one today —
   `//pkg/relational/sqldriver/stress:stress_test` — so the job runs
   `bazelisk test //pkg/... --test_tag_filters=stress --test_timeout=3600 --test_output=errors
   --test_arg=--test.v` (tag-filter form, so any future stress-tagged target is picked up
   automatically rather than a hard-coded label rotting).
   Generous `timeout-minutes` (the 1M build + run). It does NOT gate PRs (too heavy / too slow for
   every push) but a red scheduled stress run is a release blocker, surfaced like the nightly-fuzz
   failures. Frequency: nightly to start (cheap on the idle Hetzner box overnight); can drop to
   weekly if it crowds the fuzz/coverage jobs. The job greps the stress output for the per-op
   latency lines and fails if a threshold regresses (the CLAUDE.md thresholds), not just on a hard
   error — a 2× latency regression is a real cost-model bug even when rows are correct.

2. **Fuzz the 23 client targets nightly (extend `nightly-fuzz.yml`).** Add a `client-fuzz` job that
   loops every `Fuzz*` under `//pkg/fdbgo/...`, each `-fuzztime=Nm` (start at 5m each; 23 × 5m ≈ 2h,
   within a generous timeout — or shard across two jobs if it overruns). Use the SAME mechanism the
   diff-oracle job uses: resolve the bazel Go SDK, `go test -fuzz=FuzzXxx -fuzztime=… ./pkg/fdbgo/…`.
   The `FuzzDifferential*` targets need the running differential harness (real FDB via
   testcontainers + the cgo binding) — gate those on Docker being present, exactly like the
   existing differential job. A crash/new-corpus-entry fails the job and uploads the failing input
   as an artifact (so the seed is reproducible: `go test -run=FuzzXxx/<hash>`).

3. **Add `//pkg/fdbgo/...` to the PR `-race` job (`ci.yml`).** Extend the existing `race` job (or add
   a sibling) to run `bazelisk test //pkg/fdbgo/... --@rules_go//go/config:race
   --test_tag_filters=-stress --test_output=errors` on every PR, alongside the existing
   `//pkg/relational/...`. This makes the client's concurrency the SAME gate the SQL layer already
   is. `--local_test_jobs=4` (`.bazelrc:45`) already caps concurrent FDB containers so the race
   build doesn't thrash. If the full `//pkg/fdbgo/...` race run is too slow for a PR, scope it to the
   concurrency-bearing packages (`//pkg/fdbgo/client/... //pkg/fdbgo/transport/... //pkg/fdbgo/fdb/...`)
   and document what's excluded — but DEFAULT to the full set and only trim with measured numbers.

## Test plan (CI can't be unit-tested — validate the underlying commands + YAML)

- **The bazel commands are load-bearing, the YAML is glue.** Each command in the workflows is run
  LOCALLY and shown green before committing: the stress tag-filter actually selects the stress
  target; `//pkg/fdbgo/... --@rules_go//go/config:race --test_tag_filters=-stress` builds + passes
  under the race detector; a representative `go test -fuzz=FuzzXxx -fuzztime=30s ./pkg/fdbgo/wire/types/`
  runs. The local green is the proof the CI step will do real work, not no-op.
- **YAML validity + no-op guard.** Lint the workflow YAML (actionlint or a schema check). Assert the
  tag filter is `stress` (include), not `-stress` (exclude) — an inverted filter is the classic
  "green because it ran nothing" failure; a guard step greps the bazel output for "Executed N tests"
  with N>0 and fails on N==0 (the no-fake-checkbox rule applied to CI itself).
- **Race job actually exercises the client.** Confirm `bazelisk test //pkg/fdbgo/... --@rules_go//go/config:race`
  resolves >1 test target (not zero) and that backing out a known-safe `atomic` to a plain field
  reproduces a race locally (revert-proof that the gate has teeth), then restore.

## What this does NOT do

- It does not move CI off the single Hetzner runner (reproducibility / bus-factor) — that is the
  separate **#5 / RFC-108**. This RFC assumes the existing self-hosted runner and only adds jobs.
- It does not add NEW fuzz targets or stress scenarios — it runs the ones that already exist. New
  coverage is a product-code concern, out of scope for "make the existing gates real."
- It does not change `.bazelrc` defaults beyond what a job needs (the race config stays opt-in via
  the `--@rules_go//go/config:race` flag, matching the existing relational race job).
