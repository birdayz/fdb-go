# R16 cumulative fullstack — batchset50

Cumulative fullstack on exact7922a2e5a8e3a2d6cf17dfee8b8134181c88b298, patchd0a6ae57ac1143aa1247ce2461f84f74264825f901a53cb7ab943a0efe83be85 (PRs812,818,816,834,824,827,841,828,832 plus reply-error allocation). OLD=base; NEW=base+patch. OLD retains missing CAUSAL_READ_RISKY on write-only commit GRV; NEW fixes it, part of the declared intervention. CGo is unchanged V7native7.3.77 built from8ab; dependency/build pins identical to7922 (empty root-module equivalence diff archived). FrozenreviewedV7: window(C-B),drained(D-B),shutdown(E-A) perN; absoluteE unpairedonly. No profiler/build overlaps.

2500measured+200warmup,workers1,18runs/sixthree-armorders. Absolute times are medians of6run statistics in microseconds per transaction. AbsoluteE CPU is unpaired only; no cross-arm client-cost interpretation. GoMemStats excludes nativeC++ heap.

|Arm|Mean|p99|Window CPU|User|System|Drained CPU|Shutdown(E-A) CPU|Shutdown(E-A) User|Shutdown(E-A) System|AbsoluteE CPU(unpaired)|GoB/tx|Goallocs/tx|GoGC window/drained/shutdown|ForcedGC rounds measure/shutdown|
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---|---|
|old|1962.591|3741.648|902.874|507.921|402.339|942.012|1109.822|670.891|453.216|1119.777|80453.317|299.590|48/60/114|[2, 2, 2, 2, 2, 2] / [2, 2, 2, 2, 2, 2]|
|new|1934.086|3605.026|716.943|393.207|321.364|761.750|907.771|543.727|362.944|918.193|57262.926|238.328|34/46/100|[2, 2, 2, 2, 2, 2] / [2, 2, 2, 2, 2, 2]|
|cgo|2069.652|4291.775|846.506|442.354|394.953|868.516|984.250|532.109|449.267|1007.034|1616.090|59.001|0/18/72|[3, 3, 3, 3, 3, 3] / [3, 3, 3, 3, 3, 3]|

Paired geometric means of6within-block ratios, NOT ratios of displayedmedians; percentile95% whole-block bootstrap CI,10000resamples,seed796707,common resampledindices acrossmetrics/comparisons.

|Ratio|Metric|GM|95%CI|Pairs<1|
|---|---|---:|---|---:|
|new/old|mean_ns|0.946800|[0.853803,1.036979]|3/6|
|new/old|p99_ns|0.900073|[0.758477,1.055400]|3/6|
|new/old|process_cpu_ns_per_op|0.790882|[0.770418,0.811889]|6/6|
|new/old|user_cpu_ns_per_op|0.776058|[0.753710,0.800677]|6/6|
|new/old|system_cpu_ns_per_op|0.809800|[0.788763,0.832927]|6/6|
|new/old|drained_cpu_ns_per_op|0.802788|[0.781993,0.822586]|6/6|
|new/old|shutdown_process_cpu_ns_per_op|0.815733|[0.796992,0.834184]|6/6|
|new/old|shutdown_user_cpu_ns_per_op|0.819998|[0.793528,0.853968]|6/6|
|new/old|shutdown_system_cpu_ns_per_op|0.810068|[0.792484,0.828542]|6/6|
|new/old|go_alloc_bytes_per_op|0.711941|[0.711413,0.712307]|6/6|
|new/old|go_mallocs_per_op|0.795447|[0.795305,0.795598]|6/6|
|new/cgo|mean_ns|0.915182|[0.836605,0.984455]|5/6|
|new/cgo|p99_ns|0.878511|[0.789918,0.992918]|5/6|
|new/cgo|process_cpu_ns_per_op|0.848127|[0.821382,0.879195]|6/6|
|new/cgo|user_cpu_ns_per_op|0.874339|[0.842285,0.899520]|6/6|
|new/cgo|system_cpu_ns_per_op|0.819214|[0.768048,0.879725]|6/6|
|new/cgo|drained_cpu_ns_per_op|0.877778|[0.852195,0.908376]|6/6|
|new/cgo|shutdown_process_cpu_ns_per_op|0.918950|[0.892295,0.947746]|6/6|
|new/cgo|shutdown_user_cpu_ns_per_op|1.003044|[0.970990,1.025554]|2/6|
|new/cgo|shutdown_system_cpu_ns_per_op|0.817981|[0.773267,0.876202]|6/6|
|old/cgo|mean_ns|0.966606|[0.921620,1.037329]|5/6|
|old/cgo|p99_ns|0.976044|[0.853556,1.138437]|3/6|
|old/cgo|process_cpu_ns_per_op|1.072381|[1.031442,1.125123]|1/6|
|old/cgo|user_cpu_ns_per_op|1.126641|[1.102359,1.156977]|0/6|
|old/cgo|system_cpu_ns_per_op|1.011625|[0.939862,1.095499]|3/6|
|old/cgo|drained_cpu_ns_per_op|1.093412|[1.052781,1.146751]|0/6|
|old/cgo|shutdown_process_cpu_ns_per_op|1.126532|[1.086147,1.174791]|0/6|
|old/cgo|shutdown_user_cpu_ns_per_op|1.223227|[1.188783,1.258669]|0/6|
|old/cgo|shutdown_system_cpu_ns_per_op|1.009769|[0.942706,1.093207]|2/6|

V7 scopes: window=(C-B)/N; drained=(D-B)/N includes forcedGC and pending setup/warmup cleanup after nativeFIFO barrier; shutdown=(E-A)/N includes client startup,fixture,warmup,validation,CSV and forcedGC through DBclose/nativeStopNetworkjoin. A excludes earlier arm-specific executable/libraryhashing. AbsoluteE/N is raw/absolute-only, never paired. FinalJSON/process exit excluded. Drains2or moreforcedGCs may differbyarm. Notisolatedsteady-stateCPU. SixsameVM exploratoryblocks do not establish independence/nominalCIcoverage; no multiplicitycorrection or equivalence/noninferiority/globalcapacityclaim. Combinedpatchparentcorrectness/reviews/CI are not proved by benchmarksuccess.
