# V7 fresh lifecycle accounting

Baseline8ab5c5a4d6ee4858a4eacd6ac58e2a7be85dbead and authoritative R11 PR3+PR4+PR5v2 stack each compared with native7.3.77. Four independent campaigns, six paired two-arm blocks each, three of each order. Batch3000/200, read5000/200; GOMAXPROCS8; fresh prefixes. No V6 observations pooled here.

CPU scope: window=(C-B)/N; drained=(D-B)/N after forced-GC finalizer fixpoint/native FIFO barrier; shutdown=(E-A)/N after final drain, DB close and native StopNetwork join. A is after identity hashing but before backend open. Drained includes pending setup/warmup cleanup and forced GC. Shutdown includes client startup,fixture,warmup,persistedvalidation,CSV and forcedGC; excludes prior identity work,finalJSON andexit. AbsoluteE/N includes arm-specific identity work, retained unpaired and not cross-arm client-cost evidence. Orphan native timeout timers may expire after D and are discarded atStopNetwork; D is not terminal shutdown. None is isolated steady-state transaction CPU.

## v7-baseline-batchset50

Absolute medians of six run statistics, microseconds per measured transaction. GC columns sum counts across the six processes. These medians are not the paired ratios.

|Arm|Mean|p99|Window CPU|Drained CPU|Shutdown CPU(E-A)|AbsoluteE CPU(unpaired)|Window GC|Drained GC(deltaB)|Shutdown GC(absoluteE)|ForcedGC rounds measurement/shutdown|
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---|
|go|2016.054|3716.629|882.700|926.461|1073.033|1081.642|48|60|114|[2, 2, 2, 2, 2, 2] / [2, 2, 2, 2, 2, 2]|
|cgo|2188.606|3978.492|833.596|855.203|959.430|978.879|1|19|72|[3, 3, 3, 3, 3, 3] / [3, 3, 3, 3, 3, 3]|

|Go/CGo metric|Paired GM|Nominal95%block CI|Pairs<1|
|---|---:|---|---:|
|mean_ns|0.924842|[0.871268,0.982326]|5/6|
|p99_ns|0.962790|[0.862397,1.066569]|3/6|
|process_cpu_ns_per_op|1.052546|[1.021542,1.085915]|1/6|
|drained_cpu_ns_per_op|1.076805|[1.045195,1.110781]|0/6|
|shutdown_process_cpu_ns_per_op|1.119446|[1.088659,1.155948]|0/6|

## v7-baseline-read1000

Absolute medians of six run statistics, microseconds per measured transaction. GC columns sum counts across the six processes. These medians are not the paired ratios.

|Arm|Mean|p99|Window CPU|Drained CPU|Shutdown CPU(E-A)|AbsoluteE CPU(unpaired)|Window GC|Drained GC(deltaB)|Shutdown GC(absoluteE)|ForcedGC rounds measurement/shutdown|
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---|
|go|436.629|637.676|460.554|470.606|501.196|506.150|36|48|66|[2, 2, 2, 2, 2, 2] / [2, 2, 2, 2, 2, 2]|
|cgo|562.957|909.554|367.542|373.103|394.914|406.093|18|36|54|[3, 3, 3, 3, 3, 3] / [3, 3, 3, 3, 3, 3]|

|Go/CGo metric|Paired GM|Nominal95%block CI|Pairs<1|
|---|---:|---|---:|
|mean_ns|0.796266|[0.749674,0.860911]|6/6|
|p99_ns|0.704760|[0.673278,0.735865]|6/6|
|process_cpu_ns_per_op|1.282384|[1.217883,1.366370]|0/6|
|drained_cpu_ns_per_op|1.288648|[1.225303,1.369587]|0/6|
|shutdown_process_cpu_ns_per_op|1.295009|[1.238599,1.366772]|0/6|

## v7-stack-batchset50

Absolute medians of six run statistics, microseconds per measured transaction. GC columns sum counts across the six processes. These medians are not the paired ratios.

|Arm|Mean|p99|Window CPU|Drained CPU|Shutdown CPU(E-A)|AbsoluteE CPU(unpaired)|Window GC|Drained GC(deltaB)|Shutdown GC(absoluteE)|ForcedGC rounds measurement/shutdown|
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---|
|go|2025.760|3846.389|762.922|806.497|945.555|954.414|48|60|114|[2, 2, 2, 2, 2, 2] / [2, 2, 2, 2, 2, 2]|
|cgo|2206.267|4374.774|848.724|867.490|975.023|993.872|1|19|73|[3, 3, 3, 3, 3, 3] / [3, 3, 3, 3, 3, 3]|

|Go/CGo metric|Paired GM|Nominal95%block CI|Pairs<1|
|---|---:|---|---:|
|mean_ns|0.911296|[0.854909,0.955222]|6/6|
|p99_ns|0.876984|[0.770233,0.985403]|4/6|
|process_cpu_ns_per_op|0.895104|[0.881803,0.909105]|6/6|
|drained_cpu_ns_per_op|0.922994|[0.908869,0.936429]|6/6|
|shutdown_process_cpu_ns_per_op|0.964230|[0.950698,0.975822]|6/6|

## v7-stack-read1000

Absolute medians of six run statistics, microseconds per measured transaction. GC columns sum counts across the six processes. These medians are not the paired ratios.

|Arm|Mean|p99|Window CPU|Drained CPU|Shutdown CPU(E-A)|AbsoluteE CPU(unpaired)|Window GC|Drained GC(deltaB)|Shutdown GC(absoluteE)|ForcedGC rounds measurement/shutdown|
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---|
|go|386.563|612.026|365.173|374.887|403.521|408.520|36|48|66|[2, 2, 2, 2, 2, 2] / [2, 2, 2, 2, 2, 2]|
|cgo|544.838|909.058|347.074|353.067|374.489|385.801|18|36|54|[3, 3, 3, 3, 3, 3] / [3, 3, 3, 3, 3, 3]|

|Go/CGo metric|Paired GM|Nominal95%block CI|Pairs<1|
|---|---:|---|---:|
|mean_ns|0.719360|[0.672183,0.792341]|6/6|
|p99_ns|0.673407|[0.629053,0.715285]|6/6|
|process_cpu_ns_per_op|1.049768|[0.969663,1.169499]|4/6|
|drained_cpu_ns_per_op|1.059092|[0.980502,1.176652]|2/6|
|shutdown_process_cpu_ns_per_op|1.073174|[1.000030,1.192793]|1/6|

10,000 paired-block resamples, seed642001 per campaign, percentile95% intervals; not operation-resampling. Six same-VM exploratory blocks do not establish independence or nominal coverage; no multiplicity correction, equivalence/noninferiority/globalcapacity claim. GoGC includes forced accounting work, whose round count can differ by arm. GoMemStats excludes nativeC++ heap. Baseline and stack both retain the pre-existing missing CAUSAL_READ_RISKY flag on write-only commit GRVs; PR6 is NOT stacked.
