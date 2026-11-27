package rowdiff

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"fdb.dev/internal/recordlayer/query/plan/cascades/predicates"
)

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

	// applyWhere evaluates the post-join WHERE over a combined row; ok=false
	// drops it. Shared so the matched and NULL-extended (LEFT OUTER) paths run
	// exactly one WHERE evaluation each — the WHERE applies AFTER the join, so
	// an R-side predicate is UNKNOWN over a NULL-extended row and drops it,
	// which is precisely SQL's WHERE-vs-ON distinction.
	applyWhere := func(row Row) (bool, error) {
		if q.Where == nil {
			return true, nil
		}
		wb, err := evalBool(q.Where, row)
		if err != nil {
			return false, err
		}
		return wb == predicates.TriTrue, nil
	}

	var combined []Row
	emit := func(row Row) error {
		ok, err := applyWhere(row)
		if err != nil {
			return err
		}
		if ok {
			combined = append(combined, row)
		}
		return nil
	}

	// `matched` tracks ON-equality matches only — a preserved row that matched
	// but was then dropped by the WHERE is NOT re-emitted NULL-extended (it
	// matched the join; the WHERE removed it), exactly as SQL.
	if q.Join.RightOuter {
		// RIGHT OUTER: preserve every R row, NULL-extend unmatched L.
		for _, r := range c.Rows {
			matched := false
			for _, l := range c.Rows {
				tb, err := joinCmp.EvalAgainst(l[q.Join.LeftCol], r[q.Join.RightCol])
				if err != nil {
					return nil, err
				}
				if tb != predicates.TriTrue {
					continue
				}
				matched = true
				if err := emit(mkPairRow(c, l, r)); err != nil {
					return nil, err
				}
			}
			if !matched {
				if err := emit(mkPairRow(c, nil, r)); err != nil {
					return nil, err
				}
			}
		}
	} else {
		// INNER or LEFT OUTER: preserve L, NULL-extend unmatched R for LEFT.
		for _, l := range c.Rows {
			matched := false
			for _, r := range c.Rows {
				tb, err := joinCmp.EvalAgainst(l[q.Join.LeftCol], r[q.Join.RightCol])
				if err != nil {
					return nil, err
				}
				if tb != predicates.TriTrue {
					continue
				}
				matched = true
				if err := emit(mkPairRow(c, l, r)); err != nil {
					return nil, err
				}
			}
			if q.Join.LeftOuter && !matched {
				if err := emit(mkPairRow(c, l, nil)); err != nil {
					return nil, err
				}
			}
		}
	}

	return sortAndProjectJoin(combined, q, projection), nil
}

// oracleDerivedRows evaluates a derived-table query. The inner is SELECT *, so
// the derived table equals the flattened `WHERE Inner AND Outer`: filter the
// rows by both, then finalize (ORDER BY / project / DISTINCT) exactly as a plain
// single-table query. If the engine's flattening or scope resolution disagrees,
// the differential catches it.
func oracleDerivedRows(c *Case, q Query, projection []string) ([]Row, error) {
	d := q.Derived
	var out []Row
	for _, r := range c.Rows {
		if d.Inner != nil {
			tb, err := evalBool(d.Inner, r)
			if err != nil {
				return nil, err
			}
			if tb != predicates.TriTrue {
				continue
			}
		}
		if d.Outer != nil {
			tb, err := evalBool(d.Outer, r)
			if err != nil {
				return nil, err
			}
			if tb != predicates.TriTrue {
				continue
			}
		}
		out = append(out, r)
	}
	return finalizeSingleTable(out, q, projection, c), nil
}

// oracleUnionRows evaluates a UNION [ALL] of two single-table branches: filter
// and project each branch over the same columns, concatenate, then dedup unless
// ALL. The dedup uses the same typed distinctKey as SELECT DISTINCT, so UNION's
// set semantics and DISTINCT's agree by construction.
func oracleUnionRows(c *Case, q Query, projection []string) ([]Row, error) {
	u := q.Union
	cols := projection
	if cols == nil {
		cols = append([]string{"ID"}, colNames(c.Table)...)
	}
	outCols := make([]string, len(cols))
	for i, col := range cols {
		outCols[i] = OutputAlias(col)
	}
	filterProject := func(where *BoolNode) ([]Row, error) {
		var out []Row
		for _, r := range c.Rows {
			if where != nil {
				tb, err := evalBool(where, r)
				if err != nil {
					return nil, err
				}
				if tb != predicates.TriTrue {
					continue
				}
			}
			p := make(Row, len(cols))
			for _, col := range cols {
				p[OutputAlias(col)] = r[col]
			}
			out = append(out, p)
		}
		return out, nil
	}
	left, err := filterProject(u.Left)
	if err != nil {
		return nil, err
	}
	right, err := filterProject(u.Right)
	if err != nil {
		return nil, err
	}
	combined := make([]Row, 0, len(left)+len(right))
	combined = append(combined, left...)
	combined = append(combined, right...)
	if u.All {
		return combined, nil
	}
	seen := make(map[string]struct{}, len(combined))
	dedup := combined[:0]
	for _, r := range combined {
		key := distinctKey(r, outCols)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		dedup = append(dedup, r)
	}
	return dedup, nil
}

// mkPairRow builds a combined 2-way join row keyed "L.<col>" / "R.<col>". A nil
// side is NULL-extended (all its columns nil) — the outer-join case.
func mkPairRow(c *Case, l, r Row) Row {
	row := make(Row, 2*(len(c.Table.Cols)+1))
	putJoinSide(row, c, "L", l)
	putJoinSide(row, c, "R", r)
	return row
}

// putJoinSide writes one side's columns ("ID" plus the table columns) into row
// under the "<side>." prefix, or NULLs them all when src is nil.
func putJoinSide(row Row, c *Case, side string, src Row) {
	if src != nil {
		row[side+".ID"] = src["ID"]
	} else {
		row[side+".ID"] = nil
	}
	for _, col := range c.Table.Cols {
		if src != nil {
			row[side+"."+col.Name] = src[col.Name]
		} else {
			row[side+"."+col.Name] = nil
		}
	}
}

// sortAndProjectJoin applies the query's ORDER BY (if any) to the combined join
// rows and projects each to the aliased output columns (L_ID, M_A, …). Shared
// by the 2-way and 3-way join oracles so their ordering and projection are
// identical by construction. The oracle never applies LIMIT (§3): diffRows owns
// the LIMIT membership rule.
func sortAndProjectJoin(combined []Row, q Query, projection []string) []Row {
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
	return out
}

// oracleThreeWayJoin evaluates a 3-way self-join as a plain triple nested loop
// over the authoritative rows: L↔M on the first key, M↔R on the second, then
// the post-join WHERE. Independent of any build order the planner picks — that
// order-independence is exactly what the differential checks.
func oracleThreeWayJoin(c *Case, q Query, projection []string) ([]Row, error) {
	s := q.ThreeWay
	eq := predicates.NewLiteralComparison(predicates.ComparisonEquals, nil)
	var combined []Row
	for _, l := range c.Rows {
		for _, m := range c.Rows {
			lm, err := eq.EvalAgainst(l[s.LMLeft], m[s.LMRight])
			if err != nil {
				return nil, err
			}
			if lm != predicates.TriTrue {
				continue
			}
			// mkRow builds the L++M++R row; a nil r NULL-extends the R leg
			// (LEFT OUTER third leg). emit applies the WHERE and collects.
			mkRow := func(r Row) Row {
				row := make(Row, 3*(len(c.Table.Cols)+1))
				for k, v := range l {
					row["L."+k] = v
				}
				for k, v := range m {
					row["M."+k] = v
				}
				if r == nil {
					// NULL-extend every R column (LEFT JOIN no-match).
					row["R.ID"] = nil
					for _, col := range c.Table.Cols {
						row["R."+col.Name] = nil
					}
				} else {
					for k, v := range r {
						row["R."+k] = v
					}
				}
				return row
			}
			emit := func(row Row) error {
				if q.Where != nil {
					wb, werr := evalBool(q.Where, row)
					if werr != nil {
						return werr
					}
					if wb != predicates.TriTrue {
						return nil
					}
				}
				combined = append(combined, row)
				return nil
			}
			matched := false
			for _, r := range c.Rows {
				mr, err := eq.EvalAgainst(m[s.MRMid], r[s.MRRight])
				if err != nil {
					return nil, err
				}
				if mr != predicates.TriTrue {
					continue
				}
				matched = true
				if err := emit(mkRow(r)); err != nil {
					return nil, err
				}
			}
			// LEFT OUTER third leg: an (l,m) pair with no matching r yields one
			// R-NULL row (SQL applies WHERE AFTER the join, so an R-side filter
			// then evaluates UNKNOWN and drops it — handled by evalBool's
			// three-valued logic, not special-cased here).
			if s.ROuter && !matched {
				if err := emit(mkRow(nil)); err != nil {
					return nil, err
				}
			}
		}
	}
	return sortAndProjectJoin(combined, q, projection), nil
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

// aggOverflow is its own type so the sentinel cannot equal any real row value.
type aggOverflow struct{}

// errAggOverflow is foldAgg's sentinel for "SUM overflows int64 here", i.e.
// the engine is REQUIRED to raise 22003 rather than return a row. It is a
// distinct value (not an error) because it flows through the row slot.
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
		// -0.0 and +0.0 are DISTINCT dedup keys, and %v renders them
		// distinctly ("-0" vs "0") without help. Value identity in this engine
		// is tuple-key identity, and the tuple encoding preserves the sign bit.
		// For signed zero, DISTINCT, grouping, and uniqueness paths all preserve
		// those bytes rather than following IEEE `=`; NaN is deliberately
		// different because DISTINCT recursively canonicalizes its payloads (see
		// packedDedupKey's doc comment). A previous
		// revision canonicalized zero here to mirror an engine-side
		// canonicalization that has since been removed; leaving it would make
		// the oracle disagree with the engine it is meant to check, and a
		// differential whose oracle is wrong reports the correct engine as the
		// defect.
		parts = append(parts, fmt.Sprintf("%s=%v(%T)", c, r[c], r[c]))
	}
	return strings.Join(parts, "|")
}
