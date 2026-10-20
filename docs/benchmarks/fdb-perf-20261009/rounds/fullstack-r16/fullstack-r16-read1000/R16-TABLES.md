# R16 cumulative fullstack — read1000

Cumulative fullstack on exact7922a2e5a8e3a2d6cf17dfee8b8134181c88b298, patchd0a6ae57ac1143aa1247ce2461f84f74264825f901a53cb7ab943a0efe83be85 (PRs812,818,816,834,824,827,841,828,832 plus reply-error allocation). OLD=base; NEW=base+patch. OLD retains missing CAUSAL_READ_RISKY on write-only commit GRV; NEW fixes it, part of the declared intervention. CGo is unchanged V7native7.3.77 built from8ab; dependency/build pins identical to7922 (empty root-module equivalence diff archived). FrozenreviewedV7: window(C-B),drained(D-B),shutdown(E-A) perN; absoluteE unpairedonly. No profiler/build overlaps.

5000measured+200warmup,workers1,18runs/sixthree-armorders. Absolute times are medians of6run statistics in microseconds per transaction. AbsoluteE CPU is unpaired only; no cross-arm client-cost interpretation. GoMemStats excludes nativeC++ heap.

|Arm|Mean|p99|Window CPU|User|System|Drained CPU|Shutdown(E-A) CPU|Shutdown(E-A) User|Shutdown(E-A) System|AbsoluteE CPU(unpaired)|GoB/tx|Goallocs/tx|GoGC window/drained/shutdown|ForcedGC rounds measure/shutdown|
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---|---|
|old|442.053|656.645|468.134|185.223|281.642|477.029|508.971|214.285|294.699|513.920|7975.011|63.196|36/48/66|[2, 2, 2, 2, 2, 2] / [2, 2, 2, 2, 2, 2]|
|new|365.973|561.941|309.232|111.567|195.652|318.581|345.052|136.963|206.580|350.070|7288.027|53.067|36/48/66|[2, 2, 2, 2, 2, 2] / [2, 2, 2, 2, 2, 2]|
|cgo|542.030|870.538|364.394|159.865|204.529|370.691|393.121|175.100|217.934|404.148|1542.198|13.012|18/36/54|[3, 3, 3, 3, 3, 3] / [3, 3, 3, 3, 3, 3]|

Paired geometric means of6within-block ratios, NOT ratios of displayedmedians; percentile95% whole-block bootstrap CI,10000resamples,seed796707,common resampledindices acrossmetrics/comparisons.

|Ratio|Metric|GM|95%CI|Pairs<1|
|---|---|---:|---|---:|
|new/old|mean_ns|0.819941|[0.779209,0.854833]|6/6|
|new/old|p99_ns|0.826551|[0.774040,0.879764]|6/6|
|new/old|process_cpu_ns_per_op|0.664208|[0.651322,0.682796]|6/6|
|new/old|user_cpu_ns_per_op|0.611348|[0.579842,0.651063]|6/6|
|new/old|system_cpu_ns_per_op|0.698399|[0.684838,0.714464]|6/6|
|new/old|drained_cpu_ns_per_op|0.670593|[0.657888,0.688849]|6/6|
|new/old|shutdown_process_cpu_ns_per_op|0.680015|[0.668244,0.697189]|6/6|
|new/old|shutdown_user_cpu_ns_per_op|0.649662|[0.620073,0.685946]|6/6|
|new/old|shutdown_system_cpu_ns_per_op|0.701513|[0.686779,0.715039]|6/6|
|new/old|go_alloc_bytes_per_op|0.914097|[0.913400,0.914701]|6/6|
|new/old|go_mallocs_per_op|0.839727|[0.839588,0.839863]|6/6|
|new/cgo|mean_ns|0.686314|[0.640268,0.737483]|6/6|
|new/cgo|p99_ns|0.642212|[0.623058,0.663485]|6/6|
|new/cgo|process_cpu_ns_per_op|0.907813|[0.809929,1.018980]|4/6|
|new/cgo|user_cpu_ns_per_op|0.775817|[0.684102,0.898573]|5/6|
|new/cgo|system_cpu_ns_per_op|1.006485|[0.899147,1.138281]|3/6|
|new/cgo|drained_cpu_ns_per_op|0.919691|[0.820230,1.031952]|4/6|
|new/cgo|shutdown_process_cpu_ns_per_op|0.935043|[0.837884,1.043991]|4/6|
|new/cgo|shutdown_user_cpu_ns_per_op|0.863428|[0.769651,0.988878]|5/6|
|new/cgo|shutdown_system_cpu_ns_per_op|0.990110|[0.888667,1.113121]|4/6|
|old/cgo|mean_ns|0.837029|[0.777940,0.914202]|5/6|
|old/cgo|p99_ns|0.776978|[0.721714,0.830020]|6/6|
|old/cgo|process_cpu_ns_per_op|1.366760|[1.214299,1.542586]|0/6|
|old/cgo|user_cpu_ns_per_op|1.269026|[1.108953,1.480103]|0/6|
|old/cgo|system_cpu_ns_per_op|1.441131|[1.293136,1.629089]|0/6|
|old/cgo|drained_cpu_ns_per_op|1.371460|[1.220403,1.545632]|0/6|
|old/cgo|shutdown_process_cpu_ns_per_op|1.375032|[1.230682,1.538919]|0/6|
|old/cgo|shutdown_user_cpu_ns_per_op|1.329041|[1.170198,1.541562]|0/6|
|old/cgo|shutdown_system_cpu_ns_per_op|1.411391|[1.279758,1.576296]|0/6|

V7 scopes: window=(C-B)/N; drained=(D-B)/N includes forcedGC and pending setup/warmup cleanup after nativeFIFO barrier; shutdown=(E-A)/N includes client startup,fixture,warmup,validation,CSV and forcedGC through DBclose/nativeStopNetworkjoin. A excludes earlier arm-specific executable/libraryhashing. AbsoluteE/N is raw/absolute-only, never paired. FinalJSON/process exit excluded. Drains2or moreforcedGCs may differbyarm. Notisolatedsteady-stateCPU. SixsameVM exploratoryblocks do not establish independence/nominalCIcoverage; no multiplicitycorrection or equivalence/noninferiority/globalcapacityclaim. Combinedpatchparentcorrectness/reviews/CI are not proved by benchmarksuccess.
