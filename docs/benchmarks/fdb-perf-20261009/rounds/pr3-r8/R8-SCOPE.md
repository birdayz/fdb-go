# R8: PR3 buffered readLoop

Perf-lead/root #846 requested two prespecified timed campaigns and one separate strace diagnostic process per old/new arm. No PR1 or PR2 is stacked.

Old is the existing binary compiled from `9377c0863aef7ecdee250cdc323e40ebf69d5e46`, SHA256 `4d236cbfc53cea32211b5042a86e0e14dc006f8896a1204b27aee3921fb17e71`. Reuse was authorized conditional on equality to master `590145e3161c8dc5cda26b62a95d578b3bdafd3b`: exact Git diffs for `pkg/fdbgo tools` and separately `go.mod go.sum MODULE.bazel .bazelrc` were empty, and recursive remote comparisons of the frozen source subtrees and module files also passed. The old binary retains its real compiled-source identity, not a relabeled master SHA.

New is built from a full Git export of exact master590145 plus patch SHA256 `f02c03072178a7a1bfd4703e58a7401afdfcba1ccee3273ed5f721306d0c2128` (16 KiB bufio.Reader in readLoop). Its binary SHA256 is `697bbc46d7e6ff900c0b9753d4d842820d37e71eb8d802305b2541fd79f8d846`. The source diff is exactly three transport files: BUILD, conn.go and the new regression test. Full parent/new populations 6865/6866 and contents were verified before/after the build. Only NEW was built, on the VM, using unchanged frozen V6 harness source SHA256 `3d2504ae0db14fad14f0384287377998231cf049310c8289d6e7245d3428e0d0`, Go1.26.9/CGO_ENABLED=0. Its 15 standalone harness tests passed. Parent/client regression/Bazel/review approval remains root/perf-lead-owned; it is not established by this benchmark or those harness tests.

CGo is unchanged, binary SHA256 `cd762ea429647784378270613f404573ba598a5049f422e055a887f7746ecbdb`, API730/libfdb_c7.3.77 SHA256 `e4ad7fed7b7580bf5f7b38f0a4279d37e698f69d534182e5de0b8beeef6cd1b1`.

## Fixed campaign design

- read1000: 5000 measured/200 warmup, workers1. Fresh ReadTransact/Get exact1000B value, no commit. The count was fixed before execution: prior R1 used5000/200, maximum process wall3.040365898s across24arms;18*(max+3s gates)=108.727s, comfortably below180s. No adaptive repeats.
- batchset50: 2500 measured/200 warmup, workers1. Fifty distinct100B Sets+commit/transaction, same as R6 small-value design.
- Each campaign has all six permutations of old/new/CGo across six paired blocks, independent fixed seeds788106/788206, fresh equal68-byte prefixes r8r1/r8b1, and its own180-second cap. All outcomes retained.
- Same frozen runner/analyzer and environment: Go1.26.9, FDB7.3.77, GOMAXPROCS8/GOGC100/GOMEMLIMIT off, runner CPUs0-1/server2-7/clients8-15,2s cooldown+1s quiescence gate,1Hz telemetry. No build/profiler/workload overlaps either timed campaign.
- Full old/new file populations/hashes, binaries, runtime and script identities are checked around campaigns. Final-state bytes, every raw measured/warmup row, success/attempt counts and observed Go counters are validated. Native internal counters are unobserved.

Absolute values are medians of six run-level statistics. Paired geometric means use within-block ratios, NOT ratios of those medians. Nominal95% percentile intervals use10,000 resamples of whole three-arm blocks, seed788707, common indices across metrics/comparisons. CPU is measured-phase getrusage user+system per completed transaction; user/system components are reported separately. Every per-block CPU ratio is retained. Six blocks are exploratory on one continuously running VM/server; statistical independence and CI coverage are not established, and no multiplicity correction or noninferiority margin is supplied. No global/native-capacity claim. Go allocation/GC excludes the C++ heap.

## Separate syscall attribution

Exactly one old then one new `strace -f -c` read1000 process,5000measured+200warmup, fresh68-byte r8d1 prefixes, same options/affinities and quiescence checks. Each is bounded180s and starts only after both timed campaigns finish. No CGo syscall arm was requested for this specific attribution step. Its whole-process syscall counts include startup, executable-hash and fixture reads, warmup, measurement, persisted validation and shutdown. Dividing by5000 gives whole-process counts per measured transaction, NOT measured-phase syscall counts. The trace includes failed/EAGAIN calls; errors are reported separately. Instrumented CPU/latency does not enter timing conclusions. Full strace summaries are retained and their syscall row counts/errors are crosschecked against the total row.
