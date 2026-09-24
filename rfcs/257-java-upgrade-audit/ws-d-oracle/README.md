# WS-D live-JVM oracle: measured target GuardiANN behaviour

Test-only oracle; no production WS-D code exists yet. The Java side drives the
real 4.14.2.0 jars (`conformance/guardiann_probe_conformance.java`, plus
`conformance/GuardiannConformanceAccess.java` in the GuardiANN package so it can
construct package-private `VectorId` and decode metadata with the production
decoder). The Go side is `conformance/guardiann_probe_conformance_test.go`,
Describe `GuardiANN target oracle`, in the hand-maintained target
`//conformance:rfc257_oracle_test` (a separate target since design v8; see conformance/BUILD.bazel).

Measured (`/var/tmp/fdb-upgrade-recovery/ws-d-oracle-4.log`: 4/1527 specs,
uncached, all pass):

1. **VectorId hash.** Seven packed PKs/UUIDs (negative, 2^40, UTF-8 string,
   bytes, nested tuple with null, bool/double; all-zero, all-one and high-bit
   UUIDs): `VectorId.hashCode == 31*Arrays.hashCode(pk.pack()) + uuid.hashCode()`
   on the runtime (JDK feature version 21, fdb-java jar `fdb-java-7.1.26`,
   fdb-extensions 4.14.2.0), and `Tuple.hashCode == Arrays.hashCode(pack())`.
   Mutation: treating bytes as unsigned in the Go formula fails the spec
   (`ws-d-oracle-hash-mutation.log`, 1/1 selected spec failed); restored.
2. **Control split.** 10 near + 2 far, primaryClusterMax 10: neighbour
   persistence step, then a valid split into clusters of 2 and 10 primaries;
   queue drains.
3. **Split with no usable candidate (CONFIRMED target defect).** 10 near + 1
   far: after the persistence step every execution throws
   `java.util.NoSuchElementException` at `SplitMergeTask` line 397 (the
   `orElseThrow`); the task is never consumed and the 11-primary cluster keeps
   SPLIT_MERGE. This is the stall the design's throw-site terminal rule removes.
4. **Split beside an emptied neighbour (CONFIRMED target defect).** Build two
   clusters, delete both far vectors (primaryClusterMin 0 keeps the empty cluster
   out of merge), grow the near cluster to 11: every split execution throws
   `com.google.common.base.VerifyException` at
   `SplitMergeTask.writeClusterMetadataAndEnqueueTasks:909`; the task is never
   consumed. With default primaryClusterMin the same shape depends on task-UUID
   order between the pending merge and the split.

These convert two design source hypotheses into measured facts. They are
retained as regressions of the TARGET, so a Java upgrade that fixes either
defect reddens the spec and forces the Go divergence entry to be revisited.

Added after the first four (`/var/tmp/fdb-upgrade-recovery/ws-d-oracle-5.log`:
6/1529 selected specs, uncached, all pass):

5. **Queue-replay capacity stranding (CONFIRMED).** Record-layer GuardiANN index
   (hard cap 12), marked WRITE_ONLY_WITH_QUEUE on an empty store, built with the
   pending-queue policy and markReadable=false, then 30 one-cluster saves queued
   (all 30 succeed). `buildIndex(true)` drains: 12 entries commit, then the build
   fails with `VectorIndexClusterTooLargeException` caused by
   `ClusterCapacityExceededException` from `Insert.writeNeighboringClusterReferences`;
   18 queue entries remain, the index stays WRITE_ONLY_WITH_QUEUE, and the one
   cluster holds 12 primaries with SPLIT_MERGE pending (the committed split task
   never runs because no merge happens during the drain). This is the stranded
   shape the design's capacity recovery removes.
6. **Ordinary-save backpressure.** The same index left READABLE at store
   creation: saves 0-11 succeed, save 12 fails with
   `ClusterCapacityExceededException`; one cluster, 12 primaries, SPLIT_MERGE
   pending. Ordinary saves run with deferred maintenance, so a GuardiANN index
   needs external merges; the Go port must reproduce this contract.

7. **Degenerate-knob consumer map (golden, 22 variants).** Spec `maps which
   GuardiANN consumers throw for degenerate knob values` overrides one (or a
   listed set of) Config knob(s) and runs insert → drain → delete → drain →
   search → grow (second split beside a neighbour) → drain → shrink (delete-driven
   merges) → drain, plus a separate 11-identical-vector collapse structure. Two
   runs produced identical summaries (`ws-d-oracle-knobs.log`,
   `ws-d-oracle-knobs-2.log`); they are pinned verbatim. Measured:
   - throw forever at the consumer: kMeansMaxIterations 0 and kMeansMaxRestarts -1
     (`IllegalArgumentException` at `SplitMergeTask.kMeans`), DOT_PRODUCT and
     EUCLIDEAN_SQUARE metrics (`UnsupportedOperationException` there),
     collapseConcurrency 0 (`Primitives.fetchCoreClusters`), bounceConcurrency 0
     (`BounceTask.runTask`), deleteConcurrency 0 (record delete throws);
   - livelock (every one of 40 bounded drain rounds executes a task, the queue
     never empties): splitNumNearestClusters 0, mergeNumNearestClusters 0 (merge
     path), splitMergeConcurrency 0, reassignConcurrency 0;
   - degrade without throwing: insertMaxCandidateClusters 0 (inserts succeed but
     no reference is written: 0 search results), deleteMaxCandidateClusters 0
     (deletes remove identity but not references; counts unchanged);
   - no observable effect in these scenarios: reassignNumNeighboringClusters 0,
     replicatedClusterTarget 0, sampleBatchSize 0 (even with RaBitQ training);
   - RaBitQ extra bits 9 (Euclidean, trained after 5 samples): inserts and
     searches after training throw at `Primitives.quantizer`; deletes that
     enqueue no task succeed; with DOT_PRODUCT the first insert throws;
   - RaBitQ bits 4 trained mid-collapse: 5 pre-training plain duplicates and 6
     post-training encoded duplicates do NOT unify (6 primaries, 11 search hits),
     confirming signature input follows the supplied representation.
   Mutation: altering the pinned SplitNumNearestClusters summary (verified present)
   failed the spec (`ws-d-oracle-knob-mutation.log`); restored. An earlier
   mutation attempt whose pattern did not match was NOT credited.

Added for design v6 (`evidence-run.txt`: the full oracle, 9/1532 selected specs,
uncached, all pass, with the sha256 of every oracle source file;
`evidence-mutations.txt`: every mutation kill, each verified present first):

8. **Inline maintenance mode (golden, 8 scenarios, identical over two runs).**
   Engine inserts with `maintainInTransaction=true`, which runs one queued task
   before each insert and skips the hard cap (Insert.java:330-338):
   - primaryClusterMax 10, hardMax 11: all 20 inline inserts succeed and the index
     splits into three clusters; a deferred backlog already AT the cap (12) is
     relieved by 10 inline inserts (three clusters). A hard cap applied in inline
     mode would refuse inserts on these healthy indexes.
   - 10 near + 1 far preloaded in deferred mode, then inline: the first inline
     insert runs the split's neighbour-persistence phase, then all 9 later inline
     inserts fail with `NoSuchElementException` at the orElseThrow.
   - collapseConcurrency 0: all 30 inline inserts succeed (no collapse task
     arises); bounceConcurrency 0: 21 succeed, then 9 fail at `BounceTask.runTask`;
     kMeansMaxIterations 0: 12 succeed, then 18 fail at `SplitMergeTask.kMeans`;
     splitMergeConcurrency 0: all 30 succeed while the single cluster grows to 30
     with its split never completing; reassignConcurrency 0: all 30 succeed with a
     cluster at 18 primaries and a split pending.
9. **Split fallback candidates (pure Java: real KMeans.fit and PartitionEvaluator
   with GuardiANN's split parameters; golden).**
   - KMeans with the production overflowQuadraticPenalty at lambda 0.5, 1, 2, 4,
     8, 16 leaves 10 near + 1 far at 10/1 INVALID (and 20+1 at 20/1): the
     outlier's squared distance (about 2e4) swamps the penalty.
   - Outlier-excluded refit (drop children below minChildFraction, refit k=2 on
     the rest, re-home everything to the nearer refit centroid, evaluate): 10+1
     gives 5/6 and 20+1 gives 11/10, both KEEP_CURRENT (usable in Java's
     selection); an identical-coordinate mass of 10 + 1 far stays INVALID. The
     10+2 control is already usable without a refit (KEEP_CURRENT, 2/10).
   - The current partition in this probe is `vectors.get(0)` with every vector
     assigned to it. For the lone bootstrapped cluster that IS production's
     current partition up to primary enumeration order: the first cluster's
     centroid is the first inserted vector (Insert.java:429-432 inserts
     `newVector` as the centroid), and the probe's population lists that vector
     first. The INVALID bit depends only on candidate child sizes
     (PartitionEvaluator.java:124) and transfers exactly; KEEP_CURRENT versus
     ACCEPT transfers only up to summation order, so the Go port's labels are
     pinned by Go goldens over Java-ordered inputs, not by these rows.

Added for design v7 (`evidence-run.txt`: 13/1536 selected specs, uncached, all
pass, run twice with identical probe lines; `evidence-mutations.txt`: five new
mutation kills, one per new or extended spec):

10. **Inline golden extended (14 scenarios).** splitNumNearestClusters 0 inline:
    all 30 inserts succeed, one cluster of 30 with its split never completing.
    mergeNumNearestClusters 0 inline: 30 inserts and 27 inline deletes all
    succeed; four EMPTY clusters remain with SPLIT_MERGE or REASSIGN pending and
    3 results found. Inline DELETES: all 18 succeed on a healthy index; they FAIL
    whenever the head task throws, at the same site as the inserts (unsplittable
    `:397` 5/5, bounceConcurrency 0 `BounceTask.runTask:130` 10/10,
    kMeansMaxIterations 0 `SplitMergeTask.kMeans:1108` 10/10).
11. **Iterated outlier peel (pure Java, golden, 8 seeds per shape).** The peel
    the Go port adds (section 6 of the design), computed with the real KMeans and
    PartitionEvaluator: remove the latest INVALID partition's undersized child
    from the mass, refit k=2, re-home everything, evaluate, up to 8 refits.
    Selected at 1 refit: 10+1, 20+1; at 2: two outliers on opposite sides (18
    tight, +10, -100), nested outliers (30 tight, 2 at +5, 1 at -100), scattered
    outliers (10 tight, +/-50); at 3-5: a natural 60/40 structure under
    minChildFraction 0.45; a heavy-tailed line 2^0..2^15 is usable at the first
    fit on 6 seeds and after 1 refit on 2. Largest use: 5 of 8 refits. The
    identical-coordinate mass stops because the undersized child holds no mass
    member.
12. **Target on the same shapes, both modes (golden).** Every non-identical
    shape: one neighbour-persistence execution, then `NoSuchElementException` at
    `SplitMergeTask:397` on every later drain AND on every one of 5 inline
    inserts; the cluster keeps SPLIT_MERGE. The identical mass (10 identical + 1
    far) takes Java's collapse route and its inline inserts succeed.
13. **Obsolete tasks under the knob they would consume (golden).** Staged by
    rewriting the target clusters' state bits with the production metadata
    writer (`GuardiannConformanceAccess.setStates`), as a concurrent state change
    would. collapseConcurrency 0: the obsolete collapse and its bounce are
    consumed without failing; the bounce re-arms a split that collapses again,
    and only that LIVE collapse fails at `Primitives.fetchCoreClusters:1512`.
    reassignConcurrency 0: three livelocking reassigns whose clusters now carry
    SPLIT_MERGE are consumed (1,1,1,0) and the queue empties.
    mergeNumNearestClusters 0: the livelocking merge whose cluster lost
    SPLIT_MERGE is consumed (1,0).
14. **Inline maintenance through the record layer (golden).** A record store
    with `IndexDeferredMaintenanceControl.setAutoMergeDuringCommit(true)` reaches
    the engine with `maintainInTransaction=true` (VectorIndexMaintainer.java:374):
    20 saves at primaryClusterMax 10 / hard max 11 all succeed and end in the
    same three clusters as the engine-level scenario; the same saves without
    auto-merge fail from save 12 with `VectorIndexClusterTooLargeException`; 10
    near + 1 far + 5 more with auto-merge: 12 saves succeed, then 4 fail at
    `:397`.

Added for design v8 (`evidence-run.txt` regenerated; `evidence-mutations.txt`:
the v8 mutation run):

15. **Obsolete split under a trained bits-9 index (golden).** RaBitQ with extra
    bits 9 and statsThreshold 11: the 11th insert arms the split (queued before
    training) and trains the index. Live, the split fails at
    `Primitives.quantizer:359` on every execution; once its cluster's SPLIT_MERGE
    is cleared (staged), the same task is consumed as a no-op and the queue empties.
16. **reassignNumNeighboringClusters -1 (knob golden row).** The reassign's
    phase-1 fetch limit is 1 + (-1) = 0, `MoreAsyncUtil.limitRemaining` yields
    nothing, and the reassign re-enqueues itself forever: `grow` and `shrink`
    drains never end, exactly the livelock shape of the other neighbour counts.
17. **Peel at the default scale (pure algorithm, 8 seeds, under v8's empirical cap ceil(log2 n) + 2, withdrawn in v9: see item 18).**
    n = 1000: 998 tight plus two opposite outliers, 997 tight plus a nested pair
    and a far point, and 998 tight plus two scattered outliers select within 2
    refits at every seed; a 600/400 structure under minChildFraction 0.45 within 3
    to 4. The largest use across all shapes stays 5 (at n = 100). The probe seeds
    refit r with seed + 1 + r, not Go's split-RNG derivation, so these counts are
    samples; Go pins its own outcomes.

Added for design v9 (spec "measures the peel on many isolated high-dimensional
outliers", 78 pinned rows; Java step `guardiannPeelModeProbe`; runs
wsd-hd-peel-5 and -6 identical; mutation: the floor weakened to 2^r - 1 reddens it):

18. **Peel removal rules on isolated outliers (pure algorithm, 8 seeds; n = 2000
    shapes at 2 seeds).** Three removal rules over every item-11/17 shape and 14
    high-dimensional ones (d = 128 and 768, Gaussian cores at sigma 1, 0.1 and 0.01,
    m = 13/20/50 outliers at 1 to 10 sqrt(d), and 50 at strictly increasing radii):
    - `child` (v8): a tight core loses one outlier per round: m = 13 needs 6-11
      refits, m = 20 12-18, m = 50 18-33, rising radii 36-50; at n = 2000, d = 768
      23-30 refits, 1.2-1.5 s of KMeans.
    - `tail` (undersized child plus every member at least as far as its nearest
      member): no better on equal-radius outliers (KMeans' best-SSE fit isolates
      the farthest point); rejected.
    - `floor` (v9, the geometric removal floor): 4, 5, 6, 6 refits on the tight
      shapes at every seed, 6 at n = 2000, d = 768 (0.34-0.37 s over item 22's four
      runs; 0.345-0.398 s in the v9 runs -5 and -6); identical rounds
      to `child` on every item-11/17 shape and on the spread cores at 1.5 and 3
      sqrt(d); at 10 sqrt(d) one seed of eight takes 4 refits instead of 5; the
      identical-coordinate mass still exits `undersizedChildHoldsNoMass`.
    Wall-clock rows (GUARDIANN-PEEL-TIMING) are printed, never pinned.

19. **Full `just test` of the frozen v9 tree, and the conformance population
    reconciliation.** CORRECTED by item 22: `just-test-v9.log` executed 41 of 94
    targets (not the 90 claimed below), `just-test-v9b.log` executed 4 and served
    `//conformance:conformance_test` from cache, neither log contains the Ginkgo
    summaries quoted below (they come from `conformance-full-2.log`, a pre-v9 run),
    and no tree SHA was recorded. The original text follows unchanged. `just test` (`/var/tmp/fdb-upgrade-recovery/just-test-v9b.log`):
    94 of 94 targets pass. A content manifest of every tracked and untracked file
    was taken before and after the run; the only file that changed was
    `ws-j-design-review-v1/storage.log`, a gate transcript still being written,
    which no test target stages (`rfcs/BUILD.bazel` exports two named RFCs only),
    so every test input held. Four targets executed in that run; the other 90 were
    served from a run of byte-identical inputs earlier the same session
    (`just-test-v9.log`, red only on the field-path-trie guard, which this tree
    splits — see `aggregate_index_residual_test.go`). Ginkgo summaries:
    - `//conformance:conformance_test`: Ran 1405 of 1524 Specs, 1405 Passed,
      0 Failed, 0 Pending, 119 Skipped.
    - `//conformance:rfc257_oracle_test`: Ran 18 of 18 Specs, 18 Passed.
    Reconciliation against the pre-split target: 1538 total / 1419 run before, 1524 /
    1405 after, the difference exactly the 14 specs moved to `rfc257_oracle_test`
    (13 GuardiANN + 1 WS-E). The 119 Skipped are inherited from `origin/master`
    (81 yamsql cross-engine error-code cases, 29 yamsql DML cases, 3 DML facts, 4
    env-gated diagnostic proofs, 2 perf comparisons); TODO.md "RFC-257 —
    `//conformance:conformance_test` skips 119 specs on master" owns them. The final
    acceptance run is uncached (`--nocache_test_results`), not this one.

20. **Sampled refits (pure algorithm, spec "measures the peel on many isolated
    high-dimensional outliers", 130 pinned rows = 26 shapes x 5 modes; Java mode
    `floor-sample:S`).** The floor with every refit's KMeans on a stride sample of at
    most S mass members (every ceil(|M|/S)-th member of M in index order, from the
    first), the partition still assigned and scored over all of P. At S = 256 and at
    S = 64 every shape ends in the same exit as the unsampled floor on every seed,
    with a different round count on 9 of 26 shapes (counted by a script over the
    pinned lines), refits 1-7 (unsampled 1-6). Timing over the four runs of item 22:
    tight 50-outlier core at n = 2000, d = 768, refits 89-107 ms at S = 256, 65-72 ms
    at S = 64, 338-370 ms unsampled, 1.15-1.60 s for `child`; the target's own fit
    36-46 ms (tight) and 105-132 ms (spread). Mutation: sampling turned off reddens the
    spec (`evidence-mutations.txt`). CORRECTED by the v10 reviews: the spread shape's
    target fit reached 100.4 ms in `wsd-sample-2.log`, so the range is 100-132 ms, and
    "eight seeds per shape" holds for the n <= 1000 shapes only (the three n = 2000
    timing shapes run seeds 42 and 1). WITHDRAWN by design v11 on item 23's evidence:
    the rows stay pinned as the measurement of the rejected rule.
21. **Owed, not measured: the bounce follow-up under trained extra bits 9.** A head
    bounce with no outstanding dependency and an idle target builds a follow-up
    task's value tuple, which constructs the quantizer (SplitMergeTask.valueTuple:135,
    ReassignTask:158); that the target throws there is SOURCE only. The design lists
    it among the retained JVM fixtures owed before implementation.
22. **Evidence bound to the design-v10 tree.** `evidence-run.txt` is regenerated from
    two uncached full runs of `//conformance:rfc257_oracle_test`
    (`rfc257-v10-1.log`, `rfc257-v10-2.log`): 21 of 21 specs passed in each, their
    331 GUARDIANN lines (timing excluded) identical, and every source hash recorded
    there verified unchanged after both runs. The sampled-refit timing runs
    `wsd-sample-1.log` and `wsd-sample-2.log` are the other two of the "four runs".
    The frozen-tree `just test` for the gate is recorded in the gate directory's
    SCOPE.md, with the tree SHA.
23. **The unsampled floor at high dimension (pure algorithm, spec "measures the
    unsampled floor peel at high dimension", 16 pinned rows = 8 generated shapes x 2
    modes, seeds 42 and 1; Java step `guardiannPeelGeneratedProbe`, which generates
    the shape inside the JVM so d in the thousands does not cross the invoker as
    JSON).** Shapes: n = 2000 at d = 768, 1536, 3072 and 4096, a tight core (sigma
    0.01) with 50 outliers at sqrt(d) and a spread core (sigma 1) with 50 outliers at
    3 sqrt(d). Each pinned row carries the exit, the refit count, the selected
    partition's child sizes and its smaller-child fraction. Unsampled (`floor`): every
    tight shape selects in 6 refits with the smaller child at 0.33 to 0.49 of n; every
    spread shape is usable at round 0 except d = 4096 seed 1, where the target's own
    candidate is [13 1987] INVALID (the target throws) and one refit selects [1726
    274]. Sampled (`floor-sample:256`): the tight core at d = 4096 selects [243 1757]
    and [1683 317], within 43 and 117 vectors of the INVALID line of 200. Timings
    (printed, never pinned; `wsd-gen-1.log` is the capture run, made against an
    empty `want` so it failed by design and printed the lines that became the pins,
    and `wsd-gen-2.log` is the passing run against those pins; the lines of the two
    are identical): unsampled
    refits total 0.35-0.40 s (d = 768), 0.80-0.87 s (1536), 1.46-1.61 s (3072),
    1.86-2.04 s (4096); the target's own fit 38-220 ms on the tight shapes and
    97-656 ms on the spread ones. The pinned lines of the two runs are identical.
    Mutation: the `floor` mode fitting on a 256-member sample reddens the spec
    (`evidence-mutations.txt`). Design v11 withdrew sampling and admitted the peel by
    floor(log2(n - 1)) * n * d <= 4 * 10^7 on this evidence; design v12 lowers the
    bound and adds the KMeans knob factor on item 26's measurement.
24. **Heartbeat keys that sort after a live heartbeat, and (UUID, x) keys.** Go spec
    "reports an ongoing build by admission's predicate over each indexer's surviving heartbeat"
    and JVM spec "matches Java's heartbeat administration, and pins Java's ongoing
    answer on every declared population": a versionstamp-keyed heartbeat (tuple code
    0x33, after every UUID key 0x30) beside a live one is a ClassCastException in
    Java (measured: "Versionstamp cannot be cast to class java.util.UUID") and an
    IndexingHeartbeatKeyError in Go; a (UUID, "extra") key is read by Java's
    getUUID(0) as that UUID's heartbeat (measured: the read returns it, ongoing true)
    and now by Go too (`heartbeatIndexerID` accepts trailing elements). Runs
    `hb-v11-go.log`, `hb-v11-jvm.log`. Mutations (`evidence-mutations.txt`): the old
    exactly-one-element parse reddens both specs; a single-pass check that parses
    each key only until the first live heartbeat reddens both on the versionstamp
    key.
25. **Full `just test` of the design-v11 tree, with its summaries copied here (they
    are not left in `bazel-testlogs`, which later runs overwrite).** Tree
    `1cf1b5e49bed777e8a3ab9cae163ed82a6403a3b` (the working tree, written with a
    temporary index; the gate tree adds only this README item). Two runs, both with an
    md5 manifest of every tracked and untracked file taken before and after:
    - `just-test-v11.log`: executed 8 of 94 targets (chaos, recordlayer, sqldriver,
      docscheck, conformance_test, rabitq_architecture, rfc257_oracle_test, cmd), red
      only on `//pkg/recordlayer:recordlayer_test`: the spec "refuses legacy and
      malformed heartbeat keys even in mutual mode" still pinned the pre-v11
      rejection of a (UUID, 1) key, which Java's getUUID(0) reads as that UUID's
      heartbeat. The spec now pins that reading (`indexing_heartbeat_test.go`); the
      80 heartbeat specs pass (`hb-v11-go-all.log`). The manifest changed only in a
      WS-F gate transcript that was still being written (not a test input).
    - `just-test-v11b.log`, after that test edit: executed 2 of 94 (recordlayer,
      docscheck) and served the other 92 from Bazel's content-keyed cache. Run 1
      itself executed only 8; the other 86 results it reported, and so the 86 cached
      ones here, come from an earlier invocation this README does not record, valid
      because the cache is keyed by every input's content. 94 of 94 pass,
      manifest unchanged. Ginkgo summaries, read from `bazel-testlogs` straight after
      the run:
      - `//conformance:conformance_test` (executed in run 1): Ran 1405 of 1524 Specs,
        1405 Passed, 0 Failed, 119 Skipped (inherited from master, TODO.md
        "RFC-257 — `//conformance:conformance_test` skips 119 specs on master").
      - `//conformance:rfc257_oracle_test` (executed in run 1): Ran 25 of 25 Specs,
        25 Passed.
      - `//pkg/recordlayer:recordlayer_test` (run 2): Ran 3615 of 3616 Specs, 3615
        Passed, 1 Skipped (`RUN_MILLION_RECORD_TEST`, env-gated).
    The two uncached oracle runs of `evidence-run.txt` (`rfc257-v11-1.log`,
    `rfc257-v11-2.log`, 25 of 25 each) ran on the same oracle bytes.
    Only the run-2 manifest pair survives (`wsd-v11-manifest-before.txt`,
    `wsd-v11-manifest-after.txt`, matching the run-2 tree); run 1's was not kept.
26. **The unsampled floor in the admitted high-dimension corner (pure algorithm, spec
    "measures the unsampled floor peel in the admitted high-dimension corner", 6
    pinned rows, seeds 42 and 1, same generator and step as item 23).** Shapes: the
    largest dimensions design v12's admission bound admits at the default KMeans
    knobs, n = 1001 at d = 2048 and 2775 and n = 2000 at d = 1250, tight and spread.
    Every row selects (or is usable at round 0) with the smaller child at 0.376 to
    0.495 of n; refits total 0.36 to 0.85 s per peel in the capture run, the largest
    single refit there 200.9 ms at n = 2000, d = 1250 (8.0 * 10^-8 s per n * d). That
    was NOT the worst: the two v12 evidence runs measured 254.8 ms and 232.0 ms for
    the same refit (1.02 * 10^-7 and 9.3 * 10^-8) and refit totals up to 0.91 s; item 29
    took the maximum over every run [superseded: v14 stopped deriving B from any rate
    (item 31), and the rates are now the mechanical extraction of item 32]. Runs:
    `wsd-corner-1.log` (capture) and the two
    v12 evidence runs, whose pinned lines are identical.
27. **The ongoing-build check reads the collapsed heartbeat population.** When a (U)
    key and a (U, "extra") key both exist, Java's getIndexingHeartbeats keeps the
    later key's value (HashMap.put) and its ongoing check reads only that value
    (measured in the JVM spec "matches Java's heartbeat administration, ...": a live
    (U) beside a (U, x) a day ahead is not ongoing, the reverse pair is ongoing, and
    the read names the (U, x) value). Go's check now reads the same collapsed
    population (`collectIndexingHeartbeats`) and agrees on both pairs; the Go spec
    pins the same two pairs. Mutation: keeping the FIRST key's value reddens the Go
    spec ("the (U, x) value replaces the (U) value") and the Go line of the JVM spec
    (`hb-v12-mut.log`, `hb-v12-jvm-mut.log`). The JVM spec also pins the message of the
    versionstamp key's ClassCastException ("Versionstamp cannot be cast"), so it
    proves which key threw. Runs `hb-v12.log`, `hb-v12-jvm.log`.
28. **Evidence bound to the design-v12 tree.** `evidence-run.txt` is regenerated from
    two uncached full runs of `//conformance:rfc257_oracle_test` (`rfc257-v12-1.log`,
    `rfc257-v12-2.log`): 27 of 27 specs pass in both (16 GuardiANN, 5 WS-E, 1 WS-F,
    5 WS-J), the 353 GUARDIANN lines of the two runs (timing lines excluded) are
    identical, item 26's six corner pins included, and the sha256 of the spec files
    recorded before the runs verified unchanged after them. The heartbeat collapse's
    runs are item 27's. Full `just test` of tree
    `65d3ffba83b3eda56e3d97430fbd49bb3ff6b9ca` (the working tree written with a
    temporary index; the gate tree adds only this paragraph), `just-test-v12.log`, with
    an md5 manifest of every tracked and untracked file taken before and after
    (`wsd-v12-manifest-before.txt`, `-after.txt`, identical): executed 14 of 94
    targets, the other 80 from Bazel's content-keyed cache, 94 of 94 pass. Ginkgo
    summaries read from `bazel-testlogs` straight after the run:
    `//conformance:conformance_test` Ran 1418 of 1537 Specs, 1418 Passed, 119 Skipped
    (the inherited skips of TODO.md "RFC-257 — `//conformance:conformance_test` skips
    119 specs on master"); `//conformance:rfc257_oracle_test` Ran 27 of 27, 27 Passed;
    `//pkg/recordlayer:recordlayer_test` Ran 3615 of 3616, 3615 Passed, 1 Skipped
    (`RUN_MILLION_RECORD_TEST`, env-gated).
29. **Design v13: the admitted corner re-measured, and one more heartbeat pair.**
    (a) The corner spec gains the two shapes the v13 bound admits at its largest
    dimensions, B = 1.96 * 10^7 (2.0 s over the worst OBSERVED per-unit refit rate,
    1.02 * 10^-7 s, the maximum over `wsd-corner-1.log`, `rfc257-v12-1.log` and
    `rfc257-v12-2.log`): n = 1001 at d = 2175 and n = 2000 at d = 980, tight and
    spread, seeds 42 and 1. Four new pinned rows (10 in all): in the admitted corner
    (d = 2048 and 2175 at n = 1001, d = 980 at n = 2000) every row selects or is usable
    at round 0 with the smaller child at 0.286 to 0.480 of n; the v12 rows at d = 2775
    and n = 2000, d = 1250 stay as the target's behaviour just past the bound (0.422 to
    0.495). (This item's timing claims, "worst refit in the admitted corner 170.6 ms",
    "refit totals up to 0.91 s", and B's derivation from the worst observed rate, are
    superseded by item 31: the v13 evidence runs measured worse, and B is no longer
    derived from time.) Runs `wsd-corner-v13-1.log` (capture) and
    `wsd-corner-v13-2.log` (pinned, 1 of 1 passed). (b) A live (U) beside an
    UNPARSEABLE (U, x): both engines keep the invalid placeholder as the surviving value;
    Java reports the build ongoing (time 0 < now + lease), Go reports no session (the
    declared invalid-only population), and admission refuses a new exclusive session
    over the live (U) in both, which is where "ongoing" and "would be refused" part;
    the reverse pair is ongoing in both. Pinned in the JVM spec "matches Java's
    heartbeat administration, ..." (`hb-v13-jvm.log`, 1 of 1) and the Go spec, renamed
    "reports an ongoing build by admission's predicate over each indexer's surviving
    heartbeat" (`hb-v13-go.log`, 1 of 1); the three texts that claimed the two could not
    differ (indexing_heartbeat.go, indexing_heartbeat_admin.go, DIVERGENCES.md) are
    corrected.
30. **Evidence bound to the design-v13 tree.** `evidence-run.txt` is regenerated from two
    uncached full runs of `//conformance:rfc257_oracle_test` (`rfc257-v13-1.log`,
    `rfc257-v13-2.log`): 29 of 29 specs pass in both (16 GuardiANN, 5 WS-E, 1 WS-F,
    7 WS-J), the 357 GUARDIANN lines of the two runs (timing lines excluded) are
    identical, item 29's ten corner pins included, and the sha256 of the spec files
    recorded before the runs (`v13-oracle.sha`) verified unchanged after them. SimFDB:
    `//pkg/simfdb/...` 10 of 10 with `retry_limit_test.go`. Full `just test` on the
    working tree the gate snapshots (`wsd13-justtest.log`): executed 40 of 94 targets,
    the rest from Bazel's content-keyed cache, 94 of 94 pass.
31. **Design v14: B is a work bound, and the timings are extracted mechanically.** v12
    and v13 set B from the worst refit rate observed over a hand-picked set of runs, and
    each later run moved the maximum (v13's own `rfc257-v13-1.log` measured 437.6 ms at
    n = 2000, d = 1250, 1.75 * 10^-7 s per n * d, and 265.0 ms at n = 2000, d = 980 inside
    the admitted corner). v14 keeps B = 1.96 * 10^7 as a WORK bound chosen for coverage
    (design section 5, the peel's admission) and reports the time as an estimate.
    `corner-refit-rates.py` reads EVERY `GUARDIANN-PEEL-CORNER-TIMING` line of every log
    given and pairs it with its shape line; `corner-refit-rates.txt` is its output over
    the seven logs bound to v12 and v13 (116 shape-seed timings, 98 nonzero refit
    maxima): 4.13 * 10^-8 to 1.75 * 10^-7 s per n * d, median 7.22 * 10^-8; refit totals
    at most 1115.6 ms; the target's own fit 48.6 to 307.6 ms; per log, the maximum of
    `rfc257-v13-1.log` (1.75 * 10^-7) is the only one above 1.02 * 10^-7, its median
    (9.54 * 10^-8) about 1.4x the others'. No load average was recorded for those runs;
    every evidence run from v14 on records `uptime` before and after it, and the script
    is re-run over every new log, so a later run cannot move a number silently
    [superseded by item 32: v14's run of the script left out v14's own evidence run,
    so "every new log" was not true of it].
32. **Design v15: the extraction over every log that carries a timing line.** The
    population is no longer a list someone keeps: it is every log under
    /var/tmp/fdb-upgrade-recovery/ that `grep -rl --include='*.log'
    GUARDIANN-PEEL-CORNER-TIMING` finds, 13 on 2026-09-24. They are item 31's seven,
    the two v13 evidence runs (`ev13/oracle-1.log`, `-2.log`), v14's (`wsdf14/
    oracle.log`, which v14 omitted), the WS-C pre-freeze run (`wsc10-pre/oracle.log`)
    and the two WS-C revision-10 evidence runs (`wsc-v10/ev/oracle.log`,
    `wsc-v10/ev2/oracle.log`). `corner-refit-rates.txt` records each input's sha256, so
    the population is bound, and the script now keys each log by its path, where it used
    the file name and merged the four `oracle.log`s.
    - 236 shape-seed timings, 200 nonzero refit maxima: 4.13 * 10^-8 to 1.75 * 10^-7 s
      per n * d, median 7.44 * 10^-8.
    - Refit totals at most 1115.6 ms; the target's own fit 48.6 to 307.6 ms.
    - The extremes are unchanged from item 31, and the median moves from 7.22 to
      7.44 * 10^-8.
    - Every log added since item 31 has its maximum between 9.01 and 9.85 * 10^-8. Three
      of them record their load (`*.uptime`, 0.68 to 8.30 over the one-, five- and
      fifteen-minute averages).

