# Old Go R2 attribution only

No performance comparison. Exact measured-phase MemStats below; heap pprof is sampled lifetime (fixture/warmup included), not these exact deltas.

|Workload|Profile|Transactions|B/tx|Allocs/tx|GC cycles|GC cycles/tx|
|---|---|---:|---:|---:|---:|---:|
|batchset50|heap|3000|80404.291|299.606|9|0.003000|
