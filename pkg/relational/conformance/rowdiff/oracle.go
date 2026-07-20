package rowdiff

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
)

// OracleRows evaluates a query naively over the case's authoritative rows:
// full scan → predicate evaluation → ORDER BY → projection. No planner, no
// indexes. Leaf comparisons reuse the ENGINE's evaluation
// (predicates.NewLiteralComparison(...).Eval) so scalar/NULL semantics are
// shared by construction and the planner is the only component under test
// (RFC-182 §3). AND/OR combine with Kleene three-valued logic — the same
// algebra the engine's conjunct evaluation applies; a row qualifies only
// when the WHERE evaluates to TRUE (UNKNOWN drops the row, SQL semantics).
//
// The returned rows are projected copies in oracle order: sorted per
// OrderBy when present (P1 sort keys are NOT NULL and suffixed with ID, so
// the expected sequence is total), original insertion order otherwise (the
// caller compares as a multiset in that case).
func OracleRows(c *Case, q Query, projection []string) ([]Row, error) {
	if q.Agg != nil {
		return oracleAggRows(c, q)
	}
	if q.Join != nil {
		return oracleJoinRows(c, q, projection)
	}
	var out []Row
	for _, r := range c.Rows {
		if q.Where != nil {
			tb, err := evalBool(q.Where, r)
			if err != nil {
				return nil, err
			}
			if tb != predicates.TriTrue {
				continue
			}
		}
		if q.Exists != nil {
			has, err := existsHolds(c, q.Exists, r)
			if err != nil {
				return nil, err
			}
			// EXISTS keeps the outer row when a match exists; NOT EXISTS keeps
			// it when none does. has==Negated is exactly the drop condition.
			if has == q.Exists.Negated {
				continue
			}
		}
		out = append(out, r)
	}

	if len(q.OrderBy) > 0 {
		sort.SliceStable(out, func(i, j int) bool {
			for _, k := range q.OrderBy {
				cmp := compareSortKey(out[i][k.Col], out[j][k.Col], k)
				if cmp == 0 {
					continue
				}
				return cmp < 0
			}
			return false
		})
	}

	cols := projection
	if cols == nil {
		cols = append([]string{"ID"}, colNames(c.Table)...)
	}
	projected := make([]Row, 0, len(out))
	for _, r := range out {
		p := make(Row, len(cols))
		for _, col := range cols {
			p[col] = r[col]
		}
		projected = append(projected, p)
	}

	// SELECT DISTINCT: dedup the PROJECTED rows (SQL semantics — distinct
	// over the output columns). Insertion order kept; the comparator treats
	// DISTINCT results as unordered multisets (generator drops ORDER BY).
	if q.Distinct {
		seen := make(map[string]struct{}, len(projected))
		dedup := projected[:0]
		for _, r := range projected {
			key := distinctKey(r, cols)
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
			dedup = append(dedup, r)
		}
		projected = dedup
	}

	// NOTE: the oracle deliberately does NOT apply q.Limit — it returns the
	// full result multiset, and diffRows applies the §3 membership rule
	// (any min(k,|M|)-subset unordered; exact prefix ordered, valid because
	// generated sort keys are ID-suffixed total orders).
	return projected, nil
}

// oracleJoinRows evaluates a self-join as the textbook nested loop over the
// case's authoritative rows: every (l, r) pair whose join key matches and
// whose WHERE evaluates TRUE. No engine semantics are reimplemented — the
// join equality and every filter leaf go through the same
// predicates.Comparison evaluation the single-table path uses; only the
// PLANNER (join order, access selection, correlated probes) is removed.
//
// Rows are keyed "<QUAL>.<COL>" while combined, then projected to the
// unique output aliases the renderer emits (l_id, r_a, …).
func oracleJoinRows(c *Case, q Query, projection []string) ([]Row, error) {
	if q.Distinct {
		// The single-table path dedups projected rows; this one does not.
		// genJoinQuery never sets Distinct, so rather than carry an untested
		// second implementation, fail loudly if that ever changes.
		return nil, fmt.Errorf("rowdiff: DISTINCT is not implemented for join queries — " +
			"add dedup to oracleJoinRows (mirroring the single-table path) before generating it")
	}
	joinCmp := predicates.NewLiteralComparison(predicates.ComparisonEquals, nil)

	var combined []Row
	for _, l := range c.Rows {
		for _, r := range c.Rows {
			tb, err := joinCmp.EvalAgainst(l[q.Join.LeftCol], r[q.Join.RightCol])
			if err != nil {
				return nil, err
			}
			if tb != predicates.TriTrue {
				continue
			}
			row := make(Row, 2*(len(c.Table.Cols)+1))
			for k, v := range l {
				row["L."+k] = v
			}
			for k, v := range r {
				row["R."+k] = v
			}
			if q.Where != nil {
				wb, err := evalBool(q.Where, row)
				if err != nil {
					return nil, err
				}
				if wb != predicates.TriTrue {
					continue
				}
			}
			combined = append(combined, row)
		}
	}

	if len(q.OrderBy) > 0 {
		sort.SliceStable(combined, func(i, j int) bool {
			for _, k := range q.OrderBy {
				key := predKey(k.Qual, k.Col)
				cmp := compareSortKey(combined[i][key], combined[j][key], k)
				if cmp == 0 {
					continue
				}
				return cmp < 0
			}
			return false
		})
	}

	out := make([]Row, 0, len(combined))
	for _, row := range combined {
		p := make(Row, len(projection))
		for _, qualified := range projection {
			p[strings.ToUpper(joinOutputAlias(qualified))] = row[qualified]
		}
		out = append(out, p)
	}
	return out, nil
}

// existsHolds evaluates a correlated EXISTS for outer row o: does ANY row
// satisfy `r.CorrCol = o.CorrCol [AND Inner]`? The correlation equality and the
// inner comparison go through the engine's own Comparison (EvalAgainst /
// evalLeaf), so NULL/scalar semantics are shared — a NULL on either side of the
// correlation is UNKNOWN, so a NULL correlation value matches no inner row.
func existsHolds(c *Case, e *ExistsSpec, o Row) (bool, error) {
	corr := predicates.NewLiteralComparison(e.CorrOp, nil)
	for _, ir := range c.Rows {
		// EvalAgainst(inner, outer) evaluates `inner <CorrOp> outer` == the
		// rendered `r.CorrCol <CorrOp> t.CorrCol`; a NULL on either side is
		// UNKNOWN for every op, so a NULL correlation value never matches.
		tb, err := corr.EvalAgainst(ir[e.CorrCol], o[e.CorrCol])
		if err != nil {
			return false, err
		}
		if tb != predicates.TriTrue {
			continue
		}
		if e.Inner != nil {
			itb, err := evalLeaf(e.Inner, ir)
			if err != nil {
				return false, err
			}
			if itb != predicates.TriTrue {
				continue
			}
		}
		return true, nil
	}
	return false, nil
}

// oracleAggRows evaluates an aggregate query: filter (through the engine's
// own comparisons), partition by the grouping key, fold, then HAVING.
//
// This is the ONE oracle path that restates SQL semantics instead of
// sharing the engine's — aggregation has no per-row Comparison equivalent
// (see AggSpec's honesty note). The restated rules, all of them the
// well-defined ones:
//   - COUNT(*) counts ROWS, NULLs included; COUNT(col) counts NON-NULL values.
//   - SUM/MIN/MAX ignore NULLs, and yield NULL when a group has no non-NULL
//     value at all.
//   - A NULL grouping key forms its OWN group (SQL groups NULLs together).
//   - A scalar aggregate over an empty input still returns ONE row:
//     COUNT → 0, SUM/MIN/MAX → NULL. A GROUPED aggregate over an empty
//     input returns NO rows.
//   - HAVING compares the aggregate result; a NULL aggregate never passes.
//
// Results are unordered (no ORDER BY is generated for aggregates), so the
// comparator treats them as a multiset.
func oracleAggRows(c *Case, q Query) ([]Row, error) {
	a := q.Agg

	var kept []Row
	for _, r := range c.Rows {
		if q.Where != nil {
			tb, err := evalBool(q.Where, r)
			if err != nil {
				return nil, err
			}
			if tb != predicates.TriTrue {
				continue
			}
		}
		kept = append(kept, r)
	}

	// Scalar aggregate: one group over everything, emitted even when empty.
	if len(a.GroupBy) == 0 {
		return []Row{{"AGG": foldAgg(a, kept)}}, nil
	}

	// Grouped: partition on the COMPOSITE key (all GROUP BY columns), a NULL in
	// ANY key column forming a distinct group. Insertion order is kept for
	// determinism; the comparator is multiset-based.
	type group struct {
		keys []any // one value per a.GroupBy column, in order
		rows []Row
	}
	var groups []*group
	index := map[string]*group{}
	for _, r := range kept {
		keys := make([]any, len(a.GroupBy))
		var gk strings.Builder
		for i, col := range a.GroupBy {
			k := r[col]
			keys[i] = k
			// Typed and \x1f-delimited per component: the type tag keeps 1 and
			// "1" distinct, the delimiter keeps ("a","")≠("","a") composites
			// distinct — no cross-component aliasing.
			fmt.Fprintf(&gk, "%v(%T)\x1f", k, k)
		}
		g, ok := index[gk.String()]
		if !ok {
			g = &group{keys: keys}
			index[gk.String()] = g
			groups = append(groups, g)
		}
		g.rows = append(g.rows, r)
	}

	// emit builds a result row: one column per grouping key plus AGG.
	emit := func(g *group, agg any) Row {
		row := Row{"AGG": agg}
		for i, k := range g.keys {
			row[groupKeyCol(i)] = k
		}
		return row
	}

	out := make([]Row, 0, len(groups))
	for _, g := range groups {
		agg := foldAgg(a, g.rows)
		// An overflowing SUM makes the whole QUERY raise 22003, so the
		// sentinel must survive HAVING. The engine hits the overflow while
		// FOLDING — before any HAVING filter can discard the group — whereas
		// HAVING here would compare against the sentinel, fail to get
		// TriTrue, and `continue`, dropping the only evidence that an error
		// is expected. AggResultOverflows would then report false and the
		// runner would score the engine's CORRECT 22003 as a row mismatch.
		//
		// That is how `SELECT a AS g, SUM(a) AS agg FROM t_rd GROUP BY a
		// HAVING SUM(a) > 2` came out red on 27 of 3000 seeds: four rows
		// carry the generator's 2^62 boundary sentinel in `a`, so their group
		// sums to 2^64.
		if _, overflows := agg.(aggOverflow); overflows {
			out = append(out, emit(g, agg))
			continue
		}
		if a.HavingOn && a.Having != nil {
			tb, err := predicates.NewLiteralComparison(a.Having.Op, a.Having.Lit).Eval(agg)
			if err != nil {
				return nil, err
			}
			if tb != predicates.TriTrue {
				continue
			}
		}
		out = append(out, emit(g, agg))
	}
	return out, nil
}

// errAggOverflow is foldAgg's sentinel for "SUM overflows int64 here", i.e.
// the engine is REQUIRED to raise 22003 rather than return a row. It is a
// distinct value (not an error) because it flows through the row slot.
type aggOverflow struct{}

var errAggOverflow = aggOverflow{}

// AggResultOverflows reports whether the oracle's expected result for this
// query is an int64 SUM overflow — in which case the engine's 22003 is the
// CORRECT outcome and any row it returned instead would be the finding.
func AggResultOverflows(rows []Row) bool {
	for _, r := range rows {
		if _, ok := r["AGG"].(aggOverflow); ok {
			return true
		}
	}
	return false
}

// foldAgg folds one group's rows into the aggregate value. Returns nil for
// a SQL NULL result, or errAggOverflow when an int64 SUM overflows.
func foldAgg(a *AggSpec, rows []Row) any {
	switch a.Func {
	case AggCountStar:
		return int64(len(rows))
	case AggCountCol:
		var n int64
		for _, r := range rows {
			if r[a.Col] != nil {
				n++
			}
		}
		return n
	}
	// SUM / MIN / MAX: NULL-skipping, NULL when no non-NULL value exists.
	var acc int64
	seen := false
	for _, r := range rows {
		v, ok := r[a.Col].(int64)
		if !ok {
			continue // NULL (or a non-BIGINT the generator never produces)
		}
		if !seen {
			acc, seen = v, true
			continue
		}
		switch a.Func {
		case AggSum:
			// int64 overflow is a REAL engine outcome (22003), not a
			// divergence: the generator seeds boundary values deliberately.
			// Detect it here so the runner can expect the error instead of
			// reporting a bogus row difference.
			if (v > 0 && acc > math.MaxInt64-v) || (v < 0 && acc < math.MinInt64-v) {
				return errAggOverflow
			}
			acc += v
		case AggMin:
			if v < acc {
				acc = v
			}
		case AggMax:
			if v > acc {
				acc = v
			}
		}
	}
	if !seen {
		return nil
	}
	return acc
}

// distinctKey renders a projected row to a typed dedup key (same typed
// rendering the comparator uses, so dedup and diff agree on identity).
func distinctKey(r Row, cols []string) string {
	parts := make([]string, 0, len(cols))
	for _, c := range cols {
		parts = append(parts, fmt.Sprintf("%s=%v(%T)", c, r[c], r[c]))
	}
	return strings.Join(parts, "|")
}

// compareSortKey orders two values under one ORDER BY key, including NULL
// placement: the engine's default is the Java/FDB tuple rule — NULLS FIRST
// ascending, NULLS LAST descending — and explicit NULLS FIRST/LAST override
// it. Returns <0 when a sorts before b.
func compareSortKey(a, b any, k OrderKey) int {
	aNil, bNil := a == nil, b == nil
	if aNil || bNil {
		if aNil && bNil {
			return 0
		}
		nullsFirst := !k.Desc // default: ascending → NULLS FIRST, descending → NULLS LAST
		switch k.Nulls {
		case NullsFirst:
			nullsFirst = true
		case NullsLast:
			nullsFirst = false
		}
		if aNil {
			if nullsFirst {
				return -1
			}
			return 1
		}
		if nullsFirst {
			return 1
		}
		return -1
	}
	cmp := compareNonNull(a, b)
	if k.Desc {
		return -cmp
	}
	return cmp
}

func evalBool(n *BoolNode, r Row) (predicates.TriBool, error) {
	tb, err := evalBoolInner(n, r)
	if err != nil || !n.Not {
		return tb, err
	}
	// Kleene NOT: NOT UNKNOWN stays UNKNOWN.
	switch tb {
	case predicates.TriTrue:
		return predicates.TriFalse, nil
	case predicates.TriFalse:
		return predicates.TriTrue, nil
	}
	return predicates.TriUnknown, nil
}

func evalBoolInner(n *BoolNode, r Row) (predicates.TriBool, error) {
	if n.Leaf != nil {
		p := n.Leaf
		tb, err := evalLeaf(p, r)
		if err != nil || !p.Negated {
			return tb, err
		}
		switch tb {
		case predicates.TriTrue:
			return predicates.TriFalse, nil
		case predicates.TriFalse:
			return predicates.TriTrue, nil
		}
		return predicates.TriUnknown, nil
	}
	return evalBoolTree(n, r)
}

func evalLeaf(p *Pred, r Row) (predicates.TriBool, error) {
	switch {
	case p.RhsCol != "":
		// Column-vs-column: the engine's own two-sided evaluation.
		return predicates.NewLiteralComparison(p.Op, nil).EvalAgainst(r[predKey(p.Qual, p.Col)], r[predKey(p.RhsQual, p.RhsCol)])
	case p.IsBetween:
		// SQL desugaring: v >= lo AND v <= hi, Kleene AND over the
		// engine's own comparisons.
		lo, err := predicates.NewLiteralComparison(predicates.ComparisonGreaterThanEq, p.Lit).Eval(r[predKey(p.Qual, p.Col)])
		if err != nil {
			return predicates.TriUnknown, err
		}
		if lo == predicates.TriFalse {
			return predicates.TriFalse, nil
		}
		hi, err := predicates.NewLiteralComparison(predicates.ComparisonLessThanOrEq, p.BetweenHi).Eval(r[predKey(p.Qual, p.Col)])
		if err != nil {
			return predicates.TriUnknown, err
		}
		if hi == predicates.TriFalse {
			return predicates.TriFalse, nil
		}
		if lo == predicates.TriUnknown || hi == predicates.TriUnknown {
			return predicates.TriUnknown, nil
		}
		return predicates.TriTrue, nil
	case p.Op == predicates.ComparisonIn:
		return predicates.NewLiteralComparison(predicates.ComparisonIn, p.InList).Eval(r[predKey(p.Qual, p.Col)])
	default:
		return predicates.NewLiteralComparison(p.Op, p.Lit).Eval(r[predKey(p.Qual, p.Col)])
	}
}

// evalBoolTree combines child truth values with Kleene AND/OR.
func evalBoolTree(n *BoolNode, r Row) (predicates.TriBool, error) {
	if n.And {
		result := predicates.TriTrue
		for _, kid := range n.Kids {
			tb, err := evalBool(kid, r)
			if err != nil {
				return predicates.TriUnknown, err
			}
			if tb == predicates.TriFalse {
				return predicates.TriFalse, nil
			}
			if tb == predicates.TriUnknown {
				result = predicates.TriUnknown
			}
		}
		return result, nil
	}
	result := predicates.TriFalse
	for _, kid := range n.Kids {
		tb, err := evalBool(kid, r)
		if err != nil {
			return predicates.TriUnknown, err
		}
		if tb == predicates.TriTrue {
			return predicates.TriTrue, nil
		}
		if tb == predicates.TriUnknown {
			result = predicates.TriUnknown
		}
	}
	return result, nil
}

// compareNonNull orders two same-typed non-null values. P1 sort keys are
// NOT NULL by construction; a nil here is a harness bug, fail loudly.
func compareNonNull(a, b any) int {
	switch av := a.(type) {
	case int64:
		bv := b.(int64)
		switch {
		case av < bv:
			return -1
		case av > bv:
			return 1
		}
		return 0
	case string:
		return strings.Compare(av, b.(string))
	case bool:
		bv := b.(bool)
		switch {
		case !av && bv:
			return -1
		case av && !bv:
			return 1
		}
		return 0
	}
	panic(fmt.Sprintf("rowdiff: sort key with unexpected type %T (P1 sort keys are NOT NULL)", a))
}

func colNames(t TableDef) []string {
	names := make([]string, 0, len(t.Cols))
	for _, c := range t.Cols {
		names = append(names, c.Name)
	}
	return names
}

// predKey is the Row key a predicate operand reads: bare column in a
// single-table query, "<QUAL>.<COL>" in a join (where oracleJoinRows keys
// each side's fields by its alias).
func predKey(qual, col string) string {
	if qual == "" {
		return col
	}
	return qual + "." + col
}
