---
title: "Phase D1: SQL Common"
description: "SQL plan-cache point select across legacy, before, and final trees."
weight: 7
---

Part of the [hcloud-rebench-20261014](../) campaign.

Three `pkg/relational/sqlbench` benchmarks in the same three trees as
[phase B](../phase-b-recordlayer/), counterbalanced. The phase was meant to show
whether the Record Layer's parity and allocation work is visible through the
SQL layer. **It stopped early on a false correctness failure in the campaign
runner** (see [Limits](#limits)); only `PlanCacheHit` has a complete design
and is analyzed.

## Arms

| Arm | Source | What it contains |
|---|---|---|
| legacy | `9a9a448670a748e4ea721772ba96d70ccc4210a0` | The tree behind the published 2026-10-12 numbers |
| before | `3824f14654edae8431b0085de8ebf83f5a460a08` | legacy plus the read-path parity change (#888); nothing else in between changes code |
| final | `267a41434a81723b07e7150869d702636a1b91a9` | before plus the allocation work and everything merged since |

All three trees ran byte-identical benchmark source
(`pkg/relational/sqlbench/common_bench_test.go`, harness
`d94b4c61377d173b7c6884fde426a227dab0f0bb`; see
[campaign-harness.md](https://github.com/birdayz/fdb-go/blob/master/docs/benchmarks/campaign-harness.md)), so differences come from the
production code. `final/before` is the allocation work plus later changes;
`final/legacy` is both changes together.

## Environment and design

- The host and CPU groups are those of the
  [phase A client run](../phase-a-client/): Hetzner `ccx43`, `fdbserver` on
  CPUs 2–7, the benchmark process on 8–15 (`GOMAXPROCS=8`) in a network
  namespace reaching the server over a veth pair. As in phase B, the single
  `fdbserver` 7.3.77 process used the `memory` engine, single redundancy.
- Planned design: per case, 6 balanced blocks over the three arms (seed
  641301, every order once). One fresh process per arm and block, running the
  benchmark three times (`-test.count=3`): read benchmarks for 3 s each, the
  write benchmark for exactly 1,000 operations. That is 54 processes and 162
  samples. The cluster's user keys were cleared before every process.
- Executed: only `PlanCacheHit` completed all 18 processes, giving 18 samples
  per arm and 54 in total. The other two cases have no ratios; see
  [Limits](#limits).
- Estimand: the geometric mean over blocks of each block's arm ratio, using
  each process's median of its three repeats, with a nominal 95% percentile
  bootstrap over blocks (20,000 resamples, seed 391706). Absolute values are
  the median over blocks of those process medians.
- `ns/op` is testing.B's mean time per operation, not a percentile; `B/op` and
  `allocs/op` are Go heap allocations per operation. `prefix-B` is the length
  in bytes of the store's directory-layer key prefix, reported by the
  benchmark so the analyzer can flag runs whose key lengths differ between
  arms (`physical_prefix_confound`).
- Workloads, each through `database/sql` on one pinned connection:
  - `PlanCacheHit`: a fresh database with a 10-row table; per operation, the
    primary-key point select `SELECT item_id, name, price FROM Item WHERE
    item_id = 1` with the plan cache already warm (parse, cache lookup, one
    record read). The result is checked with the timer stopped.
  - `IndexScanRange` (not analyzed): `SELECT id, name FROM Products WHERE
    price > 50` over 100 rows through a price index, 50 rows per operation.
  - `TimestampInsert` (not run): one `INSERT` per operation into a table with
    an index on a `TIMESTAMP` column, all rows read back and checked after the
    loop.

## Results

<figure class="bench-figure">
<figcaption><strong>Figure D1.1: SQL FDB_PlanCacheHit Ratios (log₂ scale, 95% CI)</strong> — 6 paired blocks × 3 repeats (54 samples). Below 1.0× favors Final.</figcaption>
<svg class="bench-svg" viewBox="0 0 890 266" role="img" aria-label="Figure D1.1: SQL FDB_PlanCacheHit Ratios (log₂ scale, 95% CI)">
<line class="grid-line" x1="230.0" y1="28" x2="230.0" y2="236"/>
<text class="axis-text" x="230.0" y="22" text-anchor="middle">0.9×</text>
<text class="axis-text" x="230.0" y="251" text-anchor="middle">0.9×</text>
<line class="grid-line" x1="359.3" y1="28" x2="359.3" y2="236"/>
<text class="axis-text" x="359.3" y="22" text-anchor="middle">0.95×</text>
<text class="axis-text" x="359.3" y="251" text-anchor="middle">0.95×</text>
<line class="parity-line" x1="482.0" y1="28" x2="482.0" y2="236"/>
<text class="axis-text" x="482.0" y="22" text-anchor="middle">1.0×</text>
<text class="axis-text" x="482.0" y="251" text-anchor="middle">1.0×</text>
<line class="grid-line" x1="598.7" y1="28" x2="598.7" y2="236"/>
<text class="axis-text" x="598.7" y="22" text-anchor="middle">1.05×</text>
<text class="axis-text" x="598.7" y="251" text-anchor="middle">1.05×</text>
<line class="grid-line" x1="710.0" y1="28" x2="710.0" y2="236"/>
<text class="axis-text" x="710.0" y="22" text-anchor="middle">1.1×</text>
<text class="axis-text" x="710.0" y="251" text-anchor="middle">1.1×</text>
<text class="axis-text" x="474.0" y="9" text-anchor="end">← Final lower</text>
<text class="axis-text" x="490.0" y="9" text-anchor="start">Denominator lower →</text>
<line class="section-divider" x1="8" y1="40" x2="882" y2="40"/>
<text class="section-title" x="10" y="55">Final / Before (PlanCacheHit)</text>
<text class="row-label" x="218" y="79.2" text-anchor="end">ns/op</text>
<g class="ci-neutral"><title>ns/op: 1.002× [0.984, 1.021]</title><line x1="443.4" y1="75.2" x2="531.7" y2="75.2" stroke-width="2.2"/><line x1="443.4" y1="71.2" x2="443.4" y2="79.2" stroke-width="1.8"/><line x1="531.7" y1="71.2" x2="531.7" y2="79.2" stroke-width="1.8"/><circle cx="486.8" cy="75.2" r="3.8"/></g>
<text class="val-text" x="722" y="79.2">1.002× [0.984, 1.021]</text>
<text class="row-label" x="218" y="103.2" text-anchor="end">B/op</text>
<g class="ci-win"><title>B/op: 0.966× [0.966, 0.967]</title><line x1="399.3" y1="99.2" x2="401.8" y2="99.2" stroke-width="2.2"/><line x1="399.3" y1="95.2" x2="399.3" y2="103.2" stroke-width="1.8"/><line x1="401.8" y1="95.2" x2="401.8" y2="103.2" stroke-width="1.8"/><circle cx="399.3" cy="99.2" r="3.8"/></g>
<text class="val-text" x="722" y="103.2">0.966× [0.966, 0.967]</text>
<text class="row-label" x="218" y="127.2" text-anchor="end">allocs/op</text>
<g class="ci-win"><title>allocs/op: 0.949× [0.949, 0.949]</title><line x1="356.8" y1="123.2" x2="356.8" y2="123.2" stroke-width="2.2"/><line x1="356.8" y1="119.2" x2="356.8" y2="127.2" stroke-width="1.8"/><line x1="356.8" y1="119.2" x2="356.8" y2="127.2" stroke-width="1.8"/><circle cx="356.8" cy="123.2" r="3.8"/></g>
<text class="val-text" x="722" y="127.2">0.949× [0.949, 0.949]</text>
<line class="section-divider" x1="8" y1="138" x2="882" y2="138"/>
<text class="section-title" x="10" y="153">Final / Legacy (PlanCacheHit)</text>
<text class="row-label" x="218" y="177.2" text-anchor="end">ns/op</text>
<g class="ci-neutral"><title>ns/op: 0.999× [0.981, 1.018]</title><line x1="436.1" y1="173.2" x2="524.7" y2="173.2" stroke-width="2.2"/><line x1="436.1" y1="169.2" x2="436.1" y2="177.2" stroke-width="1.8"/><line x1="524.7" y1="169.2" x2="524.7" y2="177.2" stroke-width="1.8"/><circle cx="479.6" cy="173.2" r="3.8"/></g>
<text class="val-text" x="722" y="177.2">0.999× [0.981, 1.018]</text>
<text class="row-label" x="218" y="201.2" text-anchor="end">B/op</text>
<g class="ci-win"><title>B/op: 0.966× [0.966, 0.966]</title><line x1="399.3" y1="197.2" x2="399.3" y2="197.2" stroke-width="2.2"/><line x1="399.3" y1="193.2" x2="399.3" y2="201.2" stroke-width="1.8"/><line x1="399.3" y1="193.2" x2="399.3" y2="201.2" stroke-width="1.8"/><circle cx="399.3" cy="197.2" r="3.8"/></g>
<text class="val-text" x="722" y="201.2">0.966× [0.966, 0.966]</text>
<text class="row-label" x="218" y="225.2" text-anchor="end">allocs/op</text>
<g class="ci-win"><title>allocs/op: 0.949× [0.949, 0.949]</title><line x1="356.8" y1="221.2" x2="356.8" y2="221.2" stroke-width="2.2"/><line x1="356.8" y1="217.2" x2="356.8" y2="225.2" stroke-width="1.8"/><line x1="356.8" y1="217.2" x2="356.8" y2="225.2" stroke-width="1.8"/><circle cx="356.8" cy="221.2" r="3.8"/></g>
<text class="val-text" x="722" y="225.2">0.949× [0.949, 0.949]</text>
</svg>
<div class="bench-legend"><span><i class="bench-swatch" style="background:#0d9488"></i> 95% CI entirely &lt; 1.000 (faster / lower)</span><span><i class="bench-swatch" style="background:#64748b"></i> 95% CI overlaps 1.000 (unresolved)</span><span><i class="bench-swatch" style="background:#d97706"></i> 95% CI entirely &gt; 1.000 (slower / higher)</span></div>
</figure>

### Final vs before: the allocation work

Ratio final/before, nominal 95% interval.

| Case | ns/op | B/op | allocs/op |
|---|---:|---:|---:|
| PlanCacheHit | 1.002 [0.984, 1.021] | 0.966 [0.966, 0.967] | 0.949 [0.949, 0.949] |

### Final vs legacy: both changes together

Ratio final/legacy, nominal 95% interval. The legacy tree predates the
read-path parity change.

| Case | ns/op | B/op | allocs/op |
|---|---:|---:|---:|
| PlanCacheHit | 0.999 [0.981, 1.018] | 0.966 [0.966, 0.966] | 0.949 [0.949, 0.949] |

### Absolute values

`PlanCacheHit`, median over blocks of each process's median of its three
repeats.

| Arm | ns/op | B/op | allocs/op | prefix-B | Samples / blocks |
|---|---:|---:|---:|---:|---:|
| legacy | 1.148 ms | 48,249.5 | 687 | 6 | 18 / 6 |
| before | 1.144 ms | 48,245.5 | 687 | 6 | 18 / 6 |
| final | 1.153 ms | 46,615 | 652 | 6 | 18 / 6 |

`IndexScanRange` and `TimestampInsert`: N/A (see [Limits](#limits)).

## Interpretation

Measured:

- **Time did not change measurably.** Both `ns/op` intervals include 1
  (final/before 1.002 [0.984, 1.021], final/legacy 0.999 [0.981, 1.018]); a
  change larger than about 2% in either direction is excluded at the nominal
  level. Per-block ratios range 0.970–1.037 (final/before) and 0.963–1.036
  (final/legacy); leave-one-block-out estimates stay within 0.992–1.009.
- **The allocation work reduced allocations by a few percent.** From before
  to final, `allocs/op` fell 5.1% (687 → 652) and `B/op` 3.4%. The same
  ratios hold against legacy because legacy and before allocate the same
  (687 `allocs/op` each, `B/op` within 4 bytes).
- **The parity change is not visible here.** legacy and before differ by
  0.3% in median time (1.148 vs 1.144 ms) and not at all in `allocs/op`; this
  is a ratio of medians, not a paired estimate with an interval.
- **No key-length confound.** `prefix-B` was 6 bytes in every included sample
  of every arm, and `physical_prefix_confound` is false for every comparison.

Hypotheses (not tested):

- The point select costs about 1.15 ms per operation, against an allocation
  saving of about 1.6 kB and 35 allocations. Time is plausibly dominated by
  transaction round trips and server work over the veth path, so a 5%
  allocation reduction would not show in `ns/op`. This run did not profile
  it.
- In phase B, `LoadRecord` is 14% slower in final than in legacy, a step
  that falls on the parity change; this primary-key select shows no such
  step. A plausible reason is that the SQL point lookup
  reaches the record through a path the parity change does not alter, or that
  the parity cost is small next to the SQL layer's own per-query work; neither
  was measured.
- Whether the index scan and timestamp insert show the parity or allocation
  effects is unknown: no ratios exist for them.

## Limits

- Same single host and veth path as phase A, one `fdbserver` process, single
  redundancy, `memory` engine: no replication, no network hop between storage
  servers, no disk. Each benchmark runs one operation at a time.
- testing.B reports means; there are no latency percentiles here.
- Six blocks give limited resolution; intervals are nominal, per metric, with
  no multiplicity correction. Order diagnostics for final/legacy `ns/op`:
  0.988 in blocks where legacy ran first, 1.010 where final ran first, both
  inside the overall interval; for final/before 1.004 and 1.001.
- **This is a complete-case analysis of an aborted phase.** The runner
  executed 21 of the 54 planned processes, in plan order:
  - `PlanCacheHit`, all 6 blocks × 3 arms: 18 processes, all completed. These
    are the 54 analyzed samples.
  - `IndexScanRange`, block 0 only. The before process passed and produced 3
    samples. The legacy and final processes then passed in Go (`PASS`, exit
    code 0, every measured result row reporting a 6-byte prefix) but the
    runner marked them correctness failures: in the third repeat, the
    benchmark's `b.N` calibration invocation created its fresh database under
    a 5-byte directory-layer prefix, and the runner compared every result row
    with every audit receipt instead of with the receipt of the invocation
    that was measured. The final arm's failure stopped the phase with
    `CorrectnessFailure('SQL physical prefix differs between metrics and
    audits')`.
  - `TimestampInsert`: never executed.
- The campaign's analysis report marks `IndexScanRange` N/A as a parser
  defect, not a production SQL correctness failure, and `TimestampInsert` N/A
  as unexecuted. The 3 `IndexScanRange` samples from the before arm remain in
  the original metrics and raw logs, outside all ratios; a single arm in one
  block cannot form a ratio.
- From the runner code: the omission it recorded after the legacy failure was
  keyed by the same shadowed field name described below rather than by the
  case, so the runner did not skip the remaining `IndexScanRange` jobs; with
  correct keying it would have skipped them and continued to
  `TimestampInsert`.
- The parser defect is fixed: `tools/benchcampaign/singlenode/bench_runner.py`
  now pairs each result row with the receipt of the invocation that produced
  it, and the analyzer treats differing measured prefixes as a
  `physical_prefix_confound` instead of a rejection, with regression tests.
  The phase has not been rerun with the fix.
- Case labels in the aggregate CSV were reconstructed from the pinned plan and
  the unchanged per-run logs, for the same runner bug described in
  [phase B](phase-b-recordlayer.md#limits). All 57 original CSV rows were
  checked against the raw output and the 54 included labels restored in a
  separate working copy; no measurement was repeated or changed, and the
  frozen analyzer was used unmodified.
