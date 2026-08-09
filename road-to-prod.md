# Road to production

**Revision 2026-08-05.** Amends the 2026-08-04 revision in place: adds the MT-SaaS section
(`#624`, `#625`, `#626`, `#620`, measured at `2e4b9f930`) and **retires four watch-list claims that
verification refuted** — entry 13 (inverted), entry 14 (fixed, and it was the page's "one OPEN
wire-compat divergence"), and both client-side watch claims. B1/Tier-1 confirmed at `5ab0a87a3`,
B2/Tier-2 closed at `d6f635073`. Counts below were measured against `a1d281a63`
(= `origin/master` at the 2026-08-01 drafting) unless a later SHA is cited inline. Supersedes
the 2026-07-29 revision.

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

1. **The safety nets are now PROVEN, and the mechanism got honest before it got green.** B1's
   detection (#523) worked exactly as designed and reported that three of nine nightly nets had
   never recorded a genuine run. #556 then found the reconciler's *diagnosis* wrong — the lanes were
   fine, the window band was the wrong shape — fixed it across all five nets, and published the
   number nobody wanted: **107 of 177 scheduled runs over these lanes' whole life were fake-green**.
   **Confirmed 2026-08-02/03**: the reconciler has now passed two consecutive nights (runs
   30744450066, 30814146026) with every net showing a genuine, artifact-backed run inside its
   limit — Tier 1's gate is satisfied.
2. **The read-side safety net roughly doubled.** The generation factory (#555) landed 2000 blessed
   scenarios, and it paid for itself on its first sweep by finding a resolver defect that broke six
   boolean operand shapes — including one, boolean-CASE in WHERE, that this page had listed as a
   deliberate parity gap for months. It was not deliberate; it was unmeasured.
3. **Documentation authority was itself a defect (B6), and this pass is its fix.** Three status docs
   contradicted each other; the corrections are recorded inline below rather than applied silently.

Explicit-transaction isolation (B2) — formerly the single most dangerous defect for a real
application — is CLOSED as of 2026-08-04 (#607, merged as `d6f635073`). Tier 2 is confirmed.

## Deployment tiers

### Tier 1 — record-layer data plane + auto-commit SQL, bounded contexts
**Distance: CONFIRMED 2026-08-03.** The nightly-net gate is satisfied (below); the CQ-46 residual
stays open as a booked item, not a tier gate. Two items:

- **The nightly nets: genuinely green, twice.** Reconcile runs 30744450066 (08-02) and
  30814146026 (08-03) passed with all eleven nets showing artifact-backed genuine runs inside
  their limits (stress, rowdiff, oracles, coverage, factory-corpus, factory-batch, and the five
  fuzz lanes: diff/race/binding/client/engine — ages 0–1d against 3–7d limits, `STALE=0`). The
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
  **Status: CONFIRMED.** The reconciler passed on 2026-08-02 and again on 2026-08-03 with every
  net inside its limit and no vacuous pass possible (the reconciler fails on a missing artifact,
  not just a green badge). The "not before" discipline held: three red reconciles (07-30 to
  08-01) preceded the first genuine green.
  *Corrected from earlier in this revision:* an earlier draft blamed runner contention alone and
  called the residual "not started". Contention is real, but the band's shape was the defect, and
  `TestNightlyWindowAdmitsMeasuredLandings` (replaying every allocation hour each job has really
  been given against the band it declares) is the axis the old gate could not see.
  *Found and fixed while verifying this paragraph:* `nightly-factory.yml`'s two jobs still carried
  the old inlined band and made master red against #556's new gate — see "Landed since the audit".
- **Index candidacy is opt-OUT in Go where Java is opt-IN (CQ-46 in `TODO.md`, open).** The
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
| Generation factory corpus (NEW, #555) | **5000** scenarios · **20000** tests · **4952** feature vectors; blessings **4469 `metamorphic` + 531 `metamorphic-tlp-only`**, labeled in every header | **Yes** — componentwise census ratchet over scenario/test totals and each feature vector, plus per-scenario authority keyed by dedup key; `ByBlessing` is report-only (`factorycorpus/census_baseline.json`) |
| `.Field`-decides ratchet (RFC-197) | **48** sites, per-bucket totals gate-checked | **Yes** — `TestFieldNameNeverDecides` + `TestFieldDebtBucketsArePartition`, and `TestStatusPageQuotesTheLiveFieldDebt` for the numbers ON THIS PAGE |

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
**CONFIRMED 2026-08-04.** B2, the Tier-2 gate outright, is closed — #607 merged as `d6f635073`:

- The defect this gate existed for: inside `BeginTx`, DML joined the FDB transaction but SELECT ran
  in a fresh auto-commit transaction — no read-your-writes, no read-conflict ranges, so
  read-modify-write across two transactions was last-writer-wins with no 1020/40001. **Silent lost
  updates.** The isolation probe (`tx_select_isolation_probe_test.go`) now pins the CORRECT
  semantics.
- Shipped as **RFC-198** (`rfcs/198-explicit-transactions-read-your-writes.md`): all five phases,
  including the SimFDB/RFC-199 acceptance harness with injected 1007 and both 1021 branches. The
  review trail: joint Graefe+Torvalds lap ACK'd at the phase boundary; 1M stress clean (−0.9% wall,
  plans byte-identical); the GRV-cache span that grew out of OQ-1 went through a C++-client +
  Torvalds design review (one structural reshape: single packed fence word, epoch in the DBInfo
  snapshot, epoch-stamped min-acceptable floor) and fifteen codex rounds, every finding landed.
  The durable lesson from that span: five successive tests were green for reasons other than the
  property they named, and every one was caught by RUNNING the mutation, never by reading the test.

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
| B1 | Nightly safety nets were fake-green (window gates anchored to cron hours GitHub dispatches 2-4h late; 12 fake-green stress nights; rowdiff window unreachable by construction; oracles never ran) | Unknown-risk factory | S → M | **DONE — confirmed genuinely green 2026-08-02 and 08-03 (reconcile runs 30744450066, 30814146026, all eleven nets artifact-backed inside limits).** Detection merged (#523); the window shape fixed and merged (#556). #523 gave every windowed job a heartbeat and made the reconciler fail on silence — which then correctly exposed that three fuzz lanes had never recorded one. #556 found the cause was the band's shape, not the lanes (a non-wrapping band calling 18:00–24:00 "daytime"), fixed it across all five nets, and published the honest history: **107 of 177 scheduled runs were fake-green**. Stress 07-17 root-caused (see Tier 1, CQ-46); binding-stress 0/50 root-caused and fixed 2026-08-05 (CQ-47) |
| B2 | No read-your-writes in explicit transactions; SELECTs take no read locks → silent lost updates | Wrong data | L | **DONE — merged 2026-08-04 (#607, `d6f635073`), Tier 2 confirmed.** RFC-198 all five phases; joint Graefe+Torvalds lap ACK'd; 1M stress clean; the OQ-1 GRV-cache span survived a C++-client + Torvalds design review (fence reshape) and fifteen codex rounds, every finding folded before merge |
| B3 | RFC-195: cost estimates contradict proven cardinality bounds; comparator uses a private cardinality walk | Wrong plans (perf), not wrong rows | M | **DONE, merged (#547.)** `rfcs/195-cost-must-not-contradict-proof.md:3` — "ACCEPTED, revision 3 … implemented". Seven shapes fixed in the end, not six; zero exclusions and no mechanism to add one (`cardinality_cost_bound_test.go:36-45`). **Residual: CQ-30 in `TODO.md`, open** — criterion 2's data-access maxima are still forked; held visible by a standing test |
| B4 | RFC-197 identity migration residual (see per-bucket table) | Plan/decline-direction only; wrong-rows channels closed | M | Active; ratchet-enforced; **68 at inception → 48 now** |
| B5 | WS-N Phase D: metadata re-derived by name instead of flowing from the type (production `UnknownType` mints: see the live census, `pkg/docscheck/unknown_type_mint_census_test.go` — 43 across 20 files at `aba271454`; five name-keyed guessers, enumerated in `shifts/handoff-ws-n-phase-d-typed-metadata.md:65-81`) | Wrong client VALUES on cross-leg same-name-different-type | L | Booked; gates the typed-row-representation work. Entry point: RFC-226 (projection states its row) |
| B6 | Documentation authority contradictory/stale | Trust/decision risk, not code | S | **This revision.** Authority headers added to `PRODUCTION_READINESS.md` and `rfcs/prod-readiness-go-client.md`; stale TODO entries fixed; `TestProductionStatusAuthority` added so the redirects cannot silently rot |

**B5's count was refuted and is corrected above, recorded here rather than quietly changed.**
It read *"~347 UnknownType mints repo-wide; three named guessers"*. Neither number was right and
the first counted the wrong population. `347` was a raw line count of *mentions* —
`git grep -n UnknownType a1d281a63 -- 'pkg/**/*.go' | grep -v '_test.go:' | wc -l` → **352** at
the SHA this page measured at — which folds mints, declines, comparisons and reads together. The
repo has an authoritative AST census of *mints* (`pkg/docscheck/unknown_type_mint_census_test.go`,
ratcheted, red in both directions): **43** across 20 files. The raw number has since risen to 417
while the real mint population *fell* (45 when the census landed at `1e64d6e75` → 43), so anyone
tracking B5 by its stated metric read progress as regression. The guessers are **five**, not
three (`shifts/handoff-ws-n-phase-d-typed-metadata.md:65-81` plus the surviving last-wins
`innerByName` map and `colref.go`). Note also that the census deliberately does **not** count
`return values.UnknownType` — a classified decline is often the *cure* for a mint — so B5's entry
point (RFC-226) will not move this number, and must not be judged by it.

### B4 residual, per bucket — MEASURED at `041838856`

These are the gate-enforced group headers in `pkg/docscheck/field_name_decision_test.go`, which
`TestFieldDebtBucketsArePartition` checks against the entries they advertise. The buckets are a
partition, so they sum to the list: **48**.

**The numbers in this table and the totals quoted around it are now gate-checked ON THIS PAGE**
(`TestStatusPageQuotesTheLiveFieldDebt`). They were not before, and the guarantee column above
said they were: the two ratchet tests check the debt list against ITSELF — entries against group
headers, headers against entries — and neither one reads this file. So the quote could drift from
its source, and it had. This table said `boundary 1` / total **52** while the list held
`boundary 2` / **53**; the second `boundary` entry arrived with `#601` (RFC-204 struct types,
`protoFieldByName` learning the escaped spelling) after the `a1d281a63` measurement this section
was written against, and nothing existed to notice. The figure being stale was survivable. The
figure being stale *under a column reading "drift-guarded: Yes"* is the fake-green shape B1 was
about, one level up and in the page an adopter is handed first.

| Bucket | Audit-day | Now | Wrong rows reachable in prod? |
|---|---:|---:|---|
| boundary | 0 | **2** | No. Not a regression: the call-boundary taint made a site visible that was always there (a name handed to a helper as a plain string parameter). Reporting the bucket migrated while the walk could not reach one of its members is the false green the pass existed to end. It rose 1 → 2 for a SECOND spelling attempt inside that same nested descent, not a second site; both retire on the same ordinal resolution |
| escape | 0 | **0** | Migrated (found + fixed a live wrong-type defect on the way) |
| contract | 11 | **12** | Not alone — single naming authorities. The four newly-visible entries are the group-by output name's CONSUMERS, laundered through one helper; the bucket had listed eleven producers and not one consumer, and a migration plan written against producers alone cannot close it. It fell 16 → 10 by RETIRING (and back to 11 with RFC-218's nested-key re-anchor) `AggregateResultColumnName`'s six arms: the aggregate operand's text comes from the parse text carried on the spec (name-as-DATA, Java's `Column.of(Optional<String>, value)`), and the leaf `.Field` fallback beside it was a second, DIVERGENT copy of the Value→name rendering that dropped a qualifier and collapsed `SUM(t.v)`/`SUM(u.v)` onto one output slot |
| dotted | 6 | **14** | The WRITERS are resolved; what remains are READERS that decline, each probe-pinned, plus five newly-visible MINTs |
| name-keyed | 3 | **4** | Measured machinery gaps, each recorded on its debt entry (planner-budget re-fire on constraint growth, CQ-51; lazy carriers with no other identity). It fell 5 → 4 by REMOVAL: the projection-merge site's recorded "HEAVILY LIVE" reason was refuted by counting instead of panicking — the rule fires 897 times across the relational suite and its name-matching arm takes ZERO of them, so it was dead debt and is now a fail-closed decline |
| translator | 17 | **15** | Bounded — resolution-time text handling; misbinding requires the 42702/42703 ambiguity checks to have a hole |
| harness | 1 | **1** | No — oracle-side; affects trust in the net, not prod rows |

**The total ROSE from 41 to 53 between the audit and now, and that is the gate working.** #540 and
#544 taught the detector to follow a display name across a call boundary and through helpers; sites
that were always making the decision became reportable. A ratchet whose count only ever falls is a
ratchet that has stopped looking.

*Refuted while verifying:* the previous revision's "68 at inception → 38" and its `dotted 6 /
translator 17` cells were audit-day figures presented as current. Measured trajectory of the list:
**68** at inception (#520) → **41** (#527/#528/#529) → **54** (#544) → **52** (#556) → **53** (#601) → **52** (`b3ac5fe31`) → **46** (`46a00357a`) → **47** (`f599685d2`, RFC-218 adds the nested-key re-anchor's name match) → **48** (HEAD, RFC-222 adds its nested-SUFFIX sibling). More
consequentially, the previous revision's sequencing said the remaining ratchet was the
boundary/contract tail. **It is not:** `dotted` (14) and `translator` (15) are the two largest
buckets and together are 63% of the list.

Two further corrections to the migration's bookkeeping, both found by reading the ratchet against
`TODO.md`:

- **CQ-52 is not done, but its residual is not what this page said.** #540 landed the PROJECTION
  channel; the parsed channels (ORDER BY keys, GROUP BY keys, aggregate operands) followed. The
  four debt keys this page cited (`cascades_translator.go:5742`/`:5744`/`:6070`/`:6102`) NO LONGER
  EXIST — the surviving entries carrying those reason strings are `:5811`, `:5813`, `:6139`,
  `:6171`. More importantly the framing was wrong: the residual is **one behaviour decision**, not
  four line-keyed sites. `cascades_translator.go:6218-6237` states it and deliberately leaves it
  open — *should a star-projected body column be leg-addressable at all?* The star-body
  normalization mints output labels with no parse tree behind them, so their absent segment triple
  is STRUCTURAL and permanent; the remaining producers are machinery mints whose names are
  aggregate renderings (`MAX(E.SALARY)`), where a dot is deliberately not a qualifier. **STOPPED on
  an owner decision** — but NOT because Java is silent, which an earlier draft of this bullet
  claimed. Java *does* have projection outputs carrying no `Identifier`: `Expression.name` is an
  `Optional<Identifier>` (`Expression.java:100-113`) and `Expression.ofUnnamed`
  (`:305-322`) mints empty ones throughout. Its answer to those is that they are **not
  name-resolvable at all** — `SemanticAnalyzer.lookup` skips an attribute with no name outright
  (`SemanticAnalyzer.java:459-461`), never matching it positionally or through a synthesized
  string. What is genuinely Go-side is the *re-split*: Java never re-parses a dotted string
  (identity is `name` + `List<String> qualifier`, `Identifier.java:34-58`, built segment-by-segment
  at `IdentifierVisitor.java:56-64`, joined only for display at `:61-63`), so it has no analogue of
  the arm that would answer the question by accident. The caveat that used to sit here — "nothing
  instruments the split population, so the 110 → 0 figures are scratch measurements" — is
  CLOSED **for the two leg bakers, and for nothing else**: `name_split_census.go` counts those two
  arms per resolution decision and `AssertNameSplitCensus` gates them in the sqldriver `TestMain`.
  Measured, stable across two consecutive full-suite runs: `legQOVSegmentsOf` 9 calls (segmented 9,
  **SPLIT-QUALIFIED 0**), `flatColumnBake` 2 calls (splitBare 2, **SPLIT-QUALIFIED 0**). The zero is
  confirmed; the population is 11, not the ~110 the scratch figure implied. Four splitting siblings
  remain uninstrumented by anything — `recursiveRemapValues` (`cascades_translator.go:9547`, which
  manufactures a `CorrelationIdentifier` outright), `parseColRef` (`core/embedded/colref.go:18`, 27
  production call sites), `splitQualifier` (`:5016`) and the derived-unnest base-column split
  (`derived_unnest.go:107`) — named in the census header and booked as **CQ-94**; do not read
  these zeros as global. What carries the guarantee is the floors PLUS a unit wiring pin, not the
  floors alone: the split population is floored per site (`flatColumnBake` 1, measured 2), and
  `legQOVSegmentsOf`'s is a declared **0** — "watched, not proven", checked in the stale direction —
  because that arm is measured empty over the corpus while being demonstrably LIVE (a panic there is
  reached with a dotted name), so its recorder wiring is pinned by unit test instead. And the
  behaviour question at the arm is **RULED, not open**: Java skips a projection output carrying no
  `Identifier` before any comparison, so a machinery label is not name-resolvable by any spelling and
  the arm DECLINES — the gate's failure text now states that instead of telling the tripper to
  escalate. What stays the owner's is only whether the star-body normalization should stop minting
  such labels at all, which is upstream of this arm.
- **CQ-53 is marked done but has a surviving producer.** `TODO.md`'s CQ-53 closes it as subsumed by
  CQ-67 (#549) "carrying no separate remainder", while
  `pkg/docscheck/field_name_decision_test.go:447` pins `cascades_translator.go:3598` as "dotted:
  MINT. **CQ-53's surviving producer**" — and the mint is live at that line, on the unnest-merge
  path. Its NLJ twin was deleted; this one "dies with the same work", and that work was owned by
  nothing. **This is a real gap between a closed checkbox and the gate.** Now booked as **CQ-79**,
  and deliberately NOT folded into CQ-68 — a different axis, and folding them would let either
  close while the other's residue survived. Re-verified at `a1d281a63`: the pin stands verbatim.
  **Re-verified again at `041838856`: the entry has moved to `cascades_translator.go:3667` (test
  line `:454`), and the item's `S/M` sizing is REFUTED — see below.**

  **The CQ-68 axis is stated wrong above and the correction is measured.** This text called it
  "94 bare untyped QOVs". CQ-68's premise has since been REFUTED by a real-FDB corpus run: the
  population is **102**, it is **100% typed** (every declined leg carries a real `RecordType` —
  arity 1-3 on the FlatMap legs, 1-4 counting the `RecordQueryNestedLoopJoinPlan`-legged ones),
  and typing cannot convert any of it, because `legOrdinalSafety` refuses on
  `values.IsPositionalMergeRC` — a `*RecordConstructorValue` assertion no `QuantifiedObjectValue`
  satisfies at any typing. It is a SHAPE residue, and the target now lives on **CQ-95**. Nothing
  about CQ-79's separateness changes; only the axis it is being distinguished FROM.

RFC-197 itself is **IN IMPLEMENTATION** (`rfcs/197-column-identity-is-an-ordinal.md:3`): step 0 and
items 2, 3, 5 and 6 have landed; the remaining items are unstarted and still gated.

### The RFC-197 tail, re-verified at `041838856` — none of the four is startable as a local edit

The sequence below (item 4) booked CQ-52's producers, then CQ-51 and CQ-79, then CQ-68. Verifying
each against the tree found **no genuinely-open local work in any of them**; every one is either an
owner decision or gated on a Graefe-reviewed RFC. Each item's TODO entry now carries the full
measurement. In summary:

| Item | Verified state at `041838856` |
|---|---|
| CQ-52 | **STOP — owner behaviour decision.** Parsed channels are done; residual is the star-body leg-addressability question at `cascades_translator.go:6218-6237`, plus structural mints that correctly carry no segments. Its two "migratable" producers are not: their names come from `buildPostAggregateProjection`, where a dot is deliberately not a qualifier |
| CQ-51 | **STOP — Graefe-gated RFC**, but the diagnosis is corrected and the RFC is now a PORT, not a design. Java DOES separate "constraint widened" from "re-push required" (`CascadesRule.java:66-77`, `CascadesPlanner.java:891-908`, `ConstraintsMap.java:246-261`), and Go already contains half of it: `expressions/constraints_map.go:114` `IsExploredForAttributes` is a faithful port whose only callers are its own tests. `ConstraintDependencies` appears nowhere in `pkg/` |
| CQ-79 | **BLOCKED, and the `S/M` sizing is refuted.** The ordinal twin already exists (`cascades_translator.go:3849`) and is already taken wherever the seed is windowed; every surviving call of the name mint sits in the `!seedWindowed` arm, whose merged row is name-keyed BY CONSTRUCTION. Converting the mint alone strands the read (`:3736-3738`). The work is lifting `unnestExistsSeedSafe`'s scope gate, which `:3841-3843` couples to the executor's below-FOD hoist — the SAME `bindMergedOuterLegs` widening CQ-68 owns. CQ-79 and CQ-68 are two axes of one piece of work |
| CQ-68 | **STOP — Graefe-gated RFC.** Premise strengthens: the count is **102**, not 94 (`sqldriver/embedded_fdb_test.go:253-266`; denominator 572, ACCEPT 160, merge still hard 0). But "type it at the FlatMap" is the wrong port — Java's `RecordQueryFlatMapPlan` also flows `selectExpression.getResultValue()` verbatim (`ImplementNestedLoopJoinRule.java:187,201,214`); the typing happens upstream at `GraphExpansion.java:401`, and is structural (`QuantifiedObjectValue.of` has no untyped overload) |

**One defect from this pass IS fixed**, because it would have made CQ-68 unfalsifiable: the census
witness that separates typed from untyped result values printed `typed=%t` from `Typ != nil`, which
no constructible QOV can make false (`NewQuantifiedObjectValue` stamps `UnknownType`;
`NewQuantifiedObjectValueOfType` degrades nil to it; `UnknownType` is a non-nil `*PrimitiveType`).
It reported `typed=true` for all 102, so a typing sweep would have read as complete on the day it
started with the whole population untouched. Now `quantifiedObjectValueIsTyped`, pinned in both
directions by `TestFoldStep1Census_BareQOVWitnessSeparatesTypedFromUntyped`.

## Watch-list — pinned divergences a prod user must be told about

The section contract is that **every entry is a committed test asserting CURRENT behavior; red means
fixed.** Verifying that contract for this revision found four entries that did not meet it. They
were marked, not removed — an unpinned divergence is more dangerous than a pinned one, and hiding it
would make the list read cleaner than the code is. **All four are now closed**: entry 2 refuted by
measurement, entry 4 fixed and pinned, entry 8 given the cross-engine pin its Java half was missing,
and entry 12 REFUTED and rewritten to the divergence measurement actually found — which runs in the
opposite direction to the one the entry claimed. Two of the four were inverted by measuring them,
which is the argument for the contract rather than an embarrassment to it.

Wrong rows / wrong data:
1. **RETIRED 2026-08-04: no read-your-writes in `BeginTx` (= B2) — FIXED by #607 (`d6f635073`).**
   Inside `BeginTx`, SELECT now joins the FDB transaction: read-your-writes holds, reads take
   conflict ranges, and cross-transaction read-modify-write conflicts surface as 40001. The probe
   (`sqldriver/tx_select_isolation_probe_test.go`) asserts the CORRECT semantics per the section
   contract, with a `dml_still_atomic_on_commit` control alongside.
2. **REFUTED and retired: "INSERT of NULL into a PRIMARY KEY column silently stores 0."**
   Measurement inverted the claim: Go stores the same REAL tuple null (0x00) Java does — Java raises
   no error here (a relational scalar column is never non-nullable, DdlVisitor.java:156-161;
   the null standin flows to the PK tuple, FieldKeyExpression.java:228-243 /
   TupleTypeUtil.java:148-151; proven live by yaml-tests/functions.yamsql:34). The old "pin" could
   not have seen this: it logged-and-returned on every path. Now REALLY pinned, Java-parity, by
   `sqldriver/null_pk_java_parity_test.go` (explicit NULL, composite partial-NULL, omitted PK
   column; each asserts the null key exists — duplicate-NULL collides 23505 — and that `id=0` does
   NOT collide, i.e. nothing was stored as 0). No divergence remains.
3. **FIXED by CQ-83 and generalized by RFC-208.** Correlated FLOAT/DOUBLE `=` on a non-terminal composite-index column now
   binds an exact runtime range set for both zero signs while retaining the suffix. Pinned positive
   by `correlated_zero_composite_sentinel_test.go` and
   `runtime_signed_zero_range_set_fdb_test.go`; this is retained in the numbered history but is no
   longer a production watch.
4. **RESOLVED: `CURRENT_TIMESTAMP` drifted across rows within one statement (SELECT path),
   including across PAGE boundaries.** The statement instant is captured ONCE per execution on the
   result set (`paginatingRows.statementTime` in `cascades_generator.go`) while the driver call's
   session-clock stamp is live — lazy pages fetched from `rows.Next()` after the driver call
   returned reuse it, never re-reading the (already restored) session clock. Every frontier row
   context exposes it as the `values.StatementClock`; operators wrap the bare frontier row exactly
   when their values reference the CURRENT_TIMESTAMP family (`values.DependsOnStatementClock`),
   and the ordinal join's baked result-value build carries it too (`ordinalJoinBuild.Clock`).
   Pinned red→green by `sqldriver/current_timestamp_stability_test.go` (single-page projection and
   predicates-filter shapes + cross-statement control),
   `sqldriver/current_timestamp_crosspage_test.go` (~20-page statements looped across second
   boundaries), `sqldriver/current_timestamp_join_shapes_test.go` (join/EXISTS/scalar-subquery
   battery), and `executor/ordinal_join_clock_test.go` (baked-build clock threading).
5. NaN comparisons follow Java's **boxed-predicate** total order, not IEEE —
   `(v/z)=(v/z)` returns ALL rows. Pinned — `nan_comparison_semantics_test.go`. PostgreSQL agrees
   exactly (NaNs equal and greatest); CockroachDB makes NaNs equal/canonical but sorts them least;
   Java's direct `RelOpValue` primitive `==` instead makes NaN unequal to itself. Raw FDB NaN
   sign/payload order is a separate indexed-access gap tracked by **CQ-93** (raw NaN primary-key
   identity vs logical DISTINCT / ORDER BY). It was previously cited here as CQ-84, which is a
   different item entirely (qualified star with GROUP BY) — the gap was unfindable under that
   reference.
6. Signed zero: Go is IEEE; Java's boxed predicate is bit-identity, while its direct
   `RelOpValue` primitive `==` agrees with Go — `WHERE d = 0.0` returns a stored `-0.0` row in Go,
   not through Java's boxed predicate path. Pinned — `plandiff/corpus.go:4726`
   (`DivergenceJavaWrongRowsGoCorrect`).
7. BIGINT vs DOUBLE above 2^53 — Java promotes lossily and wrongly matches; Go rewrites exactly.
   Pinned — `plandiff/corpus.go:4741`. **Do not read this as "Go is exact everywhere"**: a DOUBLE
   *column* against an integer constant is lossy in Go too, which is correct SQL and is separately
   pinned (`numeric_precision_boundary_test.go:104`).
8. `UNION ALL` + trailing `ORDER BY` — Java orders only the right leg; Go implements the standard
   combined-result semantics. **NOW FULLY PINNED** (was half-pinned: Go's side had
   `conformance/yamsql/testdata/union_columns.yaml`, the Java side rested on the prose record of a
   live probe in `DIVERGENCES.md`, not a committed test). Both sides are now measured on every run
   by `conformance/union_trailing_orderby_java_probe_test.go`, which asserts Go's full combined-sort
   sequence, Java's per-leg orders, and — directly — that Java's answer is NOT the combined sort, so
   a Java that started sorting the set operation reds the pin instead of quietly satisfying it.
   *Measured while pinning:* Java's `UNION ALL` does not concatenate its legs in a fixed order (the
   same unordered union returned `[1 6 2 5]` and `[2 1 6 5]` on different runs, the second
   interleaving the legs), so the divergence is asserted per-leg — each leg is a PK scan and its own
   relative order is stable. A pin naming a whole expected Java sequence would have been a flake,
   and the eventual "fix" for that flake would have been to loosen the assertion carrying the claim.

Different answer / different error:
9. **CLOSED.** `SUM(int_col)` now raises 22003 "integer overflow" identically with and without a
   join around the operand's table, matching Java (measured live: `SumOverflowJoinLegJavaProbe`
   in `conformance/` — Java rejects all four overflow shapes with the same verbatim messages Go
   now emits). Root cause: the width machinery predated RFC-181 P0.5's width-faithful typing and
   only consulted the merged-row ordinal a join-leg reference cannot index; the operand's own
   static type (Java's `NumericAggregationValue.encapsulate` rule) now decides. Pinned —
   `aggregate_operand_width_fdb_test.go` (`TestFDB_AggregateOperandWidthJoinLegRaises` plus
   negative-direction, exact-boundary, BIGINT-lane and AVG/COUNT controls). The same-family
   residual over DERIVED-TABLE and UNION-ALL aggregate inputs is closed too: derived sources
   already flowed the column's type; union-bodied derived tables now type their output row by
   Java's `Type.maximumType` fold over the branches (`buildDerivedTableSourceFromUnion`), which
   also un-gapped two RFC-082 "Go rejects" union entries (outer WHERE over a union-derived
   table now plans and matches Java's rows). Pinned —
   `TestFDB_AggregateOperandWidthDerivedAndUnionInputs`, including the INTEGER∪BIGINT
   promotion direction.
10. `DELETE/UPDATE … RETURNING` via Exec silently drops the returned values; via Query → 0A000. Java
    supports it. Pinned — `returning_clause_probe_test.go:51,62`.
11. `DROP SCHEMA IF EXISTS` ignores IF EXISTS (deliberate Java-bug replication). Pinned —
    `drop_schema_ifexists_conformance_probe_test.go:29`, with three sibling controls proving
    `DROP SCHEMA TEMPLATE` / `DROP DATABASE` DO honor it.
12. **REFUTED and INVERTED — the divergence runs the other way.** The claim was "a quoted DDL column
    is created but unreferenceable by name", narrowed to the mixed-case residue (`"KeepCase"`) that
    had been unmeasured since 2026-06-28 and survived only in a Go test comment saying it was "not
    exercised here". Measured on both engines: `SELECT "KeepCase"` returns the value on Go exactly
    as on Java, in projection and in a predicate. The column is referenceable. What measurement
    found instead is that **Go over-resolves where Java rejects**: Go reaches the column through
    `KeepCase`, `"KEEPCASE"` and `"keepcase"` alike, while Java treats quoting as case-preserving
    and raises 42703 for every spelling but the exact one; Go also reports the column folded
    (`KEEPCASE` vs Java's `KeepCase`). The permissive step is the case-insensitive fallback in
    `rlcatalog.recordTypeTable.LookupColumn`, whose comment scopes it to raw-proto metadata but
    which also swallows the quoted-case distinction. Now pinned both ways:
    `conformance/quoted_identifier_case_java_probe_test.go` (cross-engine, incl. unquoted-DDL
    controls in both case directions) and
    `pkg/relational/core/embedded/ddl_quoted_case_wire_test.go`.
    **Wire compat is intact, and that is measured, not assumed** — the stored descriptor keeps
    `KeepCase` verbatim off the real `CREATE TABLE` path, so a Go-created and a Java-created table
    carry the same field name and each engine still reads the other's records. The divergence is
    read-side name resolution only. It is **query-engine subject matter (the RFC-181 WS-N name
    model, whose Phase D is the case-preserving row layout)** and is surfaced, not fixed here.

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

**Added 2026-08-01 — pinned by the day's merges (#560, #565, #559), same red-means-fixed contract:**
13. **INVERTED by verification 2026-08-05 — the pin now asserts the opposite of what this entry
    claims.** The entry said `ResultSetMetaData.isNullable` reports NOT NULL for a column on the
    null-supplying leg of a LEFT JOIN, and that
    `sqldriver/cross_leg_null_born_fdb_test.go` "asserts the metadata self-contradiction (NoNulls
    declared, SQL NULL delivered in the same test)". On master that test asserts
    `nullable == true` (`:108-113`) and its header (`:18-27`) states the hole is UNREACHABLE
    through the driver metadata surface: the NoNulls derivation keys on proto REQUIRED
    cardinality, scalar `NOT NULL` is unexpressible in DDL (Java parity — `NOT NULL` is allowed
    only on ARRAY columns), and an ARRAY `NOT NULL` column is flat REPEATED, not REQUIRED. So no
    DDL-expressible column ever derives `ColumnNoNulls`. Corroborated by
    `sqldriver/aggregate_over_mixed_nesting_outer_join_fdb_test.go:154`.
    The test is now a **negative-result pin** with a named re-arm trigger: make scalar `NOT NULL`
    expressible and the agreement-gate hole comes back. The honest user-facing statement is
    "`isNullable` is uniformly nullable for scalars; do not infer a NOT NULL constraint from it",
    which is what `docs/mt-saas.md` §8 says. D3 positional metadata is still the right fix for the
    underlying gate; it is no longer a *reachable* divergence.
14. **REFUTED and RETIRED 2026-08-05 — FIXED by `ba5f78958` (RFC-204 P1+P2+P3, #601).** The entry
    claimed Go writes a plain repeated field where Java wraps nullable arrays in a
    `{repeated T values = 1}` message, that `[]` and NULL collapse on the Go wire, and that this was
    "the one OPEN wire-compat divergence on the hard line". All three clauses are false on master.
    The wrapper is emitted (`core/metadata/builder.go:929-937`, `wrapperFor` at `:1069-1090`,
    contract stated at `:760-762`); `[]` and NULL are distinct on read-back
    (`sqldriver/array_literal_insert_fdb_test.go:143-156` and `:200-207`); and the wire bytes are
    pinned byte-equal to the directly-protobuf-built message (`:217`, header `:277-282`).
    One residual, and it is not a compat break: Go derives the wrapper's type NAME deterministically
    from `(table, element type)` where Java mints a fresh UUID per serialization. Every Java reader
    is structural (`NullableArrayUtils.isWrappedArrayDescriptor` checks shape, never the name), so
    any name is wire-valid — recorded at `builder.go:1058-1067`.
    **Consequence for this page:** there is no longer a known-open wire-compat divergence on the
    hard line. The nearest live item is entry 15 (struct descriptor emission), which is LATENT —
    `parseColumnType` rejects structs (`core/embedded/ddl.go:760-801`), so it is unreachable today.
    *How this survived:* the entry was carried forward across two revisions while the fix landed in
    a PR that never touched this file. A status page's claims rot in exactly the direction that
    flatters it least — this one was scarier than the code.
15. Struct descriptor emission (latent, wire-critical): Go's dormant struct path emits nested
    `.Table.Struct` descriptors with `LABEL_REQUIRED` where Java emits top-level
    `LABEL_OPTIONAL` messages — persisted-catalog bytes the moment structs become reachable.
    Unreachable today (`parseColumnType` rejects structs); RFC-204 Phase 1 reworks emission
    BEFORE making it reachable, with Java-server byte-goldens as the acceptance instrument.
16. Error-class divergences measured by the corpus grind, each pinned at its exact rejection in
    `javacorpus/gaps.go`: `[1] = [1]` evaluates NULL (array comparison semantics); duplicate
    GROUP BY expression in an index → Go 0AF00 where Java answers 42702; `SELECT *` after
    JOIN USING shows the right-side USING columns Java hides; typed float literal `1.0f`
    unsupported. Wrong error/wrong shape, not wrong rows.
17. `LIKE` newline semantics now match Java's measured behavior (no DOTALL; `$` before one final
    line terminator): `WHERE name LIKE 'abc'` matches `"abc\n"`. Deliberate Java-parity
    (verified against a live JDK over 1.2M cases), pinned in the LIKE test tables — listed here
    because it will read as a bug to anyone who hasn't seen the derivation.
18. ~~ON-DISK MIGRATION (CQ-89 → CQ-90): stale `CARDINALITY()` index entries written by pre-fix
    Go.~~ **RETIRED — UNREACHABLE IN PRODUCTION.** Not "migration proven": *unreachable*. The
    product is pre-release, so every production store will be created fresh by a post-CQ-89 binary.
    No pre-fix binary will ever have written a store that carries into production, and the stale
    `0x00` entry can only exist on a store some pre-fix binary wrote. The hazard therefore survives
    only on **carried-forward dev/test data**, where the recipe in the appendix below applies, and
    for **external adopters** who ran a pre-fix Go binary.
    **The premise, stated so it can be checked rather than assumed:** this retirement rests entirely
    on "no production store predates CQ-89". If a store written by a pre-CQ-89 Go binary is ever
    promoted into production — a restored dev backup, a cluster carried forward, an adopter's
    existing data — this entry is **re-armed** and the appendix recipe becomes mandatory for its
    cardinality indexes. That is the one fact to re-check, and it is a deployment fact, not a code
    one.
    CQ-89 changed the index key for an EMPTY non-nullable (flat repeated) array from a NULL key to
    the integer key `0` — measured, the entry key moved from `indexSubspace ‖ 0x00 ‖ pk` to
    `indexSubspace ‖ 0x14 ‖ pk`. Java always keyed `0`
    (`CardinalityFunctionKeyExpression.java:115-117`), so this REDUCES Go-vs-Java divergence; the
    stale entries are Go-vs-Go. **Nothing rewrites them.** Until the affected indexes are rebuilt
    (`OnlineIndexer`), an index-backed `CARDINALITY(a) = 0` can MISS rows a full scan returns, and
    `CARDINALITY(a) IS NULL` can return rows on a column whose type forbids NULL — on records
    written by a pre-fix Go binary only. Scope is narrow: only a CARDINALITY index over a NOT NULL
    array column that has held empty arrays. The base-table read half of CQ-89 stores nothing and
    needs no migration.
    **Sharper sub-hazard, on UNIQUE cardinality indexes.** The maintainer skips the uniqueness
    check when the entry key contains a NULL (`!indexKeyContainsNull`; Java:
    `StandardIndexMaintainer.java:471`). Under the OLD key two records with empty NOT NULL arrays
    BOTH keyed NULL and so were never checked — an existing store may already hold a pair the new
    key considers impossible. A rebuild surfaces that as a uniqueness violation. **That is the
    correct loud outcome, not a rebuild bug** — the invariant was silently false before, and the
    rebuild is what makes it audible. Latent today: `builder.go:606-608` honours the flag but no
    SQL DDL route reaches it. Pinned — `TestFDB_ArrayCardinalityUniqueIndex`.
    Superseded ruling (CQ-90): DOCUMENT, DO NOT FORCE — no `LastModifiedVersion` bump, because
    pre-production means every affected store is dev/test. Superseded not by the trigger firing but
    by the trigger becoming unreachable: production stores are all created fresh. See the appendix.

Unsupported on both engines: `COUNT(DISTINCT)` (0AF00), `UNION`/`EXCEPT DISTINCT` (0AF00; the
`UNION`-inside-a-recursive-CTE carve-out DOES work — `logical_predicate.go:6294`),
`EXCEPT`/`INTERSECT` (42601 — absent from the grammar, not a planner decline),
`x IN (SELECT …)` (0AF00), `NULLIF` (0AF00), `DECIMAL`/`NUMERIC` (**42601** — lexer tokens absent
from `primitiveType`, so the PARSER rejects them, not the DDL visitor's 0A000 arm; pinned
2026-08-05 by `core/parser/decimal_type_rejected_test.go`), FK/CHECK/defaults (42601),
window functions (0AF00, except the vector K-NN `ROW_NUMBER() OVER (…)` in `QUALIFY`, which works
on both).

**Corrected 2026-08-05: string functions are NOT in that list.** Go implements the whole family
(`UPPER`/`LOWER`/`SUBSTRING`/`TRIM`/`CONCAT`/`REPLACE`/`POSITION`/`REVERSE`/the `*_LENGTH` set) as
an RFC-087 read-side extension (`values/scalar_function_catalog.go:363-370`, rows pinned in
`yamsql/testdata/string_functions.yaml`, real-FDB `sqldriver/embedded_fdb_test.go:3866`); only Java
rejects them. Zero wire impact, so it is a sanctioned extension — but it is a **portability**
caveat, not an unsupported feature: that query will not run on Java.

*Secondary drift found while correcting this line, and FIXED in the same pass:* the ANSI roster
recorded `COUNT(DISTINCT)` as 0A000 and `NULLIF` as 42883 where the code emits 0AF00 for both, and
`SQL_CONFORMANCE.md` listed Go's string AND math functions as `N` when both are Go extensions with
committed row-asserting corpora (`string_functions.yaml`, `trim_concat.yaml`,
`numeric_functions.yaml`). The roster `Note` fields are free-text prose no test asserts, so the
corrections are safe; they were made rather than filed, because four documents agreeing with each
other about an error code none of them had measured is exactly the failure this section exists to
catch.

### Rebuild-path correctness (landed with CQ-90, and the reason that work matters)

This is engine behaviour on the path **every future index addition takes under a live tenant**, not
migration tooling. It was found while proving the CQ-89 migration and outlives it entirely.

*The inline rebuild's uniqueness semantics were wrong in Go, in two different directions before they
were right.* Java's `checkVersion` rebuild attempts `markIndexReadable(index)` — the one-argument
form, i.e. `allowUniquePending=FALSE` (`FDBRecordStore.java:4602` → `:3767-3768`) — so
`checkAndUpdateBuiltIndexState` (`:3821`) refuses on a violation (`:3856-3861`); the index is
neither made readable nor parked in `READABLE_UNIQUE_PENDING`. But `rebuildIndex` then **swallows**
that refusal: the chain ends in `.handle((b, t) -> { if (t != null) logExceptionAsWarn(…); …;
return null; })` (`:4602-4615`), and a handle returning normally completes the future normally. Java
pins the net effect itself in `FDBRecordStoreUniqueIndexTest.addUniqueIndexViaCheckVersion:615-627`
— index WRITE_ONLY, every conflicting primary key in the violations subspace, and the transaction
**commits**.

Go originally called `MarkIndexReadableOrUniquePending` unconditionally, silently parking a
violation in a state nobody opted into; a first correction then made the failure propagate, which
turns any index addition that meets bad data into a **store-open outage** Java does not have. The
landed behaviour is Java's: the open commits, the index is left WRITE_ONLY and therefore *not
scannable* (so queries fall back to base scans and no read is wrong), and the violations are durable
and name both records — Java writes the pair in both directions,
`StandardIndexMaintainer.java:497-498`.

`READABLE_UNIQUE_PENDING` is **not** the default landing spot. Java core reaches it from one caller
only, `IndexingBase.java:324`, gated on `IndexingPolicy.shouldAllowUniquePendingState`
(`OnlineIndexer.java:1117`) — an opt-in defaulting FALSE (`:1220`, javadoc: *"allow=false (default,
backward compatible): throw an exception"*) that also requires format version >=
`READABLE_UNIQUE_PENDING` (`FormatVersion.java:145`). Go now mirrors that via
`OnlineIndexerBuilder.SetAllowUniquePendingState`, and **both conjuncts** are pinned — the opt-in
and the format version.

*`StoreBuilder.SetFormatVersion`, and the feature gates it made necessary.* Proving the format
conjunct needed a store below format 9, which Go could not produce: `maybeUpgradeFormatVersion`
raised every store to the newest version the binary knew, on every open. Java takes that target from
the **builder** (`FDBRecordStoreBase.BaseBuilder.setFormatVersion`, `:2245`/`:2266`) precisely so a
rolling upgrade can pin every instance to the OLD format and stop any one of them writing a layout
the others cannot read. Go now has it (plus the `OnlineIndexerBuilder` twin, which Java gets free by
reusing the caller's store builder); the upgrade targets it rather than a constant, it is a ceiling
that never downgrades, an explicit `SetFormatVersion(0)` is rejected rather than read as unset, and
default behaviour is unchanged.

Pinning a version made a **wire-compat** gap reachable and it is closed with it: every version-gated
store-header feature WRITE now checks the ACTUAL header version, porting Java's per-site
`isAtLeast` guards — header user fields (`:3222`), record-count state (`:3443`), store lock state
(`:3478`/`:3494`), incarnation (`:3503`/`:3517`), and the record-count key/state written at store
creation (`:5950-5957`). Without them a store pinned at 11 wrote a v12 lock into an 11 header, and
an older instance — the very instance the pin exists to protect — ignored a lock it should have
honoured.

### Appendix (reference): the CQ-89 cardinality-key migration

Not a production action — see entry 18. This is the runbook for the two cases where a store can
still carry pre-fix `0x00` cardinality entries: **carried-forward dev/test data**, and **external
adopters** who ran a pre-CQ-89 Go binary. It is kept because the tests behind it pin engine
behaviour that runs on every future index addition, not because a fleet migration is pending.

The mechanism, not a migration of any particular schema: an index's `LastModifiedVersion` is
per-schema metadata, so nothing in the engine can bump it on an operator's behalf. Pinned —
`sqldriver/cardinality_stale_key_rebuild_fdb_test.go`, which reconstructs the pre-fix on-disk state
byte-for-byte (rewriting each cardinality-0 entry to the `0x00` key, which is the *only* thing
CQ-89 changed on the write path), reproduces the defect, runs the migration, and asserts both the
corrected answers and the absence of any surviving NULL-keyed entry.

*No automatic engine-side bump, and that is a decision.* Java has NO mechanism that force-rebuilds
an index because the library's own key derivation changed between releases: the trigger is purely
meta-data-driven (`checkVersion` → `getIndexesToBuildSince(oldMetaDataVersion)` → each index's
`lastModifiedVersion`), and `FormatVersion.java:65-66` says so in as many words while contrasting
the record-count key, which IS store-header-detected. The documented contract
(`Index.java:614-619`) is aimed at schema authors: "Any record store older than this will need to
have the index rebuilt." A Go-only auto-bump would make Go emit metadata bytes Java's builder never
produces — a wire divergence in the exact direction CQ-89 set out to remove.

**THE RECIPE.** Raise the CARDINALITY index's `LastModifiedVersion` past the stored metadata
version and raise the metadata version to match; leave `AddedVersion` alone (`MetaDataEvolution
Validator` requires it unchanged, and only `LastModifiedVersion` drives the rebuild). Bump **only**
the affected index — every index named in `indexesToBuild` is excluded from the record-count
sources that pick the rebuild policy, so bumping the whole schema drags any COUNT index along, the
count degrades to `MaxInt64`, and every index lands DISABLED regardless of store size. On the next
store open `checkVersion` reconciles: at or below `MAX_RECORDS_FOR_REBUILD` (200) it rebuilds
inline inside the open transaction; above it the index is left DISABLED — not scannable, so nothing
can read stale answers from it — and an explicit `OnlineIndexer` run completes the migration and
returns it to READABLE. **A real record count requires a COUNT index in the metadata**: without one
`getRecordCountForRebuildPolicy` falls through to an emptiness probe and reports `MaxInt64` for any
non-empty store, so the DISABLED + `OnlineIndexer` arm is the path a relational SQL schema takes **by
default**. *Narrowed 2026-08-05:* "every relational SQL schema" overstated it. SQL can create a COUNT
index explicitly (`CREATE INDEX … AS SELECT COUNT(*) … GROUP BY …`, `RelationalParser.g4:172` →
`core/metadata/builder.go:1176`, pinned by `yamsql/testdata/aggregate_index_count_star.yaml:13`) and
implicitly (the auto-emitted `__GROUP_COUNT` companion beside any grouped aggregate index,
`builder.go:660`), relational primary keys ARE record-type-prefixed (`builder.go:1232`,
`:1239-1240`), and a grouped COUNT index still qualifies as a count source
(`store_builder.go:900`). So a schema carrying one flips to the INLINE arm — which is not
automatically better, since that rebuild runs inside the store-open transaction. Which arm a store
takes is a per-schema fact, not a layer-wide one.

### Correctness-elimination order (2026-08-01)

The path to zero KNOWN correctness errors, in execution order. "Done" for each = the watch-list
entry's pin goes red and the entry is retired with the fix cited.

1. **B2 / RFC-198** (entry 1) — **RETIRED 2026-08-04**: #607 merged (`d6f635073`), the isolation
   probe pins the correct semantics, full review trail closed (joint lap, C++-client + Torvalds
   design review with fence reshape, fifteen codex rounds).
2. **The unpinned-or-unowned batch** (entries 2, 4, 3, 9) — INSERT-NULL-into-PK
   (measurement REFUTED the entry: Java allows NULL PKs and Go matches — real pins landed,
   fake pin deleted, on `fix/watchlist-insert-null-pk-timestamp`), CURRENT_TIMESTAMP drift
   (fixed statement-scoped on the same branch; cross-page fold delta-ACK'd, PR #572),
   correlated-float signed-zero composite (RFC-196 — FIXED via execution-time probe fork,
   Graefe-ACK'd, PR #571), SUM overflow inconsistency (in flight — Java-behavior measurement
   first, then its own gated lap since it flips answers into errors).
3. **RFC-202 S7** — covering/DESC plan shapes over generated indexes (KeyWithValue split
   discarded at the planner seam); in the S5-S9 agent's queue.
4. **D3 positional metadata** (entry 13) — **demoted 2026-08-05**: the cross-leg nullability class
   is UNREACHABLE through the driver metadata surface (see entry 13), so this is architectural
   hygiene sequenced with RFC-204 P4, not a live correctness item. It re-arms the day scalar
   `NOT NULL` becomes expressible.
5. ~~**RFC-143 §3a** (entry 14) — the open wire divergence.~~ **DONE** — closed by `ba5f78958`
   (RFC-204 P1+P2+P3, #601); see entry 14. **No known-open wire-compat divergence remains on the
   hard line.**
6. **Error-class divergences** (entry 16) — batch of small Java-parity fixes, each already
   pinned at its rejection.

Everything in "deliberate, documented" (entries 5-8, 11, 17 + the client-side operational list)
stays: those are Java-parity or standard-vs-Java calls with the reasoning recorded, not defects.

Client-side operational — **two of these were REFUTED by verification 2026-08-05 and are corrected
in place**:

- ~~watches register at read version, not commit-gated~~ **FIXED** (`5a8856e7c`, RFC-170 finding
  #8). On the async `fdb` facade — the API an application uses — the watch registers at the
  COMMITTED version, so `Set(k,B); w=Watch(k)` stays pending until the next EXTERNAL change instead
  of firing on the transaction's own write (`fdb/transaction.go:457-459`,
  `client/transaction.go:2221-2226`, mirroring C++ `setupWatches`). Pinned cross-client by
  `fdbgo/bench/differential_watch_test.go:266-272`. The synchronous low-level
  `client.Transaction.Watch` still registers at the read version, deliberately — it blocks in
  `WatchPoll` until the watch fires, so it structurally cannot wait for a commit
  (`client/readpath.go:1150-1155`). That residual is the only surviving half of this entry.
- ~~no `too_many_watches` limit~~ **FIXED** (`4db86c31b`, `3bc19dba7`, `af364d324`). The per-Database
  cap exists and 1032 is enforced: default 10 000 (`DEFAULT_MAX_OUTSTANDING_WATCHES`), ceiling
  1 000 000 (`ABSOLUTE_MAX_WATCHES`), `client/database.go:381-386`, `tryAcquireWatch` at `:388-401`,
  charged at `client/readpath.go:1236-1238`. Pinned by `client/watch_limit_test.go:10-14`, whose
  header names this exact gap as what it closes. The user-facing statement inverts: the cap is a
  real, shared, per-process budget tenants must plan against.
- Still open: no RYW pending-write immediate-fire; special-key space absent; `BYPASS_UNREADABLE`
  span-wipe is the one known committed-byte divergence (deliberate, reviewed).

`TODO.md`'s D1 (`:7591`) and D2 (`:7602`) still carry the two fixed items as open under an unchecked
parent; D3 (`:7607`, RYW pending-write immediate-fire) is genuinely open.

## Landed since the audit — the two safety nets this page now rests on

Both merged while this revision was being written, and both are re-verified at `a1d281a63`.

**The generation factory (#555, `491e02a7c`).** `pkg/relational/conformance/factorycorpus` exists
with **5000 committed scenarios / 20000 tests / 4952 feature vectors** (`census_baseline.json`),
generated → executed → blessed → deduped → committed. The census ratchet gates scenario/test
totals and every feature vector; authority is compared per scenario by dedup key so promotion is
allowed and downgrade is rejected. Aggregate `ByBlessing` counts are report-only, because a floor
per label would reject promotion. First batch: 1200 seeds → 2268 candidates → 965 blessed → 900
committed; 1599 TLP partitions and 3226 second-plan pairs, zero disagreements, every oracle
mutation-proven armed. The current blessings are **1763 `metamorphic` + 216
`metamorphic-tlp-only`**, labeled in every header — the Java leg is environmentally unreachable
here, so the corpus does not claim a
cross-engine authority it does not have. **This is the tier's ceiling and should be read as one:**
metamorphic blessing proves a query agrees with its own transformations, not that it agrees with
Java. The headers are promotable without regeneration, so the corpus becomes a cross-engine net the
day the Java leg is reachable — until then it catches self-inconsistency, which is how it caught the
resolver defect, and not cross-engine divergence. The full corpus is ordinary per-PR suite content:
`just test` and CI execute every committed scenario; the tagged target exists to isolate its FDB
container, not to reduce it to a sample. TLP's inherent blindness to branch misassignment is pinned
as a negative result that re-arms if the checker gains the ability.

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
- Binding-stress 0/50 (CQ-47) is root-caused and fixed (2026-08-05): the pinned Python client tar was
  archived as symlinks into the builder's Bazel output_base, so it only unpacked on the machine that
  built it; everywhere else `import fdb` bound to an empty implicit namespace package. 100/100 api and
  30/30 directory seeds pass after the fix. The 07-17 stress failure is root-caused (Tier 1).
- Repo SETTING (owner action, not code): Actions cannot create PRs, so `frl-pin-bump` has failed
  27/27 — flip "Workflow permissions → allow PR creation".

Booked by THIS revision, from defects the verification pass found:

- **CQ-80 — DONE.** All four watch-list entries that did not meet the contract now do. Entry 2's
  un-red-able test was replaced by the Java-parity pins (the divergence itself REFUTED), entry 4's
  drift was fixed and pinned red→green, entry 8 gained the cross-engine pin its Java half lacked,
  and entry 12 was REFUTED and rewritten — the real divergence is Go over-resolving folded
  spellings Java rejects, with wire compat measured intact. This was a code/test lap, deliberately
  not done in the docs-only pass that found it; each entry now cites its pin so the next fixer does
  not re-derive which and why.
- **CQ-79** — CQ-53's surviving producer mint (`cascades_translator.go:3598`) is owned by no open
  item: CQ-53 is marked `[x]` as carrying no remainder while the ratchet pins the mint as "CQ-53's
  surviving producer". Checked and NOT owned by CQ-68, which is a different axis (untyped QOV, not a
  manufactured row key). Re-verified at `a1d281a63`: the pin stands verbatim.
- The boolean-CASE folklore comments are **GONE** (#555). `walk.go` dissolved into the unified walk;
  the surviving comment states the corrected fact and cites the probe. Recorded because the shape of
  the error is worth keeping: two code comments and four test pins agreed with each other and all
  five were wrong, none having asked the Java server.

## Multi-tenant SaaS — what landed, and what is still a build

Measured at `2e4b9f930`. The operator-facing page is **[`docs/mt-saas.md`](docs/mt-saas.md)**; it
carries a `file:line` citation per claim and is the place to look for *how*. This section records
*what changed* and *what is left*, and does not repeat it.

The deployment shape assessed is: one SQL database path per tenant, one subspace per
(database, schema), the SQL connection as the trust boundary. Native FDB tenants are **not** used,
for three independent reasons — the SQL layer has no plumbing to open one
(`sqldriver/driver.go:211-215`), the pure-Go tenant API is unauthenticated
(`fdbgo/fdb/options.go:389-392` refuses `authorization_token` outright), and the cgo backend carries
no tenant ops at all (`fdbgo/fdb/backend.go:35-36`), so adopting them forfeits the `libfdb_c` escape
hatch. Stated at its sharpest: **the pure-Go client has tenants but no authorization; `libfdb_c` has
authorization but no tenants.** No single build has both halves of FDB's own tenant-isolation model.

### Landed this arc

**#624 — catalog scoping, cross-database DDL, and the DSN freeze (`aa73be70e`).** Two real
cross-tenant leaks. (a) All four `INFORMATION_SCHEMA` views and `SHOW DATABASES` called the
cluster-wide catalog reads, so any connection could enumerate every database's schemas, tables,
columns and index names — and the user's WHERE clause was no defence, because filtering runs after
the rows are materialised. All five now scope to the session database and **fail closed** on an
unscopable path (`core/embedded/system_tables.go:124-131`, `:411-431`), at path-SEGMENT granularity
(`:470-475`). `SHOW SCHEMA TEMPLATES` stays global by necessity — scoping it would be a catalog
wire-format change to a record Java also reads (`:520-533`, pinned). (b) A fully-qualified
`/otherdb/SCHEMA` was accepted from any connection, matching Java, which assumes authorization above
SQL. The Go-only `RESTRICT_DDL_TO_SESSION_DATABASE` confines DDL to the session database (42501) at
the four resolution chokepoints, on the ALREADY RESOLVED path — and, after review, also refuses
schema-TEMPLATE DDL outright, since a template has no owning database and dropping one another
tenant's schemas resolve against leaves those schemas unloadable. `CREATE DATABASE /__SYS/...`,
which previously SUCCEEDED, is now refused unconditionally. The Connector additionally deep-clones
its DSN, closing a split brain in which the option decode was frozen while `Path` and `cluster_file`
were still read live from a caller-mutable struct.

**#625 — CQ-90: the rebuild path, proven, plus `SetFormatVersion` (`36cb843e9`).** The migration
half is recorded in entry 18 and its appendix. The part that outlives it is engine behaviour on the
path **every future index addition under a live tenant takes**: Go was calling
`MarkIndexReadableOrUniquePending` unconditionally on both the inline and online paths, silently
downgrading a uniqueness violation into a pending state nobody opted into. Java's semantics are now
ported 1:1 — including the adjudication that reversed a prior lap's blocking finding, because
`rebuildIndex`'s `handle(...)` returning normally means the violation never leaves `checkVersion`;
propagating it would turn any metadata evolution meeting bad data into a store-open OUTAGE Java does
not have. `StoreBuilder.SetFormatVersion` / `OnlineIndexerBuilder.SetFormatVersion` are ported so a
rolling upgrade can pin every instance to the OLD format; before this, Go could not express opening
at anything but its binary's newest.

**#626 — three latent multi-tenancy hazards (`56c0d4865`).** (a) The plan-cache scope carried
`(schema, metadataVersion, plannerOptions)`, so two databases holding a same-named schema — the
ordinary multi-tenant shape — shared one entry for identical SQL and whichever planned first had its
plan served to the other. The database path is now a fourth length-prefixed component. (b)
`RecordTypeKeyExpression` carried a mutable binding on a SHARED object, so two metadata built from
one expression graph decided each other's record type key. The chase ended by deleting the state
entirely and resolving from the record, as Java's stateless singleton does — which then surfaced
four more defects at the door (unsigned narrow widths panicking the encoder, a `uint64` above max
int64 that builds and saves but cannot be exported, an empty bytes key vanishing on export, and five
different comparisons of "same key" that had drifted apart). (c) The process-global
derived-subspace-keys cache had no TTL and no cap — one entry per tenant store, released never — and
its first bound only swept on cache HITS, i.e. never on the ever-new-subspace workload it exists for.

**#620 — RFC-210 plus three master-reachable executor bugs (`2e4b9f930`).** The RFC itself is a
read-side extension (Java does not do secondary-UNIQUE DISTINCT elision at all) and its
index-state-evidence tri-state matters here for a different reason: `ReadableIndexes`' zero value
meant "nobody asked" while rendering identically to Java's affirmative "I checked, all readable", and
that is now fail-closed. The three **pre-existing #621 scratch-lifecycle bugs** are the
multi-tenancy-relevant part, all reachable on master and all budget-affecting under load: a
forwarded raw continuation token was not treated as a name, so the sweep retired a token a live
continuation still held and the next page could not resume; `BeginPage` left a FAILED attempt's
charge live, so repeated conflicts stacked one attempt-sized delta each until the statement could
exhaust its budget before any attempt committed; and a cursor released what it last *recorded*
rather than what it *held*, leaking charge for the statement's life.

### Still a build — booked, not done

- **Per-tenant restore.** Cluster-granular `fdbbackup`/`fdbrestore` only; there is no layer-level
  backup and none in Java either. Per-tenant restore is *not built* — and, to correct a formulation
  that has circulated, not *unbuildable*: a tenant is one contiguous subspace and the range
  primitives are exported. What makes it a project rather than a script is that each scan is one
  transaction (no cross-transaction snapshot), record versions cannot be written back
  (`SaveRecordWithOptions` takes no version), and the CLI is lossy as a capture format
  (`frl store dump` prints value *lengths*; `frl record scan` emits three fields, no version, no
  index state, no header). If per-tenant PITR is a product promise, budget it as a project.
- **Index-build fan-out orchestrator.** `frl index build` is one index on one store addressed by
  scalar flags; N tenants is N invocations. `SetMutualIndexing`/`SetTargetIndexes`/`SetSourceIndex`
  exist on the builder and are not exposed by the CLI at all.
- **Schema-evolution fan-out.** `RepairSchema` exists (`api/catalog.go:76`,
  `core/catalog/fdb_store_catalog.go:472`) and has **zero non-test call sites**; no SQL statement
  reaches it, and no loop iterates the catalog calling it. There is no `ALTER` (a 42601 syntax
  error, not an unsupported-feature message) and schema templates are immutable in practice (42F59
  on a second `CREATE`), so the fleet-migration story is two loops nobody has written.
- **Post-execution query statistics.** `PlanGenerationLogger` reports planning only — SQL, plan
  hash, EXPLAIN text, planning duration, cache event, slow flag. No rows scanned, no bytes read, no
  execution time, anywhere. Per-tenant cost attribution cannot be built from the engine today. There
  is no `EXPLAIN ANALYZE` (the token appears in no parser rule).
- **StoreTimer exporter.** `StoreTimer` mirrors Java's `FDBStoreTimer` event set and nothing exports
  it: no production `SetTimer` call site, no `fdbmetrics` equivalent, and the SQL path never surfaces
  an `FDBRecordContext` to install one on. `fdbmetrics` covers client-level counters only.
- **Transaction-tag wire marshalling.** The pure-Go client stores tags but never puts them in
  `CommitTransactionRequest.TagSet` or the GRV request, so FDB server-side per-tag throttling cannot
  engage; `libfdb_c` forwards both. Per-tenant throttling is the application's job on the default
  build.
- **OTel above the FDB layer.** Tracing exists at the client (`client.WithTracer`, RFC-115 §4 Layer
  2, `fdbgo/client/options.go:97-114`) and nowhere above it — no spans for planning or execution.
- **Online index scrubber.** Java has `OnlineIndexScrubber` (`scrubDangling`/`scrubMissing`) —
  chunked, throttled, resumable, repairing; Go has none and says so at
  `recordlayer/index_state.go:567`. What exists is **detection only** and unusable at size:
  `FDBRecordStore.ValidateIndex` (`recordlayer/index_validation.go:39`) repairs nothing, scans every
  record and every index entry into memory with no continuation and no scan limit
  (`scanAllRecords`, `:138`), and has zero non-test call sites. So for a store of any real size
  there is no usable way to detect, and no way at all to repair, dangling or missing index entries.
- **Default format version.** Unpinned, a new Go store is born at 14 while Java's default is 7
  (booked in `DIVERGENCES.md` by #625). `SetFormatVersion` now makes alignment expressible; the
  remaining work is a decision about the default, not a missing capability.

## Sequence

1. **~~Confirm B1.~~ DONE 2026-08-03** — two consecutive genuinely green reconciles (see B1);
   Tier 1 is confirmed. Residuals CQ-46/CQ-47 stay booked below, not tier-gating.
2. **~~B2 explicit-tx isolation.~~ DONE 2026-08-04** — #607 merged (`d6f635073`); Tier 2 is
   confirmed. Booked follow-up: RFC-209 implementation was held behind #607 and is now unblocked.
3. **~~CQ-80~~ DONE** — every watch-list entry now carries the committed test the section contract
   claims for it (entries 2, 4, 8 and 12 closed; 2 and 12 REFUTED by the measurement). The list is
   handable to an adopter.
4. **RFC-197 tail — RE-SEQUENCED, because verification refuted the old order.** It read "CQ-52's
   remaining producers, then CQ-51 and CQ-79 (the CQ-53 mint), then CQ-68". CQ-52 has no remaining
   migratable producers, and CQ-79 is not a local mint rewrite — it is one axis of CQ-68's executor
   widening. The real order is: **(a) an owner ruling on CQ-52's star-body leg-addressability
   question** (one decision, blocks nothing else); **(b) CQ-51's RFC**, which is now a bounded port
   of `getConstraintDependencies` + `ReExploreExpression` onto Go's already-present
   `IsExploredForAttributes`, and which unblocks the value-keyed `ReferencedFields` conversion;
   **(c) CQ-68 and CQ-79 TOGETHER**, as the two axes of the `bindMergedOuterLegs` widening. All
   review-gated; see the Tier-3 table above for the per-item measurement.
5. **CQ-46**, index candidacy inverted to opt-in per maintainer factory, with the adjacent opt-out
   leaks measured for reachability. Query-engine gated.
6. **CQ-75 — DONE via RFC-208.** `v IN (-0.0, 0.0)` now returns both signs in either element order;
   the exact plan/result regression is in `composite_index_zero_widen_test.go`. CQ-76's general
   nonzero `IN` + trailing-equality SARGability gap remains open.
7. **CQ-30**, B3's residual: criterion 2's data-access maxima are still forked.
8. **B5** typed metadata flow. L, after the migration proves the identity machinery everywhere.
9. Watch-list handed to any prod adopter, once CQ-80 has pinned it.
