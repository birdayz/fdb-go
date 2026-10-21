# RFC-264: Send-wait cost and ordering-preserving reductions

**Status:** Rejected — results only; no production optimization is proposed.
**Reference:** FoundationDB C++ 7.3.77.
**Related:** RFC-259 (batching), RFC-260 (deferred native-commit redesign),
RFC-262 (native reads and fallback ordering), RFC-263 (transaction lifecycle).

## Decision

Retain the C++-parity **10/20 µs connection-coalescing default** introduced by
#954. The owner accepts the historical latency-versus-CPU/allocation trade-off.
Neither the historical bisects nor the local candidate screen justifies lowering
or disabling that default.

The completed local screen compared baseline, P0 and P2t in 72 processes. Both
candidates passed the throughput/CPU noninferiority bounds, but neither met its
intended p50 improvement condition. **Reject both candidates.** The decision
allowed at most one qualifying candidate; none qualified. There is no optimization
PR, further prototype, or further measurement authorized by this RFC.

The retained deliverable is the results, reproduction tools and common benchmark
harness. P0 and P2t production changes are not included. The previously assigned
zero-delay option comparison remains a separate deferred TODO, not an active
RFC-264 workstream. The remaining unbuilt ideas are not pursued under this
Rejected RFC.

## Historical findings

The historical endpoints were old Go
`267a41434a81723b07e7150869d702636a1b91a9` and new Go
`fa8725f1dcf6ff3ac66825ab6e3e819efbc0abcc`. These measurements describe those
revisions and their recorded toolchains, not current master. The historical
multi-node result report's content SHA-256 is
`4bb8c9ba6ed28f1ba94835463b9bcbe50fa278afc4b4a5e4abbaeea19e621eb9`;
the diagnostic-profile interpretation's is
`0fad929dbc8c6171ba1cfb80f79190317371bd3c447bb98dffb92ba135edfa4d`.

- The multi-node campaign's new/old p50 ratios at 64 workers were **1.348** for
  get100, **1.298** for get1000 and **1.283** for set100; at 8 workers they were
  **1.113**, **1.154** and **1.137**. Lower CPU and allocations did not eliminate
  those latency increases.
- The [local get/set bisect](../docs/benchmarks/rfc-264/latency-bisect.md), using
  Go **1.26.9**, reproduced get100 at 8 workers with an endpoint p50 ratio of
  **1.197 [1.180, 1.223]**. The largest adjacent increase, and the only interval
  upward in all four primary blocks, was #954 (`590aa97e3..203560a9e`):
  **1.241 [1.217, 1.255]**. The exploratory get1000/8 result in that interval was
  **1.257 [1.232, 1.272]**. These brackets are **four-block min/max, not CIs**.
  The Hetzner multi-node 64-worker regression did not reproduce locally.
- The [local mapped-fetch bisect](../docs/benchmarks/rfc-264/mapped-fetch-bisect.md),
  also using Go **1.26.9**, measured endpoint USE_REMOTE_FETCH ns/op ratios of
  **1.177 reverse** and **1.147 forward**. At #954, remote increased
  **1.178 [1.169, 1.192] reverse** and **1.174 [1.161, 1.190] forward**;
  ordinary SCAN_AND_FETCH increased **1.119 [1.090, 1.162]** and
  **1.126 [1.112, 1.146]**. At #968, remote was approximately flat
  (**1.000 / 1.009**), while ordinary improved (**0.864 / 0.863**). The later
  remote/ordinary ratio increase was principally a denominator improvement,
  not a newly measured absolute remote slowdown. These ranges are also
  **four-block min/max, not CIs**; testing.B ns/op means are not operation p50s.

These results **localize source intervals, not mechanisms**. #954 changed the
coalescing policy together with timer/direct-write/dispatch machinery. The
bisects do not separate requested delay, timer cost, scheduling, batching or
handoffs, and do not identify a recoverable latency budget. Shared-machine
profiles were diagnostic only, not timing or causal evidence. Existing queues,
flush acknowledgments and reply wakeups must not be attributed wholesale to
#968/#969; those mechanisms predated the interval.

## C++ coalescing policy

The active C++ 7.3.77 writer is timed, not yield-only:

```cpp
// fdbrpc/FlowTransport.actor.cpp:708–710
wait(delayJittered(
    std::max<double>(FLOW_KNOBS->MIN_COALESCE_DELAY,
                     FLOW_KNOBS->MAX_COALESCE_DELAY - (now() - lastWriteTime)),
    TaskPriority::WriteSocket));
```

With default, non-BUGGIFY, non-overridden knobs, the argument is
`max(10 µs, 20 µs - age)` (`flow/Knobs.cpp:211–212`). Jitter is approximately
0.9–1.1 (`flow/Knobs.cpp:54–55`; `flow/include/flow/flow.h:1481–1484`). A fresh
cycle therefore requests approximately **18–22 µs** at age zero and
**9–11 µs** after at least 10 µs idle time: **9–22 µs is the nominal positive
argument range per outer writer cycle**, not an observed wall-clock floor or a
per-RPC tax. One cycle can drain many frames; followers can share an existing
cycle. `lastWriteTime` is stamped at each write attempt
(`fdbrpc/FlowTransport.actor.cpp:716–718`).

Partial-write tails wait for writability and conditionally yield **without a new
coalescing interval** (`:714–732`); an empty peer waits on its trigger
(`:735–737`). Removing the idle minimum or adding another delay to a tail would
change policy. Net2's shared timer queue does not imply an OS sleep or switch per
batch (`flow/Net2.actor.cpp:1550–1565,1856–1863`), and cached time/timer resolution
mean nominal arguments are not exact observed waits. None of this source context
is a causal decomposition of #954's measured step.

The explicit [`WithCoalesceDelay(0, 0)` option](../pkg/fdbgo/client/options.go)
remains available. It is a latency/batching policy choice, not a policy-preserving
optimization or an isolated timer experiment.

## Candidates screened

Both candidates used base `68b8a64d274d5e5a041912934198ece79e7e009a`, Go
**1.26.10** and the same common duration harness:

- **P0:** probe an already-published PendingGet response before the existing
  connection-wide flush, under the existing lease and mutex; retain the original
  path if no response is ready. This targeted a pre-existing post-reply flush
  dependency, not a #968/#969 regression. Its intended p50 cells were get100 at
  8 and 32 workers.
- **P2t:** cache the timer's `syscall.RawConn` control wrapper, not a naked
  descriptor; retain `RawConn.Control`, timer syscalls, relative timing, jitter,
  Reset/Wait/Close and the positive coalescing policy. Its intended p50 cells
  were all six workload/worker cells. It could affect both coalescing and GRV
  timers, so an effect would not automatically be transport-only attribution.

The screen was four paired blocks, **ABC / BCA / CAB / ABC**, over grv/get100/
set100 at 8 and 32 workers: **3 arms × 6 cells × 4 blocks = 72 processes**.
Each process used 10 seconds of warmup and a nominal 30-second measured window;
rates used the actual timer-wake window. All 72 completed and cleanup was
verified. The [full screen report](../docs/benchmarks/rfc-264/local-quick-screen.md)
contains absolute values, baseline rows, identities and evidence digests.

## Local screen results

Each positive-metric entry below is **candidate/baseline geometric mean of four
paired-block ratios [95% CI]**. CIs use 20,000 whole-block percentile bootstrap
samples, seed 492817. They are distinct from the historical bisects' min/max
ranges. Lower latency/CPU ratios are better; higher traffic ratios are better.

All measured terminal errors and transaction-callback retries were zero in all
72 processes. Error/retry ratios are undefined, with no pseudocounts; error p99
is n/a because no arm/cell had the required 20 errors. `N` is pooled completed
logical operations. CPU/op is all-process user+system CPU in the measured window
per successful operation; cores busy is that CPU divided by window wall time.
The CPU stop snapshot preceded the boundary-work join and may include a small
concurrent tail. Retries are callback reentries, not internal RPC retries or
hedges.

| Cell / arm | Latency | Traffic | Errors | Saturation |
|---|---|---|---|---|
| grv/8 · P0 | p50 1.00186 [1.00074, 1.00273]; p99 1.00528 [1.00080, 1.00977] | ops/s 0.99778 [0.99702, 0.99911] | 0/1k errors, 0/1k retries (raw 0/0); N=4,467,462 | cores 1.00132 [0.99788, 1.00477]; CPU/op 1.00355 [0.99953, 1.00758] |
| grv/8 · P2t | p50 1.00421 [1.00172, 1.00671]; p99 1.01118 [1.00505, 1.01735] | ops/s 0.99530 [0.99258, 0.99803] | 0/1k errors, 0/1k retries (raw 0/0); N=4,456,346 | cores 1.00544 [0.99894, 1.01198]; CPU/op 1.01019 [1.00092, 1.01955] |
| grv/32 · P0 | p50 1.00090 [0.99904, 1.00278]; p99 1.00330 [0.99899, 1.00772] | ops/s 0.99930 [0.99717, 1.00172] | 0/1k errors, 0/1k retries (raw 0/0); N=16,314,961 | cores 1.00317 [0.99860, 1.00777]; CPU/op 1.00387 [0.99759, 1.01024] |
| grv/32 · P2t | p50 1.00282 [1.00196, 1.00382]; p99 1.00530 [1.00348, 1.00685] | ops/s 0.99717 [0.99577, 0.99857] | 0/1k errors, 0/1k retries (raw 0/0); N=16,280,127 | cores 1.00213 [1.00086, 1.00338]; CPU/op 1.00497 [1.00258, 1.00737] |
| get100/8 · P0 | p50 0.99984 [0.99839, 1.00191]; p99 1.00693 [1.00480, 1.00883] | ops/s 0.99981 [0.99790, 1.00115] | 0/1k errors, 0/1k retries (raw 0/0); N=2,991,076 | cores 1.00366 [0.99763, 1.00972]; CPU/op 1.00385 [0.99674, 1.01101] |
| get100/8 · P2t | p50 1.00317 [1.00129, 1.00479]; p99 1.01224 [1.00966, 1.01482] | ops/s 0.99639 [0.99494, 0.99800] | 0/1k errors, 0/1k retries (raw 0/0); N=2,980,833 | cores 1.00381 [0.99878, 1.00915]; CPU/op 1.00744 [1.00153, 1.01428] |
| get100/32 · P0 | p50 1.00178 [0.99880, 1.00468]; p99 1.01606 [1.00966, 1.02251] | ops/s 0.99766 [0.99471, 1.00070] | 0/1k errors, 0/1k retries (raw 0/0); N=9,748,679 | cores 1.00564 [1.00122, 1.00961]; CPU/op 1.00800 [1.00399, 1.01326] |
| get100/32 · P2t | p50 1.00746 [1.00501, 1.00913]; p99 1.01952 [1.00051, 1.03890] | ops/s 0.99240 [0.98987, 0.99565] | 0/1k errors, 0/1k retries (raw 0/0); N=9,697,213 | cores 1.00059 [0.99067, 1.00912]; CPU/op 1.00825 [1.00080, 1.01664] |
| set100/8 · P0 | p50 0.99984 [0.99956, 1.00018]; p99 0.99962 [0.99833, 1.00100] | ops/s 1.00013 [0.99991, 1.00038] | 0/1k errors, 0/1k retries (raw 0/0); N=953,460 | cores 0.99631 [0.98873, 1.00395]; CPU/op 0.99618 [0.98842, 1.00399] |
| set100/8 · P2t | p50 1.00001 [0.99977, 1.00031]; p99 1.00037 [0.99989, 1.00085] | ops/s 0.99989 [0.99966, 1.00012] | 0/1k errors, 0/1k retries (raw 0/0); N=953,227 | cores 0.99712 [0.99054, 1.00484]; CPU/op 0.99723 [0.99094, 1.00475] |
| set100/32 · P0 | p50 1.00011 [1.00005, 1.00016]; p99 1.00048 [0.99195, 1.00933] | ops/s 0.99980 [0.99932, 1.00033] | 0/1k errors, 0/1k retries (raw 0/0); N=3,758,239 | cores 1.00409 [0.99684, 1.01049]; CPU/op 1.00430 [0.99640, 1.01127] |
| set100/32 · P2t | p50 1.00019 [1.00001, 1.00036]; p99 1.00260 [1.00048, 1.00473] | ops/s 0.99939 [0.99914, 0.99967] | 0/1k errors, 0/1k retries (raw 0/0); N=3,756,717 | cores 1.00258 [0.99799, 1.00610]; CPU/op 1.00320 [0.99832, 1.00696] |

Both candidates passed every cell's throughput lower-95% bound **≥0.98** and
CPU/successful-op upper-95% bound **≤1.02**. That was necessary, not sufficient:

- **P0 failed its intended gain:** get100/8 p50 was **0.99984
  [0.99839, 1.00191]** and get100/32 was **1.00178 [0.99880, 1.00468]**.
  Its p50 point ratio was not below one in both intended cells.
- **P2t failed its intended gain:** its p50 point ratio was **above one in all
  six cells**.

Passing those noninferiority bounds is not equivalence, a latency win, proof of
the intended mechanism, or correctness approval. These results support the
rejection, not another optimization campaign.

## Retained evidence and reproduction

The full methods, source/binary identities and content-digest references are in:

- [Mapped-fetch bisect](../docs/benchmarks/rfc-264/mapped-fetch-bisect.md).
- [Get/set latency bisect](../docs/benchmarks/rfc-264/latency-bisect.md).
- [Local P0/P2t screen](../docs/benchmarks/rfc-264/local-quick-screen.md).

Repository-relative reproduction tools are under
[`tools/benchcampaign/campaigns/rfc-264/`](../tools/benchcampaign/campaigns/rfc-264/).
Their publication preserves the measurement method; it does not authorize a new
run or promote either prototype. Benchmark completion is not a correctness-suite
pass.
