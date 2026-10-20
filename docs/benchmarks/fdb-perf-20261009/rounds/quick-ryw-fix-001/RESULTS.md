# Short RYW correctness-fix measurement — iteration 001

## Source and scope

Remote source `/opt/fdbperf/sources/current-ryw-001` is measured baseline commit `26abdbaac12be60ea8955027f6047e10474596f1` plus client-only patch SHA256 `4ba1676f5330a3e2934c86fa6182c8eae077b89e96cd1a4ede845a6de074e7c6`, exported from the frozen `${FDB_GO_WORKTREES}/fdb-fix-ryw-local` worktree. All733 client source entries were checked against that local tree; all13 changed/new frozen client entries matched. Module files have no delta. `grv.go` is byte-identical to the original baseline: NO old GRV batching patch is included.

Candidate executable SHA256 `207d170cfb6a965a81e61a2403e4096a36a93c0eceda61135a4b9414285d8ca8`. Baseline Go `a009991c6e60aa92e83bfd1c914ec6070223a34180c67f142090bda62bafcab8`; CGo `af2e91489057be13a4b74c55c047b06de15d2483a981697523b076648f929dde`. Frozen v3 harness archive `41f594c990477bc4b29c565160599a57266cecb23adaae582180f7464fb2d0ea`; candidate harness files match baseline byte-for-byte. Original tested v3 runner and analyzer are unchanged.

FDB7.3.77/API730, Go1.26.9, same existing virtual machine and SSD-backed local server. GOMAXPROCS8, client CPUs8–15, FDB2–7, collector0–1. Same2second cooldown and1second group/per-CPU quiescence gate. Every arm is a fresh process;100warmup and300measured operations. No build/test/profile ran concurrently with either comparison. Pure-Go binaries do not load a native FDB library. Each native arm loaded libfdb_c SHA256 `e4ad7fed7b7580bf5f7b38f0a4279d37e698f69d534182e5de0b8beeef6cd1b1`.

Root reports the local client/native correctness targets passed; local full-suite status was148/149 with a separate vector timing failure at authorization. Remote checks here are the frozen standalone harness unit tests plus exact raw-operation/value/persistence/counter checks, NOT a claim of a remote full-suite rerun.

## Get1000: no improvement established for remote reads

`quick-ryw-fix-get1000-001`:6blocks, all6 three-arm permutations once,18arms/5400measured operations,69seconds including cooldown/setup/analysis. All operation/persistence checks passed; zero errors or callback retries. Pure-Go cache hits were zero and measured read-version/batch counts were3600 across both Go arms. Native counters remain unobserved.

| Arm | Pooled mean µs | Pooled p50 µs | Pooled p99 µs | Median run process CPU µs/op | Median run Go heap B/op | Median run Go allocations/op |
|---|---:|---:|---:|---:|---:|---:|
| Original Go | 1655.03 | 1632.94 | 2193.38 | 895.50 | 8045.12 | 82.60 |
| RYW-fixed Go | 1672.23 | 1640.08 | 2392.48 | 908.70 | 8167.27 | 88.49 |
| Official CGo | 709.42 | 703.59 | 1009.59 | 549.32 | 1528.00 | 16.00 |

Geometric mean of paired RUN-level mean ratios: current/original Go1.01047 (six observed pair ratios0.99337–1.03071); current/CGo2.36765; originalGo/CGo2.34312. Thus this correctness fix does not show a speedup on this non-RYW workload. The roughly1% observed difference is NOT asserted as an established regression. Paired current/original run-p99 ratio1.22279 varies0.98951–2.35717 across pairs; small-N tails are diagnostic, not a stable tail estimate.

## Local RYW: strict zero-GRV behavior passes

`quick-ryw-local-001`:6paired blocks, each order3times,12arms/3600measured operations. The operation is Set/Get/Cancel, not a persisted write transaction. All exact-byte and fresh-transaction persistence checks passed, zero errors/callback retries. Across1800 measured current-Go operations:0GRV batches,0read versions,0commits,0GRVcache hits. Native RPC counters are UNOBSERVED, not measured zero.

| Arm | Pooled mean µs | Pooled p50 µs | Pooled p99 µs | Median run process CPU µs/op | Median run Go heap B/op | Median run Go allocations/op |
|---|---:|---:|---:|---:|---:|---:|
| RYW-fixed Go | 28.682 | 11.169 | 311.627 | 54.587 | 36553.76 | 43.06 |
| Official CGo | 93.724 | 80.009 | 246.617 | 120.998 | 600.00 | 14.00 |

Paired RUN-level Go/CGo ratios: mean0.30709 (~3.26x faster mean for THIS local control), p500.14019, p991.17130. This is NOT a tail-latency win or a general claim Go is faster. Go process CPU is lower in this control; Go heap allocation is high. Native C++ heap is excluded from Go allocation counters, so these are not total-memory comparisons. Process CPU covers every process thread and can exceed per-operation wall time because work overlaps across threads.

The historical original-Go strict RYW control FAILED its zero-GRV guard; it remains a failed diagnostic with persisted validation not run. Its latency must not be used to invent a fresh paired speedup estimate. The strict v3 guard was NOT relaxed for this run.

## Fresh bounded diagnostic profiles (excluded from comparisons)

`current-ryw-profiles-001`:CPU10000operations; block2000operations,100warmup each. Both summary/raw checks passed, zero cache hits. Profiles are not headline timings.

- Block profile: total caller transaction blocked delay3.07s; getReadVersion/ensureReadVersion2.78s; sendGRVRequest0.39s; PendingGet.Resolve0.30s. This re-establishes GRV admission/waiting as the main sequential-read wait, consistent with the unchanged batching timer. These are nested/concurrent cumulative waits; do not sum/subtract them as an exact additive latency decomposition. Parent WaitGroup and background loop waits are not additional transaction latency.
- CPU profile: duration16.56s,5.50s sampledCPU. Flat Syscall6:1.66s/30.18%; futex1.28s/23.27%; runtime.write1:0.19s/3.45%. Scheduler/wakeup/netpoll paths remain prominent. This does not establish that reducing every syscall or wakeup is semantically safe.

## Local-RYW allocation diagnosis (also excluded from comparisons)

`current-ryw-alloc-profiles-001`: heap50000operations measured0.619s; CPU100000operations measured1.436s,1000warmup each. All150000operations passed exact raw/summary/value/persistence checks with0errors/retries/GRV/cachehits/commits. These longer diagnostic windows are not substitutes for the short paired measurements above.

Lifetime sampled alloc_space1822.35MB: `client.init.func14`1600.93MB/87.85%, via `conflictBufAlloc → addWriteConflictForKeyLocked → stateSet`. The exact source explains the size: `transaction.go:1287–1290` conflictBufPool.New allocates a32768-byte backing buffer; conflictBufAlloc takes it at2918. Cancel at2051 does not return it; postCommitResetFields returns the owner at3502–3509. Set/Get/Cancel therefore allocates/discards a32KiB buffer peroperation instead of reusing the successful-commit pool path. Returning it from Cancel would require proving no concurrent/inflight operation still references its slices; this profile is not authorization for an unsafe lifetime shortcut.

CreateTransaction itself is only2.50% of flat sampledbytes, rywCache.set2.69%. Sampled alloc_objects instead highlights enterState15.79%, rywCache.set9.19%, opContext8.52% flat. Heap reports cover process lifetime INCLUDING setup/warmup; measured MemStats deltas separately show36523.94B/op(heaprun),36522.34B/op(CPUrun).

CPU profile2.91s samples across threads: tryDeferToSpanScan14.09%, typePointers.next13.75%, memclrNoHeapPointers8.93% flat; scanObject35.40% cumulative. The profiled100koperations triggered169GC cycles. This supports substantial allocation/GC work in the now-network-free RYW path; it is not a native-memory comparison or a fresh latency speedup claim.

## Interpretation limits

These are short, adaptive hillclimb iterations on one co-located VM/server, not independent hosts. No physical SKU or whole-host isolation is asserted. Six run blocks/1800operations perarm are insufficient for publication-grade tail claims; bootstrap output in analysis.json is exploratory only. Repeated iteration/selection bias is not hidden. Pooled percentiles are descriptive; paired ratios compare run-level metrics rather than ratios of pooled percentiles. The earlier24-block baseline remains a separate historical experiment with different run lengths/warmup. Next production delta must be supplied/tested by root; no old batching candidate was applied.
