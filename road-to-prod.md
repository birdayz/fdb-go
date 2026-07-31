# Road to production

**Revision 2026-08-01.** Measured against `f5c2c7f0e` (= `origin/master`). Supersedes the
2026-07-29 revision.

Status page for the question "what stands between this codebase and production use", tiered by
deployment mode. **This page is the authority.** `PRODUCTION_READINESS.md` is the launch-gate
checklist and `rfcs/prod-readiness-go-client.md` is a point-in-time client audit; both now carry
headers pointing here, and where any of the three disagree, this page wins.

Every claim below was re-verified against the tree for this revision — git history, the pinning
tests, the generated docs, and the CI run history. **Nothing was carried forward on trust.** That
pass refuted a substantial part of the previous revision; the refutations are recorded inline rather
than quietly corrected, because a status page whose errors vanish teaches nobody how it went wrong.

The one-line answer: the code is closer to prod than any older document claims, but **less close
than the 2026-07-29 revision claimed**. Two corrections dominate this revision:

1. **The safety nets are still not trustworthy.** B1's mechanism landed and works exactly as
   designed — and what it now reports is that three of nine nightly nets have *never* recorded a
   genuine run. The previous revision said "red until tonight proves the nets"; it has been red for
   three consecutive nights (2026-07-29, -30, -31). Detection was the deliverable and detection
   works; the nets themselves are still dead.
2. **Several things the previous revision described as "in flight" or "in review" do not exist on
   any ref.** Measured: no open PR in this repo other than #486. Work that lives only in an
   unpushed local branch is not in review, and this page will not describe it as such again — see
   "Unlanded work" below.

Explicit-transaction isolation (B2) remains the single most dangerous defect for a real
application.

## Deployment tiers

### Tier 1 — record-layer data plane + auto-commit SQL, bounded contexts
**Distance: gated on the nightly nets becoming genuinely green.** Two items:

- **The nightly nets are red, correctly.** The reconciler
  (`.github/workflows/nightly-reconcile.yml`) fails when a net has not genuinely executed inside its
  limit. Measured on run `30627489161` (2026-07-31): `3 of 9 nightly nets have not genuinely
  executed inside their limit` — `fuzz-diff`, `fuzz-binding` and `fuzz-engine` have **never**
  recorded a genuine run; the other six are current. Root cause measured from run history: all five
  nightly-fuzz jobs contend for one `hetzner-fdb` runner slot and are serialized, so the losers land
  outside the 00:00–10:00 window and self-skip (run `30612113572`: all five finished in 5–7s with
  "this runner became available at 10:00 UTC, outside the 00:00-10:00 nightly window"). **#523
  widened the windows; it did not address 5 jobs contending for 1 slot.** That is the open work, and
  it is not started (see "Unlanded work").
- **Index candidacy is opt-OUT in Go where Java is opt-IN (CQ-46, `TODO.md:10404`, open).** The
  07-17 nightly-stress failure was root-caused: `tryExistsFlatMap` matched candidates by
  first-column NAME with no index-type check, so a SUM aggregate index was built into a
  record-fetching scan; `getEntryPrimaryKey` returns an empty tuple for the short aggregate entry
  and the executor raises Java's orphan-behavior error — LOUD in every measured shape, no silent
  wrong rows, NOT an index-consistency defect. It was fixed INCIDENTALLY on 2026-07-24 by a
  FAN_OUT-cardinality commit; of the three gates now holding it shut only one names aggregate
  indexes, and adding an innocuous `CreatesDuplicates()` method re-arms the identical fault on
  `ORDER BY` shapes. Adjacent opt-out leaks (`text`, `multidimensional`, `time_window_leaderboard`,
  legacy bare `min_ever`/`max_ever`) are UNMEASURED for reachability.
  *Corrected from the previous revision:* it said "Pinning tests exist (uncommitted, land with the
  next window)". They landed — `pkg/recordlayer/query/plan/cascades/aggregate_index_shortcut_gate_test.go`
  (two tests) is committed as of #524. CQ-45, whose DONE condition was exactly that file landing, is
  therefore satisfied and is marked done in `TODO.md` in this pass.

**What this tier rests on** — all counts MEASURED at `f5c2c7f0e`, with drift-guard status stated,
because an unguarded count in a status doc is a claim with a shelf life:

| Net | Measured now | Drift-guarded? |
|---|---|---|
| Byte-identity differential vs `libfdb_c`, `pkg/fdbgo/bench/` | **80** (75 `TestDifferential_*` + 5 `FuzzDifferential*`) | **No** |
| Chaos with model verification, `pkg/recordlayer/chaos/` | **228** test funcs | **No** |
| Java conformance vs a real 4.12.11.0 server | **1362** Ginkgo specs | **No** |
| SQL corpus coverage | **342 scenarios · 2740 cases · 2399 supported (87.6%)**, 111 unsupported-feature pins, 230 error-path pins | **Yes** — `TestSQLCoverageUpToDate` regenerates `SQL_COVERAGE.md`; `FEATURE_MATRIX.md` carries the same generated totals |
| Java yamsql corpus (RFC-201, NEW since the audit) | **238** files vendored · **32** pass · **0** fail · **206** on the skip ledger · **487** asserted queries | **Yes** — `pinnedLedger` + `pinnedFileTotal` + `pinnedAssignmentDigest` in `pkg/relational/conformance/javacorpus/pinned_ledger_test.go` |
| `.Field`-decides ratchet (RFC-197) | **52** sites, per-bucket totals gate-checked | **Yes** — `TestFieldNameNeverDecides` + `TestFieldDebtBucketsArePartition` |

The first four run per-PR. Both former P0s of the client prod-readiness RFC are verified CLOSED in
code: cluster-file rotation (`pkg/fdbgo/client/database.go:614` re-reads the file when the
coordinator set rotates) and `SetTimeout` bounding an in-flight read (RFC-112 threaded the deadline
into the RPC wait contexts, porting C++'s `timebomb` races). That RFC's punch-list still lists both
as open; its header now says otherwise.

*Refuted while verifying, and worth stating because three of the four are the kind of number that
gets quoted:* the previous revision's "78 differential tests / 227 chaos tests" were exactly right
**on audit day** and have since drifted to 80 / 228 — neither is drift-guarded, so both will drift
again. "1,361 Java conformance specs" was never a spec count: it is the **Skipped** line from a
focused run (`Ran 1 of 1362 Specs … 1361 Skipped`); the total is 1362. `README.md:258` carries a
third, older number (434). And "87.5% (2,395/2,736 across 341 scenarios)" was stale against its own
generated, drift-guarded source, which reads 87.6% / 2399 / 2740 / 342. The lesson this table
encodes: quote a generated number or guard it, never both hand-copy and hand-maintain it.

### Tier 2 — full SQL surface incl. explicit transactions
**Distance: weeks at the current grind pace.** Dominated by B2, which is the Tier-2 gate outright:

- Inside `BeginTx`, DML joins the FDB transaction but SELECT runs in a fresh auto-commit
  transaction — no read-your-writes, no read-conflict ranges, so read-modify-write across two
  transactions is last-writer-wins with no 1020/40001. **Silent lost updates.** Pinned by
  `pkg/relational/sqldriver/tx_select_isolation_probe_test.go`, which asserts the CURRENT broken
  semantics and names what to change when it flips. No user-side mitigation exists.
- The design is **RFC-198** (`rfcs/198-explicit-transactions-read-your-writes.md`), joint review
  **complete** (#529, with the review's one blocker folded into the body). **Implementation is not
  started**: `respectActiveTx` is still `p.IsUpdate()` at
  `pkg/relational/core/embedded/cascades_generator.go:1255`, and the SELECT routing fork survives at
  `:1760`. B2 carries a second gate beyond the RFC: SimFDB/RFC-199 as acceptance harness with
  mandatory injected 1007 and both 1021 branches (`TODO.md:6867-6890`).
  *Corrected:* the RFC's own status line read "**proposed** … awaiting joint-review ACK before
  implementation" — stale from the moment the commit that completed its review merged. Fixed in this
  pass.

The RFC-197 identity migration does not gate Tier 2. Its wrong-rows channels are closed; what the
ratchet still holds is machinery-gated stops and boundary-layer sites, which gate the *migration's
completion*, not Tier 2.

### Tier 3 — mixed Go/Java deployment on a shared cluster
**Works today WITH the watch-list.** Wire compat is exercised per-PR; the remaining cross-engine
differences are semantic, pinned, and enumerable. A prod user must be handed that list; several
entries mean the same query returns different rows or different errors on the two engines.

## Blocking items

| # | Item | Impact | Size | State |
|---|---|---|---|---|
| B1 | Nightly safety nets were fake-green (window gates anchored to cron hours GitHub dispatches 2-4h late; 12 fake-green stress nights; rowdiff window unreachable by construction; oracles never ran) | Unknown-risk factory | S → M | **Mechanism FIXED and merged (#523); the nets are STILL RED.** Windows resized from measured dispatch landings, per-job heartbeats on all 9 windowed jobs, reconciler fails when any net has not genuinely run in N days — enforced three ways by `TestNightlyWindowGatesAreReconciled`. Measured 2026-07-31: `fuzz-diff`, `fuzz-binding`, `fuzz-engine` have never recorded a genuine run; five fuzz jobs contend for one runner slot. **Residual is unstarted, not in flight.** Stress 07-17 root-caused (see Tier 1, CQ-46); binding-stress 0/50 still open (CQ-47) |
| B2 | No read-your-writes in explicit transactions; SELECTs take no read locks → silent lost updates | Wrong data | L | **The Tier-2 gate.** RFC-198 review-complete (#529); implementation not started. Pinned by the probe test |
| B3 | RFC-195: cost estimates contradict proven cardinality bounds; comparator uses a private cardinality walk | Wrong plans (perf), not wrong rows | M | **DONE, merged (#547.)** `rfcs/195-cost-must-not-contradict-proof.md:3` — "ACCEPTED, revision 3 … implemented". Seven shapes fixed in the end, not six; zero exclusions and no mechanism to add one (`cardinality_cost_bound_test.go:36-45`). **Residual: CQ-30 (`TODO.md:10046`, open)** — criterion 2's data-access maxima are still forked; held visible by a standing test |
| B4 | RFC-197 identity migration residual (see per-bucket table) | Plan/decline-direction only; wrong-rows channels closed | M | Active; ratchet-enforced; **68 at inception → 52 now** |
| B5 | WS-N Phase D: metadata re-derived by name instead of flowing from the type (~347 UnknownType mints repo-wide; three named guessers) | Wrong client VALUES on cross-leg same-name-different-type | L | Booked; gates the typed-row-representation work |
| B6 | Documentation authority contradictory/stale | Trust/decision risk, not code | S | **This revision.** Authority headers added to `PRODUCTION_READINESS.md` and `rfcs/prod-readiness-go-client.md`; stale TODO entries fixed; `TestProductionStatusAuthority` added so the redirects cannot silently rot |

### B4 residual, per bucket — MEASURED at `f5c2c7f0e`

These are the gate-enforced group headers in `pkg/docscheck/field_name_decision_test.go`, which
`TestFieldDebtBucketsArePartition` checks against the entries they advertise. The buckets are a
partition, so they sum to the list: **52**.

| Bucket | Audit-day | Now | Wrong rows reachable in prod? |
|---|---:|---:|---|
| boundary | 0 | **1** | No. Not a regression: the call-boundary taint made a site visible that was always there (a name handed to a helper as a plain string parameter). Reporting the bucket migrated while the walk could not reach one of its members is the false green the pass existed to end |
| escape | 0 | **0** | Migrated (found + fixed a live wrong-type defect on the way) |
| contract | 11 | **16** | Not alone — single naming authorities. The four newly-visible entries are the group-by output name's CONSUMERS, laundered through one helper; the bucket had listed eleven producers and not one consumer, and a migration plan written against producers alone cannot close it |
| dotted | 6 | **14** | The WRITERS are resolved; what remains are READERS that decline, each probe-pinned, plus five newly-visible MINTs |
| name-keyed | 3 | **5** | Measured machinery gaps, each recorded on its debt entry (planner-budget re-fire on constraint growth, CQ-51; lazy carriers with no other identity) |
| translator | 17 | **15** | Bounded — resolution-time text handling; misbinding requires the 42702/42703 ambiguity checks to have a hole |
| harness | 1 | **1** | No — oracle-side; affects trust in the net, not prod rows |

**The total ROSE from 41 to 52 between the audit and now, and that is the gate working.** #540 and
#544 taught the detector to follow a display name across a call boundary and through helpers; sites
that were always making the decision became reportable. A ratchet whose count only ever falls is a
ratchet that has stopped looking.

*Refuted while verifying:* the previous revision's "68 at inception → 38" and its `dotted 6 /
translator 17` cells were audit-day figures presented as current. Measured trajectory of the list:
**68** at inception (#520) → **41** (#527/#528/#529) → **54** (#544) → **52** (HEAD). More
consequentially, the previous revision's sequencing said the remaining ratchet was the
boundary/contract tail. **It is not:** `dotted` (14) and `translator` (15) are the two largest
buckets and together are 56% of the list.

Two further corrections to the migration's bookkeeping, both found by reading the ratchet against
`TODO.md`:

- **CQ-52 is not done.** #540 landed the PROJECTION channel only; `TODO.md:10541` is still
  unchecked, and the live residual is explicit in the debt entries (`cascades_translator.go:5742`
  and `:5744` "retire when the remaining `LogicalProject` producers carry `ProjectionRefs`";
  `:6070`/`:6102` "retires when the last caller stops slicing a rendered name"). The previous
  revision's "CQ-52 retires four translator sites" arithmetic no longer holds — the call-boundary
  taint changed what those four sites are.
- **CQ-53 is marked done but has a surviving producer.** `TODO.md:10590` closes it as subsumed by
  CQ-67 (#549) "carrying no separate remainder", while
  `pkg/docscheck/field_name_decision_test.go:447` pins `cascades_translator.go:3598` as "dotted:
  MINT. **CQ-53's surviving producer**" — and the mint is live at that line, on the unnest-merge
  path. Its NLJ twin was deleted; this one "dies with the same work", which is now booked under
  nothing. **This is a real gap between a closed checkbox and the gate**, and it is booked in this
  pass rather than left to be rediscovered.

RFC-197 itself is **IN IMPLEMENTATION** (`rfcs/197-column-identity-is-an-ordinal.md:3`): step 0 and
items 2, 3, 5 and 6 have landed; the remaining items are unstarted and still gated. **CQ-68**
(`TODO.md:12347`, open, gated on CQ-67) is the largest addressable block: 94 FlatMap result values
are a bare untyped QOV where Java types unconditionally. It carries a REOPEN TRIGGER on CQ-67.

## Watch-list — pinned divergences a prod user must be told about

The section contract is that **every entry is a committed test asserting CURRENT behavior; red means
fixed.** Verifying that contract for this revision found four entries that do not meet it. They are
marked, not removed — an unpinned divergence is more dangerous than a pinned one, and hiding it
would make the list read cleaner than the code is.

Wrong rows / wrong data:
1. No read-your-writes in `BeginTx` (= B2). Pinned —
   `sqldriver/tx_select_isolation_probe_test.go:62`.
2. INSERT of NULL into a PRIMARY KEY column silently stores 0. **⚠ NOT PINNED.**
   `sqldriver/embedded_fdb_errors_test.go:125` logs and returns on both fix paths and its own final
   comment disclaims asserting a row count; it stays green whether or not the divergence is fixed,
   and it never checks that the stored value is `0`. Booked.
3. Correlated float `=` misses `-0.0`/`+0.0` on a non-terminal composite index column — silently
   missing rows, rare shape. Pinned — `correlated_zero_composite_sentinel_test.go` (RFC-196),
   "fails loudly the day it is fixed".
4. `CURRENT_TIMESTAMP` drifts across rows within one statement (SELECT path). **⚠ NOT PINNED — no
   test exists.** It is booked open work at `TODO.md:8935-8947`, whose closing line is itself the
   instruction to pin it. The session-object half (`session.go:80-133`) is done; the SELECT path is
   not.
5. NaN comparisons follow Java's total order, not IEEE — `(v/z)=(v/z)` returns ALL rows. Pinned —
   `nan_comparison_semantics_test.go`. Matches Java; both diverge from the standard/PG/CRDB.
6. Signed zero: Go is IEEE, Java is bit-identity — `WHERE d = 0.0` returns a stored `-0.0` row in
   Go, not in Java. Pinned — `plandiff/corpus.go:4726` (`DivergenceJavaWrongRowsGoCorrect`).
7. BIGINT vs DOUBLE above 2^53 — Java promotes lossily and wrongly matches; Go rewrites exactly.
   Pinned — `plandiff/corpus.go:4741`. **Do not read this as "Go is exact everywhere"**: a DOUBLE
   *column* against an integer constant is lossy in Go too, which is correct SQL and is separately
   pinned (`numeric_precision_boundary_test.go:104`).
8. `UNION ALL` + trailing `ORDER BY` — Java orders only the right leg; Go implements the standard.
   **⚠ HALF-PINNED**: Go's side is pinned (`yamsql/testdata/union_columns.yaml:35`), the Java side
   rests on a prose record of a live probe (`DIVERGENCES.md:978`), not a committed cross-engine pin.

Different answer / different error:
9. `SUM(int_col)` overflows silently from a join leg but raises 22003 outside one. Pinned —
   `aggregate_operand_width_fdb_test.go:176`, red-means-gap-closed. Closing it flips answering
   queries into errors — needs its own gated lap.
10. `DELETE/UPDATE … RETURNING` via Exec silently drops the returned values; via Query → 0A000. Java
    supports it. Pinned — `returning_clause_probe_test.go:51,62`.
11. `DROP SCHEMA IF EXISTS` ignores IF EXISTS (deliberate Java-bug replication). Pinned —
    `drop_schema_ifexists_conformance_probe_test.go:29`, with three sibling controls proving
    `DROP SCHEMA TEMPLATE` / `DROP DATABASE` DO honor it.
12. A quoted DDL column is created but unreferenceable by name. **⚠ NOT PINNED, and partly
    REFUTED.** `yamsql/testdata/quoted_identifier_pins.yaml:19` pins that a quoted column *does*
    resolve in projection, predicate and ORDER BY; `TODO.md:4880` records the quoted-lowercase
    42703 as fixed. The surviving residue is **mixed-case** quoted (`"KeepCase"`), which is
    unmeasured since 2026-06-28 and mentioned only in a comment that says it is "not exercised
    here". The entry is narrowed to that residue and booked.

Cleanly rejected (0AF00/0A000) where Java answers: derived tables with JOIN bodies; EXISTS inside
OR; correlated scalar subquery inside EXISTS; projected EXISTS; scalar subquery over FROM-less
SELECT; boolean-CASE in WHERE; grouped correlated EXISTS. Several were once silent wrong rows and
are now flip-sentinels — the correct posture.

**Boolean-CASE in WHERE, stated precisely because the tree contradicts itself about it.** The line
above is CORRECT: Java answers, Go rejects 0AF00. That is what the committed pins assert
(`yamsql/testdata/case_when_in_java.yaml:56`, `case_exists_combo.yaml:24`, both citing Java's
`Expression.java:371-400` `ValuePredicate(= TRUE)` wrap) and it is booked as RFC-180 Y4
(`TODO.md:9314`). **What is inverted is the folklore inside the code**: `walk.go:53` and
`embedded_fdb_test.go:6620` both assert in comments that *Java rejects* it and "Go follows", which
is false and is the reason the gap has looked intentional. Those two comments are a booked defect.

**NEW since the audit — four DDL clauses that now fail closed** (#551), belonging in this section
and absent from the previous revision. Each was being **silently dropped**, producing an index that
differs from Java's on the wire; each is now an `ErrCodeUnsupportedOperation` rejection at four call
sites in `pkg/relational/core/embedded/ddl.go`, pinned by `ddl_fail_closed_test.go`:
per-column `ASC`/`DESC`/`NULLS` on the ON-source list (Go built an ASCENDING index where Java wraps
in `OrderFunctionKeyExpression` — different entry bytes), and on AS-SELECT indexes `WITH ATTRIBUTES`
(Go emitted TUPLE variants where Java builds MIN_EVER_LONG/MAX_EVER_LONG), `UNIQUE` (dropped, so the
constraint silently stopped being enforced), and `WHERE` (Go built a FULL index where Java builds a
SPARSE one). *Refuted while verifying:* these were never watch-list entries that came and went —
they were never listed at all, and they are still live rejections. RFC-202 (#553) landed the RFC and
its census tests, not the generator, so nothing was retired.

Unsupported on both engines: `COUNT(DISTINCT)`, `UNION`/`EXCEPT DISTINCT`, `x IN (SELECT …)`,
`NULLIF`, string functions, `DECIMAL`, FK/CHECK/defaults, window functions.

Client-side operational: watches register at read version, not commit-gated; no `too_many_watches`
limit; no RYW pending-write immediate-fire; special-key space absent; `BYPASS_UNREADABLE` span-wipe
is the one known committed-byte divergence (deliberate, reviewed).

## Unlanded work — named here so it stops being cited as progress

Measured 2026-08-01: **the only open PR in this repository is #486** (a graph-DB RFC). Anything
described as "in review" that is not #486 is not in review. Two bodies of real, measured work sit on
an **unpushed local branch** (`feat/generation-factory-v1`, no remote ref, no PR):

- **The generation factory corpus.** 900 scenarios at `8a84a95bb`, 2000 scenarios / 8000 tests /
  1985 feature vectors at branch HEAD `e59333c33` (cited from `factorycorpus/census_baseline.json`).
  Master has **zero** — `pkg/relational/conformance/factorycorpus` does not exist on `origin/master`.
- **The resolver boolean-operand family fix.** `8a84a95bb` fixes `(a > 3) IS NULL` failing 0AF00
  where Java accepts (found by the factory's first TLP sweep: 64 of 413 `(p) IS NULL` renderings
  failed to plan); `e59333c33` generalizes it to a three-valued `walkPos`, closing six shapes at
  once, and inverts the CASE folklore by measurement against the Java conformance server. Its
  regression tests (`is_null_over_comparison_fdb_test.go`,
  `conformance/boolean_expression_position_java_probe_test.go`) **do not exist at HEAD**.

Also unlanded and, unlike the above, **not started anywhere**: the three dead fuzz lanes (B1's
residual). The branch named for it, `fix/nightly-fuzz-heartbeats`, is an empty pointer at master
with zero commits.

This section exists because the previous revision described all three as in-flight or in-review.
The distinction is not pedantry: unpushed work is unreviewed, unmerged, and one `rm -rf` from gone,
and counting it as progress is how a status page becomes a wish list.

## Newly booked from the audit

- Two LIKE implementations that provably disagree (trailing escape), one live on the
  `INFORMATION_SCHEMA` WHERE path — part of a shadow evaluator family that violates "no parallel
  pipelines". S.
- `API_PARITY.md` contradicts `options.go` on two options (doc says no-op, code rejects) + a
  docscheck gate to keep the table honest. S.
- `SetSpecialKeySpaceRelaxed`/`EnableWrites` still silent no-ops — record the decision. S.
- `pkg/fdbgo` README/doc.go missing the bounded-context requirement. S.
- Two stated-unprobed differential axes (1021 idempotency — needs wire fault injection; cross-shard
  range-merge — needs a multi-shard cluster). M.
- Binding-stress 0/50 (CQ-47), un-root-caused. The 07-17 stress failure is root-caused (Tier 1).
- Repo SETTING (owner action, not code): Actions cannot create PRs, so `frl-pin-bump` has failed
  27/27 — flip "Workflow permissions → allow PR creation".

Booked by THIS revision, from defects the verification pass found:

- Watch-list entries 2, 4, 8 and 12 are not pinned to the contract this section states. Entry 2's
  test cannot go red; entry 4 has no test; entry 8 pins only the Go half; entry 12 has no test and
  its lowercase half was fixed without the entry being narrowed. **Each needs a real pin.** This is
  a code/test lap, deliberately not done in a docs-only pass.
- `walk.go:53` and `embedded_fdb_test.go:6620` assert that Java rejects boolean-CASE in WHERE. It
  does not. Two wrong comments that made a real gap look intentional.
- CQ-53's surviving producer mint (`cascades_translator.go:3598`) is owned by no open item.

## Sequence

1. **B1's residual:** de-serialize the five nightly-fuzz jobs so `fuzz-diff`, `fuzz-binding` and
   `fuzz-engine` can record a genuine run. Until then the reconciler is correctly red and Tier 1 is
   not confirmable. Not started.
2. **B2 explicit-tx isolation.** The Tier-2 gate. RFC-198 is review-complete; implement it. L.
3. **Land the factory branch.** 2000 measured scenarios and a resolver fix for six broken boolean
   shapes are sitting unpushed; the longer they sit the worse the merge.
4. **RFC-197 tail**, sequenced behind the machinery each stop waits on: CQ-52's remaining producers,
   then CQ-51 and the CQ-53 mint, then CQ-68 (the largest block). All review-gated.
5. **CQ-46**, index candidacy inverted to opt-in per maintainer factory, with the adjacent opt-out
   leaks measured for reachability. Query-engine gated.
6. **CQ-30**, B3's residual: criterion 2's data-access maxima are still forked.
7. **B5** typed metadata flow. L, after the migration proves the identity machinery everywhere.
8. Watch-list handed to any prod adopter, with the four unpinned entries pinned first.
