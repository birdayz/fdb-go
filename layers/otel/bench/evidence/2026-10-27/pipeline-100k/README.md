# Pipelined 100,000-span diagnostic receipts

Source revision: `12c293130597477035e32b372b3c4101dbd9c2fb`.
Environment and pinned database images: [`../environment.txt`](../environment.txt).

The 16 compressed JSON files are unmodified ingest, audit and per-round query
outputs from the local synthetic campaign. Each receipt includes the workload
configuration, corpus fingerprint, revision, elapsed time and failure count.
Query receipts retain every timed sample, including the first-round tail spikes.
`binary.sha256` records the executable checksum; `source-manifests.sha256` records
identical before/after source-manifest digests. The full source manifests remain
in the local runtime directory; these digest receipts are not copies of them.

- 100,000 immutable unique spans, one tenant, eight indexed attributes, seed 283,
  match every 100, newest 100 distinct traces.
- FDB time-ordered custom index, `SaveRecordsPipelined`, batch 32.
- ClickHouse dedicated scoped/typed attribute table with time order, independent
  bulk batches of 8,192 rows / 8 MiB; `OPTIMIZE TABLE ... FINAL` on `spans` and
  `attr_lookup` before the audit/query passes. No granularity sweep was performed.
- Three rounds, each with 1,000 selective queries (200 per class) and 1,000 trace
  lookups per backend. Backend order was FDB/ClickHouse, ClickHouse/FDB, then
  FDB/ClickHouse; selective queries preceded trace lookups within each arm.
- Both source audits passed; all 12,000 timed queries passed the independent
  oracle. FDB ingestion took 194.455 s versus ClickHouse's 4.442 s. FDB rare-query
  p99 was 42.448 / 1.800 / 1.774 ms across rounds; ClickHouse's was
  9.233 / 9.787 / 7.847 ms.

This is a single-node, closed-loop end-to-end diagnostic using native FDB Go and
ClickHouse HTTP paths, not isolated client cost, sustainable ingest capacity,
controlled cold-cache measurement, an HA result or production SLO evidence.
Neither ingestion cost nor the first-round tail delay has been localized to a
Go client or Record Layer defect. See the open findings in the root `TODO.md`.
