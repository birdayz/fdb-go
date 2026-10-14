# RFC-258 PR4: remote-fetch allocations

## Scope and ownership

PR4 passes the mapped raw record by value and replaces per-row pending wrappers
and closures with one pending slot in the existing size-one pipeline. The slot
is internal scratch: returned index entries, stored records, protobuf messages,
and primary-key tuples are not recycled. Legacy-version reads retain their
readiness, memoized completion, and deserialization-error precedence.

Reconstruction uses the existing fast tuple decoder after checking actual
subspace membership. A read-only transaction avoids repacking a local-version
key; the empty-cache check and lookup share the version-cache mutex. Mapped
conflicts are inserted directly, primary then secondary, without accumulating a
temporary range slice. Earlier conflicts and the failing range's unmodified
prefix remain recorded on error 2039.

The facade already aliases client value structs. Its shared first future,
returned batch descriptors, secondary slices, and reply bytes must remain valid
when another batch is read. This change does not recycle those slices or frames.
It does not implement RFC-258 Part 2's fused cursor or transaction slab.

## Fixed-population whole-path measurement

- Baseline: `5052212f520d1f888429d0bbcc749b657e27f39e` (PR2).
- Candidate: this PR4 implementation on the same parent. At measurement time,
  the tracked implementation/test diff had SHA-256
  `951ff02e504cb6c7f97ad291091b086333b97f164bceda7051da966ea660296f`.
- Identical benchmark source in both trees:
  `pkg/recordlayer/index_remote_benchmark_test.go`, SHA-256
  `05c5f9bd9557403ef12ca2ca85f30f0896ef4c6822f87e2296683fe0413f9c0a`.
- Linux amd64, AMD Ryzen 9 3900X, Go **1.26.9**, Bazel **9.0.1**,
  FoundationDB **7.3.77**, API version **730**, default benchmark parallelism 24.
- Baseline and candidate ran sequentially under `heavy.lock`, at `nice -n 15`.
  The host was not promised quiet; **ns/op is informational, not a timing claim**.
- Each method/tree has **three samples of 20 measured operations**. An operation
  creates a transaction, opens the store, scans all **1,000** small Order records
  through a VALUE price index, validates ordered IDs/prices, retains the complete
  result, and closes/cancels the cursor/context. Seeding and instrumented
  positive-control scans are outside the measurement; timers are disabled in
  measured operations. Transaction/store-open overhead is included.

| Tree | Method | Sample | allocs/op | B/op | Informational ns/op |
|---|---|---:|---:|---:|---:|
| PR2 | SCAN_AND_FETCH | 1 | 50,109 | 4,999,315 | 24,722,986 |
| PR2 | SCAN_AND_FETCH | 2 | 50,032 | 4,976,414 | 24,803,799 |
| PR2 | SCAN_AND_FETCH | 3 | 50,044 | 4,984,429 | 24,373,238 |
| PR2 | USE_REMOTE_FETCH | 1 | 35,986 | 3,381,658 | 9,068,920 |
| PR2 | USE_REMOTE_FETCH | 2 | 35,988 | 3,382,712 | 9,237,868 |
| PR2 | USE_REMOTE_FETCH | 3 | 35,988 | 3,381,468 | 9,015,974 |
| PR4 | SCAN_AND_FETCH | 1 | 50,107 | 5,000,732 | 23,103,007 |
| PR4 | SCAN_AND_FETCH | 2 | 50,069 | 4,995,384 | 23,162,335 |
| PR4 | SCAN_AND_FETCH | 3 | 50,066 | 4,988,797 | 23,946,185 |
| PR4 | USE_REMOTE_FETCH | 1 | 21,918 | 2,641,422 | 8,148,947 |
| PR4 | USE_REMOTE_FETCH | 2 | 21,915 | 2,640,724 | 7,693,180 |
| PR4 | USE_REMOTE_FETCH | 3 | 21,912 | 2,639,134 | 8,100,945 |

Remote-fetch medians fall from **35,988 to 21,915 allocs/op** (**39.10%**) and
**3,381,658 to 2,640,724 B/op** (**21.91%**). Amortized allocations per row,
including transaction and store-open overhead, fall from **35.988 to 21.915**.
The ordinary-scan control remains approximately 50 allocations per row.

The small variation between whole-path samples includes asynchronous client and
runtime work. These figures are measurements, not exact whole-path allocation
assertions. Exact deterministic guards isolate the components below.

### Reproduce

Copy this benchmark and its Bazel source declaration into a detached baseline
worktree at the SHA above; use the same `user.bazelrc` and toolchain for both.
Run baseline first, then candidate inside one lock acquisition:

```sh
nice -n 15 flock --close ~/.local/share/fdb-perf/validation/heavy.lock bash -c '
  set -e
  for tree in /path/to/baseline /path/to/candidate; do
    cd "$tree"
    bazelisk build //pkg/recordlayer:recordlayer_test
    bin=$(bazelisk info bazel-bin)
    "$bin/pkg/recordlayer/recordlayer_test_/recordlayer_test" \
      -test.run="^$" \
      -test.bench="^BenchmarkIndexRemoteFetchScanAllocations$" \
      -test.benchtime=20x -test.benchmem -test.count=3 \
      -test.timeout=10m --ginkgo.skip=".*"
    bazelisk shutdown
  done
'
```

The benchmark checks a nonempty 1,000-row population and a remote-fetch timer
positive control before each sample. That counter proves path selection, not
RPC batch counts.

## Deterministic allocation guards

`just test` includes the following, with process isolation where needed:

- **1,000 prepared-row materialization samples:** SCAN_AND_FETCH **8**, remote
  loader **8**, direct BuildSingleRecord **8** allocations per row. This includes
  ordinary pending-load wrappers but excludes network reads, asynchronous range
  workers, and store opening. The reusable remote pending slot adds **zero**
  allocations to direct reconstruction.
- **1,000 key samples:** complete remote reconstruction **3** allocations versus
  the prior canonical tuple unpack alone at **7**; both yield the expected key.
- **256 secondary conflict rows:** exactly **257** allocations after warming
  owned conflict buffers, one endpoint copy per primary/secondary range and no
  growing temporary descriptor slice.

Retention, wrong-prefix/suffix errors, local-version sizes, orphan memoization,
legacy-version completion, and real-FDB deserialization-error precedence have
separate correctness regressions. These guards do not claim wire decoder or
protobuf materialization has reached the theoretical minimum.

The allocation guards were revert-proved through the complete fast lane with
compile-valid mutations: restore the prior growing conflict-range collection,
restore canonical record-key unpacking, and wrap the reusable pending slot in
`NewPendingRead` with bound Get/IsReady closures. The two affected targets failed:
conflict bookkeeping rose to **265** allocations versus the required **257**;
record materialization reported **SCAN_AND_FETCH 8, remote 16, direct 13** and
failed its wrapper/control bounds. The original implementation was restored
byte-for-byte before final validation.
