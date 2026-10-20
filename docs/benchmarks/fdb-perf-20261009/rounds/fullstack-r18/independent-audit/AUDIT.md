# Independent R18 raw/math/trace audit

PASS:18 raw timing runs;198 individual paired ratios and33 descriptive GM/range summaries across three comparisons exactly match published values. Both traces independently recounted from original trace.<tid> files with Decimal timestamps and exact A–E user/system/RSS matches. All whole-process and measured-window per-TID/aggregate syscall entry counts match.

|go2/go8 metric|Paired GM|Observed six-ratio range|Six ratios in block order|
|---|---:|---|---|

|mean_ns|1.034390589|[0.937792,1.296451]|0.970496, 1.057639, 0.952186, 1.296451, 0.937792, 1.030841|

|p99_ns|1.157216008|[0.787445,1.441554]|1.299722, 0.997219, 1.355056, 1.441554, 0.787445, 1.204585|

|window_cpu_ns_per_op|0.927738785|[0.886980,1.025371]|0.903613, 0.927105, 0.907060, 1.025371, 0.922597, 0.886980|

|drained_cpu_ns_per_op|0.918573189|[0.876415,1.015297]|0.898106, 0.918851, 0.895721, 1.015297, 0.913343, 0.876415|

|shutdown_cpu_ns_per_op|0.909458294|[0.866056,1.006682]|0.882685, 0.906723, 0.892245, 1.006682, 0.908853, 0.866056|


|Trace syscall|go8 phase entries|go2 phase entries|go8 /200|go2 /200|
|---|---:|---:|---:|---:|

|epoll_pwait|788|779|3.940000|3.895000|

|epoll_wait|0|0|0.000000|0.000000|

|epoll_ctl|0|0|0.000000|0.000000|

|futex|1870|1139|9.350000|5.695000|

|nanosleep|1051|864|5.255000|4.320000|

|clock_nanosleep|0|0|0.000000|0.000000|


No CI or effect-significance conclusion is calculated. These are observed descriptive ratios, not a shipping recommendation. Trace scheduling is perturbed and cannot establish CPU/latency causality;200-trace transactions are not pooled with5000-timing transactions. No native-heap/allocation claim. No sealed file modified; no VM/network actions.
