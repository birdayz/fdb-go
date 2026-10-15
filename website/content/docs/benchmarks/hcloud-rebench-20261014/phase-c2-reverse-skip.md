---
title: "Phase C2: Remote Fetch (Reverse & Skip)"
description: "Reverse scans and executor Skip 3 paths with USE_REMOTE_FETCH vs SCAN_AND_FETCH."
weight: 5
---

Two fetch methods in the final tree, counterbalanced, on paths that
[phase C1](../phase-c1-remotefetch/) did not cover: reverse index scans and a
query plan with a positive `Skip`. `USE_REMOTE_FETCH` asks the storage server to
resolve each index entry to its record (a mapped range read); `SCAN_AND_FETCH`
scans the index and loads each record from the client. Part of the
hcloud-rebench-20261014 campaign; [phase A](../phase-a-client/) describes the
host, [phase B](../phase-b-recordlayer/) the Record Layer baseline.

## Arms

| Arm | Source | Fetch method |
|---|---|---|
| final-ordinary | `267a41434a81723b07e7150869d702636a1b91a9` (final tree) | `SCAN_AND_FETCH` |
| final-remote | `267a41434a81723b07e7150869d702636a1b91a9` | `USE_REMOTE_FETCH` |

Both arms run the same executables and differ only in the method sub-benchmark
they select. The two reverse cases run `fetch-final`, built from
`pkg/recordlayer/benchfetch` (`BenchmarkIndexFetch`, as in C1). The two Skip
cases run `planskip-final`, built from `pkg/recordlayer/benchfetch/planskip`
(`BenchmarkPlanSkip`). That benchmark exists only in the final tree, because it
needs the plan-level fetch method. There is no earlier-tree arm in this phase.
Harness `d94b4c61377d173b7c6884fde426a227dab0f0bb`; see
[campaign-harness.md](https://github.com/birdayz/fdb-go/blob/master/docs/benchmarks/campaign-harness.md).

## Environment and design

- Host, CPU groups and network path as in [phase A](../phase-a-client/) and
  [phase C1](../phase-c1-remotefetch/): Hetzner `ccx43`, one `fdbserver` 7.3.77
  process on CPUs 2–7 with the `memory` engine and single redundancy, and the
  benchmark process on CPUs 8–15 in a network namespace reaching the server
  over a veth pair.
- Cases:
  - `reverse-small-1000`, `reverse-split-1000`: `BenchmarkIndexFetch`,
    `cell=rows1000`, `dir=reverse`. A store-level scan of all 1,000 records of
    the value index in reverse order, drained. `small` records are one
    key-value each; `split` records carry a 100,000-byte payload stored as two
    chunks.
  - `executor-skip3-small-10`, `executor-skip3-small-1000`:
    `BenchmarkPlanSkip`, forward. One query index plan executed with
    `ExecuteProperties.Skip = 3` over small records. The first selects prices
    100–112 and returns 10 rows; the second covers the whole index and returns
    997.
- Correctness and path checks, outside the timer: every measured operation's
  rows are checked (exact IDs and order, fields, terminal reason). An
  instrumented operation before and after each measured loop asserts the path.
  For the Skip cases this includes the Skip placement: `USE_REMOTE_FETCH` makes
  one remote-fetch opening whose entries include the three skipped ones, and
  `SCAN_AND_FETCH` skips inside its index cursor, with exactly one record load
  per returned row. Each process also reports `rows/op` and the store prefix
  length in bytes (`prefix-B`). Both are identical between the arms in every
  case: ratio 1.000 [1.000, 1.000].
- Per case: 6 blocks over the two arms, each order three times
  (preregistered, seed 641401). One fresh process per arm and block, three
  repeats each (`-test.count=3`, 3 s per repeat). That gives 18 samples per arm
  and case, 48 processes and 144 samples in total. All 48 processes completed
  with status `ok`; none was omitted or rerun.
- Estimand and bootstrap as in phases B and C1: the geometric mean over blocks
  of each block's ratio of process medians, with a nominal 95% percentile
  bootstrap over blocks (20,000 resamples, seed 391706). Absolute values are
  the median over blocks of the process medians. `ns/op` is testing.B's mean,
  not a percentile.

## Results

<figure class="bench-figure">
<figcaption><strong>Figure C2.1: Remote Fetch vs Ordinary Scan Ratios (log₂ scale, 95% CI)</strong> — 6 paired blocks × 3 repeats on single-node memory engine. Below 1.0× favors USE_REMOTE_FETCH.</figcaption>
<svg class="bench-svg" viewBox="0 0 892 436" role="img" aria-label="Figure C2.1: Remote Fetch vs Ordinary Scan Ratios (log₂ scale, 95% CI)">
<line class="grid-line" x1="232.0" y1="28" x2="232.0" y2="406"/>
<text class="axis-text" x="232.0" y="22" text-anchor="middle">0.2×</text>
<text class="axis-text" x="232.0" y="421" text-anchor="middle">0.2×</text>
<line class="grid-line" x1="375.5" y1="28" x2="375.5" y2="406"/>
<text class="axis-text" x="375.5" y="22" text-anchor="middle">0.35×</text>
<text class="axis-text" x="375.5" y="421" text-anchor="middle">0.35×</text>
<line class="grid-line" x1="467.0" y1="28" x2="467.0" y2="406"/>
<text class="axis-text" x="467.0" y="22" text-anchor="middle">0.5×</text>
<text class="axis-text" x="467.0" y="421" text-anchor="middle">0.5×</text>
<line class="grid-line" x1="553.3" y1="28" x2="553.3" y2="406"/>
<text class="axis-text" x="553.3" y="22" text-anchor="middle">0.7×</text>
<text class="axis-text" x="553.3" y="421" text-anchor="middle">0.7×</text>
<line class="parity-line" x1="644.7" y1="28" x2="644.7" y2="406"/>
<text class="axis-text" x="644.7" y="22" text-anchor="middle">1.0×</text>
<text class="axis-text" x="644.7" y="421" text-anchor="middle">1.0×</text>
<line class="grid-line" x1="712.0" y1="28" x2="712.0" y2="406"/>
<text class="axis-text" x="712.0" y="22" text-anchor="middle">1.3×</text>
<text class="axis-text" x="712.0" y="421" text-anchor="middle">1.3×</text>
<text class="axis-text" x="636.7" y="9" text-anchor="end">← USE_REMOTE_FETCH lower</text>
<text class="axis-text" x="652.7" y="9" text-anchor="start">SCAN_AND_FETCH lower →</text>
<line class="section-divider" x1="8" y1="40" x2="884" y2="40"/>
<text class="section-title" x="10" y="55">Mean Time (ns/op): USE_REMOTE_FETCH / SCAN_AND_FETCH</text>
<text class="row-label" x="220" y="79.2" text-anchor="end">reverse-small-1000</text>
<g class="ci-win"><title>reverse-small-1000: 0.337× [0.331, 0.343]</title><line x1="361.2" y1="75.2" x2="370.3" y2="75.2" stroke-width="2.2"/><line x1="361.2" y1="71.2" x2="361.2" y2="79.2" stroke-width="1.8"/><line x1="370.3" y1="71.2" x2="370.3" y2="79.2" stroke-width="1.8"/><circle cx="365.8" cy="75.2" r="3.8"/></g>
<text class="val-text" x="724" y="79.2">0.337× [0.331, 0.343]</text>
<text class="row-label" x="220" y="103.2" text-anchor="end">reverse-split-1000</text>
<g class="ci-reg"><title>reverse-split-1000: 1.146× [1.130, 1.161]</title><line x1="676.1" y1="99.2" x2="683.0" y2="99.2" stroke-width="2.2"/><line x1="676.1" y1="95.2" x2="676.1" y2="103.2" stroke-width="1.8"/><line x1="683.0" y1="95.2" x2="683.0" y2="103.2" stroke-width="1.8"/><circle cx="679.7" cy="99.2" r="3.8"/></g>
<text class="val-text" x="724" y="103.2">1.146× [1.130, 1.161]</text>
<text class="row-label" x="220" y="127.2" text-anchor="end">executor-skip3-small-10</text>
<g class="ci-win"><title>executor-skip3-small-10: 0.783× [0.764, 0.800]</title><line x1="575.7" y1="123.2" x2="587.5" y2="123.2" stroke-width="2.2"/><line x1="575.7" y1="119.2" x2="575.7" y2="127.2" stroke-width="1.8"/><line x1="587.5" y1="119.2" x2="587.5" y2="127.2" stroke-width="1.8"/><circle cx="582.0" cy="123.2" r="3.8"/></g>
<text class="val-text" x="724" y="127.2">0.783× [0.764, 0.800]</text>
<text class="row-label" x="220" y="151.2" text-anchor="end">executor-skip3-small-1000</text>
<g class="ci-win"><title>executor-skip3-small-1000: 0.455× [0.445, 0.465]</title><line x1="437.1" y1="147.2" x2="448.4" y2="147.2" stroke-width="2.2"/><line x1="437.1" y1="143.2" x2="437.1" y2="151.2" stroke-width="1.8"/><line x1="448.4" y1="143.2" x2="448.4" y2="151.2" stroke-width="1.8"/><circle cx="442.8" cy="147.2" r="3.8"/></g>
<text class="val-text" x="724" y="151.2">0.455× [0.445, 0.465]</text>
<line class="section-divider" x1="8" y1="162" x2="884" y2="162"/>
<text class="section-title" x="10" y="177">Go Bytes (B/op): USE_REMOTE_FETCH / SCAN_AND_FETCH</text>
<text class="row-label" x="220" y="201.2" text-anchor="end">reverse-small-1000</text>
<g class="ci-win"><title>reverse-small-1000: 0.506× [0.505, 0.506]</title><line x1="469.5" y1="197.2" x2="470.0" y2="197.2" stroke-width="2.2"/><line x1="469.5" y1="193.2" x2="469.5" y2="201.2" stroke-width="1.8"/><line x1="470.0" y1="193.2" x2="470.0" y2="201.2" stroke-width="1.8"/><circle cx="470.0" cy="197.2" r="3.8"/></g>
<text class="val-text" x="724" y="201.2">0.506× [0.505, 0.506]</text>
<text class="row-label" x="220" y="225.2" text-anchor="end">reverse-split-1000</text>
<g class="ci-win"><title>reverse-split-1000: 0.692× [0.691, 0.692]</title><line x1="549.9" y1="221.2" x2="550.3" y2="221.2" stroke-width="2.2"/><line x1="549.9" y1="217.2" x2="549.9" y2="225.2" stroke-width="1.8"/><line x1="550.3" y1="217.2" x2="550.3" y2="225.2" stroke-width="1.8"/><circle cx="550.3" cy="221.2" r="3.8"/></g>
<text class="val-text" x="724" y="225.2">0.692× [0.691, 0.692]</text>
<text class="row-label" x="220" y="249.2" text-anchor="end">executor-skip3-small-10</text>
<g class="ci-win"><title>executor-skip3-small-10: 0.684× [0.684, 0.684]</title><line x1="547.3" y1="245.2" x2="547.3" y2="245.2" stroke-width="2.2"/><line x1="547.3" y1="241.2" x2="547.3" y2="249.2" stroke-width="1.8"/><line x1="547.3" y1="241.2" x2="547.3" y2="249.2" stroke-width="1.8"/><circle cx="547.3" cy="245.2" r="3.8"/></g>
<text class="val-text" x="724" y="249.2">0.684× [0.684, 0.684]</text>
<text class="row-label" x="220" y="273.2" text-anchor="end">executor-skip3-small-1000</text>
<g class="ci-win"><title>executor-skip3-small-1000: 0.580× [0.580, 0.580]</title><line x1="505.0" y1="269.2" x2="505.0" y2="269.2" stroke-width="2.2"/><line x1="505.0" y1="265.2" x2="505.0" y2="273.2" stroke-width="1.8"/><line x1="505.0" y1="265.2" x2="505.0" y2="273.2" stroke-width="1.8"/><circle cx="505.0" cy="269.2" r="3.8"/></g>
<text class="val-text" x="724" y="273.2">0.580× [0.580, 0.580]</text>
<line class="section-divider" x1="8" y1="284" x2="884" y2="284"/>
<text class="section-title" x="10" y="299">Go Allocations (allocs/op): USE_REMOTE_FETCH / SCAN_AND_FETCH</text>
<text class="row-label" x="220" y="323.2" text-anchor="end">reverse-small-1000</text>
<g class="ci-win"><title>reverse-small-1000: 0.436× [0.436, 0.436]</title><line x1="431.8" y1="319.2" x2="431.8" y2="319.2" stroke-width="2.2"/><line x1="431.8" y1="315.2" x2="431.8" y2="323.2" stroke-width="1.8"/><line x1="431.8" y1="315.2" x2="431.8" y2="323.2" stroke-width="1.8"/><circle cx="431.8" cy="319.2" r="3.8"/></g>
<text class="val-text" x="724" y="323.2">0.436× [0.436, 0.436]</text>
<text class="row-label" x="220" y="347.2" text-anchor="end">reverse-split-1000</text>
<g class="ci-win"><title>reverse-split-1000: 0.403× [0.403, 0.403]</title><line x1="411.7" y1="343.2" x2="411.7" y2="343.2" stroke-width="2.2"/><line x1="411.7" y1="339.2" x2="411.7" y2="347.2" stroke-width="1.8"/><line x1="411.7" y1="339.2" x2="411.7" y2="347.2" stroke-width="1.8"/><circle cx="411.7" cy="343.2" r="3.8"/></g>
<text class="val-text" x="724" y="347.2">0.403× [0.403, 0.403]</text>
<text class="row-label" x="220" y="371.2" text-anchor="end">executor-skip3-small-10</text>
<g class="ci-win"><title>executor-skip3-small-10: 0.791× [0.791, 0.791]</title><line x1="584.6" y1="367.2" x2="584.6" y2="367.2" stroke-width="2.2"/><line x1="584.6" y1="363.2" x2="584.6" y2="371.2" stroke-width="1.8"/><line x1="584.6" y1="363.2" x2="584.6" y2="371.2" stroke-width="1.8"/><circle cx="584.6" cy="367.2" r="3.8"/></g>
<text class="val-text" x="724" y="371.2">0.791× [0.791, 0.791]</text>
<text class="row-label" x="220" y="395.2" text-anchor="end">executor-skip3-small-1000</text>
<g class="ci-win"><title>executor-skip3-small-1000: 0.534× [0.534, 0.534]</title><line x1="483.8" y1="391.2" x2="483.8" y2="391.2" stroke-width="2.2"/><line x1="483.8" y1="387.2" x2="483.8" y2="395.2" stroke-width="1.8"/><line x1="483.8" y1="387.2" x2="483.8" y2="395.2" stroke-width="1.8"/><circle cx="483.8" cy="391.2" r="3.8"/></g>
<text class="val-text" x="724" y="395.2">0.534× [0.534, 0.534]</text>
</svg>
<div class="bench-legend"><span><i class="bench-swatch" style="background:#0d9488"></i> 95% CI entirely &lt; 1.000 (faster / lower)</span><span><i class="bench-swatch" style="background:#64748b"></i> 95% CI overlaps 1.000 (unresolved)</span><span><i class="bench-swatch" style="background:#d97706"></i> 95% CI entirely &gt; 1.000 (slower / higher)</span></div>
</figure>

Ratios final-remote / final-ordinary with nominal 95% intervals. Below 1 means
remote fetch is faster or allocates less.

| Case | ns/op | B/op | allocs/op |
|---|---:|---:|---:|
| reverse-small-1000 | 0.337 [0.331, 0.343] | 0.506 [0.505, 0.506] | 0.436 [0.436, 0.436] |
| reverse-split-1000 | 1.146 [1.130, 1.161] | 0.692 [0.691, 0.692] | 0.403 [0.403, 0.403] |
| executor-skip3-small-10 | 0.783 [0.764, 0.800] | 0.684 [0.684, 0.684] | 0.791 [0.791, 0.791] |
| executor-skip3-small-1000 | 0.455 [0.445, 0.465] | 0.580 [0.580, 0.580] | 0.534 [0.534, 0.534] |

### Absolute values

Median over blocks of each process's median of its three repeats.

| Case | final-ordinary | final-remote | rows/op |
|---|---:|---:|---:|
| reverse-small-1000 | 46.5 ms | 15.6 ms | 1,000 |
| reverse-split-1000 | 165 ms | 188 ms | 1,000 |
| executor-skip3-small-10 | 1.87 ms | 1.46 ms | 10 |
| executor-skip3-small-1000 | 47.5 ms | 21.4 ms | 997 |

Allocations per operation (medians rounded to integers):

| Case | B/op ordinary | B/op remote | allocs/op ordinary | allocs/op remote |
|---|---:|---:|---:|---:|
| reverse-small-1000 | 4,606,422 | 2,329,002 | 51,954 | 22,656 |
| reverse-split-1000 | 457,458,692 | 316,335,014 | 77,708 | 31,305 |
| executor-skip3-small-10 | 189,434 | 129,568 | 1,172 | 927 |
| executor-skip3-small-1000 | 5,452,217 | 3,161,135 | 66,344 | 35,444 |

### Order and block diagnostics (`ns/op`)

Descriptive, not another inferential gate. The first two columns are the
geometric mean over the three blocks in which that arm ran first.

| Case | Remote first (3 blocks) | Ordinary first (3 blocks) | Block ratios, min–max | Leave-one-block-out, min–max |
|---|---:|---:|---:|---:|
| reverse-small-1000 | 0.333 | 0.341 | 0.322–0.345 | 0.336–0.340 |
| reverse-split-1000 | 1.142 | 1.149 | 1.117–1.173 | 1.140–1.151 |
| executor-skip3-small-10 | 0.785 | 0.782 | 0.743–0.817 | 0.777–0.792 |
| executor-skip3-small-1000 | 0.449 | 0.461 | 0.437–0.475 | 0.451–0.458 |

## Interpretation

Measured:

- **Remote fetch is faster in three of four cases.** It takes 0.337
  [0.331, 0.343] of the ordinary path's time for the reverse scan of 1,000 small
  records (46.5 → 15.6 ms). With Skip 3 it takes 0.455 [0.445, 0.465] over the
  whole index (47.5 → 21.4 ms) and 0.783 [0.764, 0.800] for ten returned rows
  (1.87 → 1.46 ms).
- **Retained negative result: reverse scan of 1,000 split records.** Remote
  fetch is about 15% slower (1.146 [1.130, 1.161]; 165 → 188 ms), although it
  allocates 31% fewer bytes (0.692) and 60% fewer objects (0.403). All six
  block ratios lie between 1.117 and 1.173.
- **Remote fetch allocates less in every case.** `B/op` ratios are
  0.506–0.692 and `allocs/op` ratios 0.403–0.791.
- **A positive Skip does not remove the remote-fetch advantage at these
  sizes.** The audits confirm that remote fetch reads the skipped entries in
  its one remote-fetch opening while the ordinary path never loads them, and remote
  fetch is still faster in both Skip cases.
- **The reverse cases match C1's forward results in direction and size.** In
  the final tree, C1 measured 0.341 [0.321, 0.356] for forward `small-all-1000`
  and 1.150 [1.129, 1.181] for forward `split-all-1000`, against 0.337 and 1.146
  for the reverse scans here. These come from different processes and phases.
  This is a descriptive comparison, not a paired estimate of a reverse-scan
  effect.

Hypotheses, not tested here:

- For 1,000 split records, the storage server reads and returns about 100 MB
  per operation itself (1,000 × 100,000-byte payloads). On one `fdbserver`
  process, that serial server-side work may cost more than the client-side
  pipelined loads it replaces, in either scan direction. The allocation ratios
  show the client does less work; server CPU was not profiled.
- The three extra entries that remote fetch reads under Skip 3 are small next
  to 10 or 997 returned small records. Whether a larger Skip, or a Skip over
  split records, would erase the advantage was not measured.
- The Skip cases go through the query executor, not the store-level scan of the
  reverse cases. The difference between 0.455 (Skip, forward, executor) and
  0.337 (reverse, store-level) is not attributed to either factor: the cases
  differ in benchmark, direction, Skip and returned rows.
- Client and server share a host and talk over a veth pair, so no
  inter-host or physical-network latency is included. Remote fetch saves client round trips, so its effect
  over a real network is likely different. The multi-node campaign
  (`tools/benchcampaign/multinode`) is designed to measure that.

## Limits

- Single host, one `fdbserver` process, single redundancy, `memory` engine: no
  replication, no network hop, no disk. One operation at a time.
- Final tree only. This phase does not compare remote fetch across trees, and
  it does not test reverse scans for the Skip cases or Skip for split records.
- testing.B reports means; there are no latency percentiles.
- Six blocks give limited resolution. Intervals are nominal, per case and
  metric, with no multiplicity correction and no aggregate across cases.
  Remote-first and ordinary-first blocks agree in direction for every time
  ratio; the largest spread is `executor-skip3-small-1000` (0.449 vs 0.461).
- Complete phase, no subset: 144 raw samples (36 per case, 18 per arm and
  case) entered the analysis, no case was excluded, there are no parser
  failures, and the analysis is not limited to complete cases. Binary and input
  pins were verified before, after and at the end of the phase.
- No physical-prefix confound: within each case both arms used the same store
  prefix length (19 bytes for the reverse cases, 22 for the Skip cases). The
  two benchmarks' prefixes differ from each other, so absolute values are not
  compared across them.
- Case labels in the aggregate CSV were reconstructed from the pinned plan and
  the unchanged per-run logs, for the same runner bug described in
  [phase B](phase-b-recordlayer.md#limits). All 144 labels were restored in a
  separate working copy; every other CSV field matched the raw output exactly,
  and no measurement was repeated or changed. Hashes are in the campaign's
  analysis report.
