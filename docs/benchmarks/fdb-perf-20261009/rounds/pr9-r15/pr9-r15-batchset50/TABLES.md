# Client benchmark

Absolute times are medians of run-level statistics in µs/transaction. Ratios are geometric means of within-block ratios, NOT ratios of these medians. Window CPU is measured-phase getrusage user+system; drained and shutdown scopes are defined below. Go allocation/GC counters exclude the native C++ heap; do not compare total Go/CGo memory from these columns. Native client counters are unobserved.

|Workload|Arm|Runs|Transactions|Mean|p50|p90|p99|p99.9|Window CPU|Window User CPU|Window System CPU|Drained CPU|Shutdown process CPU (E-A)|Whole process through shutdown (E; unpaired)|Go B/tx|Go allocs/tx|Go GC cycles: window/drained/shutdown|
|---|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|
|batchset50|cgo|6|15000|2253.664|2086.831|2800.196|4119.752|6604.840|857.940|450.819|412.675|881.361|1002.932|1024.690|1616.157|59.002|0/18/72|
|batchset50|new|6|15000|1990.923|1874.092|2353.309|4240.657|6595.155|822.022|475.681|338.318|867.171|1024.355|1034.341|80159.629|298.353|48/60/114|
|batchset50|old|6|15000|2057.055|1934.506|2288.744|4010.802|6116.803|828.147|482.770|333.803|873.817|1033.742|1043.844|80245.653|298.409|48/60/114|

|Workload|Ratio|Metric|Paired GM|Nominal 95% block-bootstrap CI|Pairs <1|Numerator Go GC: window/drained/shutdown|Denominator Go GC: window/drained/shutdown|
|---|---|---|---:|---|---:|---:|---:|
|batchset50|old/cgo|mean_ns|0.922820|[0.860909, 0.998967]|5/6|48/60/114|0/18/72|
|batchset50|old/cgo|p50_ns|0.933355|[0.873493, 1.002359]|4/6|48/60/114|0/18/72|
|batchset50|old/cgo|p90_ns|0.881267|[0.762433, 1.056964]|5/6|48/60/114|0/18/72|
|batchset50|old/cgo|p99_ns|0.965724|[0.882982, 1.051455]|4/6|48/60/114|0/18/72|
|batchset50|old/cgo|p999_ns|0.938148|[0.855368, 1.038772]|4/6|48/60/114|0/18/72|
|batchset50|old/cgo|process_cpu_ns_per_op|0.971583|[0.941584, 1.007317]|4/6|48/60/114|0/18/72|
|batchset50|old/cgo|user_cpu_ns_per_op|1.082949|[1.071237, 1.098170]|0/6|48/60/114|0/18/72|
|batchset50|old/cgo|system_cpu_ns_per_op|0.847744|[0.798585, 0.904602]|6/6|48/60/114|0/18/72|
|batchset50|old/cgo|drained_cpu_ns_per_op|0.997684|[0.968105, 1.034332]|3/6|48/60/114|0/18/72|
|batchset50|old/cgo|shutdown_process_cpu_ns_per_op|1.035832|[1.009414, 1.066192]|1/6|48/60/114|0/18/72|
|batchset50|new/cgo|mean_ns|0.905299|[0.867644, 0.948023]|6/6|48/60/114|0/18/72|
|batchset50|new/cgo|p50_ns|0.901890|[0.867946, 0.934628]|6/6|48/60/114|0/18/72|
|batchset50|new/cgo|p90_ns|0.894273|[0.804382, 0.988383]|4/6|48/60/114|0/18/72|
|batchset50|new/cgo|p99_ns|0.986652|[0.896677, 1.070773]|2/6|48/60/114|0/18/72|
|batchset50|new/cgo|p999_ns|0.987358|[0.899379, 1.081136]|3/6|48/60/114|0/18/72|
|batchset50|new/cgo|process_cpu_ns_per_op|0.965210|[0.935865, 0.995956]|5/6|48/60/114|0/18/72|
|batchset50|new/cgo|user_cpu_ns_per_op|1.060227|[1.043644, 1.085515]|0/6|48/60/114|0/18/72|
|batchset50|new/cgo|system_cpu_ns_per_op|0.859942|[0.817427, 0.911301]|6/6|48/60/114|0/18/72|
|batchset50|new/cgo|drained_cpu_ns_per_op|0.988697|[0.961060, 1.018416]|4/6|48/60/114|0/18/72|
|batchset50|new/cgo|shutdown_process_cpu_ns_per_op|1.021920|[0.998820, 1.046722]|2/6|48/60/114|0/18/72|

Exploratory same-host result, not a noninferiority, global-native or capacity claim. Bootstrap resamples paired run blocks, not individual operations. Independence and CI coverage are not established on a continuously running server; no multiplicity correction. analysis.json includes order/dependence diagnostics, raw-file hashes and any CPU-component ratios omitted because a counter was zero.

Window CPU is not complete-lifecycle per-transaction CPU: the Apple binding releases native transactions/futures through Go GC finalizers, which enqueue further network-thread teardown. Warmup cleanup can enter the window and late measured-transaction cleanup can fall after it. The size/direction of that unequal deferred-cleanup effect is unmeasured by the window alone; GC counts do not establish that all native cleanup completed.

Drained CPU is (getrusage after the post-measurement finalizer fixpoint and native FIFO barrier minus the pre-measurement snapshot)/measured transactions. It includes forced GC and pending setup/warmup cleanup; it is not isolated steady-state transaction CPU. Shutdown process CPU subtracts the post-identity, pre-backend-open snapshot A from RUSAGE_SELF user+system E after the final fixpoint, database close and native StopNetwork, divided by measured transactions. E-A includes client startup, fixture, warmup, validation and CSV output, but excludes earlier harness identity hashing, final JSON output and process exit. Whole process CPU through shutdown is absolute E/transactions, retained only in raw/per-run/absolute tables. It includes arm-specific executable/library identity hashing and is not paired cross-arm client-cost evidence. Go finalizers are drained using exact runtime counters; cleanups are runtime estimates and must not increase during either drain. The terminal shutdown, not GC counts alone, establishes that native network work has stopped.

The imported master590145 baseline omits C++ causal-read-risky on write-only commit GRVs (NativeAPI.actor.cpp:6577-6578 versus client/transaction.go:1980-1985,3398-3411). Without it the server waits for updateLastCommit (GrvProxyServer.actor.cpp:647-648). Identify whether each tested revision retains this pre-existing difference; results measure the actual implementations, not algorithmically identical clients.
