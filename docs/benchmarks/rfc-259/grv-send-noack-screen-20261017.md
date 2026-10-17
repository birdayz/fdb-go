# GRV dispatch without a write acknowledgment: negative screen

2026-10-17. A narrow prototype replaced the GRV send's queued-writer acknowledgment
with the existing `SendFrameEagerContext` path: an idle socket still gets a direct
attempt; otherwise the queue owns an immutable copy. The RPC immediately waits for
its reply, proxy change or cancellation. Queue admission can still block. The
existing ready-reply check on an interrupted send was retained. No batching,
window, allocation-pool or GC policy changed.

C++ 7.3.77 `tryGetReply` sends through `sendUnreliable` before waiting for the reply
or disconnect (`fdbrpc/include/fdbrpc/fdbrpc.h:797–810`); GRV additionally races
proxy changes (`NativeAPI.actor.cpp:7252–7263`). This experiment changes only GRV.
A broader shared-helper change was compiled and passed the fast lane but was not
benchmarked: it would also move storage-read timeout/hedge and coordinator timeout
start times into queued-writer residence. Those policies need separate correctness
work, not an assumption that the commit proof covers them.

## Comparison

- Baseline: master `590aa97e326b612cf7768c3c14578d3470281443`, including #953.
- Candidate: that source with only the GRV dispatch change described above. The
  prototype was not committed; its description and artifact digest identify this
  screen, not a reproducible production source revision.
- Baseline / prototype / retained CGo binary SHA-256:
  `c567fac1711198e15768097fdd654e4cde5a7d2e39e188e65f5c4bddceb424db`,
  `1f9ebb1931a5a697c1745c7584895565b7433cb29d342787c14adeb1feb86188`,
  `82d6dd5a779586b1a7799b1eae315048eb7ff34be4652bca33fc23b1f2dbb12c`.
- Same local three-process FDB 7.3.77 cluster and Go 1.26.9 setup as the
  [commit-dispatch campaign](commit-send-noack-20261017.md), including its shared-host,
  SMT-control-placement, reference-provenance and counter-subtraction limitations.
  `GOMAXPROCS=8`, `GOGC=100`, `GOMEMLIMIT=off`.
- [Raw results](results-grv-send-noack-screen.jsonl): **66 completed configurations**.
  Each of `get100`, `grv` and `set100` at 32 workers has ten alternating Go pairs
  and two CGo anchors. Each configuration has separate short and long invocations:
  15,000/45,000 iterations for reads/GRV and 1,000/15,000 for writes, with 5% warmup.
  Every invocation completed and passed the harness's byte checks. No builds,
  tests, CI, live profiling or reviewer work ran during timing.

| Workload | Server-cycle ratio | 95% interval | Client-CPU ratio | 95% interval | Mean-latency ratio | 95% interval |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| `get100/32` | 1.011469 | 0.998961–1.025125 | 1.006246 | 0.993859–1.018575 | 0.999975 | 0.985367–1.013741 |
| `grv/32` | 0.996565 | 0.906806–1.084494 | 1.010928 | 0.981016–1.043832 | 0.998655 | 0.988170–1.007454 |
| `set100/32` | 1.020026 | 0.987630–1.052443 | 1.007453 | 0.978194–1.045777 | 1.006109 | 0.978720–1.035359 |

Ratios are paired geometric means, prototype/baseline, not ratios of medians.
Bootstrap uses numeric block order, resets `random.Random(20261017)` per metric,
draws 10,000 paired replicates with `rng.randrange(10)`, and takes sorted indices
249 and 9749. Intervals are nominal.

| Workload | Base server cycles/tx | Prototype server cycles/tx | Base client CPU, µs/tx | Prototype client CPU, µs/tx |
| --- | ---: | ---: | ---: | ---: |
| `get100/32` | 63,036 | 63,299 | 32.145 | 32.630 |
| `grv/32` | 17,899 | 18,123 | 11.170 | 11.321 |
| `set100/32` | 227,965 | 231,114 | 41.415 | 41.298 |

These are medians of ten configurations per arm. The two CGo anchors per cell have
median server cycles/client CPU of 43,256/33.091 µs (`get100`), 22,009/19.091 µs
(`grv`) and 136,672/29.531 µs (`set100`). They are sparse anchors, not a matched
reference population or a general parity claim.

## Disposition

**Not promoted.** No cell establishes a client-CPU or server-cycle improvement.
The prototype passed the fast lane: 138 targets, 81 executed / 57 cached, with
unchanged tracked/untracked fingerprints; the benchmark binary was then built
from that frozen tree. Full-lane, race, new fault-gate regressions, compiled
mutations and final PR qualification were not performed for this rejected change.
The production implementation was restored. The remaining pre-GRV-send timing and
allocation costs need separate measured experiments; this screen does not justify
removing acknowledgments from the broader read helper.
