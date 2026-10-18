# Timer-worker handoff: negative write screen

2026-10-17. A narrow prototype moved timer-expired GRV RPC startup onto the
already-running timer worker. It registered and launched a successor timer waiter,
then synchronously flushed the detached batch and exited. The successor alone owned
future waits and timer retirement; registration rejection retired both newly pending
and already-detached work. Runtime-timer fallback and full-size-batch dispatch stayed
unchanged. No batch-window duration or GC setting changed.

This follows C++ 7.3.77's call to `getConsistentReadVersion` before resuming batching
(`NativeAPI.actor.cpp:7380–7394`), not an assertion of equivalent Flow scheduling.
The motivation was an observed 285.6 µs runnable delay before a detached GRV worker's
first execution in one earlier clean runtime trace.

## Comparison

- Baseline: master `590aa97e326b612cf7768c3c14578d3470281443`, including #953.
- Candidate: that source plus the timer-worker handoff. The prototype was not
  committed. Its artifact digest and description identify this comparison, not a
  reproducible production source revision.
- Baseline / prototype / retained CGo binary SHA-256:
  `c567fac1711198e15768097fdd654e4cde5a7d2e39e188e65f5c4bddceb424db`,
  `9b87032d54893407d9a7cbd4fcf637578e4c501c4c7ce1ce236aa9d17d08e698`,
  `82d6dd5a779586b1a7799b1eae315048eb7ff34be4652bca33fc23b1f2dbb12c`.
- Same local three-process FDB 7.3.77 cluster and Go 1.26.9 setup as the
  [commit-dispatch campaign](commit-send-noack-20261017.md), including its shared-host,
  SMT-control-placement, reference-provenance and counter-subtraction limitations.
- `set100`, 32 workers, short=1,000 / long=15,000 iterations, `GOGC=100`,
  `GOMAXPROCS=8`, `GOMEMLIMIT=off`. Ten alternating baseline/prototype pairs and
  two CGo anchors: [22 completed configurations](results-grv-dispatch-screen.jsonl).
  Every configuration completed and passed the harness's byte checks. No builds,
  tests, CI, live profiling or reviewer work ran during the timed comparison.

| Metric | Base median | Prototype median | Paired prototype/base ratio | 95% interval |
| --- | ---: | ---: | ---: | ---: |
| Server cycles/tx | 243,297 | 246,050 | 1.016 | 0.992–1.038 |
| Server syscalls/tx | 11.228 | 11.232 | 0.991 | 0.967–1.012 |
| Client CPU, µs/tx | 42.974 | 42.615 | 0.987 | 0.970–1.004 |
| Mean latency, µs | 11,209 | 11,354 | 0.979 | 0.917–1.022 |

Medians and paired ratios are different statistics. Arithmetic mean latency across
runs was 11,611→11,322 µs. Bootstrap uses numeric block order, resets
`random.Random(20261017)` per metric, draws 10,000 paired replicates with
`rng.randrange(10)`, and takes sorted indices 249 and 9749. Intervals are nominal.
The two CGo anchors have median 168,915 server cycles/tx and 31.775 µs client CPU;
they are not a matched reference population.

## Disposition

**Not promoted.** The screen establishes no server-cost improvement at the targeted
32-worker write cell and no clear client-CPU win. An observed scheduling delay in one
trace did not by itself identify a useful production remedy.

The prototype passed the fast lane: 138 targets, 94 executed / 44 cached, with
unchanged tracked/untracked input fingerprints. Static reviewers found no concrete
production ownership defect but identified missing direct execution-placement,
overlapping-RPC, and successful-handoff Close regressions. Full-lane, race, compiled
mutation and final PR qualification were not performed for this rejected prototype.
The implementation and its provisional tests were removed from the production tree.
Separate ordinary traces demonstrate that the prototype path was active: all 1,238
observed GRV reply-select waits had `runWindow` beneath `sendGRVRequest`, versus
zero of 1,189 observed waits in the fresh baseline. Both traces completed 12,000
commits without retries; their RPC counters were 1,242 and 1,194, respectively.
Buffered replies need not produce an observed blocking select. These are diagnostic
positive controls, not performance-comparison samples.

The prototype still split into persistent commit-publication clusters. Its onset
included a 231.0 µs runnable delay on an existing timer worker, a 149.3 µs startup
delay on the successor, and an allocation-assist interval before another GRV send.
In the fresh baseline, two implicated RPC flushers started within 6.2 and 1.4 µs,
then stalled in request-construction assists. Their long synchronization waits were
inside `gcMarkDone`/`gcAssistAlloc`, not serializer-pool mutex contention. Assist
elapsed includes collector coordination and rescheduling, not just marking CPU.
These observations do not establish which allocations caused collection, or that
pooling them would prevent a split. External-flusher startup is not a sufficient
explanation of the current gap; investigate the broader pre-GRV-send frontier.
