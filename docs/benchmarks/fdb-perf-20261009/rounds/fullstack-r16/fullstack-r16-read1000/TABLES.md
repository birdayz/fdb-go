# Client benchmark

Absolute times are medians of run-level statistics in µs/transaction. Ratios are geometric means of within-block ratios, NOT ratios of these medians. Window CPU is measured-phase getrusage user+system; drained and shutdown scopes are defined below. Go allocation/GC counters exclude the native C++ heap; do not compare total Go/CGo memory from these columns. Native client counters are unobserved.

|Workload|Arm|Runs|Transactions|Mean|p50|p90|p99|p99.9|Window CPU|Window User CPU|Window System CPU|Drained CPU|Shutdown process CPU (E-A)|Whole process through shutdown (E; unpaired)|Go B/tx|Go allocs/tx|Go GC cycles: window/drained/shutdown|
|---|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|
|read1000|cgo|6|30000|542.030|528.746|646.926|870.538|3116.798|364.394|159.865|204.529|370.691|393.121|404.148|1542.198|13.012|18/36/54|
|read1000|new|6|30000|365.973|354.053|417.248|561.941|1578.219|309.232|111.567|195.652|318.581|345.052|350.070|7288.027|53.067|36/48/66|
|read1000|old|6|30000|442.053|430.587|495.997|656.645|1558.649|468.134|185.223|281.642|477.029|508.971|513.920|7975.011|63.196|36/48/66|

|Workload|Ratio|Metric|Paired GM|Nominal 95% block-bootstrap CI|Pairs <1|Numerator Go GC: window/drained/shutdown|Denominator Go GC: window/drained/shutdown|
|---|---|---|---:|---|---:|---:|---:|
|read1000|old/cgo|mean_ns|0.837029|[0.777940, 0.914683]|5/6|36/48/66|18/36/54|
|read1000|old/cgo|p50_ns|0.846602|[0.760961, 0.955838]|5/6|36/48/66|18/36/54|
|read1000|old/cgo|p90_ns|0.753853|[0.709857, 0.798897]|6/6|36/48/66|18/36/54|
|read1000|old/cgo|p99_ns|0.776978|[0.723482, 0.830020]|6/6|36/48/66|18/36/54|
|read1000|old/cgo|p999_ns|0.668867|[0.441000, 1.060354]|4/6|36/48/66|18/36/54|
|read1000|old/cgo|process_cpu_ns_per_op|1.366760|[1.213357, 1.544467]|0/6|36/48/66|18/36/54|
|read1000|old/cgo|user_cpu_ns_per_op|1.269026|[1.108724, 1.480103]|0/6|36/48/66|18/36/54|
|read1000|old/cgo|system_cpu_ns_per_op|1.441131|[1.284883, 1.630295]|0/6|36/48/66|18/36/54|
|read1000|old/cgo|drained_cpu_ns_per_op|1.371460|[1.220403, 1.547651]|0/6|36/48/66|18/36/54|
|read1000|old/cgo|shutdown_process_cpu_ns_per_op|1.375032|[1.228598, 1.540379]|0/6|36/48/66|18/36/54|
|read1000|new/cgo|mean_ns|0.686314|[0.640268, 0.737483]|6/6|36/48/66|18/36/54|
|read1000|new/cgo|p50_ns|0.693902|[0.632240, 0.767222]|6/6|36/48/66|18/36/54|
|read1000|new/cgo|p90_ns|0.634696|[0.607938, 0.663150]|6/6|36/48/66|18/36/54|
|read1000|new/cgo|p99_ns|0.642212|[0.623205, 0.663485]|6/6|36/48/66|18/36/54|
|read1000|new/cgo|p999_ns|0.564185|[0.408655, 0.800611]|5/6|36/48/66|18/36/54|
|read1000|new/cgo|process_cpu_ns_per_op|0.907813|[0.808774, 1.020350]|4/6|36/48/66|18/36/54|
|read1000|new/cgo|user_cpu_ns_per_op|0.775817|[0.686026, 0.901443]|5/6|36/48/66|18/36/54|
|read1000|new/cgo|system_cpu_ns_per_op|1.006485|[0.897755, 1.138281]|3/6|36/48/66|18/36/54|
|read1000|new/cgo|drained_cpu_ns_per_op|0.919691|[0.819642, 1.033188]|4/6|36/48/66|18/36/54|
|read1000|new/cgo|shutdown_process_cpu_ns_per_op|0.935043|[0.837465, 1.053732]|4/6|36/48/66|18/36/54|

Exploratory same-host result, not a noninferiority, global-native or capacity claim. Bootstrap resamples paired run blocks, not individual operations. Independence and CI coverage are not established on a continuously running server; no multiplicity correction. analysis.json includes order/dependence diagnostics, raw-file hashes and any CPU-component ratios omitted because a counter was zero.

Window CPU is not complete-lifecycle per-transaction CPU: the Apple binding releases native transactions/futures through Go GC finalizers, which enqueue further network-thread teardown. Warmup cleanup can enter the window and late measured-transaction cleanup can fall after it. The size/direction of that unequal deferred-cleanup effect is unmeasured by the window alone; GC counts do not establish that all native cleanup completed.

Drained CPU is (getrusage after the post-measurement finalizer fixpoint and native FIFO barrier minus the pre-measurement snapshot)/measured transactions. It includes forced GC and pending setup/warmup cleanup; it is not isolated steady-state transaction CPU. Shutdown process CPU subtracts the post-identity, pre-backend-open snapshot A from RUSAGE_SELF user+system E after the final fixpoint, database close and native StopNetwork, divided by measured transactions. E-A includes client startup, fixture, warmup, validation and CSV output, but excludes earlier harness identity hashing, final JSON output and process exit. Whole process CPU through shutdown is absolute E/transactions, retained only in raw/per-run/absolute tables. It includes arm-specific executable/library identity hashing and is not paired cross-arm client-cost evidence. Go finalizers are drained using exact runtime counters; cleanups are runtime estimates and must not increase during either drain. The terminal shutdown, not GC counts alone, establishes that native network work has stopped.
