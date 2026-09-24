# RFC-257 WS-D — vector engines and GuardiANN implementation design

Status: design v16, awaiting actual milestone gates. Implementation has not started,
with three exceptions (listed below). v16 answers the v15 Torvalds NAK
(`ws-d-design-review-v15/torvalds.txt`; the Graefe lens ACKed v15), each change marked v16
at its site in section 5:
- The chain recorder's rule is per execution, for the recorder and the wrapper alike, and
  it is pinned through a test transactor that strips the chain, with a stale-error case,
  so the pins go red with the recorder removed whatever the client prerequisite does.
- The uncounted retry's spacing is stated as ExponentialDelay's uniform draw, not a
  bound. Under a simulated environment a stall fails loudly after 100 uncounted retries
  against one unchanged sealed posting (`SPFreshStalledSealError`), with a fixture that
  stalls a seal with no takeover. RFC-094 section 2 is corrected to match.
- The predicate pin is over error trees, and the terminal errors are pinned not to
  unwrap to a retryable code.
- `AttemptTransactor` has a read method, and `AttemptCall` separates executions from
  counted attempts.
- The reconcile's classes follow the C++ predicate sets (RETRYABLE_NOT_COMMITTED against
  MAYBE_COMMITTED and the rest).
- The conflicting-keys census runs under libfdb_c.
- A failed commit deactivates the context.
- The loop lives in `pkg/internal/attempt`, shared with the resolver.
- The peel's criterion is measured under the suite's concurrency, with a stopping
  condition, and the inline multi-peel shape is priced and declared.
- Stale citations are fixed, and the SimFDB pin record is bound by hash.

v15's three exceptions:
- the heartbeat key parse and the heartbeat collapse (oracle items 24, 27 and 29,
  landed with their pins and recorded in DIVERGENCES.md,
  "checkAnyOngoingOnlineIndexBuilds");
- SimFDB's retry_limit (section 5), landed ahead of D-0 because it changes nothing until
  D-0 sets a limit, with v14's correction to return the body's error chain at the limit,
  pinned;
- v15's corrected comments at spfresh_write.go:410-420 and in the corner spec, which
  change no behaviour.

v15 answers the four v14 NAKs (`ws-d-design-review-v14/`). They agreed on one P1: the
default backend's `fdb` wrapper strips the body's error chain, so the split-window signal
v14 exempted from the attempt count would have reached the loop as a plain 1020 in
production. The body's chain is now kept twice over, each by its own spec (section 5, "The
body's error chain"):
- the attempt closure records its error, which is how the target's runner sees its body's
  exception;
- the pure-Go wrapper adopts the Apple binding's rule, a second client prerequisite.

The P2s and P3s are answered in place, each marked v15:
- The loop's predicate is the runner's any-cause rule, and the first-cause rule stays
  with AutoContinuingCursor.
- A self-committing body is refused before anything commits, with Java's
  `RecordContextNotActiveException` class.
- The exhaustion reconcile checks the store against the model before and after the
  operation, with SimFDB's ground truth, and has a fixture that turns it red.
- Pages write and run through `Run`, with a page-1 conflict fixture.
- The throttled iterator's Go-only terminal classes are stated.
- An uncounted split-window retry takes the delay. The simulated delay draws but does
  not move the clock.
- The route has a named interface.
- The existing chaos tests' replay cases move to the arm that still replays.
- The resolver and the CLI dump, which call a backend directly, have policies.
- The catalog bootstrap's step 3 becomes check-then-create.
- The setup-DDL retry is tied to the target's conflict footprint.
- Go's peel has a performance criterion at W = B and at every fixture, and the owned
  1007 is priced.
- The timing extraction covers every log that carries a timing line, bound by hash.
- Stale references are restated at the v15 tree `512ae248`.

v14 answered the four v13 NAKs
(`ws-d-design-review-v13/`), which agreed on one P1: v13 set B from the worst refit
rate observed over three runs and v13's own evidence run measured worse. B is now a
WORK bound chosen for coverage and the peel's time an estimate from a mechanical
extraction over every timing line of every bound run (section 5, the peel's
admission; oracle item 31), with the 1007 consequence owned. D-0 is corrected where
v13 was untrue of the tree: the route owns every commit (the DDL and catalog-bootstrap
bodies committed themselves, so the hooks ran twice and a no-commit chaos arm could not
hold; their in-body commits go and a self-committing body fails structurally); the
owners that retry outside the route (the queue drain's throttled iterator,
AutoContinuingCursor, the heartbeat cleanup) are a named census class with the target's
bound and predicate each; the client's 1200 leaves the record layer, because libfdb_c
waits for commit proxies inside its commit and the pure-Go client is changed to do the
same (a client-gated prerequisite); SPFresh's split-window signal consumes no attempt,
which keeps RFC-094's foreground contract (v13's "far inside ten attempts" was false for
the chunked drain and for splits aborted by deletes); the chaos arms fire at most once
per attempt-loop call, with an explicit every-attempt arm for exhaustion; the route is a
call argument, not an inheritable context value; the delay waits on the DST clock; the
hunts and the chaos model reconcile an exhausted call as an unknown outcome; and the
census is counted on the reviewed tree. v13 answered the four v12 NAKs: the retry cap
held on SimFDB, the chaos faults were keyed to the route, the SQL owners were
reclassified by route, SPFresh's foreground writes took their caller's bound (withdrawn
by v14), the peel's admission floored the knob factor and set B from an observed rate
(withdrawn by v14). v12 answered the four
v11 NAKs: every attempt through the transactor, one attempt loop with the target's
retry predicate and ExponentialDelay, a policy per owner, the admission bound carrying
the KMeans knobs and evaluated after Step 1's preconditions, and Go's ongoing-build
check reading the heartbeat population Java's does, collapsing a (U) and a (U, x) key
to the later value (measured in both engines). v11 answered the four v10 NAKs: it ported the target's
attempt bounds as phase D-0, withdrew v10's stride-sampled refits and restored
unsampled refits, bounded the peel by an admission rule read from n and d, took the
peel RNG split once at the peel rule's entry (the Go rules that never refit, at the
same throw sites, draw no peel split: the n<k selection beside a viable candidate,
the empty merge core and the zero-primary drop), stated each fixture's encoding and
write volume, and made the heartbeat key parse follow Java's getUUID(0) (oracle item
24).

v10 (its stride sampling withdrawn by v11) withdrew v9's transaction-time peel budget and bounds the peel's cost
structurally instead: every refit fits KMeans on a deterministic stride sample of at
most 256 mass members while the partition is still assigned and scored over every
vector, so no clock, machine or transaction age decides a stored topology and
ClusterUnsplittableError keeps meaning "no usable partition under the rule"
(measured, oracle item 20); takes the peel's RNG split once at peel entry; writes
consumerOutcome's per-kind statement order as Java's (the quantizer before phase 1,
the neighbour fetch before the no-mergeable-neighbour clear) with four explicit
outcome values both callers act on, hands a head bounce's positioned RNG to runTask,
and adds the serializable-side and non-skipping-delete conflict fixtures with the
non-skipping delete re-reading the queue head as the target does; names the two
poison keys and which site registers each; declares the merge-side and
beside-a-viable-candidate n<k populations and the reconcile's extra replica
metadata reads; names the merger and db.Run as retry owners; restores the heartbeat
KEY parse in CheckAnyOngoingOnlineIndexBuilds (a non-UUID key is an error in both
engines, pinned against the JVM); and rebinds the oracle evidence to this tree
(README items 20 and 22). v9 replaces the peel's empirical round cap with a geometric removal floor
whose bound (floor(log2(n - 1)) refits) is structural, measured on isolated
high-dimensional outliers where v8's peel ran one round per outlier (oracle item
18); names the target's second throw site (KMeans.java:136) and labels the n<k
rule as Go's; gives the reconcile failure its own ClusterUnsplittableError and
corrects its reachability (deferred mode too) and relief threshold; decides task
poisoning once, in the execution wrapper, for any error after the removal is
buffered; makes consumerOutcome's isolation a parameter and states exactly what it
does not predict; and records the population reconciliation of the conformance
target split (ws-d-oracle/README.md item 19, corrected by item 22). v7 moved every task-kind capability check to Java's consumer point,
withdrew the config-only insert refusals except insertMaxCandidateClusters < 1,
iterated the split outlier peel, and corrected the lease-wait sizing and the
claim-race path. v8 withdraws v7's insert-side inline bound (the target's own
flow reaches its key) and bounds the reconcile residue inside the reconcile,
decides the inline-delete skip with the consumer's own read-only prologue, makes
the peel bound logarithmic with measurements at n = 1000, and poisons every typed
error raised inside task execution. This document records the implementation decision, not a request to
choose an option. Java is tag4.14.2.0 at
`fdacd162a9c8acfadc49082b89185c823ab8ae4a`; accepted WS-C source tree is
`f0cb29576cde624e0f1c6e22ca18a5180d1b298f`. C++ remains7.3.77. The umbrella
scope is RFC257 WS-D. SPFresh/LIRE and approved raw/nullable-array/scalar/sort
extensions remain independent and preserved.

## Research and precise scope

Three actual read-only gpt-6-astra/xhigh research sessions produced the source
maps, algorithms, Java regression shapes and port dependencies in:

* `ws-d-research/hnsw-options.txt`: full HNSW production package, typed options,
  engine admission, storage/results, traversal, RNG and record-layer boundary.
* `ws-d-research/guardiann-engine.txt`: full GuardiANN production package,
  KMeans and PartitionEvaluator; persistence, algorithms and task state machine.
* `ws-d-research/maintenance-integration.txt`: full record-layer vector engines,
  maintainer, counts, leases, control/merger and principal integration tests.

Each report declares supporting unread scope. Reports are source research, not
runtime evidence or ACKs. Their exact Java/Go references and regression matrices
are part of this design's implementation specification; the decisions below
resolve cross-report shorthand. Read corresponding Java classes completely at
each implementation boundary, not just report excerpts.

## 1. One engine-neutral record maintainer

Keep prefix selection/locking, PK trimming/reconstruction, pending-write payloads,
shared maintenance gates, materialized result continuations and record-entry
construction in the existing vector maintainer. Introduce a private vector-engine
interface for concrete partition search, insert, delete-with-old-vector, optional
task counts, caller-merge signaling and bounded deferred-task execution. Pass
context/transaction and primary partition subspace explicitly; GuardiANN also
receives the actual secondary subspace. Listeners remain attempt-local. Mutable
graph/node caches are operation-local, bound to the exact read transaction and
isolation level; do not reuse snapshot-fetched nodes for a serializable read or
write. Remove the cross-operation partition storage cache rather than adding
unverifiable provenance flags to cached nodes. Within an operation, cached nodes
are immutable snapshots and cache publication is synchronized if reads pipeline.
Every bounded parallel collector (Java forEach with a concurrency bound) stops
cooperatively and joins before the operation returns and before the partition
write lock is released. Neither backend offers a portable cancel: pure-Go
Future.Cancel is a no-op (pkg/fdbgo/fdb/future.go) and the cgo backend's
fdb_future_cancel or transaction cancel would abort unrelated work. So: a shared
stop flag is checked before starting each child read and before each mutation;
no future and no transaction is ever cancelled; in-flight reads finish and are
discarded; each future is created and resolved on the same goroutine; children
mutate only disjoint keys or commutative atomic ADDs, so persisted bytes do not
depend on scheduling; and the error surfaced is the lowest-index failing child's.
Java's forEach surfaces whichever child fails first in time (MoreAsyncUtil.java:
1309-1329); Go's deterministic choice is a declared divergence that affects only
which of several simultaneous errors is reported, never persisted bytes, because a
failed operation's transaction aborts either way. No Set, Clear, atomic ADD or
conflict range may be issued after the operation returns, so no mutation can miss
the commit and no count can drift. A fixture forces a child error while siblings
are mid-read and asserts the operation returns only after every child exited,
with the lowest-index error.

Locks follow Java's identities and exclusion, with one declared difference in
hold span. Locks are keyed `LockIdentifier(partitionSubspace)` in the RECORD
CONTEXT's lock registry (database.go already mirrors Java's LockRegistry), never
in a per-store or per-maintainer map, so two FDBRecordStore handles on one
context exclude each other. Insert and delete hold the partition write lock for
the single engine call (Java doWithWriteLock). deleteWhere, merge/task drain and
counts/leases take no vector maintainer lock, as in Java; operation-local caches
leave them no shared Go state. The existing SearchKNN helper read-locks the whole
index subspace while writes lock the prefix; both move to the partition
identifier so a grouped read and write actually exclude each other. Queue replay
and direct writes use the same identifiers.

The search read lock is held for the search computation only: acquire, run the
engine search that materializes the complete page, release with a single
deferred call in the same function, then hand the materialized list to the
cursor. Java's AsyncLockCursor keeps the lock until the cursor is exhausted or
closed, but by then the page is already materialized (VectorIndexMaintainer
wraps the finished search result list), so the longer hold never changes what a
scan returns; it only delays a same-context writer. In synchronous Go that delay
becomes a self-deadlock for shapes that work today (a LIMIT that abandons the
inner vector cursor, INSERT...SELECT or UPDATE/DELETE ... IN (vector subquery)
with the source cursor open, open rows followed by Exec). DIVERGENCES.md records
this hold-span difference. Consequences, each by construction rather than new
lock machinery: no lock is held across caller code, so no hold can outlive its
operation and no cancellable lock is needed; release happens exactly once
through one defer, so the double-release that a literal AsyncLockCursor port
would do on sync.RWMutex cannot occur; waits are bounded by other operations'
single engine calls; no operation holds two partition locks, so there is no lock
ordering cycle. The full write-path order is store stateMu -> maintenance gate ->
sliding-window lock -> partition lock; the scan path takes only the partition
read lock. While a partition lock is held nothing may take a store-level lock or
any partition lock again: engine-internal structures (GuardiANN's centroid HNSW,
inline task drains) are reached through direct engine calls that take no
maintainer lock. RWMutex writer preference would turn any violation into a
deadlock, so a test pins the order. The shared sync.RWMutex registry (also used by R-tree) stays;
it provides the same EXCLUSION as Java's AsyncLock. It does not reproduce Java's
strict FIFO order (Java readers wait for every earlier writer; an RWMutex unlock
releases all parked readers, so a reader can pass a second queued writer); no
result depends on that order because every hold is a single engine call. Continuation replay from materialized entries takes no lock and runs no
search, as in Java. The stale hnsw.go header comment saying locks are unneeded
is corrected.

Regressions (deterministic; each must turn red under a mutation that removes the
lock or widens the hold): (1) a test-only pause hook stops an insert after it
rewrites a neighbour list but before it writes the new node; a same-context
search from another goroutine blocks until the insert finishes and returns a
consistent page, and with the write lock removed it observes the half-written
graph; (2) two store handles on one context exclude each other the same way;
(3) SQL shapes INSERT...SELECT from a vector scan, UPDATE ... IN (vector
subquery), and LIMIT over a vector scan followed by a same-prefix write complete
in one transaction (a cursor-lifetime hold deadlocks them); each runs under a
watchdog deadline so the widen-the-hold mutation fails with an attributed
deadlock message instead of hanging the package on an uncancellable RWMutex;
(4) snapshot->
serializable search and snapshot->write conflict on a real concurrent neighbour
rewrite that leaves access-info unchanged. A race-detector run is supplementary,
not the proof. These are correctness gates, not cache-performance claims.

HNSW and GuardiANN are distinct engine implementations. SPFresh is neither an
alias nor a shared clustering implementation. Do not add a second query pipeline.
Guard known engine identity before all affected storage access/mutation: metadata
construction/open/evolution and direct maintainer entry points, including save,
delete, queued replay, rebuild, scan continuation, prefix deletion, and the same
entry points reached through a sliding-window wrapper. Invalid
engine/config errors must not leave buffered record or index mutations that a
caller can commit; they poison the record context exactly like the capability
errors below. Engine equality is immutable and parsed, not string equality.
Missing engine means HNSW; known names are case-insensitive; empty/whitespace and
unknown names fail. Interim unavailable-engine refusal is safety only; WS-D
cannot be accepted until GuardiANN actually executes through the same APIs.

A typed catalog declares canonical name, aliases, type, parser and serializer
once. Equal-valued duplicate aliases are rejected by presence. Shared metadata
writers retain canonical hnsw* names; vector* names are read aliases. Scan return
vectors writes vectorReturnVectors and reads hnswReturnVectors. Require dimensions;
no silent128 fallback. Integers use Java32-bit parsing, metrics enum spellings,
booleans Java's case-insensitive true/otherwise-false behavior. Existing explicit
Go convenience metric/input aliases remain separate adapters, not unknown-value
fallback in the shared metadata parser. Preserve all approved expression inputs.
Compare parsed/defaulted option fields for evolution, not quantizer identity or
raw strings. Use the exact mutable/immutable sets in the Java engines.

Separate Java metadata parsing/constructor validity from operational admission.
Parsing stays exactly Java's. Every admission rule below is derived from MEASURED
target behaviour: the live-JVM oracle (`ws-d-oracle/README.md`; Describe
`GuardiANN target oracle`, 16 specs, evidence and source hashes in
`ws-d-oracle/evidence-run.txt`, regenerated for this tree, README item 28) pins, per degenerate knob value, which consumer
throws, which livelocks (the queue never empties within 40 bounded drain rounds,
every round executing a task) and which degrades without throwing, in DEFERRED
mode (insert, drain, delete, search, a split beside a neighbour, delete-driven
merges, a collapse, RaBitQ training) and in INLINE mode (every insert or delete
first runs one queued task), and which obsolete tasks are consumed as no-ops
without touching their knob. A target upgrade that changes a golden reddens the
oracle and forces this section to be revisited.

Capability errors. On refusal Go returns a typed capability error (engine,
operation, task kind when a task is refused, option, requested value, cause).
Every refusal raised from an entry point that may already have buffered writes
in the record context registers a keyed commit check that returns the same error,
using the existing machinery (database.go getOrCreateCommitCheck). Those entry
points are exactly: engine insert/delete reached from record save/update/delete;
the insert refusal at queue ENQUEUE (a record save into an index building with
the pending-write queue, see insert admission below); queue replay; build
batches; and sliding-window delegate calls. A refusal raised INSIDE a task body
(merge drains, and the inline task an insert or delete runs first) registers
nothing at its raise site: the task-execution wrapper below is the one place that
poisons it. There are therefore exactly two poison keys, each registered by
exactly one kind of site: `vectorCapability/<store subspace hex>/<index name>` by
the entry points above and `vectorTask/<store subspace hex>/<index name>` by the
wrapper. When both register in one transaction (a caller catches an admission
refusal and then runs a task that fails), commit runs the checks in registration
order and returns the first one's error (database.go runCommitChecks), so the
error a commit reports is the earliest refusal of the transaction. A caller that catches such an error
and commits therefore cannot commit: the record bytes, version, record-count ADD
and every earlier index's writes buffered in that transaction are discarded. This
makes "a refused mutation leaves the store byte-for-byte unchanged" true without
a pre-buffer admission pass, which Go's store cannot provide (records are written
before maintainers run, store.go saveWithSplit/deleteSplit before
updateSecondaryIndexes) and which could not decide data-dependent cases without
duplicating each operation's read phase. Read-only refusals (search) and
metadata/open/evolution guards return the error WITHOUT poisoning: they buffered
nothing, a Java caller can commit other writes after a failed query, and a
transaction that disables or drops the index must stay committable. Continuation
replay is never refused by a CAPABILITY rule: like Java
(VectorIndexMaintainer.java:208-223) it touches no engine, access info or
quantizer. The engine-IDENTITY guard above is a different rule and does apply to
it, exactly as in Java: Java resolves the engine in the maintainer constructor
(VectorIndexMaintainer.java:123; VectorIndexEngineKind.java:57 throws
MetaDataException for an unknown name), which every entry point, a continuation
scan included, passes through, and Go raises the same error at the same point.
Both poison keys sit deliberately outside pendingWriteCommitCheckPrefix, so
DeleteStore does not cancel them: a transaction that attempted an unindexable write is never
committable, whatever else it does afterwards. Capability errors are Go-only and
terminal: the queue-replay iterator and build-range retry owners do not retry them
(no Java retry parity is at stake), so a refused queued entry fails the build
once, loudly, instead of 100 retries. Poisoning of a TASK is decided in exactly
one place, Go's task-execution wrapper (executeSingleDeferredTask): once it has
buffered the task's removal (Primitives.executeSingleDeferredTask removes, then
runs, Primitives.java:1016-1033), ANY non-nil error the task body returns poisons
the context before the wrapper returns it: a capability error at a consumer, the
terminal reconcile's ClusterUnsplittableError (section 6), an untyped invariant
error, a read error and a context cancellation alike. Each of them leaves the
removal buffered while the work the task stood for is incomplete (a cancelled task
can return with its removal and, say, an HNSW centroid replacement buffered but no
metadata or references), so a caller that caught it and committed would commit a
removed task whose cluster still carries its state flag, a cluster no task will
ever visit again. The target leaves exactly that state after catch-and-commit of
its own task throws; Go refuses to. Declared under (e). The rule keys on WHERE the
error is returned (after the removal is buffered), never on its type. Errors
returned before any removal is buffered follow their own rules: capability errors
at admission poison (above); the INSERT cap's capacity error (the Go equivalent of
ClusterCapacityExceededException, raised only at Insert.java:335, before any task
runs) neither poisons nor counts as a capability error; see sections 3 and 5. Go
fixture: a task whose context is cancelled after its removal and one mutation are
buffered returns ctx.Err(), and a caller that catches it and commits is refused;
the same fixture with the poison mutated away shows the removal committing.

Task-kind capability, raised at the consumer. Go follows each task's Java control
flow exactly and raises the typed capability error at the statement where Java
consumes the degenerate value, never earlier. Every refusable kind except bounce
has no-op exits that run, and commit, before its knob, KMeans or the quantizer is
touched: SplitMergeTask.runTask's missing-cluster, no-SPLIT_MERGE, COLLAPSE and
false-alarm returns (SplitMergeTask.java:169-187) and the phase-1 neighbour
re-enqueue (:232-236 split, :435-437 merge) come before KMeans (:1108);
CollapseTask's missing-cluster and no-COLLAPSE returns (CollapseTask.java:144-158)
come before the quantizer (:171) and fetchCoreClusters (:182); ReassignTask's
missing, not-REASSIGN, SPLIT_MERGE and COLLAPSE returns (ReassignTask.java:199-210)
come before the quantizer (:241) and :254-257. Only BounceTask consumes its knob
first (BounceTask.java:130). A task is removed from the queue in the transaction
that runs it (Primitives.executeSingleDeferredTask removes, then runs); the typed
error aborts that transaction and the removal rolls back, which is the persisted
outcome Java's own throw produces. There is no pre-removal check, no pre-claim
check and no second copy of any predicate. Measured and reproduced by Go goldens:
the KMeans-incapable configurations execute one phase-1 step and then fail at
SplitMergeTask.kMeans:1108, and once the cluster shrinks the stalled split is
consumed as a false alarm (`shrink=ok drain=1`); an obsolete collapse under
collapseConcurrency 0, obsolete reassigns under reassignConcurrency 0 and an
obsolete merge under mergeNumNearestClusters 0 are all consumed without failing
(oracle item 13).

| Knob (value) | Java consumer = Go raise point | Target behaviour (measured) | Go |
|---|---|---|---|
| kMeansMaxIterations < 1, kMeansMaxRestarts < 0, metric DOT_PRODUCT or EUCLIDEAN_SQUARE | SplitMergeTask.kMeans (:1108), after the no-op exits and phase 1 | throws there for every live split (measured, lone clusters); false alarms and stale tasks consumed (measured); a live merge reaches the same consumer after its mergeable-neighbour check (source) | same point: parity |
| collapseConcurrency < 1 | fetchCoreClusters from CollapseTask (:182), after its no-op exits | a live collapse throws; an obsolete one is consumed | same point: parity |
| bounceConcurrency < 1 | BounceTask.runTask (:130), its first statement | throws | same point: parity |
| splitMergeConcurrency < 1 | task WITH precomputed neighbours: the forEach parallelism check in fetchClusterMetadataForReferences (:239-240, :442-443); WITHOUT: the neighbour fetch pipelines zero reads and phase 1 writes the task back with an empty list (:268-275) | throws (source) / livelock (measured) | throws: parity; livelock: typed error at the point Java would write that task, declared (c) |
| reassignConcurrency < 1 | the same two shapes in ReassignTask (:254-257 / :322-327) | throws (source) / livelock (measured) | parity / declared (c) |
| splitNumNearestClusters < 1 | the phase-1 fetch limit in split() (:230-236) | livelock | typed error where Java writes the empty-list task: declared (c) |
| mergeNumNearestClusters < 1 | the phase-1 fetch limit in merge() (:433-437) | livelock | same: declared (c) |
| reassignNumNeighboringClusters < 0 | the phase-1 fetch limit 1 + value in ReassignTask (:243-251, :322); MoreAsyncUtil.limitRemaining yields nothing for a limit below 1 | livelock (measured at -1: `grow` drains never end; the value 0 has no effect) | same: declared (c) |
| deleteConcurrency < 1 | Delete (:170), after access info AND the tag-5 row are found (Delete.java:110-128,170-176) | throws there | same point: parity (deleting a never-indexed PK or on an empty index succeeds, as in Java) |
| deleteMaxCandidateClusters < 1 | record delete | identity removed, references stay | match the target |
| insertMaxCandidateClusters < 1 | record insert | identity written, no reference, never searchable | refuse the insert: declared (b) |
| reassignNumNeighboringClusters 0, replicatedClusterTarget 0, sampleBatchSize 0 | - | no observable effect | match the target |

The livelock rows are one mechanism. Java stores "not yet fetched" and "fetched,
found none" as the same empty neighbour list (SplitMergeTask.java:261,
ReassignTask.java:313), so a fetch that returns nothing writes the same step back
at high priority forever, which also starves the normal-priority work behind it
(AbstractDeferredTask.java:313-334). Go raises the typed error exactly where Java
would write that empty-list task, after every no-op exit; a task that carries
precomputed neighbours takes Java's own path. The Go condition is keyed on the
knob VALUE, not on an observed empty fetch: the fetch width (the neighbour count,
1 + reassignNumNeighboringClusters for reassign, or the pipeline width) is below
1. That is the same population, because a healthy fetch always contains the
target cluster itself (Primitives.java:1481-1483), so the fetch is empty only under
a degenerate width. splitMergeConcurrency and
reassignConcurrency are mutable, so evolving them repairs the index. The
neighbour counts are IMMUTABLE (GuardiannVectorIndexEngine.java:84-97): with
splitNumNearestClusters < 1 every split, with mergeNumNearestClusters < 1 every
merge, and with reassignNumNeighboringClusters < 0 every reassign, is refused for
the life of the index, so once such a task heads a prefix's
queue, deferred merges of that prefix fail and inline inserts into it fail until
the index is rebuilt or cleared. The target instead livelocks: its merges never
finish, and inline inserts keep succeeding while maintenance starves (measured,
oracle item 10: splitNumNearestClusters 0 grows one cluster to 30 primaries;
mergeNumNearestClusters 0 leaves four empty clusters with work pending). Declared
under (c).

Where refusals land. Merge (MergeIndexes, a build's requested merges, the
capacity hand-off): the typed error aborts that drain transaction, the merger
returns it and the session ends with it, as the target's merge fails with the
task's exception. The claim-only lease transaction has already committed, exactly
as Java's claim does (VectorIndexMergeLock claims, then a later invocation
drains), so a refused drain leaves the session's lease, which the same merge
session id reuses and which otherwise expires by Java's rule. Every target of
MergeIndexes and of a build's requested-merge loop is attempted independently and
the first error reported after all ran, which is parity with Java's whenAll
(provider/foundationdb/IndexingBase.java:1085-1096; Go's current stop-at-first
loops in indexing_merger.go change to it). Inline insert: the insert fails when
the head task it runs first raises (parity for throwing consumers, measured for
bounce, KMeans and the orElseThrow site; declared (c) for the livelock rows, where
the target's inserts succeed, measured for splitNumNearestClusters 0,
splitMergeConcurrency 0 and mergeNumNearestClusters 0 in oracle items 8 and 10).

Inline delete. MEASURED (oracle item 10): the target's inline delete fails
whenever its head task throws, at the same site as its inserts (the unsplittable
cluster 5/5 at :397, bounceConcurrency 0 10/10, kMeansMaxIterations 0 10/10), and
succeeds under the livelock rows (mergeNumNearestClusters 0, 27/27). Go runs no
inline task for a delete whose head task would be REFUSED BY A GO CONSUMER, and
nowhere else. That decision is made by the same code that makes the refusal: each
task kind's prologue is ONE read-only function, `consumerOutcome(task, isolation)`,
which walks the kind's Java statements IN JAVA'S ORDER and stops at the first one
that decides the task. For a SPLIT_MERGE task that order is (SplitMergeTask.java):
the no-op exits (missing cluster, no SPLIT_MERGE, COLLAPSE, :169-175); the false
alarm (:179-187); the split/merge decision (:189-194); then, at the entry of
split() or merge(), the task RNG (:223/:426) and the quantizer (:227/:430), so a
trained bits-9 quantizer refuses HERE, before any neighbour work (SOURCE: :227 comes
before :232 and :430 before :435; oracle item 15 MEASURED the refusal's leaf frame,
Primitives.quantizer:359, which the phase-1 re-enqueue's valueTuple:135 also reaches,
so the frame alone does not order the two); the phase-1 neighbour re-enqueue when no neighbours are precomputed
(:232-236/:435-437), whose fetch width is the splitNumNearestClusters /
mergeNumNearestClusters livelock row and whose pipeline is splitMergeConcurrency;
with precomputed neighbours, fetchClusterMetadataForReferences at
splitMergeConcurrency (:239-240/:442-443), which is where splitMergeConcurrency < 1
throws; for a merge only, classifyClusters (:472-475) and the no-mergeable-neighbour
clear (:483-493), then fetchCoreClusters (:505-507; a split's is at :349); and finally the KMeans consumer
(:1108, KMeans.java:136-137). A CollapseTask walks its exits (:144-158), the
quantizer (:171) and fetchCoreClusters (:182); a ReassignTask its exits (:199-210),
the quantizer (:241), the phase-1 fetch (:243-251) and :254-257; a BounceTask its
knob first (:130). The function returns one of four values, and both callers act on
the value, never on a second copy of any predicate:
- CONSUMED: a no-op exit, the false alarm or the no-mergeable-neighbour clear
  decides the task, and the exit's writes are the task's whole effect;
- RUNS: every statement up to the task's real work passes;
- REFUSED(err): a Go consumer raises err at that statement (a throwing consumer of
  the table above, or a Go livelock refusal);
- REFUSED_UNLESS_ALL_NK(err): the KMeans knob would raise err, but only for a
  candidate with at least k cleaned vectors, which the prologue cannot know without
  reading the posting (Java reaches KMeans.java:137 only past the size check at
  :136, and the Go n<k rule scores a smaller candidate INVALID without calling
  KMeans).
runTask proceeds on RUNS and on REFUSED_UNLESS_ALL_NK (the real consumer then
raises, or, when every candidate is n<k, the task continues to section 6's
reconcile) and returns err on REFUSED; the delete's skip treats both REFUSED values
as "skip" and CONSUMED and RUNS as "run the task". So under the written order a
phase-1 split under trained bits 9 is REFUSED by the quantizer, as in the target; a
merge whose neighbours vanished is REFUSED under trained bits 9 and under
splitMergeConcurrency 0 with precomputed neighbours, and CONSUMED otherwise; and a
phase-1 head split under kMeansMaxIterations 0 RUNS (its re-enqueue comes before
KMeans).
Isolation is a PARAMETER and never a property of the code path:
- runTask calls it with SERIALIZABLE reads, exactly Java's prologue reads
  (`fetchClusterMetadata(transaction, ...)` at SplitMergeTask.java:169 via
  Primitives.java:609-612; the dependency fetches at BounceTask.java:130-131), after
  the serializable presence check (Primitives.java:1019). A false-alarm clear
  (SplitMergeTask.java:183-185) therefore conflicts with a concurrent insert that
  saw SPLIT_MERGE set and did not re-arm (Primitives.java:1161-1163), as in Java.
- the delete's skip calls it with SNAPSHOT reads (the queue head, a bounce's
  dependencies, the target cluster's metadata and, for a merge, the neighbour
  metadata classifyClusters reads to decide the no-mergeable-neighbour clear), so a
  delete that SKIPS adds no read conflict. Declared under (d): the target's inline delete always reads the queue
  head serializably (fetchSomeDeferredTasks, Primitives.java:1053-1060, reached
  from :984) and so conflicts with a concurrent higher-priority enqueue; a skipping
  Go delete does not.
- a delete that does NOT skip runs the task exactly as the target does: it first
  re-reads the queue head with the target's own serializable range read (same
  range, same limit, Primitives.java:1053-1060), then runs the task through the
  wrapper, whose prologue re-reads serializably. So the non-skipping delete's
  conflict footprint is the target's, and a skip's snapshot reads are never reused.
For a BOUNCE at the head it returns the bounce's own refusal (bounceConcurrency < 1)
or else the outcome of the dependency the bounce would execute, together with the
outstanding-dependency list, the pick and the POSITIONED RNG: the outstanding
dependencies are read in stored order (ImmutableSet over the fetched order,
StorageAdapter.java:331-335), the RNG is Java's own `RandomHelpers.random(
getTaskId())` (BounceTask.java:123), and when a dependency is outstanding the pick
is its `nextInt(outstandingTasks.size())` (:161), after which the SAME RNG feeds
`randomUuid` (:190) and the follow-up tasks (`enqueueFollowUpTasks`, :200, and
their `randomNormalPriorityTaskId`, :285); with none outstanding the undrawn RNG
feeds the follow-ups (:149). BounceTask.runTask consumes the returned list, pick and
RNG instead of recomputing them, so every task-id byte it writes is the one Java
writes. So an obsolete or false-alarm task, which the target consumes, is consumed
by the Go delete as well, and a live split under mergeNumNearestClusters < 1 runs.

What `consumerOutcome` does NOT predict, stated exactly:
- A bounce with no outstanding dependency, or after it runs its last one, enqueues a
  follow-up split or reassign (BounceTask.java:147-150, 199-200, 285-309); building
  that task's value tuple constructs the quantizer (SplitMergeTask.valueTuple:135,
  ReassignTask:158), so under a trained bits-9 quantizer with an idle target cluster
  the target throws there (SOURCE, not measured: owed as oracle item 21 below) and
  Go's inline delete fails exactly as the target's does.
- The terminal reconcile's ClusterUnsplittableError (section 6) is not predicted: it
  depends on the cleaned population, and there Go's inline delete fails as the
  target's does (the target throws at :397 or :136).
- REFUSED_UNLESS_ALL_NK is skipped CONSERVATIVELY: for a task whose every candidate
  has fewer than k cleaned vectors (an over-max cluster with fewer than two live
  primaries, or an empty merge core) runTask would not raise, so the task stays
  queued and counted, is never lost, and runs at the next drain; declared under (d).
The population is therefore: the throwing consumers of the table above plus Go's
livelock refusals, where the Go delete succeeds while the target's fails (throwing
consumers) or also succeeds (livelock rows); plus the conservative n<k skips. The
inline deletes that still fail in Go, as in the target, are the bounce follow-up
under bits 9 and a reconcile that fails above the hard cap. Declared (d). The
skipped task stays queued and counted exactly as Java counts it. Go fixtures, one per
VALUE and per arm: CONSUMED for an obsolete head task, a false-alarm head task and a
merge with no mergeable neighbour; RUNS for a phase-1 head split (no precomputed
neighbours) under kMeansMaxIterations 0, a live split under mergeNumNearestClusters
0 and a head bounce whose dependency is healthy (the pick drawn from the task id,
BounceTask.java:123, 161, and the bounce's re-enqueued task id and follow-up task
ids asserted byte-equal to a JVM row); REFUSED for the bounce's own refusal
(bounceConcurrency 0), a head bounce whose dependency is a live collapse under
collapseConcurrency 0, a bits9Live head split in phase 1, a merge with vanished
neighbours under trained bits 9 and one under splitMergeConcurrency 0 with
precomputed neighbours (both JVM rows: the target throws); REFUSED_UNLESS_ALL_NK for
an over-max cluster with one live primary under kMeansMaxIterations 0, skipped
conservatively, after which the next drain reconciles it; a head bounce under bits 9
with no outstanding dependency fails in both engines (JVM row, item 21); a
snapshot-isolation fixture where a concurrent drain commits the head task while the
skipping delete still commits, adding no read conflict; a SERIALIZABLE-side fixture
where runTask's false-alarm clear and a concurrent insert that read SPLIT_MERGE
without re-arming race, the insert commits first and the clear's commit fails with
not_committed (1020) (a mutation that reads the prologue at snapshot lets the clear
commit, which reddens the fixture); and a non-skipping delete racing a concurrent
higher-priority enqueue fails with 1020, as the target's does. Record deletes stay impossible under
deleteConcurrency < 1 and for a delete that enqueues a task under a trained bits-9
index, as in the target; removal always remains possible through deleteWhere, clear,
disable and drop. Deferred-mode inserts and deletes run no task and are
unaffected.

Insert admission is limited to one knob. A GuardiANN insert is refused at the
engine entry, and again at queue ENQUEUE so a save into a queued index is refused
instead of leaving an entry that can never replay, when insertMaxCandidateClusters
< 1. Measured: the target writes the tag-5 identity and no reference, and no
search ever returns the vector (`InsertMaxCandidateClusters=0 ... found=0`); the
knob is immutable, so such an index can never return anything. Go refuses the
write instead of acknowledging an entry no query can find: declared (b). Every
other configuration keeps the target's acceptance. An index whose KMeans, metric
or neighbour-count settings make splits impossible accepts inserts for exactly as
long as the target does: a partition at or below primaryClusterMax never runs
KMeans and never merges alone (measured: DOT_PRODUCT `insert=ok search=ok`), and
the first live split then fails at its consumer as above, so deferred mode
strands at the hard cap and inline mode fails at the head task, both as in the
target. (v6 refused inserts for those settings, and for enabled RaBitQ with
unsupported extra bits, from configuration alone; that refused workloads the
target serves indefinitely and is withdrawn.)

Encoding capability. RaBitQuantizer accepts extra bits 1-8 (RaBitQuantizer.java:
76); Java's HNSW Config accepts 1-15 and GuardiANN's Config checks none. Go
raises the typed capability error exactly where Java constructs the quantizer
(Primitives.quantizer, :352-359), which throws once the access info is trained,
or at once for a metric that is not translation-preserving (the initial access
info uses RaBitQ immediately, Insert.java:404-412). Measured for bits 9: inserts
and searches after training throw at Primitives.quantizer:359; a GuardiANN delete
that enqueues no task succeeds; with DOT_PRODUCT the first insert throws. The
construction points are: every insert of a new PK (Insert.java:235; an existing
PK returns before it, :226-228), every search, the HNSW
delete (hnsw/Delete.java:213), the serialization of a task's centroid whenever a
task is WRITTEN (SplitMergeTask.java:135, CollapseTask.java:112,
ReassignTask.java:158), and each task body after its no-op exits
(SplitMergeTask.java:227/430, CollapseTask.java:171, ReassignTask.java:241). An
obsolete task is therefore consumed even under a trained bits-9 index, as in the
target (measured, oracle item 15: a split queued before training fails at
Primitives.quantizer:359 while live and is consumed once obsolete), and a search
refusal does not poison. Encoded bytes found under an
unsupported extra-bit value are a capability error, not corruption: a newer
writer is the plausible source. Removal always stays possible through
deleteWhere, clear, disable and drop.

The gate evaluates index entries, not records. Java's vector maintainer inherits
StandardIndexMaintainer.update, which removes entries common to the old and new
record before calling the engine (StandardIndexMaintainer.java:217-227); Go's
vectorIndexMaintainer.Update overrides that and deletes and re-inserts
unconditionally, so every HNSW record update today rewrites graph bytes the
target never touches. WS-D fixes it: Update applies the existing
removeCommonEntries (index_maintainer.go) before calling the engine, pinned by a
JVM byte fixture that a non-vector field update leaves the partition bytes
unchanged. NULL vector entries never reach the engine (VectorIndexMaintainer.java:
357-363). Two paths DO pass an unchanged vector to the engine in the target and
keep doing so in Go: queue replay (updateFromQueue applies old then new) and the
sliding window. There the insert half is subject to the insert rules above, so a
queued or in-window update of an insert-refusing index (insertMaxCandidateClusters
< 1, the only config-only insert refusal) is refused; fixtures cover both. A
queued entry holding both an old and a new vector entry is admitted only if both
halves are; otherwise the whole entry fails terminally and is retained,
never partially applied. Insert admission also runs when a write is ENQUEUED, so
an insert-refusing index rejects the save instead of leaving an entry that can
never replay.

Sliding-window-wrapped vector indexes. Java wraps any VECTOR index, GuardiANN
included, in a sliding window (SlidingWindowIndexMaintainerFactory.java:92), and a
record delete can re-elect an overflow entry by INSERTING it into the delegate
(SlidingWindowIndexMaintainer.java:667-681,769-798; Go sliding_window_index_
maintainer.go:719-822). The engine guard and the capability rules apply to every
delegate call the window makes, and a refusal there poisons the context (these
are mutation entry points), so partial window bookkeeping never commits. A record
delete whose re-election inserts into an insert-refusing delegate is refused as a
whole; removal stays possible through deleteWhere, clear, disable and drop, and a
delete that re-elects nothing is admitted. The window's subspace lock is the only
lock ever held across a delegate call, so window -> partition is the one allowed
nesting order. Because refusal poisons, the window needs no pre-write effect
prediction and no in-memory overlay: its update and queued old-then-new paths keep
reading their own writes exactly as today. JVM and Go fixtures cover a windowed
GuardiANN index with an insert-refusing configuration: delete without
re-election (admitted), delete with re-election (refused), an update that moves
the boundary record outward in a full MIN window with no overflow (the insert half
refused after the window's own writes), a queued old-plus-new entry, and
deleteWhere (admitted); after each refusal and catch-and-commit the store bytes
are unchanged.

The complete WS-D declared-divergence list, each with its DIVERGENCES.md entry and
measured or fixture-pinned target behaviour (letters are kept stable across
design revisions): (a) withdrawn in v7 (was a config-only insert refusal for
RaBitQ extra bits; the quantizer-site refusal is parity); (b) inserts refused for
insertMaxCandidateClusters < 1, the only config-only insert refusal; (c) the
livelock rows refused with a typed error at the point the target writes its
empty-list task, instead of looping, including the permanent cases for the
immutable splitNumNearestClusters, mergeNumNearestClusters and
reassignNumNeighboringClusters; (d) no inline task for a delete whose head task
(or the dependency a head bounce would execute) a Go consumer would refuse, decided
by the consumer's own `consumerOutcome`; (e) capability errors poison mutating
contexts and are not retried, and any error a task body returns after its removal
is buffered poisons, decided once in the task-execution wrapper; (f) the search lock hold span (section 1); (g) capacity hand-off for
sessions the target strands (section 5); (h) at the target's two throw sites for an
over-max cluster, SplitMergeTask.java:397 (no usable candidate) and KMeans.java:136
(a candidate with fewer vectors than k, reached through SplitMergeTask.kMeans
:1097-1111), the Go n<k rule, the iterated outlier peel with its geometric removal
floor, admitted only while its work floor(log2(n - 1)) * n * d * max(I * (R + 1), 32) / 32 is at
most 1.96 * 10^7 (a rule outcome read from n, d and the index's KMeans knobs, no clock), with Go-only 1007/2101 failures in place of the target's throw past it
and every retry owner bounded as the target's (sections 5 and 6), and the terminal reconcile, which fails with the Go-only
ClusterUnsplittableError instead of clearing when the reconciled count exceeds
primaryClusterHardMax (section 6), where the target throws forever; the same n<k rule
at KMeans.java:136 for a MERGE candidate (a 3->2 core with fewer than two cleaned
primaries, whose Go INVALID score leaves the 2->1 fallback, SplitMergeTask.java:
508-529, 550) and for a SPLIT's 1->2 candidate beside a viable 2->3 one (Go selects
the 2->3), where the target throws; (i) the zero-primary new cluster drop
and the empty-neighbour assertion skip (section 6), where the target throws
forever; (j) the tag-5 identity write placed after routing in both modes and, in
deferred mode, after the cap check (section 3); (k) the lowest-index error of a
bounded parallel collector (section 1). Everything else, including every throwing
consumer in the table above, matches Java. Go operational fixtures pin each
refusal with store bytes unchanged after catch-and-commit, queue replay and task
execution, with one carve-out: a refused merge drain follows a committed
claim-only lease transaction, exactly as in the target, so the lease row
(secondary tag 1) is excluded from that comparison and asserted on its own (owner
= the merge session id, timestamp inside Java's window). Euclidean and cosine
must pass forced split/merge, not merely insert/search; DOT_PRODUCT and
EUCLIDEAN_SQUARE are admitted as in the target and pinned failing at the KMeans
consumer on their first live split (knob golden rows), exactly as the target
fails. No invented 15-bit codec.

## 2. Shared identities, vectors and HNSW prerequisites

Retain original persisted bytes/type, current transformed representation and
encoded provenance separately alongside lazy computational coordinates. Consume
WS-A's exact RaBitQ encoder/reconstruction. Never decode/re-encode an already
encoded vector just to hash it; plain vectors must still undergo Java's active
storage transform at the same call sites as Java.

Port full unsigned packed-byte SplitMix64 folding for operation RNG (including
first-insert rotation), Java bounded nextInt and split-consumption order. Keep
HNSW top-layer hash seeding unchanged. Separate entropy-backed v4 UUIDs,
deterministic RNG v4 UUIDs, MD5/name-derived v3 identities without a namespace,
and GuardiANN SHA256-derived v8 signatures. New sample keys use tuple.UUID; old
byte-string sample suffixes remain consumable because aggregation reads count
and value, not suffix type. Preserve reverse snapshot scans and explicit consumed
key conflicts; malformed aggregate payloads surface errors, not silent skips.

Close raw vector-engine cosine differences with Java's zero/nonfinite/no-clamp
ordering; leave SPFresh's existing metric route and approved scalar semantics
unchanged. Comparator helpers use Java Double.compare and tuple/identity tie
rules; Java signed-long UUID comparison is distinct from tuple UUID byte order.

Use a complete HNSW stored-node value, including optional covering tuple, through
loads/caches/neighborhood changes/repair. Layer-zero accepts legacy3-field and
new4-field tuples, with absent versus present-empty values preserved. Upper
inlining layers remain unchanged and cannot carry covering tuples. Port all
single/batch/edge/preload/write paths and both-direction cold-cache JVM rewrites.

Common results contain PK, optional raw/reconstructed vector, optional covering
values, distance and rank. Fetch returns distance0/rank-1; cardinality examines at
most two layer-zero entries. Preserve transforms and cosine non-invertible norm.
Java record entries still contain exactly one vector-or-null value, not appended
covering columns; record insertion passes no low-level covering tuple. Preserve
Go scalar-vector input composition separately rather than reinterpreting fields.

Port shared objective-based beam search, ring objective/upper-layer widths and
stateful outward traversal. Preserve semantic expansion order and PK ties;
pipeline only independent reads. Quick-start suppresses all original quick-start
keys later, not just emitted ones. Implement the source spatial cutoff (radius0
or null minimum PK disables HNSW cutoff), repeated readiness, cancellation and
error propagation. Outward traversal is approximate, not globally sorted.
Expose Java insert-if-absent for shared engines/centroids; keep direct-Go HNSW
upsert as the explicit existing convenience.

Return-vectors is tri-state through scan options, plan identity, serialization
and executor: absent defaults to !configuredRaBitQ, including before training;
false remains distinct from absent. Apply Java efSearch defaults rather than
executor-invented overrides. Resume materialized continuations without a new
search; their wire format remains unchanged. Update the independent executor
vectorScanRangeFingerprintSalt as well as plan equality/hash/serialization:
encode canonical option names with presence AND typed value, including every
GuardiANN query knob. Absent/false/true return-vectors are distinct identities;
alias spellings with the same presence/value canonicalize identically. Changed
options reject a continuation while identical options replay without searching.
All of these (tri-state return-vectors default, Java efSearch default, new salt
fields) apply to IndexTypeVector (HNSW and GuardiANN) only. SPFresh shares the
executor vector path and salt function but keeps its kc default, its ordered-
stream horizon floor (the SPANN section 3.2.3 probe budget) and its existing
salt bytes, so SPFresh continuations issued before the change still resume;
HNSW/GuardiANN continuations issued before it are rejected with the standard
stale-continuation error, because their replay assumed a different return-vectors
default. Tests pin both. These executor/plan changes require Graefe design/
implementation ACK and the project stress comparison protocol.

## 3. GuardiANN persistence and algorithms

Port separate GuardiANN storage/types, not SPFresh layouts. Exact tuple tables
and serializers are specified in the engine report: roots0 access,1 centroid
HNSW,2 metadata,3 references,4 collapsed membership,5 current identity/covering,
6 samples,7 tasks. Preserve nested versus flattened PKs. Roles0/1/2 and state
bits1/2/4 are independently decoded; malformed/unknown codes fail with typed
errors. The target's fifth metadata field (lifetime peak) is mandatory. Reject
intermediate four-field GuardiANN metadata with explicit rebuild context, never
invent a peak. This does not reject supported three-field HNSW nodes.

Signature input is getRawData() of the representation supplied at the actual
Java call site, NOT universally the original persisted bytes. The fixed hash is
SHA256 first16 bytes with version8/IETF stamping. The representation contract is:

| Read/input state | Working representation | Signature input |
|---|---|---|
| Plain reference, no active training transform | Plain under identity transform | Working plain bytes |
| Plain reference written before training, loaded after training | Apply current rotation/translation/normalization exactly once | Transformed plain bytes, not its old persisted bytes |
| Already encoded reference | Pass encoded representation through unchanged | Exact existing encoded/calibration bytes |
| New client vector | Apply current transform; quantization only at Java's corresponding encode boundary | Bytes of the resulting representation at the signature call |

Collapse and replica folding hash the loaded working representation. Expansion
uses the representative's actual signature identity/membership keys, not a new
hash of decoded coordinates. Reference/task serialization applies the active
quantizer at Java's write boundary; preserve already encoded bytes, but do not
suppress a required encoding of working plain vectors. Deletion's target-source
call hashes transformed input; the conditional membership-correction contract
below handles a demonstrated mismatch without changing canonical signature bytes.
Pin before/after training, mixed plain/encoded references, collapse/reopen/folding/
search/delete and both-direction membership-key interoperability. The original
research shorthand claiming all signatures hash persisted bytes is superseded.
Deterministic PK identity can repeat after delete/reinsert; do not claim universally
fresh generations. RunningStats preserves Welford/inverse/Chan expression order,
IEEE identity/max behavior and narrowly allowed roundoff clamping. Lifetime peak
is separately monotone across shrinkage. Retain one final empty cluster.

Implement Java KMeans and PartitionEvaluator separately from SPFresh. Include
validation before k1, deterministic ordered means/reductions, metric/estimator
paths, bounded-int KMeans++, restarts, empty-cluster handling, final assignment
without recentering, optional balancing, normalized imbalance, percentiles,
margin calculations, score and gate order. Candidate selection compares verdict
before score. Do not substitute assignments from KMeans for final ownership:
new centroids and outside neighbors jointly determine destination homes.

Port bootstrap/transforms/samples, primary assignment, priority/occlusion-based
replication, underreplication, hard-cap backpressure, delete with bounded candidate
clusters and stale filtering. Existing-PK insertion is no-op but can first execute
inline maintenance. Capacity errors retain typed causes and index/prefix/cluster/
count/limit context; they are not FDB retry codes or automatic-disable conditions.
The hard cap applies ONLY when maintenance is not run in the transaction, exactly
as in the target (Insert.java:330-338); Go adds no insert-side cap in inline
mode. (v7 added one for clusters the terminal reconcile leaves oversized; the state
it keyed on is also reachable in the target's own flow, and the refused insert was
the one that would re-arm the split, so it is withdrawn; section 6 bounds the
residue inside the reconcile instead.) Measured inline behaviour confirms why a
general inline cap is wrong: with
primaryClusterMax 10 and hardMax 11 the target accepts 20 inline inserts and
splits into three clusters (engine level and through the record layer's
autoMergeDuringCommit alike, oracle items 8 and 14), and a deferred backlog
already at the cap is relieved by 10 inline inserts; a cap in inline mode would
refuse inserts on those healthy indexes. Operation order. The target runs the inline task (inline mode only,
Insert.java:206-212), constructs the quantizer (:235), writes the tag-5
VectorMetadata row (:293), collects the routing and replication candidates
(:295), and applies the deferred-mode cap inside the reference write
(:330-338). Go keeps that order with one move, in both modes: the tag-5 write
comes after routing and, in deferred mode, after the cap check. Nothing between
the two positions reads tag-5 rows (routing reads centroids and cluster metadata;
replication, the cap and the stats sampling read neither), and the identity UUID
is name-derived from the PK under deterministic randomness and entropy-drawn
otherwise (RandomHelpers.randomUuid(Tuple, boolean), RandomHelpers.java:174-176),
never drawn from the operation RNG, so no RNG draw moves and every successful
insert writes the same bytes. Because Java writes the identity before the cap
throw, a Java caller that catches the error and commits keeps an identity with no
references, and a later insert of that PK is a no-op; Go's cap check is a read
placed before the tag-5 write, so a refused insert leaves no GuardiANN state.
DIVERGENCES.md records the move (declared (j)). The caller's record write
is the caller's transaction, exactly as for any index-maintenance error in Java
and Go. The insert cap's capacity error does not poison the context (it is the
target's own error, raised before any task runs), so a caller that catches one and commits keeps its record write with no
index entry in both engines, and re-saving the same record is filtered as
unchanged, so the record stays unindexed exactly as in Java. The improvement is
engine-level only: Go leaves no orphan tag-5 identity. A retained JVM fixture
shows the Java residue; the Go fixture shows none.

kNN retains the bounded candidate pool, then collapsed expansion, current UUID
filtering and limit. Post-filter underfill is permitted; no unbounded refill.
Ordered retrieval expands before cutoff, bounded almost-sort, metadata filtering,
PK deduplication and limit. The window2 fixture [2,3,4,1] => [2,3,1,4] deliberately
pins non-global ordering. Defaults/config validation are the final target values
in the research tables, not earlier in-range thresholds or scaled guesses.

## 4. Persisted maintenance state machine

Implement exact split/merge, reassign, collapse and bounce tuples, including
bounce's string final-kind and task UUID priority bit. Decode using current
execution access info; it is not persisted in the task. Preserve ordered inputs
where ordering controls RNG or byte payloads; do not let Go map iteration choose.

Fetch a bounded key-ordered task batch. Point recheck each row with RYW; skip
already-consumed dependencies without notification. Remove before executing;
notify once after a present task succeeds, including a topology no-op. Failure
rolls back removal/topology/count together. Attempt the first fetched position
before the deadline test; later positions require remaining time. Count only
direct executions, not bounce's recursive dependency work; time does not preempt
an individual task. Do not overstate a count budget as a total recursive-work cap.

Port two-phase saved-neighbor discovery/revalidation, vanished-neighbor handling,
reference deltas, underreplicated ownership moves and exact cause-set gating.
Collapse uses strict > threshold, existing representatives and real memberships.
Replica folding checks membership before folding ordinary same-byte references;
fresh nonmembers remain distinct and caps apply after folding. Split/merge covers
1→2,2→3,2→1,3→2, verdict-first selection and exact fallback/collapse/bounce paths.
Bounce actively executes a remaining dependency; it is not passive requeueing.
Peak-relative merge eligibility is strictly below max(min,floor(fraction*peak)),
with flags/cardinality guards. No new background algorithm replaces these tasks.

## 5. Actual record-layer task consumption

Counts/leases live under the real secondary subspace, distinct from GuardiANN
roots and WS-C's pending-write queue. Tags0/1/2 are sparse per-prefix task counts,
lease, and index-wide nonmaterialized delete guard. Counts are little-endian
int64 atomic ADD then compare-and-clear zero, same transaction as task mutation.
Include the bare empty-prefix key in scans; reject negatives and skip lingering
zeros. Compose count and once-per-write merge signals; quiet deferred writes
restore existing backlog signals. Nil timers do not suppress task events.

Lease discovery examines at most16 positive-count prefixes, prefers already-owned
work, otherwise randomly selects a free/stale candidate. Claim with a blind
UUID/timestamp write and commit without draining; next invocation snapshot-rechecks
ownership, then refreshes/drains. Boundary timestamps <=now-60s or >=now+60s are
stale. Claims may both commit; task conflicts still provide correctness. Acquire
read-conflicts the delete guard; prefix delete clears leases/counts/partition and
write-conflicts the guard. Release serializably verifies owner.

Merge defaults to one direct task and4s quota on the drain, reports actual direct
execution and requests another pass. No actionable prefix in the16-row window is
not global emptiness. Negative count schedules keyed precommit disable and returns
success with0/0 so disabling commits. Verify interaction with build-state heartbeat
checks: never accidentally roll back corruption handling or waive ownership on
ordinary failures. The WS-C adaptive retries keep their schedule and now run OVER
the attempt-bounded transaction calls of phase D-0 (section 5, "Attempt bounds"): two
nested bounded loops, as the target's IndexingThrottle runs over its runner.

Queued replay and build ranges need an explicit capacity progress transition.
This is a DECLARED LIVENESS DIVERGENCE from target Java, not parity: Java drains
the whole queue before merging (IndexingBase.java:1069-1071, iterateAll at
IndexingPendingWriteQueue.java:91), its drain store keeps autoMergeDuringCommit
false (IndexDeferredMaintenanceControl.java:40), so the hard cap applies during
replay, and IndexingThrottle retries only FDB lessen-work codes, so a capacity
exception fails a Java build range outright. Java does merge after every
committed build range (IndexingBase.java:1063-1072), so a Java build strands only
when a single range attempt itself crosses primaryClusterHardMax; queue replay
strands whenever the queue holds enough inserts for one cluster to reach the cap,
because the whole queue drains before any merge. Go makes progress in both shapes
without changing any stored-format bytes; the implementation adds a
DIVERGENCES.md entry and retained live-JVM fixtures for exactly those two shapes,
showing the target session failing with its typed capacity exception while Go
completes on the same committed state.

Only a session that hits the cap takes a different path. Sessions that never
see a typed capacity failure keep Java's order exactly: drain the whole queue,
then merge, per build range. The declared divergence is therefore limited to
sessions target Java would strand.

Attempt bounds of the transaction owners (v15). GuardiANN work runs inside
transactions whose retry owner Go has so far left unbounded, where the target's is
bounded; this is the first WS-D phase (D-0 in section 6).
- Target (SOURCE). `FDBDatabase.run` opens a runner and runs through it
  (FDBDatabase.java:856-864). The runner's `RunRetriable` retries only while
  `currAttempt + 1 < maxAttempts` and some cause in the chain is a retryable
  `FDBException` or a `RecordCoreRetriableTransactionException`
  (FDBDatabaseRunnerImpl.java:191-207), with a fresh context per attempt and
  ExponentialDelay between attempts; maxAttempts defaults to 10 and the delay to
  10 ms initial, 1000 ms maximum (FDBDatabaseFactory.java:90-92). The merger runs
  EACH merge attempt through `common.getRunner().runAsync` (IndexingMerger.java:
  82-86, "this runAsync will retry according to the runner's maxAttempts"), so a
  retryable failure reaches `handleFailure` after at most maxAttempts, and the
  session is bounded by failureCountLimit 1000 (:78, :125-128) and the quota
  schedule. The target's relational layer runs a statement in ONE context with no
  runner (RecordLayerTransactionManager.java:49; no retry or isRetryable under
  relational/recordlayer), and its catalog initialization is one attempt too
  (RecordLayerStoreCatalogImplTest.java:375-380).
- Go today. `FDBDatabase.Run` delegates to the client's `TransactCtx` loop
  (database.go `Run` -> `runTransactCtx`), which is unbounded by default: the raw
  client keeps libfdb_c's default on purpose (`pkg/fdbgo/fdb/
  unbounded_default_pin_test.go`), and the record layer never sets a limit.
  `indexingMerger.merge` runs each attempt through `oi.db.Run`
  (indexing_merger.go:27), so a retryable failure that recurs never reaches
  handleFailure at all. `FDBDatabaseRunner.RunWithRetry` (runner.go) already has
  Java's attempt loop with maxAttempts 10. TODO.md's item "Port Java's
  FDBDatabaseRunner default maxAttempts=10 … into pkg/recordlayer.FDBDatabase.Run"
  records the Run half (its superseded recipe, an attempt loop opening transactions
  beside the transactor, is replaced there by a pointer here); the TODO.md block
  "FDBDatabase.Run attempt bound — now owned by RFC-257 WS-D phase D-0" points at this
  section.
- Decision (v15). One attempt LOOP, one commit ROUTE that owns every commit of its owners,
  a stated policy per owner, and chaos faults keyed to the CALL. v14 corrected v13 where the
  v13 gates found it untrue of the tree: bodies that commit themselves, owners that retry
  outside the route, the chaos arms' firing, the client's 1200 and SPFresh's foreground.
  v15 corrects v14 where the v14 gates (`ws-d-design-review-v14/`) found it untrue:
  - the body's error chain, which the default backend strips (the attempt closure now
    records it, and the pure-Go wrapper keeps it, a second client prerequisite);
  - the retry predicate, now the runner's any-cause rule, with the first-cause rule kept
    for AutoContinuingCursor;
  - a self-committing body, now refused before anything commits, with Java's class;
  - the exhaustion reconcile, now checked against the model before and after;
  - pages, which write and run through `Run`;
  - the throttled iterator's Go-only terminal errors;
  - the uncounted retry's delay;
  - the delay under simulation, which no longer moves the clock;
  - the route's interface;
  - the replay cases of the existing chaos tests;
  - the owners that call the backend directly;
  - the catalog bootstrap's write;
  - the peel's performance criterion and the price of an owned 1007;
  - stale references.
  (1) The route. Every attempt of every owner that runs through `Run`, its variants or
  the manual runner is ONE call into `d.transactor` (`runTransactCtx`, database.go), so
  the chaos transactor and any tracing wrapper see every attempt of THOSE owners, as
  `NewFDBDatabaseWithTransactor` promises (database.go:187-190), and the route owns the
  commit of every such attempt.
  - Bodies that commit themselves. `runDDL` (ddl.go:880-894) and `ensureCatalogInit`
    (ddl.go:851-870) call `txn.Commit()` inside `Run`'s body: that is
    `FDBRecordContext.Commit` -> `CommitWithVersionstamp`, which commits and runs the
    post-commit hooks INSIDE the body (database.go:1316-1350), after which `Run` runs the
    commit checks again, commits the committed transaction again and runs the hooks a
    second time (database.go:300-322). SimFDB documents the double commit (txn.go:1342-
    1348). D-0 removes both in-body commits (the body returns and the route commits once,
    hooks once), and makes the rule structural.
    - The refusal comes before anything commits (v14 refused after the body's commit had
      landed, which reported a durable commit as a failure). The route marks the context it
      hands the body as route-owned. On such a context, `CommitWithVersionstamp` refuses
      as its first statement, before the commit checks, the version-mutation flush and
      `tx.Commit()`, so the body's refused commit leaves nothing committed.
    - The refusal is Java's class, `RecordContextNotActiveException`, ported as
      `RecordContextNotActiveError`. Java's class extends `RecordCoreStorageException`,
      so `errors.As` finds the Go type as both it and a `RecordCoreStorageError`.
    - Java's own behaviour is the same class one step later. A Java body that calls
      `context.commit()` lands its commit, and the runner's `commitAsync` then fails in
      `ensureActive` with "Transaction is no longer active."
      (FDBRecordContext.java:479-481, 531, 548-551).
    - Go refuses the body's commit instead of the runner's, so nothing lands and the error
      is true. Declared in DIVERGENCES.md with that reason.
    - A context is deactivated by its commit WHATEVER the outcome, as Java's
      `closeTransaction(false)` runs on both (FDBRecordContext.java:515-531): after a
      failed commit a second commit is refused the same way. The route never commits a
      context twice, since each attempt opens a fresh one.
    - The same port gives Go Java's `ensureActive`: a committed context refuses a second
      commit with the same class and Java's message. Today Go commits it again, which
      SimFDB documents (txn.go:1342-1348).
    - The raw path is not intercepted: `rc.Transaction().Commit()` on the backend
      transaction the context holds. A wrapper that refused it would hide the optional
      interfaces the record layer asserts on that transaction (`fdb.
      ReadVersionInstantReporter`, cascades_generator.go:2459, among others). So the raw
      path behaves as the target's does (the commit lands; the route's own commit then
      fails on the committed transaction), and it is held to zero call sites by a census,
      restated at the implementation's tree:
      `git grep -n -E '\.Transaction\(\)\.Commit\(' <tree> -- 'pkg/*.go' 'cmd/*.go'
      ':!*_test.go'` gives 0 at the v15 tree, and `\.Transaction\(\)` gives 154 lines
      as its positive control. It is a census, not a guard: a transaction kept in a
      variable and committed later is outside what it sees, which is why the census of
      self-committing bodies below reads every census body.
    - The census of self-committing bodies is
      `git grep -n -E 'Commit(WithVersionstamp)?\(\)'` over the bodies of the census
      lines of (3), classified in the implementation evidence. At the v13 tree it finds
      these two (their `Commit` calls at ddl.go:862 and :890 at the v15 tree, inside the
      `Run` bodies that start at :857 and :884).
  - Owners that open their own transactions, a census class of their own (v13 said "none
    of them retries"; three do). Each with the target's bound and predicate:
    - explicit SQL transactions and autocommit DML statement transactions
      (`beginTransaction` -> `CreateWritableTransaction`, connection.go:783): one commit,
      never replayed (cascades_generator.go:1450-1460), the target's behaviour;
    - the queue drain's throttled iterator (throttled_retrying_iterator.go, each range on
      a transaction it opens through `OpenContext`). It retries any error except a closed
      runner up to its limit, default 100, shrinking the range to max(1, scanned*9/10), as
      the target's `ThrottledRetryingIterator` does (ThrottledRetryingIterator.java:70,
      :268-280, used from IndexingPendingWriteQueue.java:83-91). That is kept, with the
      two Go-only terminal classes sections 1 and 6 add to the same iterator (v14 wrote
      "exactly", which contradicted them):
      - a capability error (section 1);
      - `ClusterUnsplittableError` (section 6).

      Neither exists in the target, whose engine has no such failure. Today's code excludes
      only `RunnerClosedError` (throttled_retrying_iterator.go:84-92), and the phase that
      adds each class adds its exclusion. A 1007 from the peel cannot reach this iterator:
      the drain runs no maintenance task, because its store keeps `autoMergeDuringCommit`
      false, as the target's does (IndexDeferredMaintenanceControl.java:40);
    - `AutoContinuingCursor` (cursor_combinators.go, `onNextWithRetry`): the target retries
      `FDBExceptions.isRetriable` (a `RecordCoreRetriableTransactionException`, or the
      first `FDBException` in the chain and its `isRetryable`) up to
      `maxRetriesOnRetriableException` (AutoContinuingCursor.java:134-144,
      FDBExceptions.java:236-247). That first-cause predicate is ported as
      `isRetriableFirstCause`, which AutoContinuingCursor alone calls (the loop's
      predicate of (2) is the runner's any-cause rule, a different Java function). Go's
      `isRetryableForContinuation` extends runner.go's `isRetryableError`, which D-0
      deletes; it calls `isRetriableFirstCause` instead, and keeps its existing Go-only
      1031 arm (a fresh transaction resumes from the saved continuation; the target's
      predicate excludes 1031), which D-0 declares in DIVERGENCES.md with that reason;
    - the heartbeat cleanup (`cleanupHeartbeatWithin`, online_indexer_queue.go:312): its
      attempt body opens its transaction with `CreateWritableTransaction`, because its
      commits must not outlive the caller's deadline, and it is a retry owner: it calls
      `attemptLoop` with that body and the indexer's MaxAttempts.
    None of these attempts goes through `d.transactor`, so the chaos transactor sees none
    of them (declared); their faults in tests come from the backend (SimFDB's conflicts in
    DST, the real cluster's in FDB tests).
  - The retry limit. The attempt body first sets the transaction's retry limit to 0, so
    the backend's own loop makes exactly one attempt and hands a retryable error back, on
    all THREE backends: the pure-Go client's OnError returns the caller's error once
    `retryCount >= retryLimit` (pkg/fdbgo/client/transaction.go:2503-2504); libfdb_c's
    `retry_limit` does the same; SimFDB honours it in OnError and routes `ReadTransact`'s
    retries through OnError (landed with v13). `FDBDatabaseRunner.runOnce` (runner.go:283),
    which opens its transaction through `CreateWritableTransaction`, moves onto the route.
  - The body's error chain (v15; v14's claim that the chain survives was false on the
    default backend). The pure-Go `fdb` wrapper, which `fdbclient.Open` returns in the
    default build (pkg/internal/fdbclient/open_purego.go:29), replaces the body's error
    with a bare `*wire.FDBError{Code}` before the client's loop sees it (`unconvertError`,
    pkg/fdbgo/fdb/database.go:384-399, transaction.go:552-561), and turns the loop's
    terminal error into a bare `fdb.Error{Code}` (`convertError`, transaction.go:537-546).
    `ReadTransactCtx` (:415-434) and `Tenant.TransactCtx` (tenant.go:28-41) do the same.
    So on that backend a typed error that wraps a retryable code reaches the record layer
    as its bare code, and `SPFreshSplitWindowError` would be counted as an attempt.
    Two changes, each with its own spec:
    - The record layer: the attempt closure records the error its body returned, and
      `attemptLoop` uses that recorded error whenever the transactor's error carries the
      same FDB code. This is how the target's runner sees its body's exception: it owns
      the attempt and the commit, and it never hands the exception to a binding loop
      (FDBDatabaseRunnerImpl.java:180-205). It holds for any transactor, including the
      chaos and tracing wrappers and a custom transactor passed to
      `NewFDBDatabaseWithTransactor`.
    - THE RULE IS PER EXECUTION (v16), for both mechanisms. The recorded error is the LAST
      closure execution's: it is reset to nil when the closure starts, set when the body
      returns an error, and left nil when the body returns nil. A transactor that runs
      the closure more than once in one call (its own loop, a chaos arm's re-execution)
      therefore never pairs an earlier execution's typed error with a later commit
      failure of the same code. Without the reset, a `SPFreshSplitWindowError` from
      execution 1 would turn a real commit 1020 of the last execution into an uncounted,
      unbounded retry. The wrapper prerequisite follows the same rule: it compares on
      every iteration, as the Apple binding's `retryable` does
      (bindings/go/src/fdb/database.go:163-191), so its kept error is always the
      current iteration's.
    - PINNED independently of the client prerequisite (v16; v15's pins go green on every
      backend once the wrapper keeps the chain, so none of them could fail with the
      recorder deleted). A test transactor that returns a bare `fdb.Error{Code}` for
      whatever its body returned (the shape the default backend has today) drives:
      - `Run` and `RunRead` with a body error that wraps a retryable code: the caller
        receives the body's chain, and `SPFreshSplitWindowError` is retried uncounted;
        with the recorder removed both go red;
      - the stale-error case: execution 1 of a call returns `SPFreshSplitWindowError`,
        the transactor re-runs the closure, and execution 2's body succeeds and its
        commit fails with 1020. The result is a bare 1020, and it is COUNTED as an
        attempt.
    - The client, a second client-gated prerequisite beside 1200. The Apple Go binding
      the `fdb` wrapper mirrors keeps the body's error when OnError re-raises the same
      code: its `retryable` replaces the error only when OnError returns a non-`fdb.Error`
      or a different code (bindings/go/src/fdb/database.go:163-191 at 7.3.77). The
      repository's libfdb_c backend already follows that rule (libfdbc/backend.go:241-251).
      The pure-Go wrapper's `TransactCtx`, `ReadTransactCtx` and `Tenant.TransactCtx`
      adopt the same rule: the attempt keeps its body's error, and a terminal error with
      the same code returns it unchanged. Like the 1200 change, this goes through the
      fdb-client-engineer workflow and its C++ reviewer before D-0 lands.
    - SimFDB's `declinedRetryError` (landed with v14) keeps the body's chain as well. Its
      comment cited pure-Go parity, which was false at the wrapper; v15 corrects the
      comment to cite the Apple binding's rule and the libfdb_c backend (no behaviour
      change).
    - Pinned on every backend: a body error that wraps a retryable code reaches the
      caller with its chain at the limit through `Run` and `RunRead`, on the pure-Go
      client (`Transact`, `ReadTransact`, and the tenant path), SimFDB and libfdb_c.
      `TestSimRetryLimit_DeclinedRetryKeepsTheBodysErrorChain` is SimFDB's existing half.
      Its mutation run, which v14 did not record, is in
      /var/tmp/fdb-upgrade-recovery/simfdb-pin-mut/ (`binding.txt` ties the good copy to
      the tree's blob `a129771b` by `git hash-object` and sha256, v16):
      - the baseline, `TestSimRetryLimit` uncached, four tests pass;
      - the mutant returns OnError's error on a same-code decline (`return oerr`, marker
        counted 1) and exits 3;
      - the mutant reddens exactly the chain test, at its `Transact` check (:131) and its
        `ReadTransact` check (:142), "the body's wrapper was dropped";
      - the file is restored and compared afterwards.
  - What the route drops, each stated. The raw client's handling of a maybe-committed
    error (its retry copies the previous attempt's write conflicts into read conflicts,
    pkg/fdbgo/client/transaction.go:2555-2579) does not carry into a fresh attempt, for
    1021 and 1039 alike, as it does not in the target's fresh-context runner. The target's
    argument rests on its owners' bodies being safe to re-run after a commit that landed;
    for the Go-only owners of (3) the implementation evidence states, per census line,
    that the body is idempotent after a landed commit or what it does instead. A
    transaction timeout applies per attempt, as the target's per-context timeout does.
    The commit-unknown barrier stays inside Commit (transaction.go:1979-1980). The codes
    only the client's OnError retries (`onErrorRetryable`, commitpath.go:280-310, beyond
    `fdb.IsRetryable`) stop being retried, as the target's runner does not retry them:
    1079 (blob_granule_request_failed, retried by C++ `Transaction::onError` but outside
    the error predicate, fdb/error.go:436-466), and 1235 and 1242 (FDB 7.4+ forward
    compatibility). 1200 is (2)'s.
  (2) The loop. `attemptLoop(ctx, policy, route, attempt)` owns at most policy.MaxAttempts
  attempts, and retries only when the error is retriable by the rule of the target's
  runner, `RunRetriable.handle` (FDBDatabaseRunnerImpl.java:180-207: the walk at :195-205, the decision at :207). That rule walks the
  WHOLE cause chain and retries when ANY `FDBException` in it is retryable or ANY cause is a
  `RecordCoreRetriableTransactionException`. It is ported as `isRetriableAnyCause`:
  - an `fdb.Error` with `fdb.IsRetryable` anywhere in the chain (the pinned
    fdb_error_predicate RETRYABLE set, retry_predicate_pin_test.go:15-37);
  - or a `RecordCoreRetriableTransactionError` anywhere in the chain (added with the first
    of the target's throwers Go ports: FDBExceptions.FDBStoreLockTakenException and
    FDBStoreRetriableException, FDBReverseDirectoryCache.java:361, LocatableResolver.java:462;
    none exists in Go today).

  Go's chain is a tree where Java's is a list: the walk follows `Unwrap() error` and
  `Unwrap() []error` (so `errors.Join` and `fmt.Errorf` with several `%w`), and a join
  is retriable when any branch is. v14 ported `FDBExceptions.isRetriable` here, the
  first-cause rule, which in the target only AutoContinuingCursor calls
  (AutoContinuingCursor.java:140). That rule is kept for that cursor alone, as
  `isRetriableFirstCause` ((1) above). A pin runs both predicates on chains where they
  disagree. `fdb.Error` is `{Code int}` with no `Unwrap` (fdb/error.go:10-12), so a
  single-cause chain holds at most one FDB error, and on one the two predicates always
  agree (v15 described a pin over such a chain, which cannot be built). The pin is over
  TREES:
  - `errors.Join(fdb.Error{non-retryable}, fdb.Error{1020})`: first-cause false,
    any-cause true;
  - `fmt.Errorf("%w: %w", fdb.Error{non-retryable}, fdb.Error{1020})`, the same;
  - a join whose later branch is a `RecordCoreRetriableTransactionError`: any-cause
    true.

  Errors that sections 1 and 6 call terminal under `Run` never unwrap to a retryable
  code, which the any-cause walk would otherwise retry: a capability error,
  `ClusterUnsplittableError` and `RecordContextNotActiveError` carry no FDB code in their
  chains. A unit test walks each with `isRetriableAnyCause` and asserts false.

  runner.go's `isRetryableError`, which adds 1235 and 1242, is deleted.
  - 1200 leaves the record layer. v13 retried the Go client's 1200 (all_proxies_unreachable,
    raised before any commit is sent, commitpath.go:59-62) and let it consume an attempt,
    so a DDL failed on the first one and a 10-attempt owner gave up after about 1.6 s
    (3.3 s at most) of delay during a recovery the target's client waits through. That is
    a client divergence surfacing as a user-visible failure, and C++ is the client's spec:
    libfdb_c's commit with no known commit proxy load-balances over a null set, which is
    `Never()` (LoadBalance.actor.h:752-762, `if (!alternatives) return Never()`, reached
    from `tryCommit` through `getCommitProxies`, NativeAPI.actor.cpp:2768-2774 and
    :6637-6643), races it against `onProxiesChanged()` (:6646-6649; :6645 is the
    `grvTime` capture, which the client's transaction.go:2045 comment cites), and when the proxy
    set changes throws `request_maybe_delivered`, which `tryCommit` turns into
    `commit_unknown_result` after its self-conflicting fence (:6730-6772). D-0's
    prerequisite is therefore a pure-Go client change: a commit that finds no commit proxy
    waits for the proxy set to change, bounded by the transaction's timeout and its
    context, and then fails as a maybe-delivered commit, 1021, through the same fence; it
    no longer returns 1200. It goes through the client's own review gate (the
    fdb-client-engineer workflow, the FDB C++ reviewer), with a differential against
    libfdb_c, before D-0 lands; `fdb.IsOnErrorRetryable` and `onErrorRetryable` drop 1200
    in the same change. The record layer's predicate never names 1200.
  - SPFresh's split-window signal does not consume an attempt. The synthetic retryable
    1020 an SPFresh foreground write raises when its routing meets a SEALED posting
    (spfresh_write.go:410-419) becomes a typed `SPFreshSplitWindowError` that wraps
    `fdb.Error{Code: 1020}`, so every owner and the SQL mapping still read it as a
    retryable conflict (40001, connection.go:980-981). `attemptLoop` retries it WITHOUT
    counting an attempt, bounded only by the caller's context, which keeps RFC-094's
    foreground contract (a write that meets a split window re-runs until the split
    publishes, 094:127-128) on every owner and every backend. The wait for an all-SEALED
    route (spfresh_write.go:410-419) is an existing Go departure that D-0 keeps as today,
    not a LIRE property: LIRE separates foreground updates from background rebalancing,
    and a foreground write waiting on a split is Go's choice.
    - The loop sees the signal through the chain rule above. Until the wrapper
      prerequisite lands, the recorded body error is what carries it on the default
      backend. After it lands, the wrapper keeps the chain as well, and the recorder
      still serves every transactor that strips it (the test transactor above pins that).
    - An uncounted retry still takes the ported delay and advances the delay's `current`
      like any retry. The delay is drawn uniformly from [0, current)
      (ExponentialDelay.java:62-72), so two re-runs can be almost 0 ms apart; the MEAN
      gap is about current / 2, 500 ms once `current` reaches its 1000 ms maximum. That
      bounds the average rate of re-runs, not the gap between two of them (v15 said "at
      most once per maximum delay ... never in a hot loop", which is false).
    - UNDER A SIMULATED ENVIRONMENT the delay is drawn and not waited, so the retry would
      be a hot loop there, and SimFDB's 100-retry cap no longer bounds it. The hunts pass
      `context.Background()` (hunt.go:158). So a simulated environment adds a BOUND,
      which a real one does not have. After 100 consecutive uncounted retries of one
      call that each observed the SAME sealed posting (its id and its seal version
      unchanged), the call fails with `SPFreshStalledSealError`, naming the posting.
      100 is SimFDB's former backstop, `maxRetries`. The error is not retriable, so a
      DST run that stalls a seal fails loudly instead of hanging; a takeover that
      publishes or re-seals changes the observed posting and resets the count.
    - "As today" is true of the pure-Go client and libfdb_c, whose loops are unbounded.
      It is not true of SimFDB, whose loop caps at 100 retries today (simfdb.go:14).
      Under D-0 each attempt sets retry limit 0, so that cap no longer bounds a foreground
      write and only the caller's context does, on SimFDB as elsewhere (declared).
    - The lease is wall-clock (spfresh_util.go:18) and lasts 60 s (spfresh_build.go:848),
      and a single-goroutine DST driver has no second actor to take a stalled lease over.
      Two SimFDB fixtures:
      - with the takeover scheduled: the lease overridden to expired and the takeover
        run from the attempt observer after the write's first uncounted retry; the
        write completes, with its uncounted retries counted;
      - with no takeover scheduled: the write fails with `SPFreshStalledSealError`
        after exactly 100 uncounted retries, and the test's own deadline is never
        reached (a hang would reach it).
    v13 bounded it by
    the owner's attempts and argued the window "closes within the split's two
    transactions, far inside ten attempts"; the v13 gates showed that is false (the
    chunked drain keeps the parent SEALED across every chunk transaction and publishes
    only in the final one, spfresh_split.go:414-484; concurrent updates and deletes abort
    and stretch SPLIT's read, 094:351-353; the delay's draws can make ten attempts pass in
    far less than their mean). Every other retryable error of a foreground write takes
    its owner's bound. Declared Go-only (the target has no SPFresh): RFC-094 section 2
    states it now, and D-0 adds the DIVERGENCES.md entry when it lands (the behaviour
    does not exist before D-0; v14 wrote it as already recorded there, and it is not).
  - The delay. Between attempts the target's ExponentialDelay, ported exactly
    (ExponentialDelay.java:62-72: the next delay is uniform in [0, current), current starts
    at the initial delay, then doubles to the maximum with a 2 ms floor; defaults 10 ms and
    1000 ms), its randomness drawn through the one DST seam helper that the current
    allowlisted call becomes (dst_seam_gate_test.go:87), and its WAIT through the
    database's environment.
    - Under a real environment it sleeps the drawn delay in a `select` on `ctx.Done()`.
    - Under a simulated environment (`dst.NewSim`) it draws the delay (so the seeded
      stream, and with it replay, is the same as a real run's) and does not wait at all:
      no sleep, and no clock movement. v14 advanced the `SimClock`, which breaks the
      clock's contract: `Clock` has only `Now()`, and time moves only when the driver
      advances it (pkg/dst/clock.go:20-33, 41-45). Two concurrent loops would also have
      added their delays where sleeping overlaps them, so persisted timestamps would have
      depended on retry counts and interleaving.
    - Declared: the simulation does not model the retry delay's time. Nothing the record
      layer persists reads it, and a DST timer surface (After/Sleep, Track B,
      clock.go:25-27) is where it would be modelled.
  - The route is an argument of the call, not a context value. `attemptLoop` hands the
    route to the transactor call it makes, and `runClientLoop` hands the client-loop route;
    no context carries it, so an owner nested inside an attempt body cannot inherit the
    attempt route (v13's context mark could, and on the unbounded client loop an inherited
    non-committing arm would spin until its deadline). A pin runs `runClientLoop` inside
    an attempt body and asserts the client-loop route's behaviour.
  - The route's interface (v15; `fdb.Transactor` has no argument to carry it,
    database.go:191). The record layer defines the interface
    `recordlayer.AttemptTransactor`, with two methods, one per body kind (v15 had the
    writable one only, so `RunRead`'s attempts carried no call identity):
    `TransactAttempt(ctx, call AttemptCall, fn func(fdb.WritableTransaction) (any,
    error)) (any, error)` and `ReadTransactAttempt(ctx, call AttemptCall, fn
    func(fdb.ReadTransaction) (any, error)) (any, error)`. `AttemptCall` carries:
    - `Route`: the attempt route, the client-loop route, or the own-transaction route of
      the owners in (1) that open their transactions themselves;
    - `Owner`: a stable label per census line, such as "merger.attempt", "sql.page",
      "sql.ddl", "catalog.bootstrap", "heartbeat.cleanup" or "resolver";
    - `Execution`: the index of this execution within its call, counting EVERY execution,
      the uncounted split-window retries included (v15 left uncounted retries
      undefined);
    - `Attempt`: the number of COUNTED attempts made before this execution, the number
      `MaxAttempts` bounds; an uncounted retry repeats it. "Faults the call's first
      attempt" means `Execution` 0, and the attempt observer reports both numbers;
    - `CallID`: an id unique per `attemptLoop` call, which the once-per-call arms key on.

    The chaos transactor implements it. `FaultEveryAttempt`'s call filter selects on
    `Owner`, and the once-per-call arms key on `CallID`. `NewFDBDatabaseWithTransactor`
    keeps its signature: a transactor that does not implement the interface is adapted
    by the database, which calls its `TransactCtx` (or `Transact`) per attempt, so a
    custom transactor sees every attempt but not the call's identity (declared at the
    constructor). The heartbeat cleanup's `attemptLoop` call uses the own-transaction
    route, whose attempts reach no transactor, as (1) states.
  - Context: the loop checks `ctx.Err()` before every attempt and waits in a `select` on
    `ctx.Done()`; a context that ends before the first attempt returns `ctx.Err()`, and one
    that ends later returns an error that wraps both `ctx.Err()` and the last attempt's
    error (`errors.Is` finds each). After the final attempt the last error is returned
    unchanged. The loop serves `Run`, `RunWithVersionstamp`, `RunWithWeakReads`, `RunRead`
    (each attempt one `ReadTransactCtx` with retry limit 0), `FDBDatabaseRunner.RunWithRetry`
    (its 0.5x to 1.5x jitter replaced by the ported delay) and `cleanupHeartbeatWithin`.
    `RunWithWeakReads` applies the weak-read semantics on the first attempt only, as the
    target's runner does (TransactionalRunner.java:181-192). The versionstamp and the
    post-commit hooks belong to the attempt that committed, and run once.
  (3) The policy of each owner, from a census of every non-test caller of `Run`,
  `RunWithVersionstamp`, `RunWithWeakReads` and `RunRead` in pkg/ and cmd/, counted on the
  reviewed TREE rather than the working directory: `git grep -n -E
  '\.(Run|RunWithVersionstamp|RunWithWeakReads|RunRead)\(' <tree> -- 'pkg/*.go' 'cmd/*.go'
  ':!*_test.go' ':!pkg/fdbgo/*'` gives 75 lines at the v13 tree `3a934039`, and the same 75
  lines at the v14 tree `8d721bd1` and the v15 tree `512ae248` (line numbers below are the
  v15 tree's); removing the
  receivers that are not an FDBDatabase (testcontainers `Run`, `exec.Cmd`, the stack
  tester, the dst-hunt driver's `hunt.Run` and `cfg.Workload.Run`, planner tasks, the
  plandiff and factory runners) and the four comment lines leaves 53 (v13's `--untracked`
  counted the working directory; v12's miss was eight SimFDB lines and one misattributed
  line). The implementation re-runs the command on its own tree and restates the counts:
  - The online indexer, 14 lines: online_indexer.go (9), indexing_mutual.go:237, the
    merger's per-attempt call indexing_merger.go:27, and the heartbeat administration
    indexing_heartbeat_admin.go:179, :197, :216 (Java runs those through the indexer's
    runner, OnlineIndexer.java:435): the INDEXER's attempt bound, a new
    `OnlineIndexerBuilder.SetMaxAttempts` defaulting to the database's, as the target's
    builder sets its runner's (OnlineIndexOperationBaseBuilder.java:445-449) and the runner
    inherits the factory default of 10. The build's lessen-work retries (maxRetries 100)
    run OVER each attempt-bounded call, as the target's IndexingThrottle runs over its
    runner. `shouldLessenWork` (online_indexer.go:1453) is Java's
    IndexingThrottle.lessenWorkCodes 1:1, SIX codes: 1004, 1007, 1020, 1031, 2002 and 2101
    (IndexingThrottle.java:65-71); a 1021 fails the build range as in the target.
  - Go-only owners with no target counterpart, the database default (10): statistics
    (statistics.go, 4), the fleet (build.go:125, fleet.go:238), the frl CLI (10 lines in
    cmd/frl), the chaos harness (chaos/concurrent.go 5, chaos/scenario.go 5) and the DST
    hunts (pkg/simfdb/hunt/hunt.go:258, :273, :288, :304, :332 and
    hunt/continuation/continuation.go:368, :471, :512). Each line's evidence entry states
    that its body is idempotent after a landed commit (1).
  - Owners that call a backend's `Transact` or `ReadTransact` directly, outside `Run` (v15;
    the v14 census missed them). The census is `git grep -n -E
    '\.(Transact|TransactCtx|ReadTransact|ReadTransactCtx)\(' <tree> -- 'pkg/*.go'
    'cmd/*.go' ':!*_test.go' ':!pkg/fdbgo/*' ':!pkg/simfdb/*'`, 26 lines at the v15 tree.
    The route itself accounts for 10 of them (database.go:355-365, 4 lines;
    chaos/fault.go:146-228, 6 lines), and the binding stack tester for 13
    (cmd/fdb-stacktester, which exercises the client's own loop by design and keeps it).
    That leaves three:
    - `keyspace.FDBResolver.Resolve` and `ReverseLookup` (keyspace/fdb_resolver.go:62,
      :155). The target's resolver runs every read and allocation through its database's
      runner (LocatableResolver.java:124-148, `database.runAsync` and `runner.runAsync`),
      so it is runner-bounded. Go's resolver holds a raw `fdb.Database`. The loop, the
      ported delay and both predicates live in one internal package,
      `pkg/internal/attempt`, which imports only fdbgo and `pkg/dst`, so the record layer
      and `keyspace` (which cannot import the record layer) share one implementation;
      `recordlayer.attemptLoop` is a thin call into it. D-0 also threads the context the
      resolver ignores today (fdb_resolver.go:48-50) into its calls. D-0 runs each
      call through the shared loop with the database default (10) on the own-transaction
      route, each attempt one raw transaction with retry limit 0. The allocation body
      re-reads the name before it allocates, so a landed allocation is found, not
      repeated, on the next attempt.
    - `frl store dump` (cmd/frl/internal/cmd/store_dump.go:136): a Go-only operator read,
      at snapshot isolation. It keeps the client's loop, which an interrupt ends
      (declared; the CLI has no counterpart in the target).
  - The DST hunts and the chaos model under exhaustion. The hunts assume "faults are
    retried transparently by db.Run, so any surfaced error is a bug" (hunt.go:210-212) and
    update their model only after a success (hunt.go:256-300); the chaos `StoreModel`
    records successes only (`TrySaveRecord`, scenario.go:283-300). Under D-0 a call whose
    faults outlast its attempts surfaces a retryable error, and its last attempt may have
    been a 1021 that APPLIED (SimFDB's applied branch, simfdb.go:41-46, 56-80; the chaos
    arm commits before returning 1021).
    v14 reconciled such a call by copying the operation's keys from the store into the
    model. That is a blind resync (after an exhausted `deleteAll`, hunt.go:284-297, it
    resyncs everything), and its fixture held by construction. v15 checks the store
    against what the model predicts:
    - The observer. The loop reports every attempt to an observer the harness installs,
      on the loop's goroutine, as the attempt ends: its call id, its index, and its error
      code. A 1021 attempt also reports whether it applied, when the harness can know. On
      SimFDB, `LastCommitUnknownApplied` is read there, on the committing goroutine before
      it commits again, which is the scope its doc requires (simfdb.go:56-80). A chaos
      `FaultCommitUnknown` arm knows it committed.
    - The classes. A surfaced error whose chain is retriable, on a call that made
      MaxAttempts attempts, is EXHAUSTED; any other surfaced error is a bug, as before.
      An exhausted call is then one of two kinds:
      - KNOWN-NOT-APPLIED when every attempt ended in a body error, or in a code of the
        RETRYABLE_NOT_COMMITTED set (C++'s `fdb_error_predicate`; the set runner.go:
        422-433 lists: 1007, 1009, 1020, 1037, 1038, 1042, 1051, 1078, 1213, 1223, 1235,
        1242). The store must equal the model from before the operation.
      - UNKNOWN otherwise: some attempt ended in a MAYBE_COMMITTED code (1021, 1039,
        runner.go:418-420) or in any other commit error (v15 named the literal 1021, so a
        real cluster's 1039 fell into neither class). With ground truth (SimFDB, the chaos
        arm), the store must equal the model from after the operation if any such attempt
        applied, and from before if none did. Without it (a real cluster), the store must
        equal one of the two.
    - What is compared. The operation's keys are compared, and every other key of the
      model's range must equal the model unchanged. Only when that holds does the model
      take the side the store matched (before or after), and the call is counted. A store
      that matches neither side (a double apply of a non-idempotent write, a partial
      apply) fails the hunt at that operation, naming it.
    - The ceiling. A hunt or scenario whose exhausted count exceeds its stated ceiling
      fails.

    The SQL DST harnesses (sqlhunt, sqlpage, metamorphic, golden) reach the loop through
    connection.go:418 and ddl.go; their error classes are re-derived the same way and
    listed in the implementation evidence.

    Fixtures, in the hunt and in the chaos scenario:
    - an applied 1021 on the last attempt, then exhaustion, reconciles to the after-model;
    - a discarded 1021 reconciles to the before-model;
    - an exhaustion of 1020s reconciles to the before-model;
    - a test transactor that commits HALF of an operation's writes and returns 1021 on
      every attempt leaves a store that matches neither side, and the hunt fails, naming
      the operation. This is the fixture that shows the reconcile can go red.
  - SQL, split by what each route does:
    - DDL statements (`runDDL`): ONE attempt, as the target's relational layer runs a
      statement: a conflicting DDL surfaces 40001 and a maybe-committed DDL surfaces its
      1021 instead of being re-executed. The parallel suites' setup DDL on the shared
      catalog is in census (5): if a setup DDL can lose a conflict, the harness that issues
      it retries it on 40001 (a test-side retry, documented at the harness), never the
      production policy. That retry is legitimate only where the conflict is one the
      target's catalog would have too. So the evidence names, for each conflict (5)
      observes between two setup DDLs, the key ranges the two transactions conflict on.
      That report needs `report_conflicting_keys`, which only libfdb_c provides: the
      pure-Go client rejects the option (fdb/options.go:261-269, API_PARITY.md:57), and
      SimFDB ignores it. So this census runs under the libfdb_c build (v15 said "the
      conflicting-keys report the client returns", which the default client cannot
      produce). It also shows that the
      target's catalog writes those same keys for the same statements. A conflict on a
      key only Go writes is a conflict-footprint bug in Go's catalog, fixed in D-0, not
      retried.
    - The catalog bootstrap (`ensureCatalogInit`, reached from `Ping` on a session's first
      use): the database default. The target initializes its catalog once, at driver
      construction, in one attempt; Go bootstraps lazily from every session, so retrying
      it is safe and a single attempt would turn the parallel suite's contention into
      spurious `Ping` failures (declared).
      - The bootstrap is idempotent, but v14 called it write-free, and it is not. Steps 1
        and 2 check, then create. Step 3 always saves the `/__SYS/CATALOG` schema row
        (`SaveSchema`, fdb_store_catalog.go:140, then `SaveRecord` at :294), as the
        target's `saveSchema(txn, catalogSchema, true)` always overwrites.
      - The target runs that write once per driver. Go runs it on every session's first
        `Ping`, so in production a one-attempt DDL can lose to any other session's first
        `Ping`, not only in the test harnesses.
      - D-0 makes step 3 check-then-create as well. It loads the stored schema row and
        writes only when the row is absent or differs from the generated one. The bytes
        written are the target's whenever a write happens, and a bootstrap over an
        existing, equal catalog writes nothing.
      - Fixture: a second session's bootstrap over an initialized catalog commits
        read-only (no mutation, no write-conflict range), and a DDL concurrent with it
        commits in one attempt.
    - Every read route through connection.go:418 (`runInCapturedTx` with no captured
      transaction, which calls `DB.Run`, so each page is one `TransactCtx` call per
      attempt, not a `ReadTransactCtx`): the pages of an autocommit SELECT
      (cascades_generator.go:2207), the
      planner's index-state read through a store open (:2732), catalog and schema loads
      (connection.go:444, :558), the six system-table reads (system_tables.go:211, :253,
      :321, :409, :570, :613): the database default. The statistics read
      (cascades_generator.go:2645) is the one route here that is a `RunRead`, one
      `ReadTransactCtx` per attempt, with the same policy. The target runs a statement's reads in its one
      context; Go's page transactions are Go-only. A page is not free of writes (a store
      open can write its header and rebuild an index inline, cascades_generator.go:2162-
      2171); the retry is safe because those writes are idempotent, which is also why the
      target's `checkVersion` repeats them freely.
    - Autocommit DML: unchanged; it never used `Run` (above).
  - SPFresh (RFC-094, Go-only, a preserved boundary), split by route:
    - background lifecycles (`spfreshRun`, spfresh_util.go:11-16, and its 39 non-test
      callers): NOT changed. `spfreshRun` keeps the transactor's own loop through a named
      entry `runClientLoop` that nothing else calls; that loop is unbounded on the pure-Go
      client and libfdb_c and capped at 100 retries on SimFDB (simfdb.go:14, `maxRetries`,
      unchanged and declared: the simulator's backstop), so its pin uses twelve conflicts,
      above `Run`'s ten and below SimFDB's cap, on both the pure-Go client and SimFDB.
    - FOREGROUND writes (`Update`, `UpdateWhileWriteOnly`, spfresh_index_maintainer.go:212,
      :287) run in the CALLER's transaction and take the caller's policy, except the
      split-window signal of (2), which consumes no attempt. So ordinary churn, a chunked
      drain and a drain stretched by deletes are waited out as today, and a STALLED seal
      (a rebalancer that died between SEAL and SPLIT, 094:536) holds the write until the
      lease takeover finishes the split or the caller's context ends, as today. Under SQL
      autocommit (one attempt) an INSERT that meets a split window fails with 40001 today
      and still does. RFC-094:198 and :473 are corrected to point at section 2.
  - `FDBDatabaseRunner.RunWithRetry` callers: the runner's own MaxAttempts, as today.
  After the last attempt the error reaches the caller unchanged, and every caller in the
  census already treats a non-nil error as its operation's failure (the evidence names
  the handler per line); the online indexer's build is the one that reacts to the code.
  (4) The chaos harness, keyed to the call. The existing arms re-execute INSIDE one
  `TransactCtx` call (commit, then run `fn` again, fault.go:199-211), which is what a
  client loop does; on the attempt route that would hide the retry from `attemptLoop`.
  - On the attempt route an arm FIRES AT MOST ONCE PER `attemptLoop` CALL: the arm's draw
    is made once per call and, when it selects the call, faults the call's first attempt
    (a scenario can name another attempt index), so the loop's next attempt runs clean,
    which is the retry the fault exists to exercise. A separate arm, `FaultEveryAttempt`,
    configured explicitly and scoped by a call filter (the merger's per-attempt call, a
    named SQL route), faults every attempt of the calls it selects and drives exhaustion.
    v13 drew on every attempt, so a rate-1.0 arm exhausted every call:
    `TestPagedDistinctUnderAmbiguousCommitsHoldsSteady` (FaultCommitUnknown at 1.0,
    distinct_scratch_retry_charge_test.go:130) would have failed every page; under
    once-per-call firing each page's first attempt commits and returns 1021 and its
    second runs clean, so every page re-executes once, which is what the test requires,
    and the merger fixture's admission and heartbeat calls each re-run once.
  - The arms: `FaultCommitUnknown` runs `fn`, COMMITS, and returns 1021;
    `FaultConflict` and `FaultTransactionTooOld` run `fn`, do NOT commit, and return 1020
    and 1007; `FaultReadError` fails the body's reads. "Does not commit" holds on every
    attempt-route owner because the route owns every commit (1): a body's own commit is
    refused with `RecordContextNotActiveError` before anything commits.
  - What the redefinition takes from existing tests, and where it goes (v15). Today
    `FaultConflict` and `FaultTransactionTooOld` commit and re-execute, "a superset test"
    of double commit by design (chaos/fault.go:24-34). Under D-0 they stop replaying a
    landed commit, so a test that relied on them for replay would stay green while no
    longer testing it. `TestSPFreshChaos_WritePathFaults`
    (chaos_vector_spfresh_test.go:125-129, "the path most exposed to a non-idempotent
    replay") and the `FaultsAll` and `FaultsRetryHeavy` presets are among them.
    - The census is `git grep -n -E 'FaultConflict|FaultTransactionTooOld|FaultsAll|FaultsRetryHeavy' <tree> -- '*.go' ':!pkg/recordlayer/chaos/fault.go'`:
      57 lines in 15 test files of pkg/recordlayer/chaos at the v15 tree.
    - Each line is classified in the implementation evidence by what its test asserts:
      - REPLAY (a landed commit re-executed must leave the model right). Moved to
        `FaultCommitUnknown` at the same rate, which keeps committing and re-executing.
      - ROLLBACK (the not-committed path). Kept on the redefined arm.
      - BOTH. Split into one case per arm.
    - Each preset keeps its total fault rate. The share it gave the two redefined arms
      is split so that `FaultCommitUnknown` carries at least the replay share it had.
    - Every moved test records its injected-fault count before and after at its seed,
      so the move shows the replay still fires.
  - On the client-loop route (SPFresh's background lifecycles) an arm fires once per
    call inside the inner loop, in both of that route's real shapes: commit then re-execute
    for 1021, and run without committing then retry for 1020 and 1007; the unbounded loop
    absorbs both.
  - Reads: `ReadTransactCtx`, which injects nothing today (fault.go:216-229), gains
    `FaultTransactionTooOld` on the attempt route, so `RunRead` and its callers
    (statistics.go:799, cascades_generator.go:2645, hunt.go:332) are fault-driven.
  - Pages are NOT read-only (v14 said they were, and contradicted itself). A page runs
    through `Run` (connection.go:418), so every `TransactCtx` arm reaches it: 1020, 1007
    and 1021 alike. Page 1 can write the store header and rebuild an index inline, and
    it can come back not_committed (cascades_generator.go:2162-2171, "REACHED"). So the
    page fixtures inject `FaultConflict` on page 1, a write-carrying page, and assert
    that the query returns every row exactly once. They inject `FaultTransactionTooOld`
    on a later page and assert the same, and `FaultCommitUnknown` on page 1 and assert
    the re-executed page returns its rows once and its header write lands once.
  - Every scenario configured with faults asserts at its end that its injected-fault count
    reaches a stated floor (its seed, transaction count and floor, restated whenever its
    draws change), and a unit pin drives that check at and above its floor.
  (5) Finding tests that relied on unbounded retries is a census, not one run: every
  test that runs concurrent writers against shared keys (the chaos concurrent scenarios,
  the SPFresh foreground concurrency, chaos and SQL end-to-end specs,
  spfresh_concurrency_test.go:145-160 and chaos_vector_spfresh_test.go:273-283 among them,
  mutual indexing, the online indexer's concurrency specs, the sqldriver concurrency
  specs, the DST hunts, and the parallel suites' setup DDL on the shared catalog, listed
  by file in the evidence) runs with `--runs_per_test=20` before and after D-0; the
  record layer counts attempts per call, and a contended chaos workload, the SPFresh
  concurrency specs and `TestSPFreshForegroundFillBenchmark` (bench/
  spfresh_sift_benchmark_test.go:462, env-gated, run explicitly) print the attempt
  histogram before and after. No test states its own MaxAttempts to pass: an owner whose
  observed maximum reaches its bound is a finding about that owner's production policy,
  decided in this section before D-0 closes.
- Fixtures (attempts counted; each on the pure-Go client AND on SimFDB, the attempt-count
  ones under the libfdb_c build too): `FaultEveryAttempt` with `FaultConflict` returns 1020
  after exactly MaxAttempts attempts through Run, RunWithVersionstamp, RunWithWeakReads
  (weak reads observed on attempt 1 only) and RunWithRetry, and with
  `FaultTransactionTooOld` on reads returns 1007 after MaxAttempts through RunRead; a
  once-per-call arm re-runs a call exactly once; a closure failing with 2101 returns after
  one attempt; a wrapped retryable error at the limit reaches the caller with its chain
  through Run and RunRead on the pure-Go client (its Transact, ReadTransact and tenant
  paths), SimFDB and libfdb_c (2); the two predicates on the chains where they disagree
  (2); a chain whose only retryable cause sits behind a non-retryable FDB error, or on one
  branch of an `errors.Join`, is retried by Run; `FaultCommitUnknown` on attempt 1 through Run leaves the landed commit
  visible to attempt 2, runs the post-commit hooks exactly once, and RunWithVersionstamp
  returns the final attempt's versionstamp; a body that commits its own context fails with
  `RecordContextNotActiveError` and nothing it wrote is visible afterwards (read in a
  fresh transaction), and a committed context committed again fails with the same class
  and Java's message; DDL and the catalog bootstrap run a hook
  registered in their body exactly once; the injected-fault floor through Run and through
  the merger is met; a deferred-merge test maintainer whose MergeIndex runs and then fails
  every attempt (`FaultEveryAttempt` on the merger's per-attempt call) ends the merger
  session on the target's schedule with the per-attempt count and the halvings asserted
  (the maintainer sets LastStep MERGE, as the target's VectorIndexMaintainer does at :536,
  which makes the halving reachable, indexing_merger.go:127-128); two concurrent DDLs that
  conflict on the catalog leave one applied and the other failed with 40001 after one
  attempt and ABSENT from the catalog, and a DDL under `FaultCommitUnknown` returns the 1021
  without re-executing with its change present exactly once; an autocommit SELECT page
  re-executes under `FaultTransactionTooOld`, page 1 under `FaultConflict` and under
  `FaultCommitUnknown` returns every row exactly once ((4); the connection's database comes
  from a test-only `embedded` option that installs a chaos-wrapped FDBDatabase); the
  catalog bootstrap over an initialized catalog commits read-only; a cancelled
  context before the first attempt returns `ctx.Err()` and during a delay an error that
  `errors.Is` both the context error and the last FDB error; under a simulated environment
  the delay draws from the seeded stream, never sleeps and leaves the `SimClock` where it
  was; `runClientLoop` nested in an attempt
  body takes the client-loop route; `spfreshRun` completes past `Run`'s bound as stated
  above; the exhaustion reconcile of (3) in a hunt and a chaos scenario, including the
  half-applied operation that turns it red; the resolver's calls bounded at the database
  default; and SPFresh
  foreground writes, sequenced through the attempt observer rather than timed: an insert
  through Run whose routing meets a SEALED posting during a single-transaction split, a
  chunked drain of at least two chunks, and a split whose read is aborted by concurrent
  deletes completes, with every split-window retry uncounted and each one preceded by a
  drawn delay; a STALLED seal, in the shape
  where every routed candidate is SEALED (a lone posting), holds the insert until the
  lease takeover finishes the split and then completes (on SimFDB the takeover is run from
  the observer over an expired lease, (2)); the same inserts through SQL
  autocommit return 40001 (as today). Each client prerequisite of (2) carries its own
  fixture in the client suite, with a differential against libfdb_c: a commit with an
  emptied commit-proxy set waits, returns 1021 once the set changes, and never returns
  1200; and a body error that wraps a retryable code comes back from `Transact`,
  `ReadTransact` and the tenant path with its chain when OnError re-raises the code. The GuardiANN fixtures (an inline
  insert whose task raises 1007, the same fault in the merger) belong to phase 4's
  acceptance, because they need GuardiANN; the GuardiANN maintainer sets LastStep MERGE in
  its merge path in phase 4, as the target's does.

Each retry owner keeps its own existing limit schedule; only the maintenance
hand-off and the stop rule are new and shared.

* Queue replay (throttled_retrying_iterator.go) already retries every failure
  with limit max(1, scanned*9/10), where scanned includes the failing entry,
  exactly as Java's ThrottledRetryingIterator (:206, :295, :343-345). That stays.
  This alone does NOT relieve the queue shape: the split task committed with the
  crossing batch never runs, because the target and Go both drain before merging.
* Build ranges (buildRangeWithRetries / indexing_throttle) classify the typed
  capacity error in a SEPARATE branch (shouldLessenWork keeps its 1:1 Java code
  list and tests) and reduce with the existing decreaseLimit(recordsProcessed),
  where buildRange already returns the count applied before the failure. Java
  merges after every committed range, so a build usually relieves itself once a
  smaller range commits the crossing and its split task.

The shared hand-off runs only when a capacity failure recurs at limit 1, in the
driver, never inside the failed attempt; the failed attempt is aborted (including
any split task it enqueued) and the last COMMITTED continuation/range is kept.
It runs a PREFIX-TARGETED merge of the blocked index/prefix, then retries. That
merge point-reads the prefix's task count and lease at SNAPSHOT isolation, as
Java does (VectorIndexMaintainer.java:553-554,654, VectorIndexMergeLock.java:
94-96), so the reads add no conflicts with concurrent inserts' count ADDs. Count
0 means no actionable work. A live lease owned by a DIFFERENT session (another
mutual builder, or a Java background merger) is a bounded backoff that never
lets the build's own session lapse for any peer whose lease is at least 10 s (the
Java default); a peer configured with a shorter lease judges staleness by that
shorter lease and can admit mid-wait, exactly as two Java sessions with unequal
leases can. Every peer judges a heartbeat stale by ITS
OWN lease (IndexingHeartbeat.java:106-107; Go checkAdmission,
indexing_heartbeat.go), and a default-configured Java builder's lease is 10 s
(OnlineIndexOperationConfig.DEFAULT_LEASE_LENGTH_MILLIS, :61). So the wait runs in
slices of at most min(the session's own lease, 10 s)/3, and after each slice runs
one heartbeat-renewing transaction through refreshFollowupHeartbeats
(online_indexer_queue.go:443 at the v15 tree), which renews every remaining target's
heartbeat via validateBuildTarget -> renewSessionHeartbeat (:530, :559, :419) and
re-validates every remaining target's state and stamp; stop the wait as soon as
the foreign lease is released or stale, the blocked cluster shows progress, the
context is cancelled or the iterator is closed. Go's own builder default lease
was 30 s, a pre-existing divergence from Java's 10 s; it is corrected to 10 s in
this change (online_indexer.go defaultLeaseLengthMs, pinned by
TestOnlineIndexerLeaseLengthDefault with both mutations killed), so a
default-configured Go session and a default-configured Java peer agree on
staleness. The change is covered by //pkg/recordlayer; the green full `just test` of the
frozen tree that carries it, with Ginkgo summaries for both conformance targets, is
recorded, with its Ginkgo summaries and the tree it ran on, in ws-d-oracle/README.md item 25. The total wait per hand-off is capped at one merge-lease window (60s,
VectorIndexMergeLock.java:63); a lease dated further in the future than that is
already stale by Java's rule (:103). The wait is charged to the retry budget. JVM
fixture: while Go waits on a live foreign vector lease, a default-configured Java
exclusive builder probes admission on the same index (checkAndUpdateHeartbeat) at
every slice boundary and is refused every time, for a Go session at its default
lease and for one configured with a 30 s lease (which exercises the min). A
negative count follows Java's keyed disable-and-commit path
(VectorIndexMaintainer.java:605-616): the index is disabled, the hand-off
returns, and the session ends at its next transaction with the typed
RecordCoreStorageError "Unexpected index state(s)" that refreshFollowupHeartbeats
raises for a target that is neither scannable nor write-only
(online_indexer_queue.go:458-472 at the v15 tree; WS-C changed it from the
IndexingValidationError v13 and v14 named, because that class would send a BY_INDEX
build to the catcher's records-scan fallback, which the target never takes, and the
class is declared there in DIVERGENCES.md, "OnlineIndexer session start and build
catcher"). The GuardiANN fixture for this path asserts that class and message. The
lease owner is the
build's own merge session id (control.SetMergeSessionID, the admitted session
heartbeat's UUID), stable across invocations as in Java (VectorIndexMaintainer.
java:544-551, VectorIndexMergeLock.java:49), so the build's own earlier
claim-without-drain lease is recognised as its own, never as foreign. Otherwise
it reuses the merger's claim, re-verify and drain helpers for that one prefix
(claim commits alone; each later drain transaction re-reads ownership, refreshes
the lease and drains with the normal budgets), with the same commit checks on
every attempt and the merger's retry and adaptive-budget rules. The drain loop
stops at the FIRST of: the blocked cluster relieved (dissolved, or its stored
primary count below the hard cap), the prefix count at 0, an OWNED drain that
executes no task, or 100 drain transactions (the queue iterator's own retry
budget), so sustained concurrent ingest into the prefix cannot keep it running
forever. The relieved test is a SNAPSHOT read in its own read-only transaction
after each drain commits, like the baseline read, so it adds no conflict with
concurrent inserts' stats writes to that cluster. A drain whose ownership
re-verify finds a LIVE FOREIGN owner is not "a drain that executes no task":
claims are blind writes where the last writer wins and the loser learns on its
next invocation (VectorIndexMergeLock.java:111-115), a drain runs only for the
stored owner (VectorIndexMaintainer.java:573-576), and a Java background merger
claims a free prefix at random (:591-598), so two claimants on one prefix are an
ordinary concurrent path. The hand-off then enters the same bounded,
heartbeat-renewing foreign-lease wait described above (sharing its 60 s cap per
hand-off), stopping on progress, release or staleness, and only then counts
toward the stop rules. Both sessions complete only when the lease holder relieves
the loser's blocked cluster, releases, or goes stale within the loser's 60 s wait
cap; the holder's own drain loop may keep refreshing its lease for up to 100 drain
transactions, and a holder draining a DIFFERENT cluster of the same prefix can
outlast the cap, in which case the loser ends with the typed capacity error (the
progress rule). Fixtures: two concurrent hand-offs on one prefix (two mutual Go
builders, and a Go builder racing a Java merger) blocked on the SAME cluster both
commit their claims, the loser waits, both sessions complete and every committed
task is consumed exactly once; and blocked on DIFFERENT clusters of one prefix with
a holder whose drain outlasts the loser's wait cap, the loser ends with the typed
capacity error and nothing it committed is lost. The wait cap is an injectable
driver parameter (default one merge-lease window, 60 s); the fixture sets it to
200 ms and gives the holder a drain that provably outlasts it, so the arm is
deterministic rather than a 60 s wall-clock race. It is not the 16-prefix window
with random claim, whose (0,0) result cannot prove the blocked prefix has no
work. The helper covers every phase of one session: queue replay and the
by-records, by-index and mutual build ranges (mutual has no pending queue, so
only its range path applies).

The typed capacity error carries index, grouping prefix, cluster, count and
limit (Java's exception carries only the last three, Insert.java:336-337), and
the hand-off acts on that index/prefix/cluster. Termination and errors. A merger
failure during the hand-off propagates as the merger's own error and ends the
session, like any merger failure; that includes a terminal reconcile's
ClusterUnsplittableError (section 6), a distinct type the hand-off never keys on. Hand-offs count against
the phase's configured retry budget (iterator retries; build maxRetries) and the
counter resets only after a committed batch; with maxRetries <= 0 the capacity
error propagates immediately. A further hand-off is allowed only if progress
happened ON THE BLOCKED CLUSTER named in the error, by anyone. The baseline is a
committed snapshot read of that cluster's STORED primary count (RunningStats n),
taken by the driver after the failed attempt aborted and before the hand-off; it
is never the error's count field, which is Java's would-be count (n+1) and can
include the aborted attempt's own buffered inserts. Progress means a later
snapshot read shows the cluster dissolved or its stored count below that
baseline: this session's merge, another session's merge, a deleteWhere, user
deletes, and a throw-site reconcile that removes stale references all count,
because each frees real capacity. Executing unrelated tasks in a busy prefix is
not progress. Otherwise (no actionable
work, work held by another session's live lease past the wait cap, or a cluster
no split can relieve) the phase returns the typed capacity error. Iterator hooks are
optional, so existing non-vector users keep their behaviour. Tests: the measured
target queue shape (30 queued one-cluster inserts, hard cap 12; the target
strands with 18 entries, Go completes), an over-cap queued build whose first
attempt aborts its own split (no committed backlog), an over-cap by-records range
and an over-cap mutual range, a committed-backlog blocked single entry, an
unsplittable oversized cluster that must end in the typed error rather than
loop, conflict and callback abort during the hand-off, a merger failure during
the hand-off, the build's own claim-only lease reused rather than read as
foreign, and exactly-once committed consumption. Small final-batch tests are
insufficient.

Important research reconciliation: Java GuardiANN executes in the transaction
supplied by IndexingMerger; it has no internal child-transaction runner. Do not
invent nested commits to satisfy wording about future backend callbacks. Prove
real GuardiANN work and heartbeat callback share each actual merge attempt:
claim-only, drain, retries and final no-work. Lucene is the separate actual nested
child-transaction callback consumer; it remains upgrade-wide work, not Go TEXT.
Every backend-created child, when present, must register on its own context.

## 6. DFS execution and acceptance

Implement in this dependency order, with green tests per increment and one joint
implementation gate for the completed WS-D milestone (not review laps per commit):

0. D-0: the attempt bounds of the transaction owners (section 5), preceded by its two
   client prerequisites, each landed through the client's own review gate with its
   differential against libfdb_c: the pure-Go commit waits for commit proxies as
   libfdb_c's does and stops returning 1200; and the pure-Go `fdb` wrapper keeps the
   body's error chain when OnError re-raises its code, as the Apple binding does. Then
   D-0 itself, with the full suite, the 1M stress comparison, the SPFresh stress and RFC-094's section-12
   interleavings, and the `--runs_per_test=20` census taken before and after, because
   it changes the retry contract of every record-layer transaction (SimFDB's
   retry_limit, its first piece, is landed).
1. Typed options/engine admission and immutable configuration boundary.
2. Shared identity/vector/result primitives; HNSW lossless codecs and mutation
   preservation, fetch/cardinality, beam/ring/outward traversal and record options.
3. GuardiANN types/codecs/statistics, storage and shared scheduling primitives;
   independent KMeans/evaluator; task codecs/executor and complete task algorithms.
4. Bootstrap/insert/delete/search; record engine integration, counts/leases,
   merge/deletion/error handling and actual queued-build/follow-up consumption.
5. Full cross-engine storage/operation scripts and failure/retry acceptance.

The live-JVM oracle (ws-d-oracle/README.md; `conformance/guardiann_probe_
conformance.java`, `conformance/GuardiannConformanceAccess.java`, Describe
`GuardiANN target oracle`, 16 specs; `ws-d-oracle/evidence-run.txt` records the
run, spec count and source hashes, `evidence-mutations.txt` the mutation kills)
has MEASURED: the VectorId hash formula and runtime pins; the split no-usable-
candidate stall; the split-beside-an-emptied-neighbour stall; queue-replay
capacity stranding; ordinary-save backpressure in deferred mode; the
deferred-mode degenerate-knob consumer map including RaBitQ training (values 0 /
-1 and extra bits 9); inline-mode inserts AND deletes (cap skipped and healthy,
unsplittable cluster failing every later inline insert and delete, per-kind task
failures, the neighbour-count livelocks); inline mode through the record layer's
autoMergeDuringCommit; that a size-penalised KMeans cannot rescue the split; the
iterated outlier peel on the split counterexample shapes at eight seeds each, at
n up to 1000; the target on those shapes in both modes; obsolete tasks consumed
as no-ops under the knob they would consume, including a split queued before a
bits-9 index trained; the reassignNumNeighboringClusters < 0 livelock; the
unsampled floor peel at d = 768 to 4096 beside the withdrawn sampled one (item 23);
heartbeat keys that sort after a live heartbeat and (UUID, x) keys (item 24); the
unsampled floor in the admitted high-dimension corner (item 26); and the ongoing
check's collapse of a (U) and a (U, x) heartbeat (item 27). Before
porting the corresponding branches, turn the REMAINING source hypotheses into
retained JVM regression fixtures the same way: primary/replica encounter-order
merge; quantized collapsed deletion signature; underreplicated-primary deletion
and underreplication-only metadata updates; the stale-inflated split shape; the
empty merge core; vanished neighbours (a merge whose neighbours vanished under
trained bits 9 and under splitMergeConcurrency 0 with precomputed neighbours, where
the target throws before its no-mergeable-neighbour clear); n<k candidates (the
stale-inflated split, a 2->3 candidate below three, a 3->2 merge core below two
beside the 2->1 fallback, a 1->2 split below two beside a viable 2->3); a bounce
under trained bits 9 with no outstanding dependency and an idle target (item 21,
the follow-up's valueTuple quantizer, BounceTask.java:147-150); the bounce's
re-enqueued and follow-up task ids from a positioned RNG; rollback of a failed task;
a GuardiANN delete that enqueues a task under trained extra bits 9; merges under
trained extra bits 9; a LIVE merge reaching the KMeans consumer under the KMeans
knobs; the splitMergeConcurrency and reassignConcurrency throws with precomputed
neighbours; and build-range (as opposed to queue-replay) capacity stranding. Do not assert an upstream defect from
source suspicion, nor silently copy a demonstrated corrupting path. A confirmed
upstream defect is handled at the narrow boundary with its divergence documented
and regression retained; upstream reporting requires publication authorization.
All ordinary behavior follows target Java. The required conditional corrective
contracts (implemented only with a retained distinguishing fixture establishing
the source defect/reachable boundary) are concrete:

* Reference dedup must prefer a live primary in either encounter order. If target
  Java's incoming-replica branch drops it, preserve the primary; two replicas use
  Java priority/tie semantics. Keep existing wire references/identities unchanged.
* Underreplicated count is the number of physical underreplicated primaries and
  cannot exceed primary population. Deleting that role decrements both counts;
  deleting another primary leaves underreplication unchanged. Persist any actual
  underreplication-only delta even when primary/replica deltas are zero. Preserve
  lifetime peak and role/state serialization; no compensating global counter.
* Collapsed deletion must remove only the matching (signature,PK,currentUUID)
  membership and current metadata, leaving other members visible. Use a found
  representative's persisted signature identity; when bounded discovery misses
  it, check the finite identity candidates from original plain, current transformed
  plain and active encoded representations, verifying the member UUID before
  deletion. No full membership scan or invented hash generation. Target fixtures
  expose original memberships, exact signature inputs and residual rows; Go must
  preserve remaining members and not leave the deleted identity query-visible.
* Split with no usable candidate (the orElseThrow site, SplitMergeTask.java:
  386-397). Java throws when every candidate is INVALID and collapse does not
  apply. Only minChildFraction (default 0.1, Config.java:207 bounds it to
  [0, 0.5)) makes a candidate INVALID (PartitionEvaluator.java:124), KMeans runs
  with lambda 0, and a lone cluster has no 2->3 candidate, so a lone cluster whose
  KMeans isolates a sub-group under minChildFraction of its vectors reaches this
  site; so do stale-inflated counts whose survivors are few or identical.
  MEASURED (oracle items 3, 8, 12 and 14): the task fails the same way forever on
  10+1, 20+1, two outliers on opposite sides, nested outliers, scattered
  outliers, a natural 60/40 structure under minChildFraction 0.45 and a
  heavy-tailed line; in deferred mode the cluster strands at the hard cap, and in
  inline mode every later inline insert in the partition fails (engine-level on
  every shape; through the record layer's autoMergeDuringCommit and for inline
  deletes, measured on 10+1). The target's second
  throw site for the same cluster is KMeans.java:136: Java passes every candidate,
  whatever its size, to `KMeans.fit` (SplitMergeTask.kMeans, :1097-1111; the per-
  candidate lambda :352-368 has no size check), and `fit` throws
  IllegalArgumentException("vectors.size() must be >= k") at :136, before the knob
  checks at :137. The population is every candidate with fewer cleaned vectors than its
  k: the stale-inflated over-max cluster whose cleaned survivors are fewer than two
  (its 1->2 candidate) and a 2->3 candidate with fewer than three; a 3->2 merge
  candidate whose core holds fewer than two cleaned primaries (the guard at :514-516
  counts clusters, not vectors); and an empty merge core (k = 1, n = 0). The target
  fails each of these the same way forever, even where another candidate of the
  same task is feasible (a 1->2 split beside a viable 2->3, a 3->2 merge beside the
  2->1 fallback). Go rule, source-derived at KMeans.java:136 and
  declared (h): a candidate with fewer cleaned vectors than k is scored INVALID
  without calling KMeans (its RNG split is still consumed, RandomHelpers.java:
  213-218, so later candidates draw as they would); the empty merge core takes the
  empty-core rule below. A split whose candidates are then all INVALID continues
  exactly like the :397 site. The Go rule at both sites has two steps.

  Entry. The Go rule is entered exactly where the target throws (every candidate
  INVALID or n<k, and the collapse route does not apply), and only there. At entry
  the task RNG is split ONCE into the peel RNG, whichever of Step 1, the admission
  refusal or Step 2 follows; no path on which the target completes ever draws that
  split, so the ordinary path's draws (materialisation, follow-up task ids) are the
  target's, and every later draw of a Go-rule task is independent of how many
  refits ran.

  Admission. Admission is evaluated after Step 1's other preconditions (the 1->2
  candidate was fitted, so n >= 2, and no other candidate is usable), so the cause a
  ClusterUnsplittableError records is deterministic: a split that fails a
  precondition reaches Step 2 as "no usable partition", never as "not admitted".
  Step 1 then runs only when its work W = floor(log2(n - 1)) * n * d *
  max(I * (R + 1), 32) / 32 is at most B = 1.96 * 10^7, where n is the cleaned primary
  count |P|, d the dimension of the working (transformed) coordinates the task's
  estimator reads, and I and R the index's kMeansMaxIterations and kMeansMaxRestarts
  (defaults 8 and 3, Config.java:182-183; a refit runs at most I Lloyd iterations in
  each of R + 1 runs, KMeans.java:112-113, and both are per-index options fixed at
  creation, GuardiannVectorIndexEngine.java:345-346, 435-438). The knob factor is
  floored at the default's 32 because only the Lloyd rounds scale with I * (R + 1):
  KMeans++ seeding (once per restart, KMeans.java:181-182), the final assignment and
  evaluation, and the floor's distance sort do not, and Config bounds d only from below
  (Config.java:197) with I >= 1 and R >= 0 (KMeans.java:137-138). So knobs smaller than
  the default never ENLARGE admission (v12's I * (R + 1) / 32 admitted n = 2000 at
  d <= 40,000 at I = 1, R = 0: 160 MB of half-precision vectors), and larger ones
  shrink it. W is evaluated in float64: every operand is an integer below 2^53, a W
  near B is below 2^25 and therefore exact, and a product large enough to round is far
  above B and refused either way, so unbounded I and R cannot overflow it into
  admission. W is a pure function of the task's input, never of elapsed time, the
  machine or the transaction: a refused peel goes to Step 2 with no refit, so a cluster
  Go does not peel fails or clears exactly as the reconcile decides, which is the
  target's failure class (it threw). At the default knobs B admits the default hard cap
  (2000) at d <= 980 and the first over-max size (1001) at d <= 2175 (9 * 1001 * 2175 =
  19,594,575; 2176 exceeds B). B is a WORK bound, a design
  constant, not a time calibration (v12 and v13 derived it from the worst observed refit
  rate, and every new run moved that maximum; v14 stops deriving it from time). Its value
  is chosen for coverage: at the default knobs it admits the default hard cap (n = 2000)
  up to d = 980, which covers 768-dimensional embeddings, and the first over-max size
  (n = 1001, the size at which a split is due) up to d = 2175, which covers 1536 and
  2048; the edge is exact, n = 2000 at d = 980 gives W = B = 1.96 * 10^7 and is admitted,
  and d = 981 is refused (a unit fixture pins both, and 9 * 1001 * 2175 = 19,594,575
  admitted, 2176 refused). The peel's TIME on the JVM is an estimate, reported, never a
  guarantee. It comes from a mechanical extraction over every timing line of every log
  under /var/tmp/fdb-upgrade-recovery/ that carries one (ws-d-oracle/README.md item 32:
  the script, its 13 inputs with their hashes, and its output; v14's seven-log population
  left out its own evidence run). 200 nonzero refit maxima over the 13 logs give
  4.13 * 10^-8 to 1.75 * 10^-7 s per n * d, median 7.44 * 10^-8. So a peel at W = B runs
  its refits in about 0.81 s at the lowest rate, 1.46 s at the median and 3.43 s at the
  highest. The highest is from `rfc257-v13-1.log`, a run whose rates were about 1.4x
  slower across the board, with no load recorded; the three logs of the v14 and v15
  evidence runs that record their load peak at 9.82 * 10^-8. The JVM timer covers the
  refits only, which are the KMeans calls of Step 1. The floor's distance sort, the
  assignment passes and the scoring are outside it (v14 dropped this sentence). The
  target's own candidate fit took 48.6 to 307.6 ms over the same lines, and a peel's
  refit total at most 1.12 s.

  Go's performance criterion (v15, an implementation gate of phase 4; v14 had none, and
  v13's rule for a slower Go was dropped with nothing in its place). B is a work bound
  and a refused peel is harmless. An admitted peel that outlasts the transaction is not
  harmless: it fails with 1007 on every attempt and leaves its task at the head of its
  queue. So:
  - What is timed. Go's WHOLE peel is timed at implementation: the candidate fit, every
    refit, the sorts, the assignment passes and the scoring, from the rule's entry to its
    outcome. It is timed at the two edge shapes, n = 2000 at d = 980 (W = B) and n = 1001
    at d = 2175, over the corner spec's generators and seeds, and at every acceptance
    fixture's shape.
  - How. At least two runs, each with the load average recorded before and after it, and
    both UNDER THE SUITE'S CONCURRENCY: the timing spec runs inside the full Bazel test
    run, `--local_test_jobs=4`, beside the other FDB containers, as the acceptance
    fixtures do (v15 left the load unconditioned, so a quiet-machine maximum said
    nothing about the fixtures' one-attempt assertion). The statistic is the maximum over
    every run, generator and seed.
  - The margin. That maximum is at most 2.5 s, half of FDB's 5 s transaction window. The
    other half is left for the attempt's reads (about 4 MB at n = 2000 in half
    precision), its commit (about 5.3 MB) and the load.
  - A miss is a Go performance defect in the peel (KMeans, the sort, an allocation), and
    it is fixed in Go. B is never raised to meet it. "Fixed" has a stopping condition
    (v15's "Go at its best" had none): Go's per-(n·d) refit rate, measured on the same
    shapes, is within 1.5x of the JVM's median rate (7.44 * 10^-8, item 32), and the
    peel's CPU profile has no frame outside KMeans's distance loop above 10% of its
    time. Only if Go meets that condition and still misses the margin is B lowered,
    once, to the largest value at which the maximum meets the margin. Every fixture and
    the coverage statement above then move in the same change.
  - The criterion is PER PEEL. An inline transaction that inserts several rows runs a
    task per insert, so its admitted peels add up, and a transaction whose inserts
    trigger k admitted peels takes about k times one peel. That shape can pass 5 s and
    fail with 1007 at a k the criterion does not bound. It is Go-only (the target throws
    on these clusters and never peels), declared with the 1007 pricing below, and the
    deferred mode is the stated remedy for bulk inserts into such clusters.
  - The acceptance fixtures commit their split in exactly ONE attempt, asserted through
    the attempt observer, so a 1007 cannot hide behind the merger's retries.

  Past the transaction's time the consequence is owned, and priced (v14 did not price
  it):
  - The attempt fails with 1007 and its owner's bound decides (the paragraph on
    transaction_too_old below).
  - In a merger session that is up to 10 attempts of about 5 s each, per quota step, over
    the roughly 12 halvings from the 4 s quota to the minimum before the merger gives up
    (IndexingMerger.java:246-258): about 10 minutes.
  - Inline, a write whose head task outlasts the window stalls about 10 × 5 s under Run,
    and about 5 s in a one-attempt SQL statement, and then fails.
  - The target throws a non-FDB exception on these clusters, which its merger aborts on
    at once (`shouldAbort`, IndexingMerger.java:189-203). So Go's failure class matches
    the target's, but Go takes longer to reach it.

  The criterion exists so this price is not paid at an admitted shape; a peel beyond B
  is refused before any refit runs.
  Step 1, iterated outlier peel with a geometric removal floor. It runs only when
  it is admitted, the 1->2 candidate was fitted (at least two cleaned primaries) and
  scored INVALID, and no other candidate is usable: a 1->2 candidate under the n<k
  rule has no assignment to start from, and the task then continues to Step 2 with
  no refit (that includes a 1->2 candidate under n<k beside a KMeans-INVALID 2->3
  candidate). Start from the 1->2 candidate's own KMeans assignment over the
  cleaned primary population P (n vectors), with the mass M = P and r = 0. While the latest partition is INVALID:
  - its undersized child (exactly one child is below minChildFraction*n, because
    minChildFraction < 0.5; with minChildFraction 0 no candidate is INVALID and this
    rule never runs) loses its members from M. If that removes nothing (the
    undersized child holds no member of M: KMeans could not separate M, the
    coordinate-identical mass), stop;
  - otherwise, if fewer than 2^(r+1) - 1 members have left M in total, the
    shortfall is taken from the members of M farthest from the OTHER child's
    centroid (descending distance with the task's DistanceEstimator; ties keep the
    lower primary index), so after round r at least 2^(r+1) - 1 members are gone;
  - if fewer than two members of M remain, stop;
  - refit KMeans k=2 with Java's KMeans (same iterations, restarts, lambda 0,
    estimator) on ALL of M, assign EVERY primary of P to the nearer refit centroid (strict less-than, ties to the
    lower index, Java Double.compare semantics), score that final partition with the
    same PartitionEvaluator and split parameters against the current partition, and
    advance r.
  The first non-INVALID partition is the selected candidate and is materialised by
  the normal path: final ownership against new and neighbour centroids,
  replication, the zero-primary new-cluster drop, metadata, follow-ups.
  The bound is STRUCTURAL, not empirical: the floor removes at least 2^(r+1) - 1
  members by round r, and the peel stops when fewer than two remain, so it performs
  at most floor(log2(n - 1)) refits (9 at n = 1000, 10 at the default hard cap of
  2000). No separate round cap exists. In the structured shapes (the 60/40 split
  at minChildFraction 0.45) the undersized child alone already exceeds the floor, so
  the floor never binds and the peel is the v8 peel; with isolated outliers the
  outliers are the farthest members and leave M in doubling batches.
  MEASURED (oracle items 11 and 18, eight seeds per shape; the probe seeds the
  initial fit with the seed and refit r with seed + 1 + r, so the counts are SAMPLES
  of the round distribution, not Go's pinned values; Go goldens pin Go's own
  draws): on every 2-D shape of item 11 (10+1, 20+1, two outliers on opposite sides,
  nested, scattered, the 60/40 structure at 0.45 at n = 100 and n = 1000, the heavy-
  tailed line) the floor selects in exactly the rounds the v8 peel did (1 to 5). In
  d = 128 with a Gaussian core and m isolated outliers at 1.5 to 3 sqrt(d), KMeans'
  first fit already splits the core (usable at round 0 on 39 of 40 seeds, 1 refit
  on the other). With a TIGHT core (sigma 0.01) the v8 peel removes one outlier per
  round: m = 13 needs 6 to 11 refits, m = 20 12 to 18 (7 of 8 seeds past v8's bound
  of 12), m = 50 18 to 33, and 50 outliers at strictly increasing radii 36 to 50;
  the floor selects in 4, 5, 6 and 6 refits on every seed. At n = 2000, d = 768,
  m = 50 (tight) the v8 peel needed 23 to 30 refits (1.15 to 1.60 s of KMeans); the
  unsampled floor needs 6 (0.34 to 0.37 s, worst refit 121 ms), timings over the four
  runs of README item 22. A tail cut that
  removes only members at least as far as the isolated group was measured and
  rejected: KMeans' best-SSE fit isolates the FARTHEST point, so the threshold
  removes nothing more (item 18, mode "tail").
  What transfers from the pure-algorithm probe: the INVALID bit depends only on
  child sizes and transfers exactly; its current partition is the lone bootstrapped
  cluster's real one (the first cluster's centroid is the first inserted vector,
  Insert.java:429-432) up to primary enumeration order, so KEEP_CURRENT versus
  ACCEPT is pinned by Go goldens over Java-ordered inputs, not by the probe. The
  peel moves no vector against nearest-centroid ownership (removed members simply
  join their nearer child) and its children pass the same minChildFraction gate as
  any Java split, so it adds no oscillation risk beyond Java's own splits. A size-
  penalised KMeans cannot do this: at every lambda from 0.5 to 16 the outlier's
  squared distance swamps the overflow penalty and the split stays INVALID (oracle
  item 9), and any fixed lambda is scale-dependent. Only the 1->2 candidate is
  peeled: a 2->3 candidate exists only with a neighbour, and Java's ordinary
  selection covers it. A lone cluster evaluates a single 1->2 candidate (its 2->3
  classification is null, SplitMergeTask.java:339-355), so the added cost is at
  most floor(log2(n - 1)) KMeans k=2 fits over at most n vectors, each followed by
  the assignment and evaluation passes over P and the floor's distance sort over M.
  Refit r draws from the r-th split of the peel RNG taken at entry; a Go golden pins
  Go's own draws and outcomes (the target has no draw sequence here because it
  throws). Balance: the selected partition is a KMeans k=2 fit like the target's
  own. On the TIGHT shapes its smaller child is 0.33 to 0.49 of n at d = 768 to 4096
  (item 23); on the corner the v13 bound admits (n = 1001 at d = 2048 and 2175,
  n = 2000 at d = 980, tight and spread, two seeds each: eleven peel rows and one row
  where the target's own fit is usable at round 0) 0.286 to 0.480, and just past it
  (v12's corner, d = 2775 and n = 2000 at d = 1250) 0.422 to 0.495 (items 26 and 29). It can
  be far lower on a shape admission refuses: the spread shape at n = 2000, d = 4096,
  seed 1, selects [1726 274], 0.137 (item 23). Its larger child can still exceed
  primaryClusterMax (for
  example 1337 of 2000), in which case it re-arms a split exactly as a child of any
  target split does. That cascade terminates: a selected partition is not INVALID,
  so each child holds at least minChildFraction * n > 0 primaries and every child
  is strictly smaller than its parent.

  Step 2, terminal reconcile, when the peel stops without a usable partition. The
  population that reaches it is exactly: a mass KMeans cannot separate
  (coordinate-identical vectors whose stored bytes differ; identical BYTES above
  collapseMinDuplicates, which is always below primaryClusterMax, take Java's
  collapse route first, measured in oracle item 12); a peel that trims M below two
  members without a usable partition; a split whose 1->2 candidate is n<k (fewer
  than two live primaries) and whose 2->3 candidate is absent, n<k or INVALID; a
  peel that runs all floor(log2(n - 1)) refits without a usable partition; and a
  peel that admission refuses (W > B). There is no clock exit: admission reads only
  n and d, so ClusterUnsplittableError means "no usable partition under the rule,
  or a peel the rule does not admit", and which of the two is recorded on the
  error (a field, pinned by the fixtures below).
  Go reconciles the target in place and keeps its topology:
  - Serializably read all of the target's references (primaries AND replicas;
    the cleanup that fed the candidates discards replicas, so it cannot identify
    stale replicas) and each referenced PK's current metadata; remove references
    whose UUID no longer matches (collapsed representatives are kept, as in Java).
  - Recompute RunningStats from identity by adding each surviving primary's
    distance to the target centroid in reference-key order, in the task's working
    (transformed) coordinates with the task's DistanceEstimator (the one KMeans,
    the evaluator and insert statistics use); its n IS the reconciled primary
    count (ClusterMetadata.java:88-89). Recompute numPrimaryUnderreplicatedVectors
    and numReplicatedVectors from the survivors. With at least one survivor,
    maxDistanceEver is max(stored, recomputed): the stored maximum is never
    lowered. With no survivor the stats are the identity (n 0, maxDistanceEver
    -infinity), the only empty state a target writer produces (RunningStats.java:
    45-46, 79-81). lifetimePeakPrimaryCount is unchanged.
  - Clear SPLIT_MERGE, then apply the normal peak-relative merge-eligibility rule
    to the reconciled count.
  The metadata rewrite is ONE helper shared with CollapseTask's rewrite (:412-424),
  not a copy. The helper takes the new stats and states as inputs and writes them
  verbatim; the max(stored, recomputed) rule lives in this reconcile caller only,
  because CollapseTask rebuilds its stats from identity (:239-291) and its written
  maximum can fall, and that Java collapse byte behaviour must stay unchanged (a
  JVM fixture pins a collapse whose maximum falls). A Go golden pins the task-RNG
  draws that follow a terminal reconcile. Removing stale references LOWERS the
  primary count and is real relief, so it counts as progress for the capacity
  hand-off; a cluster whose count does not fall is unrelievable through the
  progress rule.

  The residue's bound is inside the reconcile. After the survivors are counted
  and BEFORE any reconcile write, if the reconciled primary count exceeds
  primaryClusterHardMax the task fails with ClusterUnsplittableError instead of
  clearing SPLIT_MERGE. That is a Go-only error type carrying index, grouping
  prefix, cluster, reconciled count, limit and cause (no usable partition under
  the rule, or a peel the rule does not admit); it never matches the insert cap's
  capacity type (the Go ClusterCapacityExceededException) under errors.As and is
  never converted to the record layer's VectorIndexClusterTooLarge error, because
  its remedy is not a merge: the target throws NoSuchElementException (:397) or
  IllegalArgumentException (KMeans.java:136) there and passes it through
  unconverted (GuardiannVectorIndexEngine.java:140-164 converts only the insert
  cap's). Classification by every retry owner: it poisons (section 1, a task-body
  error after the removal is buffered); the build-range retry and the queue-replay
  iterator treat it as terminal (not the capacity branch, not retried); the
  capacity hand-off never keys on it (section 5); the merger aborts its session on
  it, as on any non-FDB, non-timeout error (indexing_merger.go:93-105, matching
  IndexingMerger.java:189-203); db.Run returns it without a retry, because it is not
  a retryable FDB error; and a user's back-pressure loop that matches the capacity
  type does not match it. A reconciled count at or below the
  hard cap clears as described. Reachability: the hard cap is checked only at
  Insert.java:335, so the count exceeds it wherever primaries arrive without that
  check: inline inserts routed to the cluster while its re-armed split waited, and,
  in deferred mode too, split re-homing into a neighbour (SplitMergeTask.java:
  621-651), merges and reassigns. So the reconcile's read is bounded by the
  cluster's actual count, which no single cap bounds. It is the target's own
  cleanup read (every reference and each referenced primary's per-PK metadata,
  which the target's task performs before it throws) PLUS the replicas' per-PK
  metadata, which the target's cleanup drops before its metadata fetch
  (Primitives.java:1544, 1552-1560); those extra serializable reads add read
  conflicts on the replica PKs' metadata keys that the target's task does not have,
  declared under (h). Its effect on an unsplittable
  cluster: while the reconciled count is at or below the hard cap, each re-armed
  split (Primitives.java:1161-1165, written by the insert that grows the cluster,
  exactly as in the target) is evaluated, peeled and reconciled; once a reconcile
  finds the count above the cap, the task fails and stays at the head of the
  partition's queue, and every later inline insert and inline delete in the
  partition fails, exactly as the target's do from its FIRST failed split on
  (measured, oracle items 8, 10, 12 and 14). Relief differs from the target's in
  one declared respect: the target clears its flag only as a false alarm at
  primaries <= primaryClusterMax (SplitMergeTask.java:180-181), while Go's reconcile
  clears at <= primaryClusterHardMax; below that, deferred-mode deletes (no inline
  task) or deleteWhere bring the count down, after which the next execution
  reconciles and clears. Cost and frequency in inline mode: a reconciled cluster
  between max and hard max re-arms on every insert into it (Primitives.java:1161-
  1163), so each such insert pays the target's own evaluation, at most floor(log2(n
  - 1)) admitted refits and the reconcile's serializable read of all references and
  their per-PK metadata, inside the user's transaction. What happens past the work the rule admits is decided by the transaction's
  retry owner, and v11 makes every owner Go runs GuardiANN work under bound its
  attempts as the target's does (section 5, "Attempt bounds of the transaction
  owners"): FDBDatabase.Run, the runner and the merger's per-attempt runner stop
  after maxAttempts (10 by default) retryable failures and return the last error,
  and the merger's session then follows the target's failure schedule
  (IndexingMerger.java:78, :125-128 and handleFailure). The peel only runs where
  the target's task throws, so the two failure outcomes it can add are Go-only
  failures in place of the target's :397 or KMeans.java:136 throw, never a failure
  where the target succeeds:
  - transaction_too_old (1007) when the target's own work plus an admitted peel
    (estimated at 0.8 to 3.4 s at W = B over the measured JVM refit rates, section
    above) plus the
    reconcile's or the materialisation's reads exceed FDB's 5 s limit. 1007 is
    retryable: FDBDatabase.Run and the runner retry it up to their attempt bound and
    return it (an inline task inside a SQL autocommit statement makes one attempt, as
    the target's relational layer does, so the statement fails at once; under Run the
    user waits up to ten attempts of about 5 s each plus the ported delays); the merger's handleFailure halves mergesLimit or the time quota and
    retries (indexing_merger.go handleFailure, IndexingMerger.java:246-258), which
    cannot help because the first task always runs whatever the quota
    (Primitives.java:991), so the session gives up at the quota floor or after
    failureCountLimit (1000) failures. The task stays at the head of its queue, as
    the target's throwing task does.
  - transaction_too_large (2101) when materialising the partition the peel selected
    exceeds FDB's 10 MB limit: every rewritten reference carries its full encoded
    vector (StorageAdapter.java:405-419; with useRaBitQ false, the default,
    Config.java:167, the encoding is the input element type), so 2000 references
    at d = 4096 are 16 MB in half precision and 32 MB in single, over the limit in
    both engines for any split of that cluster. Within admission (n = 2000 at
    d <= 980, n = 1001 at d <= 2175) half precision stays under 10 MB with its
    replicas (at most 2000 * 1960 B = 3.9 MB, or 1001 * 4350 B = 4.4 MB, plus at most
    replicatedClusterMaxWrites = 300 replica writes, Config.java:155, 0.6 MB and
    1.3 MB); SINGLE precision is 7.8 MB and 8.7 MB of vector bytes, 9.0 MB and 11.3 MB
    with the replica bound, so an admitted single-precision peel near the n = 1001
    bound that also writes most of its replica allowance fails with 2101
    deterministically. Whether one at n = 2000 fits is NOT established: its 9.0 MB is
    vector and replica bytes only, against a limit of 10,000,000 bytes that keys, clears
    and conflict ranges also count toward, so it is a hypothesis the phase-3
    materialisation fixture measures (the transaction's approximate size at commit,
    printed beside the computed volume); either outcome takes this paragraph's path. 2101 is not retryable: Run and the
    runner return it at once; the merger's handleFailure treats it as it treats
    1007. At the peel sites it is Go-only, because the target never materialises
    there (it threw); for the target's own splits it happens in both engines.
  The peel RNG, its one split at entry and refit r's use of the r-th split of it
  are as stated under Entry above.
  Acceptance fixtures (Go, real FDB, default KMeans knobs). They assert no duration of
  their own, but FDB's 5 s transaction window is a time limit, so each split fixture
  also asserts, through the attempt observer, that its split commits in exactly ONE
  attempt; a 1007 hidden behind a retry would otherwise pass. Go's peel is timed at
  each fixture's shape under the performance criterion above (at least two runs, load
  recorded, the whole peel at most 2.5 s), which is what makes the one-attempt assertion
  stable rather than lucky (v14 wrote "no wall-clock assertion" and left the window
  unpriced).
  Each states its encoding and its computed materialisation write volume (references
  carry the encoded vector; replicas add at most replicatedClusterMaxWrites = 300
  replica writes of the same size, Config.java:155):
  - the tight 50-outlier cluster at n = 2000, d = 768, HALF precision, useRaBitQ
    false: 2000 * 1536 B = 3.1 MB of vector bytes, 3.5 MB with the replica bound,
    admitted (W = 1.5 * 10^7), split by an inline insert and by a deferred drain in
    the refit count its Go golden pins;
  - the same generator at n = 1001, d = 2048, HALF, useRaBitQ false: 1001 * 4096 B
    = 4.1 MB, 5.3 MB with the replica bound, admitted (W = 1.8 * 10^7), split by the
    deferred drain;
  - the same generator at n = 2000, d = 4096, HALF: refused by admission (W = 8.2 *
    10^7), so the drain reconciles with zero refits and, the count being at the hard
    cap, clears; one primary more and it fails with ClusterUnsplittableError whose
    cause field says "not admitted". No split of this cluster fits in a transaction
    in either engine, which is why no fixture splits it. The reconcile's read here is
    the target's own cleanup read of every reference and each referenced primary's
    metadata (at least 16 MB of references at this shape) plus the replicas'
    metadata; the fixture prints its duration (never asserted), and the reconcile is
    one transaction whose 1007, if the read cannot finish, takes the classification
    above;
  - the n = 2000 fixture with I = 16 (twice the default iterations): its W doubles to
    3.1 * 10^7 and admission refuses it, which pins the knob factor;
  - the d = 4096 fixture with I = 1, R = 0: refused, W staying 8.2 * 10^7 under the
    floor of 32 (v12's factor without the floor gave 2.6 * 10^6 and admitted it), which
    pins the floor.
  The goldens record the admission decision and each refit's mass, so lowering B
  below the d = 2048 fixture's W, removing the admission check, dropping the knob
  factor, or dropping its floor reddens a fixture (the four mutations are part of the
  implementation's evidence). The stride-sampled refit of v10 stays withdrawn, and the reason is not a
  balance measurement: on the shapes admission refuses (the tight core at d = 4096)
  it left the smaller child at 243 and 317 of 2000 against the INVALID line of 200,
  but at the admitted d = 768 and 1536 its balance (0.350 to 0.462) and the unsampled
  fit's (0.401 to 0.489) do not tell the two apart. It is withdrawn because the
  unsampled refit is exactly the target's own full KMeans fit, and the admission
  bound makes the speed sampling bought unnecessary. The v7 insert-side bound stays withdrawn: its key (count above the
  hard cap with neither SPLIT_MERGE nor COLLAPSE) is also reached by the target's
  own flow (a collapse route sets {COLLAPSE}, SplitMergeTask.java:389-394; REASSIGN
  is added while COLLAPSE is set, Primitives.java:1222-1249 with
  ClusterMetadata.java:189-190; CollapseTask clears only COLLAPSE, :415-416; the
  paired bounce skips a cluster with REASSIGN, BounceTask.java:294-300; and
  ReassignTask then writes no states, :730-735), and the insert it refused was the
  one that would re-arm the split. Go keeps the target's insert path there
  unchanged. Retained JVM fixtures: the target shapes above in both modes
  (measured), the stale-inflated cluster at KMeans.java:136, the empty merge core
  and the identical-coordinate mass. Go fixtures: the peel (every exit: selected,
  undersizedChildHoldsNoMass, massBelowTwo) and the reconcile in both maintenance
  modes; ClusterUnsplittableError from an inline insert, an inline delete and a
  merge drain, each poisoning and each NOT matching the capacity type; a deferred
  merge that pushes a neighbour above the hard cap, after which the neighbour's
  split reconciles and fails; an inline residue driven past the hard cap until its
  task fails, then relieved by deferred deletes; the collapse -> REASSIGN ->
  reassign path staged above the hard cap in inline mode, where every Go insert is
  accepted and the next one re-arms the split exactly as the target's does; and
  replays of every healthy inline golden (oracle items 8, 10, 12 and 14,
  including the identical-mass inline row) with zero Go-only failures.
* A split/merge whose FINAL ownership leaves a NEW cluster with zero primaries.
  Java scores the KMeans assignment (SplitMergeTask.java:1107-1111) but then
  re-homes every primary against the new AND neighbouring centroids (:621-651), so
  a KMeans child can end with no primaries, and writeClusterMetadataAndEnqueue-
  Tasks asserts primaries > 0 for every map entry (:909); the task fails the same
  way forever. Go decides it in two passes before replaceCentroidsInHnsw. Pass 1
  computes every primary's final home (nearest[0]). New clusters that are nobody's
  home are dropped: no centroid insert, no metadata, no references. Pass 2 rebuilds
  standardDeviationsMap from the ORIGINAL map's stats (SplitMergeTask.java:615-624)
  before re-merging the stats updates, so nothing is counted twice, then recomputes
  computeNearestClusters and replica selection over the map WITHOUT the dropped
  clusters, so a dropped cluster can neither receive nor occlude replicas; homes
  are unchanged because a dropped cluster was nobody's nearest. The cause set passed
  to metadata updates is ALL minted ids, so neighbours are force-reassigned exactly
  as after a normal split (this matters when every new cluster is dropped: the core
  is dissolved into its neighbours as underreplicated primaries, and the forced
  reassign is what repairs their replication, ReassignTask.java:570-573). Bounce
  targets are the surviving new ids; with none, no bounce is enqueued and the
  dependent tasks stand alone. Java throws in every case this changes, so there is
  no Java draw sequence to align with: the minted ids are still drawn as Java draws
  them, per-cluster task-id draws happen only for clusters actually written, and a
  Go golden pins the resulting draw sequence.
* An EXISTING neighbour with zero final primaries (an undersized cluster whose
  merge is still pending, never filtered by classifyClusters, AbstractDeferredTask.
  java:590-636) skips only the positivity assertion. Everything else follows
  Java's normal path in full, including replica counts it receives and the REASSIGN
  state it may gain; its primary count stays 0 and its lifetime peak is unchanged.
* Empty merge core ("empty" means zero physical PRIMARY references; a cluster can
  still hold replicas). Treat it as a normal merge whose product keeps the lowest-
  packed-UUID core centroid instead of minting one: every core cluster's replica
  references are pruned exactly as Java prunes replicas when it dissolves a core
  (assignPrimaryVectorReferences considers primaries only), the other core
  centroids/references/metadata are deleted, and the kept cluster's metadata is
  reset to identity stats, zero underreplicated and replicated counts and cleared
  states, keeping its own lifetime peak. The core ids are the cause set, so
  neighbours are force-reassigned exactly as after a normal merge, which repairs
  owners that lost replicas. The kept cluster then goes through the ordinary
  undersized-merge rule (Primitives.enqueueMergeTaskIfUndersizedMaybe, including
  the MULTIPLE-centroid cardinality guard), so if other centroids remain a new
  merge task is enqueued for it and it merges into a neighbour; nothing is left
  undersized and unarmed. Termination holds because every empty-core merge
  deletes at least one centroid. When the merge threshold max(primaryClusterMin,
  floor(mergeMaxEverFraction*peak)) is above 0, two disjoint empty pairs end, after
  their merges and the re-armed merges, in no empty cluster unless the whole index
  is empty. When it is 0 (primaryClusterMin 0 with a low peak) an empty cluster is
  not undersized and stays, exactly as in the target (oracle item 4 shows such a
  cluster persisting as {0 0 2}). Do not
  call KMeans on zero vectors or mint an arbitrary zero centroid. Nonempty external
  clusters remain unchanged; repeated empty merges terminate at one retained empty
  cluster when the whole index is empty. A merge with n >= 1 always has the
  feasible 2->1 candidate.
* All of the above: task consumption and count notifications still occur exactly
  once, obsolete follow-ups become ordinary counted no-ops, and valid same-byte/
  collapsed members stay out of every empty classification. JVM fixtures build each
  shape (core group next to an outside centroid; empty neighbour with pending merge;
  delete-all->drain->reinsert; vanished neighbour; n<k; distant small sub-group;
  rollback) for split and merge and record the target outcome; Go fixtures prove
  termination, count/peak/state rules, the forced repair, and exactly-once task
  accounting, exposing any target failure rather than weakening Go invariants.

Java VectorId HashMap traversal reaches KMeans before score ties and is observable
through seeding and reduction order. Port that boundary with Java-compatible
VectorId hash spreading/bucket traversal, resize order and collision/tree-bin
behavior, not Go map iteration or an invented PK sort. Limit the compatibility
container to this algorithmic boundary. Retained oracle fixtures expose the
UNMODIFIED Java cleaned input order, including resize/collision cases, then compare
Go input order and KMeans intermediate IEEE values. Fixed ordered inputs, RNG,
codec/signature bytes, statistics and evaluator results require exact scalar
agreement. Go computes the hash itself:
VectorId.hashCode = 31 * Arrays.hashCode(primaryKey.pack()) + uuid.hashCode(),
where Arrays.hashCode(byte[]) folds h = 31*h + signedByte from h = 1,
UUID.hashCode = (int)(hilo >>> 32) ^ (int)hilo with hilo = msb ^ lsb, and HashMap
spreads h ^ (h >>> 16). fdb-java 7.1.26's Tuple.hashCode is Arrays.hashCode of
packMaybeVersionstamp() (confirmed with javap), and on the pinned remotejdk_21
(21.0.9) a two-component record hashes as 31*h(a) + h(b) (confirmed with
jshell). The JLS leaves record hashCode unspecified, so the retained oracle
fixture validates this formula against real VectorId instances and asserts at
runtime that the JDK major version is 21 and fdb-java's implementation version
is 7.1.26 (fdb-java is transitive; classpath.json records it). Candidate selection uses a
deterministic Go rule instead of IdentityHashMap order: walk candidates in Java's
construction order (1->2 before 2->3; 2->1 before 3->2) and replace the current
best only on a strictly better verdict, then a strictly higher score, so ties
keep the earlier candidate and the no-usable-candidate merge fallback remains
2->1. Java's identity-map enumeration can pick either tied candidate across JVM
runs, so equal-score choices receive invariant-based topology acceptance, not a
false whole-engine deterministic-byte claim. Preserve all externally stored encodings
and cross-writer readability, and never normalize oracle ordering silently.

Require Java-shaped unit/fuzz tests for parsers/codecs/RNG/statistics/clustering,
real FDB tests for every mutation/lifecycle/error boundary, and live JVM tests
using actual production classes (package-local adapters where needed). Compare
persisted deterministic bytes exactly; separate numerical tolerances from wire
identity. Carry IEEE bits rather than JSON NaN coercion. Register new files with
Gazelle and verify actual Bazel execution; mutation-check safety guards.

The completion matrix in the research reports includes: both-direction covering
rewrites and signatures/tasks; real split/reassign/collapse/bounce/merge transitions;
nonempty structure and replica invariants; stale/underfill/ordered results; sparse
counts and two-claim/delete races; negative-count committed disable; hard cap
relieved by real merge; callback identity and rollback on real FDB conflict/lost
session; final queued build batch actually produces and consumes GuardiANN tasks.
No fake maintainer or HNSW callback-only test substitutes for that evidence.

Run just test, actual race and scoped fuzz, and uncached final populations on a
frozen source tree. Executor/plan changes additionally require same-filesystem,
exact-SHA n>=2-per-side1M stress evidence. Preserve SPFresh paper invariants and
run its existing regression coverage; no recall-threshold relaxation. Actual
Graefe, Torvalds, independent storage/wire and SPFresh design ACKs precede
implementation; the same lenses confirm final implementation and any final delta.
No commit, publication, CI trigger or PR change is authorized by this design.
