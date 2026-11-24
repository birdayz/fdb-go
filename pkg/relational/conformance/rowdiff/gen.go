// Portions derived from FoundationDB Record Layer (Type.java,
// SemanticAnalyzer.java),
// Copyright 2015-2026 Apple Inc. and the FoundationDB project authors
// Copyright 2021-2025 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

// Package rowdiff is the RFC-182 generative row-soundness differential
// harness: seeded random (schema, data, query) cases executed through the
// full production path (sqldriver → Cascades → executor → real FDB), rows
// diffed against Oracle M — a brute-force in-memory full-scan evaluation
// over the generator's own authoritative row set, reusing the engine's
// predicate evaluation (predicates.Comparison.Eval) so the planner is the
// only component removed. Any mismatch is a plan-soundness finding.
//
// The generator deliberately over-weights the matrix cell that hid the
// pk-intersection residual-drop bug (audit 2026-07-18): two or more indexed
// columns bound PLUS an unindexed residual, executed under every projection
// variant (`SELECT *` vs narrow — projection flips cost winners and masked
// that bug).
package rowdiff

import (
	"math/rand/v2"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
)

// NullsPlacement is an ORDER BY key's NULL position. Default follows the
// engine's Java/FDB-parity rule: NULLS FIRST ascending, NULLS LAST
// descending (tuple order). Explicit placements render as NULLS FIRST/LAST.
type NullsPlacement int

const (
	NullsDefault NullsPlacement = iota
	NullsFirst
	NullsLast
)

// OrderKey is one ORDER BY component. Nullable sort keys are allowed; keys
// are always suffixed with ID by the generator so the total order stays
// unique and the ordered comparator exact.
type OrderKey struct {
	Col   string
	Desc  bool
	Nulls NullsPlacement
	// Qual is the table-alias qualifier in a JOIN query ("L"/"R"), empty
	// otherwise.
	Qual string
}

// Query is one generated query body; the runner executes it under every
// projection variant and cross-checks row identity via ID.
type Query struct {
	Agg       *AggSpec          // nil = not an aggregate query
	Join      *JoinSpec         // nil = single-table query
	ThreeWay  *ThreeWayJoinSpec // non-nil = 3-way self-join
	Union     *UnionSpec        // non-nil = UNION [ALL] of two single-table branches
	Derived   *DerivedSpec      // non-nil = subquery in FROM (derived table)
	Exists    *ExistsSpec       // non-nil = append a correlated [NOT] EXISTS to WHERE
	ScalarSub *ScalarSubSpec    // non-nil = append a scalar-subquery comparison to WHERE
	Where     *BoolNode         // nil = no WHERE
	OrderBy   []OrderKey
	Limit     int  // 0 = no LIMIT
	Offset    int  // 0 = no OFFSET (only emitted alongside LIMIT + ORDER BY)
	Distinct  bool // SELECT DISTINCT (ORDER BY keys ⊆ projection enforced by generator)

	// WhereOverride, when non-nil, REPLACES the rendered WHERE conjunct list
	// with this literal SQL expression; the empty string means "no WHERE at
	// all". It exists for the RFC-201 §5.5 ternary-logic-partitioning oracle,
	// which needs `WHERE NOT (p)` and `WHERE (p) IS NULL` rendered from the
	// SAME predicate tree as `WHERE p` — forms no BoolNode can express (Kleene
	// NOT is not SQL NOT, and "the predicate evaluated to UNKNOWN" is not a
	// boolean node at all).
	//
	// Oracle M cannot evaluate an overridden query: the override is opaque SQL
	// and the oracle walks the typed tree. OracleRows therefore REFUSES such a
	// query with an error rather than silently evaluating q.Where and reporting
	// a divergence that is really a harness bug. The TLP oracle needs no Oracle
	// M — its property is the partition across the four renderings.
	WhereOverride *string
}

// Case is everything one seed produces.
type Case struct {
	Seed    uint64
	Table   TableDef
	Rows    []Row
	Queries []Query
	// Nested marks a case built by GenerateNested: its table carries a struct
	// column and its projections name dotted paths.
	Nested bool
}

// Projections returns the projection variants every query runs under:
// star, pk-only, a narrow subset, and the narrow subset reversed.
func (c *Case) Projections() [][]string {
	narrow := []string{"ID"}
	for i, col := range c.Table.Cols {
		if i%2 == 0 {
			narrow = append(narrow, col.Name)
		}
	}
	reversed := make([]string, len(narrow))
	for i, n := range narrow {
		reversed[len(narrow)-1-i] = n
	}
	return [][]string{nil /* SELECT * */, {"ID"}, narrow, reversed}
}

// joinProjections are the projection variants for a JOIN query. Every entry
// is a qualified column ("L.ID"); the renderer emits a unique alias per
// entry so no two output columns share a name.
func (c *Case) joinProjections() [][]string {
	var wide []string
	for _, side := range []string{"L", "R"} {
		wide = append(wide, side+".ID")
		for _, col := range c.Table.Cols {
			wide = append(wide, side+"."+col.Name)
		}
	}
	return [][]string{
		wide,
		{"L.ID", "R.ID"},
		// The third entry names a VALUE column, not just keys, so the projection
		// carries something the join has to thread through. It is drawn from the
		// table rather than written literally: a literal name is a column a
		// differently-shaped table does not have, and the query then fails 42703
		// for a reason that has nothing to do with what it was testing.
		{"R.ID", "L.ID", "L." + notNullSortCol(c.Table)},
	}
}

// threeWayProjections are the projection variants for a 3-way join query.
func (c *Case) threeWayProjections() [][]string {
	var wide []string
	for _, side := range []string{"L", "M", "R"} {
		wide = append(wide, side+".ID")
		for _, col := range c.Table.Cols {
			wide = append(wide, side+"."+col.Name)
		}
	}
	return [][]string{
		wide,
		{"L.ID", "M.ID", "R.ID"},
		{"R.ID", "M.ID", "L.ID", "M." + notNullSortCol(c.Table)},
	}
}

// ProjectionsFor returns the projection variants appropriate to the query
// (join queries never use `SELECT *` — see JoinSpec; an aggregate's output
// list is fixed by its own shape).
func (c *Case) ProjectionsFor(q Query) [][]string {
	switch {
	case q.Agg != nil:
		return [][]string{nil}
	case q.Join != nil:
		return c.joinProjections()
	case q.ThreeWay != nil:
		return c.threeWayProjections()
	}
	// Union and plain single-table queries share the single-table projections
	// (both UNION branches project the same columns).
	if c.Nested {
		return c.nestedProjections()
	}
	return c.Projections()
}

// Generate builds the Case for a seed. Same seed → identical Case.
func Generate(seed uint64) *Case {
	rng := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))

	table := genTable(rng)
	rows := genRows(rng, table)
	nQueries := 4 + rng.IntN(5)
	queries := make([]Query, 0, nQueries)
	for i := 0; i < nQueries; i++ {
		// ~1/4 self-joins: the join planner is a large surface and the
		// oracle stays a plain nested loop over the same authoritative rows.
		switch {
		case rng.IntN(10) == 0:
			// 3-way self-join — exercises join ORDERING (3! build orders).
			// Rarer than 2-way: the planner explores far more build orders for a
			// 3-way chain, so each such query costs ~15x a single-table one; a
			// higher rate would blow the nightly deep-sweep timeout for little
			// extra coverage (thousands of 3-way samples still accrue per night).
			queries = append(queries, genThreeWayJoin(rng, table))
			continue
		case rng.IntN(4) == 0:
			queries = append(queries, genJoinQuery(rng, table))
			continue
		case rng.IntN(4) == 0:
			// Aggregates: the surface where a dropped residual produced
			// silently wrong SUMs (the AGG-RESIDUAL class).
			queries = append(queries, genAggQuery(rng, table))
			continue
		case rng.IntN(6) == 0:
			// UNION [ALL] of two single-table branches — set-op dedup.
			queries = append(queries, genUnionQuery(rng, table))
			continue
		case rng.IntN(6) == 0:
			// Derived table (subquery in FROM) — planner flattening / pushdown.
			queries = append(queries, genDerivedQuery(rng, table))
			continue
		}
		queries = append(queries, genQuery(rng, table))
	}
	return &Case{Seed: seed, Table: table, Rows: rows, Queries: queries}
}

// genQuery builds a single-table query with killer-cell leaf bias, optional
// expression leaves, ORDER BY, EXISTS, scalar subquery, DISTINCT, and LIMIT.
func genQuery(rng *rand.Rand, table TableDef) Query {
	leaves := genQueryLeaves(rng, table)
	where := buildWhereTree(rng, leaves)
	orderBy := genOrderBy(rng, table)

	q := Query{Where: where, OrderBy: orderBy}

	// Correlated [NOT] EXISTS on ~1/4 of plain queries: a rich wrong-rows
	// surface (semi-join, decorrelation, correlation binding) the generator
	// otherwise never reaches.
	if rng.IntN(4) == 0 {
		q.Exists = genExists(rng, table)
	}

	// Non-correlated aggregate scalar subquery comparison on ~1/5 of plain
	// queries — a Go-only read extension (no Java equivalent) whose scalar
	// evaluation and NULL-when-empty semantics need their own coverage.
	if rng.IntN(5) == 0 {
		q.ScalarSub = genScalarSub(rng, table)
	}

	// DISTINCT on ~1/6 of queries. ORDER BY is dropped for DISTINCT — the
	// projection variants do not all contain every sort key, and SQL
	// requires DISTINCT's ORDER BY ⊆ projection; the unordered multiset
	// comparison stays exact.
	if rng.IntN(6) == 0 {
		q.Distinct = true
		q.OrderBy = nil
	}

	// LIMIT on ~1/4 of queries: small values exercise the boundary, large
	// values exercise the |M| < k clamp.
	if rng.IntN(4) == 0 {
		q.Limit = 1 + rng.IntN(30)
		// OFFSET only with a total ORDER BY: without one the skipped prefix
		// is implementation-defined and the result is not a checkable set.
		if len(q.OrderBy) > 0 && rng.IntN(3) == 0 {
			q.Offset = rng.IntN(5)
		}
	}

	// NOT around the whole WHERE (~1/10) — exercises the negation
	// normalization (DeMorgan) the planner applies before matching.
	if q.Where != nil && rng.IntN(10) == 0 {
		q.Where = &BoolNode{Not: true, And: true, Kids: []*BoolNode{q.Where}}
	}

	return q
}

func genQueryLeaves(rng *rand.Rand, table TableDef) []*Pred {
	indexed := map[string]bool{}
	for _, idx := range table.Indexes {
		for _, c := range idx.Cols {
			indexed[c] = true
		}
	}
	var indexedCols, unindexedCols []ColumnDef
	for _, c := range table.Cols {
		if indexed[c.Name] {
			indexedCols = append(indexedCols, c)
		} else {
			unindexedCols = append(unindexedCols, c)
		}
	}

	// The killer-cell bias: with ≥2 indexed columns available, half of all
	// queries take ≥2 indexed equality/range leaves PLUS one unindexed
	// residual leaf.
	var leaves []*Pred
	if len(indexedCols) >= 2 && rng.IntN(2) == 0 {
		perm := rng.Perm(len(indexedCols))
		nIdxLeaves := 2 + rng.IntN(len(indexedCols)-1)
		if nIdxLeaves > len(indexedCols) {
			nIdxLeaves = len(indexedCols)
		}
		for _, pi := range perm[:nIdxLeaves] {
			leaves = append(leaves, genPred(rng, table, indexedCols[pi], true))
		}
		if len(unindexedCols) > 0 && rng.IntN(4) != 0 {
			leaves = append(leaves, genPred(rng, table, unindexedCols[rng.IntN(len(unindexedCols))], false))
		}
	} else {
		nLeaves := 1 + rng.IntN(3)
		for i := 0; i < nLeaves; i++ {
			col := table.Cols[rng.IntN(len(table.Cols))]
			leaves = append(leaves, genPred(rng, table, col, indexed[col.Name]))
		}
	}
	return appendExprLeaves(rng, table, leaves)
}

func appendExprLeaves(rng *rand.Rand, table TableDef, leaves []*Pred) []*Pred {
	// Column-vs-column leaf (~1/7): non-sargable, so it forces a residual
	// filter and reaches plan shapes literal comparisons never do. Numeric
	// columns only (BIGINT/DOUBLE/FLOAT) — every pair has a common maximum
	// type (values.MaximumType), so a mixed-width pair (e.g. `b > d`)
	// exercises ResolveComparison's promoteColumnColumnNumeric the same way
	// a correlated join comparand does (see genJoinQuery), just without a
	// join: neither FieldValue is a compile-time constant here either, so
	// this is the SAME code path at row-scope instead of join-scope. A
	// cross-CATEGORY pair (numeric vs string/bool) has no common maximum
	// type and would only test the 42804 error path, so it stays excluded.
	if rng.IntN(7) == 0 {
		numericCols := numericColsOf(table)
		l := numericCols[rng.IntN(len(numericCols))]
		r := numericCols[rng.IntN(len(numericCols))]
		if l != r {
			ops := []predicates.ComparisonType{
				predicates.ComparisonEquals, predicates.ComparisonNotEquals,
				predicates.ComparisonLessThan, predicates.ComparisonGreaterThan,
				predicates.ComparisonLessThanOrEq, predicates.ComparisonGreaterThanEq,
			}
			leaves = append(leaves, &Pred{Col: l, Op: ops[rng.IntN(len(ops))], RhsCol: r})
		}
	}

	// Searched-CASE leaf (~1/8): `(CASE WHEN … THEN … ELSE … END) op lit` — a
	// conditional-expression residual the planner must evaluate per row, with
	// SQL's WHEN-TRUE-only branch selection and NULL-arm propagation.
	if rng.IntN(8) == 0 {
		leaves = append(leaves, genCaseLeaf(rng, table))
	}

	// String-function leaf (~1/8): `UPPER/LOWER/LENGTH(s) op lit` — a scalar
	// function residual with NULL propagation over the STRING column.
	if rng.IntN(8) == 0 {
		leaves = append(leaves, genStrFnLeaf(rng))
	}

	// Numeric-function leaf (~1/8): `ABS(col) op lit` / `MOD(col, k) op lit`.
	if rng.IntN(8) == 0 {
		leaves = append(leaves, genNumFnLeaf(rng, table))
	}

	// CAST leaf (~1/8): `CAST(col AS STRING) op 'lit'` over BIGINT / BOOLEAN.
	if rng.IntN(8) == 0 {
		leaves = append(leaves, genCastLeaf(rng, table))
	}

	// NOT on a leaf (~1/8 each): `NOT (a = 1)` / `a NOT IN (…)`.
	for _, lf := range leaves {
		if rng.IntN(8) == 0 {
			lf.Negated = true
		}
	}
	return leaves
}

func buildWhereTree(rng *rand.Rand, leaves []*Pred) *BoolNode {
	var where *BoolNode
	if len(leaves) == 1 {
		where = &BoolNode{Leaf: leaves[0]}
	} else if rng.IntN(4) != 0 {
		// AND of all leaves (the majority — conjunctive index planning).
		kids := make([]*BoolNode, len(leaves))
		for i, l := range leaves {
			kids[i] = &BoolNode{Leaf: l}
		}
		where = &BoolNode{And: true, Kids: kids}
	} else {
		// One OR splitting the leaves into two conjuncts.
		split := 1 + rng.IntN(len(leaves)-1)
		left := make([]*BoolNode, 0, split)
		for _, l := range leaves[:split] {
			left = append(left, &BoolNode{Leaf: l})
		}
		right := make([]*BoolNode, 0, len(leaves)-split)
		for _, l := range leaves[split:] {
			right = append(right, &BoolNode{Leaf: l})
		}
		wrap := func(kids []*BoolNode) *BoolNode {
			if len(kids) == 1 {
				return kids[0]
			}
			return &BoolNode{And: true, Kids: kids}
		}
		where = &BoolNode{And: false, Kids: []*BoolNode{wrap(left), wrap(right)}}
	}
	return where
}

func genOrderBy(rng *rand.Rand, table TableDef) []OrderKey {
	var orderBy []OrderKey
	switch rng.IntN(4) {
	case 0:
		orderBy = []OrderKey{{Col: "ID", Desc: rng.IntN(2) == 0}}
	case 1:
		// B is the NOT NULL sortable value column; append ID for a total
		// order so the oracle's expected sequence is unique.
		orderBy = []OrderKey{{Col: notNullSortCol(table), Desc: rng.IntN(2) == 0}, {Col: "ID"}}
	case 2:
		// NULLABLE sort key (A, C, S, D, or E) with a NULLS placement drawn
		// from {default, FIRST, LAST}; ID suffix keeps the total order
		// unique. D/E (DOUBLE/FLOAT) exercise compareSortKey's float64 arm.
		// On this generator's deliberately NaN-free domain (see the ColType
		// comment), values.CompareFloat64 agrees with the relevant FDB tuple/index
		// order, including the signed-zero split. It does NOT agree for arbitrary
		// raw NaN signs/payloads, which this generator never emits. The sort order
		// deliberately disagrees with predicate equality on -0.0/+0.0.
		sortPool := nullableSortCols(table)
		col := sortPool[rng.IntN(len(sortPool))]
		orderBy = []OrderKey{
			{Col: col, Desc: rng.IntN(2) == 0, Nulls: NullsPlacement(rng.IntN(3))},
			{Col: "ID"},
		}
	}
	return orderBy
}
