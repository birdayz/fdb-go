# Pure-Go FDB client: benchmark correction and measurement plan

## Withdrawn results

**The Go-vs-libfdb_c speedup and write-parity claims previously published here,
in the README and on fdb.dev are withdrawn.** The 2026-04-12 measurements used
an always-on Go read-version (GRV) cache. RFC-104 made that cache opt-in on
2026-06-13 to fix stale reads under the default options. The old comparison
therefore let Go reuse a read version while libfdb_c fetched a fresh one; it
did not compare equivalent default transaction semantics.

The old netem rows also mislabeled one-way delay as RTT: `netem delay 5ms`
was applied only to the container's egress. It adds 5 ms, not 10 ms, to a
client/server round trip; the underlying network latency is additional.

Those historical results are not evidence of current speed, write parity or
cgo/event-loop overhead. No replacement performance claim is made here.

## Measurement status

**No fresh paired before/after results are published here.** An interleaved
revision of [`bench_test.go`](bench_test.go) is pending integration and full-suite
validation. The design below describes that pending harness, not a claim about
the benchmark code currently published at HEAD. Earlier diagnostic runs are not
a substitute for a controlled comparison with that validated harness.

## Pending harness design

The proposed comparison runs both clients in one process against one single-node
FDB 7.3.77 testcontainer, on the same keys and API version (730):

- **Same work.** Paired operations use matching transaction wrappers, key/value
  sizes and range bounds. Read errors propagate through the transaction retry
  loop; final errors and unexpected result sizes fail the benchmark.
  `TestBenchmarkSanity` checks the seeded reads and read-your-writes results
  against expected bytes as well as against the other client.
- **Default read-version semantics.** Neither side opts into GRV caching.
  Reads needing a version fetch one rather than reusing a cached version.
  Read-your-writes and blind writes need not take the same path as remote reads.
- **Interleaved.** Each iteration runs one Go operation and one CGo operation,
  alternating which goes first, to reduce ordering bias from machine drift.
- **Distributions, not just means.** Each operation is timed individually and
  reported as `go-p50-us`, `go-p99-us`, `go-mean-us` and the `cgo-*` equivalents,
  plus sample counts and `*-MB/s` for the throughput benchmarks. Percentiles
  use nearest rank. `ns/op` is suppressed because it would combine both clients.
- **Labeled latency.** `BenchmarkLatencyGet/oneway=D` adds `tc netem` delay `D`
  on the container's egress only (server to client). Each round trip gets `D`
  longer, not `2D`. Setup errors fail rather than silently dropping the delay;
  the benchmark also fails if either client's measured p50 is below `D`.

This design measures sequential, single-client operations, not concurrent
load/capacity tests. The throughput metrics count value bytes, not wire bytes,
per time spent in that client's operations; they are not sustained standalone
client throughput. Both clients share server caches, disk state and a process.
Alternation reduces, but does not eliminate, those effects. Timing, validation
and sample storage add harness overhead. Allocation counts with `-test.benchmem`
combine both clients and the harness (and exclude C-heap allocations); do not
interpret them as per-client memory costs. Tail percentiles from small sample
counts are not reliable tail estimates.

Record the commit SHA (and any dirty changes), CPU, OS, Go and libfdb_c versions,
server configuration, load average and netem settings with any result. Run on
an otherwise idle host and retain all repeated samples, not just the best one.
If comparing code revisions, name both SHAs and repeat both under the same
conditions. Do not infer production performance from this single-node setup.

## Validation and measurement prerequisites

Before collecting new results, integrate the harness and its matching Bazel build
configuration, format the touched Go source, and validate the integrated client
changes with `just test-full` (the benchmark/differential target is `test-full`).
The planned commands require Docker and the repository's Bazel-managed libfdb_c
toolchain. Run timing only on an otherwise idle host, separately from builds,
correctness suites, and compiler/resource experiments.

For a before/after comparison, freeze and name both client commit SHAs. Apply the
same harness to both trees and record that harness revision or patch as well.
Use matching dependency pins, server configuration, storage engine, runtime
settings, and benchmark selection. Take at least two independent repetitions per
revision, alternating revision order. Retain every trial, per-client p50/p99/mean,
completed sample counts, and terminal failures; do not discard a slow trial or
publish a successful-looking ratio from a failed run. Retryable errors handled
inside transaction wrappers are included in elapsed time, not counted separately
by this harness.

**After integration and validation**, run the benchmark executable through Bazel
rather than consuming a cached test result. A small planned comparison covers
Get (100 B / 1 KB / 10 KB), ReadTransact (100 B), and Set+Commit (100 B):

```sh
bazelisk run //pkg/fdbgo/bench:bench_test -- \
  -test.run='^TestBenchmarkSanity$' -test.v \
  -test.bench='^(BenchmarkGet|BenchmarkReadTransact|BenchmarkSet)$' \
  -test.benchtime=10s -test.count=1 -test.timeout=10m
```

Run separate invocations in alternating before/after order; `-test.count=1`
above is one trial, not the whole campaign. Require all five expected benchmark
rows and matching Go/CGo sample counts in every trial. Report sample sizes with
tail percentiles, and lengthen both sides' trials if too few samples were taken.

A separate netem experiment can select `'^BenchmarkLatencyGet$'`. It additionally
needs Linux, container `NET_ADMIN`, and `tc`. Keep it separate from the initial
localhost comparison and report added one-way delay, not an inferred RTT. The
pending harness removes delay after each sub-benchmark and fails on cleanup
errors. No netem results are currently being claimed.
