# Baseline diagnostic findings — 2026-10-09

Production baseline: `26abdbaac12be60ea8955027f6047e10474596f1`; frozen clientbench v3 SHA256 `41f594c990477bc4b29c565160599a57266cecb23adaae582180f7464fb2d0ea`. These are diagnostic runs, not replacements for the unprofiled 24-block baseline. Server/libfdb_c 7.3.77; Go 1.26.9. Full host/configuration/provenance lives in the baseline backup.

## Established correctness/behavior finding

`baseline-controls-v3-001` stopped after four arms at `b000-ryw-p1-go` with `local RYW issued GRV: versions=2000 batches=2000`. The Go arm completed 2,000 Set/Get/Cancel operations with exact bytes and zero operation errors/callback retries, but issued 2,000 successful GRV batches/read versions. No commits occurred in the measured phase and GRV cache hits were zero. `success=false`, `persisted_validation=not_run`: this arm and its campaign are explicitly FAILED, not a successful performance sample. The preceding native arm passed its byte/persistence checks; native GRV counters are UNOBSERVED, not measured zero.

C++ 7.3.77 `fdbclient/ReadYourWrites.actor.cpp:95–109` returns the independent local write-map value without tr.get; the fallback around line114 performs tr.get. Public get at1631–1693 has no unconditional GRV. Baseline Go `Transaction.Get` ensures a version before checking the overlay (transaction.go approximately881 then899); GetPipelined approximately922 then938, and Snapshot.Get approximately604 have related ordering. The network dependence for an otherwise local read is a Go behavior bug, not merely a performance choice. Root is fixing it separately; no RYW change has been applied to the measured source.

## Concrete CPU/wait attribution

- Unprofiled main Get1000: median process CPU/op across 24 runs was about 863µs Go versus 487µs CGo. Thus the latency gap is not explained solely by a parked timer. These process CPU counters cover all threads, not one goroutine.
- Go block profile, 10,000 measured Get1000 operations: caller `ensureReadVersion`/`grvBatcher.getReadVersion` accumulated 13.82s blocked delay, `sendGRVRequest` 1.97s, and `PendingGet.Resolve` 1.60s. This strongly implicates GRV admission/waiting, consistent with the baseline fixed1ms batching timer. These are nested/concurrent cumulative blocked durations; subtracting or summing them does NOT produce an exact additive latency decomposition. Background monitor/write-loop and parent WaitGroup waits must not be charged as additional per-operation latency.
- Native Go-binding block profile, 10,000 measured operations: future blocking accumulated4.587s, including4.462s on value futures and0.125s on commit futures. Native-internal waits are not fully represented by Go's block profiler.
- Go CPU pprof, 50,000 operations: duration82.22s, total samples27.41s. Flat samples: `Syscall6`8.61s/31.41%; `runtime.futex`6.30s/22.98%; `runtime.write1`0.80s/2.92%; `runtime.usleep`0.44s/1.61%. Scheduler/netpoll/wakeup paths dominate the remaining cumulative stacks. This is CPU sampling, not blocked wall time.
- CGo CPU pprof, 50,000 operations: duration26.85s, samples17.37s; `runtime.cgocall`11.87s/68.34% flat, futex2.69s/15.49%. `cgocall` is a coarse attribution boundary, not proof CGo transition overhead itself consumed that CPU. Native symbols require external profiling.
- Go heap alloc_space profile:80.74MiB sampled process-lifetime allocation including setup and warmup. Flat `FrameReader.Read`14.52MiB/17.98%; CreateTransaction8.01MiB/9.92%; bytes.Clone7MiB/8.67%; GetPipelined5MiB/6.19%; context cancellation/deadline/timer machinery is also substantial. These are sampled lifetime allocations, not measured-phase bytes/op. Native Go heap sampling excludes libfdb_c's C++ heap entirely.

## Instrumentation validation and limitations

All46 commands in `baseline-profiles-v3-001/commands.txt` finished rc0, and all14 benchmark summaries succeeded with zero operation errors. CPU/block/trace recording is bounded around the measured phase. Heap sampling is process-lifetime; external perf includes process setup/warmup/shutdown and its own observer overhead.

Hardware-event positive controls distinguished CPU arithmetic from a256MiB pointer chase (cache-miss totals about149k versus53m, 100% time-running). This supports cache-miss usefulness on this virtual machine but is not proof of every PMU event. Branch-misses stayed zero on the limited predictable probes and is NOT validated. BPF sched-switch capability probe recorded517 switches; perf sched positive control443 samples. No BPF workload attribution was performed.

perf DWARF cycles samples: Go7394, CGo5066; no lost samples reported. Native debug symbols resolve, but addr2line emitted `could not read first record` warnings. PMU recording/stat substantially increased kernel CPU; `native_write_msr` appears prominently. Those timings and kernel percentages cannot be presented as uninstrumented workload cost or clean causal attribution.

Unfiltered `perf sched latency` grouped the expected client threads: Go10 threads,3048.813ms runtime,91667 scheduling observations,0.020ms average runnable delay/3.940ms maximum; CGo9 threads,1975.417ms runtime,37840 observations,0.025ms average/3.661ms maximum. Raw samples independently identify actual TIDs via processPID matching manifest.pid. However, `perf sched timehist --summary` filtered by either PID or those exact TIDs emits only a handful of events. These filtered reports are INCOMPLETE and must not be used to claim total off-CPU or scheduling coverage. Original reports, raw perf.data, decoded events, and thread-scope JSON are retained. The unfiltered latency report is only a separate diagnostic, not a repaired complete timehist decomposition.

## Work status / isolation

No candidate production patch has been applied or built. No candidate timings exist. v4 CONTROL-ONLY harness archive SHA256 `846da387a746cc6d87a25d72b322d0d1a806004a6207227b40707103be817299` was separately built and its9 top-level unit tests passed under CGO=0 and CGO=1. v4 control measurements have NOT run. The original strict v3 RYW guard is unchanged. The intended v4 `ryw_current` simply observes the current network-dependent behavior; `set_local` checks Set/Cancel with zero observed GRV. All new performance work is held per the user's bugs-before-performance instruction.
