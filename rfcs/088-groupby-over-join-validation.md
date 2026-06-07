# RFC-088 — `GROUP BY` over a join with a joined-table key falsely 42703s

**Status:** Draft — awaiting Graefe + Torvalds ACK.
**Found:** while triaging the un-skip-yamsql epic (the `join_optimization_probes`
`D.DNAME` failure). A real correctness bug, independent of that scenario.

## Problem

`SELECT d.dname, COUNT(*), MAX(e.salary) FROM emp e JOIN dept d ON e.did = d.did
GROUP BY d.dname` fails at PLAN time with `42703: column "D.DNAME" does not exist`.
Reproduces for **both** comma-join and `INNER JOIN`, and for **both** qualified
(`d.dname`) and bare (`dname`) group keys. The plain (non-grouped) join projection
`SELECT d.dname FROM emp e JOIN dept d …` works — only the **grouped** shape fails.

## Investigation

The throw is `validateGroupByProjection` (`logical_predicate.go:2168`), called from
`plan_visitor.go:561` and the embedded GROUP BY builder. It builds a `tableFields`
existence set from **only the first table** — `md.GetRecordType(sq.tableName)` — and
its `checkColumn` 42703s any projection / group-key column whose bare name isn't in
that set (`:2193-2196`). Over a join, `sq.tableName` is the first table (`emp`); the
joined table (`dept`) is never added, so `d.dname` → bare `DNAME` ∉ emp's fields →
42703. (The semantic resolver itself — `buildSelectScope` — *does* add all join
sources, which is why the non-grouped projection resolves; the bug is local to this
GROUP-BY existence check.)

## Fix

Build `tableFields` from **every base-table source** — the primary table AND each
`sq.joins` entry — so a joined-table group key passes the existence check. Guard:
if **any** source is a derived table / CTE (no record type, columns unknowable), set
`tableFields = nil` and skip the existence check entirely — conservative, matching
the pre-join behaviour for an unresolvable primary source (the 42803 "ungrouped" and
resolver checks still run; a true typo fails downstream at resolution).

~25 lines in `validateGroupByProjection`; no executor change (the join→aggregate
execution already keys merged rows by both bare and qualified names via `mergeRows` +
`aggregateMapRows`, so once validation passes the query runs and groups correctly).

## Performance / wire

Plan-time only; one extra small field-set build per join source. No wire impact.

## Test plan

`groupby_over_join_fdb_test.go`:
- `GROUP BY` over INNER JOIN + comma-join, qualified + bare key, with COUNT/MAX →
  correct grouped rows (eng→2/100, sales→1/80) — the was-42703 shapes.
- SUM over join (eng→190, sales→80), HAVING on the grouped join output
  (`COUNT(*) > 1` → only eng), and a multi-key GROUP BY mixing a joined-table key +
  a first-table key (2 groups).
- First-table group key over a join still works (no regression from the wider set).
- A genuinely-undefined GROUP BY column over a join **still errors** (the existence
  check isn't silently disabled for joins).
- Broader GroupBy/Aggregate/Join/QualityProbe sweep green; `just test` green.

## Follow-up (Graefe ACK, non-blocking)

`validateGroupByProjection`'s `tableFields` is a **second, hand-rolled existence
oracle** parallel to the semantic resolver — which already does existence + ambiguity
+ join-scope correctly (`resolveColumnName`→`ResolveIdentifier`, plan_visitor step 4).
That duplication is exactly what produced this bug. End-state: route `checkColumn`'s
existence test through `resolver.ResolveIdentifier` and let `validateGroupByProjection`
enforce ONLY the 42803 grouping rule — collapsing the two oracles. This RFC makes the
duplicate *consistent* (a strict improvement); the convergence is a tracked follow-up.

## Out of scope

- The yamsql un-skip epic (this is one of its buckets; fixed standalone).
- Java parity note: Java's fdb-relational 4.11.1.0 can't plan these `emp`/`dept`
  joins at all (`UnableToPlanException`), so the grouped-join shape is a Go-only
  read-side extension — this fix makes that extension correct (wire-neutral).
