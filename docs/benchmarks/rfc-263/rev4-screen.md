# RFC-263 revision 4 evidence

Owner-requested disposable prototypes and measurements. No revision-4 production client change was made in `${FDB_GO_WORKTREES}/fdb-go-perf`; that branch retains the Phase 1 checkpoint. The final RFC is PROPOSED and awaits the owner's design review. Local RFC-only commit: `6585b2919`; final text is copied to `rfc263-revision4.md`.

## Selected variant (b)

16 stable transaction producer heaps; lower-bound/no-lock successor reuse; one globally ordered drainer; a sampled clock and pre-scan global ticket frontier; weak resolution outside shard locks; one transaction GC cleanup that unlinks its node by index after unreachability. No node-slot recycling and no cancellation merely at commit/callback return.

- Source tree: `61d2597e0231dfbedcd898a27c078c3412efaf6a`.
- Executable: `bin/reclaim`, SHA256 `2f0993bd332a9d7085b1c1053659b68867286de95ad1cbe2bf91fe327f088e95`.
- Source: `reclaim-source.tar.gz`, `reclaim.patch`, `reclaim-inputs.sha256`.
- Fifteen focused race probes actually passed; these are not the fast/full suite.
- Removing cleanup compiled and failed `GC_CLEANUP_DID_NOT_UNLINK` after a separate GC-cleanup positive control. The allocation pin then also changed 9 -> 7. Exact mutant patch/log/status retained; fixed source restored before building/timing.
- Creation is 9 allocations versus baseline 11, not no-cleanup Phase 1's 7.
- Cold construction: 49 allocations; root/shard/node/ready sizes 312/112/64/24 bytes; up to 16 lazy runtime timers.

## Acceptance screen

All runs use the unchanged pure-Go harness from baseline `fa8725f1dcf6ff3ac66825ab6e3e819efbc0abcc`, Bazel opt Go 1.26.9 binaries, `GOMAXPROCS=4`, `taskset -c 2,3,14,15`, GOGC=100, GOMEMLIMIT=off, empty GODEBUG, timeout 30000ms, warmup 2000, and the existing fdb4 cluster. Each command receipt records exact argv/environment, source and binary identity, load average, idle snapshot, and return/completion status. Every raw sample and summary is retained.

Selected screen:
- `rev4-screen4-w8`: 18 runs, three alternating pairs per get100/set100/range100, 20000 measured each. All nine pairs favorable. Median CPU changes -3.43/-3.85/-1.54%.
- `rev4-mutex-draining4-w32`: six long runs, both arms; get1M/set400k/range500k. Queue+root wait/client-CPU ratios 0.01725/0.000511/0.01760%, including cleanup `remove`.
- `rev4-range4-w32`: six runs, three predeclared alternating unprofiled range pairs, 500k each. Median 59.855936 -> 58.953094 us/tx (-1.51%); all three pairs favorable, observed arm ranges overlap.

Analysis: `screen4-analysis.json`, `range4-analysis.json`, `queue-wait-analysis.json`, and their scripts. The mutex parser uses exact source unlock lines from each immutable source archive, not broad stack-name sums. `reclaim-zero-drainer-analysis-note.txt` explains the correction allowing a genuine zero sampled drainer-contention category. No workload was discarded for that analysis correction.

## Rejected screens and attribution

`screen-analysis.json` records the rotating-order baseline/lower-bound-v1/raw-independent-shard screen. Raw shards deliberately violate cross-shard C7. The lower-bound v1 also reserved eager tickets too early; its data locate cost, not a usable design. Corrected single-heap v2 (`screen2b-analysis.json`) still exceeds the queue-wait criterion on get100.

The globally merged no-cleanup candidate (`screen3-analysis.json`, `range3-analysis.json`) fixes lock wait but fails range CPU: +2.55% at 8 workers, +10.12% at 32 workers; every range pair adverse.

Same archived baseline/combined binaries, separate diagnostic CPU-profile pair:
- `rev4-attribution-cpu-w32`, range500k/32 workers.
- `pprof/attribution-range32-cpu-diff-{top,cum}.txt` are produced with `pprof -diff_base`.
- `pprof/exclusive-cpu-analysis.{py,json}` partitions every sample exactly once. GC+barriers explain +2.74 of the net +2.97 sampled CPU-seconds. Sampled totals 30.12 -> 33.09s agree with harness rusage +5.923us/tx. Cumulative scanner helpers are nested, not additive.
- Heap/gctrace runs on the SAME archived binaries: `rev4-attribution-gc-heap-w32`. `gctrace-analysis.json` shows GC/cycle16.52 -> 34.13ms and totalGC3.568 -> 6.110s, despite fewer cycles.

## Population and selected GC verification

Instrumented builds are explicitly DIAGNOSTIC ONLY, never CPU-performance evidence. The shared generic sampler file is byte-identical. It reads runtime/metrics once/sec and samples shard lengths without scanning/promoting transaction weak pointers. Root callbacks themselves retain only weak root references. Snapshots are per-shard locked, not globally atomic; peaks are 1Hz lower bounds. Counts are physical weak queue entries, not strongly reachable transactions.

- Original combined: `rev4-diagnostic-population-w32`, binaries `diag-baseline`/`diag`. `population-analysis.json`: ~396k queued nodes, 475136 pointer slots, ~31.7MB node/weak classes plus3.8MB array payload; collector outcomes all nil. This is ~10s of completed history, not 32 in-flight handles.
- Selected: `rev4-diagnostic-reclaim-population-w32`, binaries `diag-baseline`/`diag-reclaim`. `reclaim-population-analysis.json`: 741-2646 queued nodes,4592 pointer slots; a GC/cleanup cohort rather than 10s history. Available-CPU GC fraction over sampled intervals5.757 -> 5.718%; totalGC counter deltas near baseline.
- Archived-binary selected gctrace/heap: `rev4-memory4-gc-w8` and `rev4-memory4-gc-w32`. `memory4-gctrace-analysis.json`: GC/cycle at8workers1.425 ->1.423ms; at32workers16.613 ->16.291ms. Total32GC3.555 ->3.470s. These diagnostics are separate from acceptance timing.
- All diagnostic binaries have their own source archives, patches, source/tree identities, build logs, and SHA manifests.

GC source semantics were checked against Bazel's Go1.26.9 SDK, not the host compiler. Relevant SDK source files are preserved in `go-sdk-1.26.9-sources/`. Host profile-decoder version and SDK evidence are in `profile-decoder-and-sdk-gc-version.txt`.

## Correctness and parked work

- Global-frontier and global-sort mutations compiled and made named ordering/cohort assertions red; exact patches/logs preserved.
- The measured no-cleanup tree `090363b16c2e16e640a9ce3557278aa00252aab7` has12 probes. Post-screen test-only tree `b20eb3a48b1320dea86e78c3756bd74290a6e801` adds the precise staggered-exit probe; removing root.again compiles and fails it. No production code changed between those two trees. Selected tree includes that regression.
- Noscan/slab option(a) is PARKED: `noscan-source.tar.gz`, `noscan.patch`, `noscan-status.txt`. Unbuilt/incomplete/untimed;16 unexecuted top-level probes. It has an unresolved finalizer-resurrection owner-alias hazard; queue/ready generations alone do not protect an old transaction's interior slot pointer. It is not a correctness or performance result.
- Pure lazy-on-use arming and callback-return reclamation are ruled out by live-handle/recorded-cause semantics, documented in the RFC.

## Invalidated attempts

- `discarded-local-rebuild-attempt1`: entire first v2 attempt invalidated after owner withdrew the quiet window;8 completed runs plus an interrupted start, raw files and original metrics retained.
- `discarded-early-sequence-attempt2`: next start stopped when eager publication sequence bug was found. Entire attempt excluded, all available files retained.
- Neither is pooled into accepted screens. Setup/build failures and rejected noncompiling mutant attempts are retained and do not count as successful tests/mutations.

## Preservation and gates

All six remaining prototype/diagnostic worktrees were removed after matching their full archived source contents byte-for-byte, comparing live binary diffs to retained patches, and validating tree identities (`worktree-preservation-verification.json`). Noscan was similarly verified before removal. No scratch branch was created; no push/PR was made. Existing cluster/server data were not destroyed.

`run-metrics.jsonl` contains 129 completed runs in distinct primary, profile and diagnostic phases: NEVER pool them. `phase-populations.json` records populations. Each source/binary manifest remains immutable. `evidence-files.sha256` inventories the final retained evidence, excluding itself.

The main RFC-only fast lane (`rfc4-just-test.log`) passes155/155,1 executed154 cached. The real commit hook repeats that same fast-lane result and passes staged secret scanning (`rfc4-commit.log`). This does NOT verify a proposed implementation. Full/full-race validation remains owned by the parent, and production implementation awaits official design review. Historical Phase1 proof/measurement evidence remains in `/tmp/rfc263-phase1.owjNRR`.
