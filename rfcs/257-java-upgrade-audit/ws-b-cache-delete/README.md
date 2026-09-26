# WS-B deletion/cache implementation progress

Design ACKs: ../ws-b-design-review/. Workstream implementation ACKs still pending.
Read pinned Java header-aware delete, context clear, header update, and complete
MetaDataVersionStampStoreStateCache. Implemented header-only serializable read,
conditional stamp invalidation, unconditional dirty-state mark, tuple-subspace
bounds, and context range clearing with pending/local version cleanup. Cache
admission now excludes noncacheable headers; stamp-mismatch noncacheable reloads
invalidate older entries. Cacheability changes clone the proposed header so old
cacheability is preserved for invalidation; stamp-read errors propagate before
writes/dirty-state changes. Published header changes only after success.

Retained red→green: two prior cache tests had wrong expectations/manual cache
clearing; strengthened against Java behavior, both failed before fixes. Eight
new deletion specs failed before fixes. Added real-FDB cancelled-read regression
with exact1025 and unchanged header/context;16 commit-order combinations cover
cacheable/noncacheable, cold/warm, header/record-only writes, delete-first/write-
first. Both contexts pin one read version; exact1020 is asserted before retry,
and record-only writes do not spuriously conflict with header-only deletion.
Delayed version writes cannot restore deleted keys; bare-prefix/end-boundary keys
survive. Existing valid deletion behavior retained. Focused cache/delete73 specs
PASS. Six compiled/applied/killed/restored mutations cover admission, eviction,
old-header preservation, serializable deletion read, version cleanup and stamp
invalidation.

Frozen6509 source hashes unchanged throughout verification:
- uncached full recordlayer target:2083 Go RUN/PASS,3251/3252 Ginkgo PASS;
- uncached race recordlayer target:same populations,zero failures;
- the one pre-existing opt-in million-record Ginkgo spec was not enabled;
- just test:93 targets PASS,41 executed/52 cached,719.483s. Not all-uncached.
Raw complete outputs and SHA256 values are in cache-delete-results.json; the
source freeze is retained. Summary's unindented Go counters undercount nested
cases; reconciled counts above use whitespace-tolerant per-name RUN/PASS counters.
No new skips, weaker assertions, client changes, commits, pushes or merges.

Deletion/recreation composition with pending replacement-retirement checks must
still be implemented/tested with WS-B's replacement lifecycle. Its tagged-Java
probe remains pending. No claim that full WS-B or its broader Java/wire-interoperability
acceptance matrix is complete. Metadata/rank and final milestone review remain.
