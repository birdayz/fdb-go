# Replay hillclimb002 — ordered-only specialization, experimental

## Change and identity

This is the second scratch candidate under root#421's unfrozen hillclimb authorization. It addresses both Medium findings on001: full-log sorting of unordered duplicate-heavy input, and a process-global allocation-count test. The baseline remains the exact read-order WIP export5dc3a6fa61806b8ab5ecbcb4dd4c94bd9e279b894d8293461517b316635c5b17 (757files), not the earlier ownership version or the moving root worktree. Known baseline watch-view/conflict and COW correctness fixes were still pending at authorization. This is not promotion-ready merely because timing/byte checks passed.

Full patch `/tmp/astra-hillclimb-replay-002/replay-002.patch` SHA256 `b313b48ba4ce1984c6102c9bb1449149d3dd5174c04d225126ae524da7540c97`; delta from001 `7e613bbd2be664556160ebb7ba34c9f7bd0d9c858390cb52d6a5e251eede1215`; candidate manifest `ef374bd348fc4d9539c6929e27dc78cb8622bf7436e29490f84ad8ecbf7e98f8` (all757files checked).

The fast path now accepts **only already nondecreasing-byte-key snapshots of plain Sets**. It immediately falls back to the unchanged RYW replay at the first non-Set or descending key. First pass counts distinct keys; second pass retains the last equal key in an output vector sized to distinct keys, so a long hot-key overwrite log does not allocate a full-length vector. O(n) work, O(distinct) vector storage, no sort. Operand bytes remain borrowed from the immutable owned snapshot through the existing execution lease until unchanged captureCommit copies them. No live RYW state, validation, conflict, versionstamp gate or transaction-size logic changed.

Tests now use a parallel deterministic structural sentinel: fresh result vector with identical owned key/value backing pointers (old replay copies them, so reverting dispatch should fail);10,000 identical keys require result cap1; explicit ordered duplicates/binary keys/append-outside-snapshot cases;500sorted and500arbitrary-order seeded cases versus old replay plus size equality; mixed rejection with otherwise ordered input. No AllocsPerRun remains. These main-module tests still require root's actual Bazel, revert and race evidence; remote measurer did not run main-module tests. Standalone harness15/15 actualPASS on remoteGo1.26.9 is separate.

Codehealth reviewer ACK002 with no Medium-or-higher findings; low notes and remaining control requests are preserved in REVIEW-STATUS.md. FDB C++ review response pending at report creation. No active or ready worktree edited by measurer.

Binaries:
- baseline `candidate-hc01-base`: `a5beceafe01412f92adc4e4e335a63047bfb68918ec5924e4815a7f88ec21e14`
- candidate `candidate-hc02-replay`: `7874fe840ed2aa90cd0b2ae669683f24a5e35dab6d555f5e4586b93c9eb37f18`
- native `candidate-v6-cgo`: `cd762ea429647784378270613f404573ba598a5049f422e055a887f7746ecbdb`

## Batch retest

36/36 arms passed all raw/persisted/gate checks,09:52:18–09:54:31UTC,2026-10-10. Same frozenV6 harness and options,100measured40warmupworkers1; six blocks/all six arm orders per workload,unique68Bprefixes/88Bkeys. No retries/errors/cachehits. Each Go arm/workload600GRV and600started/completed commits. Native internal counters unobserved; CGo GoMemStats excludes C++ heap.

Geometric means of six paired run-level ratios (lower better):

|Workload|Comparison|Mean|p50|p99|CPU/tx|Go B/tx|Go allocs/tx|
|---|---|---:|---:|---:|---:|---:|---:|
|50×100B|002/base|0.990379|0.984739|1.038857|0.858633|0.756208|0.652882|
|50×1000B|002/base|1.008125|1.036663|0.913335|0.960108|0.788678|0.652899|
|50×100B|002/native|1.017579|0.999164|1.028369|1.208517|not comparable totalheap|not comparable totalheap|
|50×1000B|002/native|0.999159|1.022571|0.906073|1.099630|not comparable totalheap|not comparable totalheap|

CPU improved6/6 small and5/6 large pairs; bytes/allocations improved6/6 each. This is a repeatable resource improvement on the two measured ordered workloads, **not a latency or globalnative win**. Mean ratios span0.943789–1.046128 small and0.950990–1.121900 large. Small p99 worse4/6, large p50 worse5/6. No cross-campaign001-vs002 speedup is inferred; they were not directly paired against each other.

Run-median resources base→002:
- Small CPU1,186.415→1,027.870µs/tx;B113,760.56→86,127.96;allocs465.79→304.17; measured GC totals33→28.
- Large CPU1,337.555→1,273.075µs/tx;B350,485.28→275,637.08;allocs464.595→303.18; GC12→8.

## Longer flagged controls

24/24 control arms passed09:54:31–09:56:29UTC. This intentionally follows001's positive range-CPU and set-latency signals rather than dismissing them. Baseline/002 only, six paired blocks (3AB/3BA), **1,000 measured40warmup** per arm, range100 and set100. Source/binary options fixed, unique68Bprefixes. All checks passed, errors/retries/cachehits0. Each arm/workload6,000GRVs; set1006,000write commits,range1000write commits.

|Control|Mean002/base|p50|p99|CPU/op|Go B/op|Go allocs/op|
|---|---:|---:|---:|---:|---:|---:|
|range100|1.014432|1.016071|1.003662|1.013577|0.999498|0.999530|
|set100|1.005558|1.054809|0.889126|0.987439|0.861283|0.919670|

Range CPU remains positive in4/6 pairs, range0.996123–1.032911. Mean range ratio0.996896–1.032097. **This does not establish no regression.** Its magnitude is smaller than001's short-control signal, but the candidates and budgets differ; do not call that a causal resolution. Set mean spans0.755290–1.497390 (3/6 wins) and p50 remains sensitive to one large pair, so no unconditional latency assurance. Single-Set byte/alloc savings are6/6 consistent. Range byte savings are tiny (~187B/op median), one fewer allocation/op, not evidence that all range cost improved.

Run medians:range CPU2,273.715→2,319.4315µs/op, B331,697.348→331,510.664; GC totals1082→1083. Set CPU621.5395→600.2055µs/op, B9,003.264→7,760.504; GC25→23. Do not compare these absolute1,000-op values to100-op controls without accounting for GC/fixture/regime differences. Read/get controls were performed on001, not repeated on002; do not silently attribute those results to002.

## Remaining acceptance work

- Root main-module tests, deterministic revert proof, race/differential/fuzz and final-source review. Known base correctness work must be integrated correctly.
- Close reviewer low notes on new subtest parallelism when integrating, with hashes kept distinct from this measured candidate.
- Price unordered hot-key and mixed early-exit fallback inputs before broad no-regression claims. The asymptotic sorting/full-log-storage regression was removed structurally, but fallback pre-scan overhead has not been benchmarked.
- Investigate/quantify the residual positive range-control CPU/mean signal rather than claiming noninferiority from six adaptive pairs.
- Next measured allocation target is the remaining owned-arena churn, not another removal of required ownership guarantees.001's actual heap profiles identify the32KiB pool acquisition and64KiB growth separately; no002heap/CPU profile was run, so those profile attributions remain scoped to001.

Both short campaigns used the unchanged runner/analyzer, Go1.26.9/FDB7.3.77API730, clientsCPUs8–15/GOMAXPROCS8/server2–7/collector0–1,2s cooldown+1s per-CPU/group gates,1Hztelemetry. No overlapping remote work or resource lifecycle operations. All exploratory run-level ratios, no confirmatory/noninferiority/capacity/global-win assertion. Prior baselines,001source/binaries/results and every002attempt remain preserved.
