# Record-load range-read parity: before and after

This measures the correctness change from point probes to Java's default whole-record
range read. It is **not a performance improvement**: time and allocations increase.

## Source and environment

- Baseline: `32ec8ae6490b51d574923ea324a03a6ebc87a4b4` (branch merge-base).
- Candidate implementation: `747132539768bde161cd4ed141cd4f75c331d523`.
  The candidate binary was built from the source subsequently committed at this
  revision. This report and the raw-output files are documentation-only additions.
- Linux amd64, AMD Ryzen 9 3900X, 24 logical CPUs, Go 1.26.9.
- FoundationDB 7.3.77, single-node Docker, `memory` storage engine, real database.
  Each binary starts its own database; both binaries use the same filesystem and
  toolchain. Container startup and benchmark setup are outside the measured loop.
- Five samples per benchmark, three seconds per sample, `-benchmem`; each table
  cell is the median of all five samples. Runs were sequential, baseline then
  candidate, under the shared heavy-work lock at nice level 15. No local validation
  overlapped this paired run. The host also runs other work: these five samples
  do not establish statistical significance or predict production latency.
- Workload source: [`benchmark_test.go`](../../../pkg/recordlayer/benchmark_test.go).
  Save and load operations include the transaction/context lifecycle. Save includes
  commit; indexed save maintains the price value index. Both index scans consume
  1,000 records per operation, either serially or with record-load pipelining.
  These are small, unsplit protobuf records, not split-record throughput tests.

## Median results

| Benchmark | Before ns/op | After ns/op | Time change | Before B/op | After B/op | Before allocs/op | After allocs/op |
|---|---:|---:|---:|---:|---:|---:|---:|
| SaveRecord | 1,010,773 | 1,018,675 | +0.78% | 21,458 | 23,016 | 289 | 313 |
| LoadRecord | 215,370 | 264,689 | +22.90% | 12,989 | 14,682 | 180 | 205 |
| SaveRecordWithIndex | 1,013,865 | 1,173,299 | +15.73% | 25,656 | 27,346 | 359 | 384 |
| ScanIndexRecords/serial (1,000 records/op) | 58,683,625 | 84,259,234 | +43.58% | 3,375,851 | 5,394,481 | 41,222 | 72,109 |
| ScanIndexRecords/pipelined (1,000 records/op) | 24,735,823 | 26,638,001 | +7.69% | 3,969,387 | 6,014,289 | 47,381 | 78,202 |

Raw samples: [before.txt](before.txt), [after.txt](after.txt). Save benchmarks reuse
one benchmark-specific subspace across repetitions; the first repetition therefore
has a different insert/overwrite allocation population. All five samples are
retained and included in the medians, following the existing benchmark procedure.
The candidate also has noticeable timing variance; inspect the raw output rather
than treating the median percentage as a precise causal estimate.

Modern record loads now consume the entire strict record subspace, including inline
versions and malformed suffixes, instead of issuing a point probe. The extra work
preserves Java conflict ranges, corruption checks and size/version accounting.
Pipelining still issues the window's ranges before waiting, but does not eliminate
the additional decoding, result buffering and allocation costs. The regression is
reported rather than hidden by retaining the incorrect point-read fast path.

## Reproduction

Build each revision through Bazel, preserving its binary before checking out or
building the other revision:

```sh
bazelisk build //pkg/recordlayer:recordlayer_test
cp bazel-bin/pkg/recordlayer/recordlayer_test_/recordlayer_test /tmp/recordlayer-BEFORE
# In the candidate worktree, build and copy the same target to /tmp/recordlayer-AFTER.
```

Then run the two binaries sequentially (the measured run used the same flags):

```sh
nice -n 15 flock --close ~/.local/share/fdb-perf/validation/heavy.lock bash -c '
  set -e
  for revision in BEFORE AFTER; do
    /tmp/recordlayer-$revision -test.run="^$" -test.benchtime=3s \
      -test.count=5 -test.benchmem --ginkgo.skip=".*" \
      -test.bench="^Benchmark(SaveRecord|LoadRecord|SaveRecordWithIndex|ScanIndexRecords)$" \
      > /tmp/recordlayer-$revision.txt 2>&1
  done
'
```

SHA-256 of the actual measured binaries:

```text
30fffc872aee6238608fd215286e11f354d0a521490d8cfc334a872b6e166b24  baseline
238355673145aad97f3462a0ce424ea69a00c73bf6ee2f644b9b5f5a0ead4af4  candidate
```

## Correctness evidence

The regression-first real-FDB run, with original production code, had 63 expected
failures: T1 loaded a record, T2 inserted an absent record-subspace key and committed,
and T1 incorrectly committed instead of returning conflict 1020. Actual-key and
unrelated-primary-key controls distinguished missing conflict ranges from an
invalid harness. The final implementation's fast-lane hook passed all 123 targets
(2 executed, 121 cached); the immediately preceding run executed 47 targets and
passed all Record Layer and executor regressions, but failed a Docker cleanup test.

That earlier failure was `TestRun_InterruptedFileAllocation/{tmpfs,disk}` in
`//pkg/testcontainers/foundationdb:foundationdb_test`: fault and commit/read controls
passed, but Docker's stop request exceeded its 30-second timeout after container
process deletion. The target depends on Record Layer, while this failing test path
only calls Run/FDBCLIExec/Terminate, not Record Layer. Investigation evidence is
retained locally at `/tmp/docker-stop-anomaly.txt`; the same daemon behavior was
observed on unrelated containers and is under separate investigation. The normal
commit hook was permitted after that investigation and passed without changing
cleanup timeouts, skipping tests, restarting Docker or bypassing the hook.

The Java 4.14.2.0 conformance regression covers 120 transaction outcomes across
Go-default, Java-default (property unset), and Java-GETS modes. Full and Java
conformance execution belongs to CI; the local fast pass is not a full-suite claim.
