# RFC-216 — `LIKE 'prefix%'` has no index access path

Status: **DESIGN ONLY — nothing described here is implemented.**
Item: TODO.md CQ-33 · Area: Cascades planner — access-path selection

An implementation was prototyped on a working copy, measured, found to return
wrong rows, and deleted. This document is the residue: the measured defect, the
result that constrains any future design, and the blockers a future attempt has to
clear. `grep -rn LikeConstantPrefix pkg/` returns nothing — the rule, the prefix
extractor, its soundness fuzz and the `ResolveStartsWith` type gate are all
absent. RFC-217, retiring the dead `scanComparisonsToTupleRange` twin, ships in
the same PR and is independent of this one.

## 1. The defect, measured

`WHERE s LIKE 'prefix%'` on an indexed STRING column full-scans. Pinned by
`TestLikePrefix_IsNotSargable_AndTheCoveringStampIsLost`
(`pkg/relational/core/embedded/like_prefix_not_sargable_test.go`), planning
through `embedded.PlanQueryForTest` against `CREATE INDEX idx_status ON t2 (status)`:

```
SELECT id FROM t2 WHERE status LIKE 'act%'   ->  Project([ID#0], PredicatesFilter(Scan(T2), [1 preds]))
SELECT id FROM t2 WHERE status LIKE '%act'   ->  Project([ID#0], PredicatesFilter(Scan(T2), [1 preds]))
SELECT id FROM t2 WHERE status =    'active' ->  Project([ID#0], IndexScan(IDX_STATUS, [=] COVERING))
```

`predicates.ComparisonLike` is admitted by neither `isSargableComparisonForMatch`
(`match_max_match_map.go:67-81`) nor `isScanRangeCompatible`
(`scan_match_helpers.go:37-48`), so a LIKE conjunct cannot bind an index
placeholder. `Resolver.ResolveStartsWith` (`expr.go:1437-1452`) builds the
comparison the scan machinery does accept and has no production caller — the
resolver's `LikePredicateContext` arm (`walk.go:1869`) routes unconditionally to
`ResolveLikeWithEscape`. Over non-test `pkg/`, `ComparisonStartsWith` is produced
by `index_predicate_to_query.go:151` (feeding `index_expansion.go:143-155`, gated
on `*ValueIndexScanMatchCandidate`, so candidate-side), `ddl/generator_predicate.go`
(index-predicate DDL), and `ResolveStartsWith` itself. None derives a range from a
query's LIKE.

Downstream primitives do exist: the sargability admission above, the planning-time
physical veto (`physical_key_types.go:156-184`, applied by both
`match_candidate_index.go:899` and `primary_scan_match_candidate.go:344`), the
binder's STARTS_WITH tail arm (`scan_range_binding.go:929-965`) and the
`PREFIX_STRING` endpoints it feeds (`:1373-1379`). A producer is one missing piece;
§3 lists four more, two of which the prototype ran into on its first run.

### 1.1 The scenario named for this optimization cannot detect it

`yamsql/testdata/like_prefix_pushdown.yaml` is 336 lines whose header states the
pushdown exists and enumerates its bail-outs. No such pushdown is in the tree and
none has been merged; the discarded prototype is the closest anything came. All
~60 assertions are rows-only, and rows are identical with and without the
optimization, so the file passes against a tree with none of the behaviour it
describes. `grep -c plan_contains` returns 0 and `git diff master` on the file is
empty — untouched by this PR, still **OUTSTANDING**.

The gap is dimensional, not total: those rows-only assertions caught the
prototype's empty range on the first run (§3.1). It is a working semantics net that
cannot see plan shape. The file also contradicts itself on ESCAPE — line 11 scopes
the pushdown to patterns with "no other `%` or `_`, no ESCAPE", while lines 296-299
describe an ESCAPE pattern with an interior `_` *narrowing* to the prefix before
it. Both cannot be the contract; resolving it belongs with the implementation.

## 2. No LIKE pattern yields a tight range — in either direction

`%` compiles to `.*` under `Pattern.compile` with no DOTALL
(`values/like_match.go`, spec `PatternForLikeValue.java:99-116`), so it cannot
consume a Java line terminator; `_` is terminator-averse on the same grounds.
`LikeMatch("abc%","abcdef")` is TRUE and `LikeMatch("abc%","abc\ndef")` is FALSE,
while `HasPrefix("abc\ndef","abc")` is TRUE. A byte-prefix range is therefore a
**strict superset** of the predicate, for every pattern shape that could yield a
prefix. Emitting the range is fine; **dropping the residual LIKE is not.**

The counterexample needs an **INTERNAL** terminator, not merely a terminator:
Java's default-mode `$` accepts one FINAL one, which `values.LikeMatch` models by
retrying against `trimFinalLineTerminator`, so `LikeMatch("abc%","abc\n")` is TRUE.

**A wildcard-free LIKE is therefore not an equality either.** For a wildcard-free
literal `L` the match set is `L` plus `L` followed by exactly one final terminator:
`{L, L+"\r", L+"\r\n", L+NEL, L+LS, L+PS}`, and also `L+"\n"` **unless `L` ends in
`"\r"`** — Java's `$` never matches between a `\r` and a following `\n`. Six values
for a CR-ending literal, seven otherwise. Pinned at
`values/like_match_test.go:120-133`, including `LikeMatch("a\r","a\r\n") = false`
(:129) and `LikeMatch("a","a\n\n") = false` (:133 — one terminator is the limit,
not "a trailing run"). An equality range over
`L` is thus a strict SUBSET of the predicate, and the failure direction inverts: it
does not return extra rows, it **LOSES** them. The residual is mandatory in every
pattern shape; there is no wildcard-free carve-out. Same answer as CockroachDB's
`tight bool` (`LikeOp` in `idxconstraint`'s `makeSpansForSingleColumnDatum`): emit
the span, keep the filter.

`TestLikeMatch_NoPatternYieldsATightPrefixRange`
(`cascades/predicates/comparisons_test.go`) is the artifact, and it survives the
prototype's deletion because it asserts against the live matcher rather than a
helper. It quantifies over pattern classes — wildcard-free, trailing `%`, interior
`%`, `_`, an ESCAPE-escaped wildcard inside the prefix, leading `%` — and carries
per class both a witness inside the byte-prefix range that the predicate rejects
and a matched control, so no class can pass vacuously.

### 2.1 Consequence: augment, never rewrite

A future rule must ADD a conjunct implied by one already present
(`Select([…, LIKE(s,'abc%')])` → `Select([…, LIKE(s,'abc%'), STARTS_WITH(s,'abc')])`),
never replace it. The competing design — teach the matcher that LIKE is sargable
and derive the range there — is **measured unsafe**: adding
`predicates.ComparisonLike` to `isScanRangeCompatible` replans
`WHERE status LIKE '%act'` to `Project([ID#0], IndexScan(IDX_STATUS, [<>] COVERING))`
— an unbounded index scan with the LIKE **gone**, no residual filter at all. That
is the CQ-34/F11 hazard concretely: a comparison accepted as sargable enters
`matchedQueryPreds`, its compensation defaults to `NoPredicateCompensationNeeded`,
and the filter is dropped. Making it safe needs a `tight` flag threaded through the
whole compensation path. Rejected.

## 3. What blocks an implementation

Four blockers, none of them resolved — the prototype was removed, not fixed.

### 3.1 The primary-key path produced an EMPTY range

With the prototype registered, every primary-key prefix LIKE returned zero rows
(`expected [apple] [apricot], actual 0 rows`). The plan did change to
`PredicatesFilter(Scan(T, [<>]), [1 preds])`, so the STARTS_WITH bound; the range
was empty. **The loss point was never diagnosed**, and no claim is made here about
which component drops it — an earlier draft blamed the primary-scan binder while
also calling the root cause undiagnosed, and those cannot both stand. Diagnosing
this is step one of any future attempt.

### 3.2 The covering stamp is lost through an intervening residual

Reproducible at HEAD with no rule, pinned by the §1 test:

```
SELECT id FROM t2 WHERE status > 'act'                        ->  IndexScan(IDX_STATUS, [<>] COVERING)
SELECT id FROM t2 WHERE status > 'act' AND status LIKE '%zz%' ->  PredicatesFilter(IndexScan(IDX_STATUS, [<>]), [1 preds])
```

Two rules stamp covering for this shape, redundantly: `ImplementProjectionRule`
(EXPLORE, via `findIndexScanPlan`, `rule_implement_projection.go:66`) and
`MergeProjectionAndFetchRule` (PLANNING, via a direct type assertion,
`rule_merge_projection_and_fetch.go:91`). Measured: disabling either alone leaves
the plan unchanged; disabling both drops the stamp. Both fail on the same
structural condition — once `PushFilterThroughFetchRule` has pushed the residual
below the fetch, the fetch's inner is a `RecordQueryPredicatesFilterPlan`, which
neither the direct assertion nor `findIndexScanPlan` descends through. Java has no
such failure mode, structurally rather than by an extra branch: coveringness there
is a distinct class, `RecordQueryCoveringIndexPlan`, whose declaration does not
implement `RecordQueryPlanWithIndex` but HOLDS one as a field
(`RecordQueryCoveringIndexPlan.java:74-78`), and
`MergeProjectionAndFetchRule.onMatch` yields `fetchPlan.getChild()` with no shape
check (`MergeProjectionAndFetchRule.java:62-78`), leaving `Filter(CoveringIndexPlan)`.
Go collapsed the two into a `covering bool` on `RecordQueryIndexPlan`.

This becomes load-bearing for CQ-33 on secondary indexes. An unstamped index scan
satisfies `isSingularIndexScanWithFetch` (`planning_cost_model.go:1388-1398`),
which keys on `indexScanCount==1` even at `fetchCount==0`, so criterion #7
(`comparePrimaryScanVsIndexScan`, called at `:268-270`) puts it in the contested
tier (`:1239-1244`) against the primary scan's unencumbered one (`:1247-1251`) —
decided, index loses, and the scalar-cost rung that would have preferred the index
is never reached. A pure-LIKE query cannot be separated by criteria #2/#3/#4 —
the LIKE is residual on both sides by construction — so #7 decides. That step is
structural, not measured: the augmented form it would compare does not exist. In the second
EXPLAIN above, #3 separates them first on residual count, which is why the lost
stamp has stayed invisible. **The fix is to preserve the covering stamp through an
intervening residual** — the Go analogue of Java's `Filter(CoveringIndexPlan)` —
not to weaken criterion #7. That is a physical-plan change with its own
plan-movement blast radius, and it must land before CQ-33 is useful on secondary
indexes.

Two scope limits. (a) The mechanism holds under the Cascades default `PreferScan`;
`PreferIndex` penalises the lone primary scan instead (`:1222-1230`), a query with
extra predicates separates on an earlier criterion, and a genuinely non-covering
projection has no stamp to lose. (b) This particular comparison agrees with what
Java would decide, but criterion #7 **as a whole deliberately diverges** — Java's
rung is pair-restricted, is not transitive, and its adjudicated verdicts contain a
cycle (`DIVERGENCES.md:376`; `DELIBERATE DIVERGENCE` comment at
`planning_cost_model.go:1137-1154`). Do not read the agreement as a port claim.

### 3.3 The rule's type gate may disagree with the physical key type — UNVERIFIED

The prototype gated on the LOGICAL LHS type being `TypeCodeString`. The logical
LIKE gate (`expr.go:1415-1427`) also admits Unknown, Null, Enum, Date and
Timestamp; the physical layer accepts STRING. Both candidate paths apply the
planning-time veto (§1), which fails closed when the physical type is unknown, so a
disagreement most likely demotes the augmented form to residual rather than
producing a plan that errors at execution — but that is inference from reading two
call sites, not a measurement, and the primary path already behaved differently in
§3.1. **Verify before shipping; do not assume.**

Independently, `ResolveStartsWith` has no LHS type gate at all, while
`ResolveLikeWithEscape` raises 42804 on a numeric LHS (`expr.go:1415-1427`).
Unreachable today (no production caller). Anything making `ComparisonStartsWith` a
live production path must add the matching gate first, or
`STARTS_WITH(int_col,'abc')` degrades to a residual evaluating UNKNOWN per row and
silently returns zero rows where the LIKE raises.

### 3.4 Three undiagnosed test failures

Registering the prototype broke `TestRuleTypes_EveryProductionRuleHasDirectBehavioralTest`,
`TestPartitionSelect_ChainInterningBaseline` and `TestJavaCorpusRuns/files`.

## 4. Design notes for a future attempt (NOT implemented)

- **Placement.** PLANNING exploration, yielding an alternative member into the
  group rather than replacing the match. REWRITING has no index knowledge and its
  cost model's residual-count criterion discards the extra conjunct at the phase
  boundary; access-path selection happens in PLANNING.
- **Extraction.** A `values.LikeConstantPrefix(pattern, escape) string` beside
  `LikeMatch`, dispatching through the same `escapedLiteralAt` helper the matcher
  uses so the two cannot drift. One-way contract:
  `LikeMatch(p,s,e) => HasPrefix(s, LikeConstantPrefix(p,e))`. Sharing the helper
  makes the ESCAPE hazards fall out rather than need enumerating — Java's table has
  exactly two escape entries, `<esc>_` and `<esc>%`, everything else falling through
  to the ordinary rules.
- **Bail-outs.** Empty derived prefix, non-constant pattern, `NOT LIKE` (the
  complement of a prefix range is not contiguous). Unchanged by construction: NULL
  rows (`STARTS_WITH` is null-rejecting exactly as `LIKE` is,
  `null_rejecting_conjuncts.go:76`) and case sensitivity (LIKE compiles with no
  `CASE_INSENSITIVE` flag, so it is byte-exact and matches tuple byte order).
- The prototype's extractor was fuzzed for soundness on a working copy. Neither it
  nor the fuzz target is committed, so that measurement is not reproducible here.

## 5. Outstanding

1. Diagnose §3.1's empty primary-key range and pin it.
2. Land §3.2's covering-stamp fix, with its plan-movement review.
3. Verify §3.3's logical-versus-physical type question.
4. Soundness fuzz for the extractor against the live matcher, plus an exhaustive
   grid over an alphabet dense in `% _ ! \n`. A counterexample is a wrong-rows bug.
5. A reachability pin that goes RED when `ComparisonStartsWith` acquires a
   production producer reaching a primary-scan binder. §1's enumeration is grep and
   inspection — evidence, not a test, and it cannot fail. Calling inspection a pin
   is the defect class §1.1 documents.
6. `plan_contains` assertions in `like_prefix_pushdown.yaml`, a resolution of its
   line-11-versus-296 contradiction, and every §4 bail-out as a distinct case with
   `plan_not_contains: IndexScan`.
7. Residual retention: a row inside the prefix range that fails the LIKE — an
   INTERNAL-terminator subject; a final-terminator one matches and proves nothing
   (§2) — asserting both the IndexScan and the row set. This is the test that goes
   red if anyone later drops the LIKE.
8. plandiff corpus entries, and a 1M stress before/after recorded in TODO.md.
