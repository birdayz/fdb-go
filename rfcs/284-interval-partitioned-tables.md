# RFC-284: Interval-partitioned tables and local indexes

**Status:** Accepted; implementation in progress. The owner accepted opt-in
partitioning, whole-schema Java lockout, and no conversion of existing tables.
The implementation must pass the proofs in §10.

## 1. Contract

Partition the **table**, not just its text index. Rows and all supported indexes
use the same partition boundaries. Users supply ordinary rows and predicates;
they never compute, insert or filter a bucket column.

- A table without a partition clause keeps its metadata, keys, constraints,
  write path and planning behavior. No implicit migration. Existing OTEL_V1
  stores stay unchanged.
- A partitioned table is one logical SQL table in one schema store. A query
  without a partition-key predicate considers **all** its live partitions.
- Index definitions inherit the table's partitioning. Existing FULLTEXT/VECTOR
  index `PARTITION BY` continues to mean index grouping; it does not become
  table partitioning.
- Unsupported index shapes fail at DDL/metadata validation. Do not accept them
  and discover at retention time that they cannot be dropped safely.
- A schema containing a partitioned table is Go-only. Java must refuse it before
  reading or writing any table in that schema. Plain schemas remain compatible.
- No `ALTER TABLE ... PARTITION BY`, repartitioning or automatic OTEL_V1 upgrade.
  Conversion means creating a new table/store and explicitly copying data.

This is an SQL/storage extension, not a change to Java's existing mechanisms.
Reuse key expressions, index maintainers, range clears and Cascades access paths.
Add the missing routing and planning semantics explicitly.

## 2. SQL surface

Use the familiar `PARTITION BY RANGE` / `INTERVAL` vocabulary, not a new `bin()`
SQL function or a Grafana macro. This is an Oracle-inspired dialect extension,
**not** an Oracle syntax subset or a portable ISO partitioning syntax.

The first version accepts one scalar `BIGINT` source column and one positive
`BIGINT` literal interval. The origin is zero; units are the column's units.
For OTLP nanoseconds, a UTC day is `86400000000000`.

The following is a proposed schema-template definition in the existing DDL form:

```sql
CREATE SCHEMA TEMPLATE partitioned_logs
  CREATE TABLE logs (
    effective_ns BIGINT, event_id STRING, trace_id BYTES, span_id BYTES,
    severity_text STRING, body_text STRING,
    PRIMARY KEY (effective_ns, event_id)
  ) PARTITION BY RANGE (effective_ns) INTERVAL (86400000000000)
  CREATE INDEX logs_trace AS
    SELECT trace_id, span_id, effective_ns, event_id FROM logs
    ORDER BY trace_id, span_id, effective_ns, event_id
  CREATE FULLTEXT INDEX logs_text ON logs(body_text)
```

Queries do not name partitions:

```sql
SELECT event_id, body_text
FROM logs
WHERE effective_ns >= ? AND effective_ns < ?
  AND TEXT_CONTAINS_ALL(body_text, 'connection timeout')
ORDER BY effective_ns DESC
LIMIT 100;

-- Still valid: searches all live partitions, not just today's.
SELECT event_id, body_text FROM logs WHERE trace_id = ?;
```

Lifecycle DDL, on a schema-bound connection:

```sql
ALTER TABLE logs DROP PARTITION FOR (1767225600000000000);
```

`FOR` takes a constant source-column value, not an internal partition number.
It selects the containing interval. Dropping an absent partition errors; an
`IF EXISTS` form may be added separately. Insertion creates partitions on demand.
A later insert may recreate a dropped interval: `DROP` is deletion, **not** a
retention cutoff that rejects late data. Automatic TTL is outside this RFC.

Expose active partitions through `INFORMATION_SCHEMA.PARTITIONS`: schema, table,
partition identifier, interval bounds and generation. Bounds outside BIGINT's
domain need an explicit unbounded representation, not overflowing arithmetic.

### Bounds and keys

For source value `x` and width `w`, the internal coordinate is:

```
p = floor(x / w)
partition p covers [p*w, (p+1)*w), intersected with the BIGINT domain
```

Use mathematical floor, including negative values. Existing `div` truncates
around zero and must **not** be redefined. Compute coordinates without overflow;
do not materialize overflowing endpoints to prune a range. Reject zero/negative
widths and NULL partition values before staging mutations. This routing check
applies only to partitioned tables; it does not change existing NULL behavior.

The declared primary key and every unique index must include the source column
as an ordinary key column. Otherwise reject the definition, as PostgreSQL does
for local uniqueness. Do not silently turn `PRIMARY KEY (event_id)` into a
partition-local constraint. With the source in the unique key, equal logical
keys necessarily route to the same partition. SQL still reports the declared
key, not its internal prefix.

Start with integer intervals. Calendar months, time zones, DATE/TIMESTAMP
partition keys, arbitrary expressions, LIST, HASH and subpartitioning are later
work. In particular, current DATE/TIMESTAMP metadata round-trips as STRING
(`pkg/relational/core/metadata/proto_types.go`); do not hide that issue behind a
partitioning clause.

## 3. Layout: existing subspaces, one extra key coordinate

Keep the existing record-type discriminator first. Tables share a Record Store;
putting the partition ahead of that discriminator would change unrelated table
scans and prefix deletion.

Schematic keys below omit store prefixes, record splitting and the ordinary
primary-key overlap trimming. `K` is the declared primary key, `G` is an index's
existing group, and `I` identifies an index:

| Structure | Partitioned layout |
|---|---|
| Record | `records / (record_type, p, K...)` |
| VALUE index | `indexes / I / (p, declared_index_key..., uncovered_PK...)` |
| Fan-out VALUE index | Same prefix, followed by each existing array-derived key |
| Covering VALUE index | `p` in the key portion; included values stay values |
| TEXT postings | Existing bunched postings under group `(p, G...)` |
| TEXT tokenizer versions | Existing secondary structure keyed by the **full** physical PK, including `(record_type, p)` |

Single-table index subspaces already identify their table. They need not repeat
`record_type` in their leading index key. Physical PK reconstruction still uses
the existing overlap rules; test it, rather than inventing a second PK codec.

For the example, `logs_trace` is ordered by
`(p, trace_id, span_id, effective_ns, event_id, ...)`. Its order is local, not a
global order by `trace_id`. `logs_text` groups by `p`; an index declared with
`PARTITION BY (shard)` groups by `(p, shard)`. Table partitioning does not remove
the need to bind or enumerate any additional TEXT groups.

No store per day, no table per day and no second index implementation. New
indexes on a partitioned table get the same prefix during metadata construction.
Their backfill uses the existing online-index framework, with the partition
validation and lifecycle exclusions below. Existing tables get no prefix.

## 4. Metadata and the Java boundary

Represent the coordinate as a required, versioned key function, proposed name
`fdb_partition_v1(source, width)`. Its contract is scalar BIGINT floor division,
not a renamed `div`. Serialize it through the existing `FunctionKeyExpression`
format. It is not a user-visible SQL function or a stored bucket column.

The canonical primary key is `(RecordTypeKey, partition_function, K...)`.
Derive the table's immutable partition specification from that required function;
validate that every local index uses the identical expression. Preserve the
logical key and index definition in SQL metadata views. Reject malformed,
unknown-version or inconsistent partition metadata instead of guessing a layout.
Do not add a second, optional flag that can disagree with the keys.

This is new function/planner work. Go currently treats a generic function as
potentially fan-out, which makes it invalid in a primary key. The new function
must have explicit single-valued validation, serialization/deserialization,
structural equality and a typed Cascades Value. Do not change the default
fan-out behavior of existing registered functions. No unverified fallback to a
hidden field that old writers could omit.

Java 4.14.2.0 resolves key functions through its existing registry and eagerly
loads primary keys for every record type. An unknown required function is the
intended fail-closed boundary. Prove rejection on both Java Record Layer and
Relational open, including a SELECT against a plain table in a mixed schema.
Also prove old Go binaries cannot commit writes using this metadata. Do not use
an unrelated fake tokenizer/index, assume an unknown protobuf field is enforced,
or claim a compatible layout merely because metadata can be decoded. This is a
compatibility guard for normal opens, not protection against raw FDB writes or
clients deliberately supplying alternate metadata/function implementations.

Partitioned schemas are supported only once those negative probes pass. Document
the exact boundary in `docs/compatibility.md` and `DIVERGENCES.md` with the
implementation. An unpartitioned schema must retain its existing byte goldens
and cross-engine tests unchanged.

## 5. Routing and partition lifecycle

All writers use the same coordinate function: SQL INSERT/UPDATE/DELETE, direct
access, Record Layer save/delete and online backfill. An UPDATE moving the source
value deletes the old row and its indexes and inserts the new row atomically.
Retries must not publish a directory entry without the corresponding write.

Maintain a small per-table partition directory in a reserved store subspace,
created only for partitioned tables. It holds active coordinates and generations,
not row copies or a second SQL catalog. First insertion activates a coordinate in
the same transaction. Deleting the last row may leave an empty active partition;
only explicit DROP retires it. Enumeration is over active entries, never every
integer between the oldest and newest timestamp.

Every writer reads the partition's generation/fence serializably before changing
its rows or indexes. A DROP changes that fence, so a pre-drop writer cannot
commit stale maintenance afterward. Partition activation/drop also changes a
table topology epoch for cached enumeration and continuations. Ordinary writes
do not update a shared table counter on every row.

Missing/corrupt directory state is not permission to omit records. Direct writers
must participate in this protocol; tests must exercise them, not just SQL. A
repair/consistency check must detect records with no active directory entry.

## 6. Planning: ordinary SQL, explicit properties

Keep a partitioned table as one logical relation. Its metadata supplies a typed
partition expression. During access-path matching, bind a partition coordinate
and match the remaining local index normally. Cost this against record scans;
do not force an index or mutate a completed physical plan after selection.

1. **Prune conservatively.** Derive candidate intervals from typed predicates on
   the source column: equality, IN and bounded/unbounded ranges. Handle parameters
   at execution, casts, inclusive/exclusive ends, AND/OR and SQL NULL semantics.
   Keep the original predicate as a residual. If implication is not proved, use
   all active partitions. Do not expand a huge time range into a compile-time IN
   list or prune a whole OR from only one branch.
2. **Bind at execution.** A directory cursor supplies coordinates to parameterized
   local scans, using the same correlated-binding model as existing IN joins.
   This is the missing partition/skip-scan access path, not functionality the
   current planner already has. No time predicate means the same iterator over
   all active coordinates. FTS can then bind its table-partition group without a
   user-written bucket predicate. Additional user grouping retains its existing
   binding requirements; generic unbound-TEXT-group discovery is separate work.
3. **Prove ordering.** For `ORDER BY source DESC`, disjoint interval bounds permit
   descending partition concatenation only if each child supplies the requested
   order. Apply residuals before counting rows toward the global LIMIT. Other
   orders need an ordered merge or sort; a local index order is not global.
   OFFSET, DISTINCT and aggregates retain their normal global semantics.
4. **Do not invent partition-wise joins.** Equal trace IDs do not imply equal
   timestamps. A span and its logs can cross midnight. Bind inner partitions from
   proven predicates or search all of them. Only proven co-location permits a
   same-partition join.

Partition operators carry correlation, ordering, uniqueness, cardinality and
continuation properties through the existing Cascades machinery. A new physical
wrapper must not erase outer bindings. `EXPLAIN` must show the pruning bounds,
directory iteration, chosen local index and final merge/sort.

Enumeration and child cursors share scan, byte, time and cancellation budgets.
A limit returns a continuation or the configured explicit limit error, never a
successful truncated result labelled complete. Resume state includes the plan,
metadata version, topology epoch, directory position and child continuation; a
changed topology invalidates it. It need not contain every partition ID. This
adds no promise of snapshots lasting beyond FDB's transaction lifetime.

## 7. Dropping a partition is more than clearing postings

Resolve the source value to `p`, validate every affected structure, then atomically
retire the directory entry, advance fences/epoch and clear the partition's data.
The Record Layer's existing prefix-delete path is the base:

- records, versions and split-record fragments use prefix `(record_type, p)`;
- single-table local index keys use the derived prefix `(p)`;
- grouped TEXT postings can clear that entire group prefix without decoding
  individual messages.

But **`DeleteRecordsWhere` alone is not proof of complete reclamation**. Current
TEXT scoped deletion leaves per-record tokenizer-version data in its secondary
subspace. UNIQUE indexes also have uniqueness-violation entries outside their
main index. The partition-drop implementation must inventory and clear all such
structures using their actual prefixes. TEXT secondary cleanup needs the full
record prefix, not the stripped index prefix. Preserve behavior of existing
unpartitioned prefix deletes; add a partition-aware validated cleanup operation.

All checks precede all mutations. Unsupported store-level counts, multi-type
indexes or auxiliary layouts must be rejected when the partitioned metadata is
built, not partly cleared at DROP. Logical removal uses a bounded number of
range clears per supported index; physical disk reclamation remains FDB's job.

For v1, refuse DROP while an affected index has an active online build/rebuild
or is WRITE_ONLY. Serialize that check against index-state changes. Do not let a
snapshot backfill resurrect dropped entries. The same exclusion covers deferred
index-maintenance queues; DROP must drain/fence them or refuse before mutation.
Supporting concurrent drop/build needs a separate generation-aware protocol,
not an assumption that range clears conflict with every blind writer.

## 8. Index support and OTel adoption

Initial allowlist: single-table **VALUE** indexes (including validated fan-out,
covering, partial and unique forms) and **TEXT**. Both need mutation, fetch,
backfill, pagination and full-drop tests. Reject intermingled-table layouts and
universal/multi-type indexes involving a partitioned table in v1.

Other maintainers do not become supported by prepending a field. Aggregate indexes
need correct cross-partition combination; averages and distinct aggregates cannot
be combined by blindly summing results. VECTOR indexes need per-partition search
and global top-k/recall contracts. Rank, bitmap, version and leaderboard indexes
have auxiliary-state and grouping rules. Admit each only after its own proof.
This restriction does not affect any index on an unpartitioned table.

OTel is the first intended consumer, not part of the generic partition catalog:

- A separate OTEL_V2 template can partition spans by `start_ns` and logs by
  `effective_ns`; the existing stores and Grafana PR need no change for this RFC.
- Message deduplication is tenant-wide. Keep `messages` and `messages_hash`
  unpartitioned rather than weakening their unique key.
- A normal unpartitioned `trace_id -> partitions` locator can accelerate lookups
  without a time range. It is optional application data, not a prerequisite for
  SQL correctness. Stale locators after retention must be harmless.
- Changing logs from PK `(event_id)` to `(effective_ns, event_id)` **does change**
  explicit-event-ID replacement semantics. V2 needs an identity/locator policy
  for replays and timestamp correction before adoption. This is not excused by
  the existing possibility of duplicates when an event ID is absent.
- Logs FTS remains opt-in. Local partitions bound the searched history; they do
  not remove token write amplification or contention in today's partition.
  Measure indexing separately from message deduplication and ingestion.

## 9. Existing mechanisms versus new work

Source inspection against Go `9727cff54` and Java tag `4.14.2.0` establishes the
reuse points, not a passing prototype:

| Existing mechanism | New work required |
|---|---|
| Function key-expression serialization and structural equality | Singleton floor-coordinate function, validator and typed Value |
| Record-type-prefixed primary keys and PK overlap trimming | Compile/round-trip the extra coordinate without exposing a column |
| VALUE/TEXT grouping and maintenance | Partition-aware metadata validation and auxiliary cleanup |
| Go tuple-prefix `DeleteRecordsWhere` | Fences, lifecycle atomicity, complete reclamation and build exclusion |
| IN joins, local access paths and ordering properties | Directory-driven enumeration, pruning and global ordering proofs |
| Java required-function registry lookup | Live refusal probes and an explicit compatibility boundary |

Relevant sources: `pkg/recordlayer/key_expression.go`, `metadata.go`,
`store_delete_where.go`, `text_index_maintainer.go`, `index_maintainer.go`,
`pkg/relational/core/metadata/builder.go`, and
`pkg/recordlayer/query/plan/cascades/key_expression_expansion.go`.
Java references are `FunctionKeyExpression`, `RecordMetaData`,
`QueryToKeyMatcher`, `TextIndexMaintainer` and `StandardIndexMaintainer`.

## 10. Implementation gates

1. **Prove the layout first.** Add Record Layer tests for the singleton function
   in a PK, proto round-trip, matching prefix equality, PK trimming/fetch and
   typed rejection. Use two record types with overlapping partition values.
   Verify complete VALUE/TEXT/unique-aux cleanup and neighboring-partition
   survival. Add live Java and old-Go refusal probes with a plain-schema control.
2. **Compile and route.** Typed ANTLR nodes, immutable partition metadata, directory
   activation/fences and all writer APIs. Test negative/extreme integers, NULL,
   invalid widths, unique keys, replay, moves, retries and malformed metadata.
   Existing unpartitioned byte goldens must remain identical.
3. **Plan and execute.** SQL tests assert both rows and EXPLAIN. Compare against an
   unpartitioned oracle for no time filter, sparse partitions, boundary ranges,
   prepared parameters, OR, FTS, cross-boundary joins, aggregate queries and
   ORDER/LIMIT/OFFSET. Cover forward/reverse continuations, budgets, cancellation
   and topology invalidation. Prove indexes added after ingestion can backfill.
4. **Retire safely.** Race DROP against first insert, update/move, direct writes,
   metadata changes and index builds. Check keys in every auxiliary subspace,
   not just SELECT results. Prove a later insert can recreate the interval and
   no old builder/writer can resurrect pre-drop state.
5. **Measure, then adopt.** Compare partitioned/unpartitioned tables, FTS off/on,
   and explicit TEXT shard counts on identical data. Report ingest CPU/latency,
   retries, bytes, common/rare-token query p50/p99, examined partitions and
   retention mutations. Include narrow windows, all-history queries and queries
   concurrent with ingest. No historical message benchmark proves logs costs.

Run `just test` for the fast lane and the serialized `just test-full` lane for
Java and SQL integration changes. Report actual versus cached execution. No
partitioned-table implementation ships before gates 1–4 pass; OTel adoption also
requires gate 5 and its replay policy. This RFC itself changes no executable code.

## 11. Prior art and deferred work

- [ClickHouse custom partitioning](https://clickhouse.com/docs/engines/table-engines/mergetree-family/custom-partitioning-key):
  table-level expression, automatic partitions, local parts/index data. Not a
  reason to copy MergeTree compaction into FDB or promise uniqueness it lacks.
- [PostgreSQL declarative partitioning](https://www.postgresql.org/docs/current/ddl-partitioning.html):
  parent relation, pruning, local child indexes, partition key included in unique
  constraints. Explicit child bounds are a different lifecycle from this RFC.
- [MySQL partitioning](https://dev.mysql.com/doc/refman/8.4/en/partitioning.html):
  table-level RANGE/LIST/HASH and partition-key restrictions on unique keys.
- [Oracle interval partitioning](https://docs.oracle.com/en/database/oracle/oracle-database/19/vldbg/partition-create-tables-indexes.html):
  RANGE plus INTERVAL and automatic interval creation. Oracle also has an initial
  range/transition point; this RFC deliberately fixes origin zero instead.
  Oracle supports both local and global indexes; local is not a universal default.

Deferred: temporal/calendar keys, explicit nonuniform bounds, HASH/LIST, other
index types, global unique indexes, online repartitioning, TTL/late-data cutoff,
concurrent drop/backfill, and OTel migration/replay policy. Track these in TODO.md;
do not claim the prefix layout implements them already.
