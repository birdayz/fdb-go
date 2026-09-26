**ACK — WS-A design only**, for exact virtual Git tree `f6efe9b8fd99522d2b95568a99eaac9ae7acb72b`.

No blocking WS-A design issue found. The implementation obligations are concrete:

- Replace the scalar/runtime promotion at [values.go:5177](/home/birdy/projects/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values/values.go:5177) with one declared-type coercion relation shared by evaluation and checked reconstruction. Evaluate children once; retain array targets; include structured targets in the existing [descriptor bake:88](/home/birdy/projects/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/plan_finalize.go:88). A second evaluator, descriptor authority, or CAST route is unnecessary.
- Preserve [strict protobuf admission:229](/home/birdy/projects/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values/record_constructor_message.go:229) and the raw nullable-array extension. Rebuilds must revalidate source/target coercion; nested values must carry actual target descriptors and numeric carriers.
- Extend [metadata preservation:405](/home/birdy/projects/fdb-record-layer-go/pkg/recordlayer/metadata_proto.go:405), its outbound clone boundary, and Build’s slice copy with field 16. One authoritative stored-query representation suffices.
- Carry coordinate provenance through candidates and writes. [Go distance handling:250](/home/birdy/projects/fdb-record-layer-go/pkg/recordlayer/hnsw.go:250) cannot distinguish transformed access-info DOUBLE vectors from raw node vectors. Java’s [access-info reader:247](/home/birdy/projects/fdb-record-layer-go/fdb-record-layer/fdb-extensions/src/main/java/com/apple/foundationdb/async/hnsw/StorageAdapter.java:247) confirms the distinction. Reject tolerance/result patches and blanket transform deletion.

The positive mixed-width tests and reproduced metadata-loss/self-distance failures are appropriate starting proofs. [RFC:178](/home/birdy/projects/fdb-record-layer-go/rfcs/257-java-4.14.2.0-upgrade.md:178) correctly requires descriptor identity, immutability, bidirectional operations, exact bytes, and revert-failure evidence.

I reviewed the entire companion audit and traced corresponding WS-A sources. Accounting reconciles 1,189 net paths plus 84 history-only paths across 228 commits; this is **not** 228 independent commit reviews. Research reports are not reviewer ACKs.

Later workstreams/backends/codecs require separate designs and reviews. Production implementation is absent; the suite remains red. C++ stays 7.3.77 without client-contract changes. No edits, builds, tests, or agents were run.