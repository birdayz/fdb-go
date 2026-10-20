# Client benchmark

Absolute times are medians of run-level statistics in µs/transaction. Ratios are geometric means of within-block ratios, NOT ratios of these medians. Window CPU is measured-phase getrusage user+system; drained and shutdown scopes are defined below. Go allocation/GC counters exclude the native C++ heap; do not compare total Go/CGo memory from these columns. Native client counters are unobserved.

|Workload|Arm|Runs|Transactions|Mean|p50|p90|p99|p99.9|Window CPU|Window User CPU|Window System CPU|Drained CPU|Shutdown process CPU (E-A)|Whole process through shutdown (E; unpaired)|Go B/tx|Go allocs/tx|Go GC cycles: window/drained/shutdown|
|---|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|
|batchset50|cgo|6|15000|2069.652|1994.311|2215.264|4291.775|6654.559|846.506|442.354|394.953|868.516|984.250|1007.034|1616.090|59.001|0/18/72|
|batchset50|new|6|15000|1934.086|1837.658|2211.830|3605.026|5971.544|716.943|393.207|321.364|761.750|907.771|918.193|57262.926|238.328|34/46/100|
|batchset50|old|6|15000|1962.591|1890.277|2143.415|3741.648|6426.655|902.874|507.921|402.339|942.012|1109.822|1119.777|80453.317|299.590|48/60/114|

|Workload|Ratio|Metric|Paired GM|Nominal 95% block-bootstrap CI|Pairs <1|Numerator Go GC: window/drained/shutdown|Denominator Go GC: window/drained/shutdown|
|---|---|---|---:|---|---:|---:|---:|
|batchset50|old/cgo|mean_ns|0.966606|[0.922256, 1.037329]|5/6|48/60/114|0/18/72|
|batchset50|old/cgo|p50_ns|0.957902|[0.902813, 1.036029]|5/6|48/60/114|0/18/72|
|batchset50|old/cgo|p90_ns|0.980115|[0.925018, 1.065791]|5/6|48/60/114|0/18/72|
|batchset50|old/cgo|p99_ns|0.976044|[0.854978, 1.137877]|3/6|48/60/114|0/18/72|
|batchset50|old/cgo|p999_ns|0.977312|[0.841717, 1.136828]|4/6|48/60/114|0/18/72|
|batchset50|old/cgo|process_cpu_ns_per_op|1.072381|[1.030308, 1.126055]|1/6|48/60/114|0/18/72|
|batchset50|old/cgo|user_cpu_ns_per_op|1.126641|[1.102494, 1.157119]|0/6|48/60/114|0/18/72|
|batchset50|old/cgo|system_cpu_ns_per_op|1.011625|[0.939247, 1.094460]|3/6|48/60/114|0/18/72|
|batchset50|old/cgo|drained_cpu_ns_per_op|1.093412|[1.052781, 1.145658]|0/6|48/60/114|0/18/72|
|batchset50|old/cgo|shutdown_process_cpu_ns_per_op|1.126532|[1.085858, 1.174975]|0/6|48/60/114|0/18/72|
|batchset50|new/cgo|mean_ns|0.915182|[0.836605, 0.984455]|5/6|34/46/100|0/18/72|
|batchset50|new/cgo|p50_ns|0.896581|[0.829056, 0.946751]|6/6|34/46/100|0/18/72|
|batchset50|new/cgo|p90_ns|0.950119|[0.799106, 1.106342]|3/6|34/46/100|0/18/72|
|batchset50|new/cgo|p99_ns|0.878511|[0.789918, 0.990810]|5/6|34/46/100|0/18/72|
|batchset50|new/cgo|p999_ns|1.109277|[0.836582, 1.548616]|2/6|34/46/100|0/18/72|
|batchset50|new/cgo|process_cpu_ns_per_op|0.848127|[0.821382, 0.879195]|6/6|34/46/100|0/18/72|
|batchset50|new/cgo|user_cpu_ns_per_op|0.874339|[0.842285, 0.899695]|6/6|34/46/100|0/18/72|
|batchset50|new/cgo|system_cpu_ns_per_op|0.819214|[0.767953, 0.880828]|6/6|34/46/100|0/18/72|
|batchset50|new/cgo|drained_cpu_ns_per_op|0.877778|[0.852195, 0.907495]|6/6|34/46/100|0/18/72|
|batchset50|new/cgo|shutdown_process_cpu_ns_per_op|0.918950|[0.893084, 0.948584]|6/6|34/46/100|0/18/72|

Exploratory same-host result, not a noninferiority, global-native or capacity claim. Bootstrap resamples paired run blocks, not individual operations. Independence and CI coverage are not established on a continuously running server; no multiplicity correction. analysis.json includes order/dependence diagnostics, raw-file hashes and any CPU-component ratios omitted because a counter was zero.

Window CPU is not complete-lifecycle per-transaction CPU: the Apple binding releases native transactions/futures through Go GC finalizers, which enqueue further network-thread teardown. Warmup cleanup can enter the window and late measured-transaction cleanup can fall after it. The size/direction of that unequal deferred-cleanup effect is unmeasured by the window alone; GC counts do not establish that all native cleanup completed.

Drained CPU is (getrusage after the post-measurement finalizer fixpoint and native FIFO barrier minus the pre-measurement snapshot)/measured transactions. It includes forced GC and pending setup/warmup cleanup; it is not isolated steady-state transaction CPU. Shutdown process CPU subtracts the post-identity, pre-backend-open snapshot A from RUSAGE_SELF user+system E after the final fixpoint, database close and native StopNetwork, divided by measured transactions. E-A includes client startup, fixture, warmup, validation and CSV output, but excludes earlier harness identity hashing, final JSON output and process exit. Whole process CPU through shutdown is absolute E/transactions, retained only in raw/per-run/absolute tables. It includes arm-specific executable/library identity hashing and is not paired cross-arm client-cost evidence. Go finalizers are drained using exact runtime counters; cleanups are runtime estimates and must not increase during either drain. The terminal shutdown, not GC counts alone, establishes that native network work has stopped.

The imported master590145 baseline omits C++ causal-read-risky on write-only commit GRVs (NativeAPI.actor.cpp:6577-6578 versus client/transaction.go:1980-1985,3398-3411). Without it the server waits for updateLastCommit (GrvProxyServer.actor.cpp:647-648). Identify whether each tested revision retains this pre-existing difference; results measure the actual implementations, not algorithmically identical clients.
