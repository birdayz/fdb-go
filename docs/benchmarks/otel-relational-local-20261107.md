# OTel spans in relational OTEL_V1 stores: local comparison

Date 2026-11-07. Purpose: test whether the OTel trace data can live in the
relational layer (one OTEL_V1 schema per tenant, the layout that makes
traces-to-logs-to-metrics SQL joins possible) without giving up the current
custom-index backend's ingest and query performance.

[Round 2](#round-2-hill-climb-of-the-relational-path) supersedes round 1's
ingest, storage and query figures and compares against the production `fdb`
configuration. Round 1 is kept below as the starting point. The relational
store has since replaced the custom one; gates G0 to G3 below are the
replacement's receipts.

**Boundary.** One 24-core development host; one single-process FoundationDB
7.3.77 container (`fdb dev up`, storage migrated to `ssd-redwood-1`); client
and server share the host. Each figure is from one run, or two where noted.
Wall time on this server swung by 5x between identical runs for both backends
(17 s to 103 s for the same 100k-span ingest), so ingest is compared on client
CPU (user+sys of the bench process), which was stable to about 5%. This is
local diagnostic evidence, not a multi-host or production measurement.

**Measured trees.** The gates name their trees by these archive tags. Most
trees come from the stack before it was rebased onto the change's merge-base;
`g3-head` and `g2b-head` are the change's own commits. None of them but the
merge-base is in the merged history; the tags keep them resolvable.
Rounds 1 to 3 predate the tags and recorded no tree; they are history, not
reproducible receipts.

| tag | commit | measured as |
|---|---|---|
| `archive/otel-relational/pre-0a` | `a1f333131` | the stack before phase 0A: the custom store's collector-bench baseline; G0's base is its parent, which lacks only this docs commit |
| `archive/otel-relational/g0-template` | `a6095a7b3` | G0: phase 0A's OTEL_V1 rewrite |
| `archive/otel-relational/g2a` | `bddc4fc31` | G2A: `BenchmarkRelSpansVsFDB`, and otelbench at its parent, which lacks only that benchmark |
| `archive/otel-relational/g2b-logs` | `15af5fd66` | G2B: `BenchmarkRelLogsVsFDB` |
| `archive/otel-relational/messages-cost` | `42446273a` | R1/R4: `BenchmarkMessageWriteCostRealFDB` |
| `archive/otel-relational/custom-store` | `7fb269417` | the custom store (`--backend fdb`) in G3, the `fts_single_token` runs and the quiesced gate; the cutover's parent |
| `archive/otel-relational/g3-cutover` | `f68acec52` | G3: the cutover's Exporter |
| `archive/otel-relational/fts-before` | `b70072f38` | `fts_single_token`: the Exporter with the query-only harness |
| `archive/otel-relational/fts-fix` | `f7b2d43bd` | `fts_single_token`: the fix |
| `archive/otel-relational/quiesced` | `11ae9acbf` | the quiesced gate's Exporter |
| `archive/otel-relational/merge-base` | `839cc5d3f` | the change's merge-base: the custom store (`--backend fdb`) in "G3 at the merge-base", with `g3-merge-base/base-harness-839cc5d3f.patch`, and its `Exporter.ExportLogs` in "Gate G2B", with `g2b-merge-base/base-bench-839cc5d3f.patch` |
| `archive/otel-relational/g3-head` | `8d3959aa6` | the measured head: the Exporter and `--backend sql` in "G3 at the merge-base"; that section lists the later commits' code changes, none on a measured path |
| `archive/otel-relational/g2b-head` | `dc6a2f835` | a later head of the change: `BenchmarkLogExport` against the merge-base in "Gate G2B" |

The archived tags carry the export benchmarks' old names,
`BenchmarkRelSpansVsFDB` and `BenchmarkRelLogsVsFDB`; at head they are
`BenchmarkSpanExport` and `BenchmarkLogExport`, which measure the same
relational arm.

The receipts record these short hashes, or one of:

- `7fb269417+harness`: custom-store plus `fts-perf/base-harness.patch`;
- `dfbccb991+harness`: fts-before's parent with fts-before's harness change, built before that commit; `fts-perf/runs.txt` names this binary (sha256 `58073eab…`) fts-before;
- `fix3`: the fts-fix binary (sha256 `3543936d…`, reproduced by a rebuild at fts-fix);
- `unrecorded`: the G0 and G2A otelbench receipts, whose trees their sections name.

**Baseline.** CLAUDE.md and the RFC's gates name the merge-base as the
baseline. "G3 at the merge-base" below measures that comparison: the
measured head (`g3-head`) against the custom store at the merge-base, for
the collector bench's quiesced gate and for otelbench's G2A suite. It is the
G3 result. "Gate G2B" measures log ingest against the merge-base the same
way, at a later head of the change (`g2b-head`).
The earlier gates' custom-store arms are not the merge-base. Each is the
custom store on the same stack as the relational arm it is compared with:
G2A, G2B's first comparison and R1/R4 ran both stores in one binary, and the first G3 run, the
`fts_single_token` runs and the quiesced gate use custom-store, the cutover's
parent (pre-0a is the same stack before phase 0A). Both arms of those
comparisons share one Record Layer, client and relational tree, so their
ratios compare the two stores, not those libraries' changes since the
merge-base. G0 compares the relational store before and after phase 0A.
Rounds 1 to 3 are history with no recorded tree; G0's and G2A's thresholds
(283 and 353 µs/span) come from round 2's figures.

## Setup

`otelbench` fixture V2: seed 283, 8 attributes, `--match-every 100`,
`--order time`, 10 tenants, K = 100. Ingest uses 8 workers, batches of 32 spans
and `--save-mode batch` for every backend. Every query result is checked
against the bench's independent oracle; all runs below report 0 failures.

| Backend | Storage | Queries |
|---|---|---|
| `fdb` (the custom store, archived at `archive/otel-relational/custom-store`) | one Record Layer store per run, `typed_scoped_eq_v2` custom covering posting index, `span_id_v2`, `span_time_v2` | custom index scanner, one transaction |
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
  read still sees this database handle's (this Exporter's) own writes (FDB test
  `span reader`). It makes 0 GRVs
  per query instead of 1.
- **Remaining round trips:** a point query is now two: the store header and index
  states (overlapped), then the index scan.

### Gate G0: span ingest after the OTEL_V1 rewrite

Phase 0A rewrote OTEL_V1 in place. G0 requires `otelbench --backend sql` ingest
of 50k spans with `zlib-1` to stay within 1.05× round 2 (283 µs/span, 1,482
B/span), so at most 297 µs/span and 1,556 B/span. Head is
`archive/otel-relational/g0-template` (`a6095a7b3`); the same-session control
is the tree before phase 0A, the parent of `archive/otel-relational/pre-0a`
(`c191f9c17`). Five alternating runs per arm used the round 2 fixture and
flags (direct ingest of compiled rows, 8 workers, batches of 32, batch saves)
on the recreated `otelcmp` container, at a 1-minute load of 6.6–8.3 (recorded
before eight of the ten runs). CPU is user+sys of the ingest command. Raw
evidence: `otel-relational-local-20261107/g0/` (`runs.txt`, the receipts and
`run.sh`).

| arm | µs/span, 5 runs | median | mean | logical B | prefix B | keys |
|---|---|---:|---:|---:|---:|---:|
| head | 268, 257, 325, 268, 311 | 268 | 286 | 1,472 | 878 | 11 |
| base | 256, 251, 268, 257, 262 | 257 | 259 | 1,472 | 878 | 11 |

- **CPU:** the head's median (268) and mean (286) are within 297; two of its runs (325 and 311) are not, and they also took 2–4× the wall time of the others (17.6 s and 31.9 s). Head over base is 1.04× by median and 1.10× by mean.
- **Bytes:** both arms store 1,472 logical and 878 prefix bytes per span, within the gate.
- **Earlier sets** (in `runs.txt`, not gated): in the first, the head failed on `otelcmp`'s OTEL_V1 from another revision. The next two ran the head's parent, which put SPANS at record type key 1 (888 prefix B/span, fixed by g0-template), at a load of 29–103.

### Collector-bench baseline of the custom store (gates G2 and G3)

The replacement's message, log and advanced-query gates compare against the custom
store. These are its receipts at `archive/otel-relational/pre-0a` (`a1f333131`), with
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
reads through `otel.SpanReader`. The binary was built at the parent of
`archive/otel-relational/g2a` (`bddc4fc31`), which has the final template; the
tag's commit adds only the benchmark below. The cluster is the same host's
Redwood `otelcmp`, recreated this afternoon, and every run below is fresh. Runs started only
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
at `archive/otel-relational/g2a` (`bddc4fc31`)) exports the same fixture
through `ExportResourceSpans` of both exporters in one test binary, with messages off and `ExporterConfig`
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

### Gate G2B: logs through the replacement

`BenchmarkRelLogsVsFDB` (`layers/otel/logs_rel_bench_test.go`) exports 20,000
logs in calls of 500 through the custom store's `Exporter.ExportLogs` and the
relational log store, in one test binary on its own FDB testcontainer, then
reads 400 traces with `QueryLogsByTrace`. CPU is the test process's user+sys
over the export calls. Two sets of two alternating runs per arm: set 2 at
`archive/otel-relational/g2b-logs` (`15af5fd66`, clean tree), set 1 at its
parent (`6918c3314`, the benchmark's commit), at a 1-minute load of 14–24.
Raw evidence: `otel-relational-local-20261107/g2b-bench-rellogs.txt`, the
benchmark's output lines as recovered from the stream's session log.

| per log | custom store (4 runs) | relational (4 runs) | ratio |
|---|---|---|---:|
| client CPU µs | 854.0, 826.6, 811.0, 815.4 (mean 827) | 347.7, 321.8, 362.6, 316.9 (mean 337) | 0.41 |
| logical KV bytes | 2,351.5 | 2,244.6 | 0.95 |
| prefix bytes | 1,821.4 | 1,499.2 | 0.82 |
| keys | 11.8 | 13.0 | |
| `QueryLogsByTrace` p50 ms | 0.698, 0.577, 0.852, 0.597 | 0.686, 0.587, 0.862, 0.579 | 0.97–1.02 |

- **CPU** per run pair is 0.39–0.45× and bytes and p50 are at most 1.02×, so this comparison meets every G2B criterion (each ≤ 1.10×).
- **p99** of 400 reads: 2.3–8.0 ms for the custom store and 1.5–5.5 ms for the relational store.
- **Baseline:** both stores in one binary at `g2b-logs`, a same-stack comparison, not the merge-base. The merge-base comparison follows.
- **Scope:** the RFC's G2B row names collector-bench's logs; this benchmark exports logs alone. The collector bench's `correlated_logs_by_trace` class is in G3 (0.93× post-ingest) and the quiesced gate (0.89× and 0.98×).

**Against the merge-base.** The same workload and metrics, with head's
`BenchmarkLogExport` at `archive/otel-relational/g2b-head` (`dc6a2f835`, a
later head of the change) against the custom store's `Exporter.ExportLogs` at
`archive/otel-relational/merge-base` (`839cc5d3f`).

- **Merge-base arm:** `BenchmarkRelLogsVsFDB`'s custom-store arm, ported to the merge-base as `BenchmarkLogExportCustomStore` by `g2b-merge-base/base-bench-839cc5d3f.patch`. The patch adds one test file and its BUILD `srcs` line and changes no store code. The corpus, cluster and run helpers are copied unchanged from `g2b-logs`. The size scan is `g2b-logs`'s `FDBBackend.size` with the prefix count in the test, because the merge-base's `Size` has none.
- **Runs:** `bazelisk build //layers/otel:otel_test` in detached worktrees, then four `bazelisk test` invocations in the order merge-base, head, head, merge-base. Each invocation started its own testcontainer and made two fresh runs, so each arm has four runs.
- **Discipline:** every invocation started once the 1-minute load was below 12 (5.3–10.6 at the starts) and held the machine's two test-lane locks. Every invocation executed, not from cache, and passed.
- **Raw evidence:** `otel-relational-local-20261107/g2b-merge-base/`: `runs.txt` (trees, test-binary sha256, loads and the benchmark's output), the patch and the scripts.

| per log | merge-base (4 runs) | head (4 runs) | ratio |
|---|---|---|---:|
| client CPU µs | 785.2, 785.8, 783.8, 781.2 (mean 784) | 296.4, 298.0, 304.8, 296.9 (mean 299) | 0.38 |
| logical KV bytes | 2,351.5 | 2,244.5 | 0.95 |
| prefix bytes | 1,821.4 | 1,499.1 | 0.82 |
| keys | 11.8 | 13.0 | |
| `QueryLogsByTrace` p50 ms | 0.482, 0.476, 0.484, 0.481 | 0.463, 0.467, 0.453, 0.454 | 0.96 |
| `QueryLogsByTrace` p99 ms | 1.949, 5.869, 1.261, 6.484 | 0.710, 0.683, 0.642, 0.868 | 0.19 |

Ratios compare the means.

- **Verdict:** CPU per log is 0.38× (0.38–0.39× for any pair of runs), logical KV bytes 0.95×, prefix bytes 0.82× and p50 0.96× (0.94–0.98× per pair). G2B's ingest half passes against the merge-base, each criterion ≤ 1.10×. With `correlated_logs_by_trace` in "G3 at the merge-base" (0.93× and 0.95×), both halves of G2B pass against the merge-base, for this benchmark's logs (see Scope).
- **p99:** the merge-base's first run in each invocation had a p99 of 1.3–1.9 ms and its second 5.9–6.5 ms; the head's were 0.64–0.87 ms.
- **Wall** over the export calls: 5.66–5.84 s at the merge-base, 2.03–2.07 s at head.
- **Against the same-stack runs:** the merge-base's custom store wrote the same bytes and keys per log as at `g2b-logs`, and head's relational store 0.1 B less. Both arms' CPU per log is lower than in the same-stack runs (784 against 827, 299 against 337), which ran at a load of 14–24.

### Messages through the replacement (R1, R4)

`BenchmarkMessageWriteCostRealFDB` (`layers/otel/messages_rel_cost_test.go`, at
`archive/otel-relational/messages-cost` (`42446273a`), before the cutover removed
its old-Exporter arms) exported GenAI conversations with deltas and full text
through both exporters in one test binary, `record_compression none`, on its
own FDB testcontainer, two runs per arm (`-test.count=2`), 1,000 spans per
workload, at a 1-minute load of 11–19. CPU is the process's user+sys over the
export calls. Raw evidence: `otel-relational-local-20261107/messages-cost-runs.txt`,
the benchmark's output lines as recovered from the stream's session log.

| workload (conversations × turns) | old Exporter CPU ms/span | relational Exporter CPU ms/span | ratio of means |
|---|---|---|---:|
| 40 × 25 | 7.58, 7.49 | 7.56, 7.70 | 1.01 |
| 5 × 200 | 9.51, 9.00 | 9.82, 10.23 | 1.08 |

Two earlier sets of two runs per arm, on earlier trees of the stream that are
not archived (same file), gave 7.14–8.40 (old) and 7.29–7.99 (relational) at
40 × 25, and 9.96–11.45 and 10.46–11.46 at 5 × 200. The byte ratios below
are the same in all three sets.

- **R1, references in index keys:** `SPANS_REF_CHUNKS` holds 2–3% more bytes than the old `envelope_refs_v1` values.
- **MESSAGES_TEXT:** 21–23% more bytes than the old TEXT index, with the same bunch count (6,017 and 10,143 rows). The schema subspace prefix is 23 bytes against 10 (26% of the difference, and all of the 40% more written key bytes, 13 B per write); every entry's primary key carries the record type key (about 1.9 B per posting, the rest). The whole store is 1.04–1.06×. The owner accepted this at the cutover and kept D4.
- **R4, message writes in the shared store:** `MessageStoreStats.Retries` was 0 in every run.

### Gate G3: collector bench after the cutover

**Method (owner decision, after the analysis below).** G3's query classes
are gated quiesced: each ingested run is queried with `collector-bench
--query-only` after the storage and log queues drain below 1 MB. The gated
results are in "Quiesced query gate" below. This subsection is the first G3
run at `archive/otel-relational/g3-cutover` (`f68acec52`), whose query
numbers are **post-ingest**: measured while
the storage server was still draining the ingest. They are kept as
history, not as the gate.

`collector-bench` through the cutover's `otel.Exporter` (`archive/otel-relational/g3-cutover`
(`f68acec52`); its defaults: `zlib-1`, bulk index conflicts) against the custom
store at `archive/otel-relational/custom-store` (`7fb269417`, the cutover's parent;
`--backend fdb`; its defaults: brotli-4, tuple postings, 8-byte hashes, time
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

| post-ingest query class, p50 / p99 ms | custom, FTS (median) | Exporter, FTS (median) | p50 ratio | custom, off | Exporter, off |
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
- **Message persist s is 1.39×.** This is the summed per-call wait for message group commits. The message transactions themselves take the same time: their summed time is 63.6 s against 68.1 s, there are 302–304 of them against 304–308, and none retried. Spans finish sooner, so each call spends a larger share of its time waiting for its messages. MESSAGES_TEXT wrote 83.9 MB of keys against 60.2 MB (+39%, the R1 prefix and record-type-key cost). By the letter of the G2C criterion (persist ≤ 1.10×) this fails; end-to-end ingest passes. The owner accepted it: G2C's persist criterion becomes the summed message transaction time, here 0.93× (63.6 s against 68.1 s).
- **fts_single_token p50 is 1.53×**, and its p99 is 0.82×. Its p50 is bimodal in both stores: each run is either about 27–39 ms or about 44–65 ms. On the fresh cluster's first runs (`g3new_fts_1`–`4` against `g3base_fts_1`–`2`, not quiesced), the Exporter's median p50 was 0.72× the custom store's. Later, with more data on the cluster, the Exporter landed in the slow mode in every run and the custom store in 2 of 5. Here this class fails the 1.10× threshold. The next subsection explains the cause and fixes the query.
- **The p99s of 100 samples** include 100–200 ms stalls in both stores (`fts_phrase_first_turn`, `fts_multi_turn_history`), so single-run p99 differences are noise.
- **Interference between runs:** some Exporter messages-off runs started right after the custom store's 372 MB messages-off ingest, without waiting for the queues (`g3new_off_3`–`6`). They show 40–110 ms p99s and 1.3–1.8× wall time; the quiesced runs do not. They are archived but left out of the tables.
- **Cluster drift:** this container's custom-store runs ingest about 10% slower than the archived otelcmp baseline (median 10.86 s against 9.52 / 10.25 s).

### fts_single_token: the G3 gap explained and fixed

These runs are on a fresh container, `otelfts`, with G3's image, environment and
configuration. Three binaries ran, alternating:

- **custom:** `archive/otel-relational/custom-store` (`7fb269417`) with `--backend fdb`, plus a harness patch.
- **before:** `archive/otel-relational/fts-before` (`b70072f38`), the Exporter with the harness only.
- **fix:** `archive/otel-relational/fts-fix` (`f7b2d43bd`), the before tree plus the two commits below.

The harness is `collector-bench --query-only`, `--query-classes` and `--query-samples`
(commit "collector: query-only and sampled post-ingest bench queries"). It
reruns one class on a stored corpus and reports each
iteration's latency, its charged rows and bytes, and the FDB client's range
requests. The custom binary carries the same harness as a patch
(`otel-relational-local-20261107/fts-perf/base-harness.patch`). Every run
started at a 1-minute load under 15. Quiesced runs also started after the
storage and log queues had drained below 1 MB. Raw evidence is in
`otel-relational-local-20261107/fts-perf/`: receipts with per-iteration
samples, per-second server samples (`*.mon`) and `runs.txt`.

**The query.** `incident_token_<block>` with role `user` matches the 750 user
messages of one block's 50 conversations. Their 6,000 references are spread
over 50 traces, and the class keeps the newest 100. Before the fix, one query
did the following, in both stores:

- **Requests:** 818 range reads. 750 of them read the matches' MESSAGE_TRACES markers, one per message.
- **Decoding:** it decoded all of the 50 traces' references, about 14,000, with `tuple.Unpack`.
- **Allocation:** 17.7 MB (the custom store: 17.9 MB).
- **Client CPU:** 82 ms (the custom store: 86 ms), on about 3 cores for 27 ms.

**Why the p50 was bimodal.** `fts_single_token` is the first post-ingest class.

- When the ingest ends, the storage server still holds 400–500 MB of mutations that are not yet durable. Draining them takes 6–10 s, and every read waits behind that work (`post-c2*.mon`).
- Per-iteration samples taken straight after each store's own ingest show the drain. The table below gives the medians of 25-iteration buckets in ms.
- A 100-iteration p50 is slow exactly when more than half of the iterations run inside the drain.
- The custom store's queries slow down more during the drain: 55–115 ms bucket medians in `c2base1`, against 42–76 ms in the before runs. So fewer of its 100 iterations fall inside the drain, and its p50 is slow less often. That is why the Exporter was in the slow mode in every G3 run and the custom store in 2 of 5.
- The cluster's accumulated data is not the cause. Quiesced p50s on a fresh cluster and on one holding about 20 earlier runs are the same (26.3 against 26.2–26.3 ms), and the server read at most 41 B per query from disk.

| post-ingest run | iterations 1–400, 25-iteration medians, ms |
|---|---|
| custom `c2base1` | 32 100 115 55 28 26 25 26 … 26 |
| custom `c2base2` | 28 31 33 37 27 26 26 26 … 26 |
| before `c2before1` | 39 64 54 64 76 68 53 24 … 24 |
| before `c2before2` | 34 43 46 42 49 30 24 25 … 24 |
| fix `c2fix1` | 13 11 10 11 10 10 11 12 … 9 |
| fix `c2fix2` | 18 10 15 9 10 11 17 11 … 8 |

**The fix** has two parts, both in `messages_query_rel.go`.

- **Fewer allocations** (commit "otel: message queries read reference chunks in place"). `otelsql.RefChunkReader` walks a ref_chunks value without allocating and decodes ordinals only for kept references. `unpackKey` reads the marker and SPANS_REF_CHUNKS keys in place. Split references are joined per span. Both fall back to `tuple.Unpack` outside the canonical encoding, and tests and a fuzz target pin that they agree with it.
- **Fewer requests** (commit "otel: one marker read per run of a writer's matching messages"). A writer's matches at most 32 seqs apart share one MESSAGE_TRACES range read, capped at 4 rows per spanned seq. A read that reaches its cap falls back to one read per match it did not finish.

**Quiesced, `fts_single_token` only** (400 iterations per run, two runs per arm;
CPU and allocation from a CPU and heap profile of a further 400-iteration run):

| arm | p50 ms | p99 ms | range requests | rows | logical B | client CPU ms | allocated MB |
|---|---|---|---:|---:|---:|---:|---:|
| custom | 29.8 / 29.6 | 34.1 / 32.3 | 818 | 2,250 | 318,883 | 86 | 17.9 |
| before | 26.3 / 26.2 | 28.6 / 29.5 | 818 | 2,250 | 296,419 | 82 | 17.7 |
| fix | 9.4 / 8.9 | 23.9 / 10.3 | 79 | 3,010 | 346,656 | 21 | 6.4 |

The fix reads 34% more rows and 17% more bytes: the coalesced reads cover
the markers of the messages between matches. It issues a tenth of the
requests and uses a quarter of the client CPU.

**Quiesced, every class** (100 iterations each; the before and fix binaries
query the same stored runs), p50 / p99 ms, runs a and b:

| class | custom | before | fix | fix / custom p50 |
|---|---|---|---|---:|
| fts_single_token | 30.98 / 36.18, 30.70 / 35.47 | 27.24 / 32.67, 27.24 / 30.35 | 9.07 / 11.57, 9.17 / 12.63 | 0.30 |
| fts_same_span_predicates | 30.46 / 36.04, 31.08 / 36.07 | 29.09 / 32.74, 29.26 / 33.87 | 11.86 / 13.95, 11.95 / 14.93 | 0.39 |
| trace_existential_3way | 29.78 / 35.59, 29.58 / 33.36 | 26.66 / 30.36, 26.84 / 30.89 | 9.31 / 11.51, 9.62 / 12.03 | 0.32 |
| fts_phrase_first_turn | 8.45 / 10.39, 8.25 / 9.46 | 7.21 / 9.53, 7.26 / 9.61 | 6.79 / 9.13, 7.10 / 9.26 | 0.83 |
| fts_multi_turn_history | 8.31 / 11.00, 8.11 / 10.28 | 7.28 / 9.66, 7.42 / 9.85 | 7.10 / 9.28, 6.94 / 9.54 | 0.86 |
| same_span_conjunction | 1.68 / 2.58, 1.76 / 3.04 | 1.17 / 2.30, 1.17 / 2.14 | 1.11 / 2.14, 1.16 / 1.75 | 0.66 |
| trace_graph_traversal | 1.83 / 2.76, 1.83 / 2.90 | 1.26 / 2.36, 1.16 / 1.83 | 1.29 / 2.42, 1.18 / 2.44 | 0.67 |
| correlated_logs_by_trace | 0.67 / 0.87, 0.65 / 1.01 | 0.66 / 1.06, 0.52 / 0.60 | 0.60 / 0.86, 0.57 / 0.80 | 0.89 |

**G3's method** (default `collector-bench`, every class, queries straight after
ingest; four runs per arm across two sets), p50 / p99 ms:

| class | custom | fix |
|---|---|---|
| fts_single_token | 36.30 / 142.21, 28.27 / 136.80, 32.84 / 130.12, 37.12 / 136.39 | 8.86 / 51.22, 10.64 / 58.99, 15.10 / 77.15, 13.02 / 53.83 |
| fts_phrase_first_turn | 7.38 / 86.37, 7.70 / 115.76, 8.64 / 123.48, 8.40 / 60.66 | 7.43 / 115.49, 7.90 / 116.87, 9.18 / 131.60, 9.02 / 133.41 |
| fts_multi_turn_history | 7.21 / 12.81, 7.07 / 9.89, 8.03 / 10.78, 8.07 / 10.90 | 8.83 / 143.02, 8.80 / 127.82, 8.32 / 133.65, 8.73 / 122.18 |
| fts_same_span_predicates | 27.32 / 31.04, 27.07 / 32.41, 29.01 / 32.89, 27.84 / 36.11 | 11.08 / 93.91, 10.70 / 13.82, 12.00 / 15.51, 11.53 / 16.35 |
| trace_existential_3way | 24.80 / 31.11, 24.99 / 27.82, 26.95 / 31.52, 27.11 / 34.42 | 8.76 / 12.73, 8.29 / 11.92, 9.17 / 14.62, 8.95 / 13.77 |

In the same set as the first two of each, the before binary gave a
`fts_single_token` p50 of 43.75 and 40.30 ms: 1.2–1.4× the custom store, which
reproduces G3's gap. The other classes are as in G3.

- **fts_single_token** is now 0.24–0.46× the custom store at p50 and 0.36–0.59× at p99 under G3's method, and 0.30× quiesced.
- **The drain moves.** It now falls on the next classes, `fts_phrase_first_turn` and `fts_multi_turn_history`: their p50 is 1.01–1.25× the custom store's, and single 115–145 ms stalls set their p99. Quiesced, both are 0.83–0.86× the custom store at p50 and below it at p99.
- **Why those classes are exposed.** Without a role, each issues about 290 range requests: role discovery, then 16 shards for each role and token, for about 130 rows. That is the same exposure to the drain that `fts_single_token` had. The phrase fix in the next subsection removes it.
- **G3's method mixes two things.** Its post-ingest numbers combine query cost with the storage server's post-ingest durability backlog. Which classes absorb the backlog depends on how long the earlier classes take. Steady-state comparisons should run `--query-only` after the queues drain.
- **Ingest is unchanged.** The fix's process CPU is lower (79–81 s against 96–97 s for before and 105–108 s for the custom store) only because its queries are cheaper.

### Quiesced query gate

The G3 query gate under the owner's quiesced method, with the role-less
phrase fix (commit "otel: phrase and all queries scan only shards holding
their rarest term"). An "all" or phrase query of several terms now
counts each term's postings in shard 0 of a role, probes the rarest term in
the other shards and scans only the shards that hold it. The roles run
concurrently, and MESSAGES_LARGE is scanned once.

**Setup.**

- **Binaries:** the Exporter at `archive/otel-relational/quiesced` (`11ae9acbf`: the fts fix, the phrase fix and the `tuple.decodeInt` change), against the custom store at `archive/otel-relational/custom-store` (`7fb269417`) plus `fts-perf/base-harness.patch`, with `--backend fdb` and its own defaults (brotli-4, tuple postings, 8-byte hashes).
- **Container:** a fresh `otelg3q`, with G3's image, environment and configuration.
- **Runs:** `--conversations 1000`, with defaults (deltas + FTS) and with messages off. Two fresh runs per arm and configuration; the arms alternated and their order flipped in the second round.
- **Per run:**
  - The default `collector-bench` ingests, and its post-ingest queries are reported but not gated.
  - Once the storage and log queues are below 1 MB, `--query-only` runs every class 100 times on that run. Those are the gated numbers.
- **Load and outcomes:** the 1-minute load was 4.9–12.5 at every start. Every run had 0 failures and 0 fidelity mismatches.
- **Raw evidence:** `otel-relational-local-20261107/g3-quiesced/`, with per-iteration samples in the `qo-*` receipts.

**Quiesced (gated)**, p50 / p99 ms, runs 1 and 2. The ratio compares the
means of the two runs' p50s. "Requests" are the client range requests per
query, custom against Exporter.

| class | custom | Exporter | p50 ratio | requests |
|---|---|---|---:|---|
| fts_single_token | 31.15 / 35.18, 31.06 / 35.66 | 9.34 / 11.82, 8.83 / 10.55 | 0.29 | 818 / 79 |
| fts_phrase_first_turn | 8.36 / 9.71, 8.12 / 9.60 | 4.16 / 5.91, 4.24 / 6.27 | 0.51 | 227–240 / 73–74 |
| fts_multi_turn_history | 8.21 / 9.58, 7.98 / 11.42 | 3.95 / 6.22, 4.01 / 6.03 | 0.49 | 226–238 / 72 |
| fts_same_span_predicates | 30.45 / 34.45, 30.43 / 35.17 | 11.45 / 15.00, 11.05 / 13.00 | 0.37 | 1,042–1,044 / 314 |
| same_span_conjunction | 2.04 / 3.02, 1.83 / 2.95 | 1.13 / 2.21, 1.09 / 2.16 | 0.57 | 33 / 32 |
| trace_existential_3way | 29.58 / 31.97, 29.90 / 34.37 | 9.42 / 12.96, 9.54 / 12.05 | 0.32 | 828 / 87 |
| trace_graph_traversal | 1.81 / 3.01, 1.74 / 2.78 | 1.26 / 2.43, 1.29 / 2.16 | 0.72 | 5 / 4 |
| correlated_logs_by_trace | 0.62 / 1.00, 0.64 / 1.01 | 0.53 / 0.65, 0.60 / 0.87 | 0.89 | 2 / 3 |
| *messages off* | | | | |
| same_span_conjunction | 1.51 / 2.69, 1.61 / 2.40 | 1.04 / 2.01, 0.93 / 1.97 | 0.63 | 33 / 32 |
| trace_existential_3way | 4.02 / 5.29, 4.38 / 6.25 | 2.85 / 4.43, 2.70 / 4.35 | 0.66 | 12 / 10 |
| trace_graph_traversal | 4.57 / 5.73, 4.41 / 5.44 | 3.39 / 4.84, 3.75 / 5.35 | 0.79 | 7 / 6 |
| correlated_logs_by_trace | 0.57 / 0.82, 0.54 / 1.18 | 0.51 / 0.60, 0.57 / 0.87 | 0.98 | 2 / 3 |

Every class is within the 1.10× threshold at p50 (at most 0.98×), and every
p99 is below the custom store's.

- **Phrase classes:** they read 11 KB instead of 25–29 KB per query, and 158–162 rows instead of 128–153, the extra rows being the shard-0 term counts.

**Post-ingest (not gated)**, from the same runs' default phase, p50 / p99 ms:

| class | custom | Exporter | p50 ratio |
|---|---|---|---:|
| fts_single_token | 29.35 / 112.30, 42.98 / 239.11 | 9.65 / 45.30, 9.25 / 45.19 | 0.26 |
| fts_phrase_first_turn | 7.80 / 82.57, 6.96 / 11.96 | 4.03 / 5.38, 4.43 / 66.96 | 0.57 |
| fts_multi_turn_history | 7.63 / 70.92, 7.08 / 9.45 | 4.28 / 82.21, 4.39 / 81.72 | 0.59 |
| fts_same_span_predicates | 27.09 / 32.81, 27.47 / 33.01 | 11.32 / 81.92, 12.68 / 78.39 | 0.44 |
| same_span_conjunction | 1.62 / 4.49, 1.64 / 2.94 | 1.18 / 32.95, 1.20 / 2.79 | 0.73 |
| trace_existential_3way | 25.79 / 30.67, 26.60 / 31.41 | 9.05 / 77.15, 9.14 / 122.01 | 0.35 |
| trace_graph_traversal | 1.66 / 2.18, 1.76 / 2.45 | 1.07 / 2.06, 1.12 / 1.50 | 0.64 |
| correlated_logs_by_trace | 0.49 / 0.60, 0.54 / 0.79 | 0.46 / 0.68, 0.50 / 0.70 | 0.93 |
| *messages off* | | | |
| same_span_conjunction | 1.72 / 2.69, 1.66 / 2.47 | 1.14 / 2.34, 1.11 / 1.50 | 0.66 |
| trace_existential_3way | 4.15 / 6.85, 3.78 / 5.59 | 2.82 / 7.31, 2.79 / 5.18 | 0.71 |
| trace_graph_traversal | 4.49 / 6.19, 4.24 / 5.79 | 3.11 / 5.33, 3.53 / 6.00 | 0.76 |
| correlated_logs_by_trace | 0.49 / 0.69, 0.52 / 1.02 | 0.47 / 0.69, 0.53 / 0.74 | 0.99 |

Post-ingest p50s are all below the custom store's as well. Both stores' p99s
still include drain stalls: 33–122 ms for the Exporter and 71–239 ms for
the custom store.

**Ingest, the same runs.** Process CPU is user+sys of the default command.

| arm | ingest s | spans/s | process CPU s | persist s | message txn s |
|---|---|---|---|---|---|
| custom, deltas + FTS | 9.84 / 10.41 | 1,524 / 1,441 | 107.7 / 110.0 | 20.1 / 21.1 | 62.2 / 64.6 |
| Exporter, deltas + FTS | 8.84 / 8.80 | 1,696 / 1,704 | 79.2 / 78.1 | 27.6 / 27.3 | 60.5 / 59.6 |
| custom, messages off | 4.56 / 4.55 | 3,291 / 3,295 | 40.2 / 40.9 | — | — |
| Exporter, messages off | 3.40 / 3.55 | 4,413 / 4,224 | 29.1 / 29.0 | — | — |

- **With messages:** ingest wall is 0.87×, process CPU 0.72×, and the summed message transaction time (the G2C criterion) 0.95×.
- **With messages off:** wall is 0.76× and CPU 0.72×.
- **Process CPU** covers the queries too, which are now cheaper.

### G3 at the merge-base

G3 as the RFC defines it (§6): the collector bench against the merge-base,
with defaults and with messages off, and otelbench `sql --sql-query direct`
against `fdb` production, every class within the G2 thresholds. The sections
above compare against custom-store; this one is the G3 result.

**Setup.**

- **Trees:**
  - head: the measured head, `archive/otel-relational/g3-head` (`8d3959aa6`), the collector bench without `--backend` and otelbench with `--backend sql`;
  - baseline: the merge-base, `archive/otel-relational/merge-base` (`839cc5d3f`, #1034's head), with `--backend fdb` and its own defaults (brotli-4, tuple postings, 8-byte hashes, time order).
- **Harness at the merge-base:** `fts-perf/base-harness.patch` does not apply there. Its BUILD hunk's context lists files the merge-base's `layers/otel/BUILD.bazel` does not have yet. Every other hunk applied unchanged, and `exporter_clientmetrics.go` was added to the library's `srcs` by hand, after `exporter.go`. The applied change is `g3-merge-base/base-harness-839cc5d3f.patch`. It adds `--query-only`, `--query-classes`, `--query-samples` and per-iteration client counters, and changes no store code.
- **After the measured head:** later commits of the change (to `g2b-head` and after it) change library code that was not re-measured:
  - `SpanReader.Query` reads every `strconv.ParseFloat` spelling of NaN in a double attribute as the stored NaN, where it read only `NaN`; other text parses as before;
  - sqlbackend and otelsql return typed errors (`otel.NewError`, `otelsql.InvalidRowError`, `TemplateMismatchError`) on their failure paths;
  - `checkContinuationMAC` lost its domain and owner parameters and computes the same MAC; long-string span queries call `otelsql.LongStringDigest`, the same SHA-256 prefix;
  - otelbench refuses `--sql-*` flags without `--backend sql` and `--order` with it, and sqlbackend refuses `--bulk-index-conflicts` outside direct ingest of compiled rows;
  - after `g2b-head`, sqlbackend's ingest takes its span-write and batch-save functions from fields that tests replace (the defaults are the same functions), `parseCanonicalDouble` is renamed `parseAttrDouble`, the template DDL moved to `otelsql/internal/otelddl`, and `ExporterConfig`'s YAML decoding follows merge keys and aliases when it checks keys (configuration parsing, before any export);
  - none of these is on a measured path: every measured write and query succeeded, and no measured query has a double attribute;
  - the head's otelbench commands here passed `--order time`, which the sql backend ignored at `g3-head` and otelbench now refuses; to reproduce them at a later head, drop it.
- **Binaries:** `bazelisk build //layers/otel/cmd/otelbench` in detached worktrees, go1.27.3; their sha256 are in `runs.txt`.
- **Container:** a fresh `otelg3mb` (foundationdb/foundationdb:7.3.77, `configure new single ssd-redwood-1`, G3's environment), removed afterwards.
- **Discipline:** every measured command started once the 1-minute load was below 12 and the storage and log queues were below 1 MB on two polls. It held the machine's two test-lane locks, so no test lane ran beside it. The load at the starts was 2.1–6.6.
- **Collector bench**, as in "Quiesced query gate":
  - `--conversations 1000`, with defaults (deltas + FTS) and with messages off;
  - two fresh runs per arm and configuration, the arms alternating, the order flipped in round 2;
  - each run is a default `collector-bench` (post-ingest queries, reported), then `--query-only` once the queues drained (every class ×100, gated).
- **otelbench**, as in G2A:
  - 50k spans, the round 2 fixture and flags (`--seed 283 --tenants 10 --attrs 8 --match-every 100 --order time --workers 8 --batch 32 --save-mode batch`);
  - three alternating ingest runs per arm, each followed by an audit;
  - then suite `all` ×1,200 on run 1 of each arm, two interleaved runs per arm;
  - `fdb` production: `--posting-value-format tuple --hash-bytes 8 --record-compression brotli-4 --bulk-index-conflicts`; `sql`: `--record-compression zlib-1 --sql-ingest direct --sql-rows compiled`, queries with `--sql-query direct`.
- **Outcomes:** every command exited 0. Every collector-bench run had 0 failures, 0 fidelity mismatches over 32 checked spans and 0 message retries. Every otelbench ingest and query run had 0 oracle failures.
- **Raw evidence:** `otel-relational-local-20261107/g3-merge-base/`: `runs.txt` (trees, sha256, every command's load, CPU and wall time), the receipts (per-iteration samples in the `qo-*` ones), the patch and the scripts.

**Collector bench, quiesced (gated)**, p50 / p99 ms, runs 1 and 2. Ratios
compare the means of the two runs. "Requests" are the client range requests
per query, merge-base against head.

| class | merge-base | head | p50 ratio | p99 ratio | requests |
|---|---|---|---:|---:|---|
| fts_single_token | 31.71 / 35.31, 32.13 / 36.25 | 9.22 / 15.21, 8.68 / 10.12 | 0.28 | 0.35 | 818 / 76–193 |
| fts_phrase_first_turn | 9.18 / 11.05, 8.95 / 10.58 | 4.03 / 5.63, 3.95 / 5.72 | 0.44 | 0.52 | 276–298 / 61–82 |
| fts_multi_turn_history | 9.15 / 10.24, 8.99 / 10.75 | 4.01 / 5.63, 3.93 / 5.58 | 0.44 | 0.53 | 276–296 / 59–80 |
| fts_same_span_predicates | 31.22 / 33.50, 31.58 / 35.82 | 11.61 / 14.04, 10.42 / 12.31 | 0.35 | 0.38 | 1,051–1,053 / 307–424 |
| same_span_conjunction | 1.68 / 2.44, 1.80 / 2.66 | 1.10 / 1.68, 1.02 / 2.04 | 0.61 | 0.73 | 33 / 32 |
| trace_existential_3way | 29.56 / 32.18, 29.96 / 32.09 | 10.31 / 13.84, 8.62 / 10.30 | 0.32 | 0.38 | 828 / 84–200 |
| trace_graph_traversal | 1.68 / 2.43, 1.72 / 2.36 | 1.29 / 2.34, 1.10 / 2.12 | 0.70 | 0.93 | 5 / 4 |
| correlated_logs_by_trace | 0.60 / 1.03, 0.59 / 1.11 | 0.59 / 1.23, 0.52 / 1.08 | 0.93 | 1.08 | 2 / 3 |
| *messages off* | | | | | |
| same_span_conjunction | 1.64 / 2.68, 1.60 / 2.54 | 0.95 / 2.01, 1.03 / 2.38 | 0.61 | 0.84 | 33 / 32 |
| trace_existential_3way | 4.50 / 5.40, 4.48 / 5.07 | 2.61 / 3.83, 2.83 / 4.54 | 0.61 | 0.80 | 12 / 10 |
| trace_graph_traversal | 4.94 / 5.95, 4.60 / 5.18 | 3.26 / 4.47, 3.25 / 4.79 | 0.68 | 0.83 | 7 / 6–7 |
| correlated_logs_by_trace | 0.53 / 1.06, 0.54 / 0.86 | 0.51 / 0.78, 0.49 / 0.65 | 0.95 | 0.74 | 2 / 3 |

Every class is within the 1.10× threshold at p50 (at most 0.95×), and every
`fts_*` class at p99 (at most 0.53×).

**Collector bench, post-ingest (not gated)**, from the same runs' default
phase, p50 / p99 ms:

| class | merge-base | head | p50 ratio | p99 ratio |
|---|---|---|---:|---:|
| fts_single_token | 27.38 / 100.24, 27.31 / 83.15 | 8.11 / 44.90, 8.19 / 36.31 | 0.30 | 0.44 |
| fts_phrase_first_turn | 7.59 / 104.78, 7.63 / 93.42 | 3.72 / 6.06, 3.92 / 62.75 | 0.50 | 0.35 |
| fts_multi_turn_history | 7.28 / 90.14, 7.55 / 95.88 | 3.81 / 5.41, 3.84 / 6.58 | 0.52 | 0.06 |
| fts_same_span_predicates | 26.66 / 32.29, 26.87 / 32.51 | 11.96 / 75.87, 10.49 / 79.49 | 0.42 | 2.40 |
| same_span_conjunction | 1.45 / 1.98, 1.50 / 2.04 | 0.99 / 1.25, 1.09 / 24.83 | 0.71 | 6.50 |
| trace_existential_3way | 24.94 / 27.12, 25.10 / 30.68 | 10.26 / 103.93, 8.33 / 84.13 | 0.37 | 3.25 |
| trace_graph_traversal | 1.50 / 2.66, 1.52 / 2.31 | 1.07 / 1.36, 1.07 / 6.74 | 0.71 | 1.63 |
| correlated_logs_by_trace | 0.45 / 0.58, 0.45 / 0.55 | 0.46 / 0.52, 0.46 / 0.53 | 1.02 | 0.92 |
| *messages off* | | | | |
| same_span_conjunction | 1.59 / 2.82, 1.58 / 2.27 | 1.23 / 2.79, 1.07 / 1.76 | 0.73 | 0.89 |
| trace_existential_3way | 4.34 / 5.96, 4.12 / 5.17 | 3.03 / 5.73, 2.82 / 4.36 | 0.69 | 0.91 |
| trace_graph_traversal | 4.51 / 6.40, 4.26 / 5.65 | 3.62 / 5.45, 3.26 / 5.27 | 0.78 | 0.89 |
| correlated_logs_by_trace | 0.47 / 0.59, 0.47 / 0.68 | 0.52 / 0.94, 0.53 / 0.71 | 1.11 | 1.30 |

- **p50:** at most 0.78×, except `correlated_logs_by_trace` (1.02× with messages, 1.11× without; 0.45–0.53 ms in both stores).
- **p99:** the post-ingest phase still meets the storage server's drain. The head's p99s include 24–104 ms stalls in five classes with messages; the merge-base's 83–105 ms stalls fall on `fts_single_token`, `fts_phrase_first_turn` and `fts_multi_turn_history`.

**Collector bench, ingest**, the same runs. Process CPU is user+sys of the
default command, which includes its post-ingest queries.

| arm | ingest s | spans/s | process CPU s | persist s | message txn s | message txns | logical span MB |
|---|---|---|---|---|---|---|---:|
| merge-base, deltas + FTS | 8.08 / 8.85 | 1,857 / 1,694 | 109.6 / 108.4 | 17.0 / 18.3 | 53.3 / 56.5 | 305 / 305 | 13.2 |
| head, deltas + FTS | 7.80 / 7.89 | 1,923 / 1,901 | 77.0 / 74.8 | 24.0 / 24.0 | 54.2 / 52.9 | 303 / 301 | 9.8 |
| merge-base, messages off | 4.22 / 4.24 | 3,558 / 3,539 | 40.5 / 40.7 | — | — | — | 372.5 |
| head, messages off | 3.33 / 3.11 | 4,511 / 4,825 | 29.7 / 28.6 | — | — | — | 190.2 |

- **With messages:**
  - wall 0.93×, spans/s 1.08×, process CPU 0.70×;
  - the summed message transaction time, G2C's persist criterion since G3, is 0.98×;
  - persist wall is 1.36×, as at the cutover: spans finish sooner, and each call waits longer for its messages' group commit.
- **With messages off:** wall 0.76×, spans/s 1.32×, process CPU 0.72×.
- **MESSAGES_TEXT** wrote 83.9–84.0 MB of keys against 60.2 MB (1.39×) for the same 1,698,165 postings. That is R1's cost, accepted at the cutover (D4).
- **Storage:** the collector bench reports no store size. Logical span bytes are each store's own count, not storage. The span storage comparison is the otelbench audit below, and the message store's is R1/R4's 1.04–1.06×.

**otelbench, ingest** (client CPU per span, 50k spans):

| run | µs/span | wall s | load start/end |
|---|---:|---:|---|
| `fdb` merge-base 1 | 452 | 8.1 | 2.06 / 4.25 |
| `sql` head 1 | 281 | 5.9 | 4.95 / 4.96 |
| `sql` head 2 | 264 | 5.8 | 3.79 / 4.05 |
| `fdb` merge-base 2 | 381 | 5.7 | 2.57 / 3.41 |
| `fdb` merge-base 3 | 379 | 5.9 | 3.49 / 3.77 |
| `sql` head 3 | 276 | 6.2 | 4.00 / 4.32 |
| **mean** | `fdb` 404, `sql` 274 (0.68×) | | |

The first `fdb` run, the campaign's first command on the fresh container, is
the slowest. Without it the ratio is 0.72×.

| per span, every run | logical B | prefix B | records B | keys |
|---|---:|---:|---:|---:|
| `fdb` merge-base | 2,001 | not reported | 460 | 11 |
| `sql` head (SpanWriter, zlib-1) | 1,483 | 878 | 534 | 11 |

The merge-base's audit has no prefix-byte count; the stack added it later.
Its key and value bytes (56,798,202 and 43,252,257 per 50k spans) equal G2A's
`fdb` production audit within 2 bytes each, so the layout is the same as the
one G2A measured at 1,501 prefix bytes per span. The gate here uses logical
bytes: 0.74×.

**otelbench, queries.** Tenant 0 of run 1 of each arm, suite `all`, 1,200
iterations, two interleaved runs per arm; p50 in ms, run a / run b.

| class | `fdb` merge-base | `sql` direct (head) | ratio |
|---|---:|---:|---:|
| span | 0.64 / 0.61 | 0.28 / 0.29 | 0.46 |
| span_absent | 0.63 / 0.59 | 0.28 / 0.29 | 0.46 |
| trace | 0.69 / 0.67 | 0.38 / 0.37 | 0.55 |
| trace_absent | 0.62 / 0.59 | 0.27 / 0.29 | 0.46 |
| time_narrow | 0.65 / 0.62 | 0.29 / 0.30 | 0.47 |
| time_broad | 0.89 / 0.85 | 0.52 / 0.53 | 0.60 |
| time_absent | 0.62 / 0.59 | 0.28 / 0.29 | 0.47 |
| time_empty | 0.51 / 0.47 | 0 (no read) | 0 |
| genai_response | 0.64 / 0.61 | 0.29 / 0.29 | 0.47 |
| genai_type | 0.63 / 0.59 | 0.29 / 0.28 | 0.47 |
| genai_scope_resource | 0.63 / 0.57 | 0.29 / 0.29 | 0.48 |
| genai_scope_scope | 0.63 / 0.58 | 0.28 / 0.28 | 0.47 |
| genai_absent | 0.62 / 0.58 | 0.27 / 0.29 | 0.47 |
| genai_operation | 1.12 / 1.08 | 0.69 / 0.70 | 0.63 |
| genai_provider | 1.13 / 1.13 | 0.69 / 0.71 | 0.62 |
| genai_model | 1.13 / 1.12 | 0.70 / 0.70 | 0.62 |

The median per-class p99 was 1.21 ms for `fdb` and 0.64 ms for `sql`.

**Verdict against the RFC's thresholds:**

- **G3 query gate** (every class's quiesced p50 within 1.10×): passes, at most 0.95×.
- **G2C** (with messages):
  - spans/s 1.08×, process CPU 0.70×, the summed message transaction time 0.98×;
  - `fts_*` p50 at most 0.44× and p99 at most 0.53×;
  - these pass. Persist wall (1.36×) fails the criterion's original wording, as at the cutover, where the owner replaced it with the summed transaction time.
  - Storage is not measured by the collector bench; the TEXT index's written key bytes are 1.39×.
- **G2D** (`same_span_conjunction`, `trace_existential_3way`, `trace_graph_traversal` p50 within 1.10×): 0.32–0.70×, passes.
- **G2B, query half** (`correlated_logs_by_trace` p50 within 1.10×): 0.93× and 0.95×, passes. The ingest half is in "Gate G2B": 0.38× CPU and at most 0.95× bytes per log against the merge-base, passes.
- **G3's otelbench arm** (G2A's criteria against `fdb` production): every class's p50 at most 0.63×, ingest CPU 0.68×, logical bytes 0.74×, 0 oracle failures; passes.

**Against custom-store.** On the quiesced classes, the merge-base's custom
store ran at 0.90–1.12× the p50s of custom-store (`7fb269417`) in "Quiesced
query gate", and the head's Exporter at 0.91–1.00× the quiesced tree's. These
are different sessions, not a controlled comparison. For the two role-less
phrase classes, the merge-base issued 276–298 range requests per query where
custom-store issued 226–240.

## Reproduce

Phase 3B removed `otelbench --backend fdb`, its layout flags and the old
arms of the benchmarks below. The `fdb` lines, and the production `fdb`
configuration at the end, run only on the archived trees, for example
`archive/otel-relational/custom-store`; the benchmarks' custom-store arms are
at the tags their sections name. At the head, the benchmarks measure only the
relational store.

```sh
CF=$(bazelisk run //cmd/fdb -- dev up --name otelcmp --port 4510)
bazelisk build //layers/otel/cmd/otelbench
B=bazel-bin/layers/otel/cmd/otelbench/otelbench_/otelbench
F="--seed 283 --count 100000 --tenants 10 --attrs 8 --match-every 100 --cluster-file $CF"
# The fdb line runs only on an archived tree (e.g. archive/otel-relational/merge-base).
$B --backend fdb --order time --mode ingest $F --workers 8 --batch 32 --save-mode batch --run fdb_r1 --output fdb-ingest.json
$B --backend sql --mode ingest $F --workers 8 --batch 32 --save-mode batch --run sql_r1 --output sql-ingest.json
$B --backend sql --mode audit $F --run sql_r1 --output sql-audit.json
$B --backend sql --mode query $F --suite all --iterations 600 --run sql_r1 --output sql-q.json
$B --backend sql --sql-query direct --mode query $F --suite all --iterations 600 --run sql_r1 --output sqld-q.json
```

G2A's Exporter benchmark (the relational arm; the custom store's arm exists
only at `archive/otel-relational/g2a`) runs on its own FDB testcontainer:

```sh
bazelisk test //layers/otel:otel_test --test_arg=-test.run=^$ \
  --test_arg=-test.bench=BenchmarkSpanExport --test_arg=-test.benchtime=1x \
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
`just generate-otelv1`.
