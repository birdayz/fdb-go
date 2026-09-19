**NAK — WS-A design delta only**, exact virtual Git tree `822e4f49bb9f03d26659a7520835953c60998709`.

One descriptor-lifetime gap remains. [RFC:198](/home/birdy/projects/fdb-record-layer-go/rfcs/257-java-4.14.2.0-upgrade.md:198) specifies rebinding into one plan repository, but **one repository does not ensure one descriptor graph**:

- [proto_type.go:242](/home/birdy/projects/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values/proto_type.go:242) retains previously returned descriptors.
- [proto_type.go:316](/home/birdy/projects/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values/proto_type.go:316) invalidates the compiled file when another type registers; recompilation creates new descriptor identities.

Consequently, resolving `T`, then `record{f:T}`, can leave the cached descriptor for `T` different from the parent’s field descriptor. [record_constructor_message.go:257](/home/birdy/projects/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values/record_constructor_message.go:257) conceals that mismatch through another per-row copy. This pre-existing mechanism undermines WS-A’s descriptor-identity objective.

Specify a stable publication phase within the existing repository: register the complete synthesizable type closure, compile, then bind constructors and promotions before publication, preserving approved raw fallbacks. Require pointer-identity proofs across discovery orders and checked reconstruction. Target Java collects types before building its repository at [QueryPlan.java:648](/home/birdy/projects/fdb-record-layer-go/fdb-record-layer/fdb-relational-core/src/main/java/com/apple/foundationdb/relational/recordlayer/query/QueryPlan.java:648).

The remaining revised direction is sound: immutable coercion preparation, single child evaluation, strict admission, preserved extensions, and candidate/write/cache provenance. The [mandatory rebuild contract](/home/birdy/projects/fdb-record-layer-go/rfcs/257-java-4.14.2.0-upgrade.md:233) is honest and viable: operator provenance governs migration; ambiguous bytes provide no automatic detector. Required migration/refusal/rebuild and bidirectional byte proofs remain acceptance gates.

Audit reports and fixture source/producer hashes match. Accounting remains 1,189 net paths plus 84 history-only paths, not 228 independent commit reviews. Baselines establish no current comparison. No implementation or later-workstream approval; C++ remains 7.3.77. No edits, builds, tests, or agents ran.