# RFC-085 — `ORDER BY` over grouped output in a correlated scalar subquery

**Status:** Implemented — Torvalds ACK; Graefe ACK (re-review after adding the
aggregate/alias/NULLS/EXPLAIN coverage + parse-tree FN(BAREARG) recovery).
**TODO:** Known gaps line 63 (follow-up under RFC-047).
**Framing:** This is a **Go-only read-side extension**, NOT Java parity. Java rejects
correlated scalar subqueries at the grammar level entirely (zero wire impact); Go
supports them (RFC-047). So the bar is "wire-compat holds + deep tests + no silent
wrong results," not "match Java."

## Problem

In a correlated scalar subquery, `ORDER BY` combined with `GROUP BY` is **rejected**
(`buildCorrelatedScalar`, `logical_predicate.go:4059`):
```
correlated scalar subquery: ORDER BY combined with GROUP BY is not supported
```
The rejection is *correct* today (no silent wrong results) — but it blocks making a
**multi-group** scalar subquery deterministic. A correlated scalar subquery's value
is `FirstOrDefault` (first row after `LIMIT 1`); with `GROUP BY` producing >1 group
and no `ORDER BY`, *which* group's value wins is nondeterministic. `ORDER BY` over
the grouped output picks a deterministic group. `ORDER BY` *without* `GROUP BY`
(ordering rows before `LIMIT 1`) already works (`:4348-4360`).

## Investigation

`buildCorrelatedScalar` builds, for the real-aggregate path (`hasRealAgg`, `:4110`):
`Scan → Filter(correlation) → Aggregate(groupKeys, aggs)` (`innerOp = aggOp`,
`:4291`) → `LIMIT 1` (`:4297`). There is **no sort** between the aggregate and the
`LIMIT 1`, so the early guard rejects `ORDER BY` rather than silently drop it.

The non-GROUP-BY path already builds `LogicalSort(innerOp, keys)` from `sq.orderBy`
(`:4348-4360`) where each `SortKey.Expr = ob.colName` (resolved at runtime against
the row datum). The grouped case needs the same sort, but over the **post-aggregate**
row, whose datum is keyed by the canonical output names (group keys + canonical
aggregate names, e.g. `G`, `SUM(V)`) — so the sort key must resolve to one of those.

## Fix (revised per Graefe + Torvalds)

**Load-bearing fact (Torvalds, verified):** the executor's sort does an **exact-case**
datum lookup — `translateSort` → `FieldValue{Field: k.Expr}` and `FieldValue.Evaluate`
reads `row[f.Field]` with **no** ToUpper/EqualFold (`cascades_translator.go:847`,
`values.go:214`). A mismatched key returns nil → every row sorts equal →
nondeterministic. So the sort key MUST equal the **exact** datum key the aggregate
cursor emits. On this legacy `buildCorrelatedScalar` path that key is:
- a **group key**: `strings.ToUpper(parseColRef(groupCol).bare())` — the SAME
  derivation `scalarCol` uses (`:4317`);
- an **aggregate**: the materialised `aggAliases` entry (`:4181/4215`) — NOT a freshly
  recomputed `canonicalAggName` (on the join path the slot alias is `FN(BAREARG)`,
  which diverges from `canonicalAggName(explain)` for expression operands).

`upgradeSortKeyValues` (`:3033`) is the top-level canonicaliser, but it keys group
sorts by `ExplainValue(gkv)`, a DIFFERENT scheme than this path's bare-uppercase key —
reusing it would mismatch. So: derive the sort-key names from the **same source the
aggregate build here uses** (single-source, no parallel drift — Graefe's intent —
*and* exact — Torvalds').

Steps:
1. **Remove** the early `ORDER BY + GROUP BY` rejection (`:4053-4063`).
2. Build an ordered list of post-agg output `(matchName → datumKey, SortKey.Value)`
   pairs from the aggregate being built: each group key → `(bare-upper, FieldValue{bare-upper})`,
   each visible aggregate → `(its materialised alias + its source text/operand form, FieldValue{alias})`.
3. For each `sq.orderBy` ref, resolve it against that list and emit
   `SortKey{Value: FieldValue{datumKey}, Dir, NullsFirst}` — set `.Value`
   (translateSort prefers it), not raw `.Expr`. Resolution order: (a) a grouping
   column (bare-upper); (b) a selected aggregate by its written/alias form
   (`aggDatumKey[ToUpper(colName)]`); (c) a selected aggregate whose operand is
   spelled with a *different* qualifier than the SELECT — recover the producer's
   stable `FN(BAREARG)` key from the **parse tree** (`aggColRefFromExpr` →
   `extractAwfFields`, NOT text surgery on `SUM(o.amount)`, which `parseColRef`
   would split at the inner dot).
   - **Reject** (clean `ErrCodeGroupingError` / 42803) any ref that resolves to
     neither a group key nor a *selected* aggregate — including an **ORDER-BY-only
     aggregate** not in the SELECT (`ORDER BY COUNT(*)` while SELECT projects `SUM`)
     and an **expression key** (`ORDER BY SUM(x)+1`). No silent drop / no silent-nil
     sort. (Harvesting ORDER-BY-only aggregates into agg slots is a future extension;
     reject loudly for now.)
   - **Residual limitation (loud, not silent):** an *expression-argument* aggregate
     (`ORDER BY SUM(a*b)`) has no bare column name, so step (c) cannot form its key.
     It resolves only via step (b) when the SELECT spells it identically (whitespace
     and all); a differing spelling false-rejects with 42803. Acceptable for a Go
     extension (master rejected *all* ORDER BY+GROUP BY here); full support needs the
     consumer to run `canonicalAggName` over the resolved operand.
4. Insert `LogicalSort(innerOp, keys)` between `innerOp = aggOp` and the `LIMIT 1`
   (so FirstOrDefault picks the ordered-first group), in **BOTH** aggregate paths:
   the `hasRealAgg` path AND the group-key-only path (`:4338-4360`) — the latter's
   existing raw-`ob.colName` sort is the same nondeterminism bug and must use the
   canonicalised key too (Torvalds Hole 1).

### Sub-fix: qualified column resolution in the non-aggregate path (same bug class)

While testing the non-GROUP-BY guard, a **pre-existing silent-NULL bug** surfaced in
the **non-aggregate** correlated scalar subquery path — the SAME exact-case-datum-key
mismatch this RFC is centrally about, so it is fixed here:
- **Qualified projection** (`SELECT o.amount …`): `scalarCol = ToUpper(projCols[0])`
  kept the `o.` qualifier (`O.AMOUNT`); `replaceScalarSubqueryRef` re-qualifies under
  the inner alias at read time, double-prefixing to `O.O.AMOUNT` → resolves to **NULL**
  (no ORDER BY needed to trigger it — proving it is column resolution, not the sort).
- **Qualified ORDER BY key** (`… ORDER BY o.amount …`) in the non-GROUP-BY path: the
  raw single-table scan row is keyed bare (`AMOUNT`), so the qualified key missed and
  sorted every row equal → silent arbitrary row.

Fix mirrors the established join-vs-single-table convention (`:910`): for a single
inner table strip the qualifier to the bare key (`parseColRef(x).bare()`); for a join
the row is keyed qualified, so keep it. Applied to both the `projCols==1` scalarCol
derivation and the non-GROUP-BY sort keys.

## Performance

Plan-time only; one `LogicalSort` over the (already-materialised, ≤#groups) aggregate
output per scalar-subquery evaluation. The subquery already caps at `LIMIT 1`; the
sort is over a small grouped set. No wire impact (read-path-only extension). No change
to non-GROUP-BY scalar subqueries or to any non-correlated query.

## Test plan (deep — Go-only extension)

FDB integration (`*_fdb_test.go`):
- `… (SELECT SUM(amount) FROM orders o WHERE o.cust = c.id GROUP BY o.status ORDER BY
  o.status LIMIT 1)` — deterministic first-group value across 10× runs (was rejected).
- ORDER BY the **aggregate** (`ORDER BY SUM(amount) DESC`) → picks the max-sum group;
  ASC → min; pin both directions — AND a SELECT/ORDER-BY operand-qualifier mismatch
  (`SELECT SUM(amount) … ORDER BY SUM(o.amount)`) that exercises the step-(c)
  parse-tree `FN(BAREARG)` recovery (`*_ByAggregate`).
- ORDER BY a **group key** vs ORDER BY a **SELECT alias** of the aggregate
  (`*_ByAlias`).
- ORDER BY a column that is neither grouped nor aggregated → **clean reject** (not a
  silent wrong / not a runtime nil).
- NULLS FIRST/LAST honoured over the grouped output (a NULL-SUM group sorts first/last
  per the clause — `*_NullsOrdering`).
- Determinism 10× (the whole point); EXPLAIN pins a Sort over the StreamingAgg under
  the FlatMap (`*_ExplainSort`); non-GROUP-BY ORDER BY scalar subquery unchanged;
  `just test` green.
- **Qualified non-aggregate projection** (`SELECT o.amount …`) with and without
  ORDER BY → correct value, not NULL (regression for the sub-fix); qualified ORDER BY
  key in the non-GROUP-BY path → correct sorted row.

## Out of scope

- Top-level (non-correlated) `GROUP BY … ORDER BY` already works — untouched.
- `ORDER BY` over a grouped output in a *derived table* / UNION branch — separate
  builders (RFC-079 territory).
