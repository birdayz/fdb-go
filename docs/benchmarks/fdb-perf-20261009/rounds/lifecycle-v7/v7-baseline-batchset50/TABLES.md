# Client benchmark

Absolute times are medians of run-level statistics in µs/transaction. Ratios are geometric means of within-block ratios, NOT ratios of these medians. Window CPU is measured-phase getrusage user+system; drained and shutdown scopes are defined below. Go allocation/GC counters exclude the native C++ heap; do not compare total Go/CGo memory from these columns. Native client counters are unobserved.

|Workload|Arm|Runs|Transactions|Mean|p50|p90|p99|p99.9|Window CPU|Window User CPU|Window System CPU|Drained CPU|Shutdown process CPU (E-A)|Whole process through shutdown (E; unpaired)|Go B/tx|Go allocs/tx|Go GC cycles: window/drained/shutdown|
|---|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|
|batchset50|cgo|6|18000|2188.606|2096.021|2610.247|3978.492|6376.421|833.596|446.044|403.010|855.203|959.430|978.879|1616.133|59.001|1/19/72|
|batchset50|go|6|18000|2016.054|1875.792|2526.026|3716.629|5980.963|882.700|490.886|395.370|926.461|1073.033|1081.642|80378.599|299.540|48/60/114|

|Workload|Ratio|Metric|Paired GM|Nominal 95% block-bootstrap CI|Pairs <1|Numerator Go GC: window/drained/shutdown|Denominator Go GC: window/drained/shutdown|
|---|---|---|---:|---|---:|---:|---:|
|batchset50|go/cgo|mean_ns|0.924842|[0.871268, 0.982326]|5/6|48/60/114|1/19/72|
|batchset50|go/cgo|p50_ns|0.924898|[0.859997, 0.999729]|5/6|48/60/114|1/19/72|
|batchset50|go/cgo|p90_ns|0.927996|[0.849137, 0.999423]|4/6|48/60/114|1/19/72|
|batchset50|go/cgo|p99_ns|0.962790|[0.862397, 1.066569]|3/6|48/60/114|1/19/72|
|batchset50|go/cgo|p999_ns|0.958480|[0.892493, 1.086710]|5/6|48/60/114|1/19/72|
|batchset50|go/cgo|process_cpu_ns_per_op|1.052546|[1.021542, 1.085915]|1/6|48/60/114|1/19/72|
|batchset50|go/cgo|user_cpu_ns_per_op|1.098823|[1.040418, 1.159019]|1/6|48/60/114|1/19/72|
|batchset50|go/cgo|system_cpu_ns_per_op|1.001543|[0.948180, 1.054553]|2/6|48/60/114|1/19/72|
|batchset50|go/cgo|drained_cpu_ns_per_op|1.076805|[1.045195, 1.110781]|0/6|48/60/114|1/19/72|
|batchset50|go/cgo|shutdown_process_cpu_ns_per_op|1.119446|[1.088659, 1.155948]|0/6|48/60/114|1/19/72|

Exploratory same-host result, not a noninferiority, global-native or capacity claim. Bootstrap resamples paired run blocks, not individual operations. Independence and CI coverage are not established on a continuously running server; no multiplicity correction. analysis.json includes order/dependence diagnostics, raw-file hashes and any CPU-component ratios omitted because a counter was zero.

Window CPU is not complete-lifecycle per-transaction CPU: the Apple binding releases native transactions/futures through Go GC finalizers, which enqueue further network-thread teardown. Warmup cleanup can enter the window and late measured-transaction cleanup can fall after it. The size/direction of that unequal deferred-cleanup effect is unmeasured by the window alone; GC counts do not establish that all native cleanup completed.

Drained CPU is (getrusage after the post-measurement finalizer fixpoint and native FIFO barrier minus the pre-measurement snapshot)/measured transactions. It includes forced GC and pending setup/warmup cleanup; it is not isolated steady-state transaction CPU. Shutdown process CPU subtracts the post-identity, pre-backend-open snapshot A from RUSAGE_SELF user+system E after the final fixpoint, database close and native StopNetwork, divided by measured transactions. E-A includes client startup, fixture, warmup, validation and CSV output, but excludes earlier harness identity hashing, final JSON output and process exit. Whole process CPU through shutdown is absolute E/transactions, retained only in raw/per-run/absolute tables. It includes arm-specific executable/library identity hashing and is not paired cross-arm client-cost evidence. Go finalizers are drained using exact runtime counters; cleanups are runtime estimates and must not increase during either drain. The terminal shutdown, not GC counts alone, establishes that native network work has stopped.

The imported master590145 baseline omits C++ causal-read-risky on write-only commit GRVs (NativeAPI.actor.cpp:6577-6578 versus client/transaction.go:1980-1985,3398-3411). Without it the server waits for updateLastCommit (GrvProxyServer.actor.cpp:647-648). Identify whether each tested revision retains this pre-existing difference; results measure the actual implementations, not algorithmically identical clients.
