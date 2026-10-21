# Phase M2: remote fetch vs ordinary index fetch on a 3-node VPC cluster

`USE_REMOTE_FETCH` (`getMappedRange`) against `SCAN_AND_FETCH` on master
(`fa8725f1dcf6ff3ac66825ab6e3e819efbc0abcc`), from one dedicated client host
to three FoundationDB 7.3.77 server hosts (`double` redundancy, `ssd-2`,
9 storage servers). Both methods run in one master binary per benchmark. This
is **phase M2 (`M2rl` and `M2skip`)** of [fdbmn-20261020](README.md).

## Cases and shard placement

- **`M2rl` (`BenchmarkIndexFetch`):** small (unsplit) and split (100,000-byte
  padding, two chunks) records × `limit1`, `rows10`, `rows1000` ×
  forward/reverse: 12 cases.
- **`M2skip` (`BenchmarkPlanSkip`):** Skip 3 × `rows10`, `rows1000` ×
  forward/reverse: 4 cases.
- Fixtures: 1,000 Orders records, `VALUE` index `price_idx`, no record
  versions. Store prefixes are 19 bytes (`M2rl`) and 22 bytes (`M2skip`).
- **Placement:** `f` is the fraction of fetched records whose storage-server
  team is disjoint from the index shard's team. `f = 1` means disjoint storage
  processes, not disjoint machines: with two replicas on three machines, any two
  teams share a machine.
  - Placement attempt 3 was selected by the preregistered rule (the first
    attempt where every cell passes, otherwise attempt 3).
  - Small and planskip stores: `f = 1` on every cell.
  - Split store: `f = 1` on every cell except `rows1000`, which is partially
    colocated at `f = 0.820`.
- Data distribution stayed off for all of M2. Placement receipts taken before
  and after each eligible case matched.

## Exclusions

Four of 16 cases are excluded whole, each by the pre-arm client CPU gate
(15% on any single benchmark CPU, 5% aggregate):

| Phase | Case | Observed |
|---|---|---|
| `M2rl` | split-limit1-reverse | 15.2% max per CPU |
| `M2rl` | split-rows10-forward | 15.8% max per CPU |
| `M2rl` | split-rows1000-forward | 15.2% max per CPU |
| `M2skip` | skip3-rows10-forward | 23.2% max per CPU, 6.78% aggregate |

All twelve other cases have their complete six-block population and a clean R4
telemetry reconciliation. No case was rerun.

## Results

Geometric mean of six paired block ratios (median of three `testing.B` repeats
per process, 3 s benchtime), with nominal 95% block-bootstrap intervals
(20,000 resamples, preregistered seed 492817). Below 1 means
`USE_REMOTE_FETCH` is faster or allocates less. The metric is mean `ns/op`, not
a latency percentile.

### `M2rl`: `BenchmarkIndexFetch` (`USE_REMOTE_FETCH / SCAN_AND_FETCH`)

| Case | Placement | Mean ns/op | Go B/op | Go allocs/op |
|---|---|---:|---:|---:|
| small-limit1-forward | cross-server (f = 1.000) | 0.976 [0.967, 0.986] | 0.977 [0.976, 0.978] | 1.080 [1.080, 1.080] |
| small-limit1-reverse | cross-server (f = 1.000) | 0.974 [0.970, 0.978] | 0.976 [0.975, 0.977] | 1.090 [1.090, 1.090] |
| small-rows10-forward | cross-server (f = 1.000) | 1.018 [1.007, 1.029] | 0.788 [0.787, 0.788] | 0.637 [0.636, 0.637] |
| small-rows10-reverse | cross-server (f = 1.000) | 0.995 [0.984, 1.007] | 0.783 [0.782, 0.784] | 0.636 [0.636, 0.637] |
| small-rows1000-forward | cross-server (f = 1.000) | 1.213 [1.171, 1.260] | 0.565 [0.564, 0.566] | 0.481 [0.481, 0.481] |
| small-rows1000-reverse | cross-server (f = 1.000) | 1.213 [1.184, 1.242] | 0.559 [0.559, 0.560] | 0.481 [0.481, 0.481] |
| split-limit1-forward | cross-server (f = 1.000) | 1.009 [0.992, 1.030] | 0.716 [0.715, 0.716] | 0.986 [0.983, 0.988] |
| split-limit1-reverse | cross-server (f = 1.000) | N/A (pre-arm client CPU gate) | — | — |
| split-rows10-forward | cross-server (f = 1.000) | N/A (pre-arm client CPU gate) | — | — |
| split-rows10-reverse | cross-server (f = 1.000) | 1.326 [1.292, 1.365] | 0.698 [0.698, 0.699] | 0.547 [0.547, 0.548] |
| split-rows1000-forward | partially colocated (f = 0.820) | N/A (pre-arm client CPU gate) | — | — |
| split-rows1000-reverse | partially colocated (f = 0.820) | 2.304 [2.247, 2.360] | 0.693 [0.693, 0.693] | 0.454 [0.454, 0.454] |

### `M2skip`: `BenchmarkPlanSkip` (`USE_REMOTE_FETCH / SCAN_AND_FETCH`)

| Case | Placement | Mean ns/op | Go B/op | Go allocs/op |
|---|---|---:|---:|---:|
| skip3-rows10-forward | cross-server (f = 1.000) | N/A (pre-arm client CPU gate) | — | — |
| skip3-rows10-reverse | cross-server (f = 1.000) | 1.169 [1.151, 1.185] | 0.899 [0.899, 0.899] | 0.841 [0.841, 0.842] |
| skip3-rows1000-forward | cross-server (f = 1.000) | 1.220 [1.198, 1.243] | 0.634 [0.634, 0.635] | 0.583 [0.583, 0.584] |
| skip3-rows1000-reverse | cross-server (f = 1.000) | 1.152 [1.110, 1.195] | 0.629 [0.628, 0.630] | 0.583 [0.583, 0.583] |

All `rows/op` and `prefix-B` ratios in both phases are 1.000 [1.000, 1.000];
they are audit quantities, not results.

- Every 1,000-row scan is slower with `USE_REMOTE_FETCH`: small rows1000 by
  about 21% in both directions, Skip rows1000 by 15–22%, and the partially
  colocated split rows1000 reverse at 2.304.
- On small rows1000, `USE_REMOTE_FETCH` allocates about 44% fewer Go bytes and
  52% fewer Go objects per operation.
- One-row small fetches are 2–3% faster with `USE_REMOTE_FETCH` (0.976 and
  0.974).

### Absolute values

Medians of each process's three `testing.B` means, per arm.

| Phase | Case | Ordinary ms | Remote ms | Ordinary B/op | Remote B/op | Ordinary allocs/op | Remote allocs/op |
|---|---|---:|---:|---:|---:|---:|---:|
| M2rl | small-limit1-forward | 3.165 | 3.087 | 15,980 | 15,618 | 187 | 202 |
| M2rl | small-limit1-reverse | 3.196 | 3.106 | 16,054 | 15,655 | 188 | 205 |
| M2rl | small-rows10-forward | 3.654 | 3.718 | 87,286 | 68,748 | 606 | 386 |
| M2rl | small-rows10-reverse | 3.737 | 3.702 | 87,758 | 68,706 | 607 | 386 |
| M2rl | small-rows1000-forward | 102.749 | 123.646 | 4,207,936 | 2,377,137 | 46,972 | 22,590 |
| M2rl | small-rows1000-reverse | 101.429 | 122.551 | 4,257,716 | 2,380,672 | 46,970 | 22,582 |
| M2rl | split-limit1-forward | 4.428 | 4.448 | 472,976 | 338,430 | 221 | 218 |
| M2rl | split-rows10-reverse | 7.983 | 10.625 | 4,645,208 | 3,244,086 | 912 | 500 |
| M2rl | split-rows1000-reverse | 359.495 | 831.216 | 456,772,766 | 316,346,125 | 67,952 | 30,850 |
| M2skip | skip3-rows10-reverse | 4.498 | 5.259 | 140,950 | 126,702 | 1,054 | 887 |
| M2skip | skip3-rows1000-forward | 103.323 | 126.712 | 5,033,941 | 3,192,446 | 60,608 | 35,362 |
| M2skip | skip3-rows1000-reverse | 109.655 | 126.707 | 5,082,649 | 3,196,061 | 60,608 | 35,353 |

## Comparison with the 2026-10-14 campaign

The 2026-10-14 M2 ran both methods on old Go (`267a41434`); this campaign runs
both on master. No old-versus-master M2 arm was measured in either campaign,
so the table compares two cohorts and is descriptive only: no blocks are
pooled and no cross-campaign interval is computed.

| Case | f, 2026-10-14 → 2026-10-20 | Remote/ordinary ns/op, 2026-10-14 | Remote/ordinary ns/op, 2026-10-20 |
|---|---|---:|---:|
| M2rl/small-limit1-forward | 1.000 → 1.000 | 0.979 [0.964, 0.995] | 0.976 [0.967, 0.986] |
| M2rl/small-limit1-reverse | 1.000 → 1.000 | 0.985 [0.969, 1.003] | 0.974 [0.970, 0.978] |
| M2rl/small-rows10-forward | 1.000 → 1.000 | 1.031 [1.018, 1.042] | 1.018 [1.007, 1.029] |
| M2rl/small-rows10-reverse | 1.000 → 1.000 | 1.026 [1.013, 1.039] | 0.995 [0.984, 1.007] |
| M2rl/small-rows1000-forward | 1.000 → 1.000 | 1.226 [1.175, 1.280] | 1.213 [1.171, 1.260] |
| M2rl/small-rows1000-reverse | 1.000 → 1.000 | 1.255 [1.217, 1.293] | 1.213 [1.184, 1.242] |
| M2rl/split-limit1-forward | 1.000 → 1.000 | 1.110 [1.087, 1.131] | 1.009 [0.992, 1.030] |
| M2rl/split-rows10-reverse | 0.000 → 1.000 | 0.854 [0.823, 0.886] | 1.326 [1.292, 1.365] |
| M2rl/split-rows1000-reverse | 0.696 → 0.820 | 2.300 [2.206, 2.384] | 2.304 [2.247, 2.360] |
| M2skip/skip3-rows10-reverse | 1.000 → 1.000 | 0.158 [0.155, 0.160] | 1.169 [1.151, 1.185] |
| M2skip/skip3-rows1000-forward | 1.000 → 1.000 | 1.360 [1.295, 1.414] | 1.220 [1.198, 1.243] |
| M2skip/skip3-rows1000-reverse | 1.000 → 1.000 | 1.324 [1.250, 1.407] | 1.152 [1.110, 1.195] |

- **Placement changed for two cases.** `split-rows10-reverse` moved from
  `f = 0` to `f = 1` and `split-rows1000-reverse` from `f = 0.696` to
  `f = 0.820` (seven record shards then, six now). Their ratios are not a
  comparison of code.
- **Reverse Skip 10 rows** is labelled cross-server in both cohorts, but the
  physical teams differ: index and record teams both occupied the node1/node2
  pair on 2026-10-14, and span node1/node3 and node2/node3 now. Absolute
  ordinary/remote times were 31.127 ms / 4.873 ms then and 4.498 ms / 5.259 ms
  now.
- **Matched properties:** the same frozen fixture schema, key schema and store
  prefixes; the same 12-case and 4-case geometry, method orders, repeats and
  benchtime; `GOMAXPROCS=8` on CPUs 8–15. Key counts match (2,000 small,
  3,000 split and 2,000 planskip keys); serialized value bytes were not
  compared.
- **Different:** fresh hosts, freshly seeded data, different shard and replica
  layouts in all three stores, and the production revision (old Go then, master
  now).
