### Why fixing contention alone failed

The combined no-cleanup arm passes the mutex criterion but fails CPU:

| Workload, 8 workers | Baseline µs/tx | Combined µs/tx | Median change | Paired changes | 32-worker queue wait/CPU |
|---|---:|---:|---:|---|---:|
| get100 | 49.79425 | 48.89865 | −1.80% | −3.32%, −3.23%, +2.58% | 0.3782% |
| set100 | 57.9325 | 54.89895 | −5.24% | −5.62%, −4.19%, −5.29% | 0.0390% |
| range100 | 94.1853 | 96.58925 | +2.55% | +2.94%, +2.01%, +3.61% | 0.1577% |

Range arm ranges do not overlap at 8 workers. At 32 workers, three predeclared alternating
unprofiled 500,000-transaction range pairs give 59.342842 → 65.34993 µs/tx medians
(**+10.12%**), paired +11.04%, +8.69%, +10.92%, again nonoverlapping arm ranges. The
single profiled range pair also regresses (60.144206 → 66.205292 µs/tx). It is a real
CPU failure, not rescued by low queue wait or four fewer allocations.

**Same-binary attribution, separate from acceptance timing.** An untimed 32-worker
range CPU-profile pair, 500,000 measured + 2,000 warmup each, uses the exact archived
baseline and combined executables and the same settings. `pprof -diff_base` reproduces
60.382224 → 66.3054 µs/tx rusage. The complete sampled profiles total 30.12 → 33.09 CPU-s.
An exclusive stack partition assigns every sample exactly once, in this precedence:
GC mark/assist/sweep → write barriers → timeout/cleanup administration → other allocation
→ remainder. It accounts for all 1,844/2,025 sample records:

| Exclusive group | Baseline CPU-s | Combined CPU-s | Delta CPU-s | Delta µs/tx |
|---|---:|---:|---:|---:|
| GC mark/assist/sweep | 4.00 | 5.84 | +1.84 | +3.68 |
| Mutator write barriers, excluding above | 1.14 | 2.04 | +0.90 | +1.80 |
| Timeout/cleanup administration, excluding above | 1.17 | 0.95 | −0.22 | −0.44 |
| Other allocation stacks | 4.14 | 4.76 | +0.62 | +1.24 |
| Remainder, including unknown stacks | 19.67 | 19.50 | −0.17 | −0.34 |
| Total | 30.12 | 33.09 | +2.97 | +5.94 |

The +5.94 sampled µs/tx agrees with +5.923 rusage. `gcDrain` adds 1.93 s cumulative;
its nested `tryDeferToSpanScan` (+1.70 s), `scanSpan` (+1.32 s) and scanner helpers are
not independent costs to add. Flat changes include `tryDeferToSpanScan` +0.99 s,
`spanClass.sizeclass` +0.67 s and `scanObjectsSmall` +0.45 s. Only 0.02 s of the 0.90 s
barrier increase is on queue stacks; 0.88 s is elsewhere in mutator work. Removing heap
pointer-write barriers alone is not the six-microsecond answer. Retained pointer-bearing
state increases marking work and the period during which range's allocation/copy work
runs with write barriers active. The short 8-worker range failure occurs before the first
10 s interval, so interval callback churn cannot be its sole cause.

**Population and GC confirmation.** A separate diagnostic-only build reads runtime
metrics once per second and samples physical shard lengths under each shard lock. It
never scans/promotes transaction weak pointers. The same generic sampler is used in a
baseline diagnostic build. These builds support population/GC observations, **not CPU
performance claims**. Counts are weak queue entries, not strongly reachable transactions;
totals are sequential per-shard snapshots, and 1 Hz peaks are lower bounds.

| Seconds | Combined created nodes | Physical queued nodes |
|---:|---:|---:|
| 1 | 30,007 | 30,007 |
| 5 | 193,870 | 193,870 |
| 10 | 386,648 | 386,648 |
| 11 | 426,022 | 396,016 |
| 12 | 465,598 | 395,211 |
| 13 | 502,002 | 389,877 |

By 13 s the collector has observed 112,124 nil weak pointers and **zero non-nil ones**.
It retains roughly 10 s of completed transaction history, not 32 in-flight handles.
The sampled peak is about 31.7 MB of 64-byte nodes + 16-byte weak handles, plus 3.80 MB
of heap-pointer payload (475,136 slots). An independent archived-binary heap profile,
forced GC at measurement end, estimates 393k retained nodes and 426k weak handles; those
are sampled estimates, not an exact in-run census. Full transactions are not retained.

Archived-binary `gctrace=1` runs select cycles ending in the measured window using the
harness's before/after NumGC snapshots:

| 32-worker range, 500k | Baseline | Combined without cleanup |
|---|---:|---:|
| GC cycles | 216 | 179 |
| Total GC CPU, including idle marking | 3.568 s | 6.110 s |
| Mean GC CPU/cycle | 16.52 ms | 34.13 ms |
| Largest post-GC live-heap report | 120 MiB | 174 MiB |

Fewer cycles did not mean less GC CPU. The diagnostic available-CPU GC fraction is
about 5.6% baseline versus 8.0% combined. Its numerator excludes idle marking, as
`GCCPUFraction` does; total GC CPU includes idle marking because that is still real
process CPU. Gctrace figures are rounded and the first selected cycle may start before
the measured window. RSS high-water fields were flat even at identity, so they provide
no useful retention comparison. Raw traces, profiles and conservation-checked analysis
scripts are retained (`pprof/exclusive-cpu-analysis.*`, `gctrace-analysis.json`,
`population-analysis.json`).

### Selected variant (b): reclaim at proven unreachability

The selected arm adds one GC cleanup registration per transaction object, not per arm,
to the sharded lower-bound/frontier design below. Cleanup unlinks the node by index under
its leaf shard lock. It does not cancel a retained handle or recycle storage into a new
owner. This restores two cleanup allocations; creation is **11 → 9**, rather than the
no-cleanup arm's 11 → 7. The performance objective is CPU, not maximum allocation cuts.

Three fresh alternating primary pairs per workload, same opt toolchain/harness/settings:

| Workload, 8 workers | Baseline µs/tx | Selected µs/tx | Median change | Paired changes | 32-worker queue+root wait/CPU |
|---|---:|---:|---:|---|---:|
| get100 | 50.0313 | 48.3165 | −3.43% | −6.50%, −1.50%, −3.43% | 0.01725% |
| set100 | 57.3486 | 55.13825 | −3.85% | −3.85%, −4.36%, −3.69% | 0.000511% |
| range100 | 95.66025 | 94.1891 | −1.54% | −1.54%, −0.93%, −3.62% | 0.01760% |

All nine primary pairs are favorable. Three alternating unprofiled 32-worker range pairs,
500,000 measured + 2,000 warmup each, give **59.855936 → 58.953094 µs/tx**, −1.51%;
paired −1.07%, −1.51%, −0.70%. Arm ranges overlap (baseline 59.172364–60.04102,
selected 58.760324–59.39971), so this is not a claim beyond the observed screen. The
single mutex-profiled 32-worker range pair is also favorable (60.662518 → 59.122064).
The wait numerator counts producer schedule, cleanup `remove`, shard drainer and root
mutexes at exact unlock source lines, excluding transaction/runtime locks. No sampled
drainer contention in get/set is a valid zero category, not an empty-profile pass:
completed populations and total identified queue samples are nonempty. All 30 runs,
including unfavorable spreads elsewhere, remain retained. Selected-run load average is
6.36–8.49 and idle snapshots 0.969–0.993; no overlapping build is accepted.

GC and population validation is separate from these acceptance timings:

| Archived-binary range diagnostics | Baseline | Selected |
|---|---:|---:|
| 8 workers / 20k: GC cycles | 189 | 191 |
| 8 workers: total GC CPU | 0.2694 s | 0.2719 s |
| 8 workers: mean GC CPU/cycle | 1.425 ms | 1.423 ms |
| 32 workers / 500k: GC cycles | 214 | 213 |
| 32 workers: total GC CPU | 3.555 s | 3.470 s |
| 32 workers: mean GC CPU/cycle | 16.613 ms | 16.291 ms |
| 32 workers: largest post-GC live report | 122 MiB | 120 MiB |

The separate diagnostic sampler observes **741–2,646 queued nodes**, not the previous
~396k plateau; heap-pointer capacity is 4,592 rather than 475,136. Peak sampled node +
weak class bytes are 211,680 plus 36,736 bytes of heap-pointer payload. This tracks
in-flight transactions **plus a GC/async-cleanup cohort**, not literally only 32 active
transactions; cleanup provides no immediate-reclamation guarantee. The independent
available-CPU GC fraction over each sampler's first-to-last interval is 5.757% baseline
and 5.718% selected. Total GC CPU counter deltas are 3.386 and 3.396 s—near baseline,
not a claim of a diagnostic CPU win. Per-cycle GC cost is back near baseline at both
worker counts. `memory4-gctrace-analysis.json` and `reclaim-population-analysis.json`
retain the exact interval/rounding/snapshot scope and all sampled curves.

### Rejected ownership/lazy shortcuts and parked noscan option

**Callback return is not an ownership endpoint.** `Database.Transact` passes an actual
`*Transaction` to user code and returns its arbitrary result unchanged after commit
(`database.go:974-1003`); the callback can return or capture the handle. After its due
successor delivery, a new `GetVersionstamp` must see 1031 ahead of the committed stamp
(`transaction.go:2175-2201`). Disarming/recycling merely on successful commit or callback
return changes that result. This preserves the Go post-commit extension, not an assertion
that modern C++ creates that successor. The cleanup uses proven unreachability instead.

**Pure lazy-on-use registration is invalid.** A live idle handle or already blocked
operation still needs autonomous delivery (RYW:1567-1578). Making its first late admission
an expired arm records 1031 synchronously where a pending, undispatched timeout could
still be replaced without a cause (C3/C4, revision 1's rejected boundary). A staging
horizon can be legal only if it immediately retains the original logical deadline,
requested wake, replacement identity and FIFO ticket; promotion is bookkeeping, not a
new arm. Preserve the interval phase and fresh tickets on actual continuations. Such
staging may change representation, but cannot justify zero registration/state.

**Noscan heap/slab (a) is parked, not a measured competitor.** A value heap such as
`{wake, slotIndex, slotGeneration}` can remove scanning of heap-array pointers; slab state
can reduce separate pointer-bearing node objects. Weak handles still need a safe GC root,
and slab/free-list high-water capacity and larger value entries must be counted. Do not
hide weak pointers in uintptrs. The partial source uses 24-byte heap values, 64-byte state
with one weak-handle pointer, and larger immutable ready records. Its 16 top-level probes
were **not executed**; no binary or performance claim exists.

It has an unresolved owner-alias hazard: Go 1.26.9 weak `Value` becomes nil when a finalizer
is queued (`src/weak/pointer.go:80-84`), before an old owner is irretrievably gone. A
finalizer can resurrect A after A's slab slot has been reused for B. A's old interior
`timeoutNode` pointer can then mutate B's state under A's lock; heap/ready generation
checks do not protect that producer access. Safe recycling requires cleanup-authorized
ownership release, or an independent immutable owner identity on the transaction checked
before every access, with a sound renewal/exclusion protocol. A generation read inside
the recycled state alone is insufficient. Go cleanup waits for finalization and another
unreachability (`runtime/mcleanup.go:43-44`), and selected (b) does not recycle nodes.
The partial source archive/patch and hazard are retained, verified byte-for-byte, and its
worktree was removed. Reopen only as a separately reviewed future option if measurements
justify its complexity; do not add it to the selected implementation for speculative gains.
