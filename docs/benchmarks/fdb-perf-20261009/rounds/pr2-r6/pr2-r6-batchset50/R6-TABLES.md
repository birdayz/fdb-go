# R6 PR2 commit-capture borrowing — batchset50

Old=9377c0863aef7ecdee250cdc323e40ebf69d5e46;new=old+patchb7736a4a40a159b1c1e75904c28292ee7be0e0f763e89a5a8d9c5d756c876a18 (PR1 NOT stacked);CGo=immutable native7.3.77. FrozenV6,2500 measured+200warmup/run,workers1,all6three-armorders. Counts wereprespecified,not adapted afterresults.

Absolute values are medians of6run statistics;ratios are geometric means of6within-block ratios,NOT ratios of medians. Timecolumnsµs/transaction;componentmedians neednotsum. GoMemStats/GC excludeC++heap,so cross-clientallocation comparisonsareinvalid.

|Arm|Mean|p99|CPU|User CPU|System CPU|Go B/tx|Go allocs/tx|Go GC total|Go GC/tx aggregate|
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|
|cgo|2078.282|3816.013|848.520|464.489|376.634|1616.096|59.001|0|0.000000|
|old|1919.798|3413.086|881.109|491.529|385.555|80436.670|299.579|48|0.003200|
|new|1922.554|3421.356|875.186|472.997|394.967|61865.930|298.482|37|0.002467|

|Ratio|Mean|p99|CPU|User CPU|System CPU|CPU pairs <1|
|---|---:|---:|---:|---:|---:|---:|
|new/old|1.003520|1.000807|1.008998|0.990829|1.032069|3/6|
|new/cgo|0.952870|0.936712|1.031448|1.020136|1.046143|1/6|
|old/cgo|0.949528|0.935957|1.022250|1.029578|1.013636|1/6|

Go new/old paired allocation ratios: go_alloc_bytes_per_op=0.769153 (below1 in6/6), go_mallocs_per_op=0.996377 (below1 in6/6).

## Per-block paired ratios

|Block|Ratio|Mean|p99|CPU|User CPU|System CPU|
|---|---|---:|---:|---:|---:|---:|
|0|new/old|0.903890|0.924588|1.001398|1.001364|1.001441|
|1|new/old|1.028954|0.968932|0.986279|0.956162|1.026635|
|2|new/old|0.867156|0.744106|0.984060|0.972751|0.997843|
|3|new/old|1.135087|1.124944|1.002535|0.944313|1.075960|
|4|new/old|0.986123|1.102220|0.990897|0.952722|1.041782|
|5|new/old|1.131324|1.215702|1.092915|1.129234|1.050937|
|0|new/cgo|0.928058|0.944650|1.062458|1.082509|1.038156|
|1|new/cgo|0.950807|0.988499|0.990635|1.029325|0.946245|
|2|new/cgo|0.907093|0.834699|1.033861|0.989312|1.092298|
|3|new/cgo|1.045556|0.977086|1.027170|0.958261|1.115996|
|4|new/cgo|0.903823|0.899309|1.014632|1.044275|0.980697|
|5|new/cgo|0.989582|0.986320|1.061807|1.021718|1.116199|
|0|old/cgo|1.026738|1.021699|1.060974|1.081034|1.036662|
|1|old/cgo|0.924052|1.020194|1.004417|1.076517|0.921696|
|2|old/cgo|1.046056|1.121747|1.050607|1.017025|1.094659|
|3|old/cgo|0.921124|0.868564|1.024573|1.014770|1.037209|
|4|old/cgo|0.916542|0.815907|1.023953|1.096096|0.941365|
|5|old/cgo|0.874711|0.811318|0.971537|0.904789|1.062099|

## Per-run GC and allocation

|Run|Go B/tx|Go allocs/tx|Go GC cycles|Go GC/tx|
|---|---:|---:|---:|---:|
|b000-batchset50-p0-old|80489.162|299.565|8|0.003200|
|b000-batchset50-p1-cgo|1616.230|59.002|0|0.000000|
|b000-batchset50-p2-new|61822.522|298.526|6|0.002400|
|b001-batchset50-p0-new|61783.210|298.449|6|0.002400|
|b001-batchset50-p1-cgo|1616.096|59.001|0|0.000000|
|b001-batchset50-p2-old|80366.288|299.482|8|0.003200|
|b002-batchset50-p0-cgo|1616.000|59.000|0|0.000000|
|b002-batchset50-p1-new|61955.978|298.493|6|0.002400|
|b002-batchset50-p2-old|80445.850|299.604|8|0.003200|
|b003-batchset50-p0-old|80427.491|299.554|8|0.003200|
|b003-batchset50-p1-new|61895.702|298.462|6|0.002400|
|b003-batchset50-p2-cgo|1616.013|59.000|0|0.000000|
|b004-batchset50-p0-cgo|1616.096|59.001|0|0.000000|
|b004-batchset50-p1-old|80405.325|299.641|8|0.003200|
|b004-batchset50-p2-new|61836.157|298.472|7|0.002800|
|b005-batchset50-p0-new|61896.886|298.524|6|0.002400|
|b005-batchset50-p1-old|80461.821|299.592|8|0.003200|
|b005-batchset50-p2-cgo|1616.230|59.002|0|0.000000|

Sixblock exploratory singleVM/workload comparison,not capacity/globalnative/noninferiority proof. Nativeinternalcounters unobserved. No profilers/buildsoverlappedtiming. Benchmarkraw/persistence/counterchecks and remote standaloneharness tests are NOT parentcorrectness/Bazel/finalreviewapproval.
