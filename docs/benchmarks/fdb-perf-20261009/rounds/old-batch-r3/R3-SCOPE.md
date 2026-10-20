# R3 — longer batchset50 comparison and separate instrumentation

Perf-lead R3 (perf stat authorized by root#641). Existing binaries only; no builds or main-module tests. All workloads ran remotely on the existing FDB7.3.77/API730 guest, workers1/GOMAXPROCS8, runner0–1/server2–7/client8–15. No concurrent build/workload/profiler. Native internals are unobserved; Go MemStats exclude C++ heap.

Old source `affef93729368a3fb183df90c5332b5bd2289b25`, binary SHA256 `7976a1892c6de2d33d51c12ea6fd63430e0f8c78f9fb2fb82d9c24cff27f0419`; native binary SHA256 `cd762ea429647784378270613f404573ba598a5049f422e055a887f7746ecbdb`. Frozen V6 harness SHA256 `3d2504ae0db14fad14f0384287377998231cf049310c8289d6e7245d3428e0d0`. Source/binary/runtime hashes verified before and after. Full source and original binaries remain preserved in recovery001 archive SHA256 `1792eb6605ecb1cb95f33b4327594779c707325dfab855fc6439d26179fdc974`.

## Unprofiled comparison

The original immutable runner executed six counterbalanced old/CGo blocks,3000 measured transactions+200 warmup each, 50 distinct100B Sets+Commit per transaction. Both orders occur three times. Timing campaign lasted122 seconds,17:31:24–17:33:26 UTC, within180s cap. Twelve runs passed all raw/byte/persistence/counter/quiescence/identity checks.

Absolute median-run mean/p99/CPU/user/system µs per transaction:
- CGo:2225.945/3881.553/847.296/474.675/380.741.
- Old Go:2017.505/3841.589/905.354/514.630/390.752.

Paired old/CGo geometric ratios: mean0.926298,p990.947904,CPU1.072634,user1.088531,system1.055169. CPU ratio is above1 in all6 blocks,range1.035758–1.109004. Go GC cycles total old50/CGo4 over18000 measured transactions/arm, or0.002778/0.000222 cycles per transaction. CGo's count measures Go wrapper GC only, not native allocation/collection.

This is a scoped six-block result, not a steady-state capacity/noninferiority/global-native claim. The prior100-transaction campaign and R3 differ in duration,warmup,time/order and environment history. A smaller observed CPU gap does not prove GC/warmup caused the earlier gap. Absolute tables, paired ratios and per-block GC rates are in `old-batch-r3-timed/R3-TABLES.md`; raw rows and phase accounting remain in CSV/JSON.

## Separate instrumentation

Both tools were preinstalled (perf7.0.14,strace6.19). The requested perf event probe succeeded even though perf_event_paranoid=4; no policy changes or installs were performed. Instrument runs used one extra run/arm,3000 measured+200 warmup, same workload. Perf ran17:33:27–17:33:49 UTC; strace17:33:49–17:34:26. All4 workload validations passed. The instrument-only runner is a minimal derivative allowing exactly2arms/1block for the specified wrappers, with6 passing remote schedule guards. Timed comparison used the original runner, not that derivative.

`old-batch-r3-instruments/INSTRUMENTS.md` and JSON preserve perf counts and the entire syscall census. **All instrument counts are whole-process**, including fixture/seed,warmup,validation and cleanup. Counts/3000 is only normalization, not exact measured-phase counts. Strace aggregate errors are syscall returns, not transaction failures; -c does not identify errno values. Tracing perturbs scheduling and elapsed syscall time.

### Unresolved perf accounting discrepancy

The requested perf events all report100% running and exit0, but **perf task-clock does not reconcile with GNU time process CPU** for these same invocations:

|Arm|perf whole task-clock s|GNU time user s|GNU time system s|GNU time sum s|Measured-phase getrusage sum s|
|---|---:|---:|---:|---:|---:|
|old|6.197840|1.36|3.13|4.49|3.924618|
|cgo|5.467520|1.39|2.78|4.17|3.584470|

Different phase scope explains why phase getrusage is smaller than whole-process accounting, but **does not by itself explain task-clock exceeding whole GNU time**. Cause is unresolved. Do not substitute task-clock for the validated phase CPU metric or derive a performance conclusion from these instrumented timings. Instrumented system CPU is materially higher than unprofiled. Hardware cycle/instruction counts are retained as single-run guest diagnostics, not physical-host isolation or a clean instruction-cost comparison.
