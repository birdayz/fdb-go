# otel: an executable immutable OTel-span experiment

This is a **prototype**, not an OTLP receiver or a production tracing layer. It
compares real Record Layer custom-index queries with two legitimate ClickHouse
representations. It does not establish an end-to-end, HA, durability-matched,
open-loop SLO, compression, or production win merely by running successfully.
The design/research lives alongside the code under `rfcs/`, `reports/`, and
`research_notes/`.

## Implemented semantic class

* Counter-based SHA-256 corpus, configurable seed/population/tenants/attribute
  count/selectivity. Two spans per trace; timestamps have intentional ties.
  Scoped string/int attributes include literal dotted keys, same-name other
  scopes, integer/string lookalikes, repeated matching spans, and absent values.
* Real `FDBRecordStore.SaveRecord(proto.Message)` through a dynamic envelope
  containing complete synthetic OTLP `ResourceSpans` wire bytes. The registered
  `IndexMaintainerFactory` synchronously maintains custom secondary postings in
  `Update(old,new)`. WRITE_ONLY explicitly invokes that override, not an embedded
  standard-index method. Prefix deletion is refused; whole-store deletion clears
  the owned secondary structure. No SQL planner/core changes.
* Typed/scoped exact string/int equality, newest **K DISTINCT traces** ordered by
  latest matching span timestamp descending, then trace ID descending. Full
  tenant-qualified trace lookup returns span identities/timestamps in ascending
  timestamp/span order. Both systems return the same narrow metadata result shape,
  not full span/content output. FDB trace lookup still fetches primary records and
  decodes their OTLP payloads; only attribute-posting queries are index-only.
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
  fields against the fixture. FDB also counts posting keys/logical bytes.

Excluded: production OTLP admission/partial success, logs, content and privacy
services, retention/receipts/revision lifecycle, replay protocols, concurrent
writes during measurement, production continuations, numeric range search,
TraceQL, graph/session joins, online-backfill race certification, query ACLs,
HMAC epochs, normalized resources, compressed blocks, and service SLOs. The
maintainer's update/delete behavior is tested, but CH incremental MVs are
**append-only unique-corpus comparisons**, not a correction/delete equivalent.
There is no claim that the prototype implements all RFC-283 requirements.

## Physical arms

Every arm uses a fresh run namespace and maintains only its selected layout.
`--order` changes ownership metadata, not source corpus bytes.

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
Every CH source table also gets a covering `by_trace` projection before load,
ordered by tenant/trace/span and containing timestamps. Trace queries enable
projection optimization; audit captures their EXPLAIN and projection-part bytes.
Verify selection and parent/projection byte inclusion before making a trace
comparison or summing storage. All source data is retained; there is no hidden
term-time accelerator in the trace-ordered FDB arm.

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
CH database names are `otel_<run>`; FDB roots are tuples
`("otel_otelbench_v1",run)`. Drop requires `--confirm-drop` **and** matching owner
metadata, and removes only that owned database/root. A failed partial load stays
`loading`; choose a fresh run or explicitly drop it, never resume/replay blindly.

FDB ingestion uses `--save-mode pipelined` by default: immutable protobuf messages
are prepared outside the retry closure, then `SaveRecordsPipelined(..., None)`
pipelines existing-record reads while retaining ordered saves and old/new index
maintenance. `--save-mode serial` preserves the one-save-at-a-time baseline with
the same prepared messages. Receipts record the strategy in Config and ingest
details; it does not change source fingerprints or stored layout ownership.
Neither mode assumes that an identity is absent or skips old-record correctness.
FDB batches use `--batch` (default 32, max 64), conservative 300 KB estimates,
500 KB approximate-write guards, 1 s application elapsed guard and 1.5 s
transaction timeout. CH offline loading instead defaults to `--ch-batch 8192`
and `--ch-batch-bytes 8388608`: it is optimized bulk loading, not an artificial
FDB-sized synchronous tiny-part workload. **Load wall times are bulk preparation
comparisons, not equal real-time acknowledgment semantics.** No nonidempotent
external effects run in an FDB retry closure.

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

* `selective` rotates rare, varying custom key/value, absent, type-negative and
  varying identifier predicates. `mixed` additionally includes a common resource
  term. `trace` rotates trace identities. `rare` intentionally repeats one term
  and must be labeled as a hot-query diagnostic, not the mixed-workload verdict.
* No application result cache. CH explicitly disables `use_query_cache`; stdlib
  HTTP connections persist. FDB uses its native Go client with one open database.
  Client protocols, per-query ownership checks and decoding are included in wall
  latency; they are not identical protocols. FDB covering verification is included.
* Every query keeps a raw nanosecond sample, result count/digest, failure, FDB
  consumed candidate/logical-byte count, and CH server `X-ClickHouse-Summary`
  read_rows/read_bytes/elapsed_ns plus HTTP response bytes where available.
  `server_summary_available=false` means unavailable, **not zero engine work**.
  FDB logical consumed bytes exclude protocol bytes and iterator prefetch; they
  must not be equated to physical server I/O. CH physical read budgets and FDB
  consumed candidate budgets differ; disclose them. Histograms are explicitly
  successful-only and raw failures remain; any failure makes the command nonzero.
* `--max-candidates` and `--max-bytes` bound work; an insufficient budget returns
  failure, never partial exact top-K. CH has a 2 s query deadline; FDB per-attempt
  timeout is 1.5 s. Configurations that fail are not performance wins. There is no
  durable snapshot across separate queries or audit pages.
* Oracle preparation is outside query timing and scans the source corpus once
  for all selected predicates/traces. Audits are outside timing and warm caches.
  CH source audit streams once with a 180 s timeout and ordinal bitset; FDB audits
  bounded pages and commits bitset bookkeeping only after a successful page.
  Mandatory live audits avoid common-term scans; explicit `mixed` workloads and
  integration cases exercise those separately. The rare audit itself can still
  exceed budgets at very high configured match frequency: report failure, or
  deliberately raise audit budgets and label that receipt.
* Audit receipts include per-index FDB logical keys/key/value bytes (not physical
  compressed size), CH active-part compressed/uncompressed bytes for source and
  MV destination, CH version and rare-query EXPLAIN. Merge/part state is observed,
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
