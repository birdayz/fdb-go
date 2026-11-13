# Historical 10,000-span diagnostic provenance

These are the original local diagnostic receipts from 2026-10-27, not a
measurement of current HEAD or current master. Their source identity is exactly
[`source-id`](source-id):

```text
uncommitted:6080c7e9bb05a8aaaa840d59c39e480e3511d1cbd6c2f8166d6fce870f853939
```

This digest identifies an uncommitted input population, **not a recoverable Git
commit**. `source.before` and `source.after` are identical per-file SHA-256
manifests of the listed source inputs, not copies of those files or a full source
archive. The measured dirty tree cannot be reconstructed from these hashes
alone, and no later commit is asserted to contain exactly that tree.
`binary.sha256` retains the executable hash and its historical Bazel output path;
it is not an executable archive. Its SHA-256 is
`81781867ee711ac895e6e0e277c9a6bf3b3227f349d0aebe829f925e0e3fc6e3`.

## Available configuration and sample population

All 16 compressed JSON receipts report the source identity above and compiler
version `go1.27.3`. This is the compiler version recorded by the executable;
these receipts do not retain a resolved compiler artifact or complete build
command/flags. The adjacent [`environment.txt`](../environment.txt) records the
historical Linux/x86-64 host, filesystem, database image digests and container
limits (4 CPUs and 4 GiB per database). It is an environment snapshot, not proof
of isolated host load throughout the diagnostic.

There are four arms, with one ingest, one audit, one selective-query pass and
one trace-query pass retained per arm:

| File prefix | Backend | Stored order |
|---|---|---|
| `fdb_time` | FDB Record Layer | time |
| `fdb_trace` | FDB Record Layer | trace |
| `ch_map` | ClickHouse Map | trace |
| `ch_attrs_time` | ClickHouse dedicated attribute table | time |

Each arm uses 10,000 immutable unique synthetic spans, seed 283, one tenant,
eight attributes, match-every 10, hash-bytes 32 and K=100. FDB uses batches of 8;
ClickHouse config records batches of 8,192 rows / 8 MiB. Query budgets are
2,000,000 candidates and 256 MiB. The receipts retain run names, corpus
fingerprints, timestamps, elapsed times, configuration, audit details and every
timed query sample. Empty `*.optimize.log` files do not independently prove
which optimization commands ran.

Each `*.query.json.gz` has 100 samples: 20 each of `rare`, `custom_varying`,
`lookalike_type`, `absent` and `id_varying`. Each `*.trace.json.gz` has 100 trace
samples. Thus the retained timed population is 800 query samples across four
arms, not repeated independent ingest trials. All four ingest/audit pairs report
10,000 rows and all 16 receipts report zero failures; this describes those
receipts, not validation of later source changes.

## Interpretation boundary

The recorded boundary is immutable unique synthetic spans, covering string/int
equality and full-trace lookup in a local closed-loop diagnostic. These receipts
can describe the historical runs and their observed results, bytes and timings.
They do not establish sustainable ingest capacity, concurrent-load query tails,
HA/durability equivalence, production SLOs, comparative production cost or a
controlled before/after source-code improvement. The unrecoverable dirty source,
incomplete compiler/build provenance, single ingest per arm and shared-host
conditions limit reproduction and attribution. Do not relabel these receipts as
a clean-tree comparison or extrapolate them to the later V2 workloads.

The original identities, hashes, compressed receipts and logs are unchanged;
this README only explains their provenance and limitations.
