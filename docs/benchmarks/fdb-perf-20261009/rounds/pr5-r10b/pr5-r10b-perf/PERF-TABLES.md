# R10B read1000 perf-stat diagnostics

ONE old then ONE new process,5000 measured+200warmup each. These counters cover the entire process, INCLUDING startup, executable hashing, setup, warmup, measurement, persisted validation and shutdown. Divide by5000 only for a whole-process-per-measured-transaction normalization; NOT measured-phase counters. perf task-clock is NOT interchangeable with measured-phase getrusage user+system CPU. No CGo control or counterbalanced diagnostic repeats; instrumented timings do not enter campaign conclusions.

|Arm|Event|Whole-process value|Unit|Value / 5000|Running %|
|---|---|---:|---|---:|---:|
|old|context-switches|57743.000000|count|11.548600|100.00|
|old|task-clock|1488.300000|msec|0.297660|100.00|
|new|context-switches|54279.000000|count|10.855800|100.00|
|new|task-clock|1939.580000|msec|0.387916|100.00|

Whole-process context-switches new/old ratio: 0.940010.

Whole-process task-clock new/old ratio: 1.303218.

old: measured/warmup callback retries 0/0.

new: measured/warmup callback retries 0/0.
