# R1 read1000 A/A variability diagnostic

5000 measured transactions, 200 warmup, one worker, no profiling; existing immutable three binaries, old-aa reuses exact old binary. Six partial-counterbalanced blocks, not all24 orders: three reversal pairs, pairwise precedence3/3 and per-arm positions1or2 appearances. Fixed-seed shuffled schedule.

Absolute values are medians of six run statistics; ratios are geometric means of six within-block ratios, NOT ratios of medians. Latency and CPU columns are µs/transaction. User+system CPU are measured-phase getrusage deltas. Context-switch counts are **whole process**, including setup/fixture/warmup/validation, NOT measured-phase counts. Their per-run totals must not be equated to scheduler work during measurement.

|Arm|Mean|p99|CPU|User CPU|System CPU|CPU/wall|Voluntary ctx/run|Involuntary ctx/run|
|---|---:|---:|---:|---:|---:|---:|---:|---:|
|cgo|534.313|826.345|367.683|158.028|208.284|0.6831|39540.5|6.0|
|old|435.727|636.265|450.667|179.212|276.545|1.0443|63679.0|12.0|
|old-aa|440.908|644.066|470.374|183.098|288.870|1.0607|63624.0|6.5|
|new|433.333|708.560|459.734|181.759|276.248|1.0533|62444.5|7.0|

|Ratio|Mean|p99|CPU|User CPU|System CPU|CPU pairs <1|
|---|---:|---:|---:|---:|---:|---:|
|new/old|1.007650|1.112447|1.031251|1.029539|1.032802|3/6|
|old-aa/old|1.045012|1.030772|1.051955|1.023081|1.072011|1/6|
|old/cgo|0.802911|0.763292|1.273524|1.176277|1.347895|0/6|
|new/cgo|0.809053|0.849121|1.313323|1.211024|1.392109|0/6|
|old-aa/cgo|0.839051|0.786779|1.339689|1.203426|1.444958|0/6|

## Per-block ratios

|Block|Ratio|Mean|p99|CPU|User CPU|System CPU|
|---|---|---:|---:|---:|---:|---:|
|0|new/old|1.162342|1.077773|1.175860|1.192819|1.163720|
|1|new/old|0.998110|1.149538|0.973565|0.923957|1.007569|
|2|new/old|0.890501|0.949953|1.016019|1.094769|0.968510|
|3|new/old|1.058721|1.135164|1.061122|1.004841|1.101614|
|4|new/old|0.998480|1.171600|0.999951|1.016855|0.989525|
|5|new/old|0.958497|1.210836|0.974586|0.965945|0.980437|
|0|old-aa/old|1.180612|1.144381|1.110585|1.050877|1.153326|
|1|old-aa/old|0.987404|1.036890|0.971428|0.959845|0.979368|
|2|old-aa/old|1.028454|0.994443|1.079361|1.089399|1.073305|
|3|old-aa/old|1.059880|0.995800|1.099609|1.006332|1.166718|
|4|old-aa/old|0.994713|1.003457|1.009547|1.037693|0.992187|
|5|old-aa/old|1.030353|1.017234|1.048307|0.999330|1.081470|
|0|old/cgo|0.650709|0.700027|1.030987|0.970077|1.079507|
|1|old/cgo|0.854074|0.722412|1.310590|1.297438|1.319760|
|2|old/cgo|0.785595|0.756389|1.165078|0.980517|1.314327|
|3|old/cgo|0.764709|0.911577|1.165014|1.088068|1.227466|
|4|old/cgo|0.916692|0.713469|1.624308|1.405390|1.796953|
|5|old/cgo|0.875394|0.794932|1.432081|1.403645|1.451999|
|0|new/cgo|0.756347|0.754470|1.212296|1.157126|1.256244|
|1|new/cgo|0.852460|0.830440|1.275945|1.198777|1.329749|
|2|new/cgo|0.699573|0.718534|1.183741|1.073439|1.272939|
|3|new/cgo|0.809613|1.034790|1.236222|1.093336|1.352194|
|4|new/cgo|0.915298|0.835900|1.624229|1.429078|1.778130|
|5|new/cgo|0.839062|0.962532|1.395686|1.355844|1.423594|
|0|old-aa/cgo|0.768235|0.801097|1.144999|1.019432|1.245024|
|1|old-aa/cgo|0.843316|0.749062|1.273144|1.245339|1.292530|
|2|old-aa/cgo|0.807948|0.752185|1.257540|1.068174|1.410675|
|3|old-aa/cgo|0.810500|0.907749|1.281060|1.094958|1.432106|
|4|old-aa/cgo|0.911845|0.715935|1.639816|1.458363|1.782914|
|5|old-aa/cgo|0.901965|0.808632|1.501260|1.402705|1.570294|

## Per-run CPU and whole-process switches

|Run|Mean µs|CPU µs|User µs|System µs|CPU/wall|Voluntary ctx|Involuntary ctx|
|---|---:|---:|---:|---:|---:|---:|---:|
|b000-read1000-p0-old|364.975|397.903|166.003|231.900|1.0885|60744|5|
|b000-read1000-p1-old-aa|430.893|441.905|174.449|267.456|1.0248|64130|7|
|b000-read1000-p2-new|424.225|467.878|198.012|269.867|1.1020|62329|10|
|b000-read1000-p3-cgo|560.887|385.944|171.124|214.820|0.6875|39075|7|
|b001-read1000-p0-old-aa|439.218|458.459|184.224|274.235|1.0422|64426|17|
|b001-read1000-p1-cgo|520.823|360.100|147.931|212.169|0.6908|40006|8|
|b001-read1000-p2-new|443.980|459.468|177.336|282.132|1.0332|64116|4|
|b001-read1000-p3-old|444.821|471.944|191.931|280.013|1.0601|64809|12|
|b002-read1000-p0-cgo|547.804|378.782|169.356|209.426|0.6908|38976|3|
|b002-read1000-p1-new|383.229|448.379|181.793|266.586|1.1680|60033|7|
|b002-read1000-p2-old-aa|442.597|476.333|180.902|295.431|1.0745|62767|5|
|b002-read1000-p3-old|430.352|441.310|166.056|275.254|1.0247|62455|12|
|b003-read1000-p0-old|422.512|437.191|182.931|254.260|1.0333|63605|15|
|b003-read1000-p1-new|447.323|463.913|183.816|280.096|1.0363|63781|6|
|b003-read1000-p2-cgo|552.514|375.266|168.124|207.142|0.6786|38736|6|
|b003-read1000-p3-old-aa|447.813|480.739|184.089|296.650|1.0719|63440|6|
|b004-read1000-p0-cgo|481.190|283.212|124.871|158.341|0.5880|41650|4|
|b004-read1000-p1-old|441.103|460.023|175.492|284.531|1.0414|63753|9|
|b004-read1000-p2-old-aa|438.771|464.415|182.107|282.308|1.0569|63007|9|
|b004-read1000-p3-new|440.433|460.001|178.450|281.550|1.0429|61620|7|
|b005-read1000-p0-new|426.233|454.125|181.725|272.400|1.0637|62560|9|
|b005-read1000-p1-old-aa|458.187|488.476|188.005|300.471|1.0645|63808|4|
|b005-read1000-p2-old|444.690|465.967|188.131|277.836|1.0471|64118|25|
|b005-read1000-p3-cgo|507.988|325.378|134.031|191.347|0.6400|40354|6|

## Scope

This adaptive six-block A/A experiment characterizes observed variability at5000 ops/run; it does not prove zero patch cost, noninferiority, or explain away the original1000-op new/old CPU1.110292 result. The patch changes an owner24→32B and fixture slab/pool state even though each measured read fits a512B slab and never resets it. More operations and warmup change GC/lifetime conditions. No experiment was repeated to obtain a favorable result. Old/new/CGo native comparisons remain scoped to this VM/workload; native internal counters are unobserved, and GoMemStats excludes native heap. Raw/persistence/counter checks do not constitute main-module correctness approval.
