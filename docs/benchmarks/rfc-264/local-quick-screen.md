# RFC-264: local P0/P2t screen

## Result

**Reject P0 and P2t; RFC-264 is Rejected.** Both candidates passed the all-cell
throughput/CPU noninferiority bounds, but neither met its intended p50 improvement
condition. At most one qualifying candidate could be promoted; none qualified.
No production optimization, further prototype or further measurement is proposed.
The results, common harness, reproduction tools and archival prototype patches
are retained; neither prototype is applied to production code.

All **72/72 processes completed**, with verified cleanup. Terminal errors and
transaction-callback retries were **zero in every process**. This is benchmark
completion evidence, not a correctness-suite pass.

## Compared inputs

All three arms used source base
`68b8a64d274d5e5a041912934198ece79e7e009a`, **Go 1.26.10**, and the same common
duration harness. `master` is the historical baseline's arm label, not a claim
about later master revisions. The historical bisects used Go 1.26.9 and are
reported separately; their measurements have not been relabeled for a later
compiler.

- **P0:** a nonblocking already-published PendingGet response probe before the
  existing global flush, retaining the old path when not ready. Intended p50
  cells: get100 at 8 and 32 workers.
- **P2t:** cache the timer's safe `syscall.RawConn` wrapper while retaining
  `RawConn.Control`, descriptor-lifetime protection, syscalls, relative duration,
  jitter, Reset/Wait/Close and the positive coalescing policy. Intended p50 cells:
  all six. The wrapper is shared by coalescing and GRV timers.

The [P0 archival patch](../../../tools/benchcampaign/campaigns/rfc-264/local/prototypes/p0.patch)
and [P2t archival patch](../../../tools/benchcampaign/campaigns/rfc-264/local/prototypes/p2t.patch)
are reproduction inputs only. Their production changes are rejected.

| Arm | Prototype patch SHA-256 | Measured binary SHA-256 |
|---|---|---|
| master | `e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855` (empty patch) | `2730dd6f545ab61928be0516e1040faf15116b2c03497cd29e29990d139fdac0` |
| P0 | `22444af5231c7a7c85eafa7bb89dd7d5a4b0937ee3c0393b74efabc05d0b03cb` | `d4e7be3b3c492dc080f8bce8522aa6b90c3a2dc2a9de6ce9cb7c56dc3dc8b69a` |
| P2t | `c3a18eda8d65c9dc1e6d8d731d5710c4c386bfdc70b156ffc927b0f60bd60f7b` | `eb7640b901ac012d97a111d4d139f9e95a4bb29d74d4eb579c1f915151a79f1e` |

The common harness patch SHA-256 is
`a0464ae41dd96b49f8c9c9a2f3ebde54faa2ef3a1bf8bc01a8309e2d1084d77a`.
Binary identities were unchanged before and after measurement. The build manifest
retains complete per-arm source, compiler, patch and binary identities.

## Method and populations

Four paired blocks used **ABC / BCA / CAB / ABC**, where A=master, B=P0 and
C=P2t. Every block covered grv, get100 and set100 at 8 and 32 workers; cell order
rotated by block. The population was **3 arms × 6 cells × 4 blocks = 72 sequential
processes**, with no adaptive sampling or sample deletion. Each process used
**10 seconds warmup + a nominal 30-second measured window**. Rates used the
actual timer-wake window, not the nominal duration. Warmup and boundary drain
were excluded from headline samples and counts.

The closed-loop pure-Go client used `GOMAXPROCS=8`, `GOGC=100`,
`GOMEMLIMIT=off`, no `GODEBUG` override and no rate limiting. Client and server
CPU partitions were disjoint. The common harness validated raw completions,
quantiles, retry counts, timing, lifecycle and identity, and the driver verified
owned-prefix cleanup before allowing a comparative summary.

Each table has four signal groups:

- **Latency:** successful-operation p50/p99; error p99 only with at least 20
  measured errors in that arm/cell. No arm/cell met that threshold here.
- **Traffic:** successful operations per second in the actual measured window.
- **Errors:** terminal errors and actual transaction retries per 1,000
  **completed logical operations**, with pooled raw counts and denominators.
  Retries are `sum(max(transaction-callback entries - 1, 0))`, not internal
  RPC retries or hedges.
- **Saturation:** all-process user+system CPU in the measured window divided by
  window wall time (cores busy), and by successful-operation count (CPU/op).
  The CPU stop snapshot preceded the boundary-work join and may include a small
  concurrent tail; this is not request-attributed CPU.

Positive-metric ratios are the **geometric mean of four paired within-block
candidate/baseline ratios**. Brackets are **95% whole-block percentile bootstrap
CIs**, with 20,000 resamples and seed 492817. They are not ratios of displayed
absolute aggregates. These CIs are distinct from the historical bisects'
four-block min/max ranges.

Absolute latency, traffic and saturation values are geometric means across the
four blocks. Absolute error/retry rates are pooled counts divided by pooled
completed logical operations ×1,000; raw counts are sums. Their ratios would use
paired per-block rates, not ratios of pooled absolutes. Zero error/retry ratios
are undefined; no pseudocounts are used. Error-p99 absolute values would be
nearest-rank p99 of pooled raw error latencies, with ratio/CIs requiring at least
20 errors in every paired run of each arm. Here all error p99s are n/a.

## grv · 8 workers

| Arm | Latency | Traffic | Errors | Saturation |
|---|---|---|---|---|
| master | success p50 212.37µs (reference); success p99 262.54µs (reference); error p99 n/a (0 errors; <20) | successful ops/s 37312 (reference) | terminal errors/1k completed 0 (reference) (raw 0); actual retries/1k completed 0 (reference) (raw 0; 4477399 completed logical ops) | cores busy 0.7094 (reference); CPU/successful op 19.013µs (reference) |
| P0 | success p50 212.77µs (1.00186× [1.00074, 1.00273]); success p99 263.92µs (1.00528× [1.00080, 1.00977]); error p99 n/a (0 errors; <20) | successful ops/s 37229 (0.99778× [0.99702, 0.99911]) | terminal errors/1k completed 0 (ratio undefined) (raw 0); actual retries/1k completed 0 (ratio undefined) (raw 0; 4467462 completed logical ops) | cores busy 0.71034 (1.00132× [0.99788, 1.00477]); CPU/successful op 19.08µs (1.00355× [0.99953, 1.00758]) |
| P2t | success p50 213.27µs (1.00421× [1.00172, 1.00671]); success p99 265.47µs (1.01118× [1.00505, 1.01735]); error p99 n/a (0 errors; <20) | successful ops/s 37136 (0.99530× [0.99258, 0.99803]) | terminal errors/1k completed 0 (ratio undefined) (raw 0); actual retries/1k completed 0 (ratio undefined) (raw 0; 4456346 completed logical ops) | cores busy 0.71326 (1.00544× [0.99894, 1.01198]); CPU/successful op 19.207µs (1.01019× [1.00092, 1.01955]) |

## grv · 32 workers

| Arm | Latency | Traffic | Errors | Saturation |
|---|---|---|---|---|
| master | success p50 230.8µs (reference); success p99 335.27µs (reference); error p99 n/a (0 errors; <20) | successful ops/s 1.3605e+05 (reference) | terminal errors/1k completed 0 (reference) (raw 0); actual retries/1k completed 0 (reference) (raw 0; 16326325 completed logical ops) | cores busy 1.7343 (reference); CPU/successful op 12.748µs (reference) |
| P0 | success p50 231µs (1.00090× [0.99904, 1.00278]); success p99 336.38µs (1.00330× [0.99899, 1.00772]); error p99 n/a (0 errors; <20) | successful ops/s 1.3596e+05 (0.99930× [0.99717, 1.00172]) | terminal errors/1k completed 0 (ratio undefined) (raw 0); actual retries/1k completed 0 (ratio undefined) (raw 0; 16314961 completed logical ops) | cores busy 1.7398 (1.00317× [0.99860, 1.00777]); CPU/successful op 12.797µs (1.00387× [0.99759, 1.01024]) |
| P2t | success p50 231.45µs (1.00282× [1.00196, 1.00382]); success p99 337.05µs (1.00530× [1.00348, 1.00685]); error p99 n/a (0 errors; <20) | successful ops/s 1.3567e+05 (0.99717× [0.99577, 0.99857]) | terminal errors/1k completed 0 (ratio undefined) (raw 0); actual retries/1k completed 0 (ratio undefined) (raw 0; 16280127 completed logical ops) | cores busy 1.738 (1.00213× [1.00086, 1.00338]); CPU/successful op 12.811µs (1.00497× [1.00258, 1.00737]) |

## get100 · 8 workers

| Arm | Latency | Traffic | Errors | Saturation |
|---|---|---|---|---|
| master | success p50 316.63µs (reference); success p99 404.31µs (reference); error p99 n/a (0 errors; <20) | successful ops/s 24930 (reference) | terminal errors/1k completed 0 (reference) (raw 0); actual retries/1k completed 0 (reference) (raw 0; 2991643 completed logical ops) | cores busy 1.1095 (reference); CPU/successful op 44.502µs (reference) |
| P0 | success p50 316.58µs (0.99984× [0.99839, 1.00191]); success p99 407.12µs (1.00693× [1.00480, 1.00883]); error p99 n/a (0 errors; <20) | successful ops/s 24926 (0.99981× [0.99790, 1.00115]) | terminal errors/1k completed 0 (ratio undefined) (raw 0); actual retries/1k completed 0 (ratio undefined) (raw 0; 2991076 completed logical ops) | cores busy 1.1135 (1.00366× [0.99763, 1.00972]); CPU/successful op 44.673µs (1.00385× [0.99674, 1.01101]) |
| P2t | success p50 317.63µs (1.00317× [1.00129, 1.00479]); success p99 409.26µs (1.01224× [1.00966, 1.01482]); error p99 n/a (0 errors; <20) | successful ops/s 24840 (0.99639× [0.99494, 0.99800]) | terminal errors/1k completed 0 (ratio undefined) (raw 0); actual retries/1k completed 0 (ratio undefined) (raw 0; 2980833 completed logical ops) | cores busy 1.1137 (1.00381× [0.99878, 1.00915]); CPU/successful op 44.834µs (1.00744× [1.00153, 1.01428]) |

## get100 · 32 workers

| Arm | Latency | Traffic | Errors | Saturation |
|---|---|---|---|---|
| master | success p50 384.65µs (reference); success p99 645.68µs (reference); error p99 n/a (0 errors; <20) | successful ops/s 81429 (reference) | terminal errors/1k completed 0 (reference) (raw 0); actual retries/1k completed 0 (reference) (raw 0; 9771503 completed logical ops) | cores busy 2.6504 (reference); CPU/successful op 32.548µs (reference) |
| P0 | success p50 385.34µs (1.00178× [0.99880, 1.00468]); success p99 656.05µs (1.01606× [1.00966, 1.02251]); error p99 n/a (0 errors; <20) | successful ops/s 81239 (0.99766× [0.99471, 1.00070]) | terminal errors/1k completed 0 (ratio undefined) (raw 0); actual retries/1k completed 0 (ratio undefined) (raw 0; 9748679 completed logical ops) | cores busy 2.6653 (1.00564× [1.00122, 1.00961]); CPU/successful op 32.808µs (1.00800× [1.00399, 1.01326]) |
| P2t | success p50 387.52µs (1.00746× [1.00501, 1.00913]); success p99 658.29µs (1.01952× [1.00051, 1.03890]); error p99 n/a (0 errors; <20) | successful ops/s 80810 (0.99240× [0.98987, 0.99565]) | terminal errors/1k completed 0 (ratio undefined) (raw 0); actual retries/1k completed 0 (ratio undefined) (raw 0; 9697213 completed logical ops) | cores busy 2.6519 (1.00059× [0.99067, 1.00912]); CPU/successful op 32.817µs (1.00825× [1.00080, 1.01664]) |

## set100 · 8 workers

| Arm | Latency | Traffic | Errors | Saturation |
|---|---|---|---|---|
| master | success p50 1005.7µs (reference); success p99 1097.4µs (reference); error p99 n/a (0 errors; <20) | successful ops/s 7944.4 (reference) | terminal errors/1k completed 0 (reference) (raw 0); actual retries/1k completed 0 (reference) (raw 0; 953329 completed logical ops) | cores busy 0.45047 (reference); CPU/successful op 56.703µs (reference) |
| P0 | success p50 1005.6µs (0.99984× [0.99956, 1.00018]); success p99 1097µs (0.99962× [0.99833, 1.00100]); error p99 n/a (0 errors; <20) | successful ops/s 7945.4 (1.00013× [0.99991, 1.00038]) | terminal errors/1k completed 0 (ratio undefined) (raw 0); actual retries/1k completed 0 (ratio undefined) (raw 0; 953460 completed logical ops) | cores busy 0.44881 (0.99631× [0.98873, 1.00395]); CPU/successful op 56.486µs (0.99618× [0.98842, 1.00399]) |
| P2t | success p50 1005.7µs (1.00001× [0.99977, 1.00031]); success p99 1097.8µs (1.00037× [0.99989, 1.00085]); error p99 n/a (0 errors; <20) | successful ops/s 7943.5 (0.99989× [0.99966, 1.00012]) | terminal errors/1k completed 0 (ratio undefined) (raw 0); actual retries/1k completed 0 (ratio undefined) (raw 0; 953227 completed logical ops) | cores busy 0.44917 (0.99712× [0.99054, 1.00484]); CPU/successful op 56.546µs (0.99723× [0.99094, 1.00475]) |

## set100 · 32 workers

| Arm | Latency | Traffic | Errors | Saturation |
|---|---|---|---|---|
| master | success p50 1007.5µs (reference); success p99 1660.9µs (reference); error p99 n/a (0 errors; <20) | successful ops/s 31325 (reference) | terminal errors/1k completed 0 (reference) (raw 0); actual retries/1k completed 0 (reference) (raw 0; 3759018 completed logical ops) | cores busy 1.3212 (reference); CPU/successful op 42.177µs (reference) |
| P0 | success p50 1007.6µs (1.00011× [1.00005, 1.00016]); success p99 1661.7µs (1.00048× [0.99195, 1.00933]); error p99 n/a (0 errors; <20) | successful ops/s 31318 (0.99980× [0.99932, 1.00033]) | terminal errors/1k completed 0 (ratio undefined) (raw 0); actual retries/1k completed 0 (ratio undefined) (raw 0; 3758239 completed logical ops) | cores busy 1.3266 (1.00409× [0.99684, 1.01049]); CPU/successful op 42.358µs (1.00430× [0.99640, 1.01127]) |
| P2t | success p50 1007.7µs (1.00019× [1.00001, 1.00036]); success p99 1665.2µs (1.00260× [1.00048, 1.00473]); error p99 n/a (0 errors; <20) | successful ops/s 31306 (0.99939× [0.99914, 0.99967]) | terminal errors/1k completed 0 (ratio undefined) (raw 0); actual retries/1k completed 0 (ratio undefined) (raw 0; 3756717 completed logical ops) | cores busy 1.3246 (1.00258× [0.99799, 1.00610]); CPU/successful op 42.312µs (1.00320× [0.99832, 1.00696]) |

## Decision against the fixed criteria

Both P0 and P2t passed **every cell's throughput lower-95% bound ≥0.98** and
**CPU/op upper-95% bound ≤1.02**. This noninferiority screen was necessary, not
sufficient; it is not equivalence or a latency improvement claim.

P0 required a p50 point ratio below one in **both** get100 cells. Its ratios were
**0.99984 [0.99839, 1.00191]** at 8 workers and
**1.00178 [0.99880, 1.00468]** at 32 workers. It failed that condition.
P2t required p50 point ratios below one in **all six** cells; every point ratio
was above one. It failed that condition too. Neither qualifies, independent of
any additional mechanism review. The final owner decision is **reject both**,
retain the 10/20 µs default and accept a data/tools/docs outcome only.

## Reproduction and evidence integrity

The [portable driver, configuration and method](../../../tools/benchcampaign/campaigns/rfc-264/local/README.md)
and the common `tools/clientbench` duration harness are retained in the repository.
These sources describe reproduction, not permission to rerun the screen. Raw
samples, binaries and operational metadata remain external. Content SHA-256
references for the completed evidence are:

- Generated four-group report: `651c4fb3f3715aa80ca389ba2f64429477011d158f0d7906f3d40c69235a55fa`.
- Summary with full-precision block values and CIs: `95e8f7ef9b5127a9a6a86b05abad9e4eed279ff2a0a6ce4e0f22a2c03ace03c0`.
- Completion/cleanup receipt: `c0ea863f15a5bc541de2fe9991beb909e7add73f8344b3ca5fbe91b0b4ee7f82`.
- Frozen configuration: `d082e265a7b6422a68a093031332c67ab67e260f24a5b2eb9608231e4aec061e`.
- Build manifest: `addc7f4eef6513887b7b8eb5dac1756ce64023204751ce0795348dd741be8dce`.
- External artifact manifest: `a5e649135c75499111364b92f7d6f10f74ac07f1be6c70e1dd55bf0dc6893f34`,
  covering **454 artifact files**, including all 72 raw runs, source patches,
  three measured binary payloads and metadata.
