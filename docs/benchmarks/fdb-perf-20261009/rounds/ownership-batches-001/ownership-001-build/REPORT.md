# Frozen ownership correction: short two-batch exploratory comparison

## Scope and provenance

Root authorization #377 released exactly one 36-process campaign after remote standalone harness units/builds. Root update #386 explicitly restricts interpretation to the **frozen ownership correction, before the separately developed read-order correction**. This does not benchmark the current root worktree and is not evidence of a globally faster or fully current correct client.

- Campaign: `quick-ownership-batches-001`, 2026-10-10 05:15:35–05:17:48 UTC; all 36 scheduled runs completed, no failures/reruns/profiles.
- Base: `26abdbaac12be60ea8955027f6047e10474596f1`.
- New source is root's fresh local snapshot, not an old remote patch: archive SHA256 `3fe1b7c8594a78c4a7fa6bfab67ec4db01fcdc39bc47e22d30e47c6e83bc8575`; complete-base patch `cf470a02f825538d43ddfc346e27368b3870681361a7e23435218e5b225dcc1a`. All 745 selected files verified; separate patch reconstruction matched all 745.
- Old arm is prior conflictbuf512, **not** grv-idle: complete-base patch `b3c09057e81ac448c8b05881e10cd52a34067a27152fcd0a4c367e514e0861c3`; all 740 selected source files verified.
- Same minimal V6 harness in all arms: archive `3d2504ae0db14fad14f0384287377998231cf049310c8289d6e7245d3428e0d0`; patch-from-V5 `569212a209c0caa0f805c1f1600eda10355504a457f3e7659a6ed32db9cdcd07`. Fixed `batchset50x1000` workload added; original 100-byte workload and both backend adapters retained. Old Go/new Go/CGo each passed all 15 top-level standalone harness tests, actual `-count=1`, remotely with Go1.26.9. Parent-project Bazel verification is root-reported separately in `AUTHORIZATION-STATUS.json`, not executed by the measurer.
- Binary SHA256:
  - old `candidate-v6-old512-go`: `1aa2b2c7285878988007f11deb3a052cf76ed12163dbfc8aecbf9109fa2d283f`
  - new `candidate-v6-owned-go`: `5fda977a2be97bb6543d5ae2a398531f9168ab11f0f531a3db1d1b8965e57929`
  - native `candidate-v6-cgo`: `cd762ea429647784378270613f404573ba598a5049f422e055a887f7746ecbdb`
- Native library 7.3.77/API730 loaded SHA256 `e4ad7fed7b7580bf5f7b38f0a4279d37e698f69d534182e5de0b8beeef6cd1b1`, verified in each CGo manifest. Pure-Go processes did not load it.

## Design and checks

Six paired run blocks per workload, all six permutations of old/new/CGo exactly once per workload, order fixed by seed391718 before execution. Each fresh process ran 40 warmups then 100 measured transactions, workers1. Each transaction sets 50 distinct worker-specific existing keys, then commits. Values change and are precomputed outside timing. Every final slot is byte-checked outside timing. Each run uses a unique 68-byte prefix; every key is 88 bytes across both workloads and all arms. Timeout30000ms/retrylimit10/allow-writes held constant.

Totals: 3,600 measured commits / 180,000 measured Sets, plus 1,440 warmup commits / 72,000 warmup Sets. All raw observations have successful check status; all persisted validation passed. No callback retries, errors or warmup errors. Each Go arm/workload aggregate is exactly 600 GRVs and 600 started/completed commits, zero GRV cache hits, connection failures, or transaction retries. Native internal counters are **unobserved**, not zero.

VM16logical CPUs AMD EPYC-Milan/KVM, single local FDB SSD server. No physical-host-isolation claim. Server CPUs2–7, clients8–15/GOMAXPROCS8, runner/collector0–1; Docker/containerd inactive. Unchanged runner applied 2s cooldown + 1s per-CPU/group quiescence before each arm, 180s process cap and 1Hz telemetry. All gates passed; observed gate steal maximum0, max client individual busy1%, max server individual busy7.0707%. No remote build/test/other measurement ran during campaign.

## Paired run-level ratios

Each cell is the geometric mean of the six within-block numerator/denominator ratios, **not a ratio of pooled observations or a ratio of medians**. Lower is better. These six-block adaptive comparisons are exploratory; no confirmatory confidence-interval, noninferiority, capacity or universal-tail claim.

| Workload | Comparison | Mean latency | p50 | p99 | Measured process CPU/tx | Go allocated B/tx | Go allocs/tx |
|---|---|---:|---:|---:|---:|---:|---:|
| 50×100B | new / old | 1.033542 | 1.048220 | 0.943811 | 1.005042 | 0.989498 | 0.998743 |
| 50×1000B | new / old | 0.992109 | 0.981885 | 0.977292 | 1.144799 | 1.381123 | 1.001879 |
| 50×100B | new / native | 0.984754 | 0.966113 | 1.066159 | 1.375956 | not comparable total heap | not comparable total heap |
| 50×1000B | new / native | 0.954902 | 0.921223 | 1.189578 | 1.210683 | not comparable total heap | not comparable total heap |

New/old mean ratios by block span0.981459–1.226053 for 100B (3/6 below1) and0.832023–1.212970 for1000B (3/6 below1). New/old CPU is below1 in3/6 small-batch pairs and0/6 large-batch pairs; allocated bytes below1 in6/6 small pairs and0/6 large pairs. New/native mean ratio ranges0.932835–1.014907 and0.797400–1.099632 respectively (4/6 below1 for each). Native p99 and CPU advantages prevent interpreting the point mean ratios as a general win.

## Absolute resources and latency

Values below are **medians of six run-level statistics**. CPU and Go allocation statistics cover measured transactions only. GC count is the sum across six measured phases. A median table will not reproduce the paired geometric ratios above.

| Workload | Arm | Mean ms/tx | p50 ms | p99 ms | CPU µs/tx | Go B/tx | Go allocs/tx | GC count |
|---|---|---:|---:|---:|---:|---:|---:|---:|
| 50×100B | old | 2.491536 | 2.395093 | 3.940198 | 1175.025 | 115143.64 | 466.645 | 37 |
| 50×100B | new | 2.485632 | 2.395303 | 3.765238 | 1203.190 | 113909.84 | 466.370 | 36 |
| 50×100B | native | 2.517324 | 2.464253 | 2.985984 | 877.420 | 1616.16* | 59.000* | 0* |
| 50×1000B | old | 3.018743 | 2.943245 | 5.460903 | 1216.600 | 253757.08 | 464.045 | 17 |
| 50×1000B | new | 3.145375 | 3.054429 | 5.156875 | 1365.705 | 350983.16 | 464.765 | 12 |
| 50×1000B | native | 3.121317 | 3.101209 | 3.633355 | 1138.805 | 1616.16* | 59.000* | 0* |

*Native GoMemStats excludes C++ heap allocation/collection. No total-heap comparison is made. Lower GC count does not mean lower allocation cost: heap size, fixture retention and GC timing differ. Fixture payload is700,000B in the100B workload and7,000,000B in1000B, outside timing. Lifetime peak RSS medians old/new/native are14170/14448/38486KiB for100B and28476/41284/61372KiB for1000B; these include setup/persistence, not isolated measured-phase RSS.

The source/shape proof for the pool-cap control uses the shared transaction conflict buffer, **not** `captureCommit`'s separate unpooled snapshot arena. At88B keys, old shared conflict storage is50×177=8,850B in either workload. With ownership copies, small consumes50×(188+177)=18,250B and promotes512→32768 on the second Set. Large consumes50×(1088+177)=63,250B: the first operand requires the32768 pool and the26th Set's conflict append forces65536 growth outside the pool. This is static shape evidence, not runtime allocation instrumentation; the large variant also changes copying, wire traffic and fixture size, so the38.1% measured allocation increase and14.5% CPU increase are not a cap-only causal estimate. Read-result ownership cost occurs only in untimed persistence reads here.

## Telemetry and preservation limitations

New `candidate-` binary names restore the unchanged runner's client `/proc` matcher for all three arms. However processes last only0.379–0.631s (median0.502s), below the1Hz interval. The immediate sample caught a client in31/36 runs; these five have no client `/proc` observation: `b002-batchset50-p0-new`, `b005-batchset50-p2-new`, `b004-batchset50-p1-cgo`, `b003-batchset50x1000-p2-new`, `b004-batchset50x1000-p0-new`. Host/server telemetry, gate evidence, per-process lifetime time data and internal measured getrusage/MemStats exist for all36. No claim of complete client time-series coverage. Earlier V5 artifacts retain their separate known filename-matcher limitation and are not rewritten.

The first local schedule-construction attempt was rejected by a 68-byte-prefix assertion before creating a plan or running any remote workload (it produced71-byte prefixes). Its script is preserved as `make-plan-initial-rejected.py`; final20-character padding passes and was frozen before timing. There were no failed remote builds or failed workload attempts in this campaign. Raw observations, warmups, environment/options/manifests, source/patch hashes, all three binaries, original runner/analyzer, standalone tests and verification scripts are preserved.

Conclusion: the ownership fix's small-batch measured resource cost is near-flat in this short sample, while the large-batch control exposes a clear allocation/CPU tradeoff. Do not recommend the unsafe old client, infer global native superiority from point latency means, or apply these numbers to the newer read-order fix. No further workload or profile was authorized or run.
