# Prepared offline campaign analysis adapter

**Prepared only. Do not execute measured analysis until root explicitly
coordinates a quiet local window.** The 20,000-resample analysis is substantial
CPU work. No measured campaign analysis has been run during preparation.

Source: `execution/analyze_campaign.py`.
Controls: `execution/test_analyze_campaign.py` and
`execution/analysis-adapter-mock-controls.log`.
The controls use tiny synthetic files and fake analyzers/statistical functions;
they neither run benchmarks nor analyze campaign measurements.

## Ready CLI — run later, not as a readiness probe

```sh
C=${FDB_PERF_HOME}/campaigns/fdbmn-20261020
python3 -B "$C/execution/analyze_campaign.py" \
  --campaign "$C" \
  --protocol "$C/deploy/protocol.json" \
  --output "$C/analysis/measured-492817" \
  --phase M2rl --phase M2skip \
  --phase M1w1 --phase M1w64 --phase M1w8 --phase M1w32 \
  --omitted-phase 'M1w32=not executed: budget under the 11:15 rule'
```

Select only the desired official protocol phases. `--phase` is repeatable;
`--output` must be a fresh directory below this campaign's `analysis/`.
`--omitted-phase` is only accepted for an absent/unexecuted phase; it cannot
hide an existing collected phase. Omit that flag if M1w32 actually executed.
Missing phases remain explicitly N/A. Diagnostic/F0/capacity data are never
substituted. The program does not launch SSH, cloud tools, tests or benchmarks.

Default prior campaign:
`${FDB_PERF_HOME}/hcloud-multinode-vpc-20261014`.
Use `--prior PATH` only for an equivalent retained historical archive.

If protocols were deliberately amended between measured phase seals, analyze
each consistent group against its matching archived protocol in separate fresh
output directories. The adapter refuses a controller manifest whose pinned
protocol differs; it never rewrites a seal to fit current files.

## Whole-case eligibility

Inputs are `results-raw/<phase>` and `reconcile/<phase>/report.json`.
Before any case reaches statistics, the adapter requires:

- A completed, nondiagnostic, quiescent phase-controller collection, no controller
  errors, acknowledged manifest identity and final pre-R4 remote pin proof.
- The entire expected protocol case population represented exactly once in a
  completed R4 report, consistent summary counts and matching R4/sampler code.
- For each analyzed case, `verdict == clean` and the exact complete R4 run-ID
  population. An overloaded/contaminated/unverifiable case is omitted whole,
  not filtered by arm or block.
- M1: six blocks, exactly `before`, `final`, `cgo`, all 18 runs, source/binary/
  native-runtime identities, a complete campaign, and the frozen analyzer's
  existing raw/identity/lifecycle/correctness validation.
- M2: six blocks, two ordinary/remote arms, three within-process repeats,
  all 12 process identities per eligible case, unchanged execution pins,
  raw PASS/audit parsing and exact agreement of CSV, parsed rows and audits.

Input trees are hashed before and after analysis; mutations refuse the result.
No reduced-block or per-arm salvage is permitted. If no case is eligible, the
summary explicitly says `complete-no-eligible-cases` and
`performance_results_available: false`, not a vacuous performance pass.

## Existing numerical methods only

### M1

The amended frozen `analyze.py` is invoked twice on **copies**, once with
`--denominator cgo`, once with `--denominator before`; both use
`--seed 492817 --resamples 20000`. This directly yields:

- final/before = new Go / previous Go;
- final/cgo = new Go / CGo;
- before/cgo = previous Go / CGo.

The existing ten latency/CPU comparison metrics are retained, including
`shutdown_process_cpu_ns_per_op` (E−A), plus Go bytes/mallocs per transaction
through the existing `bootstrap_log_ratio` and `dependence_sensitivity` APIs.
The sampling unit is six paired blocks, not individual transactions. Zero
allocation ratios remain undefined with absolute counters retained; no epsilon
or selective block deletion is introduced. Native C++ heap is excluded from
Go allocation metrics. Whole-process absolute E is not promoted into paired
E−A evidence.

### M2

R2 plans are adapted in memory to the existing frozen single-node
`bench_analyze.load_samples` / `summarize` APIs. Raw logs are checked with
`bench_runner.parse_output` and `bench_runner_mn.audit_case`. No incompatible
generic `--input` invocation or new estimator is used. Three repeats stay
clustered within their process; their median enters each of the six paired
blocks. Metrics are the original `ns/op`, `B/op`, `allocs/op`, `rows/op`,
`prefix-B`. Testing.B means are not p99. Placement labels/fractions and physical
prefix confounds remain attached to cases and comparison records.

## Provenance and outputs

The adapter verifies and records the compatibility archive, source/build
receipts, compiler identity, per-arm executable identities, four F0 runtime
receipts, imported analyzer/helper hashes, controller pins, R4 inputs/report,
and raw artifact hashes. Client analysis uses the amended frozen scripts, not
current production clientbench pacing/workload code. Numeric analysis is not
modified by this adapter.

Outputs below the chosen fresh analysis directory:

```text
summary.json                         overall case availability, provenance
analysis-provenance.json             source/runtime/compiler/helper identities
<phase>/analysis-provenance.json     controller/R4 pins and raw input hashes
<M1phase>/summary.json                eligible cases, all 3 contrasts/12 metrics
<M1phase>/<case>/paired-all.csv       combined client comparisons including allocations
<M1phase>/<case>/denom-cgo/           original analyzer outputs on a copied raw tree
<M1phase>/<case>/denom-before/        second denominator's original analyzer outputs
<M2phase>/report.json                 existing testing.B report plus placement/provenance
<M2phase>/paired.csv                  testing.B comparisons
REFUSED.json                         explicit failure; no clean overall result
```

Client denominator subdirectories retain the analyzer's `analysis.json`,
`per-run.csv`, `absolute.csv`, `paired.csv`, `TABLES.md`, raw file hashes,
unavailable comparisons, order diagnostics and dependence sensitivity.

## Historical comparison boundary

Historical eligibility is checked against its own R4 report. Historical M1
`final` must name the same production revision as current `before`; worker and
workload geometry must match. Historical final/cgo is placed beside current
before/cgo and final/cgo, not confused with the new direct final/before pair.
Historical M2 joins require exact phase/case/metric/arm identities and matching
sealed benchmark geometry; placement differences remain visible.

Historical records are side-by-side **descriptions of different campaign
cohorts**. No old/new pooled blocks, ratio-of-ratios, confidence-bound division,
or cross-campaign inferential interval is computed. Missing or previously
contaminated historical cases stay unavailable.

## Preparation verification

Only these preparation commands were used:

```sh
python3 -B "$C/execution/test_analyze_campaign.py"
python3 -B "$C/execution/analyze_campaign.py" --help
```

The fake controls cover missing/duplicate populations, R4/controller schema,
whole-case overload omission for both harnesses, all three M1 contrasts using
two copied-tree CLI calls, six-block allocation API use, zero counters, raw M2
CSV tampering, missing budget phases and descriptive-only historical joins.
They are not measured-analysis or FoundationDB validation. Source ACK is being
requested independently from `prep-review`; do not silently edit reviewed
source after ACK.
