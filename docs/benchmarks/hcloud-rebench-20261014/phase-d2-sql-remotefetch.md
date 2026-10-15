# Phase D2: SQL remote fetch on a non-covering index

Part of the [hcloud-rebench-20261014](README.md) campaign.

`BenchmarkSQLIndexFetch` in the final tree, run once per explicit
`INDEX_FETCH_METHOD`. The question: when a SQL query really goes through a
non-covering index, what does `USE_REMOTE_FETCH` (the storage server resolves
each index entry to its record in one mapped range read) buy over
`SCAN_AND_FETCH` (the client scans the index and loads each record itself),
measured end to end through the SQL driver on a warmed, pinned connection?
[Phase C1](phase-c1-remotefetch.md) asked the same question one layer down,
directly against the Record Layer.

## Arms

| Arm | Source | Fetch method |
|---|---|---|
| final-ordinary | `267a41434a81723b07e7150869d702636a1b91a9` | `SCAN_AND_FETCH` |
| final-remote | `267a41434a81723b07e7150869d702636a1b91a9` | `USE_REMOTE_FETCH` |

Both arms are the same benchmark binary built from the final tree (the tree
behind every `final` arm in this campaign: the Record Layer read-path parity
change, the allocation work, and remote fetch with everything merged since; see
[phase B](phase-b-recordlayer.md) and [phase C1](phase-c1-remotefetch.md)).
They differ only in the method sub-benchmark the process selects
(`query=<case>/method=SCAN_AND_FETCH` or `method=USE_REMOTE_FETCH`). The
benchmark (`pkg/relational/sqlbench/fetchmethod`) exists only in trees that
have the SQL fetch-method option, so this phase has no older-tree arm. Harness
`d94b4c61377d173b7c6884fde426a227dab0f0bb`; see
[campaign-harness.md](../campaign-harness.md).

## Environment and design

- Host, CPU groups and network path as in [phase A](phase-a-client.md) and
  [phase B](phase-b-recordlayer.md): Hetzner `ccx43`, one `fdbserver` 7.3.77
  process on CPUs 2–7 with the `memory` engine and single redundancy, the
  benchmark process on CPUs 8–15 (`GOMAXPROCS=8`, Go 1.26.9) in a network
  namespace reaching the server over a veth pair.
- Fixture: table `T (id, a = id, c = 'v<id>')` with index `IDX_A` on `a` and
  1,000 rows, created in a database of its own for each benchmark body run
  and dropped afterwards. The cluster's user keys were cleared between
  processes.
- Cases, both projecting `id, c` so `IDX_A` cannot cover the query:
  - `rows10`: `a >= 100 AND a < 110 AND c IS NOT NULL`, 10 rows;
  - `rows1000`: `a >= 0 AND c IS NOT NULL`, all 1,000 rows.
- One operation executes the query and drains its rows. It runs on a pinned
  connection that carries the fetch method, has no StoreTimer, and has a warm
  plan cache, so planning is excluded. Every operation's rows (count, IDs and
  values, in order) are checked with the timer stopped. Before and after the
  measured loop, a second pinned connection with a StoreTimer checks
  `EXPLAIN` (an `IDX_A` scan, not covering), is warmed, has its timer reset,
  and runs the query once to assert the path: remote fetch with at least one
  remote-fetch opening and no index scan or record load, or `SCAN_AND_FETCH`
  with no remote fetch, an index scan and at least one record load per row.
  `EXPLAIN` is also checked on the measured connection.
- Per case: 6 blocks over the two arms; the block order (seed 641501) puts
  `final-ordinary` first in blocks 1, 2 and 6 and `final-remote` first in
  blocks 3, 4 and 5. One fresh process per arm and block, three repeats each
  (`-test.count=3`, 3 s per repeat): 18 samples per arm and case, 24 processes
  and 72 samples in total.
- Estimand and bootstrap as in phase B: geometric mean over blocks of each
  block's ratio of process medians (each process's median of its three
  repeats), nominal 95% percentile bootstrap over whole blocks (20,000
  resamples, seed 391706). Absolute values are the median over blocks of the
  process medians. `ns/op` is testing.B's mean, not a percentile; `B/op` and
  `allocs/op` are Go heap allocations per operation in the benchmark process.
  The benchmark also reports `rows/op` (10 or 1,000 in every sample of both
  arms) and `prefix-B`, the byte length of the store prefix the fixture's
  schema resolved to.

## Results

Ratios final-remote / final-ordinary with nominal 95% intervals. Below 1 means
remote fetch is faster or allocates less. The analysis flags both cases with a
physical prefix confound (see [Limits](#limits)); the `prefix-B` column is that
flag's measurement, not a performance result.

| Case | ns/op | B/op | allocs/op | prefix-B |
|---|---:|---:|---:|---:|
| rows10 | 0.885 [0.873, 0.896] | 0.843 [0.842, 0.843] | 0.850 [0.850, 0.850] | 1.031 [1.000, 1.095] |
| rows1000 | 0.536 [0.532, 0.538] | 0.657 [0.656, 0.657] | 0.614 [0.614, 0.614] | 1.063 [1.000, 1.129] |

### Absolute values

Median over blocks of each process's median of its three repeats; ranges are
the minimum and maximum over all 18 raw samples per arm.

| Case | final-ordinary | final-remote | ordinary range | remote range |
|---|---:|---:|---:|---:|
| rows10 | 2.50 ms | 2.22 ms | 2.44 ms – 2.58 ms | 2.02 ms – 2.30 ms |
| rows1000 | 46.5 ms | 24.9 ms | 44.1 ms – 47.3 ms | 24.4 ms – 27.3 ms |

Allocations per operation:

| Case | B/op ordinary | B/op remote | allocs/op ordinary | allocs/op remote |
|---|---:|---:|---:|---:|
| rows10 | 131,190 | 110,510 | 1,738 | 1,477 |
| rows1000 | 6,298,330 | 4,133,208 | 80,384 | 49,347 |

### Blocks and store prefix lengths

Per block: which arm's process ran first, each process's `prefix-B` (constant
over its three repeats in all 24 processes), and the block's remote/ordinary
ratio.

| Block | First process | rows10 prefix-B ordinary / remote | rows10 ns/op | rows10 B/op | rows1000 prefix-B ordinary / remote | rows1000 ns/op | rows1000 B/op |
|---:|---|---:|---:|---:|---:|---:|---:|
| 1 | ordinary | 6 / 6 | 0.906 | 0.842 | 6 / 6 | 0.527 | 0.656 |
| 2 | ordinary | 6 / 6 | 0.862 | 0.842 | 6 / 6 | 0.536 | 0.656 |
| 3 | remote | 6 / 6 | 0.892 | 0.842 | 5 / 6 | 0.539 | 0.657 |
| 4 | remote | 6 / 6 | 0.891 | 0.842 | 6 / 6 | 0.539 | 0.656 |
| 5 | remote | 5 / 6 | 0.891 | 0.844 | 6 / 6 | 0.535 | 0.656 |
| 6 | ordinary | 6 / 6 | 0.870 | 0.842 | 5 / 6 | 0.538 | 0.657 |

The same ratios restricted to the blocks where both processes had the same
prefix length. This is a post-hoc descriptive check, not part of the sealed
analysis, and has no interval:

| Case | Blocks with equal prefix-B | ns/op | B/op | allocs/op |
|---|---:|---:|---:|---:|
| rows10 | 5 of 6 | 0.884 | 0.842 | 0.850 |
| rows1000 | 4 of 6 | 0.534 | 0.656 | 0.614 |

Order and single-block sensitivity (descriptive):

| Case | Metric | ordinary first (3 blocks) | remote first (3 blocks) |
|---|---|---:|---:|
| rows10 | ns/op | 0.879 | 0.891 |
| rows10 | B/op | 0.842 | 0.843 |
| rows10 | allocs/op | 0.850 | 0.850 |
| rows1000 | ns/op | 0.534 | 0.538 |
| rows1000 | B/op | 0.657 | 0.657 |
| rows1000 | allocs/op | 0.614 | 0.614 |

| Case | Metric | leave-one-block-out range |
|---|---|---:|
| rows10 | ns/op | 0.881–0.890 |
| rows10 | B/op | 0.842–0.843 |
| rows10 | allocs/op | 0.850–0.850 |
| rows1000 | ns/op | 0.535–0.537 |
| rows1000 | B/op | 0.656–0.657 |
| rows1000 | allocs/op | 0.614–0.614 |

## Interpretation

Measured:

- **Remote fetch is faster through SQL for both result sizes.** It takes
  0.885 [0.873, 0.896] of the ordinary path's time for 10 rows
  (2.50 → 2.22 ms) and 0.536 [0.532, 0.538] for 1,000 rows
  (46.5 → 24.9 ms). Every block agrees: the six block ratios span 0.862–0.906
  for `rows10` and 0.527–0.539 for `rows1000`.
- **It also allocates less in the client.** `B/op` is 0.843 and 0.657 of the
  ordinary path's and `allocs/op` 0.850 and 0.614, with intervals at most
  0.001 wide; that is about 26 (`rows10`) and 31 (`rows1000`) fewer heap
  allocations per returned row.
- **The path is the intended one.** Every audit before and after every measured
  loop saw a non-covering `IDX_A` plan, remote-fetch openings with no index
  scan or record load for `final-remote`, and an index scan with record loads
  for `final-ordinary`; every measured operation returned exactly the
  expected rows.
- **Prefix-matched blocks give similar time ratios (post-hoc, descriptive).** This
  shows the estimates are robust to dropping those blocks, not that the
  confound has no effect, which cannot be separated by design. The
  blocks whose ordinary process had a 5-byte prefix have time ratios inside
  the range of the prefix-matched blocks (`rows10` block 5: 0.891 against
  0.862–0.906; `rows1000` blocks 3 and 6: 0.539 and 0.538 against
  0.527–0.539), and the prefix-matched geometric means (0.884 and 0.534) are
  within the reported intervals. Bytes are visibly affected: the three
  ordinary processes with the shorter prefix allocated less per operation
  (`rows10` 130,906 B against 131,177–131,227 B in the other five blocks;
  `rows1000` 6,285,798 and 6,289,549 B against 6,298,228–6,299,009 B), which
  raises those blocks' `B/op` ratios slightly (0.844 against 0.842 for
  `rows10`, 0.657 against 0.656 for `rows1000`).
- Process order hardly matters: time ratios are 0.879 (ordinary first) and
  0.891 (remote first) for `rows10`, 0.534 and 0.538 for `rows1000`.

Hypotheses, not tested here:

- The gain grows with the number of rows because remote fetch replaces one
  client-issued record read per index entry with server-side resolution; the
  10-row query's time is more dominated by per-query SQL work (connection,
  plan-cache lookup, result-set setup) that both methods share. No profile
  separated per-query from per-row cost.
- The relative gain through SQL is smaller than the Record Layer measured in
  phase C1 for comparable reads (C1 `small-exact-10` 0.733, `small-all-1000`
  0.341), plausibly because SQL adds per-row work on top of the fetch. The
  two phases ran different benchmarks in separate processes and blocks, so
  this is not a paired comparison.
- The shorter prefixes probably come from the directory layer's prefix
  allocation, which can hand out allocations whose encoded length differs
  between runs even on a cleared cluster. The benchmark resolves the prefix in
  its fixture before any method-specific code runs, so the arm should not
  determine it; that all three short prefixes fell on `final-ordinary`
  processes may be chance. Neither the mechanism nor the arm pattern was
  investigated.
- All of this excludes inter-host and physical-network latency: client and
  server share a host over a veth pair. Remote fetch saves client round trips, so its effect over a real
  network is likely different; the multi-node campaign
  (`tools/benchcampaign/multinode`) is designed to measure that.

## Limits

- **Physical prefix confound.** The analysis flags both cases because the
  store prefix the fixture resolved to was not the same length in every
  process: 6 bytes in all 12 `final-remote` processes, but 5 bytes in 3 of the
  12 `final-ordinary` processes (`rows10` block 5, `rows1000` blocks 3 and 6)
  and 6 bytes in the rest. Each benchmark body creates its database fresh, and
  the prefix is allocated then, so it can differ between processes despite the
  cluster being cleared in between (the harness expected clearing to prevent
  this). Every key the query reads (index entries and records) carries this
  prefix, so in those three blocks the ordinary process read keys one byte
  shorter than the remote process it is paired with, and the block ratio
  mixes the method effect with a key-length effect. Prefix length was not
  controlled, so its effect cannot be separated from the method's by design;
  the prefix-matched figures above are a post-hoc, descriptive check only.
  The `prefix-B` ratios (1.031 and 1.063) and their intervals describe how
  often the prefixes differed, not a performance effect.
- Single host, one `fdbserver` process, single redundancy, `memory` engine: no
  replication, no network hop, no disk. One operation at a time on one pinned
  connection.
- testing.B reports means; no latency percentiles.
- Six blocks give limited resolution; intervals are nominal, per case and
  metric, with no multiplicity correction and no aggregate across cases.
- Only one tree (final) and only these two queries over one 1,000-row table
  with short rows; no write path, no larger records, no covering index.
- The phase is complete: all 72 raw samples (18 per arm and case, 24
  processes) are in the analysis, with no exclusions, omitted derived metrics
  or parser failures.
- Case labels in the aggregate CSV were reconstructed from the pinned plan and
  the unchanged per-run logs, for the same runner bug described in
  [phase B](phase-b-recordlayer.md#limits); all 72 labels were restored in a
  separate working copy, every other field matched the raw output, and no
  measurement was repeated or changed. Input pins were verified before
  analysis; hashes are recorded in the campaign's analysis report.
