# WS-C maintainer queue replay increment

Tracking: TODO.md, “WS-C maintainer queue replay increment”. Accepted design:
../ws-c-design.md. This is not completed WS-C or an implementation gate ACK.

Maintainers expose capability, serialization and replay. Unsupported maintainers,
including SPFresh, explicitly refuse queue operations. HNSW captures computed
entries after filtering, retains full primary keys in the payload, and shares
ordinary-write application with replay (old entries before new). Sliding-window
replay captures flattened keys and single-sided delegate payloads and uses existing
window bookkeeping under the existing write lock. No queued store state enabled.

Java authority: IndexMaintainer defaults, VectorIndexMaintainer queue methods and
updateIndexEntry, SlidingWindowIndexMaintainer queue methods in tag 4.14.2.0.
The retained numeric-vector extension is tested locally; the JVM oracle uses the
shared binary-vector KeyWithValue representation.

Retained tests in index_maintainer_queue_test.go execute five focused specs:
unsupported capability/default errors; captured vector entries and filtering;
proto2 empty key presence; full PK capture with overlapping partition/PK columns;
and sliding replay without a stored source, duplicate insertion bookkeeping,
delete and missing-delegate diagnostics. The real-FDB vector cases verify results
and delete application, not just serialized shape.

The JVM oracle in vector_index_conformance.{java,_test.go} executes one spec with
three operations: insert, update, delete. Each compares complete serialized Any
bytes, replays Go bytes in Java and Java bytes in Go, and asserts graph results.
Java also asserts zero stored source records. All three VECTOR-QUEUE evidence
lines were observed. Empty key presence is included in that byte comparison.
This is vector interoperability, not sliding-window interoperability coverage.

Evidence logs in /var/tmp/fdb-upgrade-recovery/ws-c/:
- maintainer-queue-focused.log: five selected / 3465 specs; ordinary Go tests also run.
- maintainer-queue-race.log: five selected / 3465 specs, actual rules_go race mode.
- maintainer-queue-jvm.log: one selected / 1517 specs, three operation evidence lines.
- maintainer-queue-jvm-race.log: one selected / 1517 specs, actual rules_go race mode.
- maintainer-queue-full.log: full suite before two extra PK/presence cases and JVM
  oracle: 93/93 passed, 42 executed / 51 cached, 760.818 seconds.
- maintainer-queue-final-full.log: full suite including the final JVM oracle:
  93/93 passed, 3 executed / 90 cached, 275.143 seconds.

Seven source hashes in source.sha256 were checked after the final full run and
all matched. Earlier focused race run predates the JVM additions but its five
recordlayer source hashes are unchanged. No tests were disabled; focused Ginkgo
runs exclude unselected specs by filter. No publication or operational changes.

Still open within WS-C: deeper sliding-window replay interop/eviction coverage,
queued store dispatch/state/readability, eligibility/policy, session/drain/cleanup,
and completed implementation gate reviews. WS-D–K and actual CI remain open.
