// Portions derived from FoundationDB Record Layer (Type.java,
// SemanticAnalyzer.java),
// Copyright 2015-2026 Apple Inc. and the FoundationDB project authors
// Copyright 2021-2025 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package rowdiff

import (
	"math/rand/v2"
	"strconv"

	"fdb.dev/internal/recordlayer/query/plan/cascades/predicates"
)

// JoinSpec is a SELF-join of the case's table under aliases L and R, joined
// on `L.<LeftCol> = R.<RightCol>`. A self-join exercises the whole join
// planner (NLJ, join ordering, correlated index access) without needing a
// second table, and keeps the oracle a plain nested loop over one row set.
//
// Join queries always project EXPLICITLY ALIASED columns (l_id, r_a, …):
// unaliased `l.id, r.id` would yield two output columns both named ID, and
// the harness keys rows by column name, so the duplicate would collapse and
// silently weaken the comparison.
type JoinSpec struct {
	LeftCol  string
	RightCol string
	// Inner renders `JOIN … ON …`; when false the join is expressed as a
	// comma cross-join with the equality moved into the WHERE — the same
	// logical query, a different parse path into the planner.
	Inner bool
	// LeftOuter makes the ON-join a LEFT OUTER JOIN: unmatched left rows are
	// kept, NULL-extended on the right. Implies Inner (LEFT OUTER needs the ON
	// form — a comma join has no outer semantics). This reaches the
	// NULL-extension wrong-rows surface INNER never does, and pins the
	// WHERE-vs-ON subtlety: an R-side filter in the WHERE drops the
	// NULL-extended rows (r.col is NULL → predicate UNKNOWN), collapsing a
	// LEFT JOIN back toward inner semantics — the oracle applies the WHERE
	// post-join over the NULL-extended row, exactly as SQL does.
	LeftOuter bool
	// RightOuter makes the ON-join a RIGHT OUTER JOIN: the mirror of LeftOuter,
	// preserving every RIGHT row and NULL-extending unmatched LEFT. Implies
	// Inner and is mutually exclusive with LeftOuter. `A RIGHT JOIN B` is
	// semantically `B LEFT JOIN A`, so this exercises the engine's RIGHT→LEFT
	// normalization — a rewrite bug there is a wrong-rows divergence.
	RightOuter bool
}

// AggFunc is a supported aggregate. AVG is deliberately absent: its
// result type and rounding are their own semantic axis (integer vs
// floating division), and getting that wrong in the oracle would produce
// false findings rather than real ones.
type AggFunc int

const (
	AggCountStar AggFunc = iota // COUNT(*)   — counts rows, NULLs included
	AggCountCol                 // COUNT(col) — counts NON-NULL values only
	AggSum
	AggMin
	AggMax
)

// AggSpec is one aggregate query: optional GROUP BY key plus one aggregate
// over a BIGINT column.
//
// HONESTY NOTE (RFC-182 §7 / §12): unlike every other oracle path, the
// aggregate evaluation here is a REIMPLEMENTATION — aggregation is not
// expressible through the engine's per-row Comparison evaluation, so this
// is the one place the oracle restates SQL semantics rather than sharing
// the engine's. The restated rules are the well-defined ones (SUM/MIN/MAX
// ignore NULLs and yield NULL for an all-NULL or empty group; COUNT(*)
// counts rows; COUNT(col) counts non-NULLs; a NULL grouping key is its own
// group), and a divergence here is investigated on BOTH sides before it is
// called an engine bug.
type AggSpec struct {
	Func     AggFunc
	Col      string   // aggregated column ("" for COUNT(*))
	GroupBy  []string // empty = scalar aggregate over the whole input; 1+ = grouped
	Having   *Pred    // optional filter on the aggregate result
	HavingOn bool
}

// groupKeyCol names the i-th GROUP BY key's output column. Key 0 stays "G" so
// single-key grouping is byte-identical to the pre-multi-key schema; extra keys
// are "G1", "G2", … Both aggSQL (the aliases) and the oracle emit these names.
func groupKeyCol(i int) string {
	if i == 0 {
		return "G"
	}
	return "G" + strconv.Itoa(i)
}

// ThreeWayJoinSpec is a 3-way self-join over the case's table under aliases L,
// M, R, chained `L.LMLeft = M.LMRight` and `M.MRMid = R.MRRight`. A 3-way join
// exercises JOIN ORDERING — the planner picks among 3! build orders and must
// keep every join key correct through the chosen order — which the 2-way
// self-join cannot reach. INNER only: the outer-leg 3-way shapes are simply not
// generated here yet (the 2-way LEFT/RIGHT generator already covers outer-join
// row soundness, incl. the InJoin+sort leg-window path fixed in
// TestFDB_LeftJoinPkOrdinal_InJoinSortRegression). The oracle stays a plain
// triple nested loop over the same authoritative rows.
type ThreeWayJoinSpec struct {
	LMLeft  string // L column of the L↔M key
	LMRight string // M column of the L↔M key
	MRMid   string // M column of the M↔R key
	MRRight string // R column of the M↔R key
	// Comma expresses the joins as a 3-way comma cross-join with both
	// equalities in the WHERE; otherwise chained `JOIN … ON`. Same logical
	// query, a different parse path into the planner.
	Comma bool
	// ROuter makes the THIRD leg a LEFT OUTER join
	// (`l JOIN m ON … LEFT JOIN r ON …`): a (l,m) pair with no matching r is
	// NULL-extended rather than dropped. Only meaningful for the JOIN…ON form
	// (a comma cross-join cannot express OUTER), so the generator sets it only
	// when !Comma. This exercises the composition of the two ordinal-binding
	// fixes — outer-join multi-leg rows AND 3-way shared-column ordinals — the
	// space where a residual of either would surface.
	ROuter bool
}

// UnionSpec is a set operation between two single-table branches over the same
// table with the same projection:
//
//	SELECT <proj> FROM t WHERE <Left> UNION [ALL] SELECT <proj> FROM t WHERE <Right>
//
// UNION dedups the combined output (SQL set semantics); UNION ALL keeps every
// row. Overlapping branch predicates make a row appear in both branches, so the
// dedup does real work — a bug there (over- or under-deduping, or the wrong
// dedup key) is a wrong-rows divergence.
type UnionSpec struct {
	Left  *BoolNode // WHERE of the left branch (nil = no filter)
	Right *BoolNode // WHERE of the right branch
	All   bool      // UNION ALL (no dedup) vs UNION (dedup)
}

// DerivedSpec is a subquery in FROM (a derived table):
//
//	SELECT <proj> FROM (SELECT * FROM t WHERE <Inner>) d [WHERE <Outer>]
//
// The inner is SELECT * so the derived table is a pass-through equal to the
// flattened `WHERE Inner AND Outer` — which is exactly what it tests: whether
// the planner FLATTENS the derived table and pushes the outer predicate through
// the subquery scope. A flattening or scope-resolution bug is a wrong-rows
// divergence. Outer references the derived columns unqualified (d is the only
// source).
type DerivedSpec struct {
	Inner *BoolNode // WHERE inside the subquery
	Outer *BoolNode // WHERE over the derived table (nil = none)
	// Cte renders the subquery as a single-reference common table expression
	// (`WITH d AS (…) SELECT … FROM d …`) instead of an inline derived table.
	// A single-reference CTE is semantically identical (same flattening, same
	// oracle) but exercises the WITH scope-resolution path.
	Cte bool
}

// genThreeWayJoin builds a 3-way self-join chain `L JOIN M ON L.k=M.k JOIN R ON
// M.k=R.k` (or the comma form), plus a small conjunction of qualified
// single-sided predicates. Each key's probed side prefers an indexed column so
// the planner has correlated-index access across whichever build order it picks.
func genThreeWayJoin(rng *rand.Rand, table TableDef) Query {
	indexed := map[string]bool{}
	for _, idx := range table.Indexes {
		for _, cc := range idx.Cols {
			indexed[cc] = true
		}
	}
	allBig := []string{"ID"}
	// The PROBED side of each key (LMRight/MRRight) is heavily weighted toward
	// the unique pk. A domain-0..9 key on BOTH sides makes each self-join leg
	// fan out ~n/10, so a 3-way chain over 120 rows yields ~17k result rows and
	// an O(n^3) oracle — tripling sweep wall-clock. Probing the pk keeps most
	// chains bounded (each step matches ≤1 row) while still exercising join
	// ORDERING and correlated index access, which is the point.
	probed := []string{"ID", "ID", "ID"} // ID weighted 3x
	for _, col := range table.Cols {
		if col.Type == ColBigint {
			allBig = append(allBig, col.Name)
			if indexed[col.Name] {
				probed = append(probed, col.Name)
			}
		}
	}
	spec := &ThreeWayJoinSpec{
		LMLeft:  allBig[rng.IntN(len(allBig))],
		LMRight: probed[rng.IntN(len(probed))],
		MRMid:   allBig[rng.IntN(len(allBig))],
		MRRight: probed[rng.IntN(len(probed))],
		Comma:   rng.IntN(2) == 0,
	}
	// The third leg is LEFT OUTER ~1/3 of the JOIN…ON forms (a comma cross-join
	// cannot express OUTER).
	spec.ROuter = !spec.Comma && rng.IntN(3) == 0

	// 1-2 qualified single-sided filters (Qual L/M/R). AND-only (no top-level
	// OR), so the comma form's `joinEq AND <where>` needs no parenthesization.
	var leaves []*Pred
	n := 1 + rng.IntN(2)
	sides := []string{"L", "M", "R"}
	for i := 0; i < n; i++ {
		col := table.Cols[rng.IntN(len(table.Cols))]
		p := genPred(rng, table, col, indexed[col.Name])
		p.Qual = sides[rng.IntN(len(sides))]
		leaves = append(leaves, p)
	}
	kids := make([]*BoolNode, len(leaves))
	for i, l := range leaves {
		kids[i] = &BoolNode{Leaf: l}
	}
	var where *BoolNode
	if len(kids) == 1 {
		where = kids[0]
	} else {
		where = &BoolNode{And: true, Kids: kids}
	}

	q := Query{ThreeWay: spec, Where: where}
	// A total ORDER BY over all three ids keeps the ordered comparison exact.
	if rng.IntN(2) == 0 {
		q.OrderBy = []OrderKey{
			{Col: "ID", Qual: "L", Desc: rng.IntN(2) == 0},
			{Col: "ID", Qual: "M"},
			{Col: "ID", Qual: "R"},
		}
	}
	return q
}

// genBranchWhere builds a small 1-2 leaf AND-conjunction for a UNION branch.
// Kept simple so the two branches overlap often, exercising the dedup.
func genBranchWhere(rng *rand.Rand, table TableDef, indexed map[string]bool) *BoolNode {
	n := 1 + rng.IntN(2)
	kids := make([]*BoolNode, n)
	for i := 0; i < n; i++ {
		col := table.Cols[rng.IntN(len(table.Cols))]
		kids[i] = &BoolNode{Leaf: genPred(rng, table, col, indexed[col.Name])}
	}
	if n == 1 {
		return kids[0]
	}
	return &BoolNode{And: true, Kids: kids}
}

// genUnionQuery builds `SELECT … WHERE <l> UNION ALL SELECT … WHERE <r>` over
// the same table (same projection on both branches). Only UNION ALL: the engine
// rejects plain UNION (dedup) with 0AF00 "only UNION ALL is supported" — a
// capability it lacks, not a soundness case, so generating it would just probe
// an unsupported-query error (the boundary is pinned by
// TestFDB_UnionDedup_Unsupported instead). The oracle's dedup path stays
// exercised by the isolation test and is ready if UNION-dedup ever lands.
func genUnionQuery(rng *rand.Rand, table TableDef) Query {
	indexed := map[string]bool{}
	for _, idx := range table.Indexes {
		for _, cc := range idx.Cols {
			indexed[cc] = true
		}
	}
	return Query{Union: &UnionSpec{
		Left:  genBranchWhere(rng, table, indexed),
		Right: genBranchWhere(rng, table, indexed),
		All:   true,
	}}
}

// genDerivedQuery builds `SELECT <proj> FROM (SELECT * FROM t WHERE <inner>) d
// [WHERE <outer>]`, optionally DISTINCT or ORDER BY on the outer.
func genDerivedQuery(rng *rand.Rand, table TableDef) Query {
	indexed := map[string]bool{}
	for _, idx := range table.Indexes {
		for _, cc := range idx.Cols {
			indexed[cc] = true
		}
	}
	q := Query{Derived: &DerivedSpec{
		Inner: genBranchWhere(rng, table, indexed),
		Cte:   rng.IntN(2) == 0, // half as a WITH-CTE, half as an inline derived table
	}}
	if rng.IntN(3) != 0 {
		q.Derived.Outer = genBranchWhere(rng, table, indexed)
	}
	// DISTINCT (unordered multiset) OR an ID ORDER BY — not both, mirroring the
	// plain path (DISTINCT results are compared unordered).
	if rng.IntN(6) == 0 {
		q.Distinct = true
	} else if rng.IntN(2) == 0 {
		q.OrderBy = []OrderKey{{Col: "ID"}}
	}
	return q
}

// genAggQuery builds `SELECT [g,] <agg> FROM t [WHERE …] [GROUP BY g]
// [HAVING …]`. The WHERE is biased to indexed columns so the aggregate can
// sit over an index access — the shape where a residual filter dropped by
// the aggregate data-access path produced silently wrong sums.
func genAggQuery(rng *rand.Rand, table TableDef) Query {
	indexed := map[string]bool{}
	for _, idx := range table.Indexes {
		for _, c := range idx.Cols {
			indexed[c] = true
		}
	}
	bigints := bigintCols(table)

	spec := &AggSpec{}
	switch rng.IntN(5) {
	case 0:
		spec.Func = AggCountStar
	case 1:
		spec.Func, spec.Col = AggCountCol, bigints[rng.IntN(len(bigints))]
	case 2:
		spec.Func, spec.Col = AggSum, bigints[rng.IntN(len(bigints))]
	case 3:
		spec.Func, spec.Col = AggMin, bigints[rng.IntN(len(bigints))]
	default:
		spec.Func, spec.Col = AggMax, bigints[rng.IntN(len(bigints))]
	}
	// GROUP BY two thirds of the time; prefer an indexed key so the
	// aggregate-index / streaming-aggregation paths are reachable.
	//
	// DOUBLE and FLOAT are eligible grouping keys. They were restricted to
	// BIGINT, which meant the harness could not reach a signed-zero GROUP BY
	// at all: the D/E columns seed -0.0, but a grouping key was never chosen
	// from them, so the gap was invisible except by reading what the
	// generator was structurally incapable of generating.
	//
	// Safe now that grouping is settled: -0.0 and +0.0 are two groups on every
	// path (packedDedupKey's doc comment has the argument), and the oracle
	// agrees by construction — it keys groups on `%v(%T)`, and %v renders -0
	// and 0 distinctly. The float domain carries no NaN or Infinity (see the
	// ColType doc comment: neither has a SQL literal spelling), so there is no
	// payload-identity question to model here.
	groupable := func(t ColType) bool {
		return t == ColBigint || t == ColDouble || t == ColFloat
	}
	if rng.IntN(3) != 0 {
		keys := []string{}
		for _, col := range table.Cols {
			if groupable(col.Type) && indexed[col.Name] {
				keys = append(keys, col.Name)
			}
		}
		if len(keys) == 0 {
			for _, col := range table.Cols {
				if groupable(col.Type) {
					keys = append(keys, col.Name)
				}
			}
		}
		if len(keys) == 0 {
			keys = bigints
		}
		// One grouping key, or two distinct keys ~1/3 of the time — the
		// multi-column grouping path (composite-key equality and a NULL in
		// either key column forming its own group).
		first := keys[rng.IntN(len(keys))]
		spec.GroupBy = []string{first}
		if len(keys) > 1 && rng.IntN(3) == 0 {
			others := make([]string, 0, len(keys)-1)
			for _, k := range keys {
				if k != first {
					others = append(others, k)
				}
			}
			spec.GroupBy = append(spec.GroupBy, others[rng.IntN(len(others))])
		}
	}

	q := Query{Agg: spec}
	// A WHERE over an INDEXED column plus (often) an unindexed residual —
	// the AGG-RESIDUAL shape.
	if rng.IntN(3) != 0 {
		var leaves []*Pred
		for _, col := range table.Cols {
			if indexed[col.Name] && col.Type == ColBigint {
				leaves = append(leaves, &Pred{
					Col: col.Name, Op: predicates.ComparisonEquals, Lit: int64(rng.IntN(6)),
				})
				break
			}
		}
		if len(leaves) > 0 && rng.IntN(2) == 0 {
			leaves = append(leaves, &Pred{
				Col: "S", Op: predicates.ComparisonEquals,
				Lit: []string{"alpha", "beta", "gamma"}[rng.IntN(3)],
			})
		}
		switch len(leaves) {
		case 0:
		case 1:
			q.Where = &BoolNode{Leaf: leaves[0]}
		default:
			kids := make([]*BoolNode, len(leaves))
			for i, l := range leaves {
				kids[i] = &BoolNode{Leaf: l}
			}
			q.Where = &BoolNode{And: true, Kids: kids}
		}
	}
	// HAVING on the aggregate result, only for grouped queries.
	if len(spec.GroupBy) > 0 && rng.IntN(3) == 0 {
		spec.HavingOn = true
		spec.Having = &Pred{
			Op:  []predicates.ComparisonType{predicates.ComparisonGreaterThan, predicates.ComparisonLessThanOrEq}[rng.IntN(2)],
			Lit: int64(rng.IntN(4)),
		}
	}
	return q
}

// genJoinQuery builds a self-join: `t AS l JOIN t AS r ON l.X = r.Y`, plus a
// small conjunction of qualified single-sided predicates. Both operands of
// the join key are drawn from the indexed columns where possible so the
// planner has a correlated-access option, not only a nested-loop scan.
func genJoinQuery(rng *rand.Rand, table TableDef) Query {
	indexed := map[string]bool{}
	for _, idx := range table.Indexes {
		for _, c := range idx.Cols {
			indexed[c] = true
		}
	}
	// Join key: a genuinely cross-CATEGORY comparison (numeric vs
	// string/bool) has NO common maximum type — the engine rejects it with
	// 42804, correct behaviour that tests nothing but the error path — so
	// STRING/BOOLEAN keys are excluded. But a cross-TYPE NUMERIC pair
	// (BIGINT = DOUBLE, BIGINT = FLOAT) DOES have a common maximum type
	// (values.MaximumType / Java's Type.maximumType) and lowers to a
	// promoted correlated index probe (expr.promoteColumnColumnNumeric) —
	// exactly the shape that returned EMPTY for every row before that fix,
	// so mixing numeric column types on the two join-key sides is
	// deliberate, not an oversight. Prefer an indexed column on the right
	// (the probed side) so a correlated index access is available; ID is
	// always indexed (pk).
	rightCandidates := []string{"ID"}
	for _, col := range table.Cols {
		if isNumericColType(col.Type) && indexed[col.Name] {
			rightCandidates = append(rightCandidates, col.Name)
		}
	}
	leftCandidates := []string{"ID"}
	for _, col := range table.Cols {
		if isNumericColType(col.Type) {
			leftCandidates = append(leftCandidates, col.Name)
		}
	}

	spec := &JoinSpec{
		LeftCol:  leftCandidates[rng.IntN(len(leftCandidates))],
		RightCol: rightCandidates[rng.IntN(len(rightCandidates))],
	}
	// Join syntax/type: comma cross-join, INNER … ON, LEFT OUTER … ON, or
	// RIGHT OUTER … ON. The OUTER forms need the ON form, so they set Inner too.
	switch rng.IntN(4) {
	case 0:
		spec.Inner = false // comma cross-join (equality moves to WHERE)
	case 1:
		spec.Inner = true // INNER … ON
	case 2:
		spec.Inner, spec.LeftOuter = true, true // LEFT OUTER … ON
	default:
		spec.Inner, spec.RightOuter = true, true // RIGHT OUTER … ON
	}

	// 1-2 qualified single-sided filter leaves, biased to indexed columns so
	// each side can drive an index access.
	var leaves []*Pred
	n := 1 + rng.IntN(2)
	for i := 0; i < n; i++ {
		col := table.Cols[rng.IntN(len(table.Cols))]
		p := genPred(rng, table, col, indexed[col.Name])
		p.Qual = []string{"L", "R"}[rng.IntN(2)]
		leaves = append(leaves, p)
	}

	// A CROSS-SIDE column comparison (`l.a > r.b`) — a theta/non-equi
	// residual on top of the join equality, which the single-sided leaves
	// above can never produce. Numeric columns only (BIGINT/DOUBLE/FLOAT):
	// every pair has a common maximum type, so a mixed-width pair (e.g.
	// `l.a > r.d`) promotes rather than erroring — the same
	// promoteColumnColumnNumeric path the join KEY above exercises, reached
	// here as a residual filter instead of the ON-equality.
	if rng.IntN(3) == 0 {
		numericCols := numericColsOf(table)
		ops := []predicates.ComparisonType{
			predicates.ComparisonLessThan, predicates.ComparisonGreaterThan,
			predicates.ComparisonNotEquals, predicates.ComparisonLessThanOrEq,
		}
		leaves = append(leaves, &Pred{
			Col: numericCols[rng.IntN(len(numericCols))], Qual: "L",
			Op:     ops[rng.IntN(len(ops))],
			RhsCol: numericCols[rng.IntN(len(numericCols))], RhsQual: "R",
		})
	}

	kids := make([]*BoolNode, len(leaves))
	for i, l := range leaves {
		kids[i] = &BoolNode{Leaf: l}
	}
	var where *BoolNode
	if len(kids) == 1 {
		where = kids[0]
	} else {
		where = &BoolNode{And: true, Kids: kids}
	}

	q := Query{Join: spec, Where: where}
	// A total ORDER BY over both sides' ids keeps the ordered comparison exact.
	if rng.IntN(2) == 0 {
		q.OrderBy = []OrderKey{
			{Col: "ID", Qual: "L", Desc: rng.IntN(2) == 0},
			{Col: "ID", Qual: "R"},
		}
		if rng.IntN(3) == 0 {
			q.Limit = 1 + rng.IntN(20)
		}
	} else if rng.IntN(4) == 0 {
		// UNORDERED join + LIMIT — the exact shape that exposed the dropped
		// projection aliases. Without this arm the generator could never
		// regenerate its own finding; the comparator handles it via the
		// sub-multiset membership rule.
		q.Limit = 1 + rng.IntN(20)
	}
	return q
}
