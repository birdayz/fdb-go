# RFC-258: Index remote fetch, and hot-path refactors for later

Status: Part 1 accepted, in implementation. Part 2 recorded for later; nothing in
it is approved.

Every item here must be checked for conformance with Java Record Layer 4.14.2.0
and libfdb_c 7.3.77 by tests that run both against the same cluster, not by
reading code alone. An item whose behavior cannot be shown identical does not
ship.

## Part 1: IndexFetchMethod and remote fetch

### Problem

Java's Record Layer can fetch the records behind an index scan on the storage
server: one `getMappedRange` call returns index entries together with their
records. Go always scans the index and then issues one range read per record.
A 1,000-record index scan costs about 1,000 record reads, 59 allocations per row
and 26 ms locally; remote fetch needs a few round trips.

Go already has the pieces below the Record Layer:
- `client.Transaction.GetMappedRange` (#689, `pkg/fdbgo/client/mappedrange.go:312`),
  one batch per call, with the C++ conflict-range handling for primary and
  secondary ranges, and a libfdb_c differential
  (`pkg/fdbgo/libfdbc/mappedrange_differential_test.go`).
- The SQL option `INDEX_FETCH_METHOD` is parsed but ignored
  (`pkg/relational/api/options.go:74`); the plan proto already has
  `index_fetch_method` (`proto/apple/record_query_plan.proto:1938`).
- `FallbackCursor` (`pkg/recordlayer/fallback_cursor.go`) and
  `Index.PrimaryKeyComponentPositions` (`pkg/recordlayer/index.go:804`).

### Java behavior to port (4.14.2.0)

| Piece | Java source |
|---|---|
| Enum: SCAN_AND_FETCH (default), USE_REMOTE_FETCH, USE_REMOTE_FETCH_WITH_FALLBACK | `record/IndexFetchMethod.java` |
| Dispatch by fetch method; fallback on `UnsupportedRemoteFetchIndexException` and on any other failure | `FDBRecordStoreBase.scanIndexRecords`, :1170–1215 |
| Fallback continues from the last successful result's continuation | `FDBRecordStoreBase.remoteFetchFallbackFrom`, :2604–2620 |
| Preconditions: positive common primary key length, scannable index, BY_VALUE scan, API version ≥ 710 | `FDBRecordStore.scanIndexRemoteFetchInternal`, :1523–1557 |
| Timer event SCAN_REMOTE_FETCH_ENTRY around the cursor | same, :1557 |
| Default maintainer throws UnsupportedRemoteFetchIndexException; VALUE and VERSION maintainers support it | `IndexMaintainer.java:391`, `ValueIndexMaintainer.java:137`, `VersionIndexMaintainer.java:123` |
| Mapper: records-subspace tuple, then `{K[i+prefixLength]}` per primary-key position from `getEntryPrimaryKeyPositions(commonPrimaryKeyLength)`, then `{...}` | `StandardIndexMaintainer.createRemoteFetchMapper`, :829–840 |
| Cursor over `getMappedRange` with the same limit/continuation machinery as the key-value cursor | `IndexPrefetchRangeKeyValueCursor.java` |
| Index entry from the primary row; record rebuilt from the secondary range rows: split records, inline version, legacy unsplit format, empty range means orphan | `StandardIndexMaintainer.unpackRemoteFetchRecord`, :193; `FDBRecordStore.reconstructSingleRecord`, :1646–1683 |
| Records built with `mapPipelined(..., 1)`, byte scan limiter applied, orphan handling, SKIP filters nulls | `FDBRecordStore.indexEntriesToIndexRecords`, :1561–1585 |
| Planner: non-BY_VALUE scans downgrade to SCAN_AND_FETCH at plan time; API version below 710 downgrades at execution; plan serializes the fetch method | `RecordQueryIndexPlan`, :244–290, :749 |
| SQL option INDEX_FETCH_METHOD feeds the planner configuration | relational `OptionsUtils.getIndexFetchMethod`, `PlannerConfiguration.java:176` |
| Java's own tests: RemoteFetchTest, RemoteFetchIndexScanTest, RemoteFetchMultiColumnKeyTest, RemoteFetchSplitRecordsTest, RemoteFetchWithoutSplitRecordsTest, RemoteFetchOldVersionTest | `fdb-record-layer-core/src/test/java/.../provider/foundationdb/` |

libfdb_c constraints the Go client already follows: `getMappedRange` is
forward only (reverse throws `unsupported_operation`,
`fdbclient/ReadYourWrites.actor.cpp:1139`); it requires serializable reads and
read-your-writes enabled, and fails with `get_mapped_range_reads_your_writes`
if it would read the transaction's own writes (:1215–1240). With
WITH_FALLBACK, those failures fall back exactly as Java does.

### Design

1. **Facade capability.** Add an optional interface in `pkg/fdbgo/fdb`, in the
   style of `CtxTransactor`: a mapped-range read on `ReadTransaction`,
   implemented by the pure-Go backend over `client.Transaction.GetMappedRange`.
   The libfdb_c backend does not implement it: Apple's Go binding has no mapped
   range, and `mappedrange_cref.go` decodes a non-ABI layout and stays test-only.
   On that backend the store reports the same unsupported error Java uses, so
   USE_REMOTE_FETCH fails and WITH_FALLBACK falls back. Record this backend
   limitation in `docs/compatibility.md`.
2. **Mapped-range iteration.** The client call returns one batch. The cursor
   loops batches like the C++ range iteration: next batch begins after the last
   primary key, limits and the `more` flag as for ordinary ranges. Continuations
   use the existing key-value-cursor encoding (Java shares
   `KeyValueCursorBase`), so a token from remote fetch resumes a SCAN_AND_FETCH
   scan and the reverse, which the fallback depends on.
3. **Record Layer.** `IndexFetchMethod` enum; the dispatching `ScanIndexRecords`
   overload with fetch method and common primary-key length; `ScanRemoteFetch`
   on the value and version maintainers; mapper construction byte-identical to
   Java's; `reconstructSingleRecord` reusing the existing unsplit and version
   code; orphan behavior and `BAD_INDEX_ENTRY` as in #898;
   SCAN_REMOTE_FETCH_ENTRY timer event.
4. **Planner and SQL.** The fetch method on the index plan, with Java's plan-time
   and execution-time downgrades and plan serialization; honor
   `INDEX_FETCH_METHOD` and remove the "not honored" note.
5. **Allocation design.** The reply is bulk, so cost should be per batch, not
   per row:
   - decode a reply in one pass into a slice of value structs reused across the
     batches of one scan, not a heap object per row;
   - reconstruct records directly from the reply's secondary rows without
     intermediate `KeyValue` cursors (Java uses a `ListCursor` here; we only need
     its result);
   - reuse decode scratch and fully consumed reply frames; anything returned to
     the caller (records, bytes it keeps, index entries) is independently owned
     and never pooled;
   - report allocations per row against SCAN_AND_FETCH; the target is the
     inherent cost (protobuf message plus caller-owned bytes).

### Conformance tests required

- **Java conformance against the same cluster** for each fetch method: same
  records, order, orphan results, continuations at every position (including
  resuming across methods), limits (row, byte, time), split records, inline and
  old-format versions, multi-column primary keys, and error codes. The Java
  RemoteFetch* tests listed above are the case list to port.
- **Wire/data plane:** the mapper bytes equal Java's for each index shape; read
  conflict ranges after a remote-fetch scan equal libfdb_c's (extend the
  existing mapped-range differential).
- **Fallback:** forced failures before and after the first result, unsupported
  maintainer, reverse scans, read-your-writes conflicts, and the libfdb_c
  backend; each must continue exactly where Java's would.
- **SQL:** a yamsql scenario with `INDEX_FETCH_METHOD` set, asserting the plan
  carries the method (EXPLAIN) and the rows match.

### PR breakdown

1. Facade capability and mapped-range iteration, with the differential extended.
2. Record Layer `ScanRemoteFetch`, reconstruction, dispatch, fallback, Java
   conformance.
3. Planner, plan serialization and the SQL option, yamsql scenario.
4. Allocation work on the remote-fetch path, with measurements.

## Part 2: later candidates (not approved)

Each needs its own conformance proof before implementation.

1. **Per-read cancellation through the transaction incarnation.** libfdb_c waits
   every read on one per-transaction `resetPromise`
   (`ReadYourWrites.actor.cpp:354–395`, :2699–2733) and cancels single reads with
   `fdb_future_cancel`. Go builds a derived context per read
   (`read_incarnation.go:175`). Waiting on reply, caller context and incarnation
   directly removes that per-read context. Open question for the owner:
   `slog` callbacks would receive the caller's context and no longer see
   1025/1031 as `context.Cause`. A comparison with a merged per-read context is
   being prepared by the allocation workstream.
2. **Per-transaction slab for bookkeeping objects**, the Go counterpart of the
   C++ transaction arena: futures, pending reads, read operations, requests and
   conflict entries allocated from chunks that live as long as the transaction.
   Caller-owned results stay outside. Must show unchanged results, ownership
   and retention bounds.
3. **A fused index-to-record cursor** with a fixed window of preallocated
   slots, replacing the composed map, pending, ring and loader cursors. Must
   match Java's continuations, limits, ordering and orphan behavior.
4. **Batched record reads in the client:** one operation for a window of reads,
   so lease, context and read-your-writes setup are paid once per window.
   Requests on the wire stay one per read, as with libfdb_c.
5. **Store-state cache.** Opening a store costs about 162 µs of a 292 µs load,
   for the header and index-state reads. Check whether Go wires up Java's
   store-state cache and with which default.
6. **Smaller client items** from the franz-go comparison
   (`/tmp/franzgo-patterns.md`): reuse range-read hedge timers
   (`client/hedge.go:81/159/183`), lazy iterator header conversion, allocating the
   range request with its first batch. These are queued in the allocation
   workstream.
