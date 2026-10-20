# R15 PR9 eager lone Get — read1000

PR9 lone pipelined Get eager write, incremental over PR5v3 on exact8ab. OLD=base+patch95c1a474c2eb59f7b493b538c0d7ab179d61617e3aca4f78a39c3ee0da26c7a8; NEW=OLD+patchf81ad05e2af0c0b68fe4558a824e71aea815267435b7f6dad1b52686c2e6991c. Neither is the R11combinedstack. Both retain missing CAUSAL_READ_RISKY on write-only commit GRV. FrozenreviewedV7; window(C-B),drained(D-B),shutdown(E-A) perN; absoluteE unpairedonly. No profiler/build overlaps.

5000measured+200warmup,workers1,18runs/sixthree-armorders. Absolute times are medians of6run statistics in microseconds per transaction. AbsoluteE CPU is unpaired only; no cross-arm client-cost interpretation. GoMemStats excludes nativeC++ heap.

|Arm|Mean|p99|Window CPU|User|System|Drained CPU|Shutdown(E-A) CPU|AbsoluteE CPU(unpaired)|GoB/tx|Goallocs/tx|GoGC window/drained/shutdown|ForcedGC rounds measure/shutdown|
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---|---|
|old|438.771|670.040|438.341|165.905|266.301|448.678|480.521|485.559|7773.497|62.106|36/48/66|[2, 2, 2, 2, 2, 2] / [2, 2, 2, 2, 2, 2]|
|new|389.789|631.356|381.153|144.815|238.141|390.705|420.060|425.326|7453.198|61.084|36/48/66|[2, 2, 2, 2, 2, 2] / [2, 2, 2, 2, 2, 2]|
|cgo|539.781|857.360|365.905|155.618|211.412|371.602|391.116|402.618|1541.652|13.011|18/36/54|[3, 3, 3, 3, 3, 3] / [3, 3, 3, 3, 3, 3]|

Paired geometric means of6within-block ratios, NOT ratios of displayedmedians; percentile95% whole-block bootstrap CI,10000resamples,seed795707,common resampledindices acrossmetrics/comparisons.

|Ratio|Metric|GM|95%CI|Pairs<1|
|---|---|---:|---|---:|
|new/old|mean_ns|0.912410|[0.833705,0.991484]|5/6|
|new/old|p99_ns|0.966512|[0.898877,1.040342]|4/6|
|new/old|process_cpu_ns_per_op|0.889416|[0.866017,0.911457]|6/6|
|new/old|user_cpu_ns_per_op|0.864035|[0.845881,0.883214]|6/6|
|new/old|system_cpu_ns_per_op|0.905491|[0.870024,0.939224]|6/6|
|new/old|drained_cpu_ns_per_op|0.891243|[0.867157,0.914645]|6/6|
|new/old|shutdown_process_cpu_ns_per_op|0.892341|[0.869199,0.915092]|6/6|
|new/old|go_alloc_bytes_per_op|0.958627|[0.957730,0.959485]|6/6|
|new/old|go_mallocs_per_op|0.983555|[0.983430,0.983649]|6/6|
|new/cgo|mean_ns|0.726729|[0.643004,0.834263]|6/6|
|new/cgo|p99_ns|0.734684|[0.662488,0.808468]|6/6|
|new/cgo|process_cpu_ns_per_op|1.057480|[0.969528,1.166473]|1/6|
|new/cgo|user_cpu_ns_per_op|0.926666|[0.858534,1.018919]|5/6|
|new/cgo|system_cpu_ns_per_op|1.156590|[1.043092,1.286450]|1/6|
|new/cgo|drained_cpu_ns_per_op|1.067625|[0.979867,1.177305]|1/6|
|new/cgo|shutdown_process_cpu_ns_per_op|1.087959|[1.001085,1.188972]|1/6|
|old/cgo|mean_ns|0.796495|[0.726220,0.900055]|5/6|
|old/cgo|p99_ns|0.760139|[0.703383,0.820950]|6/6|
|old/cgo|process_cpu_ns_per_op|1.188960|[1.098391,1.307156]|0/6|
|old/cgo|user_cpu_ns_per_op|1.072486|[0.987042,1.182760]|2/6|
|old/cgo|system_cpu_ns_per_op|1.277307|[1.168053,1.409372]|0/6|
|old/cgo|drained_cpu_ns_per_op|1.197906|[1.107952,1.315834]|0/6|
|old/cgo|shutdown_process_cpu_ns_per_op|1.219219|[1.132467,1.329661]|0/6|

V7 scopes: window=(C-B)/N; drained=(D-B)/N includes forcedGC and pending setup/warmup cleanup after nativeFIFO barrier; shutdown=(E-A)/N includes client startup,fixture,warmup,validation,CSV and forcedGC through DBclose/nativeStopNetworkjoin. A excludes earlier arm-specific executable/libraryhashing. AbsoluteE/N is raw/absolute-only, never paired. FinalJSON/process exit excluded. Drains2or moreforcedGCs may differbyarm. Notisolatedsteady-stateCPU. SixsameVM exploratoryblocks do not establish independence/nominalCIcoverage; no multiplicitycorrection or equivalence/noninferiority/globalcapacityclaim. PR9parentcorrectness/reviews/CI are not proved by benchmarksuccess.
