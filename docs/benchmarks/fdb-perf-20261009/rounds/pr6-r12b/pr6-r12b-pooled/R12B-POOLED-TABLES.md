# R12 + independent R12b pooled batchset50

12 paired blocks, six from each campaign; 36 runs of2500 measured+200warmup transactions. Same immutable binaries and frozenV6, fresh nonoverlapping prefixes. No profiler overlapped timing. R12b standalone tables are separate. Pooled estimates are descriptive and post-initial-result; not a replacement for the independent repeat.

Median run statistics; times in microseconds per transaction.

|Arm|Mean|p99|Window CPU|User|System|GoB/tx|Goallocs/tx|GoGC total|
|---|---:|---:|---:|---:|---:|---:|---:|---:|
|old|1950.878|3797.553|886.271|486.687|397.995|80409.482|299.577|96|
|new|2042.443|3781.264|894.744|497.658|392.301|80436.742|299.622|97|
|cgo|2104.508|3817.679|846.901|449.454|400.050|1616.099|59.001|0|

Whole-block percentile95%bootstrap CI, 10000resamples, seed794808; common indices across metrics/comparisons. Resamples12blocks without campaign stratification; assumes independent paired blocks, not established on one continuously running VM. No multiplecomparison correction. Ratios geometric within-block, not ratios of medians.

|Ratio|Metric|GM|95%CI|Pairs<1|
|---|---|---:|---|---:|
|new/old|mean_ns|1.050142|[1.016238,1.084094]|2/12|
|new/old|p99_ns|1.016413|[0.952713,1.090999]|5/12|
|new/old|process_cpu_ns_per_op|1.009971|[0.993492,1.026338]|4/12|
|new/old|user_cpu_ns_per_op|1.024821|[1.002512,1.049213]|4/12|
|new/old|system_cpu_ns_per_op|0.991270|[0.957544,1.028645]|7/12|
|new/old|go_alloc_bytes_per_op|1.000269|[0.999771,1.000795]|5/12|
|new/old|go_mallocs_per_op|1.000068|[0.999935,1.000195]|5/12|
|new/cgo|mean_ns|0.962571|[0.908506,1.008491]|8/12|
|new/cgo|p99_ns|0.977930|[0.908808,1.056944]|7/12|
|new/cgo|process_cpu_ns_per_op|1.053746|[1.037919,1.070138]|1/12|
|new/cgo|user_cpu_ns_per_op|1.101265|[1.061560,1.139646]|1/12|
|new/cgo|system_cpu_ns_per_op|1.000649|[0.952755,1.045307]|5/12|
|old/cgo|mean_ns|0.916610|[0.881996,0.942611]|12/12|
|old/cgo|p99_ns|0.962139|[0.907574,1.015822]|8/12|
|old/cgo|process_cpu_ns_per_op|1.043343|[1.025856,1.059337]|1/12|
|old/cgo|user_cpu_ns_per_op|1.074593|[1.053742,1.096234]|0/12|
|old/cgo|system_cpu_ns_per_op|1.009461|[0.969398,1.045070]|4/12|

CPU is measured-window getrusage, NOT lifecycle-complete. Native deferred cleanup is not drained by frozenV6; GoGC counts do not establish native cleanup completion. GoMemStats excludes nativeC++ heap. OLD lacks C++CAUSAL_READ_RISKY on missing-readversion commits; NEW fixes precisely that, not otherwise stacked. The current frozen harness exports GRV completion/count and commitstarted/completed counts, not GRV/commit latency aggregates: no latency split was observed or inferred.
