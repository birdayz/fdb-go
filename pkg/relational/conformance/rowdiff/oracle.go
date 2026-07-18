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
				cmp := compareNonNull(out[i][k.Col], out[j][k.Col])
				if cmp == 0 {
					continue
				}
				if k.Desc {
					return cmp > 0
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
	return projected, nil
}

func evalBool(n *BoolNode, r Row) (predicates.TriBool, error) {
	if n.Leaf != nil {
		p := n.Leaf
		cmp := predicates.NewLiteralComparison(p.Op, p.Lit)
		return cmp.Eval(r[p.Col])
	}
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
