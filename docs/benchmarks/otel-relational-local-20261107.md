# OTel spans in relational OTEL_V1 stores: local comparison

Date 2026-11-07. Purpose: test whether the OTel trace data can live in the
relational layer (one OTEL_V1 schema per tenant, the layout that makes
traces-to-logs-to-metrics SQL joins possible) without giving up the current
custom-index backend's ingest and query performance.

[Round 2](#round-2-hill-climb-of-the-relational-path) supersedes round 1's
ingest, storage and query figures and compares against the production `fdb`
configuration. Round 1 is kept below as the starting point.

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

- Data can live in OTEL_V1 with direct Record Layer ingest: same KV count,
  fewer bytes. Round 1 measured 1.32x client CPU against the bench-default
  `fdb` configuration. After round 2 the relational store costs less than the
  production `fdb` configuration: 283 vs 353 µs/span with `zlib-1`, and
  42% fewer bytes.
- Hot queries can keep their performance on the relational store. Record
  Layer scans of the standard OTEL_V1 indexes are faster than the custom
  posting index. SQL covers ad hoc queries and joins over the same rows.
- Round 1's SQL floor of about 1 ms (a catalog transaction, then a page
  transaction, plus parsing) is now one transaction per auto-commit SELECT
  (round 2): 0.63 ms on a pinned connection and 1.04 ms pooled.
- Joins (trace to logs today, metrics later) plan as index nested loops with one
  sequential probe per outer row (`FlatMap` + `IndexScan(LOGS_TRACE, [=, =])`,
  pinned in `pkg/relational/sqltest/otel`). Pipelined probes and the metrics
  tables (template v2: exemplar trace_id fan-out and (service, time) indexes)
  are listed in `TODO.md`, "OTel relational prototype findings".

## Round 2: hill climb of the relational path

Same host, cluster and fixture, 50k spans per ingest run. The earlier `fdb`
figures used otelbench's defaults (`json` posting values, 32-byte hashes, no
record compression). The production exporter uses `tuple`, 8-byte hashes,
`brotli-4` and bulk index conflicts (`ExporterConfig` defaults), so that
configuration is the baseline below.

### Ingest and storage

Client CPU per span is the mean of two runs. Storage is per span: logical KV
bytes, and a prefix-compressed proxy for Redwood's key prefix compression
(`prefix_bytes` in the audit: each key less its common prefix with the
previous key, plus the value). Both backends write 11 KV pairs per span.

| Configuration | µs/span | logical B | prefix B |
|---|---:|---:|---:|
| `fdb` production (tuple, hash 8, brotli-4) | 353 | 2,001 | 1,501 |
| `fdb` bench default (json, hash 32, none) | 250 | 2,947 | 2,098 |
| `sql`, compiled rows, no compression | 258 | 1,932 | 1,328 |
| `sql`, compiled rows, `zlib-1` | 283 | 1,482 | 878 |
| `sql`, compiled rows, `zlib-default` | 298 | 1,458 | 854 |
| `sql`, dynamic rows, no compression | 307 | 1,932 | 1,328 |

The relational store with `zlib-1` costs 20% less client CPU than the
production `fdb` configuration and takes 42% fewer prefix-compressed bytes.

**Compression knob.** `--record-compression` now applies to `--backend sql`.
Only Java-readable deflate is allowed there: `zlib-1` and `zlib-default`
(`Deflater.DEFAULT_COMPRESSION`, which SQL `INSERT` writes). SQL connections
cannot read Brotli, a Go-only format version, so it is refused. Direct writes
use the selected serializer. SQL reads every format through the relational
default serializer, which reads uncompressed and zlib records. Compression
applies to records only, never to index entries. `zlib-1` keeps about 95% of
`zlib-default`'s record savings (534 vs 510 B per record) at less than half its
CPU (deflate at the default level was 15% of ingest CPU).

### Queries

p50 in ms, tenant 0 of the 50k-span `zlib-1` run, suite `all`, 1,200
iterations, 0 oracle failures in every column.

| class | `fdb` production | `sql` pooled | `sql` pinned conn | `sql` direct |
|---|---:|---:|---:|---:|
| span | 0.68 | 1.04 | 0.63 | 0.43 |
| span_absent | 0.67 | 0.96 | 0.61 | 0.42 |
| trace | 0.77 | 1.37 | 0.96 | 0.53 |
| time_narrow | 0.72 | 1.15 | 0.77 | 0.46 |
| time_broad | 0.97 | 1.77 | 1.28 | 0.70 |
| genai_type | 0.67 | 1.05 | 0.77 | 0.43 |
| genai_response | 0.66 | 1.17 | 0.81 | 0.44 |
| genai_model | 1.20 | 2.22 | 1.75 | 0.93 |
| genai_provider | 1.26 | 2.35 | 1.76 | 0.92 |

- **Pooled:** a pooled `*sql.DB` connection, as most applications use the driver. Its p50 went from 1.72 to 1.04 ms for a point lookup and from 3.86 to 2.22 ms for a broad attribute query, against round 1.
- **Pinned:** one `sql.Conn` for the whole run (`--sql-pinned-conn`). It keeps its session schema cache because `database/sql` never resets it.
- **Direct:** Record Layer scans of the same indexes in one transaction.

FDB requests per point query, from the client counters
(`OTELBENCH_SQL_RPCS=1`):

| path | GRVs | range reads | point reads |
|---|---:|---:|---:|
| pinned | 1 | 2 | 1 |
| direct | 1 | 2 | 1 |
| pooled | 1 | 5 | 2 |

Pinned SQL makes the same requests as the direct scan. The pooled path's
extra three range reads and one point read are the schema witness check (see
below), and they overlap the store's own reads. The remaining gap is client
CPU: parse, plan, executor and driver row conversion.

### Changes in round 2

1. **One transaction per auto-commit SELECT** (`embedded/statement_read.go`).
   - The catalog read, the planner's index-state store open and the first page now share one FDB transaction, and the page reuses the planner's store. Before, these were three transactions with three GRVs.
   - The first page commits the shared transaction before its rows are published. Later pages run in their own transactions, as before.
   - A retryable failure reruns the work in a fresh `DB.Run` transaction.
   - A successful statement without a page, such as EXPLAIN, commits the shared transaction. A failed or panicking statement cancels it.
   - Explicit transactions are unchanged.
2. **Schema witness** (`catalog/schema_witness.go`).
   - `ResetSession` drops a pooled connection's schema cache at every checkout. The connection now keeps the raw catalog bytes the schema was built from: the catalog store header and the schema and template rows.
   - The next statement re-reads those keys in one parallel round trip and reuses the parsed schema only if every byte matches. This also catches changes by Java writers and restores.
   - The statement's own store header and index-state reads are prefetched alongside the witness reads (`recordlayer.PrefetchStoreState`).
3. **Top-K reads in growing batches** (`sqlbackend`).
   - A statement's page is read whole before its first row is returned. The backend therefore issues `LIMIT K` first, then doubles the batch with an inclusive `start_ns <=` keyset bound.
   - EXPLAIN shows this stays an index range: `IndexScan(SPANS_STR_ATTR, [=, =, =, <>, ...])`.
4. **Compiled row types** (`layers/otel/otelsql/otelv1pb`).
   - Go types are generated from the records descriptor the OTEL_V1 DDL compiles to (`cmd/genotelv1`). They are bound to the catalog's metadata with `WithLocalFileDescriptor` (`OpenTenantStoreCompiled`), which validates them as an evolution of the stored descriptor.
   - `BuildCompiled` is byte-for-byte `SpanRows.Build` (test), so SQL and Java read the same records.
   - This removed dynamicpb from ingest (about 21% of CPU).
5. **Fan-out index key evaluation** (`recordlayer/key_expression.go`, `index_maintainer.go`).
   - Each child is evaluated once, and fan-out children skip the scalar and flat attempts that failed with an allocated error. The cross product is built once.
   - `evaluateIndex` no longer tries the flat path on fan-out roots, which first failed and then evaluated everything again.
   - An equivalence test checks the result against a reference cross product.

## Round 3: the Exporter's read path

The Exporter replacement (`layers/otel/rfcs/relational-exporter-migration.md`)
reads through `otel.SpanReader`, and `--sql-query direct` now uses it too, so the
direct column measures production code. Same run and suite as round 2,
1,200–1,600 iterations, 0 oracle failures in every run.

| class | `fdb` production | `sql` pooled | `sql` pinned | direct (round 2) | `SpanReader` | `SpanReader`, cached read version |
|---|---:|---:|---:|---:|---:|---:|
| span | 0.68 | 0.82 | 0.59 | 0.43 | 0.39–0.41 | 0.29–0.32 |
| trace | 0.77 | 1.16 | 0.94 | 0.53 | 0.48–0.49 | 0.38–0.41 |
| time_broad | 0.97 | 1.31 | 1.08 | 0.70 | 0.63–0.64 | 0.53–0.56 |
| genai_model | 1.20 | 1.73 | 1.49 | 0.93 | 0.81–0.84 | 0.71–0.75 |

- **SQL columns:** round 2 plus a per-connection parse cache (a repeated
  statement skips the ANTLR parse) and a memo of the executor's per-row record
  type comparison (`executor/record_type_equal_memo.go`).
- **Cached read version:** the last two columns are an interleaved A/B (three runs
  per arm, ranges shown). A read-only read takes its version from the client's GRV
  cache, at most 100 ms old and refreshed by this client's commits, so the next
  read still sees a process's own writes (FDB test `span reader`). It makes 0 GRVs
  per query instead of 1.
- **Remaining round trips:** a point query is now two: the store header and index
  states (overlapped), then the index scan.

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
`OTELBENCH_SQL_RPCS=1` prints FDB requests per query at exit.

Further `--backend sql` flags:

- `--record-compression none|zlib-1|zlib-default`
- `--sql-rows compiled|dynamic`
- `--sql-pinned-conn`
- `--sql-page-rows N`

Production `fdb` configuration: `--posting-value-format tuple --hash-bytes 8 --record-compression brotli-4 --bulk-index-conflicts`.
Regenerate the compiled types after an OTEL_V1 change with
`bazelisk run //layers/otel/otelsql/cmd/genotelv1 -- "$PWD/layers/otel/otelsql/otelv1pb"`.
