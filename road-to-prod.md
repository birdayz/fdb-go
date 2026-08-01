# Road to production

**Revision 2026-08-01.** Measured against `a1d281a63` (= `origin/master`). Supersedes the
2026-07-29 revision.

Every count below carries the SHA it was measured at. This revision was drafted against `f5c2c7f0e`
and two merges (#555, #556) landed mid-pass and invalidated three of its findings — so the SHA is
not decoration, it is the difference between a measurement and a rumour.

Status page for the question "what stands between this codebase and production use", tiered by
deployment mode. **This page is the authority.** `PRODUCTION_READINESS.md` is the launch-gate
checklist and `rfcs/prod-readiness-go-client.md` is a point-in-time client audit; both now carry
headers pointing here, and where any of the three disagree, this page wins.

Every claim below was re-verified against the tree for this revision — git history, the pinning
tests, the generated docs, and the CI run history. **Nothing was carried forward on trust.** That
pass refuted a substantial part of the previous revision; the refutations are recorded inline rather
than quietly corrected, because a status page whose errors vanish teaches nobody how it went wrong.

The one-line answer: the code is closer to prod than any older document claims. Three corrections
dominate this revision:

1. **The safety nets are not yet PROVEN, but the mechanism is now honest twice over.** B1's
   detection (#523) worked exactly as designed and reported that three of nine nightly nets had
   never recorded a genuine run. #556 then found the reconciler's *diagnosis* wrong — the lanes were
   fine, the window band was the wrong shape — fixed it across all five nets, and published the
   number nobody wanted: **107 of 177 scheduled runs over these lanes' whole life were fake-green**.
   Five validation dispatches are in flight. Tier 1 confirms on the first genuinely green reconcile.
2. **The read-side safety net roughly doubled.** The generation factory (#555) landed 2000 blessed
   scenarios, and it paid for itself on its first sweep by finding a resolver defect that broke six
   boolean operand shapes — including one, boolean-CASE in WHERE, that this page had listed as a
   deliberate parity gap for months. It was not deliberate; it was unmeasured.
3. **Documentation authority was itself a defect (B6), and this pass is its fix.** Three status docs
   contradicted each other; the corrections are recorded inline below rather than applied silently.

Explicit-transaction isolation (B2) remains the single most dangerous defect for a real
application, and is now the top item outright.

## Deployment tiers

### Tier 1 — record-layer data plane + auto-commit SQL, bounded contexts
**Distance: gated on the nightly nets becoming genuinely green.** Two items:

- **The nightly nets: the window fix is MERGED (#556), awaiting its first genuine green.** The
  reconciler fails when a net has not genuinely executed inside its limit, and it was right that
  `fuzz-diff`, `fuzz-binding` and `fuzz-engine` had no heartbeat of any age. **Its diagnosis was
  wrong, and that matters more than the fix.** The suspicion was mis-wired lanes; measurement found
  all five nightly-fuzz lanes byte-identical in their heartbeat steps, with the two healthy ones in
  the same file as the three dead ones. Nothing was wrong with any lane. **The band was the wrong
  SHAPE**: a non-wrapping 00:00–10:00 comparison calls 18:00–24:00 "daytime", so on 2026-07-30 the
  queue handed out runners at 20:22, 21:12, 23:32, 00:05 and 01:24 UTC and the three jobs landing
  BEFORE midnight were discarded to "protect daytime CI capacity" — at ten at night. On 07-31 all
  five landed at 10:22–10:42 and all five were thrown away for being twenty-two minutes late. Bands
  now WRAP and are sized from every allocation hour each job has actually been given, measured over
  39 nightly-fuzz, 47 stress, 12 rowdiff, 109 coverage and 4 oracle scheduled runs. **The same
  defect was live in all four other nets** (stress rejected hour 10 four times, rowdiff hour 20,
  coverage hour 22, oracles hour 22) and was fixed with them rather than left to surface one net at
  a time.
  The honest history the fix published — fake-green scheduled runs over each lane's whole life,
  **107 of 177**: binding-stress 24/39 (8 genuine), client-fuzz 23/33 (9), engine-fuzz 21/33 (12),
  diff-fuzz 20/39 (12), race-detector 19/33 (13). So the lanes were not dead their entire life, only
  most of it; diff-fuzz last did real work on 07-25. Since heartbeats began there have been exactly
  two scheduled runs and three lanes lost both, which is why they read as never having run.
  **Status: five validation dispatches are in flight; Tier 1 confirms on the first genuinely green
  reconcile, not before.** A merged window fix is not a proven net.
  *Corrected from earlier in this revision:* an earlier draft blamed runner contention alone and
  called the residual "not started". Contention is real, but the band's shape was the defect, and
  `TestNightlyWindowAdmitsMeasuredLandings` (replaying every allocation hour each job has really
  been given against the band it declares) is the axis the old gate could not see.
  *Found and fixed while verifying this paragraph:* `nightly-factory.yml`'s two jobs still carried
  the old inlined band and made master red against #556's new gate — see "Landed since the audit".
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

**What this tier rests on** — all counts MEASURED at `a1d281a63`, with drift-guard status stated,
because an unguarded count in a status doc is a claim with a shelf life:

| Net | Measured now | Drift-guarded? |
|---|---|---|
| Byte-identity differential vs `libfdb_c`, `pkg/fdbgo/bench/` | **80** (75 `TestDifferential_*` + 5 `FuzzDifferential*`) | **No** |
| Chaos with model verification, `pkg/recordlayer/chaos/` | **228** test funcs | **No** |
| Java conformance vs a real 4.12.11.0 server | **1363** Ginkgo specs | **No** |
| SQL corpus coverage | **342 scenarios · 2740 cases · 2401 supported (87.6%)**, 109 unsupported-feature pins, 230 error-path pins | **Yes** — `TestSQLCoverageUpToDate` regenerates `SQL_COVERAGE.md`; `FEATURE_MATRIX.md` carries the same generated totals |
| Java yamsql corpus (RFC-201, NEW since the audit) | **238** files vendored · **32** pass · **0** fail · **206** on the skip ledger · **487** asserted queries | **Yes** — `pinnedLedger` + `pinnedFileTotal` + `pinnedAssignmentDigest` in `pkg/relational/conformance/javacorpus/pinned_ledger_test.go` |
| Generation factory corpus (NEW, #555) | **2000** scenarios · **8000** tests; blessings **1785 `metamorphic` + 217 `metamorphic-tlp-only`**, labeled in every header | **Yes** — componentwise census ratchet over scenarios, tests, per-feature-vector and per-blessing (`factorycorpus/census_baseline.json`) |
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
focused run (`Ran 1 of 1362 Specs … 1361 Skipped`). `README.md:258` carries a third, older number
(434). And "87.5% (2,395/2,736 across 341 scenarios)" was stale against its own generated,
drift-guarded source. The lesson this table encodes: quote a generated number or guard it, never
both hand-copy and hand-maintain it — and this revision proved it again on itself, because the
conformance total moved 1362→1363 and SQL coverage 2399→2401 between two SHAs of the SAME pass. The
coverage move is not noise: it is the two CASE shapes flipping from unsupported-pin to supported.

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
| B1 | Nightly safety nets were fake-green (window gates anchored to cron hours GitHub dispatches 2-4h late; 12 fake-green stress nights; rowdiff window unreachable by construction; oracles never ran) | Unknown-risk factory | S → M | **Detection merged (#523); the WINDOW SHAPE fixed and merged (#556); awaiting first genuine green.** #523 gave every windowed job a heartbeat and made the reconciler fail on silence — which then correctly exposed that three fuzz lanes had never recorded one. #556 found the cause was the band's shape, not the lanes (a non-wrapping band calling 18:00–24:00 "daytime"), fixed it across all five nets, and published the honest history: **107 of 177 scheduled runs were fake-green**. Five validation dispatches in flight. Stress 07-17 root-caused (see Tier 1, CQ-46); binding-stress 0/50 still open (CQ-47) |
| B2 | No read-your-writes in explicit transactions; SELECTs take no read locks → silent lost updates | Wrong data | L | **The Tier-2 gate.** RFC-198 review-complete (#529); implementation not started. Pinned by the probe test |
| B3 | RFC-195: cost estimates contradict proven cardinality bounds; comparator uses a private cardinality walk | Wrong plans (perf), not wrong rows | M | **DONE, merged (#547.)** `rfcs/195-cost-must-not-contradict-proof.md:3` — "ACCEPTED, revision 3 … implemented". Seven shapes fixed in the end, not six; zero exclusions and no mechanism to add one (`cardinality_cost_bound_test.go:36-45`). **Residual: CQ-30 (`TODO.md:10046`, open)** — criterion 2's data-access maxima are still forked; held visible by a standing test |
| B4 | RFC-197 identity migration residual (see per-bucket table) | Plan/decline-direction only; wrong-rows channels closed | M | Active; ratchet-enforced; **68 at inception → 52 now** |
| B5 | WS-N Phase D: metadata re-derived by name instead of flowing from the type (~347 UnknownType mints repo-wide; three named guessers) | Wrong client VALUES on cross-leg same-name-different-type | L | Booked; gates the typed-row-representation work |
| B6 | Documentation authority contradictory/stale | Trust/decision risk, not code | S | **This revision.** Authority headers added to `PRODUCTION_READINESS.md` and `rfcs/prod-readiness-go-client.md`; stale TODO entries fixed; `TestProductionStatusAuthority` added so the redirects cannot silently rot |

### B4 residual, per bucket — MEASURED at `a1d281a63` (unchanged by #555/#556)

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
  path. Its NLJ twin was deleted; this one "dies with the same work", and that work was owned by
  nothing. **This is a real gap between a closed checkbox and the gate.** Now booked as **CQ-79**,
  and deliberately NOT folded into CQ-68 — CQ-68 is a different axis (94 bare untyped QOVs, not a
  display name manufactured into a row key), and folding them would let either close while the
  other's residue survived. Re-verified at `a1d281a63`: the pin stands verbatim.

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
3. **RESOLVED 2026-08-01 (CQ-83, `fix/rfc196-correlated-zero-composite`, awaiting the Graefe
   lap).** Correlated float `=` no longer misses `-0.0`/`+0.0` on a non-terminal composite index
   column. The sentinel fired on its own terms and was flipped:
   `correlated_zero_composite_sentinel_test.go` now asserts the CORRECT rows (both correlation
   directions, no duplicates, in-between guard rows, residual-path agreement, EXPLAIN pin that the
   composite probe survives). Mechanism: execution-time probe split — one index range per signed
   zero, scanned as an ordered concatenation (`zeroFork` in the executor; RFC-196's known gap,
   closed the way its own analysis prescribed: widening decided at execution time, where the
   correlated comparand's value is finally known — but as an exact two-range union, not a
   contract-changing internal filter).
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
9. **CLOSED.** `SUM(int_col)` now raises 22003 "integer overflow" identically with and without a
   join around the operand's table, matching Java (measured live: `SumOverflowJoinLegJavaProbe`
   in `conformance/` — Java rejects all four overflow shapes with the same verbatim messages Go
   now emits). Root cause: the width machinery predated RFC-181 P0.5's width-faithful typing and
   only consulted the merged-row ordinal a join-leg reference cannot index; the operand's own
   static type (Java's `NumericAggregationValue.encapsulate` rule) now decides. Pinned —
   `aggregate_operand_width_fdb_test.go` (`TestFDB_AggregateOperandWidthJoinLegRaises` plus
   negative-direction, exact-boundary, BIGINT-lane and AVG/COUNT controls).
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
SELECT; grouped correlated EXISTS. Several were once silent wrong rows and are now flip-sentinels —
the correct posture.

**RESOLVED and removed from that list: boolean-CASE in WHERE** (#555, `491e02a7c`). Go now ACCEPTS
it and returns the same rows Java does. Java plans a boolean-typed CASE as a WHERE predicate; Go
declined only because its CASE consequent resolved in a predicate context, so a comparison arm never
became a value. Resolving the consequent as a value closed it, as part of the same three-valued
`walkPos` that closed the other six operand shapes. The rows are **measured on both engines** —
`conformance/boolean_expression_position_java_probe_test.go` runs **18 shapes** through the live
Java conformance server and the Go engine and compares which are ACCEPTED, with a control shape that
reds the harness rather than the engines if it disagrees; `yamsql/testdata/case_when_in_java.yaml`
and `case_exists_combo.yaml` now assert rows instead of a rejection.

**Booked as RFC-180 Y4 and now unneeded — the parity gap was a phantom.** Four pins asserted a gap
that measurement inverted: the tree's folklore held that *Java rejects* a comparison consequent and
"Go follows". Java accepts it. `walk.go` is gone, dissolved into the unified walk, and the surviving
comment at `embedded_fdb_test.go` now states the corrected fact and cites the probe. Nothing remains
of this item except the lesson: **two code comments and four test pins agreed with each other and
all five were wrong**, because none of them had ever asked the Java server. Agreement among
unmeasured claims is not evidence.

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

## Landed since the audit — the two safety nets this page now rests on

Both merged while this revision was being written, and both are re-verified at `a1d281a63`.

**The generation factory (#555, `491e02a7c`).** `pkg/relational/conformance/factorycorpus` exists
with **2000 committed scenarios / 8000 tests** (`census_baseline.json`), generated
→executed→blessed→deduped→committed, with a componentwise census ratchet over scenarios, tests,
per-feature-vector AND per-blessing. First batch: 1200 seeds → 2268 candidates → 965 blessed → 900
committed; 1599 TLP partitions and 3226 second-plan pairs, zero disagreements, every oracle
mutation-proven armed. Blessings are **1785 `metamorphic` + 217 `metamorphic-tlp-only`**, labeled in
every header — the Java leg is environmentally unreachable here, so the corpus does not claim a
cross-engine authority it does not have. **This is the tier's ceiling and should be read as one:**
metamorphic blessing proves a query agrees with its own transformations, not that it agrees with
Java. The headers are promotable without regeneration, so the corpus becomes a cross-engine net the
day the Java leg is reachable — until then it catches self-inconsistency, which is how it caught the
resolver defect, and not cross-engine divergence. The full corpus is tagged out of the default suite; a stratified
100-scenario sample rides `just test`. TLP's inherent blindness to branch misassignment is pinned as
a negative result that re-arms if the checker gains the ability.

**The resolver boolean-operand family (#555, same merge).** Found by the factory's FIRST TLP sweep —
64 of 413 `(p) IS NULL` renderings failed to plan. `walkRecordConstructor` hardcoded the position
away on every paren-unwrap; a three-valued `walkPos` (predicate/projection/operand) now threads
through and closes **six shapes at once**: `(cmp)=(cmp)`, `=TRUE`, `IS DISTINCT FROM`, `IN`,
`BETWEEN`, `SELECT (cmp)`. This is the factory paying for itself on its first run, which is the
strongest argument for the tier it belongs to.

**The nightly window fix (#556, `a1d281a63`).** See B1 — and note it *refuted the reconciler's own
diagnosis*, which is the healthiest thing on this page.

**The two collided, and `origin/master` was RED — fixed in this pass.** #555 added
`nightly-factory.yml` with two window gates in the OLD inlined `-ge 0 -lt 10` form; #556 landed the
gate requiring every band to be declared as machine-readable `WINDOW_START`/`WINDOW_END`. Neither PR
saw the other, so `TestNightlyWindowAdmitsMeasuredLandings` and
`TestNightlyWindowGateShellMatchesDeclaredBand` failed on master. This was not cosmetic: the
brand-new factory nets had inherited **exactly the fake-green band shape** the older nets had just
been cured of — the newest safety net wired with the defect that had hidden a reproducible planner
fault for twelve days. Both gates converted to the wrapping 18:00–12:00 band, with `measuredLandings`
entries pooled from the shared `hetzner-fdb` queue and **labeled as pooled** rather than passed off
as this job's own history (all eleven windowed jobs share one slot, so the allocation hour is a
property of the queue). The two no-op guards were tightened 9 → 11, since at 9 they would have
tolerated silently losing the two new jobs.

This is the generalizable finding: **the merge queue does not run the gate a PR adds against the
files a concurrent PR adds.** Two green PRs produced a red master, and nothing in CI covers that
axis today. Recorded as CQ-81, with the residual (replace the pooled landing hours with each job's
own once it has scheduled runs) stated in place rather than left implicit.

*Refuted from earlier in this same revision, and left visible rather than silently edited:* an
earlier draft of this section described all three as unlanded, on the strength of a snapshot taken
before the merges. That was accurate at `f5c2c7f0e` and wrong within hours. The lesson is the one
this page keeps re-learning: **a status claim needs its measurement SHA attached, or it is a claim
about a tree that has moved on.** Every count here now carries `a1d281a63`.

Still not landed: nothing from the previous revision's in-flight list remains outstanding.

**One unexplained red, recorded rather than dismissed (CQ-82).**
`//conformance:conformance_test` failed once in a fresh full-suite run at `98d79a2ef` and has not
reproduced — not alone (passes in 196s), and not under a forced fresh run of all 73 targets at the
same concurrency (73/73 pass). The failing run took 524s, ~2.7x its isolated time, which points at
resource starvation rather than a logic defect: the conformance suite holds a pool of 8 JVMs at
250-400MB each and its own source documents ">30s per-request Java-side hangs" as a failure mode.
That is a hypothesis, not a finding. **The failure message and the identity of a second failing
target were lost because that run's output was piped through `tail`** — a self-inflicted gap, stated
because a status page that hides its own measurement errors is the thing this revision exists to
stop. Three green runs are on record and they do not settle it.

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

- **CQ-80** — watch-list entries 2, 4, 8 and 12 do not meet the contract this section states.
  Entry 2's test cannot go red; **entry 4 has no test at all** and is the worst of the four (a
  wrong-data divergence resting on prose); entry 8 pins only the Go half; entry 12 has no test and
  its lowercase half was fixed without the entry being narrowed. This is a code/test lap,
  deliberately not done in a docs-only pass, and booked in full so the next fixer does not re-derive
  which four and why.
- **CQ-79** — CQ-53's surviving producer mint (`cascades_translator.go:3598`) is owned by no open
  item: CQ-53 is marked `[x]` as carrying no remainder while the ratchet pins the mint as "CQ-53's
  surviving producer". Checked and NOT owned by CQ-68, which is a different axis (untyped QOV, not a
  manufactured row key). Re-verified at `a1d281a63`: the pin stands verbatim.
- The boolean-CASE folklore comments are **GONE** (#555). `walk.go` dissolved into the unified walk;
  the surviving comment states the corrected fact and cites the probe. Recorded because the shape of
  the error is worth keeping: two code comments and four test pins agreed with each other and all
  five were wrong, none having asked the Java server.

## Sequence

1. **Confirm B1.** The window fix is merged and five validation dispatches are in flight; Tier 1
   turns on the first genuinely green reconcile. Watch it, do not assume it. Nothing to build.
2. **B2 explicit-tx isolation.** The Tier-2 gate and the top item outright. RFC-198 is
   review-complete; implement it. L, plus its SimFDB acceptance harness.
3. **CQ-80** — pin the four watch-list entries that claim a test they do not have. Small, and it is
   what makes the list handable to an adopter.
4. **RFC-197 tail**, sequenced behind the machinery each stop waits on: CQ-52's remaining producers,
   then CQ-51 and CQ-79 (the CQ-53 mint), then CQ-68 (the largest block). All review-gated.
5. **CQ-46**, index candidacy inverted to opt-in per maintainer factory, with the adjacent opt-out
   leaks measured for reachability. Query-engine gated.
6. **CQ-75** — `v IN (-0.0, 0.0)` silently loses a row, order-dependently. Wrong rows, and now that
   the factory exists it is the kind of shape the factory should be generating.
7. **CQ-30**, B3's residual: criterion 2's data-access maxima are still forked.
8. **B5** typed metadata flow. L, after the migration proves the identity machinery everywhere.
9. Watch-list handed to any prod adopter, once CQ-80 has pinned it.
