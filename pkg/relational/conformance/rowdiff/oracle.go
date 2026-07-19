package rowdiff

import (
	"fmt"
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
				key := k.Qual + "." + k.Col
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
