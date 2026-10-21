# RFC-264: get/set latency bisect results

## Finding and scope

The historical local comparison between old Go
`267a41434a81723b07e7150869d702636a1b91a9` and new Go
`fa8725f1dcf6ff3ac66825ab6e3e819efbc0abcc` reproduced get100 at 8 workers
with a p50 endpoint ratio of **1.197 [1.180, 1.223]**. The largest adjacent
increase, and the only interval upward in all four primary blocks, was
**#954 (`590aa97e3..203560a9e`): 1.241 [1.217, 1.255]**. Every bracketed
range in this report is **four-block min/max, not a confidence interval**.

This local result **locates a source interval; it does not identify a unique
timer, dispatch or scheduling mechanism**. It is not a recoverable optimization
budget. The Hetzner multi-node 64-worker regression did not reproduce locally.
[RFC-264 is Rejected](../../../rfcs/264-send-wait-cost-and-ordering.md); the owner
retains #954's 10/20 µs default and accepts its latency-versus-CPU/allocation
trade-off. No further prototype or measurement follows from this report.

The motivating multi-node campaign measured new/old p50 ratios of **1.348
get100, 1.298 get1000 and 1.283 set100** at 64 workers, and **1.113, 1.154 and
1.137** respectively at 8 workers. The local comparison used a single-storage-
server, single-redundancy memory-engine cluster, not that three-node deployment.
Those historical results are not claims about current master.

## Compared inputs

All twelve binaries used **Go 1.26.9** and the same frozen M1 workload/measurement
harness from `d94b4c61377d173b7c6884fde426a227dab0f0bb`, with the #948 lifecycle-
drain fix and matching validator. The amended harness archive SHA-256 is
`76990b867f337bdad0132cc69aa3f6f9a9bcf0cb0859cc8c7f8514def684f508`.
There was no workload, pacing or measured-window difference between arms.
The anchors were the cleanup-compatible artifacts. The build manifest retains
source commits, exact amended harness inventories, compiler and binary hashes.

| Arm | Full source commit | Binary SHA-256 |
|---|---|---|
| old | `267a41434a81723b07e7150869d702636a1b91a9` | `c70e4d275d586f73430f0129c536a8fa6bcc950678c83d06a98f8c65a09f7e34` |
| #943 | `e21c7d4dcf5cc966cc4b4066c1d1697256aaaa31` | `6c51602db147458435f8ea6ea370a300cf12f6b91473b67f9e5455c6ffb029e3` |
| #948 | `066920ebb8d22c5618b31ef0a56fa7674c773cdb` | `94c0c227809d98d3b175e402321912e8c7a93889f4b3b9ba1bc7c85895755ffa` |
| #952 | `57ddc9a7773951dafbbb9669f819f2faf6f5d14d` | `78b3894b06198269c52dfc4796fd4b50960c4eb4b893339bb1f480afd90bf687` |
| #953 | `590aa97e326b612cf7768c3c14578d3470281443` | `c567fac1711198e15768097fdd654e4cde5a7d2e39e188e65f5c4bddceb424db` |
| #954 | `203560a9e21e57a3c561736423e5a27f0d443781` | `4a0f678481c8b2a6f4392dfcdc0225d8c51f85cd21fefde33b0ab9e9ddf08750` |
| #956 | `7d0f13860df22c1d9817d0d0cf73c64edaba204a` | `b3c825248ab33c9f82d4ae891769d98933f8d9a1b4bc1ff9fd1d7d88973edf72` |
| #966 | `34a94d93ffb5baf99498122c274a1c553eac1a6a` | `7a3ac54f0c884a9aaacc633e6c7c822f6f0637c8c4038dde99ff2925850f2eec` |
| #965 | `eecd839d99b2e7854e68b432552e1d6a4c333ae8` | `5e354a317d405e649e4fdce0719034766954cacb60f185377920611292bba272` |
| #967 | `0b0d1e71c67f3d1e92c26d62b8ed5af78056373c` | `0335e5a73522297cb7010eed08e9eaf79eab2f5e9f343d8425e0d7ad6ef74a6f` |
| #968 | `ae14b220883c4985031d5659f8b34cd7ecece8a9` | `79060b5b67c064c936324cd508f50a1cf1b6ea82a4476e2aa08404859813c111` |
| new / #969 | `fa8725f1dcf6ff3ac66825ab6e3e819efbc0abcc` | `187a282a8bb337a60ae88e5d54c227bed3bc79e1dc172e4184120df14408d252` |

The [source ledger](../../../tools/benchcampaign/campaigns/rfc-264/latency-bisect/client-commits.tsv)
covers all 19 `pkg/fdbgo` commits between the endpoints. Two spans deliberately
group changes: `e21c7d4dc..066920ebb` (#944/#945/#947/#948), and
`7d0f13860..34a94d93f` (#951/#961/#962/#964/#966, with record-layer-only
#963 and test/build-only #960 chronologically inside). A step in a grouped span
cannot be attributed to one PR. #943/#952/#953/#954/#956/#965/#967/#968/#969
have separate boundaries.

## Common method

Fresh processes used the frozen closed-loop M1 executable, not `testing.B`,
with no profiling, transaction timeout 30,000 ms, retry limit 10,
`GOMAXPROCS=8`, `GOGC=100`, `GOMEMLIMIT=off`, no `GODEBUG` override and client
CPUs disjoint from server CPUs. Counts were total across workers:
**12,000 measured + 800 warmup** at 8 workers, **32,000 + 1,600** at 64 workers.
Equal-length 68-byte prefixes isolated each process. Latency included retries,
materialization and exact checks; warmup was excluded.

A quiet start gate excluded detected build/container activity. Load was a start
gate, not a rule for censoring later samples. Valid runs required complete raw
counts/checks, persistence and lifecycle checks, source/binary/compiler identity,
uncached-GRV validation, unchanged inputs and verified owned-prefix cleanup.
All scheduled observations in the valid comparisons were retained; there was
no adaptive sampling, outlier deletion or partial-run comparison.

Per-process p50/p99 were empirical inverse-CDF quantiles at sorted index
`ceil(q*N)-1`. Comparisons are geometric means of four within-block arm/old or
adjacent later/earlier ratios; absolute displays are geometric means of process
quantiles. Processes were not pooled. **All ranges are four-block min/max, not
CIs:** no bootstrap or equivalence inference was used. Traffic, error rates and
saturation are not reported in these historical latency tables.

## Original anchor reproduction

The fixed original comparison completed **32/32 processes**: two anchors × four
cells × four blocks. Cells rotated by block; old/new and new/old arm orders each
occurred twice per cell. All validations passed and cleanup was verified.

The reproduction criterion required **each** cell to have a p50 new/old geometric
mean **≥1.03 at 8 workers or ≥1.10 at 64 workers**, plus at least three of four
paired-block ratios above one. P99 was descriptive. **Only get100/8 passed**,
so the all-cell gate failed and the broad original bisect was not run.

| Cell | Latency | Traffic | Errors | Saturation |
|---|---|---|---|---|
| get100/8 | p50 new/old 1.205 [1.179, 1.223]; p99 0.519 [0.369, 0.825]; reproduction pass (4/4 p50 ratios >1) | not reported | not reported | not reported |
| set100/8 | p50 new/old 0.997 [0.995, 0.999]; p99 0.536 [0.532, 0.540]; reproduction fail (0/4 p50 ratios >1) | not reported | not reported | not reported |
| get100/64 | p50 new/old 1.037 [1.000, 1.065]; p99 0.617 [0.436, 0.727]; reproduction fail (3/4 p50 ratios >1) | not reported | not reported | not reported |
| set100/64 | p50 new/old 0.969 [0.964, 0.977]; p99 0.661 [0.547, 0.820]; reproduction fail (0/4 p50 ratios >1) | not reported | not reported | not reported |

The get100/64 minimum rounded to 1.000 is exactly 0.9997231201589291.
Failure to reproduce is not equivalence or permission to relax the criterion.

## Narrow source-interval bisect

The separately fixed narrow comparison had **get100/8 p50 as primary** and
**get1000/8 p50 as exploratory secondary**, without redefining the primary.
It completed **96/96 processes**: twelve existing arms × two cells × four
blocks, each with 12,000 measured operations and 800 warmup, using the same
frozen binaries as the anchor comparison. Cells rotated left by block `b`; arms
rotated left by `(4*b + 2*cell_position) mod 12`, reversing in odd blocks.
This was not full factorial counterbalancing.

All validations passed, inputs were unchanged and cleanup was verified. All 96
measured CSVs were independently rehashed and their p50/p99 recomputed; every
value matched its process receipt. Endpoint p50 ratios were **1.197
[1.180, 1.223]** for primary get100/8 and **1.222 [1.191, 1.235]** for
exploratory get1000/8.

| Adjacent source interval | Latency | Traffic | Errors | Saturation |
|---|---|---|---|---|
| old → `e21c7d4dc` | primary get100/8 p50 0.950 [0.934, 0.963]; exploratory get1000/8 p50 0.957 [0.953, 0.961] | not reported | not reported | not reported |
| `e21c7d4dc` → `066920ebb` | primary get100/8 p50 0.998 [0.984, 1.015]; exploratory get1000/8 p50 1.005 [0.993, 1.015] | not reported | not reported | not reported |
| `066920ebb` → `57ddc9a77` | primary get100/8 p50 1.024 [0.994, 1.045]; exploratory get1000/8 p50 1.016 [0.974, 1.035] | not reported | not reported | not reported |
| `57ddc9a77` → `590aa97e3` | primary get100/8 p50 0.996 [0.974, 1.010]; exploratory get1000/8 p50 0.999 [0.984, 1.014] | not reported | not reported | not reported |
| `590aa97e3` → `203560a9e` | primary get100/8 p50 1.241 [1.217, 1.255]; exploratory get1000/8 p50 1.257 [1.232, 1.272] | not reported | not reported | not reported |
| `203560a9e` → `7d0f13860` | primary get100/8 p50 0.993 [0.970, 1.014]; exploratory get1000/8 p50 0.989 [0.961, 1.017] | not reported | not reported | not reported |
| `7d0f13860` → `34a94d93f` | primary get100/8 p50 0.988 [0.970, 1.013]; exploratory get1000/8 p50 0.974 [0.951, 1.003] | not reported | not reported | not reported |
| `34a94d93f` → `eecd839d9` | primary get100/8 p50 1.015 [0.997, 1.030]; exploratory get1000/8 p50 1.023 [0.981, 1.045] | not reported | not reported | not reported |
| `eecd839d9` → `0b0d1e71c` | primary get100/8 p50 0.994 [0.967, 1.012]; exploratory get1000/8 p50 0.999 [0.969, 1.030] | not reported | not reported | not reported |
| `0b0d1e71c` → `ae14b2208` | primary get100/8 p50 1.015 [0.997, 1.046]; exploratory get1000/8 p50 1.000 [0.977, 1.017] | not reported | not reported | not reported |
| `ae14b2208` → new | primary get100/8 p50 0.993 [0.963, 1.030]; exploratory get1000/8 p50 1.012 [0.983, 1.039] | not reported | not reported | not reported |

At #954, the primary raw block ratios were **1.247378, 1.255345, 1.245456,
1.217405**; the geometric mean was **1.241 [1.217, 1.255]**. The exploratory
cell was **1.257 [1.232, 1.272]** in the same interval. These are block
min/max ranges, not CIs.

Primary absolute p50 geometric means were **269,535 ns old**, **260,481 ns
immediately before #954**, **323,339 ns immediately after #954** and
**322,605 ns new**. Earlier improvements and other smaller changes leave the
net endpoint at 1.197 rather than the #954 interval's 1.241. Multiplying the
eleven adjacent geometric means recovers the endpoint to floating-point
rounding. Full process quantiles, block ratios, p99 diagnostics and CPU/
allocation/lifecycle data remain in the external evidence.

## C++ context and interpretation limits

#954 introduced the C++-parity positive connection-writer delay.
C++ 7.3.77 `fdbrpc/FlowTransport.actor.cpp:708–710` uses
`delayJittered(max(MIN_COALESCE_DELAY, MAX_COALESCE_DELAY -
(now - lastWriteTime)), TaskPriority::WriteSocket)` before a writer cycle.
`flow/Knobs.cpp:211–212` supplies **10/20 µs**, with approximately 0.9–1.1
jitter (`:54–55`; `flow/include/flow/flow.h:1481–1484`). The nominal positive
argument is therefore **9–22 µs per fresh writer cycle, not per RPC or an exact
wall-clock floor**. Followers can share a cycle; partial-write tails do not
start another interval (`FlowTransport.actor.cpp:714–732`).

The source interval also contains timer/direct-write/dispatch changes. These
measurements cannot separate requested delay from implementation, scheduler,
batching or handoff costs, or establish the cause of the multi-node 64-worker/
set100 differences that the original local gate did not reproduce. They do not
justify deleting the C++-parity policy. The [later local screen](local-quick-screen.md)
used a different base and Go 1.26.10; neither candidate qualified for promotion.

## Reproduction and evidence integrity

The [portable tools](../../../tools/benchcampaign/campaigns/rfc-264/latency-bisect/README.md)
retain both fixed comparisons, build/timing drivers, frozen validators and
offline checks. Portable paths are parameterized; expanded historical inputs
remain with the external evidence. Publication does not authorize another run,
and benchmark completion is not a correctness-suite pass.

Content SHA-256 references are:

- Original anchor plan: `72a5bc23927e6b2bd787bb1684546fc3782b25bf7224eef46b49a4c49ebbcb47`.
- Narrow-bisect plan: `c15d2f0f6547a5120869cefa233329e3cab4f070d84f4e35274c904187b002dc`.
- External artifact manifest: `f4c160f29ef5d943e57cbfc0b06d2d8c275c14104b2e35cc7c8e3683aa1dce4b`,
  covering **1,262 regular artifact files**, including source/build receipts,
  twelve binary payloads, raw valid comparisons, validators and pinned inputs.
  Python bytecode, Bazel caches and the root manifest itself are excluded.
  Raw samples and binaries are not committed.
