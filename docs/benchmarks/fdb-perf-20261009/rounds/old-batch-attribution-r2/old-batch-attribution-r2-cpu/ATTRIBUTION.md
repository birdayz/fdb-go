# Old Go R2 attribution only

No performance comparison. Exact measured-phase MemStats below; heap pprof is sampled lifetime (fixture/warmup included), not these exact deltas.

|Workload|Profile|Transactions|B/tx|Allocs/tx|GC cycles|GC cycles/tx|
|---|---|---:|---:|---:|---:|---:|
|batchset50|cpu|3000|80354.744|299.578|8|0.002667|
|batchset50x1000|cpu|2000|271027.556|300.502|3|0.001500|
