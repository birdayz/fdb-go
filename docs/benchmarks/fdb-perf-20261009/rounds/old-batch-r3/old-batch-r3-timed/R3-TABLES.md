# R3 longer-run batchset50 — unprofiled

Existing old/CGo binaries,3000 measured transactions+200 warmup/run,one worker,six counterbalanced blocks. Absolute values are medians of six run statistics; paired ratios are within-block geometric means, NOT ratios of medians. CPU includes process user+system time.

|Arm|Mean µs|p99 µs|CPU µs/tx|User µs/tx|System µs/tx|Go GC cycles total|Go GC cycles/tx aggregate|
|---|---:|---:|---:|---:|---:|---:|---:|
|cgo|2225.945|3881.553|847.296|474.675|380.741|4|0.000222|
|old|2017.505|3841.589|905.354|514.630|390.752|50|0.002778|

|Ratio|Mean|p99|CPU|User|System|
|---|---:|---:|---:|---:|---:|
|old/cgo|0.926298|0.947904|1.072634|1.088531|1.055169|

|Block|Order|Mean ratio|p99 ratio|CPU ratio|User ratio|System ratio|Old Go GC/tx|CGo Go GC/tx|
|---|---|---:|---:|---:|---:|---:|---:|---:|
|0|cgo/old|0.816593|0.690220|1.109004|1.211598|0.992241|0.002667|0.000333|
|1|cgo/old|0.961634|1.133868|1.035758|0.960827|1.148351|0.002667|0.000000|
|2|old/cgo|0.886524|0.962369|1.051555|1.012309|1.107910|0.002667|0.000333|
|3|cgo/old|1.071184|1.117775|1.092721|1.105140|1.078092|0.002667|0.000000|
|4|old/cgo|0.919469|0.816184|1.072061|1.111871|1.020737|0.003000|0.000333|
|5|old/cgo|0.921289|1.055727|1.076360|1.148829|0.993495|0.003000|0.000333|

Six-block exploratory single-VM result, not capacity/noninferiority/global-native claims. Longer duration and warmup are changed conditions, not a causal isolation of GC/warmup. Native internal counters are unobserved. Go GC/MemStats values include only the Go heap even in CGo, not the native allocator. Source/binary/byte checks are not main-module correctness approval.
