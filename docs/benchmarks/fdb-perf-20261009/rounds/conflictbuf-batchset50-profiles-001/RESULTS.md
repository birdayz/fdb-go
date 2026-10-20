# New512-v5 batchset50 CPU and heap diagnostics

Root authorization #352: exactly one CPU profile then one heap profile of the immutable new512-v5 candidate, each 10,000 measured transactions and 100 warmup transactions, workers1. No comparisons, rebuilds or additional workloads/profiles. Both completed successfully, approximately52seconds overall including symbol analysis. Each measured transaction contains50distinct100-byte Sets and one Commit: **20,000 measured commits /1,000,000 Sets** across both runs, plus200warmup commits/10,000warmup Sets and untimed fixtures.

## Measured resources (not comparative timing results)

| Profile | Measured wall | Total child wall incl setup/validation | Measured allocation B/transaction | Measured allocation count/transaction | Measured process CPU µs/transaction | Measured GC cycles | Measured GC pause | Process lifetime high-water RSS |
|---|---:|---:|---:|---:|---:|---:|---:|---:|
| CPU | 23.010316s | 23.613870s | 106653.2896 | 460.2629 | 981.0071 | 11 | 2.165152ms | 293620KiB |
| Heap | 20.410002s | 21.061163s | 106651.7048 | 460.2564 | 974.1931 | 11 | 1.509119ms | 288184KiB |

Measured allocated-byte deltas are exactly **1,066,532,896** and **1,066,517,048** bytes respectively. Measured CPU is process getrusage user+system delta; the CPU profile samples are a separate sampled observation, not an exhaustive decomposition of that delta. These are diagnostic executions with profiling enabled, not latency-comparison arms. No latency improvement may be inferred by comparing these durations to the earlier100-operation timing arms.

**Setup matters:** v5 pregenerates 5000payload bytes per operation. At10,000measured+100warmup, prepared payload storage is50,500,000bytes before slice/allocator overhead, versus700,000bytes for the earlier100+40budget. This larger live fixture changes heap/GC conditions even though preparation is outside timing. The lifetime heap profile attributes46.24MB (4.27%, sampled pprof display units) to batchSet50Payload/PrepareBatchSet50, explicitly setup rather than measured allocation. Do not treat the lower GC-per-operation rate or different resource cost as a new short-campaign result. High-water RSS includes the entire process and is neither live retained heap nor proof of a leak.

## Heap: remaining allocation is RYW maps and commit snapshots, not the buffer pool

Lifetime sampled alloc_space total: **1082.06MB** (pprof display units). It includes setup, warmup, measured activity and profile lifecycle; it is not the measured MemStats delta and should not be divided by measured operations as if interval-exact. The helper forces a GC before writing the heap profile. No native heap profile was taken.

| Flat allocation site | Sampled allocation | Share | Critical source/call path |
|---|---:|---:|---|
| rywCache.set | 383.71MB | 35.46% | 181.82MB from initial stateSet;201.89MB from coalesceCommitMutations replay |
| Transaction.captureCommit | 240.34MB | 22.21% | commitAdmitted→captureCommit; owned arena and mutation/conflict vectors |
| Transaction.addWriteConflictForKeyLocked | 82.23MB | 7.60% | stateSet→append conflict-vector entries |
| rywCache.materializeCommit | 79.10MB | 7.31% | coalescing→materialize; vector and reconstructed key bytes |
| Transaction.stateSet | 75.21MB | 6.95% | mutation-log storage |
| coalesceWriteConflicts | 47.12MB | 4.35% | invoked from coalesceCommitVectors and finalizeShipConflicts |
| batchSet50Payload | 46.24MB | 4.27% | **untimed setup** |
| finalizeShipConflicts | 22.56MB | 2.08% | snapshot/final write-conflict processing |

The first five **flat**, nonoverlapping rows account for79.53%. Cumulative figures overlap their callees and must not be added to these flat totals. conflictBufAlloc is only **6.02MB cumulative /0.56%**;5.50MB flows through the new small-buffer allocator (client.init.func14), with approximately0.52MB through the large-pool allocator on the setup read path. The initial32KiB allocation hotspot from Read1000 is not this batch profile's dominant cost.

### Critical allocation stacks and source annotations

All citations refer to the frozen source, not root's subsequently edited test tree.

1. `Database.Transact → Dataset.Operation callback → Transaction.stateSet → rywCache.set`:181.82MB sampled through set. `pkg/fdbgo/client/ryw.go:223` accounts for96.01MB value copies across initial+replay paths;`:229` accounts for287.70MB map/string-key assignment. The set source explicitly explains its defensive-copy lifetime requirement.
2. `Transaction.Commit → commitAdmitted → coalesceCommitVectors → coalesceCommitMutations`:291.50MB cumulative (26.94%). `ryw.go:336–348` creates a throwaway RYW map and replays the validated snapshot:201.89MB through `wm.set` at`:341`,86.61MB through materializeCommit at`:348`. The parent coalesceCommitVectors totals318.06MB (29.39%) including conflict coalescing.
3. `commitAdmitted → captureCommit`:240.34MB flat. `pkg/fdbgo/client/commit_input.go:57` owned byte arena accounts for183.69MB;mutation/read-conflict vectors at`:41`31.09MB;write-conflict vector at`:42`24.06MB;commitInput object at`:38`1.50MB.
4. `coalesceCommitMutations → materializeCommit`: `ryw.go:299` output mutation vector32.60MB;`:308` string→[]byte key copies46.50MB;`:305` sorted-key preparation7.51MB cumulative.
5. Conflict coalescing receives26.56MB cumulative from coalesceCommitVectors and23.06MB from finalizeShipConflicts; its47.12MB flat allocations are largely fresh range-slice copies. These are not two disjoint total-allocation estimates to add again to their parents.

Allocation-object profile total:4,613,274 sampled objects. Flat shares:rywCache.set45.42%,main.transaction.Set11.84%,Transaction.enterState11.37%,materializeCommit11.25%. This identifies both byte-heavy snapshot storage and many small per-Set/map/key allocations.

The replay/capture code exists to preserve immutable validated mutation/conflict bytes across concurrent mutation/reset/Commit. The profile identifies optimization candidates, **not permission to remove defensive copies or reuse live transaction state**. No production code was changed by this measurement agent.

## CPU: syscall/scheduler plus commit/coalescing work

CPU pprof duration23.01s, **8.21s total samples**. Main flat sites:

- Syscall6:1.59s,19.37%.
- futex:1.06s,12.91%.
- memmove:0.23s,2.80%;memclrNoHeapPointers:0.21s,2.56%.
- keyRangeEqualsKeyAfter:0.16s,1.95%.

Important cumulative paths (overlapping):Transaction.Commit2.42s29.48%;commitAdmitted2.35s28.62%;Conn.writeLoop1.50s18.27%;mallocgc1.10s13.40%;rywCache.set0.87s10.60%;stateSet0.82s9.99%;coalesceCommitVectors0.82s9.99%;coalesceCommitMutations0.77s9.38%;gcBgMarkWorker0.81s9.87%;materializeCommit0.34s4.14%;captureCommit0.21s2.56%.

Call-tree edges split rywCache.set CPU into approximately0.44s from initial stateSet and0.43s from commit replay (inlined callers can appear as commitAdmitted in the tree). Heap trees identify the replay function explicitly. Syscall6 includes writes, reads and scheduler/network-poll paths; its flat samples are not a measured network-wait breakdown. Runtime map assignment/access, copies, allocation and GC are substantial within the RYW/snapshot paths.

`pprof-analysis/` preserves CPU, alloc_space and alloc_objects **flat top, cumulative top, text call trees and DOT call graphs**. Trees/graphs retain120nodes with nodefraction/edgefraction0; the raw profiles remain authoritative for omitted node detail. Corrected `*-critical-source-002.txt` files annotate the important source lines. Initial source listings used the wrong trim prefix (`fdb.dev/` versus version-qualified `fdb.dev@v0.0.0/`); those failed source lookups are retained, and corrected listings resolved all requested files. No profile was rerun.

## Checks, bounds and observer behavior

Each profile passed all raw sample/warmup counts, exact-byte/final-persistence checks and original quiescence gates. Each recorded exactly10,000read versions,10,000successful GRV replies,10,000commit starts and10,000commit completions;zero cache hits, errors, callback/transaction retries, connection failures and maybe-delivered replies. Warmup100operations each also passed without retry. Manifest options/identities and binary/server/library/config pins matched before/after.

Unique CPU/heap prefixes are both68bytes, keys88bytes;177conflict bytes/Set and8850bytes/transaction imply small→large promotion at Set3 by source/shape proof. Same timeout30000ms,retrylimit10,allow-writes,workers1. Each child had a **180s total process deadline** including bootstrap; existing60s bootstrap remains. CPU and heap ran sequentially, with no concurrent build/test/profile or other remote command. Original2s cooldown+1s per-CPU/group quiescence;FDB CPUs2–7,client8–15,GOMAXPROCS8;wrapper/collector0–1;Docker/containerd inactive. One co-located VM/server, not physical-host isolation.

The diagnostic wrapper directly spawned taskset→benchmark in one owned process group, preserving the child PID through exec. Original telemetry(child_pid) captured that actual PID in **24/24CPU samples and21/21heap samples** (1Hz deliberate observer). Clean bounded process-group cleanup and full raw host/server/process snapshots retained.

### Append-only historical collector clarification

The prior sealed v5 batchset50 timing campaign used the unchanged original runner with an external time wrapper. Its per-process telemetry filter recognizes baseline-/candidate- comm prefixes, not the newly named batchset50-* executables; the explicit PID refers to time. Thus those prior process samples contain server+time wrapper, **not the benchmark child**. The prior benchmark-internal getrusage/MemStats CPU/allocation/GC/high-water measurements and host/per-CPU/server telemetry/quiescence remain available; reported paired resource results used those internal measurements. This limitation is now disclosed without rewriting any prior archive.

## Immutable identity and extended correctness addendum

- Candidate `batchset50-v5-conflictbuf-512-001-go`: `5214910d5c4084d5c95d8e919f1867b279b164968d54afbd6a65c52a6fb853c6`.
- v5 harness archive: `738348814d86cc6ec828aa3590625275e9f1b74bed7ae92e06e359c480151cfc`.
- Complete client patch: `b3c09057e81ac448c8b05881e10cd52a34067a27152fcd0a4c367e514e0861c3`, overbase `26abdbaac12be60ea8955027f6047e10474596f1`.
- Go1.26.9,FDB7.3.77,API730;exact source/binary unchanged, no rebuild.

Root #352 reported extended validation GREEN: task135whole-fast-race100/100(97actual,3cached,350.372s);task138just-verify full151/151(52actual,99cached,1797.951s,default compact),race-all15/15(13actual,2cached,201.014s),three fuzz-smoke PASS10–11s each. **Fuzz-smoke warned no coverage instrumentation; this is not claimed coverage-guided fuzzing.** Root verified all6814frozen hashes. These are root-owned parent just/Bazel results, not repeated by this measurement agent. Subsequent test-only changes do not change this frozen measured runtime. Previous status records remain untouched.

Both authorized profiles and analysis are complete. Await root release before any further workload/profile. VM/resource lifecycle remains root-owned.
