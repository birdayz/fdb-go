# perf-lead2 VM queue (after R17, R18)

These can run from local SHAs; they don't need to be merged. So the VM can stay busy even if the feature freeze is still on. Deterministic metrics (allocs/op, round-trip counters) are measured locally. The VM is only for time and CPU.

## M1: record-layer paired run (R19 candidate)
- Arms:
  - BASE = origin/master at the time of the run.
  - NEW = BASE + lazy-continuation (`perf/lazy-index-continuation`) + (A) `perf/range-eager-start` + store-open-on-(A) (`perf/storeopen-concurrent-read-on-a`).
- Benches (recordlayer_test, `just bench-one`): BenchmarkStoreOpen, SaveRecord, LoadRecord, ScanIndex, ScanIndexRecords serial/pipelined.
- Primary metrics: ns/op and process CPU. Secondary, already known locally:
  - allocs/op
  - read round trips per Open (2→1 expected)
- Size: about 30–45 min. Paired, interleaved, fixed seeds.

## M2: range-read workload, Go vs CGo (R20 candidate; needs harness work)
- New workload in the #822 clientbench harness:
  - GetRange want_all and iterator over 100 rows
  - a non-trivial-selector range (offset, lastLess)
  - unconsumed-GetRange + commit
- Arms: CGo / (A) / (A)+PR B (in-read selectors; drops 2 GetKey round trips per selector range).
- Needs pre-registration by astra-perf-measure.
- PR B must be compiled and green first. Not ready yet; ETA after the freeze lifts.

## Not queued
- Read-context RFC (client per-read allocations): RFC only, C++ review pending, no code. No VM slot until it's implemented.
- Leader/follower read: REJECTED by owner.

## Delete criterion
Delete the VM after R18 if M1 isn't ready to start within about 30 min of R18 ending. M2 alone isn't worth keeping it idle.
