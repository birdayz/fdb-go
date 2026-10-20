# R2 — old Go write-path attribution only

Perf-lead request R2, with root authorization #491: a single-arm pprof attribution may omit CGo; this exception does not apply to timed comparisons. These three runs identify sampled work inside the published old Go binary, not an old/new/native performance comparison. No client or harness build occurred.

- Source: `affef93729368a3fb183df90c5332b5bd2289b25` plus the frozen V6 standalone harness.
- Old binary: `/opt/fdbperf/bin/candidate-hc03-base`, SHA256 `7976a1892c6de2d33d51c12ea6fd63430e0f8c78f9fb2fb82d9c24cff27f0419`.
- Harness archive SHA256: `3d2504ae0db14fad14f0384287377998231cf049310c8289d6e7245d3428e0d0`.
- Go1.26.9, FDB server7.3.77, API730; workers1, warmup100, GOMAXPROCS8, fixed CPU affinity and 68-byte per-run prefixes.
- The attribution runner is a sealed derivative of the original runner, allowing exactly one arm and one block only when `attribution_only=true` and profile is cpu or heap. Six guard tests ran remotely and passed; original runner remains unchanged. This is not main-module validation.
- Both CPU runs completed 12:45:54–12:46:14 UTC, heap run 12:46:14–12:46:23 UTC; all raw/persistence/counter/quiescence/identity checks passed. Offline pprof analysis followed, ending 12:46:26 UTC. No overlapping builds, workloads, or profilers.

## Exact measured-phase MemStats

|Workload|Profile|Measured tx|B/tx|Allocs/tx|GC cycles|GC cycles/tx|
|---|---|---:|---:|---:|---:|---:|
|batchset50 (50×100B)|cpu|3000|80354.744|299.578|8|0.002667|
|batchset50x1000 (50×1000B)|cpu|2000|271027.556|300.502|3|0.001500|
|batchset50 (50×100B)|heap|3000|80404.291|299.606|9|0.003000|

## Profile guide and limitations

`*-cpu-{flat,cum}-top40.txt` contains the requested CPU views. Raw CPU profile copies are `batchset50-cpu.pprof` and `batchset50x1000-cpu.pprof`. The heap raw copy is `batchset50-heap.pprof`; heap `inuse_space` flat/cumulative top40, `alloc_space` and `alloc_objects` top30 and cumulative top40 are supplied. Critical source listings are also supplied. All pprof stderr files are empty.

CPU profiles cover the measured phase, not fixture/warmup. Sample totals are 2.31s for batchset50 and 1.98s for batchset50x1000; these differently sized samples are not a speed comparison. Cumulative rows overlap and must not be added together.

Heap pprof samples lifetime allocation, including Prepare and warmup, at the default coarse 512KB sampling rate; it is collected after runtime.GC. Its sampled allocation weights and percentages are not exact phase byte/object counts. Exact phase MemStats are the table above. In particular `batchSet50Payload` is fixture preparation, not a measured per-transaction allocation.

Within the small-write CPU sample, keyRangeEqualsKeyAfter has 130ms flat (5.63%); captureCommit 130ms cumulative, stateSet180ms and gcBgMarkWorker240ms. The large-write sample has memmove550ms flat (27.78%), stateSet540ms cumulative, ownMutationLocked190ms, RYWset190ms and captureCommit160ms. These are attribution observations, not proof which optimization will help.

The small-write heap sample totals268.06MB alloc_space. Leading flat sites: captureCommit69.95MB (26.09%), RYWset53.12MB (19.82%), addWriteConflict25.57MB (9.54%), stateSet25.07MB (9.35%). Leading sampled object sites: enterState283994 (26.37%), RYWset266294 (24.72%), main.transaction.Set131075 (12.17%). Existing safety/lifetime semantics remain load-bearing; profiles alone do not authorize weakening ownership or lease checks.

The original source exports and all three original binaries are preserved in `hillclimb-arena-003-recovery-001.tar.gz` (SHA256 `1792eb6605ecb1cb95f33b4327594779c707325dfab855fc6439d26179fdc974`). This R2 archive preserves the original source/binary manifests, derivative runner+patch+guards, plans, all three raw runs and all profile analysis.
