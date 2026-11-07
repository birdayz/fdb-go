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

### Collector-bench baseline of the custom store (gates G2 and G3)

The replacement's message, log and advanced-query gates compare against the custom
store. These are its receipts at `27e0b1b60`+ (old implementation unchanged), with
`collector-bench` defaults except `--conversations 1000` (15,000 spans, 285,000
message references), two runs per arm, on the same Redwood cluster, 0 failures and
0 fidelity mismatches. Raw evidence: `otel-relational-local-20261107/collector-bench-baseline-*.json`.

| arm | ingest s | spans/s | process CPU s (user+sys) | message persist s |
|---|---:|---:|---:|---:|
| deltas + FTS (default) | 9.52 / 10.25 | 1,576 / 1,464 | 111.4 / 112.8 | 19.9 / 21.2 |
| messages off | 4.55 / 5.08 | 3,299 / 2,952 | 41.1 / 40.1 | — |

| query class, p50 / p99 ms | deltas + FTS | messages off |
|---|---|---|
| same_span_conjunction | 1.63 / 2.13, 1.62 / 2.91 | 1.68 / 2.53, 1.68 / 2.69 |
| trace_existential_3way | 25.94 / 33.00, 26.81 / 31.11 | 3.97 / 5.99, 4.18 / 6.24 |
| trace_graph_traversal | 1.74 / 3.71, 2.00 / 3.17 | 4.30 / 36.63, 4.62 / 7.58 |
| correlated_logs_by_trace | 0.54 / 0.64, 0.57 / 0.95 | 0.53 / 1.15, 0.45 / 0.67 |
| fts_single_token | 28.65 / 84.14, 30.15 / 94.43 | — |
| fts_same_span_predicates | 27.80 / 34.65, 27.86 / 32.88 | — |
| fts_multi_turn_history | 7.91 / 88.85, 9.35 / 83.71 | — |
| fts_phrase_first_turn | 8.70 / 90.38, 8.56 / 77.09 | — |

Process CPU covers the whole command (corpus, ingest, audit, queries). With messages
on, the export's logical bytes are 13.1 MB against 372 MB with messages off
(dedup stored 20.9 MB of deltas for 181.6 MB of message bytes), and the TEXT index
wrote 60.2 MB of keys. The host ran other builds (load 10–11), so treat single
differences under ~10% as noise.

### Gate G2A: spans through the replacement's write and read paths

These runs measure the replacement's production code: `--backend sql` direct
ingest of compiled rows with `--save-mode batch` writes through
`otel.SpanWriter` (`write_path` in the ingest receipt), and `--sql-query direct`
reads through `otel.SpanReader`. The binary was built at `afe1e7dac`, which
has the final template. The cluster is the same host's Redwood `otelcmp`,
recreated this afternoon, and every run below is fresh. Runs started only
when the 1-minute load was under 15; the load at the start and end of each run
is shown. Other agents' builds ran throughout. Raw evidence is in
`otel-relational-local-20261107/g2a-*`; the query receipts there have their
per-sample arrays removed.

**Ingest.** 50k spans, the round 2 fixture and flags. `fdb` production uses
`--posting-value-format tuple --hash-bytes 8 --record-compression brotli-4 --bulk-index-conflicts`.
`sql` uses `--record-compression zlib-1 --sql-ingest direct --sql-rows compiled`.
The arms alternated.

| run | µs/span | wall s | load start/end |
|---|---:|---:|---|
| `fdb` 1 | 374 | 7.4 | 14.0 / 13.7 |
| `sql` 1 | 280 | 10.1 | 11.4 / 11.0 |
| `fdb` 2 | 342 | 8.7 | 11.4 / 10.9 |
| `sql` 2 | 277 | 10.2 | 10.3 / 11.0 |
| `fdb` 3 | 390 | 20.0 | 13.7 / 19.2 |
| `sql` 3 | 288 | 11.8 | 13.5 / 13.1 |
| `fdb` 4 | 380 | 15.7 | 13.6 / 16.6 |
| `sql` 4 | 286 | 7.8 | 13.4 / 12.7 |
| **mean** | `fdb` 372, `sql` 283 (0.76×) | | |

| per span, every run | logical B | prefix B | records B | keys |
|---|---:|---:|---:|---:|
| `fdb` production | 2,001 | 1,501 | 460 | 11 |
| `sql` zlib-1 (SpanWriter) | 1,472 | 878 | 533 | 11 |

- **CPU:** the gate is ≤ 353 µs/span, ideally ≤ 1.05 × 283 = 297. `sql` averaged 283 µs/span and its worst run was 288, so it passes both bounds.
- **Bytes:** the gate is ≤ `fdb`. `sql` stores 26% fewer logical bytes and 42% fewer prefix bytes, so it passes.
- **Baseline drift:** this session's `fdb` production mean (372) is 5% above round 2's 353; `sql` matches round 2's 283.
- **Bulk index conflicts:** otelbench refuses `--bulk-index-conflicts` outside `--backend fdb`, so the `sql` arm runs SpanWriter without coalesced index conflicts. The Exporter's default (coalescing on) is measured by the benchmark below. Since phase 3A, otelbench also accepts it with `--backend sql --save-mode batch`.

**Queries.** Tenant 0 of `fdb` run 1 and `sql` run 1, suite `all`, 1,200
iterations (75 per class), two interleaved runs per arm, load 13.6–13.8. The
table shows p50 in ms for run a / run b. 0 oracle failures in every run,
including two earlier `sql` runs at load 10.6–13.7 that gave the same p50s
within 10%.

| class | `fdb` production | `sql` direct (`SpanReader`) | ratio |
|---|---:|---:|---:|
| span | 0.79 / 0.78 | 0.38 / 0.36 | 0.47 |
| span_absent | 0.84 / 0.80 | 0.38 / 0.35 | 0.44 |
| trace | 0.97 / 0.97 | 0.51 / 0.45 | 0.50 |
| trace_absent | 0.83 / 0.80 | 0.38 / 0.35 | 0.44 |
| time_narrow | 0.90 / 0.85 | 0.39 / 0.35 | 0.43 |
| time_broad | 1.21 / 1.13 | 0.68 / 0.61 | 0.55 |
| time_absent | 0.82 / 0.76 | 0.39 / 0.33 | 0.46 |
| time_empty | 0.68 / 0.65 | 0 (no read) | 0 |
| genai_response | 0.87 / 0.80 | 0.39 / 0.35 | 0.44 |
| genai_type | 0.82 / 0.83 | 0.35 / 0.33 | 0.42 |
| genai_scope_resource | 0.84 / 0.79 | 0.37 / 0.35 | 0.44 |
| genai_scope_scope | 0.83 / 0.79 | 0.36 / 0.35 | 0.44 |
| genai_absent | 0.81 / 0.80 | 0.36 / 0.34 | 0.43 |
| genai_operation | 1.42 / 1.42 | 0.93 / 0.82 | 0.61 |
| genai_provider | 1.52 / 1.38 | 0.95 / 0.84 | 0.62 |
| genai_model | 1.62 / 1.48 | 0.94 / 0.83 | 0.57 |

Every class's p50 is below `fdb` production, so the query half of G2A passes.
The median per-class p99 was 3.2 ms for `fdb` and 1.2–1.6 ms for `sql`.

**The Exporter itself.** `BenchmarkRelSpansVsFDB` (`layers/otel/spans_rel_bench_test.go`,
at `bddc4fc31`) exports the same fixture through `ExportResourceSpans` of both
exporters in one test binary, with messages off and `ExporterConfig`
defaults. The relational exporter uses `zlib-1`; the old Exporter uses brotli-4,
tuple postings, 8-byte hashes and bulk conflicts. Each run exports 20,000
single-span envelopes of one tenant, in calls of 500 spans. CPU is the test
process's user+sys over the export calls, after a forced GC. The cluster is
the benchmark's own FDB testcontainer, and the three alternating runs per arm
ran at load 11.3 at the start and 10.0 at the end.

| arm | µs/span (3 runs) | mean | wall s | KV B/span | prefix B/span |
|---|---|---:|---|---:|---:|
| old Exporter | 469.7, 467.2, 471.1 | 469 | 2.16–2.77 | 2,003 | 1,496 |
| relational exporter | 247.1, 255.2, 254.7 | 252 | 1.71–2.34 | 1,483 | 872 |

The relational exporter costs 0.54× the old Exporter's client CPU per span. The
old Exporter costs more per span than otelbench's `fdb` backend (469 against
372). That gap was not profiled.

### Messages through the replacement (R1, R4)

`BenchmarkMessageWriteCostRealFDB` (`layers/otel/messages_rel_cost_test.go`, at
`897715044`, before the cutover removed its old-Exporter arms) exported GenAI
conversations with deltas and full text through both exporters in one test
binary, `record_compression none`, on its own FDB testcontainer. CPU is the
process's user+sys over the export calls; the ranges cover every run.

| workload (conversations × turns) | old Exporter CPU ms/span | relational Exporter CPU ms/span |
|---|---|---|
| 40 × 25 | 7.5–8.4 | 7.3–8.0 |
| 5 × 200 | 9.0–11.4 | 9.8–11.5 |

- **R1, references in index keys:** `SPANS_REF_CHUNKS` holds 2–3% more bytes than the old `envelope_refs_v1` values.
- **MESSAGES_TEXT:** 21–23% more bytes than the old TEXT index, with the same bunch count (6,017 and 10,143 rows). The schema subspace prefix is 23 bytes against 10 (26% of the difference, and all of the 40% more written key bytes, 13 B per write); every entry's primary key carries the record type key (about 1.9 B per posting, the rest). The whole store is 1.04–1.06×. The owner accepted this at the cutover and kept D4.
- **R4, message writes in the shared store:** `MessageStoreStats.Retries` was 0 in every run.

### Gate G3: collector bench after the cutover

`collector-bench` through the cutover's `otel.Exporter` (`f68acec52`; its defaults:
`zlib-1`, bulk index conflicts) against the custom store at `7fb269417`
(`--backend fdb`; its defaults: brotli-4, tuple postings, 8-byte hashes, time
order), with the archived baseline's flags: `--conversations 1000`, with defaults
(deltas + FTS) and with `--enable-message-deltas=false --enable-message-fts=false`.
otelcmp's catalog holds an OTEL_V1 template from another revision, which the
Exporter refuses (`TemplateMismatchError`). So every run here is on a fresh
container, `otelg3p3a`, with otelcmp's image, environment and configuration
(single, ssd-redwood-1). The arms alternated, and their order flipped each round.
The 1-minute load was 5–15 at every start. Each run in the tables started after
the storage and log queues had drained below 1 MB. Process CPU is user+sys of the
whole command (corpus, ingest, audit, queries). Raw evidence is
`otel-relational-local-20261107/collector-bench-g3-*.json`; the run log
`g3-collector-bench-runs.txt` has the binaries' revisions and sha256, loads and CPU.

The tables cover five runs per arm with deltas + FTS (median, with the range) and
two with messages off (a / b). Every run had 0 failures (collector, export and
query), 0 fidelity mismatches over 32 checked spans, and 0 message retries.

| arm | ingest s | spans/s | process CPU s | message persist s | logical span MB |
|---|---:|---:|---:|---:|---:|
| custom store, deltas + FTS | 10.86 (10.39–11.45) | 1,381 (1,310–1,443) | 113.2 (111.1–115.8) | 21.8 (20.9–23.0) | 13.1 |
| Exporter, deltas + FTS | 9.76 (9.61–10.09) | 1,538 (1,486–1,561) | 101.0 (99.8–102.1) | 30.2 (29.9–31.7) | 9.8 |
| custom store, messages off | 4.91 / 4.98 | 3,054 / 3,013 | 40.8 / 41.9 | — | 372.4 |
| Exporter, messages off | 3.59 / 3.95 | 4,179 / 3,799 | 29.5 / 30.2 | — | 190.2 |

Logical span bytes are each store's own `logical_bytes` count. For the Exporter
that is the span's OTLP; for the custom store it is the canonical OTLP plus its
fidelity lists. It is not a storage measurement.

| query class, p50 / p99 ms | custom, FTS (median) | Exporter, FTS (median) | p50 ratio | custom, off | Exporter, off |
|---|---|---|---:|---|---|
| same_span_conjunction | 1.91 / 2.74 | 1.22 / 1.90 | 0.64 | 1.78 / 2.59, 1.83 / 3.01 | 1.19 / 1.84, 1.20 / 1.72 |
| trace_existential_3way | 29.11 / 37.57 | 26.90 / 33.11 | 0.92 | 4.61 / 6.57, 4.56 / 7.20 | 3.01 / 5.40, 3.11 / 5.53 |
| trace_graph_traversal | 2.03 / 3.82 | 1.24 / 1.98 | 0.61 | 4.66 / 6.19, 5.47 / 130.52 | 3.86 / 6.06, 3.56 / 5.26 |
| correlated_logs_by_trace | 0.60 / 1.08 | 0.56 / 0.96 | 0.93 | 0.56 / 0.96, 0.55 / 50.81 | 0.55 / 1.01, 0.52 / 0.91 |
| fts_single_token | 38.73 / 164.41 | 59.32 / 135.36 | **1.53** | — | — |
| fts_same_span_predicates | 30.17 / 36.52 | 29.29 / 34.96 | 0.97 | — | — |
| fts_multi_turn_history | 8.51 / 12.45 | 8.39 / 12.97 | 0.99 | — | — |
| fts_phrase_first_turn | 9.17 / 164.53 | 9.11 / 135.06 | 0.99 | — | — |

- **Ingest:** with messages, the Exporter takes 0.90× the wall time, reaches 1.11× the spans/s and uses 0.89× the process CPU. With messages off, the figures are 0.76× wall and 0.72× CPU.
- **Message persist s is 1.39×.** This is the summed per-call wait for message group commits. The message transactions themselves take the same time: their summed time is 63.6 s against 68.1 s, there are 302–304 of them against 304–308, and none retried. Spans finish sooner, so each call spends a larger share of its time waiting for its messages. MESSAGES_TEXT wrote 83.9 MB of keys against 60.2 MB (+39%, the R1 prefix and record-type-key cost). By the letter of the G2C criterion (persist ≤ 1.10×) this fails; end-to-end ingest passes.
- **fts_single_token p50 is 1.53×**, and its p99 is 0.82×. Its p50 is bimodal in both stores: each run is either about 27–39 ms or about 44–65 ms. On the fresh cluster's first runs (`g3new_fts_1`–`4` against `g3base_fts_1`–`2`, not quiesced), the Exporter's median p50 was 0.72× the custom store's. Later, with more data on the cluster, the Exporter landed in the slow mode in every run and the custom store in 2 of 5. The cause is not established. Here this class fails the 1.10× threshold.
- **The p99s of 100 samples** include 100–200 ms stalls in both stores (`fts_phrase_first_turn`, `fts_multi_turn_history`), so single-run p99 differences are noise.
- **Interference between runs:** some Exporter messages-off runs started right after the custom store's 372 MB messages-off ingest, without waiting for the queues (`g3new_off_3`–`6`). They show 40–110 ms p99s and 1.3–1.8× wall time; the quiesced runs do not. They are archived but left out of the tables.
- **Cluster drift:** this container's custom-store runs ingest about 10% slower than the archived otelcmp baseline (median 10.86 s against 9.52 / 10.25 s).

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

G2A's Exporter benchmark runs on its own FDB testcontainer:

```sh
bazelisk test //layers/otel:otel_test --test_arg=-test.run=^$ \
  --test_arg=-test.bench=BenchmarkRelSpansVsFDB --test_arg=-test.benchtime=1x \
  --test_output=streamed --nocache_test_results
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
