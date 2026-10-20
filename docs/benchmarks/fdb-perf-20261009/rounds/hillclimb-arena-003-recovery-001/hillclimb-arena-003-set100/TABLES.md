# hillclimb-arena-003-set100

Absolute values are medians of six run-level statistics; ratios are geometric means of six paired within-block ratios, NOT ratios of these medians. µs per transaction operation. old=published affef937, new=affef937+arena003, cgo=immutable native7.3.77.

|Workload|Arm|Mean µs/op|p99 µs/op|CPU µs/op|
|---|---|---:|---:|---:|
|set100|cgo|2068.061|3737.886|553.028|
|set100|old|1822.659|3742.851|567.265|
|set100|new|1799.518|3277.785|561.692|

|Workload|Ratio|Mean|p99|CPU/op|
|---|---|---:|---:|---:|
|set100|new/old|0.981586|0.920249|1.009546|
|set100|new/cgo|0.873100|0.879419|1.013140|
|set100|old/cgo|0.889479|0.955631|1.003560|

Six paired exploratory run blocks; geometric mean within-block ratios, no confirmatory/noninferiority/global-speed/capacity claim. Native internal counters unobserved, not zero. Go MemStats excludes C++ heap, so no Go/CGo allocation ratios. Pooled tails descriptive. Lifetime RSS includes setup, not measured-phase peak. Source/binary identity verified per run; rawchecks do not approve candidate semantics.
