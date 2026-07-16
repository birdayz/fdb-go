# RFC-179: Query-Engine Correctness Audit

**Status:** Draft — findings confirmed, fixes in progress
**Scope:** Cascades planner, optimizer, cost model, executor, aggregate/streaming, continuations
**Reference:** Java `fdb-record-layer` tag 4.12.11.0 (the spec)

## Summary

A deep correctness sweep of the query engine (8 subsystems, each audited against
the Java reference, every candidate adversarially verified — many with live
FDB-container repros) surfaced **27 confirmed defects**. They are not random: they
cluster into eight principle-violations, each a place where Go silently diverges
from a Java invariant on the **shared query surface** (or, for F3, on the **wire**).

Severity breakdown: 16 wrong-rows, 1 crash, 3 missing-error, 2 nondeterministic,
2 dead-code, 3 structural/other. Confidence: 26 high, 1 medium (F0).

The through-line: **Go's read-side extensions (in-memory sort, hash-join fast
path, JSON continuations, DISTINCT elision) were built without preserving the
invariant Java's architecture guarantees for free.** Java sorts only via FDB
tuple order; Java serializes all resume state as typed proto; Java derives
aggregate operators from the operand's static type; Java's memo Reference is one
equivalence class. Each Go shortcut broke one of these silently — green CI,
latent wrong rows, because the test that would have caught it probed a different
dimension (ASCII continuation keys, indexed sort, small hash inputs).

This RFC states the eight principles, maps each finding to the principle it
restores, and gives the Java-grounded fix + the regression that pins it. **Every
fix reads Java first and ports Java's mechanism** — no invented shortcuts.

---

## Root causes (why these exist)

These 27 defects are not 27 accidents. They are five recurring structural
patterns, each spawning a *family* of bugs. The meta-cause underneath all five:
**this is a 1:1 Java port done at the STRUCTURE level but not the INVARIANT
level.** The port faithfully copied Java's classes/rules/algorithm shapes, but
Java's correctness *also* rests on invariants invisible in the class structure —
the type system, the typed-proto continuations, the single value representation.
Copy the structure, skip the invariant, then add extensions on top → green on the
happy path, latent at every boundary. Test coverage is the *symptom* (dimensionally
thin — every bug lives in an unprobed axis; two even pin the wrong answer), not
the cause. The bugs were *written* because of these five patterns:

1. **Go-only fast paths / extensions that don't preserve Java's invariants.**
   The dominant pattern (5 of 8 themes). Java's Cascades has one way — sort via
   index order, NLJ via per-pair `Comparisons`, typed-proto continuations. Go
   bolted on shortcuts Java lacks: in-memory sort, hash-join fast path, JSON
   continuations, an in-memory `leftOuter` flag, DISTINCT-elision heuristics.
   Each extension owns the correctness Java's architecture gave for free, and each
   dropped it. → F1, F2, F8, F9/F13, F14, F15, F7.
2. **No single value-representation domain.** The same logical value has
   different Go representations by path: `float32` (covering) vs `float64` (base),
   `int32`→`int64` losing the INT type, `[]byte` vs `[16]byte`. Java has one
   descriptor-shaped typed representation. Go never canonicalizes at the read
   boundaries, so comparator/dedup/hash disagree across paths. → F5, F10, F14b, F17.
3. **Resume state in memory instead of in the continuation.** `innerHadMatch`,
   the OrElse decision, check-value handling live in cursor memory or JSON-lossy
   blobs, not typed proto. Java's continuation IS the source of truth for resume.
   "Cross-page resume ≠ single pass" is a whole class. → F1, F2, F4, F5, F24.
4. **Structural proxies instead of algebraic properties.** Graefe's rule:
   *properties derived from the expression tree, not imperative flags.* Go
   substitutes a structural shortcut that approximates the property until an edge
   diverges — guard on `len(Accessors)==1` not "root unpinned" (F0), plan equality
   by range-type not comparand (F21), winner by map-iteration not cost (F20). → F0, F20, F21.
5. **Type/index derivation not from the operand's static type.** Go decides
   index family / operator / error behavior late or by the widened runtime type,
   not statically like Java. F3 even crosses into the WIRE. → F3, F17, F18.

## Principles (the invariants being restored)

1. **Cross-page resume is behaviorally identical to a single pass.** Every bit of
   state that influences the next row must live in the continuation, typed and
   lossless — exactly what Java's proto continuations do. (Theme 1)
2. **Go's in-memory comparator/dedup/hash agree with FDB tuple order and with the
   promoting predicate semantics — on every type, or die loud.** A read-side
   extension may exceed Java's reach but must never disagree with Java's ordering
   or its own indexed plan. (Theme 2)
3. **Aggregate operators and index types are derived from the operand's static
   type, matching Java's SUM_I/SUM_L/AVG_I and permuted_min/max.** Overflow and
   result-type behavior follow from that, statically. (Theme 3)
4. **DISTINCT elision requires the projection to BE the unique key (injective),
   not merely to reference it.** (Theme 4)
5. **The multi-leg "correct-or-loud" guard keys on the semantic property (a root
   still leg-relative ⇔ unpinned), not a structural proxy; and a guard's comment
   states its true failure mode.** (Theme 5 — RFC-173 follow-through)
6. **A memo Reference is one semantic equivalence class (equality includes
   comparands); winner selection is deterministic and cost-aware; phase-boundary
   pruning preserves canonical forms via the cost model, not a compensating
   re-fire.** (Theme 6)
7. **Doesn't-fail-silently-in-Java → must-fail-loudly-in-Go.** Orphaned index
   entries, dropped sargable predicates, and leaf-name pushdown must error or
   scan correctly, never silently return wrong/superset rows. (Theme 7)
8. **One query path — no parallel pipelines.** Delete unreachable second
   implementations and dead state. (Theme 8)

---

## Theme 1 — Continuation state loss (paging correctness)

Java serializes all resume-relevant state into a typed proto continuation; on
resume the run is indistinguishable from a single pass. Go's continuations drop
state — in-memory flags never serialized, JSON coercing types — so any query
whose page boundary lands mid-group / mid-inner returns rows Java never would.

- **F4 [wrong-rows]** `continuation.go:138` — the streaming-aggregate group key is
  `string(tuple.Pack(keys))`, then JSON-encoded. `encoding/json` replaces
  invalid UTF-8 with U+FFFD, so any integer key ≥128/negative or **any float
  key** corrupts on resume → false group break → one group emitted as two rows
  with split aggregates. *Fix:* stop round-tripping packed bytes through JSON —
  carry the raw packed key (hex/base64 tag, as the UUID workaround already does
  for `[16]byte`, generalized to all bytes) or, Java-faithfully, carry the typed
  key vals and re-pack. *Pin:* `encode→decode` of `int64(200)`, `int64(-1)`,
  `float64(1.5)`, `[]byte{0xff}` round-trips byte-identical; SQL: GROUP BY with
  `EXECUTION_SCANNED_ROWS_LIMIT` forcing a mid-group break returns one row/group.
- **F5 [wrong-rows]** `continuation.go:193` — same JSON round-trip loses *types*:
  `[]byte`→base64 string, whole `float64`→`int64`, `int64>2^53` rounded, for both
  keyVals and MIN/MAX partial state. *Fix:* same as F4 — typed encode (proto or a
  type-tagged codec). Fix F4+F5 together. *Pin:* the three-loss round-trip test +
  removing the loose `Sprintf` tolerance in `TestAggregateContinuation_FloatMinMax`.
- **F1 [wrong-rows]** `executor_new_plans.go:641` — `executeDefaultOnEmpty`
  (LEFT-JOIN null-extension) has **no continuation wrapper**: it re-decides
  empty-vs-nonempty from scratch every resume. Go already ports Java's
  `OrElseCursor` (`cursor_combinators.go:163` `OrElseWithContinuation`, full
  UNDECIDED/USE_INNER/USE_OTHER state) but this path hand-rolls a first-row peek +
  `StartContinuation{}` (nil bytes). Consequences: (a) row-limit-1 paging never
  advances (re-emits the first matched row forever); (b) a mid-stream resume
  fabricates a spurious `(outer, NULL)` row. *Fix:* route through
  `OrElseWithContinuation` (child plan primary, default-row cursor alternative) —
  Java's exact mechanism. *Pin:* the two-part FDB paging test (non-advancing +
  fabricated-NULL).
- **F2 [wrong-rows]** `flat_map_cursor.go:251` — the Go-only `leftOuter` flag emits
  the null-extended row on `!innerHadMatch`, but `innerHadMatch` lives only in
  cursor memory (`FlatMapContinuation` has no had-match bit). A mid-inner page
  break after ≥1 match, resumed, re-inits it false → spurious `(outer, NULL)`.
  *Fix:* the correct end-state is F1's — LEFT OUTER lowers to
  `DefaultOnEmpty`+`OrElse` (whose continuation carries the state), retiring the
  in-memory `leftOuter` flag (TODO.md:1802 already slates it). *Pin:* the
  correlated-scalar-subquery-with-LIMIT resume test.
- **F24 [wrong-rows]** `flat_map_cursor.go:288` — on resume, an outer check-value
  mismatch **hard-errors** the query. Java (`FlatMapPipelinedCursor.java:206-219`)
  restarts the inner from scratch — "handles the outer record being deleted…";
  and the Cascades `RecordQueryFlatMapPlan` passes **no checker at all** (4-arg
  overload, `checker=null`). Wider than concurrency: a LEFT-OUTER page boundary on
  a null-inner emission mismatches deterministically with **no** modification.
  *Fix:* on mismatch, discard the stale inner continuation and restart the inner —
  Java's behavior; and stop writing `CheckValue` in the inner-exhausted
  advanced-outer branch. *Pin:* deterministic no-concurrency LEFT-OUTER paging +
  the delete-between-pages variant.

**Theme-1 principle:** resume = single pass. These five are one architectural
fix each spelling: *carry the state Java carries.*

---

## Theme 2 — In-memory comparator / dedup / hash fidelity

Go's in-memory sort, DISTINCT, and NLJ hash fast-path are extensions Java's
Cascades lacks (Java sorts via index order only). They are allowed — but they
must agree with FDB tuple order and with the promoting predicate semantics.

- **F9 ≡ F13 [wrong-rows]** `executor_new_plans.go:1061` — `compareValues` has no
  `[]byte` arm; a BYTES sort key falls to `fmt.Sprintf("%v")` decimal-list
  comparison (`{0x02}` sorts after `{0x0A}`). Disagrees with the *indexed* plan of
  the same query and with Java. Shared by `mergeSortCursor.isBetter` (UNION
  misorder + dedup miss). *Fix:* add `case []byte: bytes.Compare` (mirror the
  `[16]byte` arm) and make the residual `Sprintf` fallback **error loudly** (the
  pending Graefe kill-list obligation, TODO.md:151). *Pin:* `compareValues` unit +
  unindexed `ORDER BY bytes` FDB test.
- **F8 [wrong-rows]** `executor.go:1396` — `distinctKey` Sprintf-joins slots with
  an unescaped `|`; a value embedding `|B=string:` reproduces the column boundary
  → two different rows collapse. *Fix:* pack slots into an FDB tuple (as
  `cteDedupKeyer`/`mergeSortCursor.extractKey` already do) — structured equality,
  Java's `Set<Key.Evaluated>` semantics — covering NULL encoding too. *Pin:*
  white-box `distinctKey` collision + `SELECT DISTINCT a,b` FDB test (2 rows).
- **F10 [wrong-rows]** `executor.go:919` — covering-index rows carry raw `float32`
  for FLOAT columns; the base-scan path widens to `float64`. `compareValues` has
  no `float32` arm (→ lexical) and `distinctKey` %T-tags them distinctly →
  sort-by-string + dedup split across access paths. *Fix:* normalize
  `float32→float64` at the covering read boundary (next to `tupleElementToUUID`) +
  add a `float32` arm to `compareValues`. *Pin:* derived-table covering `ORDER BY
  f` + `DISTINCT f` over covering∪base FDB tests.
- **F14 [wrong-rows]** `streaming_cursors.go:890` — the NLJ hash index keys
  `map[any]` by raw Go dynamic type; `int64(5)` misses the `float64(5.0)` bucket →
  the promoting predicate re-check never runs → cross-type numeric equijoins
  silently drop **all** matches once inner ≥100 rows (the hash threshold). *Fix:*
  normalize hash keys to a promotion-stable form (mirror `predicates.cmpAny`
  widening) — or decline the fast path for cross-type numeric keys. *Pin:* the
  120-inner-row white-box + the 99-row linear control (size-dependence).
- **F15 [crash]** `streaming_cursors.go:883` — the same hash index panics on a
  `[]byte` join key (`hash of unhashable type []uint8`) at ≥100 inner rows. *Fix:*
  whitelist hashable-**and**-value-comparable key types before building the hash
  (bytes/message keys → decline to the linear path). *Pin:* 120-row BYTES-key
  white-box (panics today) + 99-row linear control.

- **F27 [wrong-rows, follow-on — surfaced fixing F14/F15]** `comparisons.go`
  (`cmpAny`) — cmpAny's `<`/`>`-based equality returns **EQUAL for NaN vs any
  float** (and NaN vs NaN), diverging from Java's `Double.equals` (NaN=NaN →
  TRUE, NaN=5.0 → FALSE). The NLJ-hash fix (F15/F14) pinned hash⇔linear
  *agreement* and declines NaN keys, so it does not depend on cmpAny's NaN
  behavior — but the linear-path semantics itself is wrong. *Fix:* give `cmpAny`
  IEEE/Java-faithful NaN equality. *Pin:* `cmpAny(NaN, NaN)`==equal,
  `cmpAny(NaN, 5.0)`==not-equal; a linear NLJ over NaN float keys matches Java.

- **F28 [wrong-rows, follow-on — surfaced gating C2]** `executor_new_plans.go`
  (`compareValues` float64 arm) / `comparisons.go` (`cmpAny`) — `-0.0` and `0.0`
  compare EQUAL, but FDB tuple encoding orders `-0.0 < 0.0` and Java
  `Double.compare` likewise. An in-memory sort/dedup thus disagrees with the
  indexed plan on the sign of zero. Clusters with F27 as the float-total-ordering
  fix (do both in the comparator + cmpAny at once). *Fix:* order `-0.0 < 0.0`
  (Java `Double.compare` / `math.Signbit`). *Pin:* `compareValues(-0.0, 0.0) < 0`
  + an FDB `ORDER BY` over a FLOAT column containing both zeros.

**Theme-2 principle:** the extension must agree with the indexed plan and the
predicate. A comparator that renders with `fmt`, splits by representation, or
panics is a bug even though Java "can't express" the query.

---

## Theme 3 — Aggregate semantics & index selection

Java derives aggregate operators and index types from the operand's static type.
Go widens everything to int64/float64 and picks the wrong index family.

- **F3 [wrong-rows + WIRE]** `builder.go:738` — plain SQL `MAX`/`MIN` are created
  as and served from `MIN_EVER`/`MAX_EVER` indexes. `_EVER` is monotone: deleting
  the extremum leaves a stale value. Java uses `PERMUTED_MIN`/`PERMUTED_MAX`
  (`NumericAggregationValue.getIndexTypeName`), whose maintainer tracks the true
  current extremum under deletes. **Also a wire/metadata divergence** — identical
  `CREATE INDEX … MAX` text yields `max_ever_long` in Go vs `permuted_max` in
  Java on a shared cluster (**hard line**). Go already *has* a
  `permutedMinMaxIndexMaintainer` — unused by DDL/candidate. A committed test
  (`delete_max_holder_ever_persists`) **pins the wrong answer**. *Fix:* DDL maps
  plain MAX/MIN → permuted; candidate builder matches permuted; keep the separate
  `MAX_EVER`/`MIN_EVER` SQL functions on the ever indexes. *Pin:* flip
  `delete_max_holder_ever_persists` to the Java answer + UPDATE/MIN mirrors +
  assert the created index Type is `permuted_max`. **Highest priority** (wire).
- **F6 [wrong-rows]** `streaming_cursors.go:535` — `AVG` over BIGINT accumulates in
  `float64` and divides, losing precision >2^53; the exact `int64` sum
  (`sumsI[i]`) sits unused. Java keeps an exact long sum, divides once. *Fix:*
  `AggAvg` uses `float64(sumsI[i])/count` when `allInt[i]`. *Pin:* white-box
  `{1<<53, 1, 1}` → `…994/3`, not `…992/3`.
- **F17 [missing-error]** `streaming_cursors.go:471` — `SUM`/`AVG` over INTEGER
  (int32) never raise Java's int32-range overflow (`SUM_I` = `Math.addExact(int)`).
  Go widens int32→int64 and only checks the int64 boundary → returns a BIGINT-range
  value where Java errors. *Fix:* pick the int32 operator at plan time from the
  operand's static INT type (mirror `NumericAggregationValue.encapsulate`). *Pin:*
  `SUM(v)` over INTEGER past 2^31 errors 22003; BIGINT control does not.
- **F18 [missing-error]** `streaming_cursors.go:481` — `MIN`/`MAX` over a
  non-numeric column is rejected only per-row on a non-NULL value; an empty/
  all-NULL column silently returns a NULL row. Java rejects at **plan time**
  (`Verify.verifyNotNull`, data-independent). *Fix:* operand-type gate when the
  translator builds the `AggMin`/`AggMax` spec; the runtime check becomes a
  backstop. *Pin:* extend `TestFDB_CascadesMinMaxStringRejected` with empty +
  all-NULL cases; fix the stale `min_max_string.yaml`.

**Theme-3 principle:** operator and index type follow from the operand's static
type. F3 is the one true wire divergence in this audit — fix it first.

---

## Theme 4 — DISTINCT elision soundness

- **F7 [wrong-rows]** `rule_implement_distinct_final.go:188` — `extractFieldNames`
  collects FieldValue names at **any depth**, so `SELECT DISTINCT id/3` sees the PK
  "covered" and drops the `RecordQueryDistinctPlan` → duplicate rows (confirmed:
  6 rows for `id/3` over 1..6 instead of 3). Java never treats `f(pk)` as
  distinct-by-construction. *Fix:* elision requires each projected value to **be** a
  bare FieldValue and the set to cover the unique key (top-level check, injective),
  not to merely reference key columns. *Pin:* `DISTINCT id/3` → 3 rows; guard
  `DISTINCT id` → 6 rows (bare-PK elision stays legal).

---

## Theme 5 — Ordinal-eval invariant completeness (RFC-173 follow-through)

- **F0 [wrong-rows, medium]** `values.go:866` — the multi-leg wrong-slot guard
  fires only when `SourceRelativeBaked()` is true, which requires
  `len(Accessors)==1`. A **fused** unpinned multi-accessor path (from
  `composeFieldOverField` / `withChildren` rebuild-fuse; `WithSuffix` inherits the
  inner's unpinned root) keeps a leg-relative root but reports
  `SourceRelativeBaked()==false`, so it **skips the guard** and reads a foreign
  leg's slot — silently NULL (unpinned default arm) or a wrong nested value. The
  single-accessor twin goes loud by design; the fused twin does not. *Fix:* the
  guard (values.go:866/:897) keys on **root unpinned** (`f.Resolved != nil &&
  !f.Resolved.FrontierPinned`), not `len(Accessors)==1`; audit the plan-time
  `SourceRelativeBaked()` call sites (esp. `replace.go:394`,
  `rule_select_merge.go:716`) for the same conflation. *Pin:* the fused-twin
  extension of `TestLegWindow_WrongSlotHazard` (values-level, revert-proven). This
  is in the just-merged RFC-173 code — **treat as RFC-173 completion.**
- **F16 ≡ F25 [structural]** `rule_partition_select.go:458` / `positional_merge.go`
  — A1 is **resolved**: `lowerComponentsAreSingletons` is **live** (dropping it REDs
  `TestFDB_OrderByGather/threeleg_{asc,desc}` at runtime). But the guard comment —
  *the one committed in 5780b37a0* — claims removal yields "a loud 0AF00
  mis-partition, not a silent wrong row." **Both wrong:** the mis-partition PLANS
  cleanly and fails at **runtime** in the executor (the RFC-173 ordinal tripwire
  converted the historical silent wrong-slot into a loud `RowEvalContext` error).
  *Fix:* correct the comment to state the true failure layer (runtime executor
  error via the ordinal tripwire), and add a **unit-scope** plan-shape test on the
  4-quantifier/3-component shape so unit CI also reds on removal (today only FDB
  scope catches it). The deeper F16 item — Go can't *implement* the bipartition
  Java plans (`positionalMergeCase` mis-wires source-relative refs) — is booked as
  a follow-on; the live guard keeps it unreachable and correct today.

---

## Theme 6 — Memo & winner determinism

- **F20 [nondeterministic]** `winner_lookup.go:40` — after the exact-key winner
  miss, the code ranges `ref.GetWinners()` (a **map**) and returns the first
  `Satisfies()` hit — randomized per range, and **cost-blind** (unlike the
  un-stamped fallback right below). Confirmed: 200 identical calls → 2 distinct
  winners. *Fix:* collect all satisfying stamped winners and select via `less`
  (with the plan-hash tie-break for stability) — Java's deterministic comparator.
  The tie-break must wrap **any** `less` a caller passes, not only the default
  `PlanningCostModelLess` (Graefe), so determinism holds under a custom
  comparator. *Pin:* the 500-call determinism + cheapest-winner white-box.
- **F21 [nondeterministic]** `index_scan.go:161` — `RecordQueryIndexPlan`
  equality/hash compares only the scan **range type**, ignoring comparands, so
  `IndexScan(idx,[=5]) == IndexScan(idx,[=7])`; the memo collapses them into one
  Reference (`memoizeLeaf`), and extraction can materialize the wrong-comparand
  scan. Same defect in `RecordQueryInJoinPlan` (excludes `inValues`). Java compares
  `scanParameters` fully. *Fix:* include comparands in equality/hash (keep
  `bindingName` excluded per RFC-164). *Pin:* plans-level `!p5.Equals(p7)` + memo
  distinct-Reference probe + pipeline UNION-ALL walk asserting both comparands
  survive.
- **F26 [structural]** `planning_cost_model.go:78` — `RewritingCostModelLess` is
  missing Java's first criterion (`outerJoinCount`); the REWRITING prune prefers
  the un-rewritten LEFT-OUTER select (1 select) over the canonical rewritten form
  (2 selects) — the exact inversion Java's comment warns of — destroying the
  canonical form at the phase boundary. Compensated by a **Go-only** re-fire of
  `RewriteOuterJoinRule` during PLANNING (Java's `PlanningRuleSet` has no such
  rule). *Fix:* add the outer-join criterion ahead of `selectCount` so the
  canonical form survives the prune; then remove/justify the PLANNING re-fire.
  Correct DIVERGENCES.md ("All 4 criteria ported" — Java has 5). *Pin:* cost-model
  unit (canonical preferred) + boundary walk (no LEFT-OUTER select survives). This
  is architectural — **Graefe-led.**

---

## Theme 7 — Error-path parity

- **F19 [missing-error]** `executor.go:843` — `indexFetchCursor` silently `continue`s
  past an orphaned index entry (base record gone). Java uses
  `IndexOrphanBehavior.ERROR` for query execution → `RecordCoreStorageException`
  with index/PK/key context. Go converts detectable store corruption into silently
  fewer rows. *Fix:* port `IndexOrphanBehavior.ERROR` on the query fetch path (a
  typed `RecordCoreStorageError`); the scrubber keeps its own behavior. *Pin:*
  white-box FDB corruption test (clear the record range, leave the index entry).
- **F11 [wrong-rows, latent]** `executor.go:743` — `STARTS_WITH` is planner-sargable
  and consumed into the scan range (residual filter removed), but
  `scanComparisonsToTupleRange`'s inequality switch has no `StartsWith` arm → the
  bound is never applied → unbounded scan with the predicate dropped. Not
  SQL-reachable today (no LIKE→STARTS_WITH rewrite) but the exported
  `expr.ResolveStartsWith` makes it concrete. *Fix:* add the `PREFIX_STRING` arm
  (`TupleRangePrefixString` already exists). *Pin:* white-box
  `scanComparisonsToTupleRange` → prefix range + FDB prefix-scan test.
- **F12 [wrong-rows, latent]** `match_candidate_index.go:338` —
  `PushValueThroughFetch` matches FieldValues by **leaf name only**, dropping the
  accessor chain, so a nested `addr.city` whose leaf collides with an indexed
  top-level `city` rewrites to a flat index read. Java matches the **whole value
  tree** (`semanticEquals`) and requires no residual correlation. Latent (no struct
  columns today) — becomes wrong rows the moment they land. *Fix:* full-tree match
  + reject if still correlated to the source alias. *Pin:* white-box
  `PushValueThroughFetch(nested)` must return `ok=false`.
- **F35 [wrong-rows]** `cascades_generator.go:GetMatchCandidates` — every index
  with a value root was offered as a VALUE-index scan candidate with no type
  filter, so an atomic-mutation/aggregate-only index (`count`, `count_not_null`,
  `count_updates`, `sum`, `max_ever_*`, `min_ever_*`, `max_ever_version`,
  `bitmap_value`) leaked into value-scan candidacy. A plain SQL `MAX`/`MIN` over a
  table whose only secondary index is a monotone `MAX_EVER`/`MIN_EVER` was served
  by scanning that index as a value source → after deleting the current
  max-holder, the query returned the STALE running extremum instead of recomputing
  the current one. Java splits this at `IndexMaintainerFactory.createMatchCandidates`:
  `AtomicMutationIndexMaintainerFactory`/`BitmapValueIndexMaintainerFactory` never
  call `expandValueIndexMatchCandidate` (their maintainers reject a `BY_VALUE` scan
  at runtime). *Fix:* new `Index.IsAtomicMutationIndex()` deny-list gates the
  value-candidate fallthrough, AFTER the aggregate (permuted MIN/MAX, COUNT/SUM)
  and vector candidate checks — deny-list, not allow-list, so VALUE/VERSION/RANK/
  permuted stay value-scannable exactly as in Java. A dropped atomic index leaves a
  plain MAX/MIN to fall back to a base-record `StreamingAgg` (correct current
  extremum). *Pin:* `TestFDB_AggregateIndex_StaleEverNotServedByValueScan` (DELETE
  the max-holder → MAX returns the current 100, not the stale 250; plan has no
  IndexScan/AggregateIndex over the `_EVER` index) + plan-harness fallback shape;
  F3 permuted verified unaffected. Revert-proven red→green.

---

## Theme 8 — Dead code (no parallel pipelines)

- **F22 [dead-code]** `streaming_cursors.go:92` — `aggregateCursor.pending` is never
  assigned; the emit-pending branch is unreachable. *Fix:* delete field + block.
- **F23 [dead-code]** `sort.go:25` — `RecordQuerySortPlan`/`executeSort`/
  `expressionSortFn`/`compareAny` are production-dead (no non-test constructor); an
  orphaned port of Java's legacy-planner plan. A second sort pipeline with a weaker
  comparator violates "no parallel pipelines." *Fix:* delete the plan + executor
  arms + switch cases; migrate tests to `RecordQueryInMemorySortPlan`. Correct
  DIVERGENCES.md.

---

## Execution order

Correctness-first, contained-first, wire-first. Each fix: **read Java → port →
revert-proven regression → full gate (Graefe/Torvalds/@claude/Codex) → commit.**
Fable subagents do the coding; Graefe gates every query-engine change.

### Systemic-first (root-cause order)

Several findings are not independent — they collapse into a handful of **systemic
chokepoints**, one per root-cause pattern. Fix the chokepoint and the dependent
findings largely fall out; the residual per-finding fixes are then small. So we
lead with the chokepoints:

- **S1 — one value-canonicalization boundary (Pattern 2).** A single
  `tupleElementToRowValue`-style normalizer that every read boundary (covering
  read, base read, aggregate-index key, index-entry decode) routes through,
  widening `float32→float64`, canonicalizing int/bytes/UUID representations. → collapses
  **F10, F14b, part of F5**, and prevents the next representation bug. *(Landed as
  part of C2: the covering-read normalizer + its reuse at the aggregate-index key.)*
- **S2 — one total, tuple-order-faithful comparator (Patterns 1+2).**
  `compareValues` has a typed arm for every row-domain type (agreeing with FDB
  tuple order) and the residual `fmt` fallback dies loud (F9b). → collapses
  **F8, F9/F13, F10-comparator**, and the `mergeSort`/DISTINCT paths that share it.
  *(Arms landed in C2; loud residual is F9b.)*
- **S3 — typed continuations end-to-end (Pattern 3).** Replace the JSON
  aggregate continuation with a typed codec (no U+FFFD / type loss) and route
  LEFT-OUTER/FlatMap resume through Java's `OrElse`/DefaultOnEmpty continuation
  state instead of in-memory flags. → collapses **F4, F5, F1, F2, F24**. The
  larger workstream — sequence F4/F5 (contained, typed agg codec) before
  F1/F2/F24 (the FlatMap/OrElse lowering).
- **S4 — property-derived guards, not structural proxies (Pattern 4).** Key each
  guard/selection on the algebraic property: root-unpinned (F0), full comparand
  equality (F21), cost-minimal deterministic winner (F20). Three sites, one
  discipline — a themed batch.
- **S5 — static-type-derived aggregate/index selection (Pattern 5).** Derive
  index family / operator / error from the operand's static type at plan time:
  permuted MIN/MAX (F3, WIRE), int32 `SUM_I` overflow (F17), MIN/MAX non-numeric
  plan-time reject (F18). Themed batch; F3 first (wire).

Residual (not part of a chokepoint): F7 (DISTINCT injective elision), F16/F25
(guard comment + unit pin), F19 (index-orphan ERROR), F11/F12 (STARTS_WITH range,
full-tree pushdown), F22/F23 (dead code), F26 (outerJoinCount, after S3's
F1/F2/F24), F27 (cmpAny NaN).

**F3 runs in parallel** (Graefe): the wire divergence is independent of items
1–11, so it need not wait its ordinal position. **F26 sequences after F1/F2/F24**
(the executor-side LEFT-OUTER lowering must land before the cost-model criterion
that preserves the canonical form). The deeper F16 follow-on (plan-time
bipartition support or plan-time rejection — the runtime tripwire is a weaker
guarantee than Java's plan-time behavior) stays tracked beyond the comment fix.

1. **F15** (crash, contained) — hash bytes-key panic
2. **F9/F13** (wrong-rows, tiny) — `compareValues []byte` arm + loud fallback
3. **F6** (wrong-rows, tiny) — AVG exact int sum
4. **F8** (wrong-rows, small) — `distinctKey` tuple-pack
5. **F20** (nondeterministic, small) — deterministic cost-aware winner
6. **F10** (wrong-rows, small) — covering float32 normalize + arm
7. **F14** (wrong-rows, small) — hash cross-type numeric keys
8. **F7** (wrong-rows, medium) — DISTINCT bare-key elision
9. **F16/F25** (comment + unit pin) — fix the false guard comment I committed
10. **F0** (wrong-rows, medium) — fused-path multi-leg guard (RFC-173 completion)
11. **F4/F5** (wrong-rows, medium) — typed aggregate continuation
12. **F3** (wrong-rows + WIRE, large) — permuted MIN/MAX index family
13. **F1/F2/F24** (wrong-rows, large) — FlatMap/DefaultOnEmpty continuation via OrElse
14. **F17/F18** (missing-error) — aggregate type gates
15. **F19** (missing-error) — index-orphan ERROR
16. **F21** (nondeterministic) — comparand-aware plan equality
17. **F11/F12** (latent wrong-rows) — STARTS_WITH range + full-tree pushdown
18. **F22/F23** (dead-code) — deletions
19. **F26** (architectural, Graefe-led) — outerJoinCount criterion

Refuted (recorded, no action): LegAwareRootOrdinal total-miss fallback (by
design), unpinned-baked-non-row whole-object return (handled), REWRITING
tie-break alias hashing (already normalized).

---

## Findings index (all findings + systemic mapping + status)

Every confirmed finding, the systemic chokepoint (S1–S5) or theme it belongs to,
and its landing status. F27/F28 surfaced during fixing; F9≡F13 and F16≡F25 are
one defect each found by two auditors.

| # | Severity | Systemic fix / theme | Status |
|---|---|---|---|
| F0 | wrong-rows | S4 (root-unpinned guard) | **DONE** |
| F1 | wrong-rows | S3 (OrElse continuation) | **DONE** |
| F2 | wrong-rows | S3 (retire leftOuter flag) | **DONE** |
| F3 | wrong-rows + **WIRE** | S5 (permuted MIN/MAX) | **DONE** |
| F4 | wrong-rows | S3 (typed agg continuation) | **DONE** |
| F5 | wrong-rows | S3 (typed agg continuation) | **DONE** (F34 closed the residual) |
| F6 | wrong-rows | Theme 3 (AVG exact sum) | **DONE** `44a534a14` |
| F7 | wrong-rows | Theme 4 (injective DISTINCT elision) | **DONE** |
| F8 | wrong-rows | S2 (distinctKey tuple-pack) | **DONE** |
| F9 ≡ F13 | wrong-rows | S2 (compareValues []byte arm) | **DONE** `6663cf56e` |
| F10 | wrong-rows | S1 + S2 (covering float32 norm) | **DONE** `6663cf56e` |
| F11 | wrong-rows (latent) | Theme 7 (STARTS_WITH range) | **DONE** |
| F12 | wrong-rows (latent) | Theme 7 (full-tree pushdown) | **DONE** |
| F14 | wrong-rows | S1 (hash key normalize) | **DONE** `91b566c61` |
| F15 | crash | Theme 2 (hash key whitelist) | **DONE** `91b566c61` |
| F16 ≡ F25 | structural | Theme 5 (guard comment + unit pin) | **DONE** |
| F17 | missing-error | S5 (int32 SUM_I overflow) | **DONE** |
| F18 | missing-error | S5 (MIN/MAX plan-time type gate) | **DONE** |
| F19 | missing-error | Theme 7 (index-orphan ERROR) | **DONE** |
| F20 | nondeterministic | S4 (deterministic cost winner) | **DONE** `a5846b405` |
| F21 | nondeterministic | S4 (comparand-aware equality) | **DONE** `a5846b405` |
| F22 | dead-code | Theme 8 (aggregateCursor.pending) | **DONE** |
| F23 | dead-code | Theme 8 (RecordQuerySortPlan) | **DONE** |
| F24 | wrong-rows | S3 (FlatMap check-value restart) | **DONE** |
| F26 | structural | (was S3-after) | **REJECTED** (false positive) |
| F27 | wrong-rows (float) | Theme 2 (cmpAny NaN) | **DONE** |
| F28 | wrong-rows (float) | Theme 2 (-0.0 vs 0.0) | **DONE** |
| F30 | structural (latent) | S4 (text/distance comparands in memo equality) | **DONE** |
| F31 | wrong-rows | S2 (CTE dedup keyers tuple-pack) | **DONE** |
| F33 | wrong-rows | S3 (sort continuation type loss) | **DONE** |
| F34 | wrong-rows | S3 (agg keyVals exotic-type residual + comment) | **DONE** |
| F35 | wrong-rows | Theme 7 (atomic-mutation index candidacy) | **DONE** |

Later-surfaced (during fixing): F27, F28 (float total-ordering in the comparator /
cmpAny), F30 (semantic_identity.go still ignores text/distance comparison fields —
F21 follow-up), F31 (recursive-CTE `cteDedupKeyer.key`/`queryResultKey` shared the
F8 delimiter+NULL-sentinel flaw — fixed via the shared `packedDedupKey` encoder).
Also tracked: F9b (loud comparator residual — needs error-channel or is left
documented); F29 (`TestRuleRegistry_DuplicateNamePanics` under `-count>1`, global-
registry test isolation, not hit by count=1 CI); nit: stale `buildCoveringRow`
comment in `ordinal_join.go`. Cleanup (S3 follow-on): the typed continuation codec's
DECODE-ONLY `contValJSON` tag (15) was deleted — pre-release there are no older
binaries, nothing produced it, so it was an untested dead branch.
