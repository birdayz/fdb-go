---
title: Performance
toc: false
---

Benchmarks run nightly in CI against real FoundationDB (testcontainers), for both the pure-Go
client and the Record Layer. Reports are published openly — including regressions.

{{< callout type="info" >}}
  This page will embed the latest nightly report. Wiring it to the report pipeline is in progress
  ([reports bucket]( https://fdb-record-layer-go-reports.fsn1.your-objectstorage.com/reports/master/latest.html )).
{{< /callout >}}

## Client: pure-Go vs libfdb_c

Same process, same FDB cluster, same keys — byte-identical results verified by `TestBenchmarkSanity`.
The read-path speedup comes from the Go runtime skipping the C client's network-thread hop and
multi-version-client shim; writes go through the same commit path and run at parity. The client
speaks the FoundationDB **7.3** wire protocol (validated against 7.3.77).

| Benchmark | pure-Go | libfdb_c | Speedup |
|---|---:|---:|:--|
| Get (100 B) | 60 µs | 218 µs | **3.6×** |
| Get (1 KB) | 61 µs | 209 µs | **3.4×** |
| Get (10 KB) | 69 µs | 217 µs | **3.1×** |
| GetRange (100 keys) | 92 µs | 363 µs | **3.9×** |
| Sustained read throughput | 430 MB/s | 191 MB/s | **2.3×** |
| Set + Commit | 1 008 µs | 1 005 µs | 1.0× |

### Under simulated network latency (`tc netem`) — the realistic signal

Localhost microbenchmarks are syscall/IPC-bound. The numbers that matter for a real deployment are
under network latency:

| RTT | pure-Go | libfdb_c | Speedup |
|---|---:|---:|:--|
| 2 ms | 1 080 µs | 2 726 µs | **2.5×** |
| 10 ms | 5 254 µs | 12 635 µs | **2.4×** |
| 1 000 ms | 1 005 ms | 1 006 ms | 1.0× |

The read advantage holds under real latency (2.4× at 10 ms RTT) and converges to parity at extreme
RTT, where both clients are network-bound. Writes are at parity throughout. Full analysis in
`PERFORMANCE.md`.
