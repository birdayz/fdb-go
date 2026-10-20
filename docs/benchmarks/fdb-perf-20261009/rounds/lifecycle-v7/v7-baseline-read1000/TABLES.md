# Client benchmark

Absolute times are medians of run-level statistics in µs/transaction. Ratios are geometric means of within-block ratios, NOT ratios of these medians. Window CPU is measured-phase getrusage user+system; drained and shutdown scopes are defined below. Go allocation/GC counters exclude the native C++ heap; do not compare total Go/CGo memory from these columns. Native client counters are unobserved.

|Workload|Arm|Runs|Transactions|Mean|p50|p90|p99|p99.9|Window CPU|Window User CPU|Window System CPU|Drained CPU|Shutdown process CPU (E-A)|Whole process through shutdown (E; unpaired)|Go B/tx|Go allocs/tx|Go GC cycles: window/drained/shutdown|
|---|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|
|read1000|cgo|6|30000|562.957|550.716|643.751|909.554|3658.375|367.542|162.767|204.775|373.103|394.914|406.093|1541.094|13.011|18/36/54|
|read1000|go|6|30000|436.629|426.776|487.591|637.676|1528.850|460.554|185.487|275.627|470.606|501.196|506.150|7982.014|63.228|36/48/66|

|Workload|Ratio|Metric|Paired GM|Nominal 95% block-bootstrap CI|Pairs <1|Numerator Go GC: window/drained/shutdown|Denominator Go GC: window/drained/shutdown|
|---|---|---|---:|---|---:|---:|---:|
|read1000|go/cgo|mean_ns|0.796266|[0.749674, 0.860911]|6/6|36/48/66|18/36/54|
|read1000|go/cgo|p50_ns|0.820514|[0.755769, 0.921477]|5/6|36/48/66|18/36/54|
|read1000|go/cgo|p90_ns|0.762800|[0.738647, 0.794180]|6/6|36/48/66|18/36/54|
|read1000|go/cgo|p99_ns|0.704760|[0.673278, 0.735865]|6/6|36/48/66|18/36/54|
|read1000|go/cgo|p999_ns|0.480451|[0.334876, 0.723022]|5/6|36/48/66|18/36/54|
|read1000|go/cgo|process_cpu_ns_per_op|1.282384|[1.217883, 1.366370]|0/6|36/48/66|18/36/54|
|read1000|go/cgo|user_cpu_ns_per_op|1.149787|[1.077088, 1.238314]|0/6|36/48/66|18/36/54|
|read1000|go/cgo|system_cpu_ns_per_op|1.389742|[1.329580, 1.468445]|0/6|36/48/66|18/36/54|
|read1000|go/cgo|drained_cpu_ns_per_op|1.288648|[1.225303, 1.369587]|0/6|36/48/66|18/36/54|
|read1000|go/cgo|shutdown_process_cpu_ns_per_op|1.295009|[1.238599, 1.366772]|0/6|36/48/66|18/36/54|

Exploratory same-host result, not a noninferiority, global-native or capacity claim. Bootstrap resamples paired run blocks, not individual operations. Independence and CI coverage are not established on a continuously running server; no multiplicity correction. analysis.json includes order/dependence diagnostics, raw-file hashes and any CPU-component ratios omitted because a counter was zero.

Window CPU is not complete-lifecycle per-transaction CPU: the Apple binding releases native transactions/futures through Go GC finalizers, which enqueue further network-thread teardown. Warmup cleanup can enter the window and late measured-transaction cleanup can fall after it. The size/direction of that unequal deferred-cleanup effect is unmeasured by the window alone; GC counts do not establish that all native cleanup completed.

Drained CPU is (getrusage after the post-measurement finalizer fixpoint and native FIFO barrier minus the pre-measurement snapshot)/measured transactions. It includes forced GC and pending setup/warmup cleanup; it is not isolated steady-state transaction CPU. Shutdown process CPU subtracts the post-identity, pre-backend-open snapshot A from RUSAGE_SELF user+system E after the final fixpoint, database close and native StopNetwork, divided by measured transactions. E-A includes client startup, fixture, warmup, validation and CSV output, but excludes earlier harness identity hashing, final JSON output and process exit. Whole process CPU through shutdown is absolute E/transactions, retained only in raw/per-run/absolute tables. It includes arm-specific executable/library identity hashing and is not paired cross-arm client-cost evidence. Go finalizers are drained using exact runtime counters; cleanups are runtime estimates and must not increase during either drain. The terminal shutdown, not GC counts alone, establishes that native network work has stopped.
