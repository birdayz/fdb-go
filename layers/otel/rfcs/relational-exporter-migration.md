# Replace the custom OTel store with OTEL_V1 relational tenant stores

**Status:** Accepted implementation plan (2026-11-07), with every §7 owner decision (D1–D11) taken as recommended. Phases 0–2 are implemented and measured (G0, G2A, G2B and the R1/R4 message measurements in the benchmark doc). Phase 3A (cutover) is implemented: `otel.Exporter` is the relational store, `ExporterConfig` is trimmed per D2 and equality is typed per D1. G3 (benchmark doc, "Gate G3"): every collector-bench query class is within 1.10× at p50 except `fts_single_token` (1.53×), message persist wall is 1.39× while ingest is 0.90× wall and 0.89× CPU (the owner accepted it; G2C's persist criterion became the summed message transaction time, 0.93×), and `TestRunBenchmarkRealFDB` passes. The `fts_single_token` gap was the storage server's post-ingest drain meeting 818 range requests per query. The fix reads reference chunks in place and coalesces marker reads, and gives 0.30× the custom store quiesced (benchmark doc, "fts_single_token: the G3 gap explained and fixed"). G3's query gate is quiesced (§6). With the role-less phrase classes probing their rarest term before scanning shards, every collector-bench class is at most 0.98× the custom store at p50 quiesced, with defaults and with messages off (benchmark doc, "Quiesced query gate"). Phase 3B (deletion) is done: `FDBBackend`, its posting index, keyspace and ingest code, the old log and message storage, v1 continuation tokens, the envelope/fidelity code, Brotli records and their tests are deleted, and so are `otelbench --backend fdb`, `--posting-value-format`, `--hash-bytes`, `--diagnostics` and the Brotli values. The Exporter is the only store; the same-binary baseline is gone, so later comparisons use the archived custom-store receipts and trees (benchmark doc, "Measured trees"). G3 was re-measured against the merge-base (benchmark doc, "G3 at the merge-base"), for the collector bench and for the otelbench `sql --sql-query direct` versus `fdb` production arm, which the earlier G3 runs had not measured. Every quiesced class is at most 0.95× the merge-base at p50. Ingest is 0.70× process CPU with messages and 0.72× without, and the summed message transaction time is 0.98× while persist wall is 1.36×. The otelbench arm is at most 0.63× at p50, 0.68× ingest CPU and 0.74× logical bytes. Those figures are at the measured head, `archive/otel-relational/g3-head`; the later commits' code changes, listed in the benchmark doc ("G3 at the merge-base", "After the measured head"), are on no measured path (benchmark doc, "G3 at the merge-base"). G2B's log ingest was measured against the merge-base too, at a later head of the change, `archive/otel-relational/g2b-head` (benchmark doc, "Gate G2B"): 0.38× the CPU per log, 0.95× the logical and 0.82× the prefix bytes per log, and 0.96× the `QueryLogsByTrace` p50.

**Base:** draft PR #1035. Line citations are against the tree this plan was written on, `archive/otel-relational/rfc-base` (`27e0b1b60`), which still has the custom store. The merged change deletes most of the cited files, and its merge-base lacks some of them (`sqlbackend`, otelsql's row builders).

**Owner constraints (2026-11-07):** the OTel layer is pre-release, so this is a straight replacement:

- no data migration and no compatibility for stored bytes;
- no dual-write, dual-read or old-vs-new feature flags;
- `FDBBackend`, its posting index, raw-KV logs, manifests and their on-disk formats are deleted;
- public Go API *semantics* keep parity tests; the API *shape* may change where that simplifies the code.

**Inputs:** [RFC-283](283-transactional-otel-trace-storage.md); `docs/benchmarks/otel-relational-local-20261107.md` (the "benchmark doc"); `TODO.md` sections "OTel SQL ingest migration" (3586) and "OTel relational prototype findings" (3625); `docs/compatibility.md` "OTel SQL schema foundation" (357).

## 0. Decision summary

1. **One OTEL_V1 schema store per (run, tenant) holds every signal.** Database `/OTEL/R_<RUN>`, schema `T<tenant>`, both created on first open. The store holds spans, logs, messages, markers and a small `store_meta` table. `Run`/`Tenant` keep their meaning. The catalog (database, schema row, pinned template, store header) replaces the JSON ownership manifest.
2. **Writes go through the Record Layer** into compiled `otelv1pb` rows, as `sqlbackend` already does (`OpenTenantStoreCompiled` + `BuildCompiled` + `SaveRecordBatch`). SQL `INSERT` costs 6.5× more client CPU (benchmark doc, "Ingest") and is never used for writes.
3. **Every Exporter read is a direct Record Layer scan of a standard OTEL_V1 index.**
   - One transaction per read, with the same budgets, signed keyset tokens and early distinct-trace top-K.
   - Direct scans already beat the custom posting index on every measured class (round 2: `genai_model` 0.93 ms against 1.20 ms).
   - SQL serves ad hoc queries, joins and Java readers of the same rows. No Exporter API routes through SQL.
4. **The message design is kept; only its storage changes.** Decomposition, manifests, content-addressed dedup, `(writer, seq)` IDs, 16 TEXT shards, group commit, tails-first large messages and markers stay. Storage moves to `MESSAGES`/`MESSAGE_TRACES` rows, a FULLTEXT index on `messages.content`, and packed `ref_chunks` on `spans`.
5. **The template is rewritten in place.** `OTEL_V1` stays version 1 and the Java golden is regenerated. With no stored data there is nothing to evolve; §4.4 covers evolution after release.
6. **Attribute equality becomes typed** (D1), matching RFC-283 §2.1 and SQL. The prototype's canonical-string coercion goes.
7. **Old and new code coexist only as source until cutover** (§6 phase 3). The new implementation is unexported and unreachable from production; nothing writes or reads both formats. Cutover swaps the `Exporter` implementation and deletes the old code, its formats and its LAYOUT tests.

## 1. Public surface and its guarantees

Every Exporter method calls `begin` (exporter.go:262): after `Shutdown` it fails with kind `operation` ("exporter is shut down"). On error, methods return empty non-nil collections (for example exporter.go:860, 870, 880). The "New" column says what the replacement does.

### 1.1 Lifecycle and configuration (package `otel`)

| Item | Cite | Guarantee today | New |
|---|---|---|---|
| `ExporterConfig` | exporter.go:57-83 | `cluster_file` required (:131); `run` `^[a-z0-9_]{8,48}$` (corpus.go:63); `tenant` 0..49,999,999 (:146, :40); `order` trace\|time; `hash_bytes` 1..32; `posting_value_format` json\|tuple; `record_compression` none\|zlib-1\|zlib-default\|brotli-0..4 (corpus.go:69-83); `bulk_index_conflicts`; `enable_message_deltas`; `enable_message_full_text` implies deltas (:159-167); `batch_size` 1..64; `workers` 1..16; `max_candidates`; `max_bytes`; `k` 1..10000; `max_inflight_transactions` 1..1024, default 32 (txn_limit.go:13-16); `transaction_timeout` 1s..5m, default 10s (txn_limit.go:27-33) | Remove `order` (time only), `hash_bytes`, `posting_value_format`. `record_compression` none\|zlib-1\|zlib-default, default `zlib-1`; Brotli refused because SQL and Java cannot read it (corpus.go:76-80). Keep the rest. |
| `DefaultExporterConfig` | exporter.go:85-87 | time, 8, tuple, brotli-4, bulk on, 64, 8, 20000, 4 MiB, 100, 32, 10s | the same minus removed keys; `zlib-1` |
| JSON/YAML | exporter.go:99-128 | omitted fields keep defaults; `transaction_timeout` is a Go duration string; decode errors are kind `config` | keep |
| `Validate` | exporter.go:130-150 | each bound gives `ConfigError`; `NewExporter` validates before dialing (:210-213) | keep |
| `NewExporter` | exporter.go:206-253 | opens FDB; publishes or verifies a `ready` manifest for a fixed synthetic corpus identity (:35-40); concurrent openers conflict and retry; bootstrap uses the exporter txn policy | `EnsureTenant` (template, database, schema, created if absent) in exporter-policy transactions; `OpenTenantStoreCompiled`; `store_meta` layout check (§3.4) |
| `Config()` / `Shutdown` | exporter.go:255, 274-291 | Shutdown is idempotent: rejects new work, joins active calls and message workers, then closes the DB. On ctx expiry it returns `canceled` and drains in the background. | keep |
| `ExportSpans`, `ConsumeTraces` | exporter.go:294-303 (:33 `SpanExporter` check) | adapters onto `ExportResourceSpans` | keep |
| `ResourceSpansFromReadOnly` | exporter.go:951-1072 | SDK→OTLP mapping, grouped by resource/scope in first-appearance order | keep (storage-free) |
| `CanonicalValue`/`CanonicalAttribute` | exporter.go:509-534 | int stays `int`; every other value becomes a `string` with canonical text (bool, `formatDouble`, base64, canonical JSON) | typed: string/int/double/bool; bytes/json reported but not queryable (D1) |
| errors | errors.go:5-79 | `OtelError{Kind,Op,Message,Cause}` plus typed wrappers for config, ownership, admission, budget, corruption, http, audit, oracle, operation, transient | keep; mapping in §4.5 |

### 1.2 Span export

| Guarantee | Cite | New |
|---|---|---|
| Every span is admitted before any write; an inadmissible span fails the call with no partial write. Batches then commit independently; the first error cancels the rest; stats count only committed batches; cancellation gives `canceled`. | exporter.go:305-357, 359-414, 169-170 | keep; a save that replaced spans' index clears push past the 500 KB guard is retried in transactions that fit them, and a replacement past it even alone fails as `operation` after earlier batches committed (as a log's grown prior image) |
| Identity: nonzero 16-byte trace ID and 8-byte span ID, `start ≤ MaxInt64`; otherwise `AdmissionError`. | exporter.go:720-739 | keep |
| Per-span estimate `2048+len(otlp)+Σ2·(k+v+256)+refs` ≤ 300,000; partition by `BatchSize` and estimate. | exporter.go:42-44, 744-761, 416-434 | keep formula; refs count encoded key bytes |
| Actual transaction ≤ 500 KB (`GetApproximateSize`), else `budget`; no 1 s elapsed guard for the exporter. | ingest.go:247-256 | keep |
| One write slot per write transaction, taken before `Run` so queueing does not consume the timeout. | exporter.go:197-198, 312; txn_limit.go:59-86 | keep |
| Duplicates: within a call the later span wins (`DuplicateSpans`); across calls `SaveRecord` replaces and the index diff retracts old entries. | exporter.go:664-670; index.go:216-272 | keep (standard index maintenance) |
| Indexed set per scope: unique keys, last value at first position; keys over 1,024 or values over 64,000 bytes are not indexed (counted); non-string/int values become canonical strings. Counted in `NormalizedSpans`/`UnindexedAttributes`. | exporter.go:446-492, 674-679 | same dedup and caps; typed values (D1) |
| With messages on, the GenAI content keys are excluded from equality; equality queries on them give `ConfigError`. | exporter.go:494-507; messages_query.go:185-192 | keep |
| Fidelity: the record stores canonicalized OTLP plus `Envelope.fidelity` with the original lists; producer unknown fields (including tag 536870911) survive. | exporter.go:641-653, 683-696, 797-856 | `otlp` = original single-span ResourceSpans with message manifests; no fidelity field |
| `ExportStats` | exporter.go:169-181 | keep; `MessageStoreStats` keeps the counters the TEXT index still produces |

### 1.3 Span reads

| API | Cite | Guarantee |
|---|---|---|
| `Query(Attribute)` | exporter.go:858; fdb.go:227-265; index.go:305-373 | Newest K distinct traces by latest matching start, desc, then trace desc (corpus.go:283-298). `order=time` stops at the K-th distinct trace (index.go:361). Every posting read is charged; exceeding `MaxCandidates`/`MaxBytes` gives `QueryBudgetError`, never truncation (index.go:318-322). An unreadable index gives `OwnershipError` (fdb.go:247). Covering values are verified (index.go:327). |
| `Trace(id)` | exporter.go:868; fdb.go:267-316 | All spans, sorted start asc then span asc. Row limit `MaxCandidates+1`, byte limit `MaxBytes`; overflow gives `budget`. |
| `Span(id)` | exporter.go:878; intrinsic.go:165-176 | Every match, ordered (start, trace) asc; no K (README.md:39-41). |
| `Time(TimeRange)` | exporter.go:888; intrinsic.go:24-35, 178-197 | `[start,end)`; inverted range is `ConfigError`; an empty range reads nothing; newest K distinct traces; `Latest` = newest matching span. |
| `LoadSpanOTLP` | exporter.go:898-947 | IDs must be lowercase nonzero hex, else `ConfigError`. A missing span is `SpanNotFoundError` (kind `operation`, :183-188). Returns original attributes and byte-exact reconstructed messages. |
| concurrency | exporter.go:196 | `queryMu` serializes queries because `lastWork` is shared. New: work is per call, and the mutex goes. |

### 1.4 Advanced queries and tokens (query_advanced.go)

- **`QuerySameSpan`** (:33-51, 527-688): 1..16 predicates, deduped keeping first positions (:222-245); scopes resource/scope/span; string/int with canonical int and key/value caps (:151-174). `Predicates[0]` drives a scan over window `[lo,hi)` (:125-149); the limit defaults to K; candidates are probed in batches of 64 (:28-30, 588-688). Each page is one snapshot at `ReadVersion`; a full page always carries a continuation and the next page may be empty (:42-44, 574-585). Order is posting order: start desc, trace desc, span asc.
- **Signed tokens:** tuple `(1, kind, run, tenant, order, digest, last)` + HMAC-SHA256 under domain `otel/continuation/v1\0`, at most 4,096 bytes (:21-27, 258-305). The 32-byte secret per run is created on the first full page outside the retry loop; a concurrent winner wins (:371-417). A MAC failure or absent key is corruption; another run, tenant, order or query is config (:324-344); a malformed sort key is corruption (:346-369). The digest binds the predicate set, not its order, and the window (:202-213). Kinds `same_span` and `message_fts` do not cross.
- **`QueryTraceExistential`** (:53-78, 724-939): 1..16 clauses over spans, logs and messages. Each clause's complete term is unioned inside the window, then the clauses are joined on trace. Witnesses are newest first; `Latest` is the newest across witnesses; hits are ordered Latest desc, trace desc and cut at `limit`. Message clauses need full text and take no limit or continuation (:747-760, 1128-1131); log clauses skip uncorrelated logs (:851).
- **`TraverseTraceGraph`** (:80-105, 941-1114): reads one bounded trace and reports roots, children, parent, missing parents, orphans, cycles, depth and links. Links resolve only to stored spans of the same trace; the walk is iterative; budgets apply.

### 1.5 GenAI messages (messages*.go)

- **Layouts** none, `deltas` and `fulltext_v2` (record.go:19-24) are run identity; a mismatch is `OwnershipError` (corpus.go:441-446). Content attributes come from spans and events (messages.go:92-106, 453-485).
- **Decomposition:** an attribute value becomes a manifest of `writer.seq` IDs (value/array/JSON layouts, messages.go:108-117, 237-345, 399-452). Elements are capped at 16 MiB (:51) and manifests at 64,000 bytes, sized with placeholder IDs (:52, 540-542). `LoadSpanOTLP` reconstructs byte-exact (:1865-2040).
- **Dedup:** digest SHA-256(`otel_msg_v1\0‖tenant‖0‖encoding‖0‖element`)[:16] (:41-44, 121-134). Duplicates are caught per call, per backend through in-flight waiting (:1761-1833), and across backends by a serializable read of the unique hash index (:1003-1027). Re-export writes nothing (`Written 0`, `Present` = all). Messages are immutable and never collected (:14-15).
- **Writer/seq:** a run-wide counter allocates at most 16 writers per backend, with `shard = writer % 16` (:766-881). An ambiguous commit never reuses a seq; a large message takes a fresh seq per attempt (:1276-1352).
- **Large messages:** raw parts of at most 1 MiB; text segments of at most 1,024 distinct tokens and 64 KiB, overlapping by 15 tokens (:57-71; messages_text.go:241-365). Tails commit first and part 0 last (:1354-1437). `transaction_too_large` replans at 256, 64 and 16 tokens, then fails (:1253-1274).
- **Group commit:** at most 128 messages and 1 MB per transaction; a canceled caller's queued messages are dropped (:79-88, 1085-1251).
- **Back references:** each span packs `ref_chunks` of `(writer, seq, attribute, newest, ordinals)`, at most 16 KB per chunk, indexed by (trace, start) (:1522-1620). `MessageTrace` markers are saved before spans (:1622-1759).
- **`QueryMessageFullText`** (messages_query.go:22-183, 761-836): modes all (default), any and phrase. A query has 1..16 terms, at most 4,096 bytes, and terms of at most 1,024 bytes. The role is normalized (messages_text.go:22). Filters are attribute key, same-span predicates, window and `FirstTurnOnly` (newest reference). Hits are one per (span, attribute, message), sorted by `messageRefKey` (:448), paged with `message_fts` tokens, and every KV read is charged. Tokenizer `otel_message_v1` is the default tokenizer, except that a token over 1,024 bytes becomes "" and keeps its position (messages_text.go:181-239).

### 1.6 Logs (logs.go)

- **`ExportLogs`** (:669-777) admits the whole call before any write (:541-573).
  - **Per-log rules:** timestamps ≥ 0; event ID ≤ 256 bytes of valid UTF-8; IDs lowercased; the trace ID must be 32 hex digits and nonzero; a span ID requires a trace ID (:442-517).
  - **Caps:** raw ≤ 8 MiB and stored ≤ 4 MiB (:27-47); each covering value ≤ 100,000 bytes; native write cost ≤ 8,000,000, including prior-image clears.
  - **Effective time** is event time, else observed, else receiver (:463-470).
  - **Event IDs:** a missing ID gets a UUIDv4 outside retries (:152-161); `EventIDs` is aligned with the input.
  - **Duplicates:** within a call the last wins (`DuplicateLogs`); across calls the log is replaced atomically and its old entries retracted (`Replaced`, :592-667).
  - **Batches** are bounded by `BatchSize` and 2 MiB. A prior image that grows after admission is `OperationError` (:631-635).
- **`ConsumeLogs`/`LogRecordsFromResourceLogs`** (:779-805): one detached envelope per record; all-zero IDs mean uncorrelated; `OTLP()` returns a detached copy (:87-95).
- **`QueryLogsByTrace(trace, span?)`** (:856-931): case-insensitive; ascending by (effective, event ID); an index/record disagreement is corruption; budgets apply.
- **`QueryLogsByAttribute(a, limit)`** (:990-1041): scopes resource, scope, `log` and `intrinsic` (event_name, :49-59); string/int values. Returns distinct events newest first, correlated or not; limit 0 means K. A disagreement is corruption.

### 1.7 Transactions and error kinds

Exporter transactions use `transaction_timeout` with no retry limit (fdb.go:155-183; txn_limit.go:35-57; README.md:584-609). `exporterTxnError` (txn_limit.go:88-112):

- domain kinds are kept;
- a caller-ended context is `canceled`;
- FDB 1000..1499 and Record Layer retriable errors are `transient`;
- anything else is `operation`.

Collector `IsPermanent` (collector.go:61-80) treats config, admission, ownership, corruption and operation as permanent, and retries transient, budget and canceled.

### 1.8 Collector and benchmark (package `collector`)

- **Sink and config:** `Sink` is `{ExportResourceSpans, Shutdown}`, checked by `var _ Sink = (*otel.Exporter)(nil)` (collector.go:44-50). `Config` flattens the `ExporterConfig` keys beside `timeout`, `sending_queue` and `retry_on_failure`, and rejects unknown keys (:106-150).
- **Exporter:** `Factory.CreateTraces` opens `otel.NewExporter` on `Start` (:598-636). `TracesExporter` provides Start, ConsumeTraces (which rejects invalid identities synchronously), Flush, Shutdown and Stats (:643-1239). Queue, batch and retry follow the exporterhelper contracts (:82-104).
- **Bench:** `CollectorBenchConfig` carries the layout knobs (bench.go:77-133) and `collectorConfig` maps them onto the exporter (:236-251). `benchQueryBudget` is the old cost model (:217-234). The 8 query classes call `QueryMessageFullText`, `QuerySameSpan`, `QueryTraceExistential`, `TraverseTraceGraph`, `QueryLogsByTrace` and `LoadSpanOTLP` (:61-73, 1540-1702), driven by `RunBenchmark` (:912).

### 1.9 Other dependents of the old code

- **otelbench:** `--backend fdb` calls `otel.OpenFDBContext` (cmd/otelbench/main.go:28-36); `collector-bench` is fdb-only (:112); layout flags at :64-80, 99-100 and 239-262.
- **sqlbackend:** `otel.Backend`/`Statistics`/`Size` (fdb.go:16-54), `IntrinsicBackend` (intrinsic.go:37-42), `RecordSerializer` (record_compression.go:16), `FixtureOTLP` (record.go:203-205).
- **ClickHouse:** `Backend`/`Statistics`/`Size` and `Config`/`Manifest`.
- **Scripts:** `bench/local_campaign.sh` (the `fdb_*` arms); the old `bench/ops/ingest20k` tooling has been removed; `bench/ops/horizontal` is retained only as the provisioning and measurement library for the Kubernetes benchmark (its native-process runbook is retired).

## 2. Tests that pin the surface

Targets:

- `//layers/otel:otel_test` holds every package-`otel` test (timeout long, no tags; fast lane).
- `//layers/otel/collector:collector_test`.
- `//layers/otel/sqlbackend:sqlbackend_test` (size large).
- `//layers/otel/otelsql:otelsql_test`.
- `//layers/otel/cmd/otelbench:otelbench_test`.
- `//layers/otel/bench:local_campaign_test` (sh_test).
- `//pkg/relational/sqltest/otel`.
- `conformance` (rfc204 golden, Java probe).

FDB tests use testcontainers and skip without Docker.

Classes:

- **AGNOSTIC** tests use only the public API and port as is.
- **ADAPT** tests keep their behaviour; the harness or the expected numbers change.
- **LAYOUT** tests pin old bytes, keys or internals and are deleted.

| Files | Top-level tests | AGNOSTIC | ADAPT | LAYOUT |
|---|---:|---:|---:|---:|
| core | 114 | 53 | 21 | 40 |
| message/log | 52 | ~7 | ~20 | ~25 |

Many message tests first classed LAYOUT pin mechanisms the design keeps: queue, writers, TEXT bunches, large index, markers, manifests. They are ADAPT below.

### 2.1 Spans, Exporter, config

- **AGNOSTIC**
  - exporter_test.go :667, :822, :884.
  - `TestExporterRealFDB` (:1228) subtests `sdk_batch_span_processor` :1467, `otlp_fidelity` :1538, `concurrent_exports` :1995.
  - export_fidelity_regression_test.go:32.
  - txn_limit_test.go :19, :33, :100.
  - guards_test.go :192, :242, :267, :365, :416.
  - All 11 tests in oracle_test.go and all 15 in evidence_test.go.
  - query_advanced_test.go :228, :263, :360, :420.
- **ADAPT**
  - `TestExporterConfig{Defaults,YAML,JSON,Validate}` (:118, :131, :242, :343): drop the removed keys.
  - :448, :511, :524: typed results (D1).
  - :608, :920: go through the row builder.
  - `TestExporterRealFDB` subtests:
    - `sdk_syncer_genai` :1241: typed bool/double (:1352-1354); no `exporterLayouts` (:1216).
    - `batching_dedup_idempotent` :1739.
    - `shutdown_and_reopen` :1899: mismatch via `store_meta`.
  - Umbrella (integration_test.go:200) subtests become Exporter tests that keep their N−1/N budget edges with N recomputed: `literal_order_and_budgets` :1173, `single_source_trace_budget` :420, `high_attribute_batches`/`testOtelAdmission` :333/:1461 (the 1,024/64,000 caps), `typed_scoped_controls` :1142, `public_query_replay` (query_replay_test.go:113), `intrinsic_v2` (intrinsic_test.go:104), `selective_v2` (selectivity_test.go:148).
  - posting_value_test.go:38: becomes index-key admission with nothing written.
  - `TestTxnLimitRealFDB` :109: injects through the store's `*FDBDatabase`.
  - guards_test.go:336, bootstrap_test.go:13, exporter_bootstrap_policy_test.go:42: retarget to `EnsureTenant`.
  - otel_test.go :32, :60 and guards_test.go:46: fewer knobs.
- **LAYOUT (delete)**
  - Test functions: exporter_test.go:1037; otel_test.go :88, :107, :120, :160, :219, :231, `FuzzOTLPRoundTrip` :313; guards_test.go :141, :170, :300 and the envelope halves of :450/:480; intrinsic_test.go:17; query_advanced_test.go :131, :290, :322; export_fidelity_regression_test.go:16.
  - Whole files: all `ingest*_test.go` (~15 tests, ~13 umbrella subtests), posting_diff_test.go, posting_value_test.go except :38, keyspace_test.go, bulk_layout_test.go, record_compression_test.go, intrinsic_corruption_test.go.
  - Umbrella subtests: audit, ownership, unreadable, malformed postings, source identity, mutations, drop, interrupted ingest, worker failure.

### 2.2 Advanced queries

`TestAdvancedQueriesRealFDB` (query_advanced_test.go:550) is ADAPT:

- `same_span` :564: no `qaLayouts` (:450); ties per D3 (`qaSortSpans` :93).
- `same_span_budget` :639: recompute.
- `continuation_security` :667: unchanged; it is the token-security parity gate.
- `trace_existential` :749.
- `trace_graph` :840: read result fields instead of `e.backend.LastQueryWork()`.

`TestContinuationTokenRejections` :154 is ADAPT: keep the security matrix; rewrite the sort-key cases for v2.

### 2.3 Messages

- **AGNOSTIC**
  - messages_fdb_test.go :539 (`testMessageTracePerTurn`), :577 (`testMessageReplacement`), :767 (`testMessageExistential`).
  - message_boundary_regression_test.go:107.
  - exporter_test.go:2064.
  - export_fidelity_regression_test.go:66.
- **KEEP:** retained storage-free code, all in messages_test.go: :25, :57, :326, :443, :556, :623, :653, :701 (moves to otelsql with the tokenizer), :746, :866, :1000.
- **ADAPT**
  - messages_fdb_test.go :396 (drop index-entry and marker counts), :728 (`store_meta` instead of GetIndex), :842 (new chunk size), :878, :956, :1023, :1111, :1164 (via `store_meta`), :172, :306, :659 (recompute bunch bytes).
  - messages_test.go :824 (MESSAGES rows), :961, :1063 (v2 sort key), :399, :514.
  - Every `message_*_regression_test.go` test, with `messageTxnHook` re-created on the new store:
    - "dropped run" becomes "schema dropped";
    - corruption tampers MESSAGES rows.

### 2.4 Logs

- **AGNOSTIC**
  - logs_test.go :314, :333, :704.
  - logs_fidelity_test.go :143, :159, :179 (bool→string count is ADAPT under D1).
- **ADAPT**
  - logs_test.go :203 (through `ExportLogs`), :249 (projection rules only), :416 (drop `h1_collisions`), :538, :603 (`StoredBytes` = serialized record bytes), :640, :680, :362; logs_fidelity_test.go:94.
  - logs_admission_corruption_regression_test.go :318 (OperationError after a committed neighbour), :19/:151/:214/:400 (whole-call admission re-derived for record and index cost), :78 (tampered LOGS record).
- **LAYOUT**
  - logs_test.go :90, :137.
  - logs_admission_corruption_regression_test.go:390.

### 2.5 Collector, bench, otelbench, sqlbackend, ClickHouse

- **AGNOSTIC**
  - Every collector `fakeSink` test (collector_test.go:247-1915).
  - errors_test.go, lifecycle_regression_test.go, bench_clock_test.go.
  - `TestBenchTracker`, `TestBenchRates`, `TestBenchCPU`, `TestBenchHistogram`.
- **ADAPT**
  - collector_test.go :558, :583, :624, :685, :744, :860 (layout keys) and :2502 (`TestCollectorTracesExporterRealFDB`).
  - bench_test.go :617 (`TestRunBenchmarkRealFDB`; its message-store counters are LAYOUT), :767, :475, :568, :71.
  - bench_duration_regression_test.go:10.
  - otelbench `TestExecuteModes`, `…FlagForwarding`, `…RejectsArguments…` and the collector-bench tests (`-backend=fdb`).
  - local_campaign_test.sh.
- **LAYOUT**
  - bench_test.go:363 (`TestBenchQueryBudget`).
  - otelbench record_compression_test.go.
  - clickhouse_granularity_test.go:45.
- **KEEP-NEW**
  - sqlbackend/backend_fdb_test.go:29.
  - otelsql template_test.go:12 and rows_compiled_test.go :32/:88/:131.
  - sqltest/otel.
  - conformance rfc204_template_golden_test.go:132 and otel_sql_shapes_java_probe_test.go.
  - Other ClickHouse tests are unaffected.
- **Helpers**
  - Move and keep: `freshRun`, `requireOtelKind`, `requireOtelBudget` (integration_test.go:29-155); `testConfig` (otel_test.go:20); exporter_test.go :40-100 and :1082-1211; the `delta*`, `fts*`, `lg*`, `qa*` fixtures.
  - Delete the raw-subspace census and transactor helpers: integration_test.go:38-143, the ingest_* and posting_* helpers, messages_fdb_test.go:144, logs_test.go:406.

## 3. Mapping onto OTEL_V1

### 3.1 Write paths

| Write | Rows | Indexes maintained in the same transaction | Transaction |
|---|---|---|---|
| span | `SPANS` (`BuildCompiled`) | SPANS_TIME, SPANS_SPAN_ID, SPANS_{STR,LONG_STR,INT,DOUBLE,BOOL}_ATTR, SPANS_REF_CHUNKS | ≤ 64 spans, estimate ≤ 300 KB; `PrefetchRecordBatch` + `SaveRecordBatchWithOptions{CoalesceIndexWriteConflicts: bulk_index_conflicts}`; 500 KB guard (exporter.go:416; ingest.go:230-254) |
| message | `MESSAGES` parts; `content_hash` on part 0 only | MESSAGES_HASH (unique), MESSAGES_TEXT (FULLTEXT), MESSAGES_LARGE | unchanged group commit and tails-first (messages.go:1029-1437) |
| marker | `MESSAGE_TRACES (writer, seq, trace_id)` | MESSAGE_TRACES_TRACE | unchanged, before spans (messages.go:1671-1759) |
| log | `LOGS` | LOGS_TRACE, LOGS_TIME, LOGS_{STR,LONG_STR,INT,DOUBLE,BOOL}_ATTR, LOGS_EVENT | preflight + ≤ 2 MiB batches (logs.go:541-667) |
| meta | `STORE_META` rows `layout`, `message_writer`, `continuation_hmac` | none | as the writer counter and secret today (messages.go:819-873; query_advanced.go:388-417) |

**Span row.**

- Projection lists:
  - the canonical per-scope set (exporter.go:454-492), not every KV, so the duplicate contract is kept;
  - message keys excluded when decomposing;
  - strings over 64,000 bytes not projected; rows.go:245 has no upper cap today;
  - `any_attrs.json_value` uses deterministic `appendCanonicalJSON` (exporter.go:556-596), not `protojson.Marshal` (rows.go:271, rows_compiled.go:167), whose output is unstable by design.
- `otlp` is the deterministic single-span ResourceSpans with original attributes, producer unknown fields and manifests.
- `ref_chunks` holds `packSpanRefs` chunks (messages.go:1544-1571), sized so each encoded index key stays ≤ 8,000 bytes. Tuple escaping of 0x00 can double the bytes, so the size is measured on `tuple.Tuple{refs}.Pack()`.
- A non-GenAI span keeps round 2's 11 KV pairs (8 attributes).

**Log row.**

- Columns:
  - `event_id`; `trace_id`/`span_id` bytes (NULL when uncorrelated);
  - `time_ns`, `observed_ns`, `effective_ns`, `time_source`;
  - severity, `event_name`, `body_text` (string bodies only), resource/scope fields;
  - typed canonical attribute arrays under scopes `resource`/`scope`/`log`;
  - `otlp`: the deterministic single-log ResourceLogs.
- Record Layer split records (`enableLongRows` defaults on, metadata/builder.go:138-142) and the zlib serializer replace the private header, Brotli codec and 90,000-byte chunks (logs.go:338-440).
- Admission:
  - raw ≤ 8 MiB;
  - serialized row + Σ index keys + prior image's index keys (preflight `LoadRecordAsync`) ≤ 8,000,000.

### 3.2 Read paths: exact scans

**Mechanics.**

- **Ownership:** every read opens with `TenantStore.Builder(rc).Open()` (otelsql/store.go:88), which reads the store header and index states. This is the per-transaction ownership check (§3.4).
- **Read version (implemented, `tenant.go` `read`):** a read-only read takes its version from the client GRV cache: at most 100 ms old, and never older than this client's last commit, which refreshes the cache. A read therefore sees every write this database handle (this Exporter) committed before it began, and skips the GRV round trip. Writes committed through another handle, in this process or another (another collector replica or Exporter), can be invisible to a read for up to 100 ms; the old store read at a fresh version every time. Stream 2A's `hmac_race` test hit exactly this with two exporters. Measured on `--sql-query direct`: 0 GRVs per query and p50 −25% (span 0.41→0.30 ms). Paged reads with continuation tokens keep one snapshot per page as before.
- **Scans:** `ScanIndex` with a `ScanLimiterState` and `ReturnedRowLimit=MaxCandidates+1`; budgets are checked after every entry (intrinsic.go:100-163; sqlbackend/direct.go:51-95).
- **Probes:** batched `GetRange` futures over `store.IndexSubspace(idx).Pack(prefix)` (store_api.go:33), validated as standard index entries.
- **Record loads:** pipelined `LoadRecordAsync` (store.go:384), 64 at a time.
- **Column positions:** read from `IndexEntry.Key`/`PrimaryKey()`, never guessed; relational PKs lead with the record type key (sqlbackend/direct.go:192-196).

`v` is the typed value and `h` is `sha256(v)[:16]`.

| API | Index or records | Range | Order | Stop |
|---|---|---|---|---|
| `Query` string ≤ 1,024 B | SPANS_STR_ATTR | AllOf(scope, key, v) | reverse | K distinct trace_id |
| `Query` string 1,025..64,000 B | SPANS_LONG_STR_ATTR + LoadRecordAsync | AllOf(scope, key, h); verify `long_str_attrs` holds (scope, key, v) | reverse | K verified distinct |
| `Query` int/double/bool | SPANS_{INT,DOUBLE,BOOL}_ATTR | AllOf(scope, key, v) | reverse | K distinct |
| `Time` | SPANS_TIME | Between((s), (e)) = `[s,e)`; empty reads nothing | reverse | K distinct |
| `Span` | SPANS_SPAN_ID | AllOf(span_id) | forward | exhaust |
| `Trace`, `TraverseTraceGraph` | SPANS records | PK prefix (trace_id), `ScannedBytesLimit=MaxBytes`; graph decodes `otlp` for parents and links | forward | exhaust |
| `LoadSpanOTLP` | LoadRecord(trace, span); MESSAGES LoadRecord(writer, seq, part) | — | — | — |
| same-span driver | SPANS_<T>_ATTR | low (sc, k, v, lo) inclusive; high = min((sc, k, v, hi) excl., resume (sc, k, v, start, trace, span) excl.) | reverse | `limit` accepted |
| same-span / FTS predicate probe | SPANS_<T>_ATTR | prefix (sc, k, v, start, trace, span); 64 candidates × (n−1) futures | — | — |
| existential span clause | SPANS_<T>_ATTR | (sc, k, v) ∩ window | reverse | exhaust |
| existential log clause | LOGS_<T>_ATTR (covers trace_id, span_id); LOGS_EVENT for `intrinsic/event_name` | (sc, k, v) ∩ window on effective_ns | reverse | exhaust; skip NULL trace |
| `QueryLogsByTrace` | LOGS_TRACE + LoadRecordAsync | AllOf(trace[, span]); in-memory sort (effective, event_id) (logs.go:907-912) | forward | exhaust |
| `QueryLogsByAttribute` | LOGS_<T>_ATTR or LOGS_EVENT + LoadRecordAsync | AllOf(sc, k, v) | reverse | `limit` distinct event_id |
| FTS roles | MESSAGES_TEXT `IndexScanByTextToken`, positioned skip over (role) groups | as messages_query.go:202-240, minus tenant | forward | — |
| FTS match | `TextScan(MESSAGES_TEXT, (role, shard))` × 16 concurrent | messages_query.go:340-372 | — | — |
| FTS "all" across parts | MESSAGES_LARGE + `TextScan` within AllOf(writer, seq) | messages_query.go:375-435 | forward | exhaust |
| FTS round 1 | MESSAGE_TRACES records | PK prefix (writer, seq), 256 in flight | — | — |
| FTS round 2 | SPANS_REF_CHUNKS | prefix (trace_id), 256 in flight; unpack refs; filter window/attribute/first-turn | — | — |
| dedup read | MESSAGES_HASH | AllOf(content_hash), serializable | — | — |

**Top-K.**

- A reverse attribute or time scan yields (start desc, trace desc, span desc), and the first entry of each trace carries its `Latest`.
- So the first K distinct traces in scan order equal the oracle's order (`ExpectedContext`'s oracleHeap: Latest desc, trace desc). The early stop needs nothing beyond index order.
- `order=trace` and posting digests go. Long-string candidates count toward K only once verified.

**Budgets.**

- A candidate is one index entry, record or probe row read; bytes are key+value bytes (`ScanLimiterState`, raw KV, or a record's `KeySize+ValueSize`).
- Exceeding a budget gives `QueryBudgetError` with `Candidates`/`Bytes` filled in and no continuation. Results are never truncated.
- Exact N values change: tests pin the N−1/N edges from the new layout, and `benchQueryBudget` (bench.go:217-234) is rewritten (phase 3A).

**Tokens (v2).**

- Format: `tuple(2, kind, database_path, schema, digest, last)` + `HMAC-SHA256(secret, "otel/continuation/v2\0"‖payload)`, at most 4,096 bytes.
- `last`: `(start_ns, trace_id, span_id)` for `same_span`; `(start_ns, trace, span, attribute key, writer, seq)` for `message_fts`.
- The secret is the 32-byte `STORE_META("continuation_hmac")` row, created lazily as `ensureContinuationSecret` does.
- Unchanged: digests (query_advanced.go:202-213; messages_query.go:166-182), rejection kinds, and one transaction per page with `ReadVersion`. v1 tokens are refused as "unsupported continuation token".

**Order changes (D3).**

- Equal (start, trace) ties emit span IDs descending (index order) in same-span and FTS pages. `QueryLogsByAttribute` ties on `effective_ns` emit event IDs descending.
- The `Order` fields of `SpanQueryPage`/`MessageFullTextPage` are removed.

### 3.3 SQL vs direct

- **Direct scans** serve every Exporter path and `otelbench --sql-query direct`. These need budgets, signed tokens, single-transaction snapshot pages and early top-K. SQL autocommit pages share no snapshot and have a 0.63–1.04 ms floor against 0.43 ms direct (benchmark doc round 2; docs/compatibility.md:409-413).
- **SQL** serves:
  - ad hoc filters and aggregations;
  - joins: spans⋈logs through `LOGS_TRACE` (`FlatMap` + `IndexScan(LOGS_TRACE,[=,=])`, pinned in sqltest/otel); messages⋈message_traces; later, metrics;
  - Java readers.
- **SQL FTS** plans as a text access only in the form `TEXT_CONTAINS_*(content, ...) AND role = ? AND shard IN (0..15)`. An unbound shard falls back to record filtering (TODO.md "TEXT SQL access paths"). As implemented, `messages_text` uses the default tokenizer (below), so SQL needs no tokenizer argument or version.
- **`sqlbackend`** keeps `--sql-query sql` as the SQL arm. `direct.go` is replaced by the shared Exporter read kernel, so benchmarks measure production code.

### 3.4 Tenancy and ownership

| Old | New |
|---|---|
| One store per run; integer `tenant` leads every PK | One schema store per tenant; the tenant column disappears |
| JSON manifest re-read in every transaction (fdb.go:205-225; corpus.go:391-460) | The catalog pins the template. `OpenTenantStoreCompiled` refuses an otelv1pb/template mismatch (otelsql/store.go:63-85). `Builder(rc).Open()`, not `CreateOrOpen`, fails on a dropped store. `STORE_META("layout")` records the messages mode and is checked once at open; a mismatch gives `OwnershipError`, as corpus.go:441-446 does. |
| `Drop` clears the run (fdb.go:318-335) | The Exporter has no Drop. `sqlbackend.Drop` drops only `/OTELBENCH/R_<RUN>` (backend.go:798), so production stores are never cleared (TODO.md:3586-3593). |

A stale `OTEL_V1` on a dev cluster gives `OwnershipError`, because the stored template proto differs from the built one. The remedy is a fresh cluster.

## 4. Template gaps and the new DDL

### 4.1 Gaps in today's template.go:30-90

1. **No TEXT index on `messages.content`** (docs/compatibility.md:377-380).
   - The tokenizer `otel_message_v1` lives in package `otel` (messages_text.go:239); every store opener needs it registered, so it moves to `otelsql`.
   - The FULLTEXT grammar has no options (RelationalParser.g4:176; embedded/text_index_ddl.go:16-92). Today a tokenizer can only be set by mutating built metadata (sqltest/plan/fulltext_tokenizer_fdb_test.go:102-106).
2. **No covering reference index.** `spans.message_refs` is unpacked and unindexed. One index entry per reference is the quadratic layout already abandoned (the `"fulltext"` layout, README.md:636-639): turn k references its whole history. Packed `ref_chunks` keep one entry per span chunk.
3. **No MESSAGES_LARGE index** (`spreadAllMatches`) and **no row** for the writer counter or the HMAC secret.
4. **Log indexes lack covering `trace_id`/`span_id`** for existential witnesses. There is no `event_name` index and no long_str/double/bool log index; old logs indexed every canonical value up to 64,000 bytes.
5. **`any_attrs` JSON is non-deterministic** (protojson), so it is not equality-safe.
6. **Not needed for parity:**
   - a `parent_span_id` index: the graph reads the trace's records;
   - a covering trace index: `Trace` loads records today as well;
   - event/link attribute indexes: they are not indexed today, and a test pins that.

   `spans_parent (trace_id, parent_span_id, span_id)` is a later SQL convenience.
7. **No metrics tables.** These are a follow-up (§4.3).

### 4.2 DDL diff (in-place rewrite of `TemplateBody` plus a Go-only overlay)

```diff
-CREATE TYPE AS STRUCT message_ref (attribute BIGINT, writer BIGINT, seq BIGINT, newest BOOLEAN, ordinals BIGINT ARRAY)
+CREATE TYPE AS STRUCT message_ref_chunk (chunk BIGINT, refs BYTES)
 CREATE TABLE spans (... events span_event ARRAY, links span_link ARRAY,
-  message_refs message_ref ARRAY, otlp BYTES, PRIMARY KEY (trace_id, span_id))
+  ref_chunks message_ref_chunk ARRAY, otlp BYTES, PRIMARY KEY (trace_id, span_id))
+CREATE INDEX spans_ref_chunks AS SELECT s.trace_id, s.start_ns, s.span_id, c.chunk, c.refs
+  FROM spans AS s, (SELECT chunk, refs FROM s.ref_chunks) AS c
+  ORDER BY s.trace_id, s.start_ns, s.span_id, c.chunk, c.refs
-CREATE INDEX logs_str_attr AS SELECT a.scope, a.attr_key, a.str_value, l.effective_ns, l.event_id
+CREATE INDEX logs_str_attr AS SELECT a.scope, a.attr_key, a.str_value, l.effective_ns, l.event_id, l.trace_id, l.span_id
   FROM logs AS l, (SELECT scope, attr_key, str_value FROM l.str_attrs) AS a
-  ORDER BY a.scope, a.attr_key, a.str_value, l.effective_ns, l.event_id
+  ORDER BY a.scope, a.attr_key, a.str_value, l.effective_ns, l.event_id, l.trace_id, l.span_id
 (logs_int_attr: same change)
+CREATE INDEX logs_long_str_attr AS SELECT a.scope, a.attr_key, a.digest, l.effective_ns, l.event_id, l.trace_id, l.span_id
+  FROM logs AS l, (SELECT scope, attr_key, digest FROM l.long_str_attrs) AS a ORDER BY <same columns>
+CREATE INDEX logs_double_attr ... (double_value)      CREATE INDEX logs_bool_attr ... (bool_value)
+CREATE INDEX logs_event AS SELECT event_name, effective_ns, event_id, trace_id, span_id FROM logs
+  ORDER BY event_name, effective_ns, event_id, trace_id, span_id
+CREATE INDEX messages_large AS SELECT "L", m.writer, m.seq FROM messages AS m, m.large AS "L"
+  ORDER BY "L", m.writer, m.seq
+CREATE TABLE store_meta (name STRING, int_value BIGINT, bytes_value BYTES, str_value STRING, PRIMARY KEY (name))
 -- Go-only overlay (TemplateOverlay), appended by TemplateDDL(), never sent to Java:
+CREATE FULLTEXT INDEX messages_text ON messages(content) PARTITION BY (role, shard)
```

Superseded during 0A: the plan's `OPTIONS (TOKENIZER = 'otel_message_v1', TOKENIZER_VERSION = 0)` would have made the whole schema unreadable to Java, whose store opener refuses an unknown tokenizer (`42000 MetaDataException unrecognized text tokenizer`, DIVERGENCES.md). The overlay uses the default tokenizer instead, and writers store `otelsql.IndexableText(content)`, which replaces each token over 1,024 bytes with a placeholder, so the default tokenizer emits the tokens and positions `otel_message_v1` would. The `OPTIONS` grammar stays as a Go extension.

Unchanged: `messages`, `messages_hash`, `message_traces`, the `spans_*` attribute/time/span-ID indexes, `logs_trace` and `logs_time`.

- **Unnest form.** `messages_large` uses the unnest form Go already pins (sqltest/plan/index_definition_production_fdb_test.go:132). The rfc204 golden must prove Java accepts it. If not, use the partial index `... FROM messages WHERE parts > 1` (Go pins `WHERE` at :130), or move the index into the overlay.
- **Grammar.** Extend `textIndexDefinition` with `OPTIONS (TOKENIZER = str, TOKENIZER_VERSION = int)`, setting `IndexOptionTextTokenizerName`/`Version`. This is a Go extension of already Go-only DDL. It needs a sqltest/plan test, a DIVERGENCES.md entry, parser regeneration and updates to `TestFDB_OtelTemplateV1`.
- **Java coverage.**
  - The rfc204 golden compares the Java build of `TemplateBody` only.
  - A new Java probe opens a store created with the overlay and SELECTs spans and logs.
  - Java writers to `messages` stay unsupported: they would have to apply `IndexableText`'s overlong-token placeholder to match Go's index entries (docs/compatibility.md).
- **NULL entries.** `logs_trace`/`logs_event` keep NULL entries for uncorrelated or unnamed logs: one more KV per log than today (D11).
- **Hash domain.** Becomes `otel_msg_v2\0‖encoding‖0‖element`; the store is the tenant now.
- **Codegen.** Regenerate `otelv1pb` (otelsql/cmd/genotelv1). Add `LOGS`, `MESSAGES`, `MESSAGE_TRACES` and `STORE_META` builders plus the §3.1 projection options to `BuildCompiled`.

### 4.3 Metrics (follow-up, outside this plan)

The tables go in the same store. This is a sketch, to be revised against the OTLP metrics data model:

```sql
metric_series (series_id BYTES, name STRING, service_name STRING, kind INTEGER, unit STRING,
               <typed attr arrays>, otlp_meta BYTES, PRIMARY KEY (series_id))
metric_points (series_id BYTES, time_ns BIGINT, start_ns BIGINT, int_value BIGINT, double_value DOUBLE,
               <histogram/summary fields>, exemplars exemplar ARRAY, PRIMARY KEY (series_id, time_ns, start_ns))
```

| Index | Columns |
|---|---|
| `metric_series_name` | `(service_name, name, series_id)` |
| `metric_points_time` | `(time_ns, series_id)` |
| `metric_exemplar_trace` | fan-out `(e.trace_id, e.span_id, p.time_ns, p.series_id)` |

TODO.md:3651-3655 calls this "template v2". Adding tables later is an allowed evolution (relational_evolution_validator.go:10-15).

### 4.4 After release: evolving schemas

Not part of this replacement. Once stores must be kept, every change is a new template version:

1. `fleet.SaveTemplate` (pkg/relational/core/fleet/migrate.go:61) saves it in one catalog transaction through the evolution validator. The validator allows added tables and columns and forbids removals and type changes (relational_evolution_validator.go:10-15); an index change is a new index name.
2. `fleet.Migrate`/`MigrateToLatest` (migrate.go:112, :205) rebind each tenant schema in its own resumable transaction.
3. `fleet.BuildIndexes` (build.go:168) builds the added indexes.
4. Regenerate `otelv1pb`. The Exporter refuses schemas pinned below its generated version through the `WithLocalFileDescriptor` validation (otelsql/store.go:63-85).

### 4.5 Error mapping for new failure sources

| Failure | Error |
|---|---|
| Missing schema or database after open, dropped store, stored-template or pinned-version mismatch, `store_meta` layout mismatch, unreadable index | `OwnershipError` |
| Malformed row, index entry, ref chunk or manifest; record and index disagree | `CorruptionError` |
| `api.Error` `ErrCodeInvalidPath` from `tenantPaths` | `ConfigError` |
| FDB and Record Layer errors | `exporterTxnError`, unchanged |

`IsPermanent` is unchanged.

## 5. File layout of the new implementation (package `otel`)

The API types live in `otel`, and `otelsql` must not import `otel`, so the kernel stays in package `otel`.

**New storage files:**

| File | Contents |
|---|---|
| `tenant.go` | `tenantStore`: open/ensure; `store_meta` layout, writer counter and HMAC rows; `run(op, fn)` via `runTxnSlot`; scan, probe and load helpers with `queryBudget`; error mapping |
| `exporter_rel.go` | `relExporter`: lifecycle, config, `ExportResourceSpans` orchestration |
| `spans_rel.go` | projection, admission, batching, save |
| `query_rel.go` | exported `SpanReader` (Query/Trace/Span/Time, `LastQueryWork` for otelbench), shared with `sqlbackend --sql-query direct` |
| `logs_rel.go` | logs |
| `messages_rel.go`, `messages_query_rel.go` | message storage and full-text queries |
| `query_advanced_rel.go` | same-span, existential, graph, v2 tokens |

**Storage-free files** extracted first (phase 0B); both implementations use them unchanged:

| File | Contents |
|---|---|
| `attrs.go` | `canonicalAttributes`, typed values, canonical JSON |
| `messages_codec.go` | decompose, manifests, uses, ref packing, segmentation, extraction, plans, queue and writer bookkeeping |
| `query_plan.go` | plans, digests, `timeWindow`, validation, token sign/parse core, `buildTraceGraph` |
| `backend.go` | `Backend`, `Statistics`, `Size` (from fdb.go:16-54) |

## 6. Phased plan

**Shape.**

- Phase 0 runs three tracks on disjoint files.
- Phase 1 builds the kernel; it is small and serial.
- Phase 2 runs four streams. Each adds only new files and integrates through stub signatures fixed in phase 1.
- Phase 3 is one serial PR stack: cutover, then deletion.

**Gates.**

- Every step ends with `just test` green, reporting executed and cached targets separately.
- Benchmarks follow CLAUDE.md "Evidence and measurements":
  - same host on `ssd-redwood-1`, sequential runs;
  - client CPU (user+sys) per span or log, because wall time varied 5×;
  - ≥ 2 runs per arm;
  - logical and prefix bytes;
  - p50/p99 with zero oracle failures.
- Baselines:
  - `otelbench --backend fdb` in the same binary, until 3B;
  - the merge-base binary and receipts, for the collector bench and after 3B.
- G3's result is measured against the merge-base (`archive/otel-relational/merge-base`, `839cc5d3f`, with the query-only harness patched in), for the collector bench and the otelbench arm (benchmark doc, "G3 at the merge-base"). So is G2B's log ingest, with the custom-store arm of its benchmark patched in at the merge-base (benchmark doc, "Gate G2B"). The earlier gates' custom-store arms are not the merge-base: G2A, G2B's first comparison and R1/R4 ran both stores in one binary, and the first G3 run and the quiesced gate ran the cutover's parent, `archive/otel-relational/custom-store` (`7fb269417`), so both arms of those comparisons share one Record Layer, client and relational tree. The benchmark doc's "Measured trees" and "Baseline" paragraphs name the gates' trees. Rounds 1 to 3 are history with no recorded tree; G0's and G2A's thresholds come from round 2's figures.

### Phase 0: foundations (0A ∥ 0B ∥ 0C)

**0A. Template and rows**

- Files: `otelsql/*`, `otelv1pb`, genotelv1, the parser grammar and `text_index_ddl.go`, sqltest/otel, sqltest/plan, conformance.
- Changes:
  - the §4.2 DDL and FULLTEXT `OPTIONS`;
  - move the tokenizer to `otelsql/tokenizer.go`;
  - add `EnsureTenant(ctx, *FDBDatabase, path, schema, configurer)` via catalog actions (as fulltext_tokenizer_fdb_test.go:109-118), in exporter-policy transactions with no SQL driver;
  - compiled builders for every table, projection options, canonical JSON.
- Tests: extend `TestCompiledTypesMatchTemplateDescriptor` and `TestBuildCompiledIsBuild` to every table; builder tests for duplicates, caps, message-key exclusion and ref chunks ≤ 8,000 encoded bytes; `TestFDB_OtelTemplateV1`; FTS EXPLAIN (`TextIndexScan(MESSAGES_TEXT` with `shard IN`); FULLTEXT `OPTIONS` DDL; the rfc204 golden over `TemplateBody`; the Java probe of an overlay store; `EnsureTenant` with two openers, and stale-template refusal.
- **G0:** `otelbench --backend sql` ingest at 50k spans with `zlib-1` is ≤ 1.05× round 2 (283 µs/span, 1,482 B/span).

**0B. Extraction.** Pure moves into the §5 storage-free files; old tests stay green.

**0C. Test helpers.** Move the kept §2.5 helpers into `*_helpers_test.go`.

### Phase 1: kernel (`tenant.go`, `exporter_rel.go` skeleton)

Behaviour:

- open, ensure and layout check;
- lifecycle (`begin`, `Shutdown`, active-call join);
- txn policy via `runTxnSlot`;
- scan, probe and load helpers with budgets;
- `store_meta` HMAC and writer rows (candidate outside the retry; the winner keeps);
- stubs for `persistMessages`, `persistTraceMarkers`, `reconstructMessages`, `spanRefChunks`, `scanLogWitnesses` and `runMessageQuery`.

Tests: unit tests driving every budget-helper arm, with vacuity checks. FDB tests: a layout mismatch, a dropped schema (on the next transaction) and an unreadable index are each ownership; the bootstrap carries the txn policy (port of exporter_bootstrap_policy_test.go:42); an HMAC race between two handles.

### Phase 2: streams

| Stream | Files | Behaviour | Tests | Gate (vs merge-base unless stated) |
|---|---|---|---|---|
| 2A spans | `spans_rel.go`, `query_rel.go`; `SpanReader` replaces `sqlbackend/direct.go` | §3.1 span writes; Query, Trace, Span, Time, LoadSpanOTLP (no messages) | `relExporter` copies of §2.1; typed equality (NaN, ±0, long strings with collision verification via a forced-digest seam); index-key admission; replay publishes the final attempt only | **G2A:** `--sql-query direct` suite `all` at 50k spans: every class p50 ≤ `fdb` production, ingest ≤ 353 µs/span, bytes ≤ `fdb` |
| 2B logs | `logs_rel.go` | §1.6 on the §3.1 row | §2.4; uncorrelated logs; `scanLogWitnesses` | **G2B:** collector-bench logs and `correlated_logs_by_trace`: CPU/log, bytes/log and p50 each ≤ 1.10× |
| 2C messages | `messages_rel.go`, `messages_query_rel.go`; uses 2A's `spanRow` hook | §1.5 on MESSAGES, MESSAGE_TRACES, MESSAGES_TEXT, MESSAGES_LARGE, SPANS_REF_CHUNKS, STORE_META; `messageTxnHook` stages (fdb.go:70) on `tenantStore` | §2.3 | **G2C:** collector bench (deltas + FTS): ingest spans/s, CPU/span, persist wall, `fts_*` p50/p99 and storage each ≤ 1.10×; ref-chunk key bytes measured (R1) |
| 2D advanced | `query_advanced_rel.go` | span clauses, graph and tokens first; log and message clauses after 2B/2C | §2.2 plus the pure plan, digest and graph tests | **G2D:** `same_span_conjunction`, `trace_existential_3way`, `trace_graph` p50 ≤ 1.10× |

**Parallelism.** 2A, 2B, 2C (write side) and 2D (span part) start together. Only `exporter_rel.go` (owned by 2A) joins them, through the phase-1 stubs. No phase-2 PR edits an old file.

### Phase 3: cutover and deletion (serial)

**3A. Cutover**

1. Rename `relExporter` → `Exporter`.
2. Trim `ExporterConfig`/`DefaultExporterConfig` (§1.1). Make `CanonicalAttribute` typed. Remove `queryMu` and the page `Order` fields.
3. Collector:
   - drop the layout keys from defaults and YAML;
   - `CollectorBenchConfig`/`RunBenchmark` lose `PostingValueFormat`, `HashBytes` and `Order`;
   - rewrite `benchQueryBudget` from new-layout counts, with a test;
   - trim the `MessageStoreStats` fields in `addExportStats`.
4. The ported test copies replace the originals; adapt the §2.5 collector, bench and otelbench tests.
5. `FDBBackend` now survives only behind `otelbench --backend fdb`, as the same-binary baseline.

**G3 (final):**

- collector bench vs merge-base, with defaults (deltas + FTS) and with messages off;
- otelbench `sql --sql-query direct` vs `fdb` production;
- both measured at the merge-base in the benchmark doc's "G3 at the merge-base": every class is within the thresholds; persist wall is 1.36×, and the summed message transaction time, its criterion since G3, is 0.98×;
- every class within the G2 thresholds;
- `TestRunBenchmarkRealFDB` exact rows.

The query classes are gated quiesced (owner decision, 2026-11-07, after the
`fts_single_token` analysis). Each ingested run is queried with
`collector-bench --query-only` once the storage and log queues have drained
below 1 MB. The default post-ingest phase queries while the storage server
still drains the ingest, so its numbers are reported but not gated.

**3B. Delete**

| Area | Removed |
|---|---|
| package `otel` | <ul><li>`fdb.go` (except the backend.go types)</li><li>`index.go`</li><li>`record.go` (`FixtureOTLP` → corpus.go)</li><li>`keyspace.go`</li><li>`ingest.go`, `ingest_profile.go`, `ingest_timeout.go`</li><li>the old `intrinsic.go`</li><li>Brotli in `record_compression.go`</li><li>the old storage methods in `messages*.go`, `logs.go` and `query_advanced.go`</li><li>the exporter.go envelope and fidelity code</li></ul> |
| tests | the §2 LAYOUT tests and helpers |
| `Config`/`Manifest` | `PostingValueFormat`, `KeyLayout`, `Messages`, the FDB `SaveMode` checks; `HashBytes`/`Order` if ClickHouse no longer needs them |
| otelbench | `--backend fdb`, `--posting-value-format`, `--hash-bytes`, the Brotli values; `collector-bench` stops being fdb-only |
| bench | the `local_campaign.sh` arms and their test; the `bench/ops` drivers (D7) |
| docs | <ul><li>README sections</li><li>docs/compatibility.md:386-403 becomes the new writer contract</li><li>the RFC-283 status</li><li>TODO.md: close the delivered "OTel SQL ingest migration" items; keep metrics, two-level fan-out and the Java BYTES comparator</li></ul> |

Then run `just gazelle` and confirm every remaining target has a nonempty population.

## 7. Risks and owner decisions

### Risks

| # | Risk | Mitigation and evidence needed |
|---|---|---|
| R1 | `SPANS_REF_CHUNKS` puts packed refs in **index keys**: up to ~8 KB encoded, where today they are values. This can cost write CPU and storage, and 0x00 escaping halves the chunk capacity. | G2C measures bytes and CPU. Fallbacks in order: a Record Layer `KeyWithValue` overlay index (values, like index.go today); then a `span_refs` table `(trace_id, start_ns, span_id, chunk)` with explicit replacement deletes. A per-reference index is excluded (quadratic). |
| R2 | **Java compatibility of the Go-only FULLTEXT overlay and custom tokenizer.** The golden cannot cover the overlay; Java writers to `messages` need `otel_message_v1`; how Java treats a store whose metadata has a TEXT index is unproven. | A Java probe in 0A. Document the boundary in docs/compatibility.md and DIVERGENCES.md. Outcome: Java refuses a schema naming the custom tokenizer (DIVERGENCES.md), so the overlay uses the default tokenizer with `IndexableText` (§4.2) and Java reads every table. |
| R3 | **Semantic drift hidden by test rewrites:** typed equality, order ties, budget N values, log `StoredBytes`, token format. | Ports change assertions and never drop them. Each changed expectation cites D1/D3. The oracle-checked otelbench suites stay at zero failures as an independent check. |
| R4 | **Message writes in a shared store.** Span batches with `CoalesceIndexWriteConflicts` now share a store with TEXT BunchedMap writers, so group-commit throughput and conflict rates may move. `messagePipelineSizer` (messages.go:136-143) must still be set on the builder. FULLTEXT maintenance cost through relational metadata is unmeasured. | G2C with retry and conflict counters (`MessageStoreStats.Retries`). A test that the widened conflict ranges cover only SPANS indexes (README.md:715-720). |
| R5 | **Measurement validity.** After 3B there is no same-binary A/B, and wall time varies 5×. Long strings add record fetches; logs add KVs (`logs_time`, NULL entries). | CPU and byte gates with ≥ 2 runs per arm. Keep `--backend fdb` until G3 passes. Archive the merge-base receipts (done: `g3-merge-base/` in the benchmark doc's receipts). Never extrapolate. |
| R6 | **Per-tenant schemas:** catalog rows, store headers, `EnsureTenant` races and first-open cost at scale. | The 0A two-opener test. Measure fleet cost before any multi-tenant claim. |

### Owner decisions

| ID | Decision | Recommendation | Alternative |
|---|---|---|---|
| D1 | Attribute equality | **Typed**: string/int/double/bool are indexed; bytes/json are refused as not indexed | Keep canonical-string coercion. This adds `canon_attrs`/`long_canon_attrs` arrays and indexes and a 2-way merged scan on every string query. |
| D2 | Config keys | Remove `order`, `hash_bytes`, `posting_value_format`; keep `bulk_index_conflicts`; `record_compression` none\|zlib-1\|zlib-default, default zlib-1; refuse Brotli | — |
| D3 | Tie order | Index order: span/event IDs descending | Buffer each tie group to keep the ascending order |
| D4 | Message references | Packed `ref_chunks` with a covering index | The R1 fallbacks. This also decides whether per-span references are SQL-visible. Kept after R1: `SPANS_REF_CHUNKS` costs 2–3% more bytes than the old reference values, and MESSAGES_TEXT's 21–23% more bytes than the old TEXT index (the longer schema prefix and the record type key in every entry) were accepted at the cutover. |
| D5 | Template | Rewrite `OTEL_V1` v1 in place with a Go overlay and a FULLTEXT `OPTIONS` grammar extension | Set the tokenizer option programmatically, losing SQL-DDL creation |
| D6 | ClickHouse comparison backends | **Keep**; they need only the moved `Backend`/`Statistics`/`Size` and `Config`/`Manifest` | Delete them with their tests and README sections |
| D7 | `otelbench --backend fdb`, `bench/ops/{ingest20k,horizontal}` | Keep `fdb` as the baseline until G3, then delete. The old benchmark tooling is retired (`ingest20k` removed; `horizontal` kept only as the library the Kubernetes run imports); Kubernetes is the method, so there is no port of `bench/ops` | — |
| D8 | Tenant provisioning | `NewExporter` auto-creates `/OTEL/R_<RUN>` and `T<tenant>` | Explicit provisioning; `NewExporter` refuses a missing schema |
| D9 | Metrics tables | A separate follow-up (§4.3) | Fold them into this pre-release rewrite to avoid a first evolution |
| D10 | Message flags | Keep both booleans, recorded in `STORE_META("layout")`; a change is refused | One key, `messages: none\|deltas\|fulltext` |
| D11 | NULL index entries in logs | Keep full `logs_trace`/`logs_event` | Partial `IS NOT NULL` indexes if Java accepts them; decided by G2B bytes |
