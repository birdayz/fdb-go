# Arena003 read-regression investigation — unresolved

## Scope and disposition

Arena003 is scratch, not integrated. Root owns main-module Bazel validation and final review. Baseline is published `affef93729368a3fb183df90c5332b5bd2289b25`; candidate adds patch SHA256 `bc57f249650603cf657a2f86c8e05351de33b9c51a8b33708e45c5fadd0fe9ce`. All 108 unprofiled runs and 36 separate profiled diagnostic runs completed with raw/persistence/quiescence/identity checks passing. Those checks are not main-module correctness approval. No controls were repeated to obtain a passing result.

`ALL-TABLES.md` reports all old/new/CGo absolute mean/p99/process-CPU values and paired ratios; `PER-BLOCK.csv` has 144 rows (108 unprofiled, 36 diagnostic). The original read1000 CPU ratio new/old is **1.110292**, worse in 5/6 blocks. It remains an adverse result. Diagnostic CPU-profiled ratio 1.026848 (worse 4/6) and heap-profiled ratio 1.062074 (worse 5/6) do not replace or clear it.

## Existing-data accounting

The 54 read summaries record measured-phase process user and system CPU deltas. Every delta sum exactly matches the saved total CPU. `READ-CPU-SPLIT.md` and CSV preserve all arms and pairs. For original unprofiled read1000, old totals were user 0.978990s/system 1.578686s, new 1.155056s/1.672478s over 6000 operations each. The +269.858ms total difference consists of +176.066ms user and +93.792ms system. Paired geometric new/old ratios are user 1.183425, system 1.064719, total 1.110292. Diagnostic user CPU also rises: paired 1.056362 CPU-profiled and 1.109322 heap-profiled. This narrows accounting, not causality; it does not support attributing the entire regression to syscalls. No measured-phase context-switch counter was captured.

## Profiles: observations and limits

Six CPU profiles per arm were merged. Old/new sampled totals are 6.61/6.64s; measured getrusage totals are 8.053534/8.276283s. Samples do not explain all measured CPU. Flat syscall6 is 2.26/2.28s and futex 1.47/1.52s (~56–57% together). Cumulative readLoop 1.32/1.48s, schedule 1.39/1.48s and findRunnable 1.33/1.43s rise, but ReadTransact 1.24/1.13s, mallocgc 0.44/0.31s and gcBgMarkWorker 0.26/0.14s fall. conflictBufAlloc cumulative time is 40/10ms. No sampled retirement hotspot establishes a mechanism for the original adverse CPU result.

Heap profiles are sampled lifetime allocations, including fixture and warmup, collected after runtime.GC. Coarse 512KB sampling gives old/new alloc_space 181.33/174.13MB and inuse_space 102.19/91.45MB. These are not exact measured-phase deltas. Unchanged CreateTransaction itself samples 19.52/12.51MB, a warning against overinterpreting differences. conflictBufAlloc sampled 9.61/9.52MB is approximately unchanged. The extra owner pointer is 8 bytes on amd64 (24→32B owner); current profiles cannot price its CPU cost.

Original exact MemStats B/op medians 8122.284→8145.956, mallocs/op 63.665→63.703, GC cycles 18→18, total pause 2.490→5.287ms. CPU-profiled B/op 8007.715→8022.573, GC 30→30; heap-profiled 8035.204→8049.428, GC 36→36. Go MemStats excludes C++ heap; native internal counters are unobserved, not zero.

## Reachability / fixture confound

A timed read1000 uses a 79-byte key and 159 conflict bytes, fitting the 512-byte slab in both arms. Normal ReadTransact does not commit/reset: no slab-chain rollover/retirement is reached on this measured path. Both conflictBufAlloc implementations exist as actual non-inlined functions (objdump retained).

The frozen V6 Prepare seeds 104 keys even for reads. With the 68-byte prefix its mutation bytes (20115) plus conflict bytes (16334) total 36449, crossing 32KiB. Candidate seed cleanup returns a 512-byte slab and two 32KiB slabs; baseline copy-grows to an oversized 64KiB unpooled buffer. Thus premeasurement pool/GC state can differ. Existing 30-second transaction timeouts may retain read transactions during these short process lifetimes; this is unchanged behavior, not a new arena fix scope. These are hypotheses to isolate, not established explanations.

## Recovery / identity

Controller crash wiped /tmp after CPU campaign transport task 178 timed out at 240s; the remote campaign had in fact completed at 11:57:18 UTC with exit 0 and all 18 arms passing. Recovery verified remote status/complete.json/logs and absence of remaining benchmark processes. Original archive `hillclimb-arena-003-recovery-001.tar.gz`, SHA256 `1792eb6605ecb1cb95f33b4327594779c707325dfab855fc6439d26179fdc974`, contains 5338 verified files, full source/build exports, three binaries, runner/analyzer, original108 and CPU18 raw runs. This supplement adds full heap18 raw artifacts, all profile analysis, scripts/logs and regenerated reporting. Nothing was rerun to recover lost controller files.

Old binary SHA256 `7976a1892c6de2d33d51c12ea6fd63430e0f8c78f9fb2fb82d9c24cff27f0419`; new `2cb7b2242ff93add07d4f4ac71d51ccc1fa04a58b6abb166ef984d55fd30b7ac`; CGo `cd762ea429647784378270613f404573ba598a5049f422e055a887f7746ecbdb`. Go1.26.9, FDB/native/server7.3.77, API730; one remote 16-logical-CPU EPYC-Milan KVM guest, runner0–1/server2–7/clients8–15 and GOMAXPROCS8. No physical-host isolation claim. All builds/profiles/workloads remote; no main-module tests were run by this measurement agent.
