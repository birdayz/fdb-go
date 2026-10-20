# R6 PR2 commit-capture borrowing — batchset50x1000

Old=9377c0863aef7ecdee250cdc323e40ebf69d5e46;new=old+patchb7736a4a40a159b1c1e75904c28292ee7be0e0f763e89a5a8d9c5d756c876a18 (PR1 NOT stacked);CGo=immutable native7.3.77. FrozenV6,2000 measured+200warmup/run,workers1,all6three-armorders. Counts wereprespecified,not adapted afterresults.

Absolute values are medians of6run statistics;ratios are geometric means of6within-block ratios,NOT ratios of medians. Timecolumnsµs/transaction;componentmedians neednotsum. GoMemStats/GC excludeC++heap,so cross-clientallocation comparisonsareinvalid.

|Arm|Mean|p99|CPU|User CPU|System CPU|Go B/tx|Go allocs/tx|Go GC total|Go GC/tx aggregate|
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|
|cgo|2457.386|4362.234|1079.472|496.477|570.689|1616.172|59.002|1|0.000083|
|old|2330.614|4331.954|1061.833|517.912|557.936|271083.508|300.415|18|0.001500|
|new|2259.096|4343.585|997.239|470.115|537.131|205354.912|299.332|12|0.001000|

|Ratio|Mean|p99|CPU|User CPU|System CPU|CPU pairs <1|
|---|---:|---:|---:|---:|---:|---:|
|new/old|0.973700|1.045277|0.920064|0.883443|0.953617|6/6|
|new/cgo|0.919068|1.004037|0.951449|0.946410|0.955551|4/6|
|old/cgo|0.943893|0.960546|1.034111|1.071275|1.002029|2/6|

Go new/old paired allocation ratios: go_alloc_bytes_per_op=0.757578 (below1 in6/6), go_mallocs_per_op=0.996414 (below1 in6/6).

## Per-block paired ratios

|Block|Ratio|Mean|p99|CPU|User CPU|System CPU|
|---|---|---:|---:|---:|---:|---:|
|0|new/old|0.964157|1.141943|0.955246|0.893843|1.015350|
|1|new/old|1.100217|1.048838|0.932116|0.952412|0.914110|
|2|new/old|1.135327|1.360382|0.920899|0.862661|0.975697|
|3|new/old|0.853512|0.939183|0.882864|0.886675|0.879504|
|4|new/old|0.862785|0.941085|0.919721|0.849300|0.985745|
|5|new/old|0.960928|0.905723|0.911090|0.859644|0.957880|
|0|new/cgo|0.895042|1.115094|1.041843|1.046046|1.038248|
|1|new/cgo|1.030328|1.029546|1.023914|1.050472|1.000531|
|2|new/cgo|0.926920|0.975910|0.920073|0.891631|0.945155|
|3|new/cgo|0.880469|1.077320|0.833244|0.853244|0.816234|
|4|new/cgo|0.860325|0.964062|0.956535|0.955797|0.957131|
|5|new/cgo|0.930785|0.880402|0.948312|0.899322|0.992439|
|0|old/cgo|0.928316|0.976488|1.090654|1.170279|1.022551|
|1|old/cgo|0.936476|0.981607|1.098483|1.102960|1.094541|
|2|old/cgo|0.816435|0.717380|0.999104|1.033583|0.968697|
|3|old/cgo|1.031583|1.147081|0.943796|0.962297|0.928062|
|4|old/cgo|0.997148|1.024416|1.040027|1.125394|0.970972|
|5|old/cgo|0.968631|0.972044|1.040854|1.046156|1.036079|

## Per-run GC and allocation

|Run|Go B/tx|Go allocs/tx|Go GC cycles|Go GC/tx|
|---|---:|---:|---:|---:|
|b000-batchset50x1000-p0-old|270970.924|300.456|3|0.001500|
|b000-batchset50x1000-p1-cgo|1616.008|59.000|0|0.000000|
|b000-batchset50x1000-p2-new|205166.400|299.287|2|0.001000|
|b001-batchset50x1000-p0-new|205263.400|299.349|2|0.001000|
|b001-batchset50x1000-p1-cgo|1616.736|59.007|0|0.000000|
|b001-batchset50x1000-p2-old|270868.480|300.422|3|0.001500|
|b002-batchset50x1000-p0-cgo|1616.008|59.000|0|0.000000|
|b002-batchset50x1000-p1-new|205328.124|299.370|2|0.001000|
|b002-batchset50x1000-p2-old|271307.624|300.408|3|0.001500|
|b003-batchset50x1000-p0-new|205381.700|299.306|2|0.001000|
|b003-batchset50x1000-p1-old|271061.704|300.466|3|0.001500|
|b003-batchset50x1000-p2-cgo|1618.916|59.005|1|0.000500|
|b004-batchset50x1000-p0-old|271105.312|300.401|3|0.001500|
|b004-batchset50x1000-p1-new|205500.412|299.315|2|0.001000|
|b004-batchset50x1000-p2-cgo|1616.288|59.002|0|0.000000|
|b005-batchset50x1000-p0-cgo|1616.056|59.001|0|0.000000|
|b005-batchset50x1000-p1-old|271240.040|300.345|3|0.001500|
|b005-batchset50x1000-p2-new|205600.980|299.408|2|0.001000|

Sixblock exploratory singleVM/workload comparison,not capacity/globalnative/noninferiority proof. Nativeinternalcounters unobserved. No profilers/buildsoverlappedtiming. Benchmarkraw/persistence/counterchecks and remote standaloneharness tests are NOT parentcorrectness/Bazel/finalreviewapproval.
