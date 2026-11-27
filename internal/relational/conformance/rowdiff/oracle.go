package rowdiff

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"fdb.dev/internal/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/internal/recordlayer/query/plan/cascades/values"
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
	// A WhereOverride is opaque SQL the typed walk below cannot evaluate. It
	// must REFUSE, not fall through to q.Where: falling through would evaluate
	// a different predicate than the engine ran and report the difference as a
	// soundness divergence — a harness bug wearing a finding's clothes.
	if q.WhereOverride != nil {
		return nil, fmt.Errorf("OracleRows: query carries a WhereOverride (%q); Oracle M evaluates the typed predicate tree and cannot interpret raw SQL", *q.WhereOverride)
	}
	if q.Agg != nil {
		return oracleAggRows(c, q)
	}
	if q.Join != nil {
		return oracleJoinRows(c, q, projection)
	}
	if q.ThreeWay != nil {
		return oracleThreeWayJoin(c, q, projection)
	}
	if q.Union != nil {
		return oracleUnionRows(c, q, projection)
	}
	if q.Derived != nil {
		return oracleDerivedRows(c, q, projection)
	}
	// A non-correlated scalar subquery is evaluated ONCE, its value shared by
	// every outer row (that is the whole point of "non-correlated").
	var scalarVal any
	if q.ScalarSub != nil {
		v, err := scalarSubValue(c, q.ScalarSub)
		if err != nil {
			return nil, err
		}
		scalarVal = v
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
		if q.ScalarSub != nil {
			// outerCol <op> scalarVal, through the shared Comparison — a NULL
			// scalar (empty MIN/MAX) makes every comparison UNKNOWN → the row
			// drops, matching SQL's `col <op> NULL`.
			tb, err := predicates.NewLiteralComparison(q.ScalarSub.Op, scalarVal).Eval(r[q.ScalarSub.OuterCol])
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

// finalizeSingleTable applies a single-table query's ORDER BY, projection, and
// SELECT DISTINCT to the filtered rows. Shared by the plain and derived-table
// paths so their ordering/projection/dedup are identical by construction. It
// deliberately does NOT apply q.Limit — the oracle returns the full result
// multiset and diffRows applies the §3 LIMIT membership rule.
func finalizeSingleTable(out []Row, q Query, projection []string, c *Case) []Row {
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
	outCols := make([]string, len(cols))
	for i, col := range cols {
		outCols[i] = OutputAlias(col)
	}
	projected := make([]Row, 0, len(out))
	for _, r := range out {
		p := make(Row, len(cols))
		for _, col := range cols {
			// Keyed by the OUTPUT alias, which is the column itself for a flat
			// projection and `N_A` for a nested one — matching the alias the
			// renderer emits, because the harness compares engine rows to
			// oracle rows by output column name.
			p[OutputAlias(col)] = r[col]
		}
		projected = append(projected, p)
	}

	// SELECT DISTINCT: dedup the PROJECTED rows (SQL semantics — distinct over
	// the output columns).
	if q.Distinct {
		seen := make(map[string]struct{}, len(projected))
		dedup := projected[:0]
		for _, r := range projected {
			key := distinctKey(r, outCols)
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
			dedup = append(dedup, r)
		}
		projected = dedup
	}
	return projected
}

// scalarSubValue evaluates a non-correlated aggregate scalar subquery: fold the
// aggregate over the rows passing the optional inner filter. Reusing foldAgg
// keeps the NULL-when-empty (MIN/MAX) and 0-when-empty (COUNT) semantics
// identical to the grouped-aggregate oracle. Func is restricted to MIN/MAX/COUNT
// by the generator, so foldAgg never returns its SUM-overflow sentinel here.
func scalarSubValue(c *Case, s *ScalarSubSpec) (any, error) {
	var inner []Row
	for _, r := range c.Rows {
		if s.Filter != nil {
			tb, err := evalLeaf(s.Filter, r)
			if err != nil {
				return nil, err
			}
			if tb != predicates.TriTrue {
				continue
			}
		}
		inner = append(inner, r)
	}
	return foldAgg(&AggSpec{Func: s.Func, Col: s.Col}, inner), nil
}

// caseArmVal resolves a CASE arm: the row's column value when a column is
// named, else the literal.
func caseArmVal(col string, lit int64, r Row) any {
	if col != "" {
		return r[col]
	}
	return lit
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
	case p.Cast != nil:
		return evalCastLeaf(p, r)
	case p.NumFn != nil:
		return evalNumFnLeaf(p, r)
	case p.StrFn != nil:
		return evalStrFnLeaf(p, r)
	case p.Case != nil:
		return evalCaseLeaf(p, r)
	case p.HasArith:
		// `(Col <ArithOp> ArithCol2) <Op> Lit`. The arithmetic goes through the
		// engine's own ArithmeticValue (baked constants), so the value and its
		// NULL/overflow semantics are shared, not restated. A NULL in either
		// operand makes the LHS NULL → the comparison is UNKNOWN.
		lv := r[predKey(p.Qual, p.Col)]
		rv := r[predKey(p.Qual, p.ArithCol2)]
		li, lok := lv.(int64)
		ri, rok := rv.(int64)
		if !lok || !rok {
			return predicates.TriUnknown, nil
		}
		av, err := values.NewArithmeticValue(p.ArithOp,
			&values.ConstantValue{Value: li, Typ: values.NullableLong},
			&values.ConstantValue{Value: ri, Typ: values.NullableLong})
		if err != nil {
			return predicates.TriUnknown, err
		}
		res, err := av.Evaluate(nil)
		if err != nil {
			// Overflow: not reachable for OpSub over the value domain, but if a
			// future op adds it, surface it (the engine raises 22003).
			return predicates.TriUnknown, err
		}
		return predicates.NewLiteralComparison(p.Op, p.Lit).Eval(res)
	case p.Bitwise:
		// `(Col <bitop> BitCol2) <Op> Lit`. Folded through the engine's own
		// ArithmeticValue (the BITAND/BITOR/BITXOR lanes), so the bitwise + NULL
		// semantics are shared, not restated. A NULL in either operand makes the
		// LHS NULL → the comparison is UNKNOWN (the engine's value is NULL for a
		// NULL operand, matched here).
		lv := r[predKey(p.Qual, p.Col)]
		rv := r[predKey(p.Qual, p.BitCol2)]
		li, lok := lv.(int64)
		ri, rok := rv.(int64)
		if !lok || !rok {
			return predicates.TriUnknown, nil
		}
		op, ok := map[string]values.ArithmeticOp{
			"BITAND": values.OpBitAnd, "BITOR": values.OpBitOr, "BITXOR": values.OpBitXor,
		}[p.BitOp]
		if !ok {
			return predicates.TriUnknown, fmt.Errorf("rowdiff: bit operator %q", p.BitOp)
		}
		fn, err := values.NewArithmeticValue(op,
			&values.ConstantValue{Value: li, Typ: values.NullableLong},
			&values.ConstantValue{Value: ri, Typ: values.NullableLong})
		if err != nil {
			return predicates.TriUnknown, err
		}
		res, err := fn.Evaluate(nil)
		if err != nil {
			return predicates.TriUnknown, err
		}
		return predicates.NewLiteralComparison(p.Op, p.Lit).Eval(res)
	case p.RhsCol != "":
		// Column-vs-column: the engine's own two-sided evaluation.
		return predicates.NewLiteralComparison(p.Op, nil).EvalAgainst(r[predKey(p.Qual, p.Col)], r[predKey(p.RhsQual, p.RhsCol)])
	case p.IsBetween:
		return evalBetweenLeaf(p, r)
	case p.Op == predicates.ComparisonIn:
		return predicates.NewLiteralComparison(predicates.ComparisonIn, p.InList).Eval(r[predKey(p.Qual, p.Col)])
	default:
		return predicates.NewLiteralComparison(p.Op, p.Lit).Eval(r[predKey(p.Qual, p.Col)])
	}
}

func evalCastLeaf(p *Pred, r Row) (predicates.TriBool, error) {
	// CAST(col AS STRING): int→string is Go's FormatInt, bool→string is
	// "true"/"false" — both match the engine over the domain (verified by
	// probe). CAST(NULL AS STRING) is NULL → the comparison is UNKNOWN.
	v := r[p.Cast.Col]
	if v == nil {
		return predicates.TriUnknown, nil
	}
	var s string
	if p.Cast.FromInt {
		s = strconv.FormatInt(v.(int64), 10)
	} else {
		s = strconv.FormatBool(v.(bool))
	}
	return predicates.NewLiteralComparison(p.Op, p.Lit).Eval(s)
}

func evalNumFnLeaf(p *Pred, r Row) (predicates.TriBool, error) {
	// COALESCE absorbs NULL: a NULL column yields Default (never NULL,
	// verified: COALESCE(NULL,5)=5) — handle it before the NULL guard the
	// arithmetic functions need.
	if p.NumFn.Fn == NumFnCoalesce {
		result := p.NumFn.Default
		if v, ok := r[p.NumFn.Col].(int64); ok {
			result = v
		}
		return predicates.NewLiteralComparison(p.Op, p.Lit).Eval(result)
	}
	// ABS / MOD over BIGINT — Go's abs and % match the engine over the
	// value domain (no MinInt64 → no ABS overflow; % follows the dividend's
	// sign like SQL MOD; the MOD divisor is a nonzero literal → no divzero).
	// A NULL column makes the result NULL → the comparison is UNKNOWN.
	v, ok := r[p.NumFn.Col].(int64)
	if !ok {
		return predicates.TriUnknown, nil
	}
	var result int64
	if p.NumFn.Fn == NumFnMod {
		result = v % p.NumFn.Mod
	} else if v < 0 {
		result = -v
	} else {
		result = v
	}
	return predicates.NewLiteralComparison(p.Op, p.Lit).Eval(result)
}

func evalStrFnLeaf(p *Pred, r Row) (predicates.TriBool, error) {
	// CONCAT is the exception: it treats a NULL operand as "" (verified:
	// CONCAT(NULL,'x')='x'), so its result is NEVER NULL — handle it before
	// the NULL-propagation guard the other functions need.
	if p.StrFn.Fn == StrFnConcat {
		sv, _ := r[p.StrFn.Col].(string) // "" when the column is NULL
		return predicates.NewLiteralComparison(p.Op, p.Lit).Eval(sv + p.StrFn.Suffix)
	}
	// UPPER/LOWER/LENGTH/SUBSTR over the ASCII string domain — Go's
	// strings.ToUpper / ToLower / len / clamped-slice match the engine
	// (verified by probe). A NULL column makes the result NULL → UNKNOWN.
	sv, ok := r[p.StrFn.Col].(string)
	if !ok {
		return predicates.TriUnknown, nil
	}
	var result any
	switch p.StrFn.Fn {
	case StrFnUpper:
		result = strings.ToUpper(sv)
	case StrFnLower:
		result = strings.ToLower(sv)
	case StrFnLength:
		result = int64(len(sv))
	case StrFnSubstr:
		result = substrVal(sv, p.StrFn.Start, p.StrFn.Length)
	case StrFnTrim:
		// SQL TRIM strips leading/trailing SPACES — Go's Trim over " "
		// (the domain has no other whitespace); verified TRIM(' ab ')='ab'.
		result = strings.Trim(sv, " ")
	}
	return predicates.NewLiteralComparison(p.Op, p.Lit).Eval(result)
}

func evalCaseLeaf(p *Pred, r Row) (predicates.TriBool, error) {
	// SQL searched CASE: the WHEN arm is taken only when its condition is
	// TRUE; a FALSE or UNKNOWN (NULL-operand) condition falls through to
	// ELSE. The WHEN condition and the outer comparison both go through the
	// shared Comparison eval; only the branch pick is restated here.
	cs := p.Case
	cond, err := evalLeaf(cs.When, r)
	if err != nil {
		return predicates.TriUnknown, err
	}
	var result any
	if cond == predicates.TriTrue {
		result = caseArmVal(cs.ThenCol, cs.ThenLit, r)
	} else {
		result = caseArmVal(cs.ElseCol, cs.ElseLit, r)
	}
	return predicates.NewLiteralComparison(p.Op, p.Lit).Eval(result)
}

func evalBetweenLeaf(p *Pred, r Row) (predicates.TriBool, error) {
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
	case float64:
		// values.CompareFloat64, NOT native </>/==. On the generator's
		// deliberately NaN-free domain, this matches the relevant FDB tuple/index
		// total order, including the signed-zero split. It does NOT model raw FDB
		// NaN sign/payload order; no generated row can reach that case. This sort
		// order deliberately DIFFERS from the IEEE equality predicate.Eval's
		// cmpAny uses (-0.0 == +0.0 there) — the same
		// documented predicate-vs-sort split as RFC-082's
		// double_negative_zero_ge_predicate. Sharing the engine's own sort
		// comparator (rather than restating float ordering here) keeps that
		// split correct by construction instead of by coincidence.
		return values.CompareFloat64(av, b.(float64))
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
