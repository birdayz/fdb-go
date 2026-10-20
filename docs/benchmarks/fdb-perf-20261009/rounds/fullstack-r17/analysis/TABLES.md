# R17 confirmatory results

640 serial processes; 160 paired blocks per workload; no R16 observations pooled. Owner-authorized fresh-image rebuild of the same instance, not a new independent VM allocation; see the pinned v2 addendum.

Paired effect = exp(mean of 160 within-block NEW/CGo log ratios), NOT a ratio of displayed medians. All five endpoint vectors are jointly resampled using the same global-block indices. Two-sided 99% percentile intervals, 100,000 resamples each: circular MBB L8/seed797707 and L16/seed797708. Nearest-rank zero-based indices 499 and 99499. Both upper bounds must be strictly below 1.

|Primary endpoint|Workload|Metric|GM NEW/CGo|L8 99% CI|L16 99% CI|Decision|
|---|---|---|---:|---|---|---|

|1|batchset50|shutdown_process_cpu_ns_per_op|0.862469|[0.850940, 0.874873]|[0.850373, 0.875916]|confirmed improvement|

|2|batchset50|process_cpu_ns_per_op|0.788291|[0.776788, 0.801047]|[0.776380, 0.801644]|confirmed improvement|

|3|read1000|mean_ns|0.625737|[0.603494, 0.650043]|[0.600815, 0.652733]|confirmed improvement|

|4|read1000|p99_ns|0.507259|[0.466695, 0.548761]|[0.463166, 0.549550]|confirmed improvement|

|5|read1000|shutdown_process_cpu_ns_per_op|0.908661|[0.885677, 0.933150]|[0.883870, 0.934491]|confirmed improvement|


Confirmed endpoints: 1, 2, 3, 4, 5. Not confirmed/inconclusive: none. No equivalence/noninferiority conclusion follows from an inconclusive endpoint.


## CPU and allocation scopes

- **window:** (C-B)/N: measured-window getrusage; deferred warmup/native cleanup may enter or leave this interval.
- **drained:** (D-B)/N: through finalizer fixpoint and native FIFO barrier; includes forced GC and pending setup/warmup cleanup.
- **shutdown:** (E-A)/N: includes client startup, fixture, warmup, validation, CSV, forced GC and shutdown; excludes identity hashing before A, final JSON and process exit; NOT isolated steady-state CPU.
- **absolute_E:** E/N: raw/absolute only, includes identity hashing; NEVER paired cross-arm client-cost evidence.


Go allocation/GC counters exclude the native C++ heap. GC-drain costs may differ by arm. Absolute E/N appears only in raw/absolute tables and is never paired. All nonprimary metrics are secondary/descriptive; they cannot substitute for a failed primary endpoint.


## Absolute run medians

CPU/latency values in ns/transaction; other metric units follow names. These are medians of 160 process statistics, not pooled transaction estimates.

|Workload|Arm|Metric|Median|
|---|---|---|---:|

|read1000|new|mean_ns|312151.952900|

|read1000|new|p50_ns|303783.000000|

|read1000|new|p90_ns|368014.500000|

|read1000|new|p95_ns|390167.500000|

|read1000|new|p99_ns|490523.000000|

|read1000|new|p999_ns|1451144.000000|

|read1000|new|max_ns|3853546.500000|

|read1000|new|process_cpu_ns_per_op|291231.200000|

|read1000|new|user_cpu_ns_per_op|117786.700000|

|read1000|new|system_cpu_ns_per_op|172515.300000|

|read1000|new|drained_cpu_ns_per_op|300407.200000|

|read1000|new|drained_user_cpu_ns_per_op|126774.000000|

|read1000|new|drained_system_cpu_ns_per_op|172515.300000|

|read1000|new|shutdown_process_cpu_ns_per_op|325445.800000|

|read1000|new|shutdown_user_cpu_ns_per_op|142611.700000|

|read1000|new|shutdown_system_cpu_ns_per_op|182698.500000|

|read1000|new|go_alloc_bytes_per_op|7291.056000|

|read1000|new|go_mallocs_per_op|53.071300|

|read1000|new|whole_process_cpu_through_shutdown_ns_per_op|330135.800000|

|read1000|new|process_lifetime_high_water_rss_kib|35712.000000|

|read1000|new|closed_loop_ops_per_second|3198.922266|

|read1000|new|go_gc_cycles|6.000000|

|read1000|new|drained_go_gc_cycles|8.000000|

|read1000|new|shutdown_go_gc_cycles|11.000000|

|read1000|cgo|mean_ns|499801.404400|

|read1000|cgo|p50_ns|490091.000000|

|read1000|cgo|p90_ns|603783.500000|

|read1000|cgo|p95_ns|644966.500000|

|read1000|cgo|p99_ns|858712.500000|

|read1000|cgo|p999_ns|3360132.500000|

|read1000|cgo|max_ns|5464785.000000|

|read1000|cgo|process_cpu_ns_per_op|326969.000000|

|read1000|cgo|user_cpu_ns_per_op|155796.100000|

|read1000|cgo|system_cpu_ns_per_op|174060.500000|

|read1000|cgo|drained_cpu_ns_per_op|332065.000000|

|read1000|cgo|drained_user_cpu_ns_per_op|159973.800000|

|read1000|cgo|drained_system_cpu_ns_per_op|174914.600000|

|read1000|cgo|shutdown_process_cpu_ns_per_op|355163.600000|

|read1000|cgo|shutdown_user_cpu_ns_per_op|171441.100000|

|read1000|cgo|shutdown_system_cpu_ns_per_op|186083.800000|

|read1000|cgo|go_alloc_bytes_per_op|1542.185600|

|read1000|cgo|go_mallocs_per_op|13.011600|

|read1000|cgo|whole_process_cpu_through_shutdown_ns_per_op|366370.300000|

|read1000|cgo|process_lifetime_high_water_rss_kib|70904.000000|

|read1000|cgo|closed_loop_ops_per_second|1999.040332|

|read1000|cgo|go_gc_cycles|3.000000|

|read1000|cgo|drained_go_gc_cycles|6.000000|

|read1000|cgo|shutdown_go_gc_cycles|9.000000|

|batchset50|new|mean_ns|1881038.590000|

|batchset50|new|p50_ns|1779556.000000|

|batchset50|new|p90_ns|2174368.500000|

|batchset50|new|p95_ns|2334826.500000|

|batchset50|new|p99_ns|3669897.000000|

|batchset50|new|p999_ns|6206904.500000|

|batchset50|new|max_ns|6984710.000000|

|batchset50|new|process_cpu_ns_per_op|685767.400000|

|batchset50|new|user_cpu_ns_per_op|391296.200000|

|batchset50|new|system_cpu_ns_per_op|293497.800000|

|batchset50|new|drained_cpu_ns_per_op|729330.800000|

|batchset50|new|drained_user_cpu_ns_per_op|434027.200000|

|batchset50|new|drained_system_cpu_ns_per_op|293561.400000|

|batchset50|new|shutdown_process_cpu_ns_per_op|877258.600000|

|batchset50|new|shutdown_user_cpu_ns_per_op|538739.000000|

|batchset50|new|shutdown_system_cpu_ns_per_op|335124.600000|

|batchset50|new|go_alloc_bytes_per_op|57240.601600|

|batchset50|new|go_mallocs_per_op|238.303800|

|batchset50|new|whole_process_cpu_through_shutdown_ns_per_op|887649.400000|

|batchset50|new|process_lifetime_high_water_rss_kib|82816.000000|

|batchset50|new|closed_loop_ops_per_second|531.436608|

|batchset50|new|go_gc_cycles|6.000000|

|batchset50|new|drained_go_gc_cycles|8.000000|

|batchset50|new|shutdown_go_gc_cycles|17.000000|

|batchset50|cgo|mean_ns|2142917.558000|

|batchset50|cgo|p50_ns|2031459.000000|

|batchset50|cgo|p90_ns|2475692.500000|

|batchset50|cgo|p95_ns|2656901.500000|

|batchset50|cgo|p99_ns|3982459.500000|

|batchset50|cgo|p999_ns|6610738.500000|

|batchset50|cgo|max_ns|7528528.000000|

|batchset50|cgo|process_cpu_ns_per_op|875403.200000|

|batchset50|cgo|user_cpu_ns_per_op|491909.400000|

|batchset50|cgo|system_cpu_ns_per_op|381537.000000|

|batchset50|cgo|drained_cpu_ns_per_op|895165.400000|

|batchset50|cgo|drained_user_cpu_ns_per_op|511955.400000|

|batchset50|cgo|drained_system_cpu_ns_per_op|382853.600000|

|batchset50|cgo|shutdown_process_cpu_ns_per_op|1019990.000000|

|batchset50|cgo|shutdown_user_cpu_ns_per_op|582992.200000|

|batchset50|cgo|shutdown_system_cpu_ns_per_op|431918.200000|

|batchset50|cgo|go_alloc_bytes_per_op|1616.115200|

|batchset50|cgo|go_mallocs_per_op|59.001200|

|batchset50|cgo|whole_process_cpu_through_shutdown_ns_per_op|1043119.200000|

|batchset50|cgo|process_lifetime_high_water_rss_kib|230144.000000|

|batchset50|cgo|closed_loop_ops_per_second|466.544234|

|batchset50|cgo|go_gc_cycles|0.000000|

|batchset50|cgo|drained_go_gc_cycles|3.000000|

|batchset50|cgo|shutdown_go_gc_cycles|12.000000|


## Descriptive paired GMs — no secondary inferential claims

|Workload|Metric|GM NEW/CGo|Status|
|---|---|---:|---|

|read1000|mean_ns|0.625737|descriptive only; no confirmatory CI/claim|

|read1000|p50_ns|0.635411|descriptive only; no confirmatory CI/claim|

|read1000|p90_ns|0.611546|descriptive only; no confirmatory CI/claim|

|read1000|p95_ns|0.603407|descriptive only; no confirmatory CI/claim|

|read1000|p99_ns|0.507259|descriptive only; no confirmatory CI/claim|

|read1000|p999_ns|0.470137|descriptive only; no confirmatory CI/claim|

|read1000|max_ns|0.641008|descriptive only; no confirmatory CI/claim|

|read1000|process_cpu_ns_per_op|0.882711|descriptive only; no confirmatory CI/claim|

|read1000|user_cpu_ns_per_op|0.754416|descriptive only; no confirmatory CI/claim|

|read1000|system_cpu_ns_per_op|0.997973|descriptive only; no confirmatory CI/claim|

|read1000|drained_cpu_ns_per_op|0.895105|descriptive only; no confirmatory CI/claim|

|read1000|drained_user_cpu_ns_per_op|0.787580|descriptive only; no confirmatory CI/claim|

|read1000|drained_system_cpu_ns_per_op|0.993873|descriptive only; no confirmatory CI/claim|

|read1000|shutdown_process_cpu_ns_per_op|0.908661|descriptive only; no confirmatory CI/claim|

|read1000|shutdown_user_cpu_ns_per_op|0.828070|descriptive only; no confirmatory CI/claim|

|read1000|shutdown_system_cpu_ns_per_op|0.982963|descriptive only; no confirmatory CI/claim|

|read1000|go_alloc_bytes_per_op|4.728941|descriptive only; no confirmatory CI/claim|

|read1000|go_mallocs_per_op|4.078869|descriptive only; no confirmatory CI/claim|

|batchset50|mean_ns|0.876706|descriptive only; no confirmatory CI/claim|

|batchset50|p50_ns|0.873471|descriptive only; no confirmatory CI/claim|

|batchset50|p90_ns|0.882186|descriptive only; no confirmatory CI/claim|

|batchset50|p95_ns|0.884538|descriptive only; no confirmatory CI/claim|

|batchset50|p99_ns|0.907499|descriptive only; no confirmatory CI/claim|

|batchset50|p999_ns|0.952385|descriptive only; no confirmatory CI/claim|

|batchset50|max_ns|0.940571|descriptive only; no confirmatory CI/claim|

|batchset50|process_cpu_ns_per_op|0.788291|descriptive only; no confirmatory CI/claim|

|batchset50|user_cpu_ns_per_op|0.806678|descriptive only; no confirmatory CI/claim|

|batchset50|system_cpu_ns_per_op|0.765589|descriptive only; no confirmatory CI/claim|

|batchset50|drained_cpu_ns_per_op|0.816052|descriptive only; no confirmatory CI/claim|

|batchset50|drained_user_cpu_ns_per_op|0.856925|descriptive only; no confirmatory CI/claim|

|batchset50|drained_system_cpu_ns_per_op|0.762869|descriptive only; no confirmatory CI/claim|

|batchset50|shutdown_process_cpu_ns_per_op|0.862469|descriptive only; no confirmatory CI/claim|

|batchset50|shutdown_user_cpu_ns_per_op|0.931613|descriptive only; no confirmatory CI/claim|

|batchset50|shutdown_system_cpu_ns_per_op|0.770893|descriptive only; no confirmatory CI/claim|

|batchset50|go_alloc_bytes_per_op|35.417712|descriptive only; no confirmatory CI/claim|

|batchset50|go_mallocs_per_op|4.038932|descriptive only; no confirmatory CI/claim|


## Provenance and scope

All 80 predeclared schedules, identities, exact raw counts, zero retries/errors, lifecycle fixpoints and frozen V7 pre-run quiescence gates passed. During-run telemetry is descriptive only, not a new reject/exclusion rule. See validation.json, input-files.csv, per-run.csv, per-block.csv, primary-per-block.csv, and provenance.json. No outcome-based exclusions, optional stopping, favorable replacement, or R16 pooling.

Coverage is conditional on block-resampling assumptions on this one rebuilt instance; not independent VM/fleet replication, universal Go-vs-C++ superiority, general capacity, or a causal decomposition of PRs. Benchmark success does not establish source correctness, review/CI approval, or permission to merge.
