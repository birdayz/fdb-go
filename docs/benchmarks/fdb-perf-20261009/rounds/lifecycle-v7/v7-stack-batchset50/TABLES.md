# Client benchmark

Absolute times are medians of run-level statistics in µs/transaction. Ratios are geometric means of within-block ratios, NOT ratios of these medians. Window CPU is measured-phase getrusage user+system; drained and shutdown scopes are defined below. Go allocation/GC counters exclude the native C++ heap; do not compare total Go/CGo memory from these columns. Native client counters are unobserved.

|Workload|Arm|Runs|Transactions|Mean|p50|p90|p99|p99.9|Window CPU|Window User CPU|Window System CPU|Drained CPU|Shutdown process CPU (E-A)|Whole process through shutdown (E; unpaired)|Go B/tx|Go allocs/tx|Go GC cycles: window/drained/shutdown|
|---|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|
|batchset50|cgo|6|18000|2206.267|2033.716|2668.222|4374.774|6791.148|848.724|449.071|396.797|867.490|975.023|993.872|1616.136|59.001|1/19/73|
|batchset50|go|6|18000|2025.760|1865.157|2398.614|3846.389|6200.167|762.922|446.245|310.407|806.497|945.555|954.414|80243.340|303.330|48/60/114|

|Workload|Ratio|Metric|Paired GM|Nominal 95% block-bootstrap CI|Pairs <1|Numerator Go GC: window/drained/shutdown|Denominator Go GC: window/drained/shutdown|
|---|---|---|---:|---|---:|---:|---:|
|batchset50|go/cgo|mean_ns|0.911296|[0.854909, 0.955222]|6/6|48/60/114|1/19/73|
|batchset50|go/cgo|p50_ns|0.906511|[0.870743, 0.929215]|6/6|48/60/114|1/19/73|
|batchset50|go/cgo|p90_ns|0.920651|[0.806801, 1.026324]|4/6|48/60/114|1/19/73|
|batchset50|go/cgo|p99_ns|0.876984|[0.770233, 0.985403]|4/6|48/60/114|1/19/73|
|batchset50|go/cgo|p999_ns|0.894440|[0.813184, 0.984652]|5/6|48/60/114|1/19/73|
|batchset50|go/cgo|process_cpu_ns_per_op|0.895104|[0.881803, 0.909105]|6/6|48/60/114|1/19/73|
|batchset50|go/cgo|user_cpu_ns_per_op|1.000189|[0.962234, 1.029852]|2/6|48/60/114|1/19/73|
|batchset50|go/cgo|system_cpu_ns_per_op|0.776749|[0.750323, 0.804106]|6/6|48/60/114|1/19/73|
|batchset50|go/cgo|drained_cpu_ns_per_op|0.922994|[0.908869, 0.936429]|6/6|48/60/114|1/19/73|
|batchset50|go/cgo|shutdown_process_cpu_ns_per_op|0.964230|[0.950698, 0.975822]|6/6|48/60/114|1/19/73|

Exploratory same-host result, not a noninferiority, global-native or capacity claim. Bootstrap resamples paired run blocks, not individual operations. Independence and CI coverage are not established on a continuously running server; no multiplicity correction. analysis.json includes order/dependence diagnostics, raw-file hashes and any CPU-component ratios omitted because a counter was zero.

Window CPU is not complete-lifecycle per-transaction CPU: the Apple binding releases native transactions/futures through Go GC finalizers, which enqueue further network-thread teardown. Warmup cleanup can enter the window and late measured-transaction cleanup can fall after it. The size/direction of that unequal deferred-cleanup effect is unmeasured by the window alone; GC counts do not establish that all native cleanup completed.

Drained CPU is (getrusage after the post-measurement finalizer fixpoint and native FIFO barrier minus the pre-measurement snapshot)/measured transactions. It includes forced GC and pending setup/warmup cleanup; it is not isolated steady-state transaction CPU. Shutdown process CPU subtracts the post-identity, pre-backend-open snapshot A from RUSAGE_SELF user+system E after the final fixpoint, database close and native StopNetwork, divided by measured transactions. E-A includes client startup, fixture, warmup, validation and CSV output, but excludes earlier harness identity hashing, final JSON output and process exit. Whole process CPU through shutdown is absolute E/transactions, retained only in raw/per-run/absolute tables. It includes arm-specific executable/library identity hashing and is not paired cross-arm client-cost evidence. Go finalizers are drained using exact runtime counters; cleanups are runtime estimates and must not increase during either drain. The terminal shutdown, not GC counts alone, establishes that native network work has stopped.

The imported master590145 baseline omits C++ causal-read-risky on write-only commit GRVs (NativeAPI.actor.cpp:6577-6578 versus client/transaction.go:1980-1985,3398-3411). Without it the server waits for updateLastCommit (GrvProxyServer.actor.cpp:647-648). Identify whether each tested revision retains this pre-existing difference; results measure the actual implementations, not algorithmically identical clients.
