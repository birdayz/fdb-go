# Read CPU accounting from existing raw summaries

No new workloads. Measured user/system deltas come from getrusage snapshots; CPU = user+system. No context-switch count was captured in these snapshots. Profiled campaigns stay separate. All values are microseconds/transaction.

## hillclimb-arena-003-read1000
|Arm|User median µs/op|System median µs/op|CPU median µs/op|User total s|System total s|
|---|---:|---:|---:|---:|---:|
|cgo|173.494|235.270|405.038|0.967898|1.332085|
|old|155.735|265.873|444.842|0.978990|1.578686|
|new|196.495|276.553|474.844|1.155056|1.672478|

|Ratio|User paired GM|System paired GM|CPU paired GM|
|---|---:|---:|---:|
|new/old|1.183425|1.064719|1.110292|
|new/cgo|1.210280|1.274741|1.248844|
|old/cgo|1.022693|1.197256|1.124789|

## hillclimb-arena-003-read-cpu
|Arm|User median µs/op|System median µs/op|CPU median µs/op|User total s|System total s|
|---|---:|---:|---:|---:|---:|
|cgo|164.679|220.456|388.163|2.825300|3.840138|
|old|176.371|277.197|449.539|3.168610|4.884924|
|new|185.379|283.225|474.006|3.339817|4.936466|

|Ratio|User paired GM|System paired GM|CPU paired GM|
|---|---:|---:|---:|
|new/old|1.056362|1.008598|1.026848|
|new/cgo|1.191719|1.290366|1.248272|
|old/cgo|1.128135|1.279366|1.215635|

## hillclimb-arena-003-read-heap
|Arm|User median µs/op|System median µs/op|CPU median µs/op|User total s|System total s|
|---|---:|---:|---:|---:|---:|
|cgo|153.232|214.654|374.381|2.632432|3.754687|
|old|174.036|273.169|450.724|3.112085|4.835500|
|new|192.982|275.094|469.588|3.442513|4.978151|

|Ratio|User paired GM|System paired GM|CPU paired GM|
|---|---:|---:|---:|
|new/old|1.109322|1.032442|1.062074|
|new/cgo|1.342866|1.341791|1.340654|
|old/cgo|1.210528|1.299629|1.262298|
