# Road to production

Status page for the question "what stands between this codebase and production
use", tiered by deployment mode. Grounded in the 2026-07-29 full audit
(prod-readiness doc verified against code, every open TODO classified by
impact, safety-net run history pulled from CI, pinned-divergence inventory
greped from tests). Where this contradicts `PRODUCTION_READINESS.md` or
`rfcs/prod-readiness-go-client.md`, THIS page is newer — both are stale in the
pessimistic direction and their reconciliation is itself a booked item below.

The one-line answer: the code is closer to prod than any older document
claims; the two things that were genuinely dangerous on audit day were not
query-engine bugs but (1) safety nets reporting green over zero executed work
and (2) explicit-transaction isolation. Everything else is known, pinned,
booked, or actively shrinking under a ratchet.

## Deployment tiers

### Tier 1 — record-layer data plane + auto-commit SQL, bounded contexts
**Distance: days.** Gated on exactly two items, both in flight on audit day:

- ~~The hidden nightly-stress failure~~ **ROOT-CAUSED, benign mechanism
  confirmed, structural fragility found.** The 07-17 failure reproduced
  deterministically at its SHA: the NLJ EXISTS shortcut matched candidates by
  first-column NAME with no index-type check, so a SUM aggregate index was
  built into a record-fetching scan; `getEntryPrimaryKey` returns an empty
  tuple for the short aggregate entry and the executor raises Java's
  orphan-behavior error — LOUD in every measured shape, no silent wrong rows
  (aggregate entries are always shorter than the root column size, so a
  garbage fetch is unreachable). NOT an index-consistency defect. It was
  fixed INCIDENTALLY on 2026-07-24 by a FAN_OUT-cardinality commit; of the
  three gates now holding it shut, only one names aggregate indexes —
  measured: one innocuous added method re-arms the fault on a different
  query shape. Pinning tests exist (uncommitted, land with the next window);
  the structural fix is Java's shape — index candidacy is opt-IN per
  maintainer factory there, so a SUM index cannot produce a value-index
  candidate; Go's opt-out design is the divergence, and adjacent opt-out
  leaks (text/multidimensional/leaderboard/legacy min_ever) are unmeasured.
  Booked as its own gated work item.
- The safety nets fix is MERGED (#523); tonight is the first genuine run under corrected windows — Tier 1 confirms when the reconciler goes green on real work.

What this tier already rests on: byte-identity differential vs `libfdb_c`
running PER-PR (78 differential tests, `pkg/fdbgo/bench/`), the wire-type
oracle, chaos with model verification (227 tests, per-PR), Java conformance
(1,361 specs against a real 4.12.11.0 server, per-PR), 87.5% measured SQL
coverage (2,395/2,736 corpus cases across 341 scenarios) with drift-guarded
generated docs, both
former P0s of the prod-readiness RFC (cluster-file rotation, SetTimeout
bounding in-flight reads) verified CLOSED in code.

### Tier 2 — full SQL surface incl. explicit transactions
**Distance: weeks at the current grind pace.** Dominated by:

- **B2, the single most likely defect to bite a real application**: inside
  `BeginTx`, DML joins the FDB transaction but SELECT runs in a fresh
  auto-commit transaction — no read-your-writes, no read-conflict ranges, so
  read-modify-write across two transactions is last-writer-wins with no
  1020/40001. Silent lost updates. Pinned by
  `pkg/relational/sqldriver/tx_select_isolation_probe_test.go`. Size L,
  review-gated. No user-side mitigation exists; this is the Tier-2 gate.
- The wrong-rows residue of the RFC-197 identity migration (see table).

### Tier 3 — mixed Go/Java deployment on a shared cluster
**Works today WITH the watch-list.** Wire compat is exercised per-PR; the
remaining cross-engine differences are semantic, pinned, and enumerable (see
Watch-list). A prod user must be handed that list; several entries mean the
same query returns different rows or different errors on the two engines.

## Blocking items

| # | Item | Impact | Size | State |
|---|---|---|---|---|
| B1 | Nightly safety nets were fake-green (window gates anchored to cron hours GitHub dispatches 2-4h late; 12 fake-green stress nights; rowdiff window unreachable by construction; oracles never ran) | Unknown-risk factory | S | **FIXED, merged (#523)**: windows resized from measured dispatch landings, per-job heartbeats, reconciler fails when any net has not genuinely run in N days (red until tonight proves the nets — the truthful state). Stress 07-17 failure root-caused separately (see Tier 1); binding-stress 0/50 still open (CQ-47) |
| B2 | No read-your-writes in explicit transactions; SELECTs take no read locks → silent lost updates | Wrong data | L | Booked (TODO), pinned by probe test; needs executor scan routed through the active tx with the snapshot-vs-serializable read decision |
| B3 | RFC-195: six cost estimates contradict proven cardinality bounds; comparator uses a private cardinality walk | Wrong plans (perf), not wrong rows | M | RFC fully ACKed (rev 3), implementation queued behind the identity migration |
| B4 | RFC-197 identity migration residual (see per-bucket table) | Wrong rows (narrow, shrinking) | M | Active; ratchet-enforced; 68 at inception → 43 after the name-keyed bucket |
| B5 | WS-N Phase D: metadata re-derived by name instead of flowing from the type (~347 UnknownType mints repo-wide; three named guessers) | Wrong client VALUES on cross-leg same-name-different-type | L | Booked; gates the typed-row-representation work |
| B6 | Documentation authority contradictory/stale (prod-readiness RFC asserts closed gaps as open; PRODUCTION_READINESS.md authority claim outdated; TODO duplicates/stale entries) | Trust/decision risk, not code | S | Booked as one reconciliation item |

### B4 residual, wrong-rows reachability per bucket (audit-day counts)

| Bucket | Count | Wrong rows reachable in prod? |
|---|---:|---|
| boundary | 0 | migrated |
| escape | 0 | migrated (found + fixed a live wrong-type defect on the way) |
| name-keyed | 3 | Shrunk from 15 — the NLJ layout maps and distinct-key set are CONVERTED; the 3 stops are measured machinery gaps (planner-budget re-fire on constraint growth; resolver-upstream conversion; lazy carriers with no other identity), each recorded on its debt entry |
| dotted | 15 | Yes, narrowly — two writers key a correlation set by sliced qualifier with no ambiguity guard; alias shadowing across nested scopes can attribute a correlation to the wrong quantifier |
| translator | 13 | Bounded — resolution-time text handling; misbinding requires the 42702/42703 ambiguity checks to have a hole |
| contract | 11 | Not alone — single naming authorities; collision only becomes wrong rows via a name-keyed reader, so closing name-keyed defuses these |
| harness | 1 | No — oracle-side; affects trust in the net, not prod rows |

Enforcement: `pkg/docscheck`'s `TestFieldNameNeverDecides` — every tracked
non-generated non-test file scanned, strict ratchet (stale entries fail as
loudly as new violations), empty allowlist, per-site counts. The migration is
done when name-keyed and dotted hit zero; the gate then holds the count there.

## Watch-list — pinned divergences a prod user must be told about

Every entry is a committed test asserting CURRENT behavior; red means fixed.

Wrong rows / wrong data:
1. No read-your-writes in `BeginTx` (= B2).
2. INSERT of NULL into a PRIMARY KEY column silently stores 0
   (`embedded_fdb_errors_test.go`).
3. Correlated float `=` misses `-0.0`/`+0.0` on a non-terminal composite index
   column — silently missing rows, rare shape
   (`correlated_zero_composite_sentinel_test.go`, RFC-196).
4. `CURRENT_TIMESTAMP` drifts across rows within one statement (SELECT path).
5. NaN comparisons follow Java's total order, not IEEE — `(v/z)=(v/z)` returns
   ALL rows (`nan_comparison_semantics_test.go`). Matches Java; both diverge
   from the standard/PG/CRDB.
6. Signed zero: Go is IEEE, Java is bit-identity — `WHERE d = 0.0` returns a
   stored `-0.0` row in Go, not in Java. Cross-engine row difference on a
   shared cluster. (Java-is-wrong, pinned.)
7. BIGINT vs DOUBLE above 2^53 — Java promotes lossily and wrongly matches;
   Go rewrites exactly. (Java-is-wrong, pinned.)
8. `UNION ALL` + trailing `ORDER BY` — Java orders only the right leg; Go
   implements the standard. Different row order cross-engine.

Different answer / different error:
9. `SUM(int_col)` overflows silently from a join leg but raises 22003 outside
   one (`aggregate_operand_width_fdb_test.go`; red-means-gap-closed). Closing
   it flips answering queries into errors — needs its own gated lap.
10. `DELETE/UPDATE … RETURNING` via Exec silently drops the returned values;
    via Query → 0A000. Java supports it.
11. `DROP SCHEMA IF EXISTS` ignores IF EXISTS (deliberate Java-bug
    replication).
12. A quoted DDL column is created but unreferenceable by name.

Cleanly rejected (0AF00/0A000) where Java answers: derived tables with JOIN
bodies; EXISTS inside OR; correlated scalar subquery inside EXISTS; projected
EXISTS; scalar subquery over FROM-less SELECT; boolean-CASE in WHERE; grouped
correlated EXISTS. Several were once silent wrong rows and are now
flip-sentinels — the correct posture.

Unsupported on both engines: `COUNT(DISTINCT)`, `UNION`/`EXCEPT DISTINCT`,
`x IN (SELECT …)`, `NULLIF`, string functions, `DECIMAL`, FK/CHECK/defaults,
window functions.

Client-side operational: watches register at read version, not commit-gated;
no `too_many_watches` limit; no RYW pending-write immediate-fire; special-key
space absent; `BYPASS_UNREADABLE` span-wipe is the one known committed-byte
divergence (deliberate, reviewed).

## Newly booked from the audit (were unbooked; now TODO items)

- Two LIKE implementations that provably disagree (trailing escape), one live
  on the `INFORMATION_SCHEMA` WHERE path — part of a shadow evaluator family
  that violates "no parallel pipelines". S.
- `API_PARITY.md` contradicts `options.go` on two options (doc says no-op,
  code rejects) + a docscheck gate to keep the table honest. S.
- `SetSpecialKeySpaceRelaxed`/`EnableWrites` still silent no-ops — record the
  decision. S.
- `pkg/fdbgo` README/doc.go missing the bounded-context requirement. S.
- Two stated-unprobed differential axes (1021 idempotency — needs wire fault
  injection; cross-shard range-merge — needs multi-shard cluster). M.
- The 07-17 stress failure + binding-stress 0/50, with run IDs. Under
  investigation.
- Docs authority reconciliation (B6). M.
- Repo SETTING (owner action, not code): Actions cannot create PRs, so
  `frl-pin-bump` has failed 27/27 — flip "Workflow permissions → allow PR
  creation".

## Sequence

1. B1 (in flight): trustworthy nets + stress root-cause. Days.
2. Finish RFC-197 name-keyed + dotted (wrong-rows tail → 0). Days at current
   pace; translator/contract may follow at lower urgency.
3. B2 explicit-tx isolation. The Tier-2 gate. L, review-gated.
4. RFC-195 implementation (wrong plans). M, ACKed and ready.
5. B5 typed metadata flow. L, after the migration proves the identity
   machinery everywhere.
6. Docs reconciliation + watch-list handed to any prod adopter.
