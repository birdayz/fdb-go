# Final short controls and Read1000 diagnostics — grv-idle-001

## Frozen candidate and authorization

Root request #333 authorized the final Get100/Range10 short campaign followed by exactly one 10,000-operation Read1000 CPU profile and one 10,000-operation Read1000 heap profile. All are complete; no further campaign or profile is authorized by that request.

- Base revision: `26abdbaac12be60ea8955027f6047e10474596f1`.
- Whole-client patch SHA-256: `32d59b91cdd509ba0cb985f88f52a2d62fdae4ad0020f832183ba79555e024e3`.
- Frozen Go binary `candidate-grv-idle-001-go`: `5b6e7302f977785317a83e4d9172555c48fe400f9362536e7fe8436d0b9c355b`.
- Unchanged CGo binary `baseline-cgo`: `af2e91489057be13a4b74c55c047b06de15d2483a981697523b076648f929dde`.
- Frozen v3 harness archive: `41f594c990477bc4b29c565160599a57266cecb23adaae582180f7464fb2d0ea`.
- Go 1.26.9; FDB server and native client 7.3.77; API 730.

The source, harness, workload semantics, transaction options, runner and analyzer were not changed for these runs. Timeout 30,000 ms, retry limit 10, writes explicitly enabled. Get100 uses Transact/Get/empty Commit. Range10 checks exactly 10 ordered rows of 8-byte values with WantAll and a limit of 11. Read1000 uses ReadTransact without an empty commit. Sizes are decimal bytes.

## Final short campaign

Artifact: `artifacts/quick-grv-idle-final-controls-001`; seed 391715. Six counterbalanced paired blocks for each workload, 300 measured + 100 warmup operations per fresh process, 24 arms and 7,200 measured operations total; approximately 84 seconds including cooldown/quiescence. Candidate and CGo each contribute 1,800 measured operations per workload. Housekeeping/collector CPUs 0–1, FDB 2–7, client 8–15, GOMAXPROCS=8. Same co-located single-SSD FDB VM, no physical-host isolation claim. No concurrent build, test or profiling job ran during timing. A deliberate 1 Hz /proc collector was active.

Ratios below are geometric means of paired **run-level** ratios, candidate/CGo. Pooled percentiles are descriptive, not independent-op inferential estimates.

| Workload | Paired mean ratio | Paired p50 ratio | Paired p99 ratio | Go pooled mean / p50 / p99 (µs) | CGo pooled mean / p50 / p99 (µs) |
|---|---:|---:|---:|---|---|
| Get100 | 0.621853 | 0.613269 | 0.707715 | 457.083 / 444.227 / 766.755 | 734.493 / 719.965 / 1164.892 |
| Range10 | 0.713844 | 0.714737 | 0.830540 | 537.038 / 526.955 / 879.744 | 757.027 / 771.274 / 1063.222 |

All six paired mean comparisons favor the candidate for each workload. These are approximately 37.8% and 28.6% lower paired mean latency respectively, only for the measured workload/environment.

| Workload | Median process CPU Go / CGo (µs/op) | Paired CPU ratio | Go-runtime allocation Go / CGo (B/op) | Go-runtime allocations Go / CGo (objects/op) |
|---|---|---:|---|---|
| Get100 | 526.772 / 550.008 | 0.922785 | 6847.20 / 616.05 | 84.763 / 15.998 |
| Range10 | 573.428 / 578.118 | 0.994767 | 10633.36 / 2472.05 | 95.047 / 44.000 |

CGo's Go-runtime allocation figures exclude the native C++ heap and must not be treated as total native allocation.

### Correctness and path controls

All measured and warmup exact-value/row checks passed. Fresh-transaction persisted validation passed for every arm. All quiescence gates passed. Errors and callback retries were zero. Each workload's candidate arms recorded 1,800 read versions and 1,800 successful GRV batch replies, zero cache hits, zero commit starts/completions, zero transaction retries and zero connection failures. The exact native library hash was verified in all CGo arms. Native RPC/cache counters remain **unobserved**, not zero. See `counter-path-validation.json`, the per-arm manifests/summaries, raw sample CSVs and collector records.

## Read1000 CPU and heap diagnostics

Artifact: `artifacts/grv-idle-read1000-profiles-001`. Separate fresh-process CPU and heap runs, sequential, 100 warmup and 10,000 measured operations each; approximately 18 seconds overall. These diagnostic runs are not timing-comparison arms.

Both runs passed exact-value and fresh-transaction persisted validation, completed all 10,000 measured operations, and recorded 10,000 fresh GRVs, zero cache hits/commits/errors/retries/connection failures each. Binary pins were checked before and after.

### Allocation hotspot confirmed

The heap profile's process-lifetime `alloc_space` estimate totals **383.52 MB**. `client.init.func14`, the 32 KiB `conflictBufPool.New` allocator, accounts for **318.23 MB / 82.98%**. Its path is:

`Database.ReadTransact → GetPipelined → addReadConflictForKeyRYW → addReadConflictForKey → conflictBufAlloc → conflictBufPool.New`.

Other flat sites: FrameReader.Read 14.51 MB (3.78%), CreateTransaction 8.01 MB (2.09%), bytes.Clone 6 MB (1.56%). The sampled allocation-object total is 711,687; GRV getReadVersion accounts for 20.17%, enterState 9.21%, bytes.Clone 7.98%.

Measured MemStats allocation deltas independently show 388,693,808 B / 10,000 = **38,869.38 B/op** for the CPU run and 388,593,704 B / 10,000 = **38,859.37 B/op** for the heap run. The lifetime sampled profile includes process setup and warmup, unlike the measured MemStats delta. The helper forces a GC before writing the heap profile. These figures describe allocated bytes, not live retained heap, and no native heap was profiled.

Read-only source inspection is consistent with the allocation contrast: `client/database.go` ReadTransact returns after its callback without Commit/Cancel/Reset, whereas Transact calls Commit. The 32 KiB conflict owner is returned in postCommitResetFields, not Cancel. This identifies an optimization candidate, **not** proof that simply pooling on cancellation or adding cleanup is safe. Pending-operation and aliased-buffer lifetimes need a separate correctness proof before any change.

### CPU remains syscall/scheduler heavy

CPU-run measured time 3.918668 s. pprof duration 3.92 s, 3.58 s of samples: Syscall6 1.24 s (34.64%), futex 0.82 s (22.91%), memmove 0.12 s (3.35%) flat. Unlike the earlier local-RYW control, this network-read CPU profile is not primarily GC work; syscall/scheduler paths remain substantial. Flat syscall samples are not a causal network-wait decomposition.

Heap-run measured time 4.325682 s. Each run's measured interval had six GC cycles. Process lifetime high-water RSS was 102,072 KiB (CPU run) and 104,888 KiB (heap run); this is not live heap size or proof of a memory leak.

## Updated correctness status (append-only addendum)

Root request #333 reported the subsequently completed full103 correctness lane **GREEN: 150/150, 95 actually executed and 55 cached, 1824.574 seconds**. Root also verified all 6,813 source hashes. Root states that the current production client runtime is unchanged from the measured frozen candidate except comments. This is a root-reported correctness result; this measurement agent did not rerun that suite remotely.

Earlier immutable archives truthfully retained pending/failed status at their respective authorization times (including the earlier full91 recordlayer polling-fixture failure). This addendum does not alter those archives or retroactively claim they were green when created. The measured source/binary remains the separately pinned immutable candidate, not an unrecorded rebuild of root's current tree.

## Cross-campaign scope

Across four short adaptive campaigns, paired mean candidate/CGo ratios were: Get100 0.62185, Get1000 0.71408, Get10000 0.67654, Read1000 0.75861, Set100 0.88621, Range10 0.71384, Range100 0.64269. Thus all seven original workload means favor this candidate in their respective short campaigns. These were **not one preregistered confirmatory trial**, and adaptive selection plus six paired blocks limits inference. Tail behavior is mixed across campaigns (notably Get1000 pooled p99 and individual Read1000 p99 pairs); do not claim a universal tail or capacity win. Read1000 still had a paired process-CPU ratio of approximately 1.219 versus CGo in its timing campaign. Retain every raw run, including historical failures, and treat small-N tails/bootstrap outputs as exploratory.

No VM lifecycle change or additional benchmark/profile is part of this package. Resource disposal remains root's responsibility after durable-backup confirmation.
