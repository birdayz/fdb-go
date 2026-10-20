# hcloud-rebench-20261014

A preregistered benchmark campaign on one dedicated 16-vCPU cloud host,
comparing the pure-Go client, Record Layer and SQL layer across the trees of
the RFC-258 work. Every phase's plan was sealed before timing, every run was
executed once in a counterbalanced block order, and adverse data were kept.

## Environment

- Hetzner `ccx43` (fsn1): 16 dedicated vCPUs (AMD EPYC Milan, 8 cores × 2 SMT
  threads, one NUMA node), 64 GiB. Guest CPU groups by sibling pairs:
  housekeeping and runner 0–1, `fdbserver` 2–7, benchmark client 8–15.
- One `fdbserver` 7.3.77 process, single redundancy: `ssd-2` for the client
  phases, `memory` for the Record Layer and SQL phases. The benchmark process
  ran in a network namespace and reached the server over a private veth pair
  on the same host, with no artificial delay (except phase C3).
- Go 1.26.9. Details per phase are in each phase report.

## Trees

| Name | Commit | What it is |
|---|---|---|
| legacy | `9a9a448670a748e4ea721772ba96d70ccc4210a0` | The tree behind the published 2026-10-12 numbers |
| before | `3824f14654edae8431b0085de8ebf83f5a460a08` | legacy plus the read-path parity change (#888), before the allocation work |
| pr2 | `5052212f520d1f888429d0bbcc749b657e27f39e` | The first tree with remote fetch |
| final | `267a41434a81723b07e7150869d702636a1b91a9` | The final tree of the work |

Every tree ran the same benchmark harness,
`d94b4c61377d173b7c6884fde426a227dab0f0bb`
([campaign-harness.md](../campaign-harness.md)), so differences come from the
production code. The campaign tooling is in
[`tools/benchcampaign/singlenode`](../../../tools/benchcampaign/singlenode/README.md).

## Method

- Per case, balanced blocks in which every arm runs once in a preregistered
  order; one fresh process per arm and block.
- The paired effect is the geometric mean over blocks of each block's arm
  ratio, with a nominal 95% percentile bootstrap over blocks (20,000
  resamples, seed 391706). Intervals are per case and metric, with no
  multiplicity correction and no equivalence claim.
- testing.B phases report `ns/op` (a mean, not a percentile), `B/op` and
  `allocs/op`; the client phases report per-run latency statistics and CPU.
- The frozen protocol and its earlier versions are in
  [`protocol/`](https://github.com/birdayz/fdb-go/tree/master/docs/benchmarks/hcloud-rebench-20261014/protocol); the arm orders are
  `tools/benchcampaign/singlenode/testdata/orders.json`. The controller scripts that
  ran C2, D2 and C3 and restored the case labels are kept as
  [`as-run/`](https://github.com/birdayz/fdb-go/tree/master/tools/benchcampaign/campaigns/hcloud-rebench-20261014/as-run).

## Phases

| Phase | Question | Report |
|---|---|---|
| A | Client: final pure-Go vs CGo (`libfdb_c` 7.3.77) and vs before | [phase-a-client.md](phase-a-client.md) |
| B | Record Layer: legacy vs before vs final | [phase-b-recordlayer.md](phase-b-recordlayer.md) |
| C1 | Remote fetch vs ordinary index fetch, pr2 and final | [phase-c1-remotefetch.md](phase-c1-remotefetch.md) |
| D1 | SQL: before vs final, with legacy as anchor | [phase-d1-sql.md](phase-d1-sql.md) |
| C2 | Remote fetch on reverse scans and executor Skip paths, final | [phase-c2-reverse-skip.md](phase-c2-reverse-skip.md) |
| D2 | SQL remote fetch on a non-covering index, final | [phase-d2-sql-remotefetch.md](phase-d2-sql-remotefetch.md) |
| A2 | Client closed-loop concurrency scaling (8 workers) | [phase-a2-client-8w.md](phase-a2-client-8w.md) |
| C3 | Sensitivity to synthetic network latency (not a multi-host result) | [phase-c3-synthetic-rtt.md](phase-c3-synthetic-rtt.md) |

Headline results:

- **A:** the final pure-Go client takes 0.52–0.93 of CGo's mean latency on
  every workload with a server round trip, and 0.19 on the in-memory `ryw`
  workload.
- **B:** the allocation work cut `allocs/op` by 11–37% in every Record Layer
  case, while every `ns/op` point estimate stayed within 4% of 1.
- **C1:** remote fetch is faster than the ordinary path in five of six cases
  (0.34 for 1,000 small records) and 15% slower for 1,000 split records, a
  retained negative result.
- **D1:** only `FDB_PlanCacheHit` was analyzed; the phase stopped early on a
  false correctness failure in the runner (since fixed). No measurable time
  change (final/before 1.002 [0.984, 1.021]); `allocs/op` fell 5.1%.
- **C2:** remote fetch is faster on the reverse scan of 1,000 small records
  (0.337) and with executor Skip 3 (0.455 over the whole index, 0.783 for ten
  rows), and 15% slower on the reverse scan of 1,000 split records (1.146),
  matching C1's forward result.
- **D2:** through SQL on a non-covering index, remote fetch takes 0.885 of the
  ordinary path's time for 10 rows and 0.536 for 1,000 rows. Three ordinary
  processes got a one-byte-shorter store prefix; the report discloses this
  confound, and prefix-matched block estimates are similar (a post-hoc,
  descriptive check).
- **A2:** at 8 closed-loop workers, pure-Go is faster than CGo on `range100`
  mean latency (0.663 [0.440, 0.858]) and `batchset50` mean/p99 latency (0.817
  [0.768, 0.851] / 0.914 [0.816, 0.985]); on `get100` latency the nominal
  interval includes 1 (0.991 [0.928, 1.084], no clear difference resolved in
  this bounded sample), while pure-Go uses more shutdown process CPU (E−A)
  per transaction across all three workloads (1.150–1.638).
- **C3:** under 0.5 ms nominal synthetic veth RTT (250 µs each way, p50 0.597
  ms), remote fetch takes 0.224 [0.222, 0.226] of the ordinary path's time on
  1,000 small records (24.0 ms vs 108.0 ms), while 1,000 two-chunk split
  records remain 5.3% slower with remote fetch (1.053 [1.008, 1.101]),
  retained as a negative result.

## Shared limits

- One host, one `fdbserver` process, single redundancy: no replication and no
  network hop between machines. The 4-node VPC campaign is reported in
  [`hcloud-multinode-vpc-20261014`](../hcloud-multinode-vpc-20261014/README.md).
- A runner bug wrote a CPU field name instead of the case key into the
  testing.B phases' aggregate CSV. Case labels were restored from the pinned
  plans and unchanged per-run logs, without repeating or changing any
  measurement; the runner is fixed in this repository with a regression test.
