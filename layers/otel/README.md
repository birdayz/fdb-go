# otel: an executable immutable OTel-span experiment

This is a **prototype**, not an OTLP receiver or a production tracing layer. It
compares real Record Layer custom-index queries with two legitimate ClickHouse
representations. It does not establish an end-to-end, HA, durability-matched,
open-loop SLO, compression, or production win merely by running successfully.
The design/research lives alongside the code under `rfcs/`, `reports/`, and
`research_notes/`.

**Final PoC target: 10,000,000 stored spans.** The owner reduced the original
100M target to 10M. Complete the 10M validation/comparison and stop; no 100M load
or further scale-up is in scope. Historical measurements and failed arms remain
unchanged.

## Implemented semantic class

* Fixture **V2**, configurable seed/population/tenants/attribute count/selectivity.
  Exactly two adjacent source spans per trace (an odd final span is a singleton).
  Keyed bijective counter mixing guarantees unique nonzero span IDs and trace
  group IDs; these synthetic randomized-looking IDs are not cryptographic IDs.
  Counter-based SHA-256 timestamps have second resolution and permit ties.
  Scoped string/int attributes include literal dotted keys, same-name other
  scopes, integer/string lookalikes, repeated matching spans, and absent values.
* Real `FDBRecordStore.SaveRecord(proto.Message)` through a dynamic envelope
  containing complete synthetic OTLP `ResourceSpans` wire bytes. The registered
  `IndexMaintainerFactory` synchronously maintains custom secondary postings in
  `Update(old,new)`. WRITE_ONLY explicitly invokes the custom full-maintenance
  implementation, not an embedded standard-index method. Prefix deletion is refused; whole-store deletion clears
  the owned secondary structure. Core seams validate registered factory options
  and support external WRITE_ONLY maintenance; the SQL planner now fails closed
  for opaque index layouts. The typed OTel APIs are not SQL catalog tables.
* Typed/scoped exact string/int equality, newest **K DISTINCT traces** ordered by
  latest matching span timestamp descending, then trace ID descending. Full
  tenant-qualified trace lookup returns span identities/timestamps in ascending
  timestamp/span order. Both systems return the same narrow metadata result shape,
  not full span/content output. FDB trace lookup still fetches primary records and
  decodes their OTLP payloads. Standalone tenant-qualified span-ID lookup returns
  every matching `{trace_id,span_id,start_ns}` ordered by time/trace/span ascending,
  without a trace hint and without K truncation. Intrinsic span-time lookup uses
  `[start_ns,end_ns)` and returns newest K distinct traces by maximum timestamp
  of a matching witness, not the trace's latest nonmatching span. Both intrinsic
  queries use covering built-in Record Layer VALUE indexes; attribute postings
  remain a real synchronous custom maintainer.
* Covering postings retain the original scope/key/type/value and timestamp;
  shortened SHA-256 digests are candidates only. Verification never needs a
  primary fetch for supported values. An occurrence suffix prevents two colliding
  attributes on the same span from overwriting one another. Values exceeding the
  coverage cap reject; there is **no unimplemented fetch fallback**. SHA-256 here
  is a synthetic benchmark encoding, not the RFC's tenant-keyed privacy design.
* Independent expected results inspect generated source attributes, without the
  OTLP extractor, hashes, physical keys or encoded CH Map keys; expected ordering
  has a separate implementation. Each timed query checks identities and order.
  Source audits check every record, ordinal uniqueness/completeness, and source
  fields against the fixture. FDB counts attribute and both intrinsic index
  keys/logical bytes. Expected answers combine the two source witnesses before
  maintaining a bounded K-hit heap per unique predicate: no map of all matching
  traces is retained. Known trace/span needles use constant-size source witnesses.

The original immutable-span experiment excluded logs, message content and graph
joins; those exclusions describe its historical measurements, **not** the current
APIs below. Still excluded from the production claim: general OTLP partial-success
admission, privacy services, retention/receipts/revision lifecycle, replay
protocols, concurrent writes during measurement, general production continuations,
numeric range search, TraceQL/session joins, online-backfill race certification,
query ACLs, HMAC epochs, normalized resources and service SLOs. The
maintainer's update/delete behavior is tested, but CH incremental MVs are
**append-only unique-corpus comparisons**, not a correction/delete equivalent.
There is no claim that the prototype implements all RFC-283 requirements.

At eight or more attributes, fixture slots 4..7 are span string attributes
`gen_ai.operation.name=chat`, `gen_ai.provider.name=openai`,
`gen_ai.request.model=gpt-4`, and `gen_ai.response.id=chatcmpl-fixture-v2-<spanID>`.
The pseudorandom unique response value avoids an ascending counter-string
compression advantage. Its uniqueness is only a fixture promise, not a provider
or semantic-convention guarantee. These OTel-authored **Development** names are
pinned to [GenAI conventions e19717d](https://github.com/open-telemetry/semantic-conventions-genai/blob/e19717d65f890955777b044bca2a3cd71ae8b498/docs/registry/attributes/gen-ai.md);
there is no invented standardized `gen_ai.request.id`. Smaller 4..7-attribute
fixtures retain their original controls/fillers and cannot run `genai`/`all`.

V2 changes source identities, payloads and maintained structures. Use fresh runs;
V1 receipts/binaries (including the initial ingest-profile commits) remain
historical evidence, not compatible V2 measurements. No storage compatibility
or direct before/after performance claim is made across this fixture change.

## Current logs and joined-query APIs

These are typed OTel APIs over this experiment's run/tenant layout, not SQL catalog
tables or proof of an upstream Collector receiver pipeline.

- `ExportLogs` stores complete single-log OTLP envelopes; `ConsumeLogs` adapts OTLP
  resource logs. Missing EventIDs receive receiver-generated UUIDs outside retries;
  replay with the same explicit EventID replaces the prior log atomically and
  retracts old postings/chunks. A resent request without IDs can create duplicates.
- `logs_v1` holds header/optional chunk rows by tenant/EventID; `log_trace_v1`
  correlates trace/span/effective-time/EventID; `log_eq_v1` holds scoped exact
  attribute postings with verified covering values. Body bytes are not TEXT indexed.
  Effective time uses event time, else observed time, else receiver time, preserving
  the original timestamp and `TimeSource` (`event`, `observed`, `receiver`).
- `QueryLogsByTrace(traceID, optionalSpanID)` returns correlated logs ascending by
  effective time/EventID. `QueryLogsByAttribute(attribute, limit)` returns newest
  first, including uncorrelated logs; zero limit uses configured K. Attribute
  scopes are `resource`, `scope`, `log` and intrinsic `event_name`.
  Queries enforce configured candidate/logical-read-byte and context budgets;
  exhaustion returns a typed budget error, not a silently truncated complete set.
- Log admission caps raw/stored bytes, each actual encoded covering value at
  100,000 bytes, and native transaction mutation/conflict costs at 8,000,000 bytes,
  including old replacement clears.
  All initial/preflight admission happens before any batch writes; admitted batches
  commit independently. Concurrent replacement changes are rechecked in each batch;
  a prior image growing beyond the live budget returns `OperationError`, not the
  no-partial-write preflight `AdmissionError`. Such a runtime failure does not
  roll back earlier committed batches.
- `QuerySameSpan` requires every predicate on one span; predicate zero drives the
  posting scan (put the selective predicate first). It supports a time window,
  limit and authenticated run-bound snapshot-page continuation, not a global
  stable snapshot across calls.
- `QueryTraceExistential` allows different span/log/message witnesses in the same
  trace and returns the verified witnesses per clause, with time/limit/budget bounds.
  `TraverseTraceGraph` reads one bounded trace and reports observed parent/child
  adjacency, roots, missing parents/orphans, cycles/depth and OTLP links. Cross-trace
  link targets stay unresolved; missing ancestry is not invented.

**Deferred:** actual upstream Collector ingestion into the **same relational
catalog/tenant SQL tables** has not been migrated. Exporter adapters, typed joins,
SQL/index safety prerequisites and upper-stack planner work are not that migration.
The pending item is tracked in `TODO.md`; this source work does not execute it.

## Record Layer bulk saves and compression

`--save-mode=batch` uses `StoreBuilder.PrefetchRecordBatch` to overlap snapshot
cache warming with store opening, then `FDBRecordStore.SaveRecordBatch`:
authoritative old-record loads are pipelined, and every record still passes through the store serializer,
record-count handling and synchronous old/new index maintenance. Transaction
ownership, 1500-ms timeout, one-second save-plus-guard limit, 300-KB conservative
batch estimate and 500-KB actual transaction guard are unchanged.

`--record-compression` selects `none`, `zlib-1`, `brotli-0`,
`brotli-1`, `brotli-3` or `brotli-4` for fresh `--backend fdb` runs. Ordinary
offline `ingest` defaults to `none`. The `Exporter`, and so `collector-bench`,
accepts only `none`, `zlib-1` and `zlib-default` (Java-readable version-1
records) and defaults to `zlib-1`; a Brotli value is a `ConfigError`, because SQL
and Java read its tenant store (see "Exporter configuration").
It configures the Record Layer's
`TransformedRecordSerializer` through `StoreBuilder.SetSerializer`, before normal
split-record storage. OTLP messages, envelopes, primary keys, eight complete
covering postings and both native indexes are unchanged. This setting compresses
**primary record values only**, not posting values or keys. Its byte reduction
must not be presented as the same reduction in total storage or physical I/O.
The compression choice is immutable run ownership, not part of source-corpus
identity. Use the same choice for ingestion, reads, audits and explicit drops;
a mismatch is refused before accessing the run.

`--posting-value-format=tuple` (`--backend fdb` only: the `Exporter` has no
posting values) stores each complete covering projection as a
standard FDB tuple `(scope,key,type,value,start_ns)` instead of JSON. This removes
repeated JSON field names without shortening the original values or changing any
posting key. It is an immutable fresh-run index option; query decoding uses the
maintainer's metadata, not a caller-provided format guess. The default remains
legacy JSON. Public trace and log exports reject the entire call with
`AdmissionError` before any writes if an indexed projection's actual encoded
covering value exceeds FDB's 100,000-byte limit. This is separate from the decoded
64,000-byte projection cap: tuple NUL escaping and JSON NUL/HTML escaping expand
values. Neither raw OTLP nor admitted projections are truncated.

`--bulk-index-conflicts` selects `SaveRecordBatchWithOptions` with conservative
index write-conflict coalescing. Normal implicit write conflicts and all normal
read conflicts remain. Extra write ranges cover only owned index subspaces, never
the record subspace. The client can coalesce contained ranges in eligible commits;
this reduces shipped conflict-vector overhead, not stored posting count or bytes.
The live transaction-size estimate does not perform this coalescing and can grow
slightly, so the unchanged 500-KB guard still applies. This execution option is not
part of stored layout identity. It applies to `--backend fdb`, and to
`--backend sql --save-mode batch`, whose span writer is the `Exporter`'s
(`bulk_index_conflicts`, on by default). The widened ranges cover only the
indexes the batch maintains, so they never touch the message indexes (see
"GenAI message storage", write conflicts).

Zlib uses the existing Java-compatible version-1 format. Brotli explicitly uses
`SetGoOnlyBrotliCompression` and version 2: **Java and default Go serializers
cannot read those records**. Use it only for Go-only stores. The Record Layer
checks lengths and checksums, retains clear values when compression does not
shrink them, and continues to decode and validate old records before replacing
or deleting them. See [compatibility](../../docs/compatibility.md) and
[the format boundary](../../DIVERGENCES.md#brotli-record-compression-go-only-extension).

The bounded compression integration samples report primary/posting/key bytes
and actual transaction-size estimates for the same source population. They are
correctness and byte-cost measurements, not sustained ingest-rate results.

## Stored-population evidence

The first completed V2 scale gate stored **1,000,000 spans** on 2026-10-27,
using source `c7d00bf`, two batch workers, one tenant and eight indexed attributes.
Ingestion took 908.535 s (about 1,101 acknowledged spans/s), including 32 bounded
fresh-Run timeout recoveries. A strict drain preceded the complete source audit:
1M records, 8M attribute postings and 1M entries in each intrinsic index. All
1,600 initial timed answers and 48,000 answers in three further warm/local rounds
matched the independent source oracle.

Each additional round has 1,000 samples per class. Across those three rounds:

| Query class | Median range (ms) | p99 range (ms) |
|---|---:|---:|
| Trace ID | 0.595–0.603 | 0.996–1.012 |
| Standalone span ID | 0.554–0.561 | 0.769–0.788 |
| Unique GenAI response ID | 0.566–0.571 | 0.747–0.798 |
| Narrow time range | 0.602–0.610 | 0.938–1.052 |

These are sequential native-FDB timings including ownership/store guards, not
production SLOs. All tails remain in the receipts, including a 6.737-ms span
lookup and 6.846-ms common-provider query. The owned census is 3.435 GB of logical
KV; the 2.810-GB whole-volume growth is not isolated per-run physical storage or
a safe 100M multiplier. Earlier failed and monitor-aborted arms remain separate.
**The final 10M capacity/validation gate remains open.** A 100M load is out of
scope; broader production competitiveness is not established.

Receipts: [completed 1M load/audit](bench/evidence/2026-10-27/pilot-1m-v2-workers2-complete/)
and [three warm query rounds](bench/evidence/2026-10-27/query-1m-fdb-warm/).

## One-million-span guarded query comparison

The separate CH campaign tested fresh, source-audited 8192/512/64-granularity
layouts under the same four-CPU/16-GiB resource caps. Three rotated rounds per
layout and transport retained **28,800 oracle-checked answers**, with no failures.
One configuration was selected by the geometric mean of twelve native medians:
trace, span, unique GenAI response and narrow time, across three rounds. **8192
won** (6.297 ms versus 7.343 ms for 512 and 11.486 ms for 64); there is no
per-query layout selection. Granularity 1 was excluded after full-audit EXPLAIN
requests exceeded the unchanged two-second budget, including after three strict
zero-merge observations. Its failed load/audit/drain windows remain separate;
its native-query population is zero, not a fabricated slow result.

The selected native CH arm then completed **three 16,000-answer rounds**, 1,000
per class per round, matching the FDB sample population. Both engines' 48,000
answers matched their source oracles. These are observed ranges across rounds:

| Query class | FDB median (ms) | Native CH median (ms) | CH/FDB median ratio bounds |
|---|---:|---:|---:|
| Trace ID | 0.595–0.603 | 5.420–5.852 | 8.99–9.84× |
| Standalone span ID | 0.554–0.561 | 5.421–5.790 | 9.66–10.45× |
| Unique GenAI response ID | 0.566–0.571 | 6.537–8.131 | 11.45–14.36× |
| Narrow time range | 0.602–0.610 | 6.445–6.596 | 10.56–10.95× |

Ratios are descriptive bounds across the measured round medians, not independent
trial estimates. All sixteen classes, first post-restart CH samples and tails
are retained; no warmup samples were hidden. Timings include ownership checks,
backend requests and decoding: CH performs a separate ownership SQL request,
whereas FDB checks ownership inside its transaction. This is a **guarded query
path result against the best-tested passing CH profile**, not an isolated-index
or globally optimal ClickHouse comparison. FDB used `c7d00bf`, CH `9244882`, both
Go 1.27.3. FDB's command timeout was 20 minutes with diagnostics on; CH's was ten
minutes with diagnostics off and a two-second server-query limit. Deadlines,
durability, acknowledgment batching and record representations are not equated.
This does not establish an ingest win, 100M performance, HA or a production SLO.

After all timing, **76,800 unique data-query IDs** across 21 receipts reconciled
with CH query logs, with no missing, duplicate or error terminals. Those metrics
exclude the separate ownership requests and are not FDB-equivalent total work;
integer-millisecond zero means below one millisecond. Original native metric
fields remain explicitly unavailable, not rewritten as measured zeros.

Receipts: [CH tuning, confirmation and query-log reconciliation](bench/evidence/2026-10-27/ch-1m-eligible3-native-confirmed/).

## Physical arms

Every physical arm uses a fresh run namespace and maintains only its selected
layout. HTTP/native query transports intentionally reuse that same loaded arm;
transport does not change ownership or corpus identity. `--order` changes
ownership metadata, not source corpus bytes.

| Backend | Order | Physical/query strategy |
|---|---|---|
| `fdb` | `trace` | One covering posting per attribute, term then trace/span/occurrence. Exhaust every candidate within budgets, collision-verify, then distinct/global top-K. |
| `fdb` | `time` | Same posting count; term then negative timestamp, descending trace tie key, span/occurrence. Stream in order, verify collisions, stop only after K distinct verified traces establish the frontier. |
| `ch-map` | either | MergeTree ordered by tenant/service/time/trace/span, ZSTD, TraceId and Map-key/value bloom indexes. Map keys injectively encode `(scope,literal key,type)`. Exact paired lookup plus `GROUP BY trace_id,max(time)` and global top-K. This baseline alone cannot establish a competitive win. |
| `ch-attrs` | `trace` | Same source table plus separately costed typed/scoped attribute table maintained by MV, ordered by tenant/scope/key/type/value/trace/span. Covering grouped max/top-K. |
| `ch-attrs` | `time` | Same source/MV population, sorted by term/negative timestamp/descending trace/span. `ORDER BY ... LIMIT 1 BY trace_id LIMIT K`, `optimize_read_in_order=1`; verify actual `EXPLAIN`/server work rather than assume optimization. |

CH stores Map, aligned typed terms, and source JSON so every copy is included in
storage/ingest cost. FDB stores OTLP protobuf plus its custom index, not that JSON
schema. Report those representation differences, not an unexplained byte ratio.
CH ingest statistics retain acknowledged source progress on success **and error**:
rows, source attributes and JSON transport bytes (including each newline) count
only complete source INSERT batches whose HTTP responses succeeded. A failed or
canceled batch is excluded even if some or all of it physically applied, so a
partial physical census can exceed these acknowledged counts. A later ready-
manifest error retains all earlier source acknowledgments without inferring
publication success, retrying or adopting the run. Progress is not a substitute
for a successful ready-state source audit; earlier zero-stat error receipts do
not prove an empty database.

Every FDB arm also maintains built-in `span_id_v2` (tenant/span/time) and
`span_time_v2` (tenant/time/trace) VALUE indexes on every Record Layer save.
Every CH source table gets covering `by_trace`, `by_span`, and `by_time`
projections before load. All their synchronous write work is included in ingest;
there is no uncosted lookup accelerator. Queries enable projection optimization;
audit captures actual EXPLAIN choices, named projection row counts and bytes.
Verify selection and parent/projection byte inclusion before making a trace
comparison or summing storage. All source data is retained; there is no hidden
term-time accelerator in the trace-ordered FDB arm.

### ClickHouse index granularity

`--ch-index-granularity` sets the fresh MergeTree tables' sparse-index row
interval, from 1 to 8192 (default 8192). It is applied at initial creation to
`spans` and, for `ch-attrs`, `attr_lookup`; the three covering projections inherit
the source table setting. Values such as 512 and 64 are tuning candidates, not
claims of better performance; 1 is an exploratory extreme, not the default.
Smaller granules can improve pruning while increasing marks, index bytes and
write work. Adaptive byte granularity and part boundaries also affect actual
rows per granule. Existing tables are never altered: use a fresh run to change
this setting.

CH ownership manifests pin the value. A mismatch, including an older CH manifest
without this field, refuses queries, audits and Drop; there is no implicit
adoption. Every later command must supply the same value as ingest. This setting
does not change the source corpus, FDB manifest encoding, ToolVersion or FDB run
namespace, so existing V2 FDB runs remain compatible. HTTP and native use the same
tables and query semantics.

Receipts retain the requested value in Config. CH audits additionally retain
actual `SHOW CREATE TABLE` output for each MergeTree table, server
`system.tables` engine/settings metadata, active parent-part rows/marks/parts and
all three covering projections' rows/marks/parts. Inspect these observed settings,
marks, storage bytes and plans before describing an arm as tuned; small
correctness profiles are not full-scale performance evidence.

## ClickHouse query transports

`--ch-query-transport=http` is the default. `native` uses the pinned official
`clickhouse-go/v2 v2.43.0` client (`ch-go v0.71.0`) and requires
`--ch-native-addr=127.0.0.1:PORT` (or explicit IPv6 loopback/localhost). Native
mode also requires a loopback HTTP administration endpoint. It reuses HTTP-loaded
tables: do not load a second copy for the transport comparison.

Each timed operation performs ownership **and** data queries over the selected
transport. DDL, ingest, storage audits and EXPLAIN remain HTTP. SQL predicates,
parameters, ordering, limits, projection settings and server read budgets are
shared; only HTTP's `FORMAT JSONEachRow` suffix is removed for native blocks.
Native results scan directly into typed fields and drain through terminal error
and close checks, without a JSON round trip. Both protocols are uncompressed in
this first comparison; native encoding versus JSON/HTTP overhead is deliberately
part of the measured transport difference, not a storage-engine claim.

The native pool persists (four open/idle connections), with a five-second dial
bound and 30-second read/query-context bounds; data SQL retains its two-second
server deadline. Pool creation performs no Ping or hidden warmup. Native client
compatibility and real access paths require the real-CH validation lane; this
source description is not evidence that those tests or a benchmark passed.

Credentials may be inherited internally from the existing HTTP URL, never from
the native address or new password flags. They are absent from Config/receipts
and query IDs; diagnostic errors redact raw/escaped userinfo and nested native
exception details while preserving safe error categories/causes.

Data queries on both transports retain fresh query IDs, including failures after
execution starts. Ownership failures have no data-query ID and clear prior work.
Native server metrics are explicitly `unavailable`: fast queries need not emit
progress, and no callback is assumed to be a complete read-work summary. Native
zero metric fields mean unavailable, not zero engine work. Query-log reconciliation
is a separate future operation outside measurement timing; millisecond duration
quantization must be disclosed if used. HTTP summaries remain labeled
`http_summary` when available. Native decoded-result and HTTP response caps are
both 32 MiB, but decoded bytes and JSON wire bytes are different quantities.

## Build and validation

The owner orchestrates all expensive commands under the shared-machine lock.
After new files, run `just gazelle`; build the CLI with:

```sh
bazelisk build //layers/otel/cmd/otelbench
```

Correctness validation is **`just test`**, not a selected target or `go test`.
`//layers/otel:otel_test` contains unit/property cases and a bounded real-FDB
integration regression. The integration regression starts its own testcontainer,
uses `t.Parallel()` and random owned roots, and never discovers/reuses an existing
cluster. Only the repository's no-Docker runtime skip is allowed. Final evidence
uses `tools/lanes/run-verified-lane.sh "$PWD" fast` on committed clean inputs;
`full` is the separate applicable heavyweight/workstream lane. Do not describe a
fast pass as full verification. No test/build result is asserted by this README.

## Explicit owned namespaces and commands

The CLI never launches containers. Supply an explicitly owned FDB cluster file
or CH HTTP URL. `--run` is required: 8–48 lowercase letters/digits/underscores.
Generate it outside the tool, retain it in the evidence manifest, and use a
**different namespace per physical arm**. No fixed/default prefix is cleared.

```sh
BIN="$(bazelisk info bazel-bin)/layers/otel/cmd/otelbench/otelbench_/otelbench"
RUN="otel_$(date +%s)_$(od -An -N6 -tx1 /dev/urandom | tr -d ' \n')"
# Use identical source flags for every mode/arm. Owner manifests reject a wrong
# corpus/layout/backend on reopen. These are examples, not permission to run.
COMMON=(--run "$RUN" --seed 283 --count 10000 --tenants 10 --attrs 8 \
  --match-every 100 --order trace --save-mode pipelined --revision "$(git rev-parse HEAD)")
FDB=(--backend fdb --cluster-file /absolute/path/to/owned/fdb.cluster)
"$BIN" "${COMMON[@]}" "${FDB[@]}" --mode ingest --output "$RUN.ingest.json"
"$BIN" "${COMMON[@]}" "${FDB[@]}" --mode audit --output "$RUN.audit.json"
"$BIN" "${COMMON[@]}" "${FDB[@]}" --mode query --suite selective \
  --iterations 100 --output "$RUN.selective.json"
"$BIN" "${COMMON[@]}" "${FDB[@]}" --mode query --suite trace \
  --iterations 100 --output "$RUN.trace.json"
"$BIN" "${COMMON[@]}" "${FDB[@]}" --mode drop --confirm-drop \
  --output "$RUN.drop.json"
```

For CH, use `--backend ch-map` or `--backend ch-attrs --ch-url http://127.0.0.1:PORT`
instead of `FDB`. Create a new `RUN`; compare both attribute-table orders in fresh
databases. Select `--order time` for the corresponding FDB experiment too.
CH database names are `otel_<run>`. An FDB run's owner manifest is at
`("otel_otelbench_v2",run,"owner")`. Drop requires `--confirm-drop` **and** matching owner
metadata, and removes only that owned database/root. A failed partial load stays
`loading`; choose a fresh run or explicitly drop it, never resume/replay blindly.

**FDB key layout.** The manifest records the run's key layout, part of its
stored identity. A run created now uses `"key_layout": "directory_v1"`: its data
lives under a Record Layer `KeySpace` path whose application
(`otel_otelbench_v2`) and run directories are the Record Layer's
`DirectoryLayerDirectory`, resolved through the FDB directory layer
(`ExtendedDirectoryLayer.global`) to small integers, with integer constants for the store, the log
subspaces, the message writer counter and the query key, and integer subspace
keys for the span indexes (`typed_scoped_eq_v2` 1, `span_id_v2` 2,
`span_time_v2` 3). A key starts with about 6 bytes instead of about 40. A run
whose manifest has no `key_layout` keeps its tuple prefix
`("otel_otelbench_v2",run)` and index names and works unchanged; a manifest
naming an unknown layout is refused. Drop clears the run's tuple prefix and its
key space path; the directory layer keeps the interned names, as the Java Record
Layer does.

FDB ingestion uses `--save-mode pipelined` by default: immutable protobuf messages
are prepared outside the retry closure, then `SaveRecordsPipelined(..., None)`
retains ordered saves and old/new index maintenance. The API honors the store's
`INSERT` pipeline sizer, but this backend leaves its default depth at 1, so this
mode does not overlap existing-record reads. `--save-mode serial` preserves the one-save-at-a-time baseline with
the same prepared messages; `--save-mode batch` uses `SaveRecordBatch` to issue
all existing-record reads before collecting them, retaining old/new index
maintenance. No mode assumes that an identity is absent or skips old-record
correctness. `--workers` (default 1, range 1..8) bounds concurrent source-batch
transactions. Each Run has an independent record context and store; batches are
disjoint and prepared once outside retries. The queue holds at most `workers`
batches, with at most `workers` running and one being prepared. On failure,
admission stops, the worker context is canceled, and every worker is joined
before return; `ready` is published only after every source batch succeeds.

A source Run reporting FDB `transaction_timed_out` (1031) gets at most eight
fresh Run calls for that same prepared batch, with cancelable waits of
25/50/100/200/250/250/250 ms between calls. This fixed application policy does
not change FDB OnError classification or any transaction/save/size guard. The
1.5-second transaction timeout is armed before initial ownership and all manifest
reads; setting it again while opening a store does not reset its creation-relative
budget. Existing finite internal retries remain separate.

A timed-out commit may already have succeeded or may complete later. Each fresh
Run re-reads ownership and old records and uses normal Record Layer old/new
maintenance, not captured mutations or an absence assumption. A batch contributes
acknowledged rows/bytes only once, after a successful Run. Exhaustion, cancellation
and other errors stop the worker pool; budget/ownership/admission errors are not
recovered by this policy. Initialization and ready publication are neither retried
nor ambiguously adopted: a reported error can still have committed its manifest,
so a command error is not an authoritative persisted-population census. A second
public Ingest still rejects an existing namespace.

Receipts record strategy and worker count; these and diagnostics do not change
source fingerprints or stored layout ownership.

`--diagnostics` adds bounded instrumentation to ingest receipts (default off).
Source counters, phases and timers have scope `source_batches_only` and exclude
initialization and ready publication; preparation includes batches discarded on
cancellation. The separately scoped `client_metrics` snapshots are an explicit
exception described below. `source_runs`
counts Run calls, `source_attempts` counts closure entries, and `source_replays`
counts internal closure entries beyond the first per Run, not transport retries
or RPCs. `source_timeouts` counts source Runs reporting 1031, including exhaustion;
`source_fresh_retries` counts additional source Run calls actually started because
of 1031. Fresh retries are not internal `source_replays`. The optional
`timeout_backoff` phase measures application waits separately from Run time.
The `phase_nanos` preparation/manifest/open/save/guard/run totals overlap across
workers and parent/child phases: do not add them or label them critical-path,
GRV, or server latency. Source attempts issue the owner read before opening the
store, then validate ownership before saving; manifest timing includes the
concurrent store-open interval, and both retain their authoritative reads.
Batch saves also prefetch snapshot record ranges into the same transaction's
RYW cache alongside metadata reads; normal saves still load old images, validate
all persisted layouts, and add their ordinary conflicts. Batch `save` timing
includes prefetch, overlapping metadata work, and normal saves, so it is not
comparable to a save-only interval. The unchanged one-second save+guard bound
starts before prefetch; neither retries nor fresh Runs reuse old-image caches.
Run time includes retry backoff and observer callbacks;
the context-scoped timer does not capture database-policy retry-delay events.
`approximate_mutation_bytes` is the historical receipt field name for the sum of
successful `GetApproximateSize` samples: mutations, read/write conflict ranges and
FDB accounting overhead, including attempts that later fail a guard or commit.
It is not a count of persisted mutation bytes, wire bytes or physical storage.
`timer` is a native StoreTimer snapshot taken after workers join; event kind
identifies units (timed `CumulativeValue` is nanoseconds; size `Count` is bytes).
Native commit timing includes successful precommit/commit work **and the
successful AttemptObserver callback**, including any observer blocking or reads;
it is emitted only for successful Run returns, not a complete physical commit
census: a commit reported as 1031 can be durable yet absent from this timer.
It is not pure server commit latency. Batch mode
does not emit scalar save/load timers or scalar save
counters; missing counters are not evidence that this work was free. Failure
receipts retain acknowledged batch/row counts, not an authoritative persisted
census when a canceled commit has an unknown outcome. Existing attempt observers
are preserved.

`phase_latencies` adds exact count/total/min/max nanoseconds for each observed
phase, including failed attempt phases and discarded preparation. These are not
quantiles. Commit observations use isolated per-Run timers merged after each Run
returns, so concurrent workers cannot contaminate each other's samples; the same
successful-Run/observer boundaries apply as for `timer`. The overlap regression
covers both trace/time index layouts and exact joined moments; it does not force
every worker completion or merge order.

`client_metrics.before` and `.after` retain the existing client's cumulative
snapshots without extra RPCs. Their explicit scope is the **whole database
handle**, including initialization, ready publication and any concurrent users,
not just source batches. Latency statistics retain client API units of seconds
and lifetime populations; never subtract their means, extrema or quantiles to
claim interval distributions. Read latency covers GetValue, not all range reads.
Monotonic counters can be differenced, but snapshot fields are not an atomic
cross-counter transaction. These snapshots accompany receipts with source-batch
diagnostics; an initialization failure before source processing has no such
receipt. Neither diagnostics nor the unchanged-posting optimization establishes
a throughput improvement or a 20k-spans/s result.

For readable custom indexes, updates compare complete encoded posting keys and
covering values and leave identical pairs untouched. Changed values at the same
key still replace the old value. WRITE_ONLY maintenance retains full clear/set
behavior because unchanged entries might not have been built yet. This reduces
replay/update mutations, not the mandatory postings for fresh inserts. Batch and
worker defaults remain unchanged; any larger batch still needs fresh and
already-committed replay checks under the same actual-size guard.

FDB batches use `--batch` (default 32, max 64), conservative 300 KB estimates,
500 KB approximate-write guards, 1 s save-plus-guard elapsed bound and 1.5 s
transaction timeout with finite retries in every mode. CH offline loading instead defaults to `--ch-batch 8192`
and `--ch-batch-bytes 8388608`: it is optimized bulk loading, not an artificial
FDB-sized synchronous tiny-part workload. **Load wall times are bulk preparation
comparisons, not equal real-time acknowledgment semantics.** No nonidempotent
external effects run in an FDB retry closure.

### Profiling otelbench

Every mode can write Go profiles of one window of its run. Each flag names a
new file; an existing file or a bad path fails the command before any work.

| Flag | Profile |
| --- | --- |
| `--cpuprofile FILE` | CPU samples during the window |
| `--memprofile FILE` | heap at the end of the window, and at its start in `FILE.start` |
| `--blockprofile FILE` | goroutine blocking during the window |
| `--mutexprofile FILE` | mutex contention during the window |
| `--trace FILE` | Go execution trace of the window (`go tool trace`) |

The window starts `--profile-start` after the run starts and lasts
`--profile-duration` (0, the default, lasts until the run ends). In
`collector-bench`, `--profile-start` defaults to `--warmup`, so the profiles
cover the steady phase; set `--profile-duration` to stop before the final
flush, audit and post-ingest queries. A run that ends before the window opens
still writes its report and exits 1. `--blockprofilerate` (default 10000:
one sample per 10 µs blocked) and `--mutexprofilefraction` (default 100) set
the sampling rates, enabled only during the window.

The heap profile's allocation counts are cumulative for the process; the
window's allocations are the difference from the start snapshot:

```sh
otelbench --mode collector-bench ... --duration 120s --warmup 20s \
  --profile-duration 90s --cpuprofile cpu.pprof --memprofile heap.pprof \
  --blockprofile block.pprof --mutexprofile mutex.pprof
go tool pprof -top otelbench cpu.pprof
go tool pprof -sample_index=alloc_space -base heap.pprof.start otelbench heap.pprof
```

Profiling costs CPU while enabled (the CPU profiler samples at 100 Hz; the
block and mutex profilers at the rates above); compare throughput only between
runs profiled alike.

The exporter's process takes `GOGC` and `GOMEMLIMIT` from its environment.
Evaluate these settings with profiles of the intended workload; this guide does
not establish a CPU or allocation reduction for a particular setting.

## Exporter configuration

`otel.Exporter` is the SDK `SpanExporter`, the Collector model's sink and the
writer `collector-bench` drives. It stores every signal of a `(run, tenant)` in
one OTEL_V1 schema store (database `/OTEL/R_<RUN>`, schema `T<tenant>`), creates
the template, database and schema on first open, writes compiled rows through
the Record Layer and serves every read from a direct scan of a standard OTEL_V1
index ([migration RFC](rfcs/relational-exporter-migration.md) §0, §1). The
store's `store_meta` `layout` row records the message layout (`messages=none`,
`messages=deltas` or `messages=fulltext`); an exporter configured for another
layout is refused with `OwnershipError` before any read.

`ExporterConfig` is read from JSON or YAML; omitted keys keep
`DefaultExporterConfig`, and every key it does not have is a `ConfigError`.

| Key | Values | Default |
| --- | --- | --- |
| `cluster_file` | explicit owned FDB cluster file | required |
| `run` | `^[a-z0-9_]{8,48}$` | required |
| `tenant` | 0..49,999,999 | 0 |
| `record_compression` | `none`, `zlib-1`, `zlib-default` | `zlib-1` |
| `bulk_index_conflicts` | coalesce a span batch's index write conflicts | true |
| `enable_message_deltas` | store GenAI messages once per tenant | false |
| `enable_message_full_text` | also index their text (implies deltas) | false |
| `batch_size` | spans per write transaction, 1..64 | 64 |
| `workers` | concurrent batch transactions per call, 1..16 | 8 |
| `max_candidates`, `max_bytes` | per-read budget | 20000, 4 MiB |
| `k` | default query limit, 1..10000 | 100 |
| `max_inflight_transactions` | write transactions in flight, 1..1024 | 32 |
| `transaction_timeout` | Go duration, 1s..5m | `10s` |

The custom store's layout keys `order` (the store has one time order),
`hash_bytes` and `posting_value_format` (it has no posting values) were removed
(D2); a document naming one is refused as removed. Attribute equality is typed
(D1): `CanonicalAttribute` gives a string, int, double or bool attribute its own
type; bytes (base64) and arrays, maps and unset values (`json`, canonical JSON)
are reported but not queryable.

## Exporter transaction policy

The SDK/Collector `Exporter` does not use the benchmark's retry limit or
1.5-second timeout. Every Exporter transaction, read or write, runs with no
retry limit (`RETRY_LIMIT -1`) and the `transaction_timeout` setting (Go
duration, default `10s`, valid `1s`..`5m`). FDB's OnError loop retries
retryable codes such as `process_behind`, `transaction_too_old`,
`future_version` and `not_committed` inside one transaction until it commits,
the timeout expires or the caller's context ends. The timeout runs from
transaction creation across all retries and includes GRV waits, which take
seconds under ratekeeper throttling. It must be set because a caller's context
does not end the wait for a dispatched commit
(`rfcs/280-client-retry-and-close-bounds.md` §4). The default covers about 5 s
of GRV queueing plus FDB's 5 s MVCC window. Exporter span batches have no fresh
Runs and no 1-second elapsed guard. The 500 KB size guard still applies.
A batch that times out fails its export call.

The export calls report a failed transaction with an error kind for retry
decisions. Domain errors keep their kind: `admission`, `config`, `ownership`
and `corruption` are permanent, and `budget` is retryable. An ended caller
context is `canceled`. FDB `1xxx` codes, including `transaction_timed_out`
(1031), and Record Layer retriable errors are `transient`. Other FDB codes
(`15xx` platform errors, `2xxx` illegal operations, `4xxx` internal errors) and unclassified errors
are `operation`. The Collector's `IsPermanent` retries `transient`, `budget`
and `canceled` errors and drops `config`, `admission`, `ownership`,
`corruption` and `operation` errors.

## Collector-model measurement boundary

`layers/otel/collector` is a custom in-process model of Collector batching,
queueing, retry and configuration behavior. It is **not** an upstream
OpenTelemetry Collector component and does not use upstream `exporterhelper`.
Historical “Collector” rates measure this model and the FDB exporter only, not
receiver transport, upstream queue semantics or Collector overhead. Actual
upstream Collector integration remains required for the larger Collector-to-SQL
product goal; the optimized experimental ingest layer does not complete it.

The `Exporter` keeps each span's original single-span `ResourceSpans` in the
spans row's `otlp` column, so all producer OTLP unknown fields, including tag
536870911, attribute order and duplicates survive without a fidelity field.
With messages decomposed, the GenAI content attributes there hold message
manifests, not repeated content. The custom store behind `otelbench --backend
fdb` still keeps canonicalization fidelity in `Envelope.fidelity`.

## GenAI message storage

`enable_message_deltas` and `enable_message_full_text` store each element of
`gen_ai.input.messages`, `gen_ai.output.messages`,
`gen_ai.system_instructions` and `gen_ai.tool.definitions` (span and event
attributes) once per tenant store as `messages` rows, and replace the attribute
value with a manifest of message IDs. `LoadSpanOTLP` restores the original
value byte for byte. The layout is the store's `store_meta` `layout` row
(`messages=deltas` or `messages=fulltext`); an exporter configured for another
layout is refused before any read. The design is the custom store's; only its
storage moved to OTEL_V1 rows ([migration RFC](rfcs/relational-exporter-migration.md)
§0 item 4, §1.5, §3.1). `otelbench --backend fdb` keeps the custom store's
`Message` records, owner-manifest layouts (`deltas`, `fulltext_v2`) and
`otel_message_v1` tokenizer as the baseline until phase 3B.

* **Rows and keys.** A message ID is `(writer, seq)`, writers numbered from the
  `store_meta` `message_writer` counter. A message transaction holds one writer
  exclusively and numbers its new messages in order, so the `messages` primary
  key `(writer, seq, part)` makes each writer's TEXT postings follow its earlier
  ones. The unique index `MESSAGES_HASH` on part 0's `content_hash` (a 16-byte
  SHA-256 prefix; other parts leave it NULL) deduplicates content; a message
  transaction reads it serializably, so concurrent writers of one message
  conflict and the loser reuses the stored ID. A present message costs no
  write at all. The digest prefix is 128 bits per tenant store.
* **Group commit.** Export calls queue their small messages in the exporter;
  up to 16 loops, each holding one writer for one transaction at a time, take
  as many queued messages as a transaction holds (128, and 1 MB of raw
  bytes) and store them together. A message transaction costs a fixed latency
  `a` (read version, digest reads, the first read of each TEXT bunch, commit:
  a chain of round trips) plus per-message CPU `c`, so `W` writers store
  `W * n / (a + c * n)` messages per second. With every writer busy, waiting
  messages make the next transactions larger instead of each waiting for a
  writer. Within one exporter, writers stay one per TEXT shard. Independent
  exporters can share shards and conflict; the model is not a throughput guarantee.
* **Text index.** `fulltext` adds the Go-only FULLTEXT index `MESSAGES_TEXT`
  on each part's `content`, partitioned by `(role, shard)`, with positions. An
  exporter holds at most 16 writers, each in its own shard (`writer mod 16`),
  so writers of that exporter never modify the same BunchedMap concurrently.
  Independent exporters of one store can share a shard and conflict. A query
  scans the 16 shards of a role concurrently and unions them. Without shards
  every writer would append to the same token maps and could rewrite the same
  boundary bunch. The index uses the default tokenizer, which Java's store
  opener knows. A token is part of an index key, which FDB caps at 10 KB, so
  `content` holds the placeholder `oteloverlongtoken` for each token longer
  than 1024 bytes and keeps its position. Such tokens are counted in
  `messages.unindexed_tokens`, and a query naming one is refused. `content` is
  index-only text stored next to the raw element; record compression can
  reduce that duplication. A
  function key expression over the raw bytes is not an option: a TEXT index
  requires a string field at the text position (Java
  `TextIndexMaintainerFactory.java:117-127`; a function validates as its
  arguments, `FunctionKeyExpression.java:218-220`).
* **Large messages.** A message is admitted up to 16 MiB. Its raw bytes are cut
  into parts of at most 1 MiB (each split by the Record Layer into 100 KB
  values) and its text into segments of at most 1024 distinct tokens and
  64 KiB; consecutive segments share 15 tokens, so every phrase of up to the 16
  query terms lies in one segment. These raw/text planning ceilings apply even
  when the whole message could fit in a transaction. Multiple tail parts may
  share a transaction; it stops adding parts once its approximate size reaches
  2 MiB. An FDB transaction-too-large error replans with a quarter of the
  distinct tokens, down to 16. All tail parts commit before the final part-0 publication, so a
  stored part 0 means a complete message. An "all" query also
  checks every multi-part message (`MESSAGES_LARGE`) for terms in different
  segments; that check costs one bounded scan per multi-part message and term.
* **Back references.** `fulltext` lists on each spans row every message it
  references, packed (`ref_chunks`: writer, seq, attribute code, the
  message's positions in the span's first value of that attribute, and
  whether it is that value's newest element). The list depends only on the
  span, so it is written blind with the span and replaced with it.
  `SPANS_REF_CHUNKS` keeps each chunk in an index key under
  `(trace_id, start_ns, span_id)` (D4), at most 8,000 encoded key bytes per
  chunk with zero-byte escaping, so a trace's spans and their references are
  one range. A `message_traces` row per (message, trace) names the traces a
  message occurs in; an export call saves the ones it does not find (snapshot
  read, before its spans commit; an exporter remembers up to 2^18 it knows to
  be stored). Concurrent writers of one marker
  conflict on the save's existence read, and the retry finds it. A marker
  outlives a span that stops referencing the message, which costs a query
  only a range it filters out. A query reads the matching messages' markers,
  one range per run of a writer's matches at most 32 seqs apart (the
  messages between them are read too, up to 4 rows per spanned message;
  a read that reaches that cap leaves the rest of its run to one read per
  message), then each of their traces' reference entries, every range of a
  round in flight together, and loads no span record. `FirstTurnOnly` keeps the
  references in which the message is the newest element, which is the turn
  that introduced it whatever order the spans arrive in. Turn k of a
  conversation references its whole history: that costs one mutation per
  span and O(k) value bytes, plus one marker per new message of the trace.
* **Queries.** `QueryMessageFullText` and `TraceExistentialQuery.MessageClauses`
  match message text with the Record Layer's `TextScan` (ALL, ANY and PHRASE,
  leapfrog intersections). Hits name the message by `message_id`
  (`writer.seq`); the content digest, prefix chains, matched terms and
  positions of the earlier layout are not reported. Message attributes hold
  manifests and are not equality-indexed; equality predicates on them are
  refused.
* **Write conflicts.** Message transactions read `MESSAGES_HASH` and the
  TEXT index serializably. `bulk_index_conflicts` widens a span batch's write
  conflicts over the indexes of the record type it saves (`spans`), not over
  the message indexes of the same store, so span batches and message
  transactions do not conflict through those widened ranges (R4,
  `TestRelMessagesRealFDB/span_batch_conflicts`). Widening over every index of
  the store would instead overlap the message transactions' read ranges.

## Repeated local campaign

`bench/local_campaign.sh` runs all five physical arms against already-running,
explicitly owned services. It requires a clean committed worktree, holds the
shared heavy-lane lock, audits each corpus, rotates arm order across repetitions,
and preserves raw failures. Build and validate first; supply the executable and
service endpoints explicitly:

```sh
BIN="$(bazelisk info bazel-bin)/layers/otel/cmd/otelbench/otelbench_/otelbench" \
FDB_CLUSTER_FILE=/absolute/path/to/owned/fdb.cluster \
CH_URL=http://127.0.0.1:PORT \
OUT=/absolute/path/outside/worktree/fresh-results \
RUN_PREFIX=otel_fresh_run COUNT=100000 ITERATIONS=1000 ROUNDS=3 \
bash layers/otel/bench/local_campaign.sh
```

The script deliberately gives ClickHouse a mature static-corpus layout using
`OPTIMIZE ... FINAL` before its audit and records stabilization time separately.
Every arm then gets a five-second settling interval. These are **warm-cache,
closed-loop, immutable-corpus diagnostics**, not live-ingest or capacity/SLO
measurements. It leaves owned data in place for inspection; use the guarded CLI
drop commands or terminate only the experiment's owned containers afterward.
Outputs are outside the frozen worktree during execution; archive reviewed
results under this layer after source-integrity checks pass.

## Query/audit evidence

* `selective` rotates rare, varying, absent, type-negative and identifier
  predicates. Its legacy `custom_varying` class uses a unique response-ID at
  eight or more attributes, retained filler values at five through seven, and
  an honestly labeled `trace_varying` lookup at four (where no varying attribute
  is guaranteed). `mixed` additionally includes a common resource
  term. `trace` rotates trace identities. `rare` intentionally repeats one term
  and must be labeled as a hot-query diagnostic, not the mixed-workload verdict.
* `span` alternates standalone span-ID hits/misses. `genai` cycles high-cardinality
  response-ID equality, resource/scope/type/missing controls and common
  operation/provider/model terms. `time` cycles intrinsic one-nanosecond windows,
  empty intervals, absent intervals and a broad time window. `all` cycles trace
  hits/misses, both span cases, eight GenAI cases and four time cases (16 samples
  cover the cycle). `genai`/`all` require at least eight attributes. Complete
  trace and standalone span queries do not silently truncate to K; equality and
  time queries return K distinct traces. All obey candidate/byte budgets.
* No application result cache. CH explicitly disables `use_query_cache`; HTTP
  connections and the optional native pool persist. FDB uses its native Go client with one open database.
  Client protocols, per-query ownership checks and decoding are included in wall
  latency; they are not identical protocols. FDB covering verification is included.
* Every query keeps a raw nanosecond sample, result count/digest, failure, FDB
  consumed candidate/logical-byte count, and CH server `X-ClickHouse-Summary`
  read_rows/read_bytes/elapsed_ns plus HTTP response bytes where available.
  CH data-query IDs, selected transport and server-metrics source accompany each
  sample; native read-work metrics remain honestly unavailable until reconciliation.
  `server_summary_available=false` means unavailable, **not zero engine work**.
  FDB logical consumed bytes exclude protocol bytes and iterator prefetch; they
  must not be equated to physical server I/O. CH physical read budgets and FDB
  consumed candidate budgets differ; disclose them. Histograms are explicitly
  successful-only and raw failures remain; any failure makes the command nonzero.
* `--max-candidates` and `--max-bytes` bound work; an insufficient budget returns
  failure, never partial exact top-K. CH has a 2 s query deadline; FDB per-attempt
  timeout is 1.5 s. Configurations that fail are not performance wins. There is no
  durable snapshot across separate queries or audit pages.
* Oracle preparation is outside query timing: at most one streaming source pass
  for all unique equality/time predicates, with at most K retained hits per
  predicate and one two-span trace pair of scratch witnesses. Known trace/span
  needles need no corpus pass. Audits are outside timing and warm caches.
  CH source audit streams once with a 180 s timeout and ordinal bitset; FDB audits
  bounded pages and commits bitset bookkeeping only after a successful page.
  Mandatory live audits avoid common-term scans; explicit `mixed` workloads and
  integration cases exercise those separately. The rare audit itself can still
  exceed budgets at very high configured match frequency: report failure, or
  deliberately raise audit budgets and label that receipt.
* Audit receipts include per-index FDB logical keys/key/value bytes (not physical
  compressed size), CH active-part compressed/uncompressed bytes for source and
  MV destination, CH version and equality/trace/span/time EXPLAIN. Named
  projection components are reported separately; verify their inclusion in
  parent-part totals before adding or comparing totals. Merge/part state is observed,
  not forcibly optimized. External engine/log/backup/replica bytes remain outside
  these measurements. A source fingerprint excludes physical order/hash/backend;
  ownership separately checks these, preventing a wrong-layout reopen.

JSON output files are exclusive-create, mode 0600; default stdout is available.
No payload is printed in normal evidence. CLI failures and SQL diagnostics may
contain synthetic fixture details; this tool is not authorized for private data.
Use `--help` for all bounds/flags. Query loops are closed-loop diagnostics: a
competitive capacity/SLO claim still requires the RFC's controlled load campaign,
matched semantics/durability, repeated rotated runs, complete raw artifacts,
CPU/RAM/I/O/storage cost, maintenance/recovery and all errors accounted for.
