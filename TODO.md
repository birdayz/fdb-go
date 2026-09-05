# TODOs

FoundationDB Record Layer — Go Port. Java version: **4.12.11.0**. FDB wire protocol: **7.3.77**.

Current state: 46 test targets, 639+ SQL tests passing, 270 yamsql scenarios, 508 cross-engine
specs, 105 fuzz targets, ~65 Cascades rules, 41 plan types (36 executor-wired), 48 value types,
9 predicate types. Unified Cascades task stack (REWRITING + PLANNING). Winner-based plan selection
with per-ordering properties.

## How to read this file

**This file holds OPEN work only.** Completed entries live in
`shifts/2026-08-31-todo-completed-archive.md`, which is the verbatim record of everything that was
in this file and is done — read it when you need the reasoning behind a finished change, never as a
work list.

**Execution order.** Sections are ordered by the priority the project states: correctness first,
then the wire-compat hard line, then the read side. Within a section, pick the item whose gates are
satisfied; items are independent unless one says otherwise. Every section is a flat list of
self-contained entries — an entry carries its own measurement, its own citation trail and its own
DONE condition.

**Gates that appear throughout.** A *query-engine gate* means the change needs an RFC with a Graefe
+ Torvalds ACK before implementation (see CLAUDE.md). A *client gate* means the FDB C++ dev +
Torvalds, with libfdb_c 7.3.77 as the spec. Entries say which one applies.

**Booking convention.** Append a new finding as ONE self-contained block at the END of its section.
Never merge it into an existing entry, and never write a live defect into the prose of an entry you
then check off — a finding inside a completed item is unreachable work.

**A Java defect goes in section 9, and filing it upstream is part of the work.** A divergence the
cross-engine probes attribute to `fdb-record-layer` / `fdb-relational` itself is not a Go item and
not an excuse: book it in section 9 with both engines' measured behaviour, the probe that pins it,
and what Go does meanwhile — then report it. "It's an upstream bug" has never been a deferral here.

---

## 1. Correctness — wrong rows, wrong errors, fail-open

Defects where the engine returns the wrong answer, the wrong error, or silently truncates.
Highest priority regardless of which layer they live in.

### [ ] driver: NO read-your-writes inside an explicit transaction — SELECT auto-commits (divergence, found 2026-06-28)

Inside `BeginTx`, DML (INSERT/UPDATE/DELETE) joins the explicit FDB transaction (`runInTx` → `activeTx.rctx`) and
is atomic on Commit / undone on Rollback — correct. But **SELECT runs in a FRESH auto-commit transaction**
(`DB.Run`), NOT the explicit tx (cascades_generator.go: "DML joins an open explicit transaction (runInTx); SELECT
runs in a fresh auto-commit transaction (DB.Run)"; only `respectActiveTx` = `IsUpdate()` routes through the tx).
Consequences, confirmed by `tx_select_isolation_probe_test.go`:
- **No read-your-writes:** a SELECT in the tx does NOT see the same tx's uncommitted DML (`UPDATE v=777` then
  `SELECT v` → 100; `INSERT id=2` then `SELECT WHERE id=2` → no rows).
- **No read-write serialization:** an in-tx SELECT adds no read-conflict range, so a read-modify-write across two
  explicit txns does not raise 1020/40001 (last-writer-wins).

**Divergence from Java:** Java's relational driver (`setAutoCommit(false)`) reads through the same FDB transaction
and so DOES provide read-your-writes + read-conflict detection. This is a deliberate Go simplification (the
executor opens its own record store; binding SELECT to the user write-tx would add read-conflict ranges — the same
"spurious not_committed" hazard `cachedLoadSchema` already dodges for catalog reads). Fixing it = route the query
executor's scan through `activeTx.rctx` when one is open AND solve the spurious-conflict problem (snapshot vs
serializable reads) — a Cascades/executor + driver-tx architecture change (Graefe). Until then it's a real
read-modify-write footgun: a txn that reads then writes the same row sees stale data. Behavior pinned (flip the
probe's `no_read_your_writes_in_explicit_tx` assertion when in-tx reads land).

**Gate — acceptance harness: SimFDB (RFC-199).** This item's hard part is not making an in-tx SELECT read
through `activeTx.rctx`; it is proving the conflict behaviour that follows is right, and doing it
*deterministically*. Binding in-tx reads to the user's write transaction adds read-conflict ranges, so the
change is only correct if every resulting conflict/no-conflict verdict is the one real FDB gives — and those
verdicts are exactly what a real cluster makes nondeterministic and unreproducible. `pkg/simfdb` is that
harness: a serializable-snapshot backend whose conflict verdicts are validated against real FDB by a
400-scenario live differential, with seed-reproducible fault injection (1020/1021/1007) so a
read-modify-write across two explicit transactions can be replayed byte-for-byte. Use it to pin the
read-your-writes semantics and the read-conflict verdicts before touching the executor; the real-FDB tests
then confirm rather than discover.

**Acceptance criteria must INJECT the rare verdicts, not wait for them.** For as long as the 1007-clock item
(F4, in the RFC-199 DST entry in the completed archive) stays booked, this item's acceptance criteria MUST reach `transaction_too_old`
(1007) and BOTH branches of `commit_unknown_result` (1021 — mutations durable, and mutations never applied) by
explicit `InjectOnce`, never by natural occurrence. The reason is specific, not procedural: the M4 fix that mints
versions from the simulated clock is exactly what was reverted with `de1da5f17`, so today SimFDB's version counter
has no relationship to time and 1007 is unreachable by anything a caller can do — a criterion that says "a
long-held transaction gets 1007" would pass while never once executing the path. The 1021 branch is chosen by a
per-seed coin (`conflict.go`), so a criterion that merely runs seeds certifies whichever branch it happened to
draw and silently leaves the other — the one where the retry has to redo the work, which is the branch an
explicit-transaction COMMIT retry actually lands on — unexercised. Both are the same failure: a green acceptance
run that has not reached the case it claims to cover. When F4 lands and 1007 becomes naturally reachable, a
natural-occurrence arm may be ADDED; the injected arms stay either way, because they are the deterministic ones.

Two SimFDB tests are direct inputs to the semantics decision this item has to make, and they deliberately
pin TODAY's behaviour rather than a desired end state (it matches Java today, so it is not a bug to fix
under this item's nose): `TestSQLFault_UpdateRelative_DoubleApply` and the durably-committed-insert 23505
case. Both are 1021 (`commit_unknown_result`) autocommit hazards — a statement whose write is durable while
the client is told the outcome is unknown. RFC-198 is what decides whether an explicit transaction changes
that, so read them as the problem statement, not as failing tests.


### [ ] dml: INSERT ... SELECT arity mismatch leaks internal XX000 instead of a clean error (wrong-error, LOW-MED, found 2026-07-20 hunt)

An `INSERT ... SELECT` whose projection column count differs from the target
record's is NOT validated at plan time: `alignInsertSelectColumns`
(logical_predicate.go) and `checkInsertSelectPromotable` both loop over
`min(len(projections), len(target))` and silently truncate, so the mismatch
surfaces only at row-read as an internal XX000 ("result row carries no positional
output row aligned to column … — the plan's top operator did not emit an ordinal
output row", resultset.go:147). Repro (dst has 2 cols): `INSERT INTO dst SELECT
id, v, id FROM src` (too many) or `INSERT INTO dst SELECT id FROM src` (too few)
→ XX000. The VALUES path rejects the same mistake cleanly (42601/22000,
insert_cascades.go:82). No data corruption (nothing partial-inserts; too-few does
NOT NULL-fill — it also errors). Wrong-ERROR only.
FIX IS NOT A PLAN-TIME COUNT CHECK (attempted + reverted 2026-07-20): the output
WIDTH ≠ `len(proj.Projections)`. A RECORD/struct projection expands into several
target columns — e.g. `INSERT INTO DST SELECT "V" FROM T1, T1."ARR" AS "V", W`
projects ONE unnested element `V` that fills DST's 2 columns (pinned by
`TestFDB_ArrayUnnestDMLDuplicateAlias` control). Worse, that unnested element's
`Value.Type()` is NOT a `*values.RecordType`, so a type-based "all-scalar" guard
also false-positives. A correct check needs the source plan's DERIVED OUTPUT
SCHEMA (post-expansion column count), not the raw projection list — a real
semantic-analysis addition. Until then the XX000 stands (low severity). Do NOT
re-add a `len(proj.Projections)` check — it 42601s valid unnest/record-expansion
INSERT…SELECTs.



### Fail-open in the plan visitor (RFC-182 2026-07-18 audit)
- [ ] `plan_visitor.go:1116/:1133/:1156` discard `upgradeFirstFilter`'s success
  bool and `:1153` returns the unfiltered op when no predicate builder succeeds —
  make correct-or-loud; surface `planErr` at `cascades_generator.go:411`.

- [ ] **A struct column projected ALONGSIDE a projected EXISTS through a JOIN's
  merged row fails LOUDLY.** · S · **query-engine change — needs a Graefe ACK
  before merge**
  Found while closing the "two producers mint primary-key comparison Values" item in section 3; measured with that entire change REVERTED, so
  it is independent of it. No `ORDER BY` involved:
  `SELECT t1.id, n, EXISTS(...) FROM t1 JOIN t3 ON ...` fails
  `ordinal resolution: field "N" not resolvable in the runtime row (ordinal -1, row
  columns [ID T1_ID ID N]) — malformed plan`.
  RETRACTED, AND THE RETRACTION IS THE POINT: an earlier version of this booking
  ALSO claimed `SELECT t1.id, n FROM t1 JOIN t3 ON ...` (no EXISTS) "returns rows
  with the first column 0 instead of the ids", sized the item M on that basis, and
  escalated it as a live SILENT wrong-rows defect. It does not reproduce — that
  query returns 1, 2, 3, correctly. The zeros were an artifact of a throwaway probe
  that scanned a two-column result into one destination and ignored the `Scan`
  error. There is ONE defect here, it is loud, and nothing silent is outstanding.
  ROOT CAUSE, measured: the positional merge does not model a struct-typed column —
  the merged row and the leg window both state UNKNOWN for that slot — so the
  reference stays lazy (ordinal -1) and resolves by a name the runtime row does not
  answer to. Same gap RFC-222 §2a works around, and why the projected-root shape
  over a join still cannot be asserted as rows.
  Pinned as a tripwire that reds when the merge learns struct columns:
  `nested_sort_key_fold_fdb_test.go`'s
  `projected_struct_root_over_a_join_still_fails_for_a_reason_that_predates_this`,
  whose message names the replacement assertion.
  DONE = the merged layout carries struct column types, the query above returns
  correct rows, and the tripwire is replaced by the row assertion it names.

- [ ] **CQ-70 (MED/M, M, gated on CQ-67 landing, query-engine review gate) — a
  PREDICATE-FREE comma join under a PROJECTED-EXISTS fold does not execute:
  `multi-leg row cannot serve a source-relative ordinal / no frontier row
  resolved`.** A live planner/executor defect on master, found while probing
  CQ-67 end-to-end.

  **PREMISE CORRECTED after the first review lap.** This was first booked as "a
  projected-EXISTS fold over a THREE-source FROM does not execute", and as "what
  makes RFC-200's gate (a) unmeasurable". Both were wrong, and the branch's own
  instruments refuted them:

  - the three-source fold DOES execute when it carries EQUIJOIN predicates,
    which is the form the corpus produces
    (`TestFDB_CommaJoinProjectedExists_UnequalLegWidths`). Only the
    predicate-free comma join fails. The defect is narrower than booked, hence
    the priority drop from HIGH to MED;
  - gate (a) is blocked by something else entirely — the nested READER arm is
    LATENT (`NESTED-HIT 0` corpus-wide), so its mutation directions are not
    writable even on a query that runs. Fixing this item does NOT unblock gate
    (a), and CQ-67's entry carries that correction.

  Reproducer, over four tables (`ta` 3 cols with `k` at ordinal 1, `tb` 1 col,
  `tc` 2 cols with `k` at ordinal 1, `tp` the existential inner) — note the
  ABSENCE of any join predicate, which is the whole trigger:

  ```sql
  SELECT tc.k, EXISTS (SELECT 1 FROM tp WHERE tp.owner = ta.aid) FROM ta, tb, tc
  ```

  fails with

  ```
  correlated FieldValue "K" (correlation "TC") evaluated against an
  unbound/unrecognized context (*RowEvalContext (multi-leg row cannot serve a
  source-relative ordinal)) — no frontier row resolved (planner/executor bug)
  ```

  **PRE-EXISTING, verified with an arm-disabled control.** The same query fails
  identically with RFC-200's nested acceptance turned OFF — the comparison was
  made by disabling `legOrdinalSafety`'s FlatMap arm, the single line that
  activates it, and re-running. Same error, same message. It is not a CQ-67
  regression.

  **LOUD, not silent.** No wrong row reaches a user; the query errors. That is
  what makes it survivable rather than an emergency, and it is the FIRST thing to
  re-check if the failure mode ever changes — a silent variant is a different and
  much worse defect.

  **TWO forms are CORRECT, which is what localises this to the missing
  predicate.** `SELECT tc.k, EXISTS (...) FROM ta, tc WHERE ta.aid = tc.cid`
  executes (two sources, flat windows throughout), and so does the THREE-source
  `FROM ta, tb, tc WHERE ta.aid = tb.bid AND tb.bid = tc.cid` — the corpus's own
  shape, which returns correct rows on all four addresses. Only the
  predicate-free comma join fails. All three are pinned in
  `pkg/relational/sqldriver/nested_merge_leg_wrong_rows_fdb_test.go`; the failing
  one asserts BOTH error substrings ("multi-leg row cannot serve a
  source-relative ordinal" and "no frontier row resolved"), so a change in
  failure mode reds rather than passing.

  **WHAT THIS DOES NOT UNBLOCK.** It was booked as the blocker for RFC-200's gate
  (a); it is not. Gate (a)'s four mutation directions are unwritable because the
  nested READER arm is LATENT — `NESTED-HIT 0` at both keyed readers over the
  whole corpus, and mutating the fused two-step address back to flat leaves the
  end-to-end probe green even on a query that runs. That is a separate condition
  tracked in CQ-67, and it needs a query whose reference reaches a leg buried
  INSIDE the merge, which is a different thing from making this one execute.

  Gated on CQ-67 only so the two do not contend for the same files; the defect
  itself is independent of it.

  DONE = the PREDICATE-FREE three-source comma join under a projected-EXISTS fold
  executes and returns correct rows, and its pinned loud-failure test is replaced
  by a row assertion. NOT tied to CQ-67's gate (a), which is blocked on the
  latent reader arm and is unaffected by this item. Executor + NLJ rule, so:
  Graefe-gated.


- [ ] **CQ-75 (HIGH, wrong rows, MEASURED) — `v IN (-0.0, 0.0)` silently loses a
  row, and which row survives depends on element ORDER.** · S/M · query-engine
  review gate
  **Booked 2026-08-01 by the B6 docs-authority pass. This is not a new finding —
  it was measured long ago and written into the PROSE of CQ-28, which is marked
  `- [x]`.** Under this file's execution rule ("pick the lowest-numbered UNCHECKED
  item") that made it permanently unreachable. It is exactly the "deferred
  findings rot into invisibility" failure CLAUDE.md names, and it was found by
  reading the file rather than by any test.
  MEASURED, over rows `(-0.0,5) (5.0,5) (0.0,9)` with index `(v,w)`:

      v IN (-0.0, 0.0)  ->  [1]      plan IndexScan(T_VW,[=,*])   ONE probe
      v IN (0.0, -0.0)  ->  [3]      same plan, keeps whichever is FIRST
      v IN (-0.0, 5.0)  ->  [1] [2]  plan InJoin(...)             correct

  So the IN-list dedups `-0.0` against `0.0` under IEEE equality and collapses two
  distinct index probes into one, while the stored entries are two distinct keys.
  Independent of CQ-28, and user-visible without any of CQ-28's machinery.
  Related pin that does NOT cover this shape:
  `pkg/relational/sqldriver/negative_zero_composite_index_sarg_probe_test.go`
  pins the `v = 0 AND w = 5` shape, not the IN-list one.
  DONE = the IN-list's element dedup keys signed zero by the same bit-identity the
  index entries use (Java's `-0.0`/`0.0` are two entries, and the wire is the hard
  line), both element ORDERS return the same rows, and a regression pins the
  ORDER-dependence specifically — a single-element or same-sign test cannot express
  this defect.


- [ ] **CQ-93 (MED, query-engine — needs its own RFC + Graefe ACK): a raw NaN
  primary key is finer than logical equality, so PK-coverage DISTINCT elision
  and storage-order sort elision are both unsound over FLOAT/DOUBLE keys.**
  FDB storage preserves NaN sign and payload, so `0xfff8000000000001` and
  `0x7ff8000000000001` are two DISTINCT primary tuple keys packing on opposite
  sides of every finite value, while `values.CompareFloat64` (faithful to
  `java.lang.Double.compare`) canonicalizes both to ONE value ranked greatest.
  Two planner shortcuts assume storage identity == logical equality and storage
  order == comparator order, and neither holds here: `SELECT DISTINCT id, status
  FROM t WHERE status = 'x'` drops the distinct operator on PK coverage and
  returns the two NaN records as two rows, and `ORDER BY id` (in both
  directions, and before `LIMIT`) is answered from an index whose PK suffix is
  not in comparator order. MEASURED on a real cluster: with the fix reverted the
  DISTINCT arm fails as `raw NaN primary keys incorrectly eliminated DISTINCT:
  Project([ID#0, STATUS#1], IndexScan(STATUS_IDX, [=] COVERING))`.
  The fix and its end-to-end test are WRITTEN and CARVED OUT of PR #575
  (branch `agent/correlated-signed-zero-probes`), which is scoped to signed
  zeros: `/tmp/claude-1000/carveouts/logical_distinctness_proof.patch` +
  `.note`, carrying `logical_distinctness_proof.go`, the two rule changes
  (`rule_implement_distinct_final`, `rule_implement_sort`) and
  `sqldriver/rawnan_pk_suffix_fdb_test.go`. It applies cleanly to that head and
  passes; it is held out only because it changes when two general-purpose
  Cascades rules fire and so needs its own RFC + Graefe ACK. DONE = that RFC
  lands, the patch is submitted on its own terms, and the test runs in CI.
  (This gap was previously mis-cited in `road-to-prod.md` as CQ-84, which is the
  unrelated qualified-star-with-GROUP-BY item.)


- [ ] **`rangeScanImpl` FAILS OPEN on the impossible `more=true` with zero rows,
  where C++ asserts** · S · found during the exact-mode batch-division review
  (pre-existing, not introduced by that work)
  `client/readpath.go:808-812` guards the "misbehaving storage server" case —
  `more=true` alongside an empty batch — with a bare `break`. Its own comment
  records that C++ treats this as impossible (`ASSERT`). The `break` leaves the
  shard loop, the outer loop finds nothing further, and the function returns
  `allKVs, remaining <= 0` — with `remaining` still positive, that is `more=false`.
  So a caller asking for N rows silently receives a SHORT result labelled complete,
  and a cursor built on it reports clean exhaustion. Every layer above trusts
  `more`: the iterator latches `exhausted`, and the record layer's continuations
  end rather than resume.
  The failure direction is the problem, not the guard. Breaking the loop to avoid
  spinning is right; reporting the truncated read as a COMPLETE one is what
  converts a server-side anomaly into a silent wrong answer, which is the one
  outcome worse than an error. C++ chooses to crash; the Go equivalent is to
  surface a retryable error rather than to invent `more=false`.
  NOT MEASURED against a real misbehaving server — it needs fault injection at the
  reply-parsing boundary, which `pkg/simfdb`'s fault hooks can express.
  DONE = the arm returns a retryable error (or otherwise preserves `more`) instead
  of a silent short read, with an injected-reply test that reds on the current
  `break` and names the silent-truncation direction in its failure message.


- [ ] **The duplicate-GROUP-BY-key gate and the post-aggregate rebase decide key
  identity by DIFFERENT predicates, so two semantically-equal grouping keys reach
  a first-match loop and Go answers where Java raises** · M · found while folding
  review conditions on PR #723 · **query-engine change — needs a Graefe ACK and
  its own RFC before merge**

  Two sites must agree on "are these the same grouping key" and do not. The gate
  (`plan_visitor.go`, `visitSelectGroupBy`) refuses duplicates with 42702; the
  post-aggregate rebase (`logical_predicate.go`, `rebasePostAggregateGroupKeyValue`
  and `rebasePostAggregateComputedGroupKey`) binds a re-read key by taking the
  FIRST matching key. When the gate says "different" and the rebase's semantic
  matcher says "same", the loop silently picks a slot where Java stops.

  BOTH ROUTES ARE AFFECTED, by different mechanisms, and both are MEASURED:

  1. NESTED / name-keyed route. `plan_visitor.go:1402` gates the alias strip on
     `len(fs.joins) == 0`, so under a JOIN the normalization is off and
     `groupKeysEquivalent` compares unequal `(Bare, Qualifier)` pairs:
     - `SELECT a.r.v.z, r.v.z, max(a.q.s) FROM nested a JOIN flat b ON a.id = b.id
       GROUP BY a.r.v.z, r.v.z` -> `[[100 100 203] [140 140 330]]`
     - `SELECT max(b.c2) FROM nested a JOIN flat b ON a.id = b.id
       GROUP BY b.c1, c1 HAVING c1 > 120` -> `[[330]]`
     The FLAT twin diverging identically is what proves this is PRE-EXISTING and
     NOT nested-specific — it must not be mis-attributed to RFC-230.

  2. COMPUTED / expression route. The gate's semantic arm needs both keys to
     resolve through `Resolver.WalkExpression` (`posPredicate`), which is strictly
     weaker than the `WalkExpressionForProjection` (`posProjection`) that MINTS
     `GroupKey.Value` — only the latter folds a comparison. Measured with the gate
     instrumented:
     - `GROUP BY (c1 = 1), c1 = 1` — both gate walks fail
       (`unsupported shape: *antlrgen.BinaryComparisonPredicateContext`), identity
       falls to a `GetText` token compare, and the parentheses defeat it.
     - `GROUP BY (c1 + 1), c1 + 1` — both gate walks SUCCEED, so the semantic arm
       runs, and it STILL returns false: `(c1 + 1)` resolves to a
       `RecordConstructorValue` wrapping the arithmetic that `c1 + 1` resolves to
       bare. One expression, two Value types.

  THE NON-UNWRAP IS CORRECT AND MUST NOT BE "FIXED". Java's
  `visitRecordConstructor` does not unwrap a one-element constructor either
  (`ExpressionVisitor.java:918-925`), which is precisely why
  `walkRecordConstructorInner` does not — unwrapping in the walk collapses the
  projection case and REINTRODUCES a divergence Go already closed. Beware
  `walk.go:37`: its summary list still says "1-element unnamed → unwrap", stale
  prose contradicting the function it summarizes at `walk.go:1479-1494`.

  DELETE THAT LINE AS PART OF STEP 2 — it is not open-ended cleanup and must not
  outlive this item. It was left untouched deliberately while #723 was in flight,
  because editing another package's comment would have forfeited that PR's
  "pure addition, cannot have widened a pre-existing divergence" property for
  cosmetics. That reason expires the moment this item is worked. The line has
  already misled one review chain into proposing a "fix" that would have
  REINTRODUCED a divergence Go closed; leaving it is leaving the trap armed.

  JAVA REFUSES ANYWAY — MEASURED, and this is the fact the whole entry rests on.
  The natural inference from the paragraph above is that Java sees the same
  wrapper asymmetry and also declines to refuse, i.e. that Go is CONFORMANT and
  this route is not a divergence at all. That inference is wrong, and only the
  live JVM could settle it. `conformance/duplicate_groupby_java_probe_test.go`
  (`paren_twin_aggonly`, `paren_twin_proj`, `paren_twin_having`, `cmp_twin`)
  measures Java at tag 4.12.11.0 refusing all four:

      GROUP BY (amount+1), amount+1  ->  java: Ambiguous columns for
                                           q...._0.AMOUNT + @c12   | go: PLANS
      GROUP BY (amount=1), amount=1  ->  java: Ambiguous columns for
                                           q...._0.AMOUNT equals @c12 | go: PLANS

  Java's message names the UNWRAPPED arithmetic, which says how its guard works:
  `Expressions.pullUp` compares FieldPath DERIVATIONS and descends THROUGH the
  record wrapper, finding the same derivation twice. Both engines build the
  wrapper; only Java's guard looks past it. The probe also pins the controls
  (`paren_both_sides`, `cmp_same_text`) where Go DOES refuse, so the hole is
  the mixed spelling and not the gate as a whole.

  BOTH ROUTES ARE NOW DIRECTLY MEASURED, separately, and neither leans on the
  other's credibility. Route 1's own JOIN spelling was initially left to the
  single-source analogue (`dup_qualified_vs_bare`); that gap is closed —
  `join_qualified_vs_bare` measures Java refusing `... JOIN ... GROUP BY
  a.amount, amount` with `Ambiguous columns for q...._0.AMOUNT` while Go plans
  it, and `join_control_single_source` measures BOTH refusing the same key
  without the join, which is the alias strip working when it is switched on. The
  probe deliberately groups by `amount` (declared only by T_G1) rather than
  `category` (declared by both), so the bare reference is unambiguous and cannot
  be refused for a reason unrelated to duplication.

  THE BOUND, and it is why this is not an emergency: rows are ARITHMETICALLY
  CORRECT in every measured case. Two semantically-equal keys hold identical
  values in their two output slots, so binding either answers correctly. This is a
  CONFORMANCE divergence, not wrong rows.

  WHAT CURRENTLY KEEPS THE LOOP SINGLE-MATCHED, and the direction-asymmetric trap
  in changing it. The rebase loop instruments to `matches=1` for every query here,
  because the same wrapper mismatch that defeats the gate also keeps the two keys
  distinguishable at the loop. For the ARITHMETIC pair no normalization can arm
  it: `walkAtom` dispatches RecordConstructor identically in both positions, so
  any unwrap makes the GATE refuse before the loop. The COMPARISON pair is the
  hazard, because its gate failure is independent of the wrapper. Directions:

      unwrap in WalkExpressionForProjection ONLY -> gate still sees RCV vs AV and
        does not refuse, GroupKeys becomes [AV, AV], reference matches BOTH: ARMED
      unwrap in WalkExpression ONLY (the gate's walk) -> gate sees AV vs AV: SAFE
      unwrap in both, or beneath both                                     : SAFE

  The projection walk mints the planner's value, so it is where someone would
  naturally change it — the naive fix lands on the dangerous side by default.

  JAVA'S SIDE differs by clause and a fix must not flatten it: `Expressions.java:112`
  asserts `size() == 1` with `AMBIGUOUS_COLUMN`, its message verbatim the Go
  wording at `plan_visitor.go:1492`; `Expression.java:246` is a BARE
  `Iterables.getOnlyElement` with no assert. Only the PROJECTED half spends that
  SQLSTATE.

  THE FIX IS TWO INDEPENDENT STEPS, AND THE FIRST IS SMALL. An earlier revision of
  this entry described one coupled change with "affects every GROUP BY shape"
  blast radius; that was wrong and would have got this deferred as though it were
  the big one.

  THE ERROR SHAPE THAT PRODUCED THIS ITEM, recorded because it will recur here
  and because it is what makes the entry above worth its length. Every wrong
  claim in this item's history — five collapsed "structural interlock" arguments
  in the Go comments, and two separate review hypotheses about what Java does —
  had the same form: a TRUE PREMISE with an INVALID INFERENCE drawn from it, and
  in every case only measurement settled it.

    - "The gate and the matcher both use SemanticEqualsUnderAliasMap" was true of
      one of the gate's three arms; "therefore no multi-match can reach the loop"
      did not follow.
    - "Java does not unwrap a one-element record constructor" is true
      (ExpressionVisitor.java:918-925); "therefore Java sees the same asymmetry
      and also declines to refuse" did not follow — `Expressions.pullUp` compares
      FieldPath DERIVATIONS, not the Values, and descends through the wrapper.
    - "Making the loops raise on a multi-match is the Java port" is true;
      "therefore it closes the pinned divergences" did not follow, since a guard
      is inert where no post-aggregate reference exists.

  The practical rule this item should be worked under: an argument that some
  shape CANNOT occur is not evidence until a probe has failed to construct it.
  Every impossibility claim here that went unmeasured turned out to be false.

  STEP 1 — THE LOOP GUARD, and it IS the Java port. Make both rebase loops
  (`rebasePostAggregateComputedGroupKey` and `rebasePostAggregateGroupKeyValue`)
  COLLECT matching keys and raise `AMBIGUOUS_COLUMN` on more than one, instead of
  taking the first. This is exactly Java's structure: `Expressions.pullUp` and
  `Expression.pullUp` guard LOCALLY at the pull-up site and delegate correctness
  to no upstream duplicate-key gate. Go inverted that — the gate guards, the loop
  trusts — and every collapsed "structural interlock" claim in this file's history
  is a symptom of that single inversion. It is LOCAL, small, touches no GROUP BY
  shape outside these two loops, needs no RFC-scale review, and is SAFE IN EVERY
  ORDERING: once it is in, closing any normalization gap can only turn a silent
  answer into a raise, which is the direction Java is already in. DO THIS FIRST.

  WHAT STEP 1 ACTUALLY CLOSES, and it is LESS than the obvious reading — measured,
  because a DONE criterion that step 1 cannot satisfy is how an item rots. A loop
  guard only fires where a POST-AGGREGATE REFERENCE exists for a loop to match, so
  it is inert on any shape that has neither a HAVING nor a grouping key in the
  SELECT list.

    - CLOSED by step 1: the Go-side arm `under_a_join_two_equal_keys…`. All four
      of its spellings carry a reference, and the two equal `FieldValue` keys
      genuinely multi-match — instrumenting the sibling loop reports
      `matches=2 nkeys=2` for both the nested (`R`) and flat (`C1`) spellings. A
      `>1` guard raises on exactly these.
    - NOT closed by step 1: the Go-side arm
      `a_parenthesised_computed_key_twin…`. Its keys are
      [RecordConstructorValue, ArithmeticValue], so a reference matches only ONE
      of them (`matches=1`, the measurement recorded above) and no `>1` guard can
      trip. It needs step 2.
    - NOT closed by step 1: the `java_42702_go_plans` probes — FIVE when this was
      written, FOUR now. `paren_twin_aggonly`, `cmp_twin` and
      `join_qualified_vs_bare` are bare `SELECT COUNT(*)` with no HAVING and no
      key projected, so there is no reference and a post-aggregate guard never
      runs; `paren_twin_proj` and `paren_twin_having` have a reference but are the
      `matches=1` case.

      THE DIAGNOSIS IN THE NEXT SENTENCE WAS RIGHT AND ITS PRESCRIPTION WAS
      WRONG, which is why `join_qualified_vs_bare` has since closed WITHOUT step
      2. Java does refuse at GROUP BY CONSTRUCTION, pulling up the group-by result
      itself rather than a user reference (LogicalOperator.java:454 through the
      asserting Expressions.pullUp) — that part is exactly right. But Go's analog
      of that site is NOT the name-based duplicate gate: it is a SEMANTIC pull-up
      at the same construction point, which is what `groupByOutputConstructionPullUp`
      now does. `join_qualified_vs_bare` measured `both_42702` against the live
      JVM once it landed. The four paren/cmp probes remain open and DO need the
      normalization step, because no semantic matcher equates a
      RecordConstructorValue with an ArithmeticValue. See the closing block at the
      end of this file.

  STEP 2 — NORMALIZATION, at leisure and behind its own RFC. Converge the gate's
  identity predicate with the loops' so the two sites ask one question; this is
  what changes `groupKeysEquivalent` and reaches every GROUP BY shape. The
  tempting shortcuts are symptom fixes: widening the rebase matcher, or merely
  extending the alias strip to joins, makes the name compare agree more often by
  accident while leaving two predicates that still disagree.

  PINNED MEANWHILE, both directions, on both sides of the engine boundary:
  `pkg/relational/sqldriver/groupby_computed_key_having_fdb_test.go` arms
  `under_a_join_two_equal_keys_are_refused_42702_at_output_construction`
  (renamed and flipped to assert the REFUSAL when the output-construction pull-up
  landed; see the block at the end of this file)
  and `a_parenthesised_computed_key_twin_is_NOT_refused_either` (Go-side, still
  asserts VALUES because the construction guard cannot equate a
  RecordConstructorValue with an ArithmeticValue), plus the `java_42702_go_plans` probes above (cross-engine, and they red
  if EITHER Java stops refusing or Go starts).

  STEP 1 DONE = both loops collect matches and raise `AMBIGUOUS_COLUMN` on more
  than one, AND `under_a_join_two_equal_keys…` flipped to assert 42702 on all four
  spellings. Nothing else moves, and nothing else should be expected to.

  STEP 2 DONE = the gate and the loops share one identity predicate; all five
  `java_42702_go_plans` probes moved to `both_42702`; and
  `a_parenthesised_computed_key_twin…` flipped to assert the refusal.

  SPLIT THIS ITEM when step 1 lands rather than holding it open — the two steps
  have different blast radii, different review requirements, and, as the lists
  above show, disjoint closable sets.

---


- [ ] **`evaluateCorrelated` resolves an `OrdinalRow` binding BEFORE a datum binding, so a carrier that is both takes the row reading — and the two readings only coincide at a SINGLE accessor.**
  `pkg/recordlayer/query/plan/cascades/values/values.go`, the `*RowEvalContext`
  correlation arm:
  ```go
  if row, ok := bound.(OrdinalRow); ok {
      return f.evaluateOrdinal(row)
  }
  return f.evaluateDatumBinding(bound)
  ```
  THE TWO ARMS CONSUME THE ROOT ACCESSOR DIFFERENTLY, which is the whole of it.
  `evaluateOrdinal` treats `Accessors[0].Ordinal` as a SLOT INDEX INTO THE ROW.
  `evaluateDatumBinding` treats the BINDING ITSELF as the root read — the leg
  adapter has already unwrapped the carrier, so the root accessor is consumed by
  the binding and `descendResolvedPath` applies `Accessors[1:]`. For a
  SINGLE-accessor path the two agree by coincidence: slot 0 of a one-slot carrier
  and "the carrier itself" are the same value, and there is no remainder to
  misplace. For a MULTI-accessor path they diverge — the ordinal arm indexes the
  ROW and then descends the remainder inside whatever it found there, while the
  datum arm descends the remainder inside the bound value itself.
  WHY IT IS WORTH BOOKING NOW RATHER THAN WHENEVER: multi-accessor references
  over a DATUM binding are newly common. An unnest struct-element MEMBER
  (`x.ek`, `x.d.dk`) is exactly that shape — the Explode binds the element as one
  proto message and the member is the path's remainder — and until the fix that
  booked the entries above, such references could not resolve inside an EXISTS at
  all. The precedence was therefore never exercised by them.
  NOT A KNOWN LIVE DEFECT, and stated that way deliberately. The element datum in
  the measured paths is a `proto.Message`, which does NOT implement `OrdinalRow`,
  so it takes the datum arm and the fix's row assertions
  (`TestFDB_UnnestElementMemberInExists`) pass. The exposure is a carrier that
  satisfies BOTH interfaces reaching this arm with a multi-accessor path; nothing
  currently enumerates which carriers those are, which is the gap.
  DONE = either the precedence is shown safe by enumerating every `OrdinalRow`
  implementor that can be a correlation binding and pinning that none of them is
  also a datum carrier (a negative result, pinned like
  `TestFDB_UnnestElementMemberInGather`), or the arm is made to decide on
  something other than interface satisfaction order — the path's own arity or an
  explicit carrier kind — so "both interfaces" stops being resolved by which
  `if` was written first.


### [x] An aggregate is bound to its output slot by a RENDERING that cannot tell two aggregates apart — FIXED (RFC-241)

`SELECT COUNT(CASE WHEN "Region"='us' …), COUNT(CASE WHEN "Region"='US' …)` returned
`[2,2]`; it returns `[0,2]`. Pinned by
`TestFDB_AggregateOperandDistinguishesLiteralCase` (10 arms including the
REVERSAL — the pair and its reversal disagreed about which value both columns
took, so one order alone could not express the defect) and
`TestValidateAggCallProvenance` (7 arms). Mutation-verified: restoring the fold
reproduces the exact original wrong values on all five defect arms and leaves
both controls green.

**This entry's own diagnosis was wrong, which is why the fix is not where it
said.** It blamed the post-aggregate ordinal bind and cited
`aggregateValueOutputName` → `aggregateOrdinalFor` — a function that existed
nowhere but in this prose (`git grep -n 'aggregateOrdinalFor' -- .` returned one
hit, this entry; control `aggregateValueOutputName` returned 2 files). And
`EXPLAIN` showed the projection already reading ordinals `#0`/`#1` under two
correct distinct names, so that bind was never the defect.

The real cause was one layer earlier, in operand RESOLUTION:
`upgradeAggregateOperands` matched a parsed aggregate column to its
`agg.Calls` entry with `strings.EqualFold` over the operand's RENDERED TEXT,
which carries identifiers and string literals alike. Each column matched BOTH
calls and the second write clobbered the first — last-wins. The identical fold
had already been removed from the naming key by RFC-237, whose comment describes
this exact collision; the sibling site one file away was never touched.

The correspondence is now RECORDED by the producer (`callToAggCol`, returned by
`logicalAggregateCalls`) and validated at use rather than reconstructed from a
rendering. Follow-up in §4 books the end state — resolving the operand AT the
producer, which deletes both the table and its validator.


---


---

## 2. Wire compatibility and the pure-Go FDB client

The hard line: key encoding, record/index format, continuations, metadata, and everything
`pkg/fdbgo` puts on the wire. C++ (libfdb_c 7.3.77) is the spec for the client; Java 4.12.11.0 is
the spec for the record layer. Client gate applies to every entry here.

### [ ] Replay DB transaction defaults as an ordered option list, with TIMEOUT applied last

Pre-existing divergence on the DB-defaults replay path, surfaced while fixing the
`applyTxDefaults` data race (that fix was synchronization-only and left the replay order
byte-for-byte unchanged, before and after — so this is not a regression from it).

C++ stores the defaults as `UniqueOrderedOptionList<FDBTransactionOptions> transactionDefaults`
(`DatabaseContext.h:717`) — a `std::list` of (option, value) pairs plus a `std::map` index, where
`addOption` erases any prior entry and re-appends, i.e. **dedup by move-to-end**
(`FDBOptions.h:91-111`). At transaction construction the whole list is replayed **one option at a
time** in list order by `ReadYourWritesTransaction::applyPersistentOptions`
(`ReadYourWrites.actor.cpp:2678-2696`).

That replay singles out `TIMEOUT`: the loop **skips** it, and it is re-applied **last**, after every
other option is installed. The rationale is in the source and is deliberate, not incidental:

> Setting a timeout can immediately cause a transaction to fail. The only timeout that matters is
> the one most recently set, so we ignore any earlier set timeouts that might inadvertently fail
> the transaction.

Go instead models the defaults as a fixed struct (`txDefaults` in `pkg/fdbgo/fdb/database.go`,
`TransactionDefaults` in `pkg/fdbgo/client/database.go`) and applies them in **field order**, with
`SetTimeout` **first** (`Database.applyTxDefaults`). Two behavioural consequences, not cosmetic
ones:

1. **The timeout clock starts before the other options are installed** rather than after, so the
   window it measures differs from C++'s.
2. **Set-order is not preserved.** A struct cannot express move-to-end dedup, so an option set
   later cannot overtake one set earlier the way C++'s list guarantees.

The port is the option list: keep an ordered, dedup-by-move-to-end list and replay it in order with
TIMEOUT deferred to the end. Note the immutable-snapshot publication the race fix introduced must be
preserved — a list is a reference type, so the snapshot has to be a genuine copy, not a shared
header.


### [ ] Implement `report_conflicting_keys` + `\xff\xff/transaction/conflicting_keys/` in the pure-Go client

`fdb.TransactionOptions.SetReportConflictingKeys` currently returns `UnsupportedOptionError`
(`pkg/fdbgo/fdb/options.go`), and Go has no special-key module for the readback
(`pkg/fdbgo/fdb/API_PARITY.md` lists `\xff\xff/transaction/conflicting_keys` as absent). C++ is
the spec for both halves: the option sets `CommitTransactionRequest.transaction.report_conflicting_keys`,
which the commit proxy reads (`CommitProxyServer.actor.cpp`), and the proxy answers a conflicting
commit with `CommitID{version: invalidVersion, conflictingKRIndices}` — the in-band shape Go's
`parseCommitReply` already recognises and maps to `not_committed`
(`pkg/fdbgo/client/commitpath.go`, the `reply.Version == InvalidVersion` arm). The indices name
which of the transaction's OWN read-conflict ranges the resolver rejected; the special-key module
renders them as boundary keys valued `"1"` (a rejected range's begin) / `"0"` (its end).

This is two things at once, which is why it is booked as one item:

1. **The missing debugging surface.** When a Go transaction takes `not_committed`, there is
   currently no way to ask *which* range conflicted. Every other client can.
2. **The instrument for the go-vs-cgo spurious-conflict skew below.** The investigation that
   booked this item had to route every conflict attribution through the CGO client because the
   Go client cannot answer the question about itself.

**The skew, with its measured facts.** The version-pinned conflict differentials in
`pkg/fdbgo/bench` take spurious `not_committed(1020)` that no client caused. It hits BOTH clients
— libfdb_c included — but not evenly: **12 go-side vs 5 cgo-side** over 204 full-package runs
(`TestDifferential_GetKeyConflict`), and 25:5 on the explicit conflict-range differential in a
second run. What is already excluded, by measurement:

- The Go client's shipped read-conflict ranges for a failing transaction are exactly the intended
  narrow ranges plus its own `\xff/SC/<uid>` self-conflict range (observed off the commit path).
- `GetCommittedVersion()` does not under-report: a unique-nonce seed written by the setup
  transaction is visible at the pinned version on both clients in every failing case.
- The conflict is on the transaction's USER range, not the self-conflict range — libfdb_c's
  conflicting-keys readback names it, and a canary whose only read-conflict range is its own
  `\xff/SC/<uid>` took 0 hits in ~134k commits (`TestDifferential_SelfConflictRangeCanary`).
- Window width does not explain the skew: the Go client's vSetup→commit window is 1.30x wider at
  idle but 0.78x — i.e. *narrower* — under full-suite load, while the skew persists.
- No committed Go write-conflict range overlaps the failing read set above its read snapshot
  (ledger of the last 8192 Go commits). Bisecting the read version puts the covering write
  several thousand versions ABOVE the setup, at a commit version SHARED by conflicts in unrelated
  prefixes.

**Leading hypothesis**, to be confirmed or killed with the readback once it exists: the resolver
records the write-conflict ranges of transactions it later REJECTS, so a rejected transaction
causes false conflicts for later ones. That fits every fact above — in particular the shared
culprit version across unrelated prefixes, and the commit ledger's blindness to it, since the
ledger only observes commits that SUCCEEDED. Confirming it needs the readback on the Go side so
the rejected transaction and the false conflict can be attributed in the same client.

Until then the differentials are gated on reproducibility rather than on a single disagreement
(`pkg/fdbgo/bench/conflict_divergence_gate_test.go`), with the non-reproducing class counted
against a ceiling rather than swallowed.


- [ ] **CQ-9a (MED) — `INDEX_FETCH_METHOD` is accepted and silently ignored.**
  Surfaced by CQ-9's audit. Java's `PlannerConfiguration.of` reads a FOURTH
  planner option beyond the three CQ-9 wired: `INDEX_FETCH_METHOD`, via
  `OptionsUtils.getIndexFetchMethod` (`OptionsUtils.java:59-60`) into
  `RecordQueryPlannerConfiguration.setIndexFetchMethod`, which every
  `MatchCandidate` (`ValueIndexScanMatchCandidate:241,268`,
  `AggregateIndexMatchCandidate:417`, `WindowedIndexScanMatchCandidate:410,436`)
  stamps onto the `RecordQueryIndexPlan` it builds. `api.OptIndexFetchMethod` is
  read by nothing in Go.
  Severity is narrower than "ignored" implies, and the distinction should not have
  to be re-derived. `SCAN_AND_FETCH` asks for what Go always does, so it is honored
  in effect. `USE_REMOTE_FETCH_WITH_FALLBACK` — Java's DEFAULT, hence what nearly
  every caller gets — explicitly permits falling back to scan-and-fetch, which is
  what Go does, so the DEFAULT PATH IS CORRECT rather than merely tolerated. The
  single sharp edge is an explicit `USE_REMOTE_FETCH`, which demands remote fetch
  with no fallback: Java issues a mapped-range scan, Go silently scan-and-fetches.
  Same rows either way; a different round-trip profile than the caller asked for.
  This is NOT option plumbing and must not be closed by adding a config field the
  planner reads and no executor honors: the capability is absent end to end.
  `SCAN_AND_FETCH` is what Go already does unconditionally; `USE_REMOTE_FETCH`
  needs FDB's `Transaction.getMappedRange` (Java drives it through
  `IndexPrefetchRangeKeyValueCursor`), which neither FDB client Go uses exposes —
  `pkg/fdbgo/fdb.Transaction` has only `GetRange`, and Apple's Go binding has no
  mapped-range call either. Go's index-scan plan has no fetch-method field to
  carry the choice, and its wire form would need one for plan-serialization
  parity.
  Order of work: (1) add `GetMappedRange` to the pure-Go client against the C++
  7.3.77 spec, under the client-engineer gate (C++ is the spec; wire compat is
  the hard line); (2) add a remote-fetch index cursor in the record layer with
  the fallback semantics `USE_REMOTE_FETCH_WITH_FALLBACK` names; (3) carry the
  method on the index-scan plan and stamp it from `PlannerConfiguration` at the
  match candidates, as Java does; (4) only then read the option in
  `plannerOptionsFrom`. Until (1)-(3) exist the option's own doc comment says
  plainly that it is not honored, and its default stays Java-identical so the
  default option set does not diverge on the wire.

  **Two constraints step (1) established that bind step (2)** — read out of
  `ReadYourWrites.actor.cpp` at 7.3.77, not inferred, and they change what a
  remote-fetch cursor is ALLOWED to do:

  1. **A mapped read can never be a snapshot read.** `snapshot=true` is
     `unsupported_operation` (2108), not a mode
     (`ReadYourWrites.actor.cpp:1219-1224`), and read-your-writes-disabled is the
     same error (:1226-1233). So a remote-fetch cursor may not be offered as a
     cheap snapshot scan, and any caller that today drops to a snapshot read for
     a non-conflicting index scan must NOT route that through `GetMappedRange`.
     The two shapes are pinned differently, and the difference is the point:
     RYW-disabled is a runtime state and returns 2108, while snapshot is
     unreachable BY CONSTRUCTION — `*Snapshot` exposes no `GetMappedRange`, so
     the request cannot be expressed. `TestGetMappedRange_SnapshotCannotRequestIt`
     pins that absence, since adding the method would arm the divergence with
     every other test still green.
  2. **Every secondary read needs its own conflict range.** The mapped read is
     issued at `Snapshot::True` internally and the client then re-adds read
     conflict ranges by hand — for the primary range AND for each resolved
     secondary get/getRange (`:1163-1192`). The secondary keys are not knowable
     until the reply arrives, so they cannot be conflict-ranged up front. A
     cursor that batches, caches, or resumes mapped reads across transactions
     inherits this: dropping the secondary conflict ranges turns a serializable
     index fetch into a silently non-serializable one. Overlap with the
     transaction's own writes is `get_mapped_range_reads_your_writes` (2039) —
     the client raises it rather than serving the write-through value, because
     RYW is deliberately NOT implemented for mapped reads.

  Also settled while porting, so step (2) does not re-derive them: `MATCH_INDEX_*`
  does not exist in 7.3.77 (no `fdb_c.h` parameter, no wire slot), and
  `libfdb_c` is NOT a valid oracle for the point-lookup arm — its
  `FDBMappedKeyValue` can only represent the getRange arm and its header says so.

### [ ] fdbgo/client: deferred-error vs cancelled precedence differs from libfdb_c on a BOTH-poisoned-AND-cancelled txn

C++ checks `deferredError` at every ThreadSafeTransaction op lambda (get :431, watch :654, commit
:669, …) BEFORE the underlying actor observes `resetPromise` — so a transaction that is both
poisoned (deferred 2000/2018) and Cancel()ed surfaces the deferred code from every op. Go's
uniform entry order is cancelled-first (checkCancelled → deferredErr → checkTimeout, in
ensureReadVersion/Commit/WatchSetup), so the same txn surfaces 1025. Observable ONLY on that
double-terminal corner; deferred-beats-timeout and deferred-beats-1034 are already C++-aligned
and pinned. Resolution needs a differential probe (poison, Cancel, Get on both clients — mind
that MultiVersionTransaction may reorder) and, if confirmed, a single swap of the two gates at
each entry point in one FDB-C-dev cycle.


### [ ] fdbgo/client: GRV reply's ProxyTagThrottledDuration is discarded — GetTagThrottledDuration undercounts vs libfdb_c

C++ accumulates the GRV reply's `proxyTagThrottledDuration` into the transaction state
(`NativeAPI.actor.cpp:7410`) in addition to the client-side per-1223-error constant add
(`:7761`). Go implements only the constant add (`nextBackoff`,
`proxyMaxTagThrottleDuration`); both GRV parsers (`grv.go` sendGRVRequest callers) discard
the parsed reply field, so `GetTagThrottledDuration` under-reports whenever the proxy
throttles the GRV itself. Surfaced by the FDB-C-dev review of PR #452 (RFC-175 C2), where
the field's comment falsely claimed reply-side accumulation — comment fixed there; the
wiring (batcher → per-waiter accumulation, mind GRV batching fan-out semantics) needs its
own small FDB-C-dev cycle with a differential pin against libfdb_c.


### [ ] fdbgo/client: rywDisabled is a plain bool read lock-free on every read-path gate — same reset-boundary class as timeoutNs/deadlineNs

Written by SetReadYourWritesDisable and applyOptionDefaults (both reset paths); read lock-free by
Get/getRangeDir/GetPipelined/WatchSetup (the 1034 gate) and Commit's ship-decision. The sanctioned
Reset-cancels-watches overlap lets a cancelled watch's teardown read it concurrently with reset()'s
re-application — the exact class fixed for timeoutNs/deadlineNs (atomics) on the reset boundary.
Same treatment (atomic.Bool or inclusion in a guarded options snapshot) in a small FDB-C-dev cycle;
surfaced by the PR #449 gauntlet as a fast-follow candidate.


### [ ] fdbgo/client: system-key DB-default applied to a tenant txn — tenant audit (2026-06-19); user-path FIXED

The tenant audit confirmed the WIRE path is byte-perfect (prefix = bigEndian64(id), prepend-at-commit,
TenantInfo, key-size all match C++). One behavioral divergence (#6) was FIXED: `SetReadSystemKeys`/
`SetAccessSystemKeys` on a tenant transaction now return invalid_option (2007), matching C++
`setOption` (NativeAPI.actor.cpp:7159-7171). **Remaining edge:** the DB-LEVEL default path is not
covered. `CreateTransaction` seeds DB defaults (incl. a READ_SYSTEM_KEYS/ACCESS_SYSTEM_KEYS DB
default) while `tenantId == NoTenantID`, and `SetTenantId` runs *after* — so a tenant txn created
under a DB that defaults system-key access silently keeps the flags, where C++ rejects. Fix needs a
check at `SetTenantId` time (reject if system-key flags already set) or at use time; `SetTenantId`
returns void today, so it's a signature/ordering change — deferred. Rare (a DB-wide system-key
default + tenants is unusual). Also documented in-code: the D3 `stripTenantPrefix` clamp divergence
(unreachable — the commit proxy guarantees prefixed boundaries; comment at `locality.go`).


### [ ] fdbgo/client: special-key-space (`\xff\xff/...`) unimplemented — locality audit D1 (2026-06-19)

Go has NO special-key-space module; every `\xff\xff/...` read hits the `maxReadKey()` gate and
returns `key_outside_legal_range` (2004). C++ `ReadYourWritesTransaction::get/getRange` intercept
`specialKeys.contains(key)` and route to `specialKeySpace` BEFORE the maxReadKey gate
(`ReadYourWrites.actor.cpp:1634-1637, 1716-1721`); `DatabaseContext` registers ~30 modules
(`NativeAPI.actor.cpp:1591, 1621-1815`): `\xff\xff/status/json`, `/cluster_file_path`,
`/connection_string`, `/worker_interfaces/`, `/transaction/conflicting_keys`,
`/transaction/{read,write}_conflict_range`, management/configuration, etc. All work via
libfdb_c/Java; all fail with 2004 in Go. It LOUDLY rejects (returns an error, not silent
corruption), but the entire surface is a feature gap. `REPORT_CONFLICTING_KEYS` already noted
elsewhere; this is the broader gap. The `\xff` system-key gating itself is faithful (maxReadKey =
`\xff`/`\xff\xff` matches C++ `getMaxReadKey`). The `SetSpecialKeySpace*`/`SetReportConflictingKeys`
option setters are silent no-ops (`fdb/options.go`). Low-frequency for a record-layer port, but it
is real cross-client surface. D2 (address `:tls`/IPv6 formatting) was FIXED; D3 (INCLUDE_PORT_IN_ADDRESS
no-op — matches api≥630 default, not a real divergence), D4 (`ParseClusterString` whitespace not
collapsed like C++ `trim()`), D5 (IPv6 coordinator round-trip not re-normalized in `ClusterFile.String`;
first-vs-last `@` split on malformed input) are low-impact edges.


### [~] fdbgo/client: watch-path divergences — D1/D2/D4 FIXED; D3/D5 remain (audit 2026-06-19)

The watch audit fixed **D4** (WatchPoll now retries the SS poll-signals — watch_cancelled/process_behind/
timed_out/future_version — instead of breaking the watch). **D1 and D2 are now fixed too** — verified
2026-08-05 while writing `docs/mt-saas.md`, which needed the user-facing watch contract and found
this item, and `road-to-prod.md`'s client-side operational list, still asserting both as open.
**D3 and D5 remain.** Ranked:

- **D1 — DONE** (`4db86c31b`, `3bc19dba7`, `af364d324`; re-verified 2026-08-05 at `2e4b9f930`).
  The counter and the 1032 throw exist: `defaultMaxOutstandingWatches = 10000` /
  `absoluteMaxWatches = 1_000_000` (`client/database.go:381-386`), `tryAcquireWatch`
  (`:388-401`) charged on the registration path (`client/readpath.go:1236-1238`) and released at
  `readpath.go:1259`/`:1288` and `transaction.go:2247`. Pinned by `client/watch_limit_test.go:10-14`,
  whose header quotes the gap this entry described. Original description retained below.
  ~~no `too_many_watches` (1032) limit.~~ C++ `Transaction::watch`
  (`NativeAPI.actor.cpp:5694`) calls `increaseWatchCounter()` (`:2175`) which throws `too_many_watches`
  when `outstandingWatches >= DEFAULT_MAX_OUTSTANDING_WATCHES = 1e4` (`ClientKnobs.cpp:120`, settable to
  `ABSOLUTE_MAX_WATCHES=1e6` via `MAX_WATCHES`); `decreaseWatchCounter()` runs when the watch resolves/
  errors (`:5679`). Go has NO outstanding-watch counter — watches are unbounded; 1032 is never thrown;
  `MAX_WATCHES` is a no-op. Fix: a `db.outstandingWatches atomic.Int64` + `maxOutstandingWatches`,
  increment at `WatchSetup` (return 1032 if at the limit), decrement on EVERY watch exit (fire/error/
  cancel) — the lifecycle is the tricky part. Test with a low limit via a `MAX_WATCHES` option.
- **D2 — DONE for the async facade** (`5a8856e7c`, RFC-170 finding #8; re-verified 2026-08-05 at
  `2e4b9f930`). `fdb.Transaction.Watch` captures the commit-completion signal synchronously inside
  the `Transact` body and registers at the COMMITTED version (`fdb/transaction.go:457-459`,
  `client/transaction.go:2221-2226`, mirroring C++ `setupWatches`' `committedVersion > 0 ?
  committedVersion : readVersion`). Pinned cross-client by
  `bench/differential_watch_test.go:266-272` (`TestDifferential_WatchSelfWriteStaysPending`).
  **Residual, deliberate:** the synchronous low-level `client.Transaction.Watch` still registers at
  the read version — it blocks in `WatchPoll` until the watch fires, so it structurally cannot wait
  for a commit (`client/readpath.go:1150-1155`). Original description retained below.
  ~~watch registered at READ version, not commit-gated.~~ C++ defers the
  SS-side watch to AFTER commit via `setupWatches()` in `commitAndWatch` (`NativeAPI.actor.cpp:6418`,
  `:6909`), at `committedVersion>0 ? committedVersion : readVersion`. Go's `WatchPoll` registers at
  `tx.readVersion` immediately, with ZERO commit coordination (`commitpath.go` has no watch handling).
  A Go watch is live before its transaction commits. Deep architectural gap.
- **D3 [architectural — RFC] — no RYW pending-write watch semantics.** C++ `RYWImpl::watch`
  (`ReadYourWrites.actor.cpp:1284`) keeps a `watchMap` + `triggerWatches`/`onChangeTrigger` so a watch
  on a key with a differing same-tx pending write fires IMMEDIATELY. Go folds the pending write into the
  baseline (via `tx.ryw.get`) but has no watchMap/immediate-fire — the watch's baseline becomes the
  post-write value and it long-polls for the *next* change (wrong fire point).
- **D5 [small] — cancel returns `context.Canceled`, not `transaction_cancelled` (1025); failed commit
  doesn't cancel watches; stale comment.** `reset()→cancelWatches()` cancels the watch *context*, so
  in-flight watches return `ctx.Err()` not an FDBError 1025 (C++ `resetPromise.sendError(1025)`). And
  (tied to D2) a failed commit never tears down the watch (C++ `cancelWatches(e)`, `:6926`). Also the
  comment at `transaction.go:1595` ("Watch() calls are NOT cancelled by Reset()") contradicts the actual
  `reset()→cancelWatches()` path — cleanup.


### [~] fdbgo/client: `makeSelfConflicting` (`\xFF/SC/<uuid>` synthetic conflict range at commit) — NON-tenant LANDED; tenant + idempotency-id add remain

**STATUS (landed):** The primary `makeSelfConflicting` port is DONE for **non-tenant** transactions
(`transaction.go` `maybeMakeSelfConflicting` — the `!intersects(write, read)` gate → `makeSelfConflictingLocked`,
placed after the read-only fast path + size check exactly as C++ commitMutations). The dummy-barrier key
picker (`intersectConflictRanges`) and the guard now share one C++-faithful sorted-merge `intersectRanges`
(1:1 with `intersects`, `NativeAPI.actor.cpp:6211` — O(n log n), not the old O(w·r) scan). Revert-proven by
`self_conflict_test.go` (wire-level: SC in both vectors for a non-tenant write-only commit; gated OFF for a
tenant commit; gated OFF when real ranges already intersect). **REMAINING:** (1) the **tenant** case —
`buildCommitTransactionRequest` prefixes the `\xFF/SC/` key with the tenant prefix (only `metadataVersion`
is exempt), so a faithful tenant port must either exempt the SC key or scope it inside the tenant keyspace;
skipped for now (gate: `tenantId == NoTenantID`) because the first attempt broke `TestDifferential_Tenant*`.
(2) the SECOND, idempotency-id-based `\xFF/SC/<idempotencyId>` add at `:6850-6856` (automatic-idempotency
feature — distinct, gate on `tr.idempotencyId`).

C++ `Transaction::commitMutations` adds a synthetic self-conflict range to a commit whose write
and read conflict ranges don't already intersect: `if (!causalWriteRisky &&
!intersects(write_conflict_ranges, read_conflict_ranges)) makeSelfConflicting()`
(`NativeAPI.actor.cpp:6858-6860`), where `makeSelfConflicting()` (`:5952`) pushes a single
`\xFF/SC/<deterministicRandom()->randomUniqueID()>` range into BOTH read and write conflict sets.
(There is a SECOND, idempotency-id-based `\xFF/SC/<idempotencyId>` add at `:6850-6856` for the
automatic-idempotency feature — distinct, gate on `tr.idempotencyId`.) Go has neither: a write-only
commit (read conflicts empty → no intersection) ships WITHOUT the synthetic range, and
`commitDummyTransaction`'s `intersectConflictRanges` (`commitpath.go:250-265`) falls back to
`writes[0].Begin` — a real user key — where C++'s dummy uses the synthetic key
(`NativeAPI.actor.cpp:6744-6750`).

**Two effects:** (a) Go's commit-request conflict-range vector diverges from libfdb_c for the same
write-only transaction (request-frame semantic difference — not persisted bytes, but affects the
resolver); (b) Go's commit_unknown_result dummy conflicts on a real user key, so a concurrent writer
of that key can false-conflict the dummy, where C++'s synthetic UUID key never collides with real
traffic. PARTIALLY mitigated today: Go's `OnError(1021/1039)` copies writeConflicts→readConflicts on
the RETRY (`transaction.go:1850`), so the retry is self-conflicting via a different mechanism — but
the original commit's wire shape and the dummy's key choice still diverge.

**Why a dedicated RFC, not a grind fix:** the commit_unknown_result ↔ makeSelfConflicting ↔
commitDummyTransaction interaction is subtle (each attempt mints a FRESH random UID, so it is NOT
simple retry-idempotency), it touches the commit path + wire shape, and it can't be cleanly
differential-tested at the data plane (conflict ranges go to the resolver, not storage — a
fault-injection test that triggers commit_unknown_result is needed). Port `makeSelfConflicting` +
the `intersects(write, read)` gate faithfully under FDB-C-dev DESIGN review; pin with a Go-side
commit-request unit test (write-only commit includes a `\xFF/SC/` range in both sets) + a
SimTransport commit_unknown_result behavioral test.


### [ ] fdbgo/client: transaction-level options are PRESERVED across `onError` retry; C++ resets them to DB defaults — needs its own RFC (found by the quality-grind options audit, 2026-06-19)

C++ `Transaction::resetImpl` (`NativeAPI.actor.cpp:6166`, called by `tr.reset()` on the RYW onError
path, `ReadYourWrites.actor.cpp:1417`) does `trState = trState->cloneAndReset(...)`, and
`cloneAndReset` (`:3515`) builds a FRESH `TransactionState` whose `options` are DB-default-constructed
— it copies the old options ONLY `if (!cx->apiVersionAtLeast(16))` (ancient APIs). So for every modern
app, a retry RESETS `priority`→DEFAULT, `causalReadRisky`→0 (grvFlags), `lockAware`→`cx->lockAware`,
tx-level `sizeLimit`→DB default, `tags`→empty, `snapshotRYWDisableCount`→DB default, then re-applies
ONLY the persistent options (timeout/retry_limit/max_retry_delay/auth_token, `persistent="true"` in
`fdb.options`). Go's `reset()` (`transaction.go:2481`, comment ~`:2528`) instead PRESERVES
priority/causalReadRisky/lockAware/readLockAware/sizeLimit/tags/snapshotRYWDisableCount — the comment
asserts this "matches C++", which `cloneAndReset` disproves.

Wire-visible on the retry: a transaction-level `SetPriorityBatch`/`SetCausalReadRisky`/`SetLockAware`
keeps sending its flags on the retry GRV/commit where libfdb_c reverts to the DB default.
**Why an RFC, not a grind fix:** the faithful fix re-seeds the tx-level options from the DB defaults on
reset (factor out CreateTransaction's seeding, call it from reset, preserve only the 4 persistent
options) — a change to the hot retry path with per-option DB-default subtleties (lockAware→cx default,
not false; causalReadRisky consistency), and the existing code deliberately chose the wrong behavior, so
it needs FDB-C-dev design review. Pin with a unit test (set a tx-level option → reset → assert reverted
to DB default; persistent options survive).

**Other options-audit findings (silent no-ops where C++ acts — `fdb/options.go`):** `REPORT_CONFLICTING_KEYS`
(sets `commit.report_conflicting_keys`; Go field exists at `committransactionref_generated.go` slot 4
but always false), transaction `TAG`/`AUTO_THROTTLE_TAG` (never populate the GRV/commit/read `Tags`
slot — tag throttling non-functional; also no `tag_too_long`/`too_many_tags` validation),
`READ_SERVER_SIDE_CACHE_*` + `READ_PRIORITY_*` (set `ReadOptions.cacheResult`/`.type`; Go no-ops),
`INITIALIZE_NEW_DATABASE` (forces readVersion=0), `USE_PROVISIONAL_PROXIES` (GRV flag bit 2). Per the
conformance principle, the silently-ignored ones should at least LOUDLY reject (UnsupportedOptionError)
rather than no-op — but each is a small feature, scoped separately.

**GRV / read-version audit (same grind) — NO consistency divergence found** (version-vector is OFF by
default, `ServerKnobs.cpp:39`, so Go's empty `ssLatestCommitVersions`/`maxVersion` is exactly correct;
read-version reuse, `read_snapshot`, 1007 aging all match). Latency/observability findings only:
- **Write-only commits omit `CAUSAL_READ_RISKY` on the commit-path GRV.** C++ `tryCommit` does
  `startTransaction(GetReadVersionRequest::FLAG_CAUSAL_READ_RISKY)` (`NativeAPI.actor.cpp:6578`) — a
  write-only/no-prior-read commit doesn't need full causal consistency for its `read_snapshot`. Go's
  commit path (`transaction.go:1507`) calls plain `ensureReadVersion` → `grvFlags()`, setting the flag
  only if the USER did. Effect: an extra TLog epoch-confirmation round-trip per write-only commit
  (latency/throughput, NOT consistency — the read_snapshot is equally valid). **Infra implication, why
  not a grind fix:** Go's `grvBatcherIndex` keys batchers only on the PRIORITY mask, NOT the risky flag
  (unlike C++'s `readVersionBatcher`, keyed by full flags) — so adding the flag would mix risky/non-risky
  GRVs in one batch. The faithful fix re-keys the GRV batcher on the risky flag + threads it through the
  commit-path `ensureReadVersion`; deliberate, FDB-C-dev-reviewed.
- `SetReadVersion` accepts `v<=0` / double-set silently where libfdb_c `setVersion` throws →
  `CATCH_AND_DIE` aborts the process (`NativeAPI.actor.cpp:5519`, `fdb_c.cpp:932`). Go's graceful
  defer-to-1007 is arguably BETTER (no panic in library code per CLAUDE.md) — leave as a documented,
  intentional divergence, don't copy the abort.
- Dropped GRV-reply observability (no consistency impact): `proxyTagThrottledDuration` (the
  `getTagThrottledDuration()` accumulator), the `metadataVersion` reply cache (Go does a real read of
  `\xff/metadataVersion` — correct, one extra round-trip), `midShardSize` (no clear-range cost estimator).

**Minor OnError/knob-audit findings (same grind, low priority — note, don't necessarily fix):**
hedge `secondDelay` uses a fixed `2.0×primary-latency` where C++ uses a runtime-adaptive
`secondMultiplier (≥1.0) × second-best latency + BASE_SECOND_REQUEST_TIME(0.5ms)`
(`loadbalance.go:70` vs `LoadBalance.actor.h:560`; p99 hedge timing only); GRV batcher lacks C++'s
`MAX_BATCH_SIZE=1000` force-flush (`NativeAPI.actor.cpp:7351`; >1000 concurrent GRVs/window wait the
full window); GRV `batchTime` floors at 100µs where C++ has no floor.


- [ ] **Port libfdb_c's BYTE-target streaming-mode model (`GetRangeLimits{rows, bytes}`).**
  `StreamingMode` is modelled here as a per-fetch ROW budget; in libfdb_c every mode is a per-fetch
  BYTE target. `bindings/c/fdb_c.cpp:1002` is one unified byte table — `SMALL` 256 B, `MEDIUM`
  1000 B, `LARGE` 4096 B, `SERIAL` 80000 B, `WANT_ALL`→`SERIAL` — and `:1006` gives `ITERATOR` the
  progression `{4096, 6144, 9216, 13824, 20736, 31104, 46656, 69984, 80000, 120000}`, clamped at
  `:1019` (`iteration = std::min(iteration, max_iteration)`). Go models those four modes as
  10/100/1000/500 rows and `ITERATOR` as a doubling row count.

  The *saturation* half of `ITERATOR` is already ported (`pkg/fdbgo/fdb/range_result.go`,
  `iteratorMaxIteration`): the row progression now stops growing at the same iteration index C++
  stops at, which is what bounded per-fetch memory (it was Θ(rows) — 2M rows held 951426 entries in
  one fetch). What remains is the **model** itself, and the residual divergence is a row-size
  asymmetry, in BOTH directions around the ~120 B record where the two models agree:
    - at ~20 B records, C fetches ~6000 rows/batch where Go fetches 1024 → ~6x the round trips;
    - at 10 KB records, C fetches 12 rows/batch where Go fetches 1024 → a ~10 MB single batch.
  So the bound Go now has is a bound on ROWS, not on BYTES; the byte dimension is covered only by
  the opt-in `WithRangeByteCeiling` (RFC-115 §2).

  **Trigger — do this on the FIRST of:**
    (a) a measured `ITERATOR` divergence from libfdb_c at a record size away from ~120 B, or
    (b) any work introducing a byte dimension (`target_bytes` / `limitBytes`) into the range path.
  Not speculative-scheduled: the current model is correct-but-differently-shaped, not wrong.

  **Scope when it fires** (this is why it is not a drive-by): the one-dimensional row budget runs
  through `getRangeImpl`, the RYW merge loop, and simfdb's parallel implementation, so all three
  must carry `GetRangeLimits{rows, bytes}` with a two-dimensional `isReached()`/`decrement()`.
  Changing batch boundaries moves every read-conflict range and every cursor continuation, so
  **RFC-121** (conflict extents are clamped to what a batch returned) and **RFC-098** (the
  unreadable-`reach()` verdict is keyed on the ROW limit — a byte-triggered early stop must not
  read as "did not reach") both need rework, and the record layer's continuation goldens re-checked.
  Note the regression net cannot pre-cover this: while Go's budget is rows and C's is bytes,
  per-fetch batch lengths cannot match, so a byte-identity differential on batch boundaries is not
  expressible until this lands — which is itself an argument for landing it.
  Client gate applies (FDB C++ dev + Torvalds); wants its own RFC.


- [ ] **C2-followup. RYW key-selector + read-version correctness audit (RFC-056).** Remaining
  RYW read-resolution divergences from libfdb_c surfaced by the RFC-055 differential:
  (2) a go-vs-cgo read-version
  staleness asymmetry (go=`transaction_too_old(1007)` while cgo succeeds on the SAME pinned read
  version near the 5s MVCC edge). **Characterized (RFC-056 #235): PERF/TIMING, not a wire/
  behavioral divergence** — both clients correctly return 1007 once a read version genuinely ages
  past the 5s window; go just reaches that edge sooner under CPU starvation because its getKey
  does more per call (the materializing `buildSegmentsLocked` vs libfdb_c's lazy iterator), and
  the differential pins one version then issues 28 selectors on it. So behavioral identity HOLDS;
  the real fix is the lazy iterator (continuation item 1 below), which reduces the per-call work
  at the source. The differential is already robust (retries the transient 1007 with a fresh
  version via the canonical `gofdb.IsRetryable` predicate — `differential_getkey_ryw_test.go`).
  REMAINING: profile go-getKey 1007-rate vs cgo to confirm item-1 closes it. See rfcs/055.
  - [x] **(1) `Transaction.GetKey` ignores pending writes** — FIXED (RFC-056): faithful port of
    C++ `resolveKeySelectorFromCache` over a merged segment view (`pkg/fdbgo/client/ryw_getkey.go`:
    `rywSegmentIterator`/`buildSegmentsLocked` + `getKeyRYW`'s unknown-range server-read-remerge
    loop), wired into `Transaction.GetKey` (+ the base↔resolved RANGE read-conflict, fixing the
    old single-key conflict) and `Snapshot.GetKey` (writes visible by default via
    `includeWrites=!snapshotRYWDisabled`). A merged-GetRange shortcut was verified-WRONG on
    `{orEqual, offset>1}` — not used. Pinned by `ryw_getkey_test.go` + the
    `TestDifferential_GetKeyRYW` differential (pending Set/Clear/ClearRange vs libfdb_c) + corpus
    seeds. **Two deferred sub-edges, same root** (the rywCache doesn't preserve per-key op-type
    — it eagerly folds resolved atomics into plain entries and moves a matched CompareAndClear
    into the cleared list; faithfully closing either needs a write-map that retains op-type, like
    C++'s):
    (a) **phantom offset slot** — a PENDING atomic that resolves to no value (CompareAndClear, or
    an atomic on a locally-cleared range) is modeled as absent; libfdb_c keeps it as a "phantom"
    is_kv slot COUNTED in the offset walk. The getKey differential is scoped to non-atomic pending
    writes until then.
    (b) **conflict-range filtering** — C++ `updateConflictMap` SUBTRACTS independent-write/cleared
    segments from the getKey read-conflict (no DB read there). Go keeps the FULL base↔resolved
    range: it OVER-conflicts on those segments (extra retries, always SAFE) rather than risk a
    missed conflict on a folded dependent atomic (an UNSAFE under-conflict — a naive
    `!hasAtomics` filter was attempted and reverted after codex showed it drops the conflict for a
    Get-folded atomic). The full range is strictly better than the old single-key conflict (which
    under-conflicted). Exact filtering deferred with the op-type preservation above.
  - [x] **RYW applyAtomic on present-empty values** — FIXED: the chain conflated `nil` (absent)
    with present-empty, so a V2 op after `Xor(k,"")` took the absent→operand path (`Min(k,"0")`
    → 0x30 vs libfdb_c 0x00). The get/merge chains now keep present-empty non-nil (nil reserved
    for absent), mirroring C++ `Optional.present()`. Pinned by
    `TestRYWGetRange_V2AtomicOnPresentEmpty`.
  - (3) **versionstamped-pending read = unreadable.** A SetVersionstampedKey/Value pending on a
    key reads as ABSENT in Go pre-commit (Get→nil, GetRange→omit); C++ marks it `is_unreadable`
    and THROWS `accessed_unreadable`. Go has no unreadable state — approximated as absent,
    consistently across ALL base states: storage-absent, locally cleared, a pending plain Set,
    and a non-nil storage value the pending stamp shadows. `atomic()` refuses to eager-fold a
    versionstamp into a plain entry, and `resolveAtomics` short-circuits the chain to
    `unresolved` (terminal, dominant over cleared) so both read paths exclude the key and drop
    any stale storage value. Pinned by `TestRYW_VersionstampedAbsentNoPhantom` +
    `TestRYW_VersionstampedOverClearedOrPlainNoPhantom`. Full C++ parity (THROW on read) still
    needs an explicit unreadable concept — part of the RFC-056 audit.


- [ ] **RFC-056 continuation — ordered, ONE AT A TIME (do 1, then 2, then 3).** After the merged
  getKey-RYW core (#235), three follow-ups remain. Both 1 and 2 WILL be done (sequentially, not
  batched); 3 is the ongoing hunt.

  1. **[x] DONE (RFC-057).** Lazy `rywSegCursor` replaced the materializing
     `buildSegmentsLocked`: getKey cost is now FLAT in cache size — **57 ms / 39 MB →
     1 µs / 816 B at N=100k (55,437×)**, measured before/after (Torvalds's "no benchmark =
     no merge" gate). Behavior-identical: a 4000-state equivalence property-test oracled
     against the retained materializer + the RFC-056 differential + a 94k-exec fuzz burst,
     all green. `next`/`prev` are a single merged-boundary `skip` (no view desync). The
     original plan:
     **Lazy/windowed segment iterator for getKey-RYW.** `buildSegmentsLocked`
     (`pkg/fdbgo/client/ryw_getkey.go`) MATERIALIZES the whole merged-segment partition of
     [allKeysBegin, maxKey) — O(writes + cacheKeys) per resolution attempt — whereas libfdb_c's
     `RYWIterator` is LAZY (a steppable zip of the write-map + snapshot-cache sub-iterators).
     Port the lazy cursor (skip/next/prev computing each segment on demand, no full
     materialization), so getKey cost is bounded by the walk distance, not the cache size. This
     ALSO shrinks **item (2)** below: less work per getKey under heavy parallel-container load →
     less likely to drift past the 5s MVCC window mid-loop → fewer transient
     `transaction_too_old(1007)`. Validate with a profiling probe: go-getKey wall-clock +
     1007-rate vs libfdb_c, before/after; confirm resolution stays byte-identical
     (`TestDifferential_GetKeyRYW` + unit tests green). Then this de-flakes item (2) at the source
     rather than only via the differential's retry.

  2. **[x] DONE (RFC-058).** Op-type-preserving write-map closed BOTH sub-edges. Added `absent`
     (phantom) + `dependent` (DEPENDENT_WRITE, carried unchanged through folds like C++
     `isDependent()` reading the immutable stack bottom) to `rywEntry`; a matched CompareAndClear
     now stays a write-map entry (never moved to `cleared`). The differential **disproved the
     original framing of (a)**: getKey is a limit-1 range read in C++ (`read(GetKeyReq)` =
     `getRangeValue`/`getRangeValueBack`), so a phantom is COUNTED in the offset walk but SKIPPED
     at the landing — not "counted and landed on." Modeled as `segPhantom` (count-in-walk +
     directional skip-at-landing); the old `segEmpty` under-counted for offset>1, a naive `segKV`
     wrongly landed on it. Also fixed a pre-existing fold-path bug the same differential caught
     (`doMax(_,"")`→nil misread as absent by a later CompareAndClear). (b) Ported `updateConflictMap`
     (ReadYourWrites.actor.cpp:335) as `conflictRangesLocked` — the getKey read-conflict now
     SUBTRACTS INDEPENDENT writes + cleared ranges (safe now that op-type is preserved; the naive
     `!hasAtomics` filter codex NAK'd on #235 is impossible here). Proof: getKey differential
     re-enabled for pending CAC/atomics + 92k-exec fuzz (sub-edge a); a deterministic commit-order
     `TestDifferential_GetKeyConflict` whose INDEPENDENT/CLEARED cases FAIL without the filter and
     pass with it (sub-edge b). FDB-C-dev + Torvalds ACK on the RFC. Original (a)/(b) text:
     (a) **phantom-slot offset counting** — a PENDING atomic that resolves to no value
         (CompareAndClear, or an atomic on a locally-cleared range) is an `is_kv` "phantom" slot
         COUNTED in the getKey offset walk in libfdb_c, but Go currently models it as absent. With
         op-type preserved, count it. (Re-enable pending-atomic shapes in the getKey differential.)
     (b) **exact `updateConflictMap` conflict filtering** — getKey's read-conflict should SUBTRACT
         independent-write + cleared segments (no DB read there); Go currently keeps the
         conservative FULL base↔resolved range (safe over-conflict). With op-type preserved, the
         subtraction is safe (a naive `!hasAtomics` filter was UNSAFE — it dropped the conflict
         for a Get-folded dependent atomic; codex caught it on #235 → reverted). Port
         `updateConflictMap` (ReadYourWrites.actor.cpp:335) faithfully and pin with a conflict
         differential (concurrent write inside the range must conflict identically in both clients).

  3. **Fresh differential axes (`/hunt-divergences`).** Probe axes still uncompared vs libfdb_c:
     atomic-op edge cases across ALL of `Atomic.h` (empty / missing / present-empty operand per
     op); error-code + option semantics (RAW_ACCESS / ACCESS_SYSTEM_KEYS / snapshot-RYW); key
     encoding / tuple packing / versionstamp-offset validation. Each closed axis is more "absolute
     proof we're identical to the C client."
     - [x] **[RFC-059 — MERGED #238] RYW-disable-after-op poison.** Differential characterization
       corrected the earlier (imprecise) framing: NOT a per-read overlap check, NOT an
       option-set-time error. libfdb_c's `setOption(READ_YOUR_WRITES_DISABLE)` after any read or
       write throws `client_invalid_operation` deferred via `deferredError`, so the option call
       succeeds but EVERY subsequent op (regular + snapshot reads/GetKey, GetRange, GetReadVersion,
       GetEstimatedRangeSizeBytes, GetRangeSplitPoints, Commit) returns 2000 — the whole txn is
       poisoned. Go was silently permissive (returned 0). RFC-059 ports the poison
       (`Transaction.rywPoisonErr` set on disable-after-op, gated uniformly at `ensureReadVersion` +
       the metrics path; a `hadRead` signal covers the GetPipelined non-caching read). Pinned by
       `TestDifferential_RYWDisableAfterOp` + `TestCommit_RYWPoisonBeatsTimeout`. Reviewed by
       FDB-C++ dev + Torvalds + codex + @claude.
     - [x] **[RFC-060 — MERGED #239] tuple-codec byte-identity differential.** The tuple/key encoding is the wire
       hard line but has ZERO differential coverage vs libfdb_c's codec. `pkg/fdbgo/fdb/tuple` is a
       near-verbatim port (core encode/decode byte-identical by inspection) but adds go-only
       hot-path helpers (`PackWithPrefix`/`Pack1WithPrefix`/`Pack1ConcatWithPrefix`/
       `PackConcatWithPrefix`/`Packer.AppendInto`/`packerPool`) absent from libfdb_c that build the
       actual index/record keys on the wire. Prove `gotuple.Pack() == cgotuple.Pack()` across all
       type codes + boundaries (int size-limit boundaries, big.Int >8 bytes + leading-0xff
       zero-fill, float/double sign-bit flip, nil-escaping in bytes/strings/nested, versionstamp
       offset), the go-only helpers vs canonical `cgotuple.Pack()`, cross-client Unpack, and an
       end-to-end FDB wire round-trip. cgotuple is itself pinned to the cross-language
       `tuples.golden`, so this transitively pins go to the golden vectors.
     - [x] **[RFC-061 — MERGED #240] SNAPSHOT_RYW_ENABLE/DISABLE counter.** Found via the
       transaction-option-semantics survey, confirmed differentially: libfdb_c models snapshot
       RYW as an integer counter (ENABLE++, DISABLE--, bypass iff <=0, default 1), but Go used a
       boolean with `SetSnapshotRywEnable()` a no-op — so `disable→enable` left snapshot reads
       stuck bypassing RYW (go silently too permissive). Fixed: `snapshotRYWDisableCount int`
       (zero-value-safe inverse: DISABLE++, ENABLE--, bypass iff >0; preserved across reset as a
       persistent option). Pinned by `TestDifferential_SnapshotRYWReenable` (10 sequences, 3
       red→green + a counter-vs-boolean discriminator + negative-count axis + RYW-disable
       dominance).
     - [x] **[RFC-062 — MERGED #241] atomic-fold width/edge differential.** Atomic fold semantics
       are the wire hard line; the existing differential only used 8-byte operands on missing keys.
       Added a differential across operand/base widths + edge operands for all 12 ops. KEY finding
       (teeth-check): tx.Set/tx.Atomic ship RAW mutations (server folds at commit), so Go's
       client-side fold (doAdd/doMin/…) runs ONLY on in-txn reads — a commit-then-read-back test
       passed even with doAdd broken. Restructured to read WITHIN the txn (exercises the fold) +
       committed read-back (server fold/wire). Verify-and-pin (fold is a faithful port); teeth
       confirmed on doAdd (6 rows) + doByteMin (4 rows). Found+fixed a test-isolation bug (go/cgo
       shared a key → missing-key fold saw the other's committed value).
     - [x] **[RFC-063 — MERGED #242] versionstamp-mutation differential.** SetVersionstampedKey/Value
       were excluded from the fuzz differential; only a Go-only interop check + an offset-0 Value
       case existed. Added masked (10-byte stamp zeroed) go-vs-cgo differentials: VersionstampedKey
       (offset 0 / after-prefix / mid-key / binary), VSValueOffsets (non-zero offsets), tuple
       PackWithVersionstamp (offset + 2-byte user-version preservation), GetVersionstamp parity
       (10-byte, == materialized stamp), error/boundary (tight-valid offset+10==body vs off-by-one
       reject, negative, past-body, too-small, empty → 2000 go==cgo), multi-op. Mask offset is
       template-derived + length/surround/non-zero guards (Torvalds). Teeth: loosening
       validateVersionstampOffset by 1 reddens offbyone_reject. The differential CORRECTED a
       reviewer assumption: two versionstamped ops in one txn get the SAME stamp (txn-level, not
       per-op batch id; user differentiates via tuple user version).
       - [x] **Follow-up (tenant +8 offset) — DONE + found a BIGGER bug.** Built the tenant
         differential harness in `pkg/fdbgo/bench` (`differential_tenant_test.go`: shared tenant on
         both clients; TenantVersionstampedKey masked read-back + raw full-key +8 assertion,
         TenantVersionstampedValue value-offset-NOT-adjusted, TenantVersionstampErrors boundary).
         The +8 offset adjustment (`commitpath.go`) was already correct (go==cgo). But the harness
         immediately surfaced a REAL cross-client wire-compat divergence: the tenant `nameIndex` and
         `lastId` are `TupleCodec<int64_t>` (minimal-width); `tenant_crud.go` hard-coded the fixed
         9-byte form (`0x1C`+8) for both pack AND unpack, so a Go client could NOT open/list/delete a
         tenant created by libfdb_c/Java (`OpenTenant` failed "expected 9 bytes, got 2"), nor create
         a tenant after one (couldn't decode the C-written `lastId`). Fixed the codec to FDB's real
         minimal-width tuple-int encoding (Tuple.cpp:204-227); reads legacy 9-byte values too.
         Pinned by `TestDifferential_TenantCrossClientCRUD` (go↔cgo create/open/write/read/list) +
         `tenant_crud_internal_test.go` (FDB-spec vectors, round-trip, legacy decode, errors).
     - [x] **[RFC-064 — MERGED #243] explicit conflict-range API differential.** AddReadConflictRange/
       Key + AddWriteConflictRange/Key feed the resolver (isolation) but had no differential coverage
       (RFC-058 covered only getKey-DERIVED conflict ranges). Empirically NO divergence — edges
       (inverted→2005, empty→accept, oversized→accept) match go==cgo (the C++ NativeAPI source has no
       release inverted-check, but the C binding cgo uses returns 2005 — the differential is the spec,
       not the source). Pinned the conflict OUTCOME: read-conflict range/key (A fails 1020 iff probe
       inside, half-open r0 incl / r9 excl), write-conflict range/key (a concurrent reader fails iff
       inside A's write-conflict), snapshot-read-no-conflict, self-write+read-conflict. Reused RFC-058
       pinning (both A+B SetReadVersion(vSetup), transient→retry, fresh prefix/attempt, bounded) →
       flake-free (5 runs). Teeth: empty key-conflict range → key_exact_r5 diverges. Oversized
       committed-truncation is unobservable (keys > maxKeySize are unwritable).
     - [x] **[RFC-065] getKey boundary resolution — REAL BUG FIXED.** The existing
       getKey differentials cover the keyspace INTERIOR + clamp off-prefix results, masking the
       EDGES. A boundary probe found a real divergence: a BACKWARD selector (lastLess*) at/past
       maxReadKey (\xff) wrongly returned \xff itself instead of the greatest key < \xff. Root
       cause: resolveKeySelectorFromCache (ryw_getkey.go) short-circuited EVERY off-end seek to
       readThroughEnd, ignoring direction; C++ it.skip() clamps to the last segment and only sets
       readThroughEnd after the walk for offset>1. Fix: direction-aware off-end branch — forward
       keeps readThroughEnd; backward repositions onto the last segment and resolves backward.
       Pinned by TestDifferential_GetKeyBoundary (pinned-version differential: lastLess*(maxReadKey)
       asserted < maxReadKey, empty/large-offset/past-max edges). Teeth: re-introducing the
       unconditional shortcut reddens LLT/LLE_maxReadKey. Only the RYW path had it; rywDisabled
       delegates to the server.
     - [x] **[RFC-067 — MERGED #247] error-CODE differential → TRANSACTION_SIZE_LIMIT + 4 linked fixes.**
       A fresh error-CODE differential (`TestDifferential_ErrorCodes`, comparing the FDB error code
       each client returns for the same size/legal-range triggers) found a REAL write-path divergence:
       the Go client did NOT enforce `TRANSACTION_SIZE_LIMIT` by default — it committed >10 MB txns
       that libfdb_c rejects client-side with `transaction_too_large` (2101). C++ defaults every txn's
       sizeLimit to the 10 MB knob (NativeAPI:6133); Go's `0=disabled` default left no enforcement.
       Fix: default to the knob. Four more linked fixes surfaced via review + differential: (2) online-
       indexer lessen-work codes (Torvalds — wrong numbers, missing 2101, made latent-live by the
       limit; now matches Java `IndexingThrottle.lessenWorkCodes` 1:1); (3) commit-validation ORDER
       (codex — read-only fast path + per-mutation-before-size; then the full eager-vs-deferred model:
       key/value-size + versionstamp-offset are EAGER first-invalid-op-wins, txn-size DEFERRED; pinned
       by `TestDifferential_VersionstampValidationOrder`, 8 cases); (4) `metadataVersionKey` write
       contract (codex+FDB-C+++Torvalds — a blanket `continue` silently committed every illegal mvk
       mutation where libfdb_c returns 2000/2004; replaced with the exact C++ gate; pinned by
       `TestDifferential_MetadataVersionKey`, 8 cases); (5) size the VALIDATED snapshot not the live
       buffer (codex — a Set racing Commit could fail a small commit for an unshipped mutation; pinned
       by `TestApproximateCommitSize_SizesSnapshotNotLiveBuffer`). Also fixed a pre-existing
       differential-harness flake: pinned-version range reads now retry the transient 1007 (stale pin
       under parallel-container load) instead of `t.Fatalf` (pinned by
       `TestDifferential_PinnedRangeRetriesStaleVersion`). Reviewed clean by FDB-C++ dev + Torvalds +
       codex (per-commit deltas + full review) + @claude.


- [ ] **CQ-98 (LOW/MED, record-layer metadata — U4): Java rejects an unknown
  index type at METADATA VALIDATION; Go only refuses it on the WRITE path, so Go
  opens and reads a store Java would refuse to validate.** Found in #640's review
  lap, which landed the fail-closed maintainer dispatch that this entry is the
  remainder of.

  **Java:** `MetaDataValidator.validateIndex` calls
  `indexRegistry.getIndexValidator(index).validate(this)`
  (`MetaDataValidator.java:118`); an unregistered type has no validator and the
  registry raises `MetaDataException`. So the metadata is refused **before any
  record is read or written** — a store carrying an index type this build does
  not know is simply not a valid store.

  **Go:** the fail-closed dispatch (#640) refuses the unknown type when a
  maintainer is actually requested, i.e. on the write path. Reads and store
  opening succeed.

  **The direction is SAFE and that is why this is not urgent**: Go refuses
  strictly later than Java, never earlier, so Go cannot write something Java
  would accept. Nothing is corrupted by the gap. What diverges is **WHEN the two
  engines agree a store is valid** — Go will happily open, scan and serve reads
  from a store Java rejects outright at validation. An operator running both
  engines gets two different answers to "is this metadata valid", which is the
  same class of disagreement #640 closed for uniqueness, one layer up.

  DONE = metadata validation refuses an index type with no registered maintainer,
  at open/validate time, matching `MetaDataValidator.java:118`'s ordering; with a
  test that a store carrying an unknown index type fails to validate rather than
  failing later at first write. Check while there whether Go has a registry
  equivalent to hang the validator off, or whether the dispatch's type switch is
  the de facto registry — if the latter, that is the thing to give a validate
  entry point rather than inventing a second list that can drift from it.


- [ ] **`StreamingModeExact` materialises its ENTIRE row budget in one fetch,
  where libfdb_c splits the same read by byte target** · L · found while probing
  the (non-existent) multi-batch exact-mode path
  `batchSize` returns the whole `remaining` for EXACT
  (`pkg/fdbgo/fdb/range_result.go:199-200`), and `rangeScanImpl`
  (`pkg/fdbgo/client/readpath.go:669`) loops internally across shards until that
  budget is filled — it returns either a filled budget with `more=true` or an
  exhausted range with `more=false`, never a short batch with more pending. So a
  Go `Exact` iterator with `Limit: N` performs exactly ONE fetch and holds all N
  rows at once, at any N. MEASURED against real FDB at N up to 5000:
  `TestRangeIterator_ExactModeIsStructurallySingleBatch` records one batch for
  every limit tried, while the Iterator-mode control over the same seed takes 13.
  This is NOT a correctness divergence and must not be booked as one: rows, error
  codes and the union of per-batch read-conflict ranges all agree with libfdb_c.
  MEASURED by `bench:TestDifferential_LimitedIteratorMultiBatchRowSets`, whose
  `exact/600` arm compares Go's single-batch drain against the C client's and
  finds identical row sets. What differs is PEAK MEMORY and round-trip count:
  Θ(limit) here against a bounded per-fetch target in C — the same failure shape
  RFC-115 fixed for ITERATOR by porting the saturation clamp.
  CORRECTION, because the first version of this entry got the mechanism wrong and
  the wrong version is the more dangerous one to leave standing: it is NOT true
  that "there is no byte dimension in the Go batching path". A byte limit is on
  the wire on EVERY range request — `LimitBytes: replyByteLimit`
  (`client/readpath.go:1102`, constant `= 80000` at `readpath.go:27`,
  `CLIENT_KNOBS->REPLY_BYTE_LIMIT`) — and `parseGetKeyValuesReply` returns
  `reply.More` verbatim (`readpath.go:1134`), so the storage server does come back
  short with `More=true` when it hits that cap. The reason a byte boundary never
  reaches the iterator is that `rangeScanImpl`'s inner re-query loop
  (`readpath.go:720-820`) ABSORBS the truncation, re-querying the same shard until
  the ROW budget is filled, and only then returns. The distinction matters: "there
  is no byte dimension" invites someone to add one carelessly, whereas "the byte
  dimension exists on the wire and is swallowed one layer down" says exactly which
  loop to change.
  AND THERE IS A CONCRETE C DIVERGENCE IN FRONT OF THE ABSTRACT ONE. C derives
  `target_bytes` PER MODE from `mode_bytes_array` — SMALL 256, MEDIUM 1000, LARGE
  4096, SERIAL 80000, WANT_ALL→SERIAL, as `fdb/range_result.go:215-217` already
  records from `fdb_c.cpp:1002`. Go pins EVERY mode to 80000, because
  `StreamingMode` never reaches `pkg/fdbgo/client` at all (2 hits for
  `StreamingMode` in non-test sources under `pkg/fdbgo/client/`, both inside
  comments; control sweep for `rangeScanImpl` over the same files returns 6, so the
  command is well-formed). **A Go SMALL fetch asks the storage server for 80000
  bytes where C asks for 256.** Design principle 2 makes that a bug in Go, not a
  curiosity — it is a per-request load difference against a shared cluster.
  NOW MEASURED, not inferred. `libfdbc:TestLibFDBC_RangeBatchDivision` drives
  libfdb_c's own fetch loop over 60 rows of 200 bytes and records the division
  against the pure-Go model:
  ```
  small   libfdb_c [2]x30      | pure-Go [10]x6
  medium  libfdb_c [5]x12      | pure-Go [60]
  large   libfdb_c [18 18 18 6]| pure-Go [60]
  serial  libfdb_c [60]        | pure-Go [60]   <- positive control
  ```
  The per-fetch row counts land exactly where `mode_bytes_array` predicts for
  200-byte rows (256B→2, 1000B→5, 4096B→18), and SERIAL — the one mode whose C
  target equals the 80000 Go pins everywhere — agrees, which is what proves the
  other three are a real per-mode divergence rather than a harness artifact.
  Fixing it means plumbing the streaming mode down into `pkg/fdbgo/client` and
  giving `rangeScanImpl` a two-dimensional budget, which moves every batch
  boundary, hence every read-conflict range and every cursor continuation, across
  the client, simfdb and the record layer. That is an RFC + client-review-gate
  change (FDB C++ dev + Torvalds), not an inline fix, which is why it is booked
  rather than done.
  DONE = the byte dimension ported through `batchSize`/`rangeScanImpl`/simfdb so
  no single fetch exceeds a byte target, with the existing batch-division pins
  updated in the same change and a differential showing row sets still match C.


- [ ] **The cgo BACKEND's iterator still reports no batch boundaries, though the
  measurement capability now exists** · S · found while designing the exact-mode
  batch-boundary probe
  RESOLVED HALF, recorded because the original booking claimed a blocker that was
  smaller than stated and has since been removed: `CGetRangeBatch`
  (`pkg/fdbgo/libfdbc/range_cref.go`) now issues a single raw
  `fdb_transaction_get_range` with `target_bytes` and `iteration` exposed, and
  `TestLibFDBC_RangeBatchDivision` drives that loop to measure exactly where
  libfdb_c splits a range. C's division is no longer unobservable, and the concrete
  per-mode byte divergence it revealed is recorded in the item above.
  WHAT REMAINS is narrower and is a different kind of change: the cgo BACKEND's
  production iterator (`libfdbc/backend.go:668-671`) still implements `SetTraceLog`
  as a no-op, because it delegates to `cgofdb.RangeIterator`, which buffers a
  single KV and has no batch to report. Making it real means replacing that
  delegation with a hand-rolled loop over `CGetRangeBatch` — putting a new, less
  exercised read path in front of every caller of the cgo backend purely to serve
  observability. That is a production client-path change and takes the
  client-review gate (FDB C++ dev + Torvalds); it was deliberately NOT bundled with
  the reference-side binding, which touches nothing in production.
  DONE = either the cgo backend's iterator is driven by `CGetRangeBatch` with a
  real `SetTraceLog` and the row-set differentials re-run against it, or the no-op
  is documented as permanent with the reference-side instrument named as the
  supported way to observe C's division.
  The residual gap this leaves in the everywhere-running differential is stated at
  its head (`bench:TestDifferential_LimitedIteratorMultiBatchRowSets`): two clients
  could agree on every row while dividing it differently and it would pass. That is
  now covered by the tag-gated division test rather than being unmeasurable.


- [ ] **Port `GetRangeLimits{rows, bytes}` and `isReached()` into the pure-Go
  range path, so batch division matches libfdb_c per streaming mode** · L ·
  the fix for the divergence measured by `libfdbc:TestLibFDBC_RangeBatchDivision`
  THE DIVERGENCE. libfdb_c derives a per-mode BYTE target and ends a range call
  when either dimension is satisfied. The pure-Go client has no byte dimension in
  that decision, so it divides by rows alone and issues a different number of
  round trips with different batch boundaries. Measured, same seed, 60 rows of 200
  bytes:
  ```
  small   libfdb_c [2]x30       | pure-Go [10]x6
  medium  libfdb_c [5]x12       | pure-Go [60]
  large   libfdb_c [18 18 18 6] | pure-Go [60]
  serial  libfdb_c [60]         | pure-Go [60]   <- agrees only because SERIAL's target IS 80000
  ```
  Design principle 2 makes this a bug in Go. It is NOT merely a round-trip-count
  difference: batch boundaries are where each per-batch read-conflict range is
  taken (RFC-121) and where a cursor continuation lands, so an abandoned scan
  conflicts over a different extent in the two clients.
  THE SPEC, read rather than inferred (7.3.77):
  - `bindings/c/fdb_c.cpp:1002` — `mode_bytes_array[] = { BYTE_LIMIT_UNLIMITED,
    256, 1000, 4096, 80000 }`, indexed EXACT=0, SMALL=1, MEDIUM=2, LARGE=3,
    SERIAL=4.
  - `:1006` — `iteration_progression[] = { 4096, 6144, 9216, 13824, 20736, 31104,
    46656, 69984, 80000, 120000 }`, "Goes 1.5 * previous".
  - `:1009,1019` — `max_iteration` is the table length (10) and `iteration` is
    CLAMPED to it before indexing. Easy to miss; ITERATOR is the arm most likely
    to be subtly wrong because its target depends on the iteration number, which
    the Go range path does not currently thread anywhere.
  - `:1011` — WANT_ALL maps to SERIAL.
  - `:1017` — ITERATOR with `iteration <= 0` is `client_invalid_operation`.
  - `:1026-1029` — an explicit `target_bytes` is combined as
    `min(target_bytes, mode_bytes)`; unset takes `mode_bytes`.
  - `fdbclient/ClientKnobs.cpp:66` — `REPLY_BYTE_LIMIT` 80000 is the per-REPLY
    CEILING, not the per-mode target. Conflating the two is the present bug:
    `client/readpath.go:1102` sends the ceiling as the limit for every mode.
  WHAT A REQUEST-ONLY CHANGE WILL NOT DO, measured so nobody repeats it: setting
  `LimitBytes` per mode on the request, BY ITSELF, changes nothing observable.
  `rangeScanImpl` absorbs a truncated reply and re-queries the same shard until the
  ROW budget is filled, so the request's byte limit never reaches the division.
  Dropping `replyByteLimit` from 80000 to 256 — below SMALL's own target — left
  every division byte-for-byte identical.
  BUT THE CONCLUSION FIRST DRAWN FROM THAT — "so the port must reach
  `rangeScanImpl` and `batchSize`, NOT the request builder" — IS REFUTED, and the
  refutation is pinned by `libfdbc:TestLibFDBC_ByteTargetCutIsNotTheClientSideBudget`.
  The request half is not optional; it is where the boundary is actually chosen.
  C++ applies the budget in TWO cooperating places, and the port needs both:
  1. ON THE REQUEST. `transformRangeLimits` (`NativeAPI.actor.cpp:4223`) sets
     `req.limitBytes = min(REPLY_BYTE_LIMIT, limits.bytes)`, and it is called from
     BOTH range loops — `getExactRange:4299` and `getRange:4681`. The STORAGE
     SERVER truncates the reply there, and the server's own reply-size accounting
     is what fixes the batch boundary.
  2. IN THE LOOP, only to END the call — the soft byte limit at
     `getExactRange:4415`, `if (limits.hasSatisfiedMinRows() && output.size() > 0)`.
     `hasSatisfiedMinRows() = hasByteLimit() && minRows == 0` (`:2875`) and
     `minRows` starts at 1 (`FDBTypes.h` `GetRangeLimits` ctor), so ANY non-empty
     reply satisfies it. With a byte target set, one range call is exactly ONE
     server reply. `isReached()` is NOT what divides these batches.
  WHY IT MATTERS RATHER THAN BEING A REFRAMING: the two rules round opposite ways.
  A client-side budget over an untruncated reply stops at the first row that drives
  the budget to zero (overshooting the target); the server stops at what fits.
  MEASURED over 60 rows of 200 bytes (12-byte keys, so 8+key+value = 220/row),
  sweeping `target_bytes` explicitly through one raw `fdb_transaction_get_range`:
  ```
  target_bytes  libfdb_c first batch   loop-only client-side model
  256           2                      2
  1000          5                      5
  4096          18                     19   <- disagree
  8192          35                     38   <- disagree
  ```
  A loop-only port is off by one at LARGE's own 4096 target and by three at 8192.
  So Go cannot reproduce C's boundary by MODELLING it — the server's accounting is
  not the client's. It must send the same per-mode `LimitBytes` and let the same
  storage server apply the same rule. This makes the port SMALLER than booked, not
  larger: no per-row `decrement` budget threaded through five layers, just the
  per-mode target on the request plus "stop after the first non-empty reply when a
  byte limit is set".
  This also explains EXACT for free, rather than as a special case: `mode_bytes_array[EXACT]`
  is UNLIMITED, so `hasByteLimit()` is false, so the soft-byte-limit early return
  can never fire and the loop absorbs across replies exactly as it does today.
  EXACT IS UNAFFECTED and must stay single-batch: `mode_bytes_array[EXACT]` is
  UNLIMITED, so EXACT has no byte target and remains row-bounded. C behaves the
  same (measured: one call returns all 100 rows across 97 KB). If this port makes
  `fdb:TestRangeIterator_ExactModeIsStructurallySingleBatch` or
  `libfdbc:TestLibFDBC_ExactModeAbsorbsByteCappedReplies` fail, the port is wrong —
  they are not casualties to be updated.
  BLAST RADIUS, stated so it is not discovered halfway: mode and iteration must be
  threaded from `fdb.goRangeIterator` through `doRangeWithLimit`,
  `client.Transaction.GetRange`/`getRangeDir`, `rangeScanImpl` and the RYW layer
  (`client/ryw.go`), all of which currently carry a one-dimensional row budget.
  `pkg/simfdb` must mirror it exactly or the simulator stops standing in for the
  client. Every batch boundary moves, hence every per-batch read-conflict range and
  every cursor continuation, so record-layer cursor tests are in scope.
  THE ENUM OFFSET, which is the landmine in this port. `mode_bytes_array` is
  indexed by the **C** streaming-mode numbering, and Go does not use it:
  ```
  C   (fdb_c_options.g.h:526-544):  WANT_ALL=-2 ITERATOR=-1 EXACT=0 SMALL=1 MEDIUM=2 LARGE=3 SERIAL=4
  Go  (fdb/range.go:93-116):        WantAll=-1  Iterator=0  Exact=1 Small=2  Medium=3  Large=4  Serial=5
  ```
  Go's value is the C value PLUS ONE. Indexing `mode_bytes_array` with a Go mode
  silently hands SMALL the MEDIUM target (1000 instead of 256), MEDIUM the LARGE
  target, LARGE the SERIAL target, and reads out of bounds for Serial. It would
  compile, run, and produce a division that is wrong in a way the existing
  row-count assertions cannot see. Convert once, at one named site, and unit-test
  the conversion against both tables rather than open-coding `-1` at each use.
  THE PRIMITIVES, so they are not re-derived from the batch counts:
  - `GetRangeLimits{rows, bytes, minRows}`; `isReached() = rows == 0 || (bytes == 0
    && minRows == 0)` (`NativeAPI.actor.cpp:2856`).
  - per-row byte accounting is `bytes -= 8 + key.size() + value.size()`, floored at
    0 (`:2823` single-KV form). The 8 is a fixed per-row overhead and is NOT
    optional — dropping it inflates every batch.
  - `reachedBy` (`:2861`) is the look-ahead form used to stop BEFORE appending.
  - `hasSatisfiedMinRows() = hasByteLimit() && minRows == 0` (`:2875`); `minRows`
    is what stops a byte-limited request from returning zero rows, and RYW derives
    its per-request limit with it (`ReadYourWrites.actor.cpp:580-597`).
  STAGING AND CURRENT POSITION. The order is DIFFERENTIAL FIRST, PORT SECOND, for
  both halves: build the cross-client safety net while the code is still unchanged,
  prove it reds against a deliberately wrong merge, and only then change production.
  A net built after the change cannot tell you the change was safe.
  - [x] RYW differential LANDED — `libfdbc:TestLibFDBC_RYWRangeUnderByteLimitDifferential`.
    Drains both clients to exhaustion under each per-mode byte target and compares
    each against an INDEPENDENT Go model (not against each other, which is the
    paired-equality trap). Six scenarios — no_local_writes, shadow, extend,
    delete_keys, clear_range, mixed — each forward AND reverse, since C++ implements
    the two directions as separate functions. Fat 300-byte rows so SMALL's 256-byte
    target is exceeded by one row, which is also the `minRows` case: the test asserts
    no fetch returns zero rows before exhaustion. PROVEN DISCRIMINATING: dropping the
    last local write from the merge window reds shadow/extend/mixed and leaves
    delete_keys/clear_range green; disabling the cleared-key filter reds
    delete_keys/clear_range/mixed and leaves shadow/extend green. Disjoint arms.
  - [x] Server-path division measurement LANDED —
    `libfdbc:TestLibFDBC_RangeBatchDivision` (C's division per mode) and
    Go's own division, asserted then as row-driven and size-invariant — the
    divergence stated as currently-true. That test has since been rewritten by the
    port and RENAMED to `fdb:TestRangeIterator_DivisionIsByteDrivenNotRowDriven`;
    the old name no longer exists in any Go source, so a `--test.run` filter on it
    would match nothing and report green.
  - [x] PORT, server path — LANDED. Per-mode `target_bytes` onto the request's
    `LimitBytes` (`min` with `replyByteLimit`, porting `transformRangeLimits`) PLUS
    the soft-byte-limit early return in `rangeScanImpl`, so a byte-limited call stops
    after the first non-empty reply instead of absorbing and re-querying. Both halves,
    per the refutation above; each alone is measurably wrong (request-only divides
    `[60]`, loop-only divides LARGE as 19 where C divides 18).
    CONVERGED, measured against libfdb_c over 60 rows of 200 bytes — SMALL `[2]x30`,
    MEDIUM `[5]x12`, LARGE `[18 18 18 6]`, SERIAL `[60]`, ITERATOR `[18 26 16]`, and
    bounded reads (`medium/250 = [24 x10, 10]`, `small/25 = [7 7 7 4]`). EXACT stays a
    single batch as AGREEMENT with C, not a Go limit: `mode_bytes_array[EXACT]` is
    UNLIMITED so the soft limit can never fire.
    THE ENUM OFFSET is handled at one named site, `fdb.cModeIndex`, unit-tested against
    both numberings; mutating it to drop the `-1` reddens with SMALL reading MEDIUM's
    target, the exact silent failure the booking predicted.
    THREE BUGS FOUND DURING THE PORT, all by tests rather than by review:
    (1) the iterator derived its byte target from `iteration+1` while its counter was
    already 1-based, so the first fetch targeted 6144 and `iterationProgression[0]`
    (4096) was unreachable by any real scan — pinned now by
    `fdb:TestRangeIterator_FirstFetchUsesFirstProgressionEntry`, the dimension no
    existing test could see (the differential drives its own iteration loop; the
    saturation test asserts only relative shape);
    (2) `pkg/simfdb` applied the byte cut AFTER `fetchRange` returned, so `more`
    described the untruncated read and the unreadable-cap predicate raised 1036 on the
    first fetch having yielded nothing — the cut now happens inside `fetchRange`;
    (3) the row budget handed down became the whole remaining limit, which left
    simfdb's view build unbounded and a saturated drain quadratic — bounded now by
    `byteTarget/minRowCost`, a guard that provably cannot move the boundary.
    `fdb.BatchSize` was DELETED rather than left unused: an exported function still
    handing out the removed per-mode ROW page is an unwatched revival, since a future
    caller would reinstate row batching with every division test still green.
    THE SERVER'S ACCOUNTING, measured because it is not the client's and cannot be
    derived from it: a reply accumulates rows until `key+value+24` reaches the target,
    INCLUDING the row that crosses. Fits all 10 cross-client measurements across 4 row
    shapes; recorded as `serverRowOverheadBytes` in `pkg/simfdb/range_result.go`, which
    is the only place that has to model it — the real client sends the target and lets
    the real server apply its own rule.
  - [ ] PORT, RYW path — byte accounting into the merge helpers, guarded by the
    differential above.
  WHERE THIS STANDS: the SERVER-PATH half is LANDED (see its box above) and the RYW
  half is not started. Production files ARE modified — the per-mode target now
  reaches the request builder, `rangeScanImpl` carries the soft byte limit, and
  `pkg/simfdb` mirrors both. The next person picks up at the first unchecked box,
  which is the RYW merge path, with the net already standing.
  WHY THE SPLIT: applying the byte limit in the SERVER
  path (`rangeScanImpl`) alone already converges the no-local-writes case, which is
  what the division tests exercise. The RYW merged path (`client/ryw.go`, mirroring
  C++'s separate forward/reverse implementations at
  `ReadYourWrites.actor.cpp:597-899` and `:900-1230`, each with byte-limit early
  exits at `:693` and `:1000`) is the harder half and the one carrying real risk:
  its row-only merge helpers (`applyLimitAndDirection`, `computeMore`,
  `limitReached`, `cacheWalkBudget`) decide what a read RETURNS, not merely how it
  is divided, so a wrong byte accounting there is silent read-your-writes
  corruption rather than a visible batching difference. Stage it separately and
  test it against local writes straddling a byte boundary.
  DONE = `TestLibFDBC_RangeBatchDivision` asserts the two clients AGREE per mode
  AND asserts the literal expected counts (`[2]x30`, `[5]x12`, `[18 18 18 6]`) —
  an equality check alone is vacuous once both sides derive from one table;
  the Go-side division test is rewritten against the C table rather than relaxed
  (and renamed accordingly — `fdb:TestRangeIterator_DivisionIsByteDrivenNotRowDriven`);
  each mode arm is mutation-checked and the arms redden disjointly. Client-review
  gate (FDB C++ dev + Torvalds) applies.

### The metadata builder diverges from Java in three places, found while closing RFC-238 §7f

These are one subsystem and should land as one PR. All three were surfaced by
review during PR #761 and verified against the Java source at tag 4.12.11.0;
none is caused by that PR, and none is blocked on anything.

**1. `updateRecords()` is not ported, so a descriptor cannot be evolved at all.**
`SetRecords` now refuses a second call, matching Java
(`RecordMetaDataBuilder.java:384`, `:423`). Java's sanctioned way to change a
descriptor afterwards is `updateRecords(FileDescriptor)` /
`updateRecords(FileDescriptor, boolean processExtensionOptions)`
(`RecordMetaDataBuilder.java:451`, `:476`), plus
`FDBMetaDataStore.updateRecords` / `updateRecordsAsync`. It validates the new
descriptor, runs the evolution validator over the union
(`evolutionValidator.validateUnion`), bumps the meta-data version, adds new
record types and updates existing message descriptors via
`updateUnionFieldsAndRecordTypes`, sets `sinceVersion` on the new types and
their indexes, and finally swaps `recordsDescriptor`/`unionDescriptor`. Go has
none of it. The gap PREDATES the guard — Go never had `updateRecords` — but the
guard removes the broken approximation a caller could previously stumble into,
so the gap is now the only story.

**2. `primaryKeyComponentPositions` is computed for indexes Java never computes
it for, and nondeterministically.** Java's `build()` sets it only from
`recordTypeBuilder.getIndexes()` — single-type indexes
(`RecordMetaDataBuilder.java:1461-1468`); `addMultiTypeIndex` with >=2 types
puts the index on `getMultiTypeIndexes()` (`:1167-1178`) and `addUniversalIndex`
into `universalIndexes` (`:1184`), neither of which that loop reads, so both
keep `null` and `Index.trimPrimaryKey` is a no-op — Java writes
`valueKey + FULL primary key`. Go's `metadata.go` Build computes positions for
`rt.indexes`, `rt.multiTypeIndexes` AND `b.universalIndexes`, and for the
universal case draws the primary key from `for _, rt := range types { …; break }`
over a MAP, so the value is nondeterministic across builds of identical
metadata. WIRE IMPACT: for a multi-type index whose key contains a PK field
(two types keyed on `id` with a shared index on `(id, ts)`), Go writes
`[id, ts]` where Java writes `[id, ts, id]`. `RecordMetaDataFromProto` ends in
`builder.Build()`, so Go does this to metadata JAVA wrote. `pk_dedup_test.go`
documents the change as a bugfix ("full redundant PKs"), i.e. Go matched Java
and was "fixed" into the divergence. No conformance test uses a multi-type index
at all (`grep -rln AddMultiTypeIndex conformance/` → 0; control `AddIndex` → 5+
files), which is the dimension that let it ship green. The fix inverts two
`pk_dedup_test.go` arms and dissolves the lone `Skip` in
`index_registration_matrix_test.go:37`, whose universal arm becomes a
no-positions assertion instead of being skipped.

**3. `GetIndexesForRecordType` may be the wrong accessor at several call sites.**
It returns `rt.indexes + rt.multiTypeIndexes`. Java's `RecordType.getIndexes()`
is single-type only and `getAllIndexes()` adds multi-type AND universal
(`RecordType.java:90`, `:101`, `:118-124`); Go has the single-type analog as
`RecordType.GetIndexes()`. `IndexFunctionHelper.indexesForRecordTypes` returns
`getIndexes()` alone for ONE record type name — "the indexes that apply to
exactly the given types, no more, no less" (`IndexFunctionHelper.java:178-189`)
— so Go's `record_function.go:78` offers a BROADER candidate set than Java. The
save path composes `GetIndexesForRecordType` + `GetUniversalIndexes` explicitly
(`store.go:1079`) and is fine; the other non-test callers were not audited
against Java one by one. Audit all of them, then pick the Java-matching
accessor per site.

DONE when: `updateRecords` is ported with evolution-validator coverage;
`primaryKeyComponentPositions` is computed only where Java computes it, pinned
by a Go↔Java conformance pair for a multi-type index whose key overlaps a
primary key; and every `GetIndexesForRecordType` call site is either confirmed
against its Java counterpart or switched.

---


### Go refuses to load metadata Java loads fine: no synthetic-record-type arm in the proto reader

WIRE-COMPAT HARD LINE, pre-existing, found while reviewing RFC-238 §7f.

Java's proto loader walks each index's `getRecordTypeList()` and sorts every
name into one of THREE buckets
(`RecordMetaDataBuilder.java:183-215`): a record type, a SYNTHETIC record type
(joined or unnested), or unknown — and only the third throws. When the names
resolve to synthetic types it attaches the index to the synthetic builder
(`syntheticRecordTypeBuilder.getIndexes().add(index)`, `:204`) instead of
calling `addMultiTypeIndex`.

Go's `RecordMetaDataFromProto` (`pkg/recordlayer/metadata_proto.go`) has only
two buckets. A name that is not in `builder.recordTypes` is an error:

    unknown record type %q referenced by index %q

So any metadata Java wrote that carries an index over a joined or unnested
record type is REJECTED by Go on load. That is the wire-compat line this port
exists to hold — Go and Java share a cluster and must read each other's
metadata — and it fails in the direction that is hardest to notice, because a
Go-only deployment never produces such metadata and so never sees it.

Related, same file, same reader: synthetic record types are not built at all on
the Go side, so even with the arm added the index would need a synthetic type to
attach to. Check whether `pkg/recordlayer` models them before sizing this.

DONE when: a metadata proto carrying an index over a synthetic record type
round-trips through `RecordMetaDataFromProto` without error, pinned by a test
that fails with the current reader, and the cross-engine conformance corpus has
an entry that writes such metadata from Java and reads it from Go.

---


---

## 3. Cascades query engine — planner, cost model, rules, memo

Plan quality, rule fidelity, cost-model soundness and memo structure. Query-engine gate applies:
RFC → Graefe + Torvalds ACK → implement → one review lap per milestone.

- [ ] **CQ-10f (MED, full-scan regression CLOSED by CQ-20 — sort-elimination
  parity still open; Graefe ruling OBTAINED, option (ii)/diverge
  deliberately, gated on conditions A–F with the FDB benchmark (E) able to
  flip it) — `WHERE pk IN (...) ORDER BY pk DESC` planned a FULL
  TABLE SCAN where Java plans N bounded seeks.** Introduced by CQ-10d and left
  deliberately unpinned there rather than blessed.
  ```
  SELECT * FROM tbl WHERE id IN (1, 2, 3) ORDER BY id DESC
    before CQ-10d:  InMemorySort([ID DESC], InJoin(Scan(TBL, [=]), binding))   -- 3 bounded seeks + sort
    after  CQ-10d:  PredicatesFilter(Scan(TBL) REVERSE, [1 preds])             -- FULL TABLE SCAN
    Java:           [IN arrayDistinct(...) SORTED DESC] | INJOIN q0 -> { SCAN([IS TBL, EQUALS q0]) }
  ```
  Schema `tbl(id, k, a, b, PRIMARY KEY (id, k))`, `INDEX ia ON tbl(a)`. Planner
  default statistics, so the choice is SIZE-INDEPENDENT — the same full scan on
  a billion-row table. The Java line is MEASURED against the live fdb-relational
  planner through the conformance `SqlPlanSteps` harness, not reasoned: Java
  plans the IN-join with its bindings sorted descending, i.e. the three seeks
  with no sort at all. So this is a Go-vs-Java divergence, not the Java shape.
  **The deciding rung, measured, is NOT the one it looks like.** Criterion #2
  abstains (both whole-plan maxima unknown), residuals tie 1-1, and criterion #4
  DATA-ACCESS COUNT ties 1-1 — an IN-join executing one bounded inner scan per
  binding counts one data access, exactly like one unbounded full scan. Control
  then reaches `comparePrimaryScanVsIndexScan`, whose FIRST line
  (`primaryVsIndexRankOf`, `planning_cost_model.go`) ranks any plan carrying an
  in-memory sort into its last tier — so criterion #7 returns, and the promoted
  `inMemorySortCount` rung immediately below it never runs. The two rungs encode
  the same premise and #7's own comment says so, so the substance of "the sort
  rung breaks the tie" is right while the mechanism is not; a fix aimed at the
  sort-count rung alone would miss.
  **The premise that fails** is the one both rungs share: *once two candidates
  have done provably the same amount of real work, the one carrying an extra
  full materializing sort is strictly worse.* Three bounded seeks and one
  unbounded full scan are not the same amount of real work, and neither the
  cardinality rung (abstains) nor the data-access rung (counts NODES) can tell
  them apart.
  **Two further defects sit under it, both measured, and neither alone is
  enough.** (a) `RecordQueryInJoinPlan.HintOrdering` returns the empty ordering
  unconditionally, so a sorted IN-join can never satisfy an ORDER BY — Java
  derives one (`OrderingProperty.visitInJoinPlan`, `OrderingProperty.java:392`):
  when the inner's binding map holds the IN-bound value as a FIXED binding and
  the source is sorted, that binding becomes directional in the source's
  direction and the rest of the inner ordering is inherited. (b) The whole
  requested-ordering arm of `ImplementInJoinRule` is DEAD: it looks the
  requested part up in `richOrdering.GetBindingMap()` by Value IDENTITY, and the
  request carries the translator's baked `ID#0` while the ordering advertises
  the lazy `ID`, so it finds nothing and returns nil for every request. Every
  IN-source therefore comes from `buildSourcesFromProvided`, which hardcodes
  `sorted: true` with `reverse` left false — no descending IN-join is ever
  built. The fix for (b) is the bridge that already exists for exactly this
  (`RichOrdering.orderingKeyFor` / `CanBridgeOrderingValueRoots`).
  **Prototyped and REVERTED, with the measurement that says why it is its own
  workstream:** fixing (a)+(b) makes the descending IN-join real and eliminates
  the sort on `ORDER BY id DESC, k` — but it moves 16 further corpus plans, 15
  of them IN-union→IN-join flips on ASCENDING queries CQ-10d never touched, and
  it STILL does not fix the query above (the full scan keeps winning on a later
  structural rung). That is a milestone-sized planner behaviour change: it needs
  its own RFC and a Graefe ACK before implementation, per the query-engine gate.
  **Scope when the REMAINING (a)+(b) work is picked up:** decide the rung
  question first (is criterion #4 allowed to tie a bounded access against an
  unbounded one, or does the bounded/unbounded distinction belong in the cost
  model?), because (a)+(b) without it changes many plans and does not yet
  reach Java's no-sort shape.
  **Update (CQ-20) — the FULL-SCAN regression is CLOSED via a THIRD defect,
  distinct from (a)/(b) and NOT a milestone-sized change, so it was fixed in
  isolation while (a)/(b) stay open.** `PKScanOrdering`
  (`pkg/recordlayer/query/plan/plans/ordering.go`) reported EVERY primary-key
  column as a sorted key regardless of any equality comparison narrowing the
  scan, so a per-binding equality PK scan (`Scan(TBL,[id=q0])`, `id` really
  Fixed) and a fully unbound PK scan (`id` really Sorted-ascending) advertised
  the IDENTICAL `Ordering{Keys:[ID,K]}`. Plan partitioning
  (`expression_partition.go`'s `orderingsEqual`/`toPartitionsFromMap`) reads
  exactly that `Ordering`, so the two co-partitioned, and whichever member
  happened to be added to the `PlanPropertiesMap` first became the partition's
  sole representative — silently shadowing the bounded InJoin/InUnion
  candidate behind the unbound scan before the sort-vs-full-scan cost rungs
  above ever got to compare them. Fixed by trimming the equality-bound PK
  prefix out of `PKScanOrdering`'s `Keys`, mirroring the firstNonEq logic
  `RecordQueryIndexPlan.HintOrdering` already used and matching Java's
  `ValueIndexLikeMatchCandidate.computeOrderingFromScanComparisons`
  (`ValueIndexLikeMatchCandidate.java:126-196`), whose equality-bound prefix
  only populates the FIXED binding map and is never appended to
  `orderingSequenceBuilder` — Java's `Ordering.equals`
  (`Ordering.java:250-261`) always compares the binding map (Java has one
  `Ordering` type; Go split it into `Ordering` for partitioning and
  `RichOrdering` for richer reasoning, and the partitioning type was the one
  that lost the distinction). The reproducer above now plans
  `InMemorySort([ID DESC], InJoin(Scan(TBL, [=]), binding ASC))` — three
  bounded seeks under a sort — instead of the full reverse scan. **This is
  NOT yet Java parity**: Java needs no sort at all, because defects (a)
  (`RecordQueryInJoinPlan.HintOrdering` returning the empty ordering
  unconditionally) and (b) (the requested-ordering arm's dead Value-identity
  binding lookup) are UNCHANGED by this fix and remain their own
  milestone-sized workstream, deliberately not implemented here so the
  partition-key fix could land isolated and reviewed on its own. Corpus:
  2633 statements, **26 changed (0.99%), 13 shape flips, 0 regressions** — 5
  full-scan→bounded-InJoin (improvement, 3 DML + this SELECT reproducer's
  no-ORDER-BY control), 4 redundant `InMemorySort` eliminated (improvement),
  12 InJoin ordering-tag `binding`→`binding ASC` (neutral, all already under a
  sort), 2 InJoin→InUnion with sort eliminated (improvement), 1 REVERSE flag
  dropped on an inner scan under an outer re-sort (neutral), 1 the reproducer
  itself (full reverse scan → sorted bounded InJoin, the regression closure),
  1 `NestedLoopJoin` operand flip on an `unordered: true` cross join
  (neutral, pre-existing structural-hash tiebreak, `cte_error_codes.yaml#5`).
  Every category individually golden-reviewed against the regenerated
  `explaindiff` plan-shape baseline; zero planning-time delta. Now pinned in
  `in_over_primary_scan_sarg.yaml`: the no-ORDER-BY control asserts
  `InJoin(Scan(TBL, [=])`, and the `ORDER BY id DESC` prefix-only scenario
  (previously deliberately unpinned) now asserts
  `InMemorySort([ID DESC], InJoin(Scan(TBL, [=]), binding ASC))` with its
  exact descending rows kept, run against real FDB. Regression test:
  `TestToPlanPartitions_SeparatesFixedBoundScanFromSortedUnboundScan`
  (`pkg/recordlayer/query/plan/cascades/pk_scan_ordering_partition_test.go`)
  pins that a Fixed-bound and a Sorted-unbound PK scan land in different
  partitions, verified red without the fix.
  **Update (RFC-191 revision 3 / Graefe milestone review) — parity gap
  remains, now blocked on a Graefe ruling, not on more research.** With
  CQ-20 shipped the full-scan regression is closed; what's left is defects
  (a) and (b) (`HintOrdering` and the requested-ordering binding-lookup
  bridge). A live Java planner was instrumented (`MemoTraceProbe` on the
  `InsertIntoMemoPlannerEvent` bus, driven through the conformance server's
  `planSql` with FDB via `WithDirectIP()`) to watch what Java memoizes, not
  just what it wins. It refutes two prior hypotheses (Java DOES enumerate an
  ordered comparand InJoin for the FETCH-required shape; the ASC tie is
  decided one rung before `numSourcesInJoin`, at fetch-depth) and finds the
  actual mechanism: `PushInJoinThroughFetchRule` is registered only for
  `RecordQueryInValuesJoinPlan`/`RecordQueryInParameterJoinPlan`
  (`PlanningRuleSet.java:152-153`), never for `RecordQueryInComparandJoinPlan`
  — the class every sorted IN-source produces — so `Fetch(InJoin(Covering))`
  is structurally never built in Java, `Fetch(InUnion(...))` wins ASC on a
  depth-0-vs-depth-1 fetch-position tiebreak, and DESC reaches
  `numSourcesInJoin` instead because no depth-0 InUnion is available there
  (why the bounded-covering leg is unreachable for DESC is unresolved). This
  looks like a Java oversight, not a deliberate design choice, so Go must
  choose between reproducing the asymmetry faithfully or deliberately
  diverging — a decision this RFC documents both sides of but does not make.
  **Flagged for a Graefe ruling, full mechanism and file:line citations in
  RFC-191.** 23 of 25 previously-prototyped (a)+(b) corpus flips are
  Java-confirmed correct against the live trace; the 2 exceptions
  (`in_over_primary_scan_sarg.yaml#14,#15`, FETCH-required secondary-index
  shapes) already match Java as Go plans them today, so landing (a)+(b)
  unguarded would regress them — that is the concrete stake behind the
  ruling. Numbers corrected from earlier citations: defect (b)'s bail rate
  is 238/238 (100%) before a fix and 40/238 (17%, uncharacterised) after a
  prototyped bridge fix, not 196 or zero. The two false "InJoin gives no
  cross-value order" corpus comments are traced to two commits now, not one:
  `ec72b8e3f` originated the claim, `cebcbd94b` copied it into a second file
  citing the first rather than re-deriving it; neither cites Java.
  `ImplementInUnionRule` does not share defect (b) — its
  `bakeMergeComparisonKeys` already routes through the same
  `ColumnNameValue`/`orderingKeyFor` bridge defect (b) is about adding to
  InJoin, which is why InUnion's flips are reachable without waiting on (b).
  Status: still open, still its own milestone-sized workstream, but the
  blocker is now a decision, not an investigation.
  **Update (RFC-191 revision 4 — Graefe ruling: option (ii), diverge
  deliberately) — the ruling is IN, ACK on the design choice, NAK on
  revision 3's RFC text.** For an IN-list over a non-covering secondary
  index with an index-satisfiable requested ordering, Go elects
  `Fetch(InJoin(Covering))` where Java elects `Fetch(InUnion(Covering))` for
  ASC (DESC already agrees). Revision 3's text got three things wrong that
  the ruling corrected: (1) sortedness does NOT select Java's Comparand
  IN-join class — `SortedInValuesSource` never overrides `toInJoinPlan`;
  the actual selector is `ImplementInJoinRule.computeInSource`'s
  `isConstant()` fallback, which every SQL-originated IN-list hits
  regardless of direction, so `PushInJoinThroughFetchRule`'s exclusion of
  `RecordQueryInComparandJoinPlan` (`PlanningRuleSet.java:152-153`) excludes
  ALL SQL-driven IN-joins, not just sorted ones; (2) Go already has an inert
  guard for this at `rule_push_in_join_through_fetch.go:43-47`, with a
  fabricated "comparand values depend on the outer record" rationale, and
  it has plausibly never fired — Go **already plans**
  `Fetch(InJoin(IndexScan(IA,[=]), binding ASC))` for the unordered
  secondary-index shape today, so reproducing Java's asymmetry (option i)
  would mean REMOVING already-shipped behavior, not adding machinery — the
  RFC's cost accounting between the two options was backwards; (3) the two
  cost-model pieces revision 3 said Go would need for option (i)
  (implicit-fetch counting, fetch-depth-from-root) already exist
  (`planning_cost_model.go:321-327`, `:330-334`) — only the accident (one
  dead guard) was ever missing. The ruling's own supporting findings: an
  archaeology showing the Java exclusion is incidental (generic rule class,
  one registration site out of ~8 trio-aware sites, a 9-month gap between
  the registrations and the Comparand class shipping for an unrelated
  feature, zero comment/test/issue anywhere in Java); a reading of
  `PlanningCostModel.java:251-262`'s own "bigger [InJoin] wins" comment as
  contradicting the ASC outcome, which is instead decided by the one
  uncommented rung in the file (fetch-position) acting as an accidental
  proxy; a plan-quality argument (identical I/O, a provably degenerate
  InUnion merge for this shape family — legs can't interleave when the
  sort key's leading column IS the per-leg equality column — smaller
  open-cursor footprint, no async execution edge in Go's actual sequential
  executor). Gated on binding conditions A–F before implementation: (A)
  delete the dead guard, any future withholding must be a correlation
  predicate not a cost one; (B) `in_over_primary_scan_sarg.yaml#14,#15`
  MUST move to `Fetch(InJoin(...))` (inverted from revision 3, which had
  them as a negative test for the wrong default); (C) measure `#14`'s DESC
  shape against Java before re-blessing — revision 3's "23/25 confirmed"
  count treated it as confirmed when it was an ASC-measurement
  extrapolation; (D) a `DIVERGENCES.md` entry as a behavioural plan-choice
  divergence (not a cost-model criteria row, since no individual criterion
  diverges), with an explicit wire-compat statement — verified, not
  reasoned: continuations are decoded only by the same plan-tree shape
  that produced them (`ExecutePlan`'s type-switch, `executor.go:88-120`,
  rejects a mismatched shape loudly) — that leg is UNAFFECTED by RFC-203
  and alone carries the conclusion. RE-DERIVED by RFC-203 §8.3 for the
  SECOND leg only: the original "Go continuations are engine-private by
  construction (a Java-minted token is rejected outright,
  `cascades_generator.go:1205-1216`)" premise is retired — RFC-203 ships a
  resume entry point that PARSES a Java-minted token — and is replaced by
  RFC-203 §3.2's per-path fence (mode string on `EXECUTE CONTINUATION`,
  mode-then-plan-hash on `OptContinuation`). Transport strengthens the
  first leg too: the plan travels with its own continuation, so a shape
  mismatch is impossible by construction rather than by luck. This
  plan-choice divergence is still not a cross-engine wire concern; (E) a real-FDB benchmark at N ∈ {3,10,100}
  that can FLIP the ruling if InUnion measurably wins at any N, a permanent
  EXPLAIN test at fetch-depth 0 both directions plus a non-monotonic
  IN-list (`IN (30,10,20)`) FDB row-order test, a proof that leg
  concatenation yields the requested order for the secondary-index shape
  specifically (a correctness question that outranks the ruling if it
  fails), root-causing the 40/238 residual defect-(b) bails, and a full
  corpus/1M-stress/10x-determinism sweep at the final head; (F) (a)+(b) ship
  together with the (smaller-than-projected) third piece — deletion plus
  documentation, not new machinery — concentrating correctness risk in
  (a)+(b), reviewed against `OrderingProperty.visitInJoinPlan:392-459` line
  by line. Full text in `rfcs/191-in-join-descending-ordering.md` revision
  4. Status: **ruling obtained, option (ii)**; implementation still gated
  on conditions A–F, most consequentially the benchmark (E) which can still
  reverse the outcome.

- [ ] **CQ-21 (LOW, found in CQ-20 review) — `PKScanOrdering` and
  `RecordQueryIndexPlan.HintOrdering` disagree on the fully-equality-bound
  degenerate case.** When every PK/index column is consumed by an equality
  comparison, `PKScanOrdering` (`ordering.go`) now returns
  `Ordering{}` (`IsKnown:false`), but `RecordQueryIndexPlan.HintOrdering`'s
  analogous corner case (index key fully equality-bound AND the trimmed PK
  suffix is empty — e.g. a composite unique index over exactly the PK
  columns) falls through to `IsKnown:true` with an empty `Keys`. Flagged by
  Graefe during the CQ-20 review as INERT today — every current consumer
  (`expression_partition.go`, `plan_properties.go`'s RichOrdering synthesis,
  the distinct-union/streaming-agg rules) branches on `len(Keys)==0`
  regardless of `IsKnown`, so the two representations behave identically
  everywhere they're read — but the two producers of the same logical
  "nothing left to sort by" fact should converge on one representation
  rather than carry a silent format difference a future `IsKnown`-branching
  consumer could trip on.

- [ ] **CQ-10e (LOW) — an aggregate index's comparisons are invisible to the
  comparisons property, from both directions.** Same axis as CQ-10c, currently
  unreachable. Java's `RecordQueryAggregateIndexPlan` implements
  `RecordQueryPlanWithComparisons`, so `ComparisonsProperty` reads its scan
  comparisons. Go's (`pkg/recordlayer/query/plan/plans/aggregate_index.go:22-38`)
  neither implements `GetScanComparisons` — so it is not a
  `ScanComparisonProvider` and contributes nothing of its own — nor exposes its
  wrapped index plan as a quantifier, so the collector cannot reach that plan's
  comparisons either. **No verdict can flip today**: the planner never places an
  IN-plan over an aggregate index, so criterion #6 never walks one. Recorded so
  the gap is closed deliberately if aggregate indexes ever appear under an
  IN-plan, rather than discovered as a wrong cost then. Closing it means making
  the aggregate-index plan a `ScanComparisonProvider` that delegates to its
  wrapped index scan; it changes verdicts only once the shape is reachable, but
  it is still a cost-model change and needs the usual stress + golden review.

- [ ] **CQ-11 (MED) — enrich and calibrate planner statistics.** Add at least
  distinct-count/selectivity hooks beyond table cardinality, keep safe defaults,
  and calibrate the model against representative indexed, skewed, and join
  workloads.

**Verification, performance, and documentation:**

- [ ] **CQ-26 (LOW, found scaling the cardinality/cost bound oracle to random
  combos, 2026-07-26) — `TypeFilterCost` has no floor at the property's
  proven min, same class as the already-documented Distinct/
  StreamingAggregation-ungrouped/DefaultOnEmpty-over-zero-cost gaps.**
  `properties.TypeFilterCost` (cost_formulas.go) applies a flat
  `TypeFilterSelectivity=0.5` multiplier unconditionally; when the child
  structurally proves an exact positive floor (e.g. `RecordQueryValuesPlan`,
  which is `ExactlyOne`, min=1 max=1 — a literal row, not a "might not
  exist" point lookup), the resulting cost estimate (0.5) falls below the
  proven min (1), violating the standing invariant
  `pkg/recordlayer/query/plan/cascades/cardinality_cost_bound_test.go`
  checks. The existing fixed `typeFilter/overBoundedChild` shape cannot see
  this — its child (`pointLookupScan`) proves `AtMostOne` (min=0), and
  0.5 >= 0 never violates — so this was invisible until the random-combo
  generator (`TestCardinalityPropertyBoundsCostEstimate_RandomCombos`,
  scaled via `CARDINALITY_COMBOS`/`CARDINALITY_SEED`) composed
  `TypeFilter(Values(1))` directly. PINNED (not fixed) as
  `typeFilter/overExactlyOneChild` (`knownBelowMin`, self-cleaning) in the
  same file; the random generator's leaf pool deliberately excludes
  `RecordQueryValuesPlan` so it does not rediscover this SAME known gap on
  every run (documented at the leaf-pool site). Fixing `TypeFilterCost` to
  floor at the child's proven min (mirroring `FirstOrDefaultCost`'s
  unconditional floor) is a **cost-model change** and therefore needs the
  Graefe+Torvalds milestone-level ACK the query-engine skill requires before
  it lands — out of scope for the harness-scaling work that found it.
  Reproduce: `CARDINALITY_COMBOS=1000 CARDINALITY_SEED=1 bazelisk test
  //pkg/recordlayer/query/plan/cascades:cascades_test
  --test_arg="--test.run=TestCardinalityPropertyBoundsCostEstimate/typeFilter/overExactlyOneChild"
  --test_arg="-test.v"` (logs "KNOWN violation still reproduces").

### [ ] Finding 2-followup-a — port Java's REAL RemoveRangeOneRule (booked by RFC-188 §1)
Java `RemoveRangeOneRule.java:45-102` drops an unreferenced `RANGE(0,1)` table-function quantifier from
a `SelectExpression` (nothing to do with LIMIT) — UNPORTED. Porting it reclaims the `RemoveRangeOne`
name cleanly. Distinct missing-rule item; needs Graefe+Torvalds review (query-engine rule).


### [ ] Finding 6-followup — dense predicate-count producer for Java tiebreak parity
Go's `predCountByLevel` is SPARSE, so the highest-level tiebreak (`intCompare(maxLevelA, maxLevelB)`) uses
the highest PREDICATE level, not Java's tree-depth `getHighestLevel` (dense). Only bites when every
per-level count ties (rare). Making the producer dense (always `counts[currentLevel]+=predCount`, 0
included — matching Java's visitor) closes it, but FLIPS ~11 corpus plan lines (REWRITING survivors:
e.g. a nested redundant Project, a Limit/Project reorder) that each need Java-verification before shipping.
Pre-existing (master's producer is sparse too); booked, not bundled into RFC-188.


### [ ] Finding 7 (MED, fragile) — Quantifier.GetCorrelatedTo() returns empty; Java transitive-walks
`expressions/quantifier.go:250` returns `{}` vs Java `getRangesOver().getCorrelatedTo()`. UNDER-
approximation (dangerous direction) — any new consumer treats a correlated leg as free-standing (0-row
class). Contained today (consumers rewired), latent trap.


### [ ] Finding 9 (MED, latent) — TranslateQueryValueMaybe pulls up candidate sub-values against themselves
`max_match_map.go:771` `PullUpValue(entry.candidateValue, entry.candidateValue, alias)` collapses every
candidate part to `QOV(alias)` (case-1 self-equal always fires) vs Java's root-relative
`candidateValue.pullUp(mapping.values(), …)`. Masked until a covering-index `RecordConstructorValue`
result value appears → wrong projection. `PullUpValues(parts, m.candidateValue, alias)` exists unused.


- [~] M5: PrimaryKey for index scans — REVERTED to nil (safe); re-booked as structural. The by-COLUMN-NAME
  PK (`GetPKColumnNames()` → FieldValues) wrongly equates record types whose PK EXPRESSIONS differ but
  share field names (`Field("ID")` vs `Concat(RecordTypeKey(), Field("ID"))` both flatten to `["ID"]`),
  which would let `ImplementDistinctUnionRule` dedup two legs that must both survive (dropped rows —
  codex P1b, a finding-A-class leaf-name bug). `computePrimaryKey` now returns nil for index scans (safe
  under-report: disables the optimization, never wrong dedup). Pin:
  `TestComputePrimaryKey_IndexScanIsNilPendingStructuralPK`. See Finding 10-M5-followup.


### [ ] Finding 10-M5-followup — structural common-PK for index scans (Java parity)
Port Java `PrimaryKeyProperty.visitIndexPlan` PROPERLY: `ScalarTranslationVisitor.translateKeyExpression(
index.getCommonPrimaryKey(), …)` — translate the index's common PK KEY EXPRESSION to Values that encode
STRUCTURE (record-type-key prefixes, nesting), not bare column names. Then `commonPKFromChildren`'s
`ValuesStructurallyEqual` compares real PK identity and cannot equate `Field("ID")` with
`Concat(RecordTypeKey(), Field("ID"))`. Finding-A-class (carry the key expression / translated Values on
the plan; also handles the fan-out case — a fan-out index's entries DO carry the common PK, so it should
be surfaced for the PK property while still suppressed for the ordering suffix). Needs Graefe+Torvalds.


### [ ] Finding 10-M4-followup — Fetch stored-record transparency to activate the non-covering DISTINCT elision (codex P2)
`computeStoredRecord` has no `RecordQueryFetchFromPartialRecordPlan` arm (Java StoredRecordProperty treats
a fetch as producing a stored record → true), so `ImplementDistinctFinalRule` filters out the
`Fetch(IndexScan)` partition and the M4 non-unique-scalar-index DISTINCT elision does not fire on the
common non-covering path. Adding the arm is Java-faithful BUT activates the elision broadly: explain-diff
showed ~48 changed plan lines — `InMemorySort([ID], Fetch(InJoin(IndexScan)))` → `InUnion(IndexScan)` on
IN-list queries (sort elimination, apparent improvements). Deferred from RFC-188 because it opens a NEW
optimization surface that needs its own validation (row-level + EXPLAIN review of every flip + 1M stress
+ reviewer sign-off), not a milestone-end add. The distinct/PK/cardinality Fetch-transparency arms
already landed (safe, zero-flip); this is the storedRecord arm that turns them on.


### [ ] Finding 10-M4-followup-2 — fail-closed distinct for an UNKNOWN index-root key expression (codex)
`createsDuplicates`'s default returns false (shared with `validateSplitKeyExpression`, which uses it as
POSITIVE proof — a fail-closed `true` there wrongly admits `Split(customScalar)` that master rejects). So
an index whose RootExpression is an UNRECOGNIZED (custom/external) KeyExpression is stamped known
non-fan-out → M4 distinct=true (theoretically unsafe). NOT reachable today: every proto/SQL index root is
a recognized type. The M4 fail-closed need must be DECOUPLED from the shared default — e.g.
`index.CreatesDuplicates()` (or `metadataIndexDef.IndexCreatesDuplicates`) returns UNKNOWN (→ don't stamp
→ distinct=false, safe) for a root type not in the recognized set, without touching the Split-validation
default. Or add fan-out to the KeyExpression interface contract (Java's approach: abstract method).


### [ ] Finding 10-M4-followup-3 — refine the non-VALUE fail-closed (optimization) + audit value-candidacy vs Java
`Index.CreatesDuplicates()` fails closed to duplicate-producing for ANY non-VALUE index type that reaches a
value-scan candidate (TEXT tokenizes → dup; the conservative default also covers rank/multidimensional/
time_window_leaderboard/non-atomic bitmap). This is SAFE (identical rows cross-engine; at most a redundant
DISTINCT) but may OVER-report duplicates for a genuinely-distinct non-VALUE type (e.g. a VERSION index —
one entry per record), losing a DISTINCT elision. Refinement (safe, optimization-only): per-type analysis
of which non-VALUE types' SCANS actually emit multiple entries per record, narrowing the fail-closed to
those. Deeper (Java-parity): should Go EXCLUDE these types from value candidates entirely (as it already
does for aggregate/vector/atomic-mutation), matching Java, rather than value-scanning them — surfaced by
the M4 signal but pre-existing. Not a correctness gap now (fail-closed is safe); cross-engine reachable
(Java-created index in shared metadata).


### [ ] Finding 10-M3-followup — recognize equality-bound primary scans in the outer guard (codex P2)
`wholePlanMaxCardinalityKnown` uses `computeCardinalities`, whose `RecordQueryScanPlan` arm always returns
`UnknownMaxCardinality` — so a full-PK-equality primary scan (a point lookup, max=1, which
`concretePlanCounts`/`scanProvableMaxCard` DO bound) is reported unknown, and the M3 outer guard
over-abstains for a bounded-primary-scan-vs-unbounded comparison where Java would apply criterion #2.
Safe direction (abstain → falls through to other rungs); zero corpus impact. Fix: bound a full-PK-equality
scan in `computeCardinalities` (needs the PK column count → thread `ctx`, like `scanProvableMaxCard`).

**Codex milestone-review fold (finding B):** codex NAK surfaced 1 P1 + 3 P2.

### [ ] Finding 11 (MED) — PredicateToLogicalUnionRule expands every top-level OR
`rule_predicate_to_logical_union.go:99` fires OR→`Distinct(Union)` unconditionally; Java
`PredicateToLogicalUnionRule:197` only expands index-exploitable ORs (`partiallyMatchedOrs`). Memo bloat
+ worse-plan risk for `a=1 OR b=2` with no indexes.


### [ ] Finding 12 (LOW-MED, latent) — value-layer / compensation gaps

FOUR OF THE FIVE BULLETS BELOW ARE STALE — the defects were fixed and the entry
was not updated, so a reader picking this up re-derives work already done. Each
is struck through with the evidence that closed it; the compensation bullet is
the only one still standing. Verified by reading the code at the named sites, not
by assuming a later commit covered them.

- ~~`values/value_in.go:52` value-layer IN promotes both int64→float64 → loses precision >2^53~~
  FIXED. The site now routes same-family integers through `CompareExactInts` and
  says so: "never round-trip two int64 through float64, whose promotion ties
  adjacent values above 2^53". Pinned at both levels — `values/value_in_exact_test.go`
  (unit) and `sqldriver/numeric_precision_boundary_test.go` (SQL).
- ~~`:55`/`value_array_distinct.go:101` `==` panics on non-comparable slice/map elements~~
  FIXED. Both sites call `comparableEqual`, which falls back to `reflect.DeepEqual`
  for non-comparable dynamic types; the array site carries the reason inline.
- ~~`values/values.go:3365,3394,3519` `CAST(string AS INT/LONG/DOUBLE)` `TrimSpace` where Java rejects.~~
  FIXED. `trimJavaWhitespace` strips only code points <= U+0020, matching Java's
  `String.trim()`, so `CAST(NBSP+'5' AS INT)` throws rather than yielding 5.
- `compensation.go:1035` `ForMatchCompensation.Union` picks c's rcf even when only other's is needed
  (Java `Verify.verify` both) → wrong output shape; `:151` `ComputeResultCompensation` hardcodes
  `EmptyGroupByMappings` vs Java's `pullUpAggregateCandidateMappings`. Both latent (single-child gated).
- ~~`predicates/predicates.go:117` `PredicateEquals` has no `ExistentialValuePredicate` case~~
  FIXED. The case is present at `predicates.go:136`.

The compensation bullet above is the one that STANDS: `EmptyGroupByMappings()` is
still hardcoded (`compensation.go:122,154`). The `Union` rcf half of that bullet
was not re-checked, so it is neither confirmed nor closed here.


### [ ] Finding 13 (LOW) — dead code / missed matches / maintainability
- Dead rules: `rule_implement_intersection.go:46`, `rule_intersection_merge.go:37`,
  `rule_set_op_singleton.go:49` (no query seeds `LogicalIntersectionExpression`). RFC-190.5b resolves
  the intersection rule's latent unordered-child bug with an explicit ASC request and ordered-winner
  selection; the rule remains dead/maintainability-only.
- `rule_merge_fetch_into_covering_index.go` unreachable (`wrapScanPlanWithCoverage` strips the Fetch) —
  unported Java optimization.
- `rule_match_intermediate.go:227` MatchIntermediateRule pairs quantifiers POSITIONALLY; Java enumerates
  permutations → silently misses index matches for permuted multi-quantifier expressions.
- `matched_ordering_part.go:193` `Demote()` dead + panics; `max_match_map.go:643`
  `findMatchingReachableCandidate` dead.
- `expression_partition.go:69` `PlanPartition.GetPlans()` not index-aligned with `GetExpressions()`
  (footgun, bit `rule_implement_in_join` once); `:30` `NewPlanPartition` map-iteration nondeterminism (dead path).
- `unified_tasks.go:675/696` `isExploratoryMember` ≡ `isAlreadyExploratoryMember` (byte-identical dup);
  O(n) membership scans in per-round loops (O(n²)/group); cost comparator re-walks whole subtrees uncached.
- Maintainability: the NLJ rule remains ~3.5k LOC vs Java 331 (10×, #1 churn file, hand-rolled
  existential/buried-leg subsystem), though RFC-190.7 centralized its repeated existential
  compensation tail; ~40% comment density carries reverted-attempt process narrative (violates
  CLAUDE.md comment guidance).

---


- [~] **P2 — positional/typed runtime row** (in gauntlet, PR #427). Typed positional row emitted
  alongside the name-keyed `map[string]any` by the NON-JOIN producers (scans, covering index,
  projections; filters pass it through free) + the `PositionalRow` substrate + `shadowMismatch`.
  **Scope (gauntlet-agreed):** the JOIN/lateral producers (`mergeRows`/`flatmap`/`explode`) and the
  outer-join null-extension primitive (`appendNullLeg`) move to **Slice 2/3** (they're restructured
  positional-native there; dual-emitting over the doomed AnchoredJoin merge would be throwaway).
  **Deferred to Slice 1** (before the ordinal path goes live): (i) the dual-emission per-row cost
  benchmark (RFC §4 P2 hard part); (ii) [@claude] a dedicated **e2e shadow test for the projection
  producer** (`executeProjection`), analogous to `TestBuildCoveringRow_ShadowAndCollision_RFC173P2`
  — projection's `slots[i]=val` has no index arithmetic so risk is low, but pin it before Slice 1
  makes ordinal access authoritative. **Carry-forward:** (a) [Graefe] when a resolution path becomes
  AUTHORITATIVE, escalate `resolveOrdinal`'s absent-field / non-record decline from silent
  `(0,false)` to Java's `SemanticException`; (b) [@claude] `RecordType.FieldIndex` and `LookupField`
  are near-duplicate scans — dedup (`LookupField` → `FieldIndex` + index) when a slice touches both.


### RFC-173 residuals lifted out of completed slice entries
  - [ ] Follow-up (non-blocking, Torvalds ACK w/ next-slice defer): `disableAliasAwareInterning` is a
    test-only process-global read in Insert's hot loop. Race-free today (non-parallel test, -race
    enforced), but thread it through per-Memo state (like `mergeAliasCounter`) so no global is read in
    Insert — deferred because `Reference.Insert` has no Memo handle. Do when a slice gives Insert Memo
    context (the alias-aware tier itself survives S4, so this is not auto-retired).

  - [ ] Follow-up (W4b review residuals, master-parity — broken on master too, pinned as clean
    errors): (a) aggregate-local outer refs in a correlated scalar (`SUM(o.amount + e.ref)` over a
    clustered outer) never planned — the true enablement is an EXECUTION-CONTEXT property (the
    aggregate cursor inheriting the outer binding through the NLJ context, as Java threads it), not an
    expression rewrite (Graefe ruling on the codex P2-2 fix); (b) box-leg clusters (nested FULL boxes)
    stay on the pre-W4b path even when gated (@claude scope note — intended W4b narrowing); (c) the
    UPDATE-transform row-context site (`executor.go` ~:2754) still gates the binder on the old
    params/scalar-subqueries-only condition — the same class round 5 fixed for projections
    (`hasBindingContext`); PROBED: unreachable through SQL today — a subquery in UPDATE ... SET never
    reached the binder at all (the builder wrote the LITERAL SUBQUERY TEXT into the row — silent data
    corruption, on master too; now guarded with a clean 0AF00 decline + a no-mutation pin). Align the
    site with the shared predicate whenever subquery-in-SET support lands.
    FIXED in the gauntlet (not residuals): parenthesized-column-over-JOIN-inner (qualified scalarCol
    from the walked QOV; the ambiguous-column probe returned the WRONG LEG before the fix — pinned);
    OUTER-scope parenthesized columns (scope discrimination via the binder-exact innerSourceAliases;
    materialized, keyed by the executor's shared naming contract — a wrong-scope read before the fix);
    the unnest AT-alias scope corner (the collector mirrors unnestSourceCorrelation: AS-else-AT-only);
    the name-model projection binder gap (round 5 — outer refs over Datum rows silently NULLed).

  - [ ] Graefe recorded obligations live in RFC §4: Slice-4 kill list (sort-comparator fallback dies
    loud · dualwindow retires with the name map · `legPhysicalOutputNames` must not outlive the window ·
    bound `positionalTypeCache` dynamicpb leak) + Slices 2–3 oracle-gate rule (new birth sites extend
    `DisablePositionalEmission`).

  - [ ] **BOOKED (Graefe, RFC-173 S4 :3234 clean-lift condition 3): retire the mutable `inInnerCluster`
    field for a DOWNWARD enclosure-context PARAMETER (Volcano required-property style).** The
    `!t.inInnerCluster` read in `translateFilter` (`:2211`, the enclosed-gather rotation guard) is
    CALLER-CONTEXTUAL — "is this filter a merge leg or a fresh cluster root?" is caller state, NOT
    subtree-derivable, so there is no clean local tree predicate to replace it (unlike the join gate's
    `gatesAsFreshCluster`). The genuine decouple is threading enclosure context as an explicit downward
    parameter instead of a global mutable field (~15 threading sites). Separate architectural refactor;
    NOT a producer-retirement blocker — the `:3234` lift landed without it. Do AFTER the remaining
    enclosure setters are retired (it touches all of them).

  - [ ] **BOX-SUBSTRATE slice (in de-risk; Graefe design consult round 3 in flight).** Commit 1 landed
    (b8441025a: the ordinalSlotInLegWindow fail-closed hardening + the NullSupplyBarrier cert). RECORD
    CORRECTION owed: commit 1's message attributes the spike's silent-wrong to "empty rt.Legs → flat
    fallback" — REFUTED by a layout probe (windows already propagate through chained merged types incl.
    box bottoms: [{A 0 3},{B 3 3},{X 6 1},{Y 7 1}]; a prototyped bottom-only propagation BROKE the 2c
    certs — per-link windows are load-bearing for runtime qualified-name resolution — reverted
    uncommitted). TRUE mechanism (trace-confirmed): the box-leg conjunct takes rebaseChainedOuterLegPredicate's
    LAZY fork ({B} ⊆ outerLegs "scan-pushable"), leaving a FOREIGN-correlation name read at the merged
    select; the runtime bare-name fallback over an ordinal row first-matches across legs → A's slot 0.
    The pureBottom lazy-fork gate is the correct semantic fix but NOT sufficient: with it, the bake is
    altitude+slot-correct yet evaluates NULL — the chained-over-box FlatMap's runtime binding does not
    flip positional in sync with the seed (the seed/birth mismatch class at the CHAINED FlatMap birth).
    ROUND-3/4 RESOLUTION (executor-birth mapping + gather:93 spike): gather:93 is NOT vestigial — its
    decline and the seed arm are a COUPLED PAIR (lifting either alone builds an ordinal seed over a box
    whose NLJ is not birthActive: ContainsBakedOrdinal on the box's OWN result value decides the birth,
    and adaptLegPositional's synthesis from a merged multi-namespace box Datum is unfaithful → silent
    NULL/empty). Even the full three-piece lift (gather:93 + arm + WHERE-merge bake) fails: TWO more
    uncoupled sites gate the simple case — legIsOrdinalSafe categorically rejects non-INNER legs
    (rule_implement_nested_loop_join.go:1106) and the FULL NLJ is built with sel.GetResultValue()
    VERBATIM (:166). LANDED (Outcome A): the coherence guard on the chained gate —
    `!pureSpine && !boxOuterBirthsPositional(bottom) → decline` — AMENDED per two independent review
    falsifications: NOT a demonstrated-bug fix (pre-guard rows were CORRECT on every probed observable —
    the cleared-enclosure first-link translate ordinalizes the nested box too, so the tower is coherently
    positional with dual emission backstopping) but a CONSERVATIVE guard over an UNVALIDATED tower
    (zero e2e coverage, outside the box slices' verified surface) that becomes load-bearing when the
    name model is deleted at the cap; pinned nested_box_bottom_* + feature-off control.
    FULLY 4-GATED (b8441025a + 90e9fbe82 + amendment 2095b0ce8): Graefe ACK→re-confirm (with ownership;
    NEW STANDING RULE: latent-bug-on-HEAD claims require a rows-probe+trace on the PRE-change tree),
    Torvalds NAK→ACK (theory #4 falsified by his instrumented probes; zero refuted-claim residue; the six
    tripwires "the real prize"), @claude ACK×2 (22-probe differential byte-identical; harvest faithful),
    codex clean×3. Arc: FOUR falsified theories, THREE revertible spikes, ZERO unsound code shipped.
    BOOKED (Outcome B, the real box-substrate ordinalization — sequenced WITH the circular arms, LAST):
    the 5-site checklist — (1) chained gate ↔ birth coupling (landed as A), (2) the enclosure declines
    cluster_gate.go:315/:356 (the circular arms), (3) legIsOrdinalSafe non-INNER widening, (4) the FULL
    NLJ ordinal-seed build, (5) unnestExistsSeedSafe box-leg-conjunct/pureSpine re-scoping + the
    pureBottom lazy-fork gate + the WHERE-merge ordinal bake (all documented, none landed standalone).
    Also booked: the below-FOD hoist ordinal-4/leg-row rebase gap (the nested-box strand) + the grouped
    consumer row-loss — both surfaced only under the lift, both stay declined today.

  - [ ] **REFRAME (Graefe Outcome-B design consult, EMPIRICALLY FALSIFIED the "flip the boxes" premise —
    design-NAK on "minimal box sub-slice that flips a production shape without touching Site 2").** Two
    reproducible probes: (a) a gate census on constructed logical trees — EVERY fresh (un-enclosed) box shape
    ALREADY gates ordinal (standalone FULL/LEFT/INNER, box-over-inner-cluster, box-over-outer-box); the ONLY
    declines are the two `inInnerCluster` arms (2a arityPoison, 2b name-model). (b) Runtime producer
    instrumentation across the whole box e2e suite + a wide RFC173|Join|Outer|Unnest|Lateral net: 30+ P4/P5
    firings, **100% `enclosed=true`, zero `enclosed=false`**, all green. So the residual name-model producer
    set is EXACTLY the `inInnerCluster==true` shapes — the "box substrate" is mislabeled: it is an ENCLOSURE
    problem and the enclosure ROOTS ARE NOT BOXES. `inInnerCluster` set-sites: translateUnnest:1180 (name-model
    unnest LOWERING fallback — dominant, every P5 rode it), translateJoin:4789 (propagation from an already-non-
    gated parent), the `ordinalEligible==false` roots = correlated-scalar-in-projection (the booked W4b
    clusterPullUp) + recursive-CTE, and translateUnnestJoin:1475 (unnest-over-box where boxOuterBirthsPositional
    is false). **Producer graph collapses to {P4 buildJoinResultValue:718, P5 buildUnnestResultValue:1660} +
    the birth predicate; P1/P2 re-enumeration:469/:567 fire only when parentIsMerge, P3 legColumns:347 only
    under a name-model parent — all THREE are DERIVED, retire for free when P4/P5 hit zero.** CORRECT sequencing
    INVERTS the framing: **starve `inInnerCluster` by ordinalizing the enclosure ROOTS — do NOT flip 2a/2b
    (they are dead-code-at-the-cap, not incremental flips).** (Corrected line refs: 2a rfc173_cluster_gate.go:383,
    2b :424 — the old :315/:356 are stale. Site 3 legIsOrdinalSafe rule_implement_nested_loop_join.go:1106,
    Site 4 verbatim RV :166/:240, Site 5 unnestExistsSeedSafe:881 / rebaseChainedOuterLegPredicate:2888.)
    Graefe atomic edges: E1 2a-flip ⟺ enclosing parent's producer (fixpoint, parent must ordinalize FIRST);
    E2 AXIS1 ⟺ AXIS2 is literally ONE predicate (:1475 box-birth bit == unnestExistsSeedSafe:914 read); E3
    seed ⟺ birth (newOrdinalJoinBirth decides purely from the box RV's own ContainsBakedOrdinal — no separate
    executor gate, so a WHERE-merge ordinal bake that leaves the FlatMap birth name-model is unsound); E4 Site3
    ⟺ Site4 only on the correlated-FlatMap FOLD path (reconstructFoldStep1Seed:1188), NOT the box's own birth
    (Site 4 :166 passes the ordinal RC through once the translator gates the box — Site-4 coupling was OVERSTATED
    for the plain box). SEQUENCE: **B0 (design-ACK, FIRST) = producer census GATE** — a permanent test running
    the dualwindow corpus asserting per-P4/P5-firing the shape class + `enclosed==true` (pure observation, zero
    prod cost via a nil-in-prod observer like forceOrdinalSpike; the "can we reach zero" instrument; building it
    ALSO independently re-verifies the reframe). **B1 (design-ACK conditional, first REAL flip) = unnest-over-
    single-clustered-LEFT/RIGHT-box birth** — relax boxOuterBirthsPositional:952 `clusterArity==1` (FULL-only)
    to admit a LEFT/RIGHT box that already gates standalone; CONDITIONS: seed-gate relaxation moves ATOMICALLY
    with birth admission (E2, one predicate) + §3 control red-under-sabotage before green. **NOT first:** the
    nested-outer-box leg (deepest, "unverified tower", max multi-namespace risk) — LAST. Falsification protocol:
    `executor.SetNameModelOracle(true)` neutered control; PRE-change rows-probe on HEAD both modes (agree+match-
    Java ⇒ NO latent bug, it's coverage — say so, the Outcome-A amendment lesson); discrimination REQUIRES
    dup-named legs + buried-leg-column + null-extended leg, then SABOTAGE-test the control (mis-key a leg window,
    confirm the differential goes red). HIGHEST falsification risk: multi-namespace box Datum — adaptLegPositional
    :628 partial-matches same-named legs → silently WRONG slot; the qr.Positional passthrough:620 guards ONLY
    when positionalMatchesLegType (ordered name agreement); a reordered/covering box row drops to the colliding
    Datum synthesis. Every box control MUST include dup-named legs + exercise the passthrough-fails path.

  - [ ] **BOOKED (enclosed-CTE slice follow-ups — the LOUD reach classes Java answers).** One widening
    slice covering the 0AF00/42703 family the ON-drop fix deliberately kept loud: (a) join-bodied DERIVED
    tables in an explicit JOIN with ON (buildDerivedTableSource declines them — Q9e flip-sentinel);
    (b) UNALIASED-QUALIFIED projections in multi-leg CTE bodies (execution keys the slot "D.ID" with no
    bare key — needs a real post-CTE output-schema authority, Q11 flip-sentinel); (c) WITH c(x, y) COLUMN
    ALIASES over multi-leg bodies (scope-level renames never reach the runtime row — needs the rename
    pushed into the body projection or the CTE wrapper, Q12 flip-sentinel); (d) qualified WHERE over a
    CTE leg (already-loud 0AF00, consult finding); (e) NEW (round-6 repro of the delta-review P2, standalone
    control): a derived table whose INNER projection is qualified-spelled fails at RUNTIME with a
    malformed-plan error even with no CTE at all — `SELECT D."AID" AS "A" FROM (SELECT LA."AID" FROM LA
    LEFT JOIN LB ON …) "D"` → `field "AID" not resolvable (row columns [LA.AID])`; the derived row keys by
    the inner SPELLING instead of a canonical output name (the CTE ON derivation now declines these
    fail-closed, Q19/Q20/Q25 pins, but the standalone reach gap remains); (f) NEW (round-6 repro of the
    delta-review P1, standalone control): the 42702 ambiguity backstop does NOT run when a derived-table
    leg sits among multiple FROM legs — `SELECT "AID" FROM (SELECT LA."AID" FROM …) "D", LA "L2"` silently
    resolves the ambiguous bare ref against the enumerable leg (returns rows; should 42702) because the
    derived leg's columns are invisible to the resolver (CTE ON derivation declines fail-closed, Q18/Q21
    pins; the standalone resolver gap remains); (g) NEW (round-7, review-caught): an ON-ONLY CTE name used
    as a FROM leg gives buildSelectScope a NIL resolver — BOTH the 42702 ambiguity gate and the 42703
    unknown-column gate are skipped for the whole body standalone (`SELECT "NOPE" FROM "V", LA` plans fine
    when V is join-bodied); the CTE ON derivation declines such bodies fail-closed (Q27/Q28/Q29 pins) but
    the standalone nil-resolver gap remains — the real fix is a resolvable post-CTE output schema, i.e.
    this slice. EXPLICIT WIDENING CANDIDATE (review-recommended): the multi-leg-with-derived/ON-only
    decline over-declines the sound sub-class where every read is an ALIASED item over the ENUMERABLE legs
    only (`WITH U AS (SELECT L2."K" AS "B" FROM (SELECT "AID" FROM LA) "D", LA "L2") … ON U."B"` answered
    correctly pre-narrowing, 0AF00 now) — admitting it needs per-read leg attribution + per-leg emitted-set
    validation. All seven are one output-naming authority problem: the scope's advertised names must be
    the names execution emits. TWO SYSTEMIC RIDERS for the same slice (review-noted, pre-existing):
    (i) encodeSortContinuation drops Positional across a continuation RESUME — every positional consumer
    downstream of a resumed sort sees name-keyed rows only (benign today because the admitted classes key
    bare in the Datum too — the expr.go single-source Field rewrite — but any future positional-only
    consumer breaks at page boundaries); (ii) the GROUP-BY validator's harvestColumnRefs walk cannot
    distinguish a subquery-LOCAL ref (no group check due) from a CORRELATED-into-outer ref (group check
    due) — a potential over-loud corner on outExpr entries containing subqueries. (h) NEW (round-8 review,
    pre-existing at base and HEAD, absurd-rare but SILENT conformance divergences vs Java on DOTTED
    spellings): a quoted-dotted CTE name equal to schema.table (`WITH "S.LA" AS (…) … FROM "S.LA"`) is
    silently bypassed for the TABLE's rows; inversely `FROM "s"."LA"` with a bare CTE "LA" declared reads
    the CTE where Java's generateAccess reads the table. Rider: the explicit-dotted-alias corner
    (`AS "S.LA"`) is loud malformed both sides — same booking. Round-9-review riders, same family:
    the QUOTED-dotted spelling also evades the round-9 collision decline (typed-segments vs lossy-split
    mismatch — `WITH V AS (SELECT K FROM LA AS "s", "S.LB")` admits with a dead backstop, 4 silent rows,
    identical at base and HEAD; needs the same three stacked absurdities); JAVA-CONFORMANCE PROBE DUE for
    the R5b aliased-away-name read (`SELECT PA.ID FROM PA AS s, s.PB AS B` — the R5b pins' Java citations
    cover the LEG classification only, NOT the read; standard SQL says the alias replaces the name, so if
    Java rejects it the round-9 leniency carve-out preserves a Go-only accident and can narrow to leg
    classification, which also collapses the ON-vs-WHERE loud-vs-lenient divergence on collision bodies).
    CLOSING ITEM for the schema-qualified family: normalize sq on the visitor path as the catalog path
    does (the round-9 buildSelectScope strip closed the WHERE gap (Q40) and the ambiguity backstop (Q39);
    a full sq normalization would collapse the three strip sites into one — but the R5b collision
    carve-out must MIGRATE INTO the normalizer when it lands, and the dotted-name divergences above
    interact with the same pass: one slice for all of it). The ordering half (round-11 review): the arc's
    real one-rule ending is a SINGLE shared source-name resolution function all four scope consumers call
    (translateScan / cteLegKind / buildSelectScope.addSource / upgradeJoinOnPredicates.resolveTable — all
    four now individually CTE-first, but as four hand-mirrored copies). Two more riders, same family:
    (i) the ON-ONLY READ class (pre-existing; the SILENT residual is narrow — WHERE-based main-query
    reads are already LOUD 0AF00, only the leniency-path MAX/scalar-subquery reads stay silent):
    buildSelectScope consults cteScopes only, so an ON-only name's UN-ENCLOSED reads either fall to a
    same-named TABLE's schema (loud 42703) or, with no such table, to the NIL-resolver leniency (which is
    LOAD-BEARING for the enclosed comma-FROM reads Q1-Q5 — it lets the executor merge fabrication resolve
    them). On the leniency path a VALID non-shadow enclosed read answers while an INVALID/unresolvable
    non-shadow scalar read is silently NULL (Q51 flip-sentinel: MAX("NOPE") over a non-shadow ON-only CTE).
    SHADOW-CASE resolution: the inner shadow ON-only CTE is EVICTED from cteScopes (mirroring the derivable
    arm's shadow delete) and joins the SAME booked ON-only READ class as any non-shadow ON-only CTE.
    Boundary (Q52/Q54): a BARE read over the coinciding shadow answers via leniency+fabrication; a QUALIFIED
    read (V."X") is SILENT NULL (fabrication provides bare keys on the merged row, not the "V.X" qualified
    key) — the one remaining silent-wrong, booked here as a FLIP-SENTINEL. HISTORY (do NOT re-attempt
    lightly): installing the inner shadow schema into cteScopes to make the qualified read answer was tried
    TWICE and both are UNSOUND — a plain lossy install silently mis-resolved quoted/dup bodies; a
    read-sound install (case-preserving Ids + dup-decline) reopened flatten-evasion with an EXECUTION PANIC
    on a comma-multi-leg shadow ("divergent baked types") AND its case-preserved Ids mismatched execution's
    UPPERCASE row keys (executeProjection ToUpper) → plan-then-runtime-fail. The SOUND fix is
    cteOnScopes-aware read resolution AND fixing execution's quoted-alias uppercasing (so quoted-lowercase
    identifiers survive as lowercase) — BOTH large, both here. Until then the qualified shadow read stays
    the booked silent flip-sentinel; the exotic shape (nested WITH, join-bodied inner CTE shadowing its
    same-named outer, qualified solo read) does not justify a fragile install.
    Fix for the remaining silent leniency residuals = cteOnScopes-aware read resolution,
    CAREFULLY: the flatten-evasion gate pin (cte_form_fails_cleanly) must keep its clean decline AND the
    enclosed comma-FROM reads (Q1-Q5) must keep resolving. (i-b) FORWARD-VISIBILITY divergence (round-12
    review, both reviewers independently; pre-existing, A/B'd to before the arc): `WITH A AS (SELECT …
    FROM B), B AS (…)` ANSWERS on both pipelines where SQL/PG reject the forward reference — the chain
    builds bodies after ALL registrations, and the visitor's REBUILD arm (running after the eager build
    correctly failed and was swallowed) does too; the rebuild-arm comment now states this truthfully.
    Java-conformance probe decides the fix; if Java rejects, the preState infrastructure is the vehicle
    (each registration point's snapshot is precisely the earlier-siblings-only view). (ii) JAVA-CONFORMANCE PROBE: the ORDER BY
    output-alias-precedence shapes (Q42/Q46 — bare unique alias over FROM-scope ambiguity) are standard
    SQL/PG behavior but unverified vs Java's SemanticAnalyzer (the M5 42702 text was live-verified for
    FROM-scope shapes only); probe alongside the R5b read probe — if Java 42702s them, the pins flip to
    documented divergences or revert. (iii) FIFTH STRIP SITE (round-11 review): buildCTEColumnSource —
    the registration-time global deriver — does not apply the schema strip, so a CTE with a
    schema-qualified body lands ON-only and an aggregate read over it misroutes into the correlated
    fallback (0A000, pre-existing rounds 9-11); the normalization slice must catch it. (iv) CONSUMER
    CENSUS for the one-shared-resolution-function ending (round-11 review — the fifth copy was missed by
    the round dedicated to aligning copies, which is the argument for the collapse): LIVE-fixed CTE-first:
    translateScan, cteLegKind, buildSelectScope.addSource, upgradeJoinOnPredicates.resolveTable,
    buildCTEColumnSource's inner-table resolution (round 13 — its metadata-first was LIVE-wrong: a
    shadowing body's schema derived from the TABLE, declined on the CTE-only column, and dumped the CTE
    into the ON-only marker path; the nested-shadow pin had been green only via the stale-outer accident
    the round-12/13 evict removed). SEVENTH copy (round-13 review, live LOUD reach, booking-grade):
    buildDerivedTableSource (~:318) resolves its inner table via the analyzer with NO CTE fallback at all
    — a derived table over a shadowing CTE OVER-DECLINES the valid read (0AF00; the ready-made
    flip-sentinel — answers when it goes CTE-first), the SELECT * variant advertises the TABLE's columns
    and fails at RUNTIME where plan-time 42703 was due, and the name-coincidence variant answers by
    accident. buildDerivedTableSourceFromAgg is clean (derives from aggregate output shapes, no table
    resolution). EIGHTH copy (review, A/B-confirmed pre-existing at the round-9 baseline, live LOUD reach):
    buildCTEColumnSource (the deriver) consults cteScopes but NOT cteOnScopes, so a join-bodied CTE
    shadowing a same-named base TABLE is invisible to it — it derives the dependent CTE's schema from the
    TABLE, accepts a column the CTE never exposes, and fails at RUNTIME (malformed-plan, "field K not
    resolvable, row columns [M N]") where plan-time 42703 is due. Fix: the deriver must recognize a name
    that is a join-bodied CTE (cteOnScopes membership) and decline to plan-time rather than resolving
    through the table. LATENT
    catalog-first (masked by the text fallback / upstream gates today; goes live when the text fallback
    retires): buildWherePredicateForJoinsWithCTEScopes.addSource (~:1508, its own comment says
    "metadata first, then CTE scopes") plus ~7 more ResolveTable sites in the same masked category
    (~:320, :912, :3754, :5790, :6083, :6206 — derived-carrier and exists-planner scope builders).
    NINTH-family (review, all three gates ACK on 60268dc9e + the follow-up): the ON-only CTE schema
    (buildCTEOnOnlySource) is now COMPLETE-SCHEMA-OR-DECLINE — it installs ONLY when every runtime column
    (keyed by executeProjection's uppercased emit-name) is unique AND case-safe; ANY obstruction (a quoted
    case-sensitive alias `AS "x"`, or a duplicate runtime name incl. `AS "x", AS "X"` both emitting "X")
    declines the WHOLE source (caller's loud 0AF00), never a partial table. WHY complete-or-decline and not
    partial-omit: the schema is ONE source of the enclosing join and the resolver decides bare-ref ambiguity
    by which SOURCES carry a name (scope.ResolveColumn), so a DROPPED-but-runtime-present column lets a bare
    ref silently REBIND to another enclosing source (`WITH C AS (… AS AID, … AS AID, … AS Y) … FROM C JOIN
    LA L ON AID = L.AID` returned rows instead of 42702 — review-caught silent-wrong; Q55(c) pins it loud).
    TWO reach-restorations for a future conformance slice (both read-surface, NOT wire-visible): (a) the
    POISON-MARKER — keep the UNIQUE columns of an obstructed body usable while making the obstructed name
    resolve AMBIGUOUS via a per-source ambiguous-names set checked in scope.ResolveColumn/ResolveQualifiedColumn
    (Java's per-attribute 42702 model) — restores the unique-Y-in-dup-body reach complete-or-decline now
    declines (Q55(d)/Q54); (b) execution's quoted-alias UPPERCASING itself (`deriveProjectionColumnDef`
    `label = ToUpper(alias)`, `OutputColumnName`, executeProjection Datum keys, the RFC-173 positional-frontier
    slot names, every downstream re-reader) — Java's `getColumnLabel` returns `x`/`MixedCase` case-preserved;
    Go loses it. Graefe scoping: (i) scope the fix as the INVARIANT not the label — the positional-frontier
    slot names are the LOAD-BEARING site (a label-only fix re-creates the half-fix runtime failure); (ii)
    gate = a cross-engine DIFFERENTIAL proving Go's getColumnLabel/resolution case matches Java's for quoted,
    unquoted, mixed aliases (a conformance-parity claim → needs the harness). When (b) lands, Q55(a)'s two
    mustLoud pins flip decline→answer (matching Java). Both sequence AFTER the S4 demolition unless a shift
    picks up conformance. SEQUENCING (Graefe): (a) and (b) INTERACT — if (b) the execution-uppercasing slice
    lands FIRST (quoted identifiers case-preserving end-to-end), the case-sensitive obstruction class
    DISSOLVES entirely (an `AS "x"` body then emits key `x`, advertisable truthfully), leaving only the
    genuine dup-name class for the (a) poison-marker. Build (a) against the POST-uppercasing identifier model
    or it encodes a workaround for a bug (b) removes. The gate covers BOTH the projection path (Q55) AND the
    AGGREGATE path (Q56 — buildDerivedTableSourceFromAgg, folded via NewUnquoted with its output names
    already normalized, so the quoted flag was re-read off the Uid via cteBodyAllAliasesCaseSafe — STALE
    as of RFC-237: that helper is deleted and the capture keeps its spelling, so there is no flag to re-read;
    both the schema build and the dup gate consume the visible-only aggOutputCols authority so a hidden
    HAVING/ORDER-BY aggregate is neither advertised nor false-counted). (c) NEW (Graefe, pre-existing,
    general-read surface — NOT ON-only, NOT the rebind class): a WITHIN-SOURCE duplicate aggregate/projection
    output name in a DERIVED-TABLE or GLOBAL-CTE source silently LAST-WINS-resolves instead of 42702 —
    `WITH C AS (SELECT MIN(LA."AID") AS M, MAX(LA."K") AS M FROM LA) SELECT C.M FROM C` returns 110 (last-wins),
    Java's SemanticAnalyzer 42702s the ambiguous C.M; identical on the derived-table path
    `(SELECT … AS M, … AS M) AS d … d.M`. Single source, within-source ambiguity — belongs to the
    output-naming-authority / poison-marker family (resolver-level per-attribute 42702), booked here not
    fixed. (Distinct from the wrong-case-accept on those same paths, which is value-correct — the (b)
    uppercasing divergence — not a silent-wrong.) (d) NEW (Graefe ruling, LATENT executor silent-wrong, part
    of the (b) read-surface uppercasing family):

    **SUPERSEDED IN BOTH DIRECTIONS BY RFC-237 §8/§10 — read this before the text below.**
    (i) The MECHANISM described here is GONE: `aggResultName` no longer ToUppers,
    the naming authority carries its operand verbatim, and the operand mint now
    keeps a STRING LITERAL out of the fold, so `COUNT(CASE WHEN s='x' …)` and
    `…'X'…` publish two distinct names. Measured, both directions.
    (ii) The SEVERITY was wrong: this is NOT latent. The claim below that grouped
    CASE aggregation "does not compute in this engine" is false for the scalar
    form — it computes, and it computes WRONG. With both names now distinct the
    pair still returns `[2, 2]` where `[0, 2]` is correct, because the projection's
    ordinal bind matches aggregates by a RENDERING (`COUNT(CASE(WHEN(predicate),
    [1]))`) in which the predicate is opaque, so the two are indistinguishable to
    it. Localised: the same pair outside an aggregate answers correctly, and a
    pair whose literals differ by more than case answers correctly. Reproducer
    and controls are pinned in `quoted_identifier_aggregate_labels.yaml`; the
    open half is booked at the end of this file.

    Original text, kept because its reasoning about WHY a folded slot key is
    dangerous is still the right reasoning: aggResultName (pkg/recordlayer/query/executor/executor.go
    ~:2490) ToUppers the aggregate group-result slot key (`strings.ToUpper("SUM(%s)")` etc.), and
    finalizeGroup (streaming_cursors.go ~:349) writes each value under that folded key — so two aggregates
    differing only in a CASE-SENSITIVE token (a string literal: `COUNT(CASE WHEN s='x' …)` vs `…'X'…`)
    COLLIDE into one slot (Graefe confirmed at the datum level: `'x'` came back as slot key `'X'`; two
    case-differing aggregates → a single folded slot). Currently LATENT, not live: the only shape whose two
    case-differing aggregates compute DIFFERENT values (a string literal in a GROUPED CASE aggregate) does
    NOT evaluate in this engine today (grouped COUNT(CASE)=0, SUM(CASE)=nil), so the collision never yields
    observable wrong rows. If grouped CASE aggregation is ever made to compute, this becomes a LIVE
    silent-wrong across ALL callers (top-level HAVING too, not just ON-only CTEs) → fix aggResultName to
    case-preserve the render, or resolve HAVING/ORDER-BY refs by the case-preserved agg.Alias key
    finalizeGroup already writes (the HAVING/ORDER-BY resolution key is untraced — confirm which key it
    binds by). FRAMING CORRECTION (Graefe): the pre-004e215f2 ToUpper dup-gate that declined this shape was
    ACCIDENTALLY PROTECTIVE (consistent with the executor's ToUpper), NOT over-declining — the shape does
    NOT execute correctly at the slot level, it is merely masked by grouped-CASE-not-computing. This is why
    the fix must NOT go in the ON-only gate (principle-10 mis-location AND it would re-break the
    `COUNT(*) … HAVING COUNT(*)` false-decline: that harmless same-value collision is indistinguishable
    from the harmful case-differing one at the gate) — it belongs at aggResultName. A sentinel comment lives
    at the aggResultName site; the red→green pin lands WITH the fix (needs a string-column fixture).

  - [ ] **BOOKED (enclosed-CTE consult finding — LATENT collision hazard): the derived-table
    qualified-ref→bare-read rewrite.** `FROM (SELECT a.k AS x FROM …) AS d … WHERE d.x = 1` resolves d.x
    by rewriting to a BARE `x` read at build time — collision-unsafe in principle when another visible
    source carries an `x`. No wrong-rows repro today; audit + pin when the output-naming slice (above)
    lands. SIBLING (review round-4 note, pre-existing): nested same-named CTEs where a join-bodied INNER
    shadows a derivable OUTER — the outer's schema stays visible in cteScopes while the inner registers
    only an ON-only MARKER, so resolveTable falls through to the wrong-generation schema. Ultra-rare
    shadowing semantics; same audit.

  - [ ] **BOOKED (EXISTS-composition commit-1 follow-ups, RFC-173 S4 collision mint — see the RFC entry):**
    (a) **mint-per-leg for MULTI-SOURCE colliding EXISTS inners** — `EXISTS (SELECT 1 FROM OT OI, ST WHERE
    ST.c < OT.k)` with outer leg ST still answers per-outer-row (live Java: 3 rows, inner-shadow); commit 3
    of the slice lands the interim LOUD 0A000 decline, mint-per-leg closes the conformance gap for real.
    (b) **buildCorrelatedScalar sibling mint** — the correlated-scalar fallback shares buildCorrelatedExists'
    walk/qualify structure and therefore the same capture class; needs its own three-way-discriminating probe
    first, then the same mint. (c) **Java's array-constructor literal** (`INSERT … VALUES (1, 100, [10, 200])`,
    RelationalParser `arrayConstructor`) → Go 0A000 "unsupported expression atom ArrayConstructorExpressionAtom"
    — reach gap found by the live-Java probe; Java populates array columns via SQL, Go cannot. (d) **Case-2
    nested-EXISTS hoist dangles a middle-ref** — `EXISTS(SELECT 1 FROM M WHERE EXISTS(SELECT 1 FROM N WHERE
    N.b = M.a))` (middle has ONLY the nested EXISTS) hoists the inner plan and drops M entirely, leaving
    `M.a` unbound — pre-existing, unchanged by the mint (minted it resolves loud-deterministic instead of
    accidentally binding a same-named outer); needs its own probe + decline-or-fix. (e) **outer-CTE-leg scope
    registration** — buildOuterScopeSources.addSrc silently drops a resolve-failed leg, so an OUTER CTE leg
    (aliased or not) never enters the correlated subquery scope: correlated refs to it die 42703 (same family
    as the fixed derived-table registration), and an ALIASED CTE leg quoted `"Q$N"` escapes the mint's
    visible set (the unaliased half is folded via cteScopes names). Register CTE legs from the registry's
    ScopeSource like derived tables. SAME FAMILY: a correlated-fallback MIDDLE's JOIN-LEG names are absent
    from nestedOuterScopes (only the primary is appended), so an inner-inner ref to a middle leg that
    shadows a different-table outer alias silently binds the OUTER table — walk-time mis-binding, needs the
    same scope-registration fix. (f) **multi-EXISTS-over-unnest reach gap** (commit-2 review battery,
    live-Java-grounded): TWO sibling EXISTS subqueries over a lateral unnest fail to plan in Go ("best
    expression is not a physical plan"), colliding or not, correlated or not — pre-existing on the slice
    parent, LOUD, orthogonal to the mint. Java ANSWERS both shapes (constant-true pair → all elements;
    correlated pair OT.k>X AND MA.c<X → {10,20} on the probe fixture) → shared-surface parity gap, needs
    its own slice (likely the exists-filter path only carries one ExistsSubqueries consumer per unnest
    select). (g) **w4b in-tree quantifier mints** (rfc173_w4b_clustered_outer.go:149/:673 — outerCorr/
    innerCorr) mint raw UniqueCorrelationIdentifier into the SAME expression-tree namespace as user-named
    correlations — the translator-side half of the namespace law ("no generated identity equals a
    user-visible name"). No out-of-surface argument has been constructed for them; route them through a
    visible-name skip (the translator needs its own visible-set authority) or write the argument at the
    site. (h) **uncorrelated scalar-in-projection silent NULL + label leak** (commit-3a review battery,
    pre-existing, shape-general): `SELECT (SELECT MAX(C) FROM OT) FROM ST` returns NULL rows with the
    internal mint label in the column name (`(SCALAR_SUBQUERY Q$N)`), while the CORRELATED twin answers
    correctly — contradicts the scalar-subquery-in-WHERE booking's claim that projection-position scalars
    work; Java-probe first (silent-wrong candidate on the shared surface), then reconcile the bookings.
    (i) **name-set roles refactor** (logic-inert, own commit): buildCorrelatedExists now carries THREE
    purpose-built name sets — outerAliases (walk-time ref matching for the ON split), visibleScopeNames
    (mint-collision closure), and scopeAmbiguousName's bound set (runtime binding collision). Each is
    semantically forced and locally documented, but the mispick hazard is proven (the display-alias
    over-fire was exactly answering the binding question with the resolution set). Consolidate the
    DERIVATION under one scope-walk authority with role-named projections (displayNames / boundNames /
    allVisibleNames), each documented with the QUESTION it answers, consumed by name at each site.
    (j) **one outer-routing authority** (when the multi-EXISTS composition wall (f) falls): Case-1 has
    shipped outside the placement invariant twice; per-case discipline is proven insufficient. Consolidate
    ALL outer-routing decisions through one polarity-taking authority (OuterOnlyJoinConjuncts is the hook)
    so the invariant is enforced structurally; retires the exemption hazard class.

  - [ ] **S4 atomic demolition** (LAST — gated on QP-REF-BIND items 1+2+3, the riders, and the
    unnest-residual slice; sequencing ruling banked in the RFC). **Gates now satisfied:** items
    1+2+3 merged (item 3 = #483); rider 2 (aggregate metadata) DONE on feat/rfc173-next; rider 1
    folds into S4 (flips as the ordinal seed widens); unnest-residual c1/c2/c3 DONE on
    feat/rfc173-next. **The correlated-scalar prerequisite is DISCHARGED (W4b #465)** — JOIN-inner
    + COMPUTED-scalar ordinalize; the lone `NewScalarSubqueryAnchoredRecord` producer is now the
    UNGATED-OUTER residual (cascades_translator.go:3860). **Reachability: strong evidence :3860 is
    near-dead.** Probed with a loud marker: (1) no existing sqldriver/core-query/embedded test hits
    it; (2) direct ungated-shape probes — a correlated scalar over an UNNEST outer
    (`SELECT c.name,(SELECT COUNT(*) FROM o WHERE o.cid=c.id) FROM c, c.arr AS x`) does NOT reach
    :3860 (it declines at PLANNING, "could not plan query" 0AF00 — a pre-existing limitation, not
    the anchored fallback); a dup-alias outer declines loudly in buildCorrelatedScalar; a
    plain-single outer ordinalizes. So the research's assumption that unnest-outer "keeps the
    anchored record" is WRONG — it declines elsewhere. **BUT S4 is NOT near-done: only the SCALAR
    producer (#1) is near-dead. The name model's CORE is heavily live** — `NewAnchoredJoinRecord`
    (producers #2 unnest + #3 join) at cascades_translator.go:318/:698/:1528 (every join + unnest
    result value), and `NewReEnumerationAnchoredRecord` (#4) at rule_partition_select.go:469
    (GROUP-BY over an anchored parent). Retiring these = making EVERY join/unnest/group-by
    ORDINALIZE = the full RFC-173 endgame, a fresh multi-shift campaign (the RFC's ~2-shift estimate
    is on top of these three producers, NOT just :3860). **Remaining for the S4 exit gate:** the
    empirical zero-producers proof must cover ALL FOUR producers (probe #1's last existential-ON
    shape; then retire #2/#3/#4 by ordinalizing their shapes), then the atomic deletion. Gated on a
    Graefe RFC DESIGN-ACK of the demolition plan + all four impl gates (codex incl.). See the
    corrected RFC Slice-4 PREREQUISITE block.
    **STALE-NOTE CORRECTION (the ":1102 core heavily live" para above predates the S4 slices):** every
    join/unnest/group-by shape now ORDINALIZES — the census sweep (TestRFC173CensusSweep, 18 families
    incl. join2/leftjoin/fulljoin/join3/unnest*/groupby*/exists*/box) fires 0 P4/P5 producers, and E-1b
    zeroed the LAST EXISTS family. The ordinalspike CERTIFICATE is GREEN at 505fc32c9 (1641 corpus
    entries, 0 carve-outs, 0 mismatches) — the force-ordinalize path is result-identical to the name
    model across the whole corpus, PROVING the atomic cap is safe to fire. ReEnumeration (#4) is
    downstream of P4/P5 (fires only for anchored parents → dies mechanically); legColumns:382 is a
    name-DERIVATION helper (fires on the ordinal path too), not a seed producer. **Demolition is
    UNBLOCKED** — remaining: Graefe design-confirm the fire + the atomic deletion commit + 4 impl gates
    + exit gates (dualwindow/1M-stress/rfc153).
    **E-1b DONE (505fc32c9, 4-gate ACK — Graefe design+impl, Torvalds, codex, @claude):** the Bakeable
    leg-conjunct INNER cluster under EXISTS (`… A.K=100 AND EXISTS(…)`) ordinalizes via the one-authority
    classifyLegConjunct + in-select gatedJoinLegTypes bake; the review round added the enclosed-CTE-leg
    seedWindowed guard (P1) + the real flat-leg drift safety net (bakeGatedJoinPredicatesChecked).
    **FULL-SUITE FLIP MEASUREMENT (ed19761c1) — the cap is ONE blocker away.** Graefe NAK'd firing on the
    1641/0/0 corpus cert (it's structurally blind — no array cols — so it never exercises the producer
    deletion). The REAL exit gate is the full-suite flip: forceOrdinalSpike=true over //pkg/relational +
    //pkg/recordlayer, panic at rule_partition_select:475 temporarily neutered so it runs to completion.
    RESULT: 2/33 targets red, 0 panics, **0 SILENT wrong-rows** — every real red is a LOUD "field LA.K not
    resolvable in the runtime row — malformed plan" (the RFC-077 group-by panic class is GONE). Reds =
    (A) name-model-assertion tests that RETIRE with the cap [Item2C4/C3, WedgeGate, BareTwin does_not_bake,
    FilteredBox scalar_subquery_unbakeable — delete/rewrite IN the cap commit] + (B) ONE real blocker: the
    ENCLOSED BOX-LEG. ROOT CAUSE: a plan-time PROJECTION bake miss — the box-leg's per-leaf windows exist
    (gatedJoinLegTypes/addBuriedBakeWindows, used by the PREDICATE channel) but the PROJECTION channel
    (SELECT LA.K in a CTE body / output cols / aggregate) leaves box-leg column refs as name-model
    FieldValue("LA.K"), which the ordinal runtime row can't resolve (values.go:586, Ordinal:-1). THE SLICE:
    bake enclosed box-leg column PROJECTION refs to their windows — the projection twin of
    bakeGatedJoinPredicates. Locus: translateProject (cascades_translator.go:4724) + output-col derivation
    (cascades_generator.go ~2733-2876, the "output order from result-value Type ordinals" hard part).
    **GRAEFE DESIGN-ACK (the endgame ruling):** (1) it's ONE windowing slice — verify scalar-conj-over-box
    (FilteredBox) is windowing not a verdict split; (2) the flip (default=true, panic-neutered) is the
    STANDING exit-gate dashboard, BUT the cap must make :475 not-panic because the box genuinely windows
    (never ship the neuter), greenness is suite-scoped ("suite-green + complement is loud", state in the cap
    commit), and enumerate the retire-(A) tests explicitly; (3) agg_count_over_gather FLIPS from booked
    plan-quality → CAP BLOCKER (the cap deletes its name-model fallback) — same windowing root, structurally
    aggregate-over-EXISTS-wrapper (INNER not box), one slice or paired follow-up; (4) before firing, run the
    flip on the B2-INCLUSIVE tree + verify a DISCRIMINATING dup-column shape. Fire the cap (2 circular
    declines + producers + ReEnumeration + §5 oracle, ONE commit) when the flip is 0-red-except-retires.
    This slice is the handoff's flagged "fresh focus / 0-row-planner-bug class" — the next focused push;
    red-first is the flip dashboard. See scratchpad/flip-measurement-505fc32c9.md.
    **ENCLOSED-BOX SLICE PROGRESS (flip-dashboard-driven, all Graefe-design-locked):**
    - PART 1 LANDED (55c9d43f5): route the ENCLOSED box-unnest through translateGatheredUnnestCluster
      under the flip (cascades_translator.go:1510 `|| forceOrdinalSpike`), so a box outer's ON stays the
      null-on-empty condition INSIDE the dissolve (Graefe (2)(b): keep the ON inside, NEVER window it →
      LEFT→INNER). BYTE-IDENTICAL in production; verified LEFT-preserving (2 rows not 0). Cleared the
      GraefeImplProbe2 enclosed-CTE-over-box class (~10 shapes).
    - 🚩 STRADDLE CAP-BLOCKER (deferred to the cap, Graefe ruling; NOT production-reachable — flip-only):
      boxbase_straddle (`… T4, T4C, T4.SARR AS X … WHERE T4.ID=Z`) 0-rows under the flip (name-key read
      of a box-base column over an ordinal multi-leg box base). The box-base-local locus is architecturally
      incapable (3 attempts reverted — both shapes identical at the box base; only the ABOVE straddle read
      differs, invisible to translateChainedUnnestJoin). FIX AT THE FILTER-REBASE SITE, IN THE CAP COMMIT
      (atomic with the enclosure-decline deletion). Cap-fix: TRY leg-window resolution of the straddle read
      (T4's OrdinalSeedLegWindows) → correct 3 rows; 0AF00 decline only if the qualifier is ambiguous.
    - LOUD reach gaps (Graefe: schedule, convert runtime strand → plan-time decline for cap-cleanliness):
      FullBoxChained, scalar-subquery-conjunct (the Unbakeable-verdict split — partial-bake the leg refs,
      leave the scalar as a bound comparand, a separate slice). Both already correct-or-loud.
    - 🚩 agg_count_over_gather CAP-BLOCKER (root-caused, same class as the straddle — ordinal-child-under-
      name-model-parent): the A,B cluster ordinalizes under the flip but E-1a's underAggregate decline
      (cascades_translator.go:2766) keeps the aggregate-over-EXISTS handling name-model → the EXISTS
      correlation reads A.K/A.AID over the windowless NLJ → strand (LOUD malformed, not silent). FIX at cap:
      remove the underAggregate decline + expose the gathered seed's windows through the existential-wrapper
      NLJ to translateAggregate's gatheredSeedBakeContext. JAVA-PARITY (lateral chained unnest IS Java —
      generateCorrelatedFieldAccess; the corpus's no-array-columns is a CORPUS gap) ⇒ ORDINALIZE (COUNT(X)=2),
      NOT loud-decline (which would be a conformance regression). Same for the straddle. COMPLETE atomic-cap
      execution plan (both blockers' fixes, reach-gap dispositions, cap mechanics, exit gates) in
      scratchpad/flip-measurement-505fc32c9.md. EXIT-GATE PROVEN CLEAN: full flip = exactly 1 silent-wrong
      (the booked straddle), all else loud/retire — the cap is safe to plan on the correctness axis.
      ✅ **agg cap-blocker DONE (f67a2c8c7 + 914aa226d, 4-gate in flight):** the DIRECT aggregate-over-EXISTS
      ordinalizes — gatheredSeedBakeContext peels the single semi-join wrapper (E-1a's under-aggregate decline
      masked a nil-seedQOV bug: seedElementSlots looked for the Explode as a direct quantifier; under EXISTS
      it's one level down). COUNT(X)=2/SUM(A.K)=200/GROUP BY X all bake correctly, 0 producers. Review (codex)
      caught that removing the decline WHOLESALE was too broad — CTE/DISTINCT-WRAPPED aggregates qualify the
      group key with the wrapper alias (unbakeable over the seed windows) → silent NULL/collapse. First fix
      NARROWED admission to the direct shape (declined wrapped → name-model); superseded — see below, the
      wrapped case now ORDINALIZES. Multi-EXISTS-under-aggregate stays name-model + LOUD (pre-existing
      planner gap, confirmed at parent — agg_multiexists_loud sentinel).
      🔬 **JAVA CONFORMANCE (6-reader workflow, HIGH confidence):** Java 4.12.11.0 FULLY supports GROUP
      BY (grouped+global COUNT/SUM/AVG/MIN/MAX via streaming aggregator, no index required — AstNormalizer
      rejects only OFFSET/LIMIT). The old translateAggregate comment claiming Java lacks GROUP BY was
      STALE/FALSE — corrected. Java ALSO plans GROUP BY over a multi-source-FROM derived table
      (GroupByQueryTests.java:699 `SELECT max(y) FROM (SELECT y,b AS L FROM t1,t2) AS q GROUP BY l`), and
      a CTE ≡ derived table structurally → the WRAPPED shape (CTE+lateral-unnest+GROUP BY) is Java-parity.
      ✅ **WRAPPED CASE NOW ORDINALIZED (no post-cap deferral — a query answering on master must not
      regress at the cap):** replaced the direct-gate decline with a recursive identity-wrapper seed walk
      — gatheredSeedBakeContext's findWindowedSeed walks the outer-quantifier chain through IDENTITY
      wrappers only (SELECT-*, DISTINCT; never a row-reshaping GROUP BY) to reach the seed, and
      slotInGatheredSeed resolves a BARE leg column (D.AID → seed A-leg window) when exactly one leg
      carries it. Ordinalizes correctly in BOTH flip states (works under the demolition) for IDENTITY
      wrappers at ANY depth — findWindowedSeed's walk is UNBOUNDED (visited-set over the finite plan
      tree), so a deep (≥6) DISTINCT chain reaches the seed rather than exhausting a bound and skipping
      the bake → NULL (Torvalds-caught depth cliff). Pins: agg_cte_groupby_leg,
      agg_cte_distinct_groupby_element, agg_cte_deep6_distinct_groupby_leg.
      🔒 **PROJECTING-CTE-aggregate class → correct-or-loud LOUD (review-caught silent-NULL, Graefe +
      @claude):** a subset/reorder/qualified derived source (`SELECT A."AID"`/`SELECT B.BID, A.AID, …`)
      reshapes the row — findWindowedSeed stops at the LogicalProjection (can't bake against the reshaped
      layout) AND the name-model path mis-names the projected column (output "A.AID" ≠ D-stripped key
      "AID" → NULL, PRE-EXISTING per @claude's parent differential). Both give a silent NULL group, so
      translateAggregate refuses the whole class LOUD (UnbakeableProjectedGatherError) via the
      positionalGatherUnbaked detector. Pins: agg_cte_projecting_single_loud, agg_cte_qualreorder_loud.
      🚩 **CAP-BLOCKER (booked):** ordinalize the projecting derived source CORRECTLY — resolve the group
      key against the PROJECTED output's own layout, not the pre-projection seed (same as the enclosed-
      box-leg PROJECTION bake). Java answers it (GroupByQueryTests:699 projects `SELECT y, b AS L`), so at
      the cap this must ordinalize, not decline-to-loud. Only the STRADDLE + this projecting-derived
      ordinalization remain as booked cap-blockers.
      📌 BOOKED (Graefe, separate follow-up, no correctness urgency — Verdict-None is conservative =
      correct-or-loud already): investigate whether the NON-aggregate E-1a admission (the executor-hoist
      path) can route through a translator-side bake (like gatheredSeedBakeContext) instead of the hoist,
      and whether its surviving `Verdict-None only` restriction is consequently over-conservative (same
      NLJ-hoist misattribution the aggregate fix disproved). Reach question, not a cap-blocker.
    **FLIP-DASHBOARD RED CLASSIFICATION (Graefe guardrail — the cap fires only when every red is one of):**
    (1) booked CAP-BLOCKERS fixed IN the cap (straddle 0-row, agg, FullBox/scalar-conj plan-time declines);
    (2) RETIRE-TESTS deleted at the cap (name-model-assertion tests: Item2C4/C3, WedgeGate, BareTwin
    does_not_bake, FilteredBox scalar_subquery_unbakeable); (3) NEW/UNEXPECTED → STOP + root-cause. A 0-ROW
    IS NEVER A RETIRE-TEST.
    **E-1a DONE (ce2777bc3, 4-gate ACK):** the INNER flat cluster under EXISTS ordinalizes (translator
    alias-aware leg+element bake over the seed windows; the NLJ physicalization drops the windowed layout
    so the executor hoist can't recover it — bake in the translator). Zeros its P5 firing. BOOKED
    plan-quality follow-up (Graefe): AGGREGATE-under-INNER-EXISTS ordinalization — E-1a DECLINES the INNER
    admission under t.underAggregate (COUNT(X)/GROUP BY X would collapse to one NULL group;
    gatheredSeedBakeContext needs the raw seed, hidden behind the existential wrapper's NLJ). Name-model
    handles it correct-or-loud today (pinned agg_count_over_gather → COUNT(X)=2); ordinalizing it needs
    exposing the seed through the wrapper to the aggregate bake — same class as the box's deferred cases.
    **B1 DISCHARGED (corpus-level ordinal-spike certificate GREEN):** the whole-gate
    force-ordinalize flip (`query.SetForceOrdinalSpike` guarding the three circular declines
    :143/:157/:190, in `pkg/relational/conformance/ordinalspike`) preserves EVERY corpus row —
    **1641 entries, 0 carve-outs, 0 mismatches**. This PROVES the atomic cap (flag + producers +
    §5 oracle deletion) is SAFE TO FIRE: any shape the cap would change now surfaces as a spike
    mismatch, never silently. The certificate retires with the name model. Remaining before the cap:
    B2 (item-3/unnest-residual/rider-2 to master) + the atomic deletion itself (codex-gated). Then:
    delete the flag + trio + the
    three seeds + `NewReEnumerationAnchoredRecord` (dies mechanically) + 8 value-layer flag
    branches + 4 executor consumers + `select.go:251` arm-1 + the §5 oracle (load-bearing until
    now) — ONE commit, which also LIFTS the W5 bare-twin duplicate-column decline (the
    circularity cut: e2e matrix must row-verify that class). Kill-list amendments recorded:
    OrdinalFieldName SURVIVES (ordinal infrastructure); `select.go:274` survives for the
    CTE-rename slice. Exit gate: EMPIRICAL zero-anchored-producers proof (caller-free
    constructors, exhausted decline reasons), never inventory argument.
    S4 rider (item-3 c4 review follow-up): extend the span/window cross-agreement
    harness with DATUM-KEY accounting — datumFromSpans was a fourth layout site that
    stayed misaligned while the three window sites agreed (the silent-zero aggregate
    inversion); the FDB cells tripwire it per-shape today, but the class closes
    structurally only when the harness compares Datum keys too. Moot if S4 retires
    the coexistence Datum outright — decide at demolition time.

  - [ ] SEPARATE later slices (F2/F3): CTE-rename `select.go:274` widening (gated on CTE-column-rename
    ordinalization); lazy name-identity arm deletion (gated on FULL FieldValue baking, NOT S4).

- [ ] Slice 5 closure invariant · [ ] Slice 6 extensions + ANSI headroom.


### [ ] query-engine (RFC-173, latent — surfaced by the 2-way EXISTS-in-ON review): F2-LEFT's isScanFamilyLeg is cteScope-BLIND — VERIFIED REACH, not silent-wrong (low priority)
`isScanFamilyLeg` (cascades_translator.go:3185) is a syntactic Scan-through-Filter walk with NO cteScope
resolution: a CTE-name scan whose body is a JOIN or an OPAQUE BOX (aggregate/union/sort) reads as
scan-family. The 2-way EXISTS-in-ON gate hit this (fixed with a cteScope-aware `scanFamilyLegCteAware`), but
`isScanFamilyLeg` is ALSO used by the F2-LEFT projected-EXISTS-over-LEFT-JOIN fold
(buildExistentialJoinSelect / existsLegBirthsPositional) to gate scan-leg-only LEFT boxes.
**VERIFIED empirically (Torvalds action item, resolved): a projected-EXISTS LEFT join whose preserved leg is
a CTE-backed JOIN gives `0AF00` (unplannable), and a CTE-backed AGGREGATE leg gives `42703` — both VISIBLE
errors, NEVER wrong rows.** The distinction from the 2-way gate: that gate built the ordinal seed DIRECTLY
(so a CTE box slipped through to wrong rows), whereas F2-LEFT folds through the EXECUTOR's `legIsOrdinalSafe`
gate, which rejects a CTE-backed non-scan leg → 0AF00/name-model, structurally never silent-wrong. So this is
a REACH gap (Go rejects visibly where Java may answer), NOT a latent correctness bug — priority downgraded.
Still worth the cteScope-aware unification for cleanliness (make scanFamilyLegCteAware the shared authority)
and to convert the 0AF00 into a fold where Java answers, but it is NOT a silent-wrong hazard. Not urgent.



### RFC-070 / RFC-083 follow-ups (lifted out of completed entries)
  - [ ] **Follow-up (RFC-070): `pushValue`-into-covering-result-value modeling gap.** Java's `MergeProjectionAndFetchRule` yields a bare `fetchPlan.getChild()` because `RecordQueryFetchFromPartialRecordPlan.pushValue` rewrites the projected value into the covering plan's own result value. Go's `WithCovering` only sets a flag (the scan still flows the full partial record), so Go compensates with a thin outer `Project`. Pushing the value into the covering result value would let both rule branches collapse to a bare child yield, matching Java. Cosmetic/architectural — current behaviour is correct.

  - [ ] **Follow-up (RFC-070): other transparent unary wrappers over joins.** `Map`, `Distinct`, `Limit`, `TypeFilter`, `FirstOrDefault`, `DefaultOnEmpty` still gate `WithChildren` on `isLeafReplaceable` and could exhibit the same nil-inner-over-join bug if a rule ever builds them with a placeholder inner over a join. Not currently reachable via SQL (projections route through `LogicalProjectionExpression`, not `Map`); the **blanket** gate removal is unsafe — it regressed `TestFDB_AggregateIndexUsage` by dropping the eq-filter on aggregation/DML wrappers (which embed filter semantics in their own plan). Each wrapper needs individual analysis if/when reachable.

  - [ ] **Follow-up (RFC-083): replace the guard + `AggregateSlots` marker with Java's `PromoteValue` projection nodes** — the single mechanism that both rejects-at-plan and widens-at-runtime, dissolving the dual lattice-encoding (guard + converters) and the load-bearing "aggregate-slot ⇒ guard" coupling (Graefe's end-state). Subsumes reliably typing `FieldValue`/`ArithmeticValue` projections, which then closes the **residual deferred cases**: bare-column `SELECT double_col → BIGINT` over an empty source, and `UPDATE … SET int_col = <double-expr>` — both currently rely on the runtime converter (correct for non-empty rows, miss the 0-row case).

- [ ] **Nested IN over an intersection extracts `InJoin(<nil>)`** (RFC-167
  shell instance). `WHERE b IN (…) AND c = ? AND a IN (…)`: the inner IN
  wrapper is never handed to WithChildren, so no per-wrapper relink reaches
  its nil-inner snapshot. PRE-EXISTING on master and LOUD (XX000, never
  wrong rows). Gate-pinned by
  `TestNestedIn_OverIntersection_GatePin` — that pin goes RED the day the
  planner learns the shape, and the author then replaces it with real
  plan + row assertions.


### RFC-182 2026-07-18 audit findings — matcher, compensation, rule hygiene, dead code
- [ ] `physical_vector_index_scan_wrapper.go:36-38` returns empty correlation
  (all 34 sibling wrappers propagate); memo leaf criterion `memo.go:522-542`
  requires ALL members quantifier-free where Java's Traversal uses ANY.

- [ ] Matcher arc (Graefe audit findings 1-3, one RFC): dependency-aware alias
  matcher (`AliasMap.findCompleteMatches`), MatchIntermediate permutation
  enumeration, retire the phantom `WithSwappedQuantifiers` double-fire.

- [ ] `ForMatchCompensation.Intersect` early-returns Impossible when either
  input is impossible; Java's WithSelectCompensation.intersect recomputes (an
  impossible function on a NON-shared predicate drops out and the fold can be
  possible — Compensation.java:762 case 2). Conservative: missed plans, never
  rows. (Graefe impl-review finding, rfc182-row-soundness.)

- [ ] Rule-registration hygiene: `RemoveRangeOneRule` dead in Java but registered
  in Go; `DecorrelateValuesRule` double-registered; `OrderedPrimaryScanRule`
  zero tests; `PredicateToLogicalUnionRule` REWRITING-vs-PLANNING phase.

- [ ] ~750 LOC verified-dead code sweep (windowed candidate, `in_source.go`,
  `rule_demorgan.go`, `IntersectionInfo` island, `derivations_evaluator.go`
  computed-never-read).


- [ ] **Follow-up (Graefe v5 ACK condition): replace the `isSimpleResidualCompensation` allowlist with
  Java's exploratory-yield re-optimization.** Java yields data-access compensations via
  `FinalYields.yieldUnknownExpression` — a non-`RecordQueryPlan` lands in the *exploratory* set and is
  re-optimized by the normal PLANNING loop, so EVERY compensation shape is realized uniformly. Go's
  `pushDataAccessTasks` only `InsertFinal`s, so `implementDataAccessCompensation` + the
  `isSimpleResidualCompensation` allowlist stand in for that primitive. The allowlist is correct and
  each exclusion is pinned today, but it will rot the moment a new compensation shape appears with no
  allowlist arm (falls through to the dead-final-member path → silent no-plan). The honest fix is a Go
  `yieldUnknown`/exploratory-insert that re-optimizes all compensations and shrinks the allowlist to
  nothing — BLOCKED on Go's compensation re-optimization correctly handling IN-explode / correlated /
  index-only shapes (a naive exploratory-insert re-breaks them today, which is why the allowlist exists).

Go reaches a physical index scan / filter via THREE producers that bypass `Compensation`: the
data-access/compensation match path (`predicate_multi_map.go`), the Go-only `ImplementIndexScanRule`
(a fusion of Java's `ImplementPhysicalScanRule` + candidate matching that iterates predicates
directly), and `ImplementFilterRule` (synthesizes a `RecordQueryPredicatesFilterPlan` over the inner
winner). Java has ONE path (`AbstractDataAccessRule` → `toEquivalentPlan`) and enforces "index-only
value can't be a residual" ONCE via `Compensation.isImpossible()`. Because Go's extra rules don't
route through `Compensation`, RFC-045 enforces the index-only compensatability guard at multiple
layers: `valueContainsUncompensatable` (match path) + the residual-skip loop in
`ImplementIndexScanRule.OnMatch` (implement-index path) + a final-plan validation
`validateNoIndexOnlyResidual` in `Planner.Plan` (the `ImplementFilterRule` leak can't be guarded at
the rule — removing its member collapses the filter Reference and breaks the data-access intersection
memo, so the leaking *final* plan is rejected with `UnplannableIndexOnlyResidualError` instead).
All are load-bearing and pinned (`TestVectorPlan_QualifyPlansToVectorScan`,
`TestImplementIndexScanRule_SkipsIndexOnlyResidual`, `TestVectorPlan_MetricMismatchDoesNotMatchVector`),
so there is **no live bug** — but the layering is a smell whose root is the duplicated paths. Root fix
(Graefe-endorsed): retire `ImplementIndexScanRule` and route `ImplementFilterRule`'s filter
implementation through a single data-access rule backed by `Compensation`, at which point the
implement-layer guard AND the final-plan validation delete themselves and the property is enforced
once, as in Java. See DIVERGENCES.md "ImplementIndexScanRule is a Go-only second index-scan path".
  - **RFC-076 v3 ACK'd (Graefe + Torvalds), committed `75bf8d17`. v2's leaf-matching diagnosis was
    FALSIFIED by empirical reproduction.** Disabling `ImplementIndexScanRule` + tracing shows the
    match infra fires correctly (leaf scan↔scan `EqualsWithoutChildren=true`; `matchSingleSourceAgainstSelect`
    binds the predicate to the candidate Placeholder; `pushDataAccessTasks` fires) — the gap is that
    every seed-match path builds its MatchInfo with `maxMatchMap=nil`, so `PartialMatch.PullUp`
    (`partial_match.go:117`) returns nil → `CompensateCompleteMatch` → `ImpossibleCompensation` →
    `DataAccessForMatchPartition` skips → ZERO scans. `ImplementIndexScanRule` is the SOLE producer.
    `ComputeMaxMatchMap` (`max_match_map.go:167`) exists but is never called by the seeds.
  - **WIP STASHED (`git stash list` → top of stack on this branch).** Implemented the data-access
    completion per the Graefe-confirmed Java recipe: wire `ComputeMaxMatchMap` into the seed paths
    (leaf uses an identity map over the candidate result value; intermediate uses query/candidate
    result values + `NewAliasMapValueEquivalence`), residual compensation (re-apply unmatched
    predicates as filters via `OfPredicateCompensation` — Java produces the match even when fully
    residual), an IN-sargable guard (an IN comparison is NOT a contiguous range — leave it to the
    explode/InJoin path), and per-ref `AdjustPartialMatchesForRef` in `pushDataAccessTasks` (matches
    are seeded in PLANNING exploration, after the dead phase-start `AdjustMatches`, so ordering parts
    are only computed at consume time). **Validated:** full cascades unit suite GREEN with the rule
    enabled; 12/16 cited shape tests green with the rule disabled.
  - **REMAINING (multi-shift, per-feature vs Java — bigger than v2 stated):** broad `just test`
    exposes that the new (Java-correct) matches diverge from the rule's plans: (1) Go cost/Pareto
    pruning lets a non-unique index beat the unique one + breaks index intersection (`plangen`
    `UniqueIndexPointLookupPreferred`, `EndToEnd_IndexIntersection`); (2) `wrapScanPlanWithCoverage`
    (`abstract_data_access_rule.go:345`) doesn't propagate the candidate `unique` flag that
    `OrderedIndexScanRule` sets; (3) vector index-only-residual: a metric mismatch no longer raises
    `UnplannableIndexOnlyResidualError` (4 `TestVectorPlan_*`); (4) **DELETE over-deletes** →
    `TestFDB_DeleteOldAndLowValue` panic (correctness); (5) sort-elim ordering parts now computed but
    the satisfaction→ordered-scan→`RemoveSort` chain is incomplete (4 `TestSortElim_*`); (6) covering
    full-index-scan vs table scan (`TestPlanHarness` covering/range). Grind each rule-disabled,
    red-first, aligned to Java/plandiff; do NOT one-off guess (a `boundCount==0` guard diverged from
    Java and broke a Java-aligned unit test). THEN retire the rule + guard + final-plan validation.
    `ImplementFilterRule` STAYS (faithful Java port). Separate PR from RFC-077.
  - **RFC-076 v4 (2026-06-04): step 1 DONE (5 correctness fixes, Graefe+Torvalds ACK), full retirement
    in progress.** The data-access path is now correct for every FDB-tested shape (dual-correlation
    joins, simple joins, aggregate eq-filter, vector residuals). Full rule retirement needs: (3a)
    activate the dormant ordering-constraint pass (`constraintOnly` never set true → `PushRequestedOrderingThrough*`
    inert); (3b) template-aware costing (a nil-inner `Fetch` shell hides its inner from the cost model
    → join-order flip on `TestFDB_JoinSelPred_Repro`). See RFC-076 "v4 amendment" for the sequenced plan
    + the ref-resolving (not magic-constant) 3b. `validateNoIndexOnlyResidual` STAYS (now load-bearing
    via the DistanceRank residual). **Step-2 cleanup TODO (file/do during retirement, by the retirement
    PR): stop SEEDING `AggregateIndexMatchCandidate` partial matches onto non-GroupBy refs** in the
    leaf/intermediate match rules, so the agg-skip type-switch — currently duplicated 4× (`planner.go:465`
    data-access boundary [new], `rule_implement_index_scan.go` [dies with the rule], `rule_streaming_agg_from_index.go`,
    `rule_aggregate_data_access.go`) — collapses to one. Torvalds flagged the boundary guard as a
    defensible transition shim, NOT the permanent design; the don't-seed fix is the root cause.


### [ ] QP-REF-BIND: per-reference binding + the deferred ordinal classes (impl-review condition, W4-left)

One authority for three deferred pieces the W4-left review flagged (previously scattered as
"the 7.1 charter" — TODO 7.1 [alias-namespace unification] is DONE; this is the SUCCESSOR item):
1. **Per-reference ambiguity + fresh-id gating for duplicate FROM aliases** — Java's exact
   per-attribute 42702 at reference resolution (Go approximates at the FROM walk today; two
   marked divergence corners in the corpus: SELECT-*-over-duplicates, the predicated
   disjoint-column form).
   **DESIGN SUBSTRATE BANKED (2026-07-05, RFC § "QP-REF-BIND item 1 — design substrate"):
   19-shape live-Java probe verified every premise (SELECT * answers with duplicate labels;
   per-attribute WHERE binding; "Ambiguous reference X" byte-text for dup AND distinct
   aliases; qualified-star/table-row findFirst leftmost; the `..., a AS b` 42712 lazy quirk).
   Mechanism M1–M6: scope accepts duplicates + per-attribute qualified resolution; F3-ruled
   per-leg binding ids (later duplicates mint fresh); gate lift + binding-keyed seed; star
   layout fork F-A and message-unification fork F-B for the Graefe design ruling; ordering
   constraint — front-end acceptance and back-end binding never live separately
   (mis-pushdown wrong-rows hazard).
   **ITEM-1 COMPLETE (2026-07-05, PR #481): c1 (dark mint + binding-keyed seed, 34872539b +
   fix round) + c2+c3 (the lift — per-attribute resolution + FROM-walk 42702 retirement +
   SELECT-* star layout, 5860e3454) + the review-response (4e78ef2c2), Graefe ACK + Torvalds
   ACK on HEAD. Java's per-attribute model is LIVE: duplicate FROM aliases register per-leg,
   references resolve per-attribute (42702 at resolution, byte-equal to Java), SELECT * answers
   with Java's positional duplicate-column layout. All three flip corpus entries at parity
   (annotations deleted); dual-window + live-Java conformance + 1M stress green. Full record in
   RFC § "QP-REF-BIND item 1 — c2+c3 record". codex + @claude remain the PR-side gauntlet.**
   **c4 — the review round (2026-07-05): the PR gauntlet caught two REAL post-"COMPLETE"
   bugs (independently confirmed by both PR-side reviewers), both reproduced red-first and
   fixed; pinning their fold twin exposed a third. P1: dup-alias ORDER BY/GROUP BY keys kept
   the display alias while the gated join row is binding-keyed (silent wrong rows / NULL
   group keys) — sort+group keys now route through ResolveQualifiedProjection, group-key
   datum keyed by bare Field. P2: correlated EXISTS over an un-collapsed cross join — bisect
   pinned the buried-reference class as an ITEM-2 regression (worked pre-item-2) —
   duplicate-preserving outer scopes + gated flatten binding correlations +
   rebasePlanOuterRefsOrdinal (buried refs in the existential subplan → merged positional
   row, expression-gated, fail-closed verified). P3: the projected-EXISTS fold served NULL
   for a later dup leg's columns — buildExistentialJoinSelect + classifySortSource now speak
   bindings end-to-end. 7-shape FDB pin + 6 corpus entries live-verified + 2 dual-window
   declared-difference carve-outs (binding-qualified reads exist only positionally). Full
   record in RFC § "QP-REF-BIND item 1 — c4 record". Follow-on booked below: aggregate
   output metadata drift (labels/types) vs Java.
   MERGED (2026-07-06): PR #481 squashed to master 36c938f0a after c5 (the minted-binding
   loud-decline guard, RFC § c5 record) and c5b (P4e/P4f pins) — four-gate ACK on a789b66a9
   (architecture + code-quality + both PR-side reviewers), CI 6/6, stress at master parity
   (161.84s vs 161.68s). Item 3 UNBLOCKED.**
2. **Existential-flatten ordinalization** — translateJoinWithExists keeps the ANCHORED seed
   until the 2+1 select's data-access/correlated-FlatMap implementation paths bind legs
   POSITIONALLY (the ordinal seed was corpus-reverted twice: BakedNameContextError live).
   **DESIGN SUBSTRATE BANKED (RFC § "QP-REF-BIND item 2 — design substrate"): E2-validated
   executor binder; the W4-left rebase machinery is currently DEAD on live SQL (enclosure
   forced under EXISTS — the slice's commit-4 lift). DESIGN RULING: ACK with amendments
   (RFC § "design ruling + commit-1 record"). COMMIT 1 MERGED (PR #469, master 33291617d,
   four-gate ACK — Graefe, Torvalds after two converted NAKs, codex P1+P2 fixed-and-pinned,
   @claude ×4): the no-op existential residual (EXISTS == NOT EXISTS rows on
   LEFT+EXISTS) root-caused across four layers and fixed with the Java-shaped correlated
   step-1 (buildCorrelatedFlatMapPlan; the audited decline-only fix was insufficient —
   REWRITING promotion drops the unmerged member; full record in the RFC) + the 1+1 path's
   buried-leg rebase; unmasked matrix A–H pinned. DISCOVERED + FIXED on the branch:
   derived-alias EXISTS correlation 42703 (scope registration + the LogicalCTE alias
   carrier on all three derived arms) and the codex-caught CTE-shadow regression
   (cteShadowStack lexical scoping). LOUD-LIMITATION pins with exit gates (never wrong
   rows; flip to rows asserts): scalar-subquery-inside-EXISTS over a bare-scan outer
   (matrix class K — exit gate = the item-2 positional binders, commits 2–4);
   CTE-shadowed derived alias + EXISTS (buildDerivedTableSource resolves derived bodies
   against the catalog only — teach it CTE bodies); fetch-shell walk-terminators under
   planResultValue (same binder exit gate). The alias-unchecked frontier fallback
   follow-up (values.go evaluateCorrelated) dies with commits 2–4.**
   **CHARTER EXTENDED (post-W4-left sequencing ruling): absorbs the under-existential unnest
   class (`FROM t, t.arr AS v WHERE EXISTS(…)` — the W5 F4 rider's booking gap, closed here)
   and the EXISTS-rider clusterArity poison (a cluster whose leg filter/project carries
   exists subqueries) — same root cause, one slice, one review. The SCALAR-rider poison
   absorbs CONDITIONALLY: same binders → same slice; W4b-seed rework needed → immediate
   follow-on. Each absorbed class gets its own gate-reason string + dualwindow pins.**
   **ITEM-2 CHARTER COMPLETE (2026-07-05): commits 1–4 (PRs #469/#471/#472/#475) + 5a (#476,
   the structural exit gate) + the B/C/D/E wrong-rows batch (#478) + 5c (#479, class-K →
   rows) + 5b (#480, rider transparency) ALL MERGED, four-gate each; the EXISTS-rider and
   uncorrelated-scalar-rider poisons are LIFTED and the under-existential unnest is ordinal.
   Item 3 (below) now unblocked.**
3. **Mixed-nesting LEFT widening** — the joined-preserved class (clustered legs under a
   LEFT/RIGHT box) stays pinned residual until the flattened-cluster seed can name buried
   sources (the W4 dissolution ruling's scope). Retires the gate's :138-141 clustered-leg
   poison, the :102-113 enclosure guard, and ordinalEligible's LEFT/RIGHT leg-ineligibility;
   with items 1+2 it JOINTLY drives NewScalarSubqueryAnchoredRecord to zero callers.
   **MUST land AFTER item 2** (the enclosure guard names existential/unnest parents;
   retiring it before positional binders exist re-opens the mixed-nesting wrong-rows class).
   **IN FLIGHT (PR #483, feat/rfc173-item3): design ruling banked (three gate-arm commits,
   amendments A–J; the zero-callers claim STRICKEN — deletion rides S4; the FOURTH site
   recorded). c1 MERGED-to-branch 1ac0fe54f (S1 box roots + amendments C/D/E/F + the F-C
   guard; rfc153 plan pins verbatim green). c2 e0fcd2496 (S3 + clusterArity preserved+1;
   the RIGHT-box name-collision subs-only rule at all three layout sites; amendment G
   FULL-over-LEFT pin; pins re-cut per H). c3 in flight: LEFT-box-dup flip PINNED (P5 —
   the item-1 c5 loud class narrows as designed), records, exit gates.**

**Sequencing (Graefe ruling, banked in the RFC):** (riders ∥ item 2 ∥ item 1) → item 3 →
unnest-residual slice → S4. The riders are standalone and start immediately:

- [ ] **Unnest-residual completion slice** (books A3's W5 fail-open declines: box-leg
      owners, multi-segment `t.a.b` paths, CTE/derived rotation owners, chained unnests;
      under-existential arrives via item 2's binders; the BARE-TWIN duplicate-column decline
      rides until S4 — folded into the atomic commit per the circularity ruling, with the
      differential covering it name-model-side until then).
      **Progress (on `feat/rfc173-item3`, pending atomic-slice merge + gate re-ACK):**
      c1 (classes 1+2 — box-leg owners + multi-segment struct paths) DONE, both in-session
      gates ACK + codex re-confirm tracked for quota. c2 (class 3 — CTE/derived owners via
      body-projection→descriptor, positive whitelist, P2a-closed) DONE, both in-session
      gates ACK. c3 (class 4 — chained unnests `t.arr AS x, x.sub AS y`) DONE: nested
      FlatMap-over-FlatMap residual, all 7 Graefe conditions pinned
      (`rfc173_w5_chained_unnest_fdb_test.go` 11 subtests + `TestSelectMergeRule_ChainedUnnestBarrier`
      white-box). Two real bugs found + fixed by the FDB e2e: 3+-link enclosure collapse
      (chained dispatch de-gated from `!prevEnclosure`) and AT-on-chained-owner false 42809
      (`atOnNonArraySource` now recognizes `FindOwnerUnnest`). Remaining before merge: slice
      exit gates (dual-window, 1M stress, rfc153 verbatim, live-Java), codex on quota reset.

- [ ] **Rider: the minted-binding loud-decline class flips to rows** (item-1 c5 —
      the review-round guard): the declared-loud shapes over duplicate FROM aliases,
      each pinned in rfc173_item1_keybinding_exists_fdb_test.go (P4a–P4f) with the
      never-wrong-rows drain assert. Exit gates per shape: (a) leg-independent EXISTS
      over a minted-binding gated flatten (P4e) — flips when the executor's
      identity-FlatMap positional pass-through gate widens to key on the outer's own
      ordinal seed (probeOuterBakedType is the probe; flat_map_cursor.go documents the
      widening as the follow-on); (b) narrowed-off-the-gate flattens/joins
      (existential-alias collision P4a, arity ≠ 2 P4b, enclosure) — flip per-path as
      each learns the ordinal seed (item 3 / the N-way flatten); (c) correlated SCALAR
      subqueries over a dup outer (P4c/P4d) — flips when the scalar lowering speaks
      bindings (buildCorrelatedScalar's guard names the gap; label note: surfaces
      0A000 in SELECT position, wrapped 42703 in WHERE position); (d) the UNION face
      (P4f) — a dup-alias branch's per-attribute reference stays display-keyed and
      dies loud at the executor's ordinal guard; UPGRADE to a typed
      translation-time decline, then flip with the branch's ordinal seed. ALSO booked
      here: (arity-scope boundary) the dup-alias ARITY-3 correlated buried-EXISTS
      stays a LOUD ordinal decline (the c4 buried-reference rebase is arity-2 —
      implementJoinWithExistential's 2+1 shape), the N-way flatten slice widens it;
      (unnest owner) dup-alias unnest OWNER resolution is first-match-by-alias, not
      per-attribute (`q AS a, u AS a, a.arr AS e` → loud 42703 naming the wrong
      source) — classify vs live Java when the unnest-residual slice lands.


### RFC-180 follow-up — one authority for row-shape transparency
- [ ] **Unify the two row-shape-transparency sets (Graefe nit):**
      `projectionOverAggregate` (translator) peels Filter/Sort/Limit;
      `underlyingGroupBy` peels Filter/Sort. One authority for "operators that
      pass the row through unchanged".

- [ ] **LIKE-prefix covering access path (RFC-180 Y4, plan-shape parity):**
      Java plans `WHERE name LIKE 'bl%'` over an indexed column as an
      UNBOUNDED covering index scan + residual LIKE + deferred FETCH
      (never a LIKE→range conversion — RangeConstraints.java:780). Go
      full-scans (rows correct). Implement the covering/filter-before-fetch
      path; then flip like_patterns_java's plan_not_contains pin to a
      covering plan_contains.

### [ ] RFC-183 residual: 32 no-quantifier memo edges (RFC-184 W2/W3)

RFC-183 drove genuine unreachable memo edges (a plan child its quantifier's
group cannot produce) from 158 to 0. It left 32 edges of a DIFFERENT class:
`scanPlanExpression`, the leaf adapter that reports no quantifiers while
wrapping a `TypeFilter(Scan)` that has children — so the memo models no edge
for that child. NOT a wrong-plan or wrong-rows defect today; the memo simply
does not see those children.

Ratcheted at a hard baseline of 32 by
`TestCorpusPlanReachability` (pkg/relational/conformance/explaindiff), which
FAILS if the count rises — so the class cannot grow unobserved.

Closing it is RFC-184's W2/W3 (`rfcs/184-plan-identity-structural-elimination.md`,
now on master), NOT a memoization change. Proven the hard way: retiring the
adapter for the bare plan drove the count to 0 but drifted 57 corpus queries /
49 shape flips — a point lookup became a full scan — because the adapter
supplies scan-comparison correlations and ordering/cost properties the bare
`PlanExprBase` does not. Plans must carry those properties first, verified
property-by-property. The inert half (GetRecordQueryPlan on all 41 plan types)
is already on the RFC-183 branch.

Do NOT "fix" this by re-retiring the adapter without the property work — that
is the change that caused the 49 shape flips.


### [ ] RFC-184 W2 — physical-wrapper collapse: 26 eliminated, deferred-winner tail remains

On branch `centralize-plan-equality-hash` (held for one super-thorough end
review per owner's one-big-PR ruling). Each `physical<Name>Wrapper` adapter
stored a plan's child edge a SECOND time (wrapper quantifier + plan snapshot);
collapsing makes the plan its own cascades expression carrying the LIVE memo
edge once, so the ordinal-binding "child stored twice" state is unrepresentable.

COLLAPSED (24, each `explain-differ differing=0`, full-hook green): scan, limit,
fetch, map, default_on_empty, explode, table_function, temp_table_scan,
temp_table_insert, vector_index_scan, index_scan, aggregate_index, values,
typefilter, insert, delete, update, intersection, merge_sort_union,
multi_intersection, recursive_level_union, recursive_dfs_join, union, projection.

ENABLER — DAG-aware plan extraction (plan_extraction.go): the `visited` map was
doing double duty — cycle guard (necessary) AND permanent de-dup (harmful).
Made it STACK-SCOPED (add on descent, `delete` on ascent, mirroring
extractTieBreakHash) so a SHARED sub-DAG (two UNION legs scanning t1) re-extracts
instead of dropping its 2nd child to nil. This unblocked union + projection.

DEFERRED-WINNER TAIL — the winner CRITERION is PLAN-SPECIFIC (breakthrough):
  Each of these plans ranges a child over a MULTI-MEMBER alternatives group.
  The DESIGN-C' INFRA (landed 5022c0d7f) makes this collapsible for plans whose
  correct child winner is the group's per-ordering winner (ref.Winner()):
    - planFromQuantifier consults ref.Winner() before the singleton panic;
    - planTypeFromQuantifier for GetResultType (type is member-invariant);
    - verifyNoShell + reachability tally count via GetQuantifiers not GetChildren.
  ✅ COLLAPSED on the infra (differ=0, memoinvariant+reachability+yamsql green):
    unordered_union (5022c0d7f), in_union + in_join (a6bb74b07) — the whole
    ref.Winner() set-op family.
  STILL WRAPPED — the per-plan empirical question is now ANSWERED: all four unary
  candidates REVERTED (each empirically tested on the infra, differ>0, tree left
  byte-identical). They are the genuine snapshot-decoupled tail; bare ref.Winner()
  floats their inner to the group's GLOBAL cost-winner, losing the frozen
  streaming-enabling / SARG-pinned member. Measured shifts:
    - distinct → differing=6 (distinct_join.yaml#1..3 + friends). The distinct-final
      rule yields distinct-over-EACH-inner-member (rule_implement_distinct_final.go
      :69,118), each snapshot-frozen so the cost model can pick
      distinct-over-(ordered inner) with STREAMING dedup. Collapse merges the
      InitialOf(m) singleton into the shared inner group → ref.Winner() floats all
      to distinct-over-(global winner), losing the streaming alternative.
    - streaming_agg → differing=3 (same shape: agg needs the grouping-key ordering
      to stream; float drops it).
    - in_memory_sort → re-sorts, wants cheapest-valid-ANY-ordering
      (findBestPhysicalPlan), not the per-ordering winner. BRACKETED earlier:
      ref.Winner() → 63 flips; no-Winner → 64 errors.
    - first_or_default → differing=3, predicates_filter → differing=16: NLJ-
      entangled SARG-push (constructed as inner legs inside the Graefe-gated NLJ
      rule); their float is SARG-push, not pure-ordering — collapse WITH nlj/flatmap.
  RESOLVED DIRECTION — mechanical collapse of the tail is EXHAUSTED; it is
  load-bearing architectural work. Two proposals tested + two empirical proofs
  (full ruling: scratchpad/w2-graefe-gated-remainder.md):
    - getWinnerForOrdering-at-extraction: Graefe NAK (cost-inversion; re-entangle).
    - relink-to-memo-winner (Graefe ACK'd as Java-faithful for SELECT): tested on
      distinct (cost-tied SELECT churn, differ=6 plan_regressions=0) AND on
      first_or_default — where it CORRUPTS DML ROWS. `DELETE ... WHERE EXISTS
      (correlated)` deleted ALL rows: the correlation lives in the SARG-pushed scan
      the fod snapshot froze; relinking to the non-SARG memo winner drops it.
  TWO GATE FINDINGS THAT BLOCK THE RE-BASELINE:
    - The explain-differ IS DML-BLIND — it dumps only SELECT stanzas (skips 252 DML
      stanzas). plan_regressions=0 does NOT cover DML. Only yamsql (real FDB) is a
      sound gate for the tail. The fod collapse read differ-clean while corrupting
      the DML DELETE/UPDATE-EXISTS path.
    - The tail wrappers' snapshots are LOAD-BEARING: correlation on DML paths
      (fod/predicates_filter), ordering preconditions (Streaming distinct/agg —
      relinking to a different-ordering winner is WRONG ROWS), join-order winners
      (rest). The single collapsed edge can't reproduce them; collapsing corrupts a
      differ-invisible path.
  So the tail is the correlation/ordering-PRESERVATION rework (the bare plan must
  REPORT the correlation/ordering its snapshot carried, or its inner group must be
  optimized under that correlation/ordering), a Graefe-designed owner-gated effort
  (its own RFC) — NOT RFC-184-W2 mechanical scope. Every further mechanical collapse
  attempt risks silent DML corruption; STOPPED pending the architectural design.
  The 2 dead structs (scan/filter) are removed (commit 2f1189b94).

NESTED_LOOP_JOIN + FLAT_MAP (joint) — GRAEFE DECISION, NOT deferred-winner:
  These two ARE Class B (singleton children; correlation flows through children,
  both wrappers' GetCorrelatedToWithoutChildren empty = plan default). The joint
  collapse was implemented + gated: build green, cascades+plans unit 2/2 (no
  PlanHash-nil — collapsing both together fixes the NLJ→FlatMap snapshot bridge),
  yamsql FDB conformance PASS (correct rows). BUT the corpus differ caught ONE
  shifted query, cte_error_codes.yaml#5 (a shared-CTE self-join):
    BEFORE: InJoin(PredicatesFilter(Scan(T1),[1 preds]), binding)   (residual filter)
    AFTER:  InJoin(Scan(T1,[=]), binding)                            (SARGed — better)
  plan_regressions=0; the AFTER is the canonical SARGed plan the cost model wants.
  Root cause: the NLJ wrapper's WithChildren keeps a plan SNAPSHOT (the residual-
  filter member); the collapsed NLJ honors the DAG-aware-extraction cost WINNER
  (the SARGed scan). So the wrapper is CURRENTLY MASKING a suboptimal plan on this
  query, and the collapse would fix it. This is not a regression — it's the
  collapse revealing the plan the cost model selected. But it breaks the
  differing=0 W2 soundness contract, so per STOP-on-shift it was reverted and
  flagged. Graefe must rule: (a) accept the improvement → land NLJ+FlatMap with
  differing=1 documented, or (b) require byte-identity first. The FromQuantifiers
  constructors + WithChildren are trivial to re-add; the 9-emitter repoint is
  mechanical once ruled. Recommend (a).
  INDEPENDENT CROSS-CONFIRMATION: a second session reproduced the full joint
  collapse from scratch and reached the identical result — same single shifted
  query (cte_error_codes.yaml#5), same before/after shapes, same root cause,
  unit + yamsql green. It additionally verified DETERMINISM on both sides
  (base-vs-base2 and cur-vs-cur2 each differing=0), so the flip is a stable,
  reproducible, benign improvement — NOT planning noise. Two independent
  derivations of the same conclusion strengthen recommendation (a).

WHY THE NAIVE FIX FAILS (proven, in_memory_sort): memoizing
getWinnerForOrdering(innerRef, PRESERVE) as a singleton child at YIELD would
satisfy the invariant — but RFC-069/076 DEFER the sort's inner winner to
EXTRACTION precisely because it is not stamped at sort-yield; getWinnerForOrdering
then falls back to findBestValidPhysicalExpr (yield-time cheapest = the
"yielded-first loser" RFC-069 warns against) → likely a cost-tied RFC-069/076
regression the differ may MASK. The real fix is architectural: either relax the
singleton invariant to resolve to the cost WINNER for deferred-winner plans (needs
the winner reachable from the plans package — ref.Winner() timing), or a
memo/reference construct that is "singleton for planning-time queries but defers
winner to extraction." Both touch the load-bearing singleton invariant → Graefe
gate applies. Full analysis + candidate designs A/B/C in the shift handover.

DESIGN C' ATTEMPTED EMPIRICALLY (in_memory_sort, then reverted — precise blocker
found): (1) planFromQuantifier consults ref.Winner() before the singleton panic;
(2) a new planTypeFromQuantifier (first-member, no panic) for GetResultType —
type is member-invariant for pass-through plans; (3) verifyNoShell (and every
planning-time STRUCTURAL "has children" check) uses GetQuantifiers, not
GetChildren (identity). RESULT: the singleton panic is GONE — TestSortElim_*
passes, the sort collapses. BUT the differ shifts 66 queries / 63 shape flips
(plan_regressions=0). ROOT CAUSE — the winner CRITERION is PLAN-SPECIFIC:
ref.Winner() is the group's winner for ITS requested ordering, but the sort
re-sorts so it wants the cheapest VALID member for NO ordering
(findBestPhysicalPlan, RFC-076) — the wrapper's WithChildren used exactly that.
ref.Winner() != findBestPhysicalPlan, so the collapsed sort bakes a dominated
ordered inner. So a UNIFORM planFromQuantifier→ref.Winner() is too coarse; the
deferred-winner rework needs PER-PLAN winner selection at extraction (sort:
cheapest-valid-any-ordering; set-op: per-leg; etc.), and that criterion lives in
the cascades pkg (findBestPhysicalPlan), which the plans-pkg WithChildren cannot
reach. The clean shape is a sort-specific extraction hook (like the LogicalSort
rebuildOrderedSpine that already exists) for the PHYSICAL sort — a Graefe-ACK'd
cascades change. Design C' infra (Winner-aware planFromQuantifier + type resolver
+ structural-check-via-quantifiers) is the RIGHT foundation; it needs the
per-plan winner hook on top.

SHARPENING (localizes the design): the sort's HintCost takes child costs as
PARAMS (does not walk GetInner) and HintOrdering computes from the sort KEYS
(not the inner). So the sort's cost/ordering are self-contained — the ONLY
planning-time GetInner/GetChildren callers are GetResultType (→ type resolver)
and verifyNoShell (→ structural GetQuantifiers). Therefore the PANIC is FULLY
fixable with those two changes alone; the GLOBAL ref.Winner() change in
planFromQuantifier is NOT needed for the panic and is what introduced the bias.
The 63-query shift is PURELY the extraction inner-selection: extraction
(extractBestPlanFromSelectorVisited) prefers innerRef.Winner() — an ordered
variant — while the wrapper's WithChildren OVERRODE it with
findBestPhysicalPlan (cheapest-valid, any ordering, RFC-076). The whole design
thus reduces to ONE localized change: a physical-sort case in the extraction
switch (rebuildWithFreshChildren / rebuildExpressionFromSelectorVisited) that
resolves the sort's inner via findBestPhysicalPlan, restoring the wrapper's
override. Everything else (the two structural/type fixes) is mechanical and
differ-neutral. That is the concrete Graefe-review unit.

BRACKETED EMPIRICALLY (two attempts, both reverted) — the findBestPhysicalPlan
extraction hook is MANDATORY, not optional:
  - WITH global planFromQuantifier→ref.Winner(): resolves the inner but to the
    WRONG winner (the group's ordering-winner, an ordered/dominated variant) →
    differing=66, shape_flips=63, plan_regressions=0 (correct rows, worse shape).
  - WITHOUT it (just the two mechanical fixes + collapse): TestSortElim_* still
    passes (panic gone — the sort's HintCost/HintOrdering don't walk GetInner),
    BUT extraction cannot resolve the multi-member inner at all →
    differing=109, plan_regressions=64, and 64 NEW plan errors (258→322).
  So the two uniform options bracket the answer: extraction MUST resolve the
  multi-member inner (some winner is needed — no-Winner gives plan errors) AND
  it must be findBestPhysicalPlan specifically (ref.Winner() gives wrong plans).
  The per-plan (sort-specific) extraction hook is the ONLY thing that works;
  the two structural/type fixes are its necessary companions. That is the
  precise, empirically-bounded Graefe-review unit for the physical sort.


### [ ] The fix is REMOVAL, not a guard — proven; and it is a Go-only FAMILY

Continued the DFS on the cyclic-reference crash by trying both candidate fixes
on the 4-byte seed. Decisive:

GUARD IS WRONG. Extending MemoizeExpression's existing direct-self-loop guard
(`ref.Canonical() == c.Reference.Canonical()` → return fresh InitialOf) to
TRANSITIVE reachability stops the stack overflow but converts it into
NON-CONVERGENCE: "exploration did not converge — possible non-terminating rule
interaction". Interning is what makes the inverse-rule fixpoint terminate;
declining it to break the cycle makes Push/PullCommonFilter ping-pong forever,
each pass adding a fresh member. Crash and non-termination are two faces of the
same problem — you cannot keep these rules AND have both a finite, acyclic
memo.

REMOVAL IS RIGHT. With the Push/PullCommonFilter-Intersection pair removed from
the rule registry, the same seed CONVERGES cleanly and instantly. Java reaches
the same plans via match-then-implement data access; the rules add nothing Java
lacks.

IT IS A FAMILY, not a pair. The same Go-only inverse shape exists for UNION:
`PushFilterThroughUnionRule` + `PullCommonFilterAboveUnionRule`. Java has no
filter-through/above-Union rule either. The Union pair is almost certainly the
same latent cyclic-memo / non-termination hazard and should be removed in the
same change (verify with a fuzz seed that reaches it).

THE FIX (RFC + Graefe ACK — this is an architectural decision, removing rule
families, not a local patch):
  - Remove PushFilterThroughIntersection + PullCommonFilterAboveIntersection
    and PushFilterThroughUnion + PullCommonFilterAboveUnion (4 rules, 4 tests,
    4 registry lines).
  - VERIFY zero explain-diff drift across the 2407-query corpus (Java plans
    these via data-access, so drift is expected to be zero; any drift is a
    query that leaned on the Go-only rule and must be re-examined).
  - Re-run the planner fuzz targets to confirm the crash class is gone.
  A guard in MemoizeExpression or GetCorrelatedTo is NOT the fix (guard →
  non-termination, proven above; GetCorrelatedTo guard → masks the cyclic memo).

All experiments were bounded and reverted; no code change is on this branch —
only this diagnosis.


### [ ] FUZZ sweep result: two distinct PRE-EXISTING planner crash classes

Swept the cascades planner/memo/extraction fuzz targets after the RFC-183
merge. Two distinct crash classes, BOTH pre-existing (neither is an RFC-183
regression), plus a clean set:

CLASS 1 — cyclic-memo stack overflow (GetCorrelatedTo, reference.go:752).
Confirmed on FuzzPlanner_MemoConsistency, _Determinism, _Idempotence,
_InitialMemberPreserved — all the full-exploration targets that run
DefaultExpressionRules on a seed reaching the Go-only Push/PullCommonFilter-
Intersection pair. Single root cause (the Go-only rule family, above); fixing
it clears this whole class.

CLASS 2 — "Plan succeeded but root Reference has no BestMember stamp"
(FuzzPlanner_PlanFullPipeline, planner_fuzz_test.go:115). DISTINCT from class 1
(instant assertion, not a stack overflow). Reproducing 34-byte seed
`311028b5ee5f305a`. PRE-EXISTING — proven: the identical seed fails on
pre-RFC-183 master (15dc17a82).
  SETTLED — it is a REAL planner bug, not an over-strict test. A deterministic
  search (not fuzzing) reproduces it at `b=[35 4 4 1]` = op 5 → UNION of two
  TypeFilter(Scan) children, i.e. `Union(TypeFilter(Scan), TypeFilter(Scan))`.
  The value `p.Plan` returns for it is a `*expressions.LogicalUnionExpression`
  — a LOGICAL, UNIMPLEMENTED expression — with nil error and no root winner.

  So `ExtractBestPlanFromSelector` fell back to a LOGICAL member because the
  root union has no PHYSICAL final (ImplementUnorderedUnion never produced one
  for this shape), and returned it instead of erroring. `Plan` therefore hands
  a caller a logical expression as if it were a physical plan — a downstream
  consumer expecting `plans.RecordQueryPlan` gets a logical node. NOT a
  canonicalization gap (HasWinner canonicalizes, reference.go:313,329); the
  root's winner is genuinely nil because nothing physical was ever chosen.

  REFUTED sub-question (b). The first note said "extraction must ERROR when the
  root has no physical plan." That was TESTED and is WRONG: adding an
  isPhysicalPlan check + error in extractBestPlanFromSelectorVisited (after the
  GetBest fallback) causes ZERO corpus drift but breaks four cascades unit
  tests — TestPlanner_GenerateDataAccess_NoMatchesIsNoOp,
  _PlanningPhase_AlwaysRuns, _GenerateDataAccess_BottomUp, _Plan_FullPipeline —
  which deliberately plan with LIMITED rule sets and RELY on Plan tolerantly
  returning a non-physical result (a bare FullUnorderedScan / LogicalFilter /
  LogicalDistinct that the isolation test never gave an implementation rule
  for). So Plan's tolerance of a non-physical root is INTENTIONAL, not the bug.
  Erroring universally would break rule-isolation testing.

  So the real question is only (a): WHY does the seed's union fail to implement?
  NARROWED to a precise TRIGGER and three REFUTED hypotheses:

  TRIGGER = ASYMMETRIC LEGS. buildFuzzExpression([35,4,4,1]) is NOT the
  symmetric union I first assumed; it is
  `Union(TypeFilter(TypeFilter(Scan)), TypeFilter(Filter(Scan)))` — one leg
  nests a TypeFilter, the other a Filter. Direct-construction isolation:
    - Union(Scan, Scan)                        -> implements
    - Union(TF(Scan), TF(Scan))                -> implements
    - Union(TF(TF(Scan)), TF(TF(Scan)))        -> implements (symmetric nested)
    - Union(TF(Filter(Scan)), TF(Filter(Scan)))-> implements (symmetric filter)
    - Union(TF(TF(Scan)), TF(Filter(Scan)))    -> does NOT implement  <-- seed
  Each leg implements STANDALONE; only the asymmetric UNION fails.

  REFUTED:
    (b) extraction-error — breaks four isolation tests (above).
    rule-selection — the seed fails under FULL DefaultExpressionRules too, not
      just the sparse selectRules(b) subset.
    planExprs[0]-ordering — ImplementUnorderedUnion collects childPlans from
      planExprs[0] only; changing it to scan the whole partition for a physical
      expr does NOT fix the seed, so it is not "physical present but not first."
      One asymmetric leg has NO physical expression in the partition at all.

  RESOLVED (commit "cascades: reset per-stage exploration on the no-finals
  stage boundary"). Root cause found via a full memo dump on the asymmetric
  shape: constant-true filter elimination makes `Filter([TriTrue],Scan) ≡ Scan`,
  so the right leg's group MERGES with the left leg's inner TypeFilter group.
  That merged group reaches the REWRITING→PLANNING boundary with ZERO final
  members (Go tolerates unfinalized groups via ExploreGroupTask's
  `len(FinalMembers())>0` guard; Java finalizes universally so never hits this).
  With no finals it cannot take AdvancePlannerStage (which would empty it), so
  it fell to the plain stage-set path that changed the stage but did NOT reset
  the per-stage exploration bookkeeping. The group kept its REWRITING
  `explorationDone` state → NeedsExploration=false → ExploreGroupTask returned
  early → ImplementTypeFilter never fired → no physical member → the union saw
  an empty leg partition and bailed → root had no winner while Plan() reported
  success. Fix: `Reference.AdvanceStagePreservingMembers` resets exploration
  state (explState, rounds, winner, planProperties, constraints epoch) while
  keeping members, so surviving logical members re-explore and implement in
  PLANNING. The dead no-reset SetStage method was removed. Pinned by
  asymmetric_union_planning_test.go + the `[]byte{35,4,4,1}` seed in
  FuzzPlanner_PlanFullPipeline; five planner fuzz targets clean at 45s each;
  full suite green. Third refuted hypothesis (planExprs[0]-ordering) was right
  that "one leg has NO physical expression" — the reason was the exploration
  reset, not the partition machinery.

CLEAN (no crash in the sweep): FuzzPlanner_E2E_NoPanic,
FuzzExtractBestPlan_SingletonInvariant, FuzzMemo_MemoizeInvariant,
FuzzPlanner_ProjectionPipeline_NoPanic — extraction, memo-invariant, and
projection paths are robust to these inputs.

Crash seeds NOT committed (they would red CI for gated bugs); recorded as
hashes/reproducers. All experiments reverted; tree clean.


- [ ] **CQ-30 (HIGH) — the cost model keeps a private cardinality derivation.**
  Java has exactly one `CardinalitiesProperty`, consulted by both the cached
  properties map and `PlanningCostModel.compare`. Go has two independently-coded
  switches: `plan_properties.go`'s `computeCardinalities`/`cardinalitiesForRef`
  (weakens across ALL final members via `WeakenCardinalities`) and
  `planning_cost_model.go`'s criterion-2 walk
  (`findExpressionsByType`/`scanProvableMaxCard`/`indexProvableMaxCard`,
  descending via `bestPhysicalChild` — a SINGLE cost-tie-broken member). **The
  comparator that actually elects plans uses the narrower private one.** This
  matters because `OptimizeGroupTask` deliberately retains multiple final members
  per group (one winner per distinct requested ordering), so "the group's
  cardinality" is genuinely ambiguous and two disagreeing answers live side by
  side. Fix: route criterion 2 through `GetRefPlanPropertiesMap`/
  `computeCardinalities` and delete the duplicate. Specified in
  `rfcs/195-cost-must-not-contradict-proof.md`, which covers this together with
  CQ-29 — they are the same defect (cardinality with two homes) seen from the
  two sides, and fixing one without the other leaves the disagreement intact.

  **PARTIALLY DONE via RFC-195; the criterion-2 half is still open, and RFC-195
  cannot close it as written.** What landed: the per-operator derivation moved
  out of `cascades.computeCardinalities` into per-plan
  `ProvenCardinalities` methods behind `properties.CardinalityProver`, so the
  property map and all three COST walks (`localCost`, `combineConcreteCost`,
  `partitionCost`) now consume ONE derivation; `computeCardinalities` is a thin
  adapter that only resolves child edges, pinned by
  `TestRFC195_AdapterDoesNotReForkTheDerivation`.

  What remains: `planning_cost_model.go`'s criterion-2 walk still derives its
  own data-access maxima via `scanProvableMaxCard` / `indexProvableMaxCard` /
  `scanPlanProvableMaxCard` / `indexPlanProvableMaxCard`. Java is unambiguous
  that this is a fork — `PlanningCostModel.java:336` maps every data access
  through `cardinalities().evaluate(plan).getMaxCardinality()`, i.e. through
  CardinalitiesProperty itself — so CQ-30's premise is CORRECT and this is a
  real divergence.

  It is NOT deferred out of convenience: RFC-195's own scope text rules it out
  ("the clamp makes the rung internally consistent; **the tiers above it are
  untouched**"), and its Decision section names exactly three cost walks, none
  of which is criterion 2. Routing criterion 2 through the unified derivation is
  a change to a Java-ported TIER, and it is not free: Go's variants take a
  `PlanContext` and can prove a bound from METADATA when the plan carries no
  stamped primary-key arity or index uniqueness, which the plan-local method
  cannot. Collapsing them naively LOSES those proofs and makes criterion 2
  abstain more often — a plan-movement change needing its own design ACK,
  plan-diff and stress run.

  **The ruling: the "sanctioned Go extension" framing is REJECTED.** Go's four
  `PlanContext` variants are not a capability Java lacks; they are a WORKAROUND
  for Go plans being under-stamped. Java's plans carry their match candidate, so
  `cardinalities().evaluate(plan)` is fully plan-local and needs no context at
  all. The eventual shape is therefore: stamp Go's plans with the metadata
  equivalents Java's plans already carry (match-candidate equivalents — primary
  key arity, index uniqueness and key columns), then DELETE
  `scanProvableMaxCard`, `indexProvableMaxCard`, `scanPlanProvableMaxCard` and
  `indexPlanProvableMaxCard` outright and route criterion 2 through the single
  `ProvenCardinalities` derivation, faithful to `PlanningCostModel.java:336`.
  Own RFC, own corpus plan-diff, own 1M stress run.

  Until that lands the fork is held VISIBLE, not merely noted:
  `TestRFC195_Criterion2AgreesWithTheProvenBound` requires criterion 2 and
  `ProvenCardinalities` to reach the same verdict on every data-access shape in
  the bound-test table, exceptions carrying explicit listed reasons. That gate
  exists because this exact fork already hid a live disagreement for a long time
  — see the false proof below.

  **THAT GATE IS CURRENTLY BLIND, AND TWO SENTENCES ABOVE ARE STALE.** Found by
  the unexported-dead-code gate; the authority is
  `unreferencedFuncLedger` in `pkg/docscheck/unreferenced_func_gate_test.go`,
  which this paragraph quotes rather than the reverse.

  1. Of the four functions named for deletion above, `scanProvableMaxCard` is
     ALREADY dead — zero production callers. The live logical walk's scan arm
     takes `scanPlanProvableMaxCard(scan, ctx)`; only the index arm still uses
     the plan-local `indexProvableMaxCard`. So the scan half of the deletion is
     not gated on the plan-stamping work at all.
  2. The visibility gate's scan arm calls that dead function. It is therefore
     not measuring criterion 2; it is measuring something criterion 2 stopped
     calling, and the context-enrichment axis — the ONLY axis on which the two
     derivations can differ, and the whole reason this fork is dangerous — is
     unreachable from it. Its shape table compounds this by stamping a primary
     key on every scan, where both derivations read the same field and agree
     trivially.
  3. Repointing the arm at `scanPlanProvableMaxCard(p, ctx)` and adding an
     UNSTAMPED scan turns the gate RED: criterion 2 proves max=1 under a
     PK-resolving context while `ProvenCardinalities` returns unbounded, which
     is the condition the gate fatals on. Latent today only because
     `PrimaryScanRule` stamps under the same conditions the context fallback
     needs — the same latency argument RFC-219 makes for the index arm.

  So the fork is not held visible; it is held green. Arming the gate is the
  first step of this item, and it needs the Cascades architectural review gate
  because the red it produces is a real disagreement to resolve, not a test bug.

  While relocating the derivation, one FALSE PROOF in this area was found —
  and, in the first pass, only HALF fixed. The correction is now complete, and
  the distinction matters enough to record precisely:
  `pkFullyEqualityBound`'s zero-float widening guard sat AFTER its
  stamped-primary-key early return, so a scan with a stamped PK and a terminal
  zero-valued FLOAT equality was proven at-most-one while the cost model
  correctly declined to call it a point probe. The comment above the guard
  claimed it covered `scanProvableMaxCard` and `scanPlanProvableMaxCard`; it
  covered neither, only the ctx fallback path.

  The first pass routed the PROPERTY side through `isProvablePointProbe` (which
  guards correctly) and left the guard where it was — so the property stopped
  proving the false bound while BOTH cost walks kept proving it. That is the
  same defect with a smaller blast radius, not a fix. The guard is now HOISTED
  above the early return so it covers every path out of the helper, which is
  what the comment always claimed. Pinned by
  `TestRFC195_StampedPKZeroFloatIsNotAtMostOne` and by the SCAN half of
  `TestPointProbeProofsAgreeOnWideningEquality` — that test built only INDEX
  shapes before, which is exactly why the disagreement was invisible to it.


- [ ] **CQ-31 (HIGH) — retire `.Field` as an input to any DECISION.**
  Seven separate hand-rolled proofs of a semantic property by leaf-name
  comparison went wrong on this branch (`PushValueThroughFetch` per RFC-179 F12,
  `correlatedInnerField`, `correlatedFieldOf`, `fieldValueAliasAndCol`,
  `buriedLegOrdinalLayout`, `rebaseOuterLegValue`, and the unique-key proof), each
  found by a different route. Measured: **107 non-test `.Field` reads outside the
  values package, 98 of them DECISION-class**, and roughly half live in the SQL
  translator/generator layer (`cascades_translator.go` 30, `cascades_generator.go`
  11, `logical_predicate.go` 8). `FieldValue.Resolved` (the construction-time
  resolved accessor, Java's `ResolvedAccessor`) and `SemanticEqualsUnderAliasMap`
  already exist and are the correct inputs. CockroachDB assigns a column id during
  name resolution and the optimizer never sees a name again;
  `ColumnMeta.Alias` is documented as display-only. **Enforcement is the point** —
  a `pkg/docscheck`-style build check that `.Field` cannot feed a comparison
  outside an allowlisted display site, or an eighth instance is certain.
  (This item previously cited "RFC-193 §5.1". **That document was never
  committed** — no such file exists in the repo or its history, so the citation
  pointed at nothing. The measurements above are the actual specification; they
  were recorded here rather than in the uncommitted draft, which is why they
  survived. RFC number 193 remains unused and is NOT reused, so any stale
  reference elsewhere stays merely broken instead of silently resolving to an
  unrelated document.)


- [ ] **CQ-32 (MED) — two `MemoEqual` callers still bypass the alias gate.**
  `Memo.memoizeNonLeaf` and `Memo.refContains` (`memo.go:398-433`, `511-523`) call
  `expressions.MemoEqual` unconditionally, with no `InternsAliasAware` check — the
  same hazard fixed in `findEquivalentRef` and long-since fixed in
  `Reference.Insert`. NOT fixed because gating them breaks two deliberately-written
  tests (`TestMemoActivation_InternsAliasVariants`,
  `TestMemoActivation_BroadInterningCollapsesK` in `memo_activation_test.go`) that
  require alias-variant `Filter`s to intern into one shared Reference. RFC-077
  flagged this and never audited it. Genuine open question: either those tests
  encode a real requirement that the gate must accommodate, or they predate the
  alias-identity hazard and should change. Needs the audit, not a drive-by.


- [ ] **CQ-33 (MED) — `LIKE 'prefix%'` on an indexed column does a FULL SCAN.**
  MEASURED, pinned by `TestLikePrefix_IsNotSargable_AndTheCoveringStampIsLost`
  (`pkg/relational/core/embedded/like_prefix_not_sargable_test.go`): with
  `CREATE INDEX idx_status ON t2 (status)`, `WHERE status LIKE 'act%'` plans
  `Project([ID#0], PredicatesFilter(Scan(T2), [1 preds]))` while the `=` control
  plans `Project([ID#0], IndexScan(IDX_STATUS, [=] COVERING))` — so the full scan
  is about the comparison type, not the schema. Cause (INSPECTION):
  `ComparisonLike` is admitted by neither `isSargableComparisonForMatch`
  (`match_max_match_map.go:67`) nor `isScanRangeCompatible`
  (`scan_match_helpers.go:37`), and `ResolveStartsWith` (`expr.go:1437`), which
  builds the comparison the scan machinery does accept, has no production caller.

  *Every Java claim in this item is against **Java 4.12.11.0** — the tree at
  `fdb-record-layer/` in the REPO ROOT (gitignored, so it is absent from
  `git ls-files` and from any worktree; the version is pinned in `MODULE.bazel:117`
  as `org.foundationdb:fdb-record-layer-core:4.12.11.0`). That names exactly what
  to check out to re-verify. The two backing `file:line` citations, both
  re-verified at that tag: `PatternForLikeValue.java:111-112` (the escape table's
  only two entries, `<esc>_` and `<esc>%`, layered over `REPLACE_MAP` at `:62-79`
  where `%` → `.*`), and `:116` (the `^…$` wrap) evaluated by
  `LikeOperatorValue.likeOperation` (`LikeOperatorValue.java:93-99`:
  `Pattern.compile(rhs)` with NO flags, then `.find()`).*

  **Tightness — MEASURED; it constrains every possible design.** Java (4.12.11.0)
  compiles `%` to `.*` inside a `^…$` wrap with no DOTALL, so a wildcard cannot
  cross a line terminator: a subject that starts with the literal prefix but
  then carries a terminator lies in the byte-prefix range and does NOT match the
  pattern, so the range is NOT contained in the predicate and **the residual
  LIKE may never be dropped.** That is the only direction the witnesses
  establish; the STRICT-SUPERSET claim additionally needs containment the other
  way — every match lying inside the range — which nothing here proves and which
  `comparisons_test.go:1285-1290` explicitly leaves open. A witness of that
  separation needs an
  INTERNAL terminator, because Java's `$` tolerates exactly one FINAL one
  (`LikeMatch("abc%", "abc\n")` = TRUE) — which also makes a wildcard-free LIKE
  not an equality: its match set is the literal plus the literal-then-one-
  terminator, so an equality range is a strict SUBSET and LOSES rows. That
  tolerance is NOT universal: for `L` ending in `"\r"`, `LikeMatch(L, L+"\n")` is
  FALSE, because the subject then ends in a `"\r\n"` unit and `$` never matches
  between its two runes (`values/like_match_test.go:139`). Pinned by
  `TestLikeMatch_NoPatternYieldsATightPrefixRange`
  (`cascades/predicates/comparisons_test.go`) and
  `TestLikeMatch_ConstantPrefixBoundary` (`cascades/values/like_match_test.go`).

  **ESCAPE — corrected.** Java installs exactly two entries, `<esc>_` and
  `<esc>%`; elsewhere the escape rune falls through to the ORDINARY rules and thus
  to the METACHARACTER rules, so an escape rune that is ITSELF `%` or `_` stays a
  WILDCARD where it opens no entry. "Escape before another escape is fallthrough"
  is therefore FALSE for `"%%" ESCAPE '%'` and `"__" ESCAPE '_'`, where the pair
  IS the entry and denotes ONE literal wildcard. Both hazards are pinned only
  since `TestLikeMatch_CrossCheckSQLPatternToRegex` gained an escape rune of `_`
  and an uppercase subject rune: before that, a matcher declining `<esc>_` when
  `escape == '_'` left the whole values package green. The guard is a NON-EMPTY
  prefix on a constant pattern, NOT the absence of later wildcards.

  **Blockers.** (1) *Empty primary-key range — HISTORICAL, NOT REPRODUCIBLE, a
  LEAD AND NOT A FACT:* a prototyped producer
  returned zero rows for every primary-key prefix LIKE (`expected [apple]
  [apricot], actual 0 rows`), STARTS_WITH bound present and range empty; DELETED
  rather than fixed, loss point NEVER diagnosed. The code that produced this
  observation is not in the tree or its history and there is no committed
  artifact, so nothing here can be re-run and none of it may be relied on as a
  property of the current tree — it is a hypothesis to RE-DERIVE against a new
  prototype, and it must fail on its own terms again before it counts.
  Diagnosing it is step one of any
  retry. (2) *Covering stamp lost through an intervening residual — MEASURED at
  HEAD, pinned by the same test:* `WHERE status > 'act'` gives
  `Project([ID#0], IndexScan(IDX_STATUS, [<>] COVERING))`; adding
  `AND status LIKE '%zz%'` gives
  `Project([ID#0], PredicatesFilter(IndexScan(IDX_STATUS, [<>]), [1 preds]))` —
  two rules stamp covering redundantly and neither descends through a
  `RecordQueryPredicatesFilterPlan` (see CQ-39). Prerequisite only for a SCOPED
  shape, INFERRED and never measured: the secondary index, default `PreferScan`,
  covering-capable projection, where the lost stamp drops the decision onto cost
  criterion #7. Under `PreferIndex` the penalty falls on the primary scan; a
  non-covering projection has no stamp to lose; a query separated on an earlier
  criterion never reaches #7. (3) *Logical-vs-physical type gate — INFERRED,
  UNVERIFIED (a reading of the code, never executed):* the logical LIKE gate (`expr.go:1415-1427`) admits
  Unknown/Null/Enum/Date/Timestamp alongside String while the physical layer
  accepts STRING; whether a disagreement demotes to residual or errors at
  execution was never measured. `ResolveStartsWith` also has no LHS type gate
  where `ResolveLikeWithEscape` raises 42804, so giving it a caller means adding
  that gate first. (4) *The prototype's other failures — HISTORICAL, NOT
  REPRODUCIBLE, a LEAD AND NOT A FACT (same deleted, uncommitted prototype as
  (1)):* it also
  broke `TestRuleTypes_EveryProductionRuleHasDirectBehavioralTest`,
  `TestPartitionSelect_ChainInterningBaseline` and `TestJavaCorpusRuns/files`,
  none diagnosed before deletion. All three pass at this head; the entry says
  only that a producer of that shape once broke them, which is a place to LOOK
  on a retry, not a defect anyone has established in the current tree.

  **`yamsql/testdata/like_prefix_pushdown.yaml` cannot detect this optimization,
  and contradicts itself — OUTSTANDING, on master, untouched by this branch.** 41
  `- query:` steps, `grep -cE 'plan_contains|plan_not_contains'` = 0: no step
  asserts plan shape, and a correct pushdown returns the same rows by
  construction, so nothing separates its presence from its absence. (Rows-only
  assertions DO catch a WRONG optimization — they are reported to have caught the
  deleted prototype on its first run, which is the same NOT-REPRODUCIBLE lead as
  blocker (1) and not a re-runnable demonstration — so the gap is dimensional,
  not total.) It also states two
  incompatible contracts: the header at `:11`/`:13` says ESCAPE and interior
  wildcards BAIL, while `:297-325` say they NARROW at the first unescaped
  wildcard with the post-filter enforcing full semantics.

  **No producer design is recorded here, deliberately** — two attempts to state a
  contract both proved satisfiable by a rule that never fires. RFC-216 carried
  this material and was DELETED: as a document claiming to BE the design record it
  could not be kept true, because its prototype results have no committed artifact
  and nothing in the repo could be diffed against them. That standard does not
  transfer to this file. A TODO entry is a work list, and "a prototype produced
  zero rows and the code is gone" is exactly the lead the next attempt needs — so
  the material is kept here, explicitly labelled NOT REPRODUCIBLE (blockers (1)
  and (4)). Its Java citations are likewise kept: Java is this port's spec, the
  tree is `fdb-record-layer/` at tag 4.12.11.0 pinned in `MODULE.bazel`, and
  citing it is the repo's established practice (`DIVERGENCES.md` rests entirely
  on such citations). Gitignored is not uncheckable when the pin says what to
  check out.


- [ ] **CQ-34 (MED) — the sargable gate and the range builder are kept in manual
  lockstep.** `isSargableComparisonForMatch` (`match_max_match_map.go:67`) decides
  what may be consumed into a scan range; `bindScanComparisonsToRangeSet`
  (`scan_range_binding.go:256`) decides what actually becomes a bound. (This
  entry originally named `scanComparisonsToTupleRange`, `executor.go:693`. That
  function was a DEAD TWIN with zero production callers and has been deleted —
  see RFC-217. The lockstep concern below is unaffected and remains open; only
  the citation moved. The line numbers quoted further down refer to the deleted
  function and should be re-derived against the live binder before use.) They are two
  functions in two files with no compiler-enforced tie, which is exactly the F11
  shape (a comparison accepted as sargable whose bound is then never applied —
  wrong rows, with the residual filter already removed). The inequality switch now
  has a loud default that errors on an unrecognised comparison, but **the
  equality-prefix loop (`executor.go:699-741`) has no equivalent** — it silently
  assumes anything not `IS NULL` is a plain `=`. CockroachDB makes this class
  structurally impossible: `idxconstraint` returns a `tight bool` from the same
  call that builds the span, and `RemainingFilters()` may only drop a predicate
  when that call said `tight` (`index_constraints.go:1288-1308`), with the
  fallthrough default being `unconstrained, tight=false`. Port the shape of that,
  or at minimum add the loud default to the equality-prefix loop.


- [ ] **CQ-35 (LOW) — `ComparisonType.IsEquality()` is authoritative-looking dead
  code.** `comparisons.go:106`, doc-commented as "useful for index-pushdown
  decisions", has **zero production callers** — only its own unit test. The real
  equality-range decision is a separate hardcoded check in `ComparisonRange.Merge`
  (`comparison_range.go:146`), and the two already disagree: `IsEquality()` also
  classifies `IN`, `IS NOT DISTINCT FROM`, `TEXT_CONTAINS_*` and
  `DISTANCE_RANK_EQUALS` as equality; `Merge` does not. Harmless today because
  nothing consults it, and precisely the "looks authoritative, isn't" trap that
  produces a real bug the day someone wires it up. Either delete it or make it the
  single source `Merge` consults.


- [ ] **CQ-39 (MED) — a residual filter over a fetch-free index scan is never
  marked COVERING.** *(Renumbered from a duplicate CQ-38. Two distinct items
  carried that number — the NaN-total-order semantics item earlier in this phase
  and this covering-label item; the later one was renumbered. Any external
  reference to "CQ-38 covering" means this item.)*
  **TWO rules stamp covering for this shape, and BOTH miss the same way — a fix
  has two sites, not one.** `MergeProjectionAndFetchRule` marks the inner scan
  covering only in its direct `fetchInnerExpr.(*RecordQueryIndexPlan)` arm
  (`rule_merge_projection_and_fetch.go:91`); with a residual `PredicatesFilter`
  between projection and fetch it takes the `:103-126` fallback, which yields
  the projection over the fetch's inner group and leaves the scan unmarked.
  `ImplementProjectionRule` stamps the same shape redundantly and independently,
  via `findIndexScanPlan` (`rule_implement_projection.go:73`), and fails on the
  identical structural condition: neither descends through a
  `RecordQueryPredicatesFilterPlan`. Both are PLANNING-phase rules (the first an
  implementation rule, the second an expression rule from
  `BatchAExpressionRules`). Measured and pinned by
  `TestLikePrefix_IsNotSargable_AndTheCoveringStampIsLost`'s
  `two_rules_stamp_covering_redundantly` subtest: on the no-residual covering
  control, disabling either rule alone leaves the stamp and disabling both drops
  it. Rows are CORRECT (pinned: `covering_index_pushdown.yaml#25`, 26/26 on real
  FDB, with `plan_not_contains: Fetch` as the sharp pin) — this is a
  labeling/costing gap, not wrong results: the plan renders and is costed
  without the covering marker it earned. CQ-33's covering-stamp blocker is the
  writeup; why it becomes load-bearing for CQ-33 on secondary indexes is the
  INFERRED criterion-#7 chain recorded under CQ-33 above. Found during
  the RFC-197 step-0 review fold; deferred from that fold because marking the
  scan moves plan shapes corpus-wide and is a query-engine change needing its own
  RFC-gated lap. Read Java's MergeProjectionAndFetchRule counterpart first — if
  Java marks covering through a residual, this is a divergence; if not, it is a
  shared gap and the fix is an extension. (INSPECTION, not re-checkable from this
  tree — the Java checkout is a gitignored sibling absent from `git ls-files`:
  Java 4.12.11.0 appears to have no such failure mode, because coveringness is a
  separate class there, `RecordQueryCoveringIndexPlan`, which HOLDS the index plan
  as a field rather than flagging it, and its `MergeProjectionAndFetchRule` yields
  the fetch plan's child with no shape check. Re-derive against the checkout
  before relying on it.)


- [ ] **Two producers mint primary-key comparison Values from the same
  `GetPrimaryKeyColumns` source with DIFFERENT case handling, and lazy
  `FieldValue` equality is case-SENSITIVE** · S · found while refuting the
  RFC-197 `uniqueUpperFieldIndex` name-keyed booking
  `commonPrimaryKeyValues` (`intersector_primary_key.go:1197-1203`) mints
  `&values.FieldValue{Field: strings.ToUpper(col)}`. `PrimaryScanRule.OnMatch`
  (`rule_primary_scan.go:50-55`) mints `&values.FieldValue{Field: col}` from the
  SAME `PlanContext.GetPrimaryKeyColumns` source with no `ToUpper`. The source
  does not normalise either: `coveredPrimaryKeyColumns`
  (`cascades_generator.go:3437-3444`) appends `component.Field.GetFieldName()`
  verbatim off the key expression.
  This matters because lazy (unresolved) `FieldValue` equality is
  case-SENSITIVE — `EqualsWithoutChildren` returns `av.Field == bv.Field`
  (`map_field_values.go:354`), not an `EqualFold`. So wherever a metadata PK
  field name is not already upper-case, the two producers mint UNEQUAL Values
  for one and the same column: memo members fail to intern, and a structural
  comparison between a scan-minted primary key and an intersection-minted one
  is false for a column they both name.
  NOT MEASURED, and it is the first thing to establish: whether any live
  metadata path actually yields a non-upper-case PK field name. If none does,
  this closes by making the two producers agree and PINNING that invariant
  rather than by fixing an observable bug — which is still worth doing, because
  the asymmetry is one DDL path away from being live and nothing currently
  detects it.
  DONE = one shared mint helper behind both producers (or an asserted
  normalisation at the `GetPrimaryKeyColumns` boundary, which is the better
  shape if the boundary can state the invariant), plus a unit pin that drives a
  mixed-case PK column through both producers and reds if either side's case
  handling diverges again.


- [ ] **RFC-218 adjacent — Go appends a hidden sort column where Java derives
  instead (plan-shape only).** · S
  Java's `Expression.canBeDerivedFrom` (`Expression.java:254-264`) decides
  hidden-column membership via `Value.pullUp`, so `n.sk` IS derivable from a
  projected `n` and Java appends NO hidden column. Go's `sortKeyInOutput`
  (`cascades_translator.go`) uses exact `SemanticEqualsUnderAliasMap`, so
  `{N}{SK} != {N}` and a column IS appended, named `"N"` — putting TWO fields named
  `N` in the folded row, which is the duplicate-root layout the fold is documented
  as able to manufacture from unambiguous SQL.
  ROWS ARE CORRECT either way; this is shape, not a bug, and is pinned by
  `struct_root_projected_still_orders_correctly_despite_the_extra_column`.
  DONE = `sortKeyInOutput` matches Java's derivability, the extra column stops
  being appended, and the pin is updated to assert the shape rather than only the
  answer.


- [ ] **CQ-46 (HIGH, STRUCTURAL) — index candidacy is opt-OUT in Go and opt-IN in
  Java; port the opt-in shape.** · M/L · query-engine gated (RFC + Graefe/Torvalds
  review BEFORE implementation)
  This is the actual defect behind CQ-45, and the reason that fault could exist at
  all. JAVA: `IndexMaintainerFactory.createMatchCandidates` defaults to EMPTY —
  an index type contributes match candidates only if its maintainer factory
  explicitly emits them, and `AtomicMutationIndexMaintainerFactory` emits ONLY
  aggregate candidates. Java therefore CANNOT EXPRESS the CQ-45 bug: an aggregate
  index is never in the value-index candidate set to be matched by name in the
  first place. GO: candidacy is opt-out — every index becomes a plain value-index
  candidate unless some downstream site happens to filter it.
  MEASURED FRAGILITY: of the three raw-index shortcut sites, only
  `rule_streaming_agg_from_index.go:61` excludes aggregates DELIBERATELY. The
  other two exclusions are incidental — adding an innocuous
  `CreatesDuplicates() bool` method to `AggregateIndexMatchCandidate` re-arms the
  IDENTICAL fault on `ORDER BY` shapes. A correctness property that an unrelated
  method addition can switch off is not a property, it is a coincidence.
  ADJACENT OPT-OUT LEAKS to audit in the same pass (reachability UNMEASURED —
  measure, do not assume): `text`, `multidimensional`,
  `time_window_leaderboard`, and the legacy bare `min_ever` / `max_ever` type
  strings all become plain value-index candidates in Go where Java emits none.
  DONE = candidacy inverted to opt-in per maintainer factory, matching Java's
  structure (CLAUDE.md design principle 10: match the architectural property that
  produces the behaviour, not a downstream observable — an `if isAggregate {skip}`
  at each shortcut site is exactly the bolted-on check that rule forbids); every
  adjacent leak above measured for reachability and either closed or pinned as
  unreachable with a test naming what re-arms it; and the `CreatesDuplicates`
  probe kept as a regression, since it is the measurement that proved the current
  gates accidental.


- [ ] **CQ-49 (MED, plan improvement, measured) — `pullUpThroughRecordConstructor`
  always-bake removes a redundant InMemorySort.** Removing its bake gate
  eliminates the in-memory sort on exactly 2 plan-corpus queries
  (`SELECT a.*, b.* FROM a, b WHERE a.a1=b.b1 ORDER BY a.a1`, both variants) —
  verified correct: `a1` is table `a`'s PRIMARY KEY, so `Scan(A)` already
  provides `A1 ASC` and the FlatMap preserves it. Deliberately NOT landed in
  the RFC-197 name-keyed bucket: it MOVES PLAN SHAPES, which is what makes it a
  query-engine change and so requires its own Graefe-gated lap. (It also churns
  ~20 unit fixtures, but fixture churn is a size, not a gate — a shape-neutral
  change touching twice as many fixtures would still ride along.) The
  measurement is in hand; nothing else blocks it.


- [ ] **CQ-51 (HIGH, STRUCTURAL, query-engine-gated, MEASURED) — constraint
  GROWTH and re-exploration are the same event, so widening any constraint's
  key widens the fixpoint superlinearly.** `planner_constraint.go`'s `Set`
  returns a changed-flag from the per-key lattice combine
  (`combineForKey`, planner_constraint.go:80-95) and every change pushes the
  property and re-fires the push rules. Nothing distinguishes "this constraint
  now carries MORE information" from "downstream must be re-explored", so a
  constraint that legitimately holds more elements costs re-exploration
  proportional to its growth.

  This is not hypothetical and it is not a micro-optimization — it is the thing
  BLOCKING an RFC-197 conversion that is otherwise correct. Keying
  `ReferencedFields` by VALUE instead of by leaf name is the unambiguous port
  (Java's member is a `Set<FieldValue>` keyed by semanticEquals/semanticHashCode,
  `ReferencedFieldsConstraint.java:41`), and it was BUILT and MEASURED and then
  REVERTED, because the value-keyed set grows wherever two quantifiers share a
  leaf name:

  - 4-table chain: `tasksRun` 10255 → 12901
  - 3-spoke ordinal star: 9481 → 12644
  - hub+5 star: STOPS PLANNING — `ErrPlannerCapHit`, a rule-cycle round-cap
    divergence at 87642 tasks

  (Both budget baselines are ±2% pins; `plan_shape.golden` does not move, so
  this is purely a planning-effort regression, not a plan-quality one.)

  THOSE ARE HISTORICAL DELTAS, TAKEN AGAINST A BASELINE THAT HAS SINCE MOVED —
  do not compare them to a measurement of the current tree. The 3-spoke star's
  sentinel was 9481 when this was measured and is 13226 today (RFC-232's exact
  resolution; the whole chain is in `ordinal_star_planning_budget_test.go`'s
  `wantTasks` comment). What survives the move is the SHAPE of the finding — a
  value-keyed set grows wherever two quantifiers share a leaf name, and the
  hub+5 arm stops planning outright — not the absolute numbers. Re-measure
  against the sentinel of the day before quoting a percentage.

  So the conversion is correct and cannot land until the coupling changes. Fix
  the coupling FIRST, then land the value-keyed referenced-fields set and drop
  the `referenced_fields.go:125` entry from `pkg/docscheck`'s
  `field_name_decision_test.go` allowlist — that allowlist reason string is
  where this finding was living, which is precisely why it needed to become an
  item: prose inside an allowlist is unreachable under the pick-lowest-unchecked
  rule.

  Read Java first: whether `PlannerConstraint.combine`'s changed-flag drives
  re-exploration the same way in `PlanContext`/`ConstraintsMap`, and if Java
  separates "constraint widened" from "re-push required", the separation is the
  port. Planner machinery, so it takes a Graefe-gated RFC + lap of its own.

  **JAVA READ, AT `041838856`. The answer is the branch this item treated as
  unlikely: Java DOES separate them, and Go already contains half the port as
  DEAD CODE.** The item's measurements and its description of the coupling are
  all still true; its implied conclusion — that the decoupling is machinery to be
  designed — is refuted. Java's mechanism is three pieces:
  - `CascadesRule.java:66-77` — every rule carries
    `Set<PlannerConstraint<?>> requirementDependencies`, **empty by default**;
    exactly **24** declaration sites declare one, all through the 2-arg
    constructor (`getConstraintDependencies` is never overridden anywhere in the
    tree): 13 `PushRequestedOrdering*`, 4 `PushReferencedFields*`, 6 `Implement*`
    (`Unique`, `InUnion`, `StreamingAggregation`, `InJoin`, `NestedLoopJoin`,
    `DistinctUnion`), and `AbstractDataAccessRule:122` — the only two-constraint
    set, inherited by its two concrete subclasses for 25 concrete rule classes.
  - `CascadesPlanner.java:891-908` — `ReExploreExpression.shouldPushRule` returns
    `group.isFullyExploring() || !group.isExploredForAttributes(rule.getConstraintDependencies())`,
    against `ExploreExpression.shouldPushRule`'s unconditional `true` (`:929-932`).
  - `ConstraintsMap.java:246-261` — `isExploredForAttributes` compares each
    interesting key's `lastUpdatedTick` against `watermarkCommittedTick`.
  Routing at `CascadesPlanner.java:528-538` is on a `forceExploration` flag:
  `true` → `ExploreExpression`, `false` → `ReExploreExpression`. A fresh yield
  passes `true` (`:1073`, over `ruleCall.getNewExploratoryExpressions()`). A
  constraint push does NOT push a task variant directly — `:1088-1090` pushes
  `ExploreGroup` for each reference whose requirements moved, and only for ones
  already explored (`!reference.hasNeverBeenExplored()`); it is that group task's
  member loop that re-enters with `forceExploration=false`, which is where
  `ReExploreExpression` comes from. Net effect: a constraint-driven round
  re-fires only the rules that DECLARED a dependency on the constraint that moved
  — an empty dependency set means not pushed at all after first exploration.

  **Go's state, measured:** `expressions/constraints_map.go:114`
  `IsExploredForAttributes` is already a faithful port of
  `ConstraintsMap.java:246` — and its ONLY callers are
  `expressions/constraints_map_test.go:49,58`. It is dead in production.
  `grep -rn ConstraintDependencies pkg/` returns ZERO hits, and
  `unified_tasks.go:230-286` pushes every matching rule on every round with no
  constraint filter. So the port is: (1) add a constraint-dependency declaration
  to the rule interfaces, empty by default, declared where Java declares it;
  (2) split the explore task into forced and re-explore variants, the latter gated
  on the already-ported `IsExploredForAttributes`; (3) route yields to forced and
  `ExploreGroupTask`'s member loops to re-explore.

  Also correct in passing: `Set` is now `planner_constraint.go:76-94` and
  `combineForKey` `:107-124` (the booked `:80-95` names neither), and the cap that
  fired is `ErrPlannerRoundCapHit` (`unified_tasks.go:106-109`, `maxRoundsPerRef
  = 100`), not `ErrPlannerCapHit`. The reverted experiment was never committed —
  the measurements survive only in `c2ad0f445`'s message and the debt entry's
  reason string, which is the argument for landing the port with the budget pins
  re-measured rather than re-deriving the numbers from memory.

  **STILL a Graefe-gated RFC + lap** — it moves two ±2% budget pins
  (`partition_select_interning_baseline_test.go:291`,
  `ordinal_star_planning_budget_test.go:112`) and the round cap. But the RFC
  RECORDS a port and is graded against `CascadesPlanner.java:891-908` /
  `CascadesRule.java:66-77` / `ConstraintsMap.java:246-261`; it does not design a
  mechanism.


- [ ] **CQ-54 (MED/M, M, query-engine review gate) — one AVG in the query, two
  in the aggregate: extend `logicalAggregateCalls`' dedup past `COUNT(*)`, and
  make the call-numbering ONE authority instead of two.** RFC-197 item 5 left
  this as prose; it is a real defect and it is booked here.

  `SELECT AVG(n) AS a FROM t1 HAVING AVG(n) > 10` harvests the SELECT's `AVG(n)`
  and the HAVING's `AVG(n)` as TWO value-identical `logical.AggregateCall`s, so
  the aggregate computes AVG twice and its output row is one column wider than
  the query needs. `logicalAggregateCalls`
  (`pkg/relational/core/embedded/logical_builder.go:85-87`) suppresses a
  harvested duplicate `COUNT(*)` and nothing else:

      if countStar && call.Func == "COUNT" && call.Star && !call.Distinct {
          continue
      }

  Why it is not a one-line widening of that condition. The aggregate's runtime
  ABI is `[keys..., calls...]`, and the two functions that decide `calls...`
  take DIFFERENT inputs and count independently: `logicalAggregateCalls(cls.aggCols,
  cls.countStar, strip)` versus `buildAggregateOutputSlots(keys, outputAggCols,
  strip)` (`plan_visitor.go:1360` and `:1370`; the same pairing at
  `logical_builder.go:588`/`:593`). `buildAggregateOutputSlots` assigns
  `callOrdinal[i] = len(keys) + nextCall` over ITS list
  (`logical_builder.go:130-136`) with no knowledge of the suppression the other
  side applied. Dedup on one side and every public output slot after the removed
  call points one column too far right — silent wrong values, not an error. The
  `COUNT(*)` case survives today only because the suppressed duplicate is
  harvested (hidden) and the visible list it is numbered against never contained
  it. Extending the dedup to arbitrary calls breaks that coincidence.

  So the fix is the numbering, and the dedup follows: one authority produces the
  call list AND the ordinal each output slot points at, from one input, in one
  pass. That is the same "the slot is recorded at the composition that decides
  it" move RFC-197 item 5 made for post-aggregate group-key references
  (`logical_predicate.go:6228-6254`), applied to the aggregate calls.

  Evidence that the duplicate is observable in the plan, and the divergence it
  already exposed: one corpus query moved nine golden lines when item 5 landed —
  `SELECT a FROM (SELECT AVG(n) AS a FROM t1 HAVING AVG(n) > 10) AS sub` gained
  a Projection operator. Two value-identical calls are the ONLY thing the old
  name-keyed binder and the recorded-slot binder ever disagreed about: the
  recorded slot picks the FIRST call (matching what
  `bindPostAggregateValueToNativeOrdinals` already does for SELECT and ORDER
  BY), the retired name map's last-wins picked the SECOND. Rows and column
  labels are identical either way, which is exactly why it survived — a
  first-wins/last-wins disagreement is invisible while the two calls compute the
  same number. Dedup the calls and the disagreement has nothing to disagree
  about.

  Size, honestly: MED impact, M effort. Not a rewrite — two functions and their
  two call sites, plus the golden churn. The risk is entirely in the ordinal
  re-numbering, so it wants a pin per shape: duplicate harvested call (AVG),
  duplicate visible call, `COUNT(*)` (the existing suppression must keep
  working), and a DISTINCT/non-DISTINCT pair that must NOT dedup. Pin the output
  WIDTH, not just the rows — rows are identical under the bug, which is how it
  shipped.


- [~] **CQ-55 (HIGH/L, L, query-engine review gate) — `properties.RichOrdering`
  matches its ordering set by rendered string; make it match on column
  identity.** · **SUPERSEDED — DO NOT IMPLEMENT AS WRITTEN. Go to "CQ-55
  AMENDMENT 2" below, which is the live spec.**
  **Box deliberately `[~]`, not `[ ]`, as of 2026-08-01.** One CQ number carried
  TWO unchecked boxes, and this — the earlier, REFUTED one — sorts first. The
  execution rule is "pick the lowest-numbered unchecked item", so a worker
  following the rule would build the design the amendment measured as wrong:
  amendment 2 finds this entry's central ruling (pure `ColumnIdentity` keying)
  "over-narrowed that key space to columns and cannot hold the domain", which
  makes the bullet below about `PartitionedOrderedSet[string]` becoming
  `PartiallyOrderedSet[ColumnIdentity]` a refuted instruction, not a plan.
  Kept in place as the record of what was tried and why it failed.
  This is the STOP that RFC-197 item 5 hit and could not pass, and
  it is the last name-keyed identity decision in the ordering property.

  `properties.RichOrdering` addresses its ordering set by `values.ExplainValue`
  (`rich_ordering.go`, `orderingKeyFor`). So an aggregate's PROVIDED group-key
  ordering and an ORDER BY's REQUESTED key — two independently constructed
  FieldValues — meet only as a RENDERED STRING. They agree today solely because
  `HintOrdering` and `canonicalizeAggregateOutputValue` (the requested-side
  authority) both spell the key through the one rendering authority. That is an
  agreement by convention between two producers, not an identity.

  Escalation rationale, which is why this is HIGH and not a cleanup: a false
  SATISFACTION here is not a lost optimization. It feeds the ordering-dependent
  operators — a merge join, a streaming aggregate, a distinct that assumes
  adjacency — which then read rows they were promised were ordered and are not.
  Wrong rows, memo-wide, from a string collision.

  The reviewed shape, to build as stated:

  - `PartiallyOrderedSet[string]` becomes `PartiallyOrderedSet[ColumnIdentity]`,
    with `SameColumnPath` as the equality.
  - Domains are MINTED at the `HintOrdering` providers and at composition — the
    two places where the layout is in hand.
  - `orderingKeyFor`'s three bridge arms are DELETED. They exist to reconcile
    representations that a real identity makes indistinguishable; keeping them
    alongside the identity would preserve the string channel under a new name.
  - Lazy providers FAIL CLOSED: a provider that cannot mint a domain reports no
    ordering rather than an unverifiable one. An unordered plan is slow; a
    falsely-ordered one is wrong.

  What holds it today, so the conversion has a detector: `plans/ordering.go:598`
  is NOT display-only and the flag that it might be is refuted — probing the name
  at that site reds the FDB driver suite with `want exactly 1 InMemorySort
  (group-key sort, reused by ORDER BY), got 2` and moves the corpus plan-shape
  golden. `sqldriver/aggregate_group_key_ordering_contract_fdb_test.go` is the
  two-direction detector (provided side and requested side, mutated separately,
  on rows AND on `InMemorySort` counts, including the two-same-leaf-group-keys
  shape). `plans/streaming_aggregation_ordering_key_name_test.go` pins the
  provided side only — it CANNOT see a requested-side divergence, because it
  builds its requested value by calling `GroupByOutputColumnNames` itself. Do
  not mistake it for coverage of both.

  Cascades ordering machinery, so it takes a Graefe-gated RFC and its own review
  lap. It also unblocks `DisplayLabel`: the reason that carrier type cannot
  compile is that this consumer renders the label INTO a match key, and no
  render exit placed in `embedded` can reach a site that lives in `plans`.


- [ ] **CQ-56 (MED/S, S) — `NewFieldValueWithPinnedOrdinal` mints its FieldPath
  with `Domain: unknown`, and the domain is derivable at both call sites.** The
  ordinal domain is RFC-197 step 0's third element of identity, and the
  constructor that pins an ordinal is the one place it must not be blank.

  Measured, from the HAVING reference of `SELECT o.k, i.k, COUNT(*) ... GROUP BY
  o.k, i.k HAVING i.k > 15`:

      Accessors:[{Field:I.K Ordinal:1}] Domain:domain(unknown) FrontierPinned:false

  The ordinal is CORRECT — 1 is `i.k`'s slot, and it tracks the GROUP BY order
  (0 when the keys are listed the other way round). The composition knows
  exactly which layout it numbered against, and then throws that fact away.

  An ordinal with an unknown domain is an ordinal no consumer may trust, which
  is precisely the hazard `logical_predicate.go:6193-6206` documents from
  production: a group key's SOURCE-relative ordinal and an aggregate's
  OUTPUT-row ordinal met in one comparison and matched because the integers
  coincided, rewriting the `SUM(v)` of `HAVING g > SUM(v)` into a reference to
  `G`, after which the predicate looked key-only and was pushed onto the raw
  scan. `FieldPath.Domain` exists to fail closed on exactly that comparison; the
  `FrontierPinned` provenance bit is standing in for it because neither side
  carries a domain yet. Every such stand-in is one more consumer that will have
  to be revisited when the domain arrives.

  Derivable now, no new machinery: `OrdinalDomainOfColumnNames` at both
  composition sites. Small and self-contained — but it is a prerequisite for
  anything that wants to make an identity decision on a recorded ordinal, which
  is why the ambiguous-grouping-key refusal in
  `rule_push_filter_through_groupby.go` declines by NAME AMBIGUITY rather than
  resolving the reference by its recorded slot. With the domain minted, that
  refusal can become a decision.


- [ ] **CQ-57 (MED/S, S) — `rule_decorrelate_values`'s two Select rebuilds drop
  `quantifiersSwapped`, and whether that is CORRECT is a semantics question
  nothing has answered or pinned.** Both rebuilds go through
  `NewSelectExpressionWithJoinType` (`rule_decorrelate_values.go:222` and
  `:300`), and that constructor never sets `quantifiersSwapped` — it defaults to
  false. So a swapped Select that reaches either site comes back unswapped.

  This is NOT the field-literal-copy class, and must not be "fixed" by carrying
  the field across. Both sites MUTATE the quantifier set — `:222` removes the
  inlined values boxes, `:300` prepends the pushed-down ones — and
  `quantifiersSwapped` is a claim about the first TWO quantifiers specifically.
  A mechanical carry would assert a swap that may no longer describe the list.

  Measured over the whole `cascades` package suite (stderr counters at both
  sites, `go test -count=1 -v`):

      site=222   18 swapped=false   17 swapped=true
      site=300   10 swapped=false    2 non-Select expr (no marker to read)

  and, for the 17 swapped=true hits at `:222`, whether the removal destroys the
  swapped pair:

      14x  removed[0]=false removed[1]=true   nq 2 -> 1
       1x  removed[0]=true  removed[1]=true   nq 3 -> 1
       1x  removed[0]=true  removed[1]=true   nq 2 -> 1
       1x  removed[0]=false removed[1]=true   nq 8 -> 4

  In ALL 17, quantifier 1 is removed — the swap's second member — so the pair
  the marker describes no longer exists, and in 16 of 17 a single quantifier
  remains, where the marker is vacuous by construction
  (`WithSwappedQuantifiers` itself no-ops below 2 quantifiers). On this
  evidence dropping the marker is DEFENSIBLE at `:222` today.

  What is missing is that nothing says so and nothing enforces it. The drop is
  a side effect of which constructor was reached, not a decision: if a future
  rewrite at either site preserves quantifiers 0 and 1, the same line silently
  becomes a fail-OPEN, because both readers of the marker are safety DECLINES
  (`RemoveRangeOneRule` refuses a swapped Select; the nested-loop-join rule
  gates its correlated-scan fast path on it). Losing it admits exactly the
  shapes those gates exist to refuse — the same failure mode as the
  `SelectExpression.WithQuantifiers` literal, reached by a different route.

  The work: decide the rule ("a rebuild that removes or reorders either of the
  first two quantifiers invalidates the marker; one that preserves both carries
  it"), state it at both call sites, and pin it with a test that constructs a
  swapped Select whose values boxes sit PAST position 1 — the shape the measured
  corpus never produced, and the one where carrying vs dropping diverges.

  NOTE ON PROVENANCE: the fold report that motivated this item cited "10
  measured hits, all swapped=false". That is site `:300` exactly, and it is
  complete for that site. Site `:222` was not in that measurement and is where
  the marker is actually live — 17 of 35 hits carry swapped=true. The item was
  booked as a pure semantics question; it is that, but on a live marker rather
  than a dormant one.


- [ ] **CQ-58 (MED, latent class) — `Value.WithChildren` constructor-call rebuilds.**
  14 methods rebuild via constructor call (invisible to the composite-literal
  gate); constructor params cover all struct fields TODAY, and adding a param
  is a compile error everywhere, so this is latent and structurally weaker
  than the field-literal class — but several sites do DELIBERATE work a
  mechanical conversion would defeat (RankValue.WithChildren resets
  ArgumentValues by design), so each needs per-site semantic judgment like
  CQ-57, not a sweep. Enumerated during the field-literal class closure.


- [ ] **CQ-55 AMENDMENT 2 (supersedes the entry above and the re-scope in the
  C2 commit message) — THE ORDERING KEY IS A VALUE, NOT A COLUMN.** All
  numbers measured over the 2481-query corpus (harness:
  ordering_identity_decisions_test.go). Java's Ordering is
  PartiallyOrderedSet<Value> keyed by Value.equals (Ordering.java:176-183,
  :336) — a VALUE identity. The pure-ColumnIdentity-triple ruling
  over-narrowed that key space to columns and cannot hold the domain: 126
  corpus ordering keys are *RecordTypeValue discriminators (primary-key
  intersection comparison keys, intersector_primary_key.go:615/:807) with
  no ColumnIdentity by design, and 61 of 61 merge-path string decisions run
  on identity-unavailable pairs, so fail-closed triple keying collapses
  every set-operation merged ordering to empty. CORRECTED C1: the
  ordering-set key is the VALUE, compared structurally — ColumnIdentity as
  the FieldValue arm (answers 94.8% of provided keys), Value-structural
  equality as the other arm (what RecordTypeValue needs; NOT a name
  comparison). The name bridge still dies; truncation stays rejected (the
  merge key never truncates). CORRECTED C5: the dominant unaddressable
  producer is computeWrapperRichOrdering (plan_properties.go:367, 1218
  keys) — the pull-up site itself; the translator-branch precondition
  measured SQL-unreachable (0/2481; OrderingIdentityOf already answers
  92.4% of exact-arm keys) and is dropped; the translation locus stays the
  quantifier boundary on pull-up, never the comparator.
  orderingValuesEqual (4189/4189 via the name bridge; candidate side
  name-based by construction, accessor_name_path.go:30-33) converts at its
  PRODUCER (match-candidate ordinalization), not in place. Merge-path
  thirteenth-bug probe: identityDIFF=0 — ruled out, with the census
  showing the string key is the only key space available there today.

  AMENDMENT 2 CONDITIONS (review-ACKed, binding on implementation): dispatch
  by VALUE TYPE, never by identity availability — availability dispatch is
  intransitive inside the FieldValue class (witness: baked path [0] in
  domain D1, in D2, and with UNKNOWN domain; identity separates the first
  two, structural equates the third with both → insertion-order-dependent
  ordering sets → nondeterministic plans, arriving as the
  ordinal-across-layouts conflation column_identity.go:212-216 refuses).
  The FieldValue arm returns identity-or-DECLINE and never falls through to
  ValuesStructurallyEqual; pin the D1/D2/UNKNOWN triple as a transitivity
  net. The lazy 36 are resolved at their producer (the same move as
  orderingValuesEqual's), not compared structurally; decline is the
  residual only and must MEASURE ZERO on the corpus before implementing —
  if it does not, stop and come back. The 0/2481 SQL-unreachable negative
  for the dropped translator precondition gets committed as a test naming
  what re-arms it.

  AMENDMENT 2, DECISION 3 (intersection-key domain): a primary-key
  intersection's comparison-key ordinals are domained in the COMMON RECORD
  TYPE's row layout, never the per-index layout. The intersection's premise
  is exactly one common record type; the keys exist for cross-leg
  comparison, so per-index domains hand the legs different tokens and
  collapse the merge (the failure amendment 2 predicted for naive triple
  keying); and record-level domaining sits consistently beside the
  RecordTypeValue discriminators in the same key list, which are also
  record-level. The machinery exists: the candidate's rowLayouts()
  (RFC-197 item 1). The lazy mints at match_candidate_index.go:411/:447
  bake against that layout. Clarification for the unreachability negative:
  "the translator branch" is applySortOverRef's positional bake
  (k.Value == nil && k.Pos > 0 arm), measured 0/2481 because
  upgradeSortKeyValues resolves positional keys upstream — pin THAT.


  **SEQUENCING — RFC-215 LANDS FIRST, THEN THE CENSUS IS RE-RUN, THEN THIS.**
  Evidence below verified on `origin/master` @ `a0958983a`; every count is
  scoped to the command that produced it.

  THE NUMBERS ABOVE ARE STALE, not merely imprecise. They were measured over a
  tree in which every in-memory-sort ordering key is a `*FieldValue` BY
  CONSTRUCTION, and RFC-215 removes that construction. The chain is unbroken
  and still unconverted on master:

    - `rule_implement_in_memory_sort.go:126-130` — a sort key that is not a
      plain childless FieldValue gets `field = values.ExplainValue(sk.Value)`;
      `:135` then writes BOTH into one struct,
      `plans.SortKey{Field: field, …, ValueExpr: sk.Value}`.
    - `ordering.go:796` — `keys[i] = &values.FieldValue{Field: sk.Field, …}`
      keeps the rendering and drops `ValueExpr`. #653 merged RFC-215's RFC and
      its instruments, NOT the fix; this line is unchanged on master.
    - `plan_properties.go:326` → `:367` passes `o.Keys` unchanged into
      `properties.NewRichOrdering`.
    - `properties/rich_ordering.go` keys the ordering SET by
      `values.ExplainValue(v)` — 11 occurrences
      (`grep -c ExplainValue pkg/recordlayer/query/plan/cascades/properties/rich_ordering.go`),
      the set contract stated at `:14` and `orderingKeyFor` opening with it at
      `:365`.

  So a key minted at `:796` is a `*FieldValue` whose `Field` is ALREADY a
  rendering, and the ordering set renders it AGAIN. `plan_properties.go:367` is
  simultaneously RFC-215's downstream consumer and the site this item names as
  its dominant unaddressable producer (1218 keys): one defect, two
  instrumentation points, one pipeline.

  THE GATE IS UNSATISFIABLE UNTIL RFC-215 LANDS. `values.OrderingFieldPair`
  (`column_identity.go:221-225`) is a pure Go type test — `a.(*FieldValue)` on
  both sides and nothing else. A `:796` key for an arithmetic sort expression
  IS a `*FieldValue` by type while carrying `(q$3728.K#1 + q$3728.K#5)`, so
  under this item's binding condition — dispatch by VALUE TYPE, the FieldValue
  arm returning identity-or-DECLINE and never falling through to structural —
  it DECLINES, though it denotes an `ArithmeticValue` that belongs on the
  structural arm. Today `orderingValuesEqualIn`
  (`abstract_data_access_rule.go:1103-1115`) does fall through, which masks it;
  closing that fall-through is precisely what this item does. "Decline must
  measure zero before implementing" therefore cannot be reached while the
  producer misrepresents the type.

  THE ORDER, then:
    1. RFC-215's conversion lands (`HintOrdering` carries `sk.ValueExpr`).
    2. `ordering_identity_decisions_test.go`'s corpus census is RE-RUN, to
       re-derive the FieldValue/structural split and the decline count against
       the CONVERTED tree.
    3. This item is implemented against numbers describing the tree it will
       actually run on.

  WHAT MOVES AND WHAT DOES NOT. The 94.8% FieldValue-arm figure partitions a
  population RFC-215 redistributes: arithmetic sort keys relocate from the
  FieldValue population into the structural one, which are exactly the two
  sides of that percentage. The 126 `*RecordTypeValue` discriminators are
  themselves untouched — they never pass through `:796` — but their DENOMINATOR
  is not, so "126 of N" changes even though 126 does not.

  WHAT THIS IS NOT: a joint scope. Different files, different producers, and
  RFC-215 is strictly smaller and strictly upstream. RFC-215's producer is
  `RecordQueryInMemorySortPlan.HintOrdering()` — a physical plan's PROVIDED
  ordering. This item's comparator is `orderingValuesEqual`, on the data-access
  MATCH path; its non-test callers are `abstract_data_access_rule.go:1020` and
  the recursive `:1119`, both via `orderingValuesEqualIn`, with the `:1093`
  wrapper called only from tests
  (`grep -rn "orderingValuesEqual" pkg/ | grep -v _test.go`). They are not one
  producer under two names. What they share is the MEASUREMENT — they converge
  downstream at `ExplainValue`-keyed set membership — not the implementation.

  OPEN QUESTION, recorded as open rather than resolved: does RFC-215's F3
  population — the 599 ordinary dotted mints from
  `cascades_translator.go:6940`, `cascades_translator.go:6628` and
  `pullup.go:76` — overlap this item's territory? The evidence is PARTIAL and
  the disambiguation was not finished. What is known: one of the two translator
  sites mints from `k.Expr`, which is sort-key-shaped and would be this item's
  territory; the other sits inside a `for i, col := range p.Projections` loop,
  which is projection territory and would not be. That is one site classified
  either way and one unexamined (`pullup.go:76`). Finish it by measurement —
  the mint census already captures these three producers by stack — and do NOT
  settle it by inference from the two data points above.

- [ ] **CQ-59 (M, gated) — GroupByExpression.GetResultValue returns the INPUT
  row where Java constructs the grouping+aggregate output row**
  (GroupByExpression.java:129,:756-759 vs Go's inner.GetFlowedObjectValue()).
  Measured: PushDownThroughValue through the Go shape is the identity, so
  Java's push-then-match model cannot be ported at the groupby rule until
  this is fixed; a direct port of Java's result-value shape lost
  Fetch(IndexScan(IDX_CUST_STATUS)) to InMemorySort on the golden sentinel
  and was reverted -- the fix must come WITH the consumers that expect the
  input-row shape converted in the same change. Blocks the groupby rule's
  AccessorNamePathKey match becoming pushDown+Value.equals.
  UPDATE (typed-flowed-value sweep): the site is now FROZEN on an explicitly
  untyped quantifier object rather than riding `GetFlowedObjectValue`, which
  is typed since the sweep. Untyped, the wrong passthrough asserted nothing;
  typed, it additionally STATES that a GROUP BY flows its input row, and a
  downstream reader that believes a stated row reads every slot at the wrong
  depth (the failure LogicalProjectionExpression already measured). The
  freeze is behaviour-preserving against pre-sweep master and pinned by
  `TestFlowedValueExemptionsStateNoRowType`, which fails LOUDLY the moment
  the site is typed without the value being fixed. So this item's scope is
  unchanged and its pin is now non-vacuous.


- [ ] **CQ-65 (M) — InsertExpression and UpdateExpression state the WRONG
  result row, and are frozen untyped until they state the right one.**
  Java: `InsertExpression.java:71` is `new QueriedValue(targetType)` -- an
  INSERT flows the TARGET record's row, not the source select's, and those
  differ whenever the insert does not name every column in table order.
  `UpdateExpression.java:84` + `:209-213` is
  `new QueriedValue(RECORD<OLD: innerRow, NEW: targetType>)` -- an UPDATE
  flows the before/after PAIR, a two-column row, which is what makes
  `UPDATE ... RETURNING "OLD"."X", "NEW"."X"` expressible. Go returns the
  inner's flowed object at both sites.
  Both are now frozen on an explicitly untyped quantifier object (see CQ-59's
  update for why typed is worse than untyped here) and pinned by
  `TestFlowedValueExemptionsStateNoRowType`.
  The fix: INSERT is `values.NewQueriedValue(e.targetType)` and the
  expression already holds the target type; UPDATE needs the target TYPE
  threaded in (Go's `NewUpdateExpression` takes only the record NAME, which
  the planner's schema already resolves). Both change what the DML result
  value IS -- Java's `computeCorrelatedToWithoutChildren` for INSERT is the
  empty set precisely because its result correlates to nothing -- so the
  physical lowering and every consumer of the DML result value move in the
  same change.
  NOT in scope, and checked: `DeleteExpression.java:62` IS
  `inner.getFlowedObjectValue()`, so Go's Delete matches Java and stays
  TYPED. Likewise `LogicalUnion`/`LogicalIntersection`: Java's
  `RecordQuerySetPlan.mergeValues` resolves its result TYPE as the first
  non-existential quantifier's flowed object type
  (RecordQuerySetPlan.java:252-261, under an explicit "let's just pick the
  first result type for now"), so child 0's row is the spec's answer and
  freezing them would be a regression. All three are pinned in the opposite
  direction so the exemption cannot spread to them by analogy.


- [ ] **CQ-62 (S, bug fourteen: false prose over a live channel) —
  rule_implement_nested_loop_join.go:2380-2387 declares the leg-match arm
  "dead-in-effect TODAY ... a panic is reached only by
  TestRebaseOuterLegValue_OrdinalFirst". MEASURED FALSE: the lazy mint at
  :2400 (the QOV(merged)."LEG.COL" channel) fires 6 times across 5 tests,
  4 real-FDB (NWayCommaJoinProjectedExists, FourLegJoinDiscriminating,
  BuriedInnerJoinProjectedExists ×2, NWayProjectedExists...), all via
  implementJoinWithExistential:3247 with legLayout == nil. Only the BAKED
  arm matches the comment. Fix the prose and pin the LIVENESS (a test
  asserting the lazy arm is reached by one of those shapes, so it cannot
  be re-declared dead); the channel itself retires with CQ-61/53.
  Also: the other producer (cascades_translator.go:3579, RFC-142 mint)
  measured ZERO hits on every covered surface — but do NOT convert it to
  a loud decline: its own comment shows the mint is correct behavior for
  shapes reaching it (E.ID -> NULL -> EXISTS drops rows), and zero on
  covered surfaces is not unreachability. Pin what IS known.

  CQ-53 ENTRY CORRECTION (second): the "structural form already exists at
  concatLegPositionals" premise is REFUTED (see CQ-61). CQ-53 implements
  Java's parent-chained bindings ON TOP OF CQ-61's typed leg identity.
  Dotted is 7 (two non-merged-row group-key readers: ct:993 AND cg:4141),
  and CQ-53 closes it to 2, not 1.

  CQ-61/53 RULING (resolves the circularity the implementation found): the
  seed-rebake — Java's translateCorrelations, baking the seed against the
  chosen physical leg layout — is CQ-53's work, because it IS the mechanism
  that makes parent-chained per-alias bindings possible (the adapter's own
  comment has said so throughout); bundling it into CQ-61 would make CQ-61
  into CQ-53 under another name. CQ-61 REDUCES to the Group-A retyping
  (~5 readers whose counterparty is a correlation: ordinal_join.go:1419,
  executor.go:2805, ordinal_join.go:601, left_outer_existential.go:86,
  ordinal_seed_layout.go:143-170), landable independently, with the
  case-folding→exact conversion measured per site (SameLeg is exact; the
  leg name is documented UPPER — thread the typed alias to all ~13
  construction sites first, then measure each folding comparison's
  conversion). Group B's readers (ordinal_join.go:712,
  cascades_translator.go:5875, :3707) retire WITH their producers in
  CQ-53 — they serve live correlated-scalar and UNNEST/CTE channels
  (7 real-FDB tests break on deletion, measured) whose qualifier is
  embedded inside column-name strings; only the rebake removes the need.
  Order: CQ-61-reduced → CQ-53 (rebake + bindings + producers + Group B).


- [ ] **CQ-66 (M/L, gated) — the unnest ELEMENT quantifier states no type, and
  750 positional-merge slots inherit that.** MEASURED over the real-FDB corpus
  by the leg-local bake census's merge-slot partition: of 18246 merge slots,
  17492 are typed by the quantifier, 4 are recovered by the `legRowTypes`
  scavenger, 0 state a non-row scalar, and 750 state NOTHING. Every distinct
  witness is an unnest element alias (`AS "VAL"`, `AS "X"`, `AS "EL"`, ...).
  The cause is the array-element-inference gap already documented at
  `values.IsMixedSeedElementType`: Go does not infer array element types far
  enough for the element quantifier to state one, so its flowed value is
  UNKNOWN.
  Why the number is an UPPER BOUND rather than a defect count: an unnest
  element and a leg whose row was genuinely lost are not separable from the
  type alone -- both are UNKNOWN, for unrelated reasons -- which is exactly why
  the shared element predicate must NOT be tightened to demand a stated type
  (measured: `SELECT "X" FROM TS, TS."ITEMS" AS "X"` stops resolving). So the
  counter is REPORTED, not asserted at zero, and its job is to MOVE.
  The fix is to type the element quantifier from the array column's element
  type, which the translator already derives (`unnestArrayElementType`). That
  changes what 750 merge slots state, and the merged row type feeds ordinal
  baking and the executor -- a planner-wide change with a Graefe lap and a
  stress/golden comparison, not a rider on a typing sweep.


- [ ] **CQ-96 (MED, query-engine — needs its own RFC + Graefe ACK): the SQL
  translator MINTS an untyped `QuantifiedObjectValue` as a select result value,
  which Java cannot express at all.** Found while refuting CQ-68, and NOT part of
  that refutation: this is a different population that never reaches the decline
  classifier.

  **The site is `cascades_translator.go`'s EXISTS-bearing select builder**, where
  `resultValue = values.NewQuantifiedObjectValue(outerQ.GetAlias())` fills a
  select expression's result value with no type at all. Java builds the same
  thing as `overQuantifier.getFlowedObjectValue()` (`GraphExpansion.java:401`),
  and its guarantee is STRUCTURAL rather than disciplinary:
  `QuantifiedObjectValue.of` has no untyped overload
  (`QuantifiedObjectValue.java:187`) and `Quantifier.getFlowedObjectType` is a
  `Verify.verify` plus `requireNonNull` (`Quantifier.java:801-810`).

  **MEASURED, first-ever measurement of this site: 1,086 mints, 100% untyped, 0
  typed.** The TOTAL is not deterministic — a second consecutive run measured
  1,004, because these are rule firings and the memo explores a rule a different
  number of times per query. The RATIO is: 100% untyped both times, and no
  outcome-census equality moved across either run.
  Standing instrument, not a probe: `values.SelectResultMintCensus`, floored in
  the sqldriver harness (`selectResultMintFloors`) so the population stays
  counted while the divergence stands. The floor's DROP direction fails too — a
  closing gap and a darkening site are indistinguishable from a smaller number.

  **THE ATTRIBUTION THIS ITEM CORRECTS.** The divergence was previously booked
  against `implementExistentialSelect` (1,609) and `yieldExistsFlatMap` (269) on
  the strength of the FlatMap producer census. Those two sites BUILD NOTHING:
  they flow `sel.GetResultValue()` verbatim, exactly as Java's three
  `RecordQueryFlatMapPlan` constructions flow
  `selectExpression.getResultValue()` (`ImplementNestedLoopJoinRule.java:187,
  201, 214`). Their counts are a count of TRAFFIC through a courier. The
  divergence cannot be fixed at either of them, which is precisely why booking it
  there was load-bearing and wrong. `describeFlatMapResultOrigin` now reports the
  MINT ahead of the courier so this cannot recur silently.

  **`implementJoinWithExistential` (249 untyped) is a REAL third mint** and
  belongs to this item too — it is the one FlatMap site that mints rather than
  flows.

  **The risk that makes this an RFC and not a sweep** is measured and is the same
  one CQ-68's entry recorded: `GetFlowedObjectType`'s silent-untyped-member
  semantics. Today an untyped member "cannot contradict a typed one — it reports
  nothing" (`expressions/quantifier.go:322-328`). Typed, such members PARTICIPATE
  and can raise `MemberResultTypeDisagreementError` (`:344-346`), which declines
  the whole partition-select collapse (`positional_merge.go:79-96`,
  `rule_partition_select.go:663`, `rule_partition_binary_select.go:246-256`)
  where `DisagreeingLegs` and `UnderivableLegs` are asserted as HARD ZEROS.
  Typing also propagates into positional-merge slot types
  (`positional_merge.go:79`), moving `leg_layout_derivation_test.go:112-141`.

  DONE = the three mint sites type their result value the way Java does, the
  hard zeros and the leg-derivation classification pins are re-argued against
  their new populations (not relaxed), and the mint census's untyped count is a
  measured zero rather than a floor. **This moves CQ-95's 102 by ZERO and moves
  DIVERGENCES.md's read count by zero** — none of these values reaches the
  decline classifier. Book it for correctness, not for the residue.


- [ ] **CQ-76 (MED, plan quality, MEASURED) — `IN` on a leading column plus a
  trailing equality does not sarg at all; every such query pays a full scan.** ·
  M · query-engine review gate
  **Booked 2026-08-01 by the B6 docs-authority pass**, out of the same buried CQ-28
  prose as CQ-75. This one explicitly asked for its own item ("Far more general
  than signed zero and worth its own item") and never got one — the phrase
  "trailing equality" appeared nowhere else in this file, so the request could not
  be found by anyone looking for it.
  MEASURED, index `(v, w)`:

      v IN (5.0, 9.0) AND w = 5   ->  PredicatesFilter(Scan(T))     full scan
      v IN (5.0, 9.0)             ->  InJoin(IndexScan(T_V))        sargs
      v = 5.0 AND w = 5           ->  IndexScan(T_VW,[=,=])         sargs

  So the IN alone sargs and the equality alone sargs, but their conjunction
  degrades to a scan — the `InJoin` leg is not carrying the trailing equality into
  the index probe's suffix. Read Java's data-access rules first
  (`AbstractDataAccessRule` / the comparison-range construction) rather than
  special-casing the shape.
  DONE = `IN` + trailing equality builds a `[=,=]` probe per IN element, an EXPLAIN
  assertion pins the plan shape (not just the rows — the rows are already correct
  via the scan, which is why this was invisible to the suite), and the 1M stress
  comparison is run before/after per the stress-comparison workflow.


- [ ] **CQ-97 (query-engine): two shapes still read a partition through a single
  member.** Both are the same conflation the RFC-220 enumeration fix removed
  elsewhere, surviving in places the fix did not reach:
  - `rule_implement_in_union.go` takes the rich ordering from `innerPlans[0]`.
    That is sound only under the row-shape invariant, and it consumes
    `ToPlanPartitions` raw without the roll-up that would make a
    partition-level read legitimate. Converting it needs more than roll-up: it
    also pins a single member via `pinOrderedSpine`.
  - `MemoizeFinalExpressionsFromOther` (`implementation_rule.go:124`) mints a
    fresh Reference with NO constraint entry, so `OptimizeGroupTask`'s
    per-ordering retention looks the new reference up, finds nothing, and
    resolves by cost alone. That is what `pinOrderedSpine` is compensating for,
    which is why dropping the pin is not safe today. Measured consequence when
    dropped: an InUnion claiming ASC over a filtered full scan.

  The property-map half of this item is CLOSED. `MemoizeFinalExpressionsFromOther`
  copied the SOURCE reference's whole plan-property map onto a RESTRICTED
  reference — `ToPlanPartitions` walks the property map rather than the member
  list, so such a reference reported partitions for plans it did not contain,
  which is the identical defect its twin was written to avoid, live across NINE
  non-test call sites. Both entry points now share one implementation
  (`newRestrictedFinalReference`), so the two cannot drift apart again; the
  earlier count of "six rule callers" was low.

  What remains open here is only the CONSTRAINT half: the minted reference still
  carries no constraint entry, so `OptimizeGroupTask`'s per-ordering retention
  finds nothing and resolves by cost alone, which is what `pinOrderedSpine`
  compensates for.


- [ ] **CQ-98 (query-engine): two accepted cost movements from the RFC-220
  re-bless need a standing justification, not a one-time sign-off.**
  `set_op_fetch_pushdown#1` and `in_list_pushdown#34` moved. They were accepted
  as correct-but-different rather than as regressions; nothing currently
  re-derives that judgement, so a later change that moves them again will read
  as "already blessed". Pin the property each one turns on.


- [ ] **CQ-100 (query-engine): a projected EXISTS over a derived table that WRAPS
  A JOIN fails at Cascades TRANSLATION, before any planning decision is reached.**
  MEASURED, and measured on BOTH sides so it is booked as pre-existing rather
  than as fallout of RFC-226:

  ```
  SELECT d.aid, EXISTS (SELECT 1 FROM ld x WHERE x.id = d.aid)
  FROM (SELECT a.id AS aid, b.k AS bk FROM la a JOIN lb b ON b.id = a.id) d
  -> *api.Error 0AF00: Cascades translation failed
  ```

  Run through `embedded.PlanPhysicalForTest`, once with the RFC-226 working tree
  applied and once with it reverted via the save-diff/apply-R cycle: **identical
  error both times.** So this is not the seed/leg-ordinal family RFC-226 §1b
  fixes — that one reaches the planner and fails in the EXECUTOR on a
  source-relative ordinal. This one never gets a logical expression at all.
  Different layer, different fix, and it does not ride inside RFC-226.

  The shape is already visible in the suite and already known not to plan:
  `pkg/relational/sqldriver/leg_identity_census_fdb_test.go:164-168` carries it
  as "projected EXISTS over a derived-table leg" and its runner logs
  `shape %q did not plan … — no leg traffic contributed` and CONTINUES. That
  `t.Logf`-and-continue is why nothing has been red: the census tolerates a
  non-planning shape by design (it is measuring leg traffic, not planability), so
  the gap has been sitting in a passing test's output. Fixing this should also
  convert that log line into an assertion, or the next regression here is
  invisible in exactly the same way.

  Entry point: the derived source here is a projection over an NLJ, so the
  translator is being asked for a derived-table scope whose inner is a join.
  Read Java first — `GraphExpansion` / `SelectExpression` construction for a
  derived table — before changing the Go translator.


- [ ] **`refineRowTypes` / `refineFieldTypes` are dead, and the tests that
  document them now pin unreachable code.** RFC-232 replaced the member-agreement
  reduction in `Quantifier.GetFlowedObjectType` with strict `Equals` plus an
  explicit leg-table rule, so `expressions/refineRowTypes` and `refineFieldTypes`
  have no production caller — the only callers in `pkg/` are in
  `leg_table_population_blast_radius_test.go`. (Positive control for that sweep:
  the same grep for `legTablesAgree` returns its definition plus its live call
  site, so the zero is a real absence.)

  A test on dead code is worse than no test: those four cases report green while
  exercising nothing the planner runs, and their stated ruling — that a populated
  leg table against an empty one is a CONFLICT — is now the OPPOSITE of the live
  one, which adopts the stated boundaries (see the call site in `quantifier.go`
  for why). A reader who finds that file first will take away a rule the engine
  does not follow.

  DONE when the two functions are deleted, `leg_table_population_blast_radius_test.go`
  drives `GetFlowedObjectType` instead (keeping its argument, which is still
  correct and load-bearing: populating `Legs` is NOT behaviour-neutral even
  though `Equals`/`Hash` ignore it), and the prose sites that describe
  `refineRowTypes` as the tree's protection are re-pointed — `values/values.go`,
  `values/dotted_row_type_producer_census.go`, `executor/leg_column_provenance_census.go`,
  `core/query/clustered_outer_seed_contract_test.go` and
  `sqldriver/embedded_fdb_test.go` each assert it.

  Also drop the now-unused UNKNOWN/stated refinement with them, and PIN the
  assumption that replaced it: every member of a Reference is exactly typed, which
  is what makes strict `Equals` safe where master refined.


### The cost model decides blocking-vs-streaming on a criterion that never reads a cardinality

`PlanningCostModel` criterion #2 (max data-access cardinality) is GATED on at
least one operand having a KNOWN whole-plan maximum. Two full scans have none,
so it abstains and criterion #3 — residual predicate count — decides instead. A
sorted plan that sargs its predicate into an index range carries 0 residuals; a
streaming plan that reads an ordered index and filters carries 1. So a BLOCKING
operator is ranked against a STREAMING one by how much of the predicate reached
the access path, which is a category error between plans of different shape even
with perfect statistics.

MEASURED, and the population matters because both halves are needed to read it:
`statsNil=true` on 82 of 82 comparisons on `refactor/rfc197-mandatory-resolved`
and 23 of 23 on master, at `9a39b5006`. So NO cardinality is supplied on this
path in EITHER tree — the blindness is pre-existing, not introduced by RFC-232.
At the deciding comparison: `cardGate=false residA=0 residB=1`.

IT IS A COIN FLIP, NOT A REGRESSION, and the correction is worth keeping because
the first reading of it was wrong. `SELECT DISTINCT cat FROM t WHERE val > 0
ORDER BY cat` plans as `InMemorySort(IndexScan(IDX_VAL, [<>]))` on the branch and
`PredicatesFilter(IndexScan(IDX_CAT, [*]))` on master. Sweeping the threshold
over 300 rows showed each tree's plan is INVARIANT to selectivity — which says
neither reads a cardinality, NOT that either is better. Read the access bounds:
`[<>]` is bounded and `[*]` is not, so at 9/300 matching rows the BRANCH plan
reads 9 where master reads 300, and at 300/300 master's avoids a sort the branch
pays for. Each tree is right at one end of the range and blind at both.

THREE SHAPES WHERE SUPPRESSING THE FALLBACK IS WRONG, kept because they are the
acceptance tests for any fix and they refute the obvious one. Declining the
in-memory sort whenever a satisfying streaming alternative exists gives:

    SELECT id FROM rp WHERE region = 'us' AND plan > 'a' ORDER BY id
      InMemorySort(IndexScan(IDX_REGION_PLAN, [=, <>]))  ->  PredicatesFilter(Scan(RP))
    SELECT a, b, c FROM ab WHERE a = 1 ORDER BY c
      InMemorySort(Scan(AB, [=]))  ->  PredicatesFilter(IndexScan(IDX_AB_C, [*] COVERING))
    SELECT id, v, name FROM t WHERE v >= 20 ORDER BY name
      InMemorySort(IndexScan(IDX_V, [<>]))  ->  PredicatesFilter(IndexScan(IDX_NAME, [*]))

Each trades a tightly bounded access plus a small sort for reading everything.
Corpus-wide that rule gives 52 "improvements" and 3 red `plan_contains` targets;
the 52 are coin flips landing better, not decisions made correctly. An
access-BOUNDEDNESS rule does not fix it either: in the DISTINCT case above the
SORTED plan is the more bounded one, so boundedness declines to suppress exactly
where suppression was wanted.

- [ ] Port Java's `CardinalitiesProperty` and supply criterion #2. This is the
      only fix that decides all four shapes correctly at both ends of the range.
      It is filling an input the cost model ALREADY asks for and abstains
      without — not a new mechanism, and explicitly NOT part of RFC-232.
- [ ] Interim, if the preorder survives it: forbid criterion #3 from being
      reached when the operands differ in blocking-ness, falling through to a
      shape-neutral criterion. `cost_model_total_preorder_test.go` maintains
      total-preorder as an invariant, so "incomparable" may not be expressible;
      verify the fall-through leaves the preorder total before shipping.


### The next milestone is plan-time rebinding, and it needs a Graefe gate

The residual is not a list of allocation sites; it is one architectural fact.
**Go reconciles logical to physical ordinals per ROW at runtime, where Java rebinds
once at PLAN time.** Java's planner rebinds every FieldValue ordinal against the
physical quantifier's actual flowed type (`Value.translateCorrelations`,
`Value.java:339`), so a baked ordinal IS the physical slot and no runtime adapter
exists. Go seeds gated-join legs with the LOGICAL table-shaped leg type while a
physical leg may emit a row typed by its own plan output, so the two layouts can be
permutations of each other and the boundary gathers slots into leg order on every
row. The divergence is already documented at the fix site —
`pkg/recordlayer/query/executor/ordinal_join.go:1042` — which is the durable half
of this entry; this entry is the other half, and neither is complete without the
other.

Retiring the per-row gather means making Go's seed bake against the CHOSEN physical
leg layout, which is a change to RFC-232's runtime half rather than a local
optimisation. It is deliberately NOT started: it needs a Graefe ACK on the design
before implementation, and starting it would have invalidated the review laps in
flight on the campaign above.

- [ ] Bake join-leg FieldValue ordinals against the selected physical leg layout,
      so `ordinal_join.go`'s per-row permutation gather can be deleted. Read
      `Value.translateCorrelations` and `TranslationMap` first; the Go seed sites
      are the gated-join leg constructors. Gated on a Graefe ACK of the design.


- [ ] **Make the empty `AliasMap` singleton immutable by TYPE, not by doc.** Java's is
      backed by `ImmutableBiMap` (`AliasMap.java:169`), so a write is a compile/runtime
      error at the type. Go's is a plain map whose read-only-ness is asserted in a doc
      paragraph, and `alias_map_singleton_test.go` pins its IDENTITY rather than its
      immutability — so a future writer through the singleton is caught by nothing. Any
      mutation of the shared empty map corrupts every alias map in the process.


### An identifier-sensitive cost tie decides join nesting (RFC-235 §17)

**STATUS: open, root-caused, measured, and pinned. Blocking two cross-engine
corpus entries.**

Go's Cascades reaches a genuine cost TIE on the two nestings of an unconstrained
cross product and resolves it with a hash that consumes identifiers. The same
query, differing only in the names of the tables it reads, plans opposite
nestings — and since neither has an `ORDER BY`, the row ORDER a user sees
depends on what the tables are called.

JAVA IS NOT THE FIXED POINT — THAT WAS AN INFERENCE FROM TOO FEW SPELLINGS, AND
IT IS REFUTED. An earlier revision of this entry said "Java is stable: the first
`FROM` item is outermost in every spelling measured", and concluded Java rarely
reaches the tie because it prunes each `Reference` to one member. The scoping was
honest and the inference was wrong. Swept over EIGHT table-name pairs (each also
in its reversed spelling) at two cardinality arrangements — 16 combinations,
`conformance/cross_join_order_mechanism_probe_test.go`:

    Java deviates from FROM order in 10 of 16
    Java and Go disagree in 14 of 16
    cardinality changes NOTHING in either engine: both arrangements of a given
      name pair always answer the same way

Java's `PlanningCostModel.compare` ends in
`Integer.compare(planHash(a), planHash(b))` (`PlanningCostModel.java:320-326`) and
its `ImplementNestedLoopJoinRule` matches its two quantifiers with
`SetMatcher.exactlyInAnyOrder`, so Java generates BOTH nestings and breaks the
tie on a hash over plan structure — exactly as Go does, with a different hash.
Both engines are identifier-driven; neither guarantees FROM order.

MEASURED, not inferred:

- The divergence is PRE-EXISTING. At merge-base `e24f338e7`, with no existential
  anywhere, `SELECT a.qid FROM T_DUP_EIP AS a, T_DUP_EIQ AS a` already returns
  Java `[5 7 9 5 7 9]` vs Go `[5 5 7 7 9 9]`.
- It is NOT the Go-only statistics rung. Inverting that comparison changes none
  of these plans — mutation-checked, with the mutation's presence confirmed in
  the same invocation.
- It IS identifier-sensitive. `T_DUP_EIP/EIQ` agrees with Java on the shadowing
  spelling; the identical query over `T_DUP_SHP/SHQ` diverges.

PINNED BY `conformance/dup_alias_exists_order_probe_test.go`, which asserts both
engines' orders over six shapes and carries the renamed-table pair as its
demonstration. Java's column is the reference and must not move; Go's column
pins today's behaviour so that closing the gap turns the probe RED rather than
silently changing what conformance means.

WHAT IT BLOCKS: `dup_from_alias_leg_independent_exists` and
`dup_from_alias_shadowing_exists` report "row data diverges". Both return the
correct multiset; only the order differs. They conformed before RFC-235 because
the retired three-quantifier NLJ arm forced one nesting for `WHERE EXISTS` over
a comma join, and that nesting happened to be Java's. The arm masked this tie
rather than preventing it.

THE FIX IS A COST-MODEL CHANGE AND NEEDS ITS OWN RFC + GRAEFE ACK: either give
Go Java's prune-to-1, or replace the identifier-sensitive tie-break with a
stable one. NEITHER IS A JAVA-ALIGNMENT OPTION, because there is no Java
behaviour to align WITH: Java is identifier-driven here too (10 of 16 spellings
deviate from FROM order). Porting prune-to-1 would not produce a stable nesting —
the comparison it prunes with is the same planHash-terminated compare — and
matching Java exactly would mean reproducing Java's planHash bit-for-bit over
plan structures, which is neither a stated goal nor wire-relevant.

So the remaining question is NOT parity. It is whether Go should guarantee a
rename-invariant nesting as a quality property of its own, which is a
Go-beyond-Java choice and should be argued as one. Either way it carries a full
golden re-audit and a stress re-baseline.

THE ANNOTATION ROUTE WAS EXTENDED, and this paragraph records what it cost.
`plandiff.DivergenceDirection` had five values and every one was "Java is wrong"
or "Go rejects" (`JavaErrorsGoCorrect`, `JavaWrongRowsGoCorrect`,
`JavaIntermittentGoCorrect`, `BothErrorMessagesDrift`, `JavaSucceedsGoRejects`),
with no direction for "both engines are correct and the unordered row order
differs". A sixth was added — `UnorderedRowOrderDiffers` — and the two entries
are annotated with it.

It is only defensible because it is GUARDED rather than asserted in prose. The
harness requires Java's rows to be a PERMUTATION of Go's (so a dropped,
duplicated or altered row cannot hide under it), requires the query to have NO
`ORDER BY` (same-multiset-different-order is exactly what a dropped sort looks
like), keys the comparison on each element's TYPE as well as its value, and fails
when the engines start agreeing so a fixed tie-break cannot leave a stale pin.
Twelve unit arms drive it; the permutation guard and the recursive key are both
mutation-verified.

What REMAINS an owner call is the cost model itself, not the annotation.

Full write-up, including the plans and the mutation evidence: RFC-235 §17.

---


### [ ] A single-table `a = ? OR b = ?` full-scans with both columns indexed

MEASURED, on a two-schema twin fixture (`or_union_pk_dedup_edges_fdb_test.go`),
with an index on every probed column and 1209 rows in the probed table:

```
SELECT uid FROM u WHERE ua = 1500                     -> IndexScan(U_UA, [=] COVERING)
SELECT uid FROM u WHERE ua = 1500 OR ub = 1600        -> PredicatesFilter(Scan(U), [1 preds])
SELECT uid FROM u WHERE ua = 1500 OR ub = 1600 OR uc = 1700
                                                      -> PredicatesFilter(Scan(U), [1 preds])
(ua = 1500 OR ub = 1600) AND (uc = 1700 OR ud = 1800) -> PredicatesFilter(Scan(U), [2 preds])
```

A single equality takes the index. Add a disjunct and the whole query becomes a
full scan with a residual filter — roughly 1200 record reads where the union of
two point probes would read two index entries and two records. The corpus agrees
this is the standing shape rather than a fixture artefact:
`plan_shape.golden` records `PredicatesFilter(Scan(ITEMS))` for
`WHERE cat = 'A' OR cat = 'C'` and for `WHERE price = 10 OR price = 20 OR price
= 30`.

The union access path is NOT dead — it is chosen for the same disjunction as the
correlated inner of a LEFT JOIN, where a per-outer-row full scan is priced out:

```
SELECT d.did, u.uid FROM d LEFT JOIN u ON u.ua = d.da OR u.ub = d.db2
  -> FlatMap(outer=Scan(D), inner=DefaultOnEmpty(Fetch(UnorderedPrimaryKeyDistinct(
       UnorderedUnion(IndexScan(U_UA, [=] COVERING), IndexScan(U_UB, [=] COVERING))))))
```

So the machinery works and the enumeration reaches it; what declines is the
choice. Padding the table from 9 to 1209 rows did not move the single-table or
the conjunction-of-disjunctions cases, so it is not simply a small-table effect
at these sizes.

NOT the same as Finding 11 above, and the two point OPPOSITE ways. Finding 11 is
that `PredicateToLogicalUnionRule` expands every top-level OR where Java expands
only index-exploitable ones (memo bloat, worse-plan risk). This is that the
expansion, having happened, then LOSES on cost to a full scan in the case where
it should win by orders of magnitude. A fix for one should not be assumed to
address the other.

WHY IT IS FILED RATHER THAN FIXED: it is a cost-model question, which needs the
RFC + Graefe gate, and it is plan QUALITY rather than a wrong answer — the rows
are correct either way, which is why no test caught it and why the twin oracle
could not either. What the twin did catch, and what is now fixed, is the
correctness defect on the union path itself (the cross-leg dedup was keyed on
the row rather than the primary key).

WHAT TO CHECK FIRST, in order: whether the union alternative is COSTED at all
for a non-correlated select (it is enumerated — the rule's unit tests fire on
multi-OR shapes — but a plan absent from the memo and a plan present-and-outbid
look identical from EXPLAIN); then whether the per-leg cost carries the
cross-leg dedup's cost twice; then whether Java picks the union for the same
shape, which `plandiff` can answer directly and which decides whether this is a
divergence or a shared trait.

To reproduce: the plans above come from `EXPLAIN` on the fixture in
`TestFDB_OrUnionPrimaryKeyDedup_LegShapes`; the file header records which shapes
reach the union and which do not, measured rather than assumed.

A SECOND DATA POINT FOR THE SAME COST BEHAVIOUR, measured while writing
`sparse_index_query_safety_fdb_test.go`: a SPARSE index is not chosen either, on
a 900-row table, for any query — including one whose predicate is the index's
filter verbatim.

```
CREATE INDEX t_a_sparse AS SELECT a FROM t WHERE keep > 0

SELECT id FROM t WHERE a = 5 AND keep > 0   -> PredicatesFilter(Scan(T), [1 preds])
SELECT id FROM t WHERE a = 5 AND keep > 5   -> PredicatesFilter(Scan(T), [1 preds])
SELECT a  FROM t WHERE keep > 0 ORDER BY a  -> InMemorySort(PredicatesFilter(Scan(T)))
```

The candidate machinery is present — `ValueIndexScanMatchCandidate` carries the
sparse predicate proto and an opaque-filter flag, and such a candidate is
documented as never COMPLETE — so again what declines is the CHOICE, not the
capability. Taken with the OR observation above, the common shape is that a
secondary access path loses to a full scan at table sizes where it should not,
and it is worth checking whether both have one cause before treating them as two
items.

A sparse index that is never chosen is pure write cost: it is maintained on
every insert, update and delete, and buys nothing on the read side.

The row-level ANSWERS are pinned either way by that test file, including the two
predicates that LOOK implied by `keep > 0` and are not — `keep >= 0` (zero is
excluded) and `keep IS NOT NULL` (negatives are). Those are what will catch an
implication check that is too generous on the day sparse matching starts being
chosen.

---


### The ordered OR-union alternative is structurally unreachable

`PredicateToLogicalUnionRule` now emits `LogicalUniqueExpression` — Java's
PK dedup — where it used to emit `LogicalDistinctExpression`, the full-row
node. That was the fix for the OR-union duplicate-row defect: Go had
carried Java's *name* across instead of its *meaning*.

The audit was not finished. Two rules still match the full-row node:

- `rule_implement_distinct_union.go:33`
- `rule_distinct_over_union_dedup.go:39`

Their Java counterparts hang off Java's PK-dedup node, so with the OR path
no longer producing `LogicalDistinctExpression` the merge-sorted union is
unreachable from it, and `ImplementUniqueRule`'s required arm only ever
yields `UnorderedPrimaryKeyDistinct(member)`. An OR with a leg-compatible
`ORDER BY` therefore cannot produce Java's ordering-preserving union: it
must go unordered union -> PKDistinct -> InMemorySort, which also gives up
limit pushdown.

NOTHING REGRESSED, and that is measured rather than assumed — the ordered
alternative was already dead before the change:

    G=pkg/relational/conformance/explaindiff/testdata/plan_shape.golden
    grep -cE 'MergeSortUnion|RecordQueryUnionPlan' $G              -> 0
    git show master:$G | grep -cE 'MergeSortUnion|RecordQueryUnionPlan' -> 0
    grep -c IndexScan $G                                            -> 230   (control)

0 on both sides over 2556 queries, with 70 `UnorderedUnion` (68 on master).
So this is an architectural-coherence gap, not a live defect — which is
exactly why it would never be noticed later.

THE WORK: decide whether the two rules should match the PK-dedup node (and
then whether Java's ordered union is reachable at all in Go), or whether
the ordered alternative is genuinely out of scope and the rules are dead
code to delete. Either answer needs the golden to move or to be shown it
cannot. Query-engine change; needs the review gate before implementation.


### `IsConstantValue` is narrower than the property the explode rule wants

`InComparisonToExplodeRule` guards on `values.IsConstantValue` so a
non-constant IN list cannot be folded at plan time (which would evaluate a
field against a nil context, get `(nil, nil)` rather than an error, and
silently plan `b IN (NULL, 999)`).

The guard is SOUND — it never admits a row-dependent list. But the property
that makes an IN list explodeable is ROW-INDEPENDENCE, which is what Java
tests (correlation to the inner quantifier), not plan-time constancy.
`IsConstantValue` returns false for `ParameterValue`, `ConstantObjectValue`
and `ParameterObjectValue` — all row-independent, all explodeable, and Java
has a dedicated parameter arm for them.

Consequence: `x IN (?, 999)` is answered correctly but as a residual
filter, and for a parameterised IN over an indexed column that IS a lost
index probe. (For the column case the guard was added for, there was no
probe to lose — ComparisonIn is not scan-range compatible.)

THE WORK: widen the predicate from plan-time constancy to row-independence,
porting Java's parameter arm. The note is at the guard in
`rule_in_to_explode.go`. Query-engine change; needs the review gate.


### CQ-88 / RFC-236 — planner statistics: COLLECTED offline, read DEFENSIVELY

**Status: implemented.** The planner can now order joins by real per-table row
counts. Off by default; a connection opts in with `?planner_statistics=true`.

**Why the two obvious designs did not work**, both measured rather than argued:

- **`GetEstimatedRangeSizeBytes`** is a sampled estimator with a ~100KB floor.
  It reports 0 for a non-empty small table and quantizes everything above. A
  join-order decision turns on exactly the small-vs-large distinction it cannot
  see (`per_type_size_estimate_probe_test.go`).
- **Reading counts from index metadata** requires a COUNT index the schema may
  not have, and only answers for the types it covers — leaving the reader to
  mix real counts with defaults, which is the inverting case below.

So counts are collected by SCANNING, offline, by an operator-scheduled job.
COUNTING rather than sampling is the point: it removes the floor, the
quantization, and the bytes-to-rows conversion in one move.

The counts are EXACT for a store at rest, and only then. Collection spans
transactions -- a full scan does not fit in one -- so a record whose PRIMARY KEY
moves concurrently can be counted twice or missed. That does not restore the
estimator's problem, and the asymmetry is the reason: the estimator's error was
SYSTEMATIC and SIZE-CORRELATED, deterministically 0 for a non-empty small table
AT REST, in exactly the direction join order is most sensitive to. Key-move
error is bounded by key-move traffic, uncorrelated with table size, and cannot
manufacture a 0. See RFC-236 §4.

**Storage is outside every record store's subspace**, keyed by the store's own
subspace prefix bytes. Java owns record-store keyspaces 0-10 and marks the enum
`@API(UNSTABLE)`, so writing inside one would be a wire-compat hazard for a
feature Java does not have. Keying by prefix bytes makes it layout-agnostic: a
store is its prefix, however the prefix was derived.

**The failure mode that shaped the whole read side.** A refusal returns
`LeafScanCardinality` = 1e6, larger than almost any real count. So a PARTIAL
statistic is not merely less useful than none — it is INVERTING: one missing
type standing beside a real 150-row count makes the missing table the largest
in the schema and drives the join from the wrong side. Every gate is therefore
all-or-nothing, and completeness is schema-wide rather than query-wide (it is
undecidable query-wide at read time, and insufficient anyway, because
`FullUnorderedScanExpression` SUMS per-type cardinalities).

**Freshness is judged on FDB VERSIONS, not wall clock.** Versions are the
cluster's own clock — immune to skew between the host that collected and the
host planning. A wall-clock comparison across two machines can make an entry
effectively immortal, defeating the gate silently. A NEGATIVE age refuses
rather than reading as infinitely fresh (a restore from backup moves versions
backwards).

**Staleness cannot produce wrong rows, structurally.** The estimate side
(`Cost`) takes a `StatisticsProvider`; the proof side (`Cardinalities`,
`provenCardinalities`, `CardinalityProver`) does not, so a rule dropping a
DISTINCT because "max is 1" reasons from plan shape and cannot be misled by a
number. That was a fact about signatures, which erodes without anyone deciding
to erode it — it is now pinned by `TestCardinalityProofTakesNoStatistics` and
`TestProofInformsEstimateNotTheReverse`. `fkChainCardinalityCap` is the one
site where a statistic becomes something the code calls a bound; its binding
argument is structural, only its magnitude is statistical, and it reaches
`properties.Cost` and stops.

**What proves it works**, since a plan change is not by itself an improvement:

- Directional (`TestFDB_CollectedStatisticsDriveJoinOrder`): the same schema and
  SQL over MIRRORED arrangements. Flag off, the plans must be IDENTICAL; flag
  on, they must DIFFER and each drive from whichever table is smaller in ITS
  arrangement. A fixed tie-break cannot follow row counts across a mirrored
  pair. This caught the feature being completely inert — `ConnectionOptions`
  mapped two DSN parameters and silently ignored every other, so
  `PLANNER_STATISTICS` had no route from the connection string at all. Option
  resolved, cache key rendered, reader wired, feature dead. A
  single-arrangement test would have passed on the tie-break landing well.
- Measured (`TestFDB_Stress_StatisticsJoinOrder`): OFF vs ON *within* one
  arrangement, never across the pair — the two arrangements return different
  row counts, so a max taken across them compares different queries. The
  arrangements split into WIN (plan changed) and CONTROL (plan unchanged) BY
  THEIR PLANS, not by their timings, and both classes must be non-empty.

**The tenant question was answered by reading the codebase, not by adding
flags.** The RFC first proposed `--tenant` / `--all-tenants` over
`ListTenants()`. `grep -rnE 'ListTenants|OpenTenant|TenantName|fdb\.Tenant'
--include='*.go' pkg/relational/` returns 0 (the same sweep over `pkg/fdbgo/`
returns 90, so the command works). A case-insensitive grep for "tenant" there
returns 80 non-test hits, and that is the point: the multi-tenancy that exists
is SaaS tenancy expressed as SCHEMAS, and `pkg/relational/core/fleet` already fans out over them with
per-target transactions, failure isolation, bounded concurrency and a resumable
pass. `fleet.CollectStatistics` is one more `step` beside the index build, and
shares `ListTargets` with the other modes so a statistics pass cannot cover a
different set of schemas than a migration pass.

**TWO CORRECTNESS BUGS SURVIVED A GREEN SUITE AND WERE CAUGHT BY REVIEW.** Both
are worth recording because the suite was thorough in every dimension except the
two that mattered.

The collector DOUBLE-COUNTED a retried batch. `db.Run` retries its closure, and
the tally went into counters declared outside it, so a batch tripping
`transaction_too_old` re-read and re-added its rows. Exactness — the entire
premise — was false the first time a batch exceeded 5s, and `--batch-size` is a
shipped flag inviting exactly that. Worst possible direction: retries hit the
LONGEST batches, so the inflation lands on the biggest tables, the ones a
join-order decision is most sensitive to. Pinned by a transactor that invokes the
closure twice; mutation reads 40 for a 20-row table, exactly 2x.

An EMPTY TABLE disabled statistics for the whole schema, permanently. A declared
type with no rows produced no entry and the completeness gate is schema-wide, so
a fresh schema — mostly empty tables — could never use the feature. A test had
codified this with a rationale the provider's own clamp refutes. The dimension
that hid it: every other case in that file populates every type or none, and both
pass with the bug fully present.

**Still open, deliberately, and PRICED so the omission is not read as free:**
histograms / NDV / MCV and any distribution. The collector scans, so it COULD
compute them, but SELECTIVITY consumes them and that is a separate change: an
index probe is estimated as `RecordTypeCardinality(table) *
EqualityBoundSelectivity^equalities * RangeSelectivity^ranges`, and this work
makes only the FIRST factor a measurement. The second stays 0.1 per equality
whether the column holds two distinct values or two million.
`TestFDB_SelectivityBlindSpotWithCollectedStatistics` prices that: holding the
table count FIXED and varying only distinctness, two access paths that really
differ by 1000x are priced identically at 200 rows, so the planner intersects
them and reads 1001 index entries to reach a row one leg alone reaches in 1. No
better row count can reach that plan. The next increment therefore has a
committed number to be measured against — RFC-236 §7.1, which also records why
that increment is cheap here (every site applying `BoundSelectivity` takes scan
comparisons, so the column is always part of a key, and a key is stored sorted:
exact NDV is one ordered pass, no sketch).

Also still open: automatic or triggered collection; incremental recollection;
per-index statistics; plan-cache invalidation on data drift — a freshly
collected statistic reaches only queries planned after it.

Full design, including the measurements that killed the two rejected designs:
RFC-236.


### A nested-struct CARDINALITY index is BUILT but never MATCHED

`CREATE INDEX … AS SELECT CARDINALITY("struct"."int_arr")` now builds — that is
what RFC-237 unblocked, and it is what took `arrays-cardinality.yamsql` from 0
to 29 executed corpus queries. The index is created and maintained. The planner
never matches it:

```
SELECT "id" FROM qnest WHERE CARDINALITY("struct"."int_arr") = 1
go   : Project([_current.id#0], PredicatesFilter(Scan(QNEST), [1 preds]))
java : ISCAN(tab2_index [EQUALS promote(@c21 AS INT)])
```

The FLAT twin — `CARDINALITY("int_arr")` over a top-level array — matches
correctly and is pinned green in the same scenario, so this is specific to the
NESTED struct path, not to CARDINALITY and not to quoted identifiers.

**Rows are correct either way**, which is the whole difficulty: a declining
candidate full-scans and filters to exactly the right answer, so no row
assertion anywhere can see it. Pinned instead at the FULL SCAN in
`nested_struct_index_never_matches_gap.yaml` — a file whose NAME says it is a
gap marker rather than coverage, so the deliberately-wrong assertion inside it
cannot be misread as "nested struct indexes work". Closing the gap REDDENS that
arm; the redness is the handoff. When it lands, delete that file and add the
`plan_contains: IndexScan(...)` arm to `quoted_identifier_index_bridge.yaml`.

The Java corpus cannot cover it: the only corpus query exercising the index
sits two arms after `CARDINALITY("int_arr") = NULL`, which aborts its block
(booked separately as `conformance:java-planner-bug`).

Cascades matching change — needs its own RFC and the Graefe gate.

---


### ComparisonRange.MergeResult drops Java's residual LIST, so callers fail closed

STOP-level, needs the query-engine gate: this is an architectural change to the
matching infrastructure, so it needs an RFC with a Graefe + Torvalds ACK before
implementation, not a drive-by fix. Recording it here with the measurements in
hand rather than starting the port unreviewed.

**The divergence.** Java's `ComparisonRange.merge(Comparison)` is TOTAL — it
never fails. Its `MergeResult` carries a range plus a residual LIST, and the rule
is that equality always wins and nothing is ever dropped:

| case | Java | Go |
|---|---|---|
| NONE-type (NOT_EQUALS, IN, LIKE, TEXT_*, IS_DISTINCT_FROM) | residual, range untouched | pushed into the range as an INEQUALITY |
| Equality + INEQUALITY | keeps the equality range, residualises the inequality | `Ok=false`, `Range=nil` |
| Equality + EQUALITY (different) | keeps the range, residualises the incoming | `Ok=false`, `Range=nil` |
| Inequality + INEQUALITY (duplicate) | dedups (`inequalityComparisons.contains`) | appends the duplicate |
| Inequality + EQUALITY | becomes Equality(new), old inequalities residual | `Ok=false`, `Range=nil` |

Go's `predicates.MergeResult` has `Ok bool` and a SINGLE `Residual`, and no
caller in the tree reads `Residual` at all — so the residual channel exists in
name only. Callers therefore fail closed where Java pushes the equality down and
keeps the rest as a filter — with ONE exception this entry originally got wrong.
`AsComparisonRange` SKIPPED the rejected conjunct instead of failing, so
`x = 5 AND x > 7` converted to `x = 5`: weaker than its input, silently. Fixed —
it now returns `(nil, false)` — but the sweeping "every caller fails closed" was
false as written, and the review that caught it was reading the code rather than
this entry. `mergeComparisonRanges` states the gap in
its own comment: "equality/inequality is not representable by ComparisonRange
without a residual." That is the standing admission that the residual list is
the real answer.

**Consequence.** `tryMergeParameterBindings` turns a rejection into a LOST
MATCH: where two child branches bind the same parameter alias with an equality
and an inequality, the index candidate Java would keep — an equality seek plus a
residual filter — is not produced at all. Wrong plans, never wrong rows; the
rejected predicate is not silently dropped on any live path.

**Reachability, measured, not assumed.** Instrumented all three arms of
`Merge` and ran 10940 tests (cascades + relational/core + plan/plans, `-count=1
-v`, stderr captured to a file because `go test` swallows it for passing
packages):

- 202 hits on the Empty arm. The only NONE-type to reach it was
  TEXT_CONTAINS_ALL, 3 times, all from the `textRange` helper in
  `f21_comparand_identity_test.go` deliberately building a text range — no
  planner path.
- 19 hits on the non-empty arms, every one from `ComparisonRange`'s own unit
  tests plus `TestNullRejectedByScanRange_*`.

So all five divergences are LATENT today. That is a reason to fix them before
something reaches them, not a reason to leave them: the arms are untested
precisely because nothing exercises them.

**Pinned meanwhile.** `mergeComparisonRanges` had NO test of any kind. It now
has `match_info_merge_ranges_test.go`: the agreeing arms, plus
`TestMergeComparisonRanges_EqualityInequalityRejectsUnlikeJava`, which pins the
three rejecting arms and says in its failure message that closing the divergence
means REPLACING it with an assertion that the equality survives and the rest
comes back as a residual — never deleting it. The dedup claim in that file is
mutation-verified: disabling the dedup loop makes the overlapping union report 3
comparisons instead of 2.

- [ ] Write the RFC for porting Java's total `MergeResult` (range + residual
  list) and threading residuals through `PredicateMapping`/`MatchInfo` so a
  partial match can carry them as filter predicates. Get the Graefe + Torvalds
  ACK on the RFC, then implement, then the impl lap. The shape-only port (change
  the struct, keep every caller failing closed when residuals are non-empty) is
  NOT worth landing alone — it ships the API churn without the plans.

---


---

## 4. SQL front-end — parsing, resolution, identifiers, CLI

Semantic analysis, name resolution and the translator boundary — everything between the parse tree
and the logical plan, plus the `frl` CLI surface.

### RFC-173 residual — recursive-CTE re-keying
  - [ ] Follow-up (non-blocking, PRE-EXISTING, recursive-CTE re-keying area): (a) un-aliased QUALIFIED
    seed projection folds a qualified name into `outCols`; (c) LENGTH-MISMATCHED column-alias list is
    silently ignored rather than a loud error (SQL says error). Fix when a slice touches the area.


### RFC-173 residual — semantic-resolver gaps
  - [ ] **BOOKED (Slice 2d discovery — PRE-EXISTING semantic-resolver gap, orthogonal): a WHERE ref to a
    DEEP chained alias → 42703 "column does not exist".** Boundary: a 3-link AS-alias (`Z` in `…Y.DEEP AS Z
    WHERE Z…`) resolves; a 4+-link AS-alias (`v` in the 4-link chain), a 3-link AT-alias (`o`), and a
    mid-link AT-alias (`yo`) all 42703. Thrown at semantic resolution UPSTREAM of cascades translation, so it
    reproduces identically in name-model (NOT caused by ordinalization; the clean projections at every depth,
    incl. AT, do resolve). Affects filtered-deeper-than-3-link + any AT-in-WHERE shape in BOTH engines. A
    SemanticAnalyzer scope-depth fix, separate slice — filtered 3-link (the expressible filtered depth) already
    ordinalizes.

  - [ ] **BOOKED SLICE (Graefe, RFC-173 S4 Slice 2b follow-up — orthogonal resolver gap): scalar sibling
    of an unnested array element does not resolve (42703).** `SELECT X.K FROM T4, T4.SARR AS X` where the
    SARR element has a scalar sibling `K` → `42703 column "X.K" does not exist`, IN THE BASE CASE (no
    chaining, no filter). A semantic identifier-resolution gap (the resolver can't reach an unnest
    element's own sibling scalar by the element alias), orthogonal to filtered-chained predicate placement.
    Java resolves it (standard lateral ref) → real Go divergence, must eventually close (likely a small
    resolver fix once scoped). Discovered during the Slice-2b de-risk (the would-be x-column conjunct
    `WHERE X.K = v` is unreachable via this gap; the ⊆-outerLegs gate needs no special handling for it —
    an x-ref flows through the keep-rebase branch once the resolver closes).

### [ ] front-end follow-on (Torvalds correlated-EXISTS review, recommended, non-blocking): extract `classifyJoinOn(...)` from `buildCorrelatedExists`
`buildCorrelatedExists` (pkg/relational/core/embedded/logical_predicate.go) is ~412 lines — mostly essential
complexity (INNER/LEFT/RIGHT/FULL × correlation × ordering × CTE × nested-subquery), not cruft (Torvalds
ACK'd it). The one clean extraction: pull the per-join ON classification loop body (~70 lines: node-vs-lift-vs-decline)
into `classifyJoinOn(...) (nodeOn, lifted, err)` — the densest, most independent, most nameable sub-decision;
would drop the function to ~340 and make the loop legible. Recommended, not a should-fix.



### Plan-time rejection and resolver convergence (lifted out of completed entries)
  - [ ] **Follow-up (RFC-087, Graefe): reject aggregate-in-scalar-context at PLAN time.** `WHERE COUNT(*) > 0` reaches `AggregateValue.Evaluate` at row eval; RFC-087 made it a clean runtime `AggregateEvalError` → 42803 (was an uncaught goroutine crash on master — Graefe confirmed). Java rejects this at semantic-analysis / plan time ("unable to eval an aggregation function with eval()"). Detect an aggregate in a per-row scalar predicate (WHERE / JOIN-ON / projection-not-under-GROUP BY) during planning and reject there, matching Java exactly. Runtime 42803 is the safety net; plan-time is the parity fix.

  - [ ] **Follow-up (RFC-088, Graefe condition): converge `validateGroupByProjection`'s existence check onto the semantic resolver.** Java does NO standalone existence check for GROUP BY keys — `SemanticAnalyzer.resolveIdentifier` over the full multi-source scope already guarantees existence, and `validateGroupByAggregates` enforces only the algebraic 42803 rule (key must be grouped-or-aggregated). Go currently runs a SECOND, hand-rolled existence oracle (`tableFields` = union of all source descriptor field names, bare-name match) that is deliberately qualifier-blind, so it would false-ACCEPT a wrong-qualifier key (`e.dname` where dname is on the joined dept) — SAFE today ONLY because the precise resolver runs first at every call site (top-level `resolveColumnName` ~L1002; correlated-scalar GROUP-BY-key resolution in `buildCorrelatedScalar`), an ordering invariant now pinned by a code comment at `validateGroupByProjection` and by `TestFDB_GroupByWrongQualifierRejected`. End-state: route existence through `resolver.ResolveIdentifier` and leave `validateGroupByProjection` enforcing only 42803, removing the duplicate oracle and the ordering dependency.

  - [ ] **Cleanup (RFC-079 follow-up b): unify the SimpleTable logical builder onto `visitSelectGroupBy`.** The "one query path" endgame (CLAUDE.md "no parallel pipelines"). `buildSelectShell`/`buildLogicalPlanForSelect` is a second SELECT builder reached by plain-table SELECTs, derived tables, AND UNION branches; it has repeatedly drifted from the modern `visitSelectGroupBy` (the RFC-079 alias bug was one such drift). Route ALL of its callers through `PlanVisitor.visitSelectGroupBy` and delete the legacy builder. Larger than a single-bug fix (multiple callers, full regression surface) — Graefe's condition: must unify the WHOLE SimpleTable builder, not graft a special case onto the union entry.

- [ ] **Comma join over a nested-shadowing CTE inside a CTE body
      (RFC-180 round-7 reach):** `WITH x(m,n) AS (…) SELECT p.m, p.n, q.m
      FROM x p, x q` inside a CTE body plans but dies at runtime with an
      ordinal-resolution error (P.N vs merged-row keys [P.M N Q.M] — the
      qualified/bare name-model seam, RFC-173 surface; stars and explicit
      columns both hit it). Single-source nested bodies work (pinned).
      Make the merged-row keys carry the qualified names the projection
      mints, or decline the shape at plan time.


- [ ] **CQ-38 (MED, semantics) — NaN comparison follows Java's total order, not
  IEEE.** MEASURED: `WHERE (v/z) = (v/z)` returns ALL rows (IEEE says none),
  `<> ` returns none (IEEE says all), `> 0` returns all (IEEE: every NaN
  comparison is false). Deliberate -- predicate comparison falls through to
  `values.CompareFloat64`, the Double.compare total order with NaN greatest, and
  the comment says "matching Java Double.equals". So for NaN, Go matches Java
  and BOTH diverge from the SQL standard, Postgres and CRDB -- the opposite
  posture from signed zero, where Go keeps IEEE and diverges from Java.
  NOT symmetric with the signed-zero case and not a simple "apply the same
  ruling": strict IEEE makes the comparator NON-TRANSITIVE and destroys the
  total order that ORDER BY, tuple key order, merge joins and dedup all share.
  Splitting PREDICATE comparison (IEEE) from ORDER BY (total order) is coherent
  -- it is exactly the split RFC-082 already documents for signed zero -- but it
  touches every comparison site and needs its own design gate.
  DISTINCT/GROUP BY treating two NaNs as one value is settled and would NOT
  change: value identity is tuple-key identity and every NaN packs identically.
  Current behaviour is pinned by `nan_comparison_semantics_test.go` so it cannot
  drift unobserved while the question is open.
  **Also corrected DIVERGENCES.md, which claimed Go returned FALSE for NaN
  self-equality "(IEEE, SQL standard)".** That was asserted, never measured, and
  backwards -- written by me earlier the same day while documenting Java's `=`
  bug. A false claim in the divergence register is worse than no claim.

(Header previously read "RFC-193/194 follow-through". Neither document was ever
committed — no such file exists in the repo or its history. The findings they
were meant to hold are recorded in the cost-model and instrumentation items of sections 3 and 8, which is why they survived
the drafts being lost. Do not reuse numbers 193/194 for unrelated documents: a
stale reference that resolves to the WRONG doc is worse than one that resolves
to nothing.)


- [ ] **CQ-52 (MED, S/M, RFC-197 follow-on) — the parser HAS the qualifier/leaf
  segments, joins them into one string, and the resolver splits them back
  apart.** · **PARTIALLY LANDED (#540, `f50cee43e`) — STAYS OPEN.**
  **Status correction, 2026-08-01.** #540 landed the **PROJECTION channel only**:
  `LogicalProject` now carries the segments through to the bakers, and the ratchet
  learned to see through helpers. It did NOT close the item, and the commit did not
  check this box. The live residual is named in the ratchet's own debt entries —
  `cascades_translator.go:5742` and `:5744` retire "when the remaining
  `LogicalProject` producers carry `ProjectionRefs`"; `:6070` and `:6102` retire
  "when the last caller stops slicing a rendered name".
  Also note the arithmetic in the sentence below is superseded: the call-boundary
  taint added in #540/#544 changed which sites are visible, so the `translator`
  bucket now stands at **15** (`pkg/docscheck/field_name_decision_test.go:462`),
  and "CQ-52 retires four translator sites" no longer names the same four.
  Three debt sites exist only to undo a join the layer above performed
  for no reason. It was four; the gathered-EXISTS wrap's dotted arm is DELETED
  (its window map is keyed by leg identity, so a qualifier sliced out of a
  column name had no key it could honestly use, and it was measured unreachable
  before removal). The shape it handled now DECLINES to the name model, so
  CQ-52 no longer has to retire that site — it has to give the resolver the
  segments that would let it resolve again.

  `logical.SortKey` (`pkg/relational/core/query/logical/operators.go:298-303`)
  carries `Bare` / `Qualifier` / `Qualified`, documented as "parse-tree segments
  of a plain column reference key", with qualification defined as FullId SEGMENT
  COUNT — structure, not a string scan. `logical.GroupKey` (`:414-420`) carries
  the same three. Both are POPULATED from the parse tree
  (`embedded/logical_builder.go:679`, `embedded/plan_visitor.go:1601`,
  `embedded/select_parser.go:864/997/1353/1608`) and then DISCARDED: the
  translator mints its lazy carrier from the joined text instead
  (`core/query/cascades_translator.go:4920` and `:6006` build
  `&values.FieldValue{Field: k.Expr}`; `embedded/logical_predicate.go:2967` is
  the join itself, `display = qualifier + "." + bare`).
  `LogicalProject.Projections []string` (`operators.go:219`) never had the
  segments at all.

  The resolver then re-derives what was thrown away, by `strings.IndexByte(…,
  '.')`, at four sites — `cascades_translator.go:5674` (single-ForEach flat
  baker), `:5722` (`bakeDottedRefsToLegQOV`), `:5846`
  (`bakeFlatRefsAgainstColumns` leg-window arm) and
  `exists_gathered_cluster_wrap.go:131` (gathered-EXISTS wrap). All four are now
  tagged `translator:` in `pkg/docscheck`'s `knownFieldDecisionDebt` rather than
  `dotted:` — each guards `Child != nil → bail` before the slice, so it only
  ever sees a lazy carrier minted from parsed text, and each emits a born-baked
  value. They are name RESOLUTION, correctly performed on a representation that
  destroyed its own input.

  Fix: keep the segments end-to-end. Give `Projections` the segment triple the
  other two already have, carry it into the lazy carrier (or bake at mint time,
  where the resolver scope is in hand), and delete the four re-splits. This
  retires four debt entries at the SOURCE, shrinks the translator bucket, and
  removes the one representation in which a quoted `"A.B"` column and a
  qualified `A.B` reference are the same bytes — the ambiguity that was already
  a live defect once on the cluster-attribution path (see
  `cluster_ref_attribution_test.go`).

  **RE-VERIFIED AT `041838856`. Far more has landed than the status text says, and
  the residual is NOT four line-keyed sites — it is ONE BEHAVIOUR DECISION.**
  - The four keys named above and in `road-to-prod.md` (`:5742`, `:5744`, `:6070`,
    `:6102`) no longer exist. The surviving entries carrying those same reason
    strings are `cascades_translator.go:5811`, `:5813`, `:6139`, `:6171`. (Two of
    those reason strings still cross-reference the OLD keys `5742`/`6070`
    internally.)
  - The parsed channels are DONE. `logical.ColumnRef` (`logical/operators.go:287-292`)
    and `LogicalProject.ProjectionRefs` (`:280`) carry the triple, minted only
    through `ColumnRefFor` (`:311`), which reconciles the triple against the
    rendered name and returns the ZERO value on mismatch — the invariant behind
    "absent means unknown, never unqualified". Projection, nested projection,
    GROUP BY keys and aggregate operands all take the segmented path.
  - **Java confirms the direction unambiguously.** `Identifier.java:34-58` holds
    `name` + `List<String> qualifier`, built segment-by-segment off the ANTLR
    `FullId` in `IdentifierVisitor.java:56-64`; the join exists ONLY as
    `toString` for error text (`Identifier.java:61-63`), and
    `SemanticAnalyzer.lookup` (`:445-486`) compares whole `Identifier` objects
    with structural `withQualifier`/`withoutQualifier`. There is no `split("\\.")`
    or substring slice anywhere in Java's resolution path.
  - **But the eight remaining `LogicalProject` producers that carry no refs are
    NOT a closable list, and two of them are not "migratable" as a prior pass
    classified them.** `plan_visitor.go:539` and `logical_builder.go:706` are the
    post-sort strip projections, and their names are COPIED verbatim from
    `buildPostAggregateProjection` (`logical_builder.go:509`) — aggregate
    renderings like `MAX(E.SALARY)`, where `:526-533` DELIBERATELY aliases a
    dotted rendering precisely so the dot is not read as a qualifier. Handing
    those a segment triple would be inventing one, which
    `cascades_translator.go:2517-2535` names as the forbidden move ("an invented
    triple is trusted exactly like a real one, which is why the honest value is
    the zero one"). The rest are machinery mints with pre-baked ordinals. So
    `:5811`'s retirement condition as literally written — "when the remaining
    `LogicalProject` producers carry `ProjectionRefs`" — is UNSATISFIABLE, and
    should be reworded to name the structural class it must exclude.

  **STOP — this item's remainder is an owner behaviour decision, stated in the
  tree and deliberately left open.** `cascades_translator.go:6218-6237` (mirrored
  at the producer, `:2517-2535`) asks: **should a star-projected body column be
  leg-addressable at all?** The star-body normalization in `translateScan`
  (`:2536`) mints output labels that have NO parse tree behind them, so an absent
  triple there is STRUCTURAL and permanent. Those labels are bare by construction
  today, so the class is not firing — but a quoted identifier carrying a dot is
  legal SQL, and the re-split arm would answer the question BY ACCIDENT if it were
  converted without the decision being made.

  **Correction to an earlier draft of this paragraph, which said "Java gives no
  guidance: it has no projection whose output labels lack an `Identifier`". That
  is false.** `Expression.name` is an `Optional<Identifier>`
  (`Expression.java:100-113`) and `Expression.ofUnnamed` (`:305-322`) mints empty
  ones at roughly twenty in-tree call sites. Java's guidance is therefore
  positive, not absent: an output with no name is **not name-resolvable** —
  `SemanticAnalyzer.lookup` skips it before any comparison
  (`SemanticAnalyzer.java:459-461`) rather than matching it positionally or
  synthesizing a string for it. The part that really has no Java analogue is the
  RE-SPLIT: Java's identity is `name` + `List<String> qualifier`
  (`Identifier.java:34-58`), built segment-by-segment from the ANTLR `FullId`
  (`IdentifierVisitor.java:56-64`) and joined only for display
  (`Identifier.java:61-63`), so no dotted string is ever re-parsed and the
  accidental-answer arm cannot exist there. The owner decision stands; its basis
  is a Go-only normalization, not Java's silence.

  One honest caveat recorded at `:6239-6250`: nothing instruments the split
  population (the census beside these bakers counts qualifier MATCHES, never
  splits), so the "110 → 3 → 0" figures quoted here and in `road-to-prod.md` are
  scratch measurements, not instrument readings. If this item is resumed, an
  actual counter is the first deliverable.

  **THE FIRST DELIVERABLE IS DONE (2026-08-06). The counter exists, and the
  measurement changes what the remainder is.** `name_split_census.go` counts both
  splitting arms per resolution decision, bucketed by which representation
  DECIDED (segmented / SPLIT-QUALIFIED / splitBare); `AssertNameSplitCensus` is
  wired into the sqldriver `TestMain` beside its siblings. Measured over the
  real-FDB corpus, STABLE across two consecutive full-suite runs:

  | arm | calls | segmented | SPLIT-QUALIFIED | splitBare |
  |---|---|---|---|---|
  | `legQOVSegmentsOf` | 9 | 9 | **0** | 0 |
  | `flatColumnBake` | 2 | 0 | **0** | 2 |

  **SCOPE FIRST, because every number below is scoped and the previous revision
  of this block was not.** The census watches TWO arms — the two leg bakers in
  `cascades_translator.go` — and nothing else. It is not a census of re-splitting
  in Go. Four splitting siblings are UNINSTRUMENTED by anything, and they are
  named in the census header and booked as **CQ-94**:
  `recursiveRemapValues` (`cascades_translator.go:9547`, which
  manufactures a `CorrelationIdentifier` outright and is strictly worse than
  SPLIT-QUALIFIED), `parseColRef` (`core/embedded/colref.go:18`, 27 production
  call sites), `splitQualifier` (`cascades_translator.go:5016`) and the
  derived-unnest base-column split (`derived_unnest.go:107`). A fifth near
  sibling, `unnest_gather.go:391`, IS instrumented — by the seed-window reader
  census, not this one.

  - The "110 → 3 → 0" ZERO is CONFIRMED — for the first time by an instrument.
    No qualifier is being manufactured from a rendered name AT THESE TWO ARMS in
    production traffic. Read globally, that sentence is false; the dark siblings
    above are uncounted.
  - **The POPULATION refutes the scale the prose implied.** Eleven calls between
    the two arms, not ~110. `legQOVSegmentsOf` takes the segmented path on all
    nine of its calls — its fallback split is not merely producing no qualifiers,
    it is not being ENTERED. `flatColumnBake`'s re-split arm is reached twice,
    both by a BARE name that falls through unbaked; it has never been handed a
    dotted name at all.
  - So the hard zero is nearly vacuous on its own, and what carries the weight is
    the floors PLUS a unit wiring pin. **The CALL floors are not the floors that
    matter, and saying they were was the weaker claim this block used to make.**
    This census's zero is a zero over the SPLIT population, and at
    `legQOVSegmentsOf` the two numbers do not move together at all: 9 calls, 0
    splits. A Calls floor there measures the CONVERTED channel and would sit
    green through the splitting arm losing its recorder entirely. So the split
    population is floored SEPARATELY and per site:
    - `flatColumnBake` — split floor **1** (measured 2; both its calls are
      splits). A real floor.
    - `legQOVSegmentsOf` — split floor **0**. A DECLARATION, not an absent floor:
      "measured empty over this corpus, covered by a unit wiring pin instead".
      The assertion checks it in the STALE direction — if the arm ever acquires a
      split population, the declaration reds so somebody raises it to a real
      number rather than leaving a permanent exemption.
  - **That zero is a corpus fact, not a dead arm, and it was MEASURED.** A panic
    at the arm's entry is reached with a DOTTED name — `TestRecursiveBodyGatesOrdinal`
    drives it as `C.ORDER_ID`, so the SPLIT-QUALIFIED bucket itself is reachable.
    It therefore cannot be deleted the way `singleForEachBake`'s qualifier arm was
    (whose panic probe came back EMPTY at the same reach). The consequence is
    stated rather than smoothed: a recorder dropped from either SPLIT arm of
    `legQOVSegmentsOf` is INVISIBLE to this corpus, so it is pinned per arm and
    per class by `core/query/name_split_recorder_wiring_test.go` — five mutation
    directions verified red (each of the four recorders removed; the site
    constant swapped).
  - The `flatColumnBake calls 102` in the dotted-leg qualifier census is NOT in
    conflict: that counts `legWindowSlot` calls, which the SEGMENTED caller
    (`bakeSegmentedColumnRef`) also makes. 102 against 2 is the converted channel
    carrying essentially all the traffic.

  **The four debt entries are NOT retired — they are re-keyed and restated.**
  `:5811`→`:5815`, `:5813`→`:5819`, `:6139`→`:6145`, `:6171`→`:6177` (plus
  `:6103`→`:6109`, `:6199`→`:6205`); ratchet unchanged at 53. They cannot retire,
  and the reason is now measured rather than argued: the arms serve the carrier
  that states NO segments, and one class of such carrier — a projection MACHINERY
  mint, star-body normalization in `translateScan` — structurally never can.
  Deleting the arms would convert those carriers' behaviour by accident.

  **WHAT THE ARM SHOULD DO IS RULED, AND THE GATE NOW SAYS SO.** The previous
  revision of this block called the whole thing an open owner question and had
  the failure text tell the tripper to escalate. That was too weak by exactly one
  step. Java rules it: `SemanticAnalyzer.lookup` skips a projection output that
  carries no `Identifier` before any name comparison
  (`SemanticAnalyzer.java:459-461`), so a machinery-minted label is NOT
  name-resolvable and must not resolve by ANY spelling — least of all by this arm
  reading a dot inside it as a qualifier boundary. Widening the SPLIT-QUALIFIED
  zero is therefore the one fix that is known-wrong, and the failure text now
  states that plus the correct behaviour (DECLINE) rather than handing the
  tripper an escalation. A gate that describes a settled question without stating
  its answer gets closed by whoever trips it, in the wrong direction.

  What remains owner-owned is narrower and is NOT this arm's question: whether
  Go's star-body normalization should stop minting resolvable-looking labels in
  the first place, which moves a live projection channel. Until it does, the
  behaviour at the arm is settled.

  Not query-engine machinery: no cost model, no rule, no executor contract. The
  segments already exist and are already correct; this is deleting a join and a
  split. Pin with a `"A.B"`-quoted-column-vs-`A.B`-qualified-reference pair that
  the joined representation cannot tell apart.


- [ ] **CQ-99 (SMALL, bounded SEARCH — RFC-197): does Go have an output type
  whose column name comes from a RENDERED value, fenced only by the order its
  callers happen to run in?** This is a search with a defined stopping point, not
  an open-ended audit, and it is booked rather than answered because no grep run
  so far has been scoped widely enough for its negative to mean anything.

  **Where it comes from.** Closing the `contract:` bucket rested on Java keeping
  no name where Go renders one. That is right about aggregates — an unaliased
  aggregate is `Column.unnamedOf` (`GroupByExpression.java:754`) surfacing as the
  positional `_0` (`Expressions.java:251-253`, `Type.java:2645-2651`) — but the
  general form "Java never renders an expression into a column name" is FALSE and
  should not be carried forward. `Star.java:178-179` renders one:
  `expression.getUnderlying().toString()` installed as a `StructType` FIELD NAME,
  reached from all three `Star` factories.

  What keeps it out of result metadata is CALL ORDER. `Expressions.expanded()`
  (`Expressions.java:79-84`) flattens every `Star` before any
  `LogicalOperator.output` is built — the expansion runs at
  `LogicalOperator.java:397, 436, 473, 531, 651` — and `underlyingAsColumns()`
  (`Expressions.java:269-287`) has no rendering fallback at all: the name is an
  `Optional` and stays empty when absent. So Java's guarantee is DISCIPLINE, not
  construction, and the Go-side consequence is to look for the FENCE rather than
  for an absence.

  **The work.** Enumerate every Go site that derives an output column name and
  classify each as (a) name-as-data carried from construction, (b) rendered but
  structurally unable to reach metadata, or (c) rendered and fenced only by call
  order. Any (c) is the same pathology as `values.go`'s `explainValueOrdinals` —
  one declaration serving display and naming, separated by convention — which is
  already ruled STOP on the `.Field` ratchet for exactly that reason.

  **What makes this hard to grep, stated so the next person does not read a thin
  search as a clean result.** Go carries `Star` as a BOOLEAN on
  `logical.AggregateCall` rather than as an expression node, so there is no
  structural mirror of `Star.java` to search for, and an inconclusive sweep of
  `expr.go` has already been run and correctly declined to assert its negative.
  The honest search is over name-DERIVING sites (`ColumnNameValue`,
  `ExplainValue`, `ProjectionColumnName`, `OutputColumnName` and their callers),
  not over a Go `Star`.

  Deliverable: the classification, plus a test for every (c) found — and if the
  answer is genuinely zero, a pinned negative saying what re-arms it.


- [ ] **CQ-79 (MED, RFC-197) — CQ-53's surviving producer mint is owned by no
  item.** · S/M · query-engine review gate
  `pkg/docscheck/field_name_decision_test.go:447` pins
  `cascades_translator.go:3598` as *"dotted: MINT. **CQ-53's surviving
  producer** — turns QOV(leg).COL into QOV(merged).\"LEG.COL\" so the FlatMap
  inner's binder can resolve the merged row by that string … this one is on the
  unnest-merge path and dies with the same work"*. Its NLJ twin was deleted (the
  re-anchor now carries Java's null-named ordinal accessor,
  `FieldValue.java:335-338`); this one was not.
  **CQ-53 is marked `- [x]`** as subsumed by CQ-67 "carrying no separate
  remainder", so the gate pins a live mint that no unchecked item claims.
  **Checked and NOT owned by CQ-68**, which is a different axis: CQ-68 is about
  94 FlatMap result values being a bare UNTYPED QOV, not about a display name
  being manufactured into a row key. Booked separately rather than folded, so
  neither item can close while the other's residue survives.
  **Booked 2026-08-01** by the B6 docs-authority pass; re-verified at
  `a1d281a63` — the pin still stands verbatim and the ratchet still totals 52.
  DONE = the unnest-merge path re-anchors by ordinal as the NLJ path already
  does, the `:3598` debt entry is DELETED (not moved), and the `dotted` bucket
  header drops accordingly.

  **RE-VERIFIED AT `041838856`, AND THE SIZING IS REFUTED. This is NOT S/M and it
  is NOT independently closable.** Three corrections, the third structural:
  - The debt entry is keyed `cascades_translator.go:3667` (test line `:454`), not
    `:3598`/`:447`. The ratchet totals **53**, not 52 (a second `boundary` entry
    arrived with #601).
  - "Its NLJ twin was deleted" is imprecise. `rebaseOuterLegRefsToMerged` is alive
    with five call sites; what `df0c73e5b` deleted is the dotted MINT inside
    `rebaseOuterLegValue`. And the ARM-1 ordinal re-anchor the booking points at
    as precedent is DEAD-IN-EFFECT on every covered surface (reached only by
    `TestRebaseOuterLegValue_OrdinalFirst`), so "as the NLJ path already does" is
    a structural template, not an exercised one.
  - **The mint cannot re-anchor by ordinal in isolation, because the row it reads
    is name-keyed BY CONSTRUCTION.** The ordinal twin already exists and is
    already taken wherever it can be: `rebaseUnnestOuterLegPredicateOrdinal`
    (`cascades_translator.go:3849`) is selected at `:3547` and `:3740` whenever
    the seed is windowed. Every surviving call of the name mint (`:3059`, `:3400`,
    `:3569`, `:3590`, `:3742`) sits in the `!seedWindowed` / `!ordinalSeed`
    arm — the NAME-MODEL seed, whose merged row carries qualified `LEG.COL` keys
    (built at `:475`). The code says so at `:3736-3738`: a positional bake against
    the name-keyed row "would strand DEEP ordinal -1". So converting the mint
    alone produces a stranded read, not a fix.

  The work is therefore to make those seeds ORDINAL, which is
  `unnestExistsSeedSafe`'s scope gate. **THE CITATION THAT USED TO SIT HERE IS
  REFUTED.** It quoted `cascades_translator.go` as stating the multi-alias branch
  is "WIRED but scope-gated OFF end-to-end … it goes live only when that guard
  lifts (channel 2, coupled with the RULE-level below-FOD executor hoist)". That
  source comment was FALSE and has been corrected: `unnestExistsSeedSafe` ends in
  `len(outerBoundAliases(left)) == 1 || t.boxGatesFresh(left)`, so a fresh-gating
  multi-alias OUTER box is ALREADY ADMITTED and the multi-alias branch is LIVE
  (`TestMultiAliasOuterGatesOrdinal` pins it). There is no guard here awaiting a
  lift for that shape. What remains name-model is a multi-alias INNER cluster and
  a box with a buried outer-box leg, and those are deliberate declines. Any
  estimate for this item that rested on "the branch is off" must come DOWN
  accordingly. The executor coupling below is a separate claim and is not
  refuted by this correction. That is the SAME `executor.bindMergedOuterLegs` runtime
  binding-namespace widening (DIVERGENCES.md) that CQ-68 owns on the read axis.
  **CQ-79 and CQ-68 are two axes of one executor-widening piece of work.** They
  stay booked separately — the residues are genuinely different, and folding them
  would let either close while the other's survived — but neither is startable as
  a local edit, and CQ-79's `S/M` should be read as `L, gated on the executor
  widening`, sequenced WITH CQ-68 rather than before it.

  **RE-CONFIRMED AT `7335ad283` (2026-08-06), and the third correction is now
  MEASURED rather than inferred.** Nothing has touched
  `cascades_translator.go` since `041838856`, so all three corrections stand
  verbatim; the mint is live at `:3667` and its debt entry is live at test line
  `:454`. Two things are now instrument readings:
  - **The "NLJ precedent" does not run.** The leg-local bake census reports its
    `MergedReAnchor` partition VACUOUS over the whole real-FDB sqldriver corpus
    (`LEG-LOCAL BAKE CENSUS VACUOUS: … MergedReAnchor, which is 0`). So "re-anchor
    by ordinal as the NLJ path already does" points at an arm measured at zero
    calls. It is a structural template, not an exercised precedent, and any plan
    that cites it as proven-in-traffic is citing dead code.
  - **The reader it feeds is live at 2.** The leg-column provenance census reports
    dotted hits available 2 (`C.CV`, `I.QTY`), unstated 0, diverged 0 — so the
    executor's dotted arm cannot be deleted, and the acceptance condition
    "dotted hits → 0" is not reachable from the translator side.

  The blocker, stated in the plainest available form:
  `rebaseUnnestOuterLegPredicate` **takes no layout parameter at all**, while its
  ordinal twin takes two (`ordType`, `mergedType`) precisely because it needs
  them. Every surviving call site reaches it over a merged row that is BUILT
  with qualified `LEG.COL` keys at `:475` — but the *reason* differs across the
  five, and an earlier revision of this line flattened them all into "the
  `!seedWindowed` / `!ordinalSeed` arm", which is wrong for two of them:
  - `:3569`, `:3590`, `:3742` ARE that arm — an explicit `!seedWindowed` /
    `!ordinalSeed` else-branch, with the ordinal twin selected on the other side.
  - `:3059` is the else of `isChainedUnnest` (`:3034`), not a seed test at all.
    The `ordinalSeed` check lives INSIDE the chained arm (`:3048`), on the other
    side of the branch this call sits on.
  - `:3400` is the plain non-chained unnest merge, which likewise applies no
    seed test.

  The conclusion is unchanged, and it is the two untested sites that make it
  stronger rather than weaker: `:3059` and `:3400` are the name-keyed rebase's
  ONLY domain (`:3396-3398` says so), so they do not convert when a seed test
  flips — there is no seed test on them to flip. The ordinal twin is already
  selected wherever a windowed seed makes it correct. So this is not a mint that
  someone forgot to convert; it is the name-model arm's rebase, and it converts
  when the seed does. **STOP confirmed — do not attempt this as a local
  re-anchor.**

  **The successor is booked as CQ-95** ("the name-model seed converts to the
  windowed/ordinal seed"), carrying this round's three acceptance numbers —
  `MergedReAnchor` vacuous, executor dotted hits 2, name-split population 11 with
  SPLIT-QUALIFIED 0 — as its baseline, and the CQ-95/CQ-68 coupling. CQ-79 is the
  residue CQ-95 retires; it does not close on its own.


- [ ] **CQ-94 (MED, instrumentation): the DARK SPLITTERS — four sites recover a
  qualifier from a rendered name and NOTHING counts them.** The name-split
  census (`values/name_split_census.go`) reports SPLIT-QUALIFIED 0 over 11 calls,
  and that reading is worth exactly its scope: it watches the TWO leg bakers in
  `cascades_translator.go` and nothing else. Read as a statement about
  re-splitting in Go it is false, and the sites it misses include one that is
  strictly worse than anything it measures. Booked separately from CQ-52 because
  this is a NEW measured front, not a widening of that one — the `parseColRef`
  family in particular is a different package, a different call shape, and a
  different question (a display/lookup helper that three call sites have quietly
  promoted into a resolution decision).

  The sites, each with what it manufactures:
  - `cascades_translator.go:9547` `recursiveRemapValues` — **the worst one.** It
    does not manufacture a qualifier STRING to look up in a leg table; it
    manufactures a `CorrelationIdentifier` directly out of the bytes before the
    first dot, and hands it to a `QuantifiedObjectValue`. Its own header (~`:9515`)
    already admits the break — the lazy dotted `Field` "spells both the qualified
    `B.ID` and a QUOTED identifier containing a dot" — and notes master's
    unconditional split breaks the quoted class identically.
  - `core/embedded/colref.go:18` `parseColRef` — a LAST-dot split with **27
    production call sites**. Most are display or lookup and are not decisions;
    three MANUFACTURE a qualifier that then decides something:
    `logical_predicate.go:10329` (inner- vs outer-scoping of a projection field),
    `cascades_generator.go:6376` (a projected column's qualifier matched against
    the scan's name/alias), and `:4476` (the display label, guarded by the
    PARENTHESIS HEURISTIC — at the time of writing `colref.go:41-47`,
    `isPlainQualifiedColumnReference`, which rejects on `()` because
    "parentheses identify the rendered aggregate/function label at issue", a
    heuristic over a rendering rather than a parse. STALE AS WRITTEN: that
    function no longer exists. RFC-237 moved the parenthesis awareness INTO
    `parseColRef` itself, which now matches paren pairs and splits at the last
    depth-0 dot — so the concern is unchanged (it is still a heuristic over a
    flat rendering, with two stated limits pinned in `colref_split_test.go`) but
    the named site is gone.
  - `cascades_translator.go:5016` `splitQualifier` — the EXISTS fold's LAST-dot
    split. Its own doc concedes the deeper case: `A.B.C` is treated as qualifier
    `A.B`, column `C`.
  - `derived_unnest.go:107` — a LAST-dot split of the unnest body's source column,
    compared against the base scan's alias/table name.

  NOT in scope, listed so it is not re-counted: `unnest_gather.go:391` splits too,
  but it IS instrumented — by the SEED-WINDOW READER census's QUALIFIED-NO-IDENTITY
  hard zero, a different instrument with a different population.

  Order of work: INSTRUMENT before converting, exactly as CQ-52's first deliverable
  was a counter. Every conversion argument on this path has so far been made against
  scratch figures and then refuted by the first real measurement — the "110 → 3 → 0"
  progression turned out to be a population of 11, and the leg-column provenance
  block's "calls 52, dotted hits 4" was low by 50x on one number and HIGH by two on
  the one the decision rested on.

  DONE = each of the four sites records per resolution decision into a census with
  a standing assertion in the sqldriver `TestMain` (same shape as
  `AssertNameSplitCensus`: per-site, per-class, floors declared per site with any
  zero-population site's disposition stated and checked in the stale direction);
  the recorder wiring is pinned per arm and per class by unit test, since a site
  measuring 0 cannot have its wiring guarded by the corpus; and the measurement is
  written into this item, replacing any prose figure it contradicts.

  **MEASURED (`values/qualifier_recovery_census.go`, six sites — the parseColRef
  family is three DIFFERENT decisions with three different counterparties).
  THREE corpora, and the third one is the finding.** Classes answer "was the
  identity already in hand?": CARRIED (structured identity decided, nothing
  sliced), AGREED (split ran, identity in hand, agrees — CONVERSION-READY),
  DIVERGED (split contradicts the identity — a wrong answer, asserted ZERO),
  MANUFACTURED (split ran with NO counterparty — the hard debt, not convertible
  by a local edit), LEAF-ONLY, BARE, HEURISTIC-DECLINE.

  | site | sqldriver real-FDB | translator corpus | embedded corpus |
  |---|---|---|---|
  | recursiveRemap | 101: leafOnly 2, bare 99 | 13: carried 4, **MANUF 3**, leafOnly 2, bare 4 | 2: bare 2 |
  | existsSortSplit | 44: **AGREED 44** | 5 (fixture) | 0 |
  | derivedUnnestSource | 13: bare 13 | 4 (fixture) | 8: **AGREED 1**, bare 7 |
  | projScopeClassify | 71: carried 11, bare 60 | 0 (structural) | 18 = prod 15 (bare 15) + fixture 3 (carried/MANUF/bare) |
  | projQualVsScan | 4: bare 4 | 0 (structural) | 4 = prod 0 + fixture 4 (AGREED/DIVERGED/MANUF/bare) |
  | displayLabelStrip | 750: **AGREED 722**, MANUF 6, bare 22 | 0 (structural) | 11 = **prod 6 (MANUF 3, bare 2, heurDecline 1)** + fixture 5 |

  The `+ fixture N` splits are not bookkeeping. A fixture call proves the RECORDER
  is wired; it cannot prove the SITE is reached by anything real, and a merged
  total lets the second claim ride on the first. `projQualVsScan`'s entire
  embedded population is its own pins — production reaches that site zero times
  on every corpus — and reading its 4 as coverage is exactly the error this
  column exists to prevent.

  **DIVERGED is 0 in PRODUCTION traffic at every site on every corpus.** That is
  the one zero this census asserts, and unlike the population zeros it is a zero
  over a real population — but the population is smaller than the headline total
  and the difference is structural, not rounding. Of the 767 calls that
  manufactured a qualifier with an identity in hand, **44 are `existsSortSplit`
  and CANNOT disagree**: `sortKeyFieldRef` renders `LEG.COL` out of the very
  `FieldValue{Field, Child:QOV}` that `sortKeyQualifierIdentity` reads the
  identity back out of one call later, so the split re-parses a string joined
  from its own counterparty. Their AGREED is a tautology — worth counting,
  because it is what makes the round trip measurable and it IS that site's
  conversion answer, but it is not evidence anything survived a lossy rendering.
  **The real-population zero is ~723**, and `displayLabelStrip`'s 722 is
  essentially all of it: 722 machinery-minted display labels sliced at a dot and
  checked against the correlation the alias was minted from, none disagreeing.
  That is the genuine core result, and it is one site's.

  **THE THIRD CORPUS REFUTED TWO READINGS THE FIRST TWO AGREED ON**, which is
  this workstream's pattern holding for the fourth time. `core/embedded` had no
  census gate; it now has one, and a panic probe at its reach immediately broke
  two conclusions the sqldriver+translator pair supported:
  - `derivedUnnestSource`'s DOTTED arm reads bare 13 / dotted 0 over the SQL
    corpus. It is LIVE here — `TestDerivedUnnest_QualifiedPassthrough` drives
    `TD.ARR`, and the slot's triple AGREES.
  - the display-label PARENTHESIS HEURISTIC (`isPlainQualifiedColumnReference`,
    `colref.go`) reads 0 over 750 SQL-corpus calls. It FIRES here, on the
    aggregate label `MAX(E.SALARY)` — without the guard that label's display name
    is stripped to `SALARY)`. STALE NAME: the function is gone as of RFC-237,
    which folded the same protection into `parseColRef`. The `SALARY)` failure
    it describes is real and is the `AMOUNT)` defect that RFC-237 §10 fixed.

    **What the census did NOT do is refute a deletion, and the first writing of
    this bullet claimed it did.** Deleting the guard was already impossible:
    `logical_builder_test.go`'s
    `TestDeriveProjectionColumnDefDoesNotDequalifyExpressionLabel` pins
    `MAX(E.SALARY)` → label `MAX(E.SALARY)` on master, and has since before this
    census existed. So the "0 over 750 calls" reading could never have LICENSED a
    deletion; it could only have made one look harmless right up until that test
    went red.

    The census's actual contribution is the one it is built for: measuring the
    guard LIVE on production traffic, 2 heuristic declines including
    `MAX(E.SALARY)`, where before there was a number that said "never reached".
    And the corpus-blindness lesson survives intact and is the durable part — two
    corpora agreeing is one piece of evidence when both are blind in the same
    direction, and a zero is a fact about the CORPUS until some corpus that could
    contradict it has run.

  **CONVERSION-READINESS, per site:**
  - `existsSortSplit` — **MECHANICAL, and the cleanest conversion on this path.**
    44/44 AGREED, zero MANUFACTURED. It is a pure ROUND TRIP: `sortKeyFieldRef`
    RENDERS `LEG.COL` out of a `FieldValue{Field, Child:QOV}` and `splitQualifier`
    slices that rendering straight back apart. The identity was never lost. Not
    converted this round — query-engine change, needs RFC + Graefe ACK.

    **CONSTRAINT THE RFC MUST CARRY: the conversion removes the SPLIT, not the
    RENDER.** The rendered `LEG.COL` string is not merely an intermediate the
    split consumes — it is also the RFC-141 hidden-sort-column output-naming
    contract. `collectExtraSortColumns` names each appended remainingOrderBy
    column by `sortKeyFieldRef(k)`, the qualified rendering, precisely so it
    cannot collide with an output alias, and `resolveKeyName` returns that same
    `up` for the join arm. Convert the split to read the structured identity and
    the render must STAY, producing the identical bytes, or the hidden column
    changes name and the fold's output shape changes with it. An RFC that
    proposes "replace the join-and-reparse with the correlation" and stops there
    is proposing a wire-visible rename it has not noticed.
  - `derivedUnnestSource` — looks mechanical (1/1 AGREED, no production
    MANUFACTURED) but the population is ONE. Under-measured, not ready.
  - `displayLabelStrip` — **NOT mechanical despite 722 agreements.** There is a
    real no-counterparty population: MANUF 6 (sqldriver) plus 3 more from
    embedded production traffic, spelled `E.SALARY` and `E.SAL-ARY`. And its
    paren guard is load-bearing.
  - `recursiveRemap` — **PRODUCER CHANGE, not a local edit**, and the census
    confirms it is the worst of the four. Its input is `[]string`; there is no
    counterparty at the site to convert TO, which is why its MANUFACTURED bucket
    (3, translator corpus, witness `B.ID`) can only be retired by teaching the
    producer to carry segments. Its LEAF-ONLY witness `"(B.ID#0 + 1)"` shows a
    COMPUTED rendering being leaf-split — harmless only because an ordinal
    decides the read.
  - `projScopeClassify` / `projQualVsScan` — their manufacturing arms are
    unreached by production traffic on all three corpora, and a panic probe over
    the whole embedded suite and the whole sqldriver suite did not fire either.
    **NOT a deletion warrant**: that reach is not the full `./pkg/relational/...`
    + `./pkg/recordlayer/query/...` the warrant rule requires — the full run could
    not be completed (the 32G `/tmp` tmpfs the Go build uses for `$WORK` exhausts
    and the key packages fail to link). Stated as unmeasured rather than assumed.
    Note also that projQualVsScan's arm RAISES `ErrCodeUndefinedColumn`; its
    silence most likely means semantic analysis rejects the shape earlier, which
    makes it dead code rather than an unnecessary check.


- [ ] **CQ-95 — RE-SCOPED AND SPLIT BY MEASUREMENT (RFC-212). It is no longer
  "the name-model seed converts to the windowed/ordinal seed"; on this corpus
  that seed has ALREADY converted, and the item's real content is two other
  things.** The old framing is kept below only where it is refuted, so the
  refutations are readable against what they refute.

  RFC-212 (`rfcs/212-the-name-model-seed-is-not-what-feeds-the-dotted-readers.md`)
  instrumented the mint per call site and per branch ARM. The reading that
  re-scopes this item, whole real-FDB sqldriver corpus, uncached:

  - the three sites a lifted seed gate would convert are **reached** — buried 10,
    joinPredicate 165, chained 33 — and the **name arm is 0 on all three**. They
    take the ordinal twin, the plan-time bake, or the leg-relative fall-through.
    So the conversion this item books is not blocked; on this corpus it has
    already happened on the other side of each branch.
  - the two sites that DO reach the name mint apply no seed test at all (23 + 5
    calls) and mint **zero** qualified names.
  - the executor dotted reader's two live hits (`C.CV`, `I.QTY`) are minted by
    `clustered_outer_scalar.go`'s correlated-scalar seed, NOT by this mint —
    pinned by a disjointness assertion in the new census.

  **A PRIOR REVISION OF THIS ENTRY ASSERTED THE OPPOSITE OF THE FIRST BULLET**
  ("the three sites are DARK"), on a reading taken with the census counter placed
  BELOW the function's inert guard, where every site reads 0. That ordering bug is
  now AST-pinned (`docscheck.TestCensusReachedCallPrecedesEveryReturn`); the two
  readings support opposite follow-ups and nothing could tell them apart.

  **WHAT IS NOW BOOKED HERE**, in priority order:
  1. **Retire `RecordTypeLeg.Name`'s EXECUTOR reader** (`rowSlotForLegColumn`'s
     dotted arm) by having the correlated-scalar seed's derived type carry its
     leg table. Buildable on master; does NOT need the executor widening. The
     preconditions are measured, not assumed — see the acceptance block below.
  2. **Retire `RecordTypeLeg.Name`'s TRANSLATOR reader** (`legWindowSlot`) in two
     parts: the ALIAS route (carry the resolver's already-selected
     `ScopeSource.CorrelationName` on `logical.ColumnRef`; 98+3 of 106 calls) and
     the TABLE-NAME route (a scope capability Go must define; 1 call). RFC-212 §4.
  3. **The seed conversion / `rebaseUnnestOuterLegPredicate` deletion is
     DEMOTED**, not scheduled. It has no measured subject. It also carries NO
     deletion warrant: the three branch points are reached, so "unreached name
     arm over one corpus" is not a licence to delete the function.

  **ENTRY CONDITIONS — RESTATED. The old ones are refuted or unsatisfiable:**
  - ~~`unnestExistsSeedSafe`'s scope gate lifts~~ — item 1 above does not depend
    on it, and for item 3 the gate is not what is holding anything: the arms it
    would flip already run ordinal.
  - ~~"they SEQUENCE together, CQ-68 first"~~ — **UNSATISFIABLE AS WRITTEN and
    removed.** A gate on another item's *completion* can never be discharged if
    that item does not complete by being done. Items 1 and 2 are ungated. Item 3
    stays coupled to the executor widening, but as a COUPLING to be re-derived
    when that widening lands, not as a precondition on starting this item.
  - Query-engine review gate: RFC + Graefe ACK before implementation. **STANDS.**

  The original entry conditions, preserved because two of them are what the
  measurement refuted:
  - `unnestExistsSeedSafe`'s scope gate lifts. **ALSO REFUTED, on the same
    measurement as the copy higher in this file.** The supporting quote —
    `cascades_translator.go` stating the multi-alias branch is "WIRED but
    scope-gated OFF end-to-end … it goes live only when that guard lifts" — was a
    false source comment, now corrected. The gate's terminal disjunct is
    `len(outerBoundAliases(left)) == 1 || t.boxGatesFresh(left)`; a fresh-gating
    multi-alias OUTER box is admitted today and the branch is LIVE. So this
    condition is not "unlifted", it is ALREADY SATISFIED for the shape it names,
    and it cannot be carried as a blocker.
  - The same `executor.bindMergedOuterLegs` runtime binding-namespace widening that
    CQ-68 owns on the read axis. **CQ-95 and CQ-68 are two axes of one executor
    widening; CQ-79 is the residue CQ-95 retires.** They stay booked separately —
    the residues are genuinely different and folding them lets either close while
    the other's survives — but they SEQUENCE together, CQ-68 first.

    **THIS CONDITION IS NOW UNSATISFIABLE AS WRITTEN, and CQ-95 must not wait on
    it.** CQ-68 was run and its premise REFUTED: the 102 open firings are not
    untyped, typing converts none of them, and the item has no conversion to
    perform. So "CQ-68 first" cannot be satisfied by CQ-68 completing — there is
    nothing for it to complete. What CQ-95 actually inherits is:
    - **The read axis is NOT advanced and will not be by CQ-68.**
      `LegLocalBakeCensus` still reports every read taking the leg-alias
      pass-through and `MergedReAnchor` is still 0 — unchanged, because only
      instruments were added. (The denominator has since grown with the corpus:
      it read 174/174 when this was written and measures **190/190** on the
      current tree. The `0` is the claim; the denominator is not.)
      CQ-95's acceptance signal (`MergedReAnchor` stops being vacuous)
      is therefore still entirely CQ-95's to produce, exactly as its own entry
      says. Nothing here may be claimed against it.
    - **Two standing instruments CQ-95 can use rather than rebuild.** The
      FlatMap producer census attributes any declined leg to its constructing
      site by result-value identity, and the outcome census's witness now spells
      the flowed TYPE (arity), not a boolean. If CQ-95's seed conversion changes
      which site produces a leg, or changes a leg's row shape, both move
      visibly and both are asserted.
    - **The real conversion target, now named.** The residue converts by giving
      `buildCorrelatedFlatMapPlan`'s accumulated-inner leg a positional-merge
      result value in place of the identity QOV — a shape change, not a typing
      change. That is adjacent to CQ-95's own seed conversion and the two should
      be scoped together in CQ-95's RFC rather than sequenced behind a dead item.
    - **CQ-68'S SECOND DELIVERABLE LANDED, AND ITS ANSWER IS WORSE THAN THE
      HYPOTHESIS. This is the single most important number CQ-95 inherits.** The
      deliverable was the reachability of `correlatedStep1 && ordinalWindows !=
      nil` at `implementJoinWithExistential`'s layout read — the wall the
      conversion contacts, because `:4124`'s mint and its FlatMap construction
      run on BOTH arms (the `correlatedStep1` block only selects `step1Expr`), so
      a positionable leg result value produces a baked ordinal where a name-keyed
      row context raises `values.BakedNameContextError`. That arm carries a
      documented two-revert history. CQ-68's own re-verification predicted the
      conjunction would be "structurally REACHABLE (not dead)".

      **Measured over the whole real-FDB corpus, three consecutive uncached
      runs, the census line verbatim:**

      ```
      correlatedStep1 firings WITH a merged layout     108 of 108
      ```

      Not occasionally reachable — UNIVERSAL. Every correlated firing arrives at the layout read
      with a merged layout already derived, because on that arm `step1RV` is
      `sel.GetResultValue()` handed back unchanged and it is already a pristine
      ordinal seed. **The conversion meets the wall on day one, on 100% of the
      correlated population, not on some future corpus shape.** Scope the
      `BakedNameContextError` handling as day-one work in CQ-95's RFC, not as a
      contingency.

      The counter is standing, not a probe:
      `foldStep1SeedCounters.CorrelatedStep1WithWindows` over
      `CorrelatedStep1Firings`, printed by `FormatFoldStep1SeedCensus` and with
      the DENOMINATOR floored in the sqldriver harness
      (`FoldStep1SeedGates.CorrelatedStep1FiringsFloor`). The numerator is
      deliberately ungated: at 100% the only movement available is a DROP, and a
      drop is a finding to read (the corpus moved, or the layout stopped being
      derived on that arm) rather than a regression to block.

      **Two copies of this number shipped saying the opposite and are corrected.**
      The floor's own doc comment and the "measured shape" test fixture both read
      the numerator as ZERO. Neither was ever run: both were drafted from the
      reasoning that the correlated wall leaves nothing positioned, written before
      the counter's first execution, and not revisited when the run came back
      108 of 108. Recorded rather than quietly fixed because it is exactly the
      failure this census family exists to prevent — a prediction shipped in the
      voice of a measurement — committed inside the census.
    - **Two live Java divergences are booked OUT of this item** so CQ-95 does not
      inherit them by proximity: **CQ-96** (the translator mints an untyped
      `QuantifiedObjectValue` as a select result value — 1,086 measured, and the
      previous attribution to `implementExistentialSelect`/`yieldExistsFlatMap`
      was a count of couriers) and **CQ-97** (`RecordQueryFlatMapPlan.GetResultType`
      returns `UnknownType` where Java derives it, with `planBuriedLegConcat`'s
      plan walk standing as the documented workaround). Neither moves this
      item's 102 or DIVERGENCES.md's read count by a single unit.
  - Query-engine review gate: RFC + Graefe ACK before implementation.

  **ACCEPTANCE INSTRUMENTS, with this round's numbers as the baseline** — all three
  are already measured, and two of them refute acceptance conditions an earlier
  plan would have written:
  - **`MergedReAnchor` is VACUOUS** — the leg-local bake census reports 0 calls for
    that partition over the whole real-FDB sqldriver corpus. So the NLJ ordinal
    re-anchor is a structural template, NOT an exercised precedent: this conversion
    may not cite it as proven-in-traffic, and "do it as the NLJ path already does"
    is a description of dead code. If the conversion is right, this partition
    stops being vacuous — that is the acceptance signal, and it is the one the
    census's own vacuity assertion will announce.
  - **REFUTED BY RFC-212 — `MergedReAnchor` vacuity is not this item's acceptance
    signal.** It was booked as "if the conversion is right, this partition stops
    being vacuous". The conversion has no measured subject (the name arm is 0 on
    every reached branch because the ordinal side already runs), so the partition
    would stay vacuous through a correct outcome. Kept as a standing instrument,
    dropped as an acceptance condition.
  - **Executor dotted hits = 2** (`C.CV`, `I.QTY`; unstated 0, diverged 0, stable
    across five full-suite runs while the census's own call total swung 2394–2674
    — presence holds, multiplicity moves). That is reader one's entire remaining
    reach. An
    acceptance condition of "dotted hits → 0" is NOT reachable from the translator
    side — CQ-79 measured that — but it IS the acceptance condition here, ~~because
    this item converts the producer those 2 hits come from~~. Two, not the four an
    earlier revision of that floor block claimed.
    **REFUTED BY RFC-212 on the struck clause only: this item does NOT convert
    that producer.** The 2 hits are minted by `clustered_outer_scalar.go`'s
    correlated-scalar seed leg labels, not by `rebaseUnnestOuterLegPredicate`,
    which mints zero names over the whole corpus. "Dotted hits → 0" remains the
    acceptance condition — it just belongs to item 1 above, which is a different
    and ungated piece of work.
  - **Name-split population 11, SPLIT-QUALIFIED 0** at the two leg bakers. Reader
    two (`legWindowSlot`) is blocked at the COMPARISON, not at the channel: the
    counterparty conversion has already happened for every parsed channel, and one
    of the map's two key kinds names a TABLE rather than a quantifier
    (`matchViaTableName`, measured 1), so the map cannot be re-keyed by identity
    even in principle. This item does not unblock reader two, and must not claim to.

  ~~DONE = the seeds those five call sites sit over are ORDINAL, the name-keyed
  `rebaseUnnestOuterLegPredicate` is DELETED (not wrapped), the debt entry at
  `cascades_translator.go:3667` / test line `:454` is deleted and the ratchet drops,
  `MergedReAnchor` stops being vacuous, and the leg-column provenance dotted-hit
  count goes to 0 with its census retiring alongside the arm it measured.~~

  **DONE — RESTATED (RFC-212).** The struck list above is kept because three of
  its five clauses are refuted rather than merely superseded: the seeds ARE
  already ordinal on every reached branch, the dotted-hit clause belongs to a
  different producer, and a deletion clause cannot be discharged by a census that
  measures one corpus.

  - **Item 1 — MECHANISM WITHDRAWN, RETARGETED (RFC-212 §10).** ~~the
    correlated-scalar seed's leg table is carried on the RecordConstructorValue
    and PROPAGATED by `RecordConstructorValue.Type()`~~ — that was BUILT, measured
    INERT over two uncached corpus runs (`available 2` before and after, same two
    witnesses, nothing else moved), and REVERTED. The reader takes `qov.Type()`,
    the quantifier's own flowed type; the constructor's derived type is not on its
    path. §3.5's measurement was right that `Type()` is the sole DERIVATION path
    and the corollary that the reader takes it was wrong — derivation is not
    readership.

    **The corrected target is a RETITLING** (producer corrected again in RFC-212
    §11.3 to `scalarSubqueryOrdinalSeed`, the SINGLE-SOURCE outer seed —
    attribution measured, both witnesses), UNREVIEWED: the inner leg's
    flowed column is named by the subquery's OUTPUT TITLE
    (`scalarSubqueryOrdinalSeed`'s `innerType := &RecordType{Fields:
    [{Name: scalarCol}]}`; the same shape in `clusteredOuterOrdinalSeed` is NOT
    what the corpus drives to this reader), and a title that already contains a dot arrives at the
    dotted arm indistinguishable from a leg-qualified reference. Do NOT implement
    before its own Graefe+Torvalds lap — the target changed, so the gate applies
    again.

    **Item 1 DONE** (unchanged, and it is what refuted the old mechanism) = the
    leg-column provenance census reports `dotted HITS by identity availability:
    available 0` with `Calls` above its floor over an uncached full-corpus run.
    Whether `C.CV` retires with the same change as `I.QTY` is to be MEASURED, not
    assumed — assuming it is the same corollary error.
  - **Item 2 DONE** = `AssertDottedLegQualifierCensus` reports `flatColumnBake`
    and `legQOVBake` at zero matched calls, in two steps whose populations are
    stated separately (98+3 alias route, 1 table-name route).
  - **Item 3 DONE** = not defined here. It gets a fresh acceptance statement when
    the executor widening lands and the arm readings are re-taken; the old one
    rested on numbers this round refuted.
  - `RecordTypeLeg.Name` goes when items 1 and 2 are both done. The debt entry at
    `cascades_translator.go:3711` (was `:3667`; the line moved with the census
    site parameter) does NOT drop with item 1 — it is the mint's entry, and the
    mint survives item 1 untouched.


- [ ] **Nine `SourceRelativeBaked()` call sites perform an arity decision that the accessor-arity census cannot classify — they are now ENUMERATED but not CLASSIFIED.**
  `FieldValue.SourceRelativeBaked()` requires `len(Accessors) == 1`, and that
  requirement lives inside the PREDICATE'S NAME. A site gating on it therefore
  makes an arity decision while containing no `len(...Accessors)` expression, so
  the arity sweep — a regexp over source — cannot see it. That is not
  hypothetical: `bakeUnnestElementRefOrdinal` sat in exactly that hole with a
  LIVE defect (a struct-element MEMBER reference was skipped, mis-resolved, and
  EXISTS dropped every row SILENTLY) while `arityLiveDefect: 0` read green.
  **The direction is the danger: such a site is invisible while broken and
  becomes visible only by being FIXED**, because the repair is what introduces
  the explicit arity expression.
  ENUMERATION IS DONE and is guarded —
  `TestSourceRelativeBakedSitesAreVisibleToTheCensus`
  (`pkg/docscheck/source_relative_baked_visibility_test.go`) requires every call
  site to be CLASSIFIED in `accessorAritySites` or LEGIBLE by a comment quoting
  `len(Accessors) == 1`, fails on any site that is neither, and names GROWTH as
  the alarm direction. It found 8 unguarded; all now carry a comment. **A
  legibility comment states a FACT, never a verdict** — the classification below
  is what is still owed. 10 sites total: 1 classified, 9 legible-only.
  THE SHAPE TO PATTERN-MATCH, which is what makes this cheap for the next reader:
  `if !isFV || (fv.Resolved != nil && !fv.SourceRelativeBaked()) { return node }`
  — a SKIP that silently passes a multi-accessor unpinned reference through
  unbaked. Not every site below has it, and the differences are the point:
  · **`clustered_outer_scalar.go:183`** (`clusterPullUp.bake`) — the skip shape
    exactly. UNMEASURED.
  · **`clustered_outer_scalar.go:391`** (`collectClusterOuterRefs`) — the skip
    shape, but this walk COUNTS rather than bakes, and its own comment says the
    refs "must be COUNTED or the decline guard misses them". So the consequence
    is not a bad read but a MISSED DECLINE, which is the fail-open direction.
    UNMEASURED, and the highest-value one to look at first for that reason.
  · **`clustered_outer_scalar.go:660`** (`bakeClusterLegRefs`) — the skip shape.
    UNMEASURED.
  · **`unnest_gather.go:373`** (`bakeGatheredGroupValue`) — the skip shape, and
    the closest sibling of the fixed defect: same gate, same
    `elementSlots map[string]int`, same name-keyed `fv.Field` lookup, and its own
    doc records a prior mis-bake to the element slot. **MEASURED, DOES NOT
    REPRODUCE** across six shapes (two-level GROUP BY, HAVING, member in an
    aggregated position, and a grouped unnest with the member correlated into an
    EXISTS) — pinned by `TestFDB_UnnestElementMemberInGather`, whose doc states
    what it does NOT establish: those shapes are correct, the gate still carries
    the narrow predicate, and the reason they miss it is a property of how they
    lower rather than a guarantee anyone stated.
  · **`exists_gathered_cluster_wrap.go:159`** (`rebaseLegRefsToBox`) — the skip
    shape, but here the decline is DELIBERATE and already pinned by
    `TestRebaseLegRefsToBox_DeclinesANestedDescent`, with the surrounding comment
    arguing that a decline costs the ordinal wrap while a half-widening costs
    rows. Most likely already class (a); it needs the verdict recorded, not an
    investigation.
  · **`exists_gathered_cluster_wrap.go:343`** (`wrapRVFullyBaked`) — **INVERTED
    relative to every site above**: `if nv.Resolved == nil || nv.SourceRelativeBaked()`
    declines the SINGLE-accessor case, so a multi-accessor unpinned reference is
    NOT declined and is treated as build-evaluable. The other sites risk skipping
    a nested reference; this one risks ADMITTING one. UNMEASURED, and it is the
    site whose failure mode is least like the others.
  · **`rule_implement_nested_loop_join.go:4795`** (`correlatedFastPathOperand`)
    and **`plan_visitor.go:1935`** (`resolveBaked`) — ADMIT gates, not skips: the
    predicate admits only a flat reference, so a nested descent falls to the
    general path or fails to resolve. Consequence is a lost fast path or a loud
    miss, not a silent wrong read. Lowest risk; still owed a verdict.
  DONE = each of the nine carries a class and a reason in `accessorAritySites`,
  with the three unmeasured skip sites and the inverted one either reproduced
  (fix + row-asserting pin) or pinned as negative results the way
  `unnest_gather.go:373` was. Do NOT discharge this by deleting the legibility
  comments — that re-hides the sites.


- [ ] **The accessor-arity census records ONE class per SYMBOL, but a symbol can hold expressions of different classes — and now demonstrably does.**
  `accessorAritySites` is keyed `file#symbol` with a single `class` plus an
  `exprs` count. `exprs` works: adding an arity expression to an already-classified
  function fails the census and forces re-classification (measured — it did
  exactly that for `rewriteUnnestPredicate`). What is NOT captured is the class of
  the ADDED expression.
  `pkg/relational/core/query/cascades_translator.go#rewriteUnnestPredicate` now
  holds TWO expressions of OPPOSITE class under a single `(a)` label: the
  element-substitution arm's correct DECLINE (a), and the member-rebase arm which
  HANDLES a multi-accessor path (c). The second is recorded in the entry's prose
  only, so the class TALLY (`arityNestingOK` etc.) no longer describes the
  expressions it is counted from — it describes symbols.
  PRE-EXISTING, not introduced here: 10 symbols already carry `exprs > 1`
  (`grep -oE 'exprs: [0-9]+' pkg/docscheck/accessor_arity_census_test.go`), and
  whether any of the other nine is mixed-class has not been audited.
  `rewriteUnnestPredicate` is simply the first KNOWN mixed one.
  DELIBERATELY NOT FIXED IN THE CHANGE THAT FOUND IT: reworking the census's class
  model inside a wrong-rows fix would bury an instrument defect inside a behaviour
  change, and the two want separate review.
  DONE = either the class moves per-expression (`classes []accessorArityClass`
  matching `exprs`, so the tally counts expressions), or the census states
  explicitly that its tally is symbol-granular and the per-expression verdicts
  live in prose — and the nine other multi-expression symbols get audited for
  mixed class either way.

---


- [ ] **RFC-197 field-name debt: the `dotted` ordering constraint was refuted, and
  the RFC's own census had rotted.** Booked as one block; three findings, two of
  them corrections to things that were written down and believed.

  **(1) The wrong-rows hazard that gated the whole migration is CLOSED and
  PINNED.** `knownFieldDecisionDebt`'s `groupByOutputBaker # a map key … # 3`
  entry recorded that `groupByOutputOrdinals`' map is last-wins
  (`keys[full] = ord`), so two group keys sharing a leaf collapse to one slot,
  and that they were separated *only* because the name channel still carried the
  qualifier — i.e. that retiring `dotted:` would ARM a wrong-rows bug. That was
  the stated reason to sequence `dotted` last. **It is no longer true.**
  `groupKeyOrdinalByStructure` (`pkg/relational/core/query/cascades_translator.go`)
  decides WHICH key a reference is from the two Values (`SameColumnPath` +
  `sameQuantifierRoot`) and overrides the last-wins slot at both baker arms and at
  the ORDER BY consumer `sortKeyAggregateOutputSlot`. It is already pinned in the
  POST-DOTTED state:
  `pkg/relational/core/query/group_key_structural_ordinal_test.go` builds the maps
  with the qualified aliases `O.K`/`I.K` deliberately ABSENT. Three disjoint
  mutations, each reddening only its own arm (`sort_key_arm`,
  `qualifier_stripped_arm`, `direct_leaf_arm`), each confirmed landed via
  `git diff --stat` before running. A refuted blocker must LOWER the estimate for
  the dotted representation change, not leave it standing.
  **Residual, falsifiable:** the structural decider declines when either side has
  `Resolved == nil`, and on a decline last-wins is the sole decider again. The
  entry retires when every reference reaching `groupByOutputBaker` carries a
  non-nil `Resolved`. Falsify by finding a lazy `FieldValue` arriving at the site.

  **(2) The `dotted`-as-keystone reading is wrong at bucket granularity.** Two
  edges run BACKWARDS — a `dotted:` site downstream of another bucket's producer:
  `AccessorNamePath` is downstream of `explainValueOrdinals` (`contract:`) via
  `plans/ordering.go:985` (fixing that single producer collapsed the lazy-render
  mint class to zero and cut the arm's declines to a lone witness, touching
  nothing in `dotted:`); and `clusterFieldResolvable` / `clusterSeedSlotByName`
  compare against strings minted in the `translator:` bucket, by
  `logical_predicate.go`'s `projCol{name: qual + "." + bare}`. **THE
  "SURVIVING SUB-CHANNEL" RESCUE OF THE RETRACTED CLAIM IS ITSELF REFUTED.** This
  paragraph used to end: "The claim holds for one SUB-CHANNEL, which is where the
  leverage is: killing `rebaseUnnestOuterLegPredicate`'s mint mechanically
  retires every reader of that channel — `groupByOutputBaker`'s qualification
  probe, `rebaseOuterLegValueOrdinal`'s default arm, `rebaseOuterLegValue`,
  `legRef`." That is the SAME four-reader list the RFC retracted, re-asserted
  behind a narrower quantifier, and it is wrong for the same reason: three of the
  four are `.`-probe DECLINERS that retire on `Resolved` arity rather than on a
  producer, and the fourth is the read half of a closed pair on a different
  channel whose composed key would be a two-dot `MERGED.LEG.COL` this mint never
  produces. There is no sub-channel on which the leverage survives. The
  bucket-granularity point above (two edges run BACKWARDS) stands on its own
  evidence and is untouched by this correction.

  COUNTS ARE DELIBERATELY ABSENT HERE, and that is not stylistic. `TODO.md` is a
  durable home in its own right, this entry is open and planned-from, and nothing
  gates it — so a bucket size or a decomposition copied into it is a second
  ungated copy of a fact the RFC's census table already owns, which is the exact
  rot this item exists to record. An earlier draft of this very block shipped
  four such tallies. For sizes, read the gated table in RFC-197 `## Order`; the
  dependency order and the per-group decomposition (by NAME, not by tally) live
  in that section's "Dependency order, measured".

  **(3) RFC-197's census had rotted three ways at once, and nothing checked it.**
  `TestFieldDebtBucketsArePartition` checks the group headers INSIDE
  `knownFieldDecisionDebt` and NOT the RFC's copies, which is exactly why one
  rotted and the other did not. The RFC claimed 52 escape sites over 34
  authorities against a measured 44 over 33; its own per-bucket numbers summed to
  43 rather than the 52 the same paragraph stated; and its largest named
  concentration, `AggregateResultColumnName` at "6 of 52", had retired to ZERO
  entries without the sentence moving. Fixed by making them the SAME fact rather
  than two copies: the RFC carries the census as marked markdown tables and
  `pkg/docscheck/field_debt_rfc_census_test.go` parses them and fails the build on
  drift, in BOTH directions (a wrong number, a bucket omitted, a retired
  authority still listed, or a new concentration nobody wrote down). Mutation-
  proven on two disjoint arms.

  **Method note worth keeping — the MECHANISM, deliberately without magnitudes.**
  Counting these entries with a line-oriented regex is wrong, because many wrap
  their `: {` onto a later line, so a pattern like `^\t"pkg/.*": \{` silently
  matches only the subset that happens to fit on one line. Parse the map literal
  (`go/ast`) and read each `why`; that agrees with the instrument's own escape
  total by an independent route. Likewise do not count retirement conditions by
  grepping for the `RETIREMENT CONDITION` marker — a large minority state their
  exit in other words, so the marker under-reports.

  The numbers themselves are omitted ON PURPOSE, for the same reason this entry
  gives above: the census total is the most-gated number in this workstream, and
  a copy of it here would be a second ungated home in the very entry that defines
  that as the rot. An earlier draft of this note carried three such magnitudes.
  Read them from `pkg/docscheck`.

---


### CTE main queries skip the 42702/42703 projection gates

- [ ] A main query whose FROM is a CTE gets a **nil resolver**, so every
  projection column gate is skipped and an invalid reference dies downstream as
  an opaque `0AF00: projection slot 0 has no resolved Value` — naming neither
  the column nor the CTE.

  Measured, not inferred. Instrumenting the guard in
  `PlanVisitor.visitSimpleTableBody` (`plan_visitor.go`, the block introduced by
  `if resolver != nil && sq.projCols != nil && …`) over
  `TestCTEStarBodyPublishesSQLLabels` prints `cteScopes=0` for every arm whose
  FROM is the CTE `D`, and `resolver=false` for each. With the map empty,
  `buildSelectScope`'s `addSource` falls through the CTE branch to
  `analyzer.ResolveTable("D")`, which misses because D is not a catalog table,
  `addSource` returns false and the whole resolver is nil. `logical_predicate.go`
  has a parallel scope build that DOES populate the map; the visitor path does
  not.

  Two concrete wrong codes, both pinned as arms of
  `pkg/relational/sqldriver/cte_star_output_label_test.go` — read the gap note
  above that test's function, which points back here:
  - `WITH D AS (SELECT * FROM A, B, …) SELECT D."K" FROM D` — `K` is declared by
    both legs, so this is 42702 Ambiguous, and reports 0AF00.
  - `… SELECT A."AID" FROM D` — `A` is not a source at this level, so this is a
    source-not-found "cannot be resolved", and reports 0AF00.

  Those two arms assert **0AF00 today**. That assertion is the gap, not the
  contract: fixing this makes them fail, and they must be re-armed to the real
  codes in the same change.

  Pre-existing and independent of the exact-resolution work — the same queries
  fail the same way on master. Not a wrong ANSWER (both queries are invalid and
  both are rejected); a wrong DIAGNOSIS, and a whole class of column gates not
  running. Expect populating the map to newly ENABLE gates on a broad class of
  CTE queries, so budget a full-suite lap for shapes that currently pass only
  because the gate is silent.


### `retargetUsingJoins` builds its ON predicate through SQL text

`retargetUsingJoins` (select_parser.go) re-qualifies a chained USING's ON
predicate by `fmt.Sprintf`-ing SQL text and re-parsing it with
`parser.ParseExpression`. It inherited that from the parse-time synthesis
it replaced, but it is the one place where the metadata needed for typed
construction is already in hand — so it deepens the text-round-trip debt at
precisely the site best placed to avoid it.

THE WORK: build the predicate as a typed expression instead of as text.
Also folds in the alias-quoting duplication: `quoteUsingAlias` is a verbatim
copy of the `quoteAlias` closure inside `synthesizeUsingOnExpr`, and two
copies of one identifier-normalisation rule must not be allowed to drift.


### `retargetUsingJoins` is semantic analysis living in the parser

`retargetUsingJoins` and its helpers (`usingSource`, `usingOwnerOf`,
`legSource`, `baseTableColumns`, `cteNamePredicate`) resolve a USING
column against the visible left scope, consult the semantic catalog and
derive a CTE/subquery projection. That is name resolution — what Java does
in semantic analysis — and it currently sits in `select_parser.go`.

The LAYER is right and was reviewed as such: nothing here reaches the
memo. It emits no expression, no quantifier and no cost decision; it reads
a column surface and rewrites one predicate's qualifiers before the
logical tree is built, which is the stage Cascades requires a
name-resolved tree to arrive from. What is wrong is only the FILE.

It is booked as its own entry rather than folded into the two existing
USING items — those are about the SQL-text round-trip and the duplicated
alias quoting, and neither would lead a reader to the placement question.
A finding tucked inside an entry about something else is how findings rot.

THE WORK: move the group next to the other semantic-analysis code, or
into the semantic package outright if the dependency direction allows.
No behaviour change; the yamsql arms, the JVM probes and the golden are
the safety net for the move.


### A derived source's schema is advertised before its body is validated

`buildCTEColumnSource` derives a derived-table or CTE schema by walking the
body's FROM legs BY NAME when the body is a simple star over one table. It
does not check that the star's qualifier names a source in that body, so
`(SELECT nope.* FROM c)` is advertised as exporting c's full row.

Consequence, measured against a live JVM
(`conformance/join_using_chain_java_probe_test.go`, arm "derived body
invalid, outer USING ambiguous"):

    SELECT a.id FROM a JOIN (SELECT nope.* FROM c) d USING (id)
                       JOIN c USING (k)

    java: Unknown reference NOPE     go: 42702 (the OUTER ambiguity)

Both engines refuse the query — it is invalid twice over — so this is an
error-ORDER divergence rather than a wrong answer. Java reports the fault
INSIDE the subquery, which is both earlier and more specific.

It became observable when USING ownership started consulting derived
schemas: the outer ambiguity is now computable from the advertised
(wrong) schema and is raised before the body is ever built. The advertising
itself predates that.

THE FIX belongs in the derivation, not in the USING resolver: route a
derived source's schema through the validating builder. That path already
exists — `buildExactScopeSourceOrBodyError`, which the same function uses
for join/derived-legged bodies precisely because "a body that does not
BUILD raises its OWN error instead". The simple-star branch takes the
name-walk shortcut and skips it.

It is not fixed here because it changes SHARED CTE schema derivation —
every CTE and derived table in the engine — for an error-precedence gain
on an already-invalid query. That deserves its own change and its own
review lap rather than riding along on a USING fix.

The divergence is PINNED, not merely described: the probe arm asserts BOTH
engines' current wording, so it fails if Go is repaired or if either side
moves to a third answer.

---



### What RFC-237 did NOT close (lifted out of the closed identifier-model entry)
TWO THINGS THIS DID **NOT** CLOSE, both real and both deliberately out of
scope, with their evidence:

1. **`values.AccessorNamePath` folds both sides of a plan-rule identity
   comparison** (`accessor_name_path.go:67,84,302`). It is self-consistent, so
   it is not a disagreement — but it equates two genuinely distinct quoted
   columns `"a"` and `"A"` inside rules like `PushFilterThroughGroupBy`, which
   can push a predicate onto the wrong column. Different mechanism (identity
   comparison, not naming), its own census and ratchet. Needs its own RFC.

2. **Go still over-resolves relative to Java**, by design: `SELECT KeepCase`,
   `SELECT "KEEPCASE"` and `SELECT "keepcase"` all answer against a column
   declared `"KeepCase"`, where Java raises 42703. Measured, and pinned as
   `goOnly` arms in `QuotedIdentifierCaseJavaProbe`.

   **ONE SHAPE MOVED FURTHER FROM JAVA, and that is the honest half of this
   entry.** The old "catalog folds" entry's own reproducer —

       SELECT q1."id" FROM q1 JOIN q2 USING ("k") JOIN q3 USING ("K")

   — had BOTH engines refusing: Java `Unknown reference K`, Go 42702. Go now
   ANSWERS `[[1]]`. No wrong rows shipped before and correct rows ship now, but
   the disagreement widened from "both refuse, differently" to "Go answers what
   Java rejects". Pinned with both engines' text in
   `JoinUsingQuotedIdentifierJavaProbe`, arm "a quoted USING must not hide the
   unquoted column". Deleting master's entry was right — its named mechanism
   (`rlcatalog`'s folded `LookupColumn`) is gone — but the divergence it
   measured is not, so it lives here.

   **AND THE REMEDIATION IT RECORDED WAS WRONG.** "Plumb
   `CASE_SENSITIVE_IDENTIFIERS`" does not close it:
   `SemanticAnalyzer.normalizeString` keeps a QUOTED string verbatim in *both*
   modes, so no Java setting makes `"K"` reach `"k"`. The real work is
   preserving the QUOTING BIT through `NormalizeIdentifier` /
   `semantic.FromNormalized`, which discard it — `FromNormalized` hard-codes
   `wasQuoted: false` and is used ~59 times on the reference path. Probed: a
   `!want.WasQuoted()` gate on the relaxed pass is INERT, because there is no
   flag left to gate on. RFC-237 §3.3.

---


### Two mechanisms build the join-leg datum keys and they disagree by case

`legColumns` (cascades_translator.go) folds the COLUMN half of a leg key to
UPPER; `logicalLegFields` (logical_result_type.go) keeps it verbatim and its
comment says why — "only the ALIAS half is folded, and only because a source
alias never comes from a descriptor". So a leg column declared `"KeepCase"` is
`C.KEEPCASE` on one route and `C.KeepCase` on the other, and a proto field
`customer_id` is `C.CUSTOMER_ID` against `C.customer_id`.

Pinned by `pkg/relational/core/query/leg_column_key_case_divergence_test.go`,
which asserts both spellings so the gap can neither widen nor silently close.

Measured, so the size of the decision is known rather than guessed: removing the
fold moves zero rows of the 2627-query plan-shape golden, and twelve targeted
shapes reaching a leg key by different routes (three-way join, CTE and derived
table over a join star, UNNEST beside one, grouped and scalar aggregates over a
quoted mixed-case leg column, correlated scalar subquery, alias-list recursive
CTE) plan byte-identically either way.

**That zero is a PLANNING measurement and nothing more.** The plan-shape golden
is generated through `embedded.PlanPhysicalForTest`, which never runs the
executor. A reading that it is MASKING — that `rowSlotForLegColumn`'s
`EqualFold` comparators hide the disagreement — was written here and is refuted
by a standing gate: `AssertLegColumnProvenanceCensus` fails the build if that
reader receives any call, because the exact-ordinal seed retired its only
driver. Those comparators are in code that does not run.

What the zero establishes is that no planned shape depends on a leg key's case.
The divergence is a latent producer disagreement, which is an argument for
collapsing the producers and not for chasing a runtime symptom that does not
exist.

**The work is not "pick a spelling" — that framing is wrong, and the reason is
that the folding line does TWO JOBS.** `tableColumns` names a scan's columns
through `ToUserIdentifier`, which un-escapes and does NOT fold, so for a
hand-authored proto field `order_id` the fold is descriptor-to-SQL
NORMALIZATION. For a DDL-declared `"KeepCase"`, already canonical from the
parse boundary, the same line is RE-normalization. That is why removing it
reddens `TestLegColumns_NestedNoSpuriousKeys` (whose `order_id` is the
normalization job) while keeping it breaks the RFC-237 invariant.

**Collapse the two producers instead**, and that is only one end of it: the
qualifier is carried as a RENDERED STRING and re-parsed downstream, which is the
mechanism behind this divergence, the `a.b` label, `colref.go`'s two KNOWN
LIMITs and the deleted `seedResolvesThroughJoin` alike. RFC-238 carries the
design, the ordered steps and the acceptance criteria that make the collapse
checkable; this entry is one of its symptoms and closes when it lands.

`TestLegColumns_NamingConsistentWithAnchoredRecord` also reddens on any change
here and is NOT evidence: it builds its expectation with `strings.ToUpper(c.Name)`
itself, mirroring the implementation, so it asserts nothing about which spelling
is right.

---


### [ ] `frl sql` meta-commands ignore identifier case, so `\d` fails where SELECT works

The engine uppercases unquoted identifiers at DDL time, so `CREATE SCHEMA
/db/main` stores the schema as `MAIN`. `frl sql --database /db --schema main`
connects fine, because the DSN path normalises — but `\d` and `\d <table>` pass
`sqlRunner.schema` through verbatim to the catalog lookup, which then reports
`42F51: schema </db/main> does not exist`. One connection, two answers.

Found while building `TestIntegration_SQL_DescribeUsesSQLIdentifiers`, which is
why that test hard-codes `MAIN` rather than the spelling an operator would
type — see the NOTE in
`cmd/frl/internal/cmd/sql_identifier_names_integration_test.go`. Not fixed
there because it is a separate defect from the namespace work that test pins,
and because the fix needs the quoted-vs-unquoted rule stated explicitly:
normalising blindly would break a schema deliberately created as `"main"`.

**When fixing:** find where the DSN path normalises (`buildFDBSQLDSN` /
`functions.StripIdentifierQuotes` in `sql.go`) and apply the SAME rule to
`r.schema`/`r.database` before the catalog lookups in `loadSchemaTables` and
`describeTable` — not a `strings.ToUpper`, which would break quoted names.

---


### [ ] `AmbiguousDeclaredNames` is CASE-SENSITIVE, so a case-folded collision is undetected

The ambiguity contract every naming gate cites tests `declared[escaped]` — a map
lookup, so it is case-sensitive. A schema declaring quoted `"MY$TABLE"` (stored
`MY__1TABLE`) alongside quoted `"my__1table"` (stored `my__01table`) is therefore
NOT reported as ambiguous: the spellings differ only in case.

**Why this is not a live safety hole:** `GetRecordType` is case-sensitive too and
never resolves one to the other, so no destructive path (`record delete --type`,
which resolves through `lookupRecordType` → `GetRecordType`) can be handed the
wrong table by it. It becomes one the moment any resolver or renderer folds case
before comparing — `frl sql`'s `\d` already uses `strings.EqualFold` on the
stored-name arm, which is why that command can describe the wrong table under
this shape.

**When fixing:** state the quoted-vs-unquoted identifier rule FIRST. The engine
uppercases unquoted identifiers at DDL time but preserves case for quoted ones,
so neither "always fold" nor "never fold" is right, and guessing breaks schemas
deliberately created lower-case. The same rule is needed by the REPL
case-normalisation item above — fix them together or neither.

Documented at the contract itself (`RecordMetaData.AmbiguousDeclaredNames`,
`pkg/recordlayer/metadata.go`), which points back here.


### [ ] Resolve an aggregate's operand AT the producer, and delete the second pass

RFC-241 stops `upgradeAggregateOperands` from matching an aggregate column to its
`agg.Calls` entry by folded rendered TEXT — a string literal's case decided the match, two
calls collided, and the second write clobbered the first, returning wrong rows. It replaces
that with a side table (`callToAggCol`) returned by `logicalAggregateCalls`, so the producer
hands over the correspondence it already knew instead of having it reconstructed downstream.

**That is a correct fix and it is not the shape Java has.** Java binds the operand AT
CONSTRUCTION: `ExpressionVisitor.visitAggregateWindowedFunction`
(`fdb-relational-core/.../visitors/ExpressionVisitor.java:352-362`) builds the argument with
`visitFunctionArg(...)` and passes the resulting `Expression` straight into
`resolveFunction(functionName, expression)`. There is no later pass, so there is nothing to
correlate and the collision class cannot exist. This repo already has one producer with that
shape — `logical_predicate.go:12211`'s `addAgg` closure resolves `opVal` in place and
`:12405` assigns the operands directly — so the target is a port of a local precedent, not an
invention.

**What blocks it, measured.** `logicalAggregateCalls` has two callers that do NOT agree on
what is in scope, and a shared producer can only do what BOTH callers permit:

- `plan_visitor.go:1538` CAN build a resolver, and does, 34 lines above at `:1504`
  (`buildSelectScope(selectQueryFromClassification(cls, fs), v.md, v.schemaName, v.cteScopes)`).
- `logical_builder.go:649` CANNOT: it sits in `buildSelectShell(op, sq, stripPrefix)`
  (`:628`), whose signature carries no `md`, no `schemaName` and no `cteScopes`.

So the work is threading catalog context into `buildSelectShell` — a signature change through
the second SQL builder — and only then moving resolution into the producer. Its two non-test
callers, `logical_builder.go:543` and `logical_predicate.go:8620`, both need the threading;
sizing this from the one call site nearest the aggregate code would under-count it.

**Why this is worth doing rather than living with the side table.** The side table is a
correspondence carried alongside the calls; once the operand is resolved at construction there
is no correspondence to carry, and RFC-241's table plus the whole
`upgradeAggregateOperands` operand loop DELETE. The side table was chosen partly because it
makes this a deletion rather than a rewrite.

Related debt this would collapse, none of which is a copy of the RFC-241 defect (RFC-237
de-folded both normalizers, and both binders' structural arms consume `AggregateOperands`, so
fixing the operands fixes what they see): `normalizeAggregateBindingName`
(`logical_predicate.go:5991`) and `normalizeAggOutputName` (`cascades_translator.go:1289`) have
byte-identical bodies; `canonicalAggName` (`:6004`) and `aggregateValueOutputName` (`:1437`)
are near-identical renderers; `aggregateCallOutputSlot` (`:5788`) and
`aggregateValueNativeOrdinal` (`:1401`) are one algorithm written twice and DIVERGING —
collect-all-then-`matches[0]` versus first-match.

Query-engine change: needs its own RFC and the Graefe + Torvalds gate before implementation.

DONE when: `logicalAggregateCalls` emits calls whose operand Value is already resolved,
`upgradeAggregateOperands`' operand loop and RFC-241's `callToAggCol` table are both deleted,
and the RFC-241 census floors are removed with them rather than left pointing at code that no
longer exists.



---

## 5. Feature reach — Java parity and ANSI gaps

Queries Java answers and Go refuses (parity work), and queries neither engine answers (ANSI gaps,
allowed as read-side extensions when wire compat holds).

### [ ] POST-RFC-173 reach extension — ordinalize the FULL-OUTER-box chained lateral-unnest straddle
RFC-173 S4 cap **loud-rejects** a chained lateral unnest whose spine bottoms in a FULL OUTER box
(`SELECT … FROM A FULL OUTER JOIN B ON …, A.arr AS X, X.sub AS Y WHERE A.id = …`, and the nested
`(A LEFT B) FULL C` variant) — see `chainedSpineBottomsInFullBox` + the reject in
`translateChainedUnnestJoin` (`rfc173_w5_chained_unnest.go`), error "lateral unnest over a FULL OUTER
JOIN with a join-leg predicate is not supported". This is **Java-aligned** (Java rejects FULL OUTER JOIN
at the grammar level — `RelationalParser.g4` `joinPart` has no FULL alternative), so it is NOT a parity
gap — it caps a **Go-only extension** rather than sink a per-leg-window composition into a shape Java
cannot express. The plain FULL-box unnest (single link) and the UNFILTERED chained FULL-box spine already
ordinalize and keep working; only the box-leg-predicate straddle + nested-outer-box bottom reject.
**To make it work** (reach beyond Java, optional): give INNER/FULL clusters per-leg window composition into
the chained ordinal seed (`boxOuterBirthsPositional`/`boxGatesFresh` are OUTER-birth only today), resolve
the box-leg conjunct through the per-leg merge window in `unnestExistsSeedSafe`, then drop the reject.
Tests pinning the reject: `TestFDB_RFC173S4_FullBoxChainedSpine` (box-leg-filter + `nestedbox_*` cases),
`TestFDB_RFC173S4_ThreeLinkFilteredOrdinalizes/fullbox_bottom_boxleg_filter` — flip those `wantReject`
cases back to row assertions when ordinalized.


### [ ] POST-RFC-173 reach extension — ordinalize the box-leg-WHERE straddle over a chained LEFT/RIGHT outer box
The nested LEFT/RIGHT outer box under a chained lateral unnest (`(A LEFT B) LEFT C, A.SARR AS X, X.SUB AS Y`)
now **ordinalizes** for the element / leg-projection / element-or-AT-WHERE / deeper-link shapes (S4-B:
`chainedSpineWalk` admits a gated LEFT/RIGHT box bottom + the `SelectMergeRule` dissolved-box barrier lets
the box physicalize). But a **box-leg WHERE conjunct** over it (`… WHERE C.ID = 110`, references only box
legs, no chain element) is **loud-rejected** (`chainedSpineBottomOuterBox` + the reject in `translateFilter`,
error "WHERE on a join-leg column of an OUTER JOIN under a chained lateral unnest is not supported"). It is
the un-ordinalizable straddle: the chained merged-corr rebase bakes onto the previous unnest alias, which
**collides** with the first link's own inner Explode quantifier (a pushed-down `ofOrdinal` binds to the
element row, not the merged seed → ordinal-(-1) strand); and baking it onto the box quantifier at the first
link lets `PushFilterBelowJoinRule` **sink it below the nested outer null-extension** into the null-supplying
scan (LEFT→INNER, silent wrong rows). The name-model residual strands at physicalization too, so there is no
correct representation today — reject (correct-or-loud) rather than ship wrong rows. **To make it work**:
inject the box-leg conjunct into the FIRST-LINK box select on a NON-colliding quantifier AND teach the
pushdown to keep a positional box-leg predicate above the nested outer null-extension (the direct
non-chained nested box already does this — its box-leg WHERE plans as `PredicatesFilter(box, [pred])` above
the box). Tests pinning the reject: `TestFDB_RFC173S4_NestedLeftBoxChained` (`chained_boxleg{A,B,C}_filter`)
— flip those `wantReject` cases to row assertions when ordinalized.


### [ ] query-engine (SHARED gap with Java, pre-existing, orthogonal to the scope leak): IN-subquery is unsupported → 0AF00
`x IN (SELECT …)` does not plan in this Cascades engine — `SELECT p.id, (p.id IN (SELECT eid FROM e)) FROM p`
0AF00s ("Cascades planner could not plan query") with NO aggregate involved, and WHERE-position
`… WHERE p.id IN (SELECT COUNT(*) FROM e)` 0AF00s too. So IN-subquery is a general unsupported feature,
NOT a scope-leak residual — the scope leak is closed (the IN case went from a misleading 42803
to this honest 0AF00). **Correction (measured against Java 4.12.11.0 source):** the earlier
"(Java supports it)" parenthetical here was WRONG — Java rejects the same grammar alternative.
`ExpressionVisitor.visitInPredicate` asserts `inList().queryExpressionBody() == null` with
`UNSUPPORTED_QUERY` ("IN predicate does not support nested SELECT"), and the earlier
`AstNormalizer.visitInPredicate` NPEs on it (`ParseHelpers.isConstant` dereferences a null
`ExpressionsContext`). So this is a SHARED gap, not a Java-parity gap: closing it would be a
net-new read-side extension (IN-subquery lowering to a semi-join), sizeable; NOT an RFC-173
blocker. Sentinel: `exists_aggregate_scope_leak_fdb_test.go` `in_subquery_scope_leak_closed` flips
when IN-subquery support lands (then assert the rows).

<details><summary>original keystone characterization (kept for the audit trail)</summary>

The parser's aggregate detection (select_parser, `sq.aggCols`/`countStar`) does NOT stop at subquery
boundaries: an aggregate inside a projected/nested subquery leaks into the ENCLOSING SELECT's aggregate set.
Two observable failures, ONE root cause:
  - **42803 on a projected EXISTS-of-aggregate**: `SELECT p.id, EXISTS (SELECT COUNT(*) FROM e WHERE
    e.eref = p.id) FROM p` → `42803: column "P.ID" must appear in the GROUP BY` — the outer `SELECT p.id …
    FROM p` is wrongly classified as an aggregate query. Confirmed independent (the UNCORRELATED form
    `SELECT p.id, EXISTS (SELECT COUNT(*) FROM e) FROM p` 42803s identically); a plain `EXISTS(SELECT 1 …)`
    projects fine. Java answers it (all-TRUE for the non-grouped-aggregate inner).
  - **misclassification that broke the EXISTS-over-aggregate fix** (codex P1 on `5d4ff3711`): `EXISTS(SELECT
    EXISTS(SELECT COUNT(*) FROM f) FROM e WHERE e.eref=p.id)` — the row-preserving middle SELECT gets a
    non-empty `sq.aggCols` from the nested COUNT(*), so any consumer trusting `sq.aggCols` (the cardinality
    fix's detector) misfires.
Fix: the aggregate-detection walk must scope to the CURRENT query (an EXISTS/scalar subquery is its own
query scope; its aggregates belong to IT). This is the sequencing PREREQUISITE for the EXISTS-over-aggregate
cardinality fix above. Query-engine/front-end → four-gate.

**CONFIRMED by reproduction 2026-07-10 (correcting an intermediate mis-call).** A salvage re-do of the
cardinality fix (with the codex P1#1 LIMIT/OFFSET guard + the P1#3 outer-only-predicate decline) was built
and tested against codex's exact shapes. Result: base cases + LIMIT-0 + mixed-predicate all PASS, but
`codex_nested_scope` (`EXISTS(SELECT EXISTS(SELECT COUNT(*) FROM f) FROM e WHERE e.eref=p.id)`) returned
`[1,2,3]` instead of `[1]` — the row-preserving middle IS misclassified as unconditionally-one-row. So
codex P1#2 is REAL (an intermediate hypothesis that it was a false positive — "checkCountStar/extractAggFunc
are structural so sq.aggCols can't leak" — was WRONG; something DOES populate the middle's aggregate set from
the nested COUNT(*), same mechanism as the projected-42803). The cardinality fix therefore CANNOT be made
sound by guarding its own detector — it is HARD-BLOCKED on this scope-leak fix. Do the scope leak FIRST
(pin down which harvest in `extractFromQueryTerm`/the SELECT-element loop descends into a projected
EXISTS/scalar element), which also fixes the projected-42803, THEN the cardinality fix. The re-do was
discarded (not committed) since it fails `codex_nested_scope`.
</details>


### [ ] dml: DELETE/UPDATE ... RETURNING silently ignored — Java supports it (divergence, found 2026-06-28)

The shared grammar carries `(RETURNING selectElements)?` on `deleteStatement` and
`updateStatement`, and **Java supports it** — `QueryVisitor.visitDeleteStatement:848` /
`visitUpdateStatement:882` build a `generateSelect` from the RETURNING selectElements
and return the affected rows as a result set. Go silently DROPS the clause: via `Query`
you hit the generic DML-via-Query guard (0A000 "INSERT/UPDATE/DELETE return a row
count, not rows"; connection.go:449) before RETURNING is ever processed; via `Exec` the
DELETE/UPDATE executes correctly but the RETURNING values never surface (count only).

NOT data loss (the DML is correct) — a Java-supported feature left unimplemented.
Fix = port Java's generateSelect-from-RETURNING (build the projection over the
deleted/updated rows) and wire a DML-returning-a-result-set through the driver Query
path (the path that currently rejects all DML with 0A000). Feature port, follow-up
scope. Pinned by returning_clause_probe_test.go (flip when implemented). INSERT
RETURNING is a 42601 — not in the INSERT grammar — so it's a separate, larger gap.

**Scope note (RFC-159 investigation):** this is a Graefe-gated **Cascades** change, not a small
fix. Java models RETURNING as a `generateSelect` (a logical SELECT / projection) wrapping the
mutation operator's output — so Go needs a `Project`-over-DML the Cascades planner can plan
(`Map`/`Project` over `RecordQueryDelete/UpdatePlan`), plus driver routing to send a DML-with-RETURNING
through the Query (rows) path rather than Exec (count). The Go DML executor already returns the
mutated rows as a cursor (`recordlayer.FromList(results)`), so the executor groundwork exists; the
work is the logical Project-over-DML + its physical wrapper + `IsUpdate()` routing. Its own RFC +
Graefe ACK.


### [ ] ddl: implement covering indexes — CREATE INDEX ... INCLUDE (cols) (Java parity; found 2026-06-28)

`CREATE INDEX ... ON t (a) INCLUDE (b)` is currently REJECTED (0A000 "INCLUDE clause
(covering index) is not yet supported", ddl.go parseIndexDefinition) — a fail-closed
stopgap for what was a SILENT divergence: Go dropped the INCLUDE clause and created a
PLAIN index, while Java (DdlVisitor.java:249 → addValueColumn) creates a COVERING
(KeyWithValue) index. Same CREATE INDEX, different index structure across engines = a
wire/DDL-portability divergence. Regression: include_clause_rejected_probe_test.go.

Go's record layer ALREADY supports covering indexes — KeyWithValueExpression
(index_maintainer.go:107/217/362, "Matches Java's KeyWithValueExpression path"). The
gap is only the SQL→metadata DDL wiring: (1) Builder.AddIndex (core/metadata/builder.go)
needs an included-columns parameter; (2) build a KeyWithValueExpression root (key cols +
value cols) instead of a plain key expression when INCLUDE is present; (3) wire
def.IncludeClause().UidList() through parseIndexDefinition (ddl.go). Flip the reject +
the sentinel when implemented. Same applies to the indexAsSelect / vector paths' INCLUDE.



### RFC-165 — ANSI SQL Core gaps (rejected in BOTH engines today)
- [ ] **E091-07 `COUNT(DISTINCT)` / DISTINCT quantifier** — rejected 0A000 in both engines. The
      single highest-value Core gap. (Pinned today as `# ansi-gap: E091-07` on `count_distinct.yaml`.)

- [ ] **E071-01 / E071-03 `UNION DISTINCT` / `EXCEPT DISTINCT`** — only `UNION ALL` works (E071-02).
      No Cascades dedup rule. (`# ansi-gap: E071-01` on `union.yaml`.)

- [ ] **E061-11 subqueries in `IN` predicate** — rejected 0AF00 in both. (`# ansi-gap: E061-11` on `subquery_in.yaml`.)

- [ ] **E021-04/05/06/08/09/11 string functions** (`CHARACTER_LENGTH`, `OCTET_LENGTH`, `SUBSTRING`,
      `UPPER`/`LOWER`, `TRIM`, `POSITION`) + **E021-07** string concatenation — no function-catalog
      entry in either engine (42883). A whole Core subfeature family.

- [ ] **E011-03 `DECIMAL`/`NUMERIC`** — no exact-decimal type; BIGINT division truncates.

- [ ] **E141-04/06/07 `FOREIGN KEY` / `CHECK` / column defaults** — only NOT NULL/UNIQUE/PK today.

**Phase 1 (continuing): tag the rest of the corpus.** Each `# ansi:`/`# ansi-gap:` tag on a yamsql
scenario moves a row from `untested` to a real status. The drift guard
(`TestAnsiLedgerEvidenceExists`) rejects a tag whose scenario lacks the matching outcome, so the
scoreboard can't lie. As Go closes a gap, flip happens automatically when the new feature's scenario
is tagged — never by hand-editing the doc.

**RFC-165 follow-ups (tracked, non-blocking):**

- [ ] **Verify the `Java?` roster facts against the live 4.12.11.0 server.** The `Java?` column in
      `ansi_roster.go` is currently a hand-authored frozen-version *assertion* (sourced from
      SQL_CONFORMANCE.md), structurally contained (it can't inflate the Go headline — see RFC-165 §4.6)
      but unverified. As A3 cross-engine coverage grows, diff each tagged feature's `Java?` against the
      conformance server so the fact becomes *verified*, not asserted, and flag any mismatch. Per
      Torvalds + Graefe review of PR #400.



### RFC-180 Y4 follow-ups — reach parity with Java
- [ ] **Grouped-select ORDER BY widening (Graefe, be9e66c62 review):** a computed
      ORDER BY key over a grouped reshaping projection that is NOT a SELECT-list
      output currently declines typed 0AF00 (`translateSort` pull-up miss). Java
      widens the select with the missing expression and re-projects
      (`LogicalOperator.generateSelect`, `remainingOrderByExpressions`). Port the
      widening for the grouped path (the EXISTS-fold path already has its own
      instance booked under RFC-141 Phase 2 FOLLOW-UP). Pin: replace
      `TestGroupedOrderBy_UnderivableKeyDeclinesTyped` with a row-level pin.

- [ ] **Grouped correlated EXISTS port (RFC-180 Y4):** Java plans
      `EXISTS(… GROUP BY … HAVING …)` (existential quantifier over a
      GroupByExpression); Go's correlated-EXISTS fallback rebuilds only
      FROM+WHERE and now declines TYPED 0AF00 (buildCorrelatedExists guard —
      before the guard it silently dropped the grouping and returned wrong
      rows, yamsql exists_with_aggregate). Port the aggregate into the
      rebuilt inner; restore the [Alice] rows pin.

- [ ] **Boolean-CASE WHERE predicate wrap (RFC-180 Y4):** Java wraps a
      boolean-typed non-BooleanValue (CASE/PickValue) used as a predicate in
      ValuePredicate(= TRUE) (Expression.java:371-400) and plans it as a
      residual filter; Go declines 0AF00. Port the wrap; restore the rows
      pins in case_when_in_java / case_exists_combo.

- [ ] **Correlated scalar subquery in HAVING — quantifier lowering
      (RFC-180 Y4, extension):** outer HAVING still rejects typed 0AF00; its
      grouped-output row shape and aggregate-reference rewrite have not been
      proven compatible with the private scalar slot. This is distinct from a
      real-aggregate HAVING *inside* a correlated scalar subquery, which CQ-4
      supports and cardinality-checks after HAVING. Restore a dedicated HAVING
      rows pin only when that outer consumer is implemented.

- [ ] **Scalar subquery over a FROM-less SELECT (RFC-180 Y4, extension):**
      `SELECT (SELECT COUNT(*) FROM t) AS total` declines 0AF00 — the
      LogicalValues path carries no subquery plans. Restore rows pin when
      wired.

- [ ] **HAVING-EXISTS error-surface alignment (RFC-180 Y4):** Java rejects
      `SELECT COUNT(*) FROM t HAVING EXISTS(…)` at semantic analysis with
      42803 GROUPING_ERROR "Invalid reference to non-grouping expression …
      exists(q…)" (LogicalOperator.generateGroupBy →
      SemanticAnalyzer.isComposableFrom; live-probed). Go declines 0AF00 via
      the HavingExistsSubqueries planner gate. Align: reject at semantic
      analysis with 42803 + Java's message shape; flip the exists.yaml pin.

- [ ] **CQ-72 (M/L per item, L in aggregate) — the measured Go-engine divergence
  ledger from the RFC-201 Phase-1 corpus run.** Every entry below was found by
  RUNNING a vendored file, not predicted; every one is pinned to its exact
  rejection in `pkg/relational/conformance/javacorpus/gaps.go`, so the pin
  breaks loudly the moment the engine's behaviour changes in either direction
  (the file starting to pass fails the gap-reachability check; a DIFFERENT
  failure at the same path stays a hard failure). Counts are file counts.

  - [x] **array literal in `INSERT … VALUES` (was 6 files) — CLOSED.**
    `ConvertToProtoValue` (and the executor's `goToProtoValue`, for UPDATE)
    convert repeated fields element-wise; `walkArrayConstructor` builds
    Java's `LightArrayConstructorValue` shape (MaximumType fold +
    per-element PromoteValue injection) instead of the K-NN-only
    `[]float64`; `CAST(x AS <type> ARRAY)` keeps its ARRAY suffix and
    `CastValue` gained Java's ARRAY_TO_ARRAY element-wise arm — which
    closed `engine-gap:cast-array-literal` too. Pinned by
    `TestFDB_ArrayLiteralInsertValues` / `TestFDB_ArrayLiteralInsertWireBytes`
    (sqldriver) and the corpus (pass 32→34: `array-column.yamsql`,
    `cast-documentation-queries.yamsql`). The four other files progressed to
    DISTINCT next gaps, re-booked at their measured rejections in gaps.go:
    array comparison (`SELECT [1] = [1]` → NULL, arrays-operators), array
    subscript under CAST (0AF00, cast-tests), JOIN mixed into
    comma-separated FROM (right-deep-plan-tests), DML RETURNING result set
    (prepared). ~~NOTE: Go stores a nullable array as a PLAIN repeated field
    (RFC-143 §3a divergence) where Java wraps it in a `values` wrapper
    message — so `[]` and NULL collapse on the Go wire, and the stored bytes
    for an array column are NOT Java-interoperable until §3a closes.~~
    **STALE — §3a CLOSED by `ba5f78958` (RFC-204 P1+P2+P3, #601); struck
    2026-08-05.** The wrapper is emitted (`core/metadata/builder.go:929-937`),
    `[]` and NULL are distinct on read-back
    (`array_literal_insert_fdb_test.go:143-156`, `:200-207`), and the wire
    bytes are pinned byte-equal (`:217`). See the §3a follow-up above, already
    marked done.
  - **catalog system tables (2)** — `select … from "TEMPLATES"` / `schemas`
    from a user connection: `0AF00: no schema metadata available`.
  - **width-suffixed integer literals `1I` / `2L` (1)** — the constant folder
    passes the whole token to `strconv.ParseInt`.
  - **`__ROW_VERSION` pseudo-column (1)** — not exposed to name resolution.
  - **table-valued function in FROM (1)**, **`FROM VALUES (…)` (1)** —
    `only plain table names are supported`.
  - **correlated `EXISTS` over a set operation (1)**, **`WITH` nested inside a
    recursive CTE body (1)**, **JOIN-bodied derived table whose ON clause the
    FROM resolver cannot bind (1)**, **a query Cascades declines (1)**.
  - [x] **`CAST([1,2,3] AS STRING ARRAY)` returns NULL (1)** — CLOSED with
    the array-literal item above (walker keeps the ARRAY suffix; CastValue
    casts element-wise per Java's ARRAY_TO_ARRAY);
    `cast-documentation-queries.yamsql` passes wholly.
  - **`UPDATE … RETURNING … OPTIONS(DRY RUN)` produces no result set (1)**.
  - **an oversized record surfaces a raw executor error with no SQLSTATE (1)** —
    the corpus's `error:` assertion has nothing to compare against. This one is
    an error-mapping gap, not a feature gap, and is probably the cheapest.
  - **`select * from ta limit 5` SUCCEEDS in Go where Java raises `0AF00` (1).**
    The one entry that is not a Go deficiency. Booked anyway: the conformance
    principle governs the shared surface in BOTH directions, and an unreviewed
    widening is precisely the silent divergence the cross-engine harness exists
    to find. Decide deliberately whether this is a sanctioned read-side
    extension or a missing rejection — do not leave it undecided.

  - **`SELECT *` over JOIN … USING does not hide the right-side USING columns
    (1 file).** Newly reached when RFC-202 S2 unblocked join-tests.yamsql:
    `select * from ja join jb using(c1) join jd using(c1)` returns 6 columns
    where Java returns 4 (QueryVisitor.resolveJoinUsingClause hides the
    right-side USING columns from the star; the synthesized ON equality is
    correct on both engines — see synthesizeUsingOnExpr's divergence note).
    The fix is in the star expansion over join legs, not the join semantics.
  - **width-suffixed FLOAT literal `1.0f` (1 file, union.yamsql)** — the
    floating sibling of the `1I`/`2L` gap, same constant-folder family.
  - **schema-template serialization options / encryption (1 file)** —
    serialization-options.yamsql expects XXF01 on reads without the
    encryption key; Go's store layer has no encrypted serialization, so the
    read succeeds.
  - **the enum matcher arm is not ported (0 files today, but unbooked until
    now).** Java's `Matchers.matchField` has an arm comparing a String
    expectation to a protobuf `EnumValueDescriptor` by NAME. Go omits it,
    because whether it is needed depends on what the driver returns for an enum
    column — if that is already the name as a string the existing String arm
    covers it; if it is an ordinal or a typed value, the omission is a silent
    mismatch. Unanswerable today: every corpus file with an enum column is
    skipped before a row is compared. Answer it when `enum.yamsql` /
    `insert-enum.yamsql` unblock, and delete the comment at the site or write
    the arm. Stated rather than guessed — an untested arm written on a hunch is
    worse than a named omission.

  Order these by ledger count, not by list order: the array-literal five are
  worth more than the nine singletons combined, and the error-class one is
  cheap. Each fix removes its class from the CQ-69.1 pinned ledger and updates
  it in the same commit.


- [ ] **CQ-73 (L, gated behind RFC-201 Phase 3) — `create schema template` with
  struct types, reached through a SETUP STEP rather than a `schema_template`
  block.** Two files (`showcasing-tests.yamsql`,
  `create-drop-create-template.yamsql`) issue the template DDL as a setup query,
  so they hit the same Phase-3 struct gap by a different route and are classed
  with it (`unsupported-DDL:struct`, 39 files total). No separate work: closing
  CQ-69.6 closes these. Booked only so the two files are not mistaken for a
  distinct gap when the struct count is re-measured.


- [ ] **CQ-84 (MED, query-engine — needs its own RFC + Graefe ACK): a
  qualified star in a SELECT list that also carries GROUP BY is rejected
  unconditionally, where Java expands the star FIRST and only then applies the
  grouping rule.** Java: `LogicalOperator.generateGroupBy` walks
  `outputExpressions.expanded()` — star ALREADY expanded — and asserts each is
  `SemanticAnalyzer.isComposableFrom(expr, groupBy ∪ aggregates, aliasMap,
  outerCorrelations)`, else `GROUPING_ERROR`
  (LogicalOperator.java:435-441). So a star covering exactly the grouping list
  is legal and only a star reaching past it is 42803. Go rejects every
  star-with-GROUP-BY in `classifySelectElements`
  (`select_parser.go:1354-1363`, "SELECT qualifier.* expands to columns not in
  GROUP BY"), which runs before any schema is in hand to expand against.
  MEASURED live, both engines, in
  `conformance/duplicate_star_java_probe_test.go`:
  - `SELECT a.* FROM T_A a GROUP BY id` → JAVA `42803 "Invalid reference to
    non-grouping expression A.NAME|string ∪ ∅| ⇾ qf64031cb….NAME"`; GO `42803`
    (same class, different text) — this arm AGREES.
  - `SELECT a.* FROM T_A a GROUP BY id, name` → JAVA passes the semantic check
    and fails only at planning (`"Cascades planner could not plan query"`, no
    index on the probe table); GO `42803` — this arm DIVERGES.
  - `SELECT b.bid FROM T_B b WHERE EXISTS (SELECT a.*, b.bid FROM T_A a GROUP
    BY id, name)` → JAVA reaches planning; GO `42803`.
  The corpus proof is `select-a-star.yamsql`, which asserts RESULTS for
  `select B1 from B where exists (select A.*, B1 from A group by A1,A2,A3)`
  (line 46, repeated at 85) and for the `B.*` variant (line 89) — its table has
  index `A_idx` on exactly `(A1,A2,A3)`, so Java plans all three. The file is
  booked `engine-gap:star-group-by-expansion` in
  `pkg/relational/conformance/javacorpus/gaps.go` at that exact rejection.
  WHY IT IS NOT A ONE-LINER: the fix has to move star expansion AHEAD of the
  aggregate reclassification that the same block performs
  (`select_parser.go:1354-1397` rewrites `projCols` into `aggCols`), and that
  block runs inside `extractFromQueryTerm`, which has NO metadata and has NINE
  call sites — including the EXISTS-subquery planner, where an unexpanded star
  reaching the aggregate pipeline would be silent-wrong rather than loud.
  DONE = `select-a-star.yamsql` passes its three EXISTS-with-star-GROUP-BY
  assertions, the two GROUPING_ERROR negatives in the same file (lines 59, 82)
  still reject 42803, and the gap entry + its skip class are deleted.


- [ ] **CQ-85 (SMALL, query surface): `SELECT *, a.*` — a bare star mixed with
  any other select item — is rejected by Go and accepted by Java.** MEASURED
  live, `conformance/duplicate_star_java_probe_test.go` probe
  `bare_star_plus_qual_star`, `SELECT *, a.* FROM T_A AS a`:
  JAVA `OK cols=[ID(BIGINT) NAME(STRING) ID(BIGINT) NAME(STRING)]
  rows=[[1 alpha 1 alpha] [2 beta 2 beta]]`;
  GO `ERROR sqlstate="0A000" msg="cannot mix * with named columns in SELECT
  list"`. The rejection is `select_parser.go:777-780`, fired whenever a
  `SelectStarElementContext` is not the sole select element. Java has no such
  rule: `expandStar` resolves each star independently and concatenates, which
  is the same property that makes the duplicate qualified star legal (fixed
  separately — see the per-attribute ambiguity work). Related to CQ-84 in
  mechanism (both are star expansion running too late or not at all) but
  independent of it: this one needs no GROUP BY and no schema-time expansion,
  only the removal of a rule Java does not have plus the plumbing to expand a
  bare star into a non-sole slot.
  DONE = `SELECT *, a.*`, `SELECT a.*, *` and `SELECT *, id` answer with
  Java's column list, pinned against the live-JVM probe.


- [ ] **CQ-87 (SMALL, needs confirmation first): Java may wrap a
  PARENTHESISED SCALAR into a one-field record where Go unwraps it.** Go's
  `walkRecordConstructorInner` unwraps a one-element unnamed constructor
  because that is the parser's shape for `(expr)`; Java's
  `visitRecordConstructor` has NO such unwrap — it goes straight to
  `RecordConstructorValue.ofColumns` (ExpressionVisitor.java:918-925).
  **CONFIRMED — the "needs confirmation first" gate is now CLOSED.** The
  blocker was that the harness rendered every Java struct as
  `__unsupported__`, so only the column TYPE was visible. `encodeValue`
  (`conformance/sql_plan_steps.java`) now renders a `RelationalStruct` as its
  attributes and a `java.sql.Array` as its elements, so struct CONTENTS are
  measurable. MEASURED, live JVM, `conformance/paren_star_java_probe_test.go`:

    scalar_no_paren    `SELECT 1 + 2`     JAVA `_0(INTEGER)` `3`
                                          GO   `_0(INTEGER)` `3`     agree
    scalar_one_paren   `SELECT (1 + 2)`   JAVA `_0(STRUCT)`  `{_0: 3}`
                                          GO   `_0(INTEGER)` `3`     DIVERGE
    scalar_two_parens  `SELECT ((1 + 2))` JAVA `_0(STRUCT)`  `{_0: {_0: 3}}`
                                          GO   `_0(INTEGER)` `3`     DIVERGE
    column_one_paren   `SELECT (val)`     JAVA `_0(STRUCT)`  `{VAL: 10}`
                                          GO   `VAL(BIGINT)` `10`    DIVERGE

  So Java really does build a one-field record, it nests on each extra paren,
  and the inner field takes the ELEMENT's name (`_0` for an expression, `VAL`
  for a column reference) while the OUTER column is anonymous. Pinned by the
  assertion spec in the same file, which goes red if either engine moves.

  NEW CONSTRAINT, and it is the reason this is not simply "delete the unwrap".
  The same parenthesis in an OPERAND position stays scalar in Java:

    paren_scalar_arith        `SELECT (val) + 1`        JAVA `_0(BIGINT)` `11`
    paren_scalar_in_predicate `WHERE (val) = 10`        JAVA answers row `1`

  Both parse through the SAME `recordConstructor` rule, so Java is unwrapping
  (or coercing) a one-field record somewhere downstream of
  `visitRecordConstructor` — not in the constructor itself. Removing Go's
  unwrap wholesale would therefore re-type `(val) + 1` and `WHERE (val) = 10`
  into record-vs-scalar shapes Java does not produce. FIND THAT COERCION
  FIRST: the fix is "move the unwrap from the constructor to wherever Java has
  it", not "remove the unwrap". Both operand shapes are pinned in the
  assertion spec so the constraint cannot be lost.
  DONE = Go returns `{_0: 3}` for `SELECT (1+2)` and `{VAL: 10}` for
  `SELECT (val)`, while `SELECT (val) + 1` still returns BIGINT `11` and
  `WHERE (val) = 10` still matches — with the corpus run showing the cost.


- [ ] **A correlated array EXISTS refuses every inner projection except the bare
  element alias, including the `SELECT *` form Java's own corpus uses.**
  The correlated
  primary unnest in an EXISTS body refuses any projection other than the bare
  element alias — `SELECT *` / `SELECT 1` / `SELECT x.ek` all give
  `0AF00: correlated array EXISTS currently requires projecting its element
  alias` (`logical_predicate.go`, the projection gate in
  `tryBuildCorrelatedPrimaryUnnest`). Java accepts `select * from t.arr as x
  where …` and returns rows, so this is the shape its own yamsql corpus uses.
  It is a LOUD refusal, never a wrong answer, and it is upstream of everything
  above — it is why the tests spell the projection `SELECT x`. Pinned as a
  divergence sentinel by the `projection_gate_still_refuses_star_divergence_sentinel`
  arm, whose re-arm note says to assert rows (`ID|1`) when the gate is widened.
  DONE for the gate = EXISTS ignores the inner projection as Java does (EXISTS
  observes cardinality only), with the projection still validated rather than
  skipped, and that sentinel arm converted to a row assertion.

### Two measured Java divergences on the recursive-CTE path

Both are pinned by `conformance/dotted_and_recursive_seed_java_probe_test.go`,
which measures them against a live JVM rather than asserting them. Run it with
`bazelisk test //conformance:conformance_test --test_arg="--ginkgo.focus=DottedAndRecursiveSeedJavaProbe" --test_arg="--ginkgo.v"`
and read the per-shape lines; the probe asserts CURRENT behaviour on BOTH
engines, so it goes red when either one moves, in either direction.

**A recursive CTE alongside a plain one is a MISSING CAPABILITY, not a shared
rejection.** Java plans it and returns rows:

```sql
WITH RECURSIVE s AS (SELECT "id" FROM Q1),
     d AS (SELECT * FROM s UNION ALL SELECT d."id" + 1 FROM d WHERE d."id" < 3)
SELECT * FROM d
```

Java answers `[id]` with `[1] [2] [2] [3] [3]`; Go rejects `0A000: condition is
not met!` from `plan_visitor.go`, and does so whether or not the sibling's body
contains a join. This is the one to do first of the two — it is a capability
gap, and a corpus row asserting the `0A000` would be blessing it.

**The arity SQLSTATE disagrees.** For a four-column seed against a one-column
recursive term Java rejects `42F10 Invalid column position number: 1`, where Go
now reports `0AF00: recursive CTE branches have no exact common result row:
recursive branch 0 has width 1, want 4`. Go's message is the more informative
of the two and the SQLSTATE is the divergence; the fix is to reach Java's arity
validator before the branch-row derivation, not to reword the message. Until
then the corpus row for that shape pins the SQLSTATE only, and the explaindiff
golden carries the exact message.


---

## 6. Record layer, executor and metadata

`pkg/recordlayer` internals: cursors, executor plumbing, index maintainers, metadata builders,
result-set metadata.

- [ ] **CQ-18 (LOW) — `RecordQueryInUnionPlan.maxSize`/`GetMaxSize()` is
  stamped and preserved but never read by the executor.** Set at
  `rule_implement_in_union.go:337` from
  `PlannerConfiguration.AttemptFailedInJoinAsUnionMaxSize`, carried through
  rebuilds, but `executeInUnion` (`executor_new_plans.go:1084`) never calls
  `GetMaxSize()` — zero hits for `GetMaxSize` in the executor package. Java
  re-checks this cap at **runtime**
  (`RecordQueryInUnionPlan.java:151-154`, throws if actual fanout exceeds it)
  because Java's IN sources can resolve from live parameters. The knob
  defaults to 0 (disabled) with no production call site setting it today, so
  both engines are inert on this axis right now — a missing safety guard, not
  wrong rows. Found during the CQ-17 sweep; not implemented there (out of
  scope — recording only, per the sweep's mandate not to grow scope beyond
  the property bug it was chasing).

- [ ] **CQ-19 (LOW) — `RecordQueryVectorIndexPlan.IsReturningVectors()` is
  plumbed through but never read by the executor.** Set from
  `comparisons.go` through `vector_index_match_candidate.go:276` into the
  plan, but `executeVectorIndexScan` (`executor.go:404-536`) always does a
  full base-record fetch by PK regardless of the flag. Results are not
  wrong — a stored vector column is present in the base record either way —
  but the "read the vector straight from the index entry, skip the fetch"
  optimization the field requests is never delivered. No SQL surface
  constructs the triggering comparison today, so this is inert, not a live
  bug. Found during the CQ-17 sweep; not implemented there (out of scope —
  recording only).


### Index type naming
  - [ ] **Adjacent (separate index-type bug): `GetIndexTypeName` hardcodes `MIN_EVER_LONG`/`MAX_EVER_LONG`** — MIN/MAX over a non-long operand needs `MIN_EVER_TUPLE` (Java `permuted_min/max`).


### ComparisonKeyFunc error channel (RFC-087 follow-up)
  - [ ] **Follow-up (RFC-087, Graefe): thread `ComparisonKeyFunc` error channel.** The 5 executor merge/sort comparison-key sites (`intersectionCompKeyFunc`, `multiIntersectionCompKeyFunc`, `mergeSortCursor.isBetter`/`extractKey`, executor.go:1391) `panic(err)` on a stray key-eval error — pre-existing behaviour (no recover before/after RFC-087), and keys are pre-projected field refs so the typed-error family is unreachable today. To make it airtight, give `ComparisonKeyFunc` an `error` return and thread it (ripples into wire-adjacent `merge_cursor.go`). Low priority — not reachable from current SQL.

- [ ] **Port Java's `FDBDatabaseRunner` default `maxAttempts=10` (+ full-jitter exponential backoff) into `pkg/recordlayer.FDBDatabase.Run`.** See the executor cursor-continuation entries in the completed archive for the full citation trail (`FDBDatabase.java:856-864`, `FDBDatabaseFactory.java:90-92`, `FDBDatabaseRunnerImpl.java`'s `RunRetriable`, `TransactionalRunner.java`, `ExponentialDelay.java`). Today `FDBDatabase.Run` delegates entirely to `pkg/fdbgo/fdb`'s `Database.Transact`, which retries a retryable error INSIDE one call, unbounded by default (correct raw-client parity with libfdb_c — do not touch `pkg/fdbgo`). Java's Record Layer adds a SEPARATE, higher-level cap: `FDBDatabaseRunnerImpl` opens a fresh `FDBRecordContext` per attempt (via `TransactionalRunner.runAsync`, which does not retry on its own) and gives up after `maxAttempts` (default 10) retryable failures, surfacing the last error instead of continuing. Go currently has no Record-Layer-level attempt cap at all — a transaction that reliably fails the same way every attempt (for any reason, not just the scan/byte/time-limit gap fixed above) retries forever inside the client's own loop with no visibility or bound at the Record Layer. Port: add `MaxAttempts`/`InitialDelayMillis`/`MaxDelayMillis` fields (defaults 10/10/1000) to `FDBDatabaseFactory`/`FDBDatabase`; rewrite `Run`/`RunWithWeakReads`/`RunWithVersionstamp` to own their own attempt loop (create a transaction, run the closure, commit, classify the error as retryable via the existing `fdb.IsRetryable`-style predicate, full-jitter exponential delay between attempts, give up and return the last error past `maxAttempts`) INSTEAD OF delegating straight to `d.transactor.Transact`. This is a genuinely large, invasive change — it touches the commit/retry contract for every FDB transaction the Record Layer opens (virtually the whole codebase runs through `FDBDatabase.Run`) and needs its own design pass + review before landing, not a rushed addition alongside a leaf-cursor fix. Multi-shift effort; out of scope here.


### SPFresh — tracked in RFC-094 (status)

All SPFresh tracking — current state, shipped work, open items, frozen
performance, and measured-negative levers — is consolidated in the authoritative
tracker **`rfcs/094-spfresh-status.md`**. The former "multi-tenant scale-out" and
"recall at scale" sections (every item closed) moved there; the SQL surface is
Phase 9 above (shipped).

Open work (detail + file:line in the RFC):
- **Tier 1:** SPFresh has no chaos/model-based fault coverage — the whole
  lifecycle incl. RFC-104 refinement is untested under injected faults and
  refiner-vs-rebalancer concurrency (highest-value gap); refresh
  `SPFRESH_OPERATIONS.md` for the refinement loop (stale wrt RFC-104).
- **Tier 2:** changelog chunking for >~267M-vector single-store builds
  (`spfresh_build.go:120`); a reference maintenance worker looping sweep+refine on
  a cadence (today they're library entry points a deployment must wire).
- **SQL nice-to-haves:** yamsql vector port, `ef_search` FDB behavioral test,
  OR-of-two-KNN execution test, window-in-`WHERE` `42F21` rejection.


- [ ] **CQ-74 (MED/M, M, driver + executor result metadata) — the result-set
  metadata pipeline TRUNCATES a column's type to one flat string, so an array
  column is reported by its ELEMENT type and a struct column carries no fields
  and no declared type name.** Found by RFC-201 Phase 2 (CQ-69.2) while
  implementing the `resultMetadata:` assertion, and MEASURED against the Java
  source rather than inferred.

  **The single point of loss is `executor.ColumnDef`**
  (`pkg/recordlayer/query/executor/resultset.go:60-65`): `Name`, `Label`,
  `TypeName string`, `Nullable int`. `deriveColumnsFromPlan`
  (`pkg/relational/core/embedded/cascades_generator.go:2599`) collapses the
  planned `values.RecordType` / `values.ArrayType` into it, and everything
  downstream — `paginatingRows.ColumnTypeDatabaseTypeName` (`:1399`),
  `resultSetMetaData.ColumnDataType`
  (`pkg/recordlayer/query/executor/resultset.go:559`, which RECONSTRUCTS a
  DataType from the name string and degrades STRUCT/ARRAY to StringType) — sees
  only that string.

  Three measured consequences:

  - **An ARRAY column advertises a SCALAR scan type.** `ColumnTypeScanType`
    (`cascades_generator.go:1406`) switches on the truncated name, so `x integer
    array` reports `int32` — a caller following it allocates a scalar for a
    list. Same root cause, separate observable, measured live.
  - **An ARRAY column is spelled as its element.** `valueTypeName` has no
    `TypeCodeArray` arm (`cascades_generator.go:4297-4328` falls through to
    `""`) and `protoKindToTypeName` (`:4432`) switches on `Kind()` alone,
    ignoring `IsList()` — so `x integer array` reports `INTEGER`. Java reports
    `ARRAY(INTEGER)`, built by
    `CheckResultMetadataConfig.buildArrayTypeName` off
    `ArrayMetaData.getElementTypeName`. Pinned as a live divergence by
    `TestArrayTypeNameIsNotSurfacedByTheDriver`.
  - **A STRUCT column carries no fields and no declared type name.** Java walks
    `StructMetaData.getStructMetaData(i)` recursively and reads
    `getTypeName()`; Go has neither. A directly-selected message field is worse
    still — it reports `UNKNOWN`, since `protoKindToTypeName`'s `MessageKind`
    hits the default arm.
  - **`staticRows.colTypes` is never populated** (`system_rows.go:32`; every
    construction site sets only `cols`/`rows` — `:207`, `system_tables.go:162,
    223,302,380,417,447`, `cascades_generator.go:529`), so EXPLAIN, SHOW and
    `INFORMATION_SCHEMA` queries report `DatabaseTypeName() == ""` and
    `ScanType() == any`. That one is small and separable from the other two.

  **The Java machinery Go is missing already has a declared Go home.**
  `api.ResultSetMetaData.ColumnDataType`, `api.StructMetaData` and
  `api.ArrayMetaData` (`pkg/relational/api/resultset.go:78`, `api/struct.go:30`,
  `api/array.go:34`) are faithful ports of Java's interfaces and have ZERO
  implementations that carry real nesting — so this is not "Go lacks the
  capability", it is "the interface exists and nothing fills it". The fix is
  `executor.ColumnDef` carrying an `api.DataType` alongside the name, populated
  from the plan's result type in `deriveColumnsFromPlan` and its seven
  per-plan-shape derivations, plus the driver-side accessor a caller can reach
  (`sql.Conn.Raw` to `*embedded.EmbeddedConnection` is the established seam —
  `pkg/relational/conformance/rowdiff/run.go:142-149`).

  **Why booked rather than fixed inline:** it changes what
  `ColumnTypeDatabaseTypeName` returns for array columns, which feeds
  `ColumnTypeScanType`, the yamsql matcher's Integer/Long promotion
  (`javacorpus/match.go` `typeAt`) and `plandiff/go_runner.go`. That is an
  executor/driver surface change with cross-package blast radius, so it takes
  the query-engine review gate. It is ALSO not provable end-to-end for the
  struct half until RFC-201 Phase 3 lands struct DDL — the array half is
  provable today and should not wait for it.

  Cost, MEASURED by `TestDescendingMetadataCorpusCost` rather than estimated:
  **0 corpus files today, 14 the day Phase 3 lands, 12 of them held by struct
  DDL alone.** Every vendored file whose `resultMetadata:` descends is claimed
  first by `unsupported-DDL:struct` or `engine-gap:array-literal-values`, which
  is why `unsupported:result-metadata-nested` is a MASKED class in the CQ-69.1
  ledger. An earlier draft of this line said 8; that figure came from grepping
  for the directive instead of inspecting its SHAPE — most files carrying
  `resultMetadata:` carry only scalar expectations, which are asserted today.
  The count is now a test, so it cannot be quoted wrong again.

  **The live sentinel is `TestFDB_ArrayColumnMetadataIsTruncated`**
  (`pkg/relational/conformance/javacorpus/arraymetadata_fdb_test.go`), which
  runs `SELECT pk, x FROM t1` over an EMPTY table with `x integer array` and
  reads `database/sql`'s answer directly — no INSERT needed, because column
  metadata comes from the plan. It fails
  the moment the driver starts spelling the type Java's way, and its failure
  message is the hand-over instruction. It replaced a first attempt that
  compared two compile-time constants and could never have fired, which would
  have made the runner's decline permanent by accident.

  DONE = an array column reports `ARRAY(elem)` with a non-scalar scan type and
  a struct column reports its field list and declared type name through a
  caller-reachable surface; `unsupported:result-metadata-nested` stops being
  masked (or goes to zero); `TestFDB_ArrayColumnMetadataIsTruncated` is deleted
  together with the runner's declining branch.


- [ ] **CQ-78 (MED/M, driver + executor) — RFC-203 is merged as a DESIGN; its
  implementation is booked nowhere.** · M · query-engine review gate
  `rfcs/203-sql-continuation-envelope.md` merged in `f5c2c7f0e` (#554) and was
  commissioned explicitly for CQ-69.2's continuation half — but CQ-69.2 never
  mentions RFC-203, and the implementation has no item. MEASURED at
  `f5c2c7f0e`: no `GO_V0` anywhere in `pkg/`, and
  `pkg/relational/core/embedded/cascades_generator.go:183` still rejects with
  "only SHOW administration statements are supported", so `EXECUTE CONTINUATION`
  is unreachable from SQL.
  **Booked 2026-08-01** by the B6 pass, same failure mode as CQ-77: a merged RFC
  reads as progress, and nothing pointed at the work it commissioned.
  DONE = `EXECUTE CONTINUATION` plans and executes end-to-end, plan transport
  under `GO_V0` round-trips, per-page `MAX_ROWS` is honoured, and a yamsql
  scenario exercises it (per "NO FAKE CHECKBOXES": the RFC existing is not done).


- [ ] **CQ-91 (query-engine — needs its own RFC + Graefe ACK): Go has THREE
  independent implementations of Java's ONE field-read helper; consolidate
  them.** Java funnels every proto field read through
  `MessageHelpers.getFieldOnMessage` (MessageHelpers.java:124-143), whose
  first branch is `if (field.isRepeated())`. Go re-implements that rule three
  times, and the three sites are:
    1. `executor.protoToPositional` — `pkg/recordlayer/query/executor/query_result.go`
       (the base-scan positional row);
    2. `values.protoFieldByName` — `pkg/recordlayer/query/plan/cascades/values/values.go`
       (struct descent / `FieldValue` over a message context);
    3. `rowstruct.MessageStruct.fieldValue` — `pkg/relational/core/rowstruct/rowstruct.go`
       (the driver-visible STRUCT).
  This is not hypothetical drift: CQ-89 WAS that drift. Sites 2 and 3 already
  applied the repeated-first rule (`HasPresence()` is false for a repeated
  field) while site 1 tested presence first, so an empty NOT NULL array read
  back as `[]` or as NULL **depending on which path the plan happened to
  take**. `TestReadPathAgreement_EmptyArrayAndAbsence`
  (`read_path_agreement_test.go`) now gates the three against each other and
  against Java's answer, and a mutation to any ONE of them turns it red naming
  that site — but a gate is a smoke alarm, not a fix. Java's structure is one
  helper; Go's should be too.
  Explicitly NOT folded into the CQ-89 PR: collapsing three read paths onto a
  shared helper touches the executor row builder, the values layer and the
  driver struct surface at once, and the three legitimately differ in
  REPRESENTATION (rowstruct materializes driver values — a UUID as its
  canonical string — where the engine layers keep `[16]byte`), so the shared
  helper has to be parameterized on the leaf conversion rather than merely
  extracted. That is a design with a real choice in it. DONE = an RFC with a
  Graefe ACK, the three sites reduced to one rule with per-layer leaf
  conversion, and the agreement test kept as the regression gate.


### `RecordMetaDataBuilder.Build` does six jobs in one frame

Measured with `awk '/^func \(b \*RecordMetaDataBuilder\) Build\(\)/,/^}/'
pkg/recordlayer/metadata.go | wc -l`. **Recompute it; do not trust a number
written here.** The series over the commits that produced this entry is 658
(`103eee9e6`), 620 (`0b3a33d6e`), 644 (`e1d95cb49`), 668 (`89452ec11`) -- and
those are stable because each names a SHA. A fifth figure for "now" was written
into this entry twice and was stale both times, the second time within the same
working session that wrote it, because the edits folding a review round grew the
function underneath the sentence describing it. That is the failure this repo
calls a measurement with no stated population, and the fix is to state the
command instead of its output.

The trend is **not** monotonic: -38, +24, +24. An earlier revision said it "has
GROWN in each of the last three commits"; one of them shrank it by 38 lines, and
that sentence was written from the direction of the last two deltas rather than
from the series. The narrower true statement is enough: every correctness fix in
this area lands inside the same frame, and the one commit that shortened it did
so by DELETING a divergent loop rather than by extracting anything.

`Build` drains `buildErrors`, validates the registry/association invariant in
**six** refused classes -- `alsoUniversal`, `renamed`, `renamedUniversal`,
`unregistered`, `mismatched`, `orphaned`, which is the slice literal in
`pkg/recordlayer/metadata.go` whose first element is `alsoUniversal` (cited by
SYMBOL, not by line: an earlier revision wrote `:900-912`, and edits made in the
very commit that wrote it pushed the block to `:913-924`, so the range was stale
before it was ever read) -- computes `primaryKeyComponentPositions`, copies the containers,
validates subspace keys, precomputes union field numbers, and constructs the
result. A seventh association class, the duplicate, is deliberately ACCEPTED
because Java accepts it -- an earlier revision counted seven, which counts the
one case that is not validated.

The concrete cost is not length, it is that an ORDERING DEPENDENCY between two
of those jobs is invisible. Positions must be computed BEFORE the containers are
copied, because Java sets them on the objects the caller registered and the scan
call sites read positions off those same objects; getting that backwards
produced `go: pk=[]` against `java: pk=[1]` and was caught only by the
cross-engine conformance suite. Today a comment inside `Build` is the only thing
holding it, and that comment names this entry so the two halves point at each
other. As two calls in a short frame it would be obvious.

Two seams lift out cleanly, both self-contained: the registry/association census
and the positions-plus-copy step. `validateIndexBijection()` and `detachFrom(b)`
were the names proposed in review.

DONE when: `Build` reads as an ordered list of named steps, the
positions-before-copy dependency is expressed by call order rather than by a
comment, and the extraction changes no behaviour — same tests, uncached, before
and after.

---


---

## 7. Performance and allocation

Measured cost, not suspected cost. Every entry here carries a profile or a stress comparison;
see section 10 for the baselines they are measured against.

### DISTINCT cross-page dedup — the cost-based sort alternative (correctness already closed)
 - [ ] C5 follow-up: DISTINCT cross-page dedup — **CORRECTNESS FIXED end-to-end (2026-07-20); only a
       cost/perf optimization remains.** Both paths now resume-clean: ORDERED input streams (carries
       the last key), UNORDERED input (incl. DISTINCT + ORDER-BY-non-output-column) uses the hash
       cursor that carries its whole seen-set through gen.DistinctHashContinuation (Approach B,
       Graefe-ACK'd; distinctHashCursor in distinct_stream.go). Pinned by
       TestFDB_SelectDistinct_CrossPageDedup_Unordered (g=id%10 scattered, scanLimit=2). REMAINING
       (perf, NOT correctness): enumerate a cost-based sort-distinct alternative so a high-NDV DISTINCT
       (where the seen-set would blow the memory budget → today a LOUD budget error, never wrong rows)
       routes to a sort instead — Graefe's phase-2. Until then high-cardinality unordered DISTINCT over
       a huge table errors on the budget rather than completing; that is a performance limit, not a
       wrong-rows bug. FIXED: over input already ordered by the dedup key (`SELECT DISTINCT col
       ORDER BY col`, or a covering ordered index), a resume-clean STREAMING distinct now dedups
       adjacent rows and carries ONLY the last emitted key through the DedupContinuation —
       `RecordQueryDistinctPlan.Streaming`, set by ImplementDistinctFinalRule when
       `orderingSatisfiesGroupingKeys(EstimateOrdering(inner), projected-dedup-cols)` holds; executor
       distinctStreamCursor (distinct_stream.go). Pinned by TestFDB_SelectDistinct_CrossPageDedup
       (N=50, scanLimit=2, incl. LIMIT/OFFSET). REMAINING (needs a DESIGN pass, not a mechanical
       extension — do NOT just insert an InMemorySort): the UNORDERED case (`SELECT DISTINCT col`
       with no ORDER BY and no covering index → primary scan) still uses the fresh-per-page hash-set
       and re-admits across pages. Two candidate fixes, each with a real drawback — the choice is a
       genuine trade-off that should get a Graefe design ACK before implementation:
         (A) REQUEST the dedup-key ordering → planner inserts an InMemorySort, then stream. CORRECT
             and bounded-continuation, BUT the InMemorySort BUFFERS EVERY input row (memory-bounded +
             spills) to emit few distinct rows — a severe memory regression for the COMMON
             low-cardinality / high-row shape (`SELECT DISTINCT status FROM huge`: 2 distinct values,
             10M rows → buffer 10M to emit 2, where today's hash-set holds 2 keys). The streaming-agg
             analogy does NOT transfer: GROUP BY must see every row to aggregate anyway, so its
             InMemorySort adds no extra buffering; DISTINCT's hash-set specifically AVOIDS materializing
             the input, so an always-sort forfeits that. Also must not disturb the DISTINCT +
             ORDER-BY-on-a-NON-projected-column semantic (`SELECT DISTINCT v ORDER BY id`: dedup by v,
             output ordered by id via a first-occurrence sort — sorting by v destroys the id order), so
             gate on `GetRequestedOrderings()` being empty / a subset of the dedup cols.
         (B) Keep the hash-set but carry the seen-set THROUGH the continuation (bounded by the
             statement memory budget the set is already charged against; boundedSet.m is enumerable).
             Fixes BOTH the unordered case-2 AND the ORDER-BY-non-output-column case-3 (it does not
             rely on ordering at all — just makes the existing hash-set resume-clean). Memory-parity
             with today and a SMALL continuation for the common LOW-cardinality shape (executor-only,
             no planner/plan-shape churn). Cost: the seen-set grows monotonically, so every page
             re-sends the full set-so-far → O(pages × distinct-keys) CUMULATIVE work/transfer, heavy
             for high cardinality (where today is merely wrong). Needs a Go-internal continuation proto
             (proto/relational/, since DISTINCT is Go-only — no Java wire to match).
       No clean mechanical winner: (A) trades a correctness bug for a MEMORY regression on
       low-cardinality/high-row; (B) trades it for CUMULATIVE-TRANSFER on high cardinality. This is a
       genuine DESIGN decision (likely (B), or a cardinality-aware A/B choice) that needs a Graefe
       design ACK BEFORE implementation, per the query-engine RFC-first gate — do NOT unilaterally
       ship either. The ordering-DETECTION step already shipped (case-1) is deliberately separated from
       this and is safe on its own (only ever streams when the ordering already proves adjacency, else
       unchanged hash-set).
       ORIGINAL DIAGNOSIS (kept for the unordered follow-up): `executeDistinct` (executor.go ~1552)
       rebuilt the `seen` hash-set FRESH per page, so any `SELECT DISTINCT <non-unique-col>` over a
       table larger than one page returned WRONG ROWS. Adversarial
       repro (N=500, EXECUTION_SCANNED_ROWS_LIMIT=9 forcing ~56 page breaks): `SELECT DISTINCT w
       ORDER BY w` → 100 rows (every value twice) vs correct 50; `SELECT DISTINCT g` (no ORDER BY,
       unordered input) → 500 rows (TOTAL dedup failure) vs 25; `... LIMIT 30 OFFSET 10` → wrong
       window. `SELECT DISTINCT <unique-col>` PASSES (no duplicates to drop). This is a COMMON
       query pattern → the wrong-rows blast radius is large. Single-page baseline is always correct
       → purely a resume bug.
       The code comment's Java-parity claim is only HALF true: Java's
       `RecordQueryUnorderedPrimaryKeyDistinctPlan` also uses a fresh HashSet per page but dedups by
       PRIMARY KEY (unique per record), so the hazard doesn't bite it; Go routes `SELECT DISTINCT
       col` (dedup by projected VALUE) through the same fresh-set shape, where it DOES bite.
       DEFINITIVELY RESOLVED (2026-07-20, read the Java source): Java's fdb-relational SQL layer
       does NOT dedup SELECT DISTINCT AT ALL. `QueryVisitor.visitSimpleTable` never reads the
       DISTINCT token; there is NO `.DISTINCT()` consumer anywhere in relational-core; all 3
       Java yamsql SELECT-DISTINCT tests use already-distinct data (prove parsing, not dedup).
       So there is no Java value-distinct path to match — Go's is a pure read-side EXTENSION and
       the continuation is Go-internal. IMPLEMENTATION NOW TRACTABLE (all pieces confirmed to
       exist): (a) InMemorySort physical operator exists (RecordQueryInMemorySortPlan +
       MemorySortContinuation, resume-clean); (b) streaming dedup cursor + DedupContinuation
       (innerContinuation+lastValue) exist in pkg/recordlayer/dedup_cursor.go (no prod callers —
       ready to wire); (c) the streaming-agg rule's InMemorySort+pinOrderedSpine pattern is the
       exact template; (d) per-column sort keys built via
       values.NewFieldValueWithResolvedOrdinal(col, i, typ) over the inner's RecordType (see
       rule_aggregate_data_access.go:418). CAVEAT: inserting InMemorySort under every non-elided
       distinct churns EXPLAIN for all SELECT-DISTINCT plan-shape tests — expect broad manifest
       churn; the elision path (PK/unique-key coverage) MUST be preserved.
       FIX DESIGN (ordering-aware distinct arc — Graefe-gated, wire-compat-sensitive, ~multi-piece):
         (1) EXECUTOR: a STREAMING distinct — when the input is ordered by the distinct key, emit a
             row only when its key differs from the previous emitted key; carry ONLY that last key
             through the continuation (bounded, resume-clean). This fixes the ordered case
             (`DISTINCT w ORDER BY w`), which today still fails because the hash-set is used even
             over ordered input.
         (2) PLANNER: for value-distinct, REQUEST the input ordered by the distinct-key columns even
             without an outer ORDER BY (extend the ordering constraint pushed by
             PushRequestedOrderingThroughDistinctRule), so the unordered case (`DISTINCT g`) gets an
             ordered plan and the streaming distinct applies. Fall back to a Sort when no index
             provides the order.
         (3) CONTINUATION: the RecordQueryDistinctPlan continuation must carry the last-key — a wire
             format change; encode it in the proto continuation, version-guard the decode.
       The current fresh-set-per-page is a Go-only correctness hole (Java doesn't auto-page inside one
       statement). Pin FIRST with a large-N (>1 page) multi-value-per-key rows test at a small scan
       limit, both with and without ORDER BY, before touching the executor. Documented at
       executeDistinct.
       DEEPER DIAGNOSIS (2026-07-20, DFS): (a) `SELECT DISTINCT` is a GO-ONLY extension — Java's
       fdb-relational REJECTS it for most shapes (see rule_implement_distinct_final.go header), and
       Java's RecordQueryUnorderedDistinctPlan.executePlan(:109) also builds a fresh HashSet per
       call, so there is NO Java wire-format to match: the DISTINCT continuation is GO-INTERNAL —
       NOT a Java-wire-compat concern. That removes the "hard line" blocker; the continuation can be
       extended. (b) The scan limit returns a CLIENT-facing continuation (ScanLimitReached →
       noNextOrFail(..., limitContinuation()) in the scan cursors), i.e. a NEW Execute per page, so
       the seen-set is genuinely lost across Executes — the state MUST ride the continuation (not
       merely persist in statement State). (c) TEMPLATE: RecordQueryStreamingAggregationPlan already
       resumes cleanly across pages at large N (agent-verified) by carrying its running group key in
       the continuation — the streaming DISTINCT is the same pattern (carry the last emitted key).
       Port executeStreamingAggregation's continuation encoding. So the arc stays 3 pieces but is
       now de-risked and Go-internal: streaming-distinct executor + carry last-key in the (Go-only)
       distinct continuation + planner requests distinct-key ordering
       (PushRequestedOrderingThroughDistinctRule already pushes an OUTER ORDER BY through; extend it
       to request the distinct-key ordering even without one, falling back to a Sort). Graefe-gated
       (executor + continuation), but no cross-engine wire risk.


### Intersection resume — skip re-scanning discarded rows (Go-only, beyond Java)
  - [ ] **Follow-up (RFC-071, Go-only optimization beyond Java): skip re-scanning discarded non-matching rows on intersection resume.** Because the cached per-child continuation sits at the last *consumed* (matched) position (faithful to Java `MergeCursorState`), an out-of-band stop resumes a child from there and re-scans the non-matching rows discarded since its last match (bounded by the inter-match gap; the whole prefix-to-first-match for a never-matched child). Correct (no dup/no loss) and Java-faithful, but for very sparse intersections under a tight per-page limit the re-scan is wasted work and — pathologically — could fail to make progress within one page. Tracking the position just *before* the currently-held candidate (so resume re-reads only it) would eliminate the re-scan; this diverges from Java's model, so it's a Go-only read-path optimization, not parity. Flagged by codex on PR #252.


### RFC-232 residue — plan cache and projection anchoring
- [ ] The projection-anchoring duplicates remain, unfixed and now small. The
      obvious fix — canonicalising an unselected input program onto the
      reserved-current handle in `reanchorCurrentValueForInput` — was built and
      REVERTED: it contradicts
      `TestInMemorySortPlan_SelectedFlatMapOutputRelinkKeepsExactOutputOrdinal`,
      which pins that exploratory construction preserves the pointer-exact
      edge-bound key for the relink path to translate. Reconcile those two
      before trying it again. The other alternative — threading an AliasMap
      through `planEqualsAsExpression` — CANNOT fire as-is: the memo passes
      `EmptyAliasMap()` to plans, and reaching a real map requires opting plans
      into `InternsAliasAware`, whose own doc calls that a landmine for
      expressions whose aliases are externally resolved. A plan's quantifier
      alias IS resolved at execution, so that route trades a perf bug for a
      wrong-bindings bug.

- [ ] Separately: make the plan cache survive `ResetSession`. The cache key
      already carries DBPath, schema, metadata version and planner options, so
      the blanket invalidation looks redundant with the key rather than
      load-bearing — but prove that before removing it.

INSTRUMENTATION NOTE, because it cost several false readings here: `go test`
SWALLOWS a passing test's stdout unless `-v` is given. Every "the probe printed
nothing" conclusion in this area was wrong for that reason. Always run probes
with `-count=1 -v`, and verify the probe is present in the file it is supposed
to be in — a `perl -0pi` substitution matching a common pattern lands in the
FIRST function that matches, which is not always the intended one.



### RFC-232 residue — allocation hot spots
- [ ] `newStructuralKey` — 8.9GB plus ~12GB in the builder methods past the
      inline array. Both halves are one term: a key is built and thrown away on
      every dedup comparison, twice per equality. The structural fix is to stop
      returning a heap pointer — fold into a caller-provided stack local so
      escape analysis can keep it there — which is a signature change across all
      44 plan types and worth measuring on two of them first.

- [ ] `GetCorrelatedToOfValue` — 8.1GB in the map the walk fills (master pays
      4.1GB, so this is 2x a SHARED cost, not branch-only). Correlation sets are
      almost always 0-2 entries; a small-slice representation would remove the
      bucket allocation for both trees. The return type is
      `map[CorrelationIdentifier]struct{}` across the whole planner, so this is
      an API change, not a local one.
- `physicalFlowedRecordType` (1.65GB) and `LayoutWithSeedLegs` (1.32GB) MUTATE
  the graph they thaw, so they are correctly excluded from the shared-graph
  treatment. Do not "fix" them.


---

## 8. CI, test infrastructure and harnesses

The safety net itself: CI capacity and flakes, differential and generative harnesses, corpus
ladders, and the gates that keep measurements honest.

### [ ] Tag container-backed test targets with `resources:memory` so Bazel stops packing them

Three nightly race steps were each running four race-instrumented FDB testcontainers on
the 7.6 GB runner — the shape that starved PR #577 — and all three were fixed by writing
`--local_test_jobs=1` at the step. That fix is per-invocation and has already been
forgotten once *in the same file*: `nightly-coverage.yml`'s first race step sat three
steps above the one that did carry the cap, and `nightly-fuzz.yml` was found only by
adding a gate. The knob does not travel with the target, and it cannot say "these two are
heavy, those twelve are free", so it drags a whole lane to the pace of its worst member.

The durable fix is to declare cost where the target is defined. MEASURED on Bazel 9.0.1,
six-test fixture, `--local_test_jobs=4`, counting actual concurrency:

| declaration | concurrent |
|---|---|
| no tag | 4 |
| `size = "large"` | 4 |
| `size = "enormous"` | 4 |
| `--local_resources=memory=4500`, no tag | **4** |
| `tags = ["resources:memory:2000"]`, budget 4500 | 2 |
| `tags = ["resources:memory:1200"]`, budget 4500 | 3 |
| `tags = ["cpu:2"]`, `--local_resources=cpu=4` | 2 |
| `tags = ["cpu:4"]`, `--local_resources=cpu=4` | 1 |
| `tags = ["exclusive"]` | 1 |

So `--local_resources=memory` bounds Bazel's memory *accounting* and gates nothing, and
`size` gates nothing — which is exactly why three steps read as bounded while none were.
Only a `resources:` / `cpu:` tag gates local packing.

Per-target peaks, MEASURED under `-race` at `GOMAXPROCS=4` on four pinned cores, run to
completion:

| target | peak RSS |
|---|---|
| `//pkg/relational/conformance/factorycorpus/full:full_test` | 5607376 KB = **5.35 GiB** (steady ~3.4 GiB) |
| `//pkg/relational/sqldriver:sqldriver_test` | ~1.16 GiB |

Proposed: `tags = ["resources:memory:5600"]` on `factorycorpus/full`,
`resources:memory:1300` on `sqldriver_test`, and leave every other target untagged until
measured. Then `--local_resources=memory=HOST_RAM*0.6` becomes the budget it is currently
mistaken for, and `--local_test_jobs=1` can come back off the steps that only need it
because one member is heavy.

**What this needs that I could not supply:** validation on a 7.6 GB box (or the real
runner). The open question is Bazel's behaviour when a single action's declared cost
EXCEEDS the whole budget — `HOST_RAM*0.6` of 7.6 GB is ~4.56 GB, and `factorycorpus/full`
measured 5.35 GB, so its tag is larger than the budget it is scheduled against. Bazel is
expected to clamp and run it alone, but that is inference, not measurement, and getting it
wrong deadlocks the lane rather than slowing it. This repo's box is 62 GB, where the
question cannot be reproduced. Do not guess it on the wrong hardware — that is how the
`~5 GB` figure got deleted as "unmeasured" in the first place when it was simply true.

Related: `pkg/docscheck`'s `TestNightlyRaceStepsDeclareTheirJobCount` currently requires
every race-instrumented nightly step to WRITE `--local_test_jobs`. If the tags land, that
gate should be revisited — the point of the tags is that the number stops needing to be
restated at each call site.

### [ ] Create the fake `ssh` once, before any parallel test starts forking

MEASURED, 1 of 20 consecutive `GOWORK=off go test ./... -count=1` runs in
`tools/bazelscaleset` (the `Test the standalone runner-supervisor module` step of the
required `Build, Lint & Test` lane):

```
--- FAIL: TestRemoteLaunchArgvAndJitDelivery (0.01s)
    remote_test.go:279: remote launch: remote launch on runner@<fake-host> failed (exit -1):
    : fork/exec /tmp/TestRemoteLaunchArgvAndJitDelivery3456571678/001/ssh: text file busy
```

(The host is the test's dotted-quad placeholder, elided here only because
`TestLivingDocsCiteCurrentJavaTarget` cannot tell a fake IP from a stale 4-part
version string. Nothing else in the output is altered.)

Mechanism: the test writes a fake `ssh` into its own temp dir, and a *parallel* test's
`fork` duplicates the still-open write fd into a child that has not yet `exec`'d. O_CLOEXEC
does not help — it clears at exec, so the fd is live for the whole fork→exec window — and
Go's `ForkLock` is held across fork+exec by `os/exec` but is not taken by `os.OpenFile`.
While any process holds a write fd, executing the file fails ETXTBSY. Same fork/exec race
`startLocal` already retries past (`scaler.go`, and `copyFile`'s comment in `slots.go`
describes the sibling unlink-first cure).

**Do NOT "fix" this by adding a retry to `launchRemote`.** The missing retry is the correct
discrimination, not an inconsistency:

- `startLocal` execs `run.sh` out of a per-slot clone that this process itself wrote —
  `slots.go` `copyFile` copies `run.sh` with its 0755 perms into every slot dir. ETXTBSY is
  genuinely reachable in production there, which is why the retry exists and is pinned by
  `TestStartLocalRetriesETXTBSY` (`lifecycle_test.go:623`).
- `launchRemote` execs `/usr/bin/ssh`, which this tool never writes. A retry there could
  never fire in production; it would exist solely to mask a test-harness race.

Correct fix direction: build the fake `ssh` **once** — `sync.Once` or a `TestMain` step —
into a shared dir, before any parallel test begins forking, so no write is ever concurrent
with a fork. That removes the race at its source without putting unreachable code in the
production path.

**Not mutation-verified, and that is the first job.** It did not recur in 25 later runs, so
at ~1/20 there is no reproducer on demand yet. Build the harness first (a deliberate write-fd
hold on the fake `ssh` at exec time is how `TestStartLocalRetriesETXTBSY` forces the sibling
shape), prove it red, then fix. Booking rather than fixing was a scope call, not a judgement
that it is unimportant: it is live on a required lane.


### [ ] Conformance harness — per-step idempotency-aware retry for mid-request Java-server drops (residual)
The `POST /invoke: EOF` conformance flake's COMMON cause (reusing a keep-alive connection the pooled Java
server closed while idle) is fixed by `newJavaHTTPClient`'s `DisableKeepAlives` (fresh connection per call,
pinned by `TestJavaHTTPClient_DisablesKeepAlives`). RESIDUAL: a server drop DURING a request (GC pause /
crash mid-`/invoke`) a fresh connection cannot prevent. A blanket retry is UNSAFE — `Invoke` is generic and
some `/invoke` steps write, so a retry could double-apply. The fix is a per-step idempotency map (retry only
read/plan steps like `RunSql SELECT`/`EXPLAIN`; never retry a write step) driven off the transport-error class.
Low priority (mid-request drops are rare; keep-alive fix covers the observed flake).


- [ ] **RFC-156 budget-exhaustion 5s-deadline stress is unverified programmatically.** `spfreshDefaultStreamCellBudget=512` / `spfreshDefaultStreamCandidateBudget=4000` are calibrated-by-comment, not pinned by a test asserting that ordered-stream search+materialize stays within the FDB 5s tx deadline on a large index + a pathologically selective residual filter (the heap/stream tests verify memory bounds and truncation honesty, not the wall-clock deadline).


- [ ] **Re-verify `joinOptimizationProbesScenario` (RFC-082 cross-engine exclusion) against RFC-042 (@claude flag).** The A3 builder is excluded from `crossEngineScenarios` with the note "Go's join enumeration is still non-deterministic on some arithmetic-predicate shapes — a 3-way / arithmetic-join can return a different ROW COUNT across runs." That row-count *nondeterminism* (a correctness flake) is NOT the item tracked above — line 11-40 is the now-FIXED FROM-order-dependent (but per-order deterministic) bug, and line 42 is cost-optimality (correct results, just slower). So either the exclusion note is stale (the row-count flake was the fixed PartitionSelectRule bug → the scenario may be re-enableable cross-engine now) or there is a genuinely-still-nondeterministic join-enum shape that needs its own root-cause. Verify with a focused multi-run of the probe shapes; if still nondeterministic, the Go-only yamsql coverage for `join_optimization_probes` is itself flaky (same code path) and must be pinned, not just excluded cross-engine. Out of scope for RFC-082 (conformance determinism); tracked here for the RFC-042 follow-up.



### RFC-182 generative row-soundness differential — remaining phases
- [ ] **RFC-182 P2 (remainder)** — continuation-resume dimension, the
  minimizer + yamsql draft emitter (§6), the forced-alternative mode
  (disable dominant plan families so losing memo alternatives get
  row-checked), non-correlated EXISTS/IN if feasible.

- [ ] **RFC-182 P3** — Oracle J (Java `runSql` via plandiff runner), java-compat
  profile, three-way triage, INFRA/DECLINE/MISMATCH signature ledger
  (Go=M/Java≠M entries ONLY — Go≠M always fails), nightly target; REQUIRED:
  extract the shared decline/divergence authority with the A3 skip list.
  Subsumes/coordinates with RFC-164 WS-1 (generative Go-vs-Java differential).

- [ ] **RFC-182 P4** — DISTINCT, GROUP BY/aggregates (Oracle M reimplementation
  honesty note in §7 — Java-first coverage), joins incl. correlated EXISTS/IN,
  vector/rank K>1 shapes. Nightly-scale empty-required-family bucket = hard fail.

Audit findings (2026-07-18 quality audit) still open, in recommended order:


### Divergence-ledger corrections
- [ ] DIVERGENCES.md ledger corrections: windowed candidate is DEAD (zero
  constructors), not "Aligned"; compensation-intersect note is now partially
  wired via the RFC-182 fix. The stale cost-model "16 criteria" table was
  corrected by RFC-190.

- [ ] **C3. Ride their test designs — port FDB workloads as scenario + invariant specs.** FDB's
  `fdbserver/workloads/*.actor.cpp` (Cycle, AtomicOps, ConflictRange, Serializability,
  FuzzApiCorrectness, …) are unrunnable for us (Sim2-only), but each scenario + invariant is
  language-agnostic. Port the adversarial designs — e.g. Cycle: maintain a ring of pointer K/Vs,
  hammer it concurrently (+faults), verify the ring stays unbroken — to drive our client against
  testcontainers (and later `SimTransport`). Reimplement the harness; reuse the proven scenarios.
  Extends the existing `pkg/recordlayer/chaos` model-based approach + `cmd/fdb-binding-stress`.


- [ ] **Parallelize the whole `//conformance` suite via stdlib `t.Parallel` (drop Ginkgo). [LOW PRIO — RFC-082 follow-up]**

  **Goal.** Cut the Go↔Java conformance suite wall time (~122s today) by running *every* cross-engine
  check concurrently, uniformly — no bespoke fan-out. Today only the two SQL loops are parallel
  (each via its own hand-rolled goroutine pool); the ~40 FDB conformance families run serially.

  **Hard constraint: bazel-only.** CI is `bazelisk test //...`, which runs each `go_test` binary
  **once, directly** (serial invocation). So the only available parallelism is **in-process**.
  Ginkgo cannot parallelize in-process — its only parallel mode is the `ginkgo --procs=N` CLI, which
  spawns N worker *processes* (each would spin its own FDB container → the 290-failure resource
  exhaustion already observed) and runs **outside** `bazel test` (loses result caching + the Java
  server's bazel runfiles). Therefore the suite must move **off Ginkgo onto stdlib `testing` +
  `t.Parallel()`**, run with `-test.parallel=N` (bazel `go_test` honors this in-process, cached,
  runfiles intact). This also finally aligns the suite with the house rule ("All tests MUST call
  `t.Parallel()`") — it's the lone serial holdout.

  **Measured profile (121.6s wall, 112s in specs; `ginkgo-report.json` from a `--nocache_test_results`
  run):** container+DB startup ~10s (serial floor); `RunSql Harness` (SeedRunCorpus, ~1620 entries)
  36s — **already** 8-Java-server parallel; `yamsql A3` (859 specs) 20s — **already** 8-server
  parallel; **~40 FDB conformance families ≈ 56s — SERIAL, on the single global Java server.**

  **The load-bearing finding — the ceiling is JVM count, not Go concurrency.** The suite is
  Java-JVM-throughput-bound and JVM count is **memory-capped on CI** (16 JVMs is exactly what caused
  the earlier conformance CI timeout; 8 is the safe ceiling). The SQL work already runs 8-way — that
  56s combined is `total_java_work / 8_servers`; unifying the two pools into one does **not** speed it
  up (same work, same servers). So the **SQL floor is ~56s @ 8 JVMs**, and the rewrite's real win is
  folding the **56s serial FDB tail** (currently on *one* server, sequential) **into** that parallel
  window → **~122s → ~70-75s (~1.7x) @ 8 JVMs**. Beating ~70s needs **more JVMs** (memory), not more
  parallelism. "Everything is parallelizable" is true mechanically, but does not buy 8x here.

  **Approach (incremental, safe).** stdlib `Test*` funcs coexist with Ginkgo's `TestConformance` in
  one package (they share globals; Go runs the sequential Ginkgo blob first, then the `t.Parallel`
  batch together) — so migrate **family-by-family** with a green + spec/assertion-**count-parity**
  gate after each (silent coverage drops are the exact CLAUDE.md failure mode). Steps: (1) move
  container + Go DB + a pool of N Java servers into `TestMain` (all servers spawned before any test →
  preserves the "no JVM spawn during a query" GRV-lag discipline); Gomega assertions stay verbatim via
  `g := NewWithT(t)`; `BeforeEach` → a setup helper; nested `Describe` → flat test names / `t.Run`
  subtests. (2) Convert each FDB family (already UUID-tenant-isolated → inherently parallel-safe).
  (3) Convert A3 + SeedRunCorpus to `t.Run(..., t.Parallel())` subtests and **delete** the hand-rolled
  worker pools + `precomputed` map + `results[]` — this is the "stop special-casing A3" cleanup.
  (4) `-test.parallel=N` via the `go_test` `args`. Keep the FDB-1020 conflict-retry (shared catalog).
  Benchmark stays gated (`CONFORMANCE_RUN_BENCHMARK`). Query-engine-adjacent → needs Graefe +
  Torvalds + @claude + codex.

  **Cheaper alternative (no rewrite, ~zero risk, ~1.3x):** just raise the existing SQL pool 8→12
  (`CONFORMANCE_A3_POOL_SIZE` / `CONFORMANCE_SEED_PARALLELISM`) if the CI runner's memory allows —
  shaves the SQL floor without touching the green, reviewed suite. The FDB tail stays serial.

  **Why low prio.** The suite is green and freshly reviewed; ~1.7x for a ~32k-line mechanical rewrite
  of wire-compat-critical tests is a weak risk/reward, and the real speed lever (JVM count) is
  memory-bound regardless. Do the cheap JVM-count bump first if speed is ever urgent.



### RFC-180 follow-up — close the provenance loop against a live JVM
- [ ] **Java-harness-verify the NULLS-default corpus flips (Graefe, efc07340e
      review):** the aggregate_null_edge / aggregate_with_null_groups /
      coalesce_in_join / distinct_patterns_java / order_by_nulls_java NULL-order
      pins were corrected from ParseHelpers.java source (ASC NULLS FIRST / DESC
      NULLS LAST). Close the provenance loop with a live cross-engine run (add
      the shapes to the plandiff corpus or run them via SqlPlanSteps).

### [ ] Test infra — FDBCLIExec can hang 30 min on a stuck Docker exec (CI flake)

Surfaced by PR #508's end-gate CI: the `Cross-client wire differential` job
timed out at the 1800s Go test limit, stuck in FDB container init. Goroutine
dump: `FDBCLIExec → configureWithRetry → InitializeDatabase`, blocked on a
`chan receive` inside testcontainers' `Multiplexed()` output reader
(`exec/processor.go:124`) for 29 minutes — a Docker `fdbcli` exec whose output
stream never drained. `configureWithRetry` (foundationdb.go:444) only checks
`ctx.Err()` BETWEEN attempts, so it cannot interrupt a read already hung inside
`FDBCLIExec` (foundationdb.go:532); the passed `ctx` does not unblock the
Multiplexed stream read. One stuck exec ⇒ the full 30-min test timeout instead
of a fast retry. Re-running the job cleared it (transient Docker hang), but the
fragility is real and pre-existing. Fix: give `FDBCLIExec` a hard per-exec
deadline that actually abandons the Multiplexed read (run the exec+drain in a
goroutine, `select` on `ctx.Done()` + a short timer, and fail the attempt so
`configureWithRetry`'s backoff loop takes over). Out of scope for the planner
work; unrelated to any wire/query change.


- [ ] **CQ-36 (LOW) — `df24afebb` has no mutation verdict.** The branch-wide
  mutation audit reverted each correctness fix and confirmed the suite went red.
  `df24afebb` (threading `context` cancellation through the whole task-execution
  and plan-extraction call graph) could not be mechanically isolated — five later
  commits edit the same plumbing, `cascades_generator.go` alone conflicts in 7
  hunks, and `unified_tasks.go`'s conflict shows interleaved content from a
  different commit. It is **unknown**, not passed. Re-attempt once the surrounding
  churn settles, or write a directed test for cancellation mid-plan-extraction.


- [ ] **CQ-37 (MED) — the generative harnesses cannot emit two known bug shapes.**
  `rowdiff`'s `genExists` appends exactly ONE correlated `[NOT] EXISTS` to a
  single-table query, correlated on a bare BIGINT column against a fixed inner
  alias, and never nests an EXISTS inside an EXISTS, never gives the inner an
  explicit `JOIN…ON`, and never puts the outer query behind an aliased
  self-subquery that could collide names with the inner. Every bug in that class
  (TODO's alias-shadowing self-subquery entries; "CORRELATED EXISTS with an
  explicit JOIN..ON inner DROPS the inner ON", which took 7 codex rounds) was found
  by review, never by a harness — because the harness cannot produce the shape.
  Second shape, unresolved: whether an outer-join `WHERE`-vs-`ON` case was ever
  actually exercised by a seed. Add a directed template rather than relying on
  random weighting, so the question stops being ambiguous.


- [ ] **CQ-44 (MED) — build the two capabilities the differential suite states it
  cannot probe.** · M
  `rfcs/prod-readiness-go-client.md:207` (P2 item 7) names four unprobed axes; two
  of them are unprobed because a CAPABILITY does not exist, not because anyone
  chose to skip them:
  - **`commit_unknown_result` (1021) idempotency** — needs WIRE-LEVEL FAULT
    INJECTION: a commit whose reply is dropped after the proxy accepted it. The
    existing `ChaosTransactor` injects at tx boundaries (`pkg/recordlayer/chaos/`),
    which is one layer too high — it cannot produce the "server committed, client
    never learned" state the 1021 contract is about.
  - **cross-shard range-merge** — needs a MULTI-SHARD cluster; the testcontainer
    is a single node, so every range read resolves inside one shard and the merge
    path across a boundary is never taken.
  Per CLAUDE.md this is explicitly NOT a deferral: *"'It needs a capability that
  doesn't exist yet' is not a deferral — it is the work."* Both are things to
  BUILD. Check the C++ source first for how `libfdb_c`'s own test harness produces
  each (FDB's simulation framework has both), and port the mechanism rather than
  inventing one.
  DONE = a wire-level fault-injection hook in `pkg/fdbgo` that can drop a commit
  reply after acceptance, with a differential test asserting Go's 1021 handling
  matches C++'s; and a multi-shard test cluster (explicit shard splits at known
  boundaries) with a differential range read that spans one. Sized M because it is
  two independent harness builds; split into two items if either grows.


- [ ] **CQ-50 (LOW, flake, bounded) — `TestMonitor_DroppedPingRePingsInsteadOfStalling`
  failed once under an UNCAPPED whole-tree `go test ./pkg/...`** (alongside
  four fault_test Docker container-start timeouts — same run). 40/40 green in
  isolation; green under bazel's `--local_test_jobs=4` cap, which is the
  sanctioned harness. The repro condition is the unsanctioned run mode, but
  per the no-unrelated-flakes rule this stays open until either the timing
  assumption is proven contention-safe (deadline margin vs CPU starvation in
  `pkg/fdbgo/transport`) or the test gets an explicit guard. Do not close by
  rerunning.


- [ ] **CQ-64 (S/M, test-fidelity residue: 14 files still assert rows through a
  name-keyed projection) — convert the remaining `unnestSprint(executor.RowValue(r))`
  row renderings to a positional renderer.** Not a product defect: the row VALUES
  those files assert are correct. What is missing is that the assertions cannot see
  the failure they exist to police.

  `executor.RowValue` projects a PositionalRow through `positionalToMap` (see the
  lossiness note at its definition), which loses slot ORDER and collapses duplicate
  output names LAST-WINS. Rendering the resulting map with `%v` gives Go's own
  alphabetical `map[A:1 B:2]` form, so PERMUTING (Fields, Slots) together — which is
  exactly what a mis-bound leg window produces — reproduces a byte-identical string,
  and a duplicate-named column drops a value outright.
  `TestPositionalRenderersSeeAPermutation` in pkg/relational/sqldriver pins that
  blindness as a measured fact.

  Two rounds are already done and are the pattern to follow. The eight `map[...]`
  sites converted to `positionalPipeSprint` (values in slot order); the six
  sorted-map-key `k=v|k=v` loops converted to `positionalNamedPipeSprint` (names
  kept, slot order). Prefer the NAMED renderer: keeping the names lets the
  expectation rewrite be verified as a pure slot REORDERING (same multiset of
  NAME=value pairs per row, same multiset of rows) instead of resting on a value
  multiset plus a manual read of the SELECT list. Do the rewrite with that verifier
  in the loop and refuse any diff it cannot prove is a reordering — a rewrite that
  silently changes a value is precisely the failure the conversion is meant to
  expose.

  The files, with their multi-key `map[...]` expectation counts (measured):
  star_body_cte_join_leg_fdb_test.go 128, chained_unnest_predicate_pushdown_fdb_test.go 82,
  chained_unnest_3link_filtered_ordinal_fdb_test.go 62, nested_left_box_chained_unnest_fdb_test.go 52,
  buried_chained_rotation_fdb_test.go 52, exists_scope_shadow_fdb_test.go 41,
  fullbox_chained_spine_fdb_test.go 29, cross_leg_duplicate_column_box_unnest_fdb_test.go 27,
  baretwin_gather_fdb_test.go 26, orderby_gather_fdb_test.go 20, withinbox_dup_fdb_test.go 19,
  buried_element_predicate_fdb_test.go 10, nullsupply_barrier_fdb_test.go 9,
  fork_colliding_subfield_fdb_test.go 6, projected_exists_enclosure_lift_fdb_test.go 1
  (all under pkg/relational/sqldriver/). ~564 expectations.

  DONE when `grep -rn "executor.RowValue(" pkg/relational/sqldriver/*_test.go`
  returns only the renderer definitions themselves and sites whose rows are
  single-slot (no order to assert).


- [ ] **CQ-69 (L, multi-phase, per-phase gates) — build the RFC-201 layered test
  corpus ladder.** Design: `rfcs/201-layered-test-corpus.md` (merged, #542),
  which this item cites rather than restates. Layer 1 is Java's own acceptance
  suite vendored verbatim (**238 `.yamsql` files, 2,997 query stanzas**; root
  corpus 94 files / 2,691 queries — measured by the 2026-07-31 scoping study and
  re-measured on every re-vendor); Layer 2 multiplies it through oracles that
  need no hand-written expectations; Layer 3 generates and COMMITS blessed
  tests. The phases below are independently landable and are checked off
  individually.

  - [x] **69.0 — Phase 0: vendor + parse.** No execution. **MERGED.** What it
    carries, read off commits `f20c884a4` + `a076ba66c`: 238 `.yamsql`
    files vendored byte-for-byte under `third_party/` mirroring the upstream
    path, `VERSION` pinned to 4.12.11.0, `.metrics.*` excluded (and with them
    `metrics-diff/` entirely); the `javayamsql` parser plus `TestCorpusParses`
    over all 238, each file either parsing clean or refused for the exact reason
    upstream refuses it; block/command/config key and YAML tag as CLOSED
    switches, while option maps stay open because Java's are — it probes with
    `containsKey` and never checks for leftovers — so an unrecognised option key
    is recorded as an `InertDirective` rather than rejected; and `manifest.go`
    holding the polarity Java keeps in its test classes. The mutation matrix is
    reported at 8 directions; that count was NOT re-verified from the branch's
    commits here, so confirm it on merge.

    **Polarity discrepancy vs RFC-201 §3 ruling 3: RESOLVED, and the RFC was
    the loser — but so was the branch's own commit message.** The RFC's scoping
    study said "33 files under `shouldFail/`" and "10 include-only fragments";
    `a076ba66c`'s message said "25 parse-level, 35 execution-level, 2
    fragments". COUNTED off master, the manifest it shipped actually held **25
    parse-level, 46 execution-level, 2 fragments** — so the "35" was a third
    unverified figure, and this entry repeated it until the counts were pinned.
    The implementation's METHOD is right (each file's polarity read off the
    Java test class that asserts it, and "negative" only splits into
    parse-level and execution-level once a parser exists to draw the line); it
    was only the reported total that was wrong. `TestManifestComposition` now
    pins the composition so no figure here has to be trusted again. §3 has been
    corrected to the measured numbers.

    **69.1 then re-measured the EXECUTION-level half by RUNNING it, and
    reclassified 13 files.** Master's manifest carried 46 execution-level
    entries; it now carries 42, and the manifest totals are pinned by
    `TestManifestComposition` (84 entries: 25 parse / 42 execution / 9
    fixed-version / 2 fragment / 6 explicit-positive) so no prose has to quote
    them from memory. The 13 moves:

    - **4 → Positive.** `initial-version/wrong-{result,count,unordered,error}-less-than`.
      A Go run is a current-version run, and `InitialVersionTest` carries
      separate `shouldPassOnCurrent` / `shouldFailOnCurrent` streams; all four
      are in `shouldPassOnCurrent` (:164-167) because their less-than branch is
      never selected, so the wrong expectation they carry is never checked.
    - **9 → FixedVersionMeta** (a new polarity). 8 under `supported-version/`
      plus `initial-version/less-than-version-tests`: no current-version stream
      runs them at all, so a single-current-version runner cannot evaluate them
      in either direction. They are still EXECUTED and asserted not to
      quietly pass — Go can measure what Java has no stream for.

    Of the 42 remaining execution-level entries the ledger books **20** as
    `polarity:negative-execution` — they run and fail, as upstream requires.
    The other 22 measured as 15 whose expected failure is unreachable because
    the directive carrying it is itself a counted skip, plus 7 claimed first by
    a DDL or engine gap. That 20/15/7 split is asserted by the corpus-run test,
    not asserted here: a manifest ENTRY count and a ledger OUTCOME count are
    different denominators and fusing them is how a corrected figure becomes a
    wrong one.
  - [x] **69.1 — Phase 1: runner core, plan assertions suppressed.** Built as
    `pkg/relational/conformance/javacorpus`, executing all 238 vendored files
    against real FDB. Multi-document blocks with `include:` splicing, the
    five-statement `schema_template` lifecycle against the catalog connection,
    the `connect:` registry (integer / `(global) n` / `0` = catalog / verbatim
    `jdbc:embed:` URI), `test_block` preset+options layering with Java's real
    defaults (repetition 5, `check_cache` on), a seeded Fisher–Yates shuffle over
    java.util.Random for non-ordered modes, `connection_lifecycle`, and
    `result` / `unorderedResult` / `count` / `error` checked through a port of
    Java's `Matchers` (positional vs named cells, Integer→Long and Float→Double
    promotion, `x'…'` / `xstartswith_N'…'` bytes, `!l !f !b !ignore !null
    !not_null !sc !uuid !pos !randomStr`).

    **MEASURED LEDGER (pinned; `pkg/relational/conformance/javacorpus/pinned_ledger_test.go`):
    pass=32, fail=0, skip=206, 512 asserted queries.** Largest classes:
    `unsupported-DDL:value-index-as-select` 42, `unsupported-DDL:struct` 39,
    polarity/meta 56, `unsupported:temporary-function` 17, `plan-assertion` 7
    files / 212 configs. `queries` excludes `noChecks` queries: they execute but
    assert nothing, and counting them let a file whose ONLY query is
    config-less (`scenario-tests.yamsql`, "# TODO: add data" upstream) report a
    pass — the vacuous-pass guard was defeated by the very branch that books
    the skip. A per-file `path status class` digest is pinned beside the counts,
    because totals alone are blind to two files SWAPPING classes.

    **§3's "~95 files green" target is SUPERSEDED by this measurement**, and the
    reason is not that the runner fell short — the target was formed before
    anyone had run the corpus. The real decomposition: **100 files are outside
    Phase 1 by construction** (56 meta-tests, 7 plan-assertion-only, 35 needing
    a later phase's capability, 2 asserting nothing), leaving **138 in scope —
    32 passing and 106 blocked by an engine or DDL gap** (87 DDL: 42
    value-index-as-select + 39 struct + 6 other; 19 engine divergences). So the
    denominator is 138, and the gap to it is one DDL feature (CQ-71, bigger than
    struct types and unmeasured before this run) plus struct types. §3 has been
    corrected with the same arithmetic.

    Skip policy is the product and it holds: every non-executed file, block,
    query and config is counted with a reason class, `explain:` /
    `explainContains:` / `planHash:` are counted skips per ruling 2, a positive
    file that asserted nothing is booked `vacuous:all-assertions-skipped` rather
    than reported as a pass, and the 21 engine divergences the run FOUND are
    each pinned to their exact rejection (CQ-71/72/73) so a file that starts
    passing, or starts failing differently, reds the run.
  - [ ] **69.2 — Phase 2: `resultMetadata:` and per-page continuations /
    `EXECUTE CONTINUATION`. HALF LANDED; the continuation half is a STOP for
    the RFC + Graefe gate.**

    **§3's framing of this phase — "Runner- and driver-side; no planner risk" —
    is REFUTED for the continuation half, and by measurement rather than
    argument.** It was written before anyone probed the engine. The `+35` and
    `+16` file estimates were also counted off `grep`, not off a run.

    **CENSUS, DERIVED — join each directive's file list against the ledger's
    per-file class assignment; it re-runs verbatim.** An earlier draft of this
    entry said "26 claimed by DDL", which is wrong under every reading; the
    derivation is stated so the figures can be checked rather than trusted.

    `resultMetadata:` — 35 files: **20 claimed by a DDL class** (17
    `unsupported-DDL:struct` + 3 `unsupported-DDL:value-index-as-select`); 5
    `polarity:negative-execution` (the scalar `shouldFail` files, which now
    genuinely execute and fail); **4 not skipped at all** — they pass, and now
    assert the directive; 2 `unsupported:continuation`; 2
    `engine-gap:array-literal-values`; 1 `engine-gap:planner-declines`; 1
    `polarity:negative-parse`. So 31 of 35 are booked under some skip class and
    4 pass; the DDL-claimed subset is 20.

    `maxRows:` — 16 files: **9 claimed by a DDL class** (4 struct + 4
    value-index + 1 other); 2 `polarity:negative-parse`; 1
    `engine-gap:array-literal-values`; 1
    `conformance:go-accepts-what-java-rejects`; and **only 3 genuinely blocked
    on pagination** (`unsupported:continuation`). 13 of the 16 are held by
    something other than the continuation surface.

    **resultMetadata: DONE.** A 1:1 port of `CheckResultMetadataConfig` lives in
    `pkg/relational/conformance/javacorpus/resultmetadata.go` — the recursive
    expected-side syntax whole (`[{ID: BIGINT}]`, `{PT: [{X: BIGINT}]}`,
    `{PTS: {array: [...]}}`, `{X: {array: {array: INTEGER}}}`, the optional
    leading struct-type-name, the case-insensitive `array` key, the
    `isArray` flag that keeps a plain field list from matching an
    array-of-struct column), plus Java's sticky-metadata semantics in
    `testblock.go` (`QueryCommand.executeInternal` never executes the directive
    on its own — it arms an inline check the next result-consuming config
    performs before drawing rows).

    **MEASURED LEDGER, before → after** (`pinned_ledger_test.go`; `fail=0`
    throughout, `pass=32` unchanged):

    - `unsupported:result-metadata` **7 file / 65 config → the class is
      DELETED.** Not zeroed and kept: every reachable directive is now either
      asserted or booked under the narrower successor.
    - `polarity:negative-execution` **20 → 24**, accounting **20/15/7 →
      24/10/8**. Four of the five scalar
      `check-result-metadata/shouldFail/{extra-column, missing-column,
      wrong-column-name, wrong-column-order}.yamsql` moved from
      "assertion-suppressed" to "booked" — their only failing assertion IS the
      metadata one, so while the directive was a counted skip they ran clean.
      `wrong-column-type.yamsql` did too. THAT split, not the class counts, is
      the measurement showing the directive is really checked.
    - `engine-gap:array-literal-values` **5 → 6**, and the reason is a REAL
      DEFECT this branch found in the polarity accounting itself. The arm
      credited a negative for ANY error, so
      `check-result-metadata/shouldFail/wrong-array-element-type.yamsql` was
      booked as a passing negative while dying on the array-literal INSERT in
      its `setup:` block — the same gap its `shouldPass` sibling books — having
      never reached the descriptor mismatch upstream is testing. Setup failures
      now carry a `setupError` type and disqualify the credit, and the file is
      booked in `gaps.go` where it belongs.
    - **A second mis-credit surfaced and was REFUTED as a defect.**
      `include-block/shouldFail/verify-all-includes-execute.yamsql` also dies in
      setup — but for that file the setup step IS the assertion (it includes a
      fragment twice so the second pass re-inserts an existing primary key, and
      the 23505 is the point). The gate was too coarse; `SetupNegatives` in
      `gaps.go` declares the exception with its reason, and the corpus test
      asserts the entry is still true, so it cannot rot into a blanket
      exemption. **All 24 booked negatives were then audited** against their
      manifest reasons using the newly recorded failure text — the six
      `include-block/includes/*` credits match "standalone: no database
      available" exactly, the `supported-version/*` ones match "the broken query
      runs", and no further mis-credit exists.
    - `unsupported:continuation` **1 → 3 files**.
      `check-result-metadata/should{Pass,Fail}/*-metadata-on-continuation-page.yamsql`
      were booked against the metadata skip; with the directive implemented
      their real blocker is visible, and it is pagination.
    - `queries` **512 → 487**. The five negatives now stop at their first
      repetition instead of asserting rows five times each. Fewer asserted
      queries is the CORRECT direction here: those 25 were never proving
      anything about the engine.
    - `unsupported:result-metadata-nested` (new) is **MASKED at 0** — the **14**
      files whose expectation descends (12 of them behind struct DDL, measured
      by `TestDescendingMetadataCorpusCost`) are all claimed earlier. Recorded
      in `maskedClasses` with the masking classes named, per the standing rule
      that a class nothing emits is worse than no label.

    Mutation-checked in seven independent directions, each RED alone: matcher
    always-matches (5 files run clean → negatives that wrongly passed); type
    names unchecked (only `wrong-column-type` reds); the descend gate disabled;
    the `isArray` distinction removed; the struct-type-name check removed; the
    live array sentinel's type-name arm; its scan-type arm.

    **The four stricter-than-Java hard errors are gone.** A first draft rejected
    a `resultMetadata:` armed for `error:` / `count:` / a non-row-returning
    statement, or left with no consumer. All four are LEGAL corpus input —
    `QueryConfig.validateConfigs` admits `error:` and `count:` as the required
    consumer — and Java no-ops on each via
    `checkInlineMetadataIfPresent`'s `instanceof RelationalResultSet` guard, so
    the runner would have rejected files Java accepts. That guard is now ported
    as `metadataIsRead` and table-tested, and the positional walk it depends on
    is extracted as `classifyConfigs` and table-tested too — which is the only
    way the ORDER-dependent decisions (which consumer a directive attaches to,
    what an `initialVersion` marker removes) can be asserted at all, since none
    of them is visible in the rows.

    **Divergence found and BOOKED as CQ-74:** the result-metadata pipeline
    truncates a column's type to one flat string at `executor.ColumnDef`, so an
    array column is spelled by its ELEMENT type where Java says `ARRAY(elem)`,
    and a struct column carries no fields and no declared type name. Costs 0
    corpus files today, 8 the day Phase 3 lands.

    **Continuations: STOP, not deferral.** The runner-side walk is ~40 lines and
    is not the work; the engine has no surface for it to drive, and building one
    reverses a recorded RFC decision. RFC-181 WS-C C2 posed exactly this choice
    — "adopt ContinuationProto + hashes, or declare Go SQL tokens
    engine-private" — and the implementation took engine-private, pinned by
    `TestOptContinuation_RejectsLoudly`
    (`pkg/relational/core/embedded/continuation_option_test.go:18`) against the
    0A000 at `cascades_generator.go:1215-1218`. Four pieces are absent, measured
    against Java 4.12.11.0:

    - **(A) a page terminated by a caller-chosen row count that MINTS a token.**
      Java sets MAX_ROWS as `ExecuteProperties.setReturnedRowLimit` per
      execution (`QueryPlan.java:434`) — it is a PAGE SIZE. Go's `OptMaxRows` is
      a statement-wide TOTAL cap that ends in a silent `io.EOF`
      (`cascades_generator.go:1470`), pinned as such by
      `TestFDB_RFC106a_MaxRowsStatementWide`. Pages currently end on a scan/time
      budget the caller never chose.
    - **(B) a caller-visible way to read the token.** `api.ResultSet.Continuation()`
      exists (`api/resultset.go:44`) with ZERO production callers; the SQL path
      keeps the bytes in `paginatingRows.continuation`, an unexported field of
      an unexported type behind `driver.Rows`.
    - **(C) a self-describing token envelope — WIRE FORMAT, the hard line.**
      Java's `ContinuationImpl` wraps a `ContinuationProto` carrying version,
      execution_state, reason, binding_hash, plan_hash and a serialized
      `CompiledStatement` (`QueryPlan.enrichContinuation:452`). Go has no such
      proto; its tokens are raw operator-private bytes.
    - **(D) a resume entry point.** The grammar already parses
      `executeContinuationStatement`
      (`pkg/relational/core/parser/grammar/RelationalParser.g4:683`) — vendored
      from Java's — and NOTHING implements it: the admin dispatch rejects it
      with "only SHOW administration statements are supported"
      (`cascades_generator.go:182`). Java's handler is
      `PlanGenerator.generatePhysicalPlanForCompiledStatementContinuation`
      (`PlanGenerator.java:313`), which reconstructs the plan from the token and
      REJECTS on a plan-hash mismatch via `PlanValidator`.

      C alone is a new wire-format proto plus a plan-hash validation gate; D is
      a new statement kind in the planner. That is an RFC with a Graefe ACK
      before implementation, not a runner phase — and it is the correct next
      unit of work, since it is also the prerequisite for 69.4's
      ForceContinuations oracle.

    DONE (remaining) = the RFC for C+D lands with its ACKs, the engine gets the
    envelope + resume entry point + per-execution page size, and the runner's
    multi-page walk (sticky metadata re-checked per page, Java's
    `exhausted`/`atBeginning` assertions) moves the 3 continuation files and the
    16 inner skips.
  - [ ] **69.3 — cross-engine differential wiring (§4.1). Any time after 69.1;
    do it early.** Every query on both engines — Go in-process, Java via the
    conformance server (`plandiff.SetupRunner.RunWithSetup` is the existing entry
    point) — rows must agree, and where both error the error CLASS must agree
    (the conformance principle). This pays before any engine work lands: the Java
    leg has the full DDL surface, so every Phase-3/4 gap becomes a measured
    per-query divergence instead of an estimate.
  - [ ] **69.4 — the oracle layer (§4.2, §4.3).** ForceContinuations as a Go
    execution mode: every SELECT re-executed with forced `maxRows=1`, the
    reassembled pages equal to the one-shot result row for row, order for order.
    And plan-diversity agreement: execute the memo's LOSERS too — every
    alternative surviving to a costed candidate must return the winner's rows.
    That second one is the strongest planner-bug detector this repo has; the
    wrong-window and signed-zero-DISTINCT defects were both of its class (two
    plans, one query, different rows). Instrument: an executor-side harness
    enumerating the memo's plan set per corpus query, bounded per query, with the
    per-run plan-pair count REPORTED AND FLOORED — an oracle without a gated
    instrument is prose, not coverage.
  - [x] **69.5 — the factory pipeline (§5): generate → execute → bless-or-file →
    dedup → commit.** · **DONE, merged `491e02a7c` (#555).** Every clause below is
    satisfied and MEASURED: generation is grammar-driven, seeded and deterministic
    with generator version + seed in every emitted header (generation never reads
    the clock — the date is passed in — which is what lets a committed file be
    regenerated byte-identically); dedup keys on the explaindiff plan shape PLUS
    the spec feature vector; the dedup census is part of every run's output and is
    frozen as `census_baseline.json` behind a componentwise ratchet (scenarios,
    tests, per-feature-vector AND per-blessing); the per-run quota keeps batches
    reviewable and the nightly lane opens a PR rather than pushing.
    First batch, measured: 1200 seeds → 2268 candidates → **965 blessed → 900
    committed** (894 distinct feature vectors); at HEAD the corpus stands at
    **2000 scenarios / 8000 tests**, blessings 1785 `metamorphic` + 217
    `metamorphic-tlp-only`. 1599 TLP partitions and 3226 second-plan pairs, ZERO
    disagreements, every oracle mutation-proven armed.
    **"Oracle disagreement is a bug, fix now" was honoured on the first sweep**:
    64 of 413 `(p) IS NULL` renderings failed to plan, root-caused to
    `walkRecordConstructor` discarding the operand position on every paren-unwrap,
    fixed with a three-valued `walkPos` that closed six shapes at once, and pinned
    — the item's own standard, met by the item's own first run.
    **Standing caveat, not a gap in this checkbox:** blessing is metamorphic, so
    the corpus proves self-consistency, not Java agreement; the Java leg is
    environmentally unreachable here and every header is labeled so, promotable
    without regeneration.
    Blessed tests are COMMITTED as permanent suite content, not
    regenerated: a frozen expectation keeps testing the engine even if the
    generator, the oracle infra, or the Java server is broken or gone, and it
    converts oracle agreement (a moment-in-time fact) into a regression pin (a
    permanent one). Grammar-driven generation from the actual ANTLR grammar,
    weighted toward feature COMBINATIONS, seeded and deterministic with generator
    version + seed recorded in every emitted test. Oracle disagreement is a bug:
    auto-minimize and file the minimal reproducer for immediate root-cause under
    the standing fix-now rules, committed as a pinned regression WITH the fix.
    Dedup on plan fingerprint + feature vector, with the dedup census (candidates
    seen / points covered / committed) part of every run's output — committing
    90k variants of one shape is volume without coverage. Per-run commit quotas
    (1–5k) keep PRs reviewable.
  - [ ] **69.6 — Phase 3: struct types** (`create type as struct` + struct-typed
    columns; 41 files, the single biggest engine gap). **NOTE: this is
    query-engine + DDL scale and REQUIRES ITS OWN RFC AND A GRAEFE GATE BEFORE
    IMPLEMENTATION.** RFC-201 is the corpus design, not this feature's design; it
    does not authorize starting here.
  - [ ] **69.7 — Phase 4: SQL functions** (`create function`, temporary
    functions; 44 files), **views** (11), **enums** (3).
  - [ ] **69.8 — Phase 5: parameter injection + prepared-statement mode** (13
    files), **then the long tail** — proto-descriptor schemas, vector/semantic
    search, bitmap indexes, `copy_block`.

  Two standing rulings from §3 that constrain every phase: the corpus is
  VENDORED VERBATIM and never rewritten (re-sync is a plain rsync from the tagged
  Java checkout, moving in lockstep with the pinned version; the `.metrics.*`
  companions are deliberately NOT vendored — they assert Java-Cascades-internal
  task/transform counters that are meaningless for a different planner and would
  only rot), and there is NO Java-format plan renderer.


- [ ] **CQ-82 (MED, flake, OBSERVED ONCE, NOT ROOT-CAUSED) —
  `//conformance:conformance_test` failed once in a full-suite run and has not
  reproduced.** · S to reproduce, unknown to fix
  Recorded because "there are no unrelated flakes", and recorded HONESTLY:
  this is an unexplained red, not a closed one.
  **What is known.** In a full `just test` at `98d79a2ef` (all targets fresh),
  the run reported `Executed 18 out of 73 tests: 69 tests pass, 2 fail locally,
  and 2 were skipped`, with `//conformance:conformance_test FAILED in 524.1s`.
  **What is NOT known, and why — my own error:** that run's output was piped
  through `tail`, so the failure message and the IDENTITY OF THE SECOND FAILING
  TARGET were both discarded and are unrecoverable. Never pipe a suite run
  through `tail`; redirect the whole log.
  **Reproduction attempts, both negative.** (1) `//conformance:conformance_test`
  alone, fresh: PASSES in 196s. (2) The whole suite with
  `--nocache_test_results`, every one of the 73 targets executed under the same
  4-way `--local_test_jobs` concurrency: `Executed 73 out of 73 tests: 73 tests
  pass`. So it did not reproduce under conditions matching the failure.
  **Leading hypothesis, UNCONFIRMED — resource starvation, not a logic defect.**
  The failing run took 524s against 196s isolated, ~2.7x. The conformance suite
  runs a pool of 8 JVMs at ~250-400MB each (`defaultA3PoolSize`,
  `conformance/java_invoker_test.go`) plus a fresh JVM per
  `NewIsolatedJavaInvoker`, alongside 3 other container-heavy targets. That
  file's own comment already documents the failure mode this would produce:
  "state-leaks … compound into Java-side hangs at >30s per-request latency".
  Host at the time: 62GB total, ~14GB available.
  DONE = the failure reproduced with its message captured (try `--runs_per_test`
  on the suite, or re-run the full suite fresh under memory pressure), THEN root-
  caused. If it is starvation, the fix is a resource constraint on the target
  (`exec_properties` / a lower `CONFORMANCE_A3_POOL_SIZE` under load), not a
  retry. **Do not close this by observing another green run** — three greens are
  already recorded above and they did not settle it.

- [ ] **CQ-92 (conformance): live-JVM byte conformance for the CARDINALITY()
  index key.** `TestFDB_ArrayCardinalityIndex/"wire bytes of the NOT NULL
  cardinality index entries"` asserts the raw index KVs an empty / populated
  NOT NULL array writes (`0x14` for cardinality 0, `0x15 0x01`, `0x15 0x02`),
  and it MEASURES the Go side by reading it back out of FDB. The Java side it
  claims to match is INFERRED FROM SOURCE
  (`CardinalityFunctionKeyExpression.java:115-117` + the tuple codec), with no
  JVM in the loop — the caveat is stated in the test. For a wire-compat
  assertion that is the weak half, and it is weak precisely where the claim
  lives. DONE = a `cardinality_index_conformance` pair in the live-JVM
  conformance harness (the same shape the existing byte-conformance pairs
  use) that writes the three-row `tab1_nn` fixture from BOTH engines and
  asserts the index subspace is byte-identical, at which point the caveat
  comment comes out.


- [ ] **CQ-99 (instrumentation): `MaxNumMatchesPerRuleCall` is NOT a fan-out
  bound and must stop being cited as one.** MEASURED: the counter increments per
  MATCHER BINDING (`unified_tasks.go:350`, `:461`, `:560`), not per
  `call.Yield()`. The RFC-220 enumeration loops yield up to N times inside a
  single `OnMatch` and never touch it. The operative backstop is the far coarser
  `Planner.MaxTasks` / `MaxTaskQueueSize`, which fails the WHOLE plan with
  `ErrPlannerCapHit` rather than capping one rule's fan-out. Consequence:
  "`MaxNumMatchesPerRuleCall` was never approached" is evidence of nothing,
  because it was never the operative bound. A real per-rule fan-out claim needs
  measured yield counts, and there is currently no instrument that produces
  them. Build one, or stop making the claim.

  STATUS AFTER THE NAK ROUND: the claim is withdrawn everywhere it was made (PR
  body, `physical_wrapper.go`), and the item stays OPEN rather than being closed
  cheaply. The reason is worth stating so it is not re-litigated: a
  yield-counting instrument is easy to bolt on as a process-global counter, and
  that is exactly the shape this repo has been burned by — a census whose arms
  are driven only by whatever the corpus happens to reach. The RFC-220 round did
  build a throwaway counter of this kind (to settle which of Java's asserts hold
  in Go, see `assertMembersOf`), and its numbers were only trustworthy because
  every reading was cross-checked against a deliberate mutation. A permanent
  fan-out instrument needs the same discipline plus per-rule attribution and a
  vacuity floor, which is its own change with its own review — not a rider on
  this one.

  What IS now known, and did not need the instrument: the filter loop was inert
  before this PR and genuinely yields N parents after it, so whatever headroom
  exists is being used for the first time. Zero plan movement across the 7000-
  scenario corpus is the evidence that the fan-out is not pathological today.


- [ ] **The FDB testcontainer dies ~34 minutes into the rowdiff sweep, on every
  night measured — the ROOT cause under the wedge above, and it is still open** ·
  size unknown until reproduced · found by the same investigation
  The fix above bounds the DAMAGE of the cluster dying. It does not explain why
  the cluster dies, and the timing says it is systematic rather than
  infrastructural bad luck. MEASURED, elapsed from the `random_seeds` RUN line to
  the first INFRA line, four consecutive nights:
  ```
  run 31350088492  1909 seeds  33m60s
  run 31290367277  2111 seeds  34m17s
  run 31234733111  2235 seeds  34m47s
  run 31143737482  2304 seeds  33m27s
  ```
  It tracks ELAPSED TIME, not work done — the seed counts vary by 20% while the
  time does not. First symptom is
  `WARN fdbgo: connection to server failed address=172.16.0.3:4500`, and
  thereafter a TCP connect that hangs rather than being refused, i.e. a blackhole
  and not a listener that went away cleanly.
  Widened to EIGHT nights across FIVE distinct runners (drain-0/1/2/3 and
  gh-runner-fdb): 31m39s, 32m46s, 31m31s, 32m27s, 33m47s, 33m17s, ~34m00s,
  31m45s — mean 1959s, sigma ~55s (2.8%). Five hosts holding 2.8% rules out a
  single wedged box: this is software-deterministic.
  DID NOT REPRODUCE LOCALLY in a 42-minute run of the same sweep, and the reason
  is itself the lead. `pkg/testcontainers/foundationdb/foundationdb.go:166-208`
  mounts `/var/fdb/data` as a SIZE-LESS tmpfs, i.e. 50% of host RAM — ~32 GB on
  the dev box against ~3.78 GB on a 7745 MB runner — so a tmpfs page allocation
  can never fail here.
  RULED OUT, each with evidence: Ryuk reaping (the `Reaper.connect` goroutine is
  alive in the 4h50m dump; its timeouts are 1m/10s, not 33m); the client giving
  up (`pkg/fdbgo/client/topology.go:23-62` polls every 5s forever, and it is a
  FRESH dial that hangs); the memory storage engine filling
  (`KeyValueStoreMemory.actor.cpp:152-156` returns `Never()` on out-of-space
  rather than exiting, and measured growth — 21.7 MB at 16min, 63.6 MB at 42min
  of a 1 GiB budget — needs ~10h); an fd leak (23 fds against a 1024 soft
  limit); a slow-down ramp before the cliff (seed rate was 1.16/s at t+17m,
  1.26/s in the final 306s — flat right up to the stop).
  MECHANISM REPRODUCED DELIBERATELY: filling the data tmpfs makes fdbserver log
  `Fatal Error: Disk i/o operation failed`, trace
  `SharedTLogFailed`/`io_error`/1510 then `StopAfterError`, and exit 1 with
  **`OOMKilled=false`** — with `RestartPolicy=no`, so the container Exits and its
  bridge IP blackholes, which is why the CI stack sits in `net.(*netFD).connect`
  instead of taking an RST. A live container with a dead process would refuse the
  connection. The false OOM flag is why this has looked inexplicable.
  **NO MECHANISM IS ESTABLISHED.** That is a positive statement, not an
  omission: the two memory hypotheses that fit the signature were each pursued
  and each REFUTED by measurement, and nothing has replaced them. Recorded in
  full because a refuted hypothesis is a result, and because both are the kind
  that will be re-proposed by the next person who reads the io_error trace.
  1. **tmpfs-as-consumer — REFUTED.** `foundationdb.go:179` really does mount
     `/var/fdb/data` size-less (`{"": }`), so the kernel caps it at 50% of RAM,
     ~3872 MB on a 7745 MB cpx32; that much is confirmed from source and is
     worth bounding on its own merits. But it does not GROW into that ceiling:
     measured during the sweep it holds ~165 MB at 42min and ~130 MB at the
     death point — about 4% of its cap. Re-confirmed by re-running the sweep
     with the tmpfs explicitly pinned to the CI size (`size=3780m`): usage
     stayed 1..181 MB and nothing failed. Bounding the tmpfs would bound a thing
     that is not growing, so it CANNOT be the fix for this death.
  2. **ENOSPC via host RAM + swap exhaustion — REFUTED.** The reframe (tmpfs as
     victim rather than consumer: host pages and the 4 GiB swapfile exhaust, a
     tmpfs write returns ENOSPC, and the path in the paragraph above runs) fit
     every piece of negative evidence including `OOMKilled=false`, which is what
     made it worth measuring rather than believing. It fails on arithmetic. The
     never-measured term was the Bazel server JVM, which is UNCAPPED in this
     lane (unlike the race lane's `-Xmx3g`); isolated to this worktree's own
     server it is 2268 MB. Summed at t+33min against 7745 MB: 728 baseline
     (`container_memory_tag_test.go:100-102`) + 674 test + 2268 bazel + 253
     fdbserver + 130 tmpfs = **4053 MB, 52%** — needing ~217 further minutes of
     leaking to exhaust, and still only ~62% if CI's heap were 3 GB. The box is
     about half full when the container dies.
  WHAT WOULD DISCRIMINATE NEXT, because it will not be in anyone's head
  tomorrow: ~12 minutes on a QUIET box — the constraint is isolation, not
  duration, since the slope settles within minutes. Poll by this worktree's own
  `--output_base` rather than `grep bazel | grep java`, by the container ID the
  test actually created rather than `head -1` of an ancestor filter, and read
  RSS from the Bazel test PID. The first attempt at this measurement was
  WORTHLESS for exactly that reason — on a box with 14 sibling Bazel servers the
  `bazel` column summed all of them and the `test` column swung 45..1674..268 MB
  by matching other agents' processes, which is not a leak curve and a slope
  drawn through it would have been a confident wrong answer. Only a summed slope
  above **~110 MB/min** could reach 7745 MB by t+33min; the measured leak is
  ~17 MB/min, an order of magnitude short.
  IN FLIGHT: this change adds a "Capture FDB container forensics" step
  (`docker inspect` exit/OOM, `docker logs`, `Severity="40"` trace events, host
  `free`/`df`/kernel OOM lines) that runs on success or failure. Its three
  outcomes are mutually exclusive — exit 1 + io_error/1510 = tmpfs ENOSPC;
  exit 137 + OOMKilled=true = kernel OOM-kill; exit 143 + no fatal trace =
  stopped externally — so the NEXT run settles this without further guessing.
  DONE = mechanism confirmed from that artifact, the cause fixed rather than
  absorbed by the circuit breaker, and a pin that reds if the sweep's usable
  lifetime regresses below its budget again.


- [ ] **`sqldriver_test` leaks ~17 MB/min under the rowdiff sweep and blows past
  its own declared Bazel memory tag in about half an hour** · S-M · found while
  root-causing the rowdiff container death · a real defect on its own; it is NOT
  the cause of that death
  TWO CLAIMS, DELIBERATELY SEPARATED, because conflating them is how a real but
  unrelated defect gets written up as a root cause:
  1. the leak DOES breach the target's own declared budget — ~17 MB/min against
     `resources:memory:700`, exceeded in ~30 minutes. Bazel schedules co-located
     targets against that declaration, so it is wrong for the whole box.
  2. the leak DOES NOT account for the container death at ~33 minutes. It was
     the leading candidate for supplying the memory pressure in the ENOSPC story
     above, and that story is refuted on arithmetic: at the death point the leak
     has reached ~674 MB — still INSIDE its 700 MB declaration — and the summed
     host usage is ~52%. Fixing this leak should not be expected to stop the
     container dying.
  MEASURED over one 42-minute local sweep, process RSS: 249 MB at 8min, 300 MB
  at 11min, **808 MB at 42min** — roughly linear at ~17 MB/min, so ~675 MB by the
  33-minute mark and multiple GB over a full-length job. The target declares
  `tags = ["resources:memory:700"]`
  (`pkg/relational/sqldriver/BUILD.bazel:602`), which the sweep exceeds in ~30
  minutes; Bazel schedules against that number, so every co-scheduled target on
  the box is sized against a figure the sweep stopped honouring half an hour in.
  This is a real defect independent of the outage. It is ALSO the most plausible
  supply of the memory pressure in the tmpfs-ENOSPC hypothesis above, which is
  why the two are booked together — but the leak stands on its own measurement
  and does not depend on that hypothesis being right.
  NOT YET LOCATED. The obvious suspects are per-seed state the loop accumulates
  (nothing drops the ~2000 schemas each sweep creates —
  `grep -rn "DROP SCHEMA|DROP DATABASE" pkg/relational/conformance/rowdiff/`
  returns 0 hits) and per-seed `sql.DB` handles opened in
  `rowdiff.RunCase` (`pkg/relational/conformance/rowdiff/run.go:104-120`).
  DONE = the retention named from a heap profile, fixed, and a pin that fails if
  RSS growth per seed regresses past a declared bound.


### factorycorpus/full stalls at 6x its runtime, and master cannot see it

`//pkg/relational/conformance/factorycorpus/full:full_test` consumed **3606s**
(100% of its `eternal` budget, TIMEOUT) on a run whose assertions were entirely
green — 0 failing. Re-run on the **identical SHA**: `PASSED in 392.9s`. Master
baseline at `82275082a`, uncached: **390s**. So the stall is ~9x with no code
difference, and it is not a regression: same-box A/B over identical 8151 cases
measured merge-base 261s/211s vs branch 218s/195s.

**Mechanism (measured, from the CI goroutine dump at the wall):** 4934 goroutines
parked at `t.Parallel()`, only 4 inside `RunScenario` — waiting for a slot, not
doing work. Contention, not workload. The target's own `BUILD.bazel` records a
prior identical 3612s timeout with the same diagnosis, so this recurs.

**Why nobody sees it:** on master this target is served from cache
(`(cached) PASSED in 404.6s`) on every commit that does not touch
`core/embedded`. It genuinely executes only when that package changes, so the
real cost is invisible most of the time — and a local `just test` reports
`Executed 6 out of 88 tests: 88 tests pass` while never running it. Master is
exposed to exactly the same stall and would not report it either.

**Do not raise the timeout budget.** The budget gate is what caught this; moving
it converts a measured stall into an invisible one. Its message is already
explicit that the next occurrence burns the whole budget and blocks the merge
queue.

Also stale while you are here: that `BUILD.bazel` comment still says 5000 cases;
the corpus is **8150** (roughly doubled on 2026-08-11 by #720).

Open question, and the reason this is booked rather than fixed: whether the
contention is `--local_test_jobs=4` interacting with per-scenario parallelism, or
a runner-level resource limit. Both are checkable; neither was checked.



### Self-hosted CI runner — action-archive cache
- [ ] Give the `hetzner-fdb-vm` runner an action-archive cache so it stops re-fetching
      action tarballs from codeload per job — the runner reads
      `ACTIONS_RUNNER_ACTION_ARCHIVE_CACHE` for exactly this. The runner is provisioned
      from `infra/main.tf`, so this is a terraform change plus an apply, which is why it
      is booked rather than done inline. Verify by re-running any `hetzner-fdb-vm` job
      and confirming its log passes the "Download action repository" step.

- [ ] While there: a job that dies before its first real step should be
      distinguishable from a job whose tests failed. Right now both render as a red
      check, and the only way to tell them apart is a 21-line log — which is how this
      spent a while looking like a code regression.


### The FDB testcontainer cluster stalls under concurrent CI load, on master too

**Not this branch, and not a flake to wave away.** Both reds carry the identical
signature:

```
XX000: open catalog store: failed to read store info: context deadline exceeded
```

- PR CI's race lane: two `sqldriver` tests, on a runner shared with another
  branch's CI.
- **master's Nightly RowDiff**: TEN consecutive seeds, at exactly 60-second
  intervals, tripping the sweep's own `ROWDIFF_CLUSTER_DEAD` guard — which
  states the case better than this entry could: "That is not a flaky seed, it is
  the cluster gone … which is how this sweep once consumed a CI runner for
  4h21m."

`TestFDB_MetamorphicPagingAtScale` under `-race` PASSES locally (1766s, green,
same commit) on a machine not running a second CI concurrently. So the evidence
points at the shared runner, not at the branch.

What is NOT established, and is the actual work: whether the CONTAINER dies or
whether the pure-Go client wedges against a healthy cluster. The second would be
our bug — "C++ is the spec for the FDB client". The 60-second cadence is the
sharpest clue and is unexplained: no `WithTimeout` on the sqldriver query path
carries that value, and the client's own `DefaultRPCTimeout` is 5s with a
GRV/read fallback that retries other replicas rather than surfacing a bare
`context.DeadlineExceeded`. Note also that `OnError` cannot classify a bare
`context.DeadlineExceeded` (`errors.As(err, &fdbErr)` fails, transaction.go), so
whatever produces one ends the transaction instead of retrying — correct for a
caller's cancellation, wrong for an internal RPC deadline, and the two are the
same value.

**master's Nightly RowDiff is also red for a SECOND, unrelated reason** — a real
engine defect with a seed: `seed 3495589`, three variants, `resolution error 46
at qov.binding: exact QOV "q$…" (RECORD<ID,A,B,C,S,F,D,E>) has no declared
runtime binding`. Reproduce with `ROWDIFF_SEED_START=3495589 ROWDIFF_SEEDS=1`.

---


### Chaos suite: a dead container is reported as N deadlines, not as a dead container

The COST of this is fixed; the ATTRIBUTION is not.

What was wrong: scenario ops ran on `context.Background()`, and
`Database.TransactCtx` retries "bounded only by
SetTransactionTimeout/SetTransactionRetryLimit (default unbounded)" with neither
set. So a shared container dying mid-suite did not fail the remaining ops, it
HUNG them, until the package's 15-minute alarm fired — observed once as 14
failures plus a timeout whose stack named an arbitrary test rather than the
container. The package now carries a SUITE budget (`chaos_test.go`, 10 minutes against a
measured 90.2s healthy run) from which every op and run context derives, and
every call site is routed through it -- `scenario.go`, `concurrent.go`,
`fault.go`, `verify.go`, `spfresh_driver.go`, and the 29 test-body contexts that
previously took `context.Background()` directly. Per-op bounding ALONE does not
work and was tried: at `-test.parallel=1`, thirty tests at 30s each exhaust the
900s alarm on their own. With a shared budget a dead container spends it once
and the remaining tests fail immediately, with a TYPED
`context.DeadlineExceeded`.

What remains: those failures still say "deadline exceeded", not "the container
is gone". An earlier attempt to close that by classifying error strings was
written and then removed — its signature list was the only thing pinning it, one
signature could never be produced by any error in the tree, and a false positive
would have replaced every later scenario's real diagnosis with a guess. If
attribution is wanted, MEASURE it: probe `cleanDB` once in `NewScenario` under a
bounded context and report the container state directly. Do not infer it from
message text; this repo matches error TYPES.

Note the blast radius is smaller than it looks: `chaos_test.go` hands one
container to 228 tests (229 `func Test*` minus `TestMain`), but
`pkg/recordlayer/chaos/BUILD.bazel:66` pins `-test.parallel=1`, so they run
serially rather than concurrently.

DONE when: a scenario that cannot reach the cluster says so, from an observation
of the container rather than from the shape of an error string.

---


### A scratch tree inside the worktree can turn a docscheck census into fiction

A second copy of the repo inside the worktree DOUBLES every basename, so any
census that counts declarations or resolves cites by basename silently reports
twice the population. During PR #761 a `git archive` extract sat briefly at
`<worktree>/scratchpad/<sha>/` and a docscheck walk failed on a path inside it
that had since been deleted — loud only because the directory vanished mid-run.
A copy that STAYS is the dangerous case, and it reads as a finding.

PARTLY CLOSED. `pkg/docscheck/rfc_cite_resolution_test.go`'s
`nestedRepoRootsUnder` refuses to build a cite index while any directory below
the root carries its own `MODULE.bazel`, so the RFC-238 cite gates and
`TestRFCCiteCensusRepoWide` cannot report totals about a doubled tree.

WHAT REMAINS is every OTHER docscheck census. They reach the tree through
`fallbackWalk` (`pkg/docscheck/fallback_walk_test.go`) when git enumeration is
unavailable, and its `fallbackWalkSkippedTrees` list covers build output, VCS
metadata, the vendored Java tree and sibling worktrees — but nothing that
matches an ad-hoc extract. Note the walk that failed in #761 was NOT
`fallbackWalk`: it rolled its own `filepath.Walk` with its own exclusion switch,
so extending `fallbackWalkSkippedTrees` alone would not have prevented it. Both
shapes need the guard.

DONE when: every census walk refuses a nested second copy of the repo rather
than only the cite gates, and a unit pin drives that refusal — construct a
nested `MODULE.bazel` in a temp tree and assert the walk declines — rather than
the corpus happening to be clean.
---


### A gate for prose that restates a value an assertion already owns

The instances two reviewers surfaced after #760 are fixed in that PR's
follow-up, not booked here: the arity census now names `pinned` and
`rfcPublishedPopulation` instead of repeating them,
`field_name_decision_test.go` names `fieldDebtAuthorityTotal` instead of
contradicting it, and `cardinality_bounds.go` no longer encodes an arm count in
the word "twelfth". One reported instance was investigated and split: in
`result_type_stub_census_test.go` the ORDINAL ("a thirteenth stub") is sound,
anchored to a count the same file twice describes as closed, but the sentence
carrying it asserted in the PRESENT TENSE that the callers pass UnknownType and
the constructor defaults nil to it -- both false since RFC-232, and refuted by
the `want = 0` assertion in the very function that comment heads. Checking the
number and clearing it is what nearly hid that; the prose around a sound figure
is where this class actually survives. Fixed in the same PR.

What remains is enforcement, and the obvious formulation does not work.

The writing rule itself is settled and already stated twice in-tree:
`properties/collected_statistics.go:33` -- "The population is deliberately NOT
written as a number here: an earlier revision said 'four', RFC-236 then added
the fifth" -- followed by a grep recipe in place of the count.
`plans/in_union.go:212` states the scoping half.

The first gate proposed was: within census test files, every DIGIT in a comment
must appear as a pinned value in the same file. Review showed that cannot
enforce ownership, in both directions:

- FALSE NEGATIVE: a stale count passes whenever any unrelated assertion in the
  file happens to pin the same value. The real "25 escapes across 14
  authorities" defect would have been masked by any unrelated 14.
- FALSE POSITIVE: RFC and revision numbers (RFC-230, rev 6) fail without
  restating any population.

So value-equality is the wrong relation, and the repair is to invert what the
gate does: require the comment to CITE, never to VERIFY. A comment that states
a population must carry the identifier whose assertion owns it, or a grep
recipe -- AND must not restate the figure itself. Both halves are load-bearing,
and each closes a hole the other leaves open. Requiring only a citation still
passes "14 authorities (fieldDebtAuthorityTotal)" after the constant moves to
13: the citation is present and resolves, so a stale literal rides a valid
pointer. Comparing literal against cited owner would catch that, but drags
verification back in. Forbidding the literal removes the thing that can go
stale, which is what `collected_statistics.go` already does in prose -- "the
population is deliberately NOT written as a number here" -- so the gate is that
sentence made mechanical. Nothing is compared, so the collision false negative
disappears rather than being narrowed, and nothing is restated, so there is no
literal left to drift.

Two roles for a number have to stay separate, though. A line number inside a
`file:line` reference is not a population claim, so the scan ignores it
alongside RFC/revision numbers and dates -- a short list enumerated once. But
`file:line` is NOT an acceptable citation FORM: this repo already holds that "a
design trade defended by line numbers decays into an unfalsifiable claim"
(the phrase is in `field_name_decision_test.go`), and a gate accepting one would bless the
least durable pointer available. A citation must name an identifier or give a
recipe -- both survive an edit above them; a line number does not. That is
`collected_statistics.go:33` made mechanical rather than aspirational.

One correction to the target, earned by a near-miss during review. Two
reviewers checked the ORDINAL in `result_type_stub_census_test.go`, agreed it
was frozen, and nearly closed the file -- while the sentence carrying it
asserted in the present tense that every aggregate-index plan carries
values.UnknownType, that the callers pass the singleton explicitly, and that the
constructor defaults a nil type to it -- all three false since RFC-232. The digit
was sound; the prose around it was not. So the gate's subject is the TENSE and
OWNERSHIP of prose adjacent to a pinned figure, not the digit itself. A sound
number is exactly the cover this class hides behind.

The same round produced the other half of that lesson: the claim was fixed in
one file and left standing in another, because no repo-wide grep for the
superseded phrasing was run. A correction is not done until that grep returns
zero across every file and the count is reported.

WHAT THIS GATE CANNOT DO, written first because the positive claim kept
outrunning it. Four things, all demonstrated during review rather than imagined:

- It cannot see a stale claim carrying NO figure. The defect that produced this
  entry was a sentence asserting two files "return the singleton on a branch" --
  no number, no citation, simply false, and lifted from those files' own stale
  doc comments. No digit, citation or literal rule fires on it.
- It cannot check TENSE. An earlier draft here demanded a past-to-present verb
  flip go red two sentences after conceding tense is not decidable, which is a
  contradiction, not a specification.
- Being digit-keyed, it is blind to populations written as WORDS. "Twelve plans
  returned ..." heads the census file this entry is about, and this entry itself
  described its own defect count in words until that was noticed.
- It cannot tell whether a historical marker is WARRANTED. The marker is
  author-asserted and the whole green side rests on it, so marking a live claim
  historical immunises it. That is an escape hatch, not a check.

So the gate is a mechanical FLOOR, not the mechanism. EVERY defect in this
campaign was found by a reviewer reading prose against code, and none by any
rule proposed here -- no count is given because it kept moving. What the floor
buys is that the two shapes which ARE decidable stop recurring, and that is
worth having precisely because they recurred repeatedly while everyone was
watching for them.

DONE when: the gate REDS on a census-file comment that states a population with
no citation, and on one that restates the literal while carrying a valid
citation -- the arm a citation-only rule would miss. It stays GREEN on an RFC
number, a date, and a count inside an explicitly marked historical block. Each
arm proven by mutation, with the mutation shown present and the build result
read, not only the test result. Its scope sentence must say census files ONLY:
the production comments this campaign corrected -- in executor.go and in
fk_chain_cardinality.go -- sit outside it by construction, and claiming
otherwise would be the same over-claim one level up.


### [ ] RFC-238 cites source by LINE NUMBER, and the census only catches the lucky half

`rfcs/238-a-qualifier-is-structure-not-punctuation.md` anchors its argument to
`file.go:NNNN`. Any edit above a cite silently retargets it, and RFC-241 broke
the same two anchors TWICE in one change — once when a ~65-line net insertion
into `logical_predicate.go` moved them, and again when folding review findings
into that same file moved them further.

**This repo already ruled on this class, for a different document.**
`TestStatusPageCrossReferencesResolve` (`pkg/docscheck/status_page_crossref_test.go`)
forbids line anchors on `road-to-prod.md` outright, and its comment states the
reason: "a line anchor into it is wrong the moment anyone edits an earlier item,
and it is wrong SILENTLY, still rendering as a precise-looking citation." Every
word of that applies to RFC-238.

**The existing census is a partial guard and its gap is measurable.**
`TestRFC238WeakCitesAreTheOnesSection7dNames` catches a cite only when the line
it lands on looks WEAK — blank, a brace, a comment. A cite that drifts onto
another plausible statement stays green while pointing at the wrong code.
Measured during RFC-241: the census flagged
`logical_predicate.go:6796` (drifted onto a comment) and said nothing about
`:6620`, which had drifted off `recordTypeCI` onto an unrelated line. One wrong
citation was caught, one was not, and the difference was luck.

THE WORK: convert RFC-238's source cites from `file.go:NNNN` to the stable form
the status-page gate already requires — name the SYMBOL (`recordTypeCI`,
`buildSelectScope`'s bare-alias call) — and then hold it with a gate that
resolves each cited symbol, so a rename fails loudly instead of a renumber
failing silently. The weak-cite census can then retire with the anchors it was
compensating for, per the guard-shelf-life rule: it exists to watch a hazard that
the conversion removes.

Scope note: RFC-241 re-pointed the two anchors it broke rather than converting
the document, because a citation-style change to a merged RFC is its own change
with its own review. That is why this is booked rather than done there.


### [ ] gazelle emits 181 duplicated `srcs` entries in one target, and that made a reviewer file a wrong fix

`pkg/relational/core/embedded/BUILD.bazel`'s `embedded_test` target lists **181**
source files twice in a single `srcs` list:

```
awk '/srcs = \[/{f=1} f&&/^        "/{n=$0; gsub(/[ ",]/,"",n); c[n]++} \
     /^    \]/{f=0} END{d=0; for(k in c) if(c[k]>1) d++; print d}' \
  pkg/relational/core/embedded/BUILD.bazel
  -> 181
```

Bazel tolerates it and the target builds, so nothing fails — which is why it has
accumulated. The list has an alphabetical head and a long unsorted tail, and
gazelle appends to the tail while the head already carries the name.

**The cost is not tidiness, it is that the file misleads readers.** During
RFC-241 a reviewer saw two newly-added test files listed twice and reported it as
a defect introduced by that change. The observation was correct and the implied
fix was not: deleting the two lines made `just generate` regenerate them and the
pre-commit hook rejected the commit with "generated files are out of date". The
real condition is 90x larger than the one reported and belongs to gazelle's
handling of this file, not to any one change.

THE WORK: find why gazelle appends rather than merges for this target — most
likely the unsorted tail defeats its sort-and-dedup — and either normalise the
list so gazelle owns it cleanly, or record at the file head that duplicates are
expected output so the next reader does not file the same finding. Then assert
it: a docscheck arm counting duplicate `srcs` entries per target, floored at
whatever the normalisation achieves, so the count cannot silently grow again.

Do not "fix" this by hand-deleting entries. That is what fails the hook.


---


---

## 9. Java upstream — bugs to report, fixes to send, releases to wait for

Defects in `fdb-record-layer` / `fdb-relational` itself, measured against the pinned Java
**4.12.11.0** by the cross-engine probes. They live here because the repair belongs upstream, not
because they are excused: CLAUDE.md's rule is that "it's an upstream bug" is never a deferral —
fix it at the boundary, work around it deliberately with the divergence documented at the call
site, AND report it upstream.

Each entry must carry: the measurement on BOTH engines, the probe that pins it (so the entry goes
red if either engine moves, in either direction), the upstream issue or PR once filed, and what Go
does in the meantime. An entry with no filed issue is not done being worked — filing is the work.

**No entry below names an issue this repo filed** — the two `UPSTREAM` entries say "TO REPORT
UPSTREAM" and neither has been. That is this section's standing debt and the first thing to clear.

**Check whether a defect is already known upstream before filing.** The tree cites six upstream
issues today, and one of them (#3220) is the blocker for the third entry below:

```
grep -rnoE 'fdb-record-layer(/issues/|#)[0-9]+' --include='*.md' --include='*.go' . \
  | grep -v '^./shifts/' | sort -u
  -> issues 635, 965, 3216, 3220, 3303, 3646
```

(The `shifts/` exclusion drops the completed-work archive, which repeats these. Run it unfiltered
for the full historical set. The `--include` pattern is quoted deliberately: unquoted, a shell that
rejects an unmatched wildcard never runs the command at all and the empty result reads as "no
upstream issues cited anywhere" — see CLAUDE.md on the glob-mode false zero.)

### [ ] UPSTREAM — Java's grouped MIN returns NULL for a group holding a NULL

MEASURED on both engines
(`conformance/permuted_min_null_group_java_probe_test.go`, which asserts this
and fails if Java starts agreeing):

```
rows (g, v): (1,5) (1,NULL) (1,9) | (2,NULL) (2,NULL) | (3,2) (3,8) | (4,0) (4,NULL) (4,-4)

SELECT g, MIN(v) FROM t GROUP BY g
  java: [[1 nil] [2 nil] [3 2] [4 nil]]     <- wrong for the MIXED groups 1 and 4
  go  : [[1 5]   [2 nil] [3 2] [4 -4]]      <- SQL-correct

SELECT g, MAX(v) FROM t GROUP BY g
  java and go IDENTICAL                     <- the control: NULL never wins a MAX
```

A `PERMUTED_MIN` index stores one extremum per group, a NULL-valued record
produces an entry like any other, and NULL sorts before every value — so it wins
the comparison unconditionally and the stored extremum for a mixed group is NULL.
Java's `PermutedMinMaxIndexMaintainer` has no NULL filter and its `getExtremum`
takes the first entry of the group scan, so it both stores and ANSWERS the NULL.

Go stores the same bytes on purpose — that is what keeps the two engines able to
share a cluster — and repairs the answer at READ time by resolving a stored NULL
extremum against the index's ordinary subspace. Direction:
`DivergenceJavaWrongRowsGoCorrect`.

TO REPORT UPSTREAM. The defect is in
`fdb-record-layer-core/.../indexes/PermutedMinMaxIndexMaintainer.java`: SQL MIN
ignores NULLs, and neither `updateIndexKeys` (which lets a NULL-valued entry
compete for the extremum) nor `getExtremum` (which takes the first entry of the
group scan) excludes them. MAX is unaffected because NULL sorts lowest. Upstream's
own tests cannot see it: `PermutedMinMaxIndexTest` and `FDBPermutedMinMaxQueryTest`
use proto2 int fields that are never null, so no fixture there has a group holding
a NULL beside a value.

Nothing in this repo is blocked on the upstream fix — the read-side repair is
complete and pinned — but the report should go out, and if upstream fixes it the
probe here fails and says so.

### [ ] UPSTREAM — Java NPEs on `IN (SELECT …)` instead of raising its own assert

MEASURED on both engines
(`conformance/in_list_shapes_java_probe_test.go`, which asserts that both
engines REFUSE this and would fail if either started accepting it):

```
SELECT id FROM t WHERE b IN (SELECT b FROM t WHERE id = 1)

  java: NullPointerException: Cannot invoke
        "…RelationalParser$ExpressionsContext.expression()"
        because "expressionsContext" is null
  go  : 0AF00: Cascades planner could not plan query
```

Java HAS an assert for this shape and never reaches it.
`ExpressionVisitor.visitInPredicate` opens with

```java
Assert.thatUnchecked(ctx.inList().queryExpressionBody() == null,
        ErrorCode.UNSUPPORTED_QUERY, "IN predicate does not support nested SELECT");
```

so the intent is a clean UNSUPPORTED_QUERY. The NPE comes from elsewhere in
the visit reaching `ExpressionsContext.expression()` on the subquery branch,
where `expressions` is null because the parse took `queryExpressionBody`.
Something evaluates the expressions branch before — or instead of — that
guard.

NOT A GO PROBLEM, and nothing here is blocked on it: both engines refuse the
query, so the conformance principle holds in OUTCOME and Go's refusal is the
tidier of the two. It is booked because an NPE is an upstream defect worth
reporting whoever hits it, and because the probe compares only the outcome for
this arm — if upstream ever fixes the NPE into its intended assert, that arm
becomes comparable by MESSAGE too and the probe should be tightened.

TO REPORT UPSTREAM with the reproducer above.

### [ ] Reconcile the RecordQueryFirstOrDefaultPlan continuation format with Java

Recorded because CLAUDE.md treats continuations as wire-critical and an unrecorded known
divergence there is exactly how one becomes invisible. **Not actionable today** — closing it
requires Java to fix an upstream bug first, so this is a watch entry, not queued work.

Go and Java disagree on the bytes this operator emits, and they disagreed *before* the
tagged namespace landed:

- **Java** builds its continuation from `RecordCursor.fromFuture`
  (`RecordCursor.java:933-940`): `continuation == null` → run the child; any non-null bytes →
  `EmptyCursor`. The only value ever produced is `FutureCursor`'s shared constant `[0x00]`
  (`FutureCursor.java:51`). It is a **presence flag, not a position** — nothing identifies the
  producing plan and no child state is embedded.
- **Go** emitted `0x01` (`singleResultConsumedToken`) for the same state long before this
  change, and now tags the namespace `0x01` consumed / `0x02` checkpoint / `0x03` restart.

The divergence **cannot be closed by matching Java's bytes**, and that is the whole point:
Java's format is structurally incapable of representing a truncated child, which is precisely
why `RecordQueryFirstOrDefaultPlan.java:100-106` reads a truncated inner leg as an EMPTY one
and answers `NOT EXISTS` = true from a partial scan. Its own comment flags this as unfixed and
names the issue: **FoundationDB/fdb-record-layer#3220**. `RecordCursor.java:920-925` — the
javadoc on the exact `fromFuture` overload that plan calls — states the fix in the abstract:
when the future is backed by a cursor that may stop out-of-band, "it may be desirable to
include the progress included in the underlying cursor in the final continuation … something
more bespoke may be required". Go's checkpoint tag IS that bespoke thing. Adopting Java's
format would mean re-importing the wrong answer.

What the tagging did change is strictly in the safe direction: `0x01` keeps its meaning, so
Go continuations issued before it still decode; an unrecognised tag now raises
`ContinuationParseError` instead of being passed through as the inner leg's continuation,
which is how a Java-issued `[0x00]` would previously have been fed to the wrong cursor.

**Revisit when** Java closes #3220. At that point compare the two formats again and port
Java's, since it will finally be able to express the state. Until then the reachability bound
is what keeps this benign: this continuation is internal to the Go paginating driver, and no
cross-engine test exchanges it — if that ever changes, this entry becomes urgent.

---

## 10. Blocked — owner decisions and watch entries

Not actionable by an engineer here: each needs a ruling only the owner can make, or an external
system nobody in this repo controls. Each entry states what unblocks it and what to do the moment
it does. Nothing in this section is a deferral of work this repo can do.

### [ ] OWNER DECISION — Go accepts a parenthesized CASE condition; Java rejects it 42804

MEASURED on both engines (`conformance/case_parenthesized_condition_java_probe_test.go`,
which asserts this state and fails if either engine moves):

```
CASE WHEN  a = 1 AND b = 1  THEN 1 ELSE 0 END   java ACCEPT      go ACCEPT (same rows)
CASE WHEN (a = 1 AND b = 1) THEN 1 ELSE 0 END   java REJECT 42804  go ACCEPT (correct rows)
CASE WHEN (a = 1)           THEN 1 ELSE 0 END   java REJECT 42804  go ACCEPT (correct rows)
```

Java rejects EVERY parenthesized condition, simple or compound: the grammar has
no parenthesized-expression alternative in `expressionAtom`, so `( expr )` is a
one-element `recordConstructor`, and Java's `visitCaseFunctionCall` asserts the
condition is BOOLEAN. A record is not, so it errors with a datatype mismatch.

Go has always ACCEPTED these. What changed is that it used to answer some of
them WRONGLY: resolving the condition as a value first meant a parenthesized
COMPOUND boolean became `WHEN({_0: predicate}, TRUE)`, which is never true, so
every row took the ELSE branch. That is fixed — the condition now resolves as a
predicate, which is what a searched CASE's WHEN is — and the repair is pinned by
`pkg/relational/sqldriver/case_parenthesized_condition_fdb_test.go`.

So this is NOT a widening of the accepted surface. It is the same surface with
the wrong answers removed, and it leaves Go in the direction the harness already
names `DivergenceJavaErrorsGoCorrect`.

THE DECISION, which is why this is booked rather than settled:

  (a) KEEP Go permissive and correct. `CASE WHEN (a = 1 AND b = 1)` is ordinary
      SQL that most engines accept; Java's rejection is an artifact of its
      grammar treating grouping parentheses as a record constructor. Read-side
      query reach beyond Java is allowed when wire compat holds — it does here,
      nothing about storage changes — and the shape now has deep coverage.

  (b) NARROW Go to Java's 42804. Strict parity on the shared surface, and there
      is precedent in this repo: when Go accepted ordering comparisons over
      BOOLEAN that Java rejected, Go was changed to reject
      (boolean_expression_position_java_probe_test.go records that). Cost: a
      query that works today starts failing, including the simple `(a = 1)`
      form that Go has always answered correctly.

Whoever rules should also note that (b) narrows MORE than the defect: the simple
parenthesized condition was never wrong, so rejecting it is a behaviour change
unrelated to the bug that prompted the measurement.

- [ ] **CQ-77 (OWNER ACTION, not code) — `frl-pin-bump` has failed 27/27 because
  Actions cannot create PRs.** · XS
  Flip **Settings → Actions → General → Workflow permissions → "Allow GitHub
  Actions to create and approve pull requests"**. Until then the Java-pin bump
  workflow cannot open its PR and every run fails.
  **Booked 2026-08-01.** `road-to-prod.md`'s audit section listed this under
  "Newly booked from the audit (were unbooked; now TODO items)" — for this bullet
  that was false: it appeared in no TODO file at all. A status page asserting an
  item is booked is how an item stays unbooked.
  DONE = the setting is flipped and one `frl-pin-bump` run opens a PR.

- [ ] **No *general-purpose* window functions — and Java has none either.** Investigation (RFC-045): Java's relational layer has **no** general streaming window operator. The general `windowClause` is commented out in Java's grammar ("don't want to deal with them now"); `LAG`/`LEAD` are grammar tokens with **no** value class; `RankValue implements Value.IndexOnlyValue` (computable only from a rank/leaderboard index, never over a result set). The **only** working window function in Java is `ROW_NUMBER() OVER (... ORDER BY <distance>) <= K` via `QUALIFY`, used exclusively for **vector/HNSW K-NN search**. So "match Java's window functions" ≡ "finish the vector/HNSW relational parity" — tracked as **Phase 9** below. General windowing over plain tables would be a *Go-only extension Java lacks entirely* (allowed if wire-compat holds + deep tests), not parity — deferred, not in Phase 9.

---

## 11. Reference — stress baselines

Recorded stress results, kept so a ratio can be checked against the tree it was taken on. Read the
"Stress comparison workflow" section of CLAUDE.md before adding a row: name BOTH SHAs, take n >= 2
per side, and re-measure the baseline whenever the number is quoted in a decision.

### Stress comparison — RFC-226 projection result type (2026-08-09, same machine, same filesystem)

Baseline = detached worktree at master `aba271454`; current = `rfc/projection-result-type`.
Both legs uncached, `--test.v`, 24 `=== RUN` lines each, `/home` at 89% before starting
(a baseline taken at the 100% this box briefly hit would measure the DISK, not the change).

**No regression. 177.36s -> 177.52s total (+0.09%); all 23 subtest arms inside noise.**
Largest single delta `+0.15s` on `full_scan_count` (3.02 -> 3.17) against a ~3s scan;
`order_by_pk_full` 3.42 -> 3.51; `scan_all_wide` 3.37 -> 3.38; point lookups 0.01-0.06s
unchanged. The arms assert ROW COUNTS, so 23 passing on both sides is also the statement that
no plan started returning different rows.


### Stress test 1M baseline (2026-05-27)

**Run command:** `bazelisk test //pkg/relational/sqldriver/stress:stress_test --test_output=streamed --test_arg="--test.run=TestFDB_Stress_1M$" --test_arg="--test.v"`

**2026-08-08 (RFC-220 — coveringness as a plan type, BEFORE side only):**
branch point `4465dcc6d`, tree carrying the RFC only (no code change), so this
row IS the baseline. Same filesystem, `df -T .` = **xfs**, 89% used, 111G
available. The box is XFS; CLAUDE.md's ~95% ext4 threshold does not transfer, so
the run is judged against its own after-side, not that figure.

`TestFDB_Stress_1M` **PASS, 336.10s.** PK lookups 0.09/0.02/0.02s; idx_customer
eq 0.05s; idx_amount range 0.40s; idx_status count 0.51s; full_scan_count 4.72s;
full-scan filter 0.86s; GROUP BY status 0.02s; GROUP BY COUNT-only 0.01s; SUM by
status 0.01s; GROUP BY customer HAVING 0.79s; JOIN 10×customers 0.06s; ORDER BY
PK full (1M) 6.13s; ORDER BY PK + index filter 12.58ms; scan-all narrow (1M)
6.26s; scan-all wide (1M) 6.24s; IN-list 26.44ms; PK needle 7.70ms; PK+filter
needle 10.67ms; sparse filter (97 rows) 4.77s; UPDATE by index 14.11ms; DELETE
single 10.23ms.

**Do NOT read these against the 2026-07-31 row as a regression** — that run was
on a differently-loaded box (its ORDER BY PK full is 3.355s against 6.13s here).
Only the before/after pair taken on the SAME box in the SAME session is a
comparison; the after side is pending implementation.




**2026-08-11 (GetRangeLimits byte-division port — range batch boundaries move, so
read-conflict extents move):** the port makes range batching byte-driven instead of
row-driven, which relocates every per-batch read-conflict range and every cursor
continuation. That is correctness-adjacent under contention, not merely a
round-trip-count change, so the comparison is mandatory. Baseline
`cc8c88c06` (this branch's own merge base) in a sibling worktree under
`.claude/worktrees/`, i.e. the SAME xfs mount — `df -h /home` 91% used, 87G free.
The host is xfs, so CLAUDE.md's ~95% *ext4* threshold does not transfer; the run is
judged against its own opposite side.

VERDICT: **no detectable regression. The only systematic effect is RUN ORDER, not
branch** — and that is a measured result, not an assumption, because the pair was
run twice with the order REVERSED.

```
bulkInsert orders, rows/s     1st position      2nd position
round 1                       baseline 6421     branch   4520
round 2 (order reversed)      branch   5688     baseline 3850
```

The slow arm is whichever ran SECOND, in both rounds. Host load recorded
throughout each run (30s samples) says why: the second run of each pair landed in a
busy window as other agents' Bazel jobs ramped back up.

```
                mean idle   mean load1
round 1  base       71.1%       10.72     <- 1st
         branch     38.0%       20.75     <- 2nd
round 2  branch     66.3%       10.29     <- 1st
         base       37.3%       28.15     <- 2nd
```

So the ~30% ingest gap seen in round 1 alone would have been reported as a branch
regression, and round 2 shows the identical gap pointing the other way. It is
contention. This box carries 43 agent worktrees and up to 21 concurrent sandboxed
test actions; both rounds were started only after a watcher observed 4-5
consecutive 30s samples above 70% idle, and the quiet still did not survive a full
pair.

Comparing FIRST-position runs only (each on a freshly quiet box, the closest to a
matched comparison available here), the branch is at or ahead of baseline on every
query row:

| query | baseline (1st, 71% idle) | branch (1st, 66% idle) |
|---|---|---|
| PK lookup id=0 | 21.63ms | 8.62ms |
| PK lookup id=N/2 | 12.44ms | 8.35ms |
| PK lookup id=N-1 | 13.93ms | 6.13ms |
| idx_status count pending | 421.07ms | 361.88ms |
| full scan filter amount>5000 | 690.63ms | 583.50ms |
| GROUP BY status COUNT only | 20.78ms | 7.29ms |
| SUM by status (aggregate index) | 13.43ms | 12.05ms |

Those are NOT claimed as a speedup the port earns — the branch ran at 5 points lower
idle, the differences are within the run-to-run spread this host produces, and the
2nd-position pair is mixed in both directions. The load-bearing claim is only that
nothing regressed.

CORRECTNESS: `COUNT(*) = 1000000 (expected 1000000)` on all four runs, all 25
subtests PASS on all four, `EXIT_CODE=0` on all four. Row counts are identical
across the moved batch boundaries, which is the property that had to hold.

THRESHOLDS: point lookups are 6.13..21.63ms against the documented <5ms, on BOTH
sides — the pre-existing baseline-vs-threshold gap already booked above, unchanged
by this branch (the branch is the faster side of it here).

THE CONTENTION QUESTION IS ANSWERED SEPARATELY AND MORE DIRECTLY, because a latency
comparison cannot answer it: whether moved boundaries change WHICH KEYS CONFLICT is
a logical property, and it is pinned by
`fdb:TestByteDividedScanConflictsOverExactlyWhatItConsumed` — a partially-drained
byte-divided scan (6 rows across 3 batches) must abort on a concurrent write to a
row it consumed and must NOT abort on one beyond it. Both directions asserted, and
the over-conflicting direction mutation-checked by making `rangeConflictExtent`
return the full requested range, which reddens only the `far_beyond` arm. That test
runs on a loaded box, which is exactly why it is the right instrument for this axis.

**2026-08-19 (RFC-235 — the existential peel and the three-quantifier NLJ arm's
retirement):** baseline `e24f338e7896f1ea75c9bb016c2ca69b3ab6f93f`, which WAS the
merge-base of `nlj-existential-peel` on this date, run in a worktree named for
that SHA; branch side at `f2f367851`, whose tree is the one measured (it was the
uncommitted phase-2 worktree at measurement time and was committed unchanged). Both
SHAs are named because "vs master" expires: master moves and the sentence stops
describing the comparison anyone made. Same filesystem (`df -T .` = **xfs**, 98%
used); the box is XFS, so CLAUDE.md's ~95% ext4 threshold does not transfer and
each side is judged against the other, not against that figure. Runs were
SEQUENTIAL, never concurrent, n=2 per side, load average recorded per run
(1.37/1.89 baseline, 1.70/3.46 branch).

`TestFDB_Stress_1M` **PASS on all four runs.** Baseline 174.02s, 173.87s (mean
173.945s); branch 173.78s, 173.67s (mean 173.725s) — **0.999x, the branch
marginally faster and inside the noise.**

Per-query, the only two rows outside ±1% move in OPPOSITE directions and are
sub-second at n=2, which is what noise looks like rather than signal:
`full_scan_filter` 0.615 -> 0.650 (1.057x) and `index_status_count` 0.405 ->
0.370 (0.914x). Every other row is 1.000x +/- 0.007.

WHAT THIS DOES AND DOES NOT MEASURE, stated because the ratio is otherwise
over-read: this workload contains exactly ONE join row (`join_10_outer`, 0.040s
both sides). So it bounds the REGRESSION RISK to the rest of the engine — the
change deletes ~12k lines across the planner and executor and moves nothing —
and it does NOT measure the changed path. The join shapes RFC-235 alters are
covered by the correctness suite and the golden plan diff, not by this table.

---



- [ ] **CI: the self-hosted nightly runner loses its FDB container about 30 minutes into every
  Docker-backed nightly, and three nets have been red or dead on that account (found 2026-09-05
  while triaging the fuzz nightly for RFC-242).** Read off the run logs, not inferred:
  - `Nightly RowDiff` — every run from 2026-08-24 to 2026-09-04 red or cancelled. The deep sweep
    logs `WARN fdbgo: connection to server failed address=172.16.0.3:4500` ~30 min in, every
    later seed reports `INFRA … open catalog store: failed to read store info: context deadline
    exceeded`, and the job sits until the 4h test timeout (`panic: test timed out after 4h0m0s`,
    run 33837050450) or is cancelled. Zero MISMATCH lines in the 09-01..09-04 runs: the red is
    the container, not the engine.
  - `Nightly Stress` — red since 2026-08-30. Run 33855529576: `TestFDB_Stress_1M` fails at
    `bulkInsert: INSERT [674000..674500): XX000: open catalog store: … transaction_too_old (1007)`,
    then `connection to server failed 172.16.0.4:4500`, and every later test fails in 60s with
    `dial localhost:49951: connect: connection refused` — the mapped port is gone, so the
    container (or dockerd) died, it is not a client reconnect defect.
  - `Nightly Factory` growth lane — run 33848551268 killed with exit 137 (`Killed … bazelisk run
    //cmd/factory-run`) followed by "The runner has received a shutdown signal"; the committed
    corpus lane passed. Exit 137 is SIGKILL, consistent with the OOM killer on the runner host.
  - `Nightly Coverage` — the "Race detector (SQL conformance corpora)" step is cancelled every
    night since 2026-08-30 by the job's `timeout-minutes: 150`; Reconcile reports the coverage
    net's last genuine run as 2026-08-08 (27 days). The lane has outgrown its timeout; the
    factorycorpus race run alone was at 536s when cut.
  - `Nightly Reconcile` fails on two counts: the stale coverage heartbeat above, and open PR #769
    (`bot/frl-pin-bump`) carrying none of its 7 required checks — the bot-PR shape CLAUDE.md
    already names (its pull_request runs are created and held at `action_required`, and a
    held run surfaces no check).
  What is NOT in this entry: the fuzz nightly, whose two real crashers RFC-242 and the
  `FuzzRebaseValue_NoPanic` fixture fix close, and whose three `context deadline exceeded`
  90-second failures `cmd/fuzzrun` already classifies and retries clean.
  **Fixed in the RFC-242 pull request (a workflow edit, verified by the next nightly, not
  locally):** `frl-pin-bump.yml` now dispatches `ci.yml`, `hosted-smoke.yml` and
  `nightly-libfdbc.yml` against its branch after opening or updating the PR (the `pull_request`
  runs the bot PR raises are created and held at `action_required` — every bot push since the
  PR existed, measured in `frl-pin-bump.yml`'s header; the hold has only been observed on
  `pull_request` runs, and a `workflow_dispatch` run is not one; the job's permissions gained
  `actions: write`, which the dispatch call needs), and `ci.yml` gained the `workflow_dispatch`
  trigger. If tomorrow's Reconcile still lists `#769` as ABSENT, the dispatched runs were held
  too and a token with a real actor is the remaining fix.
  **Corrected while reviewing:** a draft blamed the coverage lane's `timeout-minutes: 150` and
  raised it; the six cancelled runs lasted 3, 55, 11, 67, 8 and 51 minutes with no
  maximum-execution-time annotation, so the cap never fired and the cancellations are the
  host class below. The cap stays at 150 and its comment now says so.
  **STOP — needs the owner, not a checkout:** the container deaths under Stress / RowDiff /
  Factory and the external cancellations of Coverage are on the runner host (memory headroom,
  or what is killing containers and jobs thirty minutes into a run); nothing in this
  repository can observe or change that.

- [ ] **Exact quantifier binding over a CTE or derived body: the derived gathered-unnest star.**
  A read addressed to a CTE's or derived table's own quantified object is bound at execution
  against the row the body flows. RFC-242's second adjacent finding made the scope state that
  row as its flowed layout (`exactVirtualScopeSource`, `FlowedColumns`), carried a registered
  CTE source whole into every reading scope (`cteSourceAs`), and let the resolver take a
  column's ordinal from its SQL position while naming the read by the flowed slot — so a
  WHERE, a sort key, an aggregate key or operand over a unique column of a body that repeats
  a bare leaf or an alias answers in both spellings, as does the union-bodied derived table.
  Two reads of a gathered multi-source unnest star body remain, both pre-existing at the
  merge-base in the same form (measured on SimFDB over `a(aid, k, arr)`, `b(bid, k)`, `ee(ck)`
  with one row each, at the merge-base `36b97f1e9` and at this head):
  - an aggregate over the DERIVED spelling — `SELECT d.aid, COUNT(*) FROM (SELECT * FROM a,
    b, a.arr AS x) d GROUP BY d.aid` — is refused at execution: `exact QOV "D" (…) has no
    declared runtime binding`; a projection or a WHERE over the same derived table answers;
  - a WHERE over the CTE spelling — `WITH d AS (SELECT * FROM a, b, a.arr AS x) SELECT d.aid
    FROM d WHERE d.aid = 1` — does not plan (`0AF00: Cascades planner could not plan query`);
    a projection or an aggregate over the same CTE answers (`TestFDB_UnnestExistsGather`,
    `agg_cte_*`).
  Two more, of the same class, over ROW-VERSIONED tables (a schema template ending in
  `WITH OPTIONS(store_row_versions=true)`; measured on SimFDB at the same two commits over
  `aa(id, y)`, `bb(id, z)` and `things3(id, x, arr)`, one row each):
  - a WHERE over the derived star join — `SELECT d.y FROM (SELECT * FROM aa, bb) d WHERE d.y
    = 10` — is refused at execution as `edge lookup D: read as RECORD(ID,Y,ID,Z), declared
    RECORD(AA.ID,Y,BB.ID,Z)`; the same read without row versions answers, and an ORDER BY over
    the same derived table answers in both spellings (`derived_star_row_versions.yaml`).
    `expandBareStarForRowVersion` has already rewritten this body into the explicit
    projection, and that projection declares its slots by the leg-qualified datum key while
    the derived scope's catalog walk publishes the bare names;
  - a star over a lateral unnest — `SELECT d.x FROM (SELECT * FROM things3, things3.arr AS x)
    d` — does not plan (`0AF00: derived projection input slot 0 cannot adopt its physical
    output names`), and its CTE spelling fails as `XX000: unclassified planner failure`; the
    same body over a table without row versions answers [7],[8] in both spellings
    (`derived_star_visibility.yaml`). The rewritten projection carries the outer X beside the
    element X (four slots, as the top-level `SELECT *` over the same FROM list returns), while
    the derived unnest scope shadows the outer column and states three.
  The CTE spelling stays out of the global scope by shape and its aggregate binds through the
  translator's seed bake (`exactGatheredCTEGroupKeyValue`); the derived spelling has no such
  arm, and `translateAggregate`'s positional-gather comment already books the
  projected-output-layout ordinalization it needs (Java answers GROUP BY over a projecting
  derived source, GroupByQueryTests:699). One boundary for both is what closes them: a
  projection-less body's quantifier declared at execution, or the body normalized to the
  explicit projection Java's expandStar always produces.

- [ ] **Ordering through a projection reaches the child group but not the index.**
  `SELECT u.g FROM (SELECT g FROM ga) u ORDER BY u.g` over `CREATE INDEX ga_g ON ga (g)` plans
  `Project(InMemorySort(Project(Scan(GA))))` while `SELECT g FROM ga ORDER BY g` plans
  `Project(IndexScan(GA_G, [*]))`; `… ORDER BY u.h DESC` over `id AS h` sorts in memory while
  `SELECT id FROM ga ORDER BY id DESC` takes `Scan(GA) REVERSE` (measured on the explain-differ
  dump at RFC-242 r9, `ordering_through_a_projection.yaml` pins both halves). Two mechanisms,
  one now fixed: `PushRequestedOrderingThroughProjectionRule` pushed the constraint through the
  projection's result value with the INNER quantifier's alias as the upper alias and without the
  rebase into the child's current-row space, so a constraint rooted at the projection's current
  — how every constraint arrives — failed the push-down's root check and nothing was pushed;
  RFC-242 r9 routes it through `requestedOrderingBelow`, and the constraint now reaches the scan
  group as `_current.G#1` (`TestPushRequestedOrderingThroughProjection_*`). What remains is on
  the receiving side: a zero-prefix index match carries NO matched ordering parts
  (`MatchInfo.GetMatchedOrderingParts()` is empty for a match over a `FullUnorderedScan`, so
  `SatisfiesRequestedOrdering` in `abstract_data_access_rule.go` returns nil for every candidate
  — for the base query too; instrumented at r9), and the ordered full index scan / reverse scan
  are produced only by `OrderedIndexScanRule`, whose matcher is a `LogicalSort` DIRECTLY over a
  `FullUnorderedScan`, and by the sort-over-scan reverse arm. Java has neither gap:
  `MatchInfo.getMatchedOrderingParts` is computed from the candidate's ordering key parts for
  every match, bound or not (`AbstractDataAccessRule.satisfiesRequestedOrdering` consumes them),
  which is what makes an ordering-driven index scan appear under any expression the constraint
  crosses. Closing it is a port of that: matched ordering parts for the zero-prefix match, so the
  data-access rule keeps the ordered full index scan (the zero-prefix skip already exempts a
  scan that satisfies a requested ordering) and the reverse direction, and the Go-only
  `OrderedIndexScanRule` retires. Until then a sort over a derived table's or CTE's column is
  never answered by an index, and a DESC over its primary key never by a reverse scan.
