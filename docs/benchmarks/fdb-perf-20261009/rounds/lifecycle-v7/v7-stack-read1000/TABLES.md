# Client benchmark

Absolute times are medians of run-level statistics in µs/transaction. Ratios are geometric means of within-block ratios, NOT ratios of these medians. Window CPU is measured-phase getrusage user+system; drained and shutdown scopes are defined below. Go allocation/GC counters exclude the native C++ heap; do not compare total Go/CGo memory from these columns. Native client counters are unobserved.

|Workload|Arm|Runs|Transactions|Mean|p50|p90|p99|p99.9|Window CPU|Window User CPU|Window System CPU|Drained CPU|Shutdown process CPU (E-A)|Whole process through shutdown (E; unpaired)|Go B/tx|Go allocs/tx|Go GC cycles: window/drained/shutdown|
|---|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|
|read1000|cgo|6|30000|544.838|539.101|646.615|909.058|3752.399|347.074|145.101|201.973|353.067|374.489|385.801|1542.165|13.011|18/36/54|
|read1000|go|6|30000|386.563|376.742|429.058|612.026|1370.281|365.173|135.585|223.936|374.887|403.521|408.520|7803.584|64.086|36/48/66|

|Workload|Ratio|Metric|Paired GM|Nominal 95% block-bootstrap CI|Pairs <1|Numerator Go GC: window/drained/shutdown|Denominator Go GC: window/drained/shutdown|
|---|---|---|---:|---|---:|---:|---:|
|read1000|go/cgo|mean_ns|0.719360|[0.672183, 0.792341]|6/6|36/48/66|18/36/54|
|read1000|go/cgo|p50_ns|0.742106|[0.662019, 0.841941]|6/6|36/48/66|18/36/54|
|read1000|go/cgo|p90_ns|0.662704|[0.622657, 0.709111]|6/6|36/48/66|18/36/54|
|read1000|go/cgo|p99_ns|0.673407|[0.629053, 0.715285]|6/6|36/48/66|18/36/54|
|read1000|go/cgo|p999_ns|0.491590|[0.366643, 0.703855]|5/6|36/48/66|18/36/54|
|read1000|go/cgo|process_cpu_ns_per_op|1.049768|[0.969663, 1.169499]|4/6|36/48/66|18/36/54|
|read1000|go/cgo|user_cpu_ns_per_op|0.926752|[0.824148, 1.043846]|5/6|36/48/66|18/36/54|
|read1000|go/cgo|system_cpu_ns_per_op|1.141071|[1.055757, 1.282798]|0/6|36/48/66|18/36/54|
|read1000|go/cgo|drained_cpu_ns_per_op|1.059092|[0.980502, 1.176652]|2/6|36/48/66|18/36/54|
|read1000|go/cgo|shutdown_process_cpu_ns_per_op|1.073174|[1.000030, 1.192793]|1/6|36/48/66|18/36/54|

Exploratory same-host result, not a noninferiority, global-native or capacity claim. Bootstrap resamples paired run blocks, not individual operations. Independence and CI coverage are not established on a continuously running server; no multiplicity correction. analysis.json includes order/dependence diagnostics, raw-file hashes and any CPU-component ratios omitted because a counter was zero.

Window CPU is not complete-lifecycle per-transaction CPU: the Apple binding releases native transactions/futures through Go GC finalizers, which enqueue further network-thread teardown. Warmup cleanup can enter the window and late measured-transaction cleanup can fall after it. The size/direction of that unequal deferred-cleanup effect is unmeasured by the window alone; GC counts do not establish that all native cleanup completed.

Drained CPU is (getrusage after the post-measurement finalizer fixpoint and native FIFO barrier minus the pre-measurement snapshot)/measured transactions. It includes forced GC and pending setup/warmup cleanup; it is not isolated steady-state transaction CPU. Shutdown process CPU subtracts the post-identity, pre-backend-open snapshot A from RUSAGE_SELF user+system E after the final fixpoint, database close and native StopNetwork, divided by measured transactions. E-A includes client startup, fixture, warmup, validation and CSV output, but excludes earlier harness identity hashing, final JSON output and process exit. Whole process CPU through shutdown is absolute E/transactions, retained only in raw/per-run/absolute tables. It includes arm-specific executable/library identity hashing and is not paired cross-arm client-cost evidence. Go finalizers are drained using exact runtime counters; cleanups are runtime estimates and must not increase during either drain. The terminal shutdown, not GC counts alone, establishes that native network work has stopped.
