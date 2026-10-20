# Read1000: 512-byte initial conflict buffer — short paired comparison

Campaign `quick-conflictbuf-read1000-001`, seed 391716, root release #350. Original frozen v3 harness unchanged. Six counterbalanced blocks containing all six old/new/CGo orders exactly once; 300 measured +100 warmup operations per fresh process, workers=1. All 18 arms completed: **5400 measured operations**, 1800 per arm; 1800 total warmup operations. Runner plus analysis/validation completed in approximately 63 seconds (2026-10-10 03:09:39–03:10:42 UTC).

## Result and limits

The allocation reduction is clear in this short campaign: new/old paired allocated-byte ratio **0.17477** (all six pairs lower), approximately **82.5% less Go allocation**. Median run allocation fell from 39155.13 to 6841.79 B/op. Paired mean latency improved modestly, ratio **0.96313** (5/6 pairs lower); paired process CPU ratio **0.97530** (4/6 pairs lower). This is not a large CPU improvement: new/CGo paired CPU remains **1.18968**, despite new/CGo latency ratio **0.72790**.

Every ratio below is the geometric mean of six **paired run-level** ratios. Bootstrap outputs in analysis.json remain diagnostic; these are short, adaptive, small-N measurements on one co-located VM/server, not a confirmatory trial or independent-host sample. No universal tail or capacity claim.

| Comparison | Mean latency | p50 latency | p99 latency | Process CPU | Go allocated bytes | Go allocation count |
|---|---:|---:|---:|---:|---:|---:|
| New / old | 0.963131 | 0.978983 | 0.813507 | 0.975300 | 0.174766 | 0.998198 |
| New / CGo | 0.727903 | 0.728180 | 0.720615 | 1.189684 | 4.596991 | 4.835682 |
| Old / CGo | 0.755767 | 0.743813 | 0.885813 | 1.219813 | 26.303705 | 4.844410 |

New/old mean pair range 0.898938–1.021581. New/old p99 pair range **0.381140–1.671235**, only 4/6 pairs lower. New/CGo p99 range 0.450513–1.481905, 5/6 lower. These mixed individual tails preclude a blanket tail win.

### Pooled descriptive latency and run-resource summaries

| Arm | Pooled mean / p50 / p99 (µs) | Median process CPU (µs/op) | Median Go allocation (B/op) | Median Go allocations/op | Total measured Go GC cycles (six runs) | Total measured Go GC pause (ms) |
|---|---|---:|---:|---:|---:|---:|
| Old grv-idle | 476.547 / 451.168 / 1223.612 | 512.182 | 39155.13 | 63.022 | 12 (2 each) | 1.197 |
| New conflictbuf512 | 458.894 / 440.477 / 771.364 | 490.562 | 6841.79 | 62.895 | 6 (1 each) | 1.054 |
| Official CGo | 632.734 / 620.955 / 1106.152 | 421.153 | 1488.00 | 13.000 | 0 | 0 |

Pooled percentiles are descriptive only, not the observations used for paired ratios. CGo's allocation and GC columns are **Go-runtime-only** and exclude the C++ native heap; their ratios are not total-memory comparisons. Process CPU includes user+system time. Process lifetime high-water RSS remains in raw summaries and counter-resource-validation.json, not presented as live/interval heap size.

## Exact workload, checks and path evidence

Each operation is one fresh ReadTransact, one Get of exactly **1000 decimal bytes**, exact byte comparison and **no Commit**. Original v3 default prefix `clientbench/v1/`, options, payload, read path and adapter semantics are unchanged. Timeout30000ms, retrylimit10, allow-writes for untimed setup, profile none. Setup and persisted checks are outside the measured interval.

- Each arm completed 1800 measured operations with exactly 1800 callback attempts, **zero errors and zero callback retries**. Each also completed 600 warmup operations/attempts with zero errors/retries.
- Every raw phase/count/index/value check, seed validation and fresh-transaction persisted validation passed. Every quiescence gate passed.
- Each Go arm recorded **1800 read versions +1800 successful GRV batch replies**, zero GRV cache hits, zero commit starts/completions, zero transaction retries and zero connection failures.
- Native client counters remain **unobserved**, never synthetic zero. The exact mapped native library hash was verified for every CGo process. Pure-Go manifests verified no mapped native client.
- Same server/library/config and binary/source pins before and after; no concurrent builds/tests/profiles or other remote commands during timing. Runner/collector CPUs0–1, FDB2–7, clients8–15, GOMAXPROCS8; original 2s cooldown, 1s quiescence and deliberate 1Hz collector. Docker/containerd inactive. No physical-host isolation claim.

## Immutable identities

Base `26abdbaac12be60ea8955027f6047e10474596f1`; Go1.26.9; server/native7.3.77; API730. Original v3 harness SHA-256 `41f594c990477bc4b29c565160599a57266cecb23adaae582180f7464fb2d0ea`.

- Old client patch: `32d59b91cdd509ba0cb985f88f52a2d62fdae4ad0020f832183ba79555e024e3`; binary `candidate-grv-idle-001-go`: `5b6e7302f977785317a83e4d9172555c48fe400f9362536e7fe8436d0b9c355b`.
- New complete client patch: `b3c09057e81ac448c8b05881e10cd52a34067a27152fcd0a4c367e514e0861c3`; binary `candidate-conflictbuf-512-001-go`: `47f7ea5872c0efcbf0435f3071e60c832915102df8c36c9e9208bb0916011cfb`.
- Official `baseline-cgo` binary: `af2e91489057be13a4b74c55c047b06de15d2483a981697523b076648f929dde`.
- Native library: `e4ad7fed7b7580bf5f7b38f0a4279d37e698f69d534182e5de0b8beeef6cd1b1`.

See plan.json, binaries.json, per-arm manifests, source archives/patches and SHA256SUMS for reconstruction and checks.

## Correctness status at release — append-only update

Root #350 reported **full131 GREEN151/151**, **95 actual +56 cached**, **1865.484 seconds**, all **6814 frozen hashes** verified. Root reported hn-client/hn-codehealth/astra-grv ACK with no Medium findings. This is root's parent just/Bazel result, not a suite rerun by the measurement agent. The exact measured binary/runtime is the reviewed frozen candidate. Root's later oversized allocator unit case/test rename does not change this runtime or the archived source identity. Earlier build/measurement archives retain their truthful status at the time; none was rewritten.

Only this campaign and the subsequent separately-versioned batchset50 control were released. Both finished; no new profile or workload is authorized. Await root's next decision after backups.
