**NAK — WS-A-only design review.**

Exact uncommitted Git tree: **`f6efe9b8fd99522d2b95568a99eaac9ae7acb72b`**.

Two blocking design gaps:

1. **Legacy Go access-info needs a migration contract.** [RFC:161](/home/birdy/projects/fdb-record-layer-go/rfcs/257-java-4.14.2.0-upgrade.md:161) assumes plain access-info bytes are transformed. Existing deletion repair returns original node bytes at [hnsw.go:1132](/home/birdy/projects/fdb-record-layer-go/pkg/recordlayer/hnsw.go:1132), then persists them unchanged while retaining centroid/seed at [hnsw.go:882](/home/birdy/projects/fdb-record-layer-go/pkg/recordlayer/hnsw.go:882). Selecting a pre-centroid node after centroid establishment can therefore leave **raw DOUBLE access-info** in an existing Go-written store. Specify how these entries are detected and repaired, refused or rebuilt. Require a parent-written Euclidean centroid→entry-deletion→upgrade→cold-reopen regression.

2. **Candidate provenance must cross inline-edge writes and caches.** Selected candidate bytes flow through [hnsw.go:430](/home/birdy/projects/fdb-record-layer-go/pkg/recordlayer/hnsw.go:430) into unchanged byte persistence/cache insertion at [hnsw.go:2236](/home/birdy/projects/fdb-record-layer-go/pkg/recordlayer/hnsw.go:2236). Java explicitly applies `quantizer.encode` at [InliningStorageAdapter.java:290](/home/birdy/projects/fdb-record-layer-go/fdb-record-layer/fdb-extensions/src/main/java/com/apple/foundationdb/async/hnsw/InliningStorageAdapter.java:290). Specify candidate-to-node encoding and cache invariants; otherwise transformed DOUBLE entry bytes can become plain neighbor bytes and be transformed again after reopening. Require targeted cross-language insert/delete/reopen checks with persisted-byte assertions.

Reviewed the complete frozen diff and companion audit, including generated changes. The 1,189 net paths plus 84 history-only paths reconcile. Scope is complete net diffs plus necessary history, **not 228 independent commit reviews**; the five researchers are not reviewer ACKs.

No builds/tests ran; archived full-suite evidence remains red. Later workstreams require separate detailed design reviews. C++ remains 7.3.77; no client-contract change is authorized.