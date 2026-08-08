# RFC-216 — `LIKE 'prefix%'` has no index access path

Item: TODO.md CQ-33 · Area: Cascades planner — access-path selection

Status: **no producer is designed here, deliberately.** An unimplemented contract
has no test that can fail, and the contracts earlier drafts stated turned out,
twice running, to be satisfiable by a rule that never fires; whoever writes the
code re-derives the design with the code in front of them. Recorded instead — the
measured defect, the matcher fact that constrains any future design, and the
blockers, each labelled with how it was established.

## 1. The defect — MEASURED, pinned

`WHERE s LIKE 'prefix%'` full-scans an indexed STRING column. Pinned by
`TestLikePrefix_IsNotSargable_AndTheCoveringStampIsLost`
(`pkg/relational/core/embedded/like_prefix_not_sargable_test.go`), planning via
`embedded.PlanQueryForTest` against `CREATE INDEX idx_status ON t2 (status)`:

```
SELECT id FROM t2 WHERE status LIKE 'act%'   ->  Project([ID#0], PredicatesFilter(Scan(T2), [1 preds]))
SELECT id FROM t2 WHERE status LIKE '%act'   ->  Project([ID#0], PredicatesFilter(Scan(T2), [1 preds]))
SELECT id FROM t2 WHERE status =    'active' ->  Project([ID#0], IndexScan(IDX_STATUS, [=] COVERING))
```

The `=` control makes the two full scans a fact about the comparison type rather
than about the schema.

## 2. Why — INSPECTION, not a test

This section is call sites, read. Nothing in it can go red once it stops being
true. `predicates.ComparisonLike` is admitted by neither `isSargableComparisonForMatch`
(`match_max_match_map.go:67`) nor `isScanRangeCompatible`
(`scan_match_helpers.go:37`), so a LIKE conjunct cannot bind an index
placeholder. `Resolver.ResolveStartsWith` (`expr.go:1437`) builds the comparison
the scan machinery does accept, and inspection found no production caller: the
resolver's `LikePredicateContext` arm (`walk.go:1869`) routes to
`ResolveLikeWithEscape` unconditionally.

## 3. No LIKE pattern yields a tight range — MEASURED, pinned

`%` translates to `.*` (`PatternForLikeValue.java:63`) inside a `^…$` wrap
(`:116`) compiled with no flags and so no DOTALL (`LikeOperatorValue.java:97-98`),
so it cannot cross a Java line terminator. A byte-prefix range is a strict
SUPERSET of the predicate: emit it if you like, but **the residual LIKE may never
be dropped.** A witness of that strictness needs an INTERNAL terminator — Java's `$` accepts
one FINAL one, which `values.LikeMatch` models by retrying against
`trimFinalLineTerminator`, so `LikeMatch("abc%", "abc\n")` is TRUE. That
tolerance also makes a wildcard-free LIKE not an equality: its match set is the
literal plus the literal followed by exactly one terminator, so an equality range
is a strict SUBSET and the failure direction inverts — it LOSES rows.

Pinned by `TestLikeMatch_NoPatternYieldsATightPrefixRange`
(`cascades/predicates/comparisons_test.go:1318`) — per pattern class, a witness
inside the byte-prefix range that the predicate rejects plus a matched control —
and by `values/like_match_test.go:130-143` (`$` never matches between `\r` and a
following `\n`, `:139`; one terminator is the limit, not a trailing run, `:143`).
`TestLikeMatch_ConstantPrefixBoundary` in that file locates a pattern's
constant-prefix boundary on the live matcher: two matched subjects begin with the
asserted prefix (refuting a longer one) and diverge at its far end (refuting a
shorter one, `""` included), and a third inside its range is rejected.

## 4. The scenario named for this optimization cannot detect it — OUTSTANDING

`yamsql/testdata/like_prefix_pushdown.yaml` is 336 lines whose header (`:3-16`)
states the pushdown exists and enumerates its bail-outs. Nothing in the tree
implements it (§2). It carries 41 `- query:` steps and
`grep -cE 'plan_contains|plan_not_contains'` returns 0: no step asserts plan
shape, and a correct pushdown returns the same rows by construction, so nothing
in the file separates its presence from its absence. The gap is dimensional, not
total — rows-only assertions do catch a WRONG optimization, and caught §5's
prototype on its first run (historical). `git diff origin/master...HEAD` is empty:
untouched by this PR, still outstanding.

## 5. Blockers

**Empty primary-key range — HISTORICAL.** A producer was prototyped on a working
copy; the primary-key prefix LIKE cases returned zero rows
(`expected [apple] [apricot], actual 0 rows`) with the STARTS_WITH bound present
and the range empty. It was deleted, not fixed, and **the loss point was never
diagnosed** — no claim is made here about which component drops it. Diagnosing it
is step one of any future attempt.

**Covering stamp lost through an intervening residual — MEASURED at HEAD,
pinned.** Reproducible with no producer:

```
SELECT id FROM t2 WHERE status > 'act'                        ->  IndexScan(IDX_STATUS, [<>] COVERING)
SELECT id FROM t2 WHERE status > 'act' AND status LIKE '%zz%' ->  PredicatesFilter(IndexScan(IDX_STATUS, [<>]), [1 preds])
```

Two PLANNING-phase rules stamp covering here, redundantly:
`ImplementProjectionRule` via `findIndexScanPlan`
(`rule_implement_projection.go:73`) and `MergeProjectionAndFetchRule` via a
direct type assertion (`rule_merge_projection_and_fetch.go:91`). The redundancy
is a disabling experiment, not a source reading — a subtest of §1's test on the
no-residual control, where disabling either alone leaves the plan byte-identical
and disabling both drops the stamp. Both then fail on one condition (inspection):
`PushFilterThroughFetchRule` puts a `RecordQueryPredicatesFilterPlan` under the
fetch, which neither descends through. Java avoids it structurally — coveringness
is a distinct class HOLDING the index plan as a field, not a flag on it
(`RecordQueryCoveringIndexPlan.java:74-78`), so an intervening `Filter` cannot
lose it and the Java rule yields `fetchPlan.getChild()` with no shape check at
all (`MergeProjectionAndFetchRule.java:62-78`). Go collapsed the two into a
`covering bool` on `RecordQueryIndexPlan`.
Preserving the stamp through the residual is a physical-plan change with its own
plan-movement blast radius.

**Logical-versus-physical type gate — INFERRED, UNVERIFIED.** The logical LIKE
gate (`expr.go:1415-1427`) admits Unknown, Null, Enum, Date and Timestamp
alongside String; the physical layer accepts STRING. Reading the two candidate
call sites suggests a disagreement demotes to residual rather than erroring at
execution — inference, not measurement, and the primary path already behaved
differently above. Separately, `ResolveStartsWith` has no LHS type gate while
`ResolveLikeWithEscape` raises 42804 on a numeric LHS: unreachable today, so
anything giving `ResolveStartsWith` a production caller adds the gate first.

**The prototype's other failures — HISTORICAL.** Registering it also broke
`TestRuleTypes_EveryProductionRuleHasDirectBehavioralTest`,
`TestPartitionSelect_ChainInterningBaseline` and `TestJavaCorpusRuns/files`,
none diagnosed before deletion.
