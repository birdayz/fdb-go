# Client benchmark

Absolute times are medians of run-level statistics in µs/transaction. Ratios are geometric means of within-block ratios, NOT ratios of these medians. Window CPU is measured-phase getrusage user+system; drained and shutdown scopes are defined below. Go allocation/GC counters exclude the native C++ heap; do not compare total Go/CGo memory from these columns. Native client counters are unobserved.

|Workload|Arm|Runs|Transactions|Mean|p50|p90|p99|p99.9|Window CPU|Window User CPU|Window System CPU|Drained CPU|Shutdown process CPU (E-A)|Whole process through shutdown (E; unpaired)|Go B/tx|Go allocs/tx|Go GC cycles: window/drained/shutdown|
|---|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|
|read1000|cgo|6|30000|539.781|533.846|666.740|857.360|2639.872|365.905|155.618|211.412|371.602|391.116|402.618|1541.652|13.011|18/36/54|
|read1000|new|6|30000|389.789|377.042|446.632|631.356|1604.164|381.153|144.815|238.141|390.705|420.060|425.326|7453.198|61.084|36/48/66|
|read1000|old|6|30000|438.771|430.377|491.196|670.040|1540.195|438.341|165.905|266.301|448.678|480.521|485.559|7773.497|62.106|36/48/66|

|Workload|Ratio|Metric|Paired GM|Nominal 95% block-bootstrap CI|Pairs <1|Numerator Go GC: window/drained/shutdown|Denominator Go GC: window/drained/shutdown|
|---|---|---|---:|---|---:|---:|---:|
|read1000|old/cgo|mean_ns|0.796495|[0.726220, 0.900055]|5/6|36/48/66|18/36/54|
|read1000|old/cgo|p50_ns|0.803404|[0.697633, 0.982662]|5/6|36/48/66|18/36/54|
|read1000|old/cgo|p90_ns|0.720174|[0.683873, 0.755534]|6/6|36/48/66|18/36/54|
|read1000|old/cgo|p99_ns|0.760139|[0.701352, 0.820950]|6/6|36/48/66|18/36/54|
|read1000|old/cgo|p999_ns|0.627892|[0.479927, 0.820810]|5/6|36/48/66|18/36/54|
|read1000|old/cgo|process_cpu_ns_per_op|1.188960|[1.099340, 1.307397]|0/6|36/48/66|18/36/54|
|read1000|old/cgo|user_cpu_ns_per_op|1.072486|[0.987042, 1.188119]|2/6|36/48/66|18/36/54|
|read1000|old/cgo|system_cpu_ns_per_op|1.277307|[1.168053, 1.409372]|0/6|36/48/66|18/36/54|
|read1000|old/cgo|drained_cpu_ns_per_op|1.197906|[1.107952, 1.315834]|0/6|36/48/66|18/36/54|
|read1000|old/cgo|shutdown_process_cpu_ns_per_op|1.219219|[1.131994, 1.330216]|0/6|36/48/66|18/36/54|
|read1000|new/cgo|mean_ns|0.726729|[0.643004, 0.834263]|6/6|36/48/66|18/36/54|
|read1000|new/cgo|p50_ns|0.726746|[0.605739, 0.911930]|5/6|36/48/66|18/36/54|
|read1000|new/cgo|p90_ns|0.662860|[0.618161, 0.703460]|6/6|36/48/66|18/36/54|
|read1000|new/cgo|p99_ns|0.734684|[0.663908, 0.808976]|6/6|36/48/66|18/36/54|
|read1000|new/cgo|p999_ns|0.643502|[0.466574, 0.840634]|6/6|36/48/66|18/36/54|
|read1000|new/cgo|process_cpu_ns_per_op|1.057480|[0.969632, 1.163473]|1/6|36/48/66|18/36/54|
|read1000|new/cgo|user_cpu_ns_per_op|0.926666|[0.858534, 1.019420]|5/6|36/48/66|18/36/54|
|read1000|new/cgo|system_cpu_ns_per_op|1.156590|[1.048103, 1.286450]|1/6|36/48/66|18/36/54|
|read1000|new/cgo|drained_cpu_ns_per_op|1.067625|[0.980725, 1.177305]|1/6|36/48/66|18/36/54|
|read1000|new/cgo|shutdown_process_cpu_ns_per_op|1.087959|[1.001085, 1.193653]|1/6|36/48/66|18/36/54|

Exploratory same-host result, not a noninferiority, global-native or capacity claim. Bootstrap resamples paired run blocks, not individual operations. Independence and CI coverage are not established on a continuously running server; no multiplicity correction. analysis.json includes order/dependence diagnostics, raw-file hashes and any CPU-component ratios omitted because a counter was zero.

Window CPU is not complete-lifecycle per-transaction CPU: the Apple binding releases native transactions/futures through Go GC finalizers, which enqueue further network-thread teardown. Warmup cleanup can enter the window and late measured-transaction cleanup can fall after it. The size/direction of that unequal deferred-cleanup effect is unmeasured by the window alone; GC counts do not establish that all native cleanup completed.

Drained CPU is (getrusage after the post-measurement finalizer fixpoint and native FIFO barrier minus the pre-measurement snapshot)/measured transactions. It includes forced GC and pending setup/warmup cleanup; it is not isolated steady-state transaction CPU. Shutdown process CPU subtracts the post-identity, pre-backend-open snapshot A from RUSAGE_SELF user+system E after the final fixpoint, database close and native StopNetwork, divided by measured transactions. E-A includes client startup, fixture, warmup, validation and CSV output, but excludes earlier harness identity hashing, final JSON output and process exit. Whole process CPU through shutdown is absolute E/transactions, retained only in raw/per-run/absolute tables. It includes arm-specific executable/library identity hashing and is not paired cross-arm client-cost evidence. Go finalizers are drained using exact runtime counters; cleanups are runtime estimates and must not increase during either drain. The terminal shutdown, not GC counts alone, establishes that native network work has stopped.
