# R15 PR9 eager lone Get — batchset50

PR9 lone pipelined Get eager write, incremental over PR5v3 on exact8ab. OLD=base+patch95c1a474c2eb59f7b493b538c0d7ab179d61617e3aca4f78a39c3ee0da26c7a8; NEW=OLD+patchf81ad05e2af0c0b68fe4558a824e71aea815267435b7f6dad1b52686c2e6991c. Neither is the R11combinedstack. Both retain missing CAUSAL_READ_RISKY on write-only commit GRV. FrozenreviewedV7; window(C-B),drained(D-B),shutdown(E-A) perN; absoluteE unpairedonly. No profiler/build overlaps.

2500measured+200warmup,workers1,18runs/sixthree-armorders. Absolute times are medians of6run statistics in microseconds per transaction. AbsoluteE CPU is unpaired only; no cross-arm client-cost interpretation. GoMemStats excludes nativeC++ heap.

|Arm|Mean|p99|Window CPU|User|System|Drained CPU|Shutdown(E-A) CPU|AbsoluteE CPU(unpaired)|GoB/tx|Goallocs/tx|GoGC window/drained/shutdown|ForcedGC rounds measure/shutdown|
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---|---|
|old|2057.055|4010.802|828.147|482.770|333.803|873.817|1033.742|1043.844|80245.653|298.409|48/60/114|[2, 2, 2, 2, 2, 2] / [2, 2, 2, 2, 2, 2]|
|new|1990.923|4240.657|822.022|475.681|338.318|867.171|1024.355|1034.341|80159.629|298.353|48/60/114|[2, 2, 2, 2, 2, 2] / [2, 2, 2, 2, 2, 2]|
|cgo|2253.664|4119.752|857.940|450.819|412.675|881.361|1002.932|1024.690|1616.157|59.002|0/18/72|[3, 3, 3, 3, 3, 3] / [3, 3, 3, 3, 3, 3]|

Paired geometric means of6within-block ratios, NOT ratios of displayedmedians; percentile95% whole-block bootstrap CI,10000resamples,seed795707,common resampledindices acrossmetrics/comparisons.

|Ratio|Metric|GM|95%CI|Pairs<1|
|---|---|---:|---|---:|
|new/old|mean_ns|0.981014|[0.910431,1.058121]|3/6|
|new/old|p99_ns|1.021671|[0.931810,1.110298]|2/6|
|new/old|process_cpu_ns_per_op|0.993440|[0.963288,1.030174]|4/6|
|new/old|user_cpu_ns_per_op|0.979019|[0.963759,1.000636]|5/6|
|new/old|system_cpu_ns_per_op|1.014389|[0.951949,1.079580]|2/6|
|new/old|drained_cpu_ns_per_op|0.990992|[0.963083,1.023038]|4/6|
|new/old|shutdown_process_cpu_ns_per_op|0.986569|[0.965712,1.009774]|4/6|
|new/old|go_alloc_bytes_per_op|0.999144|[0.997636,1.000379]|3/6|
|new/old|go_mallocs_per_op|0.999885|[0.999731,1.000029]|4/6|
|new/cgo|mean_ns|0.905299|[0.868244,0.947021]|6/6|
|new/cgo|p99_ns|0.986652|[0.896677,1.070541]|2/6|
|new/cgo|process_cpu_ns_per_op|0.965210|[0.935913,0.995956]|5/6|
|new/cgo|user_cpu_ns_per_op|1.060227|[1.043825,1.084937]|0/6|
|new/cgo|system_cpu_ns_per_op|0.859942|[0.817121,0.910960]|6/6|
|new/cgo|drained_cpu_ns_per_op|0.988697|[0.961123,1.018896]|4/6|
|new/cgo|shutdown_process_cpu_ns_per_op|1.021920|[0.999453,1.046647]|2/6|
|old/cgo|mean_ns|0.922820|[0.860909,0.998967]|5/6|
|old/cgo|p99_ns|0.965724|[0.882982,1.049923]|4/6|
|old/cgo|process_cpu_ns_per_op|0.971583|[0.942022,1.006726]|4/6|
|old/cgo|user_cpu_ns_per_op|1.082949|[1.071237,1.098170]|0/6|
|old/cgo|system_cpu_ns_per_op|0.847744|[0.798689,0.903943]|6/6|
|old/cgo|drained_cpu_ns_per_op|0.997684|[0.968105,1.032641]|3/6|
|old/cgo|shutdown_process_cpu_ns_per_op|1.035832|[1.009827,1.065756]|1/6|

V7 scopes: window=(C-B)/N; drained=(D-B)/N includes forcedGC and pending setup/warmup cleanup after nativeFIFO barrier; shutdown=(E-A)/N includes client startup,fixture,warmup,validation,CSV and forcedGC through DBclose/nativeStopNetworkjoin. A excludes earlier arm-specific executable/libraryhashing. AbsoluteE/N is raw/absolute-only, never paired. FinalJSON/process exit excluded. Drains2or moreforcedGCs may differbyarm. Notisolatedsteady-stateCPU. SixsameVM exploratoryblocks do not establish independence/nominalCIcoverage; no multiplicitycorrection or equivalence/noninferiority/globalcapacityclaim. PR9parentcorrectness/reviews/CI are not proved by benchmarksuccess.
