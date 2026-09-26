This assigned scope requires substantial parity work: HNSW storage/API changes, numerical fidelity fixes, a complete GuardiANN engine and maintenance path, and Lucene queue correctness changes. Existing Go HNSW, RaBitQ, and SPFresh implementations cover useful foundations, but they do not establish target parity.

This is a source audit. I ran no builds or tests, changed no files or branches, and performed no runtime parity verification. Implementation must await RFC design review.

**Scope and consumption**

| Item | Audited revision or count |
|---|---|
| Go baseline | `e48f5b4965543cd4d99b5578356059e12d969c7c` |
| Java base | `4.12.11.0` — `257aa83cae7f90e18ea6595fdf2cf841ca72e802` |
| Java target | `4.14.2.0` — `fdacd162a9c8acfadc49082b89185c823ab8ae4a` |
| Population | `/var/tmp/query-grind-cast/java-upgrade/audit/extensions-vector.paths` |
| Assigned paths | **170, all unique** |
| Change statuses | **91 added, 78 modified, 1 deleted** |
| Scoped additions/deletions | **25,831 / 1,748 lines** |
| Complete scoped diff | **31,860 lines; 1,673,967 bytes** |

I consumed the complete output of:

```text
git -C fdb-record-layer diff 4.12.11.0 4.14.2.0 -- <all 170 exact assigned paths>
```

The output was read in manageable chunks; truncated portions were reread. I also inspected necessary target implementation and tests, scoped history, selected intervening commits, and corresponding Go implementations through `git show e48f5b496:path`. The input path set exactly matches the scoped `--name-status` population.

**Unread assigned diff scope: none.** Supporting reads outside the assigned population were limited to dependencies needed to interpret these changes, such as index-state predicates and the throttled iterator’s commit default. They do not constitute an audit of another researcher’s group.

The population comprises 102 extension production paths, 41 extension test paths, six extension fixture paths, six Lucene production paths, eight Lucene test paths, three ICU test paths, and four Gradle paths.

Below, Java line numbers refer to the pinned target; Go line numbers refer to the baseline above. For compact references:

- `E` = `fdb-extensions/src/main/java/com/apple/foundationdb`
- `ET` = `fdb-extensions/src/test/java/com/apple/foundationdb`
- `EF` = `fdb-extensions/src/testFixtures/java/com/apple/foundationdb`
- `L` = `fdb-record-layer-lucene/src/main/java/com/apple/foundationdb/record/lucene`
- `LT` = `fdb-record-layer-lucene/src/test/java/com/apple/foundationdb/record/lucene`
- `IT` = `fdb-record-layer-icu/src/test/java/com/apple/foundationdb/record/icu`

**W1 — Async utilities and shared vector infrastructure**

Principal upstream change: `4e3d42bc8`, PR **#4083**, GuardiANN.

`MoreAsyncUtil` contains actual correctness and API changes, not just extraction:

- New supplier-backed iterable construction, consumption without materialization, exposed remaining-iterator limits, `takeWhile`, and future-collection adapters.
- `takeWhile` consumes the first failing item and then ends; repeated readiness checks retain the pending result.
- `mapConcatIterable` terminates when there is nothing left to await. The previous empty-wait case could fail to terminate correctly.
- Pipelined filtering now uses the supplied executor.
- Pipelined mapping gains an explicit-executor overload while retaining its common-pool convenience overload.
- Some no-executor overloads disappear; these are Java source API changes.
- The stateful `forLoop` overload allows the condition to inspect both iteration number and accumulator.
- `forEach` rejects parallelism below one, fixes its concurrency bound from `<=` to `<`, handles null items through an internal sentinel, and returns results in input order rather than completion order.

Evidence: `E/async/MoreAsyncUtil.java:89,119,149,229,561,614,780,899,1256,1290`. The bounded loop and input-order result assembly are at lines 1294–1330.

Go’s [cursor_util.go:25](/home/birdy/projects/fdb-record-layer-go/pkg/recordlayer/cursor_util.go:25) already implements sequential consumption with close/error propagation. Its [cursor_combinators.go:624](/home/birdy/projects/fdb-record-layer-go/pkg/recordlayer/cursor_combinators.go:624) processes flat-map inner cursors sequentially; it does not reproduce Java’s future pipeline or empty-future-wait defect. Thus:

- **Already implemented in analogous form:** ordinary sequential consumption, exhaustion and error propagation.
- **Required with GuardiANN:** ordered bounded asynchronous work where the algorithm depends on it, prefix stopping, collection adapters, and correct cancellation/exhaustion.
- **Language-only:** executor types, CompletableFuture overload shapes, Java visibility changes. A new general async framework is unnecessary.

Shared common classes add these contracts:

- `DistinctTopK` defines distinctness through comparator equality, uses a tree set, requires positive `k`, and returns sorted retained entries. GuardiANN’s comparator includes distance and vector identity.
- `TopK` retains duplicates and maintains a bounded heap. Sorting a result must not consume the original collector.
- `TimedAsyncIterable` reports waiting time once on exhaustion, asynchronous failure, or cancellation. An abandoned iterator does not report. `asList` measures its own completion.
- Read/write listeners expose key reads, writes, and range deletion; GuardiANN additionally depends on task notifications.
- Shared vector codecs and transformations are extracted from HNSW. Raw vectors are transformed; already encoded vectors remain in the stored coordinate system. Inverse transformation does not restore the original cosine norm.
- `ResultEntry` moves from HNSW to the common package and becomes a record containing primary key, optional reconstructed vector, optional covering values, distance, and rank/row number. Its vector must be a raw half/float/double representation.

Evidence: `E/async/common/DistinctTopK.java:55,70`; `TopK.java:65,96`; `TimedAsyncIterable.java:57,83,107,134`; `StorageTransform.java:61,78`; `ResultEntry.java:50`.

**Storage extraction is not a new sample wire format.** Both tags store sampled aggregates under count plus UUID, with double-vector bytes in a tuple. Target `StorageHelpers.java:69` retains snapshot reverse reads, explicit conflicts on consumed keys, and deletion of those keys. Go already implements that conflict pattern in `pkg/recordlayer/hnsw.go:683`. Its sample identifier is a byte string rather than Java’s tuple UUID; that is a **pre-existing** interoperability detail, not a range-introduced format change.

Required tests are the target utility contracts, including empty/all-filtered pipelines, partial consumption, null items, invalid parallelism, maximum active work, input-order results under reverse completion, cancellation timing, and comparator-based distinctness. These tests were source-reviewed, not executed.

**W2 — HNSW covering values, result APIs, traversal, and randomness**

Principal upstream change: `4e3d42bc8` / **#4083**; cardinality is also used by the final merge policy in `24863e8af` / **#4604**.

There are four required functional additions.

1. **Persist and preserve covering values.** Layer-zero compact nodes accept optional `additionalValues`. Legacy nodes have three tuple fields; populated covering nodes have a fourth. Upper-layer insertion passes null. Search and fetch propagate the values even when vectors are omitted.

   Target evidence: [CompactStorageAdapter.java:172](/home/birdy/projects/fdb-record-layer-go/fdb-record-layer/fdb-extensions/src/main/java/com/apple/foundationdb/async/hnsw/CompactStorageAdapter.java:172), lines 235–254; `Insert.java:518`; `Primitives.java:983`; `Search.java:309`.

   Go’s `pkg/recordlayer/hnsw.go:1669` always writes three fields. `parseNodeValue` at line 1707 stops after neighbors and ignores a fourth field. `parsedNode` at line 1556 has nowhere to retain it. Consequently, merely accepting target bytes is insufficient: **a Go neighbor rewrite can discard target covering values**.

   The port must thread values through parsing, caching, mutation, delete repair, and result construction.

2. **Fetch and cardinality APIs.** Target HNSW exposes fetch returning distance `0` and rank `-1`, plus `EMPTY`/`SINGLE`/`MULTIPLE` classification using at most two layer-zero entries. It also exposes a result-oriented layer scan for diagnostics.

   Evidence: `E/async/hnsw/HNSW.java:185,205,375`; `Primitives.java:520,566`.

   Go has node loading but no corresponding result fetch/cardinality API. This is required infrastructure for GuardiANN; replacing cardinality with a full count would change the intended algorithm and transaction footprint.

3. **Quick-start outward traversal.** `orderByDistance` gains a `shouldQuickStart` parameter. The zoom-in candidates may be emitted before outward expansion; already emitted quick-start keys are excluded later. With a nonzero starting radius, the target documents possible ordering inversions.

   Evidence: `HNSW.java:354,362`; `OutwardTraversalIterator.java:148,182,228`; `Search.java:631`.

   Go’s HNSW implementation exposes kNN search (`hnsw.go:1149`) but has no equivalent ring/outward iterator. Its existing SPFresh ordered stream is a different algorithm and continuation surface. **The missing base outward traversal is pre-existing; the quick-start option is new in this range.** Both are dependencies of faithful GuardiANN search.

4. **Operation randomness changes.** Target `RandomHelpers.random(Tuple)` folds every unsigned packed-key byte through SplitMix64, beginning at zero. UUID-based seeds use all UUID bytes. Deterministic UUID helpers provide stamped v4 random UUIDs and name-derived v3 identities; asynchronous item work receives deterministically split streams.

   Evidence: [RandomHelpers.java:51](/home/birdy/projects/fdb-record-layer-go/fdb-record-layer/fdb-extensions/src/main/java/com/apple/foundationdb/async/common/RandomHelpers.java:51), lines 101–106, 147, 174, 189, 208.

   Go still seeds operations from `splitMixLong(javaHashCode(pk.Pack()))` at `hnsw.go:2691`. First-insert rotation independently repeats the old seed calculation at line 536. Both need updating.

   **Do not change layer assignment as part of this port.** Target `Primitives.java:1110` still uses `primaryKey.hashCode()` for `topLayer`; Go’s corresponding method is `hnsw.go:2630`. Conflating operation seeding with layer selection would introduce a separate incompatibility.

The Go result path is also incomplete relative to the shared Java surface: `hnswSearchResult` and `VectorSearchResult` contain only PK and distance (`hnsw.go:1550`, `vector_index_maintainer.go:1300`), and `searchOnePartition` always writes `Value = Tuple{nil}` at [vector_index_maintainer.go:617](/home/birdy/projects/fdb-record-layer-go/pkg/recordlayer/vector_index_maintainer.go:617). Optional vector return is a **pre-existing gap**; covering-value return is a **new requirement**. Coordinate the record-layer result and continuation integration with its owning researcher.

Config conversion to a Java record, common-listener extraction, accessor renames, and most related HNSW test edits are language/refactoring changes. They do not justify changing numeric defaults without evidence.

**W3 — Numerical operations, exact encodings, and RaBitQ reconstruction**

Upstream: `17c9560fd` / **#4218** introduces SIMD operations; `4e3d42bc8` / **#4083** adds exact reductions and changes encoded-vector reconstruction.

Java adds an optional SIMD backend selected at initialization:

- `auto` attempts SIMD and falls back to scalar when unavailable.
- `scalar` forces scalar.
- Explicit `simd` fails if the backend cannot load.
- Vector operations, row/column matrix operations, and QR decomposition use the selected primitives. SIMD reductions and fused operations can change rounding.
- Exact dot, norm, squared-distance, and normalization variants force scalar accumulation. Exact norms recompute from data and bypass a previously cached SIMD norm.

Evidence: `E/linear/RealVectorPrimitives.java:130,249`; `RealVector.java:267`; the new backend and matrix parity tests.

The Java backend and incubator-module machinery have **no mandatory Go implementation analogue**. Adding SIMD or conducting performance work is unnecessary for this upgrade. The numerical and persistence contracts do matter.

RaBitQ now deliberately uses exact reductions for serialized calibration constants and normalization that feeds code selection: [RaBitQuantizer.java:161](/home/birdy/projects/fdb-record-layer-go/fdb-record-layer/fdb-extensions/src/main/java/com/apple/foundationdb/rabitq/RaBitQuantizer.java:161), line 391. The reason is functional: GuardiANN hashes encoded bytes to identify duplicate vectors.

Go’s `pkg/rabitq/rabitq.go:587` already uses a scalar loop, but source inspection does **not** establish exact byte parity:

- Java computes `xuCbNormSqr` by taking the square root of the exact sum and then squaring it (`RaBitQuantizer.java:170`).
- Go uses the dot-product sum directly (`rabitq.go:386`).
- Go clamps the error-calibration square-root argument with `math.Max(0, …)` (`rabitq.go:393`); target Java does not.
- Those two differences also exist relative to the Java base. They are **pre-existing encoding divergences newly coupled to GuardiANN’s content signatures**, not changes introduced by SIMD.

They require explicit RFC disposition and Java-byte regression coverage before reusing the Go encoder for GuardiANN. “Both are scalar” is not sufficient.

Encoded-vector reconstruction changes from the previous estimator-calibration reconstruction to norm-matched reconstruction:

```text
z[i] = code[i] - (2^extraBits - 0.5)
if !(fAddEx > 0) or sum(z[i]^2) == 0:
    return zeros
return z * sqrt(fAddEx / sum(z[i]^2))
```

Evidence: [EncodedRealVector.java:318](/home/birdy/projects/fdb-record-layer-go/fdb-record-layer/fdb-extensions/src/main/java/com/apple/foundationdb/rabitq/EncodedRealVector.java:318).

Go already implements the ordinary norm-matched idea in [rabitq.go:73](/home/birdy/projects/fdb-record-layer-go/pkg/rabitq/rabitq.go:73), but is incomplete:

- For zero, negative, or NaN `FAddEx`, it leaves the nonzero centered code vector unchanged rather than returning zeros.
- It computes `sqrt(FAddEx) / sqrt(sum)` rather than `sqrt(FAddEx / sum)`, which can differ in rounding.

The target behavior must be ported. It affects reconstructed output, pairwise distances used in graph maintenance, and clustering—not just display values.

**Wire layout does not change:** ordinal, three big-endian doubles, and code packing remain compatible. The interpretation of existing bytes changes when reconstructed. New target golden cases cover 3D/4-extra-bit, 128D/6-extra-bit, and 768D/8-extra-bit encodings under both scalar and SIMD backends (`ET/rabitq/RaBitQuantizerBackendDeterminismTest.java:61,100`).

Ordinary SIMD/scalar arithmetic tests permit numerical tolerance; persisted RaBitQ golden bytes do not. Whole-structure deterministic replay is explicitly scalar-only (`ET/async/guardiann/DeterministicReplayTest.java:87`). Do not extend that into an unsupported promise of identical ANN topology across arbitrary SIMD backends.

**W4 — K-means and partition evaluation**

Upstream: **#4083** and `24863e8af` / **#4604**.

Java K-means existed at the base. This range adds `k == 1`, computed directly as a mean, with metric-specific normalization or a farthest-vector fallback when the mean has a meaningless norm. It returns all-zero assignments, the cluster count, per-vector objective values, and their sum. Input validation still precedes this shortcut.

Evidence: `E/kmeans/KMeans.java:135,151,309`.

Partition evaluation replaces its previous smallest/largest-fraction balance pair with:

```text
relativeImbalance = sum((fraction[i] - 1/k)^2) * k/(k-1)
relativeImbalance(k=1) = 0
```

Both the gate and soft imbalance penalty use this normalized value. Only violation of `minChildFraction` makes a candidate invalid; other failed gates yield `KEEP_CURRENT`. A one-cluster merge passes the balance conditions by construction.

Evidence: [PartitionEvaluator.java:113](/home/birdy/projects/fdb-record-layer-go/fdb-record-layer/fdb-extensions/src/main/java/com/apple/foundationdb/kmeans/PartitionEvaluator.java:113), lines 124, 860, 925.

Required regression examples include:

- `(0.55, 0.435, 0.015)` being more imbalanced than `(0.60, 0.20, 0.20)`.
- Comparable normalized imbalance across different cluster counts.
- Single-cluster behavior.
- Distinguishing invalid candidates from admissible but rejected candidates.
- Accepted candidates outranking `KEEP_CURRENT` candidates even when their numeric score is lower.

The last item is a real upstream fix: `SplitMergeTask` previously selected by score alone, making soft gates ineffective. Target selection is at `E/async/guardiann/SplitMergeTask.java:1229,1250`; the regression is `ET/async/guardiann/SplitMergeTaskTest.java:42`.

Go’s [spfresh_kmeans.go:110](/home/birdy/projects/fdb-record-layer-go/pkg/recordlayer/spfresh_kmeans.go:110) is a deterministic, chunked Lloyd implementation with different validation, seeding, convergence, and result contracts. SPFresh’s split/merge policy is also different. It is **not an implementation of Java’s metric/estimator-aware K-means plus PartitionEvaluator**.

That Java-equivalent clustering gap predates the range; GuardiANN makes it a required dependency. Preserve SPFresh’s Go-only behavior rather than silently retuning it to Java’s new policy.

**W5 — GuardiANN: required new engine, including persistent maintenance**

GuardiANN is entirely new relative to the Java base. Principal commits are:

| Commit / PR | Scoped significance |
|---|---|
| `4e3d42bc8` / **#4083** | Engine, storage, search, replication, collapse, deferred tasks |
| `1ce8c71dd` / **#4357** | Record-layer integration dependency; assigned support/test evolution |
| `b9ceed781` / **#4418** | Deferred-execution controls, hard capacity, task execution accounting, stale-task robustness |
| `2abd40326` / **#4476** | Membership-aware collapsed-replica deduplication |
| `24863e8af` / **#4604** | Peak-relative merge policy, candidate verdict selection, normalized balance, defaults and diagnostics |

The Go baseline’s maintainer dispatch explicitly selects HNSW for `vector` and SPFresh for `vector_spfresh`; unknown types fail closed ([store.go:1250](/home/birdy/projects/fdb-record-layer-go/pkg/recordlayer/store.go:1250)). Actual SPFresh storage uses generation prefixes, integer cell/fine IDs, residual postings, memberships, sidecars, changelogs, and its own task rows ([spfresh_layout.go:21](/home/birdy/projects/fdb-record-layer-go/pkg/recordlayer/spfresh_layout.go:21)). Its leased rebalancer and periodic worker are likewise different.

Therefore GuardiANN is **required new parity work**. SPFresh remains an allowed Go extension, but cannot be counted as GuardiANN implementation.

The engine work divides into coupled units.

**W5a — Persistent representation and identities.** Target `StorageAdapter.java:57` defines:

| Root tag | Content |
|---|---|
| 0 | Access information: rotation seed and optional centroid |
| 1 | HNSW index of cluster centroids |
| 2 | Cluster metadata |
| 3 | Cluster vector references |
| 4 | Collapsed-signature membership |
| 5 | Primary-key vector metadata and covering values |
| 6 | Sample aggregates |
| 7 | Deferred tasks |

Vector references encode identity, role, collapsed status, vector bytes, and optional replica priority. Roles distinguish primary, underreplicated primary, and replica; collapsed status is separate. Primary-key metadata provides the current vector identity and covering tuple. Collapsed membership is keyed by signature plus the real primary key.

Signatures hash stored raw vector bytes using SHA-256, take 16 bytes, and stamp a version-8/IETF UUID (`StorageAdapter.java:540,557,589`). This is why encoder-byte fidelity is mandatory.

Cluster metadata stores underreplicated count, replica count, running statistics, state flags, and lifetime peak primary count (`StorageAdapter.java:340`). Welford statistics support add/remove/combine/subtract; the historical maximum does not shrink on removal, and only tiny negative roundoff in `M2` is clamped (`RunningStats.java:55,74,137`).

**Compatibility discrepancy:** PR #4604’s message claims tolerant reading of the widened metadata tuple. The pinned target unconditionally executes `valueTuple.getLong(4)` at [StorageAdapter.java:347](/home/birdy/projects/fdb-record-layer-go/fdb-record-layer/fdb-extensions/src/main/java/com/apple/foundationdb/async/guardiann/StorageAdapter.java:347). There is no missing-field fallback there. An intermediate GuardiANN database with the earlier four-field metadata cannot be assumed readable by this target. GuardiANN did not exist at the Java base, so this is an intermediate-version compatibility issue rather than a base-HNSW migration.

**W5b — Writes, deletes, search and result semantics.**

- Insert checks existing primary-key metadata and is a no-op for an existing key. Updating requires the delete/insert lifecycle.
- Optional in-transaction maintenance executes one deferred task before the write.
- The first vector bootstraps the centroid index. Quantized translation-preserving metrics use sampled centroid establishment; the cosine path can establish a zero-centroid rotation immediately.
- Primary assignment, priority filtering, occlusion-based replica selection, metadata/statistics updates, and task scheduling are transactional.
- With deferred maintenance, inserting beyond `primaryClusterHardMax` fails. The check is conditional on maintenance not being executed in the transaction (`Insert.java:335`).
- Delete checks current vector metadata, searches bounded candidate clusters, removes found references, handles collapsed membership, and clears primary-key metadata. It does not promise to physically remove every stale replica immediately.

Evidence: `Guardiann.java:280,301`; `Insert.java:181,291,335`; `Delete.java:103,139,227`.

kNN search uses centroid traversal, bounded candidate clusters, distance-ratio suffix pruning, distinct candidate retention, collapsed expansion, and current-metadata filtering. It may return fewer than `k` after stale candidates are removed; it does **not** automatically refill indefinitely (`Search.java:415`).

The ordered search path expands collapsed references before applying the radius/PK cutoff, uses bounded “almost sorted” buffering, validates current metadata, then deduplicates real primary keys (`Search.java:570`). This is not an exact global sort. A faithful port must not substitute SPFresh’s streaming semantics or infer an exact ordering guarantee from the method name.

Default search settings are factor **1.40**, maximum clusters **48**, minimum before pruning **16**, distance-ratio cutoff **1.5**, centroid ring/outward widths **100/400**, concurrency **10** (`SearchConfig.java:80`). The candidate factor changed from the initial in-range value 1.15.

Deterministic identities must follow the actual target helpers. In particular, name-derived IDs can repeat for the same primary key; do not invent a stronger “always fresh generation” guarantee for deterministic mode.

**W5c — Durable task execution and accounting.**

The task family comprises split/merge, reassign, bounce, and collapse. Task IDs encode priority in their high bit, consistent with tuple UUID ordering; payloads carry task-specific centroids, cluster sets, dependencies, and follow-up kind.

Execution:

- Fetches a bounded batch in key order.
- Executes the first task even if the supplied deadline has elapsed; subsequent tasks check the deadline.
- Point-reads each task again with read-your-writes semantics.
- Skips a task already removed by an earlier task in the same transaction.
- Removes the task before running it.
- Fires the execution listener only for work actually performed.

Evidence: [Primitives.java:984](/home/birdy/projects/fdb-record-layer-go/fdb-record-layer/fdb-extensions/src/main/java/com/apple/foundationdb/async/guardiann/Primitives.java:984), lines 1016–1029; `AbstractDeferredTask.java:322,349`.

That skip is essential: a bounce can execute a dependency that is also in the fetched batch. Executing/reporting it again corrupts outstanding-work accounting. Tests explicitly cover absent and present tasks (`CollapseScenarioTest.java:791,824`).

Bounce tasks actively run an outstanding dependency, re-enqueue remaining dependencies under a fresh task ID, and schedule follow-up work only where its target still exists and is eligible. Simply requeueing a bounce without making progress is not equivalent.

The engine listener/accounting contract couples to record-layer pending counts, merge scheduling, per-prefix coordination and budgets. Those record-layer files belong to another research group. **They are required integration dependencies, not omitted features.** A GuardiANN port that can enqueue but cannot reliably drain its queue is incomplete.

**W5d — Collapse, reassignment, split/merge and final policy.**

Collapse uses identical **stored bytes**, with a strict `count > collapseMinDuplicates` threshold. A representative stands for individual live records recorded in the collapsed-membership store; query expansion must preserve all of them.

The #4476 correction is especially important:

- An already collapsed representative claims its signature immediately.
- A plain replica requires a membership lookup.
- A genuine member can fold into one representative.
- A fresh same-byte record that is not a member remains a distinct record.
- Highest-priority ordering determines the retained representative; pruning follows folding.

Evidence: `ReassignTask.java:471`; regressions at `CollapseScenarioTest.java:597,703`.

Reassignment sorts candidate homes by each vector’s distance, handles vanished neighbors, moves authoritative copies, repairs replication, preserves membership-aware collapse semantics, and updates statistics and flags. Its replication gate uses actual underreplication or membership in the cause-cluster set; an empty cause set must not be interpreted as “all clusters” merely from prose.

Split/merge supports 1→2 and 2→3 splits, and 2→1 and 3→2 merges, with collapse/fallback paths. Deferred tasks must tolerate neighbors that have disappeared since enqueue. Candidate selection first respects evaluator verdict, then score.

The final merge trigger is:

```text
currentPrimaryCount <
    max(primaryClusterMin, floor(mergeMaxEverFraction * lifetimePeak))
```

The peak must increase on growth and survive shrinkage. The constructor rejects a peak below current count; it does not silently repair it (`ClusterMetadata.java:63,117`).

Merge scheduling checks that the centroid index has more than one cluster (`Primitives.java:1324`). After deleting everything and draining maintenance, the target permits **one empty cluster**; it should not churn impossible merges.

Final defaults include primary max/min/hard max **1000/100/2000**, peak fraction **0.20**, minimum child fraction **0.10**, maximum relative imbalance **0.36**, split imbalance penalty **3**, replica write limit/target **300/100**, underreplicated limit **50**, priority minimum **0.89**, collapse threshold **100**, and K-means **8 iterations/3 restarts** (`Config.java:143`). `ConfigRecommendation` is a **test utility**, not a new production auto-tuner.

History matters here: **#4387 (`8deb8fee2`) only relaxed test tolerances** for primaries assigned to a non-nearest cluster—from 2% to 8% in the standard case and 5% to 8% after deletes. It is not an algorithm fix and is not authorization to weaken Go expectations.

**W6 — Lucene queues, transaction boundaries, quota accounting and result conversion**

The Lucene backend is absent from the Go baseline. This was verified through maintainer dispatch and implementation, not just filename searches: Go’s text index is a BunchedMap token/position index (`pkg/recordlayer/text_index_maintainer.go:21`), and unknown index types fail closed at `store.go:1282`.

That is a **pre-existing major parity gap**. The following range changes remain required Lucene work; absence of the backend does not make them completed or irrelevant.

- **Queue scan/conflict correction — `359f80259`, #4380.** `PendingWriteQueue.getQueueCursor` forces snapshot reads on the inner KV cursor even when the caller requests serializable isolation. Entry-level row/byte/skip limits remain on the unsplitter. Clearing an entry explicitly adds a read-conflict range covering that entry before deleting its split values and decrementing the count. The empty check remains serializable.

  Evidence: [PendingWriteQueue.java:199](/home/birdy/projects/fdb-record-layer-go/fdb-record-layer/fdb-record-layer-lucene/src/main/java/com/apple/foundationdb/record/lucene/directory/PendingWriteQueue.java:199), lines 253 and 281.

  This permits enqueue concurrent with drain, conflicts same-entry drains, permits disjoint drains, and still makes closeout conflict with concurrent new work. The queue’s versionstamp/incarnation ordering, split-entry representation and payload format are not changed by this scoped fix.

- **Heartbeat during drain — `a58671005`, #4465.** Every transaction created by the drain cursor factory registers the index’s pre-commit callback under the drain hook. Retries must also register it (`FDBDirectoryWrapper.java:594,615`). Removal of explicit `withCommitWhenDone(true)` does not disable commit: the target throttled iterator defaults to true. That dependency was checked directly.

- **Clear-size accounting — `d1fdd9849`, #4574.** `AgileContext.clear(key)` charges key length; clearing a range charges both endpoint lengths (`AgileContext.java:285,293`). These operations participate in quota-triggered commits. Reads and arbitrary work passed through `accept`/`apply` are not automatically byte-accounted. This is transaction behavior, not merely metrics.

- **Explicit spell-check entry conversion — `576773759`, #4547.** `LuceneIndexSpellCheckQueryPlan` implements the shared entry-to-queried-record contract. It derives the indexed record type from metadata, constructs the suggestion-shaped partial record, and carries **no primary key**. Fetching maps every entry through that conversion (`LuceneIndexSpellCheckQueryPlan.java:64,80`). It remains unsuitable for the generic covering-index optimization.

- **Index-state predicates — `4b0143ac8`, #4410.** Lucene’s write-only decisions use the target predicate (`LuceneIndexMaintainer.java:225,410`). The relevant dependency is substantive: target `IndexState.isWriteOnly()` includes both ordinary write-only and new `WRITE_ONLY_WITH_QUEUE`, code **4**. Go [index_state.go:35](/home/birdy/projects/fdb-record-layer-go/pkg/recordlayer/index_state.go:35) recognizes only ordinary write-only, and its decoder at line 62 accepts only codes 0–3.

The generic pending-write queue and online-indexer rollout belong to the core researcher’s population. This audit establishes their coupling to the assigned Lucene changes; it does not claim complete coverage of those core implementations.

**W7 — Java-only, build, documentation and test maintenance**

No production collation change appears in the three assigned ICU test diffs. They adopt renamed shared test bases and fixture collection APIs.

The R-tree/client-log changes are SpotBugs annotations/imports, not algorithm changes. `FDBIndexOutput` is likewise annotation-only. `SystemKeyspace` adds documentation of the metadata-version key; it does not change its value or introduce a migration.

The four assigned Gradle paths contain test-fixture wiring, JUnit/configuration updates, lazy/configuration-cache task changes, SIMD test/module setup, and dataset-download/build-task changes. Relevant history includes #4218, #4264, #4294, #4297, #4365, #4367, #4388 and #4516. These are handed to the controller’s build work; they are not Go runtime ports.

Lucene integrity tests now restore their global integrity-check switch in `finally`. HNSW `OperationsTest` switches to bounded, interruptible waits in #4609. These improve test isolation and failure handling; they do not prove runtime parity.

**Regression contracts and expectation changes**

The implementation RFC should require the following targeted contracts, using existing test infrastructure.

| Area | Required contracts |
|---|---|
| HNSW storage | Read legacy three-field nodes; round-trip fourth-field covering tuples; preserve values through neighbor rewrites, deletion repair, and cold-cache reads; keep upper-layer behavior |
| HNSW APIs | Fetch missing/present nodes, distance/rank conventions, optional vectors, covering values independent of vector inclusion, cardinality 0/1/2/many |
| Randomness | Packed-key byte-fold known answers, unsigned bytes, UUID seed/stamping, deterministic split order; separately preserve existing `topLayer` answers |
| RaBitQ | Target golden encoding bytes; norm-matched reconstruction; zero/non-positive/NaN stored norm handling; encoded arithmetic; unchanged wire parsing |
| Clustering | One-cluster closed form and validation, cosine cancellation fallback, normalized imbalance, verdict-before-score selection |
| GuardiANN lifecycle | Insert/delete/update, primary/replica accounting, hard capacity, collapse and recollapse, fresh same-byte nonmembers, stale metadata filtering, vanished deferred-task neighbors |
| Tasks | Priority encoding, serialization, expired-deadline first task, rollback, bounce progress, already-consumed dependency skip, exactly-once listener accounting |
| Merge policy | Peak growth/shrink preservation, threshold boundaries, merge after reassignment, no impossible merge on the last cluster, delete-all drain without test-only reconciliation |
| Search | Candidate limits, pruning floor, collapsed expansion before continuation cutoff, distance/PK ties, bounded ordering, no duplicate live PKs, allowed underfill |
| Lucene | Concurrent enqueue/drain, same-entry conflict, disjoint drains, serializable empty closeout, retry hook registration, clear-byte quotas, suggestion records without PKs |

Existing Go expectations need careful classification:

- `vector_index_test.go:1868` normalizes both original and reconstructed vectors before comparing direction. It **cannot catch a reconstructed-magnitude defect**. `pkg/rabitq/rabitq_test.go:598` checks only decoded dimensionality; its zero-vector test at line 511 tests encoding/estimation, not decoding. Add the missing contracts; do not merely lower their thresholds.
- `vector_index_test.go:619` exercises the three-field compact format. Keep that legacy case and add four-field preservation cases.
- I did not find a direct Go assertion requiring the old Java RaBitQ reconstruction formula. The observed issue is incomplete coverage plus the concrete decoder mismatch above.
- The directly mapped shared numeric surface has **pre-existing** divergences: Go cosine distance returns 1 for a zero norm and clamps similarity, while Java returns positive infinity for zero norms and does not apply that clamp (`MetricDefinition.java:209`). Go explicitly enshrines the zero result at `vector_index_test.go:2467` and nonnegative clamping at line 2933. These are separate from range-introduced SIMD work and must be recorded honestly in the parity ledger.
- Direct Go graph insert performs an upsert (`hnsw.go:306`), whereas both Java tags no-op on an existing PK (`Insert.java:196`). Tests at `vector_index_test.go:440,1266,1674` explicitly require upsert behavior. This is another **pre-existing** discrepancy. The ordinary Go record maintainer already deletes then inserts (`vector_index_maintainer.go:322,350`). An RFC can preserve a separately identified Go convenience upsert, but must not call it the Java insert contract.
- Existing primitive SplitMix/SplittableRandom and top-layer tests remain useful. The operation-seed change is not permission to refresh all random-derived outputs.
- Upstream SIFT thresholds are quality contracts, not exact structural proofs. Target tests use recall at least 0.80 and ordered quality at least 0.90, and deliberately tolerate some stale/incorrectly assigned references. The #4387 relaxation must not be imported as a blanket Go expectation change.

There should be **no expectation weakening or bulk golden refresh**. Each changed expected result needs its source behavior, affected contract, and reason recorded. Exact persisted-byte tests must remain exact.

The controller-owned mixed-array/promote work remains outside this population. The accepted pre-existing **structured PromoteValue defect is re-armed and must close in this upgrade**; it is not an accepted exception. Nothing in this report authorizes the five restricted hunts, a new hunt campaign, or unrelated performance work.

**Compatibility and rollout implications**

- **HNSW compact storage:** target reads old nodes; baseline Go ignores new covering data and can erase it on rewrite. Mixed-writer use needs a covering-value-capable Go writer before claiming safety.
- **HNSW randomness:** new operations can produce different sampling, rotation seeds, repair choices and topology. Stored access information remains meaningful; a pin change alone should not trigger arbitrary index rewrites.
- **RaBitQ:** same wire shape, changed reconstruction; exact encoder differences affect content signatures. Treat byte compatibility and numerical approximation as distinct tests.
- **GuardiANN:** new layout, task formats, state flags and UUID identities need explicit codec contracts. It is not interchangeable with SPFresh storage. The target’s fifth metadata field requires special attention for intermediate GuardiANN data.
- **Continuations:** no continuation-protobuf format change is defined in these assigned extension paths. Covering/result additions and ordered distance/PK cutoff semantics must be integrated with the owner of record-layer continuation changes. Existing Go HNSW tokens serialize result entries and position; SPFresh stream tokens are not substitutes.
- **Pending queues:** preserve split-entry boundaries, incarnation/versionstamp ordering and the empty-check conflict gate. New state code 4 is a shared schema/behavior dependency and cannot be handled by merely renaming predicates.

**Prioritized port sequence**

| Priority | Concrete work | Dependencies |
|---|---|---|
| P0 — RFC prerequisite | Resolve storage/result compatibility, exact RaBitQ encoding, operation RNG versus layer RNG, intermediate GuardiANN metadata handling, and existing shared-surface expectation conflicts | W2, W3, owning metadata/continuation researchers |
| P1 | Port HNSW covering-value preservation, common result shape, fetch/cardinality, and outward/quick-start traversal | W1; existing HNSW |
| P1 | Align RaBitQ reconstruction and establish target exact-byte contracts; provide Java-equivalent clustering/evaluation needed by GuardiANN | W3, W4 |
| P1 | Implement GuardiANN storage, writes, search, replication, collapse, reassignment and final split/merge policy | W1–W4, W5a/W5b/W5d |
| P1 | Complete GuardiANN task draining, accounting, scheduling, budgets and coordination | W5c plus controller/core integration; cannot be deferred after “feature complete” |
| P1 | Carry Lucene queue conflict, heartbeat, clear-quota and conversion requirements into the Lucene/backend parity work; integrate queue-aware index-state semantics | W6 plus core pending-queue owner |
| P2 | Integrate diagnostics, instrumentation and targeted regression fixtures; apply Java-only harness/build changes through the controller | Functional ports first; no performance campaign |

Absent major features remain listed as parity work or explicit pre-existing dependencies. Their size is not a reason to mark them out of scope.

**Exhaustive path ledger**

Every entry below was included in the complete diff read. `Wn` refers to the work unit above. `T` means test source reviewed, execution still needed; `F` means fixture/support source reviewed; `J` means Java/build/documentation-only change. Grouping here accounts for paths; it does not imply that all files in a semantic group are mechanically equivalent.

Under `E/async` — **1 path**:

```text
MoreAsyncUtil.java — W1, behavioral/API changes
```

Under `E/async/common` — **11 paths**:

```text
AggregatedVector.java — W1, shared aggregate representation
DistinctTopK.java — W1, comparator-distinct bounded collector
OnKeyValueReadListener.java — W1, shared read instrumentation contract
OnKeyValueWriteListener.java — W1, write/range-delete instrumentation
RandomHelpers.java — W2/W5, changed seeds and identity helpers
ResultEntry.java — W2, relocated/expanded result contract
StorageHelpers.java — W1/W2, extracted codec/sample operations
StorageTransform.java — W1/W3, shared coordinate-system handling
TimedAsyncIterable.java — W1, completion/cancellation timing
TopK.java — W1, duplicate-preserving bounded collector
VectorEncodingConfig.java — W1/W3, shared codec configuration
```

Under `E/async/guardiann` — **38 paths**, all part of the new required engine:

```text
AbstractDeferredTask.java — W5c/W5d, task identity, payloads and cluster deltas
AccessInfo.java — W5a, stored transform information
AlmostSortedAsyncIterator.java — W5b, bounded reorder algorithm
BounceTask.java — W5c, dependency execution and follow-ups
Cluster.java — W5a/W5d, cluster working representation
ClusterCapacityExceededException.java — W5b, deferred-write capacity failure
ClusterMetadata.java — W5a/W5d, statistics, flags and lifetime peak
ClusterMetadataWithDistance.java — W5b/W5d, ordered cluster candidates
ClusterReference.java — W5a, centroid identity/reference
ClusterView.java — W5d, diagnostic cluster view
CollapseTask.java — W5d, duplicate collapse and persistence
Config.java — W5, production options/defaults/validation
Delete.java — W5b, bounded deletion and stale-reference lifecycle
Guardiann.java — W5, engine API
Insert.java — W5b, assignment, replication and task scheduling
Locator.java — W5, engine dependency/configuration holder
OnReadListener.java — W5, engine read instrumentation
OnWriteListener.java — W5c, mutation and task-accounting callbacks
PrimaryCopy.java — W5a/W5d, authoritative-copy representation
Primitives.java — W5, storage operations and task/policy machinery
ReassignTask.java — W5d, reassignment and corrected collapsed folding
ReplicaSelection.java — W5d, replica-selection result representation
ReplicatedCopy.java — W5a/W5d, replica representation
ReplicationStats.java — W5d, replication statistics
RunningStats.java — W5a/W5d, reversible/mergeable running statistics
Search.java — W5b, kNN, ordered retrieval and diagnostics
SearchConfig.java — W5b, query defaults/validation
SplitMergeTask.java — W5d/W4, repartitioning and candidate selection
StorageAdapter.java — W5a, keyspaces, tuple codecs, signatures and scoring
StructureSnapshot.java — W5d, structure/assignment/merge diagnostics
TaskKind.java — W5c, task-kind encoding
VectorId.java — W5a, primary-key plus UUID identity
VectorMetadata.java — W5a/W5b, current identity and covering values
VectorRecord.java — W5b, enriched result representation
VectorReference.java — W5a/W5d, role/collapse/reference semantics
VectorReferenceAndDistance.java — W5b, scored references
VectorReferenceVectorLens.java — W5d/W4, clustering vector access
package-info.java — J, package documentation; W5 context
```

Under `E/async/hnsw` — **22 paths**:

```text
AbstractNode.java — W2, node contract/common-type integration
AbstractStorageAdapter.java — W2, shared storage/config integration
AccessInfo.java — W2, accessor/common-code integration
Cardinality.java — W2, new coarse-cardinality API
CompactNode.java — W2, covering-value retention
CompactStorageAdapter.java — W2, three/four-field codec and preservation
Config.java — W2/J, record/accessor conversion
Delete.java — W2, common RNG and covering-aware repair
DeleteNeighborsChangeSet.java — W2, covering-preserving neighbor changes
HNSW.java — W2, fetch/cardinality/result/traversal API
InliningNode.java — W2, node-factory contract adaptation
InliningStorageAdapter.java — W2, common codec/config adaptation
Insert.java — W2, operation RNG and layer-zero covering values
NodeFactory.java — W2, additional-values creation contract
NodeReferenceWithVectorAndAdditionalValues.java — W2, new repair/fetch carrier
OnReadListener.java — W1/W2, shared listener inheritance
OnWriteListener.java — W1/W2, shared listener inheritance
OutwardTraversalIterator.java — W2, quick-start behavior and trace handling
Primitives.java — W2, fetch/cardinality/scan/RNG/storage helpers
ResultEntry.java — W2, deleted; replacement is common/ResultEntry.java
Search.java — W2, covering/reconstructed results and traversal option
StorageAdapter.java — W1/W2, helper extraction and codec delegation
```

The following **six extension production paths** are annotation-only in the complete diff, from SpotBugs work; no runtime algorithm port is indicated:

```text
E/async/rtree/AbstractNode.java — J
E/async/rtree/ChildSlot.java — J
E/async/rtree/RTree.java — J
E/clientlog/FDBClientLogEvents.java — J
E/clientlog/TupleKeyCountTree.java — J
E/clientlog/VersionFromTimestamp.java — J
```

Under `E/kmeans` — **2 paths**:

```text
KMeans.java — W4, one-cluster result and related documentation
PartitionEvaluator.java — W4, normalized balance/gates/scoring
```

Under `E/linear` — **18 paths**:

```text
AbstractRealVector.java — W3, backend primitive routing
Backend.java — W3/J, numerical backend interface
ColumnMajorRealMatrix.java — W3, matrix primitive specialization
DoubleRealVector.java — W3, primitive-based norm handling
FloatRealVector.java — W3, primitive-based norm handling
HalfRealVector.java — W3, primitive-based norm handling
MetricDefinition.java — W3, backend distance reductions
MutableDoubleRealVector.java — W3, array-based mutable operations
QRDecomposition.java — W3, backend dot/norm/AXPY calculations
Quantizer.java — W3/J, transformed-vector construction adaptation
RealVector.java — W3, backend operations and exact APIs
RealVectorPrimitives.java — W3, selection and exact/non-exact primitives
RowMajorRealMatrix.java — W3, matrix primitive specialization
ScalarBackend.java — W3, scalar numerical contract
Transformed.java — W3/W4, private construction and underlying lens
VectorOperator.java — W3/J, lens-based transformed construction
simd/SimdBackend.java — W3/J, optional JVM SIMD implementation
simd/package-info.java — J, SIMD package documentation
```

Remaining extension production paths — **4 paths**:

```text
E/rabitq/EncodedRealVector.java — W3, reconstruction and backend integration
E/rabitq/RaBitDistanceEstimator.java — J, copyright filename correction
E/rabitq/RaBitQuantizer.java — W3, exact encoding reductions
E/system/SystemKeyspace.java — J, metadata-version-key documentation
```

Under `ET/async` and `ET/async/common` — **7 paths**:

```text
MoreAsyncUtilTest.java — T/W1, new async correctness contracts
common/BaseTest.java — F, shared extension test setup
common/CommonTestHelpers.java — F, common test helpers
common/DistinctTopKTest.java — T/W1, comparator-distinct collection
common/PrimaryKeyVectorAndDistance.java — F, result test carrier
common/TimedAsyncIterableTest.java — T/W1, timing/report-once lifecycle
common/TopKTest.java — T/W1, heap retention/order contracts
```

Under `ET/async/guardiann` — **18 paths**:

```text
AlmostSortedAsyncIteratorTest.java — T/W5b, bounded ordering
ClusterMetadataTest.java — T/W5d, peak and merge-threshold invariants
CollapseScenarioTest.java — T/W5c/W5d, collapse/folding/task regressions
ConfigRecommendation.java — F, test-only derived configuration
ConfigRecommendationTest.java — T, recommendation coherence
ConfigTest.java — T/W5, production config validation
DataRecordsTest.java — T/W5, data/value contracts
DeleteReplicationPersistenceTest.java — T/W5, surviving replication
DeterministicReplayTest.java — T/W5, scalar deterministic state replay
MergeScenarioTest.java — T/W5d, merge persistence/lifecycle
ReassignConvergenceTest.java — T/W5d, reassignment improvement
ReassignScenarioTest.java — T/W5d, reassignment scenarios
RunningStatsTest.java — T/W5a, statistical operations
SearchConfigTest.java — T/W5b, query parameter validation
SiftTest.java — T/W5, recall, mutation and production drain scenarios
SplitMergeSplitScenarioTest.java — T/W5d, split persistence
SplitMergeTaskTest.java — T/W4/W5d, verdict-first candidate selection
TestHelpers.java — F/W5, scenario helpers and invariant changes
```

Under `ET/async/hnsw` — **6 paths**:

```text
CardinalityTest.java — T/W2, new 0/1/2/many classification
ConfigTest.java — T/J, record/accessor adaptation
DataRecordsTest.java — T/W2, expanded data contracts
OperationsTest.java — T/W2, API adaptation and bounded waits
SiftTest.java — T/W2, common types/new API arguments
TestHelpers.java — F/W2, common helpers and changed signatures
```

Under `ET/kmeans`, `ET/linear`, and `ET/rabitq` — **10 paths**:

```text
kmeans/KMeansTest.java — T/W4, one-cluster contracts
kmeans/PartitionEvaluatorTest.java — T/W4, normalized balance/gates
linear/BackendSelectionTest.java — T/W3, auto/scalar/strict-SIMD selection
linear/QRDecompositionTest.java — T/W3, backend QR behavior
linear/RealMatrixParityTest.java — T/W3, scalar/SIMD matrix comparisons
linear/RealMatrixTest.java — T/W3, matrix contracts
linear/RealVectorExactReductionTest.java — T/W3, exact reduction guarantees
linear/RealVectorPrimitivesParityTest.java — T/W3, ordinary backend parity
rabitq/RaBitQuantizerBackendDeterminismTest.java — T/W3, exact wire goldens
rabitq/RaBitQuantizerTest.java — T/W3, encoded arithmetic/reconstruction
```

Under `EF/async` — **6 paths**:

```text
common/PrimaryKeyAndVector.java — F, shared data carrier
common/package-info.java — J/F, fixture package documentation
guardiann/GuardiannStructureAsserts.java — F/W5, extracted and extended invariants
guardiann/SiftTestHelpers.java — F/W5, shared SIFT support
guardiann/VecsDatasetLoaders.java — F/W5, vector/ground-truth dataset loading
guardiann/package-info.java — J/F, fixture package documentation
```

These six are explicitly **not all classified as mechanical relocation**. In particular, structure assertions acquire new peak/merge/orphan diagnostics, and dataset helpers are part of the changed test support.

Under `IT` — **3 paths**, fixture/base-class adaptation with unchanged collation assertions:

```text
CollateFunctionKeyExpressionICUTest.java — T/J
FDBCollateICUQueryTest.java — T/J
TextCollatorICUTest.java — T/J
```

Under `L` — **6 paths**:

```text
LuceneIndexMaintainer.java — W6, queue-aware state predicate dependency
LuceneIndexSpellCheckQueryPlan.java — W6, explicit suggestion conversion
directory/AgileContext.java — W6, clear-size quota accounting
directory/FDBDirectoryWrapper.java — W6, drain pre-commit hook registration
directory/FDBIndexOutput.java — J, SpotBugs annotation only
directory/PendingWriteQueue.java — W6, scan/clear conflict corrections
```

Under `LT` — **8 paths**:

```text
LuceneIndexScrubbingTest.java — T/J, index-state predicate adaptation
LuceneIndexSpellCheckQueryPlanTest.java — T/W6, suggestion shape/no-PK conversion
LuceneIndexTest.java — T/J, restore global integrity flag in finally
LuceneOnlineIndexingTest.java — T/W6, state predicate adaptation
directory/AgilityContextTest.java — T/W6, clear quotas/read accounting
directory/FDBLuceneFunctionalityTest.java — T/J, restore integrity flag in finally
directory/PendingWriteQueueTest.java — T/W6, concurrency/closeout regressions
highlight/LuceneScaleTest.java — T/J, readable predicate adaptation
```

The four build paths — **4 paths**, read and handed to controller build ownership:

```text
fdb-extensions/fdb-extensions.gradle — J, SIMD/fixtures/dataset/build-task changes
fdb-record-layer-icu/fdb-record-layer-icu.gradle — J, test-fixture/build wiring
fdb-record-layer-lucene/fdb-record-layer-lucene.gradle — J, test-fixture/build wiring
fdb-record-layer-spatial/fdb-record-layer-spatial.gradle — J, test-fixture/build wiring
```

Ledger total: **170 / 170 assigned paths**. All behavioral conclusions above are source-derived; the listed regression and compatibility contracts still require implementation review and runtime verification after the RFC is accepted.