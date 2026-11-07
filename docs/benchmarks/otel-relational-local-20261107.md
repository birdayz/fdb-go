# OTel spans in relational OTEL_V1 stores: local comparison

Date 2026-11-07. Purpose: test whether the OTel trace data can live in the
relational layer (one OTEL_V1 schema per tenant, the layout that makes
traces-to-logs-to-metrics SQL joins possible) without giving up the current
custom-index backend's ingest and query performance.

**Boundary.** One 24-core development host; one single-process FoundationDB
7.3.77 container (`fdb dev up`, storage migrated to `ssd-redwood-1`); client
and server share the host. Each figure is from one run, or two where noted.
Wall time on this server swung by 5x between identical runs for both backends
(17 s to 103 s for the same 100k-span ingest), so ingest is compared on client
CPU (user+sys of the bench process), which was stable to about 5%. This is
local diagnostic evidence, not a multi-host or production measurement.

## Setup

`otelbench` fixture V2: seed 283, 8 attributes, `--match-every 100`,
`--order time`, 10 tenants, K = 100. Ingest uses 8 workers, batches of 32 spans
and `--save-mode batch` for every backend. Every query result is checked
against the bench's independent oracle; all runs below report 0 failures.

| Backend | Storage | Queries |
|---|---|---|
| `fdb` (current) | one Record Layer store per run, `typed_scoped_eq_v2` custom covering posting index, `span_id_v2`, `span_time_v2` | custom index scanner, one transaction |
| `sql --sql-query sql` | one OTEL_V1 tenant schema per tenant (`layers/otel/otelsql`); records written directly with Record Layer `SaveRecordBatch` (`--sql-ingest direct`) | SQL through `database/sql` |
| `sql --sql-query direct` | the same stores | Record Layer scans of the same OTEL_V1 indexes the SQL plans use, one transaction |

The SQL plans (EXPLAIN) are already the ideal ones: attribute top-K is
`UnorderedPrimaryKeyDistinct(IndexScan(SPANS_STR_ATTR, [=, =, =, *, *, *] COVERING) REVERSE)`,
time is a covering reverse `SPANS_TIME` scan, span id a covering
`SPANS_SPAN_ID` scan, and a trace a primary-key scan.

## Ingest

Client CPU per span (user+sys / spans), 100k spans unless noted:

| Path | µs/span | vs `fdb` |
|---|---:|---:|
| `fdb` (two runs: 249, 260) | 255 | 1.00 |
| `sql`, direct saves, before the record-layer fixes below (two runs) | 370 | 1.45 |
| `sql`, direct saves, after (two runs: 334, 341) | 337 | 1.32 |
| `sql`, SQL `INSERT` (10k spans) | 1,664 | 6.5 |

At 10k spans both direct paths ran at the same wall rate (1.5 s vs 1.6 s),
where the server was not yet the bottleneck.

Storage after 30k spans (logical KV bytes):

| | keys | bytes |
|---|---:|---:|
| `fdb` total | 330,002 | 88.4 MB |
| records | 30,000 | 17.4 MB |
| posting index | 240,000 | 66.6 MB |
| span id + time | 60,000 | 4.4 MB |
| `sql` total | 330,010 | 58.0 MB |
| SPANS records | 30,000 | 29.5 MB |
| SPANS_STR_ATTR + SPANS_INT_ATTR | 240,000 | 24.9 MB |
| SPANS_SPAN_ID + SPANS_TIME | 60,000 | 3.6 MB |

Both write 11 KV pairs per span. The relational rows are larger (typed columns
and attribute arrays beside the lossless `otlp` envelope), while the standard
covering VALUE indexes carry no posting payload, so the total is 34% smaller.

The remaining CPU gap is mostly the wide row as a `dynamicpb` message: Range,
Get and slow-path Marshal are about 22% of ingest CPU, and GC is higher
(3.1 s vs 2.1 s of 9.6 s vs 7.3 s per 30k spans). SQL `INSERT` adds per-row
parsing and planning and is not a viable ingest path.

## Queries

Tenant 0 (10k spans), suite `all`, 600 iterations (about 37 per class). p50 in
ms; `sql` is after the fixes below, with `--sql-page-rows 256`.

| class | `fdb` | `sql` | `sql` direct |
|---|---:|---:|---:|
| genai_model (broad attribute) | 1.53 | 3.86 | 1.00 |
| genai_operation | 1.52 | 3.63 | 0.94 |
| genai_provider | 1.57 | 3.70 | 1.06 |
| genai_response (selective) | 0.71 | 1.86 | 0.45 |
| genai_type | 0.68 | 1.77 | 0.46 |
| genai_scope_resource | 0.73 | 1.79 | 0.45 |
| genai_scope_scope | 0.69 | 1.67 | 0.45 |
| genai_absent | 0.70 | 1.79 | 0.44 |
| span | 0.69 | 1.72 | 0.48 |
| span_absent | 0.68 | 1.65 | 0.48 |
| trace | 0.78 | 1.99 | 0.54 |
| trace_absent | 0.69 | 1.93 | 0.45 |
| time_broad | 1.00 | 3.25 | 0.76 |
| time_narrow | 0.77 | 1.88 | 0.48 |
| time_absent | 0.70 | 1.69 | 0.44 |
| time_empty | 0.56 | 1.61 | 0 (no read) |

The `fdb` backend also reads its ownership manifest on every query, which the
direct path does not; part of the direct path's lead is that read.

SQL before the fixes, same data: genai_model 91.3 ms, time_broad 59.5 ms,
selective classes about 3.1 ms. Progression of the SQL path:

| Change | genai_model p50 | span p50 |
|---|---:|---:|
| as found | 91.3 | 3.04 |
| page bound `execution_scanned_rows_limit=256` | 6.65 | 2.95 |
| shared template cache, layout and salt memos | 3.86 | 1.72 |

## Fixes in this change

Found by profiling the comparison; each is general, not OTel-specific.

1. **Template deserialization per statement** (`pkg/relational/core/catalog/template_cache.go`).
   `database/sql` calls `ResetSession` at every pool checkout, which drops the
   connection's schema cache, so every statement re-read the catalog and rebuilt
   `RecordMetaData` from the template proto: 32% of point-query CPU on OTEL_V1.
   Read paths now share deserialized templates process-wide, keyed by name,
   version and the SHA-256 of the row's META_DATA bytes. The row is still read
   in the statement's transaction, so DDL is observed as before.
2. **Per-row deep type comparison** (`ordinal_layout_binding.go`). Attaching the
   output layout compared each row's record type structurally with the
   carrier (26% of broad-query CPU on the 30-column SPANS type). The cursor now
   remembers the row type object it has validated; `RecordType.Equals` itself
   is unchanged (pinned to Java's always-descend behaviour).
3. **Scan salt per execution** (`scan_range_execution_identity.go`). An index
   scan's continuation salt encoded the plan's whole flowed type on every
   execution of a cached plan (37% of a point query). It is now memoised per
   immutable plan object, held weakly and dropped when the plan is collected.
4. **UTF-8 validation** (`pkg/recordlayer/utf8_strings.go`, `store_batch.go`).
   The string check built a field path string for every field and ran twice per
   record in `SaveRecordBatch`. Paths are now built only for an error, and the
   batch serializes without repeating the check its preflight made (11% to 5%
   of ingest CPU).

## What this means for traces, logs and metrics in SQL

- Data can live in OTEL_V1 today with direct Record Layer ingest: same KV count,
  fewer bytes, 1.32x client CPU. The CPU gap is the dynamicpb row, not the
  index design. Compiled row types are the next step.
- Hot queries can keep their performance on the relational store. Record
  Layer scans of the standard OTEL_V1 indexes are faster than the custom
  posting index. SQL covers ad hoc queries and joins over the same rows.
- Plain SQL still has a fixed per-statement floor of about 1 ms here: a catalog
  transaction, then a page transaction, plus parsing.
- Joins (trace to logs today, metrics later) plan as index nested loops with one
  sequential probe per outer row (`FlatMap` + `IndexScan(LOGS_TRACE, [=, =])`,
  pinned in `pkg/relational/sqltest/otel`). Pipelined probes and the metrics
  tables (template v2: exemplar trace_id fan-out and (service, time) indexes)
  are listed in `TODO.md`, "OTel relational prototype findings".

## Reproduce

```sh
CF=$(bazelisk run //cmd/fdb -- dev up --name otelcmp --port 4510)
bazelisk build //layers/otel/cmd/otelbench
B=bazel-bin/layers/otel/cmd/otelbench/otelbench_/otelbench
F="--seed 283 --count 100000 --tenants 10 --attrs 8 --match-every 100 --order time --cluster-file $CF"
$B --backend fdb --mode ingest $F --workers 8 --batch 32 --save-mode batch --run fdb_r1 --output fdb-ingest.json
$B --backend sql --mode ingest $F --workers 8 --batch 32 --save-mode batch --run sql_r1 --output sql-ingest.json
$B --backend sql --mode audit $F --run sql_r1 --output sql-audit.json
$B --backend sql --mode query $F --suite all --iterations 600 --run sql_r1 --output sql-q.json
$B --backend sql --sql-query direct --mode query $F --suite all --iterations 600 --run sql_r1 --output sqld-q.json
```

`OTELBENCH_SQL_EXPLAIN=1` prints each SQL query's plan once to stderr.
