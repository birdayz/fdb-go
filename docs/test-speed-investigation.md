# Full-suite (`just test-full`) speed investigation

Measured 2026-10-07 on the 24-thread / 62 GB dev box, branch `investigate/test-speed`
(off `upgrade/java-4.14.2.0` at c359b1ee9), 114 Bazel test targets. The box was
shared with another engineer's Bazel runs throughout, so absolute times carry
noise; the structure below doesn't depend on it.

## 1. Where the wall time goes

The baseline run used a separate output base, so the build was cold. Phases were
reconstructed from the Bazel `--profile` and BEP:

| phase | wall | what bounds it |
|---|---|---|
| build (cold) | ~3.5 min | `fdb_cmake_build` genrule (~7 min CPU, partly overlapped) |
| non-exclusive tests (109 targets) | ~13.3 min | **`--local_test_jobs=4`**: 3,276 s of test time / 4 slots ≈ 13.6 min. `sqldriver_test` (433 s) waited 347 s for a slot. |
| exclusive tail, serial | ~47 min | 5 `exclusive` targets run one at a time after everything else |
| ↳ `client_test` | 172 s | |
| ↳ `million_record_test` | 116 s | |
| ↳ `factory_test` | ~120 s | (TestFactoryDeterminism 94 s) |
| ↳ `factorycorpus/full:full_test` | ~147 s | |
| ↳ **`sqldriver/stress:stress_test`** | **~38 min** | serial benchmarks, see §2 |

**Critical path:** build → 4-slot queue → five exclusive targets in sequence.
The stress target alone is ~60% of the full lane's wall time.

Top non-exclusive targets (attempt time): sqldriver 433 s, rfc257_oracle 379 s,
conformance_probes 316 s, rfc257_guardiann 310 s, recordlayer 260 s,
conformance 219 s, chaos 131 s, embedded 116 s, conformance_corpora 113 s,
memoinvariant 112 s.

## 2. `stress_test` (~38 min, `exclusive`)

Per-test durations. Source: a test.log from the main checkout, the same day:

| test | s | what it asserts |
|---|---|---|
| TestFDB_Stress_StatisticsJoinOrder | **1238** | plan-change win ≥2x, control within 0.5–2x |
| TestFDB_SQLParallelConnections | 293 | row counts only; throughput is **logged** |
| TestFDB_Stress_1M_LatencyAttribution | 211 | full 1M stress suite + telemetry validity (t.Parallel) |
| TestFDB_VectorSearch_ColdStartCapped… | 204 | truncation, heap bounds (t.Parallel, overlaps the above) |
| TestFDB_Stress_1M | 185 | same suite as LatencyAttribution, without telemetry |
| TestFDB_Ingest_Parallelism | 124 | row counts only; throughput logged |
| RawIngestBench, RawReadScaling, SaveRecord* (4) | 114 | nothing beyond row counts; throughput logged |
| Stress_10K / 100K | 18 | |

Causes:

- **StatisticsJoinOrder repeated multi-minute timings 3x.** Each (arrangement,
  setting) was timed 3 times and the minimum kept. The two slow plans take
  82–120 s per run, so the 6 extra repeats cost ~9.6 min. That minimum-of-3
  filters millisecond scheduler noise; it adds nothing on a 100 s run measured
  against 2x bands. **Fixed:** stop repeating once a sample is ≥5 s. The 15 ms
  fast plan still gets 3 samples. Measured: 1238 s → 798 s, on a run where the
  box was more loaded (the a_big OFF plan took 4m05s vs 2m00s before). On an
  equal box the expected figure is ~660 s.
- **~530 s of pure throughput benchmarks** (`SQLParallelConnections`,
  `Ingest_Parallelism`, `RawIngestBench`, `RawReadScaling`, `SaveRecord*`).
  They log rows/s and assert only row counts. Their numbers are only meaningful
  on an idle box, which is the reason the whole target is `exclusive`. *Owner
  decision, §5.*
- **`Stress_1M` and `Stress_1M_LatencyAttribution` load the same 1M-row fixture
  twice** (~160 s each) and run the same `runStressHarness` assertions. The
  latter adds per-query telemetry checks on top. *Owner decision, §5.*

## 3. `sqldriver_test` (433 s wall, ~6,300 CPU-s, 1,760 tests)

All tests are `t.Parallel()` against one shared container, so the target is
CPU-bound: the sum of test durations is 7,515 s and the CPU floor on 24 threads
is ~260 s. The wall time is the longest tests plus contention. The top 10 tests
are 26% of summed test time, the top 45 are 43%.

Causes found, with the fixes applied:

| test | s | cause | change |
|---|---|---|---|
| JoinOrderStatisticsCorrectionRate | 355 | ~14,000 single-row autocommit INSERTs | multi-row INSERTs of 100 (**fixed**) |
| MultiwayJoinOrder_Nway | 83 | 2,220 single-row INSERTs | batched (**fixed**) |
| JoinSelPred_Repro | 54 | 2,100 single-row INSERTs for an EXPLAIN-only test | batched (**fixed**) |
| SelectivityBlindSpotWithCollectedStatistics | 49 | 2,000 single-row INSERTs | batched (**fixed**) |
| MetamorphicExpressionEquivalenceSweep | 97 | 11 of 15 rules use no generated operand, yet re-ran the same SQL 40× (≈1,700 of 2,400 queries) | invariant rules run once, before the loop; the random draw sequence is unchanged (**fixed**) |
| MetamorphicRewriteEquivalenceSweep | 258 | 2 of 17 rules (distinct-vs-group-by, having-vs-derived) are constant, re-run 60× | hoisted (**fixed**) |

Batch size is 100 rows per INSERT, not 500. In an overloaded run, 500-row
statements hit the 5 s transaction limit (40001 / 1007). 100 matches the other
batched fixtures in the package. None of these tests asserts anything about the
insert path; they read only the loaded rows and statistics collected afterwards.

## 4. Scheduling: `--local_test_jobs=4` disables memory pricing

.bazelrc caps tests at 4 concurrent. Commit 79ca2395e (#600) priced 26
container suites with `tags = ["resources:memory:N"]`. It concluded those tags
are "inert for go_test", and made five targets `exclusive` instead. The real
cause is different, measured on Bazel 9.0.1 with four 5 s go_tests tagged
`resources:memory:2000` against `--local_resources=memory=4500`:

| flags | concurrency |
|---|---|
| `--local_test_jobs=4` | 4 (tags ignored) |
| no `--local_test_jobs` | 2 (tags honoured) |

**Any non-zero `--local_test_jobs` makes Bazel ignore `resources:` tags on
tests.** The tags work. Recorded in `.bazelrc`; the wrong claim in
`nightly-coverage.yml` is corrected.

Tried and reverted: dropping `--local_test_jobs=4` and the `exclusive` tags of
the four memory-motivated targets (client, million_record, factory,
factorycorpus/full). Without a count cap, Bazel ran ~22 test targets at once for
13 minutes, because each test asks for only 1 CPU and sqldriver alone uses ~15
cores. Several things failed: Java-oracle HTTP timeouts, 1007s, a SIGKILLed
cmake genrule. sqldriver took 1,131 s instead of 433 s. Memory pricing alone is
not enough on this box; CPU has to be priced too (§5).

## 5. Decisions for the owner

1. **Price CPU, then retire the count cap and the `exclusive` tags.** Add
   `resources:cpu:N` to the heavy multi-core targets, starting with sqldriver,
   rfc257_oracle, conformance_probes, rfc257_guardiann, recordlayer,
   conformance, factorycorpus/full and memoinvariant. Size N from measured
   CPU-seconds per wall-second. Then drop `--local_test_jobs=4` and replace
   `exclusive` on client/million_record/factory/full with the memory tags they
   already carry. Expected on the dev box: the four ~9 min serial targets
   overlap the main phase, and the main phase stops being bound by 4 slots.
   CI's 4-vCPU runners keep ≤4 tests at once through the CPU budget. Their
   memory budget would come from `HOST_RAM*0.6` (~4.6 GB, about the 4,500 MB #600
   computed). This changes CI packing, so it needs a CI run before it lands.
2. **Stress benchmarks out of `test-full`.** The ~530 s of logged-throughput
   benchmarks (§2) assert nothing that a regression would trip. Options:
   convert them to `Benchmark*` functions, or move them to a `stress_bench`
   target that runs only in `nightly-stress.yml`. Either way they leave the
   exclusive serial tail.
3. **Drop `TestFDB_Stress_1M`.** `Stress_1M_LatencyAttribution` runs the same
   `runStressHarness(t, h, 1_000_000)` over an identical fixture with every
   assertion `Stress_1M` makes, plus telemetry validation. The only difference
   is a pinned `Conn` with loggers installed. The plain pooled path stays
   covered at 10K and 100K. Saves ~3 min of exclusive time.
4. **Whether stress belongs in `test-full` at all.** CI's full lane already
   excludes it (`--test_tag_filters=-stress`) and `nightly-stress.yml` runs it.
   AGENTS.md defines test-full as including it. Even after the changes above it
   is the longest exclusive item (~25 min).
5. **Further safe sqldriver de-duplication, not done here** (read-only analysis,
   estimates):
   - MetamorphicOperandCommutation: ~55% of its queries are exact repeats
     because the generated value space is small.
   - Composite-PK paged pass re-reads pass-1 results: ~20%.
   - IndexDifferential: the first partition variant equals `q`: ~20%.
   - CTEBoxUnnestOnResolutionProbe2: ~70 serial read-only subtests that could be
     `t.Parallel()`.
   - CurrentTimestamp seeding: 100-row transactions could be 1,000-row ones.

   Signal-reducing options, all owner calls:
   - RowDiff_Paging: 25 serial seeds, 378 s. The nightly runs 5,000; seeds
     could run in parallel or be fewer.
   - PlannerCapHit_SelectPathSQLSTATE and PlannerOptions_PlanRightDeep: these
     run the planner to the 150k-task cap, and embedded unit tests already pin
     the same cap.
   - DistinctUniqueElisionCostProbe: 9 timed repetitions pinned by RFC-209 §7.
